#!/usr/bin/env bash
# Build the current drop_agent image and replace the remote agent container.
set -euo pipefail

REMOTE_HOST="${REMOTE_HOST:?set REMOTE_HOST, for example ubuntu@111.230.29.115}"
CENTER_ADDR="${CENTER_ADDR:?set CENTER_ADDR, for example <center-ip>:50051}"
REMOTE_AGENT_IP="${REMOTE_AGENT_IP:?set REMOTE_AGENT_IP, for example 111.230.29.115}"
REMOTE_AGENT_IP_SLUG="${REMOTE_AGENT_IP//./-}"
REMOTE_AGENT_ID="${REMOTE_AGENT_ID:-cloud-agent-${REMOTE_AGENT_IP_SLUG}}"
REMOTE_HOSTNAME="${REMOTE_HOSTNAME:-cloud-server-${REMOTE_AGENT_IP_SLUG}}"
IMAGE_NAME="${IMAGE_NAME:-mini-drop-remote-drop-agent:latest}"
TAR_PATH="/tmp/mini-drop-remote-drop-agent.tar"

DOCKER_BUILDKIT=0 docker build -t "${IMAGE_NAME}" -f drop/Dockerfile drop
docker save "${IMAGE_NAME}" -o "${TAR_PATH}"
scp "${TAR_PATH}" "${REMOTE_HOST}:${TAR_PATH}"

ssh "${REMOTE_HOST}" bash -s -- "${TAR_PATH}" "${IMAGE_NAME}" "${CENTER_ADDR}" "${REMOTE_AGENT_IP}" "${REMOTE_AGENT_ID}" "${REMOTE_HOSTNAME}" <<'REMOTE_SCRIPT'
set -euo pipefail
tar_path="$1"
image_name="$2"
center_addr="$3"
remote_agent_ip="$4"
remote_agent_id="$5"
remote_hostname="$6"

docker load -i "${tar_path}"
for name in mini-drop-drop-agent drop_agent mini-drop-project-drop_agent-1; do
  docker rm -f "${name}" >/dev/null 2>&1 || true
done
docker ps -aq --filter "label=mini-drop.role=drop-agent" | xargs -r docker rm -f >/dev/null 2>&1 || true
docker run -d --name mini-drop-drop-agent \
  --label mini-drop.role=drop-agent \
  --label mini-drop.agent-id="${remote_agent_id}" \
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
  -e DROP_PERF_BIN=/usr/local/bin/perf-real \
  -e DROP_AGENT_IP="${remote_agent_ip}" \
  -e DROP_AGENT_HOSTNAME="${remote_hostname}" \
  -e DROP_AGENT_UID="${remote_agent_id}" \
  -v /sys/kernel/debug:/sys/kernel/debug:rw \
  -v /sys/kernel/tracing:/sys/kernel/tracing:rw \
  -v /sys/kernel/tracing:/sys/kernel/debug/tracing:rw \
  -v /sys/fs/bpf:/sys/fs/bpf:rw \
  -v /lib/modules:/lib/modules:ro \
  "${image_name}" ./drop_agent "${center_addr}"

docker logs --tail=80 mini-drop-drop-agent
REMOTE_SCRIPT

echo "Remote drop_agent updated on ${REMOTE_HOST} (${REMOTE_AGENT_IP})"
