# FFLogs 同步与去重流程记忆文档（V1 为主，V2 仅背景）

本文用于快速记忆当前的同步与去重思路，重点描述 V1 下载与去重映射，以及多线程解析策略；V2 GraphQL 仅作背景说明。

## 1) 输入参数与 GraphQL 查询（V2 背景）
外部传入参数：
- name：玩家名
- serverSlug：服务器

固定项：
- serverRegion：CN
- difficulty：101（Savage）

示例查询（仅示意结构，字段可按需裁剪）：
```graphql
query CharacterData {
  characterData {
    character(name: "世无仙", serverSlug: "延夏", serverRegion: "CN") {
      recentReports(page: 1, limit: 20) {
        total
        per_page
        current_page
        from
        to
        last_page
        has_more_pages
        data {
          code
          startTime
          fights(difficulty: 101) {
            id
            name
            encounterID
            kill
            fightPercentage
            startTime
          }
        }
      }
    }
  }
}
```

说明：
- 需要能找到“该玩家在战斗中的 ID”。实践中可通过 `masterData.actors` 找到玩家 `actorID`，并在 `fights.friendlyPlayers` 中判定参战。该字段在 V2 的 `recentReports` 查询中一并返回。
- 返回结构可参考 [docs/reports.json](docs/reports.json)。

## 2) 查重与映射规则（核心）
背景：同一战斗可能被不同玩家上传到不同 report，同一时间段会出现重复记录。

时间戳计算：
- 绝对时间戳 = report.startTime + fight.startTime
- startTime 可以自定义，但要保证两者相加后得到的时间戳一致

去重键与容差：
- 去重键：boss_name + timestamp + duration
- 时间容差：±5 秒（秒级容差）
- 原因：同一玩家不可能在同一时间出现在两个不同副本

示例：
- kx4FAZ27J1zDwnyP 的 fight1：report.startTime=1768570400950，fight.startTime=87690
- mMjDp1Gby42fwCgk 的 fight1：report.startTime=1768569403420，fight.startTime=1085220
- 两者相加时间戳相同，视为同一战斗，需要映射到同一个 master（例如 D 的 fight1）

boss_name 规则：
- 使用 fights 里的 name 作为输入，再映射为规范 boss_name（M9S/M10S/M11S/M12S）
- 映射表（与同步逻辑一致）：
  - Vamp Fatale -> M9S
  - Red Hot / Deep Blue -> M10S
  - The Tyrant -> M11S
  - Lindwurm -> M12S

SQL/表结构与写入策略：
- Reports 表停止写入（仅保留历史或结构）。
- 去重主记录写入 fight_sync_maps，包含原 Reports 字段：
  - player_id/title/start_time/duration/fight_id/kill/boss_name/job/wipe_progress
  - deaths/vuln_stacks/avoidable_damage/damage_down/percentile
- source_ids 使用 JSONB，存放所有来源 report-fight ID。
- 为 source_ids 建立 GIN 索引以加速查询。
- 同步流程：先内存聚合同批次 fights，按“覆盖最大报告”选 master，再写入/合并到 fight_sync_maps。

## 3) V1 下载与报告选择策略（主路径）
目标：尽量减少重复下载与解析，优先选择覆盖玩家战斗最多的 report。

步骤：
1) 使用 V1 API 拉取 report 与 fights：
   - `GET /v1/report/fights/{code}` 
2) 统计每个 report 中“包含该玩家的 fight 数量”。
3) 选择“覆盖最大”的 report 作为优先下载对象。
4) 若多个 report 有重合战斗，以覆盖最大者为主，并把其他 report 的对应 fight 映射到该 master。
5) 对于非 difficulty=101 的普通副本：默认剔除（或按需合并，但推荐剔除以简化分析）。

说明：
- 若 A/B/C 有重合 fight，而 C 额外包含 5 条该玩家 fight，则只需要拉取 C 的完整日志。

## 4) AllReports 聚合与增量下载（基于映射表）
目标：为单个玩家生成本地聚合报告 AllReports，并避免重复拉取 V1。

命名规则：
- AllReports code：ALL_REPORTS_{name}_{server}（本地虚拟 code，不是 FFLogs 原始 code）

新增字段建议：
- players.all_reports_code (text)：AllReports code
- players.all_report_codes (jsonb)：已合并的 report codes（JSONB 数组）
- fight_sync_maps.all_reports_fight_index (int)：该战斗在 AllReports 中的位置（允许重排）

解析状态表：
- report_parse_logs：report_code、player_id、parsed_at、parsed_done

并发写入策略：
- 先写入内存桶，批量落库，避免并发冲突

流程：
1) 使用 V2 获取该玩家的 report codes（recentReports）。
2) 按 fight_sync_maps 做去重与映射，得到本地 master fights。
3) 检查每个 report code 是否已存在于 players.all_report_codes 或 report_parse_logs 已完成。
4) 若已包含：跳过 V1 下载；若未包含：请求 V1 并追加到 AllReports。
5) 更新 players.all_report_codes、report_parse_logs，并写入/更新 fight_sync_maps.all_reports_fight_index。

## 5) 多线程解析策略
目的：加速处理，同时避免 FFLogs 限速。

建议做法：
- 使用 worker pool 或信号量控制并发数。
- 并发数可配置（默认 8）。
- 对每个 report 或 fight 并行处理，但保持分页拉取与结果合并的顺序一致。

## 6) V2 GraphQL 背景说明（非主路径）
V2 主要用于：
- recentReports 分页拉取
- report tables 的批量拉取（casts/buffs/damageDone/healing/damageTaken 等）
- events 分页拉取
- 去重与映射逻辑（boss_name + timestamp + duration，含容差）

sync.go 改动记录：
- GraphQL 查询改为 `fights(difficulty: 101)`，降低无效战斗数量
- 查询字段新增 `encounterID`，用于后续对齐/排查
- Reports 表不再写入，fight_sync_maps 改为 JSONB source_ids 并承载主记录字段

参考实现：
- [internal/api/sync.go](internal/api/sync.go)

## 7) 关联资料
- [docs/fflogs_v1_report_analysis.md](docs/fflogs_v1_report_analysis.md)
- [scripts/fflogs_v1_download.go](scripts/fflogs_v1_download.go)
- [scripts/merge_v1_fights.go](scripts/merge_v1_fights.go)
- [docs/reports.json](docs/reports.json)
