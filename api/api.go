package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/cors"
	internalapi "github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/render"
)

// context key for request id
type ctxKey string

const requestIDKey ctxKey = "requestID"

type Service struct {
	SyncManager   *internalapi.SyncManager
	RadarRenderer *render.RadarChart
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
	return srv.ListenAndServe()
}
