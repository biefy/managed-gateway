#!/usr/bin/env bash
set -euo pipefail

AZURE_SUBSCRIPTION_ID="${AZURE_SUBSCRIPTION_ID:-c9e491c5-2dc4-4254-9c7f-cee19a234b2a}"
AZURE_RESOURCE_GROUP="${AZURE_RESOURCE_GROUP:-rg-mgd-gtw}"
AZURE_LOCATION="${AZURE_LOCATION:-centralus}"
AKS_VNET_NAME="${AKS_VNET_NAME:-vnet-mgd-gtw-centralus}"
AKS_KEYVAULT_NAME="${AKS_KEYVAULT_NAME:-kvmgdgtw${AZURE_SUBSCRIPTION_ID:0:6}cu}"
ACR_NAME="${ACR_NAME:-akstraffic}"
ACR_RESOURCE_ID="${ACR_RESOURCE_ID:-/subscriptions/bc7e0da9-5e4c-4a91-9252-9658837006cf/resourceGroups/aks_traffic_infra_rg/providers/Microsoft.ContainerRegistry/registries/akstraffic}"
AKS_NODE_COUNT="${AKS_NODE_COUNT:-2}"
AKS_NODE_VM_SIZE="${AKS_NODE_VM_SIZE:-Standard_D4ds_v5}"
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.2.1}"

TENANT_ALICE_KV_IDENTITY="${TENANT_ALICE_KV_IDENTITY:-id-mgd-gtw-tenant-alice-kv}"
TENANT_BOB_KV_IDENTITY="${TENANT_BOB_KV_IDENTITY:-id-mgd-gtw-tenant-bob-kv}"
WORKLOAD_ALICE_KV_IDENTITY="${WORKLOAD_ALICE_KV_IDENTITY:-id-mgd-gtw-workload-alice-kv}"
WORKLOAD_BOB_KV_IDENTITY="${WORKLOAD_BOB_KV_IDENTITY:-id-mgd-gtw-workload-bob-kv}"

require_cmd() {
  command -v "$1" >/dev/null || { echo "missing required command: $1" >&2; exit 1; }
}

require_azure_tools() {
  require_cmd az
  require_cmd kubectl
}

select_subscription() {
  az account set --subscription "${AZURE_SUBSCRIPTION_ID}"
}

subnet_id() {
  az network vnet subnet show \
    --resource-group "${AZURE_RESOURCE_GROUP}" \
    --vnet-name "${AKS_VNET_NAME}" \
    --name "$1" \
    --query id \
    --output tsv
}

cluster_context() {
  case "$1" in
    infra) echo aks-infra ;;
    workload-alice) echo aks-workload-alice ;;
    workload-bob) echo aks-workload-bob ;;
    *) echo "unknown cluster: $1" >&2; exit 1 ;;
  esac
}

cluster_exists() {
  az aks show --resource-group "${AZURE_RESOURCE_GROUP}" --name "$1" >/dev/null 2>&1
}

keyvault_secret_exists() {
  az keyvault secret show --vault-name "${AKS_KEYVAULT_NAME}" --name "$1" >/dev/null 2>&1
}

kv_identity_name() {
  case "$1" in
    tenant-alice) echo "${TENANT_ALICE_KV_IDENTITY}" ;;
    tenant-bob) echo "${TENANT_BOB_KV_IDENTITY}" ;;
    workload-alice) echo "${WORKLOAD_ALICE_KV_IDENTITY}" ;;
    workload-bob) echo "${WORKLOAD_BOB_KV_IDENTITY}" ;;
    *) echo "unknown Key Vault identity: $1" >&2; exit 1 ;;
  esac
}

kv_identity_client_id() {
  az identity show \
    --resource-group "${AZURE_RESOURCE_GROUP}" \
    --name "$(kv_identity_name "$1")" \
    --query clientId \
    --output tsv
}

kv_identity_principal_id() {
  az identity show \
    --resource-group "${AZURE_RESOURCE_GROUP}" \
    --name "$(kv_identity_name "$1")" \
    --query principalId \
    --output tsv
}
