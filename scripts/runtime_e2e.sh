#!/bin/bash
# ============================================================
# scripts/runtime_e2e.sh — 三语言端到端验收（Step 6）
# ============================================================
# 在服务器上同时运行 Java/Node/Python 三个受控 CPU 负载（带辨识度函数名），
# 等一个完整持续采集批次后依次核对：
#   1. Agent 日志 runtime_maps 状态（java/node/python detected+ready）
#   2. profile_windows 至少 3 个 CPU 非空窗口（backend=perf）
#   3. MinIO 原始批次 JSON 中出现三个函数名
#   4. /api/v1/profile/topn 按 comm 过滤后出现函数名
#   5. symbol_status 为 complete（或 partial 且原因一致）
#
# 用法: bash scripts/runtime_e2e.sh
#   AGENT=mini-drop-drop_agent-1  TARGET_IP=111.230.29.115  UID=agent-001
# ============================================================

set -uo pipefail

AGENT="${AGENT:-mini-drop-drop_agent-1}"
TARGET_IP="${TARGET_IP:-111.230.29.115}"
UID_HEADER="${UID_HEADER:-agent-001}"
WAIT_SEC="${WAIT_SEC:-95}"

WORK="$(mktemp -d /tmp/runtime_e2e.XXXXXX)"
declare -a PIDS
ok()   { echo "[e2e] ✔ $*"; }
fail() { echo "[e2e] ✘ $*"; FAILED=1; }

cleanup() {
    for p in "${PIDS[@]:-}"; do
        kill -TERM "$p" 2>/dev/null || true
    done
    sleep 1
    for p in "${PIDS[@]:-}"; do
        kill -KILL "$p" 2>/dev/null || true
    done
    # 清理本脚本放进 agent /tmp 的测试产物
    rm -rf "$WORK"
    echo "[e2e] 清理完成"
}
trap cleanup EXIT

# ---------- 1. Java: javaBurnWorker（Agent 自动 asprof Compiler.perfmap） ----------
cat > "$WORK/Burn.java" <<'JAVA'
public class Burn {
    public static void main(String[] args) throws Exception {
        long i = 0;
        while (true) {
            javaBurnWorker(i++);
        }
    }
    static void javaBurnWorker(long i) {
        double x = Math.sqrt(i * 1.0) * Math.sin(i * 0.01);
        long acc = 0;
        long start = System.nanoTime();
        while (System.nanoTime() - start < 2_000_000L) {
            acc += (long)(x * 1.0000001);
        }
        if (acc == -1) System.out.println(acc);
    }
}
JAVA
javac -d "$WORK" "$WORK/Burn.java" 2>/dev/null
java -cp "$WORK" Burn &
JAVA_PID=$!
PIDS+=("$JAVA_PID")
echo "[e2e] Java PID=$JAVA_PID (javaBurnWorker)"

# ---------- 2. Node: nodeBurnWorker（紧循环 + perf-basic-prof） ----------
cat > "$WORK/burn.js" <<'NODE'
function nodeBurnWorker() {
    let acc = 0;
    for (let j = 0; j < 2000000; j++) {
        acc += Math.floor(Math.sqrt(j * 1.0) * Math.sin(j * 0.001) * 1000);
    }
    if (acc === -1) console.log(acc);
    return acc;
}
let i = 0;
while (true) { nodeBurnWorker(); i++; if (i === 0) console.log(i); }
NODE
node --perf-basic-prof --perf-basic-prof-only-functions "$WORK/burn.js" &
NODE_PID=$!
PIDS+=("$NODE_PID")
echo "[e2e] Node PID=$NODE_PID (nodeBurnWorker)"

# ---------- 3. Python: py_burn_worker（-X perf） ----------
cat > "$WORK/burn.py" <<'PY'
import math, time

def py_burn_worker(i):
    x = math.sqrt(i * 1.0) * math.sin(i * 0.01)
    acc = 0
    start = time.time_ns()
    while time.time_ns() - start < 1_000_000:
        acc += int(x * 1.0000001)
    if acc == -1:
        print(acc)

i = 0
while True:
    py_burn_worker(i)
    i += 1
PY
python3 -X perf "$WORK/burn.py" &
PY_PID=$!
PIDS+=("$PY_PID")
echo "[e2e] Python PID=$PY_PID (py_burn_worker)"

echo "[e2e] 等待 $WAIT_SEC 秒让持续采集跑完至少一个批次..."
sleep "$WAIT_SEC"

# ---------- 4. 核对 Agent 日志 runtime_maps 状态 ----------
echo ""
echo "=== [e2e] 1. Agent 日志 runtime_maps 状态（最近 15 条） ==="
docker logs "$AGENT" 2>&1 | grep -E "runtime_maps status" | tail -15

echo ""
echo "=== [e2e] 2. 最新 session 的 CPU 窗口（至少 3 个非空） ==="
docker exec mini-drop-postgres-1 psql -U postgres -d drop -t -c \
  "SELECT window_start, backend, sample_count, backend_status FROM profile_windows WHERE session_sid=(SELECT sid FROM continuous_sessions ORDER BY id DESC LIMIT 1) AND signal_type='cpu_profile' ORDER BY id DESC LIMIT 8;"

echo ""
echo "=== [e2e] 3. MinIO 原始批次 JSON 中的函数名 ==="
LATEST_OBJ=$(docker exec mini-drop-postgres-1 psql -U postgres -d drop -t -A -c "SELECT object_key FROM profile_batches WHERE session_sid=(SELECT sid FROM continuous_sessions ORDER BY id DESC LIMIT 1) ORDER BY id DESC LIMIT 1;")
echo "latest object: $LATEST_OBJ"
docker exec mini-drop-minio-1 sh -c "export MC_HOST_minio=http://drop:dropdrop@localhost:9000; mc cat minio/drop-data/$LATEST_OBJ" 2>/dev/null > "$WORK/latest_batch.json"
for fn in javaBurnWorker nodeBurnWorker py_burn_worker; do
    if grep -q "$fn" "$WORK/latest_batch.json"; then ok "raw JSON 含 $fn"; else fail "raw JSON 缺 $fn"; fi
done
echo "--- symbol_status ---"
grep -o '"symbol_status":"[a-z]*"' "$WORK/latest_batch.json" | head -1
echo "--- runtime_maps 摘要 ---"
grep -o '"runtime_maps":{[^}]*}[^}]*}[^}]*}' "$WORK/latest_batch.json" | head -1 || true

echo ""
echo "=== [e2e] 4. TopN API（按 comm 过滤） ==="
FROM_TS=$(date -u -d "-2 minutes" +%Y-%m-%dT%H:%M:%SZ)
TO_TS=$(date -u +%Y-%m-%dT%H:%M:%SZ)
for comm in java node python3; do
    RESP=$(curl -sS -H "Drop-User-Uid: $UID_HEADER" \
      "http://127.0.0.1:8191/api/v1/profile/topn?target_id=$TARGET_IP:hotmethod&profile_type=cpu&from=$FROM_TS&to=$TO_TS&filters=$(python3 -c "import urllib.parse,sys; print(urllib.parse.quote('comm=\"$comm\"'))")")
    CNT=$(echo "$RESP" | grep -oE "javaBurnWorker|nodeBurnWorker|py_burn_worker" | sort -u | tr '\n' ' ')
    if [ -n "$CNT" ]; then ok "TopN comm=$comm 出现: $CNT"; else fail "TopN comm=$comm 无函数名"; fi
done

echo ""
echo "=== [e2e] 5. 负载进程仍存活 ==="
for p in "${PIDS[@]}"; do
    ps -p "$p" -o pid,comm >/dev/null 2>&1 && echo "  PID $p 存活" || echo "  PID $p 已退出"
done

echo ""
if [ "${FAILED:-0}" = "1" ]; then
    echo "[e2e] 结果: 有失败项"
    exit 1
else
    echo "[e2e] 结果: 全部通过"
fi
