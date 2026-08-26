#!/usr/bin/env bash
# Mini-Drop 测试服务器部署入口。用法：./deploy.sh <remote-branch>

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REMOTE_EXECUTOR="${ROOT}/scripts/deploy_remote.sh"
SSH_CMD="${DEPLOY_SSH_CMD:-${SYNC_SSH_CMD:-ssh}}"
FETCH_TIMEOUT="${DEPLOY_FETCH_TIMEOUT:-${SYNC_FETCH_TIMEOUT:-60}}"
TEST_SCOPE="${DEPLOY_TEST_SCOPE:-full}"
SERVICE_SCOPE="${DEPLOY_SERVICE_SCOPE:-full}"

die() {
  printf '[STAGE:%s] FAIL %s\n' "$1" "$2" >&2
  exit "${3:-1}"
}

if [[ -f "${ROOT}/sync.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "${ROOT}/sync.env"
  set +a
fi

REMOTE_HOST="${DEPLOY_REMOTE_HOST:-${SYNC_REMOTE_HOST:-${REMOTE_HOST:-}}}"
REMOTE_PATH="${DEPLOY_REMOTE_PATH:-${SYNC_REMOTE_PATH:-${REMOTE_PATH:-/home/ubuntu/mini-drop}}}"

[[ $# -eq 1 ]] || die CONFIG "用法: ./deploy.sh <main|feature/...|fix/...>" 2
DEPLOY_BRANCH="$1"
git check-ref-format --branch "${DEPLOY_BRANCH}" >/dev/null 2>&1 \
  || die CONFIG "非法分支名: ${DEPLOY_BRANCH}" 2
[[ "${DEPLOY_BRANCH}" != -* ]] || die CONFIG "分支名不能以 - 开头" 2

case "${TEST_SCOPE}" in
  full|smoke|none) ;;
  *) die CONFIG "未知 DEPLOY_TEST_SCOPE=${TEST_SCOPE}" 2 ;;
esac
case "${SERVICE_SCOPE}" in
  full|frontend|agent) ;;
  *) die CONFIG "未知 DEPLOY_SERVICE_SCOPE=${SERVICE_SCOPE}" 2 ;;
esac

[[ -n "${REMOTE_HOST}" ]] || die CONFIG "请在 sync.env 中设置 SYNC_REMOTE_HOST"
[[ -f "${REMOTE_EXECUTOR}" ]] || die CONFIG "缺少 ${REMOTE_EXECUTOR}"
command -v git >/dev/null 2>&1 || die CONFIG "本地缺少 git"

cd "${ROOT}"
[[ -z "$(git status --porcelain)" ]] \
  || die LOCAL_CLEAN "本地工作树不干净；请先提交或清理"

# 部署器本身也必须来自已经推送的提交，避免用未审计脚本控制服务器。
CURRENT_SHA="$(git rev-parse HEAD)"
if ! git branch -r --contains "${CURRENT_SHA}" | grep -q .; then
  die LOCAL_PUSHED "当前部署器提交 ${CURRENT_SHA:0:12} 尚未出现在任何远程分支"
fi

REMOTE_LINE="$(git ls-remote --exit-code --heads origin "refs/heads/${DEPLOY_BRANCH}" 2>/dev/null)" \
  || die REMOTE_BRANCH "远程分支 origin/${DEPLOY_BRANCH} 不存在或 origin 不可访问"
TARGET_SHA="${REMOTE_LINE%%[[:space:]]*}"
[[ "${TARGET_SHA}" =~ ^[0-9a-fA-F]{40,64}$ ]] \
  || die REMOTE_BRANCH "无法解析 origin/${DEPLOY_BRANCH} 的 SHA"

printf '==> 部署 origin/%s @ %s\n' "${DEPLOY_BRANCH}" "${TARGET_SHA:0:12}"
printf '==> 目标服务器: %s:%s，测试范围: %s，服务范围: %s\n' \
  "${REMOTE_HOST}" "${REMOTE_PATH}" "${TEST_SCOPE}" "${SERVICE_SCOPE}"

REMOTE_ENV="$(printf 'REMOTE_PATH=%q DEPLOY_BRANCH=%q TARGET_SHA=%q FETCH_TIMEOUT=%q TEST_SCOPE=%q SERVICE_SCOPE=%q' \
  "${REMOTE_PATH}" "${DEPLOY_BRANCH}" "${TARGET_SHA}" "${FETCH_TIMEOUT}" "${TEST_SCOPE}" "${SERVICE_SCOPE}")"

"${SSH_CMD}" "${REMOTE_HOST}" "${REMOTE_ENV} bash -s" < "${REMOTE_EXECUTOR}" \
  || die DEPLOY "origin/${DEPLOY_BRANCH}@${TARGET_SHA:0:12} 部署失败"

printf '==> 部署完成: origin/%s @ %s\n' "${DEPLOY_BRANCH}" "${TARGET_SHA:0:12}"
