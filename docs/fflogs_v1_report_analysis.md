# FFLogs V1 Report 下载与解析说明（pLCQ7Vz3ndAWN92G）

## 1. 本次下载产物
下载脚本输出目录：
- downloads/fflogs/pLCQ7Vz3ndAWN92G 

主要文件：
- report_fights.json：报告级元数据与全部战斗列表
- report_summary.json：本次分析摘要（示例战斗统计）
- fight_{id}_events.json：每场战斗的事件流（V1 `report/events` 完整分页）

已生成的 events 文件数量：24 个（fight_1_events.json 到 fight_24_events.json）。

## 2. 接口流程（V1）
本项目对 V1 的使用逻辑是：

1) 获取报告与战斗列表
- GET /v1/report/fights/{code}
- 作用：拿到 report 的基础信息与 fights 列表（id、难度、boss 进度、起止时间等）
- 对应代码路径：
  - [external/xivanalysis/src/reportSources/legacyFflogs/legacyStore.ts](external/xivanalysis/src/reportSources/legacyFflogs/legacyStore.ts#L34)
  - [external/xivanalysis/src/reportSources/legacyFflogs/fflogsApi.ts](external/xivanalysis/src/reportSources/legacyFflogs/fflogsApi.ts#L79)

2) 获取战斗事件流（分页）
- GET /v1/report/events/{code}?start=...&end=...&translate=true
- 作用：获取事件流并通过 nextPageTimestamp 分页拉取
- 对应代码路径：
  - [external/xivanalysis/src/reportSources/legacyFflogs/fflogsApi.ts](external/xivanalysis/src/reportSources/legacyFflogs/fflogsApi.ts#L112)

3) 事件适配与解析（xivanalysis 内部）
- 事件被适配为 xivanalysis 内部事件结构，然后交给 parser 模块链处理
- 关键点：加载 Core + Boss + Job 模块（按依赖拓扑排序），驱动分析输出
- 对应代码路径：
  - [external/xivanalysis/src/reportSources/legacyFflogs/eventAdapter/adapter.ts](external/xivanalysis/src/reportSources/legacyFflogs/eventAdapter/adapter.ts)
  - [external/xivanalysis/src/parser/core/Parser.tsx](external/xivanalysis/src/parser/core/Parser.tsx#L108)
  - [external/xivanalysis/src/components/ReportFlow/Analyse/Analyse.tsx](external/xivanalysis/src/components/ReportFlow/Analyse/Analyse.tsx#L87)

## 3. xivanalysis 实际分析用到的数据
在 V1 路径下，xivanalysis 的主要输入来自两部分：

A) 报告与战斗元数据（report/fights）
- fight id、name、difficulty、start_time、end_time、bossPercentage、fightPercentage 等
- 用于构建 Pull、Actor 列表、战斗筛选与页面导航

B) 战斗事件流（report/events）
- 事件类型与字段：
  - type（damage、heal、cast、applybuff、removebuff 等）
  - timestamp（事件时间）
  - sourceID / targetID
  - ability（技能名、id）
  - amount / absorbed / overheal / mitigated 等
- 这些事件被适配成 xivanalysis 内部事件，在模块中用于：
  - 伤害输出统计
  - Buff/Debuff 覆盖
  - 技能释放节奏
  - 机制处理（例如可规避伤害、错误处理等）

## 4. 本次报告的实际样例统计（来自 report_summary.json）
报告标题：AAC Heavyweight

示例战斗（fight id = 1）：
- Boss: Red Hot and Deep Blue
- 难度：101（Savage）
- 时长：231 秒
- 进度：bossPercentage = 6073（未击杀）
- 事件总数：8839
- Top 事件类型：
  - damage (1796)
  - cast (1542)
  - calculateddamage (1346)
  - heal (1232)
  - removebuff (645)
- Top 技能：
  - Attack (1148)
  - Asylum (255)
  - Combined HoTs (254)
  - Regeneration (239)
  - Glare III (235)

完整战斗列表见：
- [downloads/fflogs/pLCQ7Vz3ndAWN92G/report_fights.json](downloads/fflogs/pLCQ7Vz3ndAWN92G/report_fights.json)

## 5. 说明与注意点
- 本次摘要只展示了 report_summary.json 中的样例战斗统计。
- 若需要全量战斗统计，可以按 fight_{id}_events.json 逐场计算，或在脚本里扩展多场汇总。
- 若想对接 xivanalysis 前端分析页面，需要进入对应路径：
  - /fflogs/{code}/{fightId}/{actorId}
  - 进入 actor 页面后才会触发真正的 Parser 分析。

## 6. 多报告战斗合并（V1）
当同一场战斗被不同玩家上传到不同 report 时，fightId 会不一致。为避免重复解析、提高性能，可按 boss_name + startTime + duration 进行去重合并。

合并规则：
- boss_name：使用 fights.name 映射为规范 boss_name
- 映射表：
  - Vamp Fatale -> M9S
  - Red Hot / Deep Blue -> M10S
  - The Tyrant -> M11S
  - Lindwurm -> M12S
- startTime：report.start + fight.start_time
- duration：fight.end_time - fight.start_time
- 容差：±5s

合并产物：
- merged_mapping.json：报告 fight → master fight 映射
- D-f{n}_events.json：合并后的事件流

合并脚本：
- [scripts/merge_v1_fights.go](scripts/merge_v1_fights.go)

示例命令：
```bash
go run /home/FF14Rader/scripts/merge_v1_fights.go \
  -root /home/FF14Rader/downloads/fflogs \
  -out /home/FF14Rader/downloads/fflogs/merged \
  -tolerance-ms 5000
```

说明：
- 当前阶段只写文件，数据库映射暂不落库。
- 合并后可只解析 master fight，避免解析无关战斗数据。
- 更完整的同步与去重说明见 [docs/fflogs_sync_flow.md](docs/fflogs_sync_flow.md)。

数据库同步备注：
- 若启用数据库写入，主记录写入 fight_sync_maps，Reports 表停止写入。
- source_ids 使用 JSONB，保存所有来源 report-fight ID。
