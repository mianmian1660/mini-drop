#!/bin/bash
# ==============================================================================
# rollback-phase0.sh — 本地阶段 0 回滚编排
# ==============================================================================
# 把服务器切回最近一次发布前的版本（使用发布时保存的 rollback 快照）：
#   旧 image ID 重新标记 → 上一版 Compose + 环境快照 --no-build --force-recreate
#   → 全面检查 API / PostgreSQL / MinIO / Agent / Web
#
# 用法：
#   bash scripts/rollback-phase0.sh [--dry-run]
# ==============================================================================
set -euo pipefail

DRY_RUN=0
if [[ "${1:-}" == "--dry-run" ]]; then DRY_RUN=1; fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REMOTE_HOST=""
if [[ -f "${SCRIPT_DIR}/../sync.env" ]]; then
  set -a; source "${SCRIPT_DIR}/../sync.env"; set +a
fi
REMOTE_HOST="${SYNC_REMOTE_HOST:-${REMOTE_HOST:-}}"
[[ -n "${REMOTE_HOST}" ]] || { echo "❌ 请先在 sync.env 配置 SYNC_REMOTE_HOST"; exit 1; }

say() { printf '[rollback] %s\n' "$*"; }

say "上传服务器端回滚脚本"
if [[ "$DRY_RUN" == "1" ]]; then
  say "[dry-run] scp ${SCRIPT_DIR}/server-rollback-phase0.sh ${REMOTE_HOST}:/tmp/"
  say "[dry-run] ssh ${REMOTE_HOST} bash /tmp/server-rollback-phase0.sh --dry-run"
else
  scp -q "${SCRIPT_DIR}/server-rollback-phase0.sh" "${REMOTE_HOST}:/tmp/server-rollback-phase0.sh"
  ssh "${REMOTE_HOST}" "bash /tmp/server-rollback-phase0.sh"
fi

say "✅ 回滚编排完成"
