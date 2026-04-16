package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	cluster "github.com/user/ff14rader/internal/cluster"
	"github.com/user/ff14rader/internal/db"
)

const (
	defaultRegisterTimeout  = 10 * time.Second
	defaultHeartbeatTimeout = 5 * time.Second
	registerReasonInit      = "client_init_local_report"
	registerReasonReconnect = "client_reconnect_local_report"
)

type masterRegisterPayload struct {
	Host    string                    `json:"host"`
	Report  string                    `json:"report"`
	Reports []masterRegisterReportRow `json:"reports"`
	Reason  string                    `json:"reason,omitempty"`
}

type masterRegisterReportRow struct {
	Events int    `json:"events"`
	Host   string `json:"host"`
}

type masterHeartbeatPayload struct {
	Host string `json:"host"`
}

type reportPendingCountRow struct {
	ReportCode string `gorm:"column:report_code"`
	Events     int    `gorm:"column:events"`
}

// StartAutoRegisterAndHeartbeat 启动客户端自动注册与心跳。
func StartAutoRegisterAndHeartbeat() {
	if !ClusterAutoRegisterEnabled() {
		return
	}

	master := ClusterMasterEndpoint()
	host := cluster.LocalHost()
	scanDir := cluster.ReportsScanDir()
	heartbeatInterval := ClusterHeartbeatInterval()
	if heartbeatInterval < 3*time.Second {
		heartbeatInterval = 3 * time.Second
	}

	go func() {
		registerClient := &http.Client{Timeout: defaultRegisterTimeout}
		heartbeatClient := &http.Client{Timeout: defaultHeartbeatTimeout}
		registerWait := ClusterRegisterWait()

		registerOnce := func(reason string) {
			normalizedReason := normalizeRegisterReason(reason)
			count, err := registerLocalReportsWithEvents(registerClient, master, host, scanDir, normalizedReason)
			if err != nil {
				log.Printf("[CLUSTER] 自动注册失败 source=%s master=%s host=%s dir=%s err=%v", normalizedReason, master, host, scanDir, err)
				return
			}
			log.Printf("[CLUSTER] 自动注册成功 source=%s master=%s host=%s reports=%d", normalizedReason, master, host, count)
		}

		heartbeatOnce := func() error {
			return postHeartbeatToMaster(heartbeatClient, master, host)
		}

		if waitErr := waitMasterReady(registerClient, master, registerWait); waitErr != nil {
			log.Printf("[CLUSTER] 主节点就绪等待超时 master=%s wait=%s err=%v", master, registerWait, waitErr)
		}

		registerOnce(registerReasonInit)

		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := heartbeatOnce(); err != nil {
				log.Printf("[CLUSTER] 心跳失败 master=%s host=%s err=%v；尝试重新注册", master, host, err)
				registerOnce(registerReasonReconnect)
				continue
			}
		}
	}()
}

// RegisterLocalReportsWithEventsToMaster 重新上报本地 report 事件数，未解析报告上报 events>0，已完成上报 events=0。
func RegisterLocalReportsWithEventsToMaster(masterEndpoint, host, scanDir, reason string) (int, error) {
	client := &http.Client{Timeout: defaultRegisterTimeout}
	return registerLocalReportsWithEvents(client, masterEndpoint, host, scanDir, reason)
}

func registerLocalReportsWithEvents(client *http.Client, masterEndpoint, host, scanDir, reason string) (int, error) {
	normalizedReason := normalizeRegisterReason(reason)
	reports, err := cluster.CollectReportCodesFromDir(scanDir)
	if err != nil {
		return 0, fmt.Errorf("collect local reports failed: %w", err)
	}
	if len(reports) == 0 {
		return 0, nil
	}

	pendingEvents, err := loadPendingEventsByReport()
	if err != nil {
		return 0, fmt.Errorf("load pending events failed: %w", err)
	}

	seen := make(map[string]struct{}, len(reports))
	normalized := make([]string, 0, len(reports))
	for _, raw := range reports {
		code := cluster.NormalizeReportCode(raw)
		if code == "" {
			continue
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		normalized = append(normalized, code)
	}
	if len(normalized) == 0 {
		return 0, nil
	}

	success := 0
	failed := 0
	var firstErr error
	for _, code := range normalized {
		events := pendingEvents[code]
		if events < 0 {
			events = 0
		}
		if err := postRegisterReportWithEventsToMaster(client, masterEndpoint, host, code, events, normalizedReason); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		success++
	}

	if failed > 0 {
		return success, fmt.Errorf("partial register failed success=%d failed=%d firstErr=%v", success, failed, firstErr)
	}

	return success, nil
}

func normalizeRegisterReason(reason string) string {
	normalized := strings.TrimSpace(strings.ToLower(reason))
	if normalized == "" {
		return "client_unspecified"
	}
	return normalized
}

func loadPendingEventsByReport() (map[string]int, error) {
	rows := make([]reportPendingCountRow, 0)
	if err := db.DB.Raw(`
		SELECT
			split_part(master_id, '-', 1) AS report_code,
			COUNT(*)::int AS events
		FROM fight_sync_maps
		WHERE downloaded = false
		GROUP BY split_part(master_id, '-', 1)
	`).Scan(&rows).Error; err != nil {
		return nil, err
	}

	out := make(map[string]int, len(rows))
	for _, row := range rows {
		code := cluster.NormalizeReportCode(row.ReportCode)
		if code == "" {
			continue
		}
		events := row.Events
		if events < 0 {
			events = 0
		}
		out[code] = events
	}
	return out, nil
}

// waitMasterReady 等待主节点健康检查通过。
func waitMasterReady(client *http.Client, masterEndpoint string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	deadline := time.Now().Add(timeout)
	healthURL := strings.TrimRight(masterEndpoint, "/") + "/healthz"

	for {
		req, err := http.NewRequest(http.MethodGet, healthURL, nil)
		if err == nil {
			resp, doErr := client.Do(req)
			if doErr == nil {
				_ = resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					return nil
				}
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("master not ready: %s", healthURL)
		}
		time.Sleep(1 * time.Second)
	}
}

// postRegisterReportWithEventsToMaster 向主节点发送单条报告注册请求。
func postRegisterReportWithEventsToMaster(client *http.Client, masterEndpoint, host, reportCode string, events int, reason string) error {
	payload := masterRegisterPayload{
		Host:   host,
		Report: reportCode,
		Reports: []masterRegisterReportRow{
			{Events: events, Host: host},
		},
		Reason: normalizeRegisterReason(reason),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	urlText := strings.TrimRight(masterEndpoint, "/") + "/api/cluster/reports/register"
	req, err := http.NewRequest(http.MethodPost, urlText, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("register status=%d report=%s", resp.StatusCode, reportCode)
	}
	return nil
}

// postHeartbeatToMaster 向主节点发送心跳请求。
func postHeartbeatToMaster(client *http.Client, masterEndpoint, host string) error {
	payload := masterHeartbeatPayload{Host: host}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	urlText := strings.TrimRight(masterEndpoint, "/") + "/api/cluster/reports/heartbeat"
	req, err := http.NewRequest(http.MethodPost, urlText, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("heartbeat status=%d", resp.StatusCode)
	}
	return nil
}
