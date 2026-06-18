#!/bin/bash
# ============================================================
# apiserver W3 验收测试脚本
# 测试完整 gRPC 下发链路：apiserver → drop_server → drop_agent → 任务完成
# 用法：./test_w3.sh
# 前提：PostgreSQL 已运行，drop 数据库已创建
# ============================================================
set -e

BASE="http://localhost:8191"
PASS=0
FAIL=0
DROP_DIR="$(cd "$(dirname "$0")/../drop/build" && pwd)"

green() { echo -e "\033[32m$1\033[0m"; }
red()   { echo -e "\033[31m$1\033[0m"; }
blue()  { echo -e "\033[34m$1\033[0m"; }

check() {
    local desc="$1"
    local expected="$2"
    local actual="$3"
    if echo "$actual" | grep -q "$expected"; then
        green "  ✅ $desc"
        PASS=$((PASS + 1))
    else
        red "  ❌ $desc (expected: $expected)"
        red "     got: $actual"
        FAIL=$((FAIL + 1))
    fi
}

cleanup() {
    pkill -f "./apiserver" 2>/dev/null || true
    pkill -f "./drop_agent" 2>/dev/null || true
    pkill -f "./drop_server" 2>/dev/null || true
    wait 2>/dev/null || true
}
trap cleanup EXIT

echo "============================================"
echo "  apiserver W3 验收测试"
echo "  gRPC 下发链路：apiserver → drop_server → agent"
echo "============================================"
echo ""

# ---- 启动 drop_server ----
blue "1. 启动 drop_server..."
cleanup
"$DROP_DIR/drop_server" > /tmp/drop_server_w3.log 2>&1 &
sleep 1
if pgrep -f drop_server > /dev/null; then
    green "  ✅ drop_server 已启动"
    PASS=$((PASS + 1))
else
    red "  ❌ drop_server 启动失败"
    FAIL=$((FAIL + 1))
fi

# ---- 启动 drop_agent ----
blue "2. 启动 drop_agent..."
"$DROP_DIR/drop_agent" > /tmp/drop_agent_w3.log 2>&1 &
sleep 3
if grep -q "注册成功" /tmp/drop_agent_w3.log 2>/dev/null; then
    green "  ✅ drop_agent 已注册"
    PASS=$((PASS + 1))
else
    red "  ❌ drop_agent 注册失败"
    FAIL=$((FAIL + 1))
fi

# ---- 启动 apiserver ----
blue "3. 启动 apiserver..."
cd "$(dirname "$0")"
PG_DSN="host=localhost user=postgres password=dev dbname=drop sslmode=disable" \
  ./apiserver > /tmp/apiserver_w3.log 2>&1 &
sleep 3

# 检查 gRPC 连接
if grep -q "gRPC 连接 drop_server 成功" /tmp/apiserver_w3.log 2>/dev/null; then
    green "  ✅ apiserver gRPC 已连接 drop_server"
    PASS=$((PASS + 1))
else
    red "  ❌ apiserver gRPC 连接失败"
    FAIL=$((FAIL + 1))
fi

# ---- 创建任务 ----
blue "4. 创建任务 (duration=3s)..."
R=$(curl -s -X POST "$BASE/api/v1/tasks" \
  -H "Content-Type: application/json" \
  -H "Drop_user_uid: user-001" \
  -H "Drop_user_name: Alice" \
  -d '{"name":"W3验收测试","task_type":0,"profiler_type":0,"target_ip":"127.0.0.1","target_pid":1,"duration":3,"frequency":99}')
check "创建任务 code=0" '"code":0' "$R"
TID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['tid'])" 2>/dev/null)
check "返回有效 tid" "tid-" "$TID"

# ---- 验证 gRPC 下发 ----
blue "5. 验证任务已下发（status=1 RUNNING）..."
sleep 1
R=$(curl -s "$BASE/api/v1/tasks/$TID")
check "status=1 (已下发)" '"status":1' "$R"
check "包含 gRPC 响应" "drop_server" "$R"

# ---- 验证 drop_server 收到任务 ----
blue "6. 验证 drop_server 收到 CreateTask..."
if grep -q "CreateTask: targetIP=127.0.0.1" /tmp/drop_server_w3.log 2>/dev/null; then
    green "  ✅ drop_server 收到 CreateTask"
    PASS=$((PASS + 1))
else
    red "  ❌ drop_server 未收到 CreateTask"
    FAIL=$((FAIL + 1))
fi

# ---- 验证 agent 拉取并执行 ----
blue "7. 验证 agent 拉取任务并执行..."
# 等待 agent 心跳拉取（最多等 15 秒）
for i in $(seq 1 3); do
    sleep 5
    if grep -q "NotifyResult" /tmp/drop_agent_w3.log 2>/dev/null; then
        green "  ✅ agent 已执行任务并上报 NotifyResult"
        PASS=$((PASS + 1))
        break
    fi
    if [ "$i" = "3" ]; then
        red "  ❌ agent 未上报 NotifyResult"
        FAIL=$((FAIL + 1))
    fi
done

# ---- 验证 drop_server 收到结果 ----
if grep -q "收到结果" /tmp/drop_server_w3.log 2>/dev/null; then
    green "  ✅ drop_server 收到 agent 结果"
    PASS=$((PASS + 1))
else
    red "  ❌ drop_server 未收到结果"
    FAIL=$((FAIL + 1))
fi

# ---- 等待轮询器标记完成 (status=2) ----
blue "8. 等待任务完成（轮询器标记 status=2，约 40s）..."
COMPLETED=0
for i in $(seq 1 15); do
    sleep 3
    STATUS=$(curl -s "$BASE/api/v1/tasks/$TID" | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['status'])" 2>/dev/null)
    echo -n "  [$((i*3))s] status=$STATUS  "
    if [ "$STATUS" = "2" ]; then
        green "✅"
        COMPLETED=1
        PASS=$((PASS + 1))
        break
    fi
    echo ""
done
if [ "$COMPLETED" = "0" ]; then
    red "  ❌ 任务未在预期时间内完成"
    FAIL=$((FAIL + 1))
fi

# ---- 最终状态 ----
echo ""
blue "9. 最终任务详情..."
curl -s "$BASE/api/v1/tasks/$TID" | python3 -c "
import sys,json
d = json.load(sys.stdin)['data']
print(f'  tid={d[\"tid\"]}')
print(f'  status={d[\"status\"]}')
print(f'  status_info=\"{d[\"status_info\"]}\"')
print(f'  begin_time={d[\"begin_time\"]}')
print(f'  end_time={d[\"end_time\"]}')
"

# ---- 验证之前的 W2 功能仍然正常 ----
blue "10. 回归测试：W2 基础 API 仍然正常..."
R=$(curl -s "$BASE/healthz")
check "healthz ok" '"status":"ok"' "$R"

R=$(curl -s -H "Drop_user_uid: user-001" -H "Drop_user_name: Alice" "$BASE/api/v1/auth/check")
check "auth/check ok" '"code":0' "$R"

R=$(curl -s "$BASE/api/v1/agents")
check "agents ok" '"code":0' "$R"

# ---- 结果汇总 ----
echo ""
echo "============================================"
TOTAL=$((PASS + FAIL))
green "通过: $PASS / $TOTAL"
if [ "$FAIL" -gt 0 ]; then
    red "失败: $FAIL"
    echo ""
    echo "=== drop_server 日志 ==="
    tail -10 /tmp/drop_server_w3.log
    echo ""
    echo "=== apiserver 日志 ==="
    tail -10 /tmp/apiserver_w3.log
    exit 1
else
    green "🎉 W3 验收成功！全链路 gRPC 下发通过！"
fi
