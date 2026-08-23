#!/bin/bash
# ==============================================================================
# server-observe-phase0.sh — 阶段 0 发布后观察（服务器端）
# ==============================================================================
# 每 interval_min 分钟记录一次以下指标到观察日志，持续 duration_min 分钟：
#   根盘剩余空间 / Docker images+build cache / PG·MinIO volume / spool·符号缓存 /
#   采集拒收计数 / retention·compactor 错误 / 容器重启次数
#
# 用法：
#   bash server-observe-phase0.sh [duration_min] [interval_min]   # 循环采样
#   bash server-observe-phase0.sh once                            # 只采样一次
# 日志：/home/ubuntu/mini-drop-shared/deploy-state/observation/<ts>.log
# ==============================================================================
set -uo pipefail

DURATION_MIN="${1:-120}"
INTERVAL_MIN="${2:-15}"
STATE_DIR="/home/ubuntu/mini-drop-shared/deploy-state"
OBS_DIR="${STATE_DIR}/observation"
mkdir -p "${OBS_DIR}"
LOG="${OBS_DIR}/$(date +%Y%m%d-%H%M%S).log"

sample() {
  local ts avail images bc pgv miniov spool sym rej ret_err comp_err restarts
  ts="$(date +%Y-%m-%dT%H:%M:%S%z)"
  avail="$(df -B1 --output=avail / | tail -1 | tr -d ' ')"
  images="$(docker system df 2>/dev/null | awk '$1=="Images"{print $3"/"$5}')"
  bc="$(docker system df 2>/dev/null | awk '$1=="Build"{print $4}')"
  pgv="$(docker system df -v 2>/dev/null | awk -v v="mini-drop_pgdata" '$1==v{print $3}')"
  miniov="$(docker system df -v 2>/dev/null | awk -v v="mini-drop_miniodata" '$1==v{print $3}')"
  spool="$(sudo du -sb /var/lib/mini-drop/continuous-spool 2>/dev/null | awk '{print $1}' || echo 0)"
  sym="$(docker exec mini-drop-drop_agent-1 du -sb /var/lib/mini-drop/symbol-cache 2>/dev/null | awk '{print $1}' || echo 0)"
  rej="$(curl -fsS http://127.0.0.1:8191/metrics 2>/dev/null | grep '^mini_drop_collection_rejected_low_disk_total{' | awk -F' ' '{s+=$2} END{print s+0}')"
  ret_err="$(docker logs mini-drop-apiserver-1 --since "${INTERVAL_MIN}m" 2>/dev/null | grep -cE '"level":"error"' || true)"
  comp_err="$(docker logs mini-drop-apiserver-1 --since "${INTERVAL_MIN}m" 2>/dev/null | grep -cE 'compaction skip|compaction.*(error|失败)' || true)"
  restarts=0
  for c in $(docker ps -aq --filter "name=mini-drop-"); do
    r="$(docker inspect -f '{{.RestartCount}}' "$c" 2>/dev/null || echo 0)"
    restarts=$((restarts + r))
  done
  printf '%s avail=%s images=%s build_cache=%s pg_vol=%s minio_vol=%s spool=%s symbol_cache=%s rejected_total=%s apiserver_errors_15m=%s compactor_issues_15m=%s container_restarts=%s\n' \
    "${ts}" "${avail}" "${images}" "${bc}" "${pgv}" "${miniov}" "${spool}" "${sym}" "${rej}" "${ret_err}" "${comp_err}" "${restarts}" | tee -a "${LOG}"
}

if [[ "${1:-}" == "once" ]]; then
  LOG="${OBS_DIR}/$(date +%Y%m%d-%H%M%S)-once.log"
  sample
  exit 0
fi

echo "观察开始：每 ${INTERVAL_MIN} 分钟一次，共 ${DURATION_MIN} 分钟 → ${LOG}" | tee -a "${LOG}"
end=$(( $(date +%s) + DURATION_MIN * 60 ))
while :; do
  sample
  now="$(date +%s)"
  [[ now -ge end ]] && break
  sleep "$(( INTERVAL_MIN * 60 ))"
done
echo "观察结束：${LOG}" | tee -a "${LOG}"
