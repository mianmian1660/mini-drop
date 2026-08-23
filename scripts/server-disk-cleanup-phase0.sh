#!/bin/bash
# ==============================================================================
# server-disk-cleanup-phase0.sh — 服务器磁盘止血（阶段 0）
# ==============================================================================
# 用法：
#   bash server-disk-cleanup-phase0.sh report    # 只读报告，不做任何修改
#   bash server-disk-cleanup-phase0.sh clean     # 安全清理（先自动执行 report）
#
# 清理范围（仅限以下，绝不越界）：
#   1. 未被任何容器引用的 mini-drop-drop-tests / mini-drop-drop-test 镜像
#   2. 不属于任何现有容器镜像的 mini-drop/phase1-predeploy 标签
#   3. 删除上述镜像后变成无引用的构建缓存（docker builder prune -f）
#   4. 经挂载检查确认未被使用、且名称明确属于测试环境的测试卷（mini-drop-test_*）
#
# 明确禁止：
#   - docker system prune -a --volumes
#   - 删除 mini-drop_pgdata / mini-drop_miniodata / mini-drop_gosymbolcache
#   - 删除 /var/lib/mini-drop/continuous-spool
#   - 删除任何 MinIO 业务对象
#   - 按模糊名称或未解析环境变量执行递归删除
# ==============================================================================
set -euo pipefail

MODE="${1:-report}"
PROJECT="mini-drop"

say() { printf '[cleanup] %s\n' "$*"; }
die() { printf '[cleanup] ❌ %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# 只读报告
# ---------------------------------------------------------------------------
report() {
  say "===== 只读磁盘报告 ====="
  echo "--- df ---"
  df -h /
  echo "--- docker system df -v ---"
  docker system df -v
  echo "--- 运行/停止容器引用的镜像 ---"
  docker ps -a --format '{{.Names}}\t{{.Image}}\t{{.ID}}\t{{.Status}}'
  echo "--- 所有卷及挂载关系/大小 ---"
  docker volume ls --format '{{.Name}}'
  for vol in $(docker volume ls -q); do
    used="$(docker ps -aq --filter "volume=${vol}" | wc -l | tr -d ' ')"
    size="$(docker run --rm -v "${vol}:/v" alpine du -sh /v 2>/dev/null | awk '{print $1}' || echo '?')"
    printf '  %-40s containers_using=%-3s size=%s\n' "${vol}" "${used}" "${size}"
  done
  echo "--- source/build cache 大小 ---"
  du -sh /home/ubuntu/mini-drop 2>/dev/null || echo "  (no source dir)"
  du -sh /var/lib/docker/buildkit 2>/dev/null || true
  du -sh /var/lib/mini-drop/continuous-spool 2>/dev/null || echo "  (no spool)"
}

# ---------------------------------------------------------------------------
# 镜像是否被任何容器引用
# ---------------------------------------------------------------------------
container_refs() { # container_refs <image> → 引用容器数
  docker ps -aq --filter "ancestor=$1" 2>/dev/null | wc -l | tr -d ' '
}

# 所有现有容器引用的镜像 ID 集合（这些绝不允许删）
protected_image_ids() {
  for c in $(docker ps -aq); do docker inspect -f '{{.Image}}' "$c"; done | sort -u
}

# ---------------------------------------------------------------------------
# 安全清理
# ---------------------------------------------------------------------------
clean() {
  say "===== 0) 清理前确认现有容器镜像（这些镜像不会删除）====="
  docker ps -a --format '{{.Names}}\t{{.Image}}\t{{.ID}}\t{{.Status}}'

  say "===== 1) 删除未被任何容器引用的测试镜像 ====="
  for img in "mini-drop-drop-tests:latest" "mini-drop-drop-test:latest"; do
    refs="$(container_refs "${img}")"
    if [[ "${refs}" == "0" ]]; then
      say "  删除 ${img}（无容器引用）"
      docker rmi "${img}" || say "  警告：${img} 删除失败（可能与其他镜像共享层）"
    else
      say "  保留 ${img}（被 ${refs} 个容器引用）"
    fi
  done

  say "===== 2) 删除不属于任何现有容器镜像的 phase1-predeploy 标签 ====="
  mapfile -t prot_ids < <(protected_image_ids)
  for tag in $(docker images --format '{{.Repository}}:{{.Tag}}' | grep '^mini-drop/phase1-predeploy:' || true); do
    img_id="$(docker inspect -f '{{.Id}}' "${tag}")"
    if printf '%s\n' "${prot_ids[@]}" | grep -qx "${img_id}"; then
      say "  保留 ${tag}（ID ${img_id} 正被现有容器引用）"
    else
      say "  删除 ${tag}（ID ${img_id} 不属于当前/rollback 镜像）"
      docker rmi "${tag}" || say "  警告：${tag} 删除失败"
    fi
  done

  say "===== 3) 清理删除镜像后无引用的构建缓存 ====="
  docker builder prune -f || say "  警告：build cache prune 失败"

  say "===== 4) 删除明确属于测试环境且未被使用的测试卷 ====="
  for vol in mini-drop-test_gosymbolcache mini-drop-test_miniodata mini-drop-test_pgdata; do
    if docker volume inspect "${vol}" >/dev/null 2>&1; then
      refs="$(docker ps -aq --filter "volume=${vol}" | wc -l | tr -d ' ')"
      if [[ "${refs}" == "0" ]]; then
        say "  删除测试卷 ${vol}（无容器挂载）"
        docker volume rm "${vol}" || say "  警告：${vol} 删除失败"
      else
        say "  保留 ${vol}（被 ${refs} 个容器挂载）"
      fi
    fi
  done

  say "===== 清理后磁盘状态 ====="
  df -h /
  AVAIL_BYTES="$(df -B1 --output=avail / | tail -1 | tr -d ' ')"
  MIN=$((8 * 1024 * 1024 * 1024))
  if [[ "${AVAIL_BYTES}" -ge "${MIN}" ]]; then
    say "✅ 根盘可用 $((AVAIL_BYTES / 1024 / 1024 / 1024)) GiB ≥ 8GiB，可以按原 sync.sh 流程构建"
  else
    say "⚠️ 根盘可用 $((AVAIL_BYTES / 1024 / 1024 / 1024)) GiB < 8GiB，本次服务器构建停止（阶段 0 不通过删除业务数据强行腾空间）"
  fi
}

case "${MODE}" in
  report) report ;;
  clean)  report; echo; clean ;;
  *) die "用法: $0 report|clean" ;;
esac
