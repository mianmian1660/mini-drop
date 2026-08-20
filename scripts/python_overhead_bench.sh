#!/bin/bash

# Server-side performance gates for Python continuous profiling.

set -euo pipefail

AGENT="${AGENT:-mini-drop-drop_agent-1}"
PYSPY_MAX_SLOWDOWN="${PYSPY_MAX_SLOWDOWN:-5}"
MEMRAY_MAX_SLOWDOWN="${MEMRAY_MAX_SLOWDOWN:-20}"
AGENT_MAX_CPU="${AGENT_MAX_CPU:-15}"
WORK="$(mktemp -d /tmp/python_overhead_bench.XXXXXX)"
PREFIX="md-python-overhead-$$"
declare -a CONTAINERS

cleanup() {
    for name in "${CONTAINERS[@]:-}"; do
        docker rm -f "$name" >/dev/null 2>&1 || true
    done
    rm -rf "$WORK"
}
trap cleanup EXIT

measure_agent_cpu() {
    local samples="$1"
    for _ in $(seq 1 "$samples"); do
        docker stats --no-stream --format '{{.CPUPerc}}' "$AGENT" | tr -d '%'
        sleep 2
    done | awk '{sum+=$1; n++} END {printf "%.2f", sum/n}'
}

# Capture a clean baseline before any temporary Python process can enter the
# Agent's previous-window fallback queue.
agent_baseline_cpu="$(measure_agent_cpu 30)"

cat >"$WORK/throughput.py" <<'PY'
import pathlib
import time


def pythonThroughputWorker(seconds):
    deadline = time.perf_counter() + seconds
    count = 0
    value = 1
    while time.perf_counter() < deadline:
        for _ in range(2000):
            value = (value * 1664525 + 1013904223) & 0xFFFFFFFF
            value ^= value >> 13
        count += 2000
    return count


start = pathlib.Path("/bench/start")
while not start.exists():
    time.sleep(0.01)
print(pythonThroughputWorker(8), flush=True)
PY

run_pyspy_round() {
    local mode="$1"
    local round="$2"
    local name="$PREFIX-pyspy-$mode-$round"
    local output="/tmp/$name.raw"
    CONTAINERS+=("$name")
    rm -f "$WORK/start"
    docker run -d --name "$name" -v "$WORK:/bench" python:3.11-slim python /bench/throughput.py >/dev/null
    local host_pid
    host_pid="$(docker inspect "$name" --format '{{.State.Pid}}')"
    if [[ "$mode" == "sampled" ]]; then
        docker exec -d "$AGENT" py-spy record --pid "$host_pid" --rate 19 --duration 9 \
            --format raw --function --output "$output"
        sleep 1
    fi
    touch "$WORK/start"
    docker wait "$name" >/dev/null
    local count
    count="$(docker logs "$name" | tail -1)"
    if [[ "$mode" == "sampled" ]]; then
        sleep 1
        docker exec "$AGENT" grep -q pythonThroughputWorker "$output"
        docker exec "$AGENT" rm -f "$output"
    fi
    docker rm "$name" >/dev/null
    echo "$count"
}

baseline_total=0
sampled_total=0
for round in 1 2 3; do
    baseline_total=$((baseline_total + $(run_pyspy_round baseline "$round")))
    sampled_total=$((sampled_total + $(run_pyspy_round sampled "$round")))
done
pyspy_slowdown="$(awk -v base="$baseline_total" -v sampled="$sampled_total" 'BEGIN { printf "%.2f", (base-sampled)*100/base }')"
echo "PYSPY baseline_ops=$baseline_total sampled_ops=$sampled_total slowdown_pct=$pyspy_slowdown"

cat >"$WORK/memray_bench.py" <<'PY'
import sys
import time

if sys.argv[1] == "enabled":
    import mini_drop_memray
    mini_drop_memray.start(interval_seconds=60)
    time.sleep(0.2)


def memrayApplicationWorker(seconds):
    deadline = time.perf_counter() + seconds
    slots = [None] * 4096
    count = 0
    value = 1
    while time.perf_counter() < deadline:
        for _ in range(128):
            value = (value * 1664525 + 1013904223) & 0xFFFFFFFF
            value ^= value >> 13
        slots[count & 4095] = bytearray(256)
        count += 1
    return count


def memrayAllocationStressWorker(seconds):
    deadline = time.perf_counter() + seconds
    slots = [None] * 4096
    count = 0
    while time.perf_counter() < deadline:
        slots[count & 4095] = bytearray(256)
        count += 1
    return count


print(memrayApplicationWorker(8), memrayAllocationStressWorker(4), flush=True)
PY

cat >"$WORK/Dockerfile" <<'DOCKER'
FROM python:3.11-slim
RUN pip install --no-cache-dir -i https://mirrors.cloud.tencent.com/pypi/simple memray==1.20.0
DOCKER
docker build -q -t "$PREFIX-memray" "$WORK" >/dev/null

run_memray_round() {
    local mode="$1"
    docker run --rm -e PYTHONPATH=/opt/sdk -v "$(pwd)/integrations/mini_drop_memray:/opt/sdk:ro" \
        -v "$WORK:/bench:ro" "$PREFIX-memray" python /bench/memray_bench.py "$mode"
}

memray_baseline_total=0
memray_enabled_total=0
memray_stress_baseline_total=0
memray_stress_enabled_total=0
for round in 1 2 3; do
    read -r application stress <<<"$(run_memray_round disabled)"
    memray_baseline_total=$((memray_baseline_total + application))
    memray_stress_baseline_total=$((memray_stress_baseline_total + stress))
    read -r application stress <<<"$(run_memray_round enabled)"
    memray_enabled_total=$((memray_enabled_total + application))
    memray_stress_enabled_total=$((memray_stress_enabled_total + stress))
done
memray_slowdown="$(awk -v base="$memray_baseline_total" -v enabled="$memray_enabled_total" 'BEGIN { printf "%.2f", (base-enabled)*100/base }')"
memray_stress_slowdown="$(awk -v base="$memray_stress_baseline_total" -v enabled="$memray_stress_enabled_total" 'BEGIN { printf "%.2f", (base-enabled)*100/base }')"
echo "MEMRAY baseline_ops=$memray_baseline_total enabled_ops=$memray_enabled_total slowdown_pct=$memray_slowdown"
echo "MEMRAY_ALLOCATION_STRESS baseline_allocs=$memray_stress_baseline_total enabled_allocs=$memray_stress_enabled_total slowdown_pct=$memray_stress_slowdown"

cat >"$WORK/hot.py" <<'PY'
def pythonAgentCpuWorker():
    value = 1
    while True:
        value = (value * 1664525 + 1013904223) & 0xFFFFFFFF
        value ^= value >> 13


pythonAgentCpuWorker()
PY

for index in 1 2 3 4; do
    name="$PREFIX-agent-$index"
    CONTAINERS+=("$name")
    docker run -d --name "$name" -v "$WORK:/bench:ro" python:3.11-slim python /bench/hot.py >/dev/null
done
sleep 28
agent_loaded_cpu="$(measure_agent_cpu 30)"
agent_increment_cpu="$(awk -v baseline="$agent_baseline_cpu" -v loaded="$agent_loaded_cpu" 'BEGIN { value=loaded-baseline; if (value < 0) value=0; printf "%.2f", value }')"
echo "AGENT baseline_avg_cpu_pct=$agent_baseline_cpu four_fallback_processes_avg_cpu_pct=$agent_loaded_cpu incremental_cpu_points=$agent_increment_cpu"

failed=0
awk -v actual="$pyspy_slowdown" -v limit="$PYSPY_MAX_SLOWDOWN" 'BEGIN { exit !(actual <= limit) }' || failed=1
awk -v actual="$memray_slowdown" -v limit="$MEMRAY_MAX_SLOWDOWN" 'BEGIN { exit !(actual <= limit) }' || failed=1
awk -v actual="$agent_increment_cpu" -v limit="$AGENT_MAX_CPU" 'BEGIN { exit !(actual <= limit) }' || failed=1
exit "$failed"
