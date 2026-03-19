package analyzer

import (
	"context"
	"log"
	"math"
	"sort"
	"time"

	"github.com/user/ff14rader/internal/analyzer/jobs"
	"github.com/user/ff14rader/internal/api"
	"github.com/user/ff14rader/internal/models"
)

// Analyzer 核心分析接口
type Analyzer interface {
	Analyze(ctx context.Context, reportID string, fightID int) (*models.Performance, error)
	JobName() string
}

// GlobalAnalyzer 分析引擎总管
type GlobalAnalyzer struct {
	client       *api.FFLogsClient
	jobAnalyzers map[string]Analyzer
}

func NewGlobalAnalyzer(client *api.FFLogsClient) *GlobalAnalyzer {
	ga := &GlobalAnalyzer{
		client:       client,
		jobAnalyzers: make(map[string]Analyzer),
	}
	// 注册具体职业分析器
	ga.jobAnalyzers["Samurai"] = jobs.NewSAMAnalyzer(client)
	ga.jobAnalyzers["BlackMage"] = jobs.NewBLMAnalyzer(client)
	return ga
}

// GetMostUsedJob 统计该玩家在该版本所有记录中使用次数最多的职业
func (g *GlobalAnalyzer) GetMostUsedJob(history []models.Report) string {
	if len(history) == 0 {
		return ""
	}

	jobStats := make(map[string]int)
	maxCount := 0
	mostUsedJob := ""

	for _, r := range history {
		if r.Job != "" {
			jobStats[r.Job]++
			if jobStats[r.Job] > maxCount {
				maxCount = jobStats[r.Job]
				mostUsedJob = r.Job
			}
		}
	}

	log.Printf("统计完成: 玩家最常用的职业是 [%s] (记录数: %d)", mostUsedJob, maxCount)
	return mostUsedJob
}

// CalculateOverallPerformance 综合计算 9 维度评分
func (g *GlobalAnalyzer) CalculateOverallPerformance(playerID uint, job string, history []models.Report) *models.Performance {
	log.Printf("正在综合计算 [%s] 的全版本 9 维度水平指标...", job)

	perf := &models.Performance{
		PlayerID:  playerID,
		Job:       job,
		UpdatedAt: time.Now(),
		Version:   "7.0",
	}

	// 1. 输出能力 (Output)
	perf.Output = g.CalculateOutput(history)

	// 2. 稳定度 (Consistency)
	perf.Consistency = g.CalculateConsistency(history)

	// 3. 潜力值 (Potential)
	// 潜力值 = 最好的一次 Percentile 表现，体现该职业的天花板
	var killLogs []float64
	for _, r := range history {
		if r.Kill {
			killLogs = append(killLogs, r.Percentile)
		}
	}
	if len(killLogs) > 0 {
		sort.Float64s(killLogs)
		maxLog := killLogs[len(killLogs)-1]
		perf.Potential = maxLog
		log.Printf("[TRACING] 计算潜力值: 样本量=%d, 最高百分位=%.2f, 最终得分=%.2f", len(killLogs), maxLog, maxLog)
	} else {
		perf.Potential = 0
		log.Printf("[TRACING] 计算潜力值: 未发现过本记录, 得分=0")
	}

	// 4. 生存能力 (Survivability)
	perf.Survivability = g.CalculateSurvivability(history)

	// 5. 团队贡献 (Utility)
	perf.Utility = g.CalculateUtility(5, 5)

	// 6. 过本率 (代替之前的 Progression，或作为整体评价的一部分)
	perf.Progression = g.CalculateClearRate(history)

	// 7. 机制处理 (Mechanics)
	perf.Mechanics = g.CalculateMechanics(history)

	// 8. 技能覆盖 (Uptime) & 爆发利用 (Burst)
	// 这些通常由具体的 JobAnalyzer 对单场 Events 分析后得出，这里取多场平均
	perf.Uptime = 98.0
	perf.Burst = 92.5
	log.Printf("[TRACING] 技能覆盖与爆发利用 (Mocking): Uptime=%.2f, Burst=%.2f", perf.Uptime, perf.Burst)

	return perf
}

func calculateStats(data []float64) (mean, stdDev float64) {
	if len(data) == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range data {
		sum += v
	}
	mean = sum / float64(len(data))
	var sqSum float64
	for _, v := range data {
		sqSum += math.Pow(v-mean, 2)
	}
	stdDev = math.Sqrt(sqSum / float64(len(data)))
	return
}
