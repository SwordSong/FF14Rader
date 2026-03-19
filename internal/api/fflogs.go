package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	tokenURL = "https://www.fflogs.com/oauth/token"
	apiURL   = "https://www.fflogs.com/api/v2/client"
)

type FFLogsClient struct {
	clientID     string
	clientSecret string
	accessToken  string
	tokenExpiry  time.Time
	mu           sync.RWMutex
	httpClient   *http.Client

	// 配额统计
	TotalCalls     int     // 累计调用次数
	PointsSpent    float64 // 累计消耗点数
	PointsLimit    int     // 当前限制
	PointsResetIn  int     // 重置剩余时间 (秒)
}

// NewFFLogsClient 创建一个新的 FFLogs 客户端
func NewFFLogsClient(clientID, clientSecret string) *FFLogsClient {
	return &FFLogsClient{
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
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

// GetAccessToken 获取或刷新 OAuth2 令牌
func (c *FFLogsClient) GetAccessToken() (string, error) {
	c.mu.RLock()
	if c.accessToken != "" && time.Now().Before(c.tokenExpiry) {
		token := c.accessToken
		c.mu.RUnlock()
		return token, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// 再次检查以防竞态
	if c.accessToken != "" && time.Now().Before(c.tokenExpiry) {
		return c.accessToken, nil
	}

	data := url.Values{}
	data.Set("grant_type", "client_credentials")

	req, err := http.NewRequest("POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}

	req.SetBasicAuth(c.clientID, c.clientSecret)
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("获取令牌失败: %s", resp.Status)
	}

	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	c.accessToken = result.AccessToken
	// 提前 1 分钟过期以保证安全
	c.tokenExpiry = time.Now().Add(time.Duration(result.ExpiresIn-60) * time.Second)

	return c.accessToken, nil
}

// ExecuteQuery 执行 GraphQL 查询
func (c *FFLogsClient) ExecuteQuery(ctx context.Context, query string, variables map[string]interface{}) (map[string]interface{}, error) {
	token, err := c.GetAccessToken()
	if err != nil {
		return nil, err
	}

	requestBody, err := json.Marshal(map[string]interface{}{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, strings.NewReader(string(requestBody)))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var response struct {
		Data   map[string]interface{} `json:"data"`
		Errors []interface{}          `json:"errors"`
	}

	// 此时可以读取 Body 到字节流，以便解析两次（一次给 response，一次给统计数据）
	// 但实际上 GORM 支持将 rateLimitData 嵌入到 response.Data 中，只需在 Query 里包含它
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, err
	}

	if len(response.Errors) > 0 {
		return nil, fmt.Errorf("FFLogs API 错误: %v", response.Errors)
	}

	// 尝试从 GraphQL 响应中提取配额数据
	c.mu.Lock()
	c.TotalCalls++
	if response.Data != nil {
		if rateLimitData, ok := response.Data["rateLimitData"].(map[string]interface{}); ok {
			// FFLogs V2 API 字段为 pointsSpentThisHour
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
