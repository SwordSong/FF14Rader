package analyzer

import (
	"log"
	"math"
	"sort"

	"github.com/user/ff14rader/internal/models"
)

// CalculateOutput 算法: 输出能力 (偏向稳定性，使用中位数和平均值的混合加权)
func (g *GlobalAnalyzer) CalculateOutput(history []models.Report) float64 {
	var logs []float64
	for _, r := range history {
		if r.Kill && r.Percentile > 0 {
			logs = append(logs, r.Percentile)
		}
	}
	if len(logs) == 0 {
		return 0
	}

	sort.Float64s(logs)
	median := logs[len(logs)/2]

	var sum float64
	for _, v := range logs {
		sum += v
	}
	average := sum / float64(len(logs))

	// 稳定性加权: 中位数占 70%，平均值占 30%
	result := (median * 0.7) + (average * 0.3)
	log.Printf("[TRACING] 计算输出能力: 样本量=%d, 中位数=%.2f, 平均值=%.2f, 最终得分=%.2f", len(logs), median, average, result)
	return result
}

// CalculateUptime 算法: 技能覆盖 (由各个职业 Analyzer 具体分析后汇总)
func (g *GlobalAnalyzer) CalculateUptime(activeGCDs, totalPossibleGCDs int) float64 {
	if totalPossibleGCDs == 0 {
		return 0
	}
	return (float64(activeGCDs) / float64(totalPossibleGCDs)) * 100
}

// CalculateBurst 算法: 爆发利用 (2min 团辅窗口内的威力贡献)
func (g *GlobalAnalyzer) CalculateBurst(burstDamage, totalDamage float64) float64 {
	// 正常 2min 循环中爆发期伤害占比应在特定区间
	// 如果由于断层或没对齐导致较低，则得分降低
	return math.Min(100, (burstDamage/totalDamage)*250)
}

// CalculateUtility 算法: 团队贡献 (减伤覆盖关键频率)
func (g *GlobalAnalyzer) CalculateUtility(mitigationCasts, expectedCasts int) float64 {
	if expectedCasts == 0 {
		return 100
	}
	result := math.Min(100, (float64(mitigationCasts)/float64(expectedCasts))*100)
	log.Printf("[TRACING] 计算团队贡献: 实际次数=%d, 预期次数=%d, 最终得分=%.2f", mitigationCasts, expectedCasts, result)
	return result
}

// CalculateSurvivability 算法: 生存能力 (基于战斗时长加权的死亡与失误评估)
func (g *GlobalAnalyzer) CalculateSurvivability(reports []models.Report) float64 {
	if len(reports) == 0 {
		return 0
	}

	var totalDuration int
	var weightedScoreSum float64

	for _, r := range reports {
		// 单场战斗基础分 100
		// 死亡一次扣 20 分，易伤一次扣 5 分，可规避伤害一次扣 2 分
		fightScore := 100.0 - float64(r.Deaths)*20.0 - float64(r.VulnStacks)*5.0 - float64(r.AvoidableDamage)*2.0
		if fightScore < 0 {
			fightScore = 0
		}

		// 按时长加权: Σ(score * duration) / Σ(duration)
		weightedScoreSum += fightScore * float64(r.Duration)
		totalDuration += r.Duration
	}

	if totalDuration == 0 {
		return 0
	}

	result := weightedScoreSum / float64(totalDuration)
	log.Printf("[TRACING] 计算生存能力: 总场次=%d, 总战斗时长=%ds, 加权得分=%.2f", len(reports), totalDuration, result)
	return result
}

// CalculateOverallScore 综合评分算法: 0.6*生存 + 0.3*稳定性 + 0.1*过本率
func (g *GlobalAnalyzer) CalculateOverallScore(survivability, consistency, clearRate float64) float64 {
	result := (survivability * 0.6) + (consistency * 0.3) + (clearRate * 0.1)
	log.Printf("[TRACING] 计算综合评分: 0.6*生存(%.2f) + 0.3*稳定(%.2f) + 0.1*过本率(%.2f) = 最终得分: %.2f", survivability, consistency, clearRate, result)
	return result
}

// CalculateConsistency 算法: 稳定度 (Percentile 的标准差反比例)
func (g *GlobalAnalyzer) CalculateConsistency(reports []models.Report) float64 {
	if len(reports) < 2 {
		return 100
	}

	var sum float64
	var values []float64
	for _, r := range reports {
		if r.Kill {
			values = append(values, r.Percentile)
			sum += r.Percentile
		}
	}

	if len(values) < 2 {
		return 100
	}

	mean := sum / float64(len(values))
	var variance float64
	for _, v := range values {
		variance += math.Pow(v-mean, 2)
	}
	stdDev := math.Sqrt(variance / float64(len(values)))

	// 标准差越小，稳定性越高。
	// 简单映射：stdDev=0 -> 100分, stdDev=20 -> 50分
	score := 100.0 - (stdDev * 2.5)
	if score < 0 {
		score = 0
	}
	log.Printf("[TRACING] 计算稳定度: 样本量=%d, 均值=%.2f, 标准差=%.2f, 稳定度得分=%.2f", len(values), mean, stdDev, score)
	return score
}

// CalculateClearRate 算法: 过本率 (首通锚点模型)
// 排除首通前的开荒数据，仅计算首通后的稳定性。
// 如果首次过本后存在大量 Wipe（如陪练、进老板队），逻辑上依然体现其“稳定性”或在该副本的“容错性”。
func (g *GlobalAnalyzer) CalculateClearRate(reports []models.Report) float64 {
	if len(reports) == 0 {
		return 0
	}

	// 按副本（BossName）分组处理，因为每个副本的首通时间不同
	bossFights := make(map[string][]models.Report)
	for _, r := range reports {
		bossFights[r.BossName] = append(bossFights[r.BossName], r)
	}

	var totalKills, totalSampled int

	for bossName, fights := range bossFights {
		// 1. 找到该副本最早的过本记录时间 (首通锚点)
		var firstKillTime int64 = math.MaxInt64
		hasKill := false

		// 排序保证时间顺序
		sort.Slice(fights, func(i, j int) bool {
			return fights[i].StartTime < fights[j].StartTime
		})

		for _, f := range fights {
			if f.Kill {
				if f.StartTime < firstKillTime {
					firstKillTime = f.StartTime
				}
				hasKill = true
			}
		}

		if !hasKill {
			// 如果该副本从未打过，或者从未过本，暂不计入过本率样本（避免开荒影响）
			continue
		}

		// 2. 只统计首通之后（包含首通当天/那场）的所有尝试
		bossKills := 0
		bossTotal := 0
		for _, f := range fights {
			if f.StartTime >= firstKillTime {
				bossTotal++
				if f.Kill {
					bossKills++
				}
			}
		}

		log.Printf("[TRACING] 副本 [%s] 过本率分析: 首通后总场次=%d, 过本场次=%d", bossName, bossTotal, bossKills)
		totalKills += bossKills
		totalSampled += bossTotal
	}

	if totalSampled == 0 {
		return 0
	}

	result := (float64(totalKills) / float64(totalSampled)) * 100
	log.Printf("[TRACING] 综合过本率 (首通锚点法): 总样本=%d, 总过本=%d, 结果=%.2f%%", totalSampled, totalKills, result)
	return result
}

// CalculateMechanics 算法: 机制处理 (基于可规避伤害和特定 Buff)
func (g *GlobalAnalyzer) CalculateMechanics(reports []models.Report) float64 {
	// 逻辑与 Survivability 类似，但更侧重于不可原谅的机制失误
	return g.CalculateSurvivability(reports) // 暂时复用逻辑
}
