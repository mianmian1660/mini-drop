#!/bin/bash
# ==============================================================================
# release-phase0.sh — 本地阶段 0 发布（未推送候选版本）
# ==============================================================================
# 前置条件：
#   - 当前位于 storage-phase0 分支
#   - 工作区干净且 HEAD 已本地提交（不要求 origin 包含该 commit）
#   - sync.env 已配置 SYNC_REMOTE_HOST
#
# 流程：
#   1. git archive HEAD 生成精确代码包（.git/依赖/构建产物天然被排除——它们不在版本库）
#   2. 同步到服务器 /home/ubuntu/mini-drop-releases/{commit}-{timestamp}/
#   3. 调用服务器端 server-release-phase0.sh 完成：compose 校验 → 磁盘检查 →
#      rollback tag/快照 → 构建 → 依次切换 → 健康检查 → 更新 current → 清理
#
# 用法：
#   bash scripts/release-phase0.sh [--dry-run]
# ==============================================================================
set -euo pipefail

DRY_RUN=0
if [[ "${1:-}" == "--dry-run" ]]; then DRY_RUN=1; fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
REMOTE_HOST=""

if [[ -f "${REPO_ROOT}/sync.env" ]]; then
  set -a; source "${REPO_ROOT}/sync.env"; set +a
fi
REMOTE_HOST="${SYNC_REMOTE_HOST:-${REMOTE_HOST:-}}"
[[ -n "${REMOTE_HOST}" ]] || { echo "❌ 请先在 sync.env 配置 SYNC_REMOTE_HOST"; exit 1; }

say() { printf '[release] %s\n' "$*"; }
die() { printf '[release] ❌ %s\n' "$*" >&2; exit 1; }

cd "${REPO_ROOT}"

# ---------------------------------------------------------------------------
# 1) 本地前置检查
# ---------------------------------------------------------------------------
BRANCH="$(git rev-parse --abbrev-ref HEAD)"
[[ "${BRANCH}" == "storage-phase0" ]] || die "当前分支为 ${BRANCH}，必须位于 storage-phase0"

DIRTY="$(git status --porcelain)"
[[ -z "${DIRTY}" ]] || die "工作区不干净，请先提交：\n${DIRTY}"

COMMIT="$(git rev-parse HEAD)"
[[ -n "${COMMIT}" ]] || die "无法解析 HEAD commit"
say "发布 commit: ${COMMIT}（分支 ${BRANCH}，本地已提交，无需 origin 包含）"

# ---------------------------------------------------------------------------
# 2) git archive 打包 + checksum
# ---------------------------------------------------------------------------
TS="$(date +%Y%m%d-%H%M%S)"
TARBALL="/tmp/mini-drop-release-${COMMIT}.tar.gz"
say "生成代码包: git archive HEAD → ${TARBALL}"
if [[ "$DRY_RUN" == "1" ]]; then
  say "[dry-run] git archive --format=tar.gz -o ${TARBALL} HEAD"
  SHA="dry-run-sha"
else
  git archive --format=tar.gz --prefix="mini-drop/" -o "${TARBALL}" HEAD
  SHA="$(shasum -a 256 "${TARBALL}" | awk '{print $1}')"
fi
say "archive sha256: ${SHA}"

# ---------------------------------------------------------------------------
# 3) 上传代码包 + 服务器端辅助脚本
# ---------------------------------------------------------------------------
RELEASE_NAME="${COMMIT}-${TS}"
RELEASE_DIR="/home/ubuntu/mini-drop-releases/${RELEASE_NAME}"

say "上传代码包到服务器"
if [[ "$DRY_RUN" == "1" ]]; then
  say "[dry-run] scp ${TARBALL} ${REMOTE_HOST}:/tmp/mini-drop-release-${COMMIT}.tar.gz"
  say "[dry-run] scp ${SCRIPT_DIR}/server-release-phase0.sh ${REMOTE_HOST}:/tmp/"
else
  scp -q "${TARBALL}" "${REMOTE_HOST}:/tmp/mini-drop-release-${COMMIT}.tar.gz"
  scp -q "${SCRIPT_DIR}/server-release-phase0.sh" "${REMOTE_HOST}:/tmp/server-release-phase0.sh"
fi

say "服务器端解包到 ${RELEASE_DIR}"
if [[ "$DRY_RUN" == "1" ]]; then
  say "[dry-run] ssh ${REMOTE_HOST} mkdir -p ${RELEASE_DIR} && tar -xzf /tmp/mini-drop-release-${COMMIT}.tar.gz -C ${RELEASE_DIR} --strip-components=1"
else
  ssh "${REMOTE_HOST}" "mkdir -p '${RELEASE_DIR}' && tar -xzf /tmp/mini-drop-release-${COMMIT}.tar.gz -C '${RELEASE_DIR}' --strip-components=1 && rm -f /tmp/mini-drop-release-${COMMIT}.tar.gz"
fi

# ---------------------------------------------------------------------------
# 4) 调用服务器端发布脚本
# ---------------------------------------------------------------------------
say "执行服务器端发布"
DRY_ARG=""
[[ "$DRY_RUN" == "1" ]] && DRY_ARG="--dry-run"
if [[ "$DRY_RUN" == "1" ]]; then
  say "[dry-run] ssh ${REMOTE_HOST} bash /tmp/server-release-phase0.sh ${RELEASE_DIR} ${COMMIT} ${SHA} ${DRY_ARG}"
else
  ssh "${REMOTE_HOST}" "bash /tmp/server-release-phase0.sh '${RELEASE_DIR}' '${COMMIT}' '${SHA}' ${DRY_ARG}"
fi

say "✅ 本地发布编排完成（commit=${COMMIT}）"
say "   服务器 release: ${RELEASE_DIR}"
