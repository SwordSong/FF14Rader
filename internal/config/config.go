package config

import (
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	FFLogsClientID     string
	FFLogsClientSecret string
	PostgresWriteDSN   string // 写库 (Master)
	PostgresReadDSN    string // 读库 (Slave)
	MonitorPort        string // 监控 API 端口
}

// LoadConfig 获取配置。
func LoadConfig() *Config {
	// 尝试从当前目录、父目录或根目录加载 .env
	_ = godotenv.Load()             // current dir
	_ = godotenv.Load("../.env")    // parent
	_ = godotenv.Load("../../.env") // root (from cmd/xxx/)

	// if 返回if信息。
	// 	log.Println("No .env file found, reading from environment variables")
	// }

	return &Config{
		FFLogsClientID:     os.Getenv("FFLOGS_CLIENT_ID"),
		FFLogsClientSecret: os.Getenv("FFLOGS_CLIENT_SECRET"),
		PostgresWriteDSN:   os.Getenv("POSTGRES_WRITE_DSN"),
		PostgresReadDSN:    os.Getenv("POSTGRES_READ_DSN"),
		MonitorPort:        getEnvOrDefault("MONITOR_PORT", "22027"),
	}
}

// getEnvOrDefault 获取环境变量或默认值。
func getEnvOrDefault(key, fallback string) string {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	return val
}
