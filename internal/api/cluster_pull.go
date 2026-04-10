package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/user/ff14rader/internal/cluster"
)

const (
	defaultPullInterval    = 2 * time.Second
	defaultPullBatchSize   = 2
	defaultPullLease       = 20 * time.Minute
	defaultPullExecTimeout = 25 * time.Minute
)

type pullTaskClaimRequest struct {
	Host     string `json:"host"`
	Limit    int    `json:"limit"`
	LeaseSec int    `json:"leaseSec"`
}

type pullTaskAckRequest struct {
	Host    string `json:"host"`
	TaskID  string `json:"taskId"`
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

type pullTaskItem struct {
	TaskID   string   `json:"taskId"`
	PlayerID uint     `json:"playerId"`
	Reports  []string `json:"reports"`
}

type pullTaskClaimResponse struct {
	Status string         `json:"status"`
	Tasks  []pullTaskItem `json:"tasks"`
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

func clusterPullLease() time.Duration {
	return time.Duration(envIntWithDefault("CLUSTER_PULL_LEASE_SEC", int(defaultPullLease.Seconds()))) * time.Second
}

func clusterPullExecTimeout() time.Duration {
	return time.Duration(envIntWithDefault("CLUSTER_PULL_EXEC_TIMEOUT_SEC", int(defaultPullExecTimeout.Seconds()))) * time.Second
}

func (s *SyncManager) claimTasksFromMaster(client *http.Client, master, host string, limit int, lease time.Duration) ([]pullTaskItem, error) {
	payload := pullTaskClaimRequest{Host: host, Limit: limit, LeaseSec: int(lease.Seconds())}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	urlText := strings.TrimRight(master, "/") + "/api/cluster/tasks/claim"
	req, err := http.NewRequest(http.MethodPost, urlText, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("claim status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	if len(raw) == 0 {
		return nil, nil
	}

	var parsed pullTaskClaimResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	if strings.TrimSpace(strings.ToLower(parsed.Status)) != "ok" {
		return nil, fmt.Errorf("claim non-ok status: %s", parsed.Status)
	}
	return parsed.Tasks, nil
}

func (s *SyncManager) ackTaskToMaster(client *http.Client, master, host, taskID string, success bool, errText string) {
	payload := pullTaskAckRequest{Host: host, TaskID: taskID, Success: success, Error: strings.TrimSpace(errText)}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[CLUSTER] task ack marshal failed task=%s err=%v", taskID, err)
		return
	}

	urlText := strings.TrimRight(master, "/") + "/api/cluster/tasks/ack"
	req, err := http.NewRequest(http.MethodPost, urlText, bytes.NewReader(body))
	if err != nil {
		log.Printf("[CLUSTER] task ack request failed task=%s err=%v", taskID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[CLUSTER] task ack failed task=%s err=%v", taskID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		log.Printf("[CLUSTER] task ack non-2xx task=%s status=%d body=%s", taskID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}

// StartClusterTaskPullLoop 启动工作节点主动拉取任务循环（pull 模式）。
func (s *SyncManager) StartClusterTaskPullLoop() {
	if s == nil || !clusterPullEnabled() || clusterTaskMode() != "pull" {
		return
	}

	master := cluster.ClusterMasterEndpoint()
	if master == "" {
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

	go func() {
		claimClient := &http.Client{Timeout: 10 * time.Second}
		ackClient := &http.Client{Timeout: 10 * time.Second}

		runOnce := func() {
			tasks, err := s.claimTasksFromMaster(claimClient, master, host, batch, lease)
			if err != nil {
				log.Printf("[CLUSTER] 拉取任务失败 master=%s host=%s err=%v", master, host, err)
				return
			}
			if len(tasks) == 0 {
				return
			}

			for _, task := range tasks {
				if task.PlayerID == 0 || len(task.Reports) == 0 || strings.TrimSpace(task.TaskID) == "" {
					s.ackTaskToMaster(ackClient, master, host, task.TaskID, false, "invalid task payload")
					continue
				}

				log.Printf("[CLUSTER] 拉取任务成功 host=%s task=%s player=%d reports=%v", host, task.TaskID, task.PlayerID, task.Reports)
				execCtx, cancel := context.WithTimeout(context.Background(), execTimeout)
				err = s.ExecuteAssignedReports(execCtx, task.PlayerID, task.Reports)
				cancel()

				if err != nil {
					log.Printf("[CLUSTER] 任务执行失败 host=%s task=%s err=%v", host, task.TaskID, err)
					s.ackTaskToMaster(ackClient, master, host, task.TaskID, false, err.Error())
					continue
				}
				log.Printf("[CLUSTER] 任务执行完成 host=%s task=%s", host, task.TaskID)
				s.ackTaskToMaster(ackClient, master, host, task.TaskID, true, "")
			}
		}

		runOnce()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			runOnce()
		}
	}()
}
