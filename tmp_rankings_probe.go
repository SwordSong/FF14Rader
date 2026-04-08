package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/config"
	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/models"
)

type pendingRow struct {
	PlayerID uint
	MasterID string
}

func splitMasterID(masterID string) (string, int, bool) {
	idx := strings.LastIndex(masterID, "-")
	if idx <= 0 || idx >= len(masterID)-1 {
		return "", 0, false
	}
	fightID, err := strconv.Atoi(masterID[idx+1:])
	if err != nil {
		return "", 0, false
	}
	return masterID[:idx], fightID, true
}

func parseMasterData(raw []byte) (map[string]interface{}, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, false
	}
	if md, ok := payload["masterData"].(map[string]interface{}); ok {
		return md, true
	}
	if _, ok := payload["actors"].([]interface{}); ok {
		return payload, true
	}
	return nil, false
}

func main() {
	cfg := config.LoadConfig()
	db.InitDB(cfg.PostgresWriteDSN, cfg.PostgresReadDSN)

	var pendingCount int64
	if err := db.DB.Table("fight_sync_maps").
		Where("output_percentile IS NULL OR output_percentile <= 0").
		Count(&pendingCount).Error; err != nil {
		log.Fatalf("count pending failed: %v", err)
	}

	var pendingKillCount int64
	if err := db.DB.Table("fight_sync_maps").
		Where("kill = true AND (output_percentile IS NULL OR output_percentile <= 0)").
		Count(&pendingKillCount).Error; err != nil {
		log.Fatalf("count pending kill failed: %v", err)
	}
	fmt.Printf("pending_total=%d pending_kill=%d\n", pendingCount, pendingKillCount)

	var row pendingRow
	if err := db.DB.Table("fight_sync_maps").
		Select("player_id, master_id").
		Where("kill = true AND (output_percentile IS NULL OR output_percentile <= 0)").
		Order("id asc").
		First(&row).Error; err != nil {
		log.Fatalf("load pending failed: %v", err)
	}

	reportCode, fightID, ok := splitMasterID(row.MasterID)
	if !ok {
		log.Fatalf("invalid master_id: %s", row.MasterID)
	}

	var player models.Player
	if err := db.DB.Select("name").Where("id = ?", row.PlayerID).First(&player).Error; err != nil {
		log.Fatalf("load player failed: %v", err)
	}

	var reports []models.Report
	if err := db.DB.Select("master_report", "source_report", "report_metadata").
		Where("player_id = ? AND (master_report = ? OR source_report = ?)", row.PlayerID, reportCode, reportCode).
		Order("id desc").
		Find(&reports).Error; err != nil {
		log.Fatalf("load report metadata failed: %v", err)
	}

	var actorID int
	for _, r := range reports {
		md, ok := parseMasterData(r.ReportMetadata)
		if !ok {
			continue
		}
		actors, ok := md["actors"].([]interface{})
		if !ok {
			continue
		}
		for _, a := range actors {
			m, ok := a.(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := m["name"].(string)
			if name == player.Name {
				if idf, ok := m["id"].(float64); ok {
					actorID = int(idf)
					break
				}
			}
		}
		if actorID != 0 {
			break
		}
	}

	fmt.Printf("pending master=%s report=%s fight=%d player=%d name=%s actorID=%d\n", row.MasterID, reportCode, fightID, row.PlayerID, player.Name, actorID)

	client := api.NewFFLogsClient(cfg.FFLogsClientID, cfg.FFLogsClientSecret)
	query := `
query ($code: String, $fids: [Int]) {
reportData {
report(code: $code) {
rankings(fightIDs: $fids, playerMetric: rdps)
}
}
}`

	data, err := client.ExecuteQuery(context.Background(), query, map[string]interface{}{
		"code": reportCode,
		"fids": []int{fightID},
	})
	if err != nil {
		log.Fatalf("query rankings failed: %v", err)
	}

	reportData, _ := data["reportData"].(map[string]interface{})
	report, _ := reportData["report"].(map[string]interface{})
	rankings := report["rankings"]

	fmt.Printf("rankings type=%T\n", rankings)
	b, _ := json.MarshalIndent(rankings, "", "  ")
	if len(b) > 6000 {
		b = b[:6000]
	}
	fmt.Printf("rankings sample=%s\n", string(b))
}
