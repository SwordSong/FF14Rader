package server

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	cluster "github.com/user/ff14rader/internal/cluster"
)

const (
	taskStatusPending = "pending"
	taskStatusClaimed = "claimed"
	taskStatusDone    = "done"
	taskDoneKeepTTL   = 30 * time.Minute
)

// DispatchTask 表示一个待分发或执行中的任务。
type DispatchTask struct {
	ID         string    `json:"id"`
	PlayerID   int       `json:"playerId"`
	Host       string    `json:"host"`
	Reports    []string  `json:"reports"`
	Status     string    `json:"status"`
	ClaimedBy  string    `json:"claimedBy,omitempty"`
	LeaseUntil time.Time `json:"leaseUntil,omitempty"`
	LastError  string    `json:"lastError,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type DispatchTaskQueue struct {
	mu         sync.Mutex
	tasks      map[string]*DispatchTask
	byReport   map[string]string
	nextTaskID int64
}

var globalDispatchTaskQueue = NewDispatchTaskQueue()

// NewDispatchTaskQueue 创建分发任务队列。
func NewDispatchTaskQueue() *DispatchTaskQueue {
	return &DispatchTaskQueue{
		tasks:    make(map[string]*DispatchTask),
		byReport: make(map[string]string),
	}
}

// GlobalDispatchTaskQueue 返回全局分发任务队列。
func GlobalDispatchTaskQueue() *DispatchTaskQueue {
	return globalDispatchTaskQueue
}

func reportTaskKey(playerID int, reportCode string) string {
	return fmt.Sprintf("%d:%s", playerID, cluster.NormalizeReportCode(reportCode))
}

func (q *DispatchTaskQueue) cleanupDoneLocked(now time.Time) {
	for id, task := range q.tasks {
		if task == nil || task.Status != taskStatusDone {
			continue
		}
		if now.Sub(task.UpdatedAt) <= taskDoneKeepTTL {
			continue
		}
		for _, report := range task.Reports {
			delete(q.byReport, reportTaskKey(task.PlayerID, report))
		}
		delete(q.tasks, id)
	}
}

// EnqueueReports 入队报告列表。
func (q *DispatchTaskQueue) EnqueueReports(playerID int, host string, reports []string) (int, error) {
	h := cluster.NormalizeHost(host)
	if playerID == 0 {
		return 0, fmt.Errorf("invalid playerID")
	}
	if h == "" {
		return 0, fmt.Errorf("invalid host")
	}
	if len(reports) == 0 {
		return 0, nil
	}

	now := time.Now()
	queued := 0

	q.mu.Lock()
	defer q.mu.Unlock()
	q.cleanupDoneLocked(now)

	for _, raw := range reports {
		code := cluster.NormalizeReportCode(raw)
		if code == "" {
			continue
		}
		key := reportTaskKey(playerID, code)
		if existingID, ok := q.byReport[key]; ok {
			existing := q.tasks[existingID]
			if existing != nil {
				if existing.Status == taskStatusPending {
					existing.Host = h
					existing.UpdatedAt = now
				}
				continue
			}
			delete(q.byReport, key)
		}

		q.nextTaskID++
		taskID := fmt.Sprintf("t-%d", q.nextTaskID)
		q.tasks[taskID] = &DispatchTask{
			ID:        taskID,
			PlayerID:  playerID,
			Host:      h,
			Reports:   []string{code},
			Status:    taskStatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		}
		q.byReport[key] = taskID
		queued++
	}

	return queued, nil
}

// Claim 领取任务。
func (q *DispatchTaskQueue) Claim(host string, limit int, lease time.Duration) []DispatchTask {
	h := cluster.NormalizeHost(host)
	if h == "" || limit <= 0 {
		return nil
	}
	if lease <= 0 {
		lease = 10 * time.Minute
	}

	now := time.Now()

	q.mu.Lock()
	defer q.mu.Unlock()

	q.cleanupDoneLocked(now)
	for _, task := range q.tasks {
		if task == nil || task.Status != taskStatusClaimed {
			continue
		}
		if !task.LeaseUntil.IsZero() && task.LeaseUntil.After(now) {
			continue
		}
		task.Status = taskStatusPending
		task.ClaimedBy = ""
		task.LeaseUntil = time.Time{}
		task.UpdatedAt = now
	}

	candidates := make([]*DispatchTask, 0)
	for _, task := range q.tasks {
		if task == nil || task.Status != taskStatusPending {
			continue
		}
		if task.Host != h {
			continue
		}
		candidates = append(candidates, task)
	}
	if len(candidates) == 0 {
		return nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
	})

	if limit > len(candidates) {
		limit = len(candidates)
	}

	out := make([]DispatchTask, 0, limit)
	for i := 0; i < limit; i++ {
		task := candidates[i]
		task.Status = taskStatusClaimed
		task.ClaimedBy = h
		task.LeaseUntil = now.Add(lease)
		task.UpdatedAt = now
		out = append(out, DispatchTask{
			ID:         task.ID,
			PlayerID:   task.PlayerID,
			Host:       task.Host,
			Reports:    append([]string(nil), task.Reports...),
			Status:     task.Status,
			ClaimedBy:  task.ClaimedBy,
			LeaseUntil: task.LeaseUntil,
			CreatedAt:  task.CreatedAt,
			UpdatedAt:  task.UpdatedAt,
		})
	}
	return out
}

// Ack 确认任务执行结果。
func (q *DispatchTaskQueue) Ack(taskID, host string, success bool, errText string) bool {
	id := strings.TrimSpace(taskID)
	h := cluster.NormalizeHost(host)
	if id == "" || h == "" {
		return false
	}

	now := time.Now()

	q.mu.Lock()
	defer q.mu.Unlock()

	task, ok := q.tasks[id]
	if !ok || task == nil {
		return false
	}
	if task.ClaimedBy != "" && task.ClaimedBy != h {
		return false
	}

	task.LastError = strings.TrimSpace(errText)
	task.UpdatedAt = now
	if success {
		task.Status = taskStatusDone
		task.ClaimedBy = h
		task.LeaseUntil = time.Time{}
		return true
	}

	task.Status = taskStatusPending
	task.ClaimedBy = ""
	task.LeaseUntil = time.Time{}
	return true
}

// IsReportDone 判断报告是否已完成。
func (q *DispatchTaskQueue) IsReportDone(playerID int, reportCode string) bool {
	key := reportTaskKey(playerID, reportCode)
	if key == "" {
		return false
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	taskID, ok := q.byReport[key]
	if !ok {
		return false
	}
	task := q.tasks[taskID]
	if task == nil {
		delete(q.byReport, key)
		return false
	}
	return task.Status == taskStatusDone
}

// Snapshot 返回任务快照。
func (q *DispatchTaskQueue) Snapshot() []DispatchTask {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now()
	q.cleanupDoneLocked(now)

	out := make([]DispatchTask, 0, len(q.tasks))
	for _, task := range q.tasks {
		if task == nil {
			continue
		}
		out = append(out, DispatchTask{
			ID:         task.ID,
			PlayerID:   task.PlayerID,
			Host:       task.Host,
			Reports:    append([]string(nil), task.Reports...),
			Status:     task.Status,
			ClaimedBy:  task.ClaimedBy,
			LeaseUntil: task.LeaseUntil,
			LastError:  task.LastError,
			CreatedAt:  task.CreatedAt,
			UpdatedAt:  task.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}
