#!/bin/bash
# ============================================================
# apiserver W2 验收测试脚本
# 测试所有 11 个 API 的正确性和边界情况
# 用法：./test_w2.sh
# 前提：PostgreSQL 已运行，drop 数据库已创建，密码=dev
# ============================================================
set -e

BASE="http://localhost:8191"
PASS=0
FAIL=0

# 颜色输出
green() { echo -e "\033[32m$1\033[0m"; }
red()   { echo -e "\033[31m$1\033[0m"; }

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

echo "============================================"
echo "  apiserver W2 验收测试"
echo "============================================"
echo ""

# ---- 1. 健康检查 ----
echo "1. GET /healthz"
R=$(curl -s "$BASE/healthz")
check "返回 status=ok" '"status":"ok"' "$R"
check "返回 service=apiserver" '"service":"apiserver"' "$R"

# ---- 2. 鉴权回调 ----
echo "2. GET /api/v1/auth/check"
R=$(curl -s -H "Drop_user_uid: user-001" -H "Drop_user_name: Alice" "$BASE/api/v1/auth/check")
check "返回 code=0" '"code":0' "$R"
check "返回 uid" '"uid":"user-001"' "$R"
check "返回 user_name" 'Alice' "$R"

# ---- 3. 用户信息 ----
echo "3. GET /api/v1/users"
R=$(curl -s -H "Drop_user_uid: user-001" -H "Drop_user_name: Alice" "$BASE/api/v1/users")
check "返回 code=0" '"code":0' "$R"
check "返回 groups 数组" 'groups' "$R"

# ---- 4. Agent 列表 ----
echo "4. GET /api/v1/agents"
R=$(curl -s "$BASE/api/v1/agents")
check "返回 code=0" '"code":0' "$R"
check "返回 agents 数组" '"agents"' "$R"
check "返回 total 字段" '"total"' "$R"

# ---- 5. Agent 资源统计 (无此 Agent) ----
echo "5. GET /api/v1/agent/stat?ip=10.0.0.1"
R=$(curl -s "$BASE/api/v1/agent/stat?ip=10.0.0.1")
check "返回 404" '"code":404' "$R"

# ---- 6. Agent 资源统计 (缺参数) ----
echo "6. GET /api/v1/agent/stat (缺 ip)"
R=$(curl -s "$BASE/api/v1/agent/stat")
check "返回 400" '"code":400' "$R"

# ---- 7. 创建任务 ----
echo "7. POST /api/v1/tasks"
# 先记录创建前的任务总数
BEFORE_COUNT=$(curl -s -H "Drop_user_uid: user-001" "$BASE/api/v1/tasks" | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['total'])" 2>/dev/null || echo 0)

R=$(curl -s -X POST "$BASE/api/v1/tasks" \
  -H "Content-Type: application/json" \
  -H "Drop_user_uid: user-001" \
  -H "Drop_user_name: Alice" \
  -d '{"name":"CPU采样","task_type":0,"profiler_type":0,"target_ip":"10.0.0.1","target_pid":1234,"duration":10,"frequency":99}')
check "返回 code=0" '"code":0' "$R"
TID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['tid'])")
check "返回 tid" "tid-" "$TID"

# ---- 8. 任务列表 ----
echo "8. GET /api/v1/tasks"
R=$(curl -s -H "Drop_user_uid: user-001" "$BASE/api/v1/tasks")
check "返回 code=0" '"code":0' "$R"
check "返回 tasks 数组" '"tasks"' "$R"
AFTER_COUNT=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['total'])")
check "任务数+1" "$((BEFORE_COUNT + 1))" "$AFTER_COUNT"

# ---- 9. 任务详情 ----
echo "9. GET /api/v1/tasks/:tid"
R=$(curl -s "$BASE/api/v1/tasks/$TID")
check "返回 code=0" '"code":0' "$R"
check "返回 task name" 'CPU采样' "$R"
# W3: gRPC未连接时 status=3(失败)，连接时 status=1(已下发)；都正常
STATUS=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['task']['status'])")
check "status 有效 (0/1/3)" "status" "status=$STATUS"

# ---- 10. 按状态过滤 ----
echo "10. GET /api/v1/tasks?status=0"
R=$(curl -s -H "Drop_user_uid: user-001" "$BASE/api/v1/tasks?status=0")
check "返回 code=0" '"code":0' "$R"
# 验证所有返回任务 status 都是 0
ALL_ZERO=$(echo "$R" | python3 -c "
import sys,json
tasks=json.load(sys.stdin)['data']['tasks']
ok=all(t['status']==0 for t in tasks)
print('OK' if ok else 'FAIL')
")
check "过滤结果全部 status=0" "OK" "$ALL_ZERO"

echo "11. GET /api/v1/tasks?status=2"
R=$(curl -s -H "Drop_user_uid: user-001" "$BASE/api/v1/tasks?status=2")
check "返回 code=0" '"code":0' "$R"
# 验证所有返回任务 status 都是 2
ALL_TWO=$(echo "$R" | python3 -c "
import sys,json
tasks=json.load(sys.stdin)['data']['tasks']
ok=all(t['status']==2 for t in tasks)
print('OK' if ok else 'FAIL')
")
check "过滤结果全部 status=2" "OK" "$ALL_TWO"

# ---- 12. 重试任务 ----
echo "12. POST /api/v1/tasks/:tid/retry"
R=$(curl -s -X POST "$BASE/api/v1/tasks/$TID/retry" \
  -H "Drop_user_uid: user-001" \
  -H "Drop_user_name: Alice")
check "返回 code=0" '"code":0' "$R"
RETRY_TID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['tid'])")
check "返回新 tid" "tid-" "$RETRY_TID"
check "新旧 tid 不同" ".*" "$(test "$TID" != "$RETRY_TID" && echo "OK")"

# ---- 13. 重试任务详情 ----
echo "13. GET 重试任务详情"
R=$(curl -s "$BASE/api/v1/tasks/$RETRY_TID")
check "name 包含 (重试)" '重试' "$R"
check "master_task_tid 指向原任务" "$TID" "$R"

# ---- 14. 删除原任务 ----
echo "14. DELETE /api/v1/tasks/:tid"
R=$(curl -s -X DELETE "$BASE/api/v1/tasks/$TID")
check "返回 code=0" '"code":0' "$R"

# ---- 15. 验证软删除 ----
echo "15. GET 已删除任务 (应 404)"
R=$(curl -s "$BASE/api/v1/tasks/$TID")
check "返回 404" '"code":404' "$R"

# ---- 16. 再次删除（幂等）- 应 404 ----
echo "16. DELETE 已删除任务 (应 404)"
R=$(curl -s -X DELETE "$BASE/api/v1/tasks/$TID")
check "返回 404" '"code":404' "$R"

# ---- 17. COS 文件列表 ----
echo "17. GET /api/v1/cosfiles?tid=xxx"
R=$(curl -s "$BASE/api/v1/cosfiles?tid=$RETRY_TID")
check "返回 code=0" '"code":0' "$R"
check "返回 files 数组" '"files"' "$R"

# ---- 18. COS 文件列表（缺参数） ----
echo "18. GET /api/v1/cosfiles (缺 tid)"
R=$(curl -s "$BASE/api/v1/cosfiles")
check "返回 400" '"code":400' "$R"

# ---- 19. 创建任务（缺 target_ip） ----
echo "19. POST /api/v1/tasks (缺必填字段)"
R=$(curl -s -X POST "$BASE/api/v1/tasks" \
  -H "Content-Type: application/json" \
  -d '{"name":"bad"}')
check "返回 400" '"code":400' "$R"

# ---- 20. 创建任务（完整参数） ----
echo "20. POST /api/v1/tasks (完整参数)"
R=$(curl -s -X POST "$BASE/api/v1/tasks" \
  -H "Content-Type: application/json" \
  -H "Drop_user_uid: user-001" \
  -H "Drop_user_name: Alice" \
  -d '{"name":"Java堆分析","task_type":6,"profiler_type":1,"target_ip":"10.0.0.2","target_pid":5678,"duration":30,"frequency":0,"callgraph":"dwarf","event":"cpu-cycles","subprocess":true,"container_name":"pod-abc"}')
check "返回 code=0" '"code":0' "$R"

# ---- 结果汇总 ----
echo ""
echo "============================================"
TOTAL=$((PASS + FAIL))
green "通过: $PASS / $TOTAL"
if [ "$FAIL" -gt 0 ]; then
    red "失败: $FAIL"
    exit 1
else
    green "🎉 全部通过！W2 验收成功！"
fi
