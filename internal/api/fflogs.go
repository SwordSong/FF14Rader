package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/user/ff14rader/internal/db"
)

const (
	tokenURL = "https://www.fflogs.com/oauth/token"
	apiURL   = "https://www.fflogs.com/api/v2/client"
)

type FFLogsClient struct {
	mu         sync.RWMutex
	httpClient *http.Client

	// 配额统计
	TotalCalls    int     // 累计调用次数
	PointsSpent   float64 // 累计消耗点数
	PointsLimit   int     // 当前限制
	PointsResetIn int     // 重置剩余时间 (秒)
}

// NewFFLogsClient 创建日志平台 V2 客户端实例。
func NewFFLogsClient() *FFLogsClient {
	return &FFLogsClient{
		httpClient: &http.Client{Timeout: getV2Timeout()},
	}
}

// PrintQuotaSummary 打印配额概览
func (c *FFLogsClient) PrintQuotaSummary() {
	c.mu.RLock()
	defer c.mu.RUnlock()
	fmt.Printf("\n--- FFLogs API 使用概览 ---\n")
	fmt.Printf("累计调用次数: %d\n", c.TotalCalls)
	if c.PointsLimit > 0 {
		fmt.Printf("当前已用额度: %.1f / %d (重置剩余: %ds)\n", c.PointsSpent, c.PointsLimit, c.PointsResetIn)
	}
	fmt.Printf("---------------------------\n")
}

// GetAccessToken 获取或刷新日志平台 V2 访问令牌。
func GetAccessToken() (string, error) {
	var row struct {
		ID    int    `gorm:"column:id"`
		APIID string `gorm:"column:api_id"`
	}

	err := db.DB.Raw(`
	SELECT id, api_id
	FROM fflogskey
	WHERE ver = 2
	AND COALESCE(api_id, '') <> ''
	AND COALESCE(used, 0) < 2900
	ORDER BY COALESCE(used, 0) ASC, id ASC
	LIMIT 1
	`).Scan(&row).Error
	if err != nil {
		return "", err
	}

	apiID := strings.TrimSpace(row.APIID)
	if row.ID <= 0 || apiID == "" {
		return "", fmt.Errorf("fflogskey 中没有可用的 ver=2 api_id")
	}

	_ = db.DB.Exec(`UPDATE fflogskey SET used = COALESCE(used, 0) + 1 WHERE id = ?`, row.ID).Error
	return apiID, nil
}

// ExecuteQuery 执行日志平台图查询请求。
func (c *FFLogsClient) ExecuteQuery(ctx context.Context, query string, variables map[string]interface{}) (map[string]interface{}, error) {
	retries := getV2RetryCount()
	attempts := retries + 1
	requestBody, err := json.Marshal(map[string]interface{}{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, getV2Timeout())
		token, err := GetAccessToken()
		if err != nil {
			cancel()
			return nil, err
		}

		req, err := http.NewRequestWithContext(attemptCtx, "POST", apiURL, strings.NewReader(string(requestBody)))
		if err != nil {
			cancel()
			return nil, err
		}

		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			if !shouldRetryRequest(err, 0) || attempt == attempts {
				return nil, err
			}
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
			continue
		}

		if resp.StatusCode == http.StatusUnauthorized && attempt < attempts {
			resp.Body.Close()
			cancel()
			c.invalidateToken()
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			cancel()
			err = fmt.Errorf("FFLogs API 状态异常: %s", resp.Status)
			lastErr = err
			if !shouldRetryRequest(err, resp.StatusCode) || attempt == attempts {
				return nil, err
			}
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
			continue
		}

		var response struct {
			Data   map[string]interface{} `json:"data"`
			Errors []interface{}          `json:"errors"`
		}

		// 此时可以读取 Body 到字节流，以便解析两次（一次给 response，一次给统计数据）
		// 但实际上 GORM 支持将 rateLimitData 嵌入到 response.Data 中，只需在 Query 里包含它
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			resp.Body.Close()
			cancel()
			lastErr = err
			if attempt == attempts {
				return nil, err
			}
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
			continue
		}
		resp.Body.Close()
		cancel()

		if len(response.Errors) > 0 {
			return nil, fmt.Errorf("FFLogs API 错误: %v", response.Errors)
		}

		// 尝试从 GraphQL 响应中提取配额数据
		c.mu.Lock()
		c.TotalCalls++
		if response.Data != nil {
			if rateLimitData, ok := response.Data["rateLimitData"].(map[string]interface{}); ok {
				// FFLogs 返回fflogs信息。
				if spent, ok := rateLimitData["pointsSpentThisHour"].(float64); ok {
					// 注意：pointsSpentThisHour 是本小时累计，不是单次消耗
					// 我们这里仅更新 client 记录的总配额状态
					c.PointsSpent = spent
				}
				if limit, ok := rateLimitData["limitPerHour"].(float64); ok {
					c.PointsLimit = int(limit)
				}
				if reset, ok := rateLimitData["pointsResetIn"].(float64); ok {
					c.PointsResetIn = int(reset)
				}
			}
		}
		c.mu.Unlock()

		return response.Data, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("FFLogs API 调用失败")
	}
	return nil, lastErr
}

// invalidateToken 清除当前令牌，强制下次调用刷新
func (c *FFLogsClient) invalidateToken() {
	c.mu.Lock()
	c.mu.Unlock()
}

// getV2Timeout 获取V2超时。
func getV2Timeout() time.Duration {
	const defaultTimeout = 120 * time.Second
	if raw := strings.TrimSpace(os.Getenv("FFLOGS_V2_TIMEOUT_SEC")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			return time.Duration(v) * time.Second
		}
	}
	return defaultTimeout
}

// getV2RetryCount 获取V2重试数量。
func getV2RetryCount() int {
	const defaultRetries = 2
	if raw := strings.TrimSpace(os.Getenv("FFLOGS_V2_RETRY")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v >= 0 {
			return v
		}
	}
	return defaultRetries
}

// shouldRetryRequest 判断请求失败后是否应重试。
func shouldRetryRequest(err error, statusCode int) bool {
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	if statusCode >= 500 {
		return true
	}
	return err != nil
}
