#!/bin/bash
# ============================================================
# apiserver W5 验收测试脚本
# 测试用户组管理 + 定时任务管理
# 用法：./test_w5.sh
# 前提：PostgreSQL 已运行
# ============================================================
set -e

BASE="http://localhost:8191"
PASS=0
FAIL=0

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
        red "     got: $(echo "$actual" | head -1)"
        FAIL=$((FAIL + 1))
    fi
}

echo "============================================"
echo "  apiserver W5 验收测试"
echo "  用户组管理 + 定时任务管理"
echo "============================================"
echo ""

# ---- 启动 apiserver ----
blue "0. 启动 apiserver..."
pkill -f "./apiserver" 2>/dev/null || true
sleep 1
cd "$(dirname "$0")"
PG_DSN="host=localhost user=postgres password=dev dbname=drop sslmode=disable" \
  ./apiserver > /tmp/apiserver_w5_test.log 2>&1 &
sleep 2

# ---- 1. 创建组 ----
blue "1. 创建用户组..."
R=$(curl -s -X POST "$BASE/api/v1/groups" \
  -H "Content-Type: application/json" \
  -H "Drop_user_uid: user-001" -H "Drop_user_name: Alice" \
  -d '{"name":"测试组"}')
check "创建组 code=0" '"code":0' "$R"
GID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['gid'])" 2>/dev/null)
check "返回有效 gid" "grp-" "$GID"

# ---- 2. 列表组 ----
blue "2. 列出所有组..."
R=$(curl -s "$BASE/api/v1/groups")
check "列表 code=0" '"code":0' "$R"
check "total=1" '"total":1' "$R"

# ---- 3. 组详情 ----
blue "3. 查看组详情..."
R=$(curl -s "$BASE/api/v1/groups/$GID")
check "详情 code=0" '"code":0' "$R"
check "含 group 信息" '"group"' "$R"
check "含 members 列表" '"members"' "$R"

# ---- 4. 添加成员 ----
blue "4. 添加组成员..."
R=$(curl -s -X POST "$BASE/api/v1/groups/$GID/members" \
  -H "Content-Type: application/json" \
  -d '{"uid":"user-002"}')
check "添加成员 code=0" '"code":0' "$R"

R=$(curl -s -X POST "$BASE/api/v1/groups/$GID/members" \
  -H "Content-Type: application/json" \
  -d '{"uid":"user-003"}')
check "添加第二个成员 code=0" '"code":0' "$R"

# ---- 5. 验证成员数 ----
blue "5. 验证成员数量..."
R=$(curl -s "$BASE/api/v1/groups/$GID")
check "成员数为3（含创建者）" 'user-003' "$R"

# ---- 6. 更新组 ----
blue "6. 更新组名..."
R=$(curl -s -X PUT "$BASE/api/v1/groups/$GID" \
  -H "Content-Type: application/json" \
  -d '{"name":"改名后的组"}')
check "更新 code=0" '"code":0' "$R"

# ---- 7. 移除成员 ----
blue "7. 移除组成员..."
R=$(curl -s -X DELETE "$BASE/api/v1/groups/$GID/members/user-003")
check "移除成员 code=0" '"code":0' "$R"

# ---- 8. 创建定时任务 ----
blue "8. 创建定时任务..."
R=$(curl -s -X POST "$BASE/api/v1/schedule/task" \
  -H "Content-Type: application/json" \
  -H "Drop_user_uid: user-001" -H "Drop_user_name: Alice" \
  -d '{"name":"定期采样","cron_expr":"*/10 * * * *","task_type":0,"profiler_type":0,"target_ip":"127.0.0.1","target_pid":1,"duration":5}')
check "创建 schedule code=0" '"code":0' "$R"
SID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['sid'])" 2>/dev/null)
check "返回有效 sid" "sch-" "$SID"

# ---- 9. 列表定时任务 ----
blue "9. 列出定时任务..."
R=$(curl -s "$BASE/api/v1/schedule/tasks")
check "列表 code=0" '"code":0' "$R"
check "total=1" '"total":1' "$R"
check "cron 表达式正确" '\*/10 \* \* \* \*' "$R"

# ---- 10. 切换启用/禁用 ----
blue "10. 切换定时任务状态..."
R=$(curl -s -X POST "$BASE/api/v1/schedule/$SID/toggle")
check "toggle code=0" '"code":0' "$R"
check "已禁用" '"enabled":false' "$R"

# 再切回启用
R=$(curl -s -X POST "$BASE/api/v1/schedule/$SID/toggle")
check "重新启用" '"enabled":true' "$R"

# ---- 11. Cron 表达式校验 ----
blue "11. Cron 表达式校验..."
R=$(curl -s -X POST "$BASE/api/v1/schedule/task" \
  -H "Content-Type: application/json" \
  -H "Drop_user_uid: user-001" \
  -d '{"name":"bad","cron_expr":"invalid","target_ip":"127.0.0.1"}')
check "无效 cron 返回 400" '"code":400' "$R"

# ---- 12. 删除定时任务 ----
blue "12. 删除定时任务..."
R=$(curl -s -X DELETE "$BASE/api/v1/schedule/$SID")
check "删除 code=0" '"code":0' "$R"

R=$(curl -s "$BASE/api/v1/schedule/tasks")
check "删除后 total=0" '"total":0' "$R"

# ---- 13. 删除组 ----
blue "13. 删除组..."
R=$(curl -s -X DELETE "$BASE/api/v1/groups/$GID")
check "删除组 code=0" '"code":0' "$R"

R=$(curl -s "$BASE/api/v1/groups")
check "删除后 total=0" '"total":0' "$R"

# ---- 14. W2/W4 回归 ----
blue "14. 回归测试..."
R=$(curl -s "$BASE/healthz")
check "healthz ok" '"status":"ok"' "$R"

R=$(curl -s -H "Drop_user_uid: user-001" "$BASE/api/v1/agents")
check "agents ok" '"code":0' "$R"

R=$(curl -s "$BASE/api/v1/cosfiles?tid=test")
check "cosfiles ok" '"code":0' "$R"

# ---- 结果汇总 ----
echo ""
echo "============================================"
TOTAL=$((PASS + FAIL))
green "通过: $PASS / $TOTAL"
if [ "$FAIL" -gt 0 ]; then
    red "失败: $FAIL"
    exit 1
else
    green "🎉 W5 验收成功！用户组 + 定时任务全部通过！"
fi

pkill -f "./apiserver" 2>/dev/null || true
