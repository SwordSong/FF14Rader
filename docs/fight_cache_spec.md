# FF14Rader FightCache 数据格式说明

`FightCache` 表是本项目中用于存储**单场战斗深度详情**的缓存层。它主要负责保存从 FFLogs Table API 批量拉取的原始数据，以便在不重复请求 API 的情况下进行各职业（如武士波切对齐）的专项分析。

## 1. 数据库字段定义 (GORM)

在 [internal/models/models.go](internal/models/models.go) 中定义如下：

| 字段名 | 类型 | 说明 |
| :--- | :--- | :--- |
| `ID` | `string` | **主键**。格式为 `{reportCode}-{fightID}` (例如: `cAQT6ztmbZY2yVB1-35`) |
| `Duration` | `int` | 战斗时长。单位：**毫秒** (ms) |
| `Deaths` | `int` | 当前玩家在该场战斗中的死亡次数 |
| `VulnStacks` | `int` | 当前玩家在该场战斗中获得的易伤/机制惩罚总层数 |
| `AvoidableDamage` | `int` | 当前玩家受到的可规避伤害总次数 |
| `Percentile` | `float64` | 该场战斗采集到的全服排名百分位 (0.0 - 100.0) |
| `Timestamp` | `int64` | **战斗开始时间**。Unix 时间戳（秒级），代表该场战斗发生的绝对时间 |
| `Data` | `[]byte` | **原始 JSON 数据**。存储经过筛选后的局部 Table API 响应 (PostgreSQL `bytea` 类型) |

---

## 2. 核心时间逻辑说明

### 战斗开始时间 (`Timestamp`)
由于一个 `Report` 可能包含持续数小时的多场战斗，我们通过以下公式计算每场战斗的绝对开始时间：
`Timestamp = (Report.startTime / 1000) + (Fight.startTime / 1000)`
- `Report.startTime`: FFLogs 报告的全局开始时刻。
- `Fight.startTime`: 该战斗相对于报告开始时刻的偏移量。

### 原始数据格式 (`Data` 字段)
`Data` 字段目前以 JSON 格式存储了针对该 Fight 过滤后的三类数据快照：
```json
{
  "deaths": [...],   // 包含 ID, 死亡时间, 击杀者等详细记录
  "debuffs": [...],  // 包含易伤 Buff 应用的开始/结束时间、层数
  "avoidable": [...],// 包含具体被哪些可规避技能击中的明细
  "percentile": 85.5 // 该场战斗解析出的百分位
}
```

## 3. 为什么这样设计？
1. **防止限流**：一次拉取全报告详情并按 Fight 拆分后存入 `FightCache`，后续武士的 `Namikiri Alignment` 分析将直接读取 `Data` 字段。
2. **快速回放**：`Timestamp` 字段允许我们按时间轴复盘玩家在一周内的表现趋势。
3. **扩展性**：如果未来需要分析黑魔的瞬发利用率，只需从 `Data` 中增加抓取 `casts` 字段即可，无需修改模型结构。
