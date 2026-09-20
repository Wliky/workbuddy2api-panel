# FORK.md — 本 fork 的维护约定

> 这份文件是 **fork 独占**的（上游没有），永远不会在 `sync-upstream` 时冲突。
> 改动本仓库前请先读一遍。

## 这个 fork 是什么

`Wliky/workbuddy2api-panel`，fork 自 [`linguo2625469/workbuddy2api-panel`](https://github.com/linguo2625469/workbuddy2api-panel)。

在保留上游全部能力的前提下，额外做了三件事：

1. **armv7 支持 + 镜像拉取部署**（玩客云 S805 等 32 位 ARM 设备）
2. **根路径直达面板**（`server.panel_root`）与 **CORS 预检**
3. **单文件 bind mount 下的配置写入兜底**（EBUSY 降级）

## ⚠️ 核心约定：fork 改动必须外置

**这是本仓库最重要的一条规则。**

上游的 `cmd/server/main.go`、`internal/server/handler.go`、`cmd/server/config.go`
是**高频改动文件**（实测近 50 个提交里分别改了 24 / 16 / 12 次）。
把 fork 的逻辑直接写进这些文件，会导致**每一次** `sync-upstream` 都必然冲突。

因此 fork 的逻辑集中放在两个**独占文件**里：

| 文件 | 承载内容 |
|---|---|
| `internal/server/fork_fixes.go` | CORS 预检、根路径直达面板（`rootProbePaths` / `isGatewayPath` / `rewriteToPanelRoot` / `corsPreflight` / `forkBeforeRouting`） |
| `cmd/server/fork_save.go` | 配置落盘兜底（`writeFileInPlace` / `renameFile` / `writeFileAtomic` / `writeFileAtomicResilient`）、`mergeAndRender` |

**上游文件里只允许留「最小接缝」**，当前全部接缝如下（改动前请对照）：

| 位置 | 接缝 | 行数 |
|---|---|---|
| `internal/server/handler.go` — `Config` | `PanelRoot bool` 字段 | 1 个字段（+注释） |
| `internal/server/handler.go` — `ServeHTTP` | `if forkBeforeRouting(w, r, h.cfg.PanelRoot, h.cfg.Panel != nil) { return }` | 1 行 |
| `cmd/server/main.go` — `NewHandler` 装配 | `PanelRoot: cfg.Server.PanelRoot,` | 1 行 |
| `cmd/server/main.go` — `saveConfig` 落盘 | `if err := writeFileAtomicResilient(path, out, 0o600); err != nil { return nil, err }` | 2 行 |
| `cmd/server/config.go` — `Server` 段 | `PanelRoot bool \`json:"panel_root"\`` 字段 | 1 个字段（Go 语法上无法外置） |

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
| 上游在 `Config` 里紧邻 `PanelRoot` 加字段 | 冲突（大块） | 冲突 1 块 6 行（"两字段都留"即可） |

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
