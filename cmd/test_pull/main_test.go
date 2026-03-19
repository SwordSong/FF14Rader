package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/user/ff14rader/internal/analyzer"
	"github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/config"
	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/models"
	"github.com/user/ff14rader/internal/render"
)

func main() {
	// 1. 定义命令行参数
	name := flag.String("name", "", "玩家角色名")
	server := flag.String("server", "", "服务器名称 (如: Moss, Raiden, Lucrezia)")
	region := flag.String("region", "CN", "地区 (CN, JP, NA, EU)")
	flag.Parse()

	if *name == "" || *server == "" {
		log.Println("错误: 请提供玩家角色名和服务器。使用示例: go run cmd/test_pull/main.go -name='你的名字' -server='你的服务器名'")
		os.Exit(1)
	}

	log.Printf("==== 开始为玩家 [%s-%s] 执行全链路拉取测试 ====", *name, *server)

	// 2. 加载配置与初始化数据库
	cfg := config.LoadConfig()
	db.InitDB(cfg.PostgresWriteDSN, cfg.PostgresReadDSN)

	// 3. 初始化 API 客户端与同步管理器
	fflogsClient := api.NewFFLogsClient(cfg.FFLogsClientID, cfg.FFLogsClientSecret)
	syncManager := api.NewSyncManager(fflogsClient)

	// 4. 在数据库中注册/查找玩家
	var player models.Player
	db.DB.Where("name = ? AND server = ?", *name, *server).FirstOrCreate(&player, models.Player{
		Name:   *name,
		Server: *server,
		Region: *region,
	})

	// 5. 执行增量同步 (尝试拉取全量 Report)
	ctx := context.Background()
	err := syncManager.StartIncrementalSync(ctx, player.ID, player.Name, player.Server, player.Region)
	if err != nil {
		log.Fatalf("同步失败: %v", err)
	}

	// 6. 获取同步后的历史记录并统计常用职业
	var reports []models.Report
	db.DB.Where("player_id = ?", player.ID).Find(&reports)

	ana := analyzer.NewGlobalAnalyzer(fflogsClient)
	mostUsedJob := ana.GetMostUsedJob(reports)

	if mostUsedJob == "" {
		log.Println("警告: 未发现该玩家有任何零式副本记录 (Difficulty 101)，请确保该角色在本版本有公开的 Log。")
		return
	}

	// 7. 执行 9 维度综合评估
	perf := ana.CalculateOverallPerformance(player.ID, mostUsedJob, reports)

	// 8. 渲染雷达图
	radarRenderer := render.NewRadarChart(800, 800)
	imagePath := fmt.Sprintf("%s_radar.png", *name)
	err = radarRenderer.Draw(perf, imagePath)
	if err != nil {
		log.Printf("生成雷达图失败: %v", err)
	} else {
		log.Println("==== 全链路拉取测试完成! ====")
		log.Printf("玩家: %s, 常用职业: %s, 生成图片路径: %s", player.Name, mostUsedJob, imagePath)
	}
}
