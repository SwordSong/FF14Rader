package api

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/cors"
	internalapi "github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/cluster"
	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/models"
	"github.com/user/ff14rader/internal/render"
	"gorm.io/gorm"
)

// context key for request id
type ctxKey string

const requestIDKey ctxKey = "requestID"

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

type Service struct {
	SyncManager        *internalapi.SyncManager
	EnableDashboardAPI bool
	radarWorkerOnce    sync.Once
	dispatchWarmupOnce sync.Once
}

type registerReportsRequest struct {
	Host            string   `json:"host"`
	Reports         []string `json:"reports"`
	ControlEndpoint string   `json:"controlEndpoint,omitempty"`
}

type executeReportsRequest struct {
	PlayerID uint     `json:"playerId"`
	Reports  []string `json:"reports"`
}

type heartbeatRequest struct {
	Host            string `json:"host"`
	ControlEndpoint string `json:"controlEndpoint,omitempty"`
}

type claimTasksRequest struct {
	Host     string `json:"host"`
	Limit    int    `json:"limit"`
	LeaseSec int    `json:"leaseSec"`
}

type ackTaskRequest struct {
	Host    string `json:"host"`
	TaskID  string `json:"taskId"`
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

type pullTaskResponseItem struct {
	TaskID   string   `json:"taskId"`
	PlayerID uint     `json:"playerId"`
	Reports  []string `json:"reports"`
	Host     string   `json:"host"`
}

const (
	radarCacheTTL        = 24 * time.Hour
	defaultRadarImageDir = "./pic"
)

type playerRadarSnapshot struct {
	ID               uint      `gorm:"column:id"`
	Name             string    `gorm:"column:name"`
	Server           string    `gorm:"column:server"`
	OutputAbility    float64   `gorm:"column:output_ability"`
	BattleAbility    float64   `gorm:"column:battle_ability"`
	TeamContribution float64   `gorm:"column:team_contribution"`
	ProgressionSpeed float64   `gorm:"column:progression_speed"`
	StabilityScore   float64   `gorm:"column:stability_score"`
	PotentialScore   float64   `gorm:"column:potential_score"`
	PicHash          string    `gorm:"column:pichash"`
	PicUpdatedAt     time.Time `gorm:"column:pic_updated_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at"`
}

func radarTaskKey(username, server string) string {
	namePart := strings.ToLower(strings.TrimSpace(username))
	serverPart := strings.ToLower(strings.TrimSpace(server))
	return namePart + "@" + serverPart
}

func radarImageDir() string {
	if raw := strings.TrimSpace(os.Getenv("RADAR_PIC_DIR")); raw != "" {
		return raw
	}
	return defaultRadarImageDir
}

func radarImagePathFromHash(picHash string) (string, error) {
	hash := strings.TrimSpace(picHash)
	if hash == "" {
		return "", fmt.Errorf("empty pic hash")
	}
	relPath := filepath.Join(radarImageDir(), hash+".png")
	absPath, err := filepath.Abs(relPath)
	if err != nil {
		return "", err
	}
	return absPath, nil
}

func loadPlayerRadarSnapshot(name, server string) (*playerRadarSnapshot, error) {
	var row playerRadarSnapshot
	err := db.DB.Model(&models.Player{}).
		Select("id", "name", "server", "output_ability", "battle_ability", "team_contribution", "progression_speed", "stability_score", "potential_score", "pichash", "pic_updated_at", "updated_at").
		Where("LOWER(name) = LOWER(?) AND LOWER(server) = LOWER(?)", strings.TrimSpace(name), strings.TrimSpace(server)).
		Order("updated_at DESC").
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

func cachedRadarImagePath(row *playerRadarSnapshot, now time.Time) (string, bool) {
	if row == nil {
		return "", false
	}
	if strings.TrimSpace(row.PicHash) == "" || row.PicUpdatedAt.IsZero() {
		return "", false
	}
	if now.Sub(row.PicUpdatedAt) >= radarCacheTTL {
		return "", false
	}

	path, err := radarImagePathFromHash(row.PicHash)
	if err != nil {
		return "", false
	}
	info, statErr := os.Stat(path)
	if statErr != nil || info.IsDir() {
		return "", false
	}
	return path, true
}

func buildPlayerRadarHash(row *playerRadarSnapshot) string {
	seed := fmt.Sprintf("%d|%s|%s|%.4f|%.4f|%.4f|%.4f|%.4f|%.4f|%d",
		row.ID,
		row.Name,
		row.Server,
		row.OutputAbility,
		row.BattleAbility,
		row.TeamContribution,
		row.ProgressionSpeed,
		row.StabilityScore,
		row.PotentialScore,
		row.UpdatedAt.UTC().UnixNano(),
	)
	sum := sha1.Sum([]byte(seed))
	return hex.EncodeToString(sum[:12])
}

func generateAndPersistPlayerRadar(row *playerRadarSnapshot) (string, string, error) {
	if row == nil || row.ID == 0 {
		return "", "", fmt.Errorf("invalid player")
	}

	picHash := buildPlayerRadarHash(row)
	outputPath, err := radarImagePathFromHash(picHash)
	if err != nil {
		return "", "", err
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return "", "", fmt.Errorf("create radar image dir failed: %v", err)
	}

	radar := render.NewRadarChart(900, 900)
	labels := []string{"输出能力", "战斗能力", "团队贡献", "开荒速度", "稳定度", "潜力值"}
	values := []float64{
		row.OutputAbility,
		row.BattleAbility,
		row.TeamContribution,
		row.ProgressionSpeed,
		row.StabilityScore,
		row.PotentialScore,
	}
	title := strings.TrimSpace(row.Name)
	if title == "" {
		title = fmt.Sprintf("player-%d", row.ID)
	}

	if err := radar.DrawMetrics(title, labels, values, outputPath); err != nil {
		return "", "", fmt.Errorf("draw radar failed: %v", err)
	}

	now := time.Now()
	if err := db.DB.Model(&models.Player{}).
		Where("id = ?", row.ID).
		Updates(map[string]interface{}{
			"pichash":        picHash,
			"pic_updated_at": now,
		}).Error; err != nil {
		return "", "", fmt.Errorf("update player pichash failed: %v", err)
	}

	row.PicHash = picHash
	row.PicUpdatedAt = now
	return picHash, outputPath, nil
}

func (s *Service) registerReportsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed. Only POST is supported.", http.StatusMethodNotAllowed)
		return
	}

	var req registerReportsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json body: %v", err))
		return
	}

	host := cluster.NormalizeHost(req.Host)
	if host == "" {
		host = cluster.NormalizeHost(r.RemoteAddr)
	}
	if host == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("host is required"))
		return
	}

	added, total := cluster.GlobalReportHostRegistry().RegisterHostReportsWithEndpoint(host, req.Reports, req.ControlEndpoint)
	controlEndpoint, _ := cluster.GlobalReportHostRegistry().ResolveHostControlEndpoint(host)
	if persistErr := cluster.PersistHostEndpoint(host, controlEndpoint, time.Now()); persistErr != nil {
		log.Printf("[CLUSTER] 持久化 host endpoint 失败 host=%s endpoint=%s err=%v", host, controlEndpoint, persistErr)
	}
	if _, persistMapErr := cluster.PersistReportHostMappings(host, req.Reports, time.Now()); persistMapErr != nil {
		log.Printf("[CLUSTER] 持久化 report->host 映射失败 host=%s reports=%d err=%v", host, len(req.Reports), persistMapErr)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":          "ok",
		"host":            host,
		"received":        len(req.Reports),
		"addedOrMoved":    added,
		"totalReports":    total,
		"controlEndpoint": controlEndpoint,
		"time":            time.Now().Format(time.RFC3339),
	})
}

func (s *Service) listReportsHostMapHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed. Only GET is supported.", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":          "ok",
		"reports":         cluster.GlobalReportHostRegistry().Snapshot(),
		"hostLoad":        cluster.GlobalReportHostRegistry().SnapshotLoads(),
		"hostSeenAt":      cluster.GlobalReportHostRegistry().SnapshotSeenAt(),
		"hostControlApi":  cluster.GlobalReportHostRegistry().SnapshotControlEndpoints(),
		"reportsCount":    len(cluster.GlobalReportHostRegistry().Snapshot()),
		"controlApiCount": len(cluster.GlobalReportHostRegistry().SnapshotControlEndpoints()),
		"time":            time.Now().Format(time.RFC3339),
	})
}

func (s *Service) heartbeatReportsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed. Only POST is supported.", http.StatusMethodNotAllowed)
		return
	}

	var req heartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json body: %v", err))
		return
	}

	host := cluster.NormalizeHost(req.Host)
	if host == "" {
		host = cluster.NormalizeHost(r.RemoteAddr)
	}
	if host == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("host is required"))
		return
	}

	if ok := cluster.GlobalReportHostRegistry().HeartbeatHostWithEndpoint(host, req.ControlEndpoint); !ok {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid host"))
		return
	}
	controlEndpoint, _ := cluster.GlobalReportHostRegistry().ResolveHostControlEndpoint(host)
	if persistErr := cluster.PersistHostEndpoint(host, controlEndpoint, time.Now()); persistErr != nil {
		log.Printf("[CLUSTER] 持久化 host heartbeat 失败 host=%s endpoint=%s err=%v", host, controlEndpoint, persistErr)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":          "ok",
		"host":            host,
		"controlEndpoint": controlEndpoint,
		"time":            time.Now().Format(time.RFC3339),
	})
}

func (s *Service) scanLocalReportsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed. Only POST is supported.", http.StatusMethodNotAllowed)
		return
	}

	host := cluster.LocalHost()
	dir := cluster.ReportsScanDir()
	if raw := strings.TrimSpace(os.Getenv("REPORTS_SCAN_DIR")); raw != "" {
		dir = raw
	}

	added, err := cluster.GlobalReportHostRegistry().SeedHostReportsFromDir(host, dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("scan local reports failed: %v", err))
		return
	}
	if reports, scanErr := cluster.CollectReportCodesFromDir(dir); scanErr != nil {
		log.Printf("[CLUSTER] 持久化 scan-local report->host 前读取目录失败 host=%s dir=%s err=%v", host, dir, scanErr)
	} else if _, persistMapErr := cluster.PersistReportHostMappings(host, reports, time.Now()); persistMapErr != nil {
		log.Printf("[CLUSTER] 持久化 scan-local report->host 失败 host=%s reports=%d err=%v", host, len(reports), persistMapErr)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "ok",
		"host":         host,
		"scanDir":      dir,
		"addedOrMoved": added,
		"totalReports": len(cluster.GlobalReportHostRegistry().Snapshot()),
		"time":         time.Now().Format(time.RFC3339),
	})
}

func (s *Service) executeReportsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed. Only POST is supported.", http.StatusMethodNotAllowed)
		return
	}
	if s.SyncManager == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("sync manager not initialized"))
		return
	}

	var req executeReportsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json body: %v", err))
		return
	}
	if req.PlayerID == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("playerId is required"))
		return
	}
	if len(req.Reports) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("reports is required"))
		return
	}

	execCtx, cancel := context.WithTimeout(r.Context(), 25*time.Minute)
	defer cancel()

	if err := s.SyncManager.ExecuteAssignedReports(execCtx, req.PlayerID, req.Reports); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("execute reports failed: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":   "ok",
		"playerId": req.PlayerID,
		"reports":  req.Reports,
		"time":     time.Now().Format(time.RFC3339),
	})
}

func (s *Service) claimTasksHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed. Only POST is supported.", http.StatusMethodNotAllowed)
		return
	}

	var req claimTasksRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json body: %v", err))
		return
	}

	host := cluster.NormalizeHost(req.Host)
	if host == "" {
		host = cluster.NormalizeHost(r.RemoteAddr)
	}
	if host == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("host is required"))
		return
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 1
	}
	if limit > 16 {
		limit = 16
	}

	leaseSec := req.LeaseSec
	if leaseSec <= 0 {
		leaseSec = 900
	}
	if leaseSec > 3600 {
		leaseSec = 3600
	}

	tasks := cluster.GlobalDispatchTaskQueue().Claim(host, limit, time.Duration(leaseSec)*time.Second)
	resp := make([]pullTaskResponseItem, 0, len(tasks))
	for _, task := range tasks {
		resp = append(resp, pullTaskResponseItem{
			TaskID:   task.ID,
			PlayerID: task.PlayerID,
			Reports:  append([]string(nil), task.Reports...),
			Host:     task.Host,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"host":   host,
		"tasks":  resp,
		"time":   time.Now().Format(time.RFC3339),
	})
}

func (s *Service) ackTaskHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed. Only POST is supported.", http.StatusMethodNotAllowed)
		return
	}

	var req ackTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json body: %v", err))
		return
	}

	host := cluster.NormalizeHost(req.Host)
	if host == "" {
		host = cluster.NormalizeHost(r.RemoteAddr)
	}
	if host == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("host is required"))
		return
	}
	if strings.TrimSpace(req.TaskID) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("taskId is required"))
		return
	}

	updated := cluster.GlobalDispatchTaskQueue().Ack(req.TaskID, host, req.Success, req.Error)
	if !updated {
		writeError(w, http.StatusNotFound, fmt.Errorf("task not found or not claimable"))
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"taskId":  req.TaskID,
		"success": req.Success,
		"time":    time.Now().Format(time.RFC3339),
	})
}

func (s *Service) listTasksHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed. Only GET is supported.", http.StatusMethodNotAllowed)
		return
	}

	tasks := cluster.GlobalDispatchTaskQueue().Snapshot()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"tasks":  tasks,
		"count":  len(tasks),
		"time":   time.Now().Format(time.RFC3339),
	})
}

func TimeTrack(start time.Time, name string) {
	elapsed := time.Since(start)
	log.Printf("%s 花费： %s", name, elapsed)
}

// 雷达接口
func (s *Service) raderHandler(w http.ResponseWriter, r *http.Request) {
	defer TimeTrack(time.Now(), "dashboard")
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed. Only GET is supported.", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query()
	params, err := checkParams(query)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	username := params[0]
	server := params[1]

	beforeSync, err := loadPlayerRadarSnapshot(username, server)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("load player cache failed: %v", err))
		return
	}
	if imagePath, ok := cachedRadarImagePath(beforeSync, time.Now()); ok {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":       "ok",
			"cacheHit":     true,
			"playerId":     beforeSync.ID,
			"pichash":      beforeSync.PicHash,
			"imagePath":    imagePath,
			"path":         r.URL.Path,
			"query":        query,
			"picUpdatedAt": beforeSync.PicUpdatedAt.Format(time.RFC3339),
			"time":         time.Now().Format(time.RFC3339),
		})
		return
	}

	task, created, err := enqueueRadarSyncTask(username, server, "CN")
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("enqueue radar sync task failed: %v", err))
		return
	}

	status := task.Status
	inProgress := task.Status == radarTaskStatusPending || task.Status == radarTaskStatusRunning
	log.Printf("Dashboard UA:%v Args:%v cacheHit=%t enqueue=%s created=%t", r.Header.Get("User-Agent"), params, false, task.TaskKey, created)

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"status":      status,
		"cacheHit":    false,
		"taskId":      task.ID,
		"taskKey":     task.TaskKey,
		"inProgress":  inProgress,
		"created":     created,
		"requestedAt": task.RequestedAt.Format(time.RFC3339),
		"lastError":   task.LastError,
		"retryCount":  task.RetryCount,
		"path":        r.URL.Path,
		"query":       query,
		"time":        time.Now().Format(time.RFC3339),
	})
}

func (s *Service) raderTaskStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed. Only GET is supported.", http.StatusMethodNotAllowed)
		return
	}

	params, err := checkParams(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	taskKey := radarTaskKey(params[0], params[1])
	task, err := getRadarSyncTaskByTaskKey(taskKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("query radar task failed: %v", err))
		return
	}
	if task == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":     "idle",
			"taskKey":    taskKey,
			"inProgress": false,
			"time":       time.Now().Format(time.RFC3339),
		})
		return
	}

	inProgress := task.Status == radarTaskStatusPending || task.Status == radarTaskStatusRunning
	status := task.Status
	if strings.TrimSpace(status) == "" {
		status = "idle"
	}

	payload := map[string]interface{}{
		"status":      status,
		"taskId":      task.ID,
		"taskKey":     task.TaskKey,
		"inProgress":  inProgress,
		"lastError":   task.LastError,
		"retryCount":  task.RetryCount,
		"requestedAt": task.RequestedAt.Format(time.RFC3339),
		"updatedAt":   task.UpdatedAt.Format(time.RFC3339),
		"time":        time.Now().Format(time.RFC3339),
	}
	if task.StartedAt != nil {
		payload["startedAt"] = task.StartedAt.Format(time.RFC3339)
	}
	if task.FinishedAt != nil {
		payload["finishedAt"] = task.FinishedAt.Format(time.RFC3339)
	}

	if status == radarTaskStatusDone {
		if row, loadErr := loadPlayerRadarSnapshot(params[0], params[1]); loadErr == nil {
			if imagePath, ok := cachedRadarImagePath(row, time.Now()); ok {
				payload["cacheHit"] = true
				payload["playerId"] = row.ID
				payload["pichash"] = row.PicHash
				payload["imagePath"] = imagePath
				payload["picUpdatedAt"] = row.PicUpdatedAt.Format(time.RFC3339)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":     "ok",
		"taskStatus": payload,
		"time":       time.Now().Format(time.RFC3339),
	})
}

func checkParams(query url.Values) ([]string, error) {
	// 这里可以添加参数校验逻辑，比如检查必填参数、参数格式等
	username := query.Get("username")
	if username == "" {
		return nil, fmt.Errorf("未提供用户名")
	}
	if len(username) > 18 {
		return nil, fmt.Errorf("非法用户名")

	}
	server := query.Get("server")
	if server == "" {
		return nil, fmt.Errorf("未提供服务器名")
	}
	if _, ok := ServerList[server]; !ok {
		return nil, fmt.Errorf("非法服务器名")
	}
	return []string{username, server}, nil
}

// 这里是一个简单的请求 ID 中间件，确保每个请求都有一个唯一的 ID，方便日志追踪
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = uuid.New().String()
		}
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// accessLogMiddleware 打印 API 请求处理信息（方法、路径、状态码、耗时等）。
func accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}

		requestID := rec.Header().Get("X-Request-Id")
		if requestID == "" {
			requestID = "-"
		}

		log.Printf("[API] rid=%s method=%s path=%s status=%d bytes=%d cost=%s remote=%s ua=%s",
			requestID,
			r.Method,
			r.URL.RequestURI(),
			status,
			rec.bytes,
			time.Since(started),
			r.RemoteAddr,
			r.UserAgent(),
		)
	})
}

// 这里是一个简单的 panic 恢复中间件，确保服务器不会因为单个请求的 panic 而崩溃
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic recovered: %v", rec)
				writeError(w, http.StatusInternalServerError, fmt.Errorf("internal server error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// 写错误响应的工具函数
func writeError(w http.ResponseWriter, code int, err error) {
	resp := ErroStatusResponse{Status: "err", Text: err.Error(), Time: time.Now().Format(time.RFC1123)}
	writeJSON(w, code, resp)
}

// 写 JSON 响应的工具函数
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON error: %v", err)
	}
}

func (s *Service) healthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// 配置监控 API 端口，默认 22027
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthHandler)
	// mux.HandleFunc("/api/monitor/params", s.paramsHandler)

	mux.HandleFunc("/api/ff14/rader", s.raderHandler)
	mux.HandleFunc("/api/ff14/rader/status", s.raderTaskStatusHandler)
	mux.HandleFunc("/api/cluster/reports/register", s.registerReportsHandler)
	mux.HandleFunc("/api/cluster/reports/heartbeat", s.heartbeatReportsHandler)
	mux.HandleFunc("/api/cluster/reports", s.listReportsHostMapHandler)
	mux.HandleFunc("/api/cluster/reports/scan-local", s.scanLocalReportsHandler)
	mux.HandleFunc("/api/cluster/reports/execute", s.executeReportsHandler)
	mux.HandleFunc("/api/cluster/tasks/claim", s.claimTasksHandler)
	mux.HandleFunc("/api/cluster/tasks/ack", s.ackTaskHandler)
	mux.HandleFunc("/api/cluster/tasks", s.listTasksHandler)
	mux.HandleFunc("/api/cluster/unparsed-codes/upsert", s.upsertUnparsedCodesHandler)
	mux.HandleFunc("/api/cluster/unparsed-codes", s.listUnparsedCodesHandler)

	c := cors.New(cors.Options{
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "X-Request-Id"},
		AllowCredentials: true,
	})
	h := c.Handler(mux)
	h = recoverMiddleware(h)
	h = accessLogMiddleware(h)
	h = requestIDMiddleware(h)
	return h
}

func BuildServer(port string, service *Service) *http.Server {
	addr := strings.TrimSpace(port)
	if addr == "" {
		addr = ":22027"
	} else if !strings.HasPrefix(addr, ":") {
		addr = ":" + addr
	}
	if service == nil {
		service = &Service{}
	}
	return &http.Server{Addr: addr, Handler: service.Handler()}
}

func RunServer(port string, service *Service) error {
	srv := BuildServer(port, service)
	return srv.ListenAndServe()
}
