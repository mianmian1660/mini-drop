#!/bin/bash
# ==============================================================================
# Mini-Drop 服务器端部署执行器（由本地 sync.sh 经 SSH 注入，勿手工调用）
# ------------------------------------------------------------------------------
# MODE=probe   加锁并探测服务器状态：输出 TOKEN=<..> SERVER_HEAD=<sha> 或错误标签
# MODE=deploy  校验锁 -> 快进更新到 TARGET_SHA -> 测试/构建/启动/健康检查/E2E -> 解锁
# 约束：本脚本只做快进更新；任何非快进、脏工作树、token 不匹配都会立即失败。
# ==============================================================================

set -euo pipefail

MODE="${MODE:?MODE required}"
REMOTE_PATH="${REMOTE_PATH:?REMOTE_PATH required}"
DEPLOY_BRANCH="${DEPLOY_BRANCH:?DEPLOY_BRANCH required}"

LOCK_DIR="${DEPLOY_LOCK_DIR:-/tmp/mini-drop-deploy.lock}"
TOKEN_FILE="${LOCK_DIR}/token"
BUNDLE_FILE="$(dirname "${REMOTE_PATH}")/mini-drop-deploy.bundle"
STALE_SECS=1800
HEALTH_URL="${DEPLOY_HEALTH_URL:-http://127.0.0.1:8191}"
HEALTH_TRIES="${DEPLOY_HEALTH_TRIES:-30}"
HEALTH_INTERVAL="${DEPLOY_HEALTH_INTERVAL:-2}"

tag_fail() { echo "[STAGE:$1] FAIL $2"; }
die() { tag_fail "$1" "$2"; exit "${3:-1}"; }

# 可移植超时（GNU timeout 不存在时退化为 perl alarm）
run_with_timeout() {
  if command -v timeout >/dev/null 2>&1; then
    timeout "$@"
  else
    local t="$1"; shift
    perl -e 'alarm shift; exec @ARGV' "$t" "$@"
  fi
}

check_clean_tree() { # <标签前缀>
  local dirty
  dirty="$(git status --porcelain | wc -l | tr -d ' ')"
  (( dirty == 0 )) || die "SERVER_CLEAN" "${1:-}工作树存在 ${dirty} 个未提交/未跟踪条目，拒绝部署"
}

if [[ "${MODE}" == "probe" ]]; then
  now=$(date +%s)
  if ! mkdir "${LOCK_DIR}" 2>/dev/null; then
    ts=$(cut -d: -f1 "${TOKEN_FILE}" 2>/dev/null || echo "${now}")
    age=$(( now - ${ts:-now} ))
    (( age > STALE_SECS )) && rm -rf "${LOCK_DIR}" && mkdir "${LOCK_DIR}" \
      || die SERVER_LOCK "另一场部署正在进行（锁龄 ${age}s）"
  fi
  printf '%s:%s\n' "${now}" "$$" > "${TOKEN_FILE}"

  cd "${REMOTE_PATH}" || die SERVER_PATH "目录不存在: ${REMOTE_PATH}"
  branch="$(git rev-parse --abbrev-ref HEAD)"
  [[ "${branch}" == "${DEPLOY_BRANCH}" ]] || die SERVER_BRANCH "服务器分支为 ${branch}，期望 ${DEPLOY_BRANCH}"
  check_clean_tree "服务器"
  echo "TOKEN=$(cat "${TOKEN_FILE}")"
  echo "SERVER_HEAD=$(git rev-parse HEAD)"
  exit 0
fi

# ---------------------------------------------------------------- deploy 模式
TARGET_SHA="${TARGET_SHA:?TARGET_SHA required}"
TOKEN="${TOKEN:?TOKEN required}"
NEED_TRANSFER="${NEED_TRANSFER:-1}"
FETCH_TIMEOUT="${FETCH_TIMEOUT:-60}"
TEST_SCOPE="${TEST_SCOPE:-full}"

cleanup() { rm -rf "${LOCK_DIR}" "${BUNDLE_FILE}"; }
trap cleanup EXIT

[[ -f "${TOKEN_FILE}" && "$(cat "${TOKEN_FILE}")" == "${TOKEN}" ]] \
  || die SERVER_LOCK "部署锁 token 不匹配（可能已被并发部署接管），中止"

cd "${REMOTE_PATH}" || die SERVER_PATH "目录不存在: ${REMOTE_PATH}"
branch="$(git rev-parse --abbrev-ref HEAD)"
[[ "${branch}" == "${DEPLOY_BRANCH}" ]] || die SERVER_BRANCH "服务器分支为 ${branch}，期望 ${DEPLOY_BRANCH}"
check_clean_tree "服务器"

CURRENT_SHA="$(git rev-parse HEAD)"

if [[ "${NEED_TRANSFER}" == "1" ]]; then
  fetch_ok=0; via="bundle"
  if run_with_timeout "${FETCH_TIMEOUT}" git fetch origin "${DEPLOY_BRANCH}" >/dev/null 2>&1; then
    origin_sha="$(git rev-parse "origin/${DEPLOY_BRANCH}")"
    if [[ "${origin_sha}" == "${TARGET_SHA}" ]]; then
      fetch_ok=1; via="origin"
    fi
  fi
  if (( ! fetch_ok )); then
    [[ -s "${BUNDLE_FILE}" ]] || die SERVER_FETCH "origin 拉取失败且未收到 bundle"
    git bundle verify "${BUNDLE_FILE}" >/dev/null 2>&1 \
      || die SERVER_BUNDLE "bundle 校验失败（前置对象缺失？请人工介入）"
    # 非强制 refspec：若 origin/test 无法快进到 bundle 内容将直接失败
    git fetch "${BUNDLE_FILE}" "refs/heads/${DEPLOY_BRANCH}:refs/remotes/origin/${DEPLOY_BRANCH}" >/dev/null 2>&1 \
      || die SERVER_FETCH "经 bundle 更新 refs/remotes/origin/${DEPLOY_BRANCH} 失败（非快进或对象缺失）"
    origin_sha="$(git rev-parse "origin/${DEPLOY_BRANCH}")"
    [[ "${origin_sha}" == "${TARGET_SHA}" ]] || die SERVER_VERIFY "bundle 更新后 origin 与目标不一致"
  fi
  echo "[STAGE:SERVER_FETCH] PASS via=${via}"

  git merge-base --is-ancestor "${CURRENT_SHA}" "${TARGET_SHA}" \
    || die SERVER_FF "非快进更新被拒绝: 服务器 ${CURRENT_SHA:0:12} 不是目标 ${TARGET_SHA:0:12} 的祖先"
  if [[ "${CURRENT_SHA}" != "${TARGET_SHA}" ]]; then
    git merge --ff-only "refs/remotes/origin/${DEPLOY_BRANCH}" >/dev/null 2>&1 \
      || die SERVER_FF "快进合并失败"
  fi
else
  echo "[STAGE:SERVER_FETCH] PASS up-to-date"
fi

HEAD_NOW="$(git rev-parse HEAD)"
[[ "${HEAD_NOW}" == "${TARGET_SHA}" ]] \
  || die SERVER_VERIFY "更新后 HEAD(${HEAD_NOW:0:12}) != 目标(${TARGET_SHA:0:12})"
echo "[STAGE:UPDATE] PASS HEAD=$(git rev-parse --short HEAD) worktree=$(git status --porcelain | wc -l | tr -d ' ')dirty"

run_stage() { # run_stage <名称> <命令...>
  local name="$1"; shift
  echo "==> [${name}] 开始..."
  if "$@"; then
    echo "[STAGE:${name}] PASS"
  else
    tag_fail "${name}" "阶段失败 sha=${TARGET_SHA:0:12}"
    if [[ "${name}" == "BUILD" || "${name}" == "TESTS" || "${name}" == "COVERAGE" ]]; then
      echo "==> 提示: 失败发生在上线(UP)之前，旧容器未受影响，仍在正常运行。"
    elif [[ "${name}" == "UP" || "${name}" == "HEALTH" || "${name}" == "E2E" ]]; then
      echo "==> 提示: 新版本已尝试上线。请在本地 revert/fix -> commit -> push 后重新部署；不要在服务器上回退或修改文件。"
    fi
    exit 20
  fi
}

case "${TEST_SCOPE}" in
  full)
    run_stage TESTS make test
    run_stage COVERAGE make coverage
    ;;
  smoke)
    echo "==> [TESTS] 跳过单元测试（smoke 模式，依赖 E2E 兜底）"
    ;;
  none)
    echo "==> [TESTS] 跳过（none 模式）"
    ;;
esac

run_stage BUILD docker compose build
run_stage UP docker compose up -d --wait --wait-timeout 300

health_check() {
  local i
  for i in $(seq 1 "${HEALTH_TRIES}"); do
    if curl -fsS "${HEALTH_URL}/healthz" >/dev/null 2>&1; then return 0; fi
    sleep "${HEALTH_INTERVAL}"
  done
  echo "健康检查未通过: ${HEALTH_URL}/healthz (重试 ${HEALTH_TRIES} 次)" >&2
  return 1
}
run_stage HEALTH health_check

run_stage E2E bash scripts/e2e_smoke.sh

echo "[STAGE:ALL] PASS sha=$(git rev-parse --short HEAD)"
