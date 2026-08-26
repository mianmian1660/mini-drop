#!/usr/bin/env bash
# 由本地 deploy.sh 通过 SSH 注入。服务器只从 origin 获取源码。

set -euo pipefail

REMOTE_PATH="${REMOTE_PATH:?REMOTE_PATH required}"
DEPLOY_BRANCH="${DEPLOY_BRANCH:?DEPLOY_BRANCH required}"
TARGET_SHA="${TARGET_SHA:?TARGET_SHA required}"
SOURCE_REMOTE_URL="${SOURCE_REMOTE_URL:-}"
FETCH_TIMEOUT="${FETCH_TIMEOUT:-60}"
TEST_SCOPE="${TEST_SCOPE:-full}"
SERVICE_SCOPE="${SERVICE_SCOPE:-full}"
LOCK_FILE="${DEPLOY_LOCK_FILE:-/tmp/mini-drop-deploy.lock}"
HEALTH_URL="${DEPLOY_HEALTH_URL:-http://127.0.0.1:8191}"
FRONTEND_URL="${DEPLOY_FRONTEND_URL:-http://127.0.0.1}"
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
case "${SERVICE_SCOPE}" in
  full|frontend|agent) ;;
  *) fail CONFIG "未知 SERVICE_SCOPE=${SERVICE_SCOPE}" 2 ;;
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
if ! run_with_timeout "${FETCH_TIMEOUT}" git fetch --prune origin "${REFSPEC}"; then
  [[ -n "${SOURCE_REMOTE_URL}" ]] \
    || fail SERVER_FETCH "无法从服务器 origin 拉取 ${DEPLOY_BRANCH}，且没有可用的源仓库地址"
  printf '服务器 origin 拉取失败，改用已验证目标 SHA 的源仓库地址重试\n' >&2
  run_with_timeout "${FETCH_TIMEOUT}" git fetch --prune "${SOURCE_REMOTE_URL}" "${REFSPEC}" \
    || fail SERVER_FETCH "无法从服务器 origin 或源仓库地址拉取 ${DEPLOY_BRANCH}"
fi

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

frontend_health_check() {
  local attempt
  for attempt in $(seq 1 "${HEALTH_TRIES}"); do
    if curl -fsS "${FRONTEND_URL}/" >/dev/null 2>&1 && \
       curl -fsS "${HEALTH_URL}/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep "${HEALTH_INTERVAL}"
  done
  return 1
}

NON_FRONTEND_SERVICES=(drop_agent drop_server apiserver analysis postgres minio pprof_demo)
snapshot_non_frontend_containers() {
  local service container_id started_at
  for service in "${NON_FRONTEND_SERVICES[@]}"; do
    container_id="$(docker compose ps -q "${service}")"
    started_at=""
    if [[ -n "${container_id}" ]]; then
      started_at="$(docker inspect --format '{{.State.StartedAt}}' "${container_id}")"
    fi
    printf '%s=%s|%s\n' "${service}" "${container_id}" "${started_at}"
  done
}

verify_non_frontend_unchanged() {
  local after
  after="$(snapshot_non_frontend_containers)"
  if [[ "${NON_FRONTEND_BEFORE}" != "${after}" ]]; then
    printf '非前端容器在前端部署期间发生变化:\n' >&2
    diff -u <(printf '%s\n' "${NON_FRONTEND_BEFORE}") <(printf '%s\n' "${after}") >&2 || true
    return 1
  fi
}

NON_AGENT_SERVICES=(drop_server apiserver analysis postgres minio pprof_demo web_frontend)
snapshot_non_agent_containers() {
  local service container_id started_at
  for service in "${NON_AGENT_SERVICES[@]}"; do
    container_id="$(docker compose ps -q "${service}")"
    started_at=""
    if [[ -n "${container_id}" ]]; then
      started_at="$(docker inspect --format '{{.State.StartedAt}}' "${container_id}")"
    fi
    printf '%s=%s|%s\n' "${service}" "${container_id}" "${started_at}"
  done
}

verify_non_agent_unchanged() {
  local after
  after="$(snapshot_non_agent_containers)"
  if [[ "${NON_AGENT_BEFORE}" != "${after}" ]]; then
    printf '非 Agent 容器在 Agent 部署期间发生变化:\n' >&2
    diff -u <(printf '%s\n' "${NON_AGENT_BEFORE}") <(printf '%s\n' "${after}") >&2 || true
    return 1
  fi
}

if [[ "${SERVICE_SCOPE}" == "frontend" ]]; then
  case "${TEST_SCOPE}" in
    full)
      run_stage TESTS make web-frontend-unit-test
      run_stage COVERAGE make web-frontend-coverage
      ;;
    smoke|none)
      printf '[STAGE:TESTS] SKIP scope=%s service_scope=frontend\n' "${TEST_SCOPE}"
      ;;
  esac
  NON_FRONTEND_BEFORE="$(snapshot_non_frontend_containers)"
  run_stage BUILD docker compose build web_frontend
  run_stage UP docker compose up -d --no-deps web_frontend
  run_stage HEALTH frontend_health_check
  run_stage NON_FRONTEND_UNCHANGED verify_non_frontend_unchanged
  printf '[STAGE:E2E] SKIP service_scope=frontend (avoid creating tasks or restarting Agent)\n'
  printf '[STAGE:E2E_MULTILANG] SKIP service_scope=frontend\n'
elif [[ "${SERVICE_SCOPE}" == "agent" ]]; then
  case "${TEST_SCOPE}" in
    full)
      run_stage TESTS make test
      run_stage COVERAGE make coverage
      ;;
    smoke|none)
      printf '[STAGE:TESTS] SKIP scope=%s service_scope=agent\n' "${TEST_SCOPE}"
      ;;
  esac
  NON_AGENT_BEFORE="$(snapshot_non_agent_containers)"
  run_stage BUILD docker compose build drop_agent
  run_stage UP docker compose up -d --no-deps drop_agent
  run_stage HEALTH health_check
  run_stage NON_AGENT_UNCHANGED verify_non_agent_unchanged
  printf '[STAGE:E2E] SKIP service_scope=agent (avoid creating demo tasks)\n'
  printf '[STAGE:E2E_MULTILANG] SKIP service_scope=agent\n'
else
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
  run_stage HEALTH health_check
  run_stage E2E bash scripts/e2e_smoke.sh
  # full 验收同时验证 Agent 容器重建后的 Session 原地恢复。
  if [[ "${TEST_SCOPE}" == "full" ]]; then
    run_stage E2E_MULTILANG env RESTART_AGENT=1 bash scripts/continuous_process_multilang_e2e.sh
  fi
fi
check_clean_tree
printf '[STAGE:ALL] PASS branch=%s sha=%s\n' "${DEPLOY_BRANCH}" "${TARGET_SHA:0:12}"
