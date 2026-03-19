# xivanalysis 子模块与同步说明

## 目录结构
- 子模块路径：external/xivanalysis（跟踪 upstream main）
- 工作流：.github/workflows/xivanalysis-sync.yml（每 6 小时更新一次，支持手动触发）

## 凭证要求
- 仓库 Secret：SUBMODULE_BOT_TOKEN
  - 最低权限：contents:write（允许推送子模块更新）
  - 如需自动开 PR（推送受限场景），需额外 pull_requests:write
  - 建议使用专用 bot 账号，邮箱任意 noreply 均可

## 工作流行为
1. checkout 仓库并初始化子模块
2. 在 external/xivanalysis 切换/拉取 upstream main
3. 若子模块指针发生变化，使用 bot 信息提交 commit 后推送
4. 若无推送权限，可改造为使用 peter-evans/create-pull-request 生成 PR

## 本地更新子模块
```
git submodule update --remote external/xivanalysis
```
或首次初始化：
```
git submodule update --init --recursive
```

## 常见问题
- 推送失败：检查 SUBMODULE_BOT_TOKEN 是否具备 contents:write，并确保 workflow 使用该 token。
- 子模块未更新：确认 upstream/main 有新提交且 workflow cron（0 */6 * * *）已运行。
