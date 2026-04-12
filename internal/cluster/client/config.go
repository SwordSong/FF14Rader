package client

import (
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHeartbeatInterval = 30 * time.Second
	defaultRegisterWait      = 20 * time.Second
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

// ClusterMasterEndpoint 返回集群主节点端点。
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

// ClusterAutoRegisterEnabled 判断集群自动注册是否启用。
func ClusterAutoRegisterEnabled() bool {
	master := ClusterMasterEndpoint()
	if master == "" {
		return false
	}
	return envBool("CLUSTER_AUTO_REGISTER", true)
}

// ClusterHeartbeatInterval 返回集群心跳间隔。
func ClusterHeartbeatInterval() time.Duration {
	return envDurationSeconds("CLUSTER_HEARTBEAT_INTERVAL_SEC", defaultHeartbeatInterval)
}

// ClusterRegisterWait 返回集群注册等待。
func ClusterRegisterWait() time.Duration {
	return envDurationSeconds("CLUSTER_REGISTER_WAIT_SEC", defaultRegisterWait)
}
