#!/bin/bash
# ==============================================================================
# server-storage-reset-v2.sh — 两日存储模型干净切换：服务器专用重置脚本
# ==============================================================================
# 只允许两种模式：
#   bash scripts/server-storage-reset-v2.sh report
#   bash scripts/server-storage-reset-v2.sh execute RESET-PERFORMANCE-DATA
#
# report  ：只读输出磁盘、业务表数量、MinIO 对象数量、控制面数量，不修改任何状态。
# execute ：按固定顺序执行 10 步（建备份目录→停写入服务→pg_dump+pg_restore 验证→
#           MinIO 对象清单→保存 .env/Compose/控制面行数→导出调度定义→单事务
#           清空业务表→清空 Bucket 内容→清空 spool/Go 符号缓存→验证），
#           任一步失败立即停止，不自动继续启动写入服务。
#
# 保留（控制面，绝不触碰）：
#   user_infos / groups / group_members / agent_infos / agent_audit_logs
#   / schema_migrations
# 清空（业务表，显式清单 TRUNCATE ... RESTART IDENTITY，不用无边界 CASCADE）：
#   见 BUSINESS_TABLES（agent/process/session、single-shot、symbols/blobs、continuous）。
# 不删除数据库、不删除任何 named volume、不删除 Bucket 本身及其访问策略。
#
# 安全约束：
#   - 固定项目根目录 /home/ubuntu/mini-drop，校验 Compose project（mini-drop）。
#   - 拒绝 unresolved 路径、通配递归删除和空变量（spool/symbol 卷名/Bucket 名
#     均显式断言后操作）。
#   - 备份先于任何破坏性操作；验证失败即中止。
# ==============================================================================
set -euo pipefail

ROOT=/home/ubuntu/mini-drop
COMPOSE="${ROOT}/docker-compose.yml"
BACKUP_BASE="${ROOT}/backups/storage-reset"
PROJECT="mini-drop"
SPOOL_DIR=/var/lib/mini-drop/continuous-spool
SYMBOL_VOL="${PROJECT}_gosymbolcache"
BUCKET="drop-data"
STAMP="$(date +%Y%m%d-%H%M%S)"
MODE="${1:-}"

say() { printf '[reset] %s\n' "$*"; }
die() { printf '[reset] ❌ %s\n' "$*" >&2; exit 1; }

# --- 固定表清单 ------------------------------------------------------------
# 保留的控制面表（绝不触碰）。
CONTROL_TABLES=(
  user_infos
  groups
  group_members
  agent_infos
  agent_audit_logs
  schema_migrations
)

# 清空的业务表（显式清单；TRUNCATE 不跨清单外的表，依赖 FK 全在清单内）。
BUSINESS_TABLES=(
  # agent/process/session
  continuous_agent_states
  continuous_process_snapshots
  continuous_sessions
  # single-shot
  hotmethod_tasks
  multi_tasks
  task_attempts
  task_status_events
  artifacts
  analysis_jobs
  analysis_suggestions
  task_build_ids
  outboxes
  schedule_triggers
  schedule_tasks
  # symbols/blobs
  symbol_files
  kernel_symbol_files
  storage_blobs
  storage_object_gc
  storage_migration_failures
  # continuous
  profile_windows
  profile_batches
  continuous_window_summaries
  continuous_profile_blocks
  continuous_parquet_block_files
  continuous_parquet_block_members
  continuous_parquet_blocks
  continuous_coverage_segments
  continuous_migration_receipts
  continuous_migration_failures
)

# --- 辅助函数 ---------------------------------------------------------------
# psql：在 postgres 容器内执行单条 SQL，输出纯文本（tuples-only / unaligned）。
psql() { docker compose -f "${COMPOSE}" exec -T postgres psql -v ON_ERROR_STOP=1 -U postgres -d drop -t -A "$@"; }

# setup_mc：准备 mc 工具（隔离容器 + 统一配置目录），定义 mc_cmd 函数。
MC_CONFIG_DIR=""
setup_mc() {
  MC_CONFIG_DIR="$(mktemp -d)"
  trap 'rm -rf "${MC_CONFIG_DIR}"' EXIT
  mc_cmd() { docker run --rm --network "${PROJECT}_default" -v "${MC_CONFIG_DIR}:/root/.mc" minio/mc:latest "$@"; }
  if ! mc_cmd alias set myminio "http://127.0.0.1:9000" drop dropdrop >/dev/null 2>&1; then
    mc_cmd alias set myminio "http://minio:9000" drop dropdrop >/dev/null 2>&1 \
      || die "无法连接 MinIO（127.0.0.1:9000 与 minio:9000 均失败）"
  fi
}

# verify_environment：固定根目录、Compose 文件、project 网络、compose config 校验。
verify_environment() {
  [ -d "${ROOT}" ] || die "项目根目录不存在: ${ROOT}"
  [ -f "${COMPOSE}" ] || die "docker-compose.yml 不存在: ${COMPOSE}"
  (cd "${ROOT}" && docker compose config >/dev/null) || die "docker compose config 校验失败"
  docker network ls --format '{{.Name}}' | grep -qx "${PROJECT}_default" \
    || die "Compose project 网络 ${PROJECT}_default 不存在（服务未创建？）"
}

# table_count <table>：返回该表行数。
table_count() {
  psql -c "SELECT count(*) FROM $1;" | tr -d ' '
}

# ---------------------------------------------------------------------------
# report：只读报告
# ---------------------------------------------------------------------------
report() {
  verify_environment
  say "===== 只读报告（不修改任何状态）====="
  echo "--- 磁盘 ---"
  df -h /
  echo "--- 根盘可用字节 ---"
  df -B1 --output=avail / | tail -1 | tr -d ' '

  echo "--- 业务表行数 ---"
  for t in "${BUSINESS_TABLES[@]}"; do
    printf '  %-44s %s\n' "${t}" "$(table_count "${t}")"
  done

  echo "--- 控制面行数（保留清单）---"
  for t in "${CONTROL_TABLES[@]}"; do
    printf '  %-44s %s\n' "${t}" "$(table_count "${t}")"
  done

  echo "--- MinIO ${BUCKET} 对象 ---"
  setup_mc
  mc_cmd du --recursive "myminio/${BUCKET}/" 2>/dev/null \
    || echo "  (mc du 不可用，仅统计对象数)"
  OBJ_COUNT="$(mc_cmd ls --recursive "myminio/${BUCKET}/" 2>/dev/null | wc -l | tr -d ' ')"
  echo "  对象数: ${OBJ_COUNT}"

  say "报告完成（未做任何修改）。确认根盘可用空间≥5GiB 后再执行 execute。"
}

# ---------------------------------------------------------------------------
# execute RESET-PERFORMANCE-DATA：10 步干净重置
# ---------------------------------------------------------------------------
execute() {
  [ "${1:-}" = "RESET-PERFORMANCE-DATA" ] \
    || die "execute 需要精确参数 RESET-PERFORMANCE-DATA（当前: '${1:-}'）"
  verify_environment

  BACKUP_DIR="${BACKUP_BASE}/${STAMP}"
  mkdir -p "${BACKUP_DIR}"

  say "==> [1/10] 创建时间戳备份目录: ${BACKUP_DIR}"

  say "==> [2/10] 停止写入服务（drop_agent analysis apiserver drop_server pprof_demo）"
  docker compose -f "${COMPOSE}" ps --format json > "${BACKUP_DIR}/compose-ps-before.json" 2>/dev/null || true
  docker compose -f "${COMPOSE}" stop drop_agent analysis apiserver drop_server pprof_demo

  say "==> [3/10] PostgreSQL dump (-Fc) + pg_restore --list 验证"
  docker compose -f "${COMPOSE}" exec -T postgres pg_dump -U postgres -d drop -F c \
    -f "/tmp/storage-reset-${STAMP}.dump"
  docker compose -f "${COMPOSE}" cp "postgres:/tmp/storage-reset-${STAMP}.dump" \
    "${BACKUP_DIR}/postgres-${STAMP}.dump"
  docker compose -f "${COMPOSE}" exec -T postgres pg_restore --list \
    "/tmp/storage-reset-${STAMP}.dump" > "${BACKUP_DIR}/dump-list.txt"
  grep -q 'TABLE DATA' "${BACKUP_DIR}/dump-list.txt" \
    || die "pg_restore --list 验证失败（dump 未见 TABLE DATA 条目）"
  say "    dump 已验证: ${BACKUP_DIR}/postgres-${STAMP}.dump"

  say "==> [4/10] MinIO 对象清单（key/size/mtime）"
  setup_mc
  mc_cmd ls --recursive --json "myminio/${BUCKET}/" > "${BACKUP_DIR}/minio-manifest.jsonl" 2>/dev/null || true
  MANIFEST_LINES="$(wc -l < "${BACKUP_DIR}/minio-manifest.jsonl" | tr -d ' ')"
  say "    对象条目: ${MANIFEST_LINES}（${BACKUP_DIR}/minio-manifest.jsonl）"

  say "==> [5/10] 保存 .env、Compose 配置、控制面行数"
  if [ -f "${ROOT}/.env" ]; then
    cp "${ROOT}/.env" "${BACKUP_DIR}/env-backup"
  else
    say "    警告: ${ROOT}/.env 不存在，跳过"
  fi
  (cd "${ROOT}" && docker compose config > "${BACKUP_DIR}/compose-config.yaml")
  : > "${BACKUP_DIR}/control-plane-counts.txt"
  for t in "${CONTROL_TABLES[@]}"; do
    printf '%s %s\n' "${t}" "$(table_count "${t}")" >> "${BACKUP_DIR}/control-plane-counts.txt"
  done
  say "    控制面行数已保存: ${BACKUP_DIR}/control-plane-counts.txt"

  say "==> [6/10] 导出 schedule 定义留档（随后将永久清空）"
  psql -c "COPY (SELECT * FROM schedule_tasks ORDER BY id) TO STDOUT WITH CSV HEADER;" \
    > "${BACKUP_DIR}/schedule-tasks.csv"
  say "    schedule-tasks.csv 已保存；仅作留档，后续脚本不会自动恢复"

  say "==> [7/10] 单事务清空业务表（TRUNCATE ... RESTART IDENTITY）"
  TABLES_CSV="$(IFS=,; echo "${BUSINESS_TABLES[*]}")"
  [ -n "${TABLES_CSV}" ] || die "业务表清单为空，拒绝执行"
  psql -c "BEGIN; TRUNCATE ${TABLES_CSV} RESTART IDENTITY; COMMIT;"
  say "    已清空 ${#BUSINESS_TABLES[@]} 张业务表（单事务）"

  say "==> [8/10] 清空 ${BUCKET} Bucket 内容（保留 Bucket 与访问策略）"
  [ "${BUCKET}" = "drop-data" ] || die "Bucket 名断言失败: ${BUCKET}"
  mc_cmd rm --recursive --force "myminio/${BUCKET}/"

  say "==> [9/10] 清空 Agent continuous spool 与 Go symbol cache"
  [ "${SPOOL_DIR}" = "/var/lib/mini-drop/continuous-spool" ] || die "spool 路径断言失败: ${SPOOL_DIR}"
  if [ -d "${SPOOL_DIR}" ]; then
    find "${SPOOL_DIR}" -mindepth 1 -delete
    say "    spool 已清空: ${SPOOL_DIR}"
  else
    say "    spool 目录不存在，跳过: ${SPOOL_DIR}"
  fi
  [ "${SYMBOL_VOL}" = "mini-drop_gosymbolcache" ] || die "symbol 卷名断言失败: ${SYMBOL_VOL}"
  docker run --rm -v "${SYMBOL_VOL}:/cache:rw" alpine sh -c 'find /cache -mindepth 1 -delete' \
    || die "Go symbol cache 清空失败（卷保持）"
  say "    Go symbol cache 已清空（卷 ${SYMBOL_VOL} 保留）"

  say "==> [10/10] 验证"
  : > "${BACKUP_DIR}/control-plane-counts-after.txt"
  for t in "${CONTROL_TABLES[@]}"; do
    printf '%s %s\n' "${t}" "$(table_count "${t}")" >> "${BACKUP_DIR}/control-plane-counts-after.txt"
  done
  diff "${BACKUP_DIR}/control-plane-counts.txt" "${BACKUP_DIR}/control-plane-counts-after.txt" \
    > /dev/null || die "控制面行数变化！重置中止。"
  say "    控制面行数未变化 ✅"

  for t in "${BUSINESS_TABLES[@]}"; do
    c="$(table_count "${t}")"
    [ "${c}" = "0" ] || die "业务表 ${t} 行数不为零: ${c}"
  done
  say "    全部业务表为零 ✅"

  OBJ_AFTER="$(mc_cmd ls --recursive "myminio/${BUCKET}/" 2>/dev/null | wc -l | tr -d ' ')"
  [ "${OBJ_AFTER}" = "0" ] || die "Bucket 仍有 ${OBJ_AFTER} 个对象"
  say "    Bucket 为空 ✅"

  say "✅ 重置完成。写入服务保持停止；请按计划写入新 .env 后按顺序启动服务。"
  say "   备份目录: ${BACKUP_DIR}"
}

# ---------------------------------------------------------------------------
case "${MODE}" in
  report)
    report
    ;;
  execute)
    execute "${2:-}"
    ;;
  *)
    echo "用法: bash $0 report | bash $0 execute RESET-PERFORMANCE-DATA" >&2
    exit 2
    ;;
esac
