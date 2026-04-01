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
- `cmd/test_xivanalysis_payload/main.go`：导出分析请求体用于排查
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

### 4) 生成战斗分析文件

```bash
go run ./cmd/test_xivanalysis_http/main.go \
  --dir downloads/fflogs/ALL_REPORTS_世无仙_延夏/<reportCode> \
  --fight-id 1 \
  --api-url http://127.0.0.1:22026
```

输出示例：
- `downloads/fflogs/.../fight_1_analysis.json`

## 常用排查

- 导出请求体（不发请求）：

```bash
go run ./cmd/test_xivanalysis_payload/main.go --dir <report_dir> --fight-id 1
```

- 若 V1 下载失败：检查 `FFLOGS_V1_API_KEY`
- 若分析服务启动失败：优先检查 `external/xivanalysis/node_modules` 是否已安装
- 若结果不完整：先确认数据库中 `fight_sync_maps.downloaded` 状态与本地 `fight_*_events.json` 是否齐全

## 文档

- 主文档：`docs/service-usage.md`
- 文档索引：`docs/README.md`
- 同步背景：`docs/fflogs_sync_flow.md`
- Node 服务细节：`docs/xivanalysis-server.md`

## 说明

该仓库目前不是“一键上线的完整产品”，而是“可跑通的同步 + 分析工程骨架”。后续会继续补全缓存链路、配置化和稳定性收敛。