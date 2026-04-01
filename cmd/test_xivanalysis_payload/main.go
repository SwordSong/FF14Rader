package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/user/ff14rader/internal/config"
	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/models"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type fightRow struct {
	FightID         int
	MasterID        string
	Name            string
	Kill            bool
	StartTime       int64
	EndTime         int64
	FightPercentage float64
	BossPercentage  float64
	EncounterID     int
	Difficulty      int
	GameZone        datatypes.JSON
}

type reportPayload struct {
	Title      string      `json:"title"`
	StartTime  int64       `json:"startTime"`
	EndTime    int64       `json:"endTime"`
	Fights     []fightJSON `json:"fights"`
	MasterData any         `json:"masterData"`
}

type fightJSON struct {
	ID              int             `json:"id"`
	Name            string          `json:"name"`
	Kill            bool            `json:"kill"`
	StartTime       int64           `json:"startTime"`
	EndTime         int64           `json:"endTime"`
	FightPercentage float64         `json:"fightPercentage"`
	BossPercentage  float64         `json:"bossPercentage"`
	EncounterID     int             `json:"encounterID"`
	Difficulty      int             `json:"difficulty"`
	GameZone        json.RawMessage `json:"gameZone,omitempty"`
}

type requestPayload struct {
	Code    string        `json:"code"`
	FightID int           `json:"fightId"`
	Report  reportPayload `json:"report"`
	Events  any           `json:"events"`
}

func main() {
	var (
		reportDir = flag.String("dir", "", "Report directory containing fight_*_events.json")
		code      = flag.String("code", "", "Report code (master_report) to query")
		fightID   = flag.Int("fight-id", 0, "Fight ID to dump (0 = all fights in dir)")
		outDir    = flag.String("out-dir", "", "Output directory for request JSON (default report dir/requests)")
		playerID  = flag.Uint("player-id", 0, "Optional player_id to scope DB queries")
	)
	flag.Parse()

	if strings.TrimSpace(*reportDir) == "" {
		log.Fatalf("missing --dir")
	}
	resolvedDir, err := filepath.Abs(*reportDir)
	if err != nil {
		log.Fatalf("resolve dir: %v", err)
	}
	if *code == "" {
		*code = filepath.Base(resolvedDir)
	}
	if *outDir == "" {
		*outDir = filepath.Join(resolvedDir, "requests")
	}
	if err := os.MkdirAll(*outDir, 0755); err != nil {
		log.Fatalf("create out-dir failed: %v", err)
	}

	cfg := config.LoadConfig()
	if cfg.PostgresWriteDSN == "" || cfg.PostgresReadDSN == "" {
		log.Fatalf("Postgres DSN missing (POSTGRES_WRITE_DSN/POSTGRES_READ_DSN)")
	}
	db.InitDB(cfg.PostgresWriteDSN, cfg.PostgresReadDSN)

	reportRow, err := loadReportRow(*code, *playerID)
	if err != nil {
		log.Fatalf("load report failed: %v", err)
	}
	masterData, err := extractMasterData(reportRow.ReportMetadata)
	if err != nil {
		log.Fatalf("parse report_metadata failed: %v", err)
	}
	fights, err := loadFights(*code, *playerID)
	if err != nil {
		log.Fatalf("load fights failed: %v", err)
	}
	if len(fights) == 0 {
		log.Fatalf("no fights found for report %s", *code)
	}

	fightJSONs := buildFightJSONs(fights)
	report := reportPayload{
		Title:      reportRow.Title,
		StartTime:  reportRow.StartTime,
		EndTime:    reportRow.EndTime,
		Fights:     fightJSONs,
		MasterData: masterData,
	}

	fightFiles, err := listFightFiles(resolvedDir)
	if err != nil {
		log.Fatalf("list events failed: %v", err)
	}
	if len(fightFiles) == 0 {
		log.Fatalf("no fight_*_events.json files found in %s", resolvedDir)
	}

	for _, file := range fightFiles {
		id := extractFightID(file)
		if id == 0 {
			continue
		}
		if *fightID > 0 && id != *fightID {
			continue
		}

		events, err := readEvents(filepath.Join(resolvedDir, file))
		if err != nil {
			log.Printf("[skip] fight %d events: %v", id, err)
			continue
		}

		payload := requestPayload{
			Code:    *code,
			FightID: id,
			Report:  report,
			Events:  events,
		}

		outPath := filepath.Join(*outDir, fmt.Sprintf("fight_%d_request.json", id))
		if err := writeJSON(outPath, payload); err != nil {
			log.Fatalf("write request failed: %v", err)
		}
		log.Printf("[written] %s", outPath)
	}
}

func loadReportRow(code string, playerID uint) (*models.Report, error) {
	var report models.Report
	query := db.DB.Model(&models.Report{}).
		Where("master_report = ? OR source_report = ?", code, code).
		Order("id desc")
	if playerID > 0 {
		query = query.Where("player_id = ?", playerID)
	}
	if err := query.First(&report).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("report not found: %s", code)
		}
		return nil, err
	}
	return &report, nil
}

func loadFights(code string, playerID uint) ([]fightRow, error) {
	var rows []fightRow
	codeJSON, err := json.Marshal([]string{code})
	if err != nil {
		return nil, fmt.Errorf("marshal source_ids filter: %v", err)
	}
	query := db.DB.Table("fight_sync_maps").
		Select("fight_id, master_id, name, kill, start_time, end_time, fight_percentage, boss_percentage, encounter_id, difficulty, game_zone").
		Where("source_ids @> ?", datatypes.JSON(codeJSON))
	if playerID > 0 {
		query = query.Where("player_id = ?", playerID)
	}
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		return rows, nil
	}

	fallback := db.DB.Table("fight_sync_maps").
		Select("fight_id, master_id, name, kill, start_time, end_time, fight_percentage, boss_percentage, encounter_id, difficulty, game_zone").
		Where("master_id LIKE ?", code+"-%")
	if playerID > 0 {
		fallback = fallback.Where("player_id = ?", playerID)
	}
	if err := fallback.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func extractMasterData(raw datatypes.JSON) (any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("report_metadata empty")
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	md, ok := payload["masterData"]
	if !ok {
		return nil, fmt.Errorf("report_metadata.masterData missing")
	}
	return md, nil
}

func buildFightJSONs(rows []fightRow) []fightJSON {
	out := make([]fightJSON, 0, len(rows))
	for _, row := range rows {
		fight := fightJSON{
			ID:              row.FightID,
			Name:            row.Name,
			Kill:            row.Kill,
			StartTime:       row.StartTime,
			EndTime:         row.EndTime,
			FightPercentage: row.FightPercentage,
			BossPercentage:  row.BossPercentage,
			EncounterID:     row.EncounterID,
			Difficulty:      row.Difficulty,
		}
		if len(row.GameZone) > 0 {
			fight.GameZone = json.RawMessage(row.GameZone)
		}
		out = append(out, fight)
	}
	return out
}

func listFightFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "fight_") && strings.HasSuffix(name, "_events.json") {
			files = append(files, name)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		return extractFightID(files[i]) < extractFightID(files[j])
	})
	return files, nil
}

func extractFightID(filename string) int {
	var id int
	_, _ = fmt.Sscanf(filename, "fight_%d_events.json", &id)
	return id
}

func readEvents(filePath string) (any, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	var payload any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	switch v := payload.(type) {
	case []any:
		return v, nil
	case map[string]any:
		if events, ok := v["events"]; ok {
			if list, ok := events.([]any); ok {
				return list, nil
			}
			if eventsMap, ok := events.(map[string]any); ok {
				if list, ok := eventsMap["events"].([]any); ok {
					return list, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("unsupported events format")
}

func writeJSON(filePath string, payload any) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filePath, data, 0644)
}
