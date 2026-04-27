#!/usr/bin/env bash
# Build the forked pilot-discovery image and push it to the registry.
set -euo pipefail

IMG="${IMG:-akstraffic.azurecr.io/mgdgtw/istiod:agentgw}"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
BUILDER="${BUILDER:-appnet-multiarch}"

if ! docker buildx inspect "${BUILDER}" >/dev/null 2>&1; then
  docker buildx create --name "${BUILDER}" --driver docker-container --use >/dev/null
fi
docker buildx use "${BUILDER}"
docker buildx inspect --bootstrap >/dev/null

cd "$(dirname "$0")/.."
echo "==> Building and pushing ${IMG} (${PLATFORMS})"
DOCKER_BUILDKIT=1 docker buildx build \
  --platform "${PLATFORMS}" \
  -t "${IMG}" \
  -f Dockerfile.istiod \
  --push \
  .
echo "pushed ${IMG}"
