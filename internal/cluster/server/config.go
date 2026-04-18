package server

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHostTTL                     = 2 * time.Minute
	defaultEvictInterval               = 30 * time.Second
	defaultRegistryPendingSyncInterval = 3 * time.Minute
)

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

// ClusterHostTTL 返回集群主机过期时长。
func ClusterHostTTL() time.Duration {
	return envDurationSeconds("CLUSTER_HOST_TTL_SEC", defaultHostTTL)
}

// ClusterEvictInterval 返回集群剔除间隔。
func ClusterEvictInterval() time.Duration {
	return envDurationSeconds("CLUSTER_EVICT_INTERVAL_SEC", defaultEvictInterval)
}

// ClusterRegistryPendingSyncInterval 返回 registry 未解析事件回填间隔。
func ClusterRegistryPendingSyncInterval() time.Duration {
	return envDurationSeconds("CLUSTER_REGISTRY_PENDING_SYNC_INTERVAL_SEC", defaultRegistryPendingSyncInterval)
}

// ClusterTaskMode 返回集群任务调度模式。
func ClusterTaskMode() string {
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
