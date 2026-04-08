package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/user/ff14rader/internal/config"
	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/scoring"
)

func main() {
	var playerID uint
	flag.UintVar(&playerID, "player-id", 0, "optional player id; 0 means all scored players")
	flag.Parse()

	cfg := config.LoadConfig()
	if cfg.PostgresWriteDSN == "" || cfg.PostgresReadDSN == "" {
		log.Fatalf("Postgres DSN missing (POSTGRES_WRITE_DSN/POSTGRES_READ_DSN)")
	}
	db.InitDB(cfg.PostgresWriteDSN, cfg.PostgresReadDSN)

	svc := scoring.NewServiceFromEnv()
	if playerID > 0 {
		if err := svc.RefreshPlayerTeamAndStability(playerID); err != nil {
			log.Fatalf("backfill failed: %v", err)
		}
		fmt.Printf("backfill done: player_id=%d (team_contribution/stability_score/potential_score)\n", playerID)
		return
	}

	count, err := svc.RefreshAllPlayersTeamAndStability()
	if err != nil {
		log.Fatalf("backfill all failed: %v", err)
	}
	fmt.Printf("backfill done: players=%d (team_contribution/stability_score/potential_score)\n", count)
}
