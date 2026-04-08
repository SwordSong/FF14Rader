// backfill_output_percentile 用于通过 FFLogs 接口直接刷新 players.output_ability。
//
// 功能说明：
// 1) 根据 players 表中的 name/server/region 调用 FFLogs 角色 logs 排名接口。
// 2) 直接回写 players.output_ability（不依赖战斗级累计字段）。
//
// 用法：
//   - 刷新所有玩家：
//     go run ./cmd/backfill_output_percentile/main.go
//   - 仅刷新单个玩家：
//     go run ./cmd/backfill_output_percentile/main.go --player-id 123
package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/config"
	"github.com/user/ff14rader/internal/db"
)

func main() {
	var playerID uint
	flag.UintVar(&playerID, "player-id", 0, "optional player id; 0 means all players")
	flag.Parse()

	cfg := config.LoadConfig()
	if cfg.PostgresWriteDSN == "" || cfg.PostgresReadDSN == "" {
		log.Fatalf("Postgres DSN missing (POSTGRES_WRITE_DSN/POSTGRES_READ_DSN)")
	}
	if cfg.FFLogsClientID == "" || cfg.FFLogsClientSecret == "" {
		log.Fatalf("FFLogs credentials missing (FFLOGS_CLIENT_ID/FFLOGS_CLIENT_SECRET)")
	}

	db.InitDB(cfg.PostgresWriteDSN, cfg.PostgresReadDSN)

	client := api.NewFFLogsClient(cfg.FFLogsClientID, cfg.FFLogsClientSecret)
	syncManager := api.NewSyncManager(client)

	if playerID > 0 {
		refreshed, err := syncManager.BackfillPlayerOutputPercentiles(playerID)
		if err != nil {
			log.Fatalf("refresh output ability failed: %v", err)
		}
		fmt.Printf("refresh output ability done: player_id=%d refreshed=%d\n", playerID, refreshed)
		return
	}

	count, refreshed, err := syncManager.BackfillAllOutputPercentiles()
	if err != nil {
		log.Fatalf("refresh all output ability failed: %v", err)
	}
	fmt.Printf("refresh output ability done: players=%d refreshed=%d\n", count, refreshed)
}
