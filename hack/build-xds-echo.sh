#!/usr/bin/env bash
# Build the xds-echo image and push it to the registry.
set -euo pipefail
IMG="${IMG:-akstraffic.azurecr.io/mgdgtw/xds-echo:dev}"
cd "$(dirname "$0")/.."
DOCKER_BUILDKIT=1 docker build -t "$IMG" -f Dockerfile.xds-echo .
docker push "$IMG"
echo "pushed $IMG"
