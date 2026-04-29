#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

GATEWAY_CLASS="${GATEWAY_CLASS:-appnet}"
CONFORMANCE_PROFILES="${GATEWAY_API_CONFORMANCE_PROFILES:-GATEWAY-HTTP,GATEWAY-GRPC,GATEWAY-TLS}"
SUPPORTED_FEATURES="${GATEWAY_API_CONFORMANCE_FEATURES:-Gateway,ReferenceGrant,HTTPRoute,GRPCRoute,TLSRoute}"
REPORT_OUTPUT="${GATEWAY_API_CONFORMANCE_REPORT:-}"
SKIP_TESTS="${GATEWAY_API_CONFORMANCE_SKIP_TESTS:-}"
RUN_TEST="${GATEWAY_API_CONFORMANCE_RUN_TEST:-}"
ALLOW_CRDS_MISMATCH="${GATEWAY_API_CONFORMANCE_ALLOW_CRDS_MISMATCH:-true}"
export GATEWAY_API_CONFORMANCE_ECHO_IMAGE="${GATEWAY_API_CONFORMANCE_ECHO_IMAGE:-akstraffic.azurecr.io/mgdgtw/gateway-api/echo-basic:v20260204-monthly-2026.01-60-g28382302}"
export GATEWAY_API_CONFORMANCE_COREDNS_IMAGE="${GATEWAY_API_CONFORMANCE_COREDNS_IMAGE:-akstraffic.azurecr.io/mgdgtw/gateway-api/coredns:v1.12.2}"

args=(
  -gateway-class "${GATEWAY_CLASS}"
  -conformance-profiles "${CONFORMANCE_PROFILES}"
  -supported-features "${SUPPORTED_FEATURES}"
)

if [[ -n "${REPORT_OUTPUT}" ]]; then
  mkdir -p "$(dirname "${REPORT_OUTPUT}")"
  args+=(
    -report-output "${REPORT_OUTPUT}"
    -organization "${GATEWAY_API_CONFORMANCE_ORGANIZATION:-Microsoft}"
    -project "${GATEWAY_API_CONFORMANCE_PROJECT:-AppNet Managed Gateway}"
    -url "${GATEWAY_API_CONFORMANCE_URL:?GATEWAY_API_CONFORMANCE_URL is required when GATEWAY_API_CONFORMANCE_REPORT is set}"
  )
fi
if [[ -n "${SKIP_TESTS}" ]]; then
  args+=(-skip-tests "${SKIP_TESTS}")
fi
if [[ -n "${RUN_TEST}" ]]; then
  args+=(-run-test "${RUN_TEST}")
fi
if [[ "${ALLOW_CRDS_MISMATCH}" == "true" ]]; then
  args+=(-allow-crds-mismatch=true)
fi

go test -tags conformance ./controller/internal/conformance -run TestGatewayAPIStandardConformance -count=1 -args "${args[@]}"
