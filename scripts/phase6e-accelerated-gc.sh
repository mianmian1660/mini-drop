#!/bin/bash
# ==============================================================================
# phase6e-accelerated-gc.sh — 加速 GC 临时开启 / 回滚（服务器执行）
# ==============================================================================
# 前置：scripts/server-check-phase6e-enforce.sh <SID> 已 RESULT=PASS。
# 本脚本把保留期临时压到 5 分钟以加速验证细粒度 GC（正常保留期 120 分钟）。
#
# 用法：
#   bash scripts/phase6e-accelerated-gc.sh start
#       备份 .env → CONTINUOUS_FINE_ROW_GC_MODE=enforce +
#       CONTINUOUS_HOT_METADATA_RETENTION_MINUTES=5 → 重建 apiserver。
#   bash scripts/phase6e-accelerated-gc.sh restore
#       恢复 start 前的 .env（FINE_ROW_GC=observe、retention=120）→ 重建 apiserver。
#       （加速 GC 任一安全条件失败时调用，回到 observe，不得进入最终 enforce。）
#
# 观察至少 3 个 5 分钟 worker 周期（最长 30 分钟）后，若通过标准满足，
# 直接运行 phase6e-final-switch.sh 进入正式 enforce（retention=120）。
# ==============================================================================
set -euo pipefail

ROOT=/home/ubuntu/mini-drop
cd "${ROOT}"
[ -f .env ] || { echo "❌ .env 不存在" >&2; exit 1; }
mkdir -p backups/phase6
ENV_BAK="${ROOT}/backups/phase6/env-accelerated-gc.bak"

set_env() {
  local key="$1" value="$2"
  grep -q "^${key}=" .env || echo "${key}=${value}" >> .env
  sed -i.bak "s|^${key}=.*|${key}=${value}|" .env
}

case "${1:-}" in
  start)
    cp .env "${ENV_BAK}"
    echo "==> 备份 .env 到 ${ENV_BAK}"
    set_env CONTINUOUS_FINE_ROW_GC_MODE enforce
    set_env CONTINUOUS_HOT_METADATA_RETENTION_MINUTES 5
    echo "==> 临时加速 GC 配置："
    grep -E '^(CONTINUOUS_FINE_ROW_GC_MODE|CONTINUOUS_HOT_METADATA_RETENTION_MINUTES)=' .env
    docker compose up -d --no-deps --force-recreate apiserver
    echo "✅ 加速 GC 已开启（retention=5）。观察至少 3 个 5 分钟周期（最长 30 分钟）。"
    echo "   通过后运行 phase6e-final-switch.sh；失败则运行 $0 restore。"
    ;;
  restore)
    if [ ! -f "${ENV_BAK}" ]; then
      echo "❌ 未找到加速 GC 前的 .env 备份: ${ENV_BAK}" >&2
      exit 1
    fi
    cp "${ENV_BAK}" .env
    echo "==> 已恢复加速 GC 前配置："
    grep -E '^(CONTINUOUS_FINE_ROW_GC_MODE|CONTINUOUS_HOT_METADATA_RETENTION_MINUTES)=' .env
    docker compose up -d --no-deps --force-recreate apiserver
    echo "✅ 已回滚到 observe（保留期 120）。不得进入最终 enforce。"
    ;;
  *)
    echo "用法: bash $0 start | bash $0 restore" >&2
    exit 2
    ;;
esac
