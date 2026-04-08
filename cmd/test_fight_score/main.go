// test_fight_score 是手工评分脚本：
// 1) 可基于本地 fight_*_analysis.json 做单场预览评分；
// 2) 也可按 player/report/fight 从数据库批量重算并回写评分字段；
// 3) 用于排查评分公式、权重和分项结果是否符合预期。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/user/ff14rader/internal/config"
	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/scoring"
	"gorm.io/datatypes"
)

type fightRef struct {
	MasterID string
	FightID  int
}

type scoreView struct {
	MasterID            string         `json:"master_id"`
	FightID             int            `json:"fight_id"`
	Job                 string         `json:"job"`
	ScoreActorName      string         `json:"score_actor_name"`
	ChecklistAbs        float64        `json:"checklist_abs"`
	ChecklistConfidence float64        `json:"checklist_confidence"`
	ChecklistAdj        float64        `json:"checklist_adj"`
	SuggestionPenalty   float64        `json:"suggestion_penalty"`
	UtilityScore        float64        `json:"utility_score"`
	SurvivalPenalty     float64        `json:"survival_penalty"`
	JobModuleScore      float64        `json:"job_module_score"`
	BattleScore         float64        `json:"battle_score"`
	FightWeight         float64        `json:"fight_weight"`
	WeightedBattleScore float64        `json:"weighted_battle_score"`
	ScoredAt            time.Time      `json:"scored_at"`
	RawModuleMetrics    datatypes.JSON `json:"raw_module_metrics"`
}

func main() {
	var (
		playerID     = flag.Uint("player-id", 0, "player id")
		reportCode   = flag.String("code", "", "report code")
		fightID      = flag.Int("fight-id", 0, "fight id (0 = score all fights in report)")
		limit        = flag.Int("limit", 0, "max fights when --fight-id=0 (0 = all)")
		analysisFile = flag.String("analysis-file", "", "path to fight_*_analysis.json for local preview mode")
		actorName    = flag.String("actor-name", "", "actor name in analysis json (required when analysis file has multiple actors)")
		kill         = flag.Bool("kill", false, "preview: mark fight as kill")
		fightPercent = flag.Float64("fight-percentage", 0, "preview: remaining boss hp percent (e.g. 40.93 or 4093)")
		bossPercent  = flag.Float64("boss-percentage", 0, "preview: fallback remaining boss hp percent")
		showMetrics  = flag.Bool("show-metrics", false, "print raw_module_metrics JSON")
	)
	flag.Parse()

	scorer := scoring.NewServiceFromEnv()
	if *analysisFile != "" {
		preview, err := scorer.PreviewScoreFromAnalysisFileWithContext(*analysisFile, scoring.PreviewFightContext{
			ActorName:       *actorName,
			Kill:            *kill,
			FightPercentage: *fightPercent,
			BossPercentage:  *bossPercent,
		})
		if err != nil {
			log.Fatalf("preview score failed: %v", err)
		}
		printPreview(preview, *showMetrics)
		return
	}

	if *playerID == 0 {
		log.Fatalf("missing --player-id")
	}
	if *reportCode == "" {
		log.Fatalf("missing --code")
	}

	cfg := config.LoadConfig()
	if cfg.PostgresWriteDSN == "" || cfg.PostgresReadDSN == "" {
		log.Fatalf("Postgres DSN missing (POSTGRES_WRITE_DSN/POSTGRES_READ_DSN)")
	}
	db.InitDB(cfg.PostgresWriteDSN, cfg.PostgresReadDSN)

	fights, err := loadFightRefs(*playerID, *reportCode, *fightID, *limit)
	if err != nil {
		log.Fatalf("load fight refs failed: %v", err)
	}
	if len(fights) == 0 {
		log.Fatalf("no fights found for report %s", *reportCode)
	}

	for _, fight := range fights {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err := scorer.ScoreFight(ctx, *playerID, *reportCode, fight.FightID)
		cancel()
		if err != nil {
			log.Printf("[fail] %s: %v", fight.MasterID, err)
			continue
		}

		view, err := loadScoreView(*playerID, fight.MasterID)
		if err != nil {
			log.Printf("[warn] score written but fetch failed %s: %v", fight.MasterID, err)
			continue
		}

		printScore(view, *showMetrics)
	}
}

func loadFightRefs(playerID uint, reportCode string, fightID, limit int) ([]fightRef, error) {
	codeJSON, _ := json.Marshal([]string{reportCode})
	q := db.DB.Table("fight_sync_maps").
		Select("master_id, fight_id").
		Where("player_id = ? AND source_ids @> ?", playerID, datatypes.JSON(codeJSON)).
		Order("fight_id asc")
	if fightID > 0 {
		q = q.Where("fight_id = ?", fightID)
	} else if limit > 0 {
		q = q.Limit(limit)
	}
	var refs []fightRef
	if err := q.Find(&refs).Error; err != nil {
		return nil, err
	}
	return refs, nil
}

func loadScoreView(playerID uint, masterID string) (*scoreView, error) {
	var out scoreView
	if err := db.DB.Table("fight_sync_maps").
		Select("master_id, fight_id, job, score_actor_name, checklist_abs, checklist_confidence, checklist_adj, suggestion_penalty, utility_score, survival_penalty, job_module_score, battle_score, fight_weight, weighted_battle_score, scored_at, raw_module_metrics").
		Where("player_id = ? AND master_id = ?", playerID, masterID).
		First(&out).Error; err != nil {
		return nil, err
	}
	return &out, nil
}

func printScore(view *scoreView, showMetrics bool) {
	fmt.Printf("\n[评分] %s\n", view.MasterID)
	fmt.Printf("职业=%s 玩家=%s 战斗ID=%d\n", view.Job, view.ScoreActorName, view.FightID)
	fmt.Printf("清单绝对分=%.2f 可信度=%.2f 清单修正分=%.2f\n", view.ChecklistAbs, view.ChecklistConfidence, view.ChecklistAdj)
	printChecklistAlgorithm(view.RawModuleMetrics, view.ChecklistConfidence, view.ChecklistAbs)
	printChecklistRuleScores(view.RawModuleMetrics)
	fmt.Printf("建议惩罚=%.2f 团辅贡献分=%.2f 生存惩罚=%.2f\n", view.SuggestionPenalty, view.UtilityScore, view.SurvivalPenalty)
	fmt.Printf("职业模块分=%.2f 战斗分=%.2f 战斗权重=%.2f 加权战斗分=%.2f\n", view.JobModuleScore, view.BattleScore, view.FightWeight, view.WeightedBattleScore)
	fmt.Printf("评分时间=%s\n", view.ScoredAt.Format(time.RFC3339))
	if showMetrics {
		fmt.Printf("原始模块指标(raw_module_metrics)=%s\n", string(view.RawModuleMetrics))
	}
}

func printPreview(view *scoring.PreviewScore, showMetrics bool) {
	fmt.Printf("\n[预览评分] %s-%d (%s)\n", view.ReportCode, view.FightID, view.FightName)
	fmt.Printf("职业=%s 玩家=%s\n", view.Job, view.ActorName)
	fmt.Printf("清单绝对分=%.2f 可信度=%.2f 清单修正分=%.2f\n", view.ChecklistAbs, view.ChecklistConfidence, view.ChecklistAdj)
	printChecklistAlgorithm(view.RawModuleMetrics, view.ChecklistConfidence, view.ChecklistAbs)
	printChecklistRuleScores(view.RawModuleMetrics)
	fmt.Printf("建议惩罚=%.2f 团辅贡献分=%.2f 生存惩罚=%.2f\n", view.SuggestionPenalty, view.UtilityScore, view.SurvivalPenalty)
	fmt.Printf("职业模块分=%.2f 战斗分=%.2f 权重=%.2f 加权战斗分=%.2f\n", view.JobModuleScore, view.BattleScore, view.FightWeight, view.WeightedBattleScore)
	if showMetrics {
		fmt.Printf("原始模块指标(raw_module_metrics)=%s\n", string(view.RawModuleMetrics))
	}
}

func printChecklistAlgorithm(raw datatypes.JSON, confidence, checklistAbs float64) {
	comp := scoring.BuildChecklistComputationFromRaw(raw)
	if comp == nil {
		return
	}
	gate := scoring.BuildChecklistGCDGateFromRaw(raw)
	confidenceFactor := 0.7 + 0.3*confidence

	fmt.Println("清单评分算法:")
	fmt.Println("原始分 = (Σ(规则评分×优先级) / Σ优先级) × 100")
	fmt.Println("规则评分 = Σ(需求评分×需求权重) / Σ需求权重")
	fmt.Println("需求评分 = (min(数值/阈值, 1))^指数")
	fmt.Println("阈值/指数: GCD=95/2.4, 增伤常驻=95/2.2, 其他=90/1.6, 且阈值=max(target,默认阈值)")
	fmt.Println("清单绝对分 = 清单原始分 × (0.7 + 0.3×可信度) × GCD门控系数")
	fmt.Println("GCD门控系数 = α + (1-α)×g^β, 标准参数: α=0.15 β=3 g=GCD覆盖率/100")
	fmt.Printf("清单原始分展开: 分子=%.4f 分母=%.4f 原始分=%.2f\n", comp.Numerator, comp.Denominator, comp.RawScore)
	if gate != nil && gate.Found {
		fmt.Printf("GCD门控展开: 覆盖率=%.2f%% g=%.4f 系数=%.4f\n", gate.CoveragePercent, gate.Coverage01, gate.Factor)
	} else if gate != nil {
		fmt.Printf("GCD门控展开: 未识别到覆盖率，系数=%.4f\n", gate.Factor)
	}
	fmt.Printf("清单绝对分展开: 原始分=%.2f 可信度系数=%.4f 最终绝对分=%.2f\n", comp.RawScore, confidenceFactor, checklistAbs)
	for _, rule := range comp.Rules {
		fmt.Printf("%s: 规则评分=%.2f 优先级=%.2f 加权贡献=%.4f\n", rule.Name, rule.Score, rule.Priority, rule.Contribution)
	}
}

func printChecklistRuleScores(raw datatypes.JSON) {
	rules := scoring.BuildChecklistRuleScoresFromRaw(raw)
	if len(rules) == 0 {
		return
	}
	fmt.Println("清单规则明细:")
	for _, rule := range rules {
		fmt.Printf("%s 数值=%.2f 评分=%.2f\n", rule.Name, rule.Percent, rule.Score)
	}
}
