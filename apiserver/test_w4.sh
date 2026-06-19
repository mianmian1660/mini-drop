#!/bin/bash
# ============================================================
# apiserver W4 验收测试脚本
# 测试 MinIO 存储集成：文件上传、列表、预签名下载、任务详情含文件
# 用法：./test_w4.sh
# 前提：PostgreSQL 运行、MinIO 运行（端口9000，账号drop/dropdrop）
# ============================================================
set -e

BASE="http://localhost:8191"
PASS=0
FAIL=0
MINIO_ENDPOINT="${S3_ENDPOINT:-localhost:9000}"

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
echo "  apiserver W4 验收测试"
echo "  MinIO 存储：上传 / 列表 / 签名下载 / 任务详情"
echo "============================================"
echo ""

# ---- 1. 启动 apiserver ----
blue "1. 启动 apiserver（连接 MinIO: $MINIO_ENDPOINT）..."
pkill -f "./apiserver" 2>/dev/null || true
sleep 1
cd "$(dirname "$0")"
PG_DSN="host=localhost user=postgres password=dev dbname=drop sslmode=disable" \
  S3_ENDPOINT="$MINIO_ENDPOINT" \
  S3_ACCESS_KEY="drop" \
  S3_SECRET_KEY="dropdrop" \
  ./apiserver > /tmp/apiserver_w4_test.log 2>&1 &
sleep 3

if grep -q "MinIO 存储初始化成功" /tmp/apiserver_w4_test.log 2>/dev/null; then
    green "  ✅ MinIO 存储初始化成功"
    PASS=$((PASS + 1))
else
    red "  ❌ MinIO 存储初始化失败"
    grep -i "MinIO\|minio\|storage" /tmp/apiserver_w4_test.log
    FAIL=$((FAIL + 1))
fi

# ---- 2. 文件上传 ----
blue "2. 测试文件上传..."
echo "test-content-for-w4-upload" > /tmp/w4_test_file.txt
R=$(curl -s -X POST "$BASE/api/v1/cosfiles/upload" \
  -F "tid=tid-w4-test" \
  -F "file=@/tmp/w4_test_file.txt")
check "上传返回 code=0" '"code":0' "$R"
DOWNLOAD_URL=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['download_url'])" 2>/dev/null)
check "生成下载 URL" "http" "$DOWNLOAD_URL"
check "文件 key 正确" "tid-w4-test" "$R"
check "文件大小正确" "27" "$R"

# ---- 3. 预签名 URL 下载 ----
blue "3. 测试预签名 URL 下载..."
CONTENT=$(curl -s "$DOWNLOAD_URL")
check "下载内容正确" "test-content-for-w4-upload" "$CONTENT"

# ---- 4. 文件列表 ----
blue "4. 测试文件列表..."
R=$(curl -s "$BASE/api/v1/cosfiles?tid=tid-w4-test")
check "列表返回 code=0" '"code":0' "$R"
check "total=1" '"total":1' "$R"
check "包含 download_url" 'download_url' "$R"

# ---- 5. 上传第二个文件 ----
blue "5. 测试上传第二个文件..."
echo '{"func":"main","cpu":99.9}' > /tmp/w4_test_top.json
curl -s -X POST "$BASE/api/v1/cosfiles/upload" \
  -F "tid=tid-w4-test" \
  -F "file=@/tmp/w4_test_top.json" > /dev/null

R=$(curl -s "$BASE/api/v1/cosfiles?tid=tid-w4-test")
check "total=2" '"total":2' "$R"

# ---- 6. 任务详情含文件 ----
blue "6. 测试任务详情含产物文件..."
# 创建任务
R=$(curl -s -X POST "$BASE/api/v1/tasks" \
  -H "Content-Type: application/json" \
  -H "Drop_user_uid: user-001" \
  -H "Drop_user_name: Alice" \
  -d '{"name":"W4测试","task_type":0,"profiler_type":0,"target_ip":"127.0.0.1","target_pid":1,"duration":5}')
TID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['tid'])" 2>/dev/null)

# 上传文件到该任务
curl -s -X POST "$BASE/api/v1/cosfiles/upload" \
  -F "tid=$TID" \
  -F "file=@/tmp/w4_test_file.txt" > /dev/null

# 查任务详情
R=$(curl -s "$BASE/api/v1/tasks/$TID")
check "任务详情含 task" '"task":' "$R"
check "任务详情含 files" '"files":' "$R"

# ---- 7. 空 tid 列表 ----
blue "7. 测试空 tid 的文件列表..."
R=$(curl -s "$BASE/api/v1/cosfiles?tid=tid-nonexistent")
check "空列表 code=0" '"code":0' "$R"
check "空列表 total=0" '"total":0' "$R"

# ---- 8. 缺参数校验 ----
blue "8. 测试参数校验..."
R=$(curl -s "$BASE/api/v1/cosfiles")
check "缺 tid 返回 400" '"code":400' "$R"

R=$(curl -s -X POST "$BASE/api/v1/cosfiles/upload" -F "file=@/tmp/w4_test_file.txt")
check "上传缺 tid 返回 400" '"code":400' "$R"

# ---- 9. W2 回归 ----
blue "9. W2 回归测试..."
R=$(curl -s "$BASE/healthz")
check "healthz ok" '"status":"ok"' "$R"

R=$(curl -s -H "Drop_user_uid: user-001" "$BASE/api/v1/agents")
check "agents ok" '"code":0' "$R"

# ---- 结果汇总 ----
echo ""
echo "============================================"
TOTAL=$((PASS + FAIL))
green "通过: $PASS / $TOTAL"
if [ "$FAIL" -gt 0 ]; then
    red "失败: $FAIL"
    echo ""
    echo "=== apiserver 日志 ==="
    tail -15 /tmp/apiserver_w4_test.log
    exit 1
else
    green "🎉 W4 验收成功！MinIO 存储集成通过！"
fi

# 清理
pkill -f "./apiserver" 2>/dev/null || true
