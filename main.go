package main

import (
	"flag"
	"log"

	"github.com/user/ff14rader/internal/client"
	"github.com/user/ff14rader/internal/config"
	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/server"
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

	if runServer {
		if err := server.StartHTTPServer(cfg.MonitorPort); err != nil {
			log.Fatalf("监控 API 启动失败: %v", err)
		}
	}

	if runClient {
		client.StartWorker()
	}

	if !runServer {
		log.Println("客户端模式运行中（未启动 HTTP 服务）")
		select {}
	}
	if runServer {
		log.Println("服务器模式运行中")
		select {}
	}
}
