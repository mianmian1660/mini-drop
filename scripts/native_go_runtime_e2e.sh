#!/bin/bash

# End-to-end continuous profiling regression for native and Go binaries.
# Run on the profiling host after the candidate Agent/API are deployed.

set -euo pipefail

TARGET_ID="${TARGET_ID:-111.230.29.115:hotmethod}"
UID_HEADER="${UID_HEADER:-agent-001}"
WAIT_SEC="${WAIT_SEC:-155}"
WORK="$(mktemp -d /tmp/native_go_runtime_e2e.XXXXXX)"
declare -a PIDS
FAILED=0

ok() { echo "[native-go-e2e] PASS $*"; }
fail() { echo "[native-go-e2e] FAIL $*"; FAILED=1; }

cleanup() {
    for pid in "${PIDS[@]:-}"; do
        kill -TERM "$pid" 2>/dev/null || true
    done
    sleep 1
    for pid in "${PIDS[@]:-}"; do
        kill -KILL "$pid" 2>/dev/null || true
    done
    rm -rf "$WORK"
}
trap cleanup EXIT

require_cmd() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "missing required command: $1" >&2
        exit 2
    }
}

require_cmd go
require_cmd gcc
require_cmd g++
require_cmd curl
require_cmd python3

write_go_workload() {
    local path="$1"
    local function="$2"
    sed "s/WORKER_NAME/$function/g" >"$path" <<'GO'
package main

import "runtime"

//go:noinline
func WORKER_NAME(seed uint64) uint64 {
	value := seed | 1
	for i := uint64(0); i < 12000000; i++ {
		value = value*1664525 + 1013904223
		value ^= value >> 13
	}
	return value
}

func main() {
	var value uint64 = 1
	for {
		value = WORKER_NAME(value)
		runtime.KeepAlive(value)
	}
}
GO
}

write_go_workload "$WORK/go_normal.go" goNormalWorker
write_go_workload "$WORK/go_pie.go" goPieWorker
write_go_workload "$WORK/go_stripped.go" goStrippedWorker

go build -o "$WORK/go-normal" "$WORK/go_normal.go"
go build -buildmode=pie -o "$WORK/go-pie" "$WORK/go_pie.go"
go build -ldflags='-s -w' -o "$WORK/go-stripped" "$WORK/go_stripped.go"

"$WORK/go-normal" & PIDS+=("$!")
"$WORK/go-pie" & PIDS+=("$!")
"$WORK/go-stripped" & PIDS+=("$!")

cat >"$WORK/native.c" <<'C'
#include <stdint.h>

__attribute__((noinline)) uint64_t nativeCBurnWorker(uint64_t value) {
    for (uint64_t i = 0; i < 18000000; ++i) {
        value = value * 1103515245u + 12345u;
        value ^= value >> 11;
    }
    return value;
}

int main(void) {
    volatile uint64_t value = 1;
    for (;;) value = nativeCBurnWorker(value);
}
C

cat >"$WORK/native.cpp" <<'CPP'
#include <cstdint>

extern "C" __attribute__((noinline)) std::uint64_t nativeCppBurnWorker(std::uint64_t value) {
    for (std::uint64_t i = 0; i < 18000000; ++i) {
        value = value * 214013u + 2531011u;
        value ^= value >> 9;
    }
    return value;
}

int main() {
    volatile std::uint64_t value = 1;
    for (;;) value = nativeCppBurnWorker(value);
}
CPP

gcc -O1 -g -fno-omit-frame-pointer -o "$WORK/native-c" "$WORK/native.c"
g++ -O1 -g -fno-omit-frame-pointer -o "$WORK/native-cpp" "$WORK/native.cpp"
"$WORK/native-c" & PIDS+=("$!")
"$WORK/native-cpp" & PIDS+=("$!")

echo "[native-go-e2e] workdir: $WORK"
echo "[native-go-e2e] workloads: ${PIDS[*]}"
echo "[native-go-e2e] waiting ${WAIT_SEC}s for perf windows and async GoReSym cache"
sleep "$WAIT_SEC"

FROM_TS="$(date -u -d '-6 minutes' +%Y-%m-%dT%H:%M:%SZ)"
TO_TS="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

query_api() {
    local endpoint="$1"
    local runtime="$2"
    local encoded_filter
    encoded_filter="$(python3 -c 'import json, sys, urllib.parse; print(urllib.parse.quote(json.dumps({"runtime": sys.argv[1]})))' "$runtime")"
    curl -fsS -H "Drop-User-Uid: $UID_HEADER" \
        "http://127.0.0.1:8191/api/v1/profile/$endpoint?target_id=$TARGET_ID&profile_type=cpu&from=$FROM_TS&to=$TO_TS&filters=$encoded_filter&max_nodes=20000"
}

GO_TOPN="$(query_api topn go)"
GO_FLAME="$(query_api flamegraph go)"
NATIVE_TOPN="$(query_api topn native)"
NATIVE_FLAME="$(query_api flamegraph native)"

for function in goNormalWorker goPieWorker goStrippedWorker; do
    if [[ "$GO_TOPN" == *"$function"* ]]; then ok "Go TopN contains $function"; else fail "Go TopN missing $function"; fi
    if [[ "$GO_FLAME" == *"$function"* ]]; then ok "Go flamegraph contains $function"; else fail "Go flamegraph missing $function"; fi
done

for function in nativeCBurnWorker nativeCppBurnWorker; do
    if [[ "$NATIVE_TOPN" == *"$function"* ]]; then ok "native TopN contains $function"; else fail "native TopN missing $function"; fi
    if [[ "$NATIVE_FLAME" == *"$function"* ]]; then ok "native flamegraph contains $function"; else fail "native flamegraph missing $function"; fi
done

if [[ "$GO_TOPN" == *'"go":{'* ]]; then ok "Go runtime diagnostics returned"; else fail "Go runtime diagnostics missing"; fi
if [[ "$NATIVE_TOPN" == *'"native":{'* ]]; then ok "native runtime diagnostics returned"; else fail "native runtime diagnostics missing"; fi

exit "$FAILED"
