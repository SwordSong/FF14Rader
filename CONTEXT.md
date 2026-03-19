# FF14Rader 项目上下文说明 (Project Context)

本文件用于记录项目的进度，方便在开发过程中快速回顾。

## 已完成的功能 (Completed)
1. **基础架构**:
   - 初始化 Go 项目，配置 `.env` 环境变量加载。
   - 使用 GORM 连接 PostgreSQL，并支持**读写分离** (Master/Slave)。
   - 完成了数据库表的自动迁移 (AutoMigrate)，包含 `Player`, `Report`, `Performance` 表。
2. **FFLogs API 对接**:
   - 实现了 OAuth2 认证流，支持自动刷新 Access Token。
   - 封装了 GraphQL 查询接口 `ExecuteQuery`。
3. **数据同步**:
   - 实现了每 6 小时的**增量同步逻辑** (`internal/api/sync.go`)。
   - 能够根据数据库中最后一条记录的时间戳，只拉取该时间点之后的新 Report 信息。
   - 自动过滤非零式副本数据 (Difficulty 101)。
4. **代码规范**:
   - 所有逻辑注释、日志输出均已切换为**中文**。
5. **核心分析与算法**:
   - 实现了 9 维度算法 (输出、潜力值、稳定度、生存、开荒表现等)。
   - **武士 (SAM) 专项分析器**: 实现了雪月花、波切等核心技能的爆发窗口期覆盖检测 (`internal/analyzer/jobs/sam.go`)。
   - **职业统计**: 增加了 `GetMostUsedJob` 函数，自动统计玩家在当前版本使用最多的职业。
6. **雷达图可视化**:
   - 使用 `fogleman/gg` 完成 9 维度雷达图生成逻辑 (`internal/render/radar.go`)。



## 待讨论的逻辑细节
- 职业细节逻辑的首批支持目标（如：武士、黑魔）。
- 潜力值的具体加权参数。
