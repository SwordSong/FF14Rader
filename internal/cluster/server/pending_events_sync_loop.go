package server

import (
	"log"
	"time"

	cluster "github.com/user/ff14rader/internal/cluster"
	"github.com/user/ff14rader/internal/db"
)

type pendingReportEventsRow struct {
	ReportCode string `gorm:"column:report_code"`
	Events     int    `gorm:"column:events"`
}

// SyncPendingReportEventsFromDB 从 fight_sync_maps 同步未解析事件数到 report registry。
func SyncPendingReportEventsFromDB(registry *ReportHostRegistry) (int, int, error) {
	if registry == nil {
		return 0, 0, nil
	}

	rows := make([]pendingReportEventsRow, 0)
	if err := db.DB.Raw(`
		SELECT
			split_part(master_id, '-', 1) AS report_code,
			COUNT(*)::int AS events
		FROM fight_sync_maps
		WHERE parsed_done = false
		GROUP BY split_part(master_id, '-', 1)
	`).Scan(&rows).Error; err != nil {
		return 0, 0, err
	}

	pendingEvents := make(map[string]int, len(rows))
	for _, row := range rows {
		code := cluster.NormalizeReportCode(row.ReportCode)
		if code == "" {
			continue
		}
		events := row.Events
		if events < 0 {
			events = 0
		}
		pendingEvents[code] = events
	}

	updated := 0
	snapshot := registry.Snapshot()

	for code, events := range pendingEvents {
		host := ""
		if entry, ok := snapshot[code]; ok {
			host = entry.Host
		}
		added, _ := registry.RegisterReportWithEvents(code, host, events)
		if added > 0 {
			updated++
		}
	}

	for code, entry := range snapshot {
		normalized := cluster.NormalizeReportCode(code)
		if normalized == "" {
			continue
		}
		if _, ok := pendingEvents[normalized]; ok {
			continue
		}
		if entry.Events <= 0 {
			continue
		}
		added, _ := registry.RegisterReportWithEvents(normalized, entry.Host, 0)
		if added > 0 {
			updated++
		}
	}

	return updated, len(pendingEvents), nil
}

// StartRegistryPendingEventsSyncLoop 启动 registry 未解析事件回填循环。
func StartRegistryPendingEventsSyncLoop(registry *ReportHostRegistry) {
	if registry == nil {
		return
	}

	interval := ClusterRegistryPendingSyncInterval()
	if interval <= 0 {
		return
	}

	syncOnce := func(trigger string) {
		updated, pendingReports, err := SyncPendingReportEventsFromDB(registry)
		if err != nil {
			log.Printf("[CLUSTER] 同步未解析事件到registry失败 trigger=%s err=%v", trigger, err)
			return
		}
		if updated > 0 {
			log.Printf("[CLUSTER] 同步未解析事件到registry完成 trigger=%s pendingReports=%d updated=%d interval=%s", trigger, pendingReports, updated, interval)
		}
	}

	go func() {
		syncOnce("startup")
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			syncOnce("ticker")
		}
	}()
}
