#!/usr/bin/env bash
# 由本地 deploy.sh 通过 SSH 注入。服务器只从 origin 获取源码。

set -euo pipefail

REMOTE_PATH="${REMOTE_PATH:?REMOTE_PATH required}"
DEPLOY_BRANCH="${DEPLOY_BRANCH:?DEPLOY_BRANCH required}"
TARGET_SHA="${TARGET_SHA:?TARGET_SHA required}"
FETCH_TIMEOUT="${FETCH_TIMEOUT:-60}"
TEST_SCOPE="${TEST_SCOPE:-full}"
LOCK_FILE="${DEPLOY_LOCK_FILE:-/tmp/mini-drop-deploy.lock}"
HEALTH_URL="${DEPLOY_HEALTH_URL:-http://127.0.0.1:8191}"
HEALTH_TRIES="${DEPLOY_HEALTH_TRIES:-30}"
HEALTH_INTERVAL="${DEPLOY_HEALTH_INTERVAL:-2}"

fail() {
  printf '[STAGE:%s] FAIL %s\n' "$1" "$2" >&2
  exit "${3:-1}"
}

run_with_timeout() {
  if command -v timeout >/dev/null 2>&1; then
    timeout "$@"
  else
    local seconds="$1"
    shift
    perl -e 'alarm shift; exec @ARGV' "${seconds}" "$@"
  fi
}

git check-ref-format --branch "${DEPLOY_BRANCH}" >/dev/null 2>&1 \
  || fail CONFIG "非法分支名: ${DEPLOY_BRANCH}" 2
[[ "${TARGET_SHA}" =~ ^[0-9a-fA-F]{40,64}$ ]] \
  || fail CONFIG "非法目标 SHA" 2
case "${TEST_SCOPE}" in
  full|smoke|none) ;;
  *) fail CONFIG "未知 TEST_SCOPE=${TEST_SCOPE}" 2 ;;
esac

command -v flock >/dev/null 2>&1 || fail CONFIG "服务器缺少 flock"
exec 9>"${LOCK_FILE}"
flock -n 9 || fail SERVER_LOCK "另一场部署正在进行"

cd "${REMOTE_PATH}" || fail SERVER_PATH "目录不存在: ${REMOTE_PATH}"
git rev-parse --is-inside-work-tree >/dev/null 2>&1 \
  || fail SERVER_PATH "不是 Git 工作树: ${REMOTE_PATH}"

check_clean_tree() {
  local status
  status="$(git status --porcelain --untracked-files=all)"
  [[ -z "${status}" ]] || {
    printf '%s\n' "${status}" >&2
    fail SERVER_CLEAN "服务器工作树不干净；拒绝切换分支"
  }
}

check_clean_tree

REFSPEC="+refs/heads/${DEPLOY_BRANCH}:refs/remotes/origin/${DEPLOY_BRANCH}"
run_with_timeout "${FETCH_TIMEOUT}" git fetch --prune origin "${REFSPEC}" \
  || fail SERVER_FETCH "无法从 origin 拉取 ${DEPLOY_BRANCH}；禁止使用本地传输回退"

ORIGIN_SHA="$(git rev-parse "refs/remotes/origin/${DEPLOY_BRANCH}")"
[[ "${ORIGIN_SHA}" == "${TARGET_SHA}" ]] \
  || fail SERVER_RACE "远程分支在部署期间发生变化（期望 ${TARGET_SHA:0:12}，实际 ${ORIGIN_SHA:0:12}）；请重试"

# 服务器本地分支只是部署游标。工作树已确认干净，可精确对齐远程分支。
git switch --force-create "${DEPLOY_BRANCH}" --track "origin/${DEPLOY_BRANCH}" >/dev/null \
  || fail SERVER_SWITCH "无法切换到 origin/${DEPLOY_BRANCH}"

HEAD_NOW="$(git rev-parse HEAD)"
[[ "${HEAD_NOW}" == "${TARGET_SHA}" ]] \
  || fail SERVER_VERIFY "切换后 HEAD 与 origin/${DEPLOY_BRANCH} 不一致"
check_clean_tree
printf '[STAGE:UPDATE] PASS branch=%s sha=%s\n' "${DEPLOY_BRANCH}" "${HEAD_NOW:0:12}"

run_stage() {
  local name="$1"
  shift
  printf '==> [%s] 开始\n' "${name}"
  # 子进程关闭锁 FD；若 SSH/父 shell 被取消，锁会立即释放，不会被孤儿构建继承。
  if "$@" 9>&-; then
    printf '[STAGE:%s] PASS\n' "${name}"
  else
    fail "${name}" "阶段失败，branch=${DEPLOY_BRANCH} sha=${TARGET_SHA:0:12}" 20
  fi
}

case "${TEST_SCOPE}" in
  full)
    run_stage TESTS make test
    run_stage COVERAGE make coverage
    ;;
  smoke)
    printf '[STAGE:TESTS] SKIP scope=smoke\n'
    ;;
  none)
    printf '[STAGE:TESTS] SKIP scope=none\n'
    ;;
esac

run_stage BUILD docker compose build
run_stage UP docker compose up -d

health_check() {
  local attempt exited
  for attempt in $(seq 1 "${HEALTH_TRIES}"); do
    exited="$(docker compose ps --status exited --services | grep -vx 'minio-init' || true)"
    [[ -z "${exited}" ]] || {
      printf '异常退出的服务:\n%s\n' "${exited}" >&2
      return 1
    }
    if curl -fsS "${HEALTH_URL}/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep "${HEALTH_INTERVAL}"
  done
  return 1
}

run_stage HEALTH health_check
run_stage E2E bash scripts/e2e_smoke.sh
# 阶段四：TEST_SCOPE=full 时在健康检查后运行多语言持续采集 E2E
# （Native/Go/Java/Node/Python 业务热点、v2 语言状态、runtime 筛选一致性、
# 窗口幂等）。任何质量门槛失败都使部署失败。
if [[ "${TEST_SCOPE}" == "full" ]]; then
  # full 验收同时验证 Agent 容器重建后的 Session 原地恢复。
  run_stage E2E_MULTILANG env RESTART_AGENT=1 bash scripts/continuous_process_multilang_e2e.sh
fi
check_clean_tree
printf '[STAGE:ALL] PASS branch=%s sha=%s\n' "${DEPLOY_BRANCH}" "${TARGET_SHA:0:12}"
