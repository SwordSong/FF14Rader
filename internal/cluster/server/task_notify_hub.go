package server

import (
	"strings"
	"sync"
	"time"

	cluster "github.com/user/ff14rader/internal/cluster"
)

// TaskNotifyMessage 表示发往客户端的任务通知消息。
type TaskNotifyMessage struct {
	Type   string `json:"type"`
	Host   string `json:"host,omitempty"`
	Queued int    `json:"queued,omitempty"`
	Time   string `json:"time,omitempty"`
}

// TaskNotifyHub 维护按 host 订阅的任务通知通道。
type TaskNotifyHub struct {
	mu      sync.RWMutex
	byHost  map[string]map[chan TaskNotifyMessage]struct{}
	bufSize int
}

var globalTaskNotifyHub = NewTaskNotifyHub()

// NewTaskNotifyHub 创建任务通知 Hub。
func NewTaskNotifyHub() *TaskNotifyHub {
	return &TaskNotifyHub{
		byHost:  make(map[string]map[chan TaskNotifyMessage]struct{}),
		bufSize: 16,
	}
}

// GlobalTaskNotifyHub 返回全局任务通知 Hub。
func GlobalTaskNotifyHub() *TaskNotifyHub {
	return globalTaskNotifyHub
}

// Subscribe 订阅指定 host 的任务通知，返回消息通道和取消订阅函数。
func (h *TaskNotifyHub) Subscribe(host string) (<-chan TaskNotifyMessage, func()) {
	normalizedHost := cluster.NormalizeHost(host)
	if normalizedHost == "" {
		ch := make(chan TaskNotifyMessage)
		close(ch)
		return ch, func() {}
	}

	ch := make(chan TaskNotifyMessage, h.bufSize)

	h.mu.Lock()
	listeners, ok := h.byHost[normalizedHost]
	if !ok {
		listeners = make(map[chan TaskNotifyMessage]struct{})
		h.byHost[normalizedHost] = listeners
	}
	listeners[ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			h.mu.Lock()
			if listeners, exists := h.byHost[normalizedHost]; exists {
				if _, present := listeners[ch]; present {
					delete(listeners, ch)
					close(ch)
				}
				if len(listeners) == 0 {
					delete(h.byHost, normalizedHost)
				}
			}
			h.mu.Unlock()
		})
	}

	return ch, unsubscribe
}

// NotifyTaskAvailable 向指定 host 发布“有任务可领取”通知。
func (h *TaskNotifyHub) NotifyTaskAvailable(host string, queued int) int {
	normalizedHost := cluster.NormalizeHost(host)
	if normalizedHost == "" || queued <= 0 {
		return 0
	}

	msg := TaskNotifyMessage{
		Type:   "task_available",
		Host:   normalizedHost,
		Queued: queued,
		Time:   time.Now().UTC().Format(time.RFC3339),
	}

	delivered := 0
	h.mu.RLock()
	listeners := h.byHost[normalizedHost]
	for ch := range listeners {
		select {
		case ch <- msg:
			delivered++
		default:
			// 丢弃拥塞订阅者消息，避免慢消费者阻塞通知分发。
		}
	}
	h.mu.RUnlock()

	return delivered
}

// SubscriberCount 返回指定 host 的当前订阅数量。
func (h *TaskNotifyHub) SubscriberCount(host string) int {
	normalizedHost := strings.TrimSpace(cluster.NormalizeHost(host))
	if normalizedHost == "" {
		return 0
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byHost[normalizedHost])
}
