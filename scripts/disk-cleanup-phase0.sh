#!/bin/bash
# ==============================================================================
# disk-cleanup-phase0.sh — 本地调用服务器磁盘止血（只读报告 / 安全清理）
# ==============================================================================
# 用法：
#   bash scripts/disk-cleanup-phase0.sh report   # 只读报告
#   bash scripts/disk-cleanup-phase0.sh clean    # 安全清理（保护现有容器镜像和业务卷）
# ==============================================================================
set -euo pipefail

MODE="${1:-report}"
[[ "${MODE}" == "report" || "${MODE}" == "clean" ]] || { echo "用法: $0 report|clean"; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REMOTE_HOST=""
if [[ -f "${SCRIPT_DIR}/../sync.env" ]]; then
  set -a; source "${SCRIPT_DIR}/../sync.env"; set +a
fi
REMOTE_HOST="${SYNC_REMOTE_HOST:-${REMOTE_HOST:-}}"
[[ -n "${REMOTE_HOST}" ]] || { echo "❌ 请先在 sync.env 配置 SYNC_REMOTE_HOST"; exit 1; }

echo "🚀 上传磁盘止血脚本并执行 ${MODE}"
scp -q "${SCRIPT_DIR}/server-disk-cleanup-phase0.sh" "${REMOTE_HOST}:/tmp/server-disk-cleanup-phase0.sh"
ssh "${REMOTE_HOST}" "bash /tmp/server-disk-cleanup-phase0.sh ${MODE}"
echo "✅ 完成（${MODE}）"
