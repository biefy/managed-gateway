#!/usr/bin/env bash
# Build the controller image locally; set PUSH=1 to publish it.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
IMAGE="${IMAGE:-managed-gateway/controller:local}"
PUSH="${PUSH:-0}"
BUILDER="${BUILDER:-appnet-multiarch}"

case "$(uname -m)" in
  arm64|aarch64) LOCAL_PLATFORM="linux/arm64" ;;
  *) LOCAL_PLATFORM="linux/amd64" ;;
esac

if [[ "${PUSH}" == "1" ]]; then
  PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
  output_arg=(--push)
  verb="Building and pushing"
elif [[ "${PUSH}" == "0" ]]; then
  PLATFORMS="${PLATFORMS:-${LOCAL_PLATFORM}}"
  output_arg=(--load)
  verb="Building locally"
else
  echo "PUSH must be 0 or 1" >&2
  exit 1
fi

if ! docker buildx inspect "${BUILDER}" >/dev/null 2>&1; then
  docker buildx create --name "${BUILDER}" --driver docker-container --use >/dev/null
fi
docker buildx use "${BUILDER}"
docker buildx inspect --bootstrap >/dev/null

echo "==> ${verb} ${IMAGE} (${PLATFORMS})"
cd "${REPO_ROOT}"
docker buildx build \
  --platform "${PLATFORMS}" \
  -f Dockerfile.controller \
  -t "${IMAGE}" \
  "${output_arg[@]}" \
  .
echo "==> Done"
