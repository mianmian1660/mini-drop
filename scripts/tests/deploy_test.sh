#!/bin/bash
# ==============================================================================
# 部署脚本（sync.sh + scripts/deploy_remote.sh）静态/模拟测试
# ------------------------------------------------------------------------------
# 通过 SYNC_SSH_CMD 注入本地 shim 代替真实 SSH，在临时沙箱仓库中模拟：
#   错误分支 / 本地未提交 / 未 push / 服务器脏工作树 / 非快进 /
#   构建失败 / 健康检查失败 / 正常快进部署 / 重复部署(无更新)
# 运行：bash scripts/tests/deploy_test.sh
# ==============================================================================

set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC_ROOT="$(cd "${HERE}/../.." && pwd)"
SANDBOX_ROOT="$(mktemp -d /tmp/mini-drop-deploy-test.XXXXXX)"
PASS=0; FAIL=0

cleanup() { rm -rf "${SANDBOX_ROOT}"; }
trap cleanup EXIT

say()  { printf '%s\n' "$*"; }
ok()   { PASS=$((PASS+1)); say "  ✅ ${1}"; }
bad()  { FAIL=$((FAIL+1)); say "  ❌ ${1}"; say "     --- 输出 ---"; sed 's/^/     | /' <<<"${2:-}"; }

# ---------------------------------------------------------------- 沙箱构建
new_sandbox() {
  SB="${SANDBOX_ROOT}/$1"; rm -rf "${SB}"; mkdir -p "${SB}"
  ORIGIN="${SB}/origin.git"; LOCAL_REPO="${SB}/local"; SRV="${SB}/srv"
  git init --quiet --bare -b main "${ORIGIN}"
  git clone --quiet "${ORIGIN}" "${LOCAL_REPO}"

  # 初始提交：种子文件（含被部署脚本引用的路径布局）
  cd "${LOCAL_REPO}"
  mkdir -p scripts
  cp "${SRC_ROOT}/sync.sh" sync.sh
  cp "${SRC_ROOT}/scripts/deploy_remote.sh" scripts/deploy_remote.sh
  printf '#!/bin/bash\necho "[e2e-stub] PASS"\n' > scripts/e2e_smoke.sh
  echo "seed" > seed.txt
  git add -A && git commit --quiet -m "seed"
  git branch --quiet -M test
  git push --quiet origin test
  git --git-dir="${ORIGIN}" symbolic-ref HEAD refs/heads/test

  # 服务器工作树：落后一个提交
  git clone --quiet "${ORIGIN}" "${SRV}"
  cd "${LOCAL_REPO}"
  echo "v2" >> seed.txt && git commit --quiet -am "feature v2"
  git push --quiet origin test

  # ssh shim：把远端命令映射到本机执行（cwd=服务器工作树）
  SHIM="${SB}/shim"; mkdir -p "${SHIM}"
  cat > "${SHIM}/fake-ssh" <<EOF
#!/bin/bash
shift  # host
cmd="\$*"
case "\$cmd" in
  "cat > "*) target=\$(printf '%s' "\$cmd" | sed "s/^cat > '//; s/'\$//"); exec cat > "\$target" ;;
  *" bash -s") cd "\${FAKE_SERVER_DIR:?}" ; exec bash -c "\$cmd" ;;
  *) exec bash -c "\$cmd" ;;
esac
EOF
  chmod +x "${SHIM}/fake-ssh"

  # 运行时桩命令目录（docker/curl 可控失败）
  STUBS="${SB}/stubs"; mkdir -p "${STUBS}"
  make_stub() {
    cat > "${STUBS}/$1" <<STUB
#!/bin/bash
if [ -n "\${FAIL_AT:-}" ] && [ "\${FAIL_AT}" = "$2" ]; then echo "stub($1) forced failure: $2" >&2; exit 90; fi
exit 0
STUB
    chmod +x "${STUBS}/$1"
  }
  make_stub docker BUILD    # docker build 失败注入点：FAIL_AT=BUILD（UP 阶段同桩不拦截）
  make_stub curl HEALTH     # curl 失败注入点：FAIL_AT=HEALTH

  export FAKE_SERVER_DIR="${SRV}"
  export DEPLOY_LOCK_DIR="${SB}/lock"
}

run_deploy() { # run_deploy [额外环境...]；输出存 OUT，退出码存 RC
  OUT="$(
    cd "${LOCAL_REPO}"
    env SYNC_SSH_CMD="${SHIM}/fake-ssh" \
        SYNC_REMOTE_HOST="fake@host" \
        SYNC_REMOTE_PATH="${SRV}" \
        DEPLOY_LOCK_DIR="${DEPLOY_LOCK_DIR}" \
        DEPLOY_HEALTH_TRIES=2 DEPLOY_HEALTH_INTERVAL=0 \
        SYNC_FETCH_TIMEOUT=10 \
        PATH="${STUBS}:${PATH}" \
        "$@" \
        bash "${LOCAL_REPO}/sync.sh" 2>&1
  )"
  RC=$?
}

assert_stage_fail() { # assert_stage_fail <阶段> <用例名>
  if (( RC != 0 )) && grep -q "\[STAGE:$1\] FAIL" <<<"${OUT}"; then ok "$2 -> $1 按预期拦截"; else bad "$2 (期望 STAGE:$1 FAIL, rc=${RC})" "${OUT}"; fi
}

srv_head() { git -C "${SRV}" rev-parse HEAD; }
lock_gone() { [[ ! -e "${DEPLOY_LOCK_DIR}" ]]; }

# ================================================================ 用例开始
say "== 用例 1: 正常快进部署（smoke 范围） =="
new_sandbox t1
run_deploy DEPLOY_TEST_SCOPE=smoke
if (( RC == 0 )) \
  && grep -q "\[STAGE:UPDATE\] PASS" <<<"${OUT}" \
  && grep -q "\[STAGE:E2E\] PASS" <<<"${OUT}" \
  && [[ "$(srv_head)" == "$(git -C "${LOCAL_REPO}" rev-parse HEAD)" ]]; then
  ok "全流程通过且服务器 HEAD 已对齐"
else bad "正常部署失败 rc=${RC}" "${OUT}"; fi
lock_gone && ok "部署锁已释放" || bad "锁未释放: $(ls "${DEPLOY_LOCK_DIR}" 2>/dev/null)" ""

say "== 用例 2: 错误分支 =="
new_sandbox t2
git -C "${LOCAL_REPO}" checkout --quiet -b feature/wrong HEAD~1
run_deploy
assert_stage_fail LOCAL_BRANCH "非 test 分支被拒"

say "== 用例 3: 本地未提交修改 =="
new_sandbox t3
echo dirty >> "${LOCAL_REPO}/seed.txt"
run_deploy
assert_stage_fail LOCAL_CLEAN "脏工作树被拒"

say "== 用例 4: 本地提交未 push =="
new_sandbox t4
git -C "${LOCAL_REPO}" commit --quiet --allow-empty -m "wip"
run_deploy
assert_stage_fail LOCAL_PUSHED "未推送被拒"

say "== 用例 5: 服务器脏工作树 =="
new_sandbox t5
echo hack > "${SRV}/untracked_source.go"
run_deploy DEPLOY_TEST_SCOPE=none
assert_stage_fail SERVER_CLEAN "服务器脏工作树被拒"

say "== 用例 6: 非快进更新 =="
new_sandbox t6
git -C "${SRV}" checkout --quiet -B test "$(git -C "${SRV}" rev-parse HEAD)"
git -C "${SRV}" commit --quiet --allow-empty -m "server-only divergent commit"
run_deploy DEPLOY_TEST_SCOPE=none
assert_stage_fail SERVER_FF "分叉历史被拒"

say "== 用例 7: 构建失败（旧容器应不受影响） =="
new_sandbox t7
echo sentinel-running > "${SB}/old_containers.state"
run_deploy DEPLOY_TEST_SCOPE=none FAIL_AT=BUILD
assert_stage_fail BUILD "构建失败被报告"
grep -q "旧容器未受影响" <<<"${OUT}" && ok "提示旧容器不受影响" || bad "缺少旧容器提示" "${OUT}"
[[ "$(cat "${SB}/old_containers.state")" == "sentinel-running" ]] && ok "旧容器状态未被触碰" || bad "旧容器状态被改" ""

say "== 用例 8: UP 后健康检查失败 =="
new_sandbox t8
run_deploy DEPLOY_TEST_SCOPE=none FAIL_AT=HEALTH
assert_stage_fail HEALTH "健康检查失败被报告"
grep -q "不要在服务器上回退或修改文件" <<<"${OUT}" && ok "给出 revert/fix 提示" || bad "缺少上线后处置提示" "${OUT}"
lock_gone && ok "失败后锁已释放" || bad "失败后锁残留" ""

say "== 用例 9: 无更新重复部署（NEED_TRANSFER=0 路径） =="
new_sandbox t9
run_deploy DEPLOY_TEST_SCOPE=smoke >/dev/null 2>&1
run_deploy DEPLOY_TEST_SCOPE=smoke
if (( RC == 0 )) && grep -q "\[STAGE:SERVER_FETCH\] PASS up-to-date" <<<"${OUT}"; then
  ok "重复部署走 up-to-date 快速通道"
else bad "重复部署异常 rc=${RC}" "${OUT}"; fi

say ""
say "=============================================="
say "结果: PASS=${PASS} FAIL=${FAIL}  沙箱: ${SANDBOX_ROOT}"
(( FAIL == 0 )) || exit 1
exit 0
