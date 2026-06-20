#!/usr/bin/env bash
set -euo pipefail

BASE="${BASE:-http://127.0.0.1:8191}"
TARGET_IP="${TARGET_IP:-127.0.0.1}"

pass() { printf 'PASS %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1" >&2; exit 1; }

json_get() {
  python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"
}

echo "[e2e] base=$BASE target=$TARGET_IP"

curl -fsS "$BASE/healthz" >/tmp/mini-drop-e2e-health.json
grep -q '"status":"ok"' /tmp/mini-drop-e2e-health.json || fail "healthz"
pass "healthz"

agents_resp="$(curl -fsS "$BASE/api/v1/agents")"
agent_count="$(printf '%s' "$agents_resp" | json_get "len(d['data']['agents'])")"
test "$agent_count" -ge 1 || fail "agent list has online/discovered agent"
agent_ip="$(printf '%s' "$agents_resp" | json_get "d['data']['agents'][0]['ip_addr']")"
pass "agent list"

agent_detail_resp="$(curl -fsS "$BASE/api/v1/agent/detail?ip=$agent_ip")"
heartbeat_sec="$(printf '%s' "$agent_detail_resp" | json_get "d['data']['heartbeat_interval_sec']")"
offline_sec="$(printf '%s' "$agent_detail_resp" | json_get "d['data']['offline_after_sec']")"
agent_detail_audits="$(printf '%s' "$agent_detail_resp" | json_get "len(d['data'].get('audits', []))")"
test "$heartbeat_sec" = "5" || fail "agent detail heartbeat policy"
test "$offline_sec" = "30" || fail "agent detail offline policy"
test "$agent_detail_audits" -le 10 || fail "agent detail keeps latest 10 audits"
pass "agent detail heartbeat policy"

create_resp="$(curl -fsS -X POST "$BASE/api/v1/tasks" \
  -H 'Content-Type: application/json' \
  -H 'Drop_user_uid: e2e' \
  -H 'Drop_user_name: e2e' \
  -d "{\"name\":\"e2e smoke cpu\",\"task_type\":0,\"profiler_type\":0,\"target_ip\":\"$TARGET_IP\",\"target_pid\":0,\"duration\":2,\"frequency\":49,\"callgraph\":\"fp\",\"event\":\"cpu-cycles\"}")"
tid="$(printf '%s' "$create_resp" | json_get "d['data']['tid']")"
test -n "$tid" || fail "create normal task"
pass "create normal task $tid"

detail_resp="$(curl -fsS "$BASE/api/v1/tasks/$tid")"
event_count="$(printf '%s' "$detail_resp" | json_get "len(d['data'].get('status_events', []))")"
test "$event_count" -ge 1 || fail "task status event recorded"
pass "task status event recorded"

bad_code="$(curl -s -o /tmp/mini-drop-e2e-bad.json -w '%{http_code}' -X POST "$BASE/api/v1/tasks" \
  -H 'Content-Type: application/json' \
  -d "{\"target_ip\":\"$TARGET_IP\"}")"
test "$bad_code" = "400" || fail "invalid create returns 400"
pass "invalid create returns 400"

missing_code="$(curl -s -o /tmp/mini-drop-e2e-missing.json -w '%{http_code}' "$BASE/api/v1/tasks/tid-not-exist")"
test "$missing_code" = "404" || fail "missing task returns 404"
pass "missing task returns 404"

audits_resp="$(curl -fsS "$BASE/api/v1/agents/audits?limit=20")"
audit_total="$(printf '%s' "$audits_resp" | json_get "d['data']['total']")"
test "$audit_total" -ge 1 || fail "agent audit recorded"
pass "agent audit recorded"

echo "[e2e] completed"
