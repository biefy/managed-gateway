#!/usr/bin/env bash
# Build the forked pilot-discovery image and push it to the registry.
set -euo pipefail

SCRIPT_DIR="${BASH_SOURCE[0]%/*}"
REPO_ROOT="${SCRIPT_DIR}/.."
ISTIO_DIR="${ISTIO_DIR:-${REPO_ROOT}/../biefy/istio}"
IMG="${IMG:-akstraffic.azurecr.io/mgdgtw/istiod:agentgw}"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
BUILDER="${BUILDER:-appnet-multiarch}"

if [[ ! -f "${ISTIO_DIR}/go.mod" ]]; then
  echo "Istio checkout not found at ${ISTIO_DIR}; set ISTIO_DIR to github.com/biefy/istio" >&2
  exit 1
fi

if ! docker buildx inspect "${BUILDER}" >/dev/null 2>&1; then
  docker buildx create --name "${BUILDER}" --driver docker-container --use >/dev/null
fi
docker buildx use "${BUILDER}"
docker buildx inspect --bootstrap >/dev/null

echo "==> Building and pushing ${IMG} from ${ISTIO_DIR} (${PLATFORMS})"
DOCKER_BUILDKIT=1 docker buildx build \
  --platform "${PLATFORMS}" \
  -t "${IMG}" \
  -f "${REPO_ROOT}/Dockerfile.istiod" \
  --push \
  "${ISTIO_DIR}"
echo "pushed ${IMG}"
