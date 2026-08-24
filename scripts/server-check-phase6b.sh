#!/bin/bash
# Release 6B shadow 观察门禁。任何检查失败均返回非零，供定时任务可靠阻断 6C。
set -o pipefail

PG() { docker exec mini-drop-postgres-1 psql -v ON_ERROR_STOP=1 -U postgres -d drop -tAc "$1"; }
TS=$(date +%Y%m%d-%H%M%S)
LOG=/home/ubuntu/mini-drop/backups/phase6/observe-6b-$TS.log
mkdir -p "$(dirname "$LOG")"
PASS=1

STATUS=$(curl -fsS -H "Drop-User-Uid: admin" http://127.0.0.1:8191/api/v1/storage/status 2>/dev/null || true)
MODE=$(printf '%s' "$STATUS" | python3 -c 'import sys,json; print(json.load(sys.stdin)["data"]["parquet_mode"])' 2>/dev/null || echo unknown)
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
SPAN_MINUTES=$(PG "SELECT COALESCE(EXTRACT(EPOCH FROM (max(bucket_end)-min(bucket_start)))/60,0)::bigint FROM continuous_parquet_blocks WHERE status='active' AND validation='passed' AND reconcile_status='passed'")

{
  echo "=== 6B 观察门禁 $TS ==="
  echo "mode: $MODE"
  echo "disk_free: $(df -h / | tail -1 | awk '{print $4}')"
  echo "validation_backlog: $BACKLOG"
  echo "active_reconcile_failed_or_quarantined: $FAILED"
  echo "missing_recent_partitions: $MISSING_PARTITIONS"
  echo "migration_retrying: $RETRY"
  echo "unexpected_quarantined: $BAD_QUARANTINE"
  echo "orphan_windows: $ORPHAN"
  echo "fine_gc_receipt_blockers: $GC_BLOCKERS"
  echo "observed_span_minutes: $SPAN_MINUTES"
} | tee "$LOG"

check_zero() {
  value=$1
  name=$2
  [ "$value" = "0" ] || { echo "FAIL: $name=$value" | tee -a "$LOG"; PASS=0; }
}

[ "$MODE" = "shadow" ] || { echo "FAIL: parquet_mode=$MODE (期望 shadow)" | tee -a "$LOG"; PASS=0; }
check_zero "$BACKLOG" validation_backlog
check_zero "$FAILED" active_reconcile_failed_or_quarantined
check_zero "$MISSING_PARTITIONS" missing_recent_partitions
check_zero "$RETRY" migration_retrying
check_zero "$BAD_QUARANTINE" unexpected_quarantined
check_zero "$ORPHAN" orphan_windows
check_zero "$GC_BLOCKERS" fine_gc_receipt_blockers
[ "$SPAN_MINUTES" -ge 20 ] 2>/dev/null || { echo "FAIL: observed_span_minutes=$SPAN_MINUTES (<20)" | tee -a "$LOG"; PASS=0; }

if [ "$PASS" = "1" ]; then
  echo "RESULT: PASS" | tee -a "$LOG"
  exit 0
fi
echo "RESULT: FAIL" | tee -a "$LOG"
exit 1
