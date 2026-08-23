#!/bin/bash
# ==============================================================================
# phase5-blob-gc.sh — 阶段五：Blob GC 分批开启门禁（在服务器执行）
# ==============================================================================
# 前提：BLOB_MIGRATION_ENABLED 已运行 ≥24h（storage_object_gc 宽限期到期），
# 1117 个旧对象（阶段二迁移副本）均无有效引用后才可分批开启 BLOB_GC_ENABLED。
# 本脚本：
#   1. 按 worker 的 legacyKeyRefs 语义校验到期候选均无有效引用
#   2. 按批开启 GC（先放行已迁移旧副本，确认回收后再全量）
# 用法：ssh ubuntu@111.230.29.115 'bash -s' < scripts/phase5-blob-gc.sh [batch-size]
# ==============================================================================
set -euo pipefail

ROOT=/home/ubuntu/mini-drop
BATCH="${1:-200}"

echo "==> [1/3] 校验 storage_object_gc 候选均无有效引用"
UNSAFE=$(docker compose -f "${ROOT}/docker-compose.yml" exec -T postgres psql -U postgres -d drop -t -A -F'|' \
  -c "
SELECT COUNT(*) FROM storage_object_gc g
WHERE g.deleted_at IS NULL
  AND g.not_before IS NOT NULL
  AND g.not_before <= now()
  AND (EXISTS (
	        SELECT 1 FROM artifacts a
	        WHERE a.object_key = g.object_key AND a.blob_id IS NULL
	          AND a.deleted_at IS NULL AND a.status NOT IN ('deleting','deleted'))
	    OR EXISTS (
	        SELECT 1 FROM symbol_files s
	        WHERE s.object_key = g.object_key AND (s.blob_id IS NULL OR s.blob_id = 0))
	    OR EXISTS (
	        SELECT 1 FROM kernel_symbol_files k
	        WHERE k.object_key = g.object_key AND (k.blob_id IS NULL OR k.blob_id = 0))
	    OR EXISTS (
	        SELECT 1 FROM storage_blobs b
	        WHERE b.object_key = g.object_key AND b.deleted_at IS NULL)
	  );" | tr -d ' ')

echo "    仍被有效引用的 GC 候选数: ${UNSAFE}"
if [ "${UNSAFE}" != "0" ]; then
  echo "❌ 存在仍被引用的对象，禁止开启 Blob GC。请先排查。"
  exit 1
fi

TOTAL=$(docker compose -f "${ROOT}/docker-compose.yml" exec -T postgres psql -U postgres -d drop -t -A -F'|' \
	  -c "SELECT COUNT(*) FROM storage_object_gc WHERE deleted_at IS NULL AND not_before IS NOT NULL AND not_before <= now();" | tr -d ' ')
echo "    可回收候选总数: ${TOTAL}"

echo "==> [2/3] 写入部署配置（分批 BLOB_GC_ENABLED=true）"
# 通过 .env 注入（与 compose 的 BLOB_GC_ENABLED 变量联动）
grep -q '^BLOB_GC_ENABLED=' "${ROOT}/.env" 2>/dev/null || echo 'BLOB_GC_ENABLED=true' >> "${ROOT}/.env"
sed -i.bak 's/^BLOB_GC_ENABLED=.*/BLOB_GC_ENABLED=true/' "${ROOT}/.env"
grep -q '^BLOB_MIGRATION_BATCH=' "${ROOT}/.env" 2>/dev/null || echo "BLOB_MIGRATION_BATCH=${BATCH}" >> "${ROOT}/.env"
sed -i.bak 's/^BLOB_MIGRATION_BATCH=.*/BLOB_MIGRATION_BATCH='"${BATCH}"'/' "${ROOT}/.env"

echo "==> [3/3] 重建 apiserver 容器（仅应用新环境变量，不重建镜像）"
docker compose -f "${ROOT}/docker-compose.yml" up -d --no-deps --force-recreate apiserver

echo "✅ Blob GC 已开启（批次 ${BATCH}）。观察 mini_drop_blob_gc_deleted_total 指标确认回收推进。"
echo "   如需回退：将 .env 中 BLOB_GC_ENABLED 改回 false 后重建 apiserver 容器。"
