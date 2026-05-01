#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
source "${SCRIPT_DIR}/common.sh"

require_azure_tools
select_subscription

AMBIENT_TEMPLATE="${REPO_ROOT}/deploy/manifests/ambient/ambient.yaml"
EASTWEST_TEMPLATE="${REPO_ROOT}/deploy/manifests/ambient/eastwest-gateway.yaml"
AGENTGATEWAY_IMAGE="${AGENTGATEWAY_IMAGE:-managed-gateway/agentgateway:local}"

wait_lb_ip() {
  local context="$1"
  local namespace="$2"
  local service="$3"
  local deadline=$((SECONDS + 600))
  local ip=""
  while (( SECONDS < deadline )); do
    ip="$(kubectl --context "${context}" -n "${namespace}" get svc "${service}" -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [[ -n "${ip}" ]]; then
      echo "${ip}"
      return 0
    fi
    sleep 5
  done
  echo "timed out waiting for ${context}/${namespace}/${service} LoadBalancer IP" >&2
  return 1
}

is_private_ip() {
  local ip="$1"
  [[ "${ip}" == 10.* ]] || [[ "${ip}" == 192.168.* ]] || [[ "${ip}" =~ ^172\.(1[6-9]|2[0-9]|3[0-1])\. ]]
}

install_ambient() {
  local member_context="$1"
  local tenant_namespace="$2"
  local cluster_id="$3"
  local ca_cluster_id="$4"
  local workload_identity_client_id="$5"

  echo "==> Waiting for tenant istiod in ${tenant_namespace}"
  kubectl --context aks-infra -n "${tenant_namespace}" rollout status deploy/istiod --timeout=300s
  local istiod_ip
  istiod_ip="$(wait_lb_ip aks-infra "${tenant_namespace}" istiod)"
  if ! is_private_ip "${istiod_ip}"; then
    echo "tenant istiod Service ${tenant_namespace}/istiod is not internal: ${istiod_ip}" >&2
    return 1
  fi

  echo "==> Installing ambient in ${member_context} via tenant istiod ${istiod_ip}"
  kubectl --context "${member_context}" create namespace istio-system --dry-run=client -o yaml \
    | kubectl --context "${member_context}" apply -f - >/dev/null
  kubectl --context "${member_context}" label namespace istio-system \
    "topology.istio.io/network=${cluster_id}" --overwrite >/dev/null

  sed -e "s/__INFRA_NODE_IP__/${istiod_ip}/g" \
      -e "s/__ISTIOD_TLS_NODEPORT__/15012/g" \
      -e "s/__CA_CLUSTER_ID__/${ca_cluster_id}/g" \
      -e "s/__WORKLOAD_IDENTITY_CLIENT_ID__/${workload_identity_client_id}/g" \
      "${AMBIENT_TEMPLATE}" \
    | kubectl --context "${member_context}" apply -f - >/dev/null

  sed -e "s/__CLUSTER_ID__/${cluster_id}/g" \
      -e "s/__CA_CLUSTER_ID__/${ca_cluster_id}/g" \
      -e "s/__NETWORK__/${cluster_id}/g" \
      -e "s/__WORKLOAD_IDENTITY_CLIENT_ID__/${workload_identity_client_id}/g" \
      -e "s#__AGENTGATEWAY_IMAGE__#${AGENTGATEWAY_IMAGE}#g" \
      "${EASTWEST_TEMPLATE}" \
    | kubectl --context "${member_context}" apply -f - >/dev/null

  kubectl --context "${member_context}" -n istio-system rollout restart deploy/eastwest-gateway >/dev/null
  kubectl --context "${member_context}" -n istio-system rollout status ds/ztunnel --timeout=300s
  kubectl --context "${member_context}" -n istio-system rollout status deploy/eastwest-gateway --timeout=300s

  local eastwest_ip
  eastwest_ip="$(wait_lb_ip "${member_context}" istio-system eastwest-gateway)"
  if ! is_private_ip "${eastwest_ip}"; then
    echo "east-west Service in ${member_context} is not internal: ${eastwest_ip}" >&2
    return 1
  fi
  echo "==> ${member_context} east-west gateway internal IP: ${eastwest_ip}"
}

install_ambient aks-workload-alice tenant-alice workload-alice alice "$(kv_identity_client_id workload-alice)"
install_ambient aks-workload-bob tenant-bob workload-bob bob "$(kv_identity_client_id workload-bob)"

echo "AKS ambient install complete"
