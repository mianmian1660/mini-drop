#!/bin/bash
# ==============================================================================
# server-rollback-phase0.sh — 服务器端阶段 0 回滚（由本地 rollback-phase0.sh 调用）
# ==============================================================================
# 职责：
#   1. 找到最近一次发布保存的回滚快照（deploy-state/rollback-<ts>/）
#   2. 把旧 image ID 重新标记为 Compose 使用的镜像名
#   3. 用上一版 Compose 和环境快照执行 --no-build --force-recreate 切回
#   4. 不操作任何数据卷
#   5. 回滚后检查 API / PostgreSQL / MinIO / Agent / Web
#
# 用法：
#   bash server-rollback-phase0.sh [--dry-run]
# ==============================================================================
set -euo pipefail

DRY_RUN=0
if [[ "${1:-}" == "--dry-run" ]]; then DRY_RUN=1; fi

PROJECT="mini-drop"
SHARED="/home/ubuntu/mini-drop-shared"
STATE_DIR="${SHARED}/deploy-state"
ROLLBACK_SERVICES="apiserver drop_agent web_frontend"

say() { printf '[rollback] %s\n' "$*"; }
die() { printf '[rollback] ❌ %s\n' "$*" >&2; exit 1; }
run() {
  if [[ "$DRY_RUN" == "1" ]]; then printf '[rollback][dry-run] %s\n' "$*"; else "$@"; fi
}

# ---------------------------------------------------------------------------
# 1) 找到最新回滚快照
# ---------------------------------------------------------------------------
latest="$(find "${STATE_DIR}" -maxdepth 1 -type d -name 'rollback-*' 2>/dev/null | sort | tail -1)"
[[ -n "${latest}" && -d "${latest}" ]] || die "未找到回滚快照（${STATE_DIR}/rollback-*）"
say "使用回滚快照: ${latest}"

PREV_COMPOSE="$(cat "${latest}/previous-compose.txt")"
[[ -f "${PREV_COMPOSE}" ]] || die "上一版 compose 不存在: ${PREV_COMPOSE}"
say "上一版 compose: ${PREV_COMPOSE}"

# 环境快照（production.env 在切换时刻的副本）
SNAP_ENV="${latest}/production.env"
[[ -f "${SNAP_ENV}" ]] || die "环境快照缺失: ${SNAP_ENV}"

# ---------------------------------------------------------------------------
# 2) 把旧 image ID 重新标记为 Compose 使用的镜像名
# ---------------------------------------------------------------------------
say "重新标记旧镜像（image-ids.txt → <project>-<service>:latest）"
if [[ -f "${latest}/image-ids.txt" ]]; then
  while IFS='=' read -r svc imgid; do
    [[ -n "${svc}" && -n "${imgid}" ]] || continue
    tagname="${PROJECT}-${svc}:latest"
    run docker tag "${imgid}" "${tagname}"
    say "  ${tagname} ← ${imgid}"
  done < "${latest}/image-ids.txt"
else
  say "（无 image-ids.txt，跳过重新标记；依赖 compose 已有旧镜像名）"
fi

COMPOSE=("docker" "compose" "-p" "${PROJECT}" "--env-file" "${SNAP_ENV}" "-f" "${PREV_COMPOSE}")

# ---------------------------------------------------------------------------
# 3) 用上一版 Compose + 环境快照执行 --no-build --force-recreate
# ---------------------------------------------------------------------------
say "校验上一版 compose（config --quiet）"
run "${COMPOSE[@]}" config --quiet || die "上一版 compose 校验失败，无法回滚"

say "切回上一版（--no-deps --no-build --force-recreate，不操作任何数据卷）"
run "${COMPOSE[@]}" up -d --no-deps --no-build --force-recreate ${ROLLBACK_SERVICES}

if [[ "$DRY_RUN" == "0" ]]; then
  # apiserver 健康
  ok=0; for i in $(seq 1 30); do
    if curl -fsS http://127.0.0.1:8191/healthz 2>/dev/null | grep -q '"status":"ok"'; then ok=1; break; fi
    sleep 2
  done
  [[ "$ok" == "1" ]] || die "回滚后 apiserver /healthz 不健康"
  # drop_agent 运行态
  ok=0; for i in $(seq 1 30); do
    state="$(docker inspect -f '{{.State.Running}}' "${PROJECT}-drop_agent-1" 2>/dev/null || echo false)"
    [[ "$state" == "true" ]] && { ok=1; break; }
    sleep 2
  done
  [[ "$ok" == "1" ]] || die "回滚后 drop_agent 未运行"
  # web
  ok=0; for i in $(seq 1 30); do
    if curl -fsS http://127.0.0.1/health 2>/dev/null | grep -q 'OK'; then ok=1; break; fi
    sleep 2
  done
  [[ "$ok" == "1" ]] || die "回滚后 web /health 不健康"
fi

# ---------------------------------------------------------------------------
# 4) 更新 current.txt（回滚后当前部署 = 上一版）
# ---------------------------------------------------------------------------
if [[ "$DRY_RUN" == "0" ]]; then
  printf '%s\n' "${PREV_COMPOSE}" > "${STATE_DIR}/current.txt"
  say "current.txt → ${PREV_COMPOSE}"
else
  say "[dry-run] current.txt → ${PREV_COMPOSE}"
fi

# ---------------------------------------------------------------------------
# 5) 回滚后全面检查
# ---------------------------------------------------------------------------
if [[ "$DRY_RUN" == "0" ]]; then
  say "全面检查：API / PostgreSQL / MinIO / Agent / Web"
  curl -fsS http://127.0.0.1:8191/healthz >/dev/null && say "  API /healthz ok"          || die "API /healthz 异常"
  curl -fsS http://127.0.0.1:8191/readyz  >/dev/null && say "  API /readyz ok"           || die "API /readyz 异常"
  docker exec mini-drop-postgres-1 pg_isready -U postgres >/dev/null 2>&1 && say "  PostgreSQL ok" || die "PostgreSQL 异常"
  docker inspect -f '{{.State.Running}}' "${PROJECT}-minio-1" 2>/dev/null | grep -q true && say "  MinIO 容器运行中" || die "MinIO 容器异常"
  state="$(docker inspect -f '{{.State.Running}}' "${PROJECT}-drop_agent-1" 2>/dev/null || echo false)"
  [[ "$state" == "true" ]] && say "  Agent 容器运行中" || die "Agent 容器异常"
  curl -fsS http://127.0.0.1/health >/dev/null && say "  Web /health ok" || die "Web 异常"
  say "✅ 回滚完成：API / PostgreSQL / MinIO / Agent / Web 全部正常"
fi
