#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

check_no_match() {
  local description="$1"
  local pattern="$2"
  shift 2

  local matches
  if matches=$(grep -RInE "${pattern}" "$@" 2>/dev/null); then
    echo "${description}:" >&2
    printf '%s\n' "${matches}" >&2
    exit 1
  fi
}

echo "==> go test"
go test ./...

echo "==> go vet"
go vet ./...

echo "==> conformance compile"
go test -tags conformance ./controller/internal/conformance -run '^$'

echo "==> bash syntax"
find hack deploy/aks -name '*.sh' -print0 | xargs -0 -n1 bash -n

echo "==> release guardrails"
if git ls-files --error-unmatch xds-echo >/dev/null 2>&1 && [[ -e xds-echo ]]; then
  echo "xds-echo must not be tracked or present in the repository root" >&2
  exit 1
fi

for script in hack/build-*.sh; do
  [[ -e "${script}" ]] || continue
  if ! grep -q 'PUSH="${PUSH:-0}"' "${script}"; then
    echo "${script} must default PUSH to 0" >&2
    exit 1
  fi
done

check_no_match \
  "remote mutable image defaults are not allowed in release files" \
  "akstraffic\\.azurecr\\.io/[^[:space:]\"']+:(dev|latest)" \
  hack/build-*.sh Dockerfile.* .github deploy

check_no_match \
  "default cluster-admin bootstrap is not allowed" \
  "cluster-admin" \
  deploy/aks/e2e.sh deploy/manifests

check_no_match \
  "public NodePort range exposure is not allowed" \
  "30000-32767|30000/32767" \
  deploy

check_no_match \
  "plaintext xDS must not be exposed by manifests or scripts" \
  "15010" \
  deploy/aks deploy/manifests/ambient deploy/manifests/controller

check_no_match \
  "credential literals are not allowed in release files" \
  "(password|client_secret|client-secret|access_token|private_key)[[:space:]]*[:=][[:space:]]*['\"][^'\"]+" \
  hack Dockerfile.* .github

echo "==> static checks passed"
