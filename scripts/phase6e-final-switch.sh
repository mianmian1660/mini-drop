#!/bin/bash
# ==============================================================================
# phase6e-final-switch.sh — Release 6E：加速验收通过后的最终正式切换
# ==============================================================================
# 前置：先运行 scripts/server-check-phase6e-enforce.sh <SID> 且 RESULT=PASS。
# 本脚本：
#   1. 把生产配置显式写入 .env：
#        CONTINUOUS_PARQUET_MODE=enforce
#        CONTINUOUS_FINE_ROW_GC_MODE=enforce
#        CONTINUOUS_HOT_METADATA_RETENTION_MINUTES=120
#   2. 仅重建 apiserver 容器应用新配置（不重建镜像）
#   3. 等待健康检查；失败时恢复 .env 并回滚容器
# 存储重置已将 schedule_tasks 永久清空，本脚本不恢复任何历史调度。
# 用法：bash scripts/phase6e-final-switch.sh
# ==============================================================================
set -euo pipefail

ROOT=/home/ubuntu/mini-drop
cd "${ROOT}"
[ -f .env ] || { echo "❌ .env 不存在" >&2; exit 1; }

# 备份当前 .env 以便失败回滚
ENV_BAK=".env.pre-final-switch-$(date +%Y%m%d-%H%M%S)"
cp .env "${ENV_BAK}"

set_env() {
  local key="$1" value="$2"
  grep -q "^${key}=" .env || echo "${key}=${value}" >> .env
  sed -i.bak "s|^${key}=.*|${key}=${value}|" .env
}

echo "==> [1/3] 写入正式生产配置"
set_env CONTINUOUS_PARQUET_MODE enforce
set_env CONTINUOUS_FINE_ROW_GC_MODE enforce
set_env CONTINUOUS_HOT_METADATA_RETENTION_MINUTES 120
grep -E '^(CONTINUOUS_PARQUET_MODE|CONTINUOUS_FINE_ROW_GC_MODE|CONTINUOUS_HOT_METADATA_RETENTION_MINUTES)=' .env

echo "==> [2/3] 重建 apiserver 容器应用新配置（不重建镜像）"
docker compose up -d --no-deps --force-recreate apiserver

echo "==> [3/3] 等待 apiserver 健康检查"
HEALTHY=0
for _ in $(seq 1 20); do
  if docker compose ps apiserver --format '{{.Status}}' 2>/dev/null | grep -q 'healthy'; then
    HEALTHY=1
    break
  fi
  sleep 5
done
if [ "${HEALTHY}" != "1" ]; then
  echo "❌ apiserver 健康检查失败，恢复 .env 并回滚容器" >&2
  cp "${ENV_BAK}" .env
  docker compose up -d --no-deps --force-recreate apiserver
  exit 1
fi

echo "✅ 6E 正式切换完成：parquet=enforce、fine_gc=enforce、hot_retention=120。"
echo "   备份 .env: ${ENV_BAK}；历史 schedule 保持清空。"
echo "   请再观察至少 3 小时，确认两小时前的 ProfileBatch/ProfileWindow 被清理。"
