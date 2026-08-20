#!/bin/bash
# ============================================================
# scripts/runtime_symbol_smoke.sh
# 验证 Java/Node/Python 三种 runtime 的 perf map 能否被整机 perf 采集读取
# （Step 3：在写集成代码前先证明三种 runtime 都能产出 perf 可消费的用户函数符号）。
#
# 用法（在目标服务器上，作为有 docker 权限的用户）:
#   AGENT_CONTAINER=mini-drop-drop_agent-1 bash scripts/runtime_symbol_smoke.sh
#
# 每个 runtime 产出带辨识度函数名的 map：
#   Java:   javaBurnWorker   (asprof jcmd <pid> Compiler.perfmap)
#   Node:   nodeBurnWorker   (node --perf-basic-prof --perf-basic-prof-only-functions)
#   Python: py_burn_worker   (python3 -X perf)
# 然后把 map 放到 Agent 可见的 /tmp/perf-<pid>.map，做一次整机 perf capture，
# 用 perf script 检查三个函数名是否都出现在同一份 perf 数据里。
#
# 说明：默认 perf 采样事件用 cpu-clock（软件事件）。部分云 VM 上硬件 cycles
# 计数器冻结在 2^50，perf 默认的 cycles 事件采不到任何样本，这是本项目实测
# 发现的根因之一（见 Step 1 记录）。
# ============================================================

set -uo pipefail

AGENT_CONTAINER="${AGENT_CONTAINER:-mini-drop-drop_agent-1}"
PERF="${PERF:-/usr/local/bin/perf-real}"
PERF_EVENT="${PERF_EVENT:-cpu-clock}"
SAMPLE_HZ="${SAMPLE_HZ:-19}"
CAPTURE_SEC="${CAPTURE_SEC:-15}"

WORK="$(mktemp -d /tmp/runtime_smoke.XXXXXX)"
declare -a PIDS
declare -a AGENT_MAPS
FAILED=0

log()  { echo "[runtime-smoke] $*"; }
ok()   { echo "[runtime-smoke] ✔ $*"; }
fail() { echo "[runtime-smoke] ✘ $*"; FAILED=1; }

cleanup() {
    log "清理进程（精确 PID，不用 pkill -f）..."
    for p in "${PIDS[@]:-}"; do
        kill -TERM "$p" 2>/dev/null || true
    done
    sleep 1
    for p in "${PIDS[@]:-}"; do
        kill -KILL "$p" 2>/dev/null || true
    done
    for m in "${AGENT_MAPS[@]:-}"; do
        rm -f "$m" 2>/dev/null || true
    done
    rm -rf "$WORK"
    log "清理完成"
}
trap cleanup EXIT

require_cmd() { command -v "$1" >/dev/null 2>&1 || { fail "缺少命令 $1"; return 1; }; }

docker_exec() { docker exec "$AGENT_CONTAINER" "$@"; }

# ------------------------------------------------------------
log "=== 1. Java: javaBurnWorker (OpenJDK 21 + asprof Compiler.perfmap) ==="
if require_cmd java && require_cmd javac && docker_exec sh -c "command -v asprof" >/dev/null 2>&1; then
    cat > "$WORK/Burn.java" <<'JAVA'
public class Burn {
    public static void main(String[] args) throws Exception {
        long i = 0;
        while (true) {
            javaBurnWorker(i++);
            Thread.sleep(2);
        }
    }
    static void javaBurnWorker(long i) {
        double x = Math.sqrt(i * 1.0) * Math.sin(i * 0.01);
        long start = System.nanoTime();
        long acc = 0;
        while (System.nanoTime() - start < 1_000_000L) {
            acc += (long)(x * 1.0000001);
        }
        if (acc == -1) System.out.println(acc);
    }
}
JAVA
    javac -d "$WORK" "$WORK/Burn.java" || { fail "javac 编译失败"; }
    java -cp "$WORK" Burn &
    JAVA_PID=$!
    PIDS+=("$JAVA_PID")
    log "Java PID=$JAVA_PID，等待 JIT 编译（6s）..."
    sleep 6
    log "从 Agent 容器执行: asprof jcmd $JAVA_PID Compiler.perfmap"
    docker_exec sh -c "timeout 5 asprof jcmd $JAVA_PID Compiler.perfmap" 2>&1 | tail -5 || true
    sleep 2
    JAVA_MAP="/proc/$JAVA_PID/root/tmp/perf-$JAVA_PID.map"
    if [ -f "$JAVA_MAP" ]; then
        SIZE=$(stat -c%s "$JAVA_MAP")
        CNT=$(grep -c "javaBurnWorker" "$JAVA_MAP" 2>/dev/null || true)
        log "Java map 存在: $JAVA_MAP size=$SIZE javaBurnWorker条目=$CNT"
        if [ "${CNT:-0}" -gt 0 ]; then ok "Java map 含 javaBurnWorker"; else fail "Java map 不含 javaBurnWorker"; fi
        cp "$JAVA_MAP" "/tmp/perf-$JAVA_PID.map" && AGENT_MAPS+=("/tmp/perf-$JAVA_PID.map")
        docker_exec sh -c "ls -la /tmp/perf-$JAVA_PID.map" >/dev/null 2>&1 && ok "Agent 可见 Java map (/tmp/perf-$JAVA_PID.map)"
    else
        fail "Java map 不存在: $JAVA_MAP"
    fi
else
    fail "跳过 Java（缺 java/javac 或容器内无 asprof）"
fi

# ------------------------------------------------------------
log "=== 2. Node: nodeBurnWorker (node --perf-basic-prof-only-functions) ==="
if require_cmd node; then
    # 注意：必须用紧循环负载，让 V8 TurboFan 优化 nodeBurnWorker 并驻留 JIT map。
    # 若用 setInterval 短调用，nodeBurnWorker 会跑在解释器层，热点帧在
    # libnode.so 的 interpreter handler 里，perf map 解析不到（Step 3 实测）。
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
    log "Node PID=$NODE_PID，等待 map 生成..."
    sleep 6
    NODE_MAP="/proc/$NODE_PID/root/tmp/perf-$NODE_PID.map"
    if [ -f "$NODE_MAP" ]; then
        SIZE=$(stat -c%s "$NODE_MAP")
        CNT=$(grep -c "nodeBurnWorker" "$NODE_MAP" 2>/dev/null || true)
        log "Node map 存在: $NODE_MAP size=$SIZE nodeBurnWorker条目=$CNT"
        if [ "${CNT:-0}" -gt 0 ]; then ok "Node map 含 nodeBurnWorker"; else fail "Node map 不含 nodeBurnWorker"; fi
        cp "$NODE_MAP" "/tmp/perf-$NODE_PID.map" && AGENT_MAPS+=("/tmp/perf-$NODE_PID.map")
        docker_exec sh -c "ls -la /tmp/perf-$NODE_PID.map" >/dev/null 2>&1 && ok "Agent 可见 Node map (/tmp/perf-$NODE_PID.map)"
    else
        fail "Node map 不存在: $NODE_MAP"
    fi
else
    fail "跳过 Node（缺 node）"
fi

# ------------------------------------------------------------
log "=== 3. Python: py_burn_worker (python3 -X perf) ==="
if require_cmd python3; then
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
    time.sleep(0.002)
PY
    python3 -X perf "$WORK/burn.py" &
    PY_PID=$!
    PIDS+=("$PY_PID")
    log "Python PID=$PY_PID，等待 map 生成..."
    sleep 6
    PY_MAP="/proc/$PY_PID/root/tmp/perf-$PY_PID.map"
    if [ -f "$PY_MAP" ]; then
        SIZE=$(stat -c%s "$PY_MAP")
        CNT=$(grep -c "py_burn_worker" "$PY_MAP" 2>/dev/null || true)
        log "Python map 存在: $PY_MAP size=$SIZE py_burn_worker条目=$CNT"
        if [ "${CNT:-0}" -gt 0 ]; then ok "Python map 含 py_burn_worker"; else fail "Python map 不含 py_burn_worker"; fi
        cp "$PY_MAP" "/tmp/perf-$PY_PID.map" && AGENT_MAPS+=("/tmp/perf-$PY_PID.map")
        docker_exec sh -c "ls -la /tmp/perf-$PY_PID.map" >/dev/null 2>&1 && ok "Agent 可见 Python map (/tmp/perf-$PY_PID.map)"
    else
        fail "Python map 不存在: $PY_MAP"
    fi
else
    fail "跳过 Python（缺 python3）"
fi

# ------------------------------------------------------------
log "=== 4. 整机 perf capture + script 检查三个函数名 ==="
docker_exec sh -c "$PERF record -a -e $PERF_EVENT -F $SAMPLE_HZ -g -o /tmp/runtime_smoke.data -- sleep $CAPTURE_SEC" 2>&1 | tail -3
docker_exec sh -c "$PERF script -i /tmp/runtime_smoke.data" > "$WORK/script.txt" 2>/dev/null
SCRIPT_LINES=$(wc -l < "$WORK/script.txt")
log "perf script 输出行数: $SCRIPT_LINES"
for fn in javaBurnWorker nodeBurnWorker py_burn_worker; do
    if grep -q "$fn" "$WORK/script.txt"; then
        ok "perf script 中出现 $fn"
    else
        fail "perf script 中缺少 $fn"
    fi
done

if [ "$FAILED" = "1" ]; then
    log "=== 完成：有失败项 ==="
    exit 1
fi
log "=== 完成：全部通过 ==="
