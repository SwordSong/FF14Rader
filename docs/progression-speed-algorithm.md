# 开荒速度算法（当前实现口径）

## 结论先行

当前代码的判定是：
- 每个 Boss（按 `encounter_id`）只统计到第一次击杀为止。
- 也就是“首杀前所有尝试 + 首杀那一把”会进入该 Boss 的开荒速度计算。
- 首杀之后的尝试不会继续计入该 Boss 的开荒速度。

## 对应代码位置

- 玩家总开荒速度入口：`refreshPlayerProgressionSpeed`
  - `internal/scoring/service.go`
- 单 Boss 计算函数：`computeEncounterProgressionScore`
  - `internal/scoring/service.go`
- 只纳入完整 8 人队伍名单数据：`friendplayers_usable = true`
  - `internal/scoring/service.go`
  - `internal/api/sync.go`
- 8 人名单判定：`len(friendPlayers) == 8`
  - `internal/api/sync.go`

## 输入过滤

开荒速度只使用 `fight_sync_maps` 中满足以下条件的记录：

1. `player_id = 当前玩家`
2. `friendplayers_usable = true`
3. `encounter_id > 0`

说明：`friendplayers_usable` 目前在同步阶段按“队伍名单长度为 8”判定。

## 分组与排序

1. 按 `encounter_id` 分组，每组代表一个 Boss。
2. 每组内按 `timestamp asc, id asc` 排序，保证时间顺序稳定。

## 单 Boss 计分逻辑

设该 Boss 的有序尝试序列为 $a_1, a_2, ..., a_n$。

### 1) 进度定义

每次尝试的进度：

$$
progress_i =
\begin{cases}
100, & \text{若}~kill_i=true \\
\text{fightProgressPercent}(fight\_percentage_i, boss\_percentage_i), & \text{否则}
\end{cases}
$$

其中 `fightProgressPercent` 会优先使用 `fight_percentage`，缺失时回退 `boss_percentage`，并归一到 $[0,100]$。

### 2) 有效尝试次数（队伍变化补偿）

初始化：
- `effectiveAttempts = 0`
- `bestProgress = 0`

对每次尝试累加一个成本 `cost_i`：

$$
effectiveAttempts = \sum cost_i
$$

默认 `cost_i = 1`。

若本次与上一次队伍签名不同（队伍变化），根据进度变化做补偿：

$$
\Delta progress = progress_i - progress_{i-1}
$$

- 若 $\Delta progress \ge 10$，`cost_i = 0.5`
- 若 $\Delta progress \le -10$，`cost_i = 1.15`
- 否则 `cost_i = 0.85`

### 3) 首杀截断（核心）

当遇到第一条 `kill=true` 的记录时：

1. 该条记录先参与本轮累加。
2. 然后立即 `break` 结束循环。

因此，首杀后的记录不会进入该 Boss 的开荒速度计算。

### 4) 单 Boss 基础分

先做下限保护：

$$
effectiveAttempts = max(effectiveAttempts, 1)
$$

基础分：

$$
base = \frac{100}{1 + 0.22 \cdot (effectiveAttempts - 1)}
$$

如果该 Boss 尚未击杀（没有遇到首杀）：

$$
progressFactor = 0.35 + 0.65 \cdot clamp01\left(\frac{bestProgress}{100}\right)
$$

$$
base = base \cdot 0.85 \cdot progressFactor
$$

### 5) 单 Boss 权重

$$
weight = 0.7 + 0.3 \cdot clamp01\left(\frac{bestProgress}{100}\right)
$$

若该 Boss 已击杀（有首杀）：

$$
weight = 1
$$

最终返回：

- `encounterScore = clamp100(base)`
- `encounterWeight = weight`

## 跨 Boss 汇总

设所有 Boss 的结果为 $(S_k, W_k)$：

$$
progression\_speed =
\begin{cases}
clamp100\left(\frac{\sum_k S_k W_k}{\sum_k W_k}\right), & \sum_k W_k > 0 \\
0, & \text{否则}
\end{cases}
$$

并做两位小数四舍五入；若结果在 $(0,1)$，提升到 `1`。

## 验证用例

相关单测位于：
- `internal/scoring/service_progression_test.go`

覆盖点：
- 首把击杀得分与权重（应接近 100 与 1）。
- 队伍变化补偿是否生效。
- 未击杀时权重应低于 1。

## 注意事项

1. 当前是“实现口径文档”，以代码行为为准。
2. 当前只纳入 `friendplayers_usable=true` 数据，因此 8 人名单不完整的记录不会参与开荒速度。
3. 若后续要改为“严格只算首杀前，不含首杀当把”，需要调整单 Boss 循环中的 `break` 时机。
