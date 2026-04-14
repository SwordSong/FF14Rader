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
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/cors"
	"github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/cluster"
	clusterserver "github.com/user/ff14rader/internal/cluster/server"
)

// context key for request id
type ctxKey string

const requestIDKey ctxKey = "requestID"

type Service struct{}

type registerReportsRequest struct {
	Host    string   `json:"host"`
	Reports []string `json:"reports"`
}

type heartbeatRequest struct {
	Host string `json:"host"`
}

type claimTasksRequest struct {
	Host     string `json:"host"`
	Limit    int    `json:"limit"`
	LeaseSec int    `json:"leaseSec"`
}

type claimUserRequest struct {
	Host string `json:"host"`
}

type ackTaskRequest struct {
	Host    string `json:"host"`
	TaskID  string `json:"taskId"`
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

type pullTaskResponseItem struct {
	TaskID   string   `json:"taskId"`
	PlayerID int      `json:"playerId"`
	Reports  []string `json:"reports"`
	Host     string   `json:"host"`
}

// 注册报告接口，供客户端调用，注册客户端的 reportCode 列表，并返回当前映射状态
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

	added, total := clusterserver.GlobalReportHostRegistry().RegisterHostReports(host, req.Reports)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "ok",
		"host":         host,
		"received":     len(req.Reports),
		"addedOrMoved": added,
		"totalReports": total,
		"time":         time.Now().Format(time.RFC3339),
	})
}

// 列出所有 reportCode -> host 映射的接口，供客户端调用，获取当前集群中所有报告的分布状态
func (s *Service) listReportsHostMapHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed. Only GET is supported.", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "ok",
		"reports":      clusterserver.GlobalReportHostRegistry().Snapshot(),
		"hostLoad":     clusterserver.GlobalReportHostRegistry().SnapshotLoads(),
		"hostSeenAt":   clusterserver.GlobalReportHostRegistry().SnapshotSeenAt(),
		"reportsCount": len(clusterserver.GlobalReportHostRegistry().Snapshot()),
		"time":         time.Now().Format(time.RFC3339),
	})
}

// 列出所有 playerId -> user/server 映射的接口，供客户端调用，获取当前集群中的玩家路由映射状态
func (s *Service) listUsersMapHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed. Only GET is supported.", http.StatusMethodNotAllowed)
		return
	}

	users := clusterserver.GlobalReportHostRegistry().SnapshotUsers()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":     "ok",
		"users":      users,
		"usersCount": len(users),
		"time":       time.Now().Format(time.RFC3339),
	})
}

// 领取一个玩家映射接口，供客户端调用，领取成功后会立即从服务端删除该记录以避免重复领取
func (s *Service) claimUserHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed. Only POST is supported.", http.StatusMethodNotAllowed)
		return
	}

	var req claimUserRequest
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

	playerID, info, claimed := clusterserver.GlobalReportHostRegistry().ClaimUser(host)
	log.Printf("领取玩家信息成功 host=%s playerId=%d user=%s server=%s", host, playerID, info.Name, info.Server)
	if !claimed {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "ok",
			"host":    host,
			"claimed": false,
			"time":    time.Now().Format(time.RFC3339),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"host":    host,
		"claimed": true,
		"user": map[string]interface{}{
			"playerId": playerID,
			"username": info.Name,
			"server":   info.Server,
		},
		"time": time.Now().Format(time.RFC3339),
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

// 领取任务接口，供客户端调用，领取一批待处理的报告解析任务，并返回任务详情
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

	tasks := clusterserver.GlobalDispatchTaskQueue().Claim(host, limit, time.Duration(leaseSec)*time.Second)
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

// 确认任务接口，供客户端调用，确认一个已领取的任务的完成状态，并更新任务队列
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

	updated := clusterserver.GlobalDispatchTaskQueue().Ack(req.TaskID, host, req.Success, req.Error)
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

// 列出所有任务的接口，供客户端调用，获取当前任务队列的快照状态，主要用于调试和监控
func (s *Service) listTasksHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed. Only GET is supported.", http.StatusMethodNotAllowed)
		return
	}

	tasks := clusterserver.GlobalDispatchTaskQueue().Snapshot()
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

// 雷达接口 先查表内pichash，如果没有则将玩家信息注册到表内，并返回查询结果
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
				"query":    query,
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
		"query":    query,
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
	// 如果没有用户记录就创建用户记录并加入待查询用户列表，等待客户端拉取进行V2查询
	mux.HandleFunc("/api/ff14/rader", s.raderHandler)
	mux.HandleFunc("/api/cluster/users/claim", s.claimUserHandler)

	// 集群相关接口
	mux.HandleFunc("/api/cluster/reports/register", s.registerReportsHandler)
	mux.HandleFunc("/api/cluster/reports/heartbeat", s.heartbeatReportsHandler)
	mux.HandleFunc("/api/cluster/get/reports", s.listReportsHostMapHandler)
	mux.HandleFunc("/api/cluster/get/users", s.listUsersMapHandler)
	mux.HandleFunc("/api/cluster/reports/scan-local", s.scanLocalReportsHandler)
	// 任务相关接口
	mux.HandleFunc("/api/cluster/tasks/claim", s.claimTasksHandler)
	mux.HandleFunc("/api/cluster/tasks/ack", s.ackTaskHandler)
	mux.HandleFunc("/api/cluster/tasks", s.listTasksHandler)

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
