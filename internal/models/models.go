package models

import (
	"time"

	"gorm.io/datatypes"
)

// Player 玩家基础信息
type Player struct {
	ID               int       `gorm:"primaryKey" json:"id"`
	Name             string    `gorm:"size:100;not null" json:"name"`
	Server           string    `gorm:"size:50;not null" json:"server"`
	Region           string    `gorm:"size:20;not null" json:"region"`
	Race             string    `gorm:"size:50" json:"race"`
	Gender           string    `gorm:"size:20" json:"gender"`
	LodestoneID      string    `gorm:"size:50" json:"lodestone_id"`
	CommonJob        string    `gorm:"size:50" json:"common_job"`
	AllReportCodes   []string  `gorm:"type:jsonb;serializer:json" json:"all_report_codes"`
	OutputAbility    float64   `gorm:"default:0" json:"output_ability"`
	BattleAbility    float64   `gorm:"default:0" json:"battle_ability"`
	TeamContribution float64   `gorm:"default:0" json:"team_contribution"`
	ProgressionSpeed float64   `gorm:"default:0" json:"progression_speed"`
	StabilityScore   float64   `gorm:"default:0" json:"stability_score"`
	MechanicsScore   float64   `gorm:"default:0" json:"mechanics_score"`
	PotentialScore   float64   `gorm:"default:0" json:"potential_score"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	Reports          []Report  `gorm:"foreignKey:PlayerID" json:"reports"`
	PicHash          string    `gorm:"size:50" json:"pichash"`
	PicUpdatedAt     time.Time `json:"pic_updated_at"`
}

// Report 返回报告信息。
type Report struct {
	ID             uint           `gorm:"primaryKey"`
	PlayerID       uint           `gorm:"not null;index" json:"player_id"`
	MasterReport   string         `gorm:"size:50;not null;index" json:"master_report"`
	SourceReport   string         `gorm:"size:50;not null;uniqueIndex:idx_reports_player_source" json:"source_report"`
	ParsedAt       time.Time      `json:"parsed_at"`
	ParsedDone     bool           `gorm:"index" json:"parsed_done"`
	Downloaded     bool           `gorm:"index" json:"downloaded"`
	ReportMetadata datatypes.JSON `gorm:"type:jsonb" json:"report_metadata"`
	StartTime      int64          `json:"start_time"`
	EndTime        int64          `json:"end_time"`
	Title          string         `json:"title"`
	CreatedAt      time.Time      `json:"created_at"`
}

// FightCache 战斗详情缓存表
type FightCache struct {
	ID              string  `gorm:"primaryKey"`             // 格式: reportCode-fightID
	Duration        int     `json:"duration"`               // 毫秒
	Deaths          int     `json:"deaths"`                 // 死亡
	VulnStacks      int     `json:"vuln_stacks"`            // 易伤
	AvoidableDamage int     `json:"avoidable_damage"`       // 可规避伤害
	Percentile      float64 `json:"percentile"`             // 排名百分位
	Timestamp       int64   `gorm:"index" json:"timestamp"` // 战斗发生的绝对时间
	Data            []byte  `gorm:"type:bytea" json:"-"`    // 原始原始 JSON 以后备扩展使用
}

// FightSyncMap 战斗同步映射表 (用于多用户上传去重)
type FightSyncMap struct {
	ID                  uint           `gorm:"primaryKey"`
	MasterID            string         `gorm:"index;size:100"`             // 基准 ID (第一个入库的战斗记录)
	SourceIDs           []string       `gorm:"type:jsonb;serializer:json"` // 包含的所有原始报告 ID 列表 (JSON 数组)
	FriendPlayers       []string       `gorm:"column:friendplayers;type:jsonb;serializer:json" json:"friendplayers"`
	FriendPlayersUsable bool           `gorm:"column:friendplayers_usable;default:false;index" json:"friendplayers_usable"`
	PlayerID            uint           `gorm:"index" json:"player_id"`
	Timestamp           int64          `gorm:"index"` // 战斗开始时间 (Master 的时间)
	FightID             int            `json:"fight_id"`
	Kill                bool           `json:"kill"`
	Job                 string         `json:"job"`
	Downloaded          bool           `gorm:"index" json:"downloaded"`
	DownloadedAt        time.Time      `json:"downloaded_at"`
	ParsedDone          bool           `gorm:"index" json:"parsed_done"`
	StartTime           int64          `json:"start_time"`
	EndTime             int64          `json:"end_time"`
	Name                string         `gorm:"size:100;index" json:"name"`
	BossPercentage      float64        `json:"boss_percentage"`
	FightPercentage     float64        `json:"fight_percentage"`
	Floor               string         `gorm:"size:20;index" json:"floor"`
	GameZone            datatypes.JSON `gorm:"type:jsonb" json:"game_zone"`
	Difficulty          int            `json:"difficulty"`
	EncounterID         int            `json:"encounter_id"`

	// 单场评分字段（按 master_id 写入）
	ScoreActorName      string         `gorm:"size:120" json:"score_actor_name"`
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
	RawModuleMetrics    datatypes.JSON `gorm:"type:jsonb" json:"raw_module_metrics"`
	ScoredAt            time.Time      `gorm:"index" json:"scored_at"`
}

// Performance 指标汇总
type Performance struct {
	ID        uint      `gorm:"primaryKey"`
	PlayerID  uint      `gorm:"index"`
	Version   string    `json:"version"` // 如 "7.0x"
	Job       string    `json:"job"`
	UpdatedAt time.Time `json:"updated_at"`

	// 9 维度评分 (0-100)
	Output        float64 `json:"output"`        // 输出能力
	Burst         float64 `json:"burst"`         // 爆发利用
	Uptime        float64 `json:"uptime"`        // 技能覆盖
	Utility       float64 `json:"utility"`       // 团队贡献
	Survivability float64 `json:"survivability"` // 生存能力
	Consistency   float64 `json:"consistency"`   // 稳定度
	Progression   float64 `json:"progression"`   // 开荒表现
	Mechanics     float64 `json:"mechanics"`     // 机制处理
	Potential     float64 `json:"potential"`     // 潜力值
}
