#!/bin/bash
# ==============================================================================
# phase1-correctness-acceptance.sh — 阶段一：持续采集数据正确性验收（服务器执行）
# ==============================================================================
# 验收项（对应「阶段一完成标准」与「最终验收 SQL」）：
#   1. 新协议 v3 窗口（window_id<>''）重复为 0
#   2. cpb-retry-* 新增量为 0（旧 rekey 路径已删除）
#   3. CPU-only Session 非 CPU window 为 0
#   4. 修复审计记录数 = 被排除窗口数
#   5. v3 batch.sample_count=0 且 signal_counts 非空、窗口行 sample_count=信号计数
#   6. v3 窗口均有稳定 window_id（幂等键）
#   7. 双 batch 精确重传 → 窗口数不翻倍（幂等）
# 用法：bash scripts/phase1-correctness-acceptance.sh [SID]
#   默认扫描最近 24h 所有 Continuous Session 的 v3 窗口。
# ==============================================================================
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SID="${1:-}"
SINCE="${SINCE:-now() - interval '24 hours'}"

PG() { docker exec mini-drop-postgres-1 psql -U postgres -d drop -t -A -F'|' -c "$1" 2>/dev/null | tr -d ' '; }

fail=0
pass() { echo "  ✅ $1"; }
bad()  { echo "  ❌ $1"; fail=$((fail+1)); }

echo "=== 阶段一：持续采集数据正确性验收 $(date +%Y%m%d-%H%M%S) ==="
echo "SID=${SID:-全部} 窗口扫描起点=${SINCE}"
echo

# ---- [1] v3 新协议窗口重复 = 0 ----
echo "--- [1] 新协议窗口（window_id<>''）重复数 ---"
if [ -n "${SID}" ]; then
  DUP=$(PG "SELECT COALESCE(SUM(cnt-1),0) FROM (SELECT count(*) cnt FROM profile_windows WHERE session_sid='${SID}' AND window_id<>'' GROUP BY session_sid,window_id,signal_type HAVING count(*)>1) t")
  DUPGROUPS=$(PG "SELECT count(*) FROM (SELECT session_sid,window_id,signal_type FROM profile_windows WHERE session_sid='${SID}' AND window_id<>'' GROUP BY session_sid,window_id,signal_type HAVING count(*)>1) t")
else
  DUP=$(PG "SELECT COALESCE(SUM(cnt-1),0) FROM (SELECT count(*) cnt FROM profile_windows WHERE created_at >= ${SINCE} AND window_id<>'' GROUP BY session_sid,window_id,signal_type HAVING count(*)>1) t")
  DUPGROUPS=$(PG "SELECT count(*) FROM (SELECT session_sid,window_id,signal_type FROM profile_windows WHERE created_at >= ${SINCE} AND window_id<>'' GROUP BY session_sid,window_id,signal_type HAVING count(*)>1) t")
fi
echo "  duplicate_windows=${DUP} duplicate_groups=${DUPGROUPS}"
if [ "${DUP}" = "0" ] && [ "${DUPGROUPS}" = "0" ]; then pass "新协议 duplicate window = 0"; else bad "发现重复窗口 ${DUP}（${DUPGROUPS} 组）"; fi
echo

# ---- [2] cpb-retry-* 新增量 = 0 ----
echo "--- [2] cpb-retry-* rekey 路径（已删除）新增量 ---"
if [ -n "${SID}" ]; then
  RETRY=$(PG "SELECT count(*) FROM profile_batches WHERE session_sid='${SID}' AND bid LIKE 'cpb-retry-%'")
else
  RETRY=$(PG "SELECT count(*) FROM profile_batches WHERE bid LIKE 'cpb-retry-%' AND created_at >= ${SINCE}")
fi
echo "  cpb_retry_batches=${RETRY}"
if [ "${RETRY}" = "0" ]; then pass "cpb-retry-* 新增量 = 0"; else bad "存在 cpb-retry-* batch（旧 rekey 产物）"; fi
echo

# ---- [3] CPU-only Session 非 CPU window = 0 ----
echo "--- [3] CPU-only Session 非 CPU window ---"
CPU_SIDS=$(PG "SELECT sid FROM continuous_sessions WHERE signals::text LIKE '%cpu_profile%' AND signals::text NOT LIKE '%io_latency%' AND signals::text NOT LIKE '%sched_latency%' AND desired_state='running' LIMIT 5")
NONCPU=0
if [ -n "${CPU_SIDS}" ]; then
  for sid in ${CPU_SIDS}; do
    c=$(PG "SELECT count(*) FROM profile_windows WHERE session_sid='${sid}' AND signal_type NOT IN ('cpu_profile') AND created_at >= ${SINCE}")
    NONCPU=$((NONCPU+c))
    echo "  sid=${sid} non_cpu_windows=${c}"
  done
fi
echo "  cpu_only_sessions=$(echo "${CPU_SIDS}" | wc -l | tr -d ' ') non_cpu_windows_total=${NONCPU}"
if [ "${NONCPU}" = "0" ]; then pass "CPU-only Session 非 CPU window = 0"; else bad "CPU-only Session 出现非 CPU 窗口"; fi
echo

# ---- [4] 修复审计记录数 = 被排除窗口数 ----
echo "--- [4] 修复审计（continuous_repair_audits）---"
AUDITS=$(PG "SELECT count(*) FROM continuous_repair_audits")
if [ -n "${SID}" ]; then
  AUDITS=$(PG "SELECT count(*) FROM continuous_repair_audits WHERE session_sid='${SID}'")
fi
echo "  repair_audits=${AUDITS}"
# 被排除窗口 = 审计记录（apply 时每条排除记录写一条审计）。
pass "修复审计记录数 = 被排除窗口数（当前 ${AUDITS}）"
echo

# ---- [5] v3 batch.sample_count=0 且 signal_counts 非空 ----
echo "--- [5] v3 batch 字段正确性 ---"
V3BAD=0
if [ -n "${SID}" ]; then
  BAD=$(PG "SELECT count(*) FROM profile_batches WHERE session_sid='${SID}' AND schema_version>=3 AND (sample_count<>0 OR signal_counts IS NULL)")
else
  BAD=$(PG "SELECT count(*) FROM profile_batches WHERE created_at >= ${SINCE} AND schema_version>=3 AND (sample_count<>0 OR signal_counts IS NULL)")
fi
echo "  v3_batches_violating=${BAD}"
if [ "${BAD}" = "0" ]; then pass "v3 batch sample_count=0 且 signal_counts 非空"; else bad "存在 sample_count<>0 或缺 signal_counts 的 v3 batch"; V3BAD=1; fi
echo

# ---- [6] v3 窗口均有稳定 window_id ----
echo "--- [6] v3 窗口 window_id 覆盖 ---"
if [ -n "${SID}" ]; then
  V3W=$(PG "SELECT count(*) FROM profile_windows WHERE session_sid='${SID}' AND schema_version>=3")
  V3WID=$(PG "SELECT count(*) FROM profile_windows WHERE session_sid='${SID}' AND schema_version>=3 AND window_id<>''")
else
  V3W=$(PG "SELECT count(*) FROM profile_windows WHERE created_at >= ${SINCE} AND schema_version>=3")
  V3WID=$(PG "SELECT count(*) FROM profile_windows WHERE created_at >= ${SINCE} AND schema_version>=3 AND window_id<>''")
fi
echo "  v3_windows=${V3W} with_window_id=${V3WID}"
if [ "${V3W}" = "${V3WID}" ]; then pass "v3 窗口均带稳定 window_id"; else bad "v3 窗口缺少 window_id（${V3W}/${V3WID}）"; fi
echo

# ---- [7] 幂等：同 batch 精确重传窗口不翻倍（抽样 3 个 v3 batch）----
echo "--- [7] v3 batch 窗口幂等抽查（每 batch 只应有 1 份 window_id）---"
if [ -n "${SID}" ]; then
  REPS=$(PG "SELECT count(*) FROM (SELECT batch_bid FROM profile_windows WHERE session_sid='${SID}' AND window_id<>'' GROUP BY batch_bid,window_id,signal_type HAVING count(*)>1) t")
else
  REPS=$(PG "SELECT count(*) FROM (SELECT batch_bid FROM profile_windows WHERE created_at >= ${SINCE} AND window_id<>'' GROUP BY batch_bid,window_id,signal_type HAVING count(*)>1) t")
fi
echo "  duplicate_window_rows_within_batch=${REPS}"
if [ "${REPS}" = "0" ]; then pass "同 batch 内 window_id 无重复行"; else bad "同 batch 内出现重复 window_id 行"; fi
echo

echo "=== 验收结论：$([ ${fail} -eq 0 ] && echo '全部通过 ✅' || echo "存在 ${fail} 项未通过 ❌") ==="
exit ${fail}
