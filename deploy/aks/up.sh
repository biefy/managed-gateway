#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

require_azure_tools
select_subscription

create_cluster() {
  local name="$1"
  local subnet="$2"
  local service_cidr="$3"
  local dns_ip="$4"
  local subnet_resource_id
  subnet_resource_id="$(subnet_id "${subnet}")"

  if cluster_exists "${name}"; then
    echo "==> AKS cluster already exists: ${name}"
  else
    echo "==> Creating AKS cluster: ${name}"
    az aks create \
      --resource-group "${AZURE_RESOURCE_GROUP}" \
      --name "${name}" \
      --location "${AZURE_LOCATION}" \
      --node-count "${AKS_NODE_COUNT}" \
      --node-vm-size "${AKS_NODE_VM_SIZE}" \
      --network-plugin azure \
      --vnet-subnet-id "${subnet_resource_id}" \
      --service-cidr "${service_cidr}" \
      --dns-service-ip "${dns_ip}" \
      --load-balancer-sku standard \
      --enable-oidc-issuer \
      --enable-workload-identity \
      --enable-addons azure-keyvault-secrets-provider \
      --attach-acr "${ACR_RESOURCE_ID}" \
      --generate-ssh-keys \
      --output none
  fi

  az aks update \
    --resource-group "${AZURE_RESOURCE_GROUP}" \
    --name "${name}" \
    --attach-acr "${ACR_RESOURCE_ID}" \
    --output none

  az aks get-credentials \
    --resource-group "${AZURE_RESOURCE_GROUP}" \
    --name "${name}" \
    --overwrite-existing \
    --output none

  local target_context
  local current_context
  target_context="$(cluster_context "${name}")"
  current_context="$(kubectl config current-context)"
  if [[ "${current_context}" != "${target_context}" ]]; then
    kubectl config rename-context "${current_context}" "${target_context}" >/dev/null 2>&1 || true
  fi
  kubectl config use-context "${target_context}" >/dev/null
}

ensure_nsg_intravnet() {
  local resource_group="$1"
  local nsg_name="$2"
  az network nsg rule create \
    --resource-group "${resource_group}" \
    --nsg-name "${nsg_name}" \
    --name Allow-AKS-IntraVNet \
    --priority 110 \
    --direction Inbound \
    --access Allow \
    --protocol '*' \
    --source-address-prefixes 10.240.0.0/16 \
    --source-port-ranges '*' \
    --destination-address-prefixes 10.240.0.0/16 \
    --destination-port-ranges '*' \
    --output none
}

ensure_nsg_public_gateway() {
  local resource_group="$1"
  local nsg_name="$2"
  az network nsg rule create \
    --resource-group "${resource_group}" \
    --nsg-name "${nsg_name}" \
    --name Allow-AKS-Public-Gateway-Inbound \
    --priority 100 \
    --direction Inbound \
    --access Allow \
    --protocol Tcp \
    --source-address-prefixes Internet \
    --source-port-ranges '*' \
    --destination-address-prefixes 10.240.0.0/22 \
    --destination-port-ranges 80 443 \
    --output none
}

ensure_nsg_load_balancer() {
  local resource_group="$1"
  local nsg_name="$2"
  local destination_ports=(${AZURE_LOAD_BALANCER_DESTINATION_PORTS:-80 443})
  az network nsg rule create \
    --resource-group "${resource_group}" \
    --nsg-name "${nsg_name}" \
    --name Allow-AKS-LoadBalancer-Inbound \
    --priority 102 \
    --direction Inbound \
    --access Allow \
    --protocol Tcp \
    --source-address-prefixes AzureLoadBalancer \
    --source-port-ranges '*' \
    --destination-address-prefixes 10.240.0.0/16 \
    --destination-port-ranges "${destination_ports[@]}" \
    --output none
}

ensure_subnet_nsg_intravnet() {
  local subnet="$1"
  local nsg_id
  nsg_id="$(az network vnet subnet show \
    --resource-group "${AZURE_RESOURCE_GROUP}" \
    --vnet-name "${AKS_VNET_NAME}" \
    --name "${subnet}" \
    --query networkSecurityGroup.id \
    --output tsv)"
  if [[ -z "${nsg_id}" || "${nsg_id}" == "None" ]]; then
    return
  fi
  ensure_nsg_intravnet "${AZURE_RESOURCE_GROUP}" "${nsg_id##*/}"
  ensure_nsg_load_balancer "${AZURE_RESOURCE_GROUP}" "${nsg_id##*/}"
  if [[ "${subnet}" == "snet-infra" ]]; then
    ensure_nsg_public_gateway "${AZURE_RESOURCE_GROUP}" "${nsg_id##*/}"
  fi
}

ensure_cluster_nsg_intravnet() {
  local cluster="$1"
  local node_resource_group
  local nsg_name
  node_resource_group="$(az aks show \
    --resource-group "${AZURE_RESOURCE_GROUP}" \
    --name "${cluster}" \
    --query nodeResourceGroup \
    --output tsv)"
  while IFS= read -r nsg_name; do
    [[ -z "${nsg_name}" ]] && continue
    ensure_nsg_intravnet "${node_resource_group}" "${nsg_name}"
    ensure_nsg_load_balancer "${node_resource_group}" "${nsg_name}"
    if [[ "${cluster}" == "infra" ]]; then
      ensure_nsg_public_gateway "${node_resource_group}" "${nsg_name}"
    fi
  done < <(az network nsg list --resource-group "${node_resource_group}" --query '[].name' --output tsv)
}

create_cluster infra snet-infra 10.250.0.0/16 10.250.0.10
create_cluster workload-alice snet-workload-alice 10.251.0.0/16 10.251.0.10
create_cluster workload-bob snet-workload-bob 10.252.0.0/16 10.252.0.10

ensure_subnet_nsg_intravnet snet-infra
ensure_subnet_nsg_intravnet snet-workload-alice
ensure_subnet_nsg_intravnet snet-workload-bob
ensure_cluster_nsg_intravnet infra
ensure_cluster_nsg_intravnet workload-alice
ensure_cluster_nsg_intravnet workload-bob

ensure_federated_credential() {
  local purpose="$1"
  local cluster="$2"
  local namespace="$3"
  local service_account="$4"
  local name="$5"
  local identity_name
  local issuer
  identity_name="$(kv_identity_name "${purpose}")"
  issuer="$(az aks show \
    --resource-group "${AZURE_RESOURCE_GROUP}" \
    --name "${cluster}" \
    --query oidcIssuerProfile.issuerUrl \
    --output tsv)"
  if az identity federated-credential show \
    --resource-group "${AZURE_RESOURCE_GROUP}" \
    --identity-name "${identity_name}" \
    --name "${name}" >/dev/null 2>&1; then
    return
  fi
  az identity federated-credential create \
    --resource-group "${AZURE_RESOURCE_GROUP}" \
    --identity-name "${identity_name}" \
    --name "${name}" \
    --issuer "${issuer}" \
    --subject "system:serviceaccount:${namespace}:${service_account}" \
    --audiences api://AzureADTokenExchange \
    --output none
}

ensure_federated_credential tenant-alice infra tenant-alice istiod-keyvault infra-tenant-alice-istiod
ensure_federated_credential tenant-bob infra tenant-bob istiod-keyvault infra-tenant-bob-istiod
ensure_federated_credential workload-alice workload-alice istio-system ztunnel workload-alice-ztunnel
ensure_federated_credential workload-alice workload-alice istio-system eastwest-gateway workload-alice-eastwest
ensure_federated_credential workload-bob workload-bob istio-system ztunnel workload-bob-ztunnel
ensure_federated_credential workload-bob workload-bob istio-system eastwest-gateway workload-bob-eastwest

GATEWAY_API_URL="https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml"
for ctx in aks-workload-alice aks-workload-bob; do
  echo "==> Installing Gateway API CRDs into ${ctx}"
  kubectl --context "${ctx}" apply -f "${GATEWAY_API_URL}"
  kubectl --context "${ctx}" wait --for=condition=Established --timeout=120s \
    crd/backendtlspolicies.gateway.networking.k8s.io \
    crd/gatewayclasses.gateway.networking.k8s.io \
    crd/gateways.gateway.networking.k8s.io \
    crd/grpcroutes.gateway.networking.k8s.io \
    crd/httproutes.gateway.networking.k8s.io \
    crd/listenersets.gateway.networking.k8s.io \
    crd/referencegrants.gateway.networking.k8s.io \
    crd/tlsroutes.gateway.networking.k8s.io
 done

echo "AKS clusters ready: aks-infra aks-workload-alice aks-workload-bob"
