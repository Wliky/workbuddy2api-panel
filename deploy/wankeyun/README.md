# 玩客云（armv7l）镜像拉取部署

面向 **OneCloud / S805 / armv7l 32 位** 设备的部署与升级。目标是把「在设备上本地 build
镜像」换成「**直接 `docker pull`**」—— S805 只有 4×Cortex-A5 @1.5GHz 和 1GB 内存，
本地编译又慢又容易 OOM。

> 本目录只解决**运行与升级**。armv7 的**构建**在 GitHub Actions 完成
> （`.github/workflows/build-armv7.yml` → 推送到 GHCR）。

---

## 目录内容

| 文件 | 作用 |
|---|---|
| `docker-compose.yml` | 设备上 `/var/lib/casaos/apps/wb2api/docker-compose.yml` 的镜像拉取版 |
| `wb2api-pull-update.sh` | **在设备上跑**：备份账号数据 → 拉镜像 → 切换 image → 重建 → 冒烟（校验账号池）→ 失败回滚 |
| `README.md` | 本文件 |

---

## 为什么是 GHCR，不是 Docker Hub

2026-09-19 在设备上实测的结果：

| 目标 | 结果 |
|---|---|
| `ghcr.io` | **HTTP 401** —— registry 在线，这是未认证请求的正常响应，说明可直连 |
| `registry-1.docker.io` | **10 秒超时** —— 直连不通，只能借 `daemon.json` 里的国内镜像源 |

设备 `/etc/docker/daemon.json` 配的是 Docker Hub 的镜像源（`docker.1ms.run`、
`docker.xuanyuan.me`），对 GHCR 无效。既然 GHCR 能直连，就用 GHCR 最省事。

---

## 一次性配置

### 1. 把 GHCR 包设为 Public

**这一步不做，设备上 `docker pull` 会 403。**

首次 `build-armv7` 跑完后，去
`https://github.com/users/Wliky/packages/container/workbuddy2api-panel/settings`
把可见性改成 **Public**。

（不想公开的话，就得在设备上 `docker login ghcr.io -u Wliky` 并输入一个
`read:packages` 权限的 PAT。）

### 2. 触发一次构建

push 到 `main` 就会自动构建并推送；也可以到 Actions 页面手动跑 `build-armv7`。

产出的镜像标签：

| 标签 | 含义 |
|---|---|
| `ghcr.io/wliky/workbuddy2api-panel:armv7` | 移动标签，始终指向 main 最新（设备用这个） |
| `:main` | 同上，语义化别名 |
| `:sha-xxxxxxx` | 钉死某个提交，回滚到具体版本时用 |
| `:<版本>-armv7` / `:<版本>` | 打 `v*` tag 时才有 |

### 3. 切换设备上的 compose

**推荐：直接跑脚本**，它会自己改 `image` 行、重建容器，而且会**先备份账号数据**：

```bash
bash /root/wb2api-pull-update.sh
```

**手工方式**（等同，适合想自己控制每一步的人）：

```bash
# 在玩客云上
cd /var/lib/casaos/apps/wb2api
cp docker-compose.yml docker-compose.yml.before-pull-mode
sed -i 's#^\([[:space:]]*image:\).*#\1 ghcr.io/wliky/workbuddy2api-panel:armv7#' docker-compose.yml
docker pull ghcr.io/wliky/workbuddy2api-panel:armv7
docker-compose up -d --force-recreate
```

与设备原文件的**唯一实质差异**就是这一行：

```diff
-        image: wb2api:local
+        image: ghcr.io/wliky/workbuddy2api-panel:armv7
```

本目录的 `docker-compose.yml` 是完整参考版（含全部原字段，可整份替换）。

---

## 日常升级

把 `wb2api-pull-update.sh` 传到设备（例如 `/root/wb2api-pull-update.sh`），然后：

```bash
bash /root/wb2api-pull-update.sh --check       # 先看有没有新版（会拉镜像比对，不改配置、不重建）
bash /root/wb2api-pull-update.sh               # 升级
bash /root/wb2api-pull-update.sh --list        # 看有哪些备份点
bash /root/wb2api-pull-update.sh --rollback    # 回滚到升级前的镜像与数据
```

执行顺序是刻意设计的：

1. **拉镜像** → 判断是否真有新版。判断要看三件事：compose 的 `image` 行、
   容器现用镜像 id、目标镜像是否已拉到本地 —— 只比"镜像 id 变没变"会误判
   （镜像已被 `--check` 拉到本地时 id 相同，但 compose 可能还指着 `wb2api:local`）。
   `--check` 在这一步退出：不改配置、不重建，但**已下载的镜像会留在本地**
2. **备份账号数据**（见下一节）—— 只在确认要升级之后才做
3. 改 compose 的 `image` 行（只动这一行，CasaOS 的其它字段一字不改）
4. `docker-compose up -d --force-recreate`
5. **冒烟**：容器 `running` + `/healthz` 返回 200 + **账号文件数没变** +
   `/status` 里 `healthy > 0`（确认账号池真的加载上了，而不是只看进程活着）
6. 清理悬空镜像

**任何一步失败都会自动回滚**：还原 compose 并重建回原镜像。数据全程只挂载、不写入，
所以回滚不会损伤账号。

### 账号数据备份

账号凭据在 `/DATA/AppData/wb2api/auths/*.json`。**这些文件丢了就得重新扫码登录**，
所以脚本把「备份」放在动手之前，而且是两份冗余：

| 位置 | 内容 | 用途 |
|---|---|---|
| `/root/wb2api-backups/<时间戳>/data.tar.gz` | 整个数据目录（auths + data + config.json） | 整体恢复 |
| `/root/wb2api-backups/<时间戳>/auths/` | 每个账号 json 的明文副本 | 人工核对 / 单点恢复 |
| `/root/wb2api-backups/<时间戳>/docker-compose.yml` | CasaOS 原始 compose | 回滚 |
| `/root/wb2api-backups/<时间戳>/meta.txt` | 升级前的镜像 id、账号数、文件数 | 对账 |

只想备份、不动服务：

```bash
bash /root/wb2api-pull-update.sh --backup-only
```

`--rollback` 读 `/root/wb2api-backups/last-good` 指向的那份备份，把 compose 和数据一起还原。
**回滚本身也可逆** —— 还原前会先把当前那份另存为
`/DATA/AppData/wb2api.before-rollback-<时间戳>`。

清理旧备份（保留最近 5 份）：

```bash
ls -1dt /root/wb2api-backups/*/ | tail -n +6 | xargs -r rm -rf
```

### 自动检查更新（可选）

设备上目前**没有任何 crontab**。想让它每天早上自己看一眼：

```bash
crontab -e
```

```cron
# 每天 04:30 检查一次，有新版才升级
30 4 * * * /bin/bash /root/wb2api-pull-update.sh >> /var/log/wb2api-update.log 2>&1
```

> 用 `--check` 更保守（只报告不升级），配合日志自己决定什么时候升。
> 注意设备根分区只有 2.8G 可用，脚本收尾会 `docker image prune -f`。

---

## 权限：容器为什么以 root 运行

设备上现役容器 `docker inspect` 显示 `User=[]`（即 root），
`/DATA/AppData/wb2api/config.json` 与 `auths/` 的属主也是 `root:root`。

`Dockerfile.armv7` 刻意**不写 `USER`**，与现役保持一致。
改成非 root（比如上游 Dockerfile 的 `USER app`，uid 10001）会立刻踩到：

```
rename /app/config.json.tmp /app/config.json: permission denied
```

要长期改成非 root，就得同时 `chown -R` 数据目录并给 compose 加
`user: "<uid>:<gid>"` —— 属于额外改动，本套配置不引入。

---

## 与升级前方案的关系

| | 升级前（本地 build） | 现在（镜像拉取） |
|---|---|---|
| 构建位置 | 玩客云本机（S805，极慢） | GitHub Actions（amd64 runner 交叉编译） |
| 升级动作 | Windows 上 `update.sh`，SFTP 传二进制 | 设备上一条 `docker pull` |
| 需要 Windows 参与 | 是 | **否** |
| 镜像来源 | `wb2api:local` | `ghcr.io/wliky/workbuddy2api-panel:armv7` |
| 数据目录 | `/DATA/AppData/wb2api` | **不变** |

`Dockerfile.armv7` 产出的 `/app` 布局与设备上 `wb2api:local` 逐字节同构
（`/app/{wb2api,signin_bin,login,credit,*.sh,probe_active.py}`），
三个挂载点 `/app/data`、`/app/auths`、`/app/config.json` 全部不变 ——
所以这是**原地替换**：账号池、配置、用量数据一个都不会丢。

旧的 `玩客云部署/update.sh`（本机交叉编译 → SFTP 传二进制 → 设备上本地 build 镜像）
在 GitHub Actions 出问题时仍可作兜底，但它**不更新镜像来源**，日常升级请用本目录方案。

---

## 排错

**`docker pull` 403 / unauthorized**
→ GHCR 包还是私有的。见上面「一次性配置 1」。

**拉得很慢或超时**
→ 国内连 GHCR 时好时坏。备选：到 Actions 页面手动跑 `build-armv7` 并勾选
`offline_tar`，下载 `wb2api-armv7-image-tar` 产物，传到设备后
`gunzip -c wb2api-armv7.tar.gz | docker load`。

**容器起来又立刻退出**
→ `docker logs --tail 50 wb2api`。最常见原因是 `/DATA/AppData/wb2api/config.json`
被当成目录创建了（宿主机上原本没有这个文件），`rm -rf` 该目录后用
`cp config.example.json config.json` 重建。

**CasaOS 里显示「待重建」**
→ 容器的 `com.docker.compose.project=wb2api` 标签丢了。
不要用 `docker run` 起容器，一律用 `docker-compose -f $APP_DIR/docker-compose.yml up -d`。

**升级后域名打不开面板**
→ 面板走根路径直达，需要配置里 `server.panel_root = true`。
确认 `https://wb.005201.xyz/` 能开（而不是 `/panel/`）；
再看 cloudflared 隧道 ingress 是否还指向 `localhost:7863`。

---

## 切换实测记录（2026-09-19）

在玩客云（S805 / armv7l / 983MB）上实跑了一次完整切换，留档备查。

| 项 | 结果 |
|---|---|
| fork 推送 | `Wliky/workbuddy2api-panel` main = `87d20ba`（8 个提交，含 armv7 CI） |
| 首次构建 | run 35427472608 —— 镜像其实已推送，但架构校验步骤误报失败（原因见下） |
| 修复后构建 | run 35427819434 / 35428084705 —— **全绿** |
| 镜像 | `ghcr.io/wliky/workbuddy2api-panel:armv7` = `sha256:30e0504e…`（92.2MB），config 里 `architecture=arm / variant=v7` |
| 包可见性 | 新建即 public —— 设备上**匿名 `docker pull` 直接成功**，无需 `docker login` |
| 升级前备份 | `/root/wb2api-backups/20260919-145808`（`data.tar.gz` + `auths/` 明文 4 份 + compose 原文） |
| 切换动作 | compose 第 17 行 `wb2api:local` → GHCR 镜像，`docker-compose up -d --force-recreate` |
| 冒烟 | 容器 `running` + `health=healthy` + `/healthz` 200 + 账号数 **4 → 4** + `/status healthy=4` |
| 数据 | 15 个用量桶（`data/usage.json` 2906B）完整恢复；日志 `loaded 4 account(s) from ./auths` |
| 域名 | `https://wb.005201.xyz/` → **200（51439B，`<title>WorkBuddy2API · 控制台</title>`）**，根路径直达 |
| CORS | 公网 OPTIONS 预检 → **204** 且带 `access-control-allow-origin: *`；带 Bearer 的真实请求同样带头 |
| 上游同步 | 手动 dispatch `sync-upstream` → success；上游无新提交，合并/推送/触发三步按预期 skipped |
| 磁盘 | 根分区 2877MB 可用，悬空镜像 0 |

### 与 `wb2api:local` 的差异（只有一处，且是刻意加的）

新镜像**多一个 `HEALTHCHECK`**（打 `127.0.0.1:7863/healthz`），现役旧镜像没有。
compose 里不写 healthcheck，于是容器会带上健康状态 —— CasaOS 与脚本都能据此
区分「进程活着」和「服务就绪」。其余 `EXPOSE`（都没有）、`USER`（都是 root）、
`Entrypoint/Cmd`、`Volumes`、`/app` 四个二进制大小**完全一致**。

### 首次构建失败的真正原因（不是镜像问题）

本工作流 `provenance=false` 且只推 `linux/arm/v7` 单平台，registry 里存的是一份
image manifest 而非 index，`docker buildx imagetools inspect` 对这种镜像**不打印
`Platform:` 行**，grep `linux/arm/v7` 必然误判失败，还会连带跳过离线 tar 与摘要两步。
已改为硬证据校验：`docker pull --platform linux/arm/v7` 后用
`docker image inspect --format '{{.Architecture}}{{.Variant}}'` 读镜像 config，
得到 `armv7` 才通过。

### CasaOS 会不会覆写 compose？

实测排查过：整机只有 `/var/lib/casaos/apps/wb2api/docker-compose.yml`（及 `.bak`）
引用 wb2api，`/var/lib/casaos/db/*.db` 里没有镜像定义 —— **compose 文件就是唯一事实源**，
直接改它 + `docker-compose up -d` 是安全的。切换后容器的
`com.docker.compose.project=wb2api` 标签仍在，CasaOS 不会显示「待重建」。
