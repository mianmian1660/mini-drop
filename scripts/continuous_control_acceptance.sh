#!/usr/bin/env bash
set -Eeuo pipefail

# Server-side acceptance for Continuous Session control-plane invariants.
# The test deliberately uses missing absolute exe selectors for the 16-way
# boundary so it exercises reconciliation without launching 16 workloads.

BASE=${BASE:-http://127.0.0.1:8191}
TARGET_IP=${TARGET_IP:-111.230.29.115}
AUTH_UID=${AUTH_UID:-continuous-boundary}
AUTH_NAME=${AUTH_NAME:-continuous-boundary}
RUN_TAG="boundary-$(date -u +%Y%m%dT%H%M%SZ)-$$"
TEST_ROOT="$(mktemp -d /tmp/mini-drop-continuous-boundary.XXXXXX)"
PSQL=(docker exec mini-drop-postgres-1 psql -U postgres -d drop -At)
HEADERS=(-H "Drop-User-Uid: $AUTH_UID" -H "Drop-User-Name: $AUTH_NAME")
SIDS=()

pass() { printf 'PASS %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1" >&2; exit 1; }

stop_session() {
    curl -sS -X POST "$BASE/api/v1/continuous/sessions/$1/stop" "${HEADERS[@]}" >/dev/null || true
}

cleanup() {
    set +e
    for sid in "${SIDS[@]-}"; do
        [[ -n "$sid" ]] && stop_session "$sid"
    done
    rm -rf "$TEST_ROOT"
}
trap cleanup EXIT

json_value() {
    python3 - "$1" "$2" <<'PY'
import json, sys
body = json.load(open(sys.argv[1], encoding="utf-8"))
value = body
for part in sys.argv[2].split('.'):
    value = value[part]
print(value)
PY
}

create_request() {
    local name="$1" scope="$2" selector="${3:-}" output="$4"
    python3 - "$output" "$name" "$TARGET_IP" "$scope" "$selector" <<'PY'
import json, sys
path, name, target_ip, scope, selector = sys.argv[1:]
body = {
    "name": name,
    "target_ip": target_ip,
    "hostname": "cloud-server-111-230-29-115",
    "scope": scope,
    "signals": ["cpu_profile", "io_latency", "io_syscall_latency", "sched_latency"],
    "sample_rate_hz": 9,
    "aggregation_window_sec": 5,
    "upload_batch_sec": 15,
    "retention_hours": 1,
    "continuity_mode": "strict",
    "allow_degraded": True,
    "labels": {"acceptance": "continuous-boundary"},
}
if scope == "process":
    body["selector_exe"] = selector
    body["selector_mode"] = "all_instances"
json.dump(body, open(path, "w", encoding="utf-8"), separators=(",", ":"))
PY
}

post_create() {
    local request="$1" response="$2"
    curl -sS -o "$response" -w '%{http_code}' -X POST \
        "$BASE/api/v1/continuous/sessions" "${HEADERS[@]}" \
        -H 'Content-Type: application/json' -d "@$request"
}

assert_rejected() {
    local request="$1" phrase="$2" label="$3"
    local response="$TEST_ROOT/rejected-$label.json"
    local code
    code="$(post_create "$request" "$response")"
    [[ "$code" == "409" ]] || { cat "$response" >&2; fail "$label expected HTTP 409, got $code"; }
    grep -Fq "$phrase" "$response" || { cat "$response" >&2; fail "$label missing explicit error: $phrase"; }
    pass "$label rejected with explicit error"
}

wait_all_stopped() {
    local deadline=$((SECONDS + 45))
    while (( SECONDS < deadline )); do
        local remaining
        remaining="$("${PSQL[@]}" -c "select count(*) from continuous_sessions where sid in ($(printf "'%s'," "${SIDS[@]}" | sed 's/,$//')) and observed_state <> 'stopped';")"
        [[ "$remaining" == "0" ]] && return 0
        sleep 2
    done
    return 1
}

printf '[boundary] run=%s target=%s\n' "$RUN_TAG" "$TARGET_IP"
curl -fsS "$BASE/healthz" >/dev/null || fail "apiserver healthz"
active_before="$("${PSQL[@]}" -c "select count(*) from continuous_sessions where target_ip='$TARGET_IP' and desired_state='running';")"
[[ "$active_before" == "0" ]] || fail "host already has $active_before active Continuous Sessions"

for index in $(seq 1 16); do
    request="$TEST_ROOT/process-$index.json"
    response="$TEST_ROOT/process-$index-response.json"
    selector="/tmp/mini-drop-$RUN_TAG-$index"
    create_request "$RUN_TAG-process-$index" process "$selector" "$request"
    code="$(post_create "$request" "$response")"
    [[ "$code" == "200" ]] || { cat "$response" >&2; fail "create process Session $index HTTP $code"; }
    SIDS+=("$(json_value "$response" data.session.sid)")
done
pass "created exactly 16 process Sessions"

request17="$TEST_ROOT/process-17.json"
create_request "$RUN_TAG-process-17" process "/tmp/mini-drop-$RUN_TAG-17" "$request17"
assert_rejected "$request17" "同一主机最多同时运行 16 个进程持续任务" "process-limit-17"

duplicate="$TEST_ROOT/process-duplicate.json"
create_request "$RUN_TAG-duplicate" process "/tmp/mini-drop-$RUN_TAG-1" "$duplicate"
assert_rejected "$duplicate" "同一主机已存在相同 exe 的活动持续任务" "duplicate-exe"

host_conflict="$TEST_ROOT/host-conflict.json"
create_request "$RUN_TAG-host-conflict" host "" "$host_conflict"
assert_rejected "$host_conflict" "同一主机的整机持续任务与进程持续任务互斥" "host-process-mutual-exclusion"

before_restart="$("${PSQL[@]}" -c "select string_agg(sid,',' order by sid) from continuous_sessions where name like '$RUN_TAG-%' and desired_state='running';")"
(cd /home/ubuntu/mini-drop && docker compose restart apiserver >/dev/null)
for attempt in $(seq 1 30); do
    curl -fsS "$BASE/healthz" >/dev/null 2>&1 && break
    sleep 1
done
curl -fsS "$BASE/healthz" >/dev/null || fail "apiserver did not recover after restart"
sleep 8
after_restart="$("${PSQL[@]}" -c "select string_agg(sid,',' order by sid) from continuous_sessions where name like '$RUN_TAG-%' and desired_state='running';")"
[[ "$after_restart" == "$before_restart" ]] || fail "apiserver restart changed active Session SIDs"
run_count="$("${PSQL[@]}" -c "select count(*) from continuous_sessions where name like '$RUN_TAG-%';")"
[[ "$run_count" == "16" ]] || fail "apiserver restart created unexpected Session rows: $run_count"
pass "apiserver restart preserved the same 16 SIDs and created no Session"

for sid in "${SIDS[@]}"; do stop_session "$sid"; done
wait_all_stopped || fail "process Sessions did not reach stopped after final Agent acknowledgement"
pass "all 16 process Sessions stopped"

SIDS=()
host_request="$TEST_ROOT/host.json"
host_response="$TEST_ROOT/host-response.json"
create_request "$RUN_TAG-host" host "" "$host_request"
code="$(post_create "$host_request" "$host_response")"
[[ "$code" == "200" ]] || { cat "$host_response" >&2; fail "create host Session HTTP $code"; }
host_sid="$(json_value "$host_response" data.session.sid)"
SIDS+=("$host_sid")

second_host="$TEST_ROOT/host-second.json"
create_request "$RUN_TAG-host-second" host "" "$second_host"
assert_rejected "$second_host" "同一主机最多同时运行 1 个整机持续任务" "host-limit-1"

process_under_host="$TEST_ROOT/process-under-host.json"
create_request "$RUN_TAG-process-under-host" process "/tmp/mini-drop-$RUN_TAG-under-host" "$process_under_host"
assert_rejected "$process_under_host" "同一主机的整机持续任务与进程持续任务互斥" "process-host-mutual-exclusion"

backpressure_seen=0
for attempt in $(seq 1 20); do
    row="$("${PSQL[@]}" -F '|' -c "select observed_state,degradation_reason from continuous_sessions where sid='$host_sid';")"
    if [[ "$row" == degraded*spool\ backpressure* ]]; then
        backpressure_seen=1
        break
    fi
    sleep 1
done
[[ "$backpressure_seen" == "1" ]] || fail "host Session did not expose low-disk spool backpressure"
rolling_processes="$(ps -eo args | awk '/--switch-output/ && !/awk/ {count++} END {print count+0}')"
[[ "$rolling_processes" == "0" ]] || fail "rolling perf started despite spool backpressure"
pass "low-disk host Session paused without launching rolling perf"

stop_session "$host_sid"
wait_all_stopped || fail "host Session did not stop"
final_active="$("${PSQL[@]}" -c "select count(*) from continuous_sessions where target_ip='$TARGET_IP' and desired_state='running';")"
[[ "$final_active" == "0" ]] || fail "acceptance left $final_active active Sessions"
pass "host Session stopped and no active acceptance task remains"
printf '[boundary] completed run=%s\n' "$RUN_TAG"
