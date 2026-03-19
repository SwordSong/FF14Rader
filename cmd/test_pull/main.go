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
	"io"
)

func setupLogger() {
	// 创建 logs 目录
	_ = os.MkdirAll("logs", 0755)
	logFile, err := os.OpenFile("logs/sync.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("无法创建日志文件: %v\n", err)
		return
	}
	// 同时输出到控制台和文件
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multiWriter)
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
}

func main() {
	// 1. 定义命令行参数
	name := flag.String("name", "", "玩家角色名")
	server := flag.String("server", "", "服务器名称 (如: Moss, Raiden, Lucrezia)")
	region := flag.String("region", "CN", "地区 (CN, JP, NA, EU)")
	flag.Parse()

	if *name == "" || *server == "" {
		fmt.Println("FF14 Rader Chart CLI Test (Go + FFLogs V2)")
		fmt.Printf("Usage: go run cmd/test_pull/main.go -name=\"角色名\" -server=\"服务器\"\n\n")
		flag.Usage()
		os.Exit(1)
	}

	log.Printf("==== 开始为玩家 [%s-%s] 执行全链路拉取测试 ====", *name, *server)

	// 2. 加载配置、初始化日志与数据库
	cfg := config.LoadConfig()
	setupLogger()
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

	// 5. 执行同步
	ctx := context.Background()
	err := syncManager.StartIncrementalSync(ctx, player.ID, player.Name, player.Server, player.Region)
	if err != nil {
		log.Fatalf("同步数据失败: %v", err)
	}

	// 6. 获取同步后的历史记录并统计常用职业
	var reports []models.Report
	db.DB.Where("player_id = ?", player.ID).Find(&reports)

	if len(reports) == 0 {
		log.Printf("警告: 数据库中未发现玩家 %s 的任何零式副本记录。", *name)
		return
	}

	ana := analyzer.NewGlobalAnalyzer(fflogsClient)
	mostUsedJob := ana.GetMostUsedJob(reports)
	log.Printf("同步成功，共 %d 条记录。主要职业: %s", len(reports), mostUsedJob)

	// 7. 执行综合评估
	perf := ana.CalculateOverallPerformance(player.ID, mostUsedJob, reports)

	// 8. 渲染雷达图
	radarRenderer := render.NewRadarChart(800, 800)
	imagePath := fmt.Sprintf("%s_radar.png", *name)
	err = radarRenderer.Draw(perf, imagePath)
	if err != nil {
		log.Fatalf("渲染失败: %v", err)
	}

	fmt.Printf("\n==== 测试成功! ====\n")
	fmt.Printf("生成雷达图: %s\n", imagePath)
	fmt.Printf("职业详情解析 (SAM Alignment等) 已入库，可检查数据库 performance 表。\n")

	// 9. 打印 API 配额报告
	fflogsClient.PrintQuotaSummary()
}
