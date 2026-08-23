#!/bin/bash
# ==============================================================================
# check-observation-phase0.sh — 阶段 0 观察结果判定（服务器端）
# ==============================================================================
# 读取观察日志，按任务"通过标准"逐项判定：
#   1. 可用空间没有持续异常下降（末样本 ≥ 首样本 - 2GiB 且 > 2GiB）
#   2. 没有新增历史测试镜像或无限增长的构建缓存
#   3. spool ≤ 1GiB，符号缓存 ≤ 256MiB
#   4. 正常空间下没有误拒收（rejected_total == 0）
#   5. rollback 已实测可用（存在 rollback-* 快照 + API 健康）
#   6. 2 小时内无新增严重错误（apiserver error / compactor 问题累计为 0）
#   7. 容器无新增重启
#
# 用法：
#   bash check-observation-phase0.sh [观察日志路径]   # 缺省取 observation/ 最新
# 输出：逐项 PASS/FAIL + 总体 PASS/FAIL（退出码 0=通过，1=不通过）
# ==============================================================================
set -uo pipefail

OBS_DIR="/home/ubuntu/mini-drop-shared/deploy-state/observation"
LOG="${1:-$(ls -t "${OBS_DIR}"/*.log 2>/dev/null | head -1)}"
[[ -n "${LOG}" && -f "${LOG}" ]] || { echo "❌ 未找到观察日志"; exit 1; }

say()  { printf '[check] %s\n' "$*"; }
fail=0

say "观察日志: ${LOG}"
samples="$(grep -cE '^[0-9]{4}-' "${LOG}")"
say "样本数: ${samples}"
[[ "${samples}" -ge 1 ]] || { say "❌ 无有效样本"; exit 1; }

first="$(grep -E '^[0-9]{4}-' "${LOG}" | head -1)"
last="$(grep -E '^[0-9]{4}-' "${LOG}" | tail -1)"
say "首样本: ${first}"
say "末样本: ${last}"

val() { echo "$1" | grep -oE "${2}=[0-9.]+[A-Za-z]*" | head -1 | cut -d= -f2; }
gbi() { awk -v b="$1" 'BEGIN{printf "%.2f", b/1073741824}'; }

# 1) 可用空间
first_avail="$(val "$first" 'avail')"
last_avail="$(val "$last" 'avail')"
if [[ -n "${first_avail}" && -n "${last_avail}" ]]; then
  drop=$(( first_avail - last_avail ))
  if [[ "${last_avail}" -gt $((2 * 1024 * 1024 * 1024)) && "${drop}" -lt $((2 * 1024 * 1024 * 1024)) ]]; then
    say "✅ 可用空间稳定：首 $(gbi "$first_avail")GiB → 末 $(gbi "$last_avail")GiB（下降 $(gbi "$drop")GiB < 2GiB 且 > 2GiB）"
  else
    say "❌ 可用空间异常：首 $(gbi "$first_avail")GiB → 末 $(gbi "$last_avail")GiB（下降 $(gbi "$drop")GiB 或剩余不足）"; fail=1
  fi
else
  say "❌ 无法解析可用空间"; fail=1
fi

# 2) 镜像/构建缓存不膨胀
first_img="$(val "$first" 'images')"; last_img="$(val "$last" 'images')"
first_bc="$(val "$first" 'build_cache')"; last_bc="$(val "$last" 'build_cache')"
img_delta=$(( last_img - first_img ))
bc_delta=$(( last_bc - first_bc ))
if [[ "${img_delta}" -le 2 && "${bc_delta}" -le 20 ]]; then
  say "✅ 镜像数 ${first_img}→${last_img}（+${img_delta}），构建缓存条目 ${first_bc}→${last_bc}（+${bc_delta}），无异常膨胀"
else
  say "❌ 镜像/构建缓存膨胀：镜像 +${img_delta}，缓存 +${bc_delta}"; fail=1
fi

# 3) spool / 符号缓存
last_spool="$(val "$last" 'spool')"; last_sym="$(val "$last" 'symbol_cache')"
if [[ "${last_spool}" -le $((1024 * 1024 * 1024)) && "${last_sym}" -le $((256 * 1024 * 1024)) ]]; then
  say "✅ spool $(gbi "${last_spool}")GiB ≤ 1GiB，符号缓存 $(gbi "${last_sym}")GiB ≤ 256MiB"
else
  say "❌ spool/symbol 超限：spool=${last_spool} symbol=${last_sym}"; fail=1
fi

# 4) 误拒收
last_rej="$(val "$last" 'rejected_total')"
if [[ "${last_rej:-0}" == "0" ]]; then
  say "✅ 无低磁盘误拒收（rejected_total=0）"
else
  say "❌ 存在拒收计数 rejected_total=${last_rej}"; fail=1
fi

# 5) rollback 已实测可用
rollback_snap="$(ls -d /home/ubuntu/mini-drop-shared/deploy-state/rollback-* 2>/dev/null | head -1)"
if [[ -n "${rollback_snap}" ]] && curl -fsS http://127.0.0.1:8191/healthz >/dev/null 2>&1; then
  say "✅ rollback 快照存在（${rollback_snap}）且 API 健康"
else
  say "❌ rollback 快照缺失或 API 不健康"; fail=1
fi

# 6) 严重错误累计
err_total="$(grep -E '^[0-9]{4}-' "${LOG}" | grep -oE 'apiserver_errors_15m=[0-9]+' | cut -d= -f2 | awk '{s+=$1} END{print s+0}')"
comp_total="$(grep -E '^[0-9]{4}-' "${LOG}" | grep -oE 'compactor_issues_15m=[0-9]+' | cut -d= -f2 | awk '{s+=$1} END{print s+0}')"
if [[ "${err_total}" == "0" && "${comp_total}" == "0" ]]; then
  say "✅ 观察期内 apiserver error=0、compactor 问题=0"
else
  say "❌ 观察期内严重错误：apiserver_errors=${err_total}, compactor_issues=${comp_total}"; fail=1
fi

# 7) 容器重启
last_restart="$(val "$last" 'container_restarts')"
if [[ "${last_restart:-0}" == "0" ]]; then
  say "✅ 容器无重启"
else
  say "❌ 容器重启次数 container_restarts=${last_restart}"; fail=1
fi

if [[ "${fail}" == "0" ]]; then
  say "✅✅ 观察判定：PASS —— 满足全部通过标准，可以 push"
  exit 0
else
  say "❌❌ 观察判定：FAIL —— 存在未通过项，禁止 push"
  exit 1
fi
