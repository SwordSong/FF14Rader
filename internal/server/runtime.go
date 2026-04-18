package server

import (
	appapi "github.com/user/ff14rader/api"
	clusterserver "github.com/user/ff14rader/internal/cluster/server"
)

// StartHTTPServer 启动服务端角色所需组件并启动 HTTP 服务。
func StartHTTPServer(monitorPort string) error {
	clusterserver.StartRegistryEvictLoop(clusterserver.GlobalReportHostRegistry())
	clusterserver.StartRegistryPendingEventsSyncLoop(clusterserver.GlobalReportHostRegistry())
	service := &appapi.Service{}
	return appapi.RunServer(monitorPort, service)
}
