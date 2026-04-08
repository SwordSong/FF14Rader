# 职业清单评估（按 DPS / N / T）

## 目的
- 将所有职业按 DPS / N / T（N=奶=治疗）分组，统一做“清单评估”。
- 该文档只定义评估框架与记录模板，具体分数由分析输出计算后填写。
- 本文档仅做单职业评分，不做跨职业横向评分。

## 数据来源（当前实现）
- 通用模块：`checklist`、`suggestions`、`utilities` / `defensives`。
- 职业关键模块：从单场 `modules` 中提取“非通用 handle”作为该职业补充项。
- 战斗权重：沿用 [docs/七维评分算法](docs/七维评分算法) 的进度加权规则。
- checklist 明细字段：使用 `checklist.rules[].requirements[]` 中的 `percent/target/weight`。

## 通用评分模板

### 模板 T（坦克）
- 清单执行分（checklist_abs）: 35%
- 建议严重度惩罚（suggestions）: 15%
- 防御覆盖（defensives/utilities）: 30%
- 生存硬失误惩罚（死亡/可规避伤害）: 20%

### 模板 N（治疗）
- 清单执行分（checklist_abs）: 35%
- 建议严重度惩罚（suggestions）: 20%
- 团队防御与辅助覆盖（utilities/defensives）: 30%
- 生存硬失误惩罚（死亡/可规避伤害）: 15%

### 模板 DPS
- 清单执行分（checklist_abs）: 45%
- 建议严重度惩罚（suggestions）: 20%
- 团辅与资源使用（utilities/defensives）: 15%
- 生存硬失误惩罚（死亡/可规避伤害）: 20%

## checklist 单职业评分规则（重点）

不同职业 checklist 内容不一致。
本方案只做单职业评分，不做跨职业标准化。
最终仅输出：
- `checklist_abs`：同职业内部执行质量分（绝对分）。
- `confidence`：该场 checklist 规则命中充分度。

### 第 1 步：单场原始清单分（0-100）

对每个 requirement：

$$
s_i = \left(min\left(\frac{percent_i}{threshold_i}, 1\right)\right)^{expo_i}
$$

其中：
- 若规则属于 GCD：$threshold_i=max(target_i,95)$，$expo_i=2.4$。
- 若规则属于增伤常驻类：$threshold_i=max(target_i,95)$，$expo_i=2.2$。
- 其他规则（含普通执行项）：$threshold_i=max(target_i,90)$，$expo_i=1.6$。

单条规则分：

$$
S_{rule} = \frac{\sum_i w_i s_i}{\sum_i w_i}
$$

规则优先级（动态权重）：

$$
\beta_r = min\left(6, 1 \times m_{gcd} \times m_{dot} \times m_{buff}\right)
$$

其中：
- $m_{gcd}=3$（GCD 相关规则）或 $1$。
- $m_{dot}=2$（DoT 相关规则）或 $1$。
- $m_{buff}=2.2$（增伤常驻规则）或 $1$。

单场 checklist 原始分：

$$
S_{check,raw} = 100 \cdot \frac{\sum_r \beta_r S_{rule,r}}{\sum_r \beta_r}
$$

说明：
- $w_i$ 取 requirement.weight（缺失时按 1）。
- $\beta_r$ 由规则类型自动计算（见上式）。

### 第 2 步：可用性修正（避免缺项误伤）

可用性系数：

$$
A = \frac{\sum \text{命中规则权重}}{\sum \text{期望规则权重}}
$$

绝对分：

$$
checklist_{abs,pre} = S_{check,raw} \cdot (0.7 + 0.3A)
$$

并输出：

$$
confidence = A
$$

### 第 2.3 步：GCD 门控（标准参数）

为体现“GCD 是输出底盘”，在绝对分上增加门控系数：

$$
g = \frac{GCDCoverage}{100}
$$

$$
F_{gcd} = \alpha + (1-\alpha) \cdot g^{\beta}
$$

标准参数固定为：
- $\alpha=0.15$
- $\beta=3$

最终绝对分：

$$
checklist_{abs} = checklist_{abs,pre} \cdot F_{gcd}
$$

说明：
- 若未识别到 GCD 覆盖率字段，则默认 $F_{gcd}=1$（不额外惩罚）。
- 该门控与规则级 GCD 权重共同作用：前者控制“底盘”，后者控制“细项区分”。

### 第 2.5 步：清单数量不均的处理（有些职业清单很少）

问题：有些职业 checklist 很多，有些职业只有少量 requirement。
只看 `checklist_abs` 时，少条目职业可能出现“分数看起来很高但不稳定”。

处理方式：把“分数”和“可信度”分离，并做收缩。

设该职业历史基线分为 $S_{base,job}$（建议取该职业近 30 天中位数，缺失时先用 70）。

$$
checklist_{adj} = A \cdot checklist_{abs} + (1-A) \cdot S_{base,job}
$$

其中：
- 当规则命中充分（$A$ 高）时，更相信本场 `checklist_abs`。
- 当规则命中不足（$A$ 低）时，分数向职业基线收缩，避免“少条目虚高/虚低”。

同时，checklist 在总评中的有效权重也乘可信度：

$$
w_{check}^{eff} = w_{check} \cdot A
$$

推荐可信度标签：
- 高可信：$A \ge 0.8$
- 中可信：$0.6 \le A < 0.8$
- 低可信：$A < 0.6$（建议提示“样本不足/规则不足”）

### 第 3 步：在评分中的使用方式
- 展示给玩家：`checklist_abs`（直观看执行质量）。
- 计入模板权重：`checklist_adj`（数量不均修正后分数）。
- 当 `confidence < 0.6`：标记“样本不足/规则缺失”，总评可降权处理。

## 职业清单（T）

| 职业 | 缩写 | 使用模板 | 职业关键模块（从 modules 动态发现） | 评估状态 |
|---|---|---|---|---|
| Paladin | PLD | T | 非通用 handle 列表 | 待评估 |
| Warrior | WAR | T | 非通用 handle 列表 | 待评估 |
| Dark Knight | DRK | T | 非通用 handle 列表 | 待评估 |
| Gunbreaker | GNB | T | 非通用 handle 列表 | 待评估 |

## 职业清单（N）

| 职业 | 缩写 | 使用模板 | 职业关键模块（从 modules 动态发现） | 评估状态 |
|---|---|---|---|---|
| White Mage | WHM | N | 非通用 handle 列表 | 待评估 |
| Scholar | SCH | N | 非通用 handle 列表 | 待评估 |
| Astrologian | AST | N | 非通用 handle 列表 | 待评估 |
| Sage | SGE | N | 非通用 handle 列表 | 待评估 |

## 职业清单（DPS）

### 近战 DPS
| 职业 | 缩写 | 使用模板 | 职业关键模块（从 modules 动态发现） | 评估状态 |
|---|---|---|---|---|
| Monk | MNK | DPS | 非通用 handle 列表 | 待评估 |
| Dragoon | DRG | DPS | 非通用 handle 列表 | 待评估 |
| Ninja | NIN | DPS | 非通用 handle 列表 | 待评估 |
| Samurai | SAM | DPS | 非通用 handle 列表 | 待评估 |
| Reaper | RPR | DPS | 非通用 handle 列表 | 待评估 |
| Viper | VPR | DPS | 非通用 handle 列表 | 待评估 |

### 远程物理 DPS
| 职业 | 缩写 | 使用模板 | 职业关键模块（从 modules 动态发现） | 评估状态 |
|---|---|---|---|---|
| Bard | BRD | DPS | 非通用 handle 列表 | 待评估 |
| Machinist | MCH | DPS | 非通用 handle 列表 | 待评估 |
| Dancer | DNC | DPS | 非通用 handle 列表 | 待评估 |

### 远程魔法 DPS
| 职业 | 缩写 | 使用模板 | 职业关键模块（从 modules 动态发现） | 评估状态 |
|---|---|---|---|---|
| Black Mage | BLM | DPS | 非通用 handle 列表 | 待评估 |
| Summoner | SMN | DPS | 非通用 handle 列表 | 待评估 |
| Red Mage | RDM | DPS | 非通用 handle 列表 | 待评估 |
| Pictomancer | PCT | DPS | 非通用 handle 列表 | 待评估 |
| Blue Mage* | BLU | DPS | 非通用 handle 列表 | 待评估 |

备注：BLU 为特殊职业，仅做 BLU 职业内样本评分，不与常规职业混算。

## 单职业评估记录模板（复制使用）

```
职业: <JOB>
角色组: <T|N|DPS>
统计区间: <起止时间>

1) 清单绝对分(checklist_abs): <xx.xx>
2) checklist 可信度(confidence): <xx.xx>
3) 清单修正分(checklist_adj): <xx.xx>
4) 建议严重度惩罚(suggestions): <xx.xx>
5) 团辅/防御覆盖(utilities/defensives): <xx.xx>
6) 生存硬失误惩罚: <xx.xx>
7) 职业关键模块得分: <xx.xx>

单场加权分: <xx.xx>
跨战斗汇总分: <xx.xx>
备注: <异常战斗/样本不足说明>
```

## 实施建议
- 第一阶段：先按模板 T/N/DPS 跑通全职业，不做职业特例权重。
- 第二阶段：每个职业积累样本后，再给“职业关键模块”单独加权。
- 第三阶段：引入难度系数（按模块达标率与区分度自动调权）。

默认预览（不传进度，权重仍是 1.00）
go run ./cmd/test_fight_score --analysis-file fight_1_analysis.json --actor-name 世无仙

按手工进度预览（例如剩余血量 40.93%，进度=59.07%）
go run ./cmd/test_fight_score --analysis-file fight_1_analysis.json --actor-name 世无仙 --fight-percentage 40.93