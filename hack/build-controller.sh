#!/usr/bin/env bash
# Build and push the controller image.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
IMAGE="${IMAGE:-akstraffic.azurecr.io/mgdgtw/controller:dev}"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
BUILDER="${BUILDER:-appnet-multiarch}"

if ! docker buildx inspect "${BUILDER}" >/dev/null 2>&1; then
  docker buildx create --name "${BUILDER}" --driver docker-container --use >/dev/null
fi
docker buildx use "${BUILDER}"
docker buildx inspect --bootstrap >/dev/null

echo "==> Building and pushing ${IMAGE} (${PLATFORMS})"
cd "${REPO_ROOT}"
docker buildx build \
  --platform "${PLATFORMS}" \
  -f Dockerfile.controller \
  -t "${IMAGE}" \
  --push \
  .
echo "==> Done"
