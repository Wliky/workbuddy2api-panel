# FORK.md — 本 fork 的维护约定

> 这份文件是 **fork 独占**的（上游没有），永远不会在 `sync-upstream` 时冲突。
> 改动本仓库前请先读一遍。

## 这个 fork 是什么

`Wliky/workbuddy2api-panel`，fork 自 [`linguo2625469/workbuddy2api-panel`](https://github.com/linguo2625469/workbuddy2api-panel)。

在保留上游全部能力的前提下，额外做了这些事：

1. **armv7 支持 + 镜像拉取部署**（玩客云 S805 等 32 位 ARM 设备）
2. **根路径直达面板**（`server.panel_root`）与 **CORS 预检**

> **已归还上游的改动**：单文件 bind mount 下的配置写入兜底（EBUSY 降级）——
> 上游 `5e1422c` + `ab9a162`（PR #36）自行实现了同一语义且更完善，
> 本 fork 已删除自有实现、改为直接引用上游。详见下文「改动归还」。

## ⚠️ 核心约定：fork 改动必须外置

**这是本仓库最重要的一条规则。**

上游的 `cmd/server/main.go`、`internal/server/handler.go`、`cmd/server/config.go`
是**高频改动文件**（实测近 50 个提交里分别改了 24 / 16 / 12 次）。
把 fork 的逻辑直接写进这些文件，会导致**每一次** `sync-upstream` 都必然冲突。

因此 fork 的逻辑集中放在**独占文件**里：

| 文件 | 承载内容 |
|---|---|
| `internal/server/fork_fixes.go` | CORS 预检、根路径直达面板（`rootProbePaths` / `isGatewayPath` / `rewriteToPanelRoot` / `corsPreflight` / `forkBeforeRouting`） |

**上游文件里只允许留「最小接缝」**，当前全部接缝如下（改动前请对照）：

| 位置 | 接缝 | 行数 |
|---|---|---|
| `internal/server/handler.go` — `Config` | `PanelRoot bool` 字段 | 1 个字段（+注释） |
| `internal/server/handler.go` — `ServeHTTP` | `if forkBeforeRouting(w, r, h.cfg.PanelRoot, h.cfg.Panel != nil) { return }` | 1 行 |
| `cmd/server/main.go` — `NewHandler` 装配 | `PanelRoot: cfg.Server.PanelRoot,` | 1 行 |
| `cmd/server/config.go` — `Server` 段 | `PanelRoot bool \`json:"panel_root"\`` 字段 | 1 个字段（Go 语法上无法外置） |

> 落盘段（`cmd/server/main.go` 的 `saveConfig`）**已不再是接缝** —— 见「改动归还」。

### ⚠️ 改动归还：上游实现了就引用上游

**当上游自己实现了 fork 的某项改动时，删掉 fork 版、改用上游版**，别维护两份。

理由：两份实现并存 = 每次上游动那块代码都必然冲突；而且 fork 版长期会落后于
上游的后续加固。识别信号是「上游提交标题描述的功能与某项 fork 改动高度重合」。

**已归还的改动（2026-09-23）**：bind mount 落盘兜底。

- 上游 `5e1422c fix: save config on Docker bind mounts` —— 与 fork 的 `fork_save.go` 同一件事
- 上游 `ab9a162` 还加固了：降级写失败时**保留 tmp**（挂载文件已被 `O_TRUNC` 截断，
  tmp 里是完整新内容可手工恢复），而 fork 版是直接 `os.Remove(tmp)` 丢掉

处置：`main.go` 的落盘段还原为上游内联实现、删除 `cmd/server/fork_save.go`。
**净效果是接触面又减 1 处、fork 独占文件少 1 个** —— 这是最理想的方向。

归还时**测试要跟着改**：原先 `config_write_test.go` 靠 fork 的 `renameFile` 变量
注入来模拟 EBUSY，上游实现是内联直调 `os.Rename`、没有注入点。
改法不是删测试，而是改为测 `saveConfig` 的**可观测行为**（正常路径原子替换不留 tmp、
校验失败不落盘、校验前置顺序、写失败如实报错）；降级分支（EBUSY → 原地写）
单测造不出来（需 mount 权限），交由 `deploy/wankeyun/wb2api-pull-update.sh`
的「保存配置」冒烟项验收。

#### 怎么自查「上游有没有实现我的改动」

```bash
git fetch origin main
# 1) 看上游新提交的标题与涉及文件，找功能重合
git log --oneline HEAD..origin/main
git diff --stat HEAD...origin/main
# 2) 关键功能名直接在上游树里搜（比翻 diff 快）
git grep -n "panel_root" origin/main -- '*.go'
# 3) 预演合并：有冲突说明双方都动了同一处，正是要审视的地方
git merge-tree --write-tree --messages HEAD origin/main   # rc=1 → 有冲突
```

### 预测冲突（不碰工作区）

`git merge-tree --write-tree --messages <ours> <theirs>` 是**纯内存**合并：
`rc=0` 表示能自动合并、`rc=1` 表示有冲突（并列出冲突文件）。
**改代码前后各跑一次就能量化验证重构效果** —— 不必真的 merge，更别用
`git reset --hard` 去清理（浅克隆下它会删掉 `cmd/` 下大量文件，
且 `git status` 会**假报干净**，见「已知事项」）。

### 新增 fork 功能时怎么做

1. **优先新建文件**：`internal/<pkg>/fork_xxx.go` 或 `cmd/server/fork_xxx.go`
2. 若必须接进上游的调用链（如 `ServeHTTP`、`saveConfig`），**在接缝函数里加分支**
   （如给 `forkBeforeRouting` 加参数），而不是在上游文件里展开逻辑
3. 确实无法避免改上游文件时（如新增 struct 字段），**紧贴现有接缝**，
   并在注释里标注 `⚠️ fork 对接点`

### 效果（实测）

| 场景 | 重构前 | 重构后 |
|---|---|---|
| 上游改 `saveConfig` 热应用区 | 冲突 2 文件、需手工解 | **自动合并，零冲突** |
| 上游自行实现 bind mount 兜底（`5e1422c`+`ab9a162`） | 冲突（双方都改落盘段） | **先归还再合并 → 零冲突** |
| 上游在 `Config` 里紧邻 `PanelRoot` 加字段 | 冲突（大块） | 冲突 1 块 6 行（"两字段都留"即可） |

2026-09-23 吸收上游 `ab9a162`（6 个提交）时，`Merge made by the 'ort' strategy.`
**完全零冲突** —— 这是本仓库第一次上游更新能全自动合并。

## 上游同步

自动化：`.github/workflows/sync-upstream.yml`（每 6 小时检查一次，也可手动触发）。

- 上游无更新 → 不动
- 上游有更新且**能自动合并** → merge（保留 fork 提交）→ 推送 → 触发 armv7 构建
- 上游有更新但**冲突** → `merge --abort` 回滚、**不推送**，并在 Actions 摘要里列出冲突文件与解法

> 注意：fork 的提交会保留。**绝不要**点 GitHub 网页上的 `Sync fork` →
> `Discard N commits`（那会删掉本 fork 全部改动）。

## 手工解冲突后的必做验证

```bash
go build ./...        # 编译
go vet ./...          # 静态检查
go test ./...         # 全量测试（16 个包）
# armv7 未被破坏（应输出 machine=0x28）
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -o /tmp/wb2api_arm ./cmd/server/
```

## 镜像与部署

- **镜像**：`ghcr.io/wliky/workbuddy2api-panel:armv7`（public，交叉编译后 `COPY` 产物，不在 CI 里跑 armv7 模拟）
- **构建**：`.github/workflows/build-armv7.yml`（push 到 main 或手动触发）
- **部署**：详见 `deploy/wankeyun/README.md`（玩客云 / CasaOS，含账号数据备份与双向回滚）

## 已知事项

- 上游 `73fe1f8` 起 `server.max_body_mb` 与 `WB2A_MAX_BODY_MB` **已退役**
  （请求体不再由网关预拦截）。本 fork 的 `config.go` 里 `Server` 段只保留 `panel_root`。
- `README.md` 是**上游文件**（「与上游的差异」章节由上游维护），fork 对它零改动 ——
  fork 的说明请写在本文件里，别往 README 里塞。

### ⚠️ 浅克隆陷阱（本项目踩过，务必知道）

本仓库是浅克隆（`--depth`）。浅克隆下有两个反直觉行为：

1. **`git status` 会假报「干净」** —— 若文件被外部删除（或 `reset --hard` 删掉），
   `git status --short` 可能**什么都不显示**，看着像没动过。
   **别信 `git status`，要这样查真实完整性**：

   ```bash
   git ls-files -z | while IFS= read -r -d '' f; do [ -e "$f" ] || echo "MISSING $f"; done
   ```

2. **`git reset --hard` 在浅克隆下极危险** —— 会删掉 `cmd/` 下大量文件且不报错。
   需要撤销试验时，用 `git merge --abort` 或 `git checkout -- .`，
   **不要用 `reset --hard`**。真要预演合并，用上面说的 `git merge-tree`（不碰工作区）。

