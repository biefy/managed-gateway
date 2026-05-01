#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

require_azure_tools
select_subscription

if ! az group show --name "${AZURE_RESOURCE_GROUP}" >/dev/null 2>&1; then
  az group create \
    --name "${AZURE_RESOURCE_GROUP}" \
    --location "${AZURE_LOCATION}" \
    --output none
fi

if ! az network vnet show --resource-group "${AZURE_RESOURCE_GROUP}" --name "${AKS_VNET_NAME}" >/dev/null 2>&1; then
  az network vnet create \
    --resource-group "${AZURE_RESOURCE_GROUP}" \
    --location "${AZURE_LOCATION}" \
    --name "${AKS_VNET_NAME}" \
    --address-prefixes 10.240.0.0/16 \
    --subnet-name snet-infra \
    --subnet-prefixes 10.240.0.0/22 \
    --output none
fi

az network vnet subnet create \
  --resource-group "${AZURE_RESOURCE_GROUP}" \
  --vnet-name "${AKS_VNET_NAME}" \
  --name snet-infra \
  --address-prefixes 10.240.0.0/22 \
  --output none
az network vnet subnet create \
  --resource-group "${AZURE_RESOURCE_GROUP}" \
  --vnet-name "${AKS_VNET_NAME}" \
  --name snet-workload-alice \
  --address-prefixes 10.240.4.0/22 \
  --output none
az network vnet subnet create \
  --resource-group "${AZURE_RESOURCE_GROUP}" \
  --vnet-name "${AKS_VNET_NAME}" \
  --name snet-workload-bob \
  --address-prefixes 10.240.8.0/22 \
  --output none

if ! az keyvault show --resource-group "${AZURE_RESOURCE_GROUP}" --name "${AKS_KEYVAULT_NAME}" >/dev/null 2>&1; then
  az keyvault create \
    --resource-group "${AZURE_RESOURCE_GROUP}" \
    --location "${AZURE_LOCATION}" \
    --name "${AKS_KEYVAULT_NAME}" \
    --enable-rbac-authorization false \
    --output none
fi

ensure_kv_identity() {
  local purpose="$1"
  local name
  local principal_id
  name="$(kv_identity_name "${purpose}")"
  if ! az identity show --resource-group "${AZURE_RESOURCE_GROUP}" --name "${name}" >/dev/null 2>&1; then
    az identity create \
      --resource-group "${AZURE_RESOURCE_GROUP}" \
      --location "${AZURE_LOCATION}" \
      --name "${name}" \
      --output none
  fi
  principal_id="$(kv_identity_principal_id "${purpose}")"
  az keyvault set-policy \
    --name "${AKS_KEYVAULT_NAME}" \
    --object-id "${principal_id}" \
    --secret-permissions get \
    --output none
}

ensure_kv_identity tenant-alice
ensure_kv_identity tenant-bob
ensure_kv_identity workload-alice
ensure_kv_identity workload-bob

principal_id="${AZURE_PRINCIPAL_OBJECT_ID:-}"
if [[ -z "${principal_id}" ]]; then
  principal_id="$(az ad signed-in-user show --query id --output tsv 2>/dev/null || true)"
fi
if [[ -n "${principal_id}" ]]; then
  az keyvault set-policy \
    --name "${AKS_KEYVAULT_NAME}" \
    --object-id "${principal_id}" \
    --secret-permissions get set \
    --output none
fi

echo "AKS infrastructure ready: resourceGroup=${AZURE_RESOURCE_GROUP} vnet=${AKS_VNET_NAME} keyVault=${AKS_KEYVAULT_NAME}"
