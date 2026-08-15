#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8191}"
AUTH_UID="${DROP_NATIVE_CP_UID:-agent-001}"
TARGET_IP="${TARGET_IP:-127.0.0.1}"
WAIT_SECONDS="${WAIT_SECONDS:-75}"

echo "[native-cp-smoke] starting apiserver and drop_agent with Native CP enabled"
DROP_NATIVE_CP_ENABLED=true docker compose up -d --build apiserver drop_agent

echo "[native-cp-smoke] waiting for apiserver ${BASE_URL}"
for _ in $(seq 1 60); do
  if curl -fsS "${BASE_URL}/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl -fsS "${BASE_URL}/healthz" >/dev/null

echo "[native-cp-smoke] waiting ${WAIT_SECONDS}s for a Native CP upload batch"
sleep "${WAIT_SECONDS}"

targets_json="$(curl -fsS -H "Drop-User-Uid: ${AUTH_UID}" "${BASE_URL}/api/v1/profile/targets")"
target_id="$(TARGET_IP="${TARGET_IP}" TARGETS_JSON="${targets_json}" python3 - <<'PY'
import json, os
body = json.loads(os.environ["TARGETS_JSON"])
targets = (body.get("data") or {}).get("targets") or []
target_ip = os.environ.get("TARGET_IP")
chosen = next((t for t in targets if t.get("ip") == target_ip), targets[0] if targets else None)
if chosen:
    print(chosen.get("id", ""))
PY
)"

if [[ -z "${target_id}" ]]; then
  echo "[native-cp-smoke] no profile target visible for uid=${AUTH_UID}; targets response:"
  echo "${targets_json}"
  exit 1
fi

from="$(python3 - <<'PY'
from datetime import datetime, timedelta, timezone
print((datetime.now(timezone.utc) - timedelta(minutes=5)).isoformat().replace("+00:00", "Z"))
PY
)"
to="$(python3 - <<'PY'
from datetime import datetime, timezone
print(datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"))
PY
)"

topn_json="$(curl -fsS -G -H "Drop-User-Uid: ${AUTH_UID}" \
  --data-urlencode "target_id=${target_id}" \
  --data-urlencode "from=${from}" \
  --data-urlencode "to=${to}" \
  --data-urlencode "profile_type=cpu" \
  "${BASE_URL}/api/v1/profile/topn")"

labels_json="$(curl -fsS -G -H "Drop-User-Uid: ${AUTH_UID}" \
  --data-urlencode "target_id=${target_id}" \
  --data-urlencode "from=${from}" \
  --data-urlencode "to=${to}" \
  --data-urlencode "profile_type=cpu" \
  --data-urlencode "label=comm" \
  "${BASE_URL}/api/v1/profile/label-values")"

TOPN_JSON="${topn_json}" python3 - <<'PY'
import json, os
body = json.loads(os.environ["TOPN_JSON"])
if body.get("code") not in (0, 200):
    raise SystemExit(f"topn API failed: {body}")
data = body.get("data") or {}
items = data.get("items") or []
print(f"[native-cp-smoke] topn empty={data.get('empty')} total={data.get('total')} items={len(items)} message={data.get('message', '')}")
PY

LABELS_JSON="${labels_json}" python3 - <<'PY'
import json, os
body = json.loads(os.environ["LABELS_JSON"])
if body.get("code") not in (0, 200):
    raise SystemExit(f"label-values API failed: {body}")
data = body.get("data") or {}
values = data.get("values") or []
print(f"[native-cp-smoke] comm labels available={data.get('available')} count={len(values)} values={values[:8]} message={data.get('message', '')}")
PY

echo "[native-cp-smoke] done. If results are empty, inspect: docker compose logs --tail=120 drop_agent apiserver"
