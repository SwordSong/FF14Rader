package api

// 本文件实现了 FF14Rader 的 HTTP API 服务，提供监控接口、集群协调接口和雷达图数据接口。
// 设计目标是让 FF14Rader 能够通过 HTTP API 提供实时的监控数据和集群协调功能，同时为未来的雷达图数据接口做好准备。
// 主要组件包括：
// - Service: 定义了所有 HTTP 处理函数，负责处理监控请求、集群报告注册/心跳/查询、任务领取/确认等接口。
// - requestIDMiddleware: 为每个请求生成一个唯一的 ID，方便日志追踪和调试。
// - recoverMiddleware: 捕获处理函数中的 panic，防止服务器崩溃，并返回统一的错误响应。
// - writeError/writeJSON: 工具函数，用于统一格式化错误响应和 JSON 响应。
// - BuildServer/RunServer: 构建和运行 HTTP 服务器，监听指定端口并使用 Service 作为处理器。
// - checkParams: 验证雷达图接口的查询参数是否合法，确保提供必要的用户名和服务器名，并且服务器名在预定义的列表中。
// 通过这些组件，FF14Rader 的 HTTP API 服务能够稳定地提供监控数据和集群协调功能，为多个实例之间的协同工作提供支持。
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/rs/cors"
	"github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/cluster"
	clusterserver "github.com/user/ff14rader/internal/cluster/server"
	"github.com/user/ff14rader/internal/db"
)

// context key for request id
type ctxKey string

const requestIDKey ctxKey = "requestID"

type Service struct{}

var taskWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type registerReportEntry struct {
	Events int    `json:"events"`
	Host   string `json:"host"`
}

type registerReportsRequest struct {
	Host    string          `json:"host"`
	Report  string          `json:"report"`
	Reports json.RawMessage `json:"reports"`
}

type heartbeatRequest struct {
	Host string `json:"host"`
}

type raderRequest struct {
	Username string `json:"username"`
	Server   string `json:"server"`
}

type pullTaskResponseItem struct {
	TaskID   string   `json:"taskId"`
	PlayerID int      `json:"playerId"`
	Reports  []string `json:"reports"`
	Host     string   `json:"host"`
}

type taskWSRequest struct {
	Type      string          `json:"type"`
	RequestID string          `json:"requestId,omitempty"`
	Action    string          `json:"action,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type taskWSResponse struct {
	Type      string      `json:"type"`
	RequestID string      `json:"requestId,omitempty"`
	Action    string      `json:"action,omitempty"`
	Status    string      `json:"status"`
	Error     string      `json:"error,omitempty"`
	Payload   interface{} `json:"payload,omitempty"`
}

type taskWSClaimPayload struct {
	Limit    int `json:"limit"`
	LeaseSec int `json:"leaseSec"`
}

type taskWSAckPayload struct {
	TaskID  string `json:"taskId"`
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

type pendingClaimSeedRow struct {
	PlayerID   int    `gorm:"column:player_id"`
	ReportCode string `gorm:"column:report_code"`
	Events     int    `gorm:"column:events"`
}

// enqueuePendingTasksForHost 当任务队列为空时，根据待处理 fights 为当前 host 补充可领取任务。
func enqueuePendingTasksForHost(host string, limit int) (int, error) {
	h := cluster.NormalizeHost(host)
	if h == "" {
		return 0, nil
	}
	if limit <= 0 {
		limit = 1
	}
	if limit > 128 {
		limit = 128
	}

	rows := make([]pendingClaimSeedRow, 0)
	if err := db.DB.Raw(`
		SELECT
			player_id,
			split_part(master_id, '-', 1) AS report_code,
			COUNT(*)::int AS events
		FROM fight_sync_maps
		WHERE downloaded = false
		GROUP BY player_id, split_part(master_id, '-', 1)
		ORDER BY COUNT(*) DESC
		LIMIT ?
	`, limit*8).Scan(&rows).Error; err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	snapshot := clusterserver.GlobalReportHostRegistry().Snapshot()
	queuedTotal := 0
	for _, row := range rows {
		code := cluster.NormalizeReportCode(row.ReportCode)
		if row.PlayerID == 0 || code == "" {
			continue
		}

		entry, exists := snapshot[code]
		mappedHost := ""
		if exists {
			mappedHost = cluster.NormalizeHost(entry.Host)
		}
		if mappedHost != "" && mappedHost != h {
			continue
		}

		events := row.Events
		if events <= 0 {
			events = 1
		}

		queued, err := clusterserver.GlobalDispatchTaskQueue().EnqueueReports(row.PlayerID, h, []string{code})
		if err != nil {
			continue
		}
		if queued > 0 {
			queuedTotal += queued
			_, _ = clusterserver.GlobalReportHostRegistry().RegisterReportWithEvents(code, h, events)
		}
		if queuedTotal >= limit {
			break
		}
	}

	return queuedTotal, nil
}

func normalizeClaimArgs(limit int, leaseSec int) (int, int) {
	if limit <= 0 {
		limit = 1
	}
	if limit > 16 {
		limit = 16
	}

	if leaseSec <= 0 {
		leaseSec = 900
	}
	if leaseSec > 3600 {
		leaseSec = 3600
	}

	return limit, leaseSec
}

func claimTasksForHost(host string, limit int, leaseSec int) ([]pullTaskResponseItem, error) {
	limit, leaseSec = normalizeClaimArgs(limit, leaseSec)

	tasks := clusterserver.GlobalDispatchTaskQueue().Claim(host, limit, time.Duration(leaseSec)*time.Second)
	if len(tasks) == 0 {
		seeded, seedErr := enqueuePendingTasksForHost(host, limit)
		if seedErr != nil {
			log.Printf("[CLUSTER] 任务队列补入队失败 host=%s err=%v", host, seedErr)
		} else if seeded > 0 {
			log.Printf("[CLUSTER] 任务队列为空，按 host 补入队 host=%s seeded=%d", host, seeded)
			tasks = clusterserver.GlobalDispatchTaskQueue().Claim(host, limit, time.Duration(leaseSec)*time.Second)
		}
	}

	resp := make([]pullTaskResponseItem, 0, len(tasks))
	for _, task := range tasks {
		resp = append(resp, pullTaskResponseItem{
			TaskID:   task.ID,
			PlayerID: task.PlayerID,
			Reports:  append([]string(nil), task.Reports...),
			Host:     task.Host,
		})
	}

	return resp, nil
}

// 注册报告接口，供客户端调用，注册客户端的本地已有 reportCode 列表，并返回当前映射状态
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
	rawReports := strings.TrimSpace(string(req.Reports))
	if rawReports == "" || rawReports == "null" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("reports is required"))
		return
	}

	var legacyReports []string
	if err := json.Unmarshal(req.Reports, &legacyReports); err == nil {
		added, total := clusterserver.GlobalReportHostRegistry().RegisterHostReports(host, legacyReports)
		log.Printf("[CLUSTER] 注册报告(旧格式) host=%s reports=%d addedOrMoved=%d 当前总报告数=%d", host, len(legacyReports), added, total)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":       "ok",
			"host":         host,
			"received":     len(legacyReports),
			"addedOrMoved": added,
			"totalReports": total,
			"time":         time.Now().Format(time.RFC3339),
		})
		return
	}

	var entries []registerReportEntry
	if err := json.Unmarshal(req.Reports, &entries); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid reports payload: %v", err))
		return
	}

	reportCode := cluster.NormalizeReportCode(req.Report)
	if reportCode == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("report is required"))
		return
	}

	effectiveHost := host
	events := 0
	for _, entry := range entries {
		if effectiveHost == "" {
			if h := cluster.NormalizeHost(entry.Host); h != "" {
				effectiveHost = h
			}
		}
		if entry.Events > events {
			events = entry.Events
		}
	}

	added, total := clusterserver.GlobalReportHostRegistry().RegisterReportWithEvents(reportCode, effectiveHost, events)
	log.Printf("[CLUSTER] 注册报告(新格式) host=%s report=%s entries=%d events=%d addedOrMoved=%d 当前总报告数=%d", effectiveHost, reportCode, len(entries), events, added, total)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "ok",
		"host":         effectiveHost,
		"report":       reportCode,
		"received":     1,
		"addedOrMoved": added,
		"totalReports": total,
		"time":         time.Now().Format(time.RFC3339),
	})
}

// 心跳接口，供客户端调用，刷新客户端存活时间
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

	if ok := clusterserver.GlobalReportHostRegistry().HeartbeatHost(host); !ok {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid host"))
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"host":   host,
		"time":   time.Now().Format(time.RFC3339),
	})
}

// 扫描本地 reports 目录并注册到集群中的接口，供客户端调用，触发一次本地报告扫描和注册，适用于启动后或运行中新增报告的情况
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

	added, err := clusterserver.GlobalReportHostRegistry().SeedHostReportsFromDir(host, dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("scan local reports failed: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "ok",
		"host":         host,
		"scanDir":      dir,
		"addedOrMoved": added,
		"totalReports": len(clusterserver.GlobalReportHostRegistry().Snapshot()),
		"time":         time.Now().Format(time.RFC3339),
	})
}

// 任务 WS 长连接接口，客户端按 host 订阅任务可领取通知。
func (s *Service) taskWSHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed. Only GET is supported.", http.StatusMethodNotAllowed)
		return
	}

	host := cluster.NormalizeHost(r.URL.Query().Get("host"))
	if host == "" {
		host = cluster.NormalizeHost(r.Header.Get("X-Cluster-Host"))
	}
	if host == "" {
		host = cluster.NormalizeHost(r.RemoteAddr)
	}
	if host == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("host is required"))
		return
	}

	conn, err := taskWSUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[CLUSTER] 任务WS升级失败 host=%s err=%v", host, err)
		return
	}
	defer conn.Close()

	msgCh, unsubscribe := clusterserver.GlobalTaskNotifyHub().Subscribe(host)
	defer unsubscribe()

	log.Printf("[CLUSTER] 任务WS已连接 host=%s subscribers=%d", host, clusterserver.GlobalTaskNotifyHub().SubscriberCount(host))

	conn.SetReadLimit(1024)
	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	})

	var writeMu sync.Mutex
	writeJSON := func(v interface{}) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteJSON(v)
	}
	writePing := func() error {
		writeMu.Lock()
		defer writeMu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteMessage(websocket.PingMessage, []byte("ping"))
	}

	handleWSRequest := func(req taskWSRequest) taskWSResponse {
		action := strings.TrimSpace(strings.ToLower(req.Action))
		resp := taskWSResponse{
			Type:      "response",
			RequestID: strings.TrimSpace(req.RequestID),
			Action:    action,
			Status:    "ok",
		}

		switch action {
		case "get_reports":
			resp.Payload = map[string]interface{}{
				"reports": clusterserver.GlobalReportHostRegistry().Snapshot(),
			}
			return resp
		case "claim_tasks":
			var payload taskWSClaimPayload
			if len(req.Payload) > 0 && string(req.Payload) != "null" {
				if err := json.Unmarshal(req.Payload, &payload); err != nil {
					resp.Status = "err"
					resp.Error = fmt.Sprintf("invalid claim payload: %v", err)
					return resp
				}
			}

			tasks, err := claimTasksForHost(host, payload.Limit, payload.LeaseSec)
			if err != nil {
				resp.Status = "err"
				resp.Error = err.Error()
				return resp
			}
			resp.Payload = map[string]interface{}{
				"host":  host,
				"tasks": tasks,
			}
			return resp
		case "ack_task":
			var payload taskWSAckPayload
			if len(req.Payload) > 0 && string(req.Payload) != "null" {
				if err := json.Unmarshal(req.Payload, &payload); err != nil {
					resp.Status = "err"
					resp.Error = fmt.Sprintf("invalid ack payload: %v", err)
					return resp
				}
			}
			payload.TaskID = strings.TrimSpace(payload.TaskID)
			if payload.TaskID == "" {
				resp.Status = "err"
				resp.Error = "taskId is required"
				return resp
			}

			updated := clusterserver.GlobalDispatchTaskQueue().Ack(payload.TaskID, host, payload.Success, payload.Error)
			if !updated {
				resp.Status = "err"
				resp.Error = "task not found or not claimable"
				return resp
			}

			resp.Payload = map[string]interface{}{
				"taskId":  payload.TaskID,
				"success": payload.Success,
			}
			return resp
		case "get_users_count":
			users := clusterserver.GlobalReportHostRegistry().SnapshotUsers()
			resp.Payload = map[string]interface{}{
				"usersCount": len(users),
			}
			return resp
		case "claim_user":
			playerID, info, claimed := clusterserver.GlobalReportHostRegistry().ClaimUser(host)
			if claimed {
				info.PlayerID = playerID
			}
			resp.Payload = map[string]interface{}{
				"host":    host,
				"claimed": claimed,
				"user":    info,
			}
			return resp
		default:
			resp.Status = "err"
			resp.Error = fmt.Sprintf("unsupported action: %s", action)
			return resp
		}
	}

	connected := clusterserver.TaskNotifyMessage{
		Type: "connected",
		Host: host,
		Time: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeJSON(connected); err != nil {
		log.Printf("[CLUSTER] 任务WS初始化消息发送失败 host=%s err=%v", host, err)
		return
	}

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))

			var req taskWSRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				continue
			}
			if strings.TrimSpace(strings.ToLower(req.Type)) != "request" {
				continue
			}

			resp := handleWSRequest(req)
			if err := writeJSON(resp); err != nil {
				return
			}
		}
	}()

	pingTicker := time.NewTicker(25 * time.Second)
	defer pingTicker.Stop()

	for {
		select {
		case msg, ok := <-msgCh:
			if !ok {
				return
			}
			if err := writeJSON(msg); err != nil {
				log.Printf("[CLUSTER] 任务WS消息发送失败 host=%s err=%v", host, err)
				return
			}
		case <-pingTicker.C:
			if err := writePing(); err != nil {
				log.Printf("[CLUSTER] 任务WS心跳失败 host=%s err=%v", host, err)
				return
			}
		case <-readDone:
			log.Printf("[CLUSTER] 任务WS已断开 host=%s subscribers=%d", host, clusterserver.GlobalTaskNotifyHub().SubscriberCount(host))
			return
		}
	}
}

func TimeTrack(start time.Time, name string) {
	elapsed := time.Since(start)
	log.Printf("%s 花费： %s", name, elapsed)
}

func parseRaderInput(r *http.Request) (url.Values, error) {
	values := make(url.Values)
	contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))

	if strings.Contains(contentType, "application/json") {
		var payload raderRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			if err == io.EOF {
				return nil, fmt.Errorf("未提供请求体")
			}
			return nil, fmt.Errorf("invalid json body: %v", err)
		}
		values.Set("username", strings.TrimSpace(payload.Username))
		values.Set("server", strings.TrimSpace(payload.Server))
		return values, nil
	}

	if err := r.ParseForm(); err != nil {
		return nil, fmt.Errorf("invalid form body: %v", err)
	}
	values.Set("username", strings.TrimSpace(r.Form.Get("username")))
	values.Set("server", strings.TrimSpace(r.Form.Get("server")))
	return values, nil
}

// 雷达接口 先查表内pichash，如果没有则将玩家信息注册到表内，并返回查询结果
func (s *Service) raderHandler(w http.ResponseWriter, r *http.Request) {
	defer TimeTrack(time.Now(), "dashboard")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed. Only POST is supported.", http.StatusMethodNotAllowed)
		return
	}

	input, err := parseRaderInput(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	params, err := checkParams(input)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	log.Printf("Dashboard UA:%v Args:%v", r.Header.Get("User-Agent"), params)
	player, pichash, err := api.EnsurePlayerID(params[0], params[1], "CN")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 如果是新玩家就返回提示
	if (player.NewPlayer || pichash == "") && player.PlayerID != 0 {
		log.Printf("新玩家注册或无雷达图: %d %s-%s", player.PlayerID, params[0], params[1])
		storedUser := clusterserver.GlobalReportHostRegistry().RegisterUserServer(player)
		if storedUser != false {
			log.Printf("玩家注册到集群服务器用户映射表成功: ID: %d 新玩家：%t %s-%s", player.PlayerID, player.NewPlayer, player.Name, player.Server)
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"status":   "ok",
				"path":     r.URL.Path,
				"query":    input,
				"playerId": player.PlayerID,
				"查询添加":     storedUser,
				"picHash":  pichash,
				"time":     time.Now().Format(time.RFC3339),
			})

			return
		}
		//传入
	} else {
		log.Printf("玩家查询: %d %s-%s", player.PlayerID, player.Name, player.Server)
	}
	//这里是把玩家注册到集群服务器的用户映射表里，后续可以在调度任务时根据 playerID 定向调度到特定实例，或者在查询报告时优先查询这个玩家所在实例的报告
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":   "ok",
		"path":     r.URL.Path,
		"query":    input,
		"playerId": player.PlayerID,
		"picHash":  pichash,
		"time":     time.Now().Format(time.RFC3339),
	})
}

func checkParams(query url.Values) ([]string, error) {
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
	// 查询username+server，返回图片hash
	// 如果没有图片hash就加入待查询用户列表，等待客户端拉取进行V2查询
	// 如果没有用户记录就创建用户记录并加入待查询用户列表，等待客户端拉取进行V2查询,第一阶段
	mux.HandleFunc("/api/ff14/rader", s.raderHandler)

	//reports 相关接口
	mux.HandleFunc("/api/cluster/reports/register", s.registerReportsHandler)
	mux.HandleFunc("/api/cluster/tasks/ws", s.taskWSHandler)

	// 集群相关接口
	mux.HandleFunc("/api/cluster/reports/heartbeat", s.heartbeatReportsHandler)
	mux.HandleFunc("/api/cluster/reports/scan-local", s.scanLocalReportsHandler) //好像没啥用

	// 任务传输仅保留 WS：/api/cluster/tasks/ws

	c := cors.New(cors.Options{
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "X-Request-Id"},
		AllowCredentials: true,
	})
	h := c.Handler(mux)
	h = requestIDMiddleware(h)
	h = recoverMiddleware(h)
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
	log.Println("服务器模式运行中")
	return srv.ListenAndServe()
}
