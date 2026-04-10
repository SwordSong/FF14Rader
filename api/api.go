package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/cors"
	internalapi "github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/cluster"
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
}

type registerReportsRequest struct {
	Host    string   `json:"host"`
	Reports []string `json:"reports"`
}

type executeReportsRequest struct {
	PlayerID uint     `json:"playerId"`
	Reports  []string `json:"reports"`
}

type heartbeatRequest struct {
	Host string `json:"host"`
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

	added, total := cluster.GlobalReportHostRegistry().RegisterHostReports(host, req.Reports)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "ok",
		"host":         host,
		"received":     len(req.Reports),
		"addedOrMoved": added,
		"totalReports": total,
		"time":         time.Now().Format(time.RFC3339),
	})
}

func (s *Service) listReportsHostMapHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed. Only GET is supported.", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "ok",
		"reports":      cluster.GlobalReportHostRegistry().Snapshot(),
		"hostLoad":     cluster.GlobalReportHostRegistry().SnapshotLoads(),
		"hostSeenAt":   cluster.GlobalReportHostRegistry().SnapshotSeenAt(),
		"reportsCount": len(cluster.GlobalReportHostRegistry().Snapshot()),
		"time":         time.Now().Format(time.RFC3339),
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

	if ok := cluster.GlobalReportHostRegistry().HeartbeatHost(host); !ok {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid host"))
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"host":   host,
		"time":   time.Now().Format(time.RFC3339),
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
	if s.SyncManager == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("sync manager not initialized"))
		return
	}
	query := r.URL.Query()
	params, err := checkParams(query)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx := context.Background()
	if err := s.SyncManager.StartIncrementalSync(ctx, params[0], params[1], "CN"); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to start incremental sync: %v", err))
		return
	}
	log.Printf("Dashboard UA:%v Args:%v", r.Header.Get("User-Agent"), params)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"path":   r.URL.Path,
		"query":  query,
		"time":   time.Now().Format(time.RFC3339),
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

	if s.EnableDashboardAPI {
		mux.HandleFunc("/api/ff14/rader", s.raderHandler)
	}
	mux.HandleFunc("/api/cluster/reports/register", s.registerReportsHandler)
	mux.HandleFunc("/api/cluster/reports/heartbeat", s.heartbeatReportsHandler)
	mux.HandleFunc("/api/cluster/reports", s.listReportsHostMapHandler)
	mux.HandleFunc("/api/cluster/reports/scan-local", s.scanLocalReportsHandler)
	mux.HandleFunc("/api/cluster/reports/execute", s.executeReportsHandler)
	mux.HandleFunc("/api/cluster/tasks/claim", s.claimTasksHandler)
	mux.HandleFunc("/api/cluster/tasks/ack", s.ackTaskHandler)
	mux.HandleFunc("/api/cluster/tasks", s.listTasksHandler)

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
