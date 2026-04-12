package server

import (
	"log"
	"time"
)

// StartRegistryEvictLoop 启动服务端主机过期剔除循环。
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
