#!/bin/bash
# ==============================================================================
# phase5-prep.sh — 阶段五 Release A 前置：备份与基线记录（在服务器执行）
# ==============================================================================
# 在首次构建前：
#   1. 备份 PostgreSQL 到 /home/ubuntu/mini-drop/backups/phase5/
#   2. 记录 MinIO 前缀大小/对象数、数据库表大小、镜像 ID
# 用法：ssh ubuntu@111.230.29.115 'bash -s' < scripts/phase5-prep.sh
# ==============================================================================
set -euo pipefail

ROOT=/home/ubuntu/mini-drop
BACKUP_DIR="${ROOT}/backups/phase5"
STAMP="$(date +%Y%m%d-%H%M%S)"
BASELINE="${BACKUP_DIR}/baseline-${STAMP}.json"
mkdir -p "${BACKUP_DIR}"

echo "==> [1/4] 备份 PostgreSQL"
docker compose -f "${ROOT}/docker-compose.yml" exec -T postgres pg_dump -U postgres -d drop \
  -F custom -f /tmp/phase5-${STAMP}.dump
docker compose -f "${ROOT}/docker-compose.yml" cp postgres:/tmp/phase5-${STAMP}.dump "${BACKUP_DIR}/postgres-${STAMP}.dump"
docker compose -f "${ROOT}/docker-compose.yml" exec -T postgres rm -f /tmp/phase5-${STAMP}.dump
echo "    备份完成: ${BACKUP_DIR}/postgres-${STAMP}.dump"

echo "==> [2/4] 记录 MinIO 前缀统计"
MC_CONFIG_DIR="$(mktemp -d)"
cleanup_mc_config() { rm -rf "${MC_CONFIG_DIR}"; }
trap cleanup_mc_config EXIT
# 所有 mc 调用挂载同一配置目录；否则 alias set 所在的临时容器退出后配置丢失。
mc_cmd() { docker run --rm --network mini-drop_default -v "${MC_CONFIG_DIR}:/root/.mc" minio/mc:latest "$@"; }
# 使用宿主机直连（容器网络名可能不同，退化用 minio 服务名）
if ! mc_cmd alias set myminio http://127.0.0.1:9000 drop dropdrop >/dev/null 2>&1; then
  echo "    mc 直连失败，尝试容器网络"
  mc_cmd alias set myminio http://minio:9000 drop dropdrop >/dev/null 2>&1 || true
fi

list_prefix() {
  local prefix="$1"
  mc_cmd du --recursive myminio/drop-data/${prefix} 2>/dev/null | awk '{print $1}' | head -1
}

MINIO_JSON=$(cat <<EOF
{
  "minio_prefixes": {
    "continuous/": "$(list_prefix continuous/)",
    "continuous-blocks/": "$(list_prefix continuous-blocks/)",
    "continuous/v2/": "$(list_prefix continuous/v2/)",
    "blobs/": "$(list_prefix blobs/)",
    "tasks/": "$(list_prefix tasks/)"
  }
}
EOF
)

echo "==> [3/4] 记录数据库表大小"
DB_TABLES=$(docker compose -f "${ROOT}/docker-compose.yml" exec -T postgres psql -U postgres -d drop -t -A -F'|' \
  -c "SELECT tablename, pg_total_relation_size(quote_ident(tablename)) FROM pg_tables WHERE schemaname='public' ORDER BY 2 DESC LIMIT 20;" || true)

echo "==> [4/4] 记录镜像 ID"
IMAGES=$(docker compose -f "${ROOT}/docker-compose.yml" images --format json 2>/dev/null || true)
IMAGES_JSON=$(echo "${IMAGES}" | python3 -c '
import json,sys
items=[]
for line in sys.stdin:
    line=line.strip()
    if not line:
        continue
    value=json.loads(line)
    items.extend(value if isinstance(value,list) else [value])
print(json.dumps(items))
' 2>/dev/null || echo '[]')

cat > "${BASELINE}" <<EOF
{
  "stamp": "${STAMP}",
  "created_at": "$(date -Iseconds)",
  "postgres_backup": "${BACKUP_DIR}/postgres-${STAMP}.dump",
  "minio": $(echo "${MINIO_JSON}" | python3 -c "import sys,json;print(json.dumps(json.load(sys.stdin)['minio_prefixes']))"),
  "db_tables_bytes": $(echo "${DB_TABLES}" | python3 -c "
import sys,json
rows=[]
for line in sys.stdin:
    line=line.strip()
    if not line or '|' not in line: continue
    t,s=line.split('|')[:2]
    rows.append({'table':t,'bytes':int(s)})
print(json.dumps(rows))" 2>/dev/null || echo '[]'),
  "images": ${IMAGES_JSON}
}
EOF
echo "==> 基线已记录: ${BASELINE}"
echo "==> 完成。请记录以下基线摘要："
python3 -c "
import json
d=json.load(open('${BASELINE}'))
print('  postgres_backup:', d['postgres_backup'])
for k,v in d['minio'].items():
    print(f'  minio {k}: {v} bytes')
for row in d.get('db_tables_bytes', [])[:5]:
    print(f\"  db {row['table']}: {row['bytes']} bytes\")
"
