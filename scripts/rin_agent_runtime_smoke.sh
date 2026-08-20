#!/usr/bin/env bash
set -euo pipefail

API_BASE="${API_BASE:-http://127.0.0.1:8191}"
TARGET_IP="${TARGET_IP:-111.230.29.115}"
AGENT_CONTAINER="${AGENT_CONTAINER:-mini-drop-drop_agent-1}"
SERVER_CONTAINER="${SERVER_CONTAINER:-mini-drop-drop_server-1}"
POSTGRES_CONTAINER="${POSTGRES_CONTAINER:-mini-drop-postgres-1}"
WAIT_SEC="${WAIT_SEC:-75}"
SHORT_PROFILE_SEC="${SHORT_PROFILE_SEC:-3}"
LONG_PROFILE_SEC="${LONG_PROFILE_SEC:-30}"
TEST_RUN_ID="${TEST_RUN_ID:-rin-runtime-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
START_ISO="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
PPROF_URL="${PPROF_URL:-http://127.0.0.1:6060/debug/pprof/profile}"
USER_UID="${USER_UID:-rin-runtime-smoke}"
USER_NAME="${USER_NAME:-Rin Runtime Smoke}"
# Diagnostic-only compatibility switch for assessing an older deployed build.
# The default remains strict and requires the fixed CANCELED/TASK_CANCELED state.
ALLOW_DEPLOYED_CANCEL_REGRESSION="${ALLOW_DEPLOYED_CANCEL_REGRESSION:-false}"

declare -a TEST_TIDS=()
LAST_TID=""

log() { printf '[rin-smoke] %s\n' "$*"; }

api_curl() {
    curl -fsS "$@" \
        -H "Drop-User-Uid: ${USER_UID}" \
        -H "Drop-User-Name: ${USER_NAME}"
}

task_detail() {
    api_curl "${API_BASE}/api/v1/tasks/$1"
}

attempt_id() {
    docker exec "${POSTGRES_CONTAINER}" psql -U postgres -d drop -t -A -c \
        "SELECT id FROM task_attempts WHERE task_tid='$1' ORDER BY id DESC LIMIT 1;"
}

dump_diagnostics() {
    log "diagnostics for run ${TEST_RUN_ID}"
    curl -sS "${API_BASE}/healthz" || true
    for tid in "${TEST_TIDS[@]}"; do
        printf '\n--- task %s ---\n' "${tid}"
        task_detail "${tid}" || true
    done
    printf '\n--- agent logs ---\n'
    docker logs "${AGENT_CONTAINER}" --since "${START_ISO}" 2>&1 \
        | grep -E "${TEST_RUN_ID}|$(IFS='|'; printf '%s' "${TEST_TIDS[*]}")" | tail -160 || true
    printf '\n--- database rows ---\n'
    docker exec "${POSTGRES_CONTAINER}" psql -U postgres -d drop -P pager=off -c \
        "SELECT t.tid,t.name,t.status,t.cancel_requested,a.id AS attempt_id,a.exit_code,a.error_code,a.end_time AS attempt_end FROM hotmethod_tasks t LEFT JOIN task_attempts a ON a.task_tid=t.tid WHERE t.name LIKE '${TEST_RUN_ID}%' ORDER BY t.create_time;" || true
}

fail() {
    log "FAIL: $*"
    dump_diagnostics
    exit 1
}

cleanup() {
    for tid in "${TEST_TIDS[@]}"; do
        api_curl -X POST "${API_BASE}/api/v1/tasks/${tid}/cancel" >/dev/null 2>&1 || true
    done
}
trap cleanup EXIT

wait_until() {
    local description="$1"
    shift
    local deadline=$((SECONDS + WAIT_SEC))
    while (( SECONDS < deadline )); do
        if "$@"; then
            return 0
        fi
        sleep 1
    done
    fail "timed out waiting for ${description}"
}

logs_contain() {
    docker logs "${AGENT_CONTAINER}" --since "${START_ISO}" 2>&1 | grep -Fq "$1"
}

task_has_status() {
    task_detail "$1" | jq -e --argjson status "$2" '.data.task.status == $status' >/dev/null
}

task_is_complete_with_artifact() {
    task_detail "$1" | jq -e '
        .data.task.status == 2 and
        ((.data.attempts // []) | length) > 0 and
        .data.attempts[-1].end_time != null and
        ((((.data.files // []) | length) > 0) or (((.data.artifacts // []) | length) > 0))
    ' >/dev/null
}

task_attempt_is_canceled() {
    if [[ "${ALLOW_DEPLOYED_CANCEL_REGRESSION}" == "true" ]]; then
        task_detail "$1" | jq -e '
            ((.data.task.status == 5) or (.data.task.status == 3)) and
            ((.data.attempts // []) | length) > 0 and
            .data.attempts[-1].end_time != null and
            ((.data.attempts[-1].error_code == "TASK_CANCELED") or
             (.data.attempts[-1].error_code == "TASK_EXECUTION_FAILED"))
        ' >/dev/null
        return
    fi
    task_detail "$1" | jq -e '
        .data.task.status == 5 and
        ((.data.attempts // []) | length) > 0 and
        .data.attempts[-1].end_time != null and
        (.data.attempts[-1].error_code == "TASK_CANCELED")
    ' >/dev/null
}

create_pprof_task() {
    local label="$1"
    local duration="$2"
    local response
    response="$(api_curl -X POST "${API_BASE}/api/v1/tasks" \
        -H "Content-Type: application/json" \
        -d "{\"name\":\"${TEST_RUN_ID} ${label}\",\"task_type\":2,\"profiler_type\":2,\"target_ip\":\"${TARGET_IP}\",\"target_pid\":0,\"duration\":${duration},\"frequency\":1,\"callgraph\":\"fp\",\"event\":\"cpu\",\"pprof_url\":\"${PPROF_URL}\"}")"
    LAST_TID="$(jq -r '.data.tid // empty' <<<"${response}")"
    [[ -n "${LAST_TID}" ]] || fail "create ${label} did not return tid: ${response}"
    TEST_TIDS+=("${LAST_TID}")
    log "created ${label}: ${LAST_TID}"
}

cancel_task() {
    local tid="$1"
    local response
    response="$(api_curl -X POST "${API_BASE}/api/v1/tasks/${tid}/cancel")"
    jq -e '.data.status == 5 and .data.cancel_requested == true' <<<"${response}" >/dev/null \
        || fail "cancel API did not return canceled state for ${tid}: ${response}"
}

command -v jq >/dev/null || fail "jq is required"
api_curl "${API_BASE}/healthz" | jq -e '.status == "ok" and .ready == true' >/dev/null \
    || fail "API is not ready"

AGENT_RESTARTS_BEFORE="$(docker inspect -f '{{.RestartCount}}' "${AGENT_CONTAINER}")"
SERVER_RESTARTS_BEFORE="$(docker inspect -f '{{.RestartCount}}' "${SERVER_CONTAINER}")"
log "run_id=${TEST_RUN_ID} agent_restarts=${AGENT_RESTARTS_BEFORE} server_restarts=${SERVER_RESTARTS_BEFORE}"
sha256sum \
    drop/agent/HeartbeatThread.cpp \
    drop/agent/WorkerThread.cpp \
    drop/agent/UploadWorker.cpp \
    drop/server/TaskQueue.cpp

# 1. Baseline: collection, upload, notification, attempt closure and artifact.
curl -fsS "http://127.0.0.1:6060/burn" >/dev/null || true
create_pprof_task "baseline" "${SHORT_PROFILE_SEC}"
BASELINE_TID="${LAST_TID}"
wait_until "baseline worker start" logs_contain "[worker] 开始执行任务! taskID=${BASELINE_TID}"
wait_until "baseline completion and artifact" task_is_complete_with_artifact "${BASELINE_TID}"
log "PASS baseline: ${BASELINE_TID}"

# 2. Cancel a task after its real curl child has started.
create_pprof_task "active-cancel" "${LONG_PROFILE_SEC}"
ACTIVE_TID="${LAST_TID}"
wait_until "active worker start" logs_contain "[worker] 开始执行任务! taskID=${ACTIVE_TID}"
ACTIVE_ATTEMPT="$(attempt_id "${ACTIVE_TID}")"
[[ "${ACTIVE_ATTEMPT}" =~ ^[0-9]+$ ]] || fail "missing attempt for ${ACTIVE_TID}"
ACTIVE_PID="$(docker exec "${AGENT_CONTAINER}" sh -c \
    "ps -eo pid,args | awk '\$0 ~ /curl/ && \$0 ~ /debug\\/pprof\\/profile/ && \$0 !~ /awk/ {print \$1; exit}'")"
[[ "${ACTIVE_PID}" =~ ^[0-9]+$ ]] || fail "active pprof child was not running"
cancel_task "${ACTIVE_TID}"
wait_until "active cancellation stop" logs_contain "stage=Stop taskID=${ACTIVE_TID} reason=Cancel"
wait_until "active child exit" sh -c "! docker exec '${AGENT_CONTAINER}' kill -0 '${ACTIVE_PID}' >/dev/null 2>&1"
wait_until "active canceled attempt convergence" task_attempt_is_canceled "${ACTIVE_TID}"
docker exec "${AGENT_CONTAINER}" test -d "/tmp/drop_agent/tasks/${ACTIVE_TID}/${ACTIVE_ATTEMPT}" \
    || fail "active attempt directory is missing"
log "PASS active cancellation: ${ACTIVE_TID} attempt=${ACTIVE_ATTEMPT} pid=${ACTIVE_PID}"

# 3. Hold the single worker, then cancel a task that only reached its queue.
create_pprof_task "queue-blocker" "${LONG_PROFILE_SEC}"
BLOCKER_TID="${LAST_TID}"
wait_until "blocker worker start" logs_contain "[worker] 开始执行任务! taskID=${BLOCKER_TID}"
create_pprof_task "queued-cancel" "${SHORT_PROFILE_SEC}"
QUEUED_TID="${LAST_TID}"
wait_until "queued task receipt" logs_contain "[heartbeat] 收到任务! taskID=${QUEUED_TID}"
QUEUED_ATTEMPT="$(attempt_id "${QUEUED_TID}")"
[[ "${QUEUED_ATTEMPT}" =~ ^[0-9]+$ ]] || fail "missing attempt for ${QUEUED_TID}"
logs_contain "[worker] 开始执行任务! taskID=${QUEUED_TID}" \
    && fail "queued task started before cancellation"
cancel_task "${QUEUED_TID}"
wait_until "queued cancellation acknowledgement" logs_contain "取消指令命中排队中任务(尚未开始采集)，直接摘除: taskID=${QUEUED_TID}"
wait_until "queued canceled attempt convergence" task_attempt_is_canceled "${QUEUED_TID}"
logs_contain "[worker] 开始执行任务! taskID=${QUEUED_TID}" \
    && fail "queued task started after cancellation"
if docker exec "${AGENT_CONTAINER}" test -e "/tmp/drop_agent/tasks/${QUEUED_TID}/${QUEUED_ATTEMPT}"; then
    fail "queued canceled task unexpectedly created an attempt directory"
fi
cancel_task "${BLOCKER_TID}"
wait_until "blocker cancellation" task_attempt_is_canceled "${BLOCKER_TID}"
log "PASS queued cancellation: ${QUEUED_TID}"

# 4. A fresh task must still complete after both cancellation paths.
create_pprof_task "post-cancel-recovery" "${SHORT_PROFILE_SEC}"
RECOVERY_TID="${LAST_TID}"
wait_until "post-cancel worker start" logs_contain "[worker] 开始执行任务! taskID=${RECOVERY_TID}"
wait_until "post-cancel completion and artifact" task_is_complete_with_artifact "${RECOVERY_TID}"
log "PASS post-cancel recovery: ${RECOVERY_TID}"

AGENT_RESTARTS_AFTER="$(docker inspect -f '{{.RestartCount}}' "${AGENT_CONTAINER}")"
SERVER_RESTARTS_AFTER="$(docker inspect -f '{{.RestartCount}}' "${SERVER_CONTAINER}")"
[[ "${AGENT_RESTARTS_AFTER}" == "${AGENT_RESTARTS_BEFORE}" ]] \
    || fail "agent restart count changed: ${AGENT_RESTARTS_BEFORE} -> ${AGENT_RESTARTS_AFTER}"
[[ "${SERVER_RESTARTS_AFTER}" == "${SERVER_RESTARTS_BEFORE}" ]] \
    || fail "server restart count changed: ${SERVER_RESTARTS_BEFORE} -> ${SERVER_RESTARTS_AFTER}"

if docker logs "${AGENT_CONTAINER}" --since "${START_ISO}" 2>&1 \
    | grep -E "发现孤儿进程|NotifyResult 重试耗尽" | grep -E "${TEST_RUN_ID}|$(IFS='|'; printf '%s' "${TEST_TIDS[*]}")"; then
    fail "orphan cleanup or exhausted result notification occurred"
fi

dump_diagnostics
log "PASS all scenarios: baseline, active cancel, queued cancel, post-cancel recovery"
trap - EXIT
