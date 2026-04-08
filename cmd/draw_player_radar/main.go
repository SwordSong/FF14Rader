package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/user/ff14rader/internal/config"
	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/models"
	"github.com/user/ff14rader/internal/render"
	"gorm.io/gorm"
)

type playerRadarRow struct {
	ID               uint
	Name             string
	Server           string
	OutputAbility    float64
	BattleAbility    float64
	TeamContribution float64
	ProgressionSpeed float64
	StabilityScore   float64
	PotentialScore   float64
}

func main() {
	var (
		name   string
		server string
		output string
		width  int
		height int
	)

	flag.StringVar(&name, "name", "", "player name")
	flag.StringVar(&server, "server", "", "server name")
	flag.StringVar(&output, "output", "", "output png path (optional)")
	flag.IntVar(&width, "width", 900, "image width")
	flag.IntVar(&height, "height", 900, "image height")
	flag.Parse()

	name = strings.TrimSpace(name)
	server = strings.TrimSpace(server)
	if name == "" || server == "" {
		log.Fatalf("missing required args: --name and --server")
	}

	cfg := config.LoadConfig()
	if cfg.PostgresWriteDSN == "" || cfg.PostgresReadDSN == "" {
		log.Fatalf("Postgres DSN missing (POSTGRES_WRITE_DSN/POSTGRES_READ_DSN)")
	}
	db.InitDB(cfg.PostgresWriteDSN, cfg.PostgresReadDSN)

	player, err := loadPlayerByNameAndServer(name, server)
	if err != nil {
		log.Fatalf("load player failed: %v", err)
	}

	if output == "" {
		picDir := filepath.Join(".", "pic")
		if err := os.MkdirAll(picDir, 0755); err != nil {
			log.Fatalf("create pic dir failed: %v", err)
		}
		filename := fmt.Sprintf("%s_%s.png", sanitizeFilenameToken(player.Name), sanitizeFilenameToken(player.Server))
		output = filepath.Join(picDir, filename)
	}

	radar := render.NewRadarChart(width, height)
	labels := []string{"输出能力", "战斗能力", "团队贡献", "开荒速度", "稳定度", "潜力值"}
	values := []float64{
		player.OutputAbility,
		player.BattleAbility,
		player.TeamContribution,
		player.ProgressionSpeed,
		player.StabilityScore,
		player.PotentialScore,
	}
	title := player.Name

	if err := radar.DrawMetrics(title, labels, values, output); err != nil {
		log.Fatalf("draw radar failed: %v", err)
	}

	fmt.Printf("player: %s @ %s (id=%d)\n", player.Name, player.Server, player.ID)
	fmt.Printf("output_ability=%.2f\n", player.OutputAbility)
	fmt.Printf("battle_ability=%.2f\n", player.BattleAbility)
	fmt.Printf("team_contribution=%.2f\n", player.TeamContribution)
	fmt.Printf("progression_speed=%.2f\n", player.ProgressionSpeed)
	fmt.Printf("stability_score=%.2f\n", player.StabilityScore)
	fmt.Printf("potential_score=%.2f\n", player.PotentialScore)
	fmt.Printf("radar saved: %s\n", output)
}

func loadPlayerByNameAndServer(name, server string) (*playerRadarRow, error) {
	var row playerRadarRow
	err := db.DB.Model(&models.Player{}).
		Select("id", "name", "server", "output_ability", "battle_ability", "team_contribution", "progression_speed", "stability_score", "potential_score").
		Where("LOWER(name) = LOWER(?) AND LOWER(server) = LOWER(?)", name, server).
		Order("updated_at DESC").
		First(&row).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, fmt.Errorf("player not found by name+server: %s@%s", name, server)
		}
		return nil, err
	}
	return &row, nil
}

func sanitizeFilenameToken(v string) string {
	trimmed := strings.TrimSpace(v)
	if trimmed == "" {
		return "unknown"
	}
	re := regexp.MustCompile(`[^\p{L}\p{N}._-]+`)
	cleaned := re.ReplaceAllString(trimmed, "_")
	cleaned = strings.Trim(cleaned, "._-")
	if cleaned == "" {
		return "unknown"
	}
	return cleaned
}
