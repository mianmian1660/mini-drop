#!/bin/bash
# ==============================================================================
# phase6e-session-check.sh — 加速验收 Continuous Session 状态与数据流动检查
# ==============================================================================
# 用法：bash scripts/phase6e-session-check.sh <SID>
#   输出：会话状态（running 时长）、profile_batches/windows/summaries 行数、
#         信号分布、spool 状态、Agent 最近上传时间。
# ==============================================================================
set -u

SID="${1:-}"
if [ -z "${SID}" ]; then
  echo "❌ 需要 Continuous Session SID" >&2
  echo "用法: bash scripts/phase6e-session-check.sh <SID>" >&2
  exit 2
fi

PG() { docker compose -f /home/ubuntu/mini-drop/docker-compose.yml exec -T postgres psql -U postgres -d drop -t -A -F'|' -c "$1" | tr -d ' '; }

echo "=== 会话状态 ==="
PG "SELECT 'sid='||sid||' status='||status||' observed='||observed_state||' started='||started_at||' running_sec='||EXTRACT(EPOCH FROM (now()-started_at))::int FROM continuous_sessions WHERE sid='${SID}'"

echo "=== 数据流动 ==="
PG "SELECT 'batches='||(SELECT count(*) FROM profile_batches WHERE session_sid='${SID}')||' windows='||(SELECT count(*) FROM profile_windows WHERE session_sid='${SID}')||' summaries='||(SELECT count(*) FROM continuous_window_summaries WHERE session_sid='${SID}')"

echo "=== 信号分布（window） ==="
PG "SELECT COALESCE(string_agg(signal_type||':'||cnt, ','), '(无)') FROM (SELECT signal_type, count(*) cnt FROM profile_windows WHERE session_sid='${SID}' GROUP BY signal_type) t"

echo "=== Agent 最近上报 ==="
PG "SELECT 'agent_last_upload='||COALESCE(last_upload_at::text,'NULL')||' observed='||observed_state FROM continuous_sessions WHERE sid='${SID}'"
