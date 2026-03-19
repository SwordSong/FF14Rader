package db

import (
	"log"

	"github.com/user/ff14rader/internal/models"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/plugin/dbresolver"
)

var DB *gorm.DB

func InitDB(writeDSN, readDSN string) {
	var err error

	// 1. 先连接 Master 库 (写库)
	DB, err = gorm.Open(postgres.Open(writeDSN), &gorm.Config{
		PrepareStmt: false, // 禁用全局预编译语句，解决某些 PostgreSQL 环境下的 prepared statement name is already in use 错误
	})
	if err != nil {
		log.Fatalf("无法连接数据库 (Master): %v", err)
	}

	// 2. 配置读写分离 (dbresolver)
	// 使用 dbresolver 插件，读操作将分发给 readDSN
	err = DB.Use(dbresolver.Register(dbresolver.Config{
		Sources:  []gorm.Dialector{postgres.Open(writeDSN)},
		Replicas: []gorm.Dialector{postgres.Open(readDSN)},
		Policy:   dbresolver.RandomPolicy{}, // 随机读策略
	}, &models.Player{}, &models.Report{}, &models.Performance{}, &models.FightCache{}, &models.FightSyncMap{}))
	if err != nil {
		log.Fatalf("无法注册读写分离插件 (dbresolver): %v", err)
	}

	// 3. 自动迁移表结构 (在 Master 执行)
	log.Println("正在 Master 库执行表结构迁移...")
	err = DB.AutoMigrate(&models.Player{}, &models.Report{}, &models.Performance{}, &models.FightCache{}, &models.FightSyncMap{})
	if err != nil {
		log.Fatalf("数据库迁移失败: %v", err)
	}
	log.Println("数据库表结构迁移成功。")
}
