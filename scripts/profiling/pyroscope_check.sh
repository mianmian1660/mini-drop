#!/usr/bin/env bash
set -euo pipefail

PYROSCOPE_URL="${PYROSCOPE_URL:-http://localhost:4040}"
ALLOY_URL="${ALLOY_URL:-http://localhost:12345}"
COMPOSE_FILE="${COMPOSE_FILE:-deploy/profiling/pyroscope/docker-compose.yml}"
READY_ATTEMPTS="${READY_ATTEMPTS:-45}"
READY_DELAY_SEC="${READY_DELAY_SEC:-2}"

wait_ready() {
  local name="$1"
  local url="$2"
  local body=""

  for i in $(seq 1 "${READY_ATTEMPTS}"); do
    if body="$(curl -fsS "${url}" 2>&1)"; then
      echo "[pyroscope-check] ${name} is ready"
      return 0
    fi
    echo "[pyroscope-check] ${name} not ready (${i}/${READY_ATTEMPTS}): ${body}"
    sleep "${READY_DELAY_SEC}"
  done

  echo "[pyroscope-check] ${name} did not become ready: ${url}"
  return 1
}

echo "[pyroscope-check] compose file: ${COMPOSE_FILE}"
test -f "${COMPOSE_FILE}"

echo "[pyroscope-check] kernel: $(uname -r)"

if command -v docker >/dev/null 2>&1; then
  docker info --format '[pyroscope-check] docker kernel: {{.KernelVersion}}' 2>/dev/null || true
else
  echo "[pyroscope-check] docker command not found"
  exit 1
fi

echo "[pyroscope-check] containers:"
docker compose -f "${COMPOSE_FILE}" ps

echo "[pyroscope-check] pyroscope readiness: ${PYROSCOPE_URL}/ready"
wait_ready "pyroscope" "${PYROSCOPE_URL}/ready"

echo "[pyroscope-check] alloy endpoint: ${ALLOY_URL}"
wait_ready "alloy" "${ALLOY_URL}/-/ready"

echo "[pyroscope-check] discovered profile series:"
END_MS="$(date +%s%3N)"
START_MS="$((END_MS - 7200000))"
curl -fsS -X POST "${PYROSCOPE_URL}/querier.v1.QuerierService/Series" \
  -H "Content-Type: application/json" \
  -d "{\"matchers\":[],\"labelNames\":[\"service_name\",\"container\",\"compose_project\",\"project\",\"profiler\",\"__profile_type__\"],\"start\":${START_MS},\"end\":${END_MS}}"
echo

cat <<EOF
[pyroscope-check] open:
  Pyroscope UI: ${PYROSCOPE_URL}
  Alloy UI:     ${ALLOY_URL}

[pyroscope-check] In Pyroscope, choose profile type "process_cpu" and a container/service label.
EOF
