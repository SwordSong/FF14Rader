package api

import (
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/user/ff14rader/internal/cluster"
	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/models"
)

const defaultDispatchWarmupInterval = 60 * time.Second

type pendingParsedFightRow struct {
	PlayerID uint   `gorm:"column:player_id"`
	MasterID string `gorm:"column:master_id"`
}

type dispatchWarmupKey struct {
	PlayerID   uint
	ReportCode string
}

type dispatchWarmupStats struct {
	PendingRows      int
	GroupedTasks     int
	Queued           int
	AlreadyQueued    int
	AutoAssigned     int
	CandidateHosts   int
	MissingHost      int
	InvalidMasterID  int
	EnqueueErrors    int
	QueueSizeAfter   int
	DistinctPlayers  int
	DistinctReports  int
	StartedAtRFC3339 string
}

func parseDispatchWarmupInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CLUSTER_PREWARM_INTERVAL_SEC"))
	if raw == "" {
		return defaultDispatchWarmupInterval
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return defaultDispatchWarmupInterval
	}
	return time.Duration(v) * time.Second
}

func splitMasterReportCode(masterID string) (string, bool) {
	text := strings.TrimSpace(masterID)
	idx := strings.LastIndex(text, "-")
	if idx <= 0 {
		return "", false
	}
	code := cluster.NormalizeReportCode(text[:idx])
	if code == "" {
		return "", false
	}
	return code, true
}

func collectOnlineWarmupCandidateHosts(now time.Time) []string {
	seenAt := cluster.GlobalReportHostRegistry().SnapshotSeenAt()
	if len(seenAt) == 0 {
		return nil
	}

	ttl := cluster.ClusterHostTTL()
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	cutoff := now.Add(-ttl)

	hosts := make([]string, 0, len(seenAt))
	for host, tsText := range seenAt {
		normHost := cluster.NormalizeHost(host)
		if normHost == "" {
			continue
		}

		ts, err := time.Parse(time.RFC3339, strings.TrimSpace(tsText))
		if err != nil {
			continue
		}
		if ts.Before(cutoff) {
			continue
		}

		hosts = append(hosts, normHost)
	}

	sort.Strings(hosts)
	return hosts
}

func prewarmDispatchTaskQueueFromDB() (dispatchWarmupStats, error) {
	stats := dispatchWarmupStats{StartedAtRFC3339: time.Now().Format(time.RFC3339)}
	now := time.Now()
	candidateHosts := collectOnlineWarmupCandidateHosts(now)
	stats.CandidateHosts = len(candidateHosts)

	var rows []pendingParsedFightRow
	err := db.DB.Model(&models.FightSyncMap{}).
		Select("player_id", "master_id").
		Where("downloaded = ? AND parsed_done = ?", true, false).
		Find(&rows).Error
	if err != nil {
		return stats, err
	}

	stats.PendingRows = len(rows)
	if len(rows) == 0 {
		stats.QueueSizeAfter = len(cluster.GlobalDispatchTaskQueue().Snapshot())
		return stats, nil
	}

	grouped := make(map[dispatchWarmupKey]struct{}, len(rows))
	distinctPlayers := make(map[uint]struct{})
	distinctReports := make(map[string]struct{})

	for _, row := range rows {
		code, ok := splitMasterReportCode(row.MasterID)
		if !ok {
			stats.InvalidMasterID++
			continue
		}
		if row.PlayerID == 0 {
			stats.InvalidMasterID++
			continue
		}

		k := dispatchWarmupKey{PlayerID: row.PlayerID, ReportCode: code}
		grouped[k] = struct{}{}
		distinctPlayers[row.PlayerID] = struct{}{}
		distinctReports[code] = struct{}{}
	}

	keys := make([]dispatchWarmupKey, 0, len(grouped))
	for k := range grouped {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].PlayerID == keys[j].PlayerID {
			return keys[i].ReportCode < keys[j].ReportCode
		}
		return keys[i].PlayerID < keys[j].PlayerID
	})

	stats.GroupedTasks = len(keys)
	stats.DistinctPlayers = len(distinctPlayers)
	stats.DistinctReports = len(distinctReports)

	for _, key := range keys {
		host, ok := cluster.GlobalReportHostRegistry().ResolveHost(key.ReportCode)
		if !ok || strings.TrimSpace(host) == "" {
			host = cluster.GlobalReportHostRegistry().AssignHostForReport(key.ReportCode, 1, candidateHosts)
			if strings.TrimSpace(host) == "" {
				stats.MissingHost++
				continue
			}
			if _, persistMapErr := cluster.PersistReportHostMappings(host, []string{key.ReportCode}, now); persistMapErr != nil {
				log.Printf("[WARMUP] 持久化补偿映射失败 report=%s host=%s err=%v", key.ReportCode, host, persistMapErr)
			}
			stats.AutoAssigned++
		}

		queued, enqueueErr := cluster.GlobalDispatchTaskQueue().EnqueueReports(key.PlayerID, host, []string{key.ReportCode})
		if enqueueErr != nil {
			stats.EnqueueErrors++
			continue
		}
		if queued > 0 {
			stats.Queued += queued
		} else {
			stats.AlreadyQueued++
		}
	}

	stats.QueueSizeAfter = len(cluster.GlobalDispatchTaskQueue().Snapshot())
	return stats, nil
}

func (s *Service) StartDispatchTaskWarmupLoop() {
	if s == nil {
		return
	}

	interval := parseDispatchWarmupInterval()
	s.dispatchWarmupOnce.Do(func() {
		runOnce := func() {
			stats, err := prewarmDispatchTaskQueueFromDB()
			if err != nil {
				log.Printf("[WARMUP] dispatch prewarm failed: %v", err)
				return
			}
			log.Printf("[WARMUP] dispatch prewarm rows=%d grouped=%d players=%d reports=%d queued=%d existing=%d autoAssigned=%d candidateHosts=%d missingHost=%d invalidMasterID=%d enqueueErrors=%d queueSize=%d startedAt=%s",
				stats.PendingRows,
				stats.GroupedTasks,
				stats.DistinctPlayers,
				stats.DistinctReports,
				stats.Queued,
				stats.AlreadyQueued,
				stats.AutoAssigned,
				stats.CandidateHosts,
				stats.MissingHost,
				stats.InvalidMasterID,
				stats.EnqueueErrors,
				stats.QueueSizeAfter,
				stats.StartedAtRFC3339,
			)
		}

		runOnce()
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for range ticker.C {
				runOnce()
			}
		}()
	})
}
