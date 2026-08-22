#!/usr/bin/env bash
set -Eeuo pipefail

# ============================================================
# 数据库性能采集（DBSnapshotSampler）端到端验收
# ============================================================
# 覆盖阶段一（标量指标）+ 阶段二（慢查询 digest + 锁等待链）：
#   1. 起一个 MySQL 目标容器（bridge + 端口映射，agent 是 host 网络，
#      所以 db_targets.host 用 127.0.0.1）
#   2. 建只读账号（SHOW GLOBAL STATUS 要 PROCESS，digest/锁要
#      performance_schema / sys 读权限）
#   3. agent 容器内放 password_ref 密码文件
#   4. POST /api/v1/continuous/sessions 手填 labels.db_targets 创建 Session
#   5. 等第一个窗口（digest 首窗口是基线不上报）后造慢查询/锁等待数据
#   6. 等增量窗口，查 /api/v1/continuous/db-snapshot 断言 digest 与锁等待
#
# 环境变量可覆盖：
#   BASE        apiserver 地址（默认 http://127.0.0.1:8191）
#   TARGET_IP   agent 自报 IP（默认 111.230.29.115，见 docker-compose DROP_AGENT_IP）
#   AUTH_UID / AUTH_NAME  鉴权头（默认 dba-accept）
#   AGENT       agent 容器名（默认 mini-drop-drop_agent-1）
#   MYSQL_IMAGE MySQL 镜像（默认 mysql:8）
#   LOCK_WAIT_TEST  1 时额外跑锁等待场景（默认 1，比 digest 更脆弱）
# ============================================================

BASE=${BASE:-http://127.0.0.1:8191}
TARGET_IP=${TARGET_IP:-111.230.29.115}
AUTH_UID=${AUTH_UID:-dba-accept}
AUTH_NAME=${AUTH_NAME:-dba-accept}
AGENT=${AGENT:-mini-drop-drop_agent-1}
MYSQL_IMAGE=${MYSQL_IMAGE:-mysql:8}
LOCK_WAIT_TEST=${LOCK_WAIT_TEST:-1}
MYSQL_CTN="fault-mysql-dba-$$"
PASSWD_FILE="/etc/mini-drop/db-credentials.d/orders-mysql.env"
SID=""

pass() { printf 'PASS %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1" >&2; exit 1; }

cleanup() {
    set +e
    [[ -n "$SID" ]] && curl -sS -X POST "$BASE/api/v1/continuous/sessions/$SID/stop" \
        -H "Drop-User-Uid: $AUTH_UID" -H "Drop-User-Name: $AUTH_NAME" >/dev/null 2>&1
    docker rm -f "$MYSQL_CTN" >/dev/null 2>&1
    docker exec "$AGENT" rm -f "$PASSWD_FILE" >/dev/null 2>&1
}
trap cleanup EXIT

json_value() {
    python3 - "$1" "$2" <<'PY'
import json, sys
body = json.load(open(sys.argv[1], encoding="utf-8"))
value = body
for part in sys.argv[2].split('.'):
    value = value[part]
print(value)
PY
}

# ---------- 1. 起 MySQL 目标 ----------
printf '[dba] run=%s target=%s\n' "dba-$$" "$TARGET_IP"
curl -fsS "$BASE/healthz" >/dev/null || fail "apiserver healthz"
docker rm -f "$MYSQL_CTN" >/dev/null 2>&1 || true
docker run -d --name "$MYSQL_CTN" -p 3306:3306 -e MYSQL_ROOT_PASSWORD=root "$MYSQL_IMAGE" >/dev/null \
    || fail "start MySQL container"
# mysqladmin ping 只探测服务器存活、不校验凭据，MySQL 初始化临时服务器阶段也会
# 返回成功（此时 root 密码尚未生效），所以用 mysql -e "SELECT 1" 验证真正可认证。
for _ in $(seq 1 30); do
    docker exec "$MYSQL_CTN" mysql -uroot -proot -e "SELECT 1" >/dev/null 2>&1 && break
    sleep 2
done
docker exec "$MYSQL_CTN" mysql -uroot -proot -e "SELECT 1" >/dev/null 2>&1 \
    || fail "MySQL did not become ready"
pass "MySQL target ready"

# ---------- 2. 建只读账号 + 前置条件检查 ----------
# 错误不吞：失败时把 stderr 打出来，便于定位是 Access denied 还是 sys schema 缺失。
if ! docker exec "$MYSQL_CTN" mysql -uroot -proot -e "
    CREATE USER IF NOT EXISTS 'mini_drop'@'%' IDENTIFIED BY 'md_pass';
    GRANT PROCESS ON *.* TO 'mini_drop'@'%';
    GRANT SELECT ON performance_schema.* TO 'mini_drop'@'%';
    GRANT SELECT ON sys.* TO 'mini_drop'@'%';
    FLUSH PRIVILEGES;" 2>&1; then
    fail "create mini_drop account"
fi
docker exec "$MYSQL_CTN" mysql -uroot -proot -N -e \
    "SELECT @@performance_schema; SHOW DATABASES LIKE 'sys';" 2>/dev/null | grep -q "sys" \
    || fail "performance_schema/sys schema not available"
pass "mini_drop account + sys schema ready"

# ---------- 3. agent 本机放密码文件 ----------
docker exec "$AGENT" sh -c \
    "mkdir -p /etc/mini-drop/db-credentials.d && \
     printf 'md_pass' > '$PASSWD_FILE' && chmod 600 '$PASSWD_FILE'" \
    || fail "write password_ref file"
pass "password_ref file staged in agent"

# ---------- 4. 创建 Session ----------
cat > /tmp/dba-session-$$.json <<JSON
{
  "name": "dba-accept-mysql-$$",
  "target_ip": "$TARGET_IP",
  "hostname": "cloud-server-111-230-29-115",
  "scope": "host",
  "signals": ["cpu_profile", "io_latency", "sched_latency"],
  "sample_rate_hz": 9,
  "aggregation_window_sec": 10,
  "upload_batch_sec": 30,
  "retention_hours": 1,
  "continuity_mode": "strict",
  "allow_degraded": true,
  "labels": {
    "db_targets": [{
      "engine": "mysql",
      "instance_label": "orders-mysql",
      "host": "127.0.0.1",
      "port": 3306,
      "user": "mini_drop",
      "password_ref": "$PASSWD_FILE",
      "poll_interval_sec": 10,
      "query_timeout_ms": 500
    }]
  }
}
JSON
code=$(curl -sS -o /tmp/dba-session-resp-$$.json -w '%{http_code}' -X POST \
    "$BASE/api/v1/continuous/sessions" \
    -H "Drop-User-Uid: $AUTH_UID" -H "Drop-User-Name: $AUTH_NAME" \
    -H "Content-Type: application/json" -d @/tmp/dba-session-$$.json)
[[ "$code" == "200" ]] || { cat /tmp/dba-session-resp-$$.json >&2; fail "create session HTTP $code"; }
SID=$(json_value /tmp/dba-session-resp-$$.json data.session.sid)
[[ -n "$SID" && "$SID" != "None" ]] || fail "empty session sid"
pass "session created sid=$SID"

# ---------- 5. 等第一个窗口（digest 基线窗口，不上报） ----------
sleep 14
pass "baseline window elapsed (digest first-window baseline)"

# ---------- 6. 造慢查询数据 ----------
docker exec "$MYSQL_CTN" mysql -uroot -proot -e "
    CREATE DATABASE IF NOT EXISTS orders;
    USE orders;
    CREATE TABLE IF NOT EXISTS order_line(
        id INT PRIMARY KEY AUTO_INCREMENT,
        status VARCHAR(10),
        created_at DATETIME);
    INSERT INTO order_line(status, created_at)
        VALUES ('new', NOW()),('new', NOW()),('ship', NOW());
    SELECT COUNT(*) FROM order_line WHERE status='new';" >/dev/null 2>&1 \
    || fail "seed slow-query data"
pass "slow-query data seeded"

# ---------- 7.（可选）造锁等待 ----------
if [[ "$LOCK_WAIT_TEST" == "1" ]]; then
    # 连接 A：持锁 25s（事务保持打开），连接 B 紧接着 UPDATE 同一行被阻塞。
    docker exec "$MYSQL_CTN" mysql -uroot -proot -e \
        "USE orders; START TRANSACTION; UPDATE order_line SET status='lock' WHERE id=1; SELECT SLEEP(25);" \
        >/dev/null 2>&1 &
    LOCKER_PID=$!
    sleep 2
    docker exec "$MYSQL_CTN" mysql -uroot -proot -e \
        "USE orders; UPDATE order_line SET status='blocked' WHERE id=1;" >/dev/null 2>&1 &
    BLOCKED_PID=$!
    pass "lock-wait scenario injected (pid $LOCKER_PID / $BLOCKED_PID)"
fi

# ---------- 8. 等增量窗口（让造的数据产生 digest 增量并上报） ----------
sleep 24
wait "$LOCKER_PID" "$BLOCKED_PID" >/dev/null 2>&1 || true

# ---------- 9. 查询并断言 ----------
FROM_MS=$(( $(date +%s%3N) - 120000 ))
TO_MS=$(date +%s%3N)
resp=$(curl -sS "$BASE/api/v1/continuous/db-snapshot?session_sid=$SID&host=$TARGET_IP&from=$FROM_MS&to=$TO_MS" \
    -H "Drop-User-Uid: $AUTH_UID" -H "Drop-User-Name: $AUTH_NAME")
echo "$resp" > /tmp/dba-snapshot-$$.json

digest_count=$(python3 - /tmp/dba-snapshot-$$.json <<'PY'
import json, sys
body = json.load(open(sys.argv[1], encoding="utf-8"))
print(len(body.get("digests") or []))
PY
)
lock_count=$(python3 - /tmp/dba-snapshot-$$.json <<'PY'
import json, sys
body = json.load(open(sys.argv[1], encoding="utf-8"))
print(len(body.get("lock_waits") or []))
PY
)

[[ "$digest_count" -gt 0 ]] || fail "no digest captured (got empty digests)"
pass "digest captured: $digest_count entries"

if [[ "$LOCK_WAIT_TEST" == "1" ]]; then
    [[ "$lock_count" -gt 0 ]] || fail "no lock-wait captured"
    pass "lock-wait captured: $lock_count entries"
else
    printf 'SKIP lock-wait assertion (LOCK_WAIT_TEST=0)\n'
fi

printf '[dba] completed run=%s sid=%s digest=%s lock=%s\n' "dba-$$" "$SID" "$digest_count" "$lock_count"
