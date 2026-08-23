#!/usr/bin/env bash
# ============================================================
# 数据库性能采集开销对比测试（导师要求 1：端到端开销对比）
# ============================================================
# 同一台 VM、相同 CPU profiling 配置，仅差异 db_targets：
#   baseline : 无 db 采集（纯 CPU profiling）
#   withdb   : 带 db_targets（CPU profiling + DBSnapshotSampler）+ MySQL 持续流量
# 对比指标：
#   - agent 容器 CPU 均值 / 内存均值（docker stats 采样取平均）
#   - cpu_profile / io / sched 窗口生成速率（profile_windows 数量）
#   - cpu 采样质量（sample_count 均值）
#   - withdb 轮额外验证 db_snapshot 窗口出现（链路确实工作）
#
# 用法:
#   bash db_overhead_compare.sh baseline   # 阶段 A
#   bash db_overhead_compare.sh withdb     # 阶段 B
# 环境变量可覆盖：
#   RUN_SEC    总运行时长（默认 180）
#   SAMPLE_N   docker stats 采样次数（默认 24）
#   SAMPLE_INT 采样间隔秒（默认 4）
# ============================================================

set -Eeuo pipefail

MODE="${1:?usage: $0 baseline|withdb}"
BASE="${BASE:-http://127.0.0.1:8191}"
AUTH_UID="${AUTH_UID:-dba-accept}"
AUTH_NAME="${AUTH_NAME:-dba-accept}"
TARGET_IP="${TARGET_IP:-111.230.29.115}"
AGENT="${AGENT:-mini-drop-drop_agent-1}"
MYSQL_CTN="${MYSQL_CTN:-fault-mysql}"
PASSWD_FILE="${PASSWD_FILE:-/etc/mini-drop/db-credentials.d/orders-mysql.env}"
RUN_SEC="${RUN_SEC:-180}"
SAMPLE_N="${SAMPLE_N:-24}"
SAMPLE_INT="${SAMPLE_INT:-4}"
RUN_TAG="ovh-$(date +%H%M%S)-$$"
SID=""

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

stop_all_sessions() {
    local sids
    sids=$(curl -s "$BASE/api/v1/continuous/sessions" \
        -H "Drop-User-Uid: $AUTH_UID" -H "Drop-User-Name: $AUTH_NAME" \
        | python3 -c '
import json, sys
body = json.load(sys.stdin)
items = body.get("data", {})
if isinstance(items, dict):
    items = items.get("sessions", [])
for s in items:
    if s.get("desired_state") == "running":
        print(s.get("sid"))
' 2>/dev/null || true)
    for sid in $sids; do
        echo "[prep] stopping stale session $sid"
        curl -sS -X POST "$BASE/api/v1/continuous/sessions/$sid/stop" \
            -H "Drop-User-Uid: $AUTH_UID" -H "Drop-User-Name: $AUTH_NAME" >/dev/null 2>&1 || true
    done
    sleep 3
}

create_session() {
    local labels="{}"
    if [[ "$MODE" == "withdb" ]]; then
        labels='{"db_targets": [{"engine": "mysql", "instance_label": "orders-mysql", "host": "127.0.0.1", "port": 3306, "user": "mini_drop", "password_ref": "'"$PASSWD_FILE"'", "poll_interval_sec": 10, "query_timeout_ms": 500}]}'
    fi
    cat > /tmp/ovh-session-$$.json <<JSON
{
  "name": "ovh-$MODE-$RUN_TAG",
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
  "labels": $labels
}
JSON
    local code
    code=$(curl -sS -o /tmp/ovh-session-resp-$$.json -w '%{http_code}' -X POST \
        "$BASE/api/v1/continuous/sessions" \
        -H "Drop-User-Uid: $AUTH_UID" -H "Drop-User-Name: $AUTH_NAME" \
        -H "Content-Type: application/json" -d @/tmp/ovh-session-$$.json)
    if [[ "$code" != "200" ]]; then
        cat /tmp/ovh-session-resp-$$.json >&2
        return 1
    fi
    SID=$(json_value /tmp/ovh-session-resp-$$.json data.session.sid)
    echo "[session] created sid=$SID mode=$MODE"
}

start_mysql_traffic() {
    # 轻量查询走主键，产生 digest 增量即可（开销对比不测 MySQL 负载）
    ( for _ in $(seq 1 200); do
          docker exec "$MYSQL_CTN" mysql -uroot -proot orders -N -e \
              "SELECT id,status FROM order_line WHERE id=1;" >/dev/null 2>&1 || true
          sleep 1
      done ) &
    TRAFFIC_PID=$!
    echo "[traffic] mysql light query loop started pid=$TRAFFIC_PID"
}

stop_mysql_traffic() {
    [[ -n "${TRAFFIC_PID:-}" ]] && kill "$TRAFFIC_PID" >/dev/null 2>&1 || true
}

sample_agent() {
    local total_cpu=0 total_mem=0 n=0 i=1
    for _ in $(seq 1 "$SAMPLE_N"); do
        read -r cpu mem <<< "$(docker stats --no-stream --format '{{.CPUPerc}} {{.MemUsage}}' "$AGENT" 2>/dev/null | tr -d '%')"
        mem_mb=$(awk '{print $1}' <<< "$mem")
        [[ -n "$cpu" ]] && { total_cpu=$(awk -v a="$total_cpu" -v b="$cpu" 'BEGIN{print a+b}'); total_mem=$(awk -v a="$total_mem" -v b="$mem_mb" 'BEGIN{print a+b}'); n=$((n+1)); }
        sleep "$SAMPLE_INT"
    done
    if [[ "$n" -gt 0 ]]; then
        awk -v c="$total_cpu" -v m="$total_mem" -v n="$n" 'BEGIN{printf "avg_cpu_pct=%.2f avg_mem_mb=%.2f samples=%d", c/n, m/n, n}'
    fi
}

query_windows() {
    local sid="$1"
    docker exec mini-drop-postgres-1 psql -U postgres -d drop -t -A -c \
        "SELECT signal_type || '|' || count(*) || '|' || round(coalesce(avg(sample_count),0)) 
         FROM profile_windows 
         WHERE session_sid='$sid' 
         GROUP BY signal_type ORDER BY signal_type;" 2>/dev/null
}

# ---------- 主流程 ----------
echo "===== 开销对比测试 [$MODE] $RUN_TAG ====="
stop_all_sessions

if [[ "$MODE" == "withdb" ]]; then
    docker exec "$AGENT" sh -c "mkdir -p /etc/mini-drop/db-credentials.d && printf 'md_pass' > '$PASSWD_FILE' && chmod 600 '$PASSWD_FILE'" \
        || echo "[warn] password_ref rewrite failed"
    start_mysql_traffic
fi

create_session || { stop_mysql_traffic; exit 1; }

# 预热：digest 首窗口基线不上报 + batch 攒批 30s
echo "[wait] warmup 40s ..."
sleep 40

# 中段采样
echo "[sample] sampling agent $SAMPLE_N x ${SAMPLE_INT}s ..."
SAMPLE_RESULT="$(sample_agent)"
echo "[sample] $SAMPLE_RESULT"

# 跑满剩余时长
ELAPSED=$((40 + SAMPLE_N * SAMPLE_INT))
REMAIN=$(( RUN_SEC - ELAPSED ))
if [[ "$REMAIN" -gt 0 ]]; then
    echo "[wait] remaining ${REMAIN}s ..."
    sleep "$REMAIN"
fi

echo "[windows] profile_windows for sid=$SID:"
query_windows "$SID"

# 停 Session
curl -sS -X POST "$BASE/api/v1/continuous/sessions/$SID/stop" \
    -H "Drop-User-Uid: $AUTH_UID" -H "Drop-User-Name: $AUTH_NAME" >/dev/null 2>&1 || true
echo "[done] session stopped"

stop_mysql_traffic
echo "===== [$MODE] finished ====="
