#!/usr/bin/env bash
# Replace the remote parca-agent container and point it at the central Parca server.
set -euo pipefail

REMOTE_HOST="${REMOTE_HOST:?set REMOTE_HOST, for example ubuntu@111.230.29.115}"
CENTER_GRPC_ADDR="${CENTER_GRPC_ADDR:?set CENTER_GRPC_ADDR, for example <center-ip>:7070}"
REMOTE_AGENT_IP="${REMOTE_AGENT_IP:?set REMOTE_AGENT_IP, for example 111.230.29.115}"
REMOTE_AGENT_IP_SLUG="${REMOTE_AGENT_IP//./-}"
REMOTE_NODE="${REMOTE_NODE:-cloud-server-${REMOTE_AGENT_IP_SLUG}}"
MINI_DROP_ENV="${MINI_DROP_ENV:-development}"
PARCA_AGENT_IMAGE="${PARCA_AGENT_IMAGE:-ghcr.io/parca-dev/parca-agent:v0.49.0}"
PARCA_AGENT_BIN="${PARCA_AGENT_BIN:-}"
PARCA_AGENT_EXTRA_ARGS="${PARCA_AGENT_EXTRA_ARGS:---remote-store-use-v2-schema=false}"

ssh "${REMOTE_HOST}" bash -s -- "${CENTER_GRPC_ADDR}" "${REMOTE_AGENT_IP}" "${REMOTE_NODE}" "${MINI_DROP_ENV}" "${PARCA_AGENT_IMAGE}" "${PARCA_AGENT_BIN}" "${PARCA_AGENT_EXTRA_ARGS}" <<'REMOTE_SCRIPT'
set -euo pipefail
center_grpc_addr="$1"
remote_agent_ip="$2"
remote_node="$3"
mini_drop_env="$4"
image="$5"
agent_bin="$6"
extra_args_raw="${7:-}"

if [ -z "${remote_node}" ]; then
  remote_node="$(hostname)"
fi
agent_cmd=()
if [ -n "${agent_bin}" ]; then
  agent_cmd=("${agent_bin}")
fi
extra_args=()
if [ -n "${extra_args_raw}" ]; then
  read -r -a extra_args <<< "${extra_args_raw}"
fi

for name in mini-drop-parca-agent parca-agent parca_agent mini-drop-project-parca_agent-1; do
  docker rm -f "${name}" >/dev/null 2>&1 || true
done
docker ps -aq --filter "label=mini-drop.role=parca-agent" | xargs -r docker rm -f >/dev/null 2>&1 || true
docker run -d --name mini-drop-parca-agent \
  --label mini-drop.role=parca-agent \
  --label mini-drop.instance="${remote_agent_ip}" \
  --restart unless-stopped \
  --privileged \
  --pid host \
  --network host \
  --security-opt apparmor=unconfined \
  --cap-add SYS_ADMIN \
  --cap-add SYS_PTRACE \
  --cap-add PERFMON \
  --cap-add BPF \
  --cap-add SYS_RESOURCE \
  --ulimit memlock=-1:-1 \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v /sys/kernel/debug:/sys/kernel/debug:rw \
  -v /sys/kernel/tracing:/sys/kernel/tracing:rw \
  -v /sys/fs/bpf:/sys/fs/bpf:rw \
  -v /proc:/host/proc:ro \
  "${image}" \
  "${agent_cmd[@]}" \
  --node="${remote_node}" \
  --remote-store-address="${center_grpc_addr}" \
  --remote-store-insecure \
  "${extra_args[@]}" \
  --metadata-external-labels="job=hotmethod;env=${mini_drop_env};instance=${remote_agent_ip};node=${remote_node}" \
  --log-level=info

docker logs --tail=80 mini-drop-parca-agent
REMOTE_SCRIPT

echo "Remote parca-agent updated on ${REMOTE_HOST} (${REMOTE_AGENT_IP})"
