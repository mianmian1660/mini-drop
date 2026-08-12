#!/usr/bin/env bash
# Quick local verification for the Mini-Drop + Parca single-host loop.
set -euo pipefail

API_URL="${API_URL:-http://127.0.0.1:8191}"
TARGET_ID="${TARGET_ID:-127.0.0.1:hotmethod}"
PROFILE_TYPE="${PROFILE_TYPE:-cpu}"

echo "[parca-smoke] checking Mini-Drop profile targets at ${API_URL}"
curl -fsS "${API_URL}/api/v1/profile/targets" | sed 's/.*/[parca-smoke] targets response received/'

echo "[parca-smoke] creating a short CPU workload"
timeout 20s bash -c 'while :; do :; done' >/dev/null 2>&1 || true

from="$(date -u -d '30 minutes ago' +%Y-%m-%dT%H:%M:%SZ)"
to="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
query="${API_URL}/api/v1/profile/topn?target_id=${TARGET_ID}&profile_type=${PROFILE_TYPE}&from=${from}&to=${to}"

echo "[parca-smoke] querying TopN"
curl -fsS "$query"
echo
echo "[parca-smoke] open Mini-Drop, enter host ${TARGET_ID}, then use the 持续 profiling tab."
