#!/bin/bash
# ==============================================================================
# phase6e-acceptance-report.sh — 加速验收证据留存（服务器执行）
# ==============================================================================
# 记录第 4 节要求的所有验收证据：Continuous SID 的覆盖/块状态/交叉追溯计数、
# storage/status 关键字段、单次采样 TID 的任务/Artifact/分析状态、磁盘等。
# 用法：bash scripts/phase6e-acceptance-report.sh <SID> [TID] [输出目录]
#   输出：默认 /home/ubuntu/mini-drop/backups/phase6/acceptance/acceptance-<ts>.txt
# ==============================================================================
set -u

SID="${1:-}"
TID="${2:-}"
if [ -z "${SID}" ]; then
  echo "❌ 需要 Continuous Session SID" >&2
  echo "用法: bash scripts/phase6e-acceptance-report.sh <SID> [TID]" >&2
  exit 2
fi
OUT_DIR="${3:-/home/ubuntu/mini-drop/backups/phase6/acceptance}"
mkdir -p "${OUT_DIR}"
TS=$(date +%Y%m%d-%H%M%S)
REPORT="${OUT_DIR}/acceptance-${TS}.txt"

PG() { docker exec mini-drop-postgres-1 psql -U postgres -d drop -t -A -F'|' -c "$1" 2>/dev/null | tr -d ' '; }

{
  echo "=== 加速验收证据报告 ${TS} ==="
  echo "SID=${SID} TID=${TID:-}"
  echo
  echo "--- [1] Continuous Session ---"
  PG "SELECT 'sid='||sid||' name='||name||' status='||status||' desired='||desired_state||' observed='||observed_state||' allow_degraded='||allow_degraded||' rate_hz='||sample_rate_hz||' agg_sec='||aggregation_window_sec||' upload_sec='||upload_batch_sec||' retention_h='||retention_hours||' started='||started_at||' stopped='||COALESCE(stopped_at::text,'NULL') FROM continuous_sessions WHERE sid='${SID}'"
  echo
  echo "--- [2] CPU 实际覆盖（continuous_coverage_segments, cpu_profile）---"
  echo "  coverage_minutes=$(PG "SELECT COALESCE(EXTRACT(EPOCH FROM SUM(segment_end-segment_start))/60,0)::int FROM continuous_coverage_segments WHERE session_sid='${SID}' AND signal_type='cpu_profile'")"
  echo "  coverage_samples=$(PG "SELECT COALESCE(SUM(sample_count),0) FROM continuous_coverage_segments WHERE session_sid='${SID}' AND signal_type='cpu_profile'")"
  echo
  echo "--- [3] 本次 Session 涉及的 active raw Block（经 block_members 追溯）---"
  echo "  passed_blocks=$(PG "SELECT count(*) FROM continuous_parquet_blocks b WHERE b.status='active' AND b.validation='passed' AND b.reconcile_status='passed' AND EXISTS (SELECT 1 FROM continuous_parquet_block_members m WHERE m.block_id=b.block_id AND m.session_sid='${SID}')")"
  echo "  failed_or_pending_blocks=$(PG "SELECT count(*) FROM continuous_parquet_blocks b WHERE b.status='active' AND (b.validation<>'passed' OR b.reconcile_status<>'passed') AND EXISTS (SELECT 1 FROM continuous_parquet_block_members m WHERE m.block_id=b.block_id AND m.session_sid='${SID}')")"
  echo
  echo "--- [4] 交叉追溯计数（receipt/coverage/member/batch/window）---"
  echo "  migration_receipts=$(PG "SELECT count(*) FROM continuous_migration_receipts WHERE session_sid='${SID}'")"
  echo "  coverage_segments=$(PG "SELECT count(*) FROM continuous_coverage_segments WHERE session_sid='${SID}'")"
  echo "  block_members=$(PG "SELECT count(*) FROM continuous_parquet_block_members WHERE session_sid='${SID}'")"
  echo "  profile_batches=$(PG "SELECT count(*) FROM profile_batches WHERE session_sid='${SID}'")"
  echo "  profile_windows=$(PG "SELECT count(*) FROM profile_windows WHERE session_sid='${SID}'")"
  echo "  batch_receipt_cover=$(PG "SELECT count(*) FROM profile_batches b WHERE b.session_sid='${SID}' AND NOT EXISTS (SELECT 1 FROM continuous_migration_receipts r WHERE r.source_kind='batch' AND r.source_ref=b.bid AND r.status='passed')")"
  echo
  echo "--- [5] storage/status 关键字段 ---"
  curl -fsS -H "Drop-User-Uid: admin" http://127.0.0.1:8191/api/v1/storage/status 2>/dev/null | python3 -c '
import sys,json
d=json.load(sys.stdin).get("data",{})
for k in ["parquet_mode","fine_gc_mode","migration_receipt_count","migration_receipt_gc_eligible","migration_receipt_gc_deleted_total","fine_gc_candidates_total","fine_gc_deleted_total","fine_gc_failures_total","fine_gc_blocked_by_reason","parquet_query_errors_total","parquet_v1_fallback_total","hot_window_count","hot_batch_count","orphan_window_count"]:
    print(f"  {k} = {d.get(k)}")
' 2>/dev/null
  echo
  echo "--- [6] 单次采样任务（TID）---"
  if [ -n "${TID}" ]; then
    echo "  task: $(PG "SELECT 'tid='||tid||' name='||name||' kind='||task_kind||' status='||status||' analysis_status='||analysis_status||' create='||create_time||' end='||COALESCE(end_time::text,'NULL') FROM hotmethod_tasks WHERE tid='${TID}'")"
    echo "  attempts: $(PG "SELECT 'count='||count(*)||' exit_codes='||string_agg(COALESCE(exit_code::text,'')||':'||COALESCE(error_code,'-'),',') FROM task_attempts WHERE task_tid='${TID}'")"
    echo "  artifacts: $(PG "SELECT 'count='||COALESCE(sum(cnt),0)||' by_status='||COALESCE(string_agg(status||':'||cnt,',' ORDER BY status),'') FROM (SELECT status,count(*) cnt FROM artifacts WHERE task_tid='${TID}' GROUP BY status) s")"
    echo "  artifacts_blob: $(PG "SELECT 'with_blob='||count(*) FILTER (WHERE blob_id IS NOT NULL)||' without_blob='||count(*) FILTER (WHERE blob_id IS NULL) FROM artifacts WHERE task_tid='${TID}'")"
    echo "  analysis_jobs: $(PG "SELECT 'count='||count(*)||' by_status='||string_agg(status||'g'||generation,',') FROM analysis_jobs WHERE task_tid='${TID}' GROUP BY task_tid")"
  else
    echo "  （未提供 TID，跳过）"
  fi
  echo
  echo "--- [7] 磁盘与容器健康 ---"
  df -h / | tail -1
  docker compose -f /home/ubuntu/mini-drop/docker-compose.yml ps --format '{{.Name}}={{.Status}}' 2>/dev/null | tr '\n' ' ' | sed 's/  */ /g'
  echo
} | tee "${REPORT}"

echo "✅ 证据报告已写入: ${REPORT}"
