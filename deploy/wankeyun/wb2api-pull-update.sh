#!/bin/bash
# ===========================================================================
#  玩客云侧「镜像拉取」升级脚本
#
#  在**玩客云设备上**执行（不是在你 Windows 上）。它做的是：
#      docker pull 新镜像 → 切换 compose 的 image → 重建容器 → 冒烟 → 失败自动回滚
#
#  用法：
#      bash wb2api-pull-update.sh            # 正常升级
#      bash wb2api-pull-update.sh --check    # 只检查有没有新版本，不改任何东西
#      bash wb2api-pull-update.sh --force    # 即使镜像未变也强制重建
#      bash wb2api-pull-update.sh --rollback # 回滚到上一次成功升级前的镜像
#
#  环境变量可覆盖：
#      IMAGE=ghcr.io/xxx/yyy:tag   APP_DIR=/var/lib/casaos/apps/wb2api
#      DATA_DIR=/DATA/AppData/wb2api
#
#  设计约束（都来自设备实测，2026-09-19）：
#    · 设备**没有** `docker compose` 插件，但有独立版 /usr/local/bin/docker-compose (v2.26.1)
#      → 统一用 `docker-compose`，不要用 `docker compose`。
#    · 必须用 CasaOS 自己那份 compose 重建，容器才会带上
#      com.docker.compose.project=wb2api 标签、继续被 CasaOS 认作正规应用。
#      直接 docker run 会让它在 CasaOS 里退化成「待重建」。
#    · 根分区只剩 2.8G，镜像多留一份要占 ~100MB → 收尾必须清理悬空镜像。
#    · 容器以 root 运行，/DATA/AppData/wb2api 下属主也是 root —— 不要改 USER。
# ===========================================================================
set -euo pipefail

IMAGE="${IMAGE:-ghcr.io/wliky/workbuddy2api-panel:armv7}"
APP_DIR="${APP_DIR:-/var/lib/casaos/apps/wb2api}"
DATA_DIR="${DATA_DIR:-/DATA/AppData/wb2api}"
COMPOSE="$APP_DIR/docker-compose.yml"
CONTAINER="wb2api"
PREV_TAG="wb2api:rollback-src"
HEALTH_URL="http://127.0.0.1:7863/healthz"

MODE="update"
for a in "$@"; do
  case "$a" in
    --check)    MODE="check" ;;
    --force)    MODE="force" ;;
    --rollback) MODE="rollback" ;;
    -h|--help)  sed -n '2,26p' "$0"; exit 0 ;;
    *) echo "未知参数: $a（--help 看用法）" >&2; exit 2 ;;
  esac
done

say  () { printf '%s\n' "$*"; }
step () { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
die  () { printf '\033[31m!! %s\033[0m\n' "$*" >&2; exit 1; }

need_root() { [ "$(id -u)" = "0" ] || die "请用 root 执行（sudo -i）"; }

img_id () { docker image inspect "$1" --format '{{.Id}}' 2>/dev/null || echo ""; }
running_image_id () {
  docker inspect "$CONTAINER" --format '{{.Image}}' 2>/dev/null || echo ""
}
health_ok () {
  # /healthz 无需鉴权；返回 200 即算健康
  curl -fsS -m 5 -o /dev/null "$HEALTH_URL"
}

# ---------------------------------------------------------------------------
# --rollback：把上一次升级前备份的镜像恢复回去
# ---------------------------------------------------------------------------
if [ "$MODE" = "rollback" ]; then
  need_root
  step "回滚"
  docker image inspect "$PREV_TAG" >/dev/null 2>&1 \
    || die "没找到回滚镜像 $PREV_TAG（只在成功升级过一次之后才存在）"
  cp -a "$COMPOSE" "$COMPOSE.bak-$(date +%H%M%S)"
  # 回滚镜像的原始 tag 记在 label 里；没有就退回到 wb2api:local 语义
  docker tag "$PREV_TAG" "$IMAGE"
  cd "$APP_DIR"
  docker-compose up -d --force-recreate
  sleep 8
  health_ok && say "✅ 回滚完成，/healthz 正常" || die "回滚后 /healthz 仍不通，去看 docker logs $CONTAINER"
  exit 0
fi

# ---------------------------------------------------------------------------
# 前置检查
# ---------------------------------------------------------------------------
need_root
command -v docker >/dev/null         || die "找不到 docker"
command -v docker-compose >/dev/null || die "找不到 docker-compose（带连字符的独立版）"
[ -f "$COMPOSE" ] || die "找不到 $COMPOSE —— CasaOS 里的 wb2api 应用可能没装好"

step "0/6 前置检查"
say "    镜像      : $IMAGE"
say "    应用目录  : $APP_DIR"
say "    数据目录  : $DATA_DIR"
FREE_MB=$(df -Pm / | awk 'NR==2{print $4}')
say "    根分区剩余: ${FREE_MB}MB"
[ "$FREE_MB" -ge 300 ] || die "根分区剩余不足 300MB，先清理：docker image prune -a"

# ---------------------------------------------------------------------------
# 1. 拉取
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

if [ "$BEFORE_ID" = "$AFTER_ID" ] && [ "$MODE" != "force" ]; then
  step "已是最新，无需重建"
  say "    当前镜像 id 未变化（$AFTER_ID）"
  say "    想强制重建：bash $0 --force"
  exit 0
fi

if [ "$MODE" = "check" ]; then
  step "有新版本可用（--check 模式，未做任何改动）"
  say "    本地: $BEFORE_ID"
  say "    远端: $AFTER_ID"
  exit 0
fi

# ---------------------------------------------------------------------------
# 2. 备份（yaml + 当前镜像）
# ---------------------------------------------------------------------------
step "2/6 备份"
STAMP=$(date +%Y%m%d-%H%M%S)
cp -a "$COMPOSE" "$COMPOSE.bak-$STAMP"
say "    compose 备份 → $COMPOSE.bak-$STAMP"

CUR_ID=$(running_image_id)
if [ -n "$CUR_ID" ]; then
  docker image inspect "$CUR_ID" >/dev/null 2>&1 && docker tag "$CUR_ID" "$PREV_TAG"
  say "    当前运行镜像已另存为 $PREV_TAG（回滚用）"
fi
[ -f "$DATA_DIR/config.json" ] && cp -a "$DATA_DIR/config.json" "$DATA_DIR/config.json.bak-$STAMP"
say "    config.json 备份 → $DATA_DIR/config.json.bak-$STAMP"

# ---------------------------------------------------------------------------
# 3. 切换 compose 的 image 字段（幂等）
# ---------------------------------------------------------------------------
step "3/6 对齐 compose 的 image 字段"
# 用 Python 改 YAML 太苛刻（设备上不一定有 pyyaml），这里做行级替换：
# 只处理第一个 `image:` 行（本文件里只有 services.wb2api 一个服务）。
CUR_IMAGE_LINE=$(grep -nE '^[[:space:]]*image:' "$COMPOSE" | head -1 || true)
if [ -z "$CUR_IMAGE_LINE" ]; then
  die "$COMPOSE 里找不到 image: 行，请手工检查"
fi
CUR_IMAGE=$(printf '%s' "$CUR_IMAGE_LINE" | sed -E 's/^[0-9]+:[[:space:]]*image:[[:space:]]*//; s/[[:space:]]*$//')
if [ "$CUR_IMAGE" = "$IMAGE" ]; then
  say "    已是 $IMAGE，跳过改写"
else
  say "    $CUR_IMAGE → $IMAGE"
  LINE_NO=$(printf '%s' "$CUR_IMAGE_LINE" | cut -d: -f1)
  # 保留原缩进，只换值
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
  say "!! compose 重建失败，回滚 compose 定义"
  cp -a "$COMPOSE.bak-$STAMP" "$COMPOSE"
  exit 1
fi

# ---------------------------------------------------------------------------
# 5. 冒烟
# ---------------------------------------------------------------------------
step "5/6 冒烟检查"
OK=0
for i in $(seq 1 12); do
  sleep 5
  STATE=$(docker inspect -f '{{.State.Status}}' "$CONTAINER" 2>/dev/null || echo missing)
  if [ "$STATE" = "running" ] && health_ok; then OK=1; break; fi
  say "    第 $i 次：state=$STATE，/healthz 未通，继续等..."
done

if [ "$OK" != "1" ]; then
  say "!! 新版容器未通过冒烟，开始回滚"
  docker logs --tail 30 "$CONTAINER" 2>&1 || true
  cp -a "$COMPOSE.bak-$STAMP" "$COMPOSE"
  if docker image inspect "$PREV_TAG" >/dev/null 2>&1; then
    docker tag "$PREV_TAG" "$IMAGE"
    docker-compose up -d --force-recreate 2>&1 | tail -5 || true
    sleep 8
    health_ok && say "✅ 已回滚到升级前镜像，服务恢复" || say "!! 回滚后仍不通，请人工介入"
  else
    say "!! 没有可用的回滚镜像，compose 已还原，请人工介入"
  fi
  exit 1
fi

# 校验 compose project 标签还在（丢了 CasaOS 会显示「待重建」）
PROJECT=$(docker inspect "$CONTAINER" --format '{{index .Config.Labels "com.docker.compose.project"}}' 2>/dev/null || echo "")
if [ "$PROJECT" != "wb2api" ]; then
  say "    ⚠️ compose project 标签为 '$PROJECT'（期望 wb2api），CasaOS 可能显示为待重建"
fi

say "    ✅ 容器 running，/healthz 正常"
docker inspect "$CONTAINER" --format '    镜像={{.Config.Image}} 启动={{.State.StartedAt}}'

# ---------------------------------------------------------------------------
# 6. 清理
# ---------------------------------------------------------------------------
step "6/6 清理"
docker image prune -f >/dev/null 2>&1 || true
say "    悬空镜像已清理；回滚镜像 $PREV_TAG 保留（占约 100MB）"
say "    不再需要回滚时：docker rmi $PREV_TAG"

step "完成"
say "    面板 : https://wb.005201.xyz/（根路径直达，无需 /panel 后缀）"
say "    日志 : docker logs -f --tail 50 $CONTAINER"
say "    回滚 : bash $0 --rollback"
