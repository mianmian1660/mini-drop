#!/bin/bash
# ==============================================================================
# phase6e-task-inspect.sh — 单次采样任务分析失败/产物排查
# ==============================================================================
# 用法：bash scripts/phase6e-task-inspect.sh <TID>
#   输出：任务状态/status_info、analysis_jobs 详情（含 last_error）、
#         artifacts 清单（kind/object_key/size/status/format/blob_id）。
# ==============================================================================
set -u

TID="${1:-}"
if [ -z "${TID}" ]; then
  echo "❌ 需要任务 TID" >&2
  echo "用法: bash scripts/phase6e-task-inspect.sh <TID>" >&2
  exit 2
fi

PG() { docker compose -f /home/ubuntu/mini-drop/docker-compose.yml exec -T postgres psql -U postgres -d drop -t -A -F'|' -c "$1" | tr -d ' '; }

echo "=== 任务状态 ==="
PG "SELECT 'status='||status||' analysis_status='||analysis_status||' info='||COALESCE(status_info,'') FROM hotmethod_tasks WHERE tid='${TID}'"

echo "=== analysis_jobs ==="
PG "SELECT 'id='||id||' pipeline='||pipeline||' status='||status||' attempt='||attempt||'/'||max_attempts||' gen='||generation||' err='||COALESCE(last_error,'') FROM analysis_jobs WHERE task_tid='${TID}' ORDER BY id"

echo "=== artifacts ==="
PG "SELECT 'id='||id||' kind='||kind||' obj='||object_key||' size='||size||' status='||status||' fmt='||COALESCE(format,'')||' blob='||COALESCE(blob_id::text,'NULL') FROM artifacts WHERE task_tid='${TID}' ORDER BY id"

echo "=== task_attempts ==="
PG "SELECT 'seq='||attempt_seq||' exit='||COALESCE(exit_code::text,'')||' err='||COALESCE(error_code,'')||' msg='||COALESCE(error_message,'') FROM task_attempts WHERE task_tid='${TID}' ORDER BY attempt_seq"
