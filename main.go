package main

import (
	"flag"
	"log"

	appapi "github.com/user/ff14rader/api"
	internalapi "github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/cluster"
	"github.com/user/ff14rader/internal/config"
	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/render"
)

func main() {
	runServerFlag := flag.Bool("server", false, "run server role")
	runClientFlag := flag.Bool("client", false, "run client(worker) role")
	flag.Parse()

	runServer := *runServerFlag
	runClient := *runClientFlag
	if !runServer && !runClient {
		// Backward compatible default: run both roles.
		runServer = true
		runClient = true
	}

	log.Println("正在启动 FF14Rader 服务...")
	log.Printf("启动角色: server=%t client=%t", runServer, runClient)

	// 1. 加载配置
	cfg := config.LoadConfig()

	// 2. 初始化数据库 (读写分离)
	db.InitDB(cfg.PostgresWriteDSN, cfg.PostgresReadDSN)

	// 3. 初始化同步管理器与渲染器
	fflogsClient := internalapi.NewFFLogsClient(cfg.FFLogsClientID, cfg.FFLogsClientSecret)
	syncManager := internalapi.NewSyncManager(fflogsClient)
	radarRenderer := render.NewRadarChart(800, 800)
	localHost := cluster.LocalHost()
	scanDir := cluster.ReportsScanDir()
	seeded, seedErr := cluster.GlobalReportHostRegistry().SeedHostReportsFromDir(localHost, scanDir)
	if seedErr != nil {
		log.Printf("[CLUSTER] 本地 reports 扫描失败 host=%s dir=%s err=%v", localHost, scanDir, seedErr)
	} else {
		log.Printf("[CLUSTER] 本地 reports 初始化完成 host=%s dir=%s reports=%d", localHost, scanDir, seeded)
	}

	if runServer {
		cluster.StartRegistryEvictLoop(cluster.GlobalReportHostRegistry())
	}
	if runClient {
		cluster.StartAutoRegisterAndHeartbeat(cluster.GlobalReportHostRegistry())
		syncManager.StartClusterTaskPullLoop()
	}

	if !runServer {
		log.Println("客户端模式运行中（未启动 HTTP 服务）")
		select {}
	}

	log.Println("FF14Rader 服务正常运行中...")
	log.Printf("监控 API 已启动: http://0.0.0.0:%s", cfg.MonitorPort)

	service := &appapi.Service{
		SyncManager:   syncManager,
		RadarRenderer: radarRenderer,
	}
	if err := appapi.RunServer(cfg.MonitorPort, service); err != nil {
		log.Fatalf("监控 API 启动失败: %v", err)
	}

}
