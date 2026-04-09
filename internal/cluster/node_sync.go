package cluster

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHeartbeatInterval = 30 * time.Second
	defaultHostTTL           = 2 * time.Minute
	defaultEvictInterval     = 30 * time.Second
	defaultRegisterTimeout   = 10 * time.Second
	defaultHeartbeatTimeout  = 5 * time.Second
	defaultRegisterWait      = 20 * time.Second
)

type masterRegisterPayload struct {
	Host    string   `json:"host"`
	Reports []string `json:"reports"`
}

type masterHeartbeatPayload struct {
	Host string `json:"host"`
}

func envDurationSeconds(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return fallback
	}
	return time.Duration(v) * time.Second
}

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

func ClusterMasterEndpoint() string {
	raw := strings.TrimSpace(os.Getenv("CLUSTER_MASTER_ENDPOINT"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("CLUSTER_MASTER_URL"))
	}
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil || strings.TrimSpace(u.Host) == "" {
		return ""
	}
	u.Path = ""
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}

func ClusterAutoRegisterEnabled() bool {
	master := ClusterMasterEndpoint()
	if master == "" {
		return false
	}
	return envBool("CLUSTER_AUTO_REGISTER", true)
}

func ClusterHeartbeatInterval() time.Duration {
	return envDurationSeconds("CLUSTER_HEARTBEAT_INTERVAL_SEC", defaultHeartbeatInterval)
}

func ClusterHostTTL() time.Duration {
	return envDurationSeconds("CLUSTER_HOST_TTL_SEC", defaultHostTTL)
}

func ClusterEvictInterval() time.Duration {
	return envDurationSeconds("CLUSTER_EVICT_INTERVAL_SEC", defaultEvictInterval)
}

func ClusterRegisterWait() time.Duration {
	return envDurationSeconds("CLUSTER_REGISTER_WAIT_SEC", defaultRegisterWait)
}

func StartRegistryEvictLoop(registry *ReportHostRegistry) {
	if registry == nil {
		return
	}
	ttl := ClusterHostTTL()
	interval := ClusterEvictInterval()
	if ttl <= 0 || interval <= 0 {
		return
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			expiredHosts, removedReports := registry.EvictExpiredHosts(ttl)
			if len(expiredHosts) == 0 {
				continue
			}
			log.Printf("[CLUSTER] 节点过期剔除 hosts=%v removedReports=%d ttl=%s", expiredHosts, removedReports, ttl)
		}
	}()
}

func StartAutoRegisterAndHeartbeat(registry *ReportHostRegistry) {
	if registry == nil || !ClusterAutoRegisterEnabled() {
		return
	}

	master := ClusterMasterEndpoint()
	host := LocalHost()
	scanDir := ReportsScanDir()
	heartbeatInterval := ClusterHeartbeatInterval()
	if heartbeatInterval < 3*time.Second {
		heartbeatInterval = 3 * time.Second
	}

	go func() {
		registerClient := &http.Client{Timeout: defaultRegisterTimeout}
		heartbeatClient := &http.Client{Timeout: defaultHeartbeatTimeout}
		registerWait := ClusterRegisterWait()

		registerOnce := func() {
			reports, err := CollectReportCodesFromDir(scanDir)
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
			registry.HeartbeatHost(host)
		}
	}()
}

func waitMasterReady(client *http.Client, masterEndpoint string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = defaultRegisterWait
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

func postRegisterToMaster(client *http.Client, masterEndpoint, host string, reports []string) error {
	payload := masterRegisterPayload{
		Host:    host,
		Reports: reports,
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
		return fmt.Errorf("register status=%d", resp.StatusCode)
	}
	return nil
}

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
