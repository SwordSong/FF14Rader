package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/models"
)

const (
	defaultV1BaseURL     = "https://www.fflogs.com/v1"
	defaultAllReportsDir = "./downloads/fflogs"
)

type v1FightsResponse struct {
	Code   string    `json:"code"`
	Start  int64     `json:"start"`
	End    int64     `json:"end"`
	Title  string    `json:"title"`
	Fights []v1Fight `json:"fights"`
	// Processing is true when FFLogs is still building the report.
	Processing bool `json:"processing"`
}

type v1Fight struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Kill        bool   `json:"kill"`
	Difficulty  int    `json:"difficulty"`
	EncounterID int    `json:"encounterID"`
	StartTime   int64  `json:"start_time"`
	EndTime     int64  `json:"end_time"`
}

type v1EventsResponse struct {
	Events            []map[string]interface{} `json:"events"`
	NextPageTimestamp *int64                   `json:"nextPageTimestamp"`
}

type v1SavedEventsPayload struct {
	Events []map[string]interface{} `json:"events"`
	Count  int                      `json:"count"`
}

type v1EventsFetchStats struct {
	Pages        int
	Events       int
	FetchElapsed time.Duration
}

type allReportsFightEntry struct {
	SourceID   string
	AbsStart   int64
	Duration   int64
	ReportCode string
	FightID    int
}

type pendingFightEntry struct {
	MappingID  uint
	MasterID   string
	ReportCode string
	FightID    int
}

// downloadV1Reports 并行下载多个报告的战斗详情和事件数据，返回成功下载的报告代码列表

// downloadV1Reports 根据 fight_sync_maps 的 MasterID 下载 V1 事件数据
func (s *SyncManager) downloadV1Reports(ctx context.Context, playerID uint) ([]string, error) {
	reportWorkers := getV1ReportConcurrency()
	fightWorkers := getV1DownloadConcurrency()
	reportTimeout := getV1ReportTimeout()
	apiKey, err := getV1ApiKey()
	if err != nil {
		return nil, err
	}

	baseURL := getV1BaseURL()
	rootDir := getAllReportsDir()
	client := newV1HTTPClient(reportTimeout)

	if err := os.MkdirAll(rootDir, 0755); err != nil {
		return nil, fmt.Errorf("create reports dir failed: %v", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	downloadedSet := make(map[string]struct{})
	downloaded := make([]string, 0)
	pass := 0
	for {
		allPending, err := loadPendingFights(playerID)
		if err != nil {
			return downloaded, err
		}
		if len(allPending) == 0 {
			// 即使没有待下载战斗，也尝试补跑一次已下载但未解析的战斗评分。
			if err := s.scorePendingDownloadedFights(ctx, playerID); err != nil {
				log.Printf("[SCORE] final pending score pass failed: %v", err)
			}
			if err := markCompletedReportParseLogs(playerID); err != nil {
				return downloaded, err
			}
			return downloaded, nil
		}

		pass++

		grouped := groupPendingFights(allPending)
		reportCodes := make([]string, 0, len(grouped))
		for code := range grouped {
			reportCodes = append(reportCodes, code)
		}

		log.Printf("[INFO] 第 %d 轮待拉取 V1 fights 数量: %d (报告并发=%d, fight并发=%d)", pass, len(allPending), reportWorkers, fightWorkers)

		jobs := make(chan string)
		results := make(chan string)
		var wg sync.WaitGroup
		for i := 0; i < reportWorkers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for code := range jobs {
					if ctx.Err() != nil {
						return
					}

					pendingFights := grouped[code]
					if len(pendingFights) == 0 {
						continue
					}

					log.Printf("[V1] 报告 %s 下载开始 | 已下载=0/%d 当前并发=0/%d 当前速度=0.00 fights/s 预计完成=--", code, len(pendingFights), fightWorkers)
					start := time.Now()
					fightCount, reportDone, err := downloadV1Report(ctx, client, baseURL, apiKey, rootDir, playerID, code, pendingFights, fightWorkers, reportTimeout)
					if err != nil {
						log.Printf("[V1] 报告 %s 跳过: %v", code, err)
						continue
					}
					log.Printf("[V1] 下载完成 %s (fights=%d, elapsed=%s)", code, fightCount, time.Since(start))
					if reportDone {
						results <- code
					}
				}
			}()
		}

		go func() {
			for _, code := range reportCodes {
				select {
				case <-ctx.Done():
					close(jobs)
					return
				case jobs <- code:
				}
			}
			close(jobs)
		}()

		go func() {
			wg.Wait()
			close(results)
		}()

		for code := range results {
			if _, exists := downloadedSet[code]; !exists {
				downloadedSet[code] = struct{}{}
				downloaded = append(downloaded, code)
			}
		}

		passDone := make([]string, 0)
		for _, code := range reportCodes {
			complete, err := isReportDownloadedByMaster(playerID, code)
			if err != nil {
				log.Printf("[V1] 检查报告 %s 下载状态失败: %v", code, err)
				continue
			}
			if complete {
				passDone = append(passDone, code)
			}
		}
		if len(passDone) > 0 {
			if err := s.finalizeAllReportsDownloads(playerID, passDone); err != nil {
				return downloaded, err
			}
		}
		if err := s.scorePendingDownloadedFights(ctx, playerID); err != nil {
			log.Printf("[SCORE] pending score pass failed: %v", err)
		}
		if err := markCompletedReportParseLogs(playerID); err != nil {
			return downloaded, err
		}
	}
}

func markCompletedReportParseLogs(playerID uint) error {
	return db.DB.Exec(`
UPDATE reports r
SET downloaded = true
WHERE r.player_id = ?
	AND r.downloaded = false
	AND NOT EXISTS (
		SELECT 1
		FROM fight_sync_maps f
		WHERE f.player_id = r.player_id
			AND f.downloaded = false
			AND f.master_id LIKE r.master_report || '-%'
	)
`, playerID).Error
}

func getV1ApiKey() (string, error) {
	key := strings.TrimSpace(os.Getenv("FFLOGS_V1_API_KEY"))
	if key == "" {
		return "", fmt.Errorf("missing FFLOGS_V1_API_KEY env")
	}
	return key, nil
}

func getV1BaseURL() string {
	if raw := strings.TrimSpace(os.Getenv("FFLOGS_V1_BASE_URL")); raw != "" {
		return strings.TrimRight(raw, "/")
	}
	return defaultV1BaseURL
}

func getAllReportsDir() string {
	if raw := strings.TrimSpace(os.Getenv("FFLOGS_ALL_REPORTS_DIR")); raw != "" {
		return raw
	}
	return defaultAllReportsDir
}

func getV1DownloadConcurrency() int {
	const defaultConcurrency = 3
	if raw := strings.TrimSpace(os.Getenv("FFLOGS_V1_CONCURRENCY")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			return v
		}
	}
	return defaultConcurrency
}

func getV1ReportConcurrency() int {
	const defaultConcurrency = 2
	if raw := strings.TrimSpace(os.Getenv("FFLOGS_V1_REPORT_CONCURRENCY")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			return v
		}
	}
	return defaultConcurrency
}

func getV1ReportTimeout() time.Duration {
	const defaultTimeout = 120 * time.Second
	if raw := strings.TrimSpace(os.Getenv("FFLOGS_V1_REPORT_TIMEOUT_SEC")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			return time.Duration(v) * time.Second
		}
	}
	return defaultTimeout
}

func getV1RetryCount() int {
	const defaultRetry = 1
	if raw := strings.TrimSpace(os.Getenv("FFLOGS_V1_RETRY")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			return v
		}
	}
	return defaultRetry
}

func getV1TranslateEnabled() bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv("FFLOGS_V1_TRANSLATE")))
	switch raw {
	case "", "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func newV1HTTPClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}

	if raw := strings.TrimSpace(os.Getenv("FFLOGS_V1_MAX_CONNS_PER_HOST")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			transport.MaxConnsPerHost = v
		}
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}

func fetchV1Fights(ctx context.Context, client *http.Client, baseURL, apiKey, code string) (*v1FightsResponse, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/report/fights/" + code

	q := u.Query()
	q.Set("api_key", apiKey)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("v1 fights request failed: %s: %s", resp.Status, truncate(string(body), 256))
	}

	var parsed v1FightsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	return &parsed, nil
}

func fetchV1Events(ctx context.Context, client *http.Client, baseURL, apiKey, code string, fight v1Fight) ([]map[string]interface{}, error) {
	if fight.EndTime <= fight.StartTime {
		return nil, fmt.Errorf("invalid fight time range")
	}
	events, _, err := fetchV1EventsRange(ctx, client, baseURL, apiKey, code, fight.StartTime, fight.EndTime)
	if err != nil {
		return nil, err
	}
	return events, nil
}

func fetchV1EventsRange(ctx context.Context, client *http.Client, baseURL, apiKey, code string, start, end int64) ([]map[string]interface{}, v1EventsFetchStats, error) {
	if end <= start {
		return nil, v1EventsFetchStats{}, fmt.Errorf("invalid time range")
	}

	startedAt := time.Now()
	all := make([]map[string]interface{}, 0)
	pages := 0

	for {
		page, err := fetchV1EventsPage(ctx, client, baseURL, apiKey, code, start, end)
		if err != nil {
			return nil, v1EventsFetchStats{}, err
		}
		pages++

		all = append(all, page.Events...)
		if page.NextPageTimestamp == nil || len(page.Events) == 0 {
			break
		}
		start = *page.NextPageTimestamp
	}

	stats := v1EventsFetchStats{
		Pages:        pages,
		Events:       len(all),
		FetchElapsed: time.Since(startedAt),
	}
	return all, stats, nil
}

func fetchV1EventsPage(ctx context.Context, client *http.Client, baseURL, apiKey, code string, start, end int64) (*v1EventsResponse, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/report/events/" + code

	q := u.Query()
	q.Set("api_key", apiKey)
	q.Set("start", fmt.Sprintf("%d", start))
	q.Set("end", fmt.Sprintf("%d", end))
	if getV1TranslateEnabled() {
		q.Set("translate", "true")
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("v1 events request failed: %s: %s", resp.Status, truncate(string(body), 256))
	}

	var parsed v1EventsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	return &parsed, nil
}

func writeJSON(path string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func truncate(input string, max int) string {
	if max <= 0 || len(input) <= max {
		return input
	}
	return input[:max]
}

func toV1SavedEventsPayload(events []map[string]interface{}) v1SavedEventsPayload {
	return v1SavedEventsPayload{
		Events: events,
		Count:  len(events),
	}
}

func downloadV1Report(ctx context.Context, client *http.Client, baseURL, apiKey, rootDir string, playerID uint, code string, pending []pendingFightEntry, fightWorkers int, fightTimeout time.Duration) (int, bool, error) {
	fights, err := fetchV1Fights(ctx, client, baseURL, apiKey, code)
	if err != nil {
		return 0, false, err
	}

	if fights.Processing || fights.Fights == nil {
		log.Printf("[V1] 报告 %s 仍在处理，稍后重试", code)
		return 0, false, nil
	}
	if len(fights.Fights) == 0 {
		log.Printf("[V1] 报告 %s 无战斗数据，跳过", code)
		return 0, false, nil
	}

	reportDir := filepath.Join(rootDir, code)
	if err := os.MkdirAll(reportDir, 0755); err != nil {
		return 0, false, fmt.Errorf("create report dir failed: %v", err)
	}

	if err := writeJSON(filepath.Join(reportDir, "report_fights.json"), fights); err != nil {
		return 0, false, err
	}

	fightByID := make(map[int]v1Fight, len(fights.Fights))
	for _, fight := range fights.Fights {
		fightByID[fight.ID] = fight
	}

	if len(pending) == 0 {
		return 0, false, nil
	}

	if fightWorkers <= 0 {
		fightWorkers = 1
	}

	var allDone int32 = 1
	var successCount int64
	var activeCount int64
	totalCount := int64(len(pending))
	startedAt := time.Now()

	logProgress := func(fightID int, status string) {
		downloaded := atomic.LoadInt64(&successCount)
		active := atomic.LoadInt64(&activeCount)

		elapsedSec := time.Since(startedAt).Seconds()
		speed := 0.0
		if elapsedSec > 0 {
			speed = float64(downloaded) / elapsedSec
		}

		remaining := totalCount - downloaded
		eta := "--"
		if remaining <= 0 {
			eta = "完成"
		} else if speed > 0 {
			etaAt := time.Now().Add(time.Duration(float64(remaining)/speed) * time.Second)
			eta = etaAt.Format("15:04:05")
		}

		log.Printf("[V1] fight %s-%d %s | 已下载=%d/%d 当前并发=%d/%d 当前速度=%.2f fights/s 预计完成=%s", code, fightID, status, downloaded, totalCount, active, int64(fightWorkers), speed, eta)
	}

	jobs := make(chan pendingFightEntry)
	var wg sync.WaitGroup
	var firstErr atomic.Value
	worker := func() {
		defer wg.Done()
		for entry := range jobs {
			if ctx.Err() != nil {
				return
			}
			atomic.AddInt64(&activeCount, 1)
			func(entry pendingFightEntry) {
				defer atomic.AddInt64(&activeCount, -1)

				fight, ok := fightByID[entry.FightID]
				if !ok {
					log.Printf("[V1] 报告 %s 缺少 fight %d，跳过", code, entry.FightID)
					atomic.StoreInt32(&allDone, 0)
					logProgress(entry.FightID, "失败(报告中不存在该fight)")
					return
				}

				eventsPath := filepath.Join(reportDir, fmt.Sprintf("fight_%d_events.json", fight.ID))
				reuseByMaster := false
				if downloaded, err := isFightDownloadedByMasterID(entry.MasterID); err != nil {
					log.Printf("[V1] 检查 master 下载状态失败 %s: %v", entry.MasterID, err)
				} else {
					reuseByMaster = downloaded
				}

				if isUsableV1EventsFile(eventsPath) {
					if err := markFightDownloadedByID(entry.MappingID); err != nil {
						if firstErr.Load() == nil {
							firstErr.Store(err)
						}
						logProgress(entry.FightID, "失败(标记下载)")
						return
					}
					atomic.AddInt64(&successCount, 1)
					if reuseByMaster {
						logProgress(entry.FightID, "复用(master)")
					} else {
						logProgress(entry.FightID, "复用本地")
					}
					return
				}

				attempts := getV1RetryCount()
				var events []map[string]interface{}
				var fetchStats v1EventsFetchStats
				var err error
				fightStartedAt := time.Now()
				for attempt := 1; attempt <= attempts; attempt++ {
					fightCtx, cancelFight := context.WithTimeout(ctx, fightTimeout)
					events, fetchStats, err = fetchV1EventsRange(fightCtx, client, baseURL, apiKey, code, fight.StartTime, fight.EndTime)
					cancelFight()
					if err == nil {
						break
					}
					log.Printf("[V1] fight %s-%d 下载失败(尝试 %d/%d): %v", code, fight.ID, attempt, attempts, err)
					if attempt < attempts {
						time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
					}
				}
				if err != nil {
					log.Printf("[V1] fight %s-%d 失败，跳过", code, fight.ID)
					atomic.StoreInt32(&allDone, 0)
					logProgress(entry.FightID, "失败(下载)")
					return
				}

				writeStartedAt := time.Now()
				if err := writeJSON(eventsPath, toV1SavedEventsPayload(events)); err != nil {
					if firstErr.Load() == nil {
						firstErr.Store(err)
					}
					logProgress(entry.FightID, "失败(写文件)")
					return
				}
				writeElapsed := time.Since(writeStartedAt)

				var fileBytes int64
				if info, statErr := os.Stat(eventsPath); statErr == nil {
					fileBytes = info.Size()
				}
				if err := markFightDownloadedByID(entry.MappingID); err != nil {
					if firstErr.Load() == nil {
						firstErr.Store(err)
					}
					logProgress(entry.FightID, "失败(更新数据库)")
					return
				}

				log.Printf("[V1][PROFILE] fight %s-%d 拉取完成 | pages=%d events=%d fetch耗时=%s 写盘耗时=%s 文件大小=%.2fMB 总耗时=%s", code, fight.ID, fetchStats.Pages, fetchStats.Events, fetchStats.FetchElapsed, writeElapsed, float64(fileBytes)/(1024*1024), time.Since(fightStartedAt))

				atomic.AddInt64(&successCount, 1)
				logProgress(entry.FightID, "完成")
			}(entry)
		}
	}

	for i := 0; i < fightWorkers; i++ {
		wg.Add(1)
		go worker()
	}
	for _, entry := range pending {
		jobs <- entry
	}
	close(jobs)
	wg.Wait()

	if errVal := firstErr.Load(); errVal != nil {
		return int(successCount), false, errVal.(error)
	}

	reportDone, err := isReportDownloadedByMaster(playerID, code)
	if err != nil {
		return int(successCount), false, err
	}
	if !reportDone {
		atomic.StoreInt32(&allDone, 0)
	}

	return int(successCount), reportDone && atomic.LoadInt32(&allDone) == 1, nil
}

func markFightDownloadedByID(mappingID uint) error {
	return db.DB.Model(&models.FightSyncMap{}).
		Where("id = ?", mappingID).
		Updates(map[string]interface{}{"downloaded": true, "downloaded_at": time.Now()}).Error
}

func isFightDownloadedByMasterID(masterID string) (bool, error) {
	var count int64
	if err := db.DB.Model(&models.FightSyncMap{}).
		Where("master_id = ? AND downloaded = ?", masterID, true).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func isUsableV1EventsFile(eventsPath string) bool {
	data, err := os.ReadFile(eventsPath)
	if err != nil || len(data) == 0 {
		return false
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		return false
	}

	if eventsRaw, ok := payload["events"]; ok {
		switch v := eventsRaw.(type) {
		case []interface{}:
			return true
		case map[string]interface{}:
			_, ok := v["events"].([]interface{})
			return ok
		}
	}

	return false
}

func isReportDownloadedByMaster(playerID uint, code string) (bool, error) {
	var count int64
	if err := db.DB.Model(&models.FightSyncMap{}).
		Where("player_id = ? AND downloaded = ? AND master_id LIKE ?", playerID, false, code+"-%").
		Count(&count).Error; err != nil {
		return false, err
	}
	return count == 0, nil
}

func loadPendingFights(playerID uint) ([]pendingFightEntry, error) {
	var maps []models.FightSyncMap
	if err := db.DB.Select("id", "master_id").
		Where("player_id = ? AND downloaded = ?", playerID, false).
		Find(&maps).Error; err != nil {
		return nil, err
	}

	if len(maps) == 0 {
		return nil, nil
	}

	entries := make([]pendingFightEntry, 0, len(maps))
	for _, mapping := range maps {
		code, fightID, ok := splitMasterID(mapping.MasterID)
		if !ok {
			continue
		}
		entries = append(entries, pendingFightEntry{
			MappingID:  mapping.ID,
			MasterID:   mapping.MasterID,
			ReportCode: code,
			FightID:    fightID,
		})
	}

	return entries, nil
}

func groupPendingFights(entries []pendingFightEntry) map[string][]pendingFightEntry {
	grouped := make(map[string][]pendingFightEntry)
	for _, entry := range entries {
		grouped[entry.ReportCode] = append(grouped[entry.ReportCode], entry)
	}
	return grouped
}

func splitMasterID(masterID string) (string, int, bool) {
	idx := strings.LastIndex(masterID, "-")
	if idx <= 0 || idx >= len(masterID)-1 {
		return "", 0, false
	}
	code := masterID[:idx]
	if code == "" {
		return "", 0, false
	}
	fightID, err := strconv.Atoi(masterID[idx+1:])
	if err != nil {
		return "", 0, false
	}
	return code, fightID, true
}

// BackfillPlayerOutputPercentiles 兼容旧命名：直接通过 FFLogs 接口刷新指定玩家 output_ability。
func (s *SyncManager) BackfillPlayerOutputPercentiles(playerID uint) (int, error) {
	if playerID == 0 {
		return 0, nil
	}

	var player models.Player
	if err := db.DB.Select("id", "name", "server", "region").Where("id = ?", playerID).First(&player).Error; err != nil {
		return 0, err
	}

	region := strings.TrimSpace(player.Region)
	if region == "" {
		region = "CN"
	}
	if err := s.refreshPlayerOutputAbilityFromLogs(context.Background(), player.ID, player.Name, player.Server, region); err != nil {
		return 0, err
	}

	return 1, nil
}

// BackfillAllOutputPercentiles 兼容旧命名：批量通过 FFLogs 接口刷新所有玩家 output_ability。
// 返回处理玩家数量和成功刷新数量。
func (s *SyncManager) BackfillAllOutputPercentiles() (int, int, error) {
	var players []models.Player
	if err := db.DB.Select("id", "name", "server", "region").
		Where("name <> '' AND server <> ''").
		Find(&players).Error; err != nil {
		return 0, 0, err
	}

	success := 0
	for _, player := range players {
		region := strings.TrimSpace(player.Region)
		if region == "" {
			region = "CN"
		}
		if err := s.refreshPlayerOutputAbilityFromLogs(context.Background(), player.ID, player.Name, player.Server, region); err != nil {
			log.Printf("[WARN] 刷新玩家输出能力失败 player_id=%d name=%s: %v", player.ID, player.Name, err)
			continue
		}
		success++
	}

	return len(players), success, nil
}

func buildAllReportsIndex() (map[string]int, error) {
	rootDir := getAllReportsDir()
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return nil, fmt.Errorf("read reports dir failed: %v", err)
	}

	var fights []allReportsFightEntry
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		reportCode := entry.Name()
		reportPath := filepath.Join(rootDir, reportCode, "report_fights.json")
		data, err := os.ReadFile(reportPath)
		if err != nil {
			continue
		}

		var report v1FightsResponse
		if err := json.Unmarshal(data, &report); err != nil {
			return nil, fmt.Errorf("parse %s: %v", reportPath, err)
		}

		for _, fight := range report.Fights {
			if fight.Difficulty != 101 {
				continue
			}
			absStart := report.Start + fight.StartTime
			duration := fight.EndTime - fight.StartTime
			sourceID := fmt.Sprintf("%s-%d", report.Code, fight.ID)
			fights = append(fights, allReportsFightEntry{
				SourceID:   sourceID,
				AbsStart:   absStart,
				Duration:   duration,
				ReportCode: report.Code,
				FightID:    fight.ID,
			})
		}
	}

	if len(fights) == 0 {
		return map[string]int{}, nil
	}

	sort.Slice(fights, func(i, j int) bool {
		if fights[i].AbsStart != fights[j].AbsStart {
			return fights[i].AbsStart < fights[j].AbsStart
		}
		if fights[i].Duration != fights[j].Duration {
			return fights[i].Duration < fights[j].Duration
		}
		if fights[i].ReportCode != fights[j].ReportCode {
			return fights[i].ReportCode < fights[j].ReportCode
		}
		return fights[i].FightID < fights[j].FightID
	})

	indexMap := make(map[string]int, len(fights))
	for idx, fight := range fights {
		indexMap[fight.SourceID] = idx
	}

	return indexMap, nil
}
