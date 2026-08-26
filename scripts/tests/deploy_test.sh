#!/usr/bin/env bash
# deploy.sh + deploy_remote.sh 的无网络沙箱测试。

set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC_ROOT="$(cd "${HERE}/../.." && pwd)"
SANDBOX_ROOT="$(mktemp -d /tmp/mini-drop-deploy-test.XXXXXX)"
PASS=0
FAIL=0

cleanup() { rm -rf "${SANDBOX_ROOT}"; }
trap cleanup EXIT

ok() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
bad() {
  FAIL=$((FAIL + 1))
  printf '  FAIL %s\n' "$1"
  printf '%s\n' "${2:-}" | sed 's/^/    | /'
}

new_sandbox() {
  local name="$1"
  SB="${SANDBOX_ROOT}/${name}"
  ORIGIN="${SB}/origin.git"
  LOCAL_REPO="${SB}/local"
  SERVER_REPO="${SB}/server"
  SHIMS="${SB}/shims"
  LOCK_FILE="${SB}/deploy.lock"
  DOCKER_LOG="${SB}/docker.log"
  DOCKER_STATE="${SB}/docker.state"
  mkdir -p "${SB}" "${SHIMS}"

  git init --quiet --bare -b main "${ORIGIN}"
  git clone --quiet "${ORIGIN}" "${LOCAL_REPO}"
  (
    cd "${LOCAL_REPO}"
    mkdir -p scripts
    cp "${SRC_ROOT}/deploy.sh" deploy.sh
    cp "${SRC_ROOT}/sync.sh" sync.sh
    cp "${SRC_ROOT}/scripts/deploy_remote.sh" scripts/deploy_remote.sh
    printf '#!/usr/bin/env bash\nexit 0\n' > scripts/e2e_smoke.sh
    printf 'seed\n' > app.txt
    chmod +x deploy.sh sync.sh scripts/deploy_remote.sh scripts/e2e_smoke.sh
    git add -A
    git commit --quiet -m seed
    git push --quiet origin main
    git switch --quiet -c feature/example
    printf 'feature\n' >> app.txt
    git commit --quiet -am feature
    git push --quiet -u origin feature/example
    git switch --quiet main
    printf 'main-v2\n' >> app.txt
    git commit --quiet -am main-v2
    git push --quiet origin main
  )

  git clone --quiet --branch main "${ORIGIN}" "${SERVER_REPO}"
  # 让服务器 main 落后，以覆盖同分支更新。
  git -C "${SERVER_REPO}" reset --quiet --hard HEAD~1

  cat > "${SHIMS}/fake-ssh" <<'SHIM'
#!/usr/bin/env bash
shift
exec bash -c "$*"
SHIM
  cat > "${SHIMS}/flock" <<'SHIM'
#!/usr/bin/env bash
exit 0
SHIM
  cat > "${SHIMS}/docker" <<'SHIM'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${DEPLOY_DOCKER_LOG}"
if [[ "$*" == "compose ps -q "* ]]; then
  service="${!#}"
  if [[ "${MUTATE_NON_FRONTEND:-}" == "1" && -f "${DEPLOY_DOCKER_STATE}" && "${service}" == "drop_agent" ]]; then
    printf 'container-%s-changed\n' "${service}"
  elif [[ "${MUTATE_NON_AGENT:-}" == "1" && -f "${DEPLOY_DOCKER_STATE}" && "${service}" == "drop_server" ]]; then
    printf 'container-%s-changed\n' "${service}"
  else
    printf 'container-%s\n' "${service}"
  fi
  exit 0
fi
if [[ "$1" == "inspect" && "$2" == "--format" ]]; then
  container_id="${!#}"
  if [[ "${MUTATE_NON_FRONTEND_START:-}" == "1" && -f "${DEPLOY_DOCKER_STATE}" && "${container_id}" == "container-drop_agent" ]]; then
    printf '2026-08-27T00:01:00Z\n'
  else
    printf '2026-08-27T00:00:00Z\n'
  fi
  exit 0
fi
if [[ "${FAIL_AT:-}" == BUILD && "$*" == *"compose build"* ]]; then exit 90; fi
if [[ "${FAIL_AT:-}" == UP && "$*" == *"compose up"* ]]; then exit 91; fi
if [[ ( "${MUTATE_NON_FRONTEND:-}" == "1" || "${MUTATE_NON_FRONTEND_START:-}" == "1" ) && "$*" == "compose up -d --no-deps web_frontend" ]]; then
  : > "${DEPLOY_DOCKER_STATE}"
fi
if [[ "${MUTATE_NON_AGENT:-}" == "1" && "$*" == "compose up -d --no-deps drop_agent" ]]; then
  : > "${DEPLOY_DOCKER_STATE}"
fi
exit 0
SHIM
  cat > "${SHIMS}/curl" <<'SHIM'
#!/usr/bin/env bash
[[ "${FAIL_AT:-}" != HEALTH ]]
SHIM
  cat > "${SHIMS}/make" <<'SHIM'
#!/usr/bin/env bash
[[ "${FAIL_AT:-}" != TESTS ]]
SHIM
  chmod +x "${SHIMS}"/*

  export DEPLOY_SSH_CMD="${SHIMS}/fake-ssh"
  export DEPLOY_REMOTE_HOST="fake@server"
  export DEPLOY_REMOTE_PATH="${SERVER_REPO}"
  export DEPLOY_LOCK_FILE="${LOCK_FILE}"
  export DEPLOY_DOCKER_LOG="${DOCKER_LOG}"
  export DEPLOY_DOCKER_STATE="${DOCKER_STATE}"
  export DEPLOY_HEALTH_TRIES=2
  export DEPLOY_HEALTH_INTERVAL=0
  export DEPLOY_FETCH_TIMEOUT=10
  export PATH="${SHIMS}:${PATH}"
}

run_deploy() {
  local branch="$1"
  shift
  OUT="$(cd "${LOCAL_REPO}" && env "$@" ./deploy.sh "${branch}" 2>&1)"
  RC=$?
}

expect_success() {
  if (( RC == 0 )) && grep -q '\[STAGE:ALL\] PASS' <<<"${OUT}"; then
    ok "$1"
  else
    bad "$1 (rc=${RC})" "${OUT}"
  fi
}

expect_failure() {
  local stage="$1"
  local label="$2"
  if (( RC != 0 )) && grep -q "\[STAGE:${stage}\] FAIL" <<<"${OUT}"; then
    ok "${label}"
  else
    bad "${label} (期望 ${stage}, rc=${RC})" "${OUT}"
  fi
}

printf '== 任意远程分支部署测试 ==\n'

new_sandbox switch
run_deploy main DEPLOY_TEST_SCOPE=none
expect_success "同分支快进 main"
grep -qx 'compose build' "${DOCKER_LOG}" && grep -qx 'compose up -d' "${DOCKER_LOG}" \
  && ok "默认 full 范围保持完整 Compose 构建与启动" \
  || bad "默认 full 范围命令发生变化" "$(cat "${DOCKER_LOG}")"
[[ "$(git -C "${SERVER_REPO}" rev-parse HEAD)" == "$(git --git-dir="${ORIGIN}" rev-parse refs/heads/main)" ]] \
  && ok "服务器 main 精确等于 origin/main" || bad "main SHA 不一致" "${OUT}"
run_deploy feature/example DEPLOY_TEST_SCOPE=none
expect_success "main 切换到 feature/example"
[[ "$(git -C "${SERVER_REPO}" branch --show-current)" == feature/example ]] \
  && ok "服务器显示目标功能分支" || bad "功能分支名错误" "${OUT}"
run_deploy main DEPLOY_TEST_SCOPE=none
expect_success "feature/example 切回 main"

new_sandbox missing
run_deploy fix/not-found DEPLOY_TEST_SCOPE=none
expect_failure REMOTE_BRANCH "不存在的远程分支被拒绝"

new_sandbox local_dirty
printf 'dirty\n' >> "${LOCAL_REPO}/app.txt"
run_deploy main DEPLOY_TEST_SCOPE=none
expect_failure LOCAL_CLEAN "本地脏工作树被拒绝"

new_sandbox server_dirty
printf 'dirty\n' > "${SERVER_REPO}/untracked.go"
run_deploy main DEPLOY_TEST_SCOPE=none
expect_failure SERVER_CLEAN "服务器脏工作树被拒绝"
rm "${SERVER_REPO}/untracked.go"
run_deploy main DEPLOY_TEST_SCOPE=none
expect_success "失败退出后部署锁自动释放"

new_sandbox fetch_fallback
git -C "${SERVER_REPO}" remote set-url origin "${SB}/unavailable-origin.git"
run_deploy main DEPLOY_TEST_SCOPE=none
expect_success "服务器 origin 失败后使用源仓库地址拉取"
grep -q '源仓库地址重试' <<<"${OUT}" \
  && ok "fetch 回退路径被明确记录" \
  || bad "fetch 回退路径未被记录" "${OUT}"
[[ "$(git -C "${SERVER_REPO}" rev-parse HEAD)" == "$(git --git-dir="${ORIGIN}" rev-parse refs/heads/main)" ]] \
  && ok "fetch 回退后仍精确锁定目标 SHA" || bad "fetch 回退后的 SHA 不一致" "${OUT}"

new_sandbox build_failure
run_deploy main DEPLOY_TEST_SCOPE=none FAIL_AT=BUILD
expect_failure BUILD "镜像构建失败被报告"

new_sandbox health_failure
run_deploy main DEPLOY_TEST_SCOPE=none FAIL_AT=HEALTH
expect_failure HEALTH "健康检查失败被报告"

new_sandbox frontend
run_deploy main DEPLOY_TEST_SCOPE=none DEPLOY_SERVICE_SCOPE=frontend
expect_success "前端独立部署成功"
grep -qx 'compose build web_frontend' "${DOCKER_LOG}" \
  && grep -qx 'compose up -d --no-deps web_frontend' "${DOCKER_LOG}" \
  && ! grep -qx 'compose build' "${DOCKER_LOG}" \
  && ! grep -qx 'compose up -d' "${DOCKER_LOG}" \
  && ok "前端范围仅构建和更新 web_frontend" \
  || bad "前端范围触发了其他 Compose 命令" "$(cat "${DOCKER_LOG}")"
grep -q '\[STAGE:E2E_MULTILANG\] SKIP service_scope=frontend' <<<"${OUT}" \
  && ok "前端范围跳过重启 Agent 的多语言 E2E" \
  || bad "前端范围未明确跳过多语言 E2E" "${OUT}"

new_sandbox frontend_container_changed
run_deploy main DEPLOY_TEST_SCOPE=none DEPLOY_SERVICE_SCOPE=frontend MUTATE_NON_FRONTEND=1
expect_failure NON_FRONTEND_UNCHANGED "非前端容器变化会使前端部署失败"

new_sandbox frontend_container_restarted
run_deploy main DEPLOY_TEST_SCOPE=none DEPLOY_SERVICE_SCOPE=frontend MUTATE_NON_FRONTEND_START=1
expect_failure NON_FRONTEND_UNCHANGED "非前端容器启动时间变化会使前端部署失败"

new_sandbox agent
run_deploy main DEPLOY_TEST_SCOPE=none DEPLOY_SERVICE_SCOPE=agent
expect_success "Agent 独立部署成功"
grep -qx 'compose build drop_agent' "${DOCKER_LOG}" \
  && grep -qx 'compose up -d --no-deps drop_agent' "${DOCKER_LOG}" \
  && ! grep -qx 'compose build' "${DOCKER_LOG}" \
  && ! grep -qx 'compose up -d' "${DOCKER_LOG}" \
  && ok "Agent 范围仅构建和更新 drop_agent" \
  || bad "Agent 范围触发了其他 Compose 命令" "$(cat "${DOCKER_LOG}")"
grep -q '\[STAGE:E2E_MULTILANG\] SKIP service_scope=agent' <<<"${OUT}" \
  && ok "Agent 范围跳过会创建任务的多语言 E2E" \
  || bad "Agent 范围未明确跳过多语言 E2E" "${OUT}"

new_sandbox agent_container_changed
run_deploy main DEPLOY_TEST_SCOPE=none DEPLOY_SERVICE_SCOPE=agent MUTATE_NON_AGENT=1
expect_failure NON_AGENT_UNCHANGED "非 Agent 容器变化会使 Agent 部署失败"

new_sandbox invalid
OUT="$(cd "${LOCAL_REPO}" && ./deploy.sh 'bad..branch' 2>&1)"
RC=$?
expect_failure CONFIG "非法分支名被拒绝"
run_deploy main DEPLOY_SERVICE_SCOPE=backend
expect_failure CONFIG "非法服务范围被拒绝"

printf '结果: PASS=%d FAIL=%d\n' "${PASS}" "${FAIL}"
(( FAIL == 0 ))
