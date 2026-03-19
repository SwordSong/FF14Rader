package jobs

import (
	"context"

	"github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/models"
)

// BLMAnalyzer 黑魔导士 (Black Mage) 专项分析
type BLMAnalyzer struct {
	client *api.FFLogsClient
}

func NewBLMAnalyzer(client *api.FFLogsClient) *BLMAnalyzer {
	return &BLMAnalyzer{client: client}
}

func (a *BLMAnalyzer) JobName() string {
	return "BlackMage"
}

// Analyze 分析黑魔的数据对齐情况
// 核心关注：
// 1. 激情咏唱 (Sharpcast) 的利用率 (7.0 之前适用，7.0 后需关注悖论和瞬发利用)
// 2. 爆发期 (Manafont/魔力泉) 的利用
// 3. 星灵易位 (Transposition) 在转场中的使用
func (a *BLMAnalyzer) Analyze(ctx context.Context, reportID string, fightID int) (*models.Performance, error) {
	// 复杂的 GraphQL 事件查询可以在此处实现
	// 暂时返回一个 mock 结构，展示扩展性
	return &models.Performance{
		Job:   "BlackMage",
		Burst: 88.5, // 假设根据魔力泉期间的技能覆盖计算
	}, nil
}
