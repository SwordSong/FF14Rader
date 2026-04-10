package main

import (
	"flag"
	"log"

	appapi "github.com/user/ff14rader/api"
	internalapi "github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/cluster"
	"github.com/user/ff14rader/internal/config"
	"github.com/user/ff14rader/internal/db"
)

func main() {
	runServerFlag := flag.Bool("server", false, "服务器状态运行，包含 HTTP API 和集群管理")
	runClientFlag := flag.Bool("client", false, "客户端模式运行，仅执行任务拉取、下载与解析")
	flag.Parse()

	runServer := *runServerFlag
	runClient := *runClientFlag
	if !runServer && !runClient {
		// 默认两者都启动
		runServer = true
		runClient = true
	}
	log.Println("正在启动 FF14Rader 服务...")
	log.Printf("启动角色: server=%t client=%t", runServer, runClient)

	// 1. 加载配置
	cfg := config.LoadConfig()

	// 2. 初始化数据库 (读写分离)
	db.InitDB(cfg.PostgresWriteDSN, cfg.PostgresReadDSN)

	var syncManager *internalapi.SyncManager
	if runClient {
		fflogsClient := internalapi.NewFFLogsClient(cfg.FFLogsClientID, cfg.FFLogsClientSecret)
		syncManager = internalapi.NewSyncManager(fflogsClient)
	}

	// server 角色：仅做分发与集群管理。
	if runServer {
		restoredEndpoints, restoreEndpointsErr := cluster.RestorePersistedHostEndpoints(cluster.GlobalReportHostRegistry())
		if restoreEndpointsErr != nil {
			log.Printf("[CLUSTER] 恢复持久化 host->endpoint 映射失败: %v", restoreEndpointsErr)
		} else {
			log.Printf("[CLUSTER] 已恢复持久化 host->endpoint 映射 count=%d", restoredEndpoints)
		}

		restoredReports, restoreReportsErr := cluster.RestorePersistedReportHosts(cluster.GlobalReportHostRegistry())
		if restoreReportsErr != nil {
			log.Printf("[CLUSTER] 恢复持久化 report->host 映射失败: %v", restoreReportsErr)
		} else {
			log.Printf("[CLUSTER] 已恢复持久化 report->host 映射 count=%d", restoredReports)
		}
		cluster.StartRegistryEvictLoop(cluster.GlobalReportHostRegistry())
	}

	// client 角色：仅做注册、拉取任务、下载与解析。
	if runClient {
		localHost := cluster.LocalHost()
		scanDir := cluster.ReportsScanDir()
		seeded, seedErr := cluster.GlobalReportHostRegistry().SeedHostReportsFromDir(localHost, scanDir)
		if seedErr != nil {
			log.Printf("[CLUSTER] 本地 reports 扫描失败 host=%s dir=%s err=%v", localHost, scanDir, seedErr)
		} else {
			log.Printf("[CLUSTER] 本地 reports 初始化完成 host=%s dir=%s reports=%d", localHost, scanDir, seeded)
		}

		cluster.StartAutoRegisterAndHeartbeat(cluster.GlobalReportHostRegistry())
		if syncManager != nil {
			syncManager.StartClusterTaskPullLoop()
		}
	}

	if !runServer {
		log.Println("客户端模式运行中（未启动 HTTP 服务）")
		select {}
	}

	service := &appapi.Service{
		SyncManager:        syncManager,
		EnableDashboardAPI: false,
	}
	if runServer {
		service.StartDispatchTaskWarmupLoop()
	}
	if runServer && syncManager != nil {
		service.StartRadarTaskWorker()
	}

	log.Println("FF14Rader 服务正常运行中...")
	log.Printf("监控 API 已启动: http://0.0.0.0:%s", cfg.MonitorPort)

	if err := appapi.RunServer(cfg.MonitorPort, service); err != nil {
		log.Fatalf("监控 API 启动失败: %v", err)
	}

}
