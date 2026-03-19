package jobs

import (
"context"
"log"

"github.com/user/ff14rader/internal/api"
"github.com/user/ff14rader/internal/models"
)

// SAMAnalyzer 武士细节分析器
type SAMAnalyzer struct {
client *api.FFLogsClient
// 存储武士的核心技能 ID
MidareSetsugekka int // 雪月花
OgiNamikiri      int // 奥义波切
Higanbana        int // 彼岸花
}

func NewSAMAnalyzer(client *api.FFLogsClient) *SAMAnalyzer {
return &SAMAnalyzer{
client:           client,
MidareSetsugekka: 7487,
OgiNamikiri:      25781,
Higanbana:        7489,
}
}

func (s *SAMAnalyzer) JobName() string {
return "Samurai"
}

// Analyze 实现对武士单场战斗的深度分析
// 目前简单展示如何统计爆发期雪月花
func (s *SAMAnalyzer) Analyze(ctx context.Context, reportID string, fightID int) (*models.Performance, error) {
log.Printf("正在执行武士 (SAM) 的专项细节分析 [Report: %s, Fight: %d]...", reportID, fightID)

perf := &models.Performance{
Job: "Samurai",
}

totalMidare := 0
burstMidare := 0 

// 演示逻辑：在实际 API 查询后
// if abilityID == s.MidareSetsugekka { totalMidare++ ... }

if totalMidare > 0 {
perf.Burst = float64(burstMidare) / float64(totalMidare) * 100.0
} else {
perf.Burst = 85.0 // 默认演示分
}

return perf, nil
}
