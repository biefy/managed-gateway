#!/usr/bin/env bash
# Build the local agentgateway fork image and push it to the registry.
set -euo pipefail

SCRIPT_DIR="${BASH_SOURCE[0]%/*}"
REPO_ROOT="${SCRIPT_DIR}/.."
AGENTGATEWAY_DIR="${AGENTGATEWAY_DIR:-${REPO_ROOT}/../biefy/agentgateway}"
IMAGE="${IMAGE:-akstraffic.azurecr.io/mgdgtw/agentgateway:dev}"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
VERSION="${VERSION:-dev}"
GIT_REVISION="${GIT_REVISION:-dev}"
BUILDER="${BUILDER:-appnet-multiarch}"

if [[ ! -f "${AGENTGATEWAY_DIR}/Dockerfile" ]]; then
  echo "agentgateway checkout not found at ${AGENTGATEWAY_DIR}; set AGENTGATEWAY_DIR to github.com/biefy/agentgateway" >&2
  exit 1
fi

if ! docker buildx inspect "${BUILDER}" >/dev/null 2>&1; then
  docker buildx create --name "${BUILDER}" --driver docker-container --use >/dev/null
fi
docker buildx use "${BUILDER}"
docker buildx inspect --bootstrap >/dev/null

echo "==> Building and pushing ${IMAGE} (${PLATFORMS})"
DOCKER_BUILDKIT=1 docker buildx build \
  --platform "${PLATFORMS}" \
  --build-arg VERSION="${VERSION}" \
  --build-arg GIT_REVISION="${GIT_REVISION}" \
  -f "${AGENTGATEWAY_DIR}/Dockerfile" \
  -t "${IMAGE}" \
  --push \
  "${AGENTGATEWAY_DIR}"
echo "pushed ${IMAGE}"
