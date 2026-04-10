package main

import (
	"log"

	appapi "github.com/user/ff14rader/api"
	internalapi "github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/config"
	"github.com/user/ff14rader/internal/db"
)

func main() {
	cfg := config.LoadConfig()
	if cfg.PostgresWriteDSN == "" || cfg.PostgresReadDSN == "" {
		log.Fatalf("Postgres DSN missing (POSTGRES_WRITE_DSN/POSTGRES_READ_DSN)")
	}
	db.InitDB(cfg.PostgresWriteDSN, cfg.PostgresReadDSN)

	fflogsClient := internalapi.NewFFLogsClient(cfg.FFLogsClientID, cfg.FFLogsClientSecret)
	syncManager := internalapi.NewSyncManager(fflogsClient)

	service := &appapi.Service{
		SyncManager:        syncManager,
		EnableDashboardAPI: true,
	}

	log.Printf("debug monitor api listening on :%s", cfg.MonitorPort)
	if err := appapi.RunServer(cfg.MonitorPort, service); err != nil {
		log.Fatalf("debug monitor api failed: %v", err)
	}
}
