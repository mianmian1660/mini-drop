#!/bin/bash
# Validate Rin's Agent execution changes through the live API -> Server -> Agent path.

set -euo pipefail

API_BASE="${API_BASE:-http://127.0.0.1:8191}"
TARGET_IP="${TARGET_IP:-111.230.29.115}"
USER_UID="${USER_UID:-rin-runtime-smoke}"
AGENT_CONTAINER="${AGENT_CONTAINER:-mini-drop-drop_agent-1}"
POSTGRES_CONTAINER="${POSTGRES_CONTAINER:-mini-drop-postgres-1}"
PROFILE_SEC="${PROFILE_SEC:-30}"
WAIT_SEC="${WAIT_SEC:-40}"

TID=""
STARTED_PID=""
START_ISO="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

log() { echo "[rin-runtime] $*"; }
fail() { echo "[rin-runtime] FAIL: $*" >&2; exit 1; }

api_curl() {
    curl -fsS -H "Drop-User-Uid: ${USER_UID}" -H "Drop-User-Name: Rin runtime smoke" "$@"
}

cleanup() {
    if [[ -n "${TID}" ]]; then
        api_curl -X POST "${API_BASE}/api/v1/tasks/${TID}/cancel" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

wait_until() {
    local description="$1"
    local command="$2"
    local deadline=$((SECONDS + WAIT_SEC))
    while (( SECONDS < deadline )); do
        if eval "${command}"; then
            return 0
        fi
        sleep 1
    done
    fail "timed out waiting for ${description}"
}

api_curl "${API_BASE}/healthz" | jq -e '.status == "ok" and .ready == true' >/dev/null
curl -fsS "http://127.0.0.1:6060/burn" >/dev/null || true

CREATE_RESPONSE="$(api_curl -X POST "${API_BASE}/api/v1/tasks" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"Rin runtime cancel smoke $(date +%s)\",\"task_type\":2,\"profiler_type\":2,\"target_ip\":\"${TARGET_IP}\",\"target_pid\":0,\"duration\":${PROFILE_SEC},\"frequency\":1,\"callgraph\":\"fp\",\"event\":\"cpu\",\"pprof_url\":\"http://127.0.0.1:6060/debug/pprof/profile\"}")"
TID="$(jq -r '.data.tid // empty' <<<"${CREATE_RESPONSE}")"
[[ -n "${TID}" ]] || fail "create task did not return tid: ${CREATE_RESPONSE}"
log "created task ${TID}"

wait_until "Agent to start ${TID}" \
    "docker logs '${AGENT_CONTAINER}' --since '${START_ISO}' 2>&1 | grep -Fq '[worker] 开始执行任务! taskID=${TID}'"

ATTEMPT_ID="$(docker exec "${POSTGRES_CONTAINER}" psql -U postgres -d drop -t -A -c \
    "SELECT id FROM task_attempts WHERE task_tid='${TID}' ORDER BY id DESC LIMIT 1;")"
[[ "${ATTEMPT_ID}" =~ ^[0-9]+$ ]] || fail "missing task attempt for ${TID}"

STARTED_PID="$(docker exec "${AGENT_CONTAINER}" sh -c \
    "ps -eo pid,args | awk '\$0 ~ /curl/ && \$0 ~ /debug\\/pprof\\/profile/ && \$0 !~ /awk/ {print \$1; exit}'")"
[[ "${STARTED_PID}" =~ ^[0-9]+$ ]] || fail "pprof curl process was not running"
log "Agent started attempt ${ATTEMPT_ID} with child pid ${STARTED_PID}"

CANCEL_RESPONSE="$(api_curl -X POST "${API_BASE}/api/v1/tasks/${TID}/cancel")"
jq -e '.data.status == 5 and .data.cancel_requested == true' <<<"${CANCEL_RESPONSE}" >/dev/null \
    || fail "cancel API did not return canceled state: ${CANCEL_RESPONSE}"

wait_until "Runner cancellation for ${TID}" \
    "docker logs '${AGENT_CONTAINER}' --since '${START_ISO}' 2>&1 | grep -Fq 'stage=Stop taskID=${TID} reason=Cancel'"
wait_until "pprof child ${STARTED_PID} to exit" \
    "! docker exec '${AGENT_CONTAINER}' kill -0 '${STARTED_PID}' >/dev/null 2>&1"
wait_until "task ${TID} to remain canceled" \
    "api_curl '${API_BASE}/api/v1/tasks/${TID}' | jq -e '.data.task.status == 5' >/dev/null"

docker exec "${AGENT_CONTAINER}" test -d "/tmp/drop_agent/tasks/${TID}/${ATTEMPT_ID}" \
    || fail "isolated task attempt directory is missing"

docker logs "${AGENT_CONTAINER}" --since "${START_ISO}" 2>&1 \
    | grep -F "taskID=${TID}" | tail -20
log "PASS: cancellation propagated, child exited, task stayed canceled, attempt directory is isolated"

trap - EXIT
