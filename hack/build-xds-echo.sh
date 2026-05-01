#!/usr/bin/env bash
# Build the xds-echo image locally; set PUSH=1 to publish it.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
IMG="${IMG:-managed-gateway/xds-echo:local}"
PUSH="${PUSH:-0}"

if [[ "${PUSH}" != "0" && "${PUSH}" != "1" ]]; then
  echo "PUSH must be 0 or 1" >&2
  exit 1
fi

cd "${REPO_ROOT}"
DOCKER_BUILDKIT=1 docker build -t "${IMG}" -f Dockerfile.xds-echo .
if [[ "${PUSH}" == "1" ]]; then
  docker push "${IMG}"
fi
echo "==> Done"
