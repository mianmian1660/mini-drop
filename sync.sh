#!/usr/bin/env bash
# 兼容旧入口；不再执行 rsync。新流程请直接使用 ./deploy.sh <branch>。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
printf '提示: sync.sh 已废弃，转交给 deploy.sh；不会复制本地文件。\n' >&2
exec "${ROOT}/deploy.sh" "$@"
