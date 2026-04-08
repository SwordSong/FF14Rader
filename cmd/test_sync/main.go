// test_sync 是手工联调脚本：
// 1) 根据 --name/--server（可选 --player-id）触发一次 FFLogs 增量同步；
// 2) 写入/更新 players、reports、fight_sync_maps 等数据；
// 3) 若存在待下载日志，会继续触发 V1 事件下载流程。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/config"
	"github.com/user/ff14rader/internal/db"
)

func main() {
	var (
		playerID = flag.Uint("player-id", 0, "Player ID in DB")
		name     = flag.String("name", "", "Character name")
		server   = flag.String("server", "", "Server slug")
		region   = "CN"
	)
	flag.Parse()

	if strings.TrimSpace(*name) == "" || strings.TrimSpace(*server) == "" {
		log.Fatalf("missing required flags: --name, --server")
	}

	cfg := config.LoadConfig()
	log.Printf("[DEBUG] FFLOGS_CLIENT_ID loaded=%t, FFLOGS_CLIENT_SECRET loaded=%t", cfg.FFLogsClientID != "", cfg.FFLogsClientSecret != "")
	if cfg.FFLogsClientID == "" || cfg.FFLogsClientSecret == "" {
		log.Fatalf("FFLogs credentials missing (FFLOGS_CLIENT_ID/FFLOGS_CLIENT_SECRET)")
	}
	if cfg.PostgresWriteDSN == "" || cfg.PostgresReadDSN == "" {
		log.Fatalf("Postgres DSN missing (POSTGRES_WRITE_DSN/POSTGRES_READ_DSN)")
	}
	if strings.TrimSpace(os.Getenv("FFLOGS_V1_API_KEY")) == "" {
		log.Println("[WARN] FFLOGS_V1_API_KEY missing; V1 downloads will fail if pending reports exist")
	}
	if strings.TrimSpace(os.Getenv("FFLOGS_ALL_REPORTS_DIR")) == "" {
		log.Println("[INFO] FFLOGS_ALL_REPORTS_DIR not set; using ./downloads/fflogs")
	}
	if strings.TrimSpace(os.Getenv("FFLOGS_V1_CONCURRENCY")) == "" {
		log.Println("[INFO] FFLOGS_V1_CONCURRENCY not set; using default 3")
	}
	if strings.TrimSpace(os.Getenv("FFLOGS_V1_RETRY")) == "" {
		log.Println("[INFO] FFLOGS_V1_RETRY not set; using default 2")
	}

	db.InitDB(cfg.PostgresWriteDSN, cfg.PostgresReadDSN)

	client := api.NewFFLogsClient(cfg.FFLogsClientID, cfg.FFLogsClientSecret)
	syncManager := api.NewSyncManager(client)

	ctx := context.Background()
	if err := syncManager.StartIncrementalSync(ctx, uint(*playerID), strings.TrimSpace(*name), strings.TrimSpace(*server), region); err != nil {
		log.Fatalf("sync failed: %v", err)
	}

	fmt.Println("sync completed")
	client.PrintQuotaSummary()
}
