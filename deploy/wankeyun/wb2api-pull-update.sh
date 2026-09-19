#!/bin/bash
# ===========================================================================
#  玩客云侧「镜像拉取」升级脚本  v2
#
#  在**玩客云设备上**执行（不是在你 Windows 上）。它做的是：
#      拉镜像 → 判断有无新版本 → 备份全部账号数据 → 切 compose 的 image
#      → 重建 → 冒烟（含账号池校验）→ 失败自动回滚
#
#  用法：
#      bash wb2api-pull-update.sh              # 正常升级
#      bash wb2api-pull-update.sh --check      # 只看有没有新版本，零副作用
#      bash wb2api-pull-update.sh --force      # 镜像未变也强制重建
#      bash wb2api-pull-update.sh --backup-only# 只备份账号数据，不动服务
#      bash wb2api-pull-update.sh --rollback   # 回滚到最近一次成功升级前的状态
#      bash wb2api-pull-update.sh --list       # 列出所有备份点
#
#  环境变量可覆盖：
#      IMAGE=ghcr.io/xxx/yyy:tag   APP_DIR=/var/lib/casaos/apps/wb2api
#      DATA_DIR=/DATA/AppData/wb2api   BACKUP_ROOT=/root/wb2api-backups
#
#  设计约束（全部来自设备实测，2026-09-19）：
#    · 设备**没有** `docker compose` 插件，但有独立版 /usr/local/bin/docker-compose
#      (v2.26.1) → 统一用 `docker-compose`，不要用 `docker compose`。
#    · 必须用 CasaOS 自己那份 compose 重建，容器才会带上
#      com.docker.compose.project=wb2api 标签、继续被 CasaOS 认作正规应用。
#      直接 docker run 会让它在 CasaOS 里退化成「待重建」。
#    · 根分区只剩 2.8G，镜像多留一份要占 ~100MB → 收尾必须清理悬空镜像。
#    · 容器以 root 运行，/DATA/AppData/wb2api 下属主也是 root —— 不要改 USER。
#    · 账号凭据在 $DATA_DIR/auths/*.json，丢了就得重新扫码登录 —— 所以
#      **备份先行**，且冒烟阶段必须校验账号文件数量没变、账号池真的加载了。
# ===========================================================================
set -euo pipefail

IMAGE="${IMAGE:-ghcr.io/wliky/workbuddy2api-panel:armv7}"
APP_DIR="${APP_DIR:-/var/lib/casaos/apps/wb2api}"
DATA_DIR="${DATA_DIR:-/DATA/AppData/wb2api}"
BACKUP_ROOT="${BACKUP_ROOT:-/root/wb2api-backups}"
COMPOSE="$APP_DIR/docker-compose.yml"
CONTAINER="wb2api"
PORT="${PORT:-7863}"
LAST_STATE="$BACKUP_ROOT/last-good"

MODE="update"
for a in "$@"; do
  case "$a" in
    --check)       MODE="check" ;;
    --force)       MODE="force" ;;
    --backup-only) MODE="backup" ;;
    --rollback)    MODE="rollback" ;;
    --list)        MODE="list" ;;
    -h|--help)     sed -n '2,26p' "$0"; exit 0 ;;
    *) echo "未知参数: $a（--help 看用法）" >&2; exit 2 ;;
  esac
done

say  () { printf '%s\n' "$*"; }
step () { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
warn () { printf '\033[33m!! %s\033[0m\n' "$*"; }
die  () { printf '\033[31m!! %s\033[0m\n' "$*" >&2; exit 1; }

need_root() { [ "$(id -u)" = "0" ] || die "请用 root 执行（sudo -i）"; }

img_id () { docker image inspect "$1" --format '{{.Id}}' 2>/dev/null || echo ""; }
health_ok () { curl -fsS -m 5 -o /dev/null "http://127.0.0.1:${PORT}/healthz"; }
auth_count () { ls -1 "$DATA_DIR/auths"/*.json 2>/dev/null | wc -l | tr -d ' '; }

# 从 config.json 抠出 api_key（宿主机不一定有 python3，用 sed）
read_api_key () {
  sed -n 's/.*"api_key"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$DATA_DIR/config.json" 2>/dev/null | head -1
}

# 用 /status 验证账号池真的加载了（比 /healthz 强得多）
# 成功：stdout 为 healthy 数，return 0
# 失败：stdout 为错误标记，return 1
status_probe () {
  local key js h
  key="$(read_api_key)"
  [ -n "$key" ] || { echo "no-api-key"; return 1; }
  js="$(curl -fsS -m 8 -H "Authorization: Bearer $key" \
             "http://127.0.0.1:${PORT}/status" 2>/dev/null || true)"
  [ -n "$js" ] || { echo "no-status-response"; return 1; }

  if command -v python3 >/dev/null 2>&1; then
    h="$(printf '%s' "$js" | python3 -c 'import sys,json
try:
    d = json.load(sys.stdin)
    v = d.get("healthy", "")
    print(v if isinstance(v, int) else "")
except Exception:
    print("")' 2>/dev/null || true)"
  else
    h="$(printf '%s' "$js" | tr ',{}' '\n\n\n' \
         | sed -n 's/.*"healthy"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' | head -1)"
  fi

  case "$h" in
    ''|*[!0-9]*) echo "healthy-unparsable"; return 1 ;;
  esac
  echo "$h"
}

# ---------------------------------------------------------------------------
# --list：列出备份点
# ---------------------------------------------------------------------------
if [ "$MODE" = "list" ]; then
  if [ -d "$BACKUP_ROOT" ]; then
    ls -1dt "$BACKUP_ROOT"/*/ 2>/dev/null | while read -r d; do
      printf '%s  compose=%s  data=%s  auths=%s\n' \
        "$(basename "$d")" \
        "$([ -f "$d/docker-compose.yml" ] && echo ok || echo -)" \
        "$([ -f "$d/data.tar.gz" ] && echo ok || echo -)" \
        "$(ls -1 "$d/auths"/*.json 2>/dev/null | wc -l | tr -d ' ')"
    done
  else
    say "还没有任何备份"
  fi
  exit 0
fi

need_root

# ---------------------------------------------------------------------------
# --rollback：恢复到最近一次成功升级前的状态
# ---------------------------------------------------------------------------
if [ "$MODE" = "rollback" ]; then
  step "回滚"
  [ -f "$LAST_STATE" ] || die "找不到 $LAST_STATE（只在成功升级过一次之后才存在）"
  TARGET="$(cat "$LAST_STATE")"
  [ -f "$TARGET/docker-compose.yml" ] || die "回滚点 $TARGET 里没有 docker-compose.yml"
  say "    回滚点: $TARGET"

  cp -a "$COMPOSE" "$COMPOSE.before-rollback-$(date +%Y%m%d-%H%M%S)"
  cp -a "$TARGET/docker-compose.yml" "$COMPOSE"
  say "    compose 已还原"

  if [ -f "$TARGET/data.tar.gz" ]; then
    cp -a "$DATA_DIR" "$DATA_DIR.before-rollback-$(date +%Y%m%d-%H%M%S)"
    rm -rf "$DATA_DIR"
    tar xzf "$TARGET/data.tar.gz" -C "$(dirname "$DATA_DIR")"
    say "    数据已还原（还原前那份留在 $DATA_DIR.before-rollback-*）"
  fi

  cd "$APP_DIR"
  docker-compose up -d --force-recreate 2>&1 | tail -5
  sleep 8
  health_ok && say "✅ 回滚完成，/healthz 正常" || die "回滚后 /healthz 仍不通，去看 docker logs $CONTAINER"
  exit 0
fi

# ---------------------------------------------------------------------------
# 前置检查
# ---------------------------------------------------------------------------
command -v docker >/dev/null         || die "找不到 docker"
command -v docker-compose >/dev/null || die "找不到 docker-compose（带连字符的独立版）"
[ -f "$COMPOSE" ] || die "找不到 $COMPOSE —— CasaOS 里的 wb2api 应用可能没装好"

step "0/6 前置检查"
say "    镜像      : $IMAGE"
say "    应用目录  : $APP_DIR"
say "    数据目录  : $DATA_DIR"
say "    备份目录  : $BACKUP_ROOT"
FREE_MB=$(df -Pm / | awk 'NR==2{print $4}')
say "    根分区剩余: ${FREE_MB}MB"
[ "$FREE_MB" -ge 300 ] || die "根分区剩余不足 300MB，先清理：docker image prune -a"

AUTH_BEFORE="$(auth_count)"
say "    当前账号数: $AUTH_BEFORE"
[ "$AUTH_BEFORE" -gt 0 ] || warn "auths 里没有账号文件 —— 继续，但升级后需要重新登录"

# ---------------------------------------------------------------------------
# 备份函数
# ---------------------------------------------------------------------------
do_backup () {
  local stamp="$1" bdir="$BACKUP_ROOT/$1"
  mkdir -p "$bdir"
  # 1) 整个数据目录（auths/ + data/ + config.json）—— 保命的那一份
  tar czf "$bdir/data.tar.gz" -C "$(dirname "$DATA_DIR")" "$(basename "$DATA_DIR")"
  # 2) 账号文件另留一份明文，方便人工核对/单点恢复
  if [ -d "$DATA_DIR/auths" ]; then
    mkdir -p "$bdir/auths"
    cp -a "$DATA_DIR/auths/." "$bdir/auths/" 2>/dev/null || true
  fi
  # 3) CasaOS 的 compose 原文（回滚靠它）
  cp -a "$COMPOSE" "$bdir/docker-compose.yml"
  # 4) 元信息
  {
    echo "stamp=$stamp"
    echo "image_in_compose=$(sed -n 's/^[[:space:]]*image:[[:space:]]*//p' "$COMPOSE" | head -1)"
    echo "running_image_id=$(docker inspect "$CONTAINER" --format '{{.Image}}' 2>/dev/null || echo none)"
    echo "auth_count=$(auth_count)"
    echo "data_files=$(find "$DATA_DIR" -type f | wc -l | tr -d ' ')"
  } > "$bdir/meta.txt"
  echo "$bdir"
}

# ---------------------------------------------------------------------------
# --backup-only：只备份
# ---------------------------------------------------------------------------
if [ "$MODE" = "backup" ]; then
  STAMP="$(date +%Y%m%d-%H%M%S)"
  step "只备份（不动服务）"
  BDIR="$(do_backup "$STAMP")"
  say "    $BDIR（$(du -sh "$BDIR" | cut -f1)，账号 $AUTH_BEFORE 个）"
  exit 0
fi

# ---------------------------------------------------------------------------
# 1. 拉取（--check 在这里就结束，零副作用）
# ---------------------------------------------------------------------------
step "1/6 拉取镜像"
BEFORE_ID=$(img_id "$IMAGE")
say "    本地当前  : ${BEFORE_ID:-（无）}"

if ! docker pull "$IMAGE"; then
  if [ "$MODE" = "check" ]; then
    die "拉取失败（--check 模式下不改动任何东西）"
  fi
  die "docker pull 失败。检查：1) 网络 2) GHCR 包是否已设为 Public 3) 是否需 docker login ghcr.io"
fi

AFTER_ID=$(img_id "$IMAGE")
say "    远端最新  : $AFTER_ID"

SAME=0
[ "$BEFORE_ID" = "$AFTER_ID" ] && SAME=1

if [ "$MODE" = "check" ]; then
  if [ "$SAME" = "1" ]; then
    step "已是最新（--check 模式，未做任何改动）"
  else
    step "有新版本可用（--check 模式，未做任何改动）"
    say "    本地: $BEFORE_ID"
    say "    远端: $AFTER_ID"
  fi
  exit 0
fi

if [ "$SAME" = "1" ] && [ "$MODE" != "force" ]; then
  step "已是最新，无需重建"
  say "    当前镜像 id 未变化（$AFTER_ID）"
  say "    想强制重建：bash $0 --force"
  exit 0
fi

# ---------------------------------------------------------------------------
# 2. 备份账号数据（动手前最后一道保险）
# ---------------------------------------------------------------------------
STAMP="$(date +%Y%m%d-%H%M%S)"
step "2/6 备份账号数据"
BDIR="$(do_backup "$STAMP")"
say "    数据打包  → $BDIR/data.tar.gz"
say "    账号明文  → $BDIR/auths/（$AUTH_BEFORE 个）"
say "    compose   → $BDIR/docker-compose.yml"
say "    体积      : $(du -sh "$BDIR" | cut -f1)"

# ---------------------------------------------------------------------------
# 3. 切换 compose 的 image 字段（幂等）
# ---------------------------------------------------------------------------
step "3/6 对齐 compose 的 image 字段"
CUR_IMAGE_LINE=$(grep -nE '^[[:space:]]*image:' "$COMPOSE" | head -1 || true)
[ -n "$CUR_IMAGE_LINE" ] || die "$COMPOSE 里找不到 image: 行，请手工检查"

CUR_IMAGE=$(printf '%s' "$CUR_IMAGE_LINE" | sed -E 's/^[0-9]+:[[:space:]]*image:[[:space:]]*//; s/[[:space:]]*$//')
if [ "$CUR_IMAGE" = "$IMAGE" ]; then
  say "    已是 $IMAGE，跳过改写"
else
  say "    $CUR_IMAGE → $IMAGE"
  LINE_NO=$(printf '%s' "$CUR_IMAGE_LINE" | cut -d: -f1)
  INDENT=$(sed -n "${LINE_NO}p" "$COMPOSE" | sed -E 's/^( *).*/\1/')
  {
    head -n $((LINE_NO - 1)) "$COMPOSE"
    printf '%simage: %s\n' "$INDENT" "$IMAGE"
    tail -n +$((LINE_NO + 1)) "$COMPOSE"
  } > "$COMPOSE.new"
  mv "$COMPOSE.new" "$COMPOSE"
  say "    已改写（第 ${LINE_NO} 行）"
fi

# ---------------------------------------------------------------------------
# 4. 重建容器
# ---------------------------------------------------------------------------
step "4/6 重建容器"
cd "$APP_DIR"
# --force-recreate：image tag 未变但 digest 变了时 compose 不会自动重建，
# 必须显式强制（这正是设备上 update.sh 踩过的点）。
if ! docker-compose up -d --force-recreate 2>&1 | tail -6; then
  warn "compose 重建失败，回滚 compose 定义"
  cp -a "$BDIR/docker-compose.yml" "$COMPOSE"
  exit 1
fi

# ---------------------------------------------------------------------------
# 5. 冒烟：容器状态 + /healthz + 账号文件数 + /status 账号池
# ---------------------------------------------------------------------------
step "5/6 冒烟检查"
OK=0
for i in $(seq 1 12); do
  sleep 5
  STATE=$(docker inspect -f '{{.State.Status}}' "$CONTAINER" 2>/dev/null || echo missing)
  if [ "$STATE" = "running" ] && health_ok; then OK=1; break; fi
  say "    第 $i 次：state=$STATE，/healthz 未通，继续等..."
done

FAIL_REASON=""
if [ "$OK" != "1" ]; then
  FAIL_REASON="容器未 running 或 /healthz 不通"
else
  AUTH_AFTER="$(auth_count)"
  say "    /healthz 通过；账号数 $AUTH_BEFORE → $AUTH_AFTER"
  if [ "$AUTH_AFTER" != "$AUTH_BEFORE" ]; then
    FAIL_REASON="账号文件数变了（$AUTH_BEFORE → $AUTH_AFTER）"
  else
    HEALTHY="$(status_probe || true)"
    say "    /status healthy=$HEALTHY"
    case "$HEALTHY" in
      ''|no-api-key|no-status-response|healthy-unparsable)
        FAIL_REASON="/status 鉴权或响应异常（$HEALTHY）" ;;
      *)
        [ "$HEALTHY" -gt 0 ] || FAIL_REASON="/status healthy=0，账号池没加载上" ;;
    esac
  fi
fi

if [ -n "$FAIL_REASON" ]; then
  warn "冒烟失败：$FAIL_REASON"
  say "--- 最近 30 行日志 ---"
  docker logs --tail 30 "$CONTAINER" 2>&1 || true
  say "--- 回滚 ---"
  cp -a "$BDIR/docker-compose.yml" "$COMPOSE"
  cd "$APP_DIR"
  docker-compose up -d --force-recreate 2>&1 | tail -5 || true
  sleep 8
  if health_ok; then
    say "✅ 已回滚到升级前的镜像与 compose，服务恢复"
    say "   数据未被动过：本次升级只挂载 auths/data，不写入"
  else
    say "!! 回滚后 /healthz 仍不通，请人工介入：docker logs $CONTAINER"
    say "   账号数据完好躺在 $BDIR/data.tar.gz，恢复方法见 README"
  fi
  exit 1
fi

# 校验 compose project 标签还在（丢了 CasaOS 会显示「待重建」）
PROJECT=$(docker inspect "$CONTAINER" --format '{{index .Config.Labels "com.docker.compose.project"}}' 2>/dev/null || echo "")
if [ "$PROJECT" != "wb2api" ]; then
  warn "compose project 标签为 '$PROJECT'（期望 wb2api），CasaOS 可能显示为待重建"
fi

say "    ✅ 容器 running，/healthz 正常，账号池已加载"
docker inspect "$CONTAINER" --format '    镜像={{.Config.Image}} 启动={{.State.StartedAt}}'

# 记下这次升级前的状态，供 --rollback 使用
mkdir -p "$BACKUP_ROOT"
echo "$BDIR" > "$LAST_STATE"

# ---------------------------------------------------------------------------
# 6. 清理
# ---------------------------------------------------------------------------
step "6/6 清理"
docker image prune -f >/dev/null 2>&1 || true
say "    悬空镜像已清理"
say "    备份保留在 $BDIR（$(du -sh "$BDIR" | cut -f1)），可随时 --rollback"
say "    清理旧备份（保留最近 5 份）：ls -1dt $BACKUP_ROOT/*/ | tail -n +6 | xargs rm -rf"

step "完成"
say "    面板 : https://wb.005201.xyz/（根路径直达，无需 /panel 后缀）"
say "    状态 : docker inspect -f '{{.State.Status}}' $CONTAINER"
say "    日志 : docker logs -f --tail 50 $CONTAINER"
say "    回滚 : bash $0 --rollback"
say "    备份 : bash $0 --list"
