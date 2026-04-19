package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	cluster "github.com/user/ff14rader/internal/cluster"
	clusterserver "github.com/user/ff14rader/internal/cluster/server"
	"github.com/user/ff14rader/internal/models"
)

const (
	defaultPullInterval    = 2 * time.Second
	defaultPullBatchSize   = 2
	defaultPullLease       = 20 * time.Minute
	defaultPullExecTimeout = 25 * time.Minute
	defaultTaskWSReconnect = 3 * time.Second
)

type taskPullExecutor interface {
	ExecuteAssignedReports(ctx context.Context, playerID int, reports []string, eventIndexes []int) error
	StartIncrementalSync(ctx context.Context, player models.PlayerLite, region string) error
}

type pullTaskItem struct {
	TaskID       string   `json:"taskId"`
	PlayerID     int      `json:"playerId"`
	Reports      []string `json:"reports"`
	EventIndexes []int    `json:"eventIndexes,omitempty"`
}

type pullClaimUser struct {
	PlayerID  int    `json:"playerId"`
	NewPlayer bool   `json:"newPlayer"`
	Name      string `json:"name"`
	Username  string `json:"username"`
	Server    string `json:"server"`
	Region    string `json:"region"`
}

type pullReportHostEntry struct {
	Events int    `json:"events"`
	Host   string `json:"host"`
}

type pullTaskWSMessage struct {
	Type   string `json:"type"`
	Host   string `json:"host,omitempty"`
	Queued int    `json:"queued,omitempty"`
	Time   string `json:"time,omitempty"`
}

type pullTaskWSRequest struct {
	Type      string          `json:"type"`
	RequestID string          `json:"requestId,omitempty"`
	Action    string          `json:"action,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type pullTaskWSResponse struct {
	Type      string          `json:"type"`
	RequestID string          `json:"requestId,omitempty"`
	Action    string          `json:"action,omitempty"`
	Status    string          `json:"status"`
	Error     string          `json:"error,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type taskWSClaimPayload struct {
	Limit    int `json:"limit"`
	LeaseSec int `json:"leaseSec"`
	Capacity int `json:"capacity,omitempty"`
	InFlight int `json:"inFlight,omitempty"`
}

type taskWSAckPayload struct {
	TaskID  string `json:"taskId"`
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

type taskWSClient struct {
	wsURL     string
	host      string
	reconnect time.Duration
	trigger   chan<- string

	mu      sync.Mutex
	conn    *websocket.Conn
	pending map[string]chan pullTaskWSResponse
	nextID  uint64

	writeMu sync.Mutex
}

func pulledTaskSummary(reports []string, eventIndexes []int) string {
	reportsCount := len(reports)
	eventCount := len(eventIndexes)
	if reportsCount == 0 {
		return fmt.Sprintf("reports=%d eventIndexes=%d", reportsCount, eventCount)
	}
	first := cluster.NormalizeReportCode(reports[0])
	if reportsCount == 1 {
		return fmt.Sprintf("reports=%d eventIndexes=%d report=%s", reportsCount, eventCount, first)
	}
	return fmt.Sprintf("reports=%d eventIndexes=%d firstReport=%s", reportsCount, eventCount, first)
}

func envIntWithDefault(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func clusterPullEnabled() bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv("CLUSTER_PULL_ENABLED")))
	if raw == "" {
		return true
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func clusterTaskMode() string {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv("CLUSTER_TASK_MODE")))
	switch raw {
	case "push":
		return "push"
	case "pull", "":
		return "pull"
	default:
		return "pull"
	}
}

func clusterPullInterval() time.Duration {
	return time.Duration(envIntWithDefault("CLUSTER_PULL_INTERVAL_SEC", int(defaultPullInterval.Seconds()))) * time.Second
}

func clusterPullBatchSize() int {
	v := envIntWithDefault("CLUSTER_PULL_BATCH", defaultPullBatchSize)
	if v > 16 {
		return 16
	}
	return v
}

func clusterHostCapacity() int {
	v := envIntWithDefault("CLUSTER_HOST_CAPACITY", 0)
	if v <= 0 {
		v = runtime.NumCPU()
	}
	if v <= 0 {
		v = 1
	}
	if v > 64 {
		v = 64
	}
	return v
}

func readLoadAvg1() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return -1
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return -1
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return -1
	}
	if v < 0 {
		return -1
	}
	return v
}

func currentHostCapacity() (int, float64, float64) {
	base := clusterHostCapacity()
	capacity := base
	loadAvg1 := readLoadAvg1()
	loadPerCore := -1.0

	cpus := runtime.NumCPU()
	if cpus <= 0 {
		cpus = 1
	}

	if loadAvg1 >= 0 {
		loadPerCore = loadAvg1 / float64(cpus)
		switch {
		case loadPerCore >= 1.5:
			capacity = maxInt(1, base/3)
		case loadPerCore >= 1.0:
			capacity = maxInt(1, base/2)
		case loadPerCore >= 0.7:
			capacity = maxInt(1, (base*3)/4)
		}
	}

	if capacity <= 0 {
		capacity = 1
	}
	if capacity > 64 {
		capacity = 64
	}
	return capacity, loadAvg1, loadPerCore
}

func maxInt(a, b int) int {
	if a >= b {
		return a
	}
	return b
}

func clusterPullLease() time.Duration {
	return time.Duration(envIntWithDefault("CLUSTER_PULL_LEASE_SEC", int(defaultPullLease.Seconds()))) * time.Second
}

func clusterPullExecTimeout() time.Duration {
	return time.Duration(envIntWithDefault("CLUSTER_PULL_EXEC_TIMEOUT_SEC", int(defaultPullExecTimeout.Seconds()))) * time.Second
}

func clusterTaskWSEnabled() bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv("CLUSTER_TASK_WS_ENABLED")))
	if raw == "" {
		return true
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func clusterTaskWSReconnect() time.Duration {
	seconds := envIntWithDefault("CLUSTER_TASK_WS_RECONNECT_SEC", int(defaultTaskWSReconnect.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return time.Duration(seconds) * time.Second
}

func clusterTaskWSURL(master, host string) (string, error) {
	master = strings.TrimSpace(master)
	if master == "" {
		return "", fmt.Errorf("empty master endpoint")
	}

	parsed, err := url.Parse(master)
	if err != nil {
		return "", err
	}

	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	switch scheme {
	case "https":
		parsed.Scheme = "wss"
	default:
		parsed.Scheme = "ws"
	}
	parsed.Path = "/api/cluster/tasks/ws"
	query := parsed.Query()
	query.Set("host", cluster.NormalizeHost(host))
	parsed.RawQuery = query.Encode()
	parsed.Fragment = ""

	return parsed.String(), nil
}

func newTaskWSClient(master, host string, trigger chan<- string) (*taskWSClient, error) {
	wsURL, err := clusterTaskWSURL(master, host)
	if err != nil {
		return nil, err
	}

	return &taskWSClient{
		wsURL:     wsURL,
		host:      cluster.NormalizeHost(host),
		reconnect: clusterTaskWSReconnect(),
		trigger:   trigger,
		pending:   make(map[string]chan pullTaskWSResponse),
	}, nil
}

func (c *taskWSClient) sendTrigger(reason string) {
	if c == nil || c.trigger == nil {
		return
	}
	select {
	case c.trigger <- reason:
	default:
	}
}

func (c *taskWSClient) ensureConnected() error {
	if c == nil {
		return fmt.Errorf("ws client is nil")
	}

	c.mu.Lock()
	if c.conn != nil {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	conn, resp, err := websocket.DefaultDialer.Dial(c.wsURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return err
	}

	conn.SetReadLimit(64 * 1024)
	conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	conn.SetPingHandler(func(appData string) error {
		if err := conn.SetReadDeadline(time.Now().Add(120 * time.Second)); err != nil {
			return err
		}
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
	})
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	})

	c.mu.Lock()
	if c.conn != nil {
		c.mu.Unlock()
		_ = conn.Close()
		return nil
	}
	c.conn = conn
	c.mu.Unlock()

	log.Printf("[CLUSTER] 任务WS已连接 ws=%s", c.wsURL)
	go c.readLoop(conn)
	return nil
}

func (c *taskWSClient) readLoop(conn *websocket.Conn) {
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			c.setDisconnected(conn, err)
			return
		}

		_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))

		var base struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &base); err != nil {
			continue
		}

		switch strings.TrimSpace(strings.ToLower(base.Type)) {
		case "response":
			var resp pullTaskWSResponse
			if err := json.Unmarshal(raw, &resp); err != nil {
				continue
			}
			c.deliverResponse(resp)
		case "task_available":
			c.sendTrigger("ws_task_available")
		case "connected":
			var msg pullTaskWSMessage
			if err := json.Unmarshal(raw, &msg); err == nil {
				log.Printf("[CLUSTER] 任务WS握手成功 host=%s", msg.Host)
			}
			c.sendTrigger("ws_connected")
		}
	}
}

func (c *taskWSClient) deliverResponse(resp pullTaskWSResponse) {
	requestID := strings.TrimSpace(resp.RequestID)
	if requestID == "" {
		return
	}

	c.mu.Lock()
	ch, ok := c.pending[requestID]
	if ok {
		delete(c.pending, requestID)
	}
	c.mu.Unlock()

	if !ok {
		return
	}

	select {
	case ch <- resp:
	default:
	}
}

func (c *taskWSClient) removePending(requestID string) {
	id := strings.TrimSpace(requestID)
	if id == "" {
		return
	}

	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *taskWSClient) setDisconnected(conn *websocket.Conn, err error) {
	c.mu.Lock()
	if c.conn != conn {
		c.mu.Unlock()
		return
	}
	c.conn = nil
	pending := c.pending
	c.pending = make(map[string]chan pullTaskWSResponse)
	c.mu.Unlock()

	for _, ch := range pending {
		close(ch)
	}
	_ = conn.Close()
	log.Printf("[CLUSTER] 任务WS断开，等待重连 ws=%s err=%v", c.wsURL, err)
}

func (c *taskWSClient) request(action string, payload interface{}, timeout time.Duration) (json.RawMessage, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	if err := c.ensureConnected(); err != nil {
		return nil, err
	}

	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	reqID := fmt.Sprintf("r-%d", atomic.AddUint64(&c.nextID, 1))
	respCh := make(chan pullTaskWSResponse, 1)

	c.mu.Lock()
	conn := c.conn
	c.pending[reqID] = respCh
	c.mu.Unlock()

	if conn == nil {
		c.removePending(reqID)
		return nil, fmt.Errorf("ws not connected")
	}

	req := pullTaskWSRequest{
		Type:      "request",
		RequestID: reqID,
		Action:    strings.TrimSpace(strings.ToLower(action)),
		Payload:   payloadRaw,
	}

	c.writeMu.Lock()
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err = conn.WriteJSON(req)
	c.writeMu.Unlock()
	if err != nil {
		c.removePending(reqID)
		c.setDisconnected(conn, err)
		return nil, err
	}

	select {
	case resp, ok := <-respCh:
		if !ok {
			return nil, fmt.Errorf("ws disconnected")
		}
		if strings.TrimSpace(strings.ToLower(resp.Status)) != "ok" {
			msg := strings.TrimSpace(resp.Error)
			if msg == "" {
				msg = "ws response status is not ok"
			}
			return nil, fmt.Errorf("%s", msg)
		}
		return resp.Payload, nil
	case <-time.After(timeout):
		c.removePending(reqID)
		return nil, fmt.Errorf("ws request timeout action=%s", action)
	}
}

func (c *taskWSClient) close() {
	if c == nil {
		return
	}

	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	pending := c.pending
	c.pending = make(map[string]chan pullTaskWSResponse)
	c.mu.Unlock()

	for _, ch := range pending {
		close(ch)
	}
	if conn != nil {
		_ = conn.Close()
	}
}

func syncReportHostRegistryFromMasterWS(ws *taskWSClient) error {
	if ws == nil {
		return fmt.Errorf("ws client is nil")
	}

	raw, err := ws.request("get_reports", nil, 10*time.Second)
	if err != nil {
		return err
	}

	var payload struct {
		Reports map[string]pullReportHostEntry `json:"reports"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return err
		}
	}

	for code, entry := range payload.Reports {
		_, _ = clusterserver.GlobalReportHostRegistry().RegisterReportWithEvents(code, entry.Host, entry.Events)
	}

	return nil
}

func claimTasksFromMasterWS(ws *taskWSClient, limit int, lease time.Duration, capacity int, inFlight int) ([]pullTaskItem, error) {
	if ws == nil {
		return nil, fmt.Errorf("ws client is nil")
	}

	raw, err := ws.request("claim_tasks", taskWSClaimPayload{Limit: limit, LeaseSec: int(lease.Seconds()), Capacity: capacity, InFlight: inFlight}, 12*time.Second)
	if err != nil {
		return nil, err
	}

	var payload struct {
		Host  string         `json:"host"`
		Tasks []pullTaskItem `json:"tasks"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, err
		}
	}

	log.Printf("[CLUSTER] claim 成功(ws) host=%s tasks=%d", payload.Host, len(payload.Tasks))
	return payload.Tasks, nil
}

func ackTaskToMasterWS(ws *taskWSClient, taskID string, success bool, errText string) {
	if ws == nil {
		return
	}
	if strings.TrimSpace(taskID) == "" {
		return
	}

	_, err := ws.request("ack_task", taskWSAckPayload{
		TaskID:  strings.TrimSpace(taskID),
		Success: success,
		Error:   strings.TrimSpace(errText),
	}, 8*time.Second)
	if err != nil {
		log.Printf("[CLUSTER] task ack(ws) failed task=%s err=%v", taskID, err)
	}
}

// claimPlayerLiteTasksFromMasterWS 从主节点WS接口拉取玩家信息任务。
func claimPlayerLiteTasksFromMasterWS(ws *taskWSClient, limit int, lease time.Duration) (models.PlayerLite, error) {
	if ws == nil {
		return models.PlayerLite{}, fmt.Errorf("ws client is nil")
	}

	raw, err := ws.request("get_users_count", nil, 8*time.Second)
	if err != nil {
		return models.PlayerLite{}, err
	}

	var usersPayload struct {
		UsersCount int `json:"usersCount"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &usersPayload); err != nil {
			return models.PlayerLite{}, err
		}
	}
	if usersPayload.UsersCount <= 0 {
		return models.PlayerLite{}, nil
	}

	raw, err = ws.request("claim_user", nil, 10*time.Second)
	if err != nil {
		return models.PlayerLite{}, err
	}

	var payload struct {
		Host    string        `json:"host"`
		Claimed bool          `json:"claimed"`
		User    pullClaimUser `json:"user"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return models.PlayerLite{}, err
		}
	}
	if !payload.Claimed {
		return models.PlayerLite{}, nil
	}

	name := strings.TrimSpace(payload.User.Name)
	if name == "" {
		name = strings.TrimSpace(payload.User.Username)
	}

	return models.PlayerLite{
		PlayerID:  payload.User.PlayerID,
		NewPlayer: payload.User.NewPlayer,
		Name:      name,
		Server:    strings.TrimSpace(payload.User.Server),
		Region:    strings.TrimSpace(payload.User.Region),
	}, nil
}

// StartClusterTaskPullLoop 启动集群任务拉取循环。
func StartClusterTaskPullLoop(executor taskPullExecutor) {
	if executor == nil {
		log.Printf("[CLUSTER] 任务拉取未启动: executor 为空")
		return
	}
	if !clusterPullEnabled() {
		log.Printf("[CLUSTER] 任务拉取未启动: CLUSTER_PULL_ENABLED=false")
		return
	}
	if !clusterTaskWSEnabled() {
		log.Printf("[CLUSTER] 任务拉取未启动: 已禁用HTTP回退，需启用 CLUSTER_TASK_WS_ENABLED")
		return
	}
	mode := clusterTaskMode()
	if mode != "pull" {
		log.Printf("[CLUSTER] 任务拉取未启动: CLUSTER_TASK_MODE=%s", mode)
		return
	}
	master := ClusterMasterEndpoint()
	if master == "" {
		log.Printf("[CLUSTER] 任务拉取未启动: 未配置主节点地址")
		return
	}

	host := cluster.LocalHost()
	interval := clusterPullInterval()
	if interval < 1*time.Second {
		interval = 1 * time.Second
	}
	batch := clusterPullBatchSize()
	lease := clusterPullLease()
	execTimeout := clusterPullExecTimeout()
	var executingTasks int32

	claimTrigger := make(chan string, 1)

	wsClient, err := newTaskWSClient(master, host, claimTrigger)
	if err != nil {
		log.Printf("[CLUSTER] 创建任务WS客户端失败 master=%s host=%s err=%v", master, host, err)
		return
	}
	defer wsClient.close()

	if err := wsClient.ensureConnected(); err != nil {
		log.Printf("[CLUSTER] 初始连接任务WS失败 master=%s host=%s err=%v", master, host, err)
	}

	log.Printf("[CLUSTER] 任务拉取循环启动 master=%s host=%s interval=%s batch=%d lease=%s execTimeout=%s", master, host, interval, batch, lease, execTimeout)
	runOnce := func(trigger string) {
		if strings.TrimSpace(trigger) == "" {
			trigger = "ticker"
		}
		log.Printf("[CLUSTER] 开始拉取任务 master=%s host=%s trigger=%s", master, host, trigger)
		if trigger == "ws_connected" {
			reason := normalizeRegisterReason(registerReasonReconnect)
			reasonPurpose := registerReasonPurpose(reason)
			scanDir := cluster.ReportsScanDir()
			count, regErr := RegisterLocalReportsWithEventsToMaster(master, host, scanDir, registerReasonReconnect)
			if regErr != nil {
				log.Printf("[CLUSTER] 注册报告：%s source=%s master=%s host=%s dir=%s status=failed err=%v", reasonPurpose, reason, master, host, scanDir, regErr)
			} else {
				log.Printf("[CLUSTER] 注册报告：%s source=%s master=%s host=%s reports=%d", reasonPurpose, reason, master, host, count)
			}
		}

		if err := syncReportHostRegistryFromMasterWS(wsClient); err != nil {
			log.Printf("[CLUSTER] 同步报告映射失败(ws) master=%s host=%s err=%v", master, host, err)
		}

		capacity, loadAvg1, loadPerCore := currentHostCapacity()
		inFlight := int(atomic.LoadInt32(&executingTasks))
		reqLimit := batch
		if capacity > reqLimit {
			reqLimit = capacity
		}
		if reqLimit > 16 {
			reqLimit = 16
		}
		if loadAvg1 >= 0 {
			log.Printf("[CLUSTER] 动态能力 host=%s requestLimit=%d capacity=%d inFlight=%d loadAvg1=%.2f loadPerCore=%.2f", host, reqLimit, capacity, inFlight, loadAvg1, loadPerCore)
		} else {
			log.Printf("[CLUSTER] 动态能力 host=%s requestLimit=%d capacity=%d inFlight=%d", host, reqLimit, capacity, inFlight)
		}

		// 先处理玩家查询任务，避免被 report 任务执行时长阻塞。
		playerLite, playerClaimErr := claimPlayerLiteTasksFromMasterWS(wsClient, batch, lease)
		if playerClaimErr != nil {
			log.Printf("[CLUSTER] 拉取玩家信息任务失败(ws) master=%s host=%s err=%v", master, host, playerClaimErr)
		} else if playerLite.PlayerID != 0 && strings.TrimSpace(playerLite.Name) != "" && strings.TrimSpace(playerLite.Server) != "" {
			log.Printf("[CLUSTER] 拉取玩家信息任务成功 host=%s playerId=%d name=%s server=%s", host, playerLite.PlayerID, playerLite.Name, playerLite.Server)
			region := strings.TrimSpace(playerLite.Region)
			if region == "" {
				region = "CN"
			}
			if err := executor.StartIncrementalSync(context.Background(), playerLite, region); err != nil {
				log.Printf("[CLUSTER] 玩家增量同步失败 host=%s playerId=%d err=%v", host, playerLite.PlayerID, err)
			}
		}

		tasks, taskClaimErr := claimTasksFromMasterWS(wsClient, reqLimit, lease, capacity, inFlight)
		if taskClaimErr != nil {
			log.Printf("[CLUSTER] 拉取任务失败(ws) master=%s host=%s err=%v", master, host, taskClaimErr)
		}
		// if len(tasks) == 0 {
		// 	log.Printf("[CLUSTER] 本轮未领取到 reportCode 任务 host=%s", host)
		// }
		if len(tasks) > 0 {
			for _, task := range tasks {
				if task.PlayerID == 0 || len(task.Reports) == 0 || strings.TrimSpace(task.TaskID) == "" {
					ackTaskToMasterWS(wsClient, task.TaskID, false, "invalid task payload")
					continue
				}

				summary := pulledTaskSummary(task.Reports, task.EventIndexes)
				log.Printf("[CLUSTER] 拉取任务成功 host=%s task=%s player=%d %s", host, task.TaskID, task.PlayerID, summary)
				atomic.AddInt32(&executingTasks, 1)
				execCtx, cancel := context.WithTimeout(context.Background(), execTimeout)
				err = executor.ExecuteAssignedReports(execCtx, task.PlayerID, task.Reports, task.EventIndexes)
				cancel()
				atomic.AddInt32(&executingTasks, -1)

				if err != nil {
					log.Printf("[CLUSTER] 任务执行失败 host=%s task=%s err=%v", host, task.TaskID, err)
					ackTaskToMasterWS(wsClient, task.TaskID, false, err.Error())
					continue
				}
				log.Printf("[CLUSTER] 任务执行完成 host=%s task=%s", host, task.TaskID)
				ackTaskToMasterWS(wsClient, task.TaskID, true, "")
			}
		}

	}

	// 启动前先同步一轮
	runOnce("startup")
	// 启动定时拉取循环（WS 失败时仍可兜底）
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			runOnce("ticker")
		case trigger := <-claimTrigger:
			runOnce(trigger)
		}
	}
}
