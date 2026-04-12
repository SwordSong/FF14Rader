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
	Host    string   `json:"host"`
	Reports []string `json:"reports"`
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
			reports, err := cluster.CollectReportCodesFromDir(scanDir)
			if err != nil {
				log.Printf("[CLUSTER] 自动注册读取本地 reports 失败 host=%s dir=%s err=%v", host, scanDir, err)
				return
			}
			if err := postRegisterToMaster(registerClient, master, host, reports); err != nil {
				log.Printf("[CLUSTER] 自动注册失败 master=%s host=%s reports=%d err=%v", master, host, len(reports), err)
				return
			}
			log.Printf("[CLUSTER] 自动注册成功 master=%s host=%s reports=%d", master, host, len(reports))
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

// postRegisterToMaster 向主节点发送报告注册请求。
func postRegisterToMaster(client *http.Client, masterEndpoint, host string, reports []string) error {
	payload := masterRegisterPayload{Host: host, Reports: reports}
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
		return fmt.Errorf("register status=%d", resp.StatusCode)
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
