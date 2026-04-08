// test_xivanalysis_http 是手工联调脚本：
// 1) 从本地报告目录读取 fight_*_events.json；
// 2) 组合 DB 中的 report_metadata/fight 信息，请求 xivanalysis HTTP 服务；
// 3) 将返回结果写入 fight_*_analysis.json 供后续评分使用。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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

type fightTask struct {
	ID   int
	File string
}

func main() {
	// Load .env first so flag defaults can inherit environment values.
	cfg := config.LoadConfig()

	defaultAPIURL := envString("XIVA_API_URL", "http://127.0.0.1:22026")
	defaultAPIHost := envString("XIVA_API_HOST", "http://127.0.0.1")
	defaultAPIPortStart := envInt("XIVA_PORT_START", 0)
	defaultAPIPortCount := envInt("XIVA_PORT_COUNT", 10)
	defaultCallConcurrency := envInt("XIVA_CALL_CONCURRENCY", 0)

	var (
		reportDir    = flag.String("dir", "", "Report directory containing fight_*_events.json")
		code         = flag.String("code", "", "Report code (master_report) to query")
		fightID      = flag.Int("fight-id", 0, "Fight ID to analyze (0 = all fights in dir)")
		apiURL       = flag.String("api-url", defaultAPIURL, "xivanalysis API base URL (default from XIVA_API_URL)")
		apiHost      = flag.String("api-host", defaultAPIHost, "API host when using multi-port mode (default from XIVA_API_HOST)")
		apiPortStart = flag.Int("api-port-start", defaultAPIPortStart, "start API port for multi-port mode (default from XIVA_PORT_START)")
		apiPortCount = flag.Int("api-port-count", defaultAPIPortCount, "API port count for multi-port mode (default from XIVA_PORT_COUNT)")
		concurrency  = flag.Int("concurrency", defaultCallConcurrency, "request concurrency (0 = auto, default from XIVA_CALL_CONCURRENCY)")
		outDir       = flag.String("out-dir", "", "Output directory for analysis JSON (default report dir)")
		playerID     = flag.Uint("player-id", 0, "Optional player_id to scope DB queries")
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
		*outDir = resolvedDir
	}

	if cfg.PostgresWriteDSN == "" || cfg.PostgresReadDSN == "" {
		log.Fatalf("Postgres DSN missing (POSTGRES_WRITE_DSN/POSTGRES_READ_DSN)")
	}
	db.InitDB(cfg.PostgresWriteDSN, cfg.PostgresReadDSN)

	log.Printf("[config] api_url=%s api_host=%s api_port_start=%d api_port_count=%d concurrency=%d", *apiURL, *apiHost, *apiPortStart, *apiPortCount, *concurrency)

	apiEndpoints, err := resolveAPIEndpoints(*apiURL, *apiHost, *apiPortStart, *apiPortCount)
	if err != nil {
		log.Fatalf("resolve api endpoints failed: %v", err)
	}

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

	tasks := make([]fightTask, 0, len(fightFiles))
	for _, file := range fightFiles {
		id := extractFightID(file)
		if id == 0 {
			continue
		}
		if *fightID > 0 && id != *fightID {
			continue
		}
		tasks = append(tasks, fightTask{ID: id, File: file})
	}

	if len(tasks) == 0 {
		log.Fatalf("no fight files matched (--fight-id=%d)", *fightID)
	}

	workerCount := *concurrency
	if workerCount <= 0 {
		workerCount = len(apiEndpoints)
	}
	if workerCount <= 0 {
		workerCount = 1
	}
	if workerCount > len(tasks) {
		workerCount = len(tasks)
	}

	log.Printf("[info] dispatch fights=%d workers=%d endpoints=%d", len(tasks), workerCount, len(apiEndpoints))
	if err := processFightTasks(tasks, resolvedDir, *outDir, *code, report, apiEndpoints, workerCount); err != nil {
		log.Fatalf("analyze failed: %v", err)
	}
}

func processFightTasks(tasks []fightTask, reportDir, outDir, reportCode string, report reportPayload, endpoints []string, workerCount int) error {
	jobs := make(chan fightTask)
	results := make(chan error, len(tasks))

	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		endpoint := endpoints[i%len(endpoints)]
		workerID := i + 1
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &http.Client{Timeout: 90 * time.Second}
			for task := range jobs {
				events, err := readEvents(filepath.Join(reportDir, task.File))
				if err != nil {
					results <- fmt.Errorf("worker=%d fight=%d read events: %w", workerID, task.ID, err)
					continue
				}

				payload := requestPayload{
					Code:    reportCode,
					FightID: task.ID,
					Report:  report,
					Events:  events,
				}

				log.Printf("[request] worker=%d fight=%d endpoint=%s events=%d", workerID, task.ID, endpoint, eventCount(events))
				respBody, err := postJSON(client, endpoint, payload)
				if err != nil {
					results <- fmt.Errorf("worker=%d fight=%d request: %w", workerID, task.ID, err)
					continue
				}

				outPath := filepath.Join(outDir, fmt.Sprintf("fight_%d_analysis.json", task.ID))
				if err := os.WriteFile(outPath, respBody, 0644); err != nil {
					results <- fmt.Errorf("worker=%d fight=%d write output: %w", workerID, task.ID, err)
					continue
				}

				log.Printf("[written] fight=%d path=%s", task.ID, outPath)
				results <- nil
			}
		}()
	}

	for _, task := range tasks {
		jobs <- task
	}
	close(jobs)

	wg.Wait()
	close(results)

	failCount := 0
	for err := range results {
		if err == nil {
			continue
		}
		failCount++
		log.Printf("[fail] %v", err)
	}
	if failCount > 0 {
		return fmt.Errorf("%d fights failed", failCount)
	}
	return nil
}

func resolveAPIEndpoints(apiURL, apiHost string, portStart, portCount int) ([]string, error) {
	if portStart > 0 {
		if portCount <= 0 {
			return nil, fmt.Errorf("invalid --api-port-count: %d", portCount)
		}
		host := strings.TrimSpace(apiHost)
		if host == "" {
			host = "http://127.0.0.1"
		}
		host = strings.TrimRight(host, "/")

		endpoints := make([]string, 0, portCount)
		for i := 0; i < portCount; i++ {
			raw := fmt.Sprintf("%s:%d", host, portStart+i)
			ep, err := normalizeAPIURL(raw)
			if err != nil {
				return nil, fmt.Errorf("invalid endpoint %s: %w", raw, err)
			}
			endpoints = append(endpoints, ep)
		}
		return endpoints, nil
	}

	ep, err := normalizeAPIURL(apiURL)
	if err != nil {
		return nil, err
	}
	return []string{ep}, nil
}

func loadReportRow(code string, playerID uint) (*models.Report, error) {
	var reports []models.Report
	query := db.DB.Model(&models.Report{}).
		Where("master_report = ? OR source_report = ?", code, code).
		Order("id desc")
	if playerID > 0 {
		query = query.Where("player_id = ?", playerID)
	}
	if err := query.Find(&reports).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("report not found: %s", code)
		}
		return nil, err
	}
	if len(reports) == 0 {
		return nil, fmt.Errorf("report not found: %s", code)
	}

	if selected := selectPreferredReportRow(reports, code); selected != nil {
		return selected, nil
	}

	for i := range reports {
		if reportMetadataHasMasterData(reports[i].ReportMetadata) {
			return &reports[i], nil
		}
	}

	fallback := reports[0]
	if fallback.MasterReport != "" {
		var sameMaster []models.Report
		masterQuery := db.DB.Model(&models.Report{}).
			Where("master_report = ?", fallback.MasterReport).
			Order("id desc")
		if playerID > 0 {
			masterQuery = masterQuery.Where("player_id = ?", playerID)
		}
		if err := masterQuery.Find(&sameMaster).Error; err == nil {
			for i := range sameMaster {
				if reportMetadataHasMasterData(sameMaster[i].ReportMetadata) {
					return &sameMaster[i], nil
				}
			}
		}
	}

	return &fallback, nil
}

func selectPreferredReportRow(reports []models.Report, code string) *models.Report {
	normalizedCode := strings.TrimSpace(code)
	bestIndex := -1
	bestRank := 1 << 30

	for i := range reports {
		rank := reportRowRank(reports[i], normalizedCode)
		if rank >= 100 {
			continue
		}
		if !reportMetadataHasMasterData(reports[i].ReportMetadata) {
			rank += 10
		}
		if rank < bestRank {
			bestRank = rank
			bestIndex = i
		}
	}

	if bestIndex >= 0 {
		return &reports[bestIndex]
	}
	return nil
}

func reportRowRank(report models.Report, code string) int {
	masterMatch := strings.EqualFold(strings.TrimSpace(report.MasterReport), code)
	sourceMatch := strings.EqualFold(strings.TrimSpace(report.SourceReport), code)

	switch {
	case masterMatch && sourceMatch:
		return 0
	case masterMatch:
		return 1
	case sourceMatch:
		return 2
	default:
		return 100
	}
}

func reportMetadataHasMasterData(raw datatypes.JSON) bool {
	if len(raw) == 0 {
		return false
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false
	}
	_, ok := payload["masterData"]
	return ok
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

func eventCount(events any) int {
	switch v := events.(type) {
	case []any:
		return len(v)
	default:
		return 0
	}
}

func normalizeAPIURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/analyze"
	}
	return u.String(), nil
}

func postJSON(client *http.Client, endpoint string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

func envString(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}
