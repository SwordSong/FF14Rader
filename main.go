package main

import (
	"log"

	"github.com/user/ff14rader/internal/api"
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
	fflogsClient := api.NewFFLogsClient(cfg.FFLogsClientID, cfg.FFLogsClientSecret)
	syncManager := api.NewSyncManager(fflogsClient)
	radarRenderer := render.NewRadarChart(800, 800)

	log.Println("FF14Rader 服务正常运行中...")

	// 示例：生成一张测试雷达图
	/*
		testPerf := &models.Performance{
			Output: 85, Burst: 92, Uptime: 98,
			Utility: 70, Survivability: 100, Consistency: 88,
			Progression: 95, Mechanics: 90, Potential: 80,
		}
		err := radarRenderer.Draw(testPerf, "test_radar.png")
		if err != nil {
			log.Printf("生成测试雷达图失败: %v", err)
		} else {
			log.Println("测试雷达图已生成: test_radar.png")
		}
	*/

	// 定时器挂载 (每 6 小时同步一次)
	// c := cron.New()
	// c.AddFunc("@every 6h", func() { syncManager.SyncAllPlayers(ctx) })
	// c.Start()

	// 保持主服务不退出
	select {}
}
