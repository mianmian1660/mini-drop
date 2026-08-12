#!/usr/bin/env bash
# Read-only checks for MiniDrop remote parca-agent visibility through central Parca gRPC.
set -euo pipefail

PARCA_GRPC_ADDR="${PARCA_GRPC_ADDR:-parca:7070}"
REMOTE_INSTANCE="${REMOTE_INSTANCE:-111.230.29.115}"
REMOTE_JOB="${REMOTE_JOB:-hotmethod}"
REMOTE_NODE="${REMOTE_NODE:-cloud-server-111-230-29-115}"
REMOTE_HOST="${REMOTE_HOST:-ubuntu@${REMOTE_INSTANCE}}"
PROFILE_TYPE="${PROFILE_TYPE:-parca_agent:samples:count:cpu:nanoseconds:delta}"
GRPCURL_IMAGE="${GRPCURL_IMAGE:-fullstorydev/grpcurl:latest}"

grpcurl_compose() {
  docker run --rm --network mini-drop-project_default "${GRPCURL_IMAGE}" -plaintext "$@"
}

echo "== Parca gRPC services =="
grpcurl_compose "${PARCA_GRPC_ADDR}" list | sed -n '/parca.query.v1alpha1.QueryService/p'

echo
echo "== Profile types =="
grpcurl_compose -d '{}' "${PARCA_GRPC_ADDR}" parca.query.v1alpha1.QueryService/ProfileTypes

echo
echo "== job label values =="
grpcurl_compose \
  -d "{\"labelName\":\"job\",\"profileType\":\"${PROFILE_TYPE}\"}" \
  "${PARCA_GRPC_ADDR}" parca.query.v1alpha1.QueryService/Values

echo
echo "== instance label values =="
grpcurl_compose \
  -d "{\"labelName\":\"instance\",\"profileType\":\"${PROFILE_TYPE}\"}" \
  "${PARCA_GRPC_ADDR}" parca.query.v1alpha1.QueryService/Values

echo
echo "== node label values =="
grpcurl_compose \
  -d "{\"labelName\":\"node\",\"profileType\":\"${PROFILE_TYPE}\"}" \
  "${PARCA_GRPC_ADDR}" parca.query.v1alpha1.QueryService/Values

echo
echo "== remote parca-agent container =="
ssh "${REMOTE_HOST}" "docker ps --filter name=mini-drop-parca-agent --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Command}}'"

echo
echo "== remote parca-agent recent errors =="
ssh "${REMOTE_HOST}" "docker logs --tail=200 mini-drop-parca-agent 2>&1 | grep -Ei 'error|failed|denied|remote-store|tracer|write|grpc|ebpf' || true"

echo
echo "Expected remote labels: job=${REMOTE_JOB}, instance=${REMOTE_INSTANCE}, node=${REMOTE_NODE}"
