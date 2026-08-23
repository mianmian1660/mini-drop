#!/bin/bash
# ==============================================================================
# switch-to-release-phase0.sh — 无重建切回指定 release（本地编排）
# ==============================================================================
# 场景：rollback 之后要把服务切回候选 release，且不重新构建——
#   1. 读取 release.json 中记录的 image_ids，重新标记为 <project>-<service>:latest
#   2. 用该 release 的 compose + production.env 执行 --no-deps --no-build --force-recreate
#   3. 更新 current.txt 与 mini-drop-current 软链
#   4. 健康检查（apiserver /healthz、drop_agent 运行、web /health）
#
# 用法：
#   bash scripts/switch-to-release-phase0.sh [release_dir]   # 缺省用 mini-drop-current
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REMOTE_HOST=""
if [[ -f "${SCRIPT_DIR}/../sync.env" ]]; then
  set -a; source "${SCRIPT_DIR}/../sync.env"; set +a
fi
REMOTE_HOST="${SYNC_REMOTE_HOST:-${REMOTE_HOST:-}}"
[[ -n "${REMOTE_HOST}" ]] || { echo "❌ 请先在 sync.env 配置 SYNC_REMOTE_HOST"; exit 1; }

RELEASE_DIR="${1:-/home/ubuntu/mini-drop-current}"

say() { printf '[switch] %s\n' "$*"; }
die() { printf '[switch] ❌ %s\n' "$*" >&2; exit 1; }

say "目标 release: ${RELEASE_DIR}"
ssh "${REMOTE_HOST}" "bash -s" -- "${RELEASE_DIR}" <<'REMOTE'
set -euo pipefail
RELEASE_DIR="$1"
PROJECT="mini-drop"
SHARED="/home/ubuntu/mini-drop-shared"
STATE_DIR="${SHARED}/deploy-state"
PROD_ENV="${SHARED}/production.env"

[[ -f "${RELEASE_DIR}/release.json" ]] || { echo "❌ release.json 不存在: ${RELEASE_DIR}"; exit 1; }
[[ -f "${RELEASE_DIR}/docker-compose.yml" ]] || { echo "❌ compose 不存在"; exit 1; }

API_IMG="$(python3 -c "import json;print(json.load(open('${RELEASE_DIR}/release.json'))['image_ids']['apiserver'])")"
WEB_IMG="$(python3 -c "import json;print(json.load(open('${RELEASE_DIR}/release.json'))['image_ids']['web_frontend'])")"

echo "== 重新标记候选镜像 =="
docker tag "${API_IMG}" "${PROJECT}-apiserver:latest"
docker tag "${WEB_IMG}" "${PROJECT}-web_frontend:latest"
echo "  apiserver ← ${API_IMG}"
echo "  web_frontend ← ${WEB_IMG}"

echo "== compose config --quiet =="
docker compose -p "${PROJECT}" --env-file "${PROD_ENV}" -f "${RELEASE_DIR}/docker-compose.yml" config --quiet

echo "== 切换（--no-deps --no-build --force-recreate）=="
docker compose -p "${PROJECT}" --env-file "${PROD_ENV}" -f "${RELEASE_DIR}/docker-compose.yml" \
  up -d --no-deps --no-build --force-recreate apiserver drop_agent web_frontend

echo "== 健康检查 =="
ok=0; for i in $(seq 1 30); do
  curl -fsS http://127.0.0.1:8191/healthz 2>/dev/null | grep -q '"status":"ok"' && { ok=1; break; }
  sleep 2
done
[[ "$ok" == "1" ]] || { echo "❌ apiserver /healthz 不健康"; exit 1; }
ok=0; for i in $(seq 1 30); do
  docker inspect -f '{{.State.Running}}' "${PROJECT}-drop_agent-1" 2>/dev/null | grep -q true && { ok=1; break; }
  sleep 2
done
[[ "$ok" == "1" ]] || { echo "❌ drop_agent 未运行"; exit 1; }
curl -fsS http://127.0.0.1/health >/dev/null || { echo "❌ web /health 异常"; exit 1; }

echo "== 更新 current =="
ln -sfn "${RELEASE_DIR}" /home/ubuntu/mini-drop-current
printf '%s\n' "${RELEASE_DIR}/docker-compose.yml" > "${STATE_DIR}/current.txt"
echo "✅ 切回完成：${RELEASE_DIR}"
REMOTE
