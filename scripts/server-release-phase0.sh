#!/bin/bash
# ==============================================================================
# server-release-phase0.sh — 服务器端阶段 0 发布（由本地 release-phase0.sh 调用）
# ==============================================================================
# 职责：
#   1. 校验 compose（config --quiet），失败禁止构建/切换
#   2. 构建前确认根盘可用 ≥ 8GiB，不足则中止
#   3. 切换前给所有运行服务镜像打 rollback tag，并保存回滚快照
#   4. 只构建 apiserver 与 web_frontend（drop_agent 只因环境变量变化 recreate）
#   5. 先构建全部候选，再依次切换 apiserver → drop_agent → web_frontend
#   6. 每步健康检查失败立即停止，不再继续切换
#   7. 全部即时验收通过后才更新 mini-drop-current
#   8. 只保留当前和上一个 release
#
# 用法：
#   bash server-release-phase0.sh <release_dir> <commit> <archive_sha256> [--dry-run]
# ==============================================================================
set -euo pipefail

RELEASE_DIR="${1:?usage: server-release-phase0.sh <release_dir> <commit> <sha> [--dry-run]}"
COMMIT="${2:?missing commit}"
ARCHIVE_SHA="${3:?missing archive sha256}"
DRY_RUN=0
if [[ "${4:-}" == "--dry-run" ]]; then DRY_RUN=1; fi

PROJECT="mini-drop"
RELEASES_ROOT="/home/ubuntu/mini-drop-releases"
SHARED="/home/ubuntu/mini-drop-shared"
PROD_ENV="${SHARED}/production.env"
STATE_DIR="${SHARED}/deploy-state"
MIN_BUILD_FREE_BYTES=$((8 * 1024 * 1024 * 1024))   # 8 GiB
BUILD_SERVICES="apiserver web_frontend"
SWITCH_SERVICES="apiserver drop_agent web_frontend"
TS="$(date +%Y%m%d-%H%M%S)"

say()  { printf '[release] %s\n' "$*"; }
die()  { printf '[release] ❌ %s\n' "$*" >&2; exit 1; }
run()  { # run <cmd...>  — dry-run 时只打印
  if [[ "$DRY_RUN" == "1" ]]; then printf '[release][dry-run] %s\n' "$*"; else "$@"; fi
}

# ---------------------------------------------------------------------------
# 0) 目录与基础文件
# ---------------------------------------------------------------------------
run mkdir -p "${RELEASES_ROOT}" "${SHARED}" "${STATE_DIR}"
if [[ "$DRY_RUN" == "0" ]]; then
  [[ -d "${RELEASE_DIR}" ]] || die "release 目录不存在: ${RELEASE_DIR}"
  [[ -f "${RELEASE_DIR}/docker-compose.yml" ]] || die "release 缺少 docker-compose.yml"
else
  say "[dry-run] 假定 release 目录已存在: ${RELEASE_DIR}"
fi

# production.env：首次生成安全默认值；已有则沿用（合并旧 .env 覆盖）
if [[ ! -f "${PROD_ENV}" ]]; then
  say "生成 production.env（阶段 0 安全默认值）"
  run bash -c "cat > '${PROD_ENV}' <<'EOF'
# 阶段 0 磁盘止血安全默认值（发布脚本统一注入）
DROP_NATIVE_CP_SPOOL_MAX_BYTES=1073741824
DROP_NATIVE_CP_SPOOL_MIN_FREE_BYTES=2147483648
DROP_GO_SYMBOL_CACHE_MAX_BYTES=268435456
STORAGE_WARNING_FREE_BYTES=8589934592
STORAGE_CRITICAL_FREE_BYTES=4294967296
STORAGE_MIN_FREE_BYTES=1073741824
CONTINUOUS_BLOCK_ENABLED=true
EOF"
fi
if [[ -f "/home/ubuntu/mini-drop/.env" ]]; then
  say "合并 /home/ubuntu/mini-drop/.env 覆盖到 production.env"
  run bash -c "cat '/home/ubuntu/mini-drop/.env' >> '${PROD_ENV}'"
fi

COMPOSE=("docker" "compose" "-p" "${PROJECT}" "--env-file" "${PROD_ENV}" "-f" "${RELEASE_DIR}/docker-compose.yml")

# ---------------------------------------------------------------------------
# 1) compose 校验：失败禁止构建和切换
# ---------------------------------------------------------------------------
say "校验 compose 配置（config --quiet）"
run "${COMPOSE[@]}" config --quiet || die "docker compose config 校验失败，禁止构建与切换"

# ---------------------------------------------------------------------------
# 2) 构建前磁盘检查：可用 ≥ 8GiB
# ---------------------------------------------------------------------------
if [[ "$DRY_RUN" == "0" ]]; then
  AVAIL_BYTES="$(df -B1 --output=avail / | tail -1 | tr -d ' ')"
  say "构建前根盘可用: $((AVAIL_BYTES / 1024 / 1024 / 1024)) GiB（要求 ≥ 8 GiB）"
  if [[ "${AVAIL_BYTES}" -lt "${MIN_BUILD_FREE_BYTES}" ]]; then
    die "根盘可用 ${AVAIL_BYTES} bytes < 8GiB，本次服务器构建停止（阶段 0 不通过删除业务数据强行腾空间）"
  fi
else
  say "[dry-run] 构建前磁盘检查：根盘可用需 ≥ 8 GiB"
fi

# ---------------------------------------------------------------------------
# 3) 切换前：给运行镜像打 rollback tag + 保存回滚快照
# ---------------------------------------------------------------------------
# 上一版 compose 路径：首次发布为 /home/ubuntu/mini-drop，后续为上一个 release
PREV_COMPOSE="/home/ubuntu/mini-drop/docker-compose.yml"
if [[ -f "${STATE_DIR}/current.txt" ]]; then
  PREV_COMPOSE="$(cat "${STATE_DIR}/current.txt")"
fi
if [[ "$DRY_RUN" == "0" ]]; then
  [[ -f "${PREV_COMPOSE}" ]] || die "上一版 compose 不存在: ${PREV_COMPOSE}"
else
  say "[dry-run] 上一版 compose: ${PREV_COMPOSE}"
fi

SNAP_DIR="${STATE_DIR}/rollback-${TS}"
run mkdir -p "${SNAP_DIR}"
if [[ "$DRY_RUN" == "1" ]]; then
  say "[dry-run] 保存回滚快照到 ${SNAP_DIR}"
else
  printf '%s\n' "${PREV_COMPOSE}" > "${SNAP_DIR}/previous-compose.txt"
  cp "${PROD_ENV}" "${SNAP_DIR}/production.env"
  "${COMPOSE[@]}" ps > "${SNAP_DIR}/compose-ps.txt" 2>&1 || true
  pg_isready -h 127.0.0.1 -p 15432 -U postgres > "${SNAP_DIR}/pg-health.txt" 2>&1 || true
fi

say "为当前运行服务镜像添加 rollback tag（rollback-${TS}）"
declare -A IMG_IDS=()
for svc in apiserver drop_agent drop_server web_frontend analysis; do
  cname="${PROJECT}-${svc}-1"
  img="$(docker inspect -f '{{.Image}}' "${cname}" 2>/dev/null || true)"
  if [[ -n "${img}" ]]; then
    IMG_IDS["${svc}"]="${img}"
    run docker tag "${img}" "${PROJECT}-${svc}:rollback-${TS}"
  fi
done
if [[ "$DRY_RUN" == "1" ]]; then
  say "[dry-run] 保存 image IDs 到 ${SNAP_DIR}/image-ids.txt"
else
  : > "${SNAP_DIR}/image-ids.txt"
  for svc in "${!IMG_IDS[@]}"; do
    printf '%s=%s\n' "${svc}" "${IMG_IDS[$svc]}" >> "${SNAP_DIR}/image-ids.txt"
  done
fi

# ---------------------------------------------------------------------------
# 4) 只构建 apiserver + web_frontend（drop_agent 不重建 C++ 镜像）
# ---------------------------------------------------------------------------
say "构建候选镜像：${BUILD_SERVICES}"
run "${COMPOSE[@]}" build ${BUILD_SERVICES}

# 记录构建后的 image ID（验收用）
API_IMG="$(docker inspect -f '{{.Id}}' "${PROJECT}-apiserver:latest")"
WEB_IMG="$(docker inspect -f '{{.Id}}' "${PROJECT}-web_frontend:latest")"

# ---------------------------------------------------------------------------
# 5) 写 release.json
# ---------------------------------------------------------------------------
run bash -c "cat > '${RELEASE_DIR}/release.json' <<EOF
{
  \"commit\": \"${COMMIT}\",
  \"branch\": \"storage-phase0\",
  \"build_time\": \"$(date -Iseconds)\",
  \"services\": [\"apiserver\", \"web_frontend\"],
  \"archive_sha256\": \"${ARCHIVE_SHA}\",
  \"previous_compose\": \"${PREV_COMPOSE}\",
  \"image_ids\": {\"apiserver\": \"${API_IMG}\", \"web_frontend\": \"${WEB_IMG}\"}
}
EOF"

# ---------------------------------------------------------------------------
# 6) 依次切换 + 每步健康检查（失败立即停止）
# ---------------------------------------------------------------------------
say "切换 apiserver（--no-deps --no-build）"
run "${COMPOSE[@]}" up -d --no-deps --no-build apiserver
if [[ "$DRY_RUN" == "0" ]]; then
  ok=0; for i in $(seq 1 30); do
    if curl -fsS http://127.0.0.1:8191/healthz 2>/dev/null | grep -q '"status":"ok"'; then ok=1; break; fi
    sleep 2
  done
  [[ "$ok" == "1" ]] || die "apiserver 健康检查失败（/healthz），立即停止切换，请运行 rollback-phase0.sh"
fi

say "切换 drop_agent（环境变量变化 → recreate，不重建镜像）"
run "${COMPOSE[@]}" up -d --no-deps --no-build drop_agent
if [[ "$DRY_RUN" == "0" ]]; then
  ok=0; for i in $(seq 1 30); do
    state="$(docker inspect -f '{{.State.Running}}' "${PROJECT}-drop_agent-1" 2>/dev/null || echo false)"
    [[ "$state" == "true" ]] && { ok=1; break; }
    sleep 2
  done
  [[ "$ok" == "1" ]] || die "drop_agent 切换后未处于运行状态，立即停止，请运行 rollback-phase0.sh"
fi

say "切换 web_frontend（--no-deps --no-build）"
run "${COMPOSE[@]}" up -d --no-deps --no-build web_frontend
if [[ "$DRY_RUN" == "0" ]]; then
  ok=0; for i in $(seq 1 30); do
    if curl -fsS http://127.0.0.1/health 2>/dev/null | grep -q 'OK'; then ok=1; break; fi
    sleep 2
  done
  [[ "$ok" == "1" ]] || die "web_frontend 健康检查失败（/health），立即停止，请运行 rollback-phase0.sh"
fi

# ---------------------------------------------------------------------------
# 7) 即时验收（端点级）
# ---------------------------------------------------------------------------
if [[ "$DRY_RUN" == "0" ]]; then
  say "即时验收：健康端点"
  curl -fsS http://127.0.0.1:8191/healthz   >/dev/null || die "验收失败: /healthz"
  curl -fsS http://127.0.0.1:8191/readyz    >/dev/null || die "验收失败: /readyz"
  curl -fsS http://127.0.0.1:8191/metrics   >/dev/null || die "验收失败: /metrics"
  curl -fsS http://127.0.0.1:8191/metrics   | grep -q 'mini_drop_storage_' || die "验收失败: /metrics 缺少 mini_drop_storage_* 指标"
  curl -fsS -H 'Drop-User-Uid: system-check' http://127.0.0.1:8191/api/v1/storage/status \
    | grep -q '"level"' || die "验收失败: /api/v1/storage/status 结构异常"
  curl -fsS http://127.0.0.1/health >/dev/null || die "验收失败: web /health"
fi

# ---------------------------------------------------------------------------
# 8) 全部通过 → 更新 mini-drop-current 与 current.txt，清理旧 release
# ---------------------------------------------------------------------------
say "更新 mini-drop-current → ${RELEASE_DIR}"
run ln -sfn "${RELEASE_DIR}" "/home/ubuntu/mini-drop-current"
run bash -c "printf '%s\n' '${RELEASE_DIR}/docker-compose.yml' > '${STATE_DIR}/current.txt'"

say "清理：只保留当前和上一个 release"
if [[ "$DRY_RUN" == "0" ]]; then
  # 按 {commit}-{timestamp} 字典序排序，保留最后两个（-mindepth 1 防止误删根目录）
  mapfile -t all < <(find "${RELEASES_ROOT}" -mindepth 1 -maxdepth 1 -type d -name '*-*-*' | sort)
  keep=2
  if [[ ${#all[@]} -gt ${keep} ]]; then
    for old in "${all[@]:0:$(( ${#all[@]} - keep ))}"; do
      # 兜底安全：仅删除含 docker-compose.yml 的 release 目录
      if [[ -f "${old}/docker-compose.yml" && "${old}" == "${RELEASES_ROOT}/"* ]]; then
        say "删除旧 release: ${old}"
        rm -rf "${old}"
      else
        say "跳过（非规范 release 目录）: ${old}"
      fi
    done
  fi
else
  say "[dry-run] 清理旧 release（保留最近两个）"
fi

say "✅ 发布完成：commit=${COMMIT} release=${RELEASE_DIR}"
say "   下一步：执行完整验收（轻量单次任务 / Continuous Session / rollback 实测）"
