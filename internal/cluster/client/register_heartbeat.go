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
)

const (
	defaultRegisterTimeout  = 10 * time.Second
	defaultHeartbeatTimeout = 5 * time.Second
)

type masterRegisterPayload struct {
	Host    string                    `json:"host"`
	Report  string                    `json:"report"`
	Reports []masterRegisterReportRow `json:"reports"`
}

type masterRegisterReportRow struct {
	Events int    `json:"events"`
	Host   string `json:"host"`
}

type masterHeartbeatPayload struct {
	Host string `json:"host"`
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

		registerOnce := func() {
			count, err := registerLocalCompletedReports(registerClient, master, host, scanDir)
			if err != nil {
				log.Printf("[CLUSTER] 自动注册失败(本地完成报告) master=%s host=%s dir=%s err=%v", master, host, scanDir, err)
				return
			}
			log.Printf("[CLUSTER] 自动注册成功(本地完成报告) master=%s host=%s reports=%d events=0", master, host, count)
		}

		heartbeatOnce := func() error {
			return postHeartbeatToMaster(heartbeatClient, master, host)
		}

		if waitErr := waitMasterReady(registerClient, master, registerWait); waitErr != nil {
			log.Printf("[CLUSTER] 主节点就绪等待超时 master=%s wait=%s err=%v", master, registerWait, waitErr)
		}

		registerOnce()

		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := heartbeatOnce(); err != nil {
				log.Printf("[CLUSTER] 心跳失败 master=%s host=%s err=%v；尝试重新注册", master, host, err)
				registerOnce()
				continue
			}
		}
	}()
}

// RegisterLocalCompletedReportsToMaster 重新上报本地已存在 report，events=0 表示本地已完成。
func RegisterLocalCompletedReportsToMaster(masterEndpoint, host, scanDir string) (int, error) {
	client := &http.Client{Timeout: defaultRegisterTimeout}
	return registerLocalCompletedReports(client, masterEndpoint, host, scanDir)
}

func registerLocalCompletedReports(client *http.Client, masterEndpoint, host, scanDir string) (int, error) {
	reports, err := cluster.CollectReportCodesFromDir(scanDir)
	if err != nil {
		return 0, fmt.Errorf("collect local reports failed: %w", err)
	}
	if len(reports) == 0 {
		return 0, nil
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
		if err := postRegisterReportWithEventsToMaster(client, masterEndpoint, host, code, 0); err != nil {
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
func postRegisterReportWithEventsToMaster(client *http.Client, masterEndpoint, host, reportCode string, events int) error {
	payload := masterRegisterPayload{
		Host:   host,
		Report: reportCode,
		Reports: []masterRegisterReportRow{
			{Events: events, Host: host},
		},
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
