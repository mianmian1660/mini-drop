#!/bin/bash
# 追加一个最终观察样本到指定观察日志（引号安全版：避免 awk/$ 转义问题）
set -uo pipefail
LOG="${1:-/home/ubuntu/mini-drop-shared/deploy-state/observation/latest.log}"

ts="$(date +%Y-%m-%dT%H:%M:%S%z)"
avail="$(df -B1 --output=avail / | tail -1 | tr -d ' ')"
images="$(docker system df 2>/dev/null | grep '^Images ' | tr -s ' ' | cut -d' ' -f3,5 | sed 's/ /\//')"
bc="$(docker system df 2>/dev/null | grep '^Build Cache' | tr -s ' ' | cut -d' ' -f4)"
pgv="$(docker system df -v 2>/dev/null | grep -w 'mini-drop_pgdata' | tr -s ' ' | cut -d' ' -f3)"
miniov="$(docker system df -v 2>/dev/null | grep -w 'mini-drop_miniodata' | tr -s ' ' | cut -d' ' -f3)"
spool="$(sudo du -sb /var/lib/mini-drop/continuous-spool 2>/dev/null | cut -f1 || echo 0)"
sym="$(docker exec mini-drop-drop_agent-1 du -sb /var/lib/mini-drop/symbol-cache 2>/dev/null | cut -f1 || echo 0)"
rej="$(curl -fsS http://127.0.0.1:8191/metrics 2>/dev/null | grep '^mini_drop_collection_rejected_low_disk_total{' | cut -d' ' -f2 | python3 -c 'import sys; print(sum(int(x) for x in sys.stdin if x.strip()) or 0)')"
ret_err="$(docker logs mini-drop-apiserver-1 --since 15m 2>/dev/null | grep -cE '"level":"error"' || true)"
comp_err="$(docker logs mini-drop-apiserver-1 --since 15m 2>/dev/null | grep -cE 'compaction skip|compaction.*(error|失败)' || true)"
restarts=0
for c in $(docker ps -aq --filter 'name=mini-drop-'); do
  r="$(docker inspect -f '{{.RestartCount}}' "$c" 2>/dev/null || echo 0)"
  restarts=$((restarts + r))
done

printf '%s avail=%s images=%s build_cache=%s pg_vol=%s minio_vol=%s spool=%s symbol_cache=%s rejected_total=%s apiserver_errors_15m=%s compactor_issues_15m=%s container_restarts=%s\n' \
  "${ts}" "${avail}" "${images}" "${bc}" "${pgv}" "${miniov}" "${spool}" "${sym}" "${rej}" "${ret_err}" "${comp_err}" "${restarts}" | tee -a "${LOG}"
