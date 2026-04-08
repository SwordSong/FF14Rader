# FF14Rader

FF14Rader 是一个围绕 FFLogs 数据的本地分析工作流项目，当前重点是：

- 同步玩家零式战斗数据到 PostgreSQL
- 下载并缓存 V1 报告事件
- 调用本仓库内置的 xivanalysis Node 服务，输出结构化分析 JSON

## 项目状态

当前是可用但仍在迭代中的版本。

已实现：
- FFLogs V2 OAuth 与 GraphQL 拉取（recentReports 分页）
- 同步筛选 101 难度战斗并写入 `fight_sync_maps`
- 多报告战斗去重与 source_ids 合并
- V1 报告事件下载与落地到 `downloads/fflogs/...`
- Node 分析服务 `scripts/xivanalysis-server.js` 与 Go 调用工具联通
- 分析输出支持结构化 metrics（含通用兜底，避免空对象）

仍未完全完成：
- `internal/api/sync.go` 中部分事件缓存链路仍是注释状态（fetch events + merge + save cache）
- 楼层映射目前是手工白名单（`mapFloor`）
- 同步起始时间当前仍有硬编码常量
- 根 `main.go` 目前仅初始化并阻塞，不是完整对外 API 服务

## 仓库结构（核心）

- `internal/api/fflogs.go`：FFLogs V2 客户端（token、query、配额统计）
- `internal/api/sync.go`：增量同步主流程
- `internal/api/fflogs_v1.go`：V1 报告/事件下载
- `scripts/xivanalysis-server.js`：Node 分析服务（POST /analyze）
- `cmd/test_sync/main.go`：手工触发同步
- `cmd/test_xivanalysis_http/main.go`：调用分析服务并生成 `fight_*_analysis.json`
- `cmd/backfill_team_stability/main.go`：回填玩家团队贡献与稳定度（写入 `players`）
- `cmd/backfill_output_percentile/main.go`：通过 FFLogs 角色 logs 排名接口直接刷新 `players.output_ability`
- `docs/service-usage.md`：当前主文档（推荐先读）

## 环境要求

- Go（以 `go.mod` 声明为准）
- Node.js（建议 LTS）
- PostgreSQL（需要写库和读库 DSN）
- 可访问 FFLogs API

## 配置

项目通过环境变量读取配置（支持 `.env`）。

最低必需：
- `FFLOGS_CLIENT_ID`
- `FFLOGS_CLIENT_SECRET`
- `POSTGRES_WRITE_DSN`
- `POSTGRES_READ_DSN`

建议配置：
- `FFLOGS_V1_API_KEY`
- `FFLOGS_ALL_REPORTS_DIR`（默认 `./downloads/fflogs`）
- `FFLOGS_SYNC_PAGE_CONCURRENCY`（V2 分页并发）
- `FFLOGS_V2_TIMEOUT_SEC`
- `FFLOGS_V2_RETRY`
- `FFLOGS_V1_CONCURRENCY`
- `FFLOGS_V1_REPORT_TIMEOUT_SEC`

Node 分析服务可选：
- `PORT`（默认 `22026`）
- `MAX_BODY_BYTES`
- `MAX_CONCURRENCY`
- `ANALYZE_TIMEOUT_MS`
- `LOG_LEVEL`
- `XIVA_OUTPUT_MODE`（默认 `numeric`）
- `XIVA_OUTPUT_LOCALE`（默认 `zh`）
- `XIVA_HOSTS_CONFIG`（解析服务多主机配置文件，默认 `./docs/xiva-hosts.json`）
- `XIVA_EXECUTION_MODE`
- `XIVA_PORT_COUNT`
- `XIVA_THREAD_POOL_SIZE`
- `XIVA_CALL_CONCURRENCY`

## 快速开始

### 1) 准备依赖

```bash
# Go 依赖
go mod download

# Node 依赖（如需要）
npm install

# xivanalysis 子模块依赖（Node 服务依赖此目录）
cd external/xivanalysis
pnpm install
cd ../..
```

### 2) 执行一次同步

```bash
go run ./cmd/test_sync/main.go --name 世无仙 --server 延夏
```

同步完成后会写入数据库，并在本地目录生成对应报告数据。

### 3) 启动分析服务

```bash
node scripts/xivanalysis-server.js
```

健康检查：

```bash
curl http://127.0.0.1:22026/health
```

### 3.1) 多主机解析配置（配置文档）

评分服务会优先读取 `XIVA_HOSTS_CONFIG` 指定的 JSON 文件（默认 `./docs/xiva-hosts.json`），并按配置中的 `servers` 列表轮询调用解析服务。

示例：

```json
{
	"servers": [
		{"name": "host-a", "host": "10.0.0.11", "port": 22026, "enabled": true, "weight": 2},
		{"name": "host-b", "host": "10.0.0.12", "port": 22026, "enabled": true, "weight": 1}
	]
}
```

说明：
- `weight` 可用于加权轮询（值越大，命中概率越高）。
- 若未配置该文件或文件无效，会回退到原有环境变量方式（`XIVA_ANALYZE_URL` / `XIVA_API_HOST` + 端口范围）。

### 4) 生成战斗分析文件

```bash
go run ./cmd/test_xivanalysis_http/main.go --dir downloads/fflogs/pxwb4WJa6d8yGt1X/ --fight-id 6 --api-url http://127.0.0.1:22026
```

输出示例：
- `downloads/fflogs/.../fight_1_analysis.json`

### 5) 回填团队贡献与稳定度

当历史战斗已经评分完成，需要把玩家维度字段统一回写到 `players` 表时：

```bash
# 回填所有已评分玩家
go run ./cmd/backfill_team_stability/main.go

# 仅回填单个玩家
go run ./cmd/backfill_team_stability/main.go --player-id 123
```

### 6) 通过 FFLogs 接口直接计算输出能力

同步完成后会直接调用 FFLogs 角色 logs 排名接口（`zoneRankings(metric: rdps)`）并回写：
- `players.output_ability`

不再依赖战斗级 `percentile` 累计字段。

```bash
# 按角色名触发增量同步（会自动刷新 output_ability）
go run ./cmd/test_sync/main.go --name 世无仙 --server 延夏

# 已知玩家ID时也可指定（同样会刷新 output_ability）
go run ./cmd/test_sync/main.go --player-id 123 --name 世无仙 --server 延夏
```

### 7) 批量刷新玩家输出能力

当需要基于最新 logs 排名重算玩家输出能力时：

```bash
# 刷新所有玩家 output_ability
go run ./cmd/backfill_output_percentile/main.go

# 仅刷新单个玩家
go run ./cmd/backfill_output_percentile/main.go --player-id 123
```

### 8) 按 name + server 生成六维雷达图

从 `players` 表读取以下六项并出图：
- `output_ability`
- `battle_ability`
- `team_contribution`
- `progression_speed`
- `stability_score`
- `potential_score`

```bash
# 基础用法
go run ./cmd/draw_player_radar/main.go --name 情事 --server 延夏

# 指定输出路径与图片尺寸
go run ./cmd/draw_player_radar/main.go --name 情事 --server 延夏 --output ./radar.png --width 1000 --height 1000
```

## 常用排查

- 若 V1 下载失败：检查 `FFLOGS_V1_API_KEY`
- 若分析服务启动失败：优先检查 `external/xivanalysis/node_modules` 是否已安装
- 若结果不完整：先确认数据库中 `fight_sync_maps.downloaded` 状态与本地 `fight_*_events.json` 是否齐全

## 文档

- 主文档：`docs/service-usage.md`
- 文档索引：`docs/README.md`
- 评分算法：`docs/七维评分算法`
- 开荒速度细节：`docs/progression-speed-algorithm.md`
- 职业清单评估：`docs/job-checklist-evaluation.md`

## 说明

该仓库目前不是“一键上线的完整产品”，而是“可跑通的同步 + 分析工程骨架”。后续会继续补全缓存链路、配置化和稳定性收敛。