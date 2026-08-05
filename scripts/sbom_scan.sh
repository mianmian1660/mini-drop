#!/usr/bin/env bash
set -euo pipefail

IMAGE_PREFIX="${IMAGE_PREFIX:-mini-drop-project}"
OUT_DIR="${OUT_DIR:-./sbom}"
mkdir -p "$OUT_DIR"

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "[sbom] missing tool: $1" >&2
    return 1
  fi
}

if ! need syft; then
  echo "[sbom] install syft first: https://github.com/anchore/syft" >&2
  exit 2
fi

scanner=""
if command -v grype >/dev/null 2>&1; then
  scanner="grype"
elif command -v trivy >/dev/null 2>&1; then
  scanner="trivy"
else
  echo "[sbom] install grype or trivy for vulnerability scanning" >&2
  exit 2
fi

images=(
  "${IMAGE_PREFIX}-apiserver:latest"
  "${IMAGE_PREFIX}-analysis:latest"
  "${IMAGE_PREFIX}-drop_agent:latest"
  "${IMAGE_PREFIX}-web_frontend:latest"
)

for image in "${images[@]}"; do
  safe_name="${image//[:\/]/_}"
  echo "[sbom] generating SBOM for ${image}"
  syft "$image" -o "spdx-json=${OUT_DIR}/${safe_name}.spdx.json"
  echo "[sbom] scanning ${image} with ${scanner}"
  if [[ "$scanner" == "grype" ]]; then
    grype "$image" -o table
  else
    trivy image --exit-code 1 --severity HIGH,CRITICAL "$image"
  fi
done
