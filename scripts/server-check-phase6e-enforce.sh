#!/bin/bash
# ==============================================================================
# server-check-phase6e-enforce.sh — Release 6E：enforce 加速验收门禁（服务器执行）
# ==============================================================================
# 用法：bash scripts/server-check-phase6e-enforce.sh <SID>
#   <SID>：本次加速验收 Continuous Session 的 sid（必填）。
# 门禁条件（全部满足才可进入加速 GC / 最终 enforce）：
#   - parquet_mode = enforce
#   - 本次 Session 的 continuous_coverage_segments 中 cpu_profile 实际区间
#     总和 >= 20 分钟，且 sample_count > 0（真实数据已封存进 Parquet）
#   - 无 building/validating backlog
#   - active Block 全部 validation=passed + reconcile=passed
#   - 无近期 missing partition
#   - 无 retrying migration failure、无 unexpected quarantined
#   - 无 orphan window、无 fine_gc receipt 阻塞
#   - Parquet 查询错误 = 0
#   - 根盘可用 >= 8GiB
# Parquet Block 小时边界跨度（observed_span_minutes）仅作诊断字段，不作为通过条件。
# ==============================================================================
set -o pipefail

SID="${1:-}"
if [ -z "${SID}" ]; then
  echo "❌ 需要传入 Continuous Session SID 作为参数" >&2
  echo "用法: bash scripts/server-check-phase6e-enforce.sh <SID>" >&2
  exit 2
fi

PG() { docker exec mini-drop-postgres-1 psql -v ON_ERROR_STOP=1 -U postgres -d drop -tAc "$1"; }
TS=$(date +%Y%m%d-%H%M%S)
LOG=/home/ubuntu/mini-drop/backups/phase6/observe-6e-$TS.log
mkdir -p "$(dirname "$LOG")"
PASS=1

STATUS=$(curl -fsS -H "Drop-User-Uid: admin" http://127.0.0.1:8191/api/v1/storage/status 2>/dev/null || true)
MODE=$(printf '%s' "$STATUS" | python3 -c 'import sys,json; print(json.load(sys.stdin)["data"]["parquet_mode"])' 2>/dev/null || echo unknown)
PQ_QUERY_ERR=$(printf '%s' "$STATUS" | python3 -c 'import sys,json; print(json.load(sys.stdin)["data"].get("parquet_query_errors_total",-1))' 2>/dev/null || echo -1)
RECEIPT_CNT=$(printf '%s' "$STATUS" | python3 -c 'import sys,json; print(json.load(sys.stdin)["data"].get("migration_receipt_count",-1))' 2>/dev/null || echo -1)
RECEIPT_ELIG=$(printf '%s' "$STATUS" | python3 -c 'import sys,json; print(json.load(sys.stdin)["data"].get("migration_receipt_gc_eligible",-1))' 2>/dev/null || echo -1)

BACKLOG=$(PG "SELECT count(*) FROM continuous_parquet_blocks WHERE status IN ('building','validating')")
FAILED=$(PG "SELECT count(*) FROM continuous_parquet_blocks WHERE status='active' AND reconcile_status IN ('failed','quarantined')")
RETRY=$(PG "SELECT count(*) FROM continuous_migration_failures WHERE status='retrying'")
BAD_QUARANTINE=$(PG "SELECT count(*) FROM continuous_migration_failures WHERE status='quarantined' AND error_type NOT IN ('missing_object','orphan_window')")
ORPHAN=$(PG "SELECT count(*) FROM profile_windows w LEFT JOIN profile_batches b ON b.bid=w.batch_bid WHERE b.bid IS NULL")
MISSING_PARTITIONS=$(PG "WITH src AS (
  SELECT DISTINCT date_trunc('hour', window_start) AS hour,
    CASE
      WHEN signal_type IN ('cpu_profile','cpu','python_memory','memory') THEN 'cpu'
      WHEN signal_type IN ('metrics','python_rss') THEN 'metrics'
      WHEN signal_type IN ('io_latency','io_syscall_latency','sched_latency') THEN 'histogram'
      WHEN signal_type IN ('db','db_snapshot') THEN 'db'
    END AS signal
  FROM profile_windows
  WHERE window_start >= now() - interval '24 hours'
    AND window_start < date_trunc('hour', now())
), covered AS (
  SELECT bucket_start AS hour, signal_type AS signal
  FROM continuous_parquet_blocks
  WHERE status='active' AND validation='passed' AND reconcile_status='passed'
)
SELECT count(*) FROM src LEFT JOIN covered USING(hour,signal)
WHERE src.signal IS NOT NULL AND covered.hour IS NULL")
GC_BLOCKERS=$(PG "WITH missing_interval AS (
  SELECT DISTINCT w.batch_bid AS bid
  FROM profile_windows w
  JOIN profile_batches b ON b.bid=w.batch_bid
  WHERE b.created_at < now() - interval '120 minutes'
    AND NOT EXISTS (
      SELECT 1 FROM continuous_migration_receipts r
      WHERE r.source_kind='batch' AND r.source_ref=w.batch_bid AND r.status='passed'
        AND r.signal_type = CASE
          WHEN w.signal_type IN ('cpu_profile','cpu','python_memory','memory') THEN 'cpu'
          WHEN w.signal_type IN ('metrics','python_rss') THEN 'metrics'
          WHEN w.signal_type IN ('io_latency','io_syscall_latency','sched_latency') THEN 'histogram'
          WHEN w.signal_type IN ('db','db_snapshot') THEN 'db' END
        AND r.start_time <= w.window_start AND r.end_time >= w.window_end
    )
), unevidenced_empty AS (
  SELECT b.bid FROM profile_batches b
  WHERE b.created_at < now() - interval '120 minutes'
    AND NOT EXISTS (SELECT 1 FROM profile_windows w WHERE w.batch_bid=b.bid)
    AND NOT EXISTS (SELECT 1 FROM continuous_migration_receipts r WHERE r.source_kind='batch' AND r.source_ref=b.bid AND r.status='passed')
    AND NOT EXISTS (SELECT 1 FROM continuous_migration_failures f WHERE f.source_kind='window' AND f.object_key=b.object_key AND f.status='quarantined')
)
SELECT (SELECT count(*) FROM missing_interval) + (SELECT count(*) FROM unevidenced_empty)")
# 本次 Session 实际 CPU 覆盖：continuous_coverage_segments 的 cpu_profile 区间总和
COV_MIN=$(PG "SELECT COALESCE(EXTRACT(EPOCH FROM SUM(segment_end-segment_start))/60,0)::int FROM continuous_coverage_segments WHERE session_sid='${SID}' AND signal_type='cpu_profile'")
COV_SAMPLES=$(PG "SELECT COALESCE(SUM(sample_count),0) FROM continuous_coverage_segments WHERE session_sid='${SID}' AND signal_type='cpu_profile'")
# 诊断字段：active raw Parquet Block 小时边界跨度（不再作为通过条件）
SPAN_MINUTES=$(PG "SELECT COALESCE(EXTRACT(EPOCH FROM (max(bucket_end)-min(bucket_start)))/60,0)::bigint FROM continuous_parquet_blocks WHERE status='active' AND validation='passed' AND reconcile_status='passed'")
DISK_FREE_GB=$(df -B1 --output=avail / | tail -1 | tr -d ' ')

{
  echo "=== 6E enforce 加速验收门禁 $TS SID=$SID ==="
  echo "mode: $MODE"
  echo "session_cpu_coverage_minutes: $COV_MIN"
  echo "session_cpu_coverage_samples: $COV_SAMPLES"
  echo "disk_free_gb: $DISK_FREE_GB"
  echo "validation_backlog: $BACKLOG"
  echo "active_reconcile_failed_or_quarantined: $FAILED"
  echo "missing_recent_partitions: $MISSING_PARTITIONS"
  echo "migration_retrying: $RETRY"
  echo "unexpected_quarantined: $BAD_QUARANTINE"
  echo "orphan_windows: $ORPHAN"
  echo "fine_gc_receipt_blockers: $GC_BLOCKERS"
  echo "parquet_query_errors: $PQ_QUERY_ERR"
  echo "migration_receipt_count: $RECEIPT_CNT"
  echo "migration_receipt_gc_eligible: $RECEIPT_ELIG"
  echo "observed_span_minutes(diagnostic): $SPAN_MINUTES"
} | tee "$LOG"

check_zero() {
  value=$1
  name=$2
  [ "$value" = "0" ] || { echo "FAIL: $name=$value" | tee -a "$LOG"; PASS=0; }
}

[ "$MODE" = "enforce" ] || { echo "FAIL: parquet_mode=$MODE (期望 enforce)" | tee -a "$LOG"; PASS=0; }
# 本次 Session 实际 CPU 覆盖 >= 20 分钟
[ "$COV_MIN" -ge 20 ] 2>/dev/null || { echo "FAIL: session_cpu_coverage_minutes=$COV_MIN (<20，需先产生真实 Continuous 数据)" | tee -a "$LOG"; PASS=0; }
# 覆盖区间必须含真实样本
[ "$COV_SAMPLES" -gt 0 ] 2>/dev/null || { echo "FAIL: session_cpu_coverage_samples=$COV_SAMPLES (=0，无真实样本)" | tee -a "$LOG"; PASS=0; }
check_zero "$BACKLOG" validation_backlog
check_zero "$FAILED" active_reconcile_failed_or_quarantined
check_zero "$MISSING_PARTITIONS" missing_recent_partitions
check_zero "$RETRY" migration_retrying
check_zero "$BAD_QUARANTINE" unexpected_quarantined
check_zero "$ORPHAN" orphan_windows
check_zero "$GC_BLOCKERS" fine_gc_receipt_blockers
check_zero "$PQ_QUERY_ERR" parquet_query_errors
# 根盘 >= 8GiB（8589934592 字节）
[ "$DISK_FREE_GB" -ge 8589934592 ] 2>/dev/null || { echo "FAIL: disk_free=$DISK_FREE_GB (<8GiB)" | tee -a "$LOG"; PASS=0; }

if [ "$PASS" = "1" ]; then
  echo "RESULT: PASS" | tee -a "$LOG"
  exit 0
fi
echo "RESULT: FAIL" | tee -a "$LOG"
exit 1
