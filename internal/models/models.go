package models

import (
	"time"

	"github.com/lib/pq"
)

// Player 玩家基础信息
type Player struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"size:100;not null" json:"name"`
	Server    string    `gorm:"size:50;not null" json:"server"`
	Region    string    `gorm:"size:20;not null" json:"region"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Reports   []Report  `gorm:"foreignKey:PlayerID" json:"reports"`
}

// Report FFLogs 报告元数据
type Report struct {
	ID                string  `gorm:"primaryKey" json:"id"`
	PlayerID          uint    `gorm:"index" json:"player_id"`
	Title             string  `json:"title"`
	StartTime         int64   `json:"start_time"`
	Duration          int     `json:"duration"` // 毫秒
	FightID           int     `json:"fight_id"`
	Kill              bool    `json:"kill"`
	BossName          string  `json:"boss_name"`
	Job               string  `json:"job"`
	WipeProgress      float64 `json:"wipe_progress"`
	Deaths            int     `json:"deaths"`             // 死亡次数
	VulnStacks        int     `json:"vuln_stacks"`        // 易伤/机制惩罚堆叠
	AvoidableDamage   int     `json:"avoidable_damage"`   // 可规避伤害次数
	DamageDown        int     `json:"damage_down"`        // 伤害降低 Buff 次数
	Percentile        float64 `json:"percentile"`         // 排名百分比
}

// FightCache 战斗详情缓存表
type FightCache struct {
	ID              string    `gorm:"primaryKey"`          // 格式: reportCode-fightID
	Duration        int       `json:"duration"`            // 毫秒
	Deaths          int       `json:"deaths"`              // 死亡
	VulnStacks      int       `json:"vuln_stacks"`         // 易伤
	AvoidableDamage int       `json:"avoidable_damage"`    // 可规避伤害
	Percentile      float64   `json:"percentile"`          // 排名百分位
	Timestamp       int64     `gorm:"index" json:"timestamp"` // 战斗发生的绝对时间
	Data            []byte    `gorm:"type:bytea" json:"-"` // 原始原始 JSON 以后备扩展使用
}

// FightSyncMap 战斗同步映射表 (用于多用户上传去重)
type FightSyncMap struct {
	ID        uint           `gorm:"primaryKey"`
	MasterID  string         `gorm:"index;size:100"`      // 基准 ID (第一个入库的战斗记录)
	SourceIDs pq.StringArray `gorm:"type:text[];index"`   // 包含的所有原始报告 ID 列表 (PostgreSQL 数组)
	BossName  string         `gorm:"size:50;index"`
	Timestamp int64          `gorm:"index"`               // 战斗开始时间 (Master 的时间)
	Duration  int            `json:"duration"`            // 战斗时长
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
