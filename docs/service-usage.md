# FF14Rader 使用手册（当前实现）

本文档按当前代码状态整理，覆盖从同步到评分展示的最短可用流程。
若与历史文档冲突，以代码实现为准。

## 1. 环境准备

必需环境变量：
- FFLOGS_CLIENT_ID
- FFLOGS_CLIENT_SECRET
- POSTGRES_WRITE_DSN
- POSTGRES_READ_DSN

可选但常用：
- FFLOGS_V1_API_KEY
- FFLOGS_ALL_REPORTS_DIR
- XIVA_HOSTS_CONFIG（默认 ./docs/xiva-hosts.json）

安装依赖：

go mod download

如需启动本地分析服务（xivanalysis）：

cd external/xivanalysis
pnpm install
cd ../..

多主机解析配置（JSON 文档）：

默认读取 `docs/xiva-hosts.json`，也可通过 `XIVA_HOSTS_CONFIG` 指向其他路径。

示例：

```json
{
  "servers": [
    {"name": "local", "host": "127.0.0.1", "port": 22026, "enabled": true, "weight": 1},
    {"name": "remote-a", "host": "10.0.0.11", "port": 22026, "enabled": true, "weight": 2}
  ]
}
```

解析阶段会根据本机可识别的解析地址和本地配置（如 `XIVA_CALL_CONCURRENCY`）给出推荐并发，并在日志中输出。

分布式低带宽场景建议：
- `XIVA_EVENTS_TRANSPORT_MODE=remote-fetch`（A 主机仅下发任务元数据，不再上传 events 大包）
- `XIVA_REMOTE_FETCH_MODE=on`（解析节点收到无 events 请求时，自行从 FFLogs 拉取）
- `XIVA_EVENTS_CACHE_DIR=./downloads/fflogs`（节点本地缓存，重复解析可复用）
- `XIVA_EVENTS_FETCH_TIMEOUT_MS=45000`（节点拉取超时）
- `XIVA_PREFETCH_CONCURRENCY=6`（节点后台下载并发，独立于解析线程池）
- `XIVA_PREFETCH_QUEUE_LIMIT=4096`（节点预拉取排队上限）
- `XIVA_ANALYZE_REQUIRE_CACHE=true`（启用“下载/解析异步分离”，缓存未命中返回 pending）
- `FFLOGS_SCORE_PREFETCH_AHEAD=10`（Go 侧预热窗口，常用为“解析并发+10”）
- `FFLOGS_SCORE_PREFETCH_CONCURRENCY=<与节点数匹配>`（Go 侧预热请求并发）

异步链路说明：
- Go 侧先调用节点 `/prefetch` 入队下载。
- 节点后台按 `XIVA_PREFETCH_CONCURRENCY` 拉取并写本地缓存。
- Go 侧调用 `/analyze` 时，若 `XIVA_ANALYZE_REQUIRE_CACHE=true` 且缓存未命中，会收到 `REMOTE_FETCH_PENDING`，随后自动短重试。

说明：
- Go 侧会优先读取 `FFLOGS_V1_API_KEY`；若为空，将尝试从数据库表 `fflogskey` 读取 `ver=1` 的 `api_id`。
- 当解析节点未启用 remote-fetch 时，Go 侧会自动回退为内联 events 上传模式，保证兼容性。

## 2. 同步玩家战斗数据

按角色名同步（示例）：

go run ./cmd/test_sync/main.go --name 世无仙 --server 延夏

说明：
- 会同步 fight_sync_maps 等数据。
- 同步流程内会调用 FFLogs 角色 logs 排名接口刷新 players.output_ability。
- V2 reports 查询有 24 小时门控：仅当 players.updated_at 超过 24 小时才会拉取 recentReports。
- 新创建玩家不受该门控限制，首次同步会直接查询 V2 reports。

## 3. 评分与回填

3.1 回填输出能力（基于 FFLogs 角色排名）

go run ./cmd/backfill_output_percentile/main.go

go run ./cmd/backfill_output_percentile/main.go --player-id 123

3.2 回填团队贡献、稳定度、潜力值

go run ./cmd/backfill_team_stability/main.go

go run ./cmd/backfill_team_stability/main.go --player-id 123

说明：
- backfill_team_stability 现已同时刷新：
  - team_contribution
  - stability_score
  - potential_score

## 4. 按 name + server 生成六维雷达图

命令：

go run ./cmd/draw_player_radar/main.go --name 玩家名 --server 服务器名

可选参数：
- --output 指定输出路径
- --width 图片宽度（默认 900）
- --height 图片高度（默认 900）

六维图读取字段：
- output_ability
- battle_ability
- team_contribution
- progression_speed
- stability_score
- potential_score

## 5. 常见问题

1. 提示 player not found by name+server
- 检查 players 表是否已写入该玩家。
- 确认名称与服务器是否一致（命令匹配不区分大小写）。

2. 输出能力为 0
- 可能是角色在指定统计范围内无可用 logs 排名，或接口暂未返回数据。
- 可先执行一次同步，再运行 backfill_output_percentile。

3. 潜力值偏低
- 潜力值含样本收缩；战斗样本不足时会向中性分回归。

## 6. 监控 API（独立端口）

主程序会启动监控接口，端口由 `MONITOR_PORT` 控制（默认 `22027`）。

接口：
- `GET /healthz`：健康检查。
- `GET/POST /api/monitor/params`：返回请求携带的 query/form 参数。

示例：

```bash
curl "http://127.0.0.1:22027/api/monitor/params?name=猫米&server=延夏"
```
