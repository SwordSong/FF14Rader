package main

import (
	"log"

	appapi "github.com/user/ff14rader/api"
	internalapi "github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/cluster"
	"github.com/user/ff14rader/internal/config"
	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/render"
)

func main() {
	log.Println("正在启动 FF14Rader 服务...")

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
	cluster.StartRegistryEvictLoop(cluster.GlobalReportHostRegistry())
	cluster.StartAutoRegisterAndHeartbeat(cluster.GlobalReportHostRegistry())

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
