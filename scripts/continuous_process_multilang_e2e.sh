#!/usr/bin/env bash
set -Eeuo pipefail

# Server-side acceptance test for user-driven process Continuous Sessions.
# It starts several language workloads, uses their real /proc executable paths,
# and validates stored batch objects through the API.

BASE=${BASE:-http://127.0.0.1:8191}
TARGET_IP=${TARGET_IP:-111.230.29.115}
AUTH_UID=${AUTH_UID:-continuous-e2e}
AUTH_NAME=${AUTH_NAME:-continuous-e2e}
WAIT_DATA_SEC=${WAIT_DATA_SEC:-32}
WAIT_STOP_SEC=${WAIT_STOP_SEC:-30}
WAIT_VALID_DATA_SEC=${WAIT_VALID_DATA_SEC:-120}
TEST_ROOT="$(mktemp -d /tmp/mini-drop-continuous-e2e.XXXXXX)"
RUN_TAG="$(date -u +%Y%m%dT%H%M%SZ)-$$"

declare -a PIDS=()
declare -a START_MS=()
declare -a EXES=()
declare -a NAMES=()
declare -a SIDS=()

pass() { printf 'PASS %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1" >&2; exit 1; }

api_headers=(-H "Drop-User-Uid: $AUTH_UID" -H "Drop-User-Name: $AUTH_NAME")

process_start_ms() {
    python3 - "$1" <<'PY'
import os, sys
pid = int(sys.argv[1])
with open(f"/proc/{pid}/stat", encoding="ascii") as handle:
    raw = handle.read()
right = raw.rsplit(")", 1)[1].split()
start_ticks = int(right[19])
with open("/proc/stat", encoding="ascii") as handle:
    btime = next(int(line.split()[1]) for line in handle if line.startswith("btime "))
print(int((btime + start_ticks / os.sysconf("SC_CLK_TCK")) * 1000))
PY
}

stop_session() {
    local sid="$1"
    curl -sS -X POST "$BASE/api/v1/continuous/sessions/$sid/stop" "${api_headers[@]}" >/dev/null || true
}

cleanup() {
    set +e
    for sid in "${SIDS[@]-}"; do
        [[ -n "$sid" ]] && stop_session "$sid"
    done
    for pid in "${PIDS[@]-}"; do
        [[ -n "$pid" ]] && kill -TERM "$pid" 2>/dev/null || true
    done
    sleep 1
    for pid in "${PIDS[@]-}"; do
        [[ -n "$pid" ]] && kill -KILL "$pid" 2>/dev/null || true
        [[ -n "$pid" ]] && wait "$pid" 2>/dev/null || true
    done
    rm -rf "$TEST_ROOT"
}
trap cleanup EXIT

printf '[e2e] base=%s target=%s run=%s\n' "$BASE" "$TARGET_IP" "$RUN_TAG"
curl -fsS "$BASE/healthz" >/dev/null || fail "apiserver healthz"
[[ -x /home/ubuntu/profiling_test/test_c_profiling ]] || fail "C workload missing"
[[ -x /home/ubuntu/profiling_test/test_go_profiling ]] || fail "Go workload missing"
command -v node >/dev/null || fail "node unavailable"
command -v python3 >/dev/null || fail "python3 unavailable"
command -v java >/dev/null || fail "java unavailable"

# Use a private executable path for the Python workload. The acceptance script
# itself invokes short-lived python helpers while Sessions are live; pointing at
# the system interpreter would intentionally follow those helpers as additional
# all-instances targets and churn the shared recorder. A copied interpreter
# still exercises the Python runtime, while its exact exe selector remains
# deterministic and proves that unrelated Python processes are excluded.
PYTHON_WORKLOAD_EXE="$TEST_ROOT/python3-e2e"
cp "$(readlink -f "$(command -v python3)")" "$PYTHON_WORKLOAD_EXE"
chmod 0755 "$PYTHON_WORKLOAD_EXE"

start_workload() {
    local name="$1"
    shift
    nice -n 10 "$@" >/dev/null 2>&1 &
    local pid=$!
    sleep 1
    kill -0 "$pid" 2>/dev/null || fail "$name workload exited during startup"
    local exe="$(readlink -f "/proc/$pid/exe")"
    local start="$(process_start_ms "$pid")"
    [[ -n "$exe" && -e "$exe" && "$start" -gt 0 ]] || fail "$name process identity unavailable"
    PIDS+=("$pid")
    START_MS+=("$start")
    EXES+=("$exe")
    NAMES+=("$name")
    printf '[workload] name=%s pid=%s start_ms=%s exe=%s\n' "$name" "$pid" "$start" "$exe"
}

start_workload c /home/ubuntu/profiling_test/test_c_profiling
start_workload go /home/ubuntu/profiling_test/test_go_profiling
# 阶段四：带 --perf-basic-prof 的 Node（ready 场景）；无 flag 场景由
# node-plain Session 覆盖（复制的解释器路径，避免与 ready Session 选择器重叠）。
start_workload node node --perf-basic-prof /home/ubuntu/profiling_test/test_node_profiling.js
NODE_PLAIN_EXE="$TEST_ROOT/node3-e2e"
cp "$(readlink -f "$(command -v node)")" "$NODE_PLAIN_EXE"
chmod 0755 "$NODE_PLAIN_EXE"
start_workload node-plain "$NODE_PLAIN_EXE" /home/ubuntu/profiling_test/test_node_profiling.js
start_workload python "$PYTHON_WORKLOAD_EXE" /home/ubuntu/profiling_test/test_python_profiling.py
start_workload java java -cp /home/ubuntu/profiling_test TestJavaProfiling

create_session() {
    local index="$1"
    local name="multilang-$RUN_TAG-${NAMES[$index]}"
    local request="$TEST_ROOT/create-${NAMES[$index]}.json"
    local response="$TEST_ROOT/response-${NAMES[$index]}.json"
    python3 - "$request" "$name" "$TARGET_IP" "${EXES[$index]}" <<'PY'
import json, sys
path, name, target_ip, exe = sys.argv[1:]
body = {
    "name": name, "target_ip": target_ip,
    "hostname": "cloud-server-111-230-29-115",
    "scope": "process", "selector_exe": exe, "selector_mode": "all_instances",
    "signals": ["cpu_profile", "io_latency", "io_syscall_latency", "sched_latency"],
    "sample_rate_hz": 9, "aggregation_window_sec": 5, "upload_batch_sec": 15,
    "retention_hours": 1, "continuity_mode": "strict", "allow_degraded": True,
    "labels": {"acceptance": "multilang-process", "run": name},
}
json.dump(body, open(path, "w", encoding="utf-8"), separators=(",", ":"))
PY
    local code
    code="$(curl -sS -o "$response" -w '%{http_code}' -X POST "$BASE/api/v1/continuous/sessions" "${api_headers[@]}" -H 'Content-Type: application/json' -d "@$request")"
    [[ "$code" == "200" ]] || { cat "$response" >&2; fail "create ${NAMES[$index]} Session HTTP $code"; }
    local sid
    sid="$(python3 - "$response" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
if d.get("code") != 0:
    raise SystemExit(d.get("message", "create failed"))
print(d["data"]["session"]["sid"])
PY
)"
    [[ -n "$sid" ]] || fail "create ${NAMES[$index]} returned empty SID"
    SIDS+=("$sid")
    printf '[session] name=%s sid=%s selector=%s\n' "${NAMES[$index]}" "$sid" "${EXES[$index]}"
}

for index in "${!PIDS[@]}"; do create_session "$index"; done
pass "created ${#SIDS[@]} process Sessions"

get_session_json() {
    curl -fsS "$BASE/api/v1/continuous/sessions/$1" "${api_headers[@]}"
}

# 阶段四修复：把"所有 Session 已 attach（running/degraded 且带 PID）"的判定
# 提取为单一函数，消除启动等待与 Agent 重启恢复两处重复嵌套的循环体。
sessions_attached() {
    local ready=0
    local index payload state active
    for index in "${!SIDS[@]}"; do
        payload="$(get_session_json "${SIDS[$index]}")"
        state="$(printf '%s' "$payload" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["session"].get("observed_state",""))')"
        active="$(printf '%s' "$payload" | python3 -c 'import base64,json,sys; x=json.load(sys.stdin)["data"]["session"].get("active_processes",""); print(base64.b64decode(x).decode() if isinstance(x,str) else json.dumps(x))')"
        [[ "$state" == "running" || "$state" == "degraded" ]] || ready=1
        grep -q '"pid":' <<<"$active" || ready=1
        printf '[state] %s sid=%s observed=%s active=%s\n' "${NAMES[$index]}" "${SIDS[$index]}" "$state" "$active" >&2
    done
    return "$ready"
}

wait_for_assignments() {
    local deadline=$((SECONDS + 30))
    while (( SECONDS < deadline )); do
        sessions_attached && return 0
        sleep 2
    done
    return 1
}

wait_for_assignments || fail "Agent did not attach all process Sessions"
pass "Agent attached all selected exe instances"

for ((elapsed=0; elapsed<WAIT_DATA_SEC; elapsed+=4)); do
    if (( elapsed % 8 == 0 )); then
        printf '[monitor] elapsed=%ss ' "$elapsed"
        df -P / | tail -1
        docker stats --no-stream --format '{{.Name}} {{.CPUPerc}} {{.MemUsage}}' 2>/dev/null | grep -E 'drop_agent|apiserver|minio|postgres' || true
    fi
    sleep 4
done

PSQL=(docker exec mini-drop-postgres-1 psql -U postgres -d drop -At)

wait_for_valid_data() {
    local deadline=$((SECONDS + WAIT_VALID_DATA_SEC))
    while (( SECONDS < deadline )); do
        local ready=1
        for index in "${!SIDS[@]}"; do
            local row batches windows cpu_batches
            row="$("${PSQL[@]}" -F '|' -c "select count(*), coalesce(sum(window_count),0), count(*) filter (where signal_types @> '[\"cpu_profile\"]'::jsonb) from profile_batches where session_sid='${SIDS[$index]}';")"
            IFS='|' read -r batches windows cpu_batches <<<"$row"
            printf '[data-wait] %s sid=%s batches=%s windows=%s cpu_batches=%s\n' \
                "${NAMES[$index]}" "${SIDS[$index]}" "${batches:-0}" "${windows:-0}" "${cpu_batches:-0}" >&2
            [[ "${batches:-0}" -gt 0 && "${windows:-0}" -gt 0 && "${cpu_batches:-0}" -gt 0 ]] || ready=0
        done
        (( ready == 1 )) && return 0
        sleep 3
    done
    return 1
}

wait_for_valid_data || fail "not every process Session produced a valid CPU batch before timeout"
pass "all Sessions produced at least one valid CPU batch"

validate_session_data() {
    local index="$1"
    local sid="${SIDS[$index]}"
    local exe="${EXES[$index]}"
    local pid="${PIDS[$index]}"
    local start="${START_MS[$index]}"
    local row
    row="$("${PSQL[@]}" -F '|' -c "select count(*), coalesce(sum(window_count),0), count(*) filter (where signal_types @> '[\"cpu_profile\"]'::jsonb), coalesce(string_agg(distinct signal_types::text, ','),''), coalesce(string_agg(distinct selected_backend, ','),'') from profile_batches where session_sid='$sid';")"
    local batches windows cpu_batches signals backends
    IFS='|' read -r batches windows cpu_batches signals backends <<<"$row"
    [[ "${batches:-0}" -gt 0 ]] || fail "${NAMES[$index]} has no ProfileBatch"
    [[ "${windows:-0}" -gt 0 ]] || fail "${NAMES[$index]} has no profile windows"
    [[ "${cpu_batches:-0}" -gt 0 ]] || fail "${NAMES[$index]} has no batch with CPU samples"
    [[ -n "$backends" ]] || fail "${NAMES[$index]} batch has no backend"

    local matched=0 non_target=0 identity_mismatch=0 missing_identity=0
    while IFS= read -r key; do
        [[ -n "$key" ]] || continue
        local object="$TEST_ROOT/object-$index-$matched.json"
        curl -fsS -G "$BASE/api/v1/continuous/raw" "${api_headers[@]}" --data-urlencode "key=$key" -o "$object"
        read -r m n i z <<<"$(python3 - "$object" "$exe" "$pid" "$start" <<'PY'
import json, sys
path, expected_exe, expected_pid, expected_start = sys.argv[1:]
expected_pid, expected_start = int(expected_pid), int(expected_start)
body = json.load(open(path, encoding="utf-8"))
matched = non_target = identity_mismatch = missing_identity = 0
for window in body.get("windows", []):
    records = list(window.get("samples", [])) + list(window.get("metrics", []))
    for profile in window.get("profiles", []):
        records.extend(profile.get("samples", []))
    for record in records:
        exe = record.get("exe", "")
        if exe != expected_exe:
            non_target += 1
            continue
        matched += 1
        process_start = int(record.get("process_start_ms", 0) or 0)
        if process_start <= 0:
            missing_identity += 1
        elif record.get("pid") == expected_pid and process_start != expected_start:
            identity_mismatch += 1
print(matched, non_target, identity_mismatch, missing_identity)
PY
)"
        matched=$((matched + m))
        non_target=$((non_target + n))
        identity_mismatch=$((identity_mismatch + i))
        missing_identity=$((missing_identity + z))
    done < <("${PSQL[@]}" -c "select object_key from profile_batches where session_sid='$sid' order by end_time;")
    [[ "$matched" -gt 0 ]] || fail "${NAMES[$index]} has no samples for selector exe"
    [[ "$non_target" -eq 0 ]] || fail "${NAMES[$index]} contains $non_target non-target samples"
    [[ "$identity_mismatch" -eq 0 ]] || fail "${NAMES[$index]} contains PID/start-time identity mismatch"
    [[ "$missing_identity" -eq 0 ]] || fail "${NAMES[$index]} contains $missing_identity samples without process_start_ms"
    printf '[data] %s sid=%s batches=%s windows=%s cpu_batches=%s matched_samples=%s signals=%s backends=%s\n' \
        "${NAMES[$index]}" "$sid" "$batches" "$windows" "$cpu_batches" "$matched" "$signals" "$backends"
}

for index in "${!SIDS[@]}"; do validate_session_data "$index"; done
pass "all Session objects contain only their selected executable"

# ============================================================
# 阶段 4 质量门禁：真实业务函数、v2 语言状态、runtime 筛选一致性、
# 窗口幂等。任何一条失败都阻止验收（部署 TEST_SCOPE=full 时阻断部署）。
# ============================================================

# 每个 Session 期望的 runtime 分类与业务热点函数。
EXPECT_RUNTIMES=(native go node node python java)
HOT_FUNCS=("hot_a|hot_b" "main.hotA|main.hotB|main.main" "hotA|hotB" "-" "hot_a|hot_b" "TestJavaProfiling.hotA|hotA")
NODE_PLAIN_INDEX=3

api_from_to() {
    API_FROM="$(date -u -d "15 minutes ago" +%Y-%m-%dT%H:%M:%SZ)"
    API_TO="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}

fetch_topn() {
    local sid="$1"; shift
    curl -fsS -G "$BASE/api/v1/profile/topn" "${api_headers[@]}" \
        --data-urlencode "session_sid=$sid" \
        --data-urlencode "profile_type=cpu" \
        --data-urlencode "host=$TARGET_IP" \
        --data-urlencode "from=$API_FROM" --data-urlencode "to=$API_TO" "$@"
}

phase4_wait_for_hot_function() {
    local index="$1"
    local sid="${SIDS[$index]}"
    local deadline=$((SECONDS + WAIT_VALID_DATA_SEC))
    while (( SECONDS < deadline )); do
        api_from_to
        local topn
        if topn="$(fetch_topn "$sid")"; then
            if python3 - "$topn" "${HOT_FUNCS[$index]}" <<'PY'
import json, sys
data = json.loads(sys.argv[1]).get("data") or {}
patterns = [p for p in sys.argv[2].split("|") if p != "-"]
names = []
def walk(nodes):
    for node in nodes:
        names.append(node.get("name", ""))
        walk(node.get("children") or [])
walk(data.get("items") or [])
ok = any(any(p in n for p in patterns) for n in names)
raise SystemExit(0 if ok else 1)
PY
            then
                return 0
            fi
        fi
        sleep 4
    done
    return 1
}

for index in "${!SIDS[@]}"; do
    [[ "${HOT_FUNCS[$index]}" == "-" ]] && continue
    if phase4_wait_for_hot_function "$index"; then
        pass "${NAMES[$index]} TopN shows real business hot function (${HOT_FUNCS[$index]})"
    else
        fail "${NAMES[$index]} TopN never showed business hot function (${HOT_FUNCS[$index]})"
    fi
done

assert_language_status() {
    local index="$1"
    local sid="${SIDS[$index]}"
    local expect_runtime="${EXPECT_RUNTIMES[$index]}"
    local deadline=$((SECONDS + WAIT_VALID_DATA_SEC))
    local topn
    while (( SECONDS < deadline )); do
        api_from_to
        topn="$(fetch_topn "$sid" || true)"
        if RUNTIME_JSON="$topn" EXPECT_RUNTIME="$expect_runtime" SESSION_NAME="${NAMES[$index]}" python3 <<'PY'
import json, os, sys
try:
    data = (json.loads(os.environ["RUNTIME_JSON"]) or {}).get("data") or {}
except (json.JSONDecodeError, TypeError):
    raise SystemExit(1)
expected = os.environ["EXPECT_RUNTIME"]
name = os.environ["SESSION_NAME"]
diagnostics = data.get("runtime_diagnostics") or {}
entry = diagnostics.get(expected)
if entry is None:
    print(f"FAIL {name}: runtime_diagnostics has no '{expected}' row: keys={list(diagnostics)}")
    raise SystemExit(1)
version = entry.get("diagnostics_version", 0)
if version < 2:
    print(f"FAIL {name}: {expected} diagnostics missing v2 contract (diagnostics_version={version})")
    raise SystemExit(1)
collector = entry.get("collector_status", "")
if name == "node-plain":
    reasons = " ".join(entry.get("reasons") or [])
    if collector != "missing" or "--perf-basic-prof" not in reasons:
        print(f"FAIL node-plain must report missing --perf-basic-prof, got status={collector} reasons={entry.get('reasons')}")
        raise SystemExit(1)
    print(f"OK node-plain: {expected} v2 status={collector} reasons={entry.get('reasons')}")
    raise SystemExit(0)

semantic_sample = float(entry.get("semantic_sample_percent") or 0)
unresolved = float(entry.get("unresolved_frame_percent") or 0)
samples = float(entry.get("sample_count") or 0)
if collector != "ready" or samples <= 0 or semantic_sample < 70:
    print(f"WAIT {name}: status={collector} semantic_sample={semantic_sample}% samples={samples} reasons={entry.get('reasons')}")
    raise SystemExit(1)
if expected == "native":
    target_unresolved = float(entry.get("target_module_unresolved_percent") or 0)
    target_frames = float(entry.get("target_module_frame_weight") or 0)
    if target_frames <= 0 or target_unresolved >= 5:
        print(f"WAIT {name}: native target-module frames={target_frames} unresolved={target_unresolved}% (want frames>0 and unresolved<5%)")
        raise SystemExit(1)
elif unresolved > 20:
    print(f"WAIT {name}: unresolved={unresolved}% (want <=20%)")
    raise SystemExit(1)
print(f"OK {name}: {expected} v2 ready semantic_sample={semantic_sample}% semantic_frame={entry.get('semantic_frame_percent')}% unresolved={unresolved}% target_unresolved={entry.get('target_module_unresolved_percent')}% samples={samples}")
PY
        then
            return 0
        fi
        sleep 4
    done
    fail "${NAMES[$index]} language status did not reach the production quality gate"
}
for index in "${!SIDS[@]}"; do assert_language_status "$index"; done
pass "all Sessions expose truthful v2 language status"

assert_runtime_filter_consistency() {
    local index="$1"
    local sid="${SIDS[$index]}"
    local expect_runtime="${EXPECT_RUNTIMES[$index]}"
    api_from_to
    # runtime filter 命中样本
    local filtered
    filtered="$(curl -fsS -G "$BASE/api/v1/profile/topn" "${api_headers[@]}" \
        --data-urlencode "session_sid=$sid" --data-urlencode "profile_type=cpu" \
        --data-urlencode "host=$TARGET_IP" \
        --data-urlencode "from=$API_FROM" --data-urlencode "to=$API_TO" \
        --data-urlencode 'filters={"runtime":"'"$expect_runtime"'"}' || true)"
    # label-values 与 filter 口径一致
    local label_values
    label_values="$(curl -fsS -G "$BASE/api/v1/profile/label-values" "${api_headers[@]}" \
        --data-urlencode "session_sid=$sid" --data-urlencode "label=runtime" \
        --data-urlencode "host=$TARGET_IP" \
        --data-urlencode "from=$API_FROM" --data-urlencode "to=$API_TO" || true)"
    FILTERED_JSON="$filtered" LABEL_JSON="$label_values" EXPECT_RUNTIME="$expect_runtime" SESSION_NAME="${NAMES[$index]}" python3 <<'PY'
import json, os, sys
filtered = (json.loads(os.environ["FILTERED_JSON"]) or {}).get("data") or {}
label = (json.loads(os.environ["LABEL_JSON"]) or {}).get("data") or {}
expected = os.environ["EXPECT_RUNTIME"]
name = os.environ["SESSION_NAME"]
if float(filtered.get("total") or 0) <= 0:
    print(f"FAIL {name}: runtime filter runtime={expected} matched no samples")
    raise SystemExit(1)
values = label.get("values") or []
if expected not in values:
    print(f"FAIL {name}: label-values runtime={values} missing {expected} (filter/label mismatch)")
    raise SystemExit(1)
print(f"OK {name}: runtime={expected} filter total={filtered.get('total')} matches label-values {values}")
PY
    [[ $? -eq 0 ]] || fail "${NAMES[$index]} runtime filter gate failed"
}
for index in "${!SIDS[@]}"; do assert_runtime_filter_consistency "$index"; done
pass "runtime filters agree with label-values for every Session"

assert_window_idempotency() {
    local index="$1"
    local sid="${SIDS[$index]}"
    local row
    row="$("${PSQL[@]}" -F '|' -c "select count(*), count(distinct window_id) from profile_windows where session_sid='$sid' and signal_type='cpu_profile';")"
    IFS='|' read -r total distinct <<<"$row"
    [[ "${total:-0}" -eq "${distinct:-0}" ]] || fail "${NAMES[$index]} has duplicate windows ($total rows vs $distinct window_ids)"
}
for index in "${!SIDS[@]}"; do assert_window_idempotency "$index"; done
pass "no duplicate windows for any Session"

# Optional recovery gate used by server acceptance runs. Restarting only the
# Agent container must not create replacement SIDs or turn the user's desired
# running Sessions into stopped tasks; the cached assignments should reattach
# to the same PID/start-time identities and continue uploading.
if [[ "${RESTART_AGENT:-0}" == "1" ]]; then
    printf '[recovery] restarting drop_agent while all process Sessions remain active\n'
    # Preserve the running container's spool reserve during recreation. Server
    # acceptance may deliberately use a lower temporary reserve on a nearly
    # full test host; silently reverting to Compose's 2 GiB default would test
    # backpressure instead of restart recovery.
    current_spool_min="$(docker inspect mini-drop-drop_agent-1 --format '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^DROP_NATIVE_CP_SPOOL_MIN_FREE_BYTES=//p' | head -n 1)"
    [[ "$current_spool_min" =~ ^[0-9]+$ ]] || fail "cannot read current Agent spool reserve"
    (cd /home/ubuntu/mini-drop && DROP_NATIVE_CP_SPOOL_MIN_FREE_BYTES="$current_spool_min" docker compose up -d --no-deps --no-build --force-recreate drop_agent >/tmp/continuous-e2e-agent-restart.log 2>&1)
    recovery_deadline=$((SECONDS + 45))
    recovered=0
    while (( SECONDS < recovery_deadline )); do
        if sessions_attached; then
            recovered=1
            break
        fi
        sleep 2
    done
    [[ "$recovered" -eq 1 ]] || fail "Agent restart did not reattach all existing process Sessions"
    sleep 12
    for index in "${!SIDS[@]}"; do
        row="$("${PSQL[@]}" -F '|' -c "select count(*), coalesce(sum(window_count),0) from profile_batches where session_sid='${SIDS[$index]}';")"
        IFS='|' read -r batches windows <<<"$row"
        [[ "${batches:-0}" -gt 0 && "${windows:-0}" -gt 0 ]] || fail "${NAMES[$index]} produced no data after Agent restart"
    done
    pass "Agent restart reattached the same process Sessions and continued uploading"
fi

old_pid="${PIDS[0]}"
old_start="${START_MS[0]}"
kill -TERM "$old_pid" 2>/dev/null || true
wait "$old_pid" 2>/dev/null || true
nice -n 10 /home/ubuntu/profiling_test/test_c_profiling >/dev/null 2>&1 &
new_pid=$!
sleep 1
kill -0 "$new_pid" 2>/dev/null || fail "restarted C workload exited"
new_start="$(process_start_ms "$new_pid")"
new_exe="$(readlink -f "/proc/$new_pid/exe")"
PIDS[0]="$new_pid"
START_MS[0]="$new_start"
[[ "$new_exe" == "${EXES[0]}" ]] || fail "restarted workload executable changed"
[[ "$new_pid" != "$old_pid" || "$new_start" != "$old_start" ]] || fail "restarted workload kept same identity"

# 共享物理采集器在目标变化时需要重建（等待新引擎首个不可变段解析完成），
# 六个 Session 并行且 Agent 高负载时可能超过 20 秒，放宽到 60 秒。
restart_deadline=$((SECONDS + 60))
seen_new=0
while (( SECONDS < restart_deadline )); do
    payload="$(get_session_json "${SIDS[0]}")"
    active="$(printf '%s' "$payload" | python3 -c 'import base64,json,sys; x=json.load(sys.stdin)["data"]["session"].get("active_processes",""); print(base64.b64decode(x).decode() if isinstance(x,str) else json.dumps(x))')"
    if python3 -c '
import json, sys
active = json.loads(sys.stdin.read() or "[]")
new_pid, old_pid, new_start = map(int, sys.argv[1:])
new_matches = [p for p in active if int(p.get("pid", 0)) == new_pid and int(p.get("process_start_ms", 0)) == new_start]
old_matches = [p for p in active if int(p.get("pid", 0)) == old_pid]
raise SystemExit(0 if new_matches and not old_matches else 1)
' "$new_pid" "$old_pid" "$new_start" <<<"$active"; then
        seen_new=1
        break
    fi
    sleep 2
done
[[ "$seen_new" -eq 1 ]] || fail "same SID did not follow restarted process"
printf '[restart] sid=%s old_pid=%s old_start=%s new_pid=%s new_start=%s\n' "${SIDS[0]}" "$old_pid" "$old_start" "$new_pid" "$new_start"
pass "same SID followed a restarted process"

new_pid_samples=0
restart_data_deadline=$((SECONDS + WAIT_VALID_DATA_SEC))
while (( SECONDS < restart_data_deadline )); do
    new_pid_samples=0
    while IFS= read -r key; do
        [[ -n "$key" ]] || continue
        object="$TEST_ROOT/restarted-c.json"
        curl -fsS -G "$BASE/api/v1/continuous/raw" "${api_headers[@]}" --data-urlencode "key=$key" -o "$object"
        found="$(python3 - "$object" "${EXES[0]}" "$new_pid" "$new_start" <<'PY'
import json, sys
path, expected_exe, expected_pid, expected_start = sys.argv[1:]
expected_pid, expected_start = int(expected_pid), int(expected_start)
body = json.load(open(path, encoding="utf-8"))
count = 0
for window in body.get("windows", []):
    records = list(window.get("samples", [])) + list(window.get("metrics", []))
    for profile in window.get("profiles", []):
        records.extend(profile.get("samples", []))
    for record in records:
        if (record.get("exe") == expected_exe and int(record.get("pid", 0)) == expected_pid
                and int(record.get("process_start_ms", 0) or 0) == expected_start):
            count += 1
print(count)
PY
        )"
        new_pid_samples=$((new_pid_samples + found))
    done < <("${PSQL[@]}" -c "select object_key from profile_batches where session_sid='${SIDS[0]}' order by end_time;")
    (( new_pid_samples > 0 )) && break
    sleep 3
done
[[ "$new_pid_samples" -gt 0 ]] || fail "same SID has no stored sample for restarted PID/start-time"
pass "same SID stored samples for the restarted PID/start-time"

for sid in "${SIDS[@]}"; do stop_session "$sid"; done
stop_deadline=$((SECONDS + WAIT_STOP_SEC))
stopped=0
while (( SECONDS < stop_deadline )); do
    stopped=1
    for sid in "${SIDS[@]}"; do
        state="$(get_session_json "$sid" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["session"].get("observed_state",""))')"
        [[ "$state" == "stopped" ]] || stopped=0
    done
    (( stopped == 1 )) && break
    sleep 2
done
[[ "$stopped" -eq 1 ]] || fail "Sessions were not acknowledged after final batch"
for sid in "${SIDS[@]}"; do
    row="$("${PSQL[@]}" -F '|' -c "select desired_state,observed_state,stopped_at from continuous_sessions where sid='$sid';")"
    IFS='|' read -r desired observed stopped_at <<<"$row"
    [[ "$desired" == "stopped" && "$observed" == "stopped" && -n "$stopped_at" ]] || fail "Session $sid final DB state is $row"
done
pass "all Sessions stopped after final spool acknowledgement"
printf '[e2e] completed run=%s sessions=%s\n' "$RUN_TAG" "${#SIDS[@]}"
