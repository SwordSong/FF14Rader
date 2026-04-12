package scoring

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/user/ff14rader/internal/cluster"
	clusterserver "github.com/user/ff14rader/internal/cluster/server"
	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/models"
	"gorm.io/datatypes"
)

const (
	defaultAnalyzeURL            = "http://127.0.0.1:22026/analyze"
	defaultAnalyzeHostsConfig    = "./docs/xiva-hosts.json"
	defaultReportRoot            = "./downloads/fflogs"
	defaultFFLogsV1BaseURL       = "https://www.fflogs.com/v1"
	defaultWeightMin             = 0.01
	defaultWeightMax             = 1.0
	defaultWeightExp             = 1.0
	defaultChecklistGCDGateAlpha = 0.15
	defaultChecklistGCDGateBeta  = 3.0
	defaultAnalyzeAliveTTL       = 2 * time.Second
	defaultAnalyzeReadyTimeout   = 800 * time.Millisecond
)

type Service struct {
	apiURLs    []string
	rrCounter  uint64
	root       string
	client     *http.Client
	weightMin  float64
	weightMax  float64
	weightExpo float64
	aliveMu    sync.RWMutex
	aliveURLs  []string
	aliveAt    time.Time
}

type NoMatchedActorError struct {
	ReportCode  string
	FightID     int
	PlayerName  string
	ExpectedJob string
	Candidates  []string
}

// Error 返回错误字符串。
func (e *NoMatchedActorError) Error() string {
	if e == nil {
		return "no matched actor"
	}
	return fmt.Sprintf("no matched actor for report=%s fight=%d player=%s expected_job=%s candidates=%v", e.ReportCode, e.FightID, strings.TrimSpace(e.PlayerName), strings.TrimSpace(e.ExpectedJob), e.Candidates)
}

// IsNoMatchedActorError 判断错误是否为无匹配角色错误。
func IsNoMatchedActorError(err error) bool {
	if err == nil {
		return false
	}
	var target *NoMatchedActorError
	return errors.As(err, &target)
}

// NewServiceFromEnv 根据环境变量创建一个新的评分服务配置。
func NewServiceFromEnv() *Service {
	//这里是评分服务的初始化，包括V2解析和fight events的相关配置。
	// 解析服务地址配置时，优先级为：配置文件 > XIVA_ANALYZE_URL > XIVA_HOST+XIVA_PORT_START/COUNT > 默认值。
	apiURLs := resolveAnalyzeURLsFromEnv()
	// 报告数据根目录配置，默认为 ./downloads/fflogs。
	root := strings.TrimSpace(os.Getenv("FFLOGS_ALL_REPORTS_DIR"))
	if root == "" {
		root = defaultReportRoot
	}
	weightMin := parseEnvFloat("SCORE_WEIGHT_MIN", defaultWeightMin)
	weightMax := parseEnvFloat("SCORE_WEIGHT_MAX", defaultWeightMax)
	weightExpo := parseEnvFloat("SCORE_WEIGHT_EXP", defaultWeightExp)
	if weightMin < 0.0001 {
		weightMin = defaultWeightMin
	}
	if weightMax <= 0 {
		weightMax = defaultWeightMax
	}
	if weightMax < weightMin {
		weightMin, weightMax = weightMax, weightMin
	}
	if weightExpo <= 0 {
		weightExpo = defaultWeightExp
	}
	return &Service{
		apiURLs:    apiURLs,
		root:       root,
		client:     &http.Client{Timeout: 90 * time.Second},
		weightMin:  weightMin,
		weightMax:  weightMax,
		weightExpo: weightExpo,
	}
}

// resolveAnalyzeURLsFromEnv 解析分析URL 列表来源环境变量。
func resolveAnalyzeURLsFromEnv() []string {
	if urls := resolveAnalyzeURLsFromConfigDoc(); len(urls) > 0 {
		return urls
	}

	if raw := strings.TrimSpace(os.Getenv("XIVA_ANALYZE_URL")); raw != "" {
		if normalized, err := normalizeAnalyzeURL(raw); err == nil {
			return []string{normalized}
		}
	}

	host := strings.TrimSpace(os.Getenv("XIVA_API_HOST"))
	if host == "" {
		host = "http://127.0.0.1"
	}
	host = strings.TrimRight(host, "/")

	portStart := envInt("XIVA_PORT_START", 22026)
	portCount := envInt("XIVA_PORT_COUNT", 1)
	if portCount <= 0 {
		portCount = 1
	}

	mode := strings.ToLower(strings.TrimSpace(os.Getenv("XIVA_EXECUTION_MODE")))
	isProcessMode := mode == "process" || mode == "processes" || mode == "pool"
	isThreadsMode := mode == "thread" || mode == "threads"

	// Thread 返回thread信息。
	if isThreadsMode || (!isProcessMode && mode == "" && portCount > 1) {
		singlePort := envInt("XIVA_API_PORT", envInt("PORT", 22026))
		raw := fmt.Sprintf("%s:%d", host, singlePort)
		if normalized, err := normalizeAnalyzeURL(raw); err == nil {
			return []string{normalized}
		}
	}

	out := make([]string, 0, portCount)
	for i := 0; i < portCount; i++ {
		raw := fmt.Sprintf("%s:%d", host, portStart+i)
		normalized, err := normalizeAnalyzeURL(raw)
		if err != nil {
			continue
		}
		out = append(out, normalized)
	}
	if len(out) > 0 {
		return out
	}

	return []string{defaultAnalyzeURL}
}

type analyzeHostsConfig struct {
	Servers []analyzeHostEntry `json:"servers"`
}

type analyzeHostEntry struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Enabled *bool  `json:"enabled"`
	Weight  int    `json:"weight"`
}

// resolveAnalyzeURLsFromConfigDoc 解析分析URL 列表来源配置文档。
func resolveAnalyzeURLsFromConfigDoc() []string {
	configPath := strings.TrimSpace(os.Getenv("XIVA_HOSTS_CONFIG"))
	if configPath == "" {
		configPath = defaultAnalyzeHostsConfig
	}

	urls, err := loadAnalyzeURLsFromConfigFile(configPath)
	if err != nil {
		return nil
	}
	return urls
}

// loadAnalyzeURLsFromConfigFile 获取分析URL 列表来源配置文件。
func loadAnalyzeURLsFromConfigFile(configPath string) ([]string, error) {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return nil, fmt.Errorf("empty config path")
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	content := strings.TrimSpace(string(raw))
	if content == "" {
		return nil, fmt.Errorf("empty config file")
	}

	var root analyzeHostsConfig
	if err := json.Unmarshal(raw, &root); err == nil && len(root.Servers) > 0 {
		return buildAnalyzeURLsFromHostEntries(root.Servers), nil
	}

	var entryList []analyzeHostEntry
	if err := json.Unmarshal(raw, &entryList); err == nil && len(entryList) > 0 {
		return buildAnalyzeURLsFromHostEntries(entryList), nil
	}

	var urlList []string
	if err := json.Unmarshal(raw, &urlList); err == nil && len(urlList) > 0 {
		out := make([]string, 0, len(urlList))
		for _, candidate := range urlList {
			normalized, err := normalizeAnalyzeURL(strings.TrimSpace(candidate))
			if err != nil {
				continue
			}
			out = append(out, normalized)
		}
		if len(out) > 0 {
			return out, nil
		}
	}

	return nil, fmt.Errorf("unsupported config format")
}

// buildAnalyzeURLsFromHostEntries 构建分析URL 列表来源主机条目列表。
func buildAnalyzeURLsFromHostEntries(entries []analyzeHostEntry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Enabled != nil && !*entry.Enabled {
			continue
		}

		rawURL := strings.TrimSpace(entry.URL)
		if rawURL == "" {
			host := strings.TrimSpace(entry.Host)
			if host == "" || entry.Port <= 0 {
				continue
			}
			if !strings.Contains(host, "://") {
				host = "http://" + host
			}
			host = strings.TrimRight(host, "/")
			rawURL = fmt.Sprintf("%s:%d", host, entry.Port)
		}

		normalized, err := normalizeAnalyzeURL(rawURL)
		if err != nil {
			continue
		}

		weight := entry.Weight
		if weight <= 0 {
			weight = 1
		}
		if weight > 16 {
			weight = 16
		}
		for i := 0; i < weight; i++ {
			out = append(out, normalized)
		}
	}
	return out
}

// RecommendedWorkerCount 返回根据当前解析服务配置计算的建议并发。
func (s *Service) RecommendedWorkerCount() int {
	if live := s.recommendedWorkerCountFromHealth(); live > 0 {
		return clampRecommendedConcurrency(live)
	}
	return clampRecommendedConcurrency(s.recommendedWorkerCountFromConfig())
}

// recommendedWorkerCountFromHealth 根据健康探测结果计算建议并发数。
func (s *Service) recommendedWorkerCountFromHealth() int {
	endpoints := uniqueAnalyzeURLs(s.apiURLs)
	if len(endpoints) == 0 {
		return 0
	}

	client := &http.Client{Timeout: 1200 * time.Millisecond}
	total := 0
	for _, endpoint := range endpoints {
		c := probeAnalyzeEndpointConcurrency(client, endpoint)
		if c <= 0 {
			continue
		}
		total += c
	}
	return total
}

// recommendedWorkerCountFromConfig 根据配置估算建议并发数。
func (s *Service) recommendedWorkerCountFromConfig() int {
	slots := normalizeAnalyzeURLs(s.apiURLs)
	if len(slots) == 0 {
		return 1
	}

	perWorker := envInt("XIVA_CALL_CONCURRENCY", 1)
	if perWorker <= 0 {
		perWorker = 1
	}

	// Thread 返回thread信息。
	threadWorkers := envInt("XIVA_THREAD_POOL_SIZE", 0)
	if threadWorkers == 0 {
		threadWorkers = envInt("XIVA_PORT_COUNT", 0)
	}
	if threadWorkers > 0 {
		return threadWorkers * perWorker
	}

	localSlots := countLocalAnalyzeURLs(slots)
	remoteSlots := len(slots) - localSlots
	if localSlots < 0 {
		localSlots = 0
	}
	if remoteSlots < 0 {
		remoteSlots = 0
	}

	if localSlots > 0 {
		cpuCap := runtime.NumCPU() * 2
		if cpuCap <= 0 {
			cpuCap = 1
		}
		localRecommended := localSlots * perWorker
		if localRecommended > cpuCap {
			localRecommended = cpuCap
		}
		return localRecommended + remoteSlots*perWorker
	}

	return len(slots) * perWorker
}

// probeAnalyzeEndpointConcurrency 探测分析端点并发。
func probeAnalyzeEndpointConcurrency(client *http.Client, analyzeURL string) int {
	healthURL, err := analyzeURLToHealthURL(analyzeURL)
	if err != nil {
		return 0
	}

	req, err := http.NewRequest(http.MethodGet, healthURL, nil)
	if err != nil {
		return 0
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return 0
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0
	}

	if v := parsePositiveIntAny(payload["maxConcurrency"]); v > 0 {
		return v
	}
	if v := parsePositiveIntAny(payload["threadPoolSize"]); v > 0 {
		return v
	}

	if threadPoolRaw, ok := payload["threadPool"]; ok {
		if threadPool, ok := threadPoolRaw.(map[string]any); ok {
			if v := parsePositiveIntAny(threadPool["desiredSize"]); v > 0 {
				return v
			}
			if v := parsePositiveIntAny(threadPool["size"]); v > 0 {
				return v
			}
		}
	}

	return 1
}

// analyzeURLToHealthURL 将分析端点 URL 转换为健康检查 URL。
func analyzeURLToHealthURL(analyzeURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(analyzeURL))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid analyze url: %s", analyzeURL)
	}
	parsed.Path = "/health"
	parsed.RawQuery = ""
	return parsed.String(), nil
}

// analyzeURLToReadyURL 将分析端点 URL 转换为就绪检查 URL。
func analyzeURLToReadyURL(analyzeURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(analyzeURL))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid analyze url: %s", analyzeURL)
	}
	parsed.Path = "/ready"
	parsed.RawQuery = ""
	return parsed.String(), nil
}

// probeAnalyzeEndpointReady 探测分析端点就绪。
func probeAnalyzeEndpointReady(client *http.Client, analyzeURL string) bool {
	readyURL, err := analyzeURLToReadyURL(analyzeURL)
	if err != nil {
		return false
	}

	req, err := http.NewRequest(http.MethodGet, readyURL, nil)
	if err != nil {
		return false
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// parsePositiveIntAny 从任意值中解析正整数。
func parsePositiveIntAny(v any) int {
	switch value := v.(type) {
	case int:
		if value > 0 {
			return value
		}
	case int32:
		if value > 0 {
			return int(value)
		}
	case int64:
		if value > 0 {
			return int(value)
		}
	case float64:
		if value > 0 {
			return int(math.Floor(value))
		}
	case json.Number:
		if parsed, err := strconv.Atoi(value.String()); err == nil && parsed > 0 {
			return parsed
		}
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && parsed > 0 {
			return parsed
		}
	}
	return 0
}

// normalizeAnalyzeURLs 解析分析URL 列表。
func normalizeAnalyzeURLs(urls []string) []string {
	if len(urls) == 0 {
		return nil
	}
	out := make([]string, 0, len(urls))
	for _, raw := range urls {
		u := strings.TrimSpace(raw)
		if u == "" {
			continue
		}
		out = append(out, u)
	}
	return out
}

// uniqueAnalyzeURLs 去重分析URL 列表。
func uniqueAnalyzeURLs(urls []string) []string {
	if len(urls) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(urls))
	out := make([]string, 0, len(urls))
	for _, raw := range urls {
		u := strings.TrimSpace(raw)
		if u == "" {
			continue
		}
		if _, exists := seen[u]; exists {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

// countLocalAnalyzeURLs 返回数量本地分析URL 列表。
func countLocalAnalyzeURLs(urls []string) int {
	if len(urls) == 0 {
		return 0
	}

	localIPs := map[string]struct{}{}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet == nil || ipNet.IP == nil {
				continue
			}
			localIPs[ipNet.IP.String()] = struct{}{}
		}
	}

	count := 0
	for _, endpoint := range urls {
		u, err := url.Parse(endpoint)
		if err != nil {
			continue
		}
		hostname := strings.ToLower(strings.TrimSpace(u.Hostname()))
		if hostname == "" {
			continue
		}
		if hostname == "localhost" {
			count++
			continue
		}
		if ip := net.ParseIP(hostname); ip != nil {
			if ip.IsLoopback() {
				count++
				continue
			}
			if _, ok := localIPs[ip.String()]; ok {
				count++
				continue
			}
		}
	}
	return count
}

// clampRecommendedConcurrency 将建议并发限制在安全范围内。
func clampRecommendedConcurrency(v int) int {
	if v <= 0 {
		return 1
	}
	if v > 128 {
		return 128
	}
	return v
}

// envInt 读取整数。
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

// envBool 读取布尔值。
func envBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

type fightRow struct {
	MasterID        string
	FightID         int
	Name            string
	Kill            bool
	StartTime       int64
	EndTime         int64
	FightPercentage float64
	BossPercentage  float64
	EncounterID     int
	Difficulty      int
	Job             string
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

type analyzeRequest struct {
	Code    string        `json:"code"`
	FightID int           `json:"fightId"`
	Report  reportPayload `json:"report"`
	Events  any           `json:"events,omitempty"`
	Fetch   *analyzeFetch `json:"fetch,omitempty"`
}

type analyzeFetch struct {
	Provider  string `json:"provider,omitempty"`
	APIKey    string `json:"apiKey,omitempty"`
	BaseURL   string `json:"baseUrl,omitempty"`
	Translate *bool  `json:"translate,omitempty"`
}

type prefetchRequest struct {
	Code    string        `json:"code"`
	FightID int           `json:"fightId"`
	StartMS int64         `json:"startMs"`
	EndMS   int64         `json:"endMs"`
	Fetch   *analyzeFetch `json:"fetch,omitempty"`
}

type analyzeResponse struct {
	RequestID  string `json:"requestId"`
	DurationMs int64  `json:"durationMs"`
	Results    []struct {
		Status     string          `json:"status"`
		ReportCode string          `json:"reportCode"`
		FightID    int             `json:"fightId"`
		FightName  string          `json:"fightName"`
		StartTime  int64           `json:"startTime"`
		EndTime    int64           `json:"endTime"`
		Actors     []analyzedActor `json:"actors"`
	} `json:"results"`
}

type analyzedActor struct {
	ActorID string           `json:"actorId"`
	Name    string           `json:"name"`
	Job     string           `json:"job"`
	Modules []analyzedModule `json:"modules"`
}

type analyzedModule struct {
	Handle  string                 `json:"handle"`
	Name    string                 `json:"name"`
	Metrics map[string]interface{} `json:"metrics"`
}

type PreviewScore struct {
	ReportCode          string
	FightID             int
	FightName           string
	ActorName           string
	Job                 string
	ChecklistAbs        float64
	ChecklistConfidence float64
	ChecklistAdj        float64
	SuggestionPenalty   float64
	UtilityScore        float64
	SurvivalPenalty     float64
	JobModuleScore      float64
	BattleScore         float64
	FightWeight         float64
	WeightedBattleScore float64
	RawModuleMetrics    datatypes.JSON
}

type PreviewFightContext struct {
	ActorName       string
	Kill            bool
	FightPercentage float64
	BossPercentage  float64
}

type ChecklistRuleScore struct {
	Name    string
	Percent float64
	Score   float64
}

type ChecklistRuleComputation struct {
	Name         string
	Percent      float64
	Score        float64
	Priority     float64
	Contribution float64
}

type ChecklistComputation struct {
	Numerator   float64
	Denominator float64
	RawScore    float64
	Rules       []ChecklistRuleComputation
}

type ChecklistGCDGate struct {
	Found           bool
	CoveragePercent float64
	Coverage01      float64
	Alpha           float64
	Beta            float64
	Factor          float64
}

// BuildChecklistRuleScoresFromRaw 构建清单规则评分结果来源原始数据。
func BuildChecklistRuleScoresFromRaw(raw datatypes.JSON) []ChecklistRuleScore {
	comp := BuildChecklistComputationFromRaw(raw)
	if comp == nil || len(comp.Rules) == 0 {
		return nil
	}
	out := make([]ChecklistRuleScore, 0, len(comp.Rules))
	for _, rule := range comp.Rules {
		out = append(out, ChecklistRuleScore{
			Name:    rule.Name,
			Percent: rule.Percent,
			Score:   rule.Score,
		})
	}
	return out
}

// BuildChecklistComputationFromRaw 从原始数据构建清单计算结果。
func BuildChecklistComputationFromRaw(raw datatypes.JSON) *ChecklistComputation {
	if len(raw) == 0 {
		return nil
	}

	var moduleMap map[string]map[string]interface{}
	if err := json.Unmarshal(raw, &moduleMap); err == nil {
		return buildChecklistComputation(moduleMap["checklist"])
	}

	var generic map[string]interface{}
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil
	}
	checklist, _ := generic["checklist"].(map[string]interface{})
	return buildChecklistComputation(checklist)
}

// BuildChecklistGCDGateFromRaw 从原始数据构建清单 GCD 门控信息。
func BuildChecklistGCDGateFromRaw(raw datatypes.JSON) *ChecklistGCDGate {
	if len(raw) == 0 {
		return defaultChecklistGCDGate()
	}

	var moduleMap map[string]map[string]interface{}
	if err := json.Unmarshal(raw, &moduleMap); err == nil {
		return buildChecklistGCDGate(moduleMap["checklist"])
	}

	var generic map[string]interface{}
	if err := json.Unmarshal(raw, &generic); err != nil {
		return defaultChecklistGCDGate()
	}
	checklist, _ := generic["checklist"].(map[string]interface{})
	return buildChecklistGCDGate(checklist)
}

// ScoreFight 返回评分战斗信息。
func (s *Service) ScoreFight(ctx context.Context, playerID uint, reportCode string, fightID int) error {
	_, err := s.ScoreFightWithEndpoint(ctx, playerID, reportCode, fightID)
	return err
}

// ScoreFightWithEndpoint 返回评分战斗端点信息。
func (s *Service) ScoreFightWithEndpoint(ctx context.Context, playerID uint, reportCode string, fightID int) (string, error) {
	report, err := loadReportRow(playerID, reportCode)
	if err != nil {
		return "", err
	}
	fight, err := loadFightRow(playerID, reportCode, fightID)
	if err != nil {
		return "", err
	}

	masterData, err := extractMasterData(report.ReportMetadata)
	if err != nil {
		return "", fmt.Errorf("parse report metadata failed: %v", err)
	}

	payload := analyzeRequest{
		Code:    reportCode,
		FightID: fightID,
		Report: reportPayload{
			Title:      report.Title,
			StartTime:  report.StartTime,
			EndTime:    report.EndTime,
			Fights:     []fightJSON{buildFightJSON(fight)},
			MasterData: masterData,
		},
	}

	useRemoteFetch := shouldUseRemoteFetchTransport()
	if useRemoteFetch {
		if fetchCfg, fetchErr := resolveRemoteFetchConfig(); fetchErr == nil {
			payload.Fetch = fetchCfg
		}
	}

	if payload.Fetch == nil {
		events, err := readEvents(filepath.Join(s.root, reportCode, fmt.Sprintf("fight_%d_events.json", fightID)))
		if err != nil {
			return "", fmt.Errorf("read events failed: %v", err)
		}
		payload.Events = events
	}

	resp, rawBody, endpoint, err := s.postAnalyze(ctx, payload)
	if err != nil && shouldRetryAnalyzePending(err) {
		for attempt := 1; attempt <= 4; attempt++ {
			if waitErr := waitRetry(ctx, attempt); waitErr != nil {
				break
			}
			resp, rawBody, endpoint, err = s.postAnalyze(ctx, payload)
			if err == nil || !shouldRetryAnalyzePending(err) {
				break
			}
		}
	}
	analyzeHost := analyzeEndpointHost(endpoint)
	if err != nil && payload.Fetch != nil && shouldFallbackAnalyzeInline(err) {
		events, readErr := readEvents(filepath.Join(s.root, reportCode, fmt.Sprintf("fight_%d_events.json", fightID)))
		if readErr == nil {
			payload.Events = events
			payload.Fetch = nil
			resp, rawBody, endpoint, err = s.postAnalyze(ctx, payload)
			analyzeHost = analyzeEndpointHost(endpoint)
		}
	}
	if err != nil {
		return analyzeHost, err
	}

	if err := os.WriteFile(filepath.Join(s.root, reportCode, fmt.Sprintf("fight_%d_analysis.json", fightID)), rawBody, 0644); err != nil {
		return analyzeHost, fmt.Errorf("write analysis output failed: %v", err)
	}

	playerName := loadPlayerName(playerID)
	allActors := make([]analyzedActor, 0)
	availableActors := make([]string, 0)
	for _, result := range resp.Results {
		if result.Status != "ok" {
			continue
		}
		for _, actor := range result.Actors {
			allActors = append(allActors, actor)
			availableActors = append(availableActors, fmt.Sprintf("%s|%s|%s", strings.TrimSpace(actor.ActorID), strings.TrimSpace(actor.Name), canonicalJobKey(actor.Job)))
		}
	}

	selectedActor, ok := selectScoringActor(playerName, fight.Job, masterData, allActors)
	if !ok {
		return analyzeHost, &NoMatchedActorError{
			ReportCode:  reportCode,
			FightID:     fightID,
			PlayerName:  playerName,
			ExpectedJob: fight.Job,
			Candidates:  availableActors,
		}
	}

	score, err := s.buildScoreRow(playerID, reportCode, fight, selectedActor)
	if err != nil {
		return analyzeHost, err
	}
	if err := updateFightSyncScore(score); err != nil {
		return analyzeHost, err
	}
	if err := refreshPlayerBattleAbility(playerID); err != nil {
		return analyzeHost, fmt.Errorf("refresh player battle ability failed: %v", err)
	}
	if err := refreshPlayerTeamContribution(playerID); err != nil {
		return analyzeHost, fmt.Errorf("refresh player team contribution failed: %v", err)
	}
	if err := refreshPlayerStabilityScore(playerID); err != nil {
		return analyzeHost, fmt.Errorf("refresh player stability score failed: %v", err)
	}
	if err := refreshPlayerProgressionSpeed(playerID); err != nil {
		return analyzeHost, fmt.Errorf("refresh player progression speed failed: %v", err)
	}
	if err := refreshPlayerPotentialScore(playerID); err != nil {
		return analyzeHost, fmt.Errorf("refresh player potential score failed: %v", err)
	}
	return analyzeHost, nil
}

// RefreshPlayerTeamAndStability 回填单个玩家的团队贡献、稳定度与潜力值。
func (s *Service) RefreshPlayerTeamAndStability(playerID uint) error {
	if playerID == 0 {
		return nil
	}
	if err := refreshPlayerTeamContribution(playerID); err != nil {
		return fmt.Errorf("refresh player team contribution failed: %v", err)
	}
	if err := refreshPlayerStabilityScore(playerID); err != nil {
		return fmt.Errorf("refresh player stability score failed: %v", err)
	}
	if err := refreshPlayerPotentialScore(playerID); err != nil {
		return fmt.Errorf("refresh player potential score failed: %v", err)
	}
	return nil
}

// RefreshAllPlayersTeamAndStability 回填所有已评分玩家的团队贡献与稳定度，返回回填玩家数。
func (s *Service) RefreshAllPlayersTeamAndStability() (int, error) {
	var playerIDs []uint
	if err := db.DB.Table("fight_sync_maps").
		Distinct("player_id").
		Where("player_id > 0 AND scored_at IS NOT NULL").
		Pluck("player_id", &playerIDs).Error; err != nil {
		return 0, err
	}

	for _, playerID := range playerIDs {
		if err := s.RefreshPlayerTeamAndStability(playerID); err != nil {
			return 0, fmt.Errorf("player_id=%d: %w", playerID, err)
		}
	}

	return len(playerIDs), nil
}

// PreviewScoreFromAnalysisFile 从分析文件预览评分结果。
func (s *Service) PreviewScoreFromAnalysisFile(analysisFile, actorName string) (*PreviewScore, error) {
	return s.PreviewScoreFromAnalysisFileWithContext(analysisFile, PreviewFightContext{ActorName: actorName})
}

// PreviewScoreFromAnalysisFileWithContext 从本地分析结果文件预览评分，支持提供战斗上下文（如击杀、战斗百分比等）以获得更准确的评分。
func (s *Service) PreviewScoreFromAnalysisFileWithContext(analysisFile string, ctx PreviewFightContext) (*PreviewScore, error) {
	data, err := os.ReadFile(analysisFile)
	if err != nil {
		return nil, fmt.Errorf("read analysis file failed: %v", err)
	}

	var resp analyzeResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse analysis json failed: %v", err)
	}

	allActors := make([]analyzedActor, 0)
	for _, result := range resp.Results {
		if result.Status != "ok" {
			continue
		}
		allActors = append(allActors, result.Actors...)

		actor, ok := pickActor(result.Actors, ctx.ActorName)
		if !ok {
			continue
		}

		moduleMap := make(map[string]map[string]interface{}, len(actor.Modules))
		for _, module := range actor.Modules {
			moduleMap[strings.ToLower(module.Handle)] = module.Metrics
		}

		checkRaw, conf := computeChecklist(moduleMap["checklist"])
		checkAbs := computeChecklistAbs(checkRaw, conf, moduleMap["checklist"])
		// File 返回文件信息。
		checkAdj := conf*checkAbs + (1-conf)*70

		suggestionPenalty := computeSuggestionPenalty(moduleMap["suggestions"])
		suggestionScore := clamp100(100 - suggestionPenalty)
		utilityScore := computeUtilityScore(moduleMap)
		survivalPenalty := 0.0
		survivalScore := 100.0
		jobModuleScore := computeJobModuleScore(moduleMap)

		useFightContext := ctx.Kill || ctx.FightPercentage > 0 || ctx.BossPercentage > 0
		if useFightContext {
			survivalPenalty = computeSurvivalPenalty(fightRow{
				Kill:            ctx.Kill,
				FightPercentage: ctx.FightPercentage,
				BossPercentage:  ctx.BossPercentage,
			})
			survivalScore = clamp100(100 - survivalPenalty)
		}

		normalizedJob := canonicalJobKey(actor.Job)
		if normalizedJob == "" {
			normalizedJob = strings.ToUpper(strings.TrimSpace(actor.Job))
		}

		role := roleFromJob(normalizedJob)
		battleScore := combineBattleScore(role, checkAdj, suggestionScore, utilityScore, survivalScore, jobModuleScore)
		weight := 1.0
		if useFightContext {
			weight = fightWeight(ctx.Kill, ctx.FightPercentage, ctx.BossPercentage, s.weightMin, s.weightMax, s.weightExpo)
		}

		raw, err := json.Marshal(moduleMap)
		if err != nil {
			return nil, fmt.Errorf("marshal raw module metrics failed: %v", err)
		}

		return &PreviewScore{
			ReportCode:          result.ReportCode,
			FightID:             result.FightID,
			FightName:           result.FightName,
			ActorName:           actor.Name,
			Job:                 normalizedJob,
			ChecklistAbs:        checkAbs,
			ChecklistConfidence: conf,
			ChecklistAdj:        checkAdj,
			SuggestionPenalty:   suggestionPenalty,
			UtilityScore:        utilityScore,
			SurvivalPenalty:     survivalPenalty,
			JobModuleScore:      jobModuleScore,
			BattleScore:         battleScore,
			FightWeight:         weight,
			WeightedBattleScore: battleScore * weight,
			RawModuleMetrics:    datatypes.JSON(raw),
		}, nil
	}

	names := strings.Join(uniqueActorNames(allActors), ", ")
	if ctx.ActorName == "" {
		return nil, fmt.Errorf("actor not resolved, please provide --actor-name (available: %s)", names)
	}
	return nil, fmt.Errorf("actor not found: %s (available: %s)", ctx.ActorName, names)
}

type fightScoreUpdate struct {
	PlayerID            uint
	MasterID            string
	ReportCode          string
	FightID             int
	Job                 string
	ActorName           string
	ChecklistAbs        float64
	ChecklistConfidence float64
	ChecklistAdj        float64
	SuggestionPenalty   float64
	UtilityScore        float64
	SurvivalPenalty     float64
	JobModuleScore      float64
	BattleScore         float64
	FightWeight         float64
	WeightedBattleScore float64
	RawModuleMetrics    datatypes.JSON
	ScoredAt            time.Time
}

// buildScoreRow 构建评分记录。
func (s *Service) buildScoreRow(playerID uint, reportCode string, fight fightRow, actor analyzedActor) (*fightScoreUpdate, error) {
	moduleMap := make(map[string]map[string]interface{}, len(actor.Modules))
	for _, module := range actor.Modules {
		moduleMap[strings.ToLower(module.Handle)] = module.Metrics
	}

	normalizedJob := canonicalJobKey(actor.Job)
	if normalizedJob == "" {
		normalizedJob = strings.ToUpper(strings.TrimSpace(actor.Job))
	}

	checkRaw, conf := computeChecklist(moduleMap["checklist"])
	checkAbs := computeChecklistAbs(checkRaw, conf, moduleMap["checklist"])
	checkBase := loadJobBaseline(playerID, normalizedJob)
	checkAdj := conf*checkAbs + (1-conf)*checkBase

	suggestionPenalty := computeSuggestionPenalty(moduleMap["suggestions"])
	suggestionScore := clamp100(100 - suggestionPenalty)
	utilityScore := computeUtilityScore(moduleMap)
	survivalPenalty := computeSurvivalPenalty(fight)
	survivalScore := clamp100(100 - survivalPenalty)
	jobModuleScore := computeJobModuleScore(moduleMap)

	role := roleFromJob(normalizedJob)
	battleScore := combineBattleScore(role, checkAdj, suggestionScore, utilityScore, survivalScore, jobModuleScore)
	weight := fightWeight(fight.Kill, fight.FightPercentage, fight.BossPercentage, s.weightMin, s.weightMax, s.weightExpo)
	weighted := battleScore * weight

	raw, err := json.Marshal(moduleMap)
	if err != nil {
		return nil, fmt.Errorf("marshal raw module metrics failed: %v", err)
	}

	return &fightScoreUpdate{
		PlayerID:            playerID,
		MasterID:            fight.MasterID,
		ReportCode:          reportCode,
		FightID:             fight.FightID,
		Job:                 normalizedJob,
		ActorName:           actor.Name,
		ChecklistAbs:        checkAbs,
		ChecklistConfidence: conf,
		ChecklistAdj:        checkAdj,
		SuggestionPenalty:   suggestionPenalty,
		UtilityScore:        utilityScore,
		SurvivalPenalty:     survivalPenalty,
		JobModuleScore:      jobModuleScore,
		BattleScore:         battleScore,
		FightWeight:         weight,
		WeightedBattleScore: weighted,
		RawModuleMetrics:    datatypes.JSON(raw),
		ScoredAt:            time.Now(),
	}, nil
}

// selectScoringActor 从候选角色中选择评分目标角色。
func selectScoringActor(playerName, expectedJob string, masterData any, actors []analyzedActor) (analyzedActor, bool) {
	if len(actors) == 0 {
		return analyzedActor{}, false
	}

	playerName = strings.TrimSpace(playerName)
	targetActorIDs := extractMasterActorIDs(masterData, playerName)

	if playerName != "" {
		for _, actor := range actors {
			if actorNameEquals(playerName, actor.Name) {
				return actor, true
			}
		}

		for _, actor := range actors {
			if _, ok := targetActorIDs[strings.TrimSpace(actor.ActorID)]; ok {
				return actor, true
			}
		}

		return analyzedActor{}, false
	}

	jobKey := canonicalJobKey(expectedJob)
	if jobKey != "" {
		matches := make([]analyzedActor, 0, 1)
		for _, actor := range actors {
			if canonicalJobKey(actor.Job) == jobKey {
				matches = append(matches, actor)
			}
		}
		if len(matches) == 1 {
			return matches[0], true
		}
	}

	if len(actors) == 1 {
		return actors[0], true
	}

	return analyzedActor{}, false
}

// extractMasterActorIDs 提取主节点角色ID 列表。
func extractMasterActorIDs(masterData any, playerName string) map[string]struct{} {
	playerName = strings.TrimSpace(playerName)
	if playerName == "" {
		return map[string]struct{}{}
	}

	payload, ok := masterData.(map[string]any)
	if !ok {
		return map[string]struct{}{}
	}

	actorsRaw, ok := payload["actors"].([]any)
	if !ok {
		return map[string]struct{}{}
	}

	ids := make(map[string]struct{})
	for _, actorRaw := range actorsRaw {
		actor, ok := actorRaw.(map[string]any)
		if !ok {
			continue
		}

		name, _ := actor["name"].(string)
		if !actorNameEquals(playerName, name) {
			continue
		}

		if actorID, ok := toActorIDString(actor["id"]); ok {
			ids[actorID] = struct{}{}
		}
	}

	return ids
}

// toActorIDString 将角色 ID 转换为字符串表示。
func toActorIDString(v any) (string, bool) {
	switch val := v.(type) {
	case float64:
		return strconv.Itoa(int(val)), true
	case float32:
		return strconv.Itoa(int(val)), true
	case int:
		return strconv.Itoa(val), true
	case int64:
		return strconv.FormatInt(val, 10), true
	case int32:
		return strconv.FormatInt(int64(val), 10), true
	case json.Number:
		trimmed := strings.TrimSpace(val.String())
		if trimmed == "" {
			return "", false
		}
		return trimmed, true
	case string:
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			return "", false
		}
		return trimmed, true
	default:
		return "", false
	}
}

// actorNameEquals 判断两个角色名称是否等价。
func actorNameEquals(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if strings.EqualFold(a, b) {
		return true
	}
	return normalizeNameKey(a) == normalizeNameKey(b)
}

// normalizeNameKey 解析名称键。
func normalizeNameKey(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	replacer := strings.NewReplacer(
		" ", "",
		"　", "",
		"·", "",
		"・", "",
		"'", "",
		"’", "",
		"`", "",
		"-", "",
		"_", "",
	)
	return replacer.Replace(v)
}

// pickActor 选择角色。
func pickActor(actors []analyzedActor, actorName string) (analyzedActor, bool) {
	if actorName != "" {
		for _, actor := range actors {
			if strings.EqualFold(actor.Name, actorName) {
				return actor, true
			}
		}
		return analyzedActor{}, false
	}
	if len(actors) == 1 {
		return actors[0], true
	}
	return analyzedActor{}, false
}

// uniqueActorNames 去重角色名称列表。
func uniqueActorNames(actors []analyzedActor) []string {
	if len(actors) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(actors))
	names := make([]string, 0, len(actors))
	for _, actor := range actors {
		name := strings.TrimSpace(actor.Name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// postAnalyze 发送分析。
func (s *Service) postAnalyze(ctx context.Context, payload analyzeRequest) (*analyzeResponse, []byte, string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, "", err
	}

	candidates, pickErr := s.pickAliveAnalyzeURLs()
	if pickErr != nil {
		return nil, nil, "", pickErr
	}

	const maxAttempts = 5
	var lastErr error
	lastEndpoint := ""

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		endpoint := s.pickAnalyzeURLForPayloadFromURLs(payload, attempt, candidates)
		lastEndpoint = endpoint
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if reqErr != nil {
			return nil, nil, endpoint, reqErr
		}
		req.Header.Set("Content-Type", "application/json")

		resp, doErr := s.client.Do(req)
		if doErr != nil {
			lastErr = doErr
			if attempt < maxAttempts && isTransientAnalyzeError(doErr) {
				if err := waitRetry(ctx, attempt); err != nil {
					return nil, nil, endpoint, err
				}
				continue
			}
			return nil, nil, endpoint, doErr
		}

		respBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if attempt < maxAttempts && isTransientAnalyzeError(readErr) {
				if err := waitRetry(ctx, attempt); err != nil {
					return nil, nil, endpoint, err
				}
				continue
			}
			return nil, nil, endpoint, readErr
		}

		if resp.StatusCode >= 400 {
			err = fmt.Errorf("analyze failed: status=%d body=%s", resp.StatusCode, string(respBody))
			lastErr = err
			if attempt < maxAttempts && (resp.StatusCode == 429 || resp.StatusCode >= 500) {
				if err := waitRetry(ctx, attempt); err != nil {
					return nil, nil, endpoint, err
				}
				continue
			}
			return nil, nil, endpoint, err
		}

		var parsed analyzeResponse
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return nil, nil, endpoint, err
		}
		return &parsed, respBody, endpoint, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("analyze failed after retries")
	}
	return nil, nil, lastEndpoint, lastErr
}

// PrefetchFightEvents 返回预取战斗事件列表。
func (s *Service) PrefetchFightEvents(ctx context.Context, reportCode string, fightID int, startMS, endMS int64) (string, error) {
	if !shouldUseRemoteFetchTransport() {
		return "", nil
	}
	if strings.TrimSpace(reportCode) == "" || fightID <= 0 {
		return "", fmt.Errorf("prefetch requires valid reportCode and fightID")
	}
	if endMS <= startMS {
		return "", fmt.Errorf("prefetch requires valid start/end range")
	}

	fetchCfg, err := resolveRemoteFetchConfig()
	if err != nil {
		return "", err
	}

	payload := prefetchRequest{
		Code:    strings.TrimSpace(reportCode),
		FightID: fightID,
		StartMS: startMS,
		EndMS:   endMS,
		Fetch:   fetchCfg,
	}

	endpoint, err := s.postPrefetch(ctx, payload)
	return analyzeEndpointHost(endpoint), err
}

// postPrefetch 发送预取。
func (s *Service) postPrefetch(ctx context.Context, payload prefetchRequest) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	candidates, pickErr := s.pickAliveAnalyzeURLs()
	if pickErr != nil {
		return "", pickErr
	}

	const maxAttempts = 4
	var lastErr error
	lastEndpoint := ""

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		endpoint := s.pickAnalyzeURLForPayloadFromURLs(analyzeRequest{Code: payload.Code, FightID: payload.FightID}, attempt, candidates)
		prefetchURL := analyzeURLToPrefetchURL(endpoint)
		lastEndpoint = prefetchURL

		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, prefetchURL, bytes.NewReader(body))
		if reqErr != nil {
			return prefetchURL, reqErr
		}
		req.Header.Set("Content-Type", "application/json")

		resp, doErr := s.client.Do(req)
		if doErr != nil {
			lastErr = doErr
			if attempt < maxAttempts && isTransientAnalyzeError(doErr) {
				if err := waitRetry(ctx, attempt); err != nil {
					return prefetchURL, err
				}
				continue
			}
			return prefetchURL, doErr
		}

		respBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if attempt < maxAttempts && isTransientAnalyzeError(readErr) {
				if err := waitRetry(ctx, attempt); err != nil {
					return prefetchURL, err
				}
				continue
			}
			return prefetchURL, readErr
		}

		if resp.StatusCode >= 400 {
			err = fmt.Errorf("prefetch failed: status=%d body=%s", resp.StatusCode, string(respBody))
			lastErr = err
			if attempt < maxAttempts && (resp.StatusCode == 429 || resp.StatusCode >= 500) {
				if err := waitRetry(ctx, attempt); err != nil {
					return prefetchURL, err
				}
				continue
			}
			return prefetchURL, err
		}

		return prefetchURL, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("prefetch failed after retries")
	}
	return lastEndpoint, lastErr
}

// analyzeURLToPrefetchURL 将分析端点 URL 转换为预取端点 URL。
func analyzeURLToPrefetchURL(endpoint string) string {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return ""
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		if strings.HasSuffix(trimmed, "/analyze") {
			return strings.TrimSuffix(trimmed, "/analyze") + "/prefetch"
		}
		return strings.TrimRight(trimmed, "/") + "/prefetch"
	}

	u.Path = "/prefetch"
	return u.String()
}

// pickAliveAnalyzeURLs 选择可用分析URL 列表。
func (s *Service) pickAliveAnalyzeURLs() ([]string, error) {
	now := time.Now()

	s.aliveMu.RLock()
	if len(s.aliveURLs) > 0 && now.Sub(s.aliveAt) < defaultAnalyzeAliveTTL {
		cached := append([]string(nil), s.aliveURLs...)
		s.aliveMu.RUnlock()
		return cached, nil
	}
	s.aliveMu.RUnlock()

	endpoints := uniqueAnalyzeURLs(s.apiURLs)
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("no analyze endpoints configured")
	}

	probeClient := &http.Client{Timeout: defaultAnalyzeReadyTimeout}
	aliveSet := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		if probeAnalyzeEndpointReady(probeClient, endpoint) {
			aliveSet[endpoint] = struct{}{}
		}
	}

	aliveWeighted := make([]string, 0, len(s.apiURLs))
	for _, endpoint := range s.apiURLs {
		normalized := strings.TrimSpace(endpoint)
		if normalized == "" {
			continue
		}
		if _, ok := aliveSet[normalized]; ok {
			aliveWeighted = append(aliveWeighted, normalized)
		}
	}

	if len(aliveWeighted) == 0 {
		return nil, fmt.Errorf("no alive analyze endpoint (all /ready probes failed)")
	}

	s.aliveMu.Lock()
	s.aliveURLs = append([]string(nil), aliveWeighted...)
	s.aliveAt = now
	s.aliveMu.Unlock()

	return aliveWeighted, nil
}

// CandidateAnalyzeHosts 返回候选分析主机列表。
func (s *Service) CandidateAnalyzeHosts() []string {
	urls := s.apiURLs
	if alive, err := s.pickAliveAnalyzeURLs(); err == nil && len(alive) > 0 {
		urls = alive
	}

	seen := make(map[string]struct{}, len(urls))
	out := make([]string, 0, len(urls))
	for _, endpoint := range urls {
		h := cluster.NormalizeHost(analyzeEndpointHost(endpoint))
		if h == "" {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// pickAnalyzeURLForPayload 为载荷选择分析服务 URL。
func (s *Service) pickAnalyzeURLForPayload(payload analyzeRequest, attempt int) string {
	return s.pickAnalyzeURLForPayloadFromURLs(payload, attempt, s.apiURLs)
}

// pickAnalyzeURLForPayloadFromURLs 从候选 URL 集合中为载荷选择分析地址。
func (s *Service) pickAnalyzeURLForPayloadFromURLs(payload analyzeRequest, attempt int, urls []string) string {
	if len(urls) == 0 {
		return defaultAnalyzeURL
	}
	if attempt < 1 {
		attempt = 1
	}

	if code := strings.TrimSpace(payload.Code); code != "" {
		if mappedHost, ok := clusterserver.GlobalReportHostRegistry().ResolveHost(code); ok {
			preferred := make([]string, 0, len(urls))
			for _, endpoint := range urls {
				if hostMatches(analyzeEndpointHost(endpoint), mappedHost) {
					preferred = append(preferred, endpoint)
				}
			}
			if len(preferred) > 0 {
				if payload.FightID > 0 {
					h := fnv.New32a()
					_, _ = h.Write([]byte(code))
					_, _ = h.Write([]byte("#"))
					_, _ = h.Write([]byte(strconv.Itoa(payload.FightID)))
					base := int(h.Sum32() % uint32(len(preferred)))
					idx := (base + (attempt - 1)) % len(preferred)
					return preferred[idx]
				}

				idx := (attempt - 1) % len(preferred)
				return preferred[idx]
			}
		}
	}

	if shouldUseRemoteFetchTransport() && strings.TrimSpace(payload.Code) != "" && payload.FightID > 0 {
		h := fnv.New32a()
		_, _ = h.Write([]byte(strings.TrimSpace(payload.Code)))
		_, _ = h.Write([]byte("#"))
		_, _ = h.Write([]byte(strconv.Itoa(payload.FightID)))
		base := int(h.Sum32() % uint32(len(urls)))
		idx := (base + (attempt - 1)) % len(urls)
		return urls[idx]
	}

	idx := atomic.AddUint64(&s.rrCounter, 1)
	base := int((idx - 1) % uint64(len(urls)))
	final := (base + (attempt - 1)) % len(urls)
	return urls[final]
}

// hostMatches 判断主机匹配是否满足条件。
func hostMatches(a, b string) bool {
	ha := cluster.NormalizeHost(a)
	hb := cluster.NormalizeHost(b)
	return ha != "" && hb != "" && ha == hb
}

// analyzeEndpointHost 返回分析端点主机信息。
func analyzeEndpointHost(endpoint string) string {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return ""
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return trimmed
	}
	if u.Host != "" {
		return u.Host
	}
	return trimmed
}

// pickAnalyzeURL 选择分析URL。
func (s *Service) pickAnalyzeURL() string {
	if len(s.apiURLs) == 0 {
		return defaultAnalyzeURL
	}
	idx := atomic.AddUint64(&s.rrCounter, 1)
	return s.apiURLs[(idx-1)%uint64(len(s.apiURLs))]
}

// normalizeAnalyzeURL 解析分析URL。
func normalizeAnalyzeURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/analyze"
	}
	return u.String(), nil
}

// computeChecklist 计算清单。
func computeChecklist(metrics map[string]interface{}) (raw float64, confidence float64) {
	if len(metrics) == 0 {
		return 0, 0
	}
	evals := evaluateChecklistRules(metrics)
	if len(evals) == 0 {
		avg := numberFromUnknown(metrics["averagePercent"])
		if avg < 0 {
			return 0, 0
		}
		return clamp100(avg), 0.6
	}

	totalRuleScore := 0.0
	totalRuleWeight := 0.0
	observedWeight := 0.0
	expectedWeight := 0.0

	for _, eval := range evals {
		totalRuleScore += eval.score01 * eval.priority
		totalRuleWeight += eval.priority
		observedWeight += eval.observedWeight
		expectedWeight += eval.expectedWeight
	}

	if totalRuleWeight <= 0 {
		avg := numberFromUnknown(metrics["averagePercent"])
		if avg < 0 {
			return 0, 0
		}
		return clamp100(avg), 0.6
	}
	raw = clamp100((totalRuleScore / totalRuleWeight) * 100)
	if expectedWeight <= 0 {
		confidence = 0.6
	} else {
		confidence = clamp01(observedWeight / expectedWeight)
	}
	return raw, confidence
}

// computeChecklistAbs 计算清单绝对值。
func computeChecklistAbs(raw, confidence float64, metrics map[string]interface{}) float64 {
	confidenceFactor := 0.7 + 0.3*clamp01(confidence)
	gate := buildChecklistGCDGate(metrics)
	gateFactor := 1.0
	if gate != nil {
		gateFactor = gate.Factor
	}
	return clamp100(raw * confidenceFactor * gateFactor)
}

type checklistRuleEval struct {
	name           string
	percent        float64
	score01        float64
	priority       float64
	observedWeight float64
	expectedWeight float64
}

// buildChecklistRuleScores 构建清单规则评分结果。
func buildChecklistRuleScores(metrics map[string]interface{}) []ChecklistRuleScore {
	comp := buildChecklistComputation(metrics)
	if comp == nil || len(comp.Rules) == 0 {
		return nil
	}
	out := make([]ChecklistRuleScore, 0, len(comp.Rules))
	for _, rule := range comp.Rules {
		out = append(out, ChecklistRuleScore{
			Name:    rule.Name,
			Percent: rule.Percent,
			Score:   rule.Score,
		})
	}
	return out
}

// buildChecklistComputation 构建清单计算明细。
func buildChecklistComputation(metrics map[string]interface{}) *ChecklistComputation {
	evals := evaluateChecklistRules(metrics)
	if len(evals) == 0 {
		return nil
	}

	numerator := 0.0
	denominator := 0.0
	rules := make([]ChecklistRuleComputation, 0, len(evals))
	for _, eval := range evals {
		contribution := eval.score01 * eval.priority
		numerator += contribution
		denominator += eval.priority
		rules = append(rules, ChecklistRuleComputation{
			Name:         eval.name,
			Percent:      clamp100(eval.percent),
			Score:        clamp100(eval.score01 * 100),
			Priority:     eval.priority,
			Contribution: contribution,
		})
	}

	rawScore := 0.0
	if denominator > 0 {
		rawScore = clamp100((numerator / denominator) * 100)
	}

	return &ChecklistComputation{
		Numerator:   numerator,
		Denominator: denominator,
		RawScore:    rawScore,
		Rules:       rules,
	}
}

// buildChecklistGCDGate 构建清单 GCD 门控数据。
func buildChecklistGCDGate(metrics map[string]interface{}) *ChecklistGCDGate {
	base := defaultChecklistGCDGate()
	percent, ok := extractChecklistGCDCoveragePercent(metrics)
	if !ok {
		return base
	}

	coverage01 := clamp01(percent / 100)
	factor := checklistGCDGateFactor(coverage01)
	return &ChecklistGCDGate{
		Found:           true,
		CoveragePercent: clamp100(percent),
		Coverage01:      coverage01,
		Alpha:           defaultChecklistGCDGateAlpha,
		Beta:            defaultChecklistGCDGateBeta,
		Factor:          factor,
	}
}

// defaultChecklistGCDGate 返回默认清单 GCD 门控配置。
func defaultChecklistGCDGate() *ChecklistGCDGate {
	return &ChecklistGCDGate{
		Found:  false,
		Alpha:  defaultChecklistGCDGateAlpha,
		Beta:   defaultChecklistGCDGateBeta,
		Factor: 1,
	}
}

// checklistGCDGateFactor 计算清单 GCD 门控系数。
func checklistGCDGateFactor(coverage01 float64) float64 {
	g := clamp01(coverage01)
	return defaultChecklistGCDGateAlpha + (1-defaultChecklistGCDGateAlpha)*math.Pow(g, defaultChecklistGCDGateBeta)
}

// extractChecklistGCDCoveragePercent 提取清单 GCD 覆盖率百分比。
func extractChecklistGCDCoveragePercent(metrics map[string]interface{}) (float64, bool) {
	if len(metrics) == 0 {
		return 0, false
	}
	rulesRaw, ok := metrics["rules"].([]interface{})
	if !ok || len(rulesRaw) == 0 {
		return 0, false
	}

	for _, ruleAny := range rulesRaw {
		ruleMap, ok := ruleAny.(map[string]interface{})
		if !ok {
			continue
		}
		ruleName := strings.ToLower(textFromUnknown(ruleMap["name"]))
		reqsRaw, _ := ruleMap["requirements"].([]interface{})
		for _, reqAny := range reqsRaw {
			reqMap, ok := reqAny.(map[string]interface{})
			if !ok {
				continue
			}
			reqName := strings.ToLower(textFromUnknown(reqMap["name"]))
			if !containsAny(reqName, "gcd覆盖率", "gcd uptime", "global cooldown", "gcd") {
				continue
			}
			percent := numberFromUnknown(reqMap["percent"])
			if percent >= 0 {
				return percent, true
			}
		}

		if containsAny(ruleName, "保持使用技能", "gcd", "global cooldown") {
			percent := numberFromUnknown(ruleMap["percent"])
			if percent >= 0 {
				return percent, true
			}
		}
	}

	return 0, false
}

// evaluateChecklistRules 评估清单规则并产出规则评分。
func evaluateChecklistRules(metrics map[string]interface{}) []checklistRuleEval {
	rulesRaw, ok := metrics["rules"].([]interface{})
	if !ok || len(rulesRaw) == 0 {
		return nil
	}

	evals := make([]checklistRuleEval, 0, len(rulesRaw))
	for idx, ruleAny := range rulesRaw {
		ruleMap, ok := ruleAny.(map[string]interface{})
		if !ok {
			continue
		}
		ruleNameRaw := textFromUnknown(ruleMap["name"])
		ruleName := strings.ToLower(ruleNameRaw)
		ruleDesc := strings.ToLower(textFromUnknown(ruleMap["description"]))
		reqsRaw, _ := ruleMap["requirements"].([]interface{})
		if len(reqsRaw) == 0 {
			continue
		}

		ruleKind := classifyChecklistRule(ruleName, ruleDesc, nil)
		ruleScore := 0.0
		ruleWeight := 0.0
		observedWeight := 0.0
		expectedWeight := 0.0
		reqNames := make([]string, 0, len(reqsRaw))

		for _, reqAny := range reqsRaw {
			reqMap, ok := reqAny.(map[string]interface{})
			if !ok {
				continue
			}
			reqName := strings.ToLower(textFromUnknown(reqMap["name"]))
			if reqName != "" {
				reqNames = append(reqNames, reqName)
			}
			weight := numberFromUnknown(reqMap["weight"])
			if weight <= 0 {
				weight = 1
			}
			expectedWeight += weight

			percent := numberFromUnknown(reqMap["percent"])
			target := numberFromUnknown(reqMap["target"])
			if target <= 0 {
				target = 100
			}
			score := checklistRequirementScore(percent, target, ruleKind)
			if percent >= 0 {
				observedWeight += weight
			}
			ruleScore += score * weight
			ruleWeight += weight
		}
		if ruleWeight <= 0 {
			continue
		}

		ruleKind = classifyChecklistRule(ruleName, ruleDesc, reqNames)
		rulePriority := checklistRulePriority(ruleKind)
		rulePercent := numberFromUnknown(ruleMap["percent"])
		if rulePercent < 0 {
			rulePercent = clamp100((ruleScore / ruleWeight) * 100)
		}
		ruleDisplayName := strings.TrimSpace(ruleNameRaw)
		if ruleDisplayName == "" {
			ruleDisplayName = fmt.Sprintf("规则%d", idx+1)
		}

		evals = append(evals, checklistRuleEval{
			name:           ruleDisplayName,
			percent:        rulePercent,
			score01:        clamp01(ruleScore / ruleWeight),
			priority:       rulePriority,
			observedWeight: observedWeight,
			expectedWeight: expectedWeight,
		})
	}
	return evals
}

// computeSuggestionPenalty 计算建议惩罚。
func computeSuggestionPenalty(metrics map[string]interface{}) float64 {
	if len(metrics) == 0 {
		return 0
	}
	sRaw, ok := metrics["suggestions"].([]interface{})
	if !ok || len(sRaw) == 0 {
		return 0
	}
	penalty := 0.0
	for _, item := range sRaw {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		severity := numberFromUnknown(entry["severity"])
		switch {
		case severity >= 3:
			penalty += 8
		case severity >= 2:
			penalty += 4
		case severity >= 1:
			penalty += 2
		default:
			penalty += 1
		}
	}
	if penalty > 35 {
		return 35
	}
	return penalty
}

// computeUtilityScore 计算效用评分。
func computeUtilityScore(moduleMap map[string]map[string]interface{}) float64 {
	candidates := []map[string]interface{}{moduleMap["utilities"], moduleMap["defensives"]}
	rateValues := make([]float64, 0, 2)
	for _, metrics := range candidates {
		if len(metrics) == 0 {
			continue
		}
		if val := numberFromUnknown(metrics["overallUsageRate"]); val >= 0 {
			rateValues = append(rateValues, clamp100(val))
		}
		if actions, ok := metrics["actions"].([]interface{}); ok {
			sum := 0.0
			count := 0.0
			for _, actionAny := range actions {
				action, ok := actionAny.(map[string]interface{})
				if !ok {
					continue
				}
				v := numberFromUnknown(action["usageRate"])
				if v < 0 {
					continue
				}
				sum += clamp100(v)
				count++
			}
			if count > 0 {
				rateValues = append(rateValues, sum/count)
			}
		}
	}
	if len(rateValues) == 0 {
		return 60
	}
	sum := 0.0
	for _, v := range rateValues {
		sum += v
	}
	return clamp100(sum / float64(len(rateValues)))
}

// computeSurvivalPenalty 计算生存惩罚。
func computeSurvivalPenalty(f fightRow) float64 {
	penalty := 0.0
	if !f.Kill {
		penalty += 6
	}
	progress := fightProgressPercent(f.FightPercentage, f.BossPercentage)
	if progress < 50 {
		penalty += 4
	}
	if progress < 30 {
		penalty += 4
	}
	if penalty > 20 {
		penalty = 20
	}
	return penalty
}

// computeJobModuleScore 计算职业模块评分。
func computeJobModuleScore(moduleMap map[string]map[string]interface{}) float64 {
	common := map[string]struct{}{
		"about": {}, "checklist": {}, "suggestions": {}, "utilities": {}, "defensives": {}, "statistics": {},
	}
	scores := make([]float64, 0)
	for handle, metrics := range moduleMap {
		if _, ok := common[handle]; ok {
			continue
		}
		if len(metrics) == 0 {
			continue
		}
		if uptime := numberFromUnknown(metrics["uptimePercent"]); uptime >= 0 {
			scores = append(scores, clamp100(uptime))
			continue
		}
		if bad := numberFromUnknown(metrics["totalBadUsages"]); bad >= 0 {
			scores = append(scores, clamp100(100-bad*8))
			continue
		}
		if issues := numberFromUnknown(metrics["issueCount"]); issues >= 0 {
			scores = append(scores, clamp100(100-issues*8))
			continue
		}
		if avg := numberFromUnknown(metrics["averagePercent"]); avg >= 0 {
			scores = append(scores, clamp100(avg))
			continue
		}
		scores = append(scores, 70)
	}
	if len(scores) == 0 {
		return 70
	}
	sum := 0.0
	for _, v := range scores {
		sum += v
	}
	return clamp100(sum / float64(len(scores)))
}

// combineBattleScore 汇总各维度分数得到战斗评分。
func combineBattleScore(role string, checklistAdj, suggestionScore, utilityScore, survivalScore, jobModuleScore float64) float64 {
	type part struct {
		weight float64
		value  float64
	}
	parts := []part{}
	switch role {
	case "T":
		parts = []part{{0.35, checklistAdj}, {0.15, suggestionScore}, {0.30, utilityScore}, {0.20, survivalScore}}
	case "N":
		parts = []part{{0.35, checklistAdj}, {0.20, suggestionScore}, {0.30, utilityScore}, {0.15, survivalScore}}
	default:
		parts = []part{{0.45, checklistAdj}, {0.20, suggestionScore}, {0.15, utilityScore}, {0.20, survivalScore}}
	}
	sum := 0.0
	weight := 0.0
	for _, p := range parts {
		sum += p.weight * p.value
		weight += p.weight
	}
	if weight <= 0 {
		return 0
	}
	base := sum / weight
	// Keep 返回keep信息。
	return clamp100(base*0.9 + jobModuleScore*0.1)
}

// fightWeight 返回战斗权重信息。
func fightWeight(kill bool, fightPercent, bossPercent, minWeight, maxWeight, exponent float64) float64 {
	if kill {
		return maxWeight
	}
	if minWeight <= 0 {
		minWeight = defaultWeightMin
	}
	if maxWeight < minWeight {
		maxWeight = minWeight
	}
	if exponent <= 0 {
		exponent = defaultWeightExp
	}

	progress := fightProgressPercent(fightPercent, bossPercent)
	if progress <= 0 {
		return minWeight
	}

	ratio := clamp01(progress / 100)
	weight := minWeight + (maxWeight-minWeight)*math.Pow(ratio, exponent)
	if weight > maxWeight {
		return maxWeight
	}
	if weight < minWeight {
		return minWeight
	}
	return weight
}

// fightProgressPercent 计算战斗进度百分比。
func fightProgressPercent(fightPercent, bossPercent float64) float64 {
	remaining := normalizeRemainingPercent(fightPercent)
	if remaining < 0 {
		remaining = normalizeRemainingPercent(bossPercent)
	}
	if remaining < 0 {
		return 0
	}
	progress := 100 - remaining
	if progress < 0 {
		return 0
	}
	if progress > 100 {
		return 100
	}
	return progress
}

// normalizeRemainingPercent 规范化剩余血量百分比。
func normalizeRemainingPercent(v float64) float64 {
	if v <= 0 {
		return -1
	}
	// FFLogs 返回fflogs信息。
	if v > 100 {
		v = v / 100
	}
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// parseEnvFloat 解析环境变量浮点值。
func parseEnvFloat(key string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return v
}

// roleFromJob 返回角色来源职业。
func roleFromJob(job string) string {
	upper := strings.ToUpper(job)
	switch upper {
	case "PALADIN", "WARRIOR", "DARK_KNIGHT", "GUNBREAKER":
		return "T"
	case "WHITE_MAGE", "SCHOLAR", "ASTROLOGIAN", "SAGE":
		return "N"
	default:
		return "DPS"
	}
}

// canonicalJobKey 返回标准化职业键。
func canonicalJobKey(raw string) string {
	v := strings.ToUpper(strings.TrimSpace(raw))
	v = strings.ReplaceAll(v, "-", "_")
	v = strings.ReplaceAll(v, " ", "_")
	for strings.Contains(v, "__") {
		v = strings.ReplaceAll(v, "__", "_")
	}

	switch v {
	case "PLD":
		return "PALADIN"
	case "WAR":
		return "WARRIOR"
	case "DRK":
		return "DARK_KNIGHT"
	case "GNB":
		return "GUNBREAKER"
	case "WHM":
		return "WHITE_MAGE"
	case "SCH":
		return "SCHOLAR"
	case "AST":
		return "ASTROLOGIAN"
	case "SGE":
		return "SAGE"
	case "MNK":
		return "MONK"
	case "DRG":
		return "DRAGOON"
	case "NIN":
		return "NINJA"
	case "SAM":
		return "SAMURAI"
	case "RPR":
		return "REAPER"
	case "VPR":
		return "VIPER"
	case "BRD":
		return "BARD"
	case "MCH":
		return "MACHINIST"
	case "DNC":
		return "DANCER"
	case "BLM":
		return "BLACK_MAGE"
	case "SMN":
		return "SUMMONER"
	case "RDM":
		return "RED_MAGE"
	case "PCT":
		return "PICTOMANCER"
	case "BLU":
		return "BLUE_MAGE"
	default:
		return v
	}
}

// CanonicalJobKey 返回标准化职业键。
func CanonicalJobKey(raw string) string {
	return canonicalJobKey(raw)
}

// isTransientAnalyzeError 判断瞬时分析错误是否满足条件。
func isTransientAnalyzeError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "eof") ||
		strings.Contains(msg, "refused")
}

// shouldFallbackAnalyzeInline 判断回退分析内联是否满足条件。
func shouldFallbackAnalyzeInline(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "remote_fetch_unavailable") {
		return true
	}
	if strings.Contains(msg, "missing events") {
		return true
	}
	if strings.Contains(msg, "缺少 events") {
		return true
	}
	return false
}

// shouldRetryAnalyzePending 判断重试分析待处理是否满足条件。
func shouldRetryAnalyzePending(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "remote_fetch_pending")
}

// shouldUseRemoteFetchTransport 判断是否使用远端拉取传输方式。
func shouldUseRemoteFetchTransport() bool {
	mode := strings.TrimSpace(strings.ToLower(os.Getenv("XIVA_EVENTS_TRANSPORT_MODE")))
	switch mode {
	case "remote", "remote-fetch", "self-fetch", "distributed-fetch":
		return true
	case "inline", "legacy", "payload":
		return false
	}
	return envBool("XIVA_REMOTE_FETCH_EVENTS", false)
}

// resolveRemoteFetchConfig 解析远端拉取配置。
func resolveRemoteFetchConfig() (*analyzeFetch, error) {
	apiKey := strings.TrimSpace(os.Getenv("FFLOGS_V1_API_KEY"))
	if apiKey == "" {
		v, err := loadV1APIKeyFromDB()
		if err == nil {
			apiKey = v
		}
	}
	if apiKey == "" {
		return nil, fmt.Errorf("missing FFLOGS V1 API key for remote fetch")
	}

	baseURL := strings.TrimSpace(os.Getenv("FFLOGS_V1_BASE_URL"))
	if baseURL == "" {
		baseURL = defaultFFLogsV1BaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	translate := envBool("FFLOGS_V1_TRANSLATE", true)

	return &analyzeFetch{
		Provider:  "fflogs-v1",
		APIKey:    apiKey,
		BaseURL:   baseURL,
		Translate: &translate,
	}, nil
}

// loadV1APIKeyFromDB 从数据库加载 V1 API Key。
func loadV1APIKeyFromDB() (string, error) {
	var row struct {
		ID    int    `gorm:"column:id"`
		APIID string `gorm:"column:api_id"`
	}

	err := db.DB.Raw(`
SELECT id, api_id
FROM fflogskey
WHERE ver = 1
  AND COALESCE(api_id, '') <> ''
ORDER BY COALESCE(used, 0) ASC, id ASC
LIMIT 1
`).Scan(&row).Error
	if err != nil {
		return "", err
	}

	apiKey := strings.TrimSpace(row.APIID)
	if row.ID <= 0 || apiKey == "" {
		return "", fmt.Errorf("no v1 api key in fflogskey")
	}

	_ = db.DB.Exec(`UPDATE fflogskey SET used = COALESCE(used, 0) + 1 WHERE id = ?`, row.ID).Error
	return apiKey, nil
}

// waitRetry 等待重试。
func waitRetry(ctx context.Context, attempt int) error {
	if attempt < 1 {
		attempt = 1
	}
	delay := time.Duration(attempt*attempt) * 250 * time.Millisecond
	if delay > 4*time.Second {
		delay = 4 * time.Second
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// loadPlayerName 获取玩家名称。
func loadPlayerName(playerID uint) string {
	var player models.Player
	if err := db.DB.Select("name").Where("id = ?", playerID).First(&player).Error; err != nil {
		return ""
	}
	return player.Name
}

// loadReportRow 获取报告记录。
func loadReportRow(playerID uint, code string) (*models.Report, error) {
	var reports []models.Report
	q := db.DB.Model(&models.Report{}).
		Where("player_id = ? AND (master_report = ? OR source_report = ?)", playerID, code, code).
		Order("id desc")
	if err := q.Find(&reports).Error; err != nil {
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
		if err := db.DB.Model(&models.Report{}).
			Where("player_id = ? AND master_report = ?", playerID, fallback.MasterReport).
			Order("id desc").
			Find(&sameMaster).Error; err == nil {
			for i := range sameMaster {
				if reportMetadataHasMasterData(sameMaster[i].ReportMetadata) {
					return &sameMaster[i], nil
				}
			}
		}
	}
	return &fallback, nil
}

// selectPreferredReportRow 选择优先报告记录。
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

// reportRowRank 返回报告记录排序。
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

// reportMetadataHasMasterData 判断报告元数据是否包含主战斗数据。
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

// loadFightRow 获取战斗记录。
func loadFightRow(playerID uint, reportCode string, fightID int) (fightRow, error) {
	var rows []fightRow
	codeJSON, _ := json.Marshal([]string{reportCode})
	if err := db.DB.Table("fight_sync_maps").
		Select("master_id, fight_id, name, kill, start_time, end_time, fight_percentage, boss_percentage, encounter_id, difficulty, job, game_zone").
		Where("player_id = ? AND fight_id = ? AND source_ids @> ?", playerID, fightID, datatypes.JSON(codeJSON)).
		Order("id desc").
		Find(&rows).Error; err != nil {
		return fightRow{}, err
	}
	if len(rows) == 0 {
		var row fightRow
		if err := db.DB.Table("fight_sync_maps").
			Select("master_id, fight_id, name, kill, start_time, end_time, fight_percentage, boss_percentage, encounter_id, difficulty, job, game_zone").
			Where("player_id = ? AND master_id = ?", playerID, fmt.Sprintf("%s-%d", reportCode, fightID)).
			Order("id desc").
			First(&row).Error; err != nil {
			return fightRow{}, fmt.Errorf("fight row not found: %s-%d", reportCode, fightID)
		}
		return row, nil
	}
	return rows[0], nil
}

// readEvents 读取事件列表。
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
			if nested, ok := events.(map[string]any); ok {
				if list, ok := nested["events"].([]any); ok {
					return list, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("unsupported events format")
}

// extractMasterData 提取报告中的主战斗数据。
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

// buildFightJSON 构建战斗JSON。
func buildFightJSON(row fightRow) fightJSON {
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
	return fight
}

// updateFightSyncScore 更新战斗同步评分。
func updateFightSyncScore(score *fightScoreUpdate) error {
	updates := map[string]interface{}{
		"job":                   score.Job,
		"score_actor_name":      score.ActorName,
		"checklist_abs":         roundToFixed(score.ChecklistAbs, 2),
		"checklist_confidence":  roundToFixed(score.ChecklistConfidence, 2),
		"checklist_adj":         roundToFixed(score.ChecklistAdj, 2),
		"suggestion_penalty":    roundToFixed(score.SuggestionPenalty, 2),
		"utility_score":         roundToFixed(score.UtilityScore, 2),
		"survival_penalty":      roundToFixed(score.SurvivalPenalty, 2),
		"job_module_score":      roundToFixed(score.JobModuleScore, 2),
		"battle_score":          roundToFixed(score.BattleScore, 2),
		"fight_weight":          roundToFixed(score.FightWeight, 2),
		"weighted_battle_score": roundToFixed(score.WeightedBattleScore, 2),
		"raw_module_metrics":    score.RawModuleMetrics,
		"scored_at":             score.ScoredAt,
	}

	res := db.DB.Model(&models.FightSyncMap{}).
		Where("player_id = ? AND master_id = ?", score.PlayerID, score.MasterID).
		Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		var cnt int64
		if err := db.DB.Model(&models.FightSyncMap{}).
			Where("player_id = ? AND master_id = ?", score.PlayerID, score.MasterID).
			Count(&cnt).Error; err != nil {
			return err
		}
		if cnt == 0 {
			return fmt.Errorf("fight_sync_maps row not found for score update: player_id=%d master_id=%s", score.PlayerID, score.MasterID)
		}
		return nil
	}
	return nil
}

// refreshPlayerBattleAbility 更新玩家战斗能力。
func refreshPlayerBattleAbility(playerID uint) error {
	if playerID == 0 {
		return nil
	}

	type aggregate struct {
		WeightedSum float64
		TotalWeight float64
	}
	var agg aggregate
	if err := db.DB.Table("fight_sync_maps").
		Select("COALESCE(SUM(weighted_battle_score), 0) AS weighted_sum, COALESCE(SUM(fight_weight), 0) AS total_weight").
		Where("player_id = ? AND scored_at IS NOT NULL AND fight_weight > 0", playerID).
		Scan(&agg).Error; err != nil {
		return err
	}

	score := 0.0
	if agg.TotalWeight > 0 {
		score = clamp100(agg.WeightedSum / agg.TotalWeight)
		if score > 0 && score < 1 {
			score = 1
		}
		score = roundToFixed(score, 2)
	}

	if err := db.DB.Model(&models.Player{}).
		Where("id = ?", playerID).
		Update("battle_ability", score).Error; err != nil {
		return err
	}
	return nil
}

// refreshPlayerTeamContribution 刷新玩家团队贡献评分。
func refreshPlayerTeamContribution(playerID uint) error {
	if playerID == 0 {
		return nil
	}

	type aggregate struct {
		WeightedSum float64
		TotalWeight float64
	}
	var agg aggregate
	if err := db.DB.Table("fight_sync_maps").
		Select("COALESCE(SUM(utility_score * fight_weight), 0) AS weighted_sum, COALESCE(SUM(fight_weight), 0) AS total_weight").
		Where("player_id = ? AND scored_at IS NOT NULL AND fight_weight > 0", playerID).
		Scan(&agg).Error; err != nil {
		return err
	}

	score := 0.0
	if agg.TotalWeight > 0 {
		score = clamp100(agg.WeightedSum / agg.TotalWeight)
		if score > 0 && score < 1 {
			score = 1
		}
		score = roundToFixed(score, 2)
	}

	if err := db.DB.Model(&models.Player{}).
		Where("id = ?", playerID).
		Update("team_contribution", score).Error; err != nil {
		return err
	}
	return nil
}

type stabilityFightRow struct {
	ID                uint
	EncounterID       int
	Kill              bool
	Timestamp         int64
	FightWeight       float64
	SuggestionPenalty float64
	SurvivalPenalty   float64
	RawModuleMetrics  datatypes.JSON
}

type rawStabilityMetrics struct {
	Deaths        float64
	AvoidableHits float64
	MajorMistakes float64
}

// refreshPlayerStabilityScore 更新玩家稳定性评分。
func refreshPlayerStabilityScore(playerID uint) error {
	if playerID == 0 {
		return nil
	}

	var fights []stabilityFightRow
	if err := db.DB.Table("fight_sync_maps").
		Select("id", "encounter_id", "kill", "timestamp", "fight_weight", "suggestion_penalty", "survival_penalty", "raw_module_metrics").
		Where("player_id = ? AND scored_at IS NOT NULL AND fight_weight > 0", playerID).
		Order("encounter_id asc, timestamp asc, id asc").
		Scan(&fights).Error; err != nil {
		return err
	}

	firstKillByEncounter := make(map[int]int64)
	for _, fight := range fights {
		if !fight.Kill || fight.EncounterID <= 0 {
			continue
		}
		current, exists := firstKillByEncounter[fight.EncounterID]
		if !exists || fight.Timestamp < current {
			firstKillByEncounter[fight.EncounterID] = fight.Timestamp
		}
	}

	weightedSum := 0.0
	totalWeight := 0.0
	fightScores := make([]float64, 0, len(fights))
	fightWeights := make([]float64, 0, len(fights))

	for _, fight := range fights {
		phaseWeight := stabilityPhaseWeight(fight, firstKillByEncounter)
		weight := fight.FightWeight * phaseWeight
		if weight <= 0 {
			continue
		}

		score := computeFightStabilityScore(fight)
		weightedSum += score * weight
		totalWeight += weight
		fightScores = append(fightScores, score)
		fightWeights = append(fightWeights, weight)
	}

	score := 0.0
	if totalWeight > 0 {
		mean := weightedSum / totalWeight
		volatility := weightedStdDev(fightScores, fightWeights)
		volatilityPenalty := math.Min(18, volatility*0.6)
		score = clamp100(mean - volatilityPenalty)
		if score > 0 && score < 1 {
			score = 1
		}
		score = roundToFixed(score, 2)
	}

	if err := db.DB.Model(&models.Player{}).
		Where("id = ?", playerID).
		Update("stability_score", score).Error; err != nil {
		return err
	}
	return nil
}

// stabilityPhaseWeight 返回稳定性阶段权重。
func stabilityPhaseWeight(fight stabilityFightRow, firstKillByEncounter map[int]int64) float64 {
	if fight.EncounterID <= 0 {
		if fight.Kill {
			return 1
		}
		return 0.7
	}

	firstKillTs, ok := firstKillByEncounter[fight.EncounterID]
	if !ok {
		return 0.7
	}
	if fight.Timestamp < firstKillTs {
		return 0.7
	}
	return 1
}

// computeFightStabilityScore 计算战斗稳定性评分。
func computeFightStabilityScore(fight stabilityFightRow) float64 {
	raw := extractRawStabilityMetrics(fight.RawModuleMetrics)
	extraPenalty := raw.Deaths*12 + raw.AvoidableHits*2.5 + raw.MajorMistakes*5
	if extraPenalty > 35 {
		extraPenalty = 35
	}

	penalty := 0.45*fight.SurvivalPenalty + 0.25*fight.SuggestionPenalty + 0.30*extraPenalty
	if penalty > 99 {
		penalty = 99
	}
	return clamp100(100 - penalty)
}

// extractRawStabilityMetrics 提取原始数据稳定性指标集。
func extractRawStabilityMetrics(raw datatypes.JSON) rawStabilityMetrics {
	if len(raw) == 0 {
		return rawStabilityMetrics{}
	}

	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return rawStabilityMetrics{}
	}

	deaths := findMaxNumericByKey(payload, func(key string) bool {
		return strings.Contains(key, "death")
	})
	if deaths < 0 {
		deaths = 0
	}

	avoidable := findMaxNumericByKey(payload, func(key string) bool {
		if !strings.Contains(key, "avoidable") {
			return false
		}
		return strings.Contains(key, "count") || strings.Contains(key, "damage") || strings.Contains(key, "hit") || strings.Contains(key, "event")
	})
	if avoidable < 0 {
		avoidable = 0
	}

	majorMistakes := findMaxNumericByKey(payload, func(key string) bool {
		if strings.Contains(key, "major") && (strings.Contains(key, "mistake") || strings.Contains(key, "error") || strings.Contains(key, "failure")) {
			return true
		}
		if strings.Contains(key, "critical") && (strings.Contains(key, "mistake") || strings.Contains(key, "error")) {
			return true
		}
		if strings.Contains(key, "failedmechanic") || strings.Contains(key, "failedmechanics") {
			return true
		}
		if strings.Contains(key, "totalbadusages") || strings.Contains(key, "badusagecount") || strings.Contains(key, "issuecount") {
			return true
		}
		return false
	})
	if majorMistakes < 0 {
		majorMistakes = 0
	}

	return rawStabilityMetrics{
		Deaths:        deaths,
		AvoidableHits: avoidable,
		MajorMistakes: majorMistakes,
	}
}

// findMaxNumericByKey 查找最大值数值键。
func findMaxNumericByKey(v any, matcher func(string) bool) float64 {
	maxVal := -1.0
	var walk func(any)
	walk = func(node any) {
		switch val := node.(type) {
		case map[string]interface{}:
			for key, inner := range val {
				normalized := normalizeMetricKey(key)
				if matcher(normalized) {
					num := numberFromUnknown(inner)
					if num >= 0 && num > maxVal {
						maxVal = num
					}
				}
				walk(inner)
			}
		case []interface{}:
			for _, inner := range val {
				walk(inner)
			}
		}
	}
	walk(v)
	return maxVal
}

// normalizeMetricKey 解析指标键。
func normalizeMetricKey(key string) string {
	normalized := strings.ToLower(strings.TrimSpace(key))
	replacer := strings.NewReplacer("_", "", "-", "", " ", "", ".", "")
	return replacer.Replace(normalized)
}

// weightedStdDev 返回加权标准偏差。
func weightedStdDev(values, weights []float64) float64 {
	if len(values) == 0 || len(values) != len(weights) {
		return 0
	}

	sumWeight := 0.0
	weightedSum := 0.0
	for i, value := range values {
		weight := weights[i]
		if weight <= 0 {
			continue
		}
		sumWeight += weight
		weightedSum += value * weight
	}
	if sumWeight <= 0 {
		return 0
	}

	mean := weightedSum / sumWeight
	varianceWeightedSum := 0.0
	for i, value := range values {
		weight := weights[i]
		if weight <= 0 {
			continue
		}
		delta := value - mean
		varianceWeightedSum += weight * delta * delta
	}

	variance := varianceWeightedSum / sumWeight
	if variance <= 0 {
		return 0
	}
	return math.Sqrt(variance)
}

// refreshPlayerProgressionSpeed 刷新玩家开荒速度评分。
func refreshPlayerProgressionSpeed(playerID uint) error {
	if playerID == 0 {
		return nil
	}

	var fights []models.FightSyncMap
	if err := db.DB.Model(&models.FightSyncMap{}).
		Select("id", "encounter_id", "kill", "fight_percentage", "boss_percentage", "timestamp", "friendplayers", "friendplayers_usable").
		Where("player_id = ? AND friendplayers_usable = ?", playerID, true).
		Order("encounter_id asc, timestamp asc, id asc").
		Find(&fights).Error; err != nil {
		return err
	}

	score := 0.0
	if len(fights) > 0 {
		grouped := make(map[int][]models.FightSyncMap)
		for _, fight := range fights {
			if fight.EncounterID <= 0 {
				continue
			}
			grouped[fight.EncounterID] = append(grouped[fight.EncounterID], fight)
		}

		totalScore := 0.0
		totalWeight := 0.0
		for _, attempts := range grouped {
			encounterScore, weight := computeEncounterProgressionScore(attempts)
			if weight <= 0 {
				continue
			}
			totalScore += encounterScore * weight
			totalWeight += weight
		}

		if totalWeight > 0 {
			score = clamp100(totalScore / totalWeight)
			if score > 0 && score < 1 {
				score = 1
			}
			score = roundToFixed(score, 2)
		}
	}

	if err := db.DB.Model(&models.Player{}).
		Where("id = ?", playerID).
		Update("progression_speed", score).Error; err != nil {
		return err
	}

	return nil
}

// refreshPlayerPotentialScore 更新玩家潜力评分。
func refreshPlayerPotentialScore(playerID uint) error {
	if playerID == 0 {
		return nil
	}

	type playerAggregate struct {
		BattleAbility    float64
		TeamContribution float64
		StabilityScore   float64
		ProgressionSpeed float64
		MechanicsScore   float64
	}

	var player playerAggregate
	if err := db.DB.Model(&models.Player{}).
		Select("battle_ability", "team_contribution", "stability_score", "progression_speed", "mechanics_score").
		Where("id = ?", playerID).
		First(&player).Error; err != nil {
		return err
	}

	// 潜力值综合：战斗能力、稳定度、开荒表现、机制处理、团队贡献。
	raw := 0.35*player.BattleAbility +
		0.10*player.TeamContribution +
		0.20*player.StabilityScore +
		0.20*player.ProgressionSpeed +
		0.15*player.MechanicsScore

	// 高分项一致性加成：当核心项都高时，额外体现上限潜力。
	coreMin := minFloat(player.BattleAbility, player.StabilityScore, player.MechanicsScore)
	if coreMin >= 85 {
		raw += 3
	} else if coreMin >= 75 {
		raw += 1.5
	}

	type confidenceAgg struct {
		TotalWeight float64
	}
	var conf confidenceAgg
	if err := db.DB.Table("fight_sync_maps").
		Select("COALESCE(SUM(fight_weight), 0) AS total_weight").
		Where("player_id = ? AND scored_at IS NOT NULL AND fight_weight > 0", playerID).
		Scan(&conf).Error; err != nil {
		return err
	}

	// 样本收缩：样本不足时向中性分回归，避免潜力值过拟合。
	confidence := clamp01(conf.TotalWeight / 8.0)
	score := confidence*raw + (1-confidence)*60
	score = clamp100(score)
	if score > 0 && score < 1 {
		score = 1
	}
	score = roundToFixed(score, 2)

	if err := db.DB.Model(&models.Player{}).
		Where("id = ?", playerID).
		Update("potential_score", score).Error; err != nil {
		return err
	}

	return nil
}

// computeEncounterProgressionScore 计算单个遭遇战的开荒进度评分。
func computeEncounterProgressionScore(attempts []models.FightSyncMap) (float64, float64) {
	if len(attempts) == 0 {
		return 0, 0
	}

	sort.SliceStable(attempts, func(i, j int) bool {
		if attempts[i].Timestamp == attempts[j].Timestamp {
			return attempts[i].ID < attempts[j].ID
		}
		return attempts[i].Timestamp < attempts[j].Timestamp
	})

	effectiveAttempts := 0.0
	bestProgress := 0.0
	killed := false

	prevSignature := ""
	prevProgress := 0.0
	for i, attempt := range attempts {
		progress := fightProgressPercent(attempt.FightPercentage, attempt.BossPercentage)
		if attempt.Kill {
			progress = 100
		}
		if progress > bestProgress {
			bestProgress = progress
		}

		cost := 1.0
		signature := partySignature(attempt.FriendPlayers)
		if i > 0 && signature != "" && prevSignature != "" && signature != prevSignature {
			improvement := progress - prevProgress
			switch {
			case improvement >= 10:
				// Team 返回team信息。
				cost = 0.5
			case improvement <= -10:
				cost = 1.15
			default:
				cost = 0.85
			}
		}

		effectiveAttempts += cost
		prevSignature = signature
		prevProgress = progress

		if attempt.Kill {
			killed = true
			break
		}
	}

	if effectiveAttempts < 1 {
		effectiveAttempts = 1
	}

	base := 100 / (1 + 0.22*(effectiveAttempts-1))
	if !killed {
		progressFactor := 0.35 + 0.65*clamp01(bestProgress/100)
		base = base * 0.85 * progressFactor
	}

	weight := 0.7 + 0.3*clamp01(bestProgress/100)
	if killed {
		weight = 1
	}

	return clamp100(base), weight
}

// partySignature 返回队伍签名信息。
func partySignature(names []string) string {
	if len(names) == 0 {
		return ""
	}
	normalized := make([]string, 0, len(names))
	for _, name := range names {
		n := strings.ToLower(strings.TrimSpace(name))
		if n == "" {
			continue
		}
		normalized = append(normalized, n)
	}
	if len(normalized) == 0 {
		return ""
	}
	sort.Strings(normalized)
	return strings.Join(normalized, "|")
}

// loadJobBaseline 获取职业基线。
func loadJobBaseline(playerID uint, job string) float64 {
	canonical := canonicalJobKey(job)
	if canonical == "" {
		canonical = strings.ToUpper(strings.TrimSpace(job))
	}

	type baselineRow struct {
		Job          string
		ChecklistAdj float64
	}

	rows := make([]baselineRow, 0, 64)
	if err := db.DB.Table("fight_sync_maps").
		Select("job, checklist_adj").
		Where("player_id = ? AND scored_at IS NOT NULL", playerID).
		Order("scored_at desc").
		Limit(200).
		Scan(&rows).Error; err != nil {
		return 70
	}

	values := make([]float64, 0, len(rows))
	for _, row := range rows {
		if canonicalJobKey(row.Job) != canonical {
			continue
		}
		values = append(values, row.ChecklistAdj)
		if len(values) >= 30 {
			break
		}
	}

	if len(values) == 0 {
		return 70
	}
	sort.Float64s(values)
	mid := len(values) / 2
	if len(values)%2 == 1 {
		return values[mid]
	}
	return (values[mid-1] + values[mid]) / 2
}

type checklistRuleKind struct {
	isGCD              bool
	isDot              bool
	isDamageBuffUptime bool
}

// classifyChecklistRule 返回分类清单规则。
func classifyChecklistRule(ruleName, ruleDesc string, reqNames []string) checklistRuleKind {
	texts := make([]string, 0, 2+len(reqNames))
	if ruleName != "" {
		texts = append(texts, ruleName)
	}
	if ruleDesc != "" {
		texts = append(texts, ruleDesc)
	}
	texts = append(texts, reqNames...)
	joined := strings.Join(texts, " ")
	nameReqJoined := strings.Join(append([]string{ruleName}, reqNames...), " ")

	isGCD := containsAny(nameReqJoined, "gcd", "global cooldown", "保持使用技能", "gcd覆盖率", "保持你的技能在冷却")
	isDot := containsAny(joined,
		"dot",
		"higanbana",
		"dia",
		"thunder",
		"dosis",
		"combust",
		"bio",
		"caustic",
		"stormbite",
		"demolish",
		"chaos thrust",
	)

	// Passive 返回passive信息。
	isDamageBuffUptime := containsAny(joined,
		"buff常驻",
		"保持暗黑状态",
		"暗黑持续时间",
		"standard finish 覆盖率",
		"fugetsu 覆盖率",
		"fuka 覆盖率",
	)
	if !isDamageBuffUptime {
		hasDamageHint := containsAny(joined, "提高", "增伤", "伤害")
		hasUptimeHint := containsAny(joined, "常驻", "覆盖率", "不断", "持续时间", "状态", "buff")
		isDamageBuffUptime = hasDamageHint && hasUptimeHint
	}

	return checklistRuleKind{
		isGCD:              isGCD,
		isDot:              isDot,
		isDamageBuffUptime: isDamageBuffUptime,
	}
}

// checklistRequirementScore 返回清单要求评分。
func checklistRequirementScore(percent, target float64, kind checklistRuleKind) float64 {
	if percent <= 0 {
		return 0
	}
	threshold := target
	expo := 1.6
	if kind.isGCD {
		threshold = maxFloat(threshold, 95)
		expo = 2.4
	} else if kind.isDamageBuffUptime {
		threshold = maxFloat(threshold, 95)
		expo = 2.2
	} else {
		threshold = maxFloat(threshold, 90)
	}
	if threshold <= 0 {
		threshold = 100
	}
	ratio := clamp01(percent / threshold)
	return math.Pow(ratio, expo)
}

// checklistRulePriority 返回清单规则优先级。
func checklistRulePriority(kind checklistRuleKind) float64 {
	multiplier := 1.0
	if kind.isGCD {
		multiplier *= 3.0
	}
	if kind.isDamageBuffUptime {
		multiplier *= 2.2
	}
	if kind.isDot {
		multiplier *= 2.0
	}
	if multiplier > 6.0 {
		return 6.0
	}
	return multiplier
}

// maxFloat 返回最大值浮点值信息。
func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// minFloat 返回最小值浮点值信息。
func minFloat(values ...float64) float64 {
	if len(values) == 0 {
		return 0
	}
	minVal := values[0]
	for i := 1; i < len(values); i++ {
		if values[i] < minVal {
			minVal = values[i]
		}
	}
	return minVal
}

// textFromUnknown 从未知类型值提取文本。
func textFromUnknown(v interface{}) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// containsAny 判断任意值是否满足条件。
func containsAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if needle != "" && strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// numberFromUnknown 返回数值来源未知值。
func numberFromUnknown(v interface{}) float64 {
	switch val := v.(type) {
	case float64:
		return val
	case float32:
		return float64(val)
	case int:
		return float64(val)
	case int64:
		return float64(val)
	case json.Number:
		if out, err := val.Float64(); err == nil {
			return out
		}
	case string:
		if val == "" {
			return -1
		}
		if out, err := strconvParseFloat(val); err == nil {
			return out
		}
	}
	return -1
}

// strconvParseFloat 安全解析字符串为浮点数。
func strconvParseFloat(s string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(s), 64)
}

// clamp01 将数值限制在 0 到 1 区间。
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// clamp100 将数值限制在 0 到 100 区间。
func clamp100(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// roundToFixed 转换四舍五入定点。
func roundToFixed(v float64, digits int) float64 {
	if digits < 0 {
		return v
	}
	factor := math.Pow10(digits)
	return math.Round(v*factor) / factor
}
