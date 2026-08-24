#!/bin/bash
# ==============================================================================
# Mini-Drop 受控部署脚本（唯一部署入口）
# ------------------------------------------------------------------------------
# 流程：本地校验 -> 服务器加锁探测 -> 快进更新到 origin/test -> 测试/构建/启动/健康检查/E2E
# 约束：
#   * 只接受 test 分支；本地工作树必须干净；HEAD 必须已推送到 origin/test。
#   * 本脚本不代替你 commit / push。
#   * 服务器仅做快进更新并校验 HEAD == origin/test；禁止在服务器改代码或提交。
#   * UP 阶段之前的任何失败都不会影响正在运行的旧容器（build 只产镜像不动容器）。
#   * UP 之后失败：请在本地 revert/fix -> commit -> push -> 重新执行本脚本；
#     不要在服务器上回退或修改文件。
# 用法：
#   ./sync.sh                              # 部署当前 test 分支 HEAD
#   DEPLOY_TEST_SCOPE=smoke ./sync.sh      # full(默认) | smoke | none
# 可注入项（供静态/模拟测试）：
#   SYNC_SSH_CMD / SYNC_FETCH_TIMEOUT / DEPLOY_TEST_SCOPE / DEPLOY_HEALTH_URL
# ==============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_BRANCH="test"
LOCAL_PATH="${SCRIPT_DIR}"
REMOTE_SCRIPT="${SCRIPT_DIR}/scripts/deploy_remote.sh"

SSH_CMD="${SYNC_SSH_CMD:-ssh}"
FETCH_TIMEOUT="${SYNC_FETCH_TIMEOUT:-60}"
TEST_SCOPE="${DEPLOY_TEST_SCOPE:-full}"
case "${TEST_SCOPE}" in
  full|smoke|none) ;;
  *) echo "[STAGE:CONFIG] FAIL 未知 DEPLOY_TEST_SCOPE=${TEST_SCOPE}"; exit 2 ;;
esac

if [[ -f "${SCRIPT_DIR}/sync.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "${SCRIPT_DIR}/sync.env"
  set +a
fi
REMOTE_HOST="${SYNC_REMOTE_HOST:-${REMOTE_HOST:-}}"
REMOTE_PATH="${SYNC_REMOTE_PATH:-${REMOTE_PATH:-/home/ubuntu/mini-drop}}"

die() { # die <STAGE> <消息>
  echo "[STAGE:$1] FAIL $2 (目标 SHA: ${TARGET_SHA:-未知})"
  exit "${3:-1}"
}

need() { command -v "$1" >/dev/null 2>&1 || { echo "缺少命令: $1"; exit 2; }; }
need git
[[ -f "${REMOTE_SCRIPT}" ]] || die CONFIG "缺少服务器端脚本: ${REMOTE_SCRIPT}"

# ---------------------------------------------------------------- 本地预检
if [[ $# -gt 0 && "$1" != "${DEPLOY_BRANCH}" ]]; then
  die LOCAL_BRANCH "本脚本只部署 ${DEPLOY_BRANCH} 分支，收到非法参数: $1" 3
fi
[[ -n "${REMOTE_HOST}" ]] || die SYNC_ENV "请创建 sync.env 并设置 SYNC_REMOTE_HOST=ubuntu@服务器IP"

cd "${LOCAL_PATH}"
CURRENT_BRANCH="$(git rev-parse --abbrev-ref HEAD)"
[[ "${CURRENT_BRANCH}" == "${DEPLOY_BRANCH}" ]] \
  || die LOCAL_BRANCH "当前分支为 ${CURRENT_BRANCH}，只允许从 ${DEPLOY_BRANCH} 部署"

[[ -z "$(git status --porcelain)" ]] \
  || die LOCAL_CLEAN "本地工作树不干净，请先提交或清理（本脚本不自动提交）"

git rev-parse --verify "${DEPLOY_BRANCH}" >/dev/null 2>&1 \
  || die LOCAL_BRANCH "本地不存在 ${DEPLOY_BRANCH} 分支"

if ! git fetch origin "${DEPLOY_BRANCH}" --quiet; then
  die LOCAL_PUSHED "无法访问远程 origin，请检查网络后重试"
fi

TARGET_SHA="$(git rev-parse HEAD)"
ORIGIN_SHA="$(git rev-parse "origin/${DEPLOY_BRANCH}")"
[[ "${TARGET_SHA}" == "${ORIGIN_SHA}" ]] \
  || die LOCAL_PUSHED "HEAD(${TARGET_SHA:0:12}) 尚未推送到 origin/${DEPLOY_BRANCH}($(git rev-parse --short "${ORIGIN_SHA}" 2>/dev/null || echo '?'))，请先执行 git push origin ${DEPLOY_BRANCH}"

echo "==> 本地预检通过: branch=${DEPLOY_BRANCH} sha=${TARGET_SHA:0:12} scope=${TEST_SCOPE}"
echo "==> 目标服务器: ${REMOTE_HOST}:${REMOTE_PATH}"

# ---------------------------------------------------- 第一阶段：加锁与状态探测
TMP_BUNDLE="$(mktemp -t mini-drop-deploy.XXXXXX)"
trap 'rm -f "${TMP_BUNDLE}"' EXIT

PROBE_ENV="$(printf 'MODE=%q REMOTE_PATH=%q DEPLOY_BRANCH=%q' probe "${REMOTE_PATH}" "${DEPLOY_BRANCH}")"
STATE_OUT=""
SRC_RC=0
STATE_OUT="$("${SSH_CMD}" "${REMOTE_HOST}" "${PROBE_ENV} bash -s" <"${REMOTE_SCRIPT}")" || SRC_RC=$?

if (( SRC_RC != 0 )); then
  TAG="$(grep -o 'SERVER_[A-Z_]*' <<<"${STATE_OUT}" | head -1 || true)"
  [[ -n "${TAG}" ]] && die "${TAG#SERVER_}" "${STATE_OUT}"
  die SERVER_CONN "无法连接服务器或在服务器预检失败: ${STATE_OUT}"
fi
TOKEN="$(sed -n 's/^TOKEN=//p' <<<"${STATE_OUT}")"
SERVER_HEAD="$(sed -n 's/^SERVER_HEAD=//p' <<<"${STATE_OUT}")"
[[ -n "${TOKEN}" && -n "${SERVER_HEAD}" ]] || die SERVER_CONN "服务器返回异常: ${STATE_OUT}"
echo "==> 已取得部署锁 token=${TOKEN%%:*} 服务器当前 HEAD=${SERVER_HEAD:0:12}"

NEED_TRANSFER=1
[[ "${SERVER_HEAD}" == "${TARGET_SHA}" ]] && NEED_TRANSFER=0

if (( NEED_TRANSFER )); then
  echo "==> 生成增量 bundle（基线: 服务器 ${SERVER_HEAD:0:12}）..."
  if ! git bundle create "${TMP_BUNDLE}" "${DEPLOY_BRANCH}" --not "${SERVER_HEAD}" --quiet 2>/dev/null; then
    echo "    增量不可用（历史分叉或对象缺失），退化为全量 bundle..."
    git bundle create "${TMP_BUNDLE}" "${DEPLOY_BRANCH}" --quiet || die LOCAL_BUNDLE "生成 bundle 失败"
  fi
  echo "==> 上传 bundle ($(du -h "${TMP_BUNDLE}" | cut -f1)) ..."
  "${SSH_CMD}" "${REMOTE_HOST}" "cat > '${REMOTE_PATH}/../mini-drop-deploy.bundle'" <"${TMP_BUNDLE}" \
    || die TRANSFER "bundle 上传失败"
else
  echo "==> 服务器已是目标版本，跳过代码传输，仅执行验证与启动流程。"
fi

# ------------------------------------- 第二阶段：服务器更新 + 测试/构建/启动/验证
echo "==> 在服务器执行受控更新与启动流程..."
DEPLOY_ENV="$(printf 'MODE=%q REMOTE_PATH=%q DEPLOY_BRANCH=%q TARGET_SHA=%q TOKEN=%q NEED_TRANSFER=%q FETCH_TIMEOUT=%q TEST_SCOPE=%q' \
  deploy "${REMOTE_PATH}" "${DEPLOY_BRANCH}" "${TARGET_SHA}" "${TOKEN}" \
  "${NEED_TRANSFER}" "${FETCH_TIMEOUT}" "${TEST_SCOPE}")"
RC=0
"${SSH_CMD}" "${REMOTE_HOST}" "${DEPLOY_ENV} bash -s" <"${REMOTE_SCRIPT}" || RC=$?

(( RC == 0 )) || die DEPLOY "服务器端部署失败 (exit=${RC})，见上方 [STAGE:*] 输出。若失败发生在 UP 之后：请在本地 revert/fix 后 push 并重新部署，不要在服务器上改文件。"

echo "==> ✅ 部署完成: branch=${DEPLOY_BRANCH} sha=${TARGET_SHA:0:12} scope=${TEST_SCOPE}"
