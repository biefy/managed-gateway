#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

require_azure_tools
require_cmd openssl
select_subscription

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

store_secret_file() {
  local name="$1"
  local file="$2"
  az keyvault secret set \
    --vault-name "${AKS_KEYVAULT_NAME}" \
    --name "${name}" \
    --file "${file}" \
    --output none
}

generate_ca_hierarchy() {
  echo "==> Generating root and intermediate CAs"
  openssl genrsa -out "${TMP_DIR}/root-key.pem" 4096
  openssl req -x509 -new -nodes \
    -key "${TMP_DIR}/root-key.pem" \
    -sha256 \
    -days 3650 \
    -out "${TMP_DIR}/root-cert.pem" \
    -subj "/O=appnet.azure.com/CN=Root CA"

  cat > "${TMP_DIR}/ext.cnf" <<'EOF'
basicConstraints=CA:TRUE
keyUsage = cRLSign, keyCertSign
EOF

  for cluster in infra workload-alice workload-bob; do
    openssl genrsa -out "${TMP_DIR}/${cluster}-ca-key.pem" 4096
    openssl req -new \
      -key "${TMP_DIR}/${cluster}-ca-key.pem" \
      -out "${TMP_DIR}/${cluster}-ca.csr" \
      -subj "/O=appnet.azure.com/CN=${cluster} Intermediate CA"
    openssl x509 -req \
      -in "${TMP_DIR}/${cluster}-ca.csr" \
      -CA "${TMP_DIR}/root-cert.pem" \
      -CAkey "${TMP_DIR}/root-key.pem" \
      -CAcreateserial \
      -days 3650 \
      -extfile "${TMP_DIR}/ext.cnf" \
      -out "${TMP_DIR}/${cluster}-ca-cert.pem"
    cat "${TMP_DIR}/${cluster}-ca-cert.pem" "${TMP_DIR}/root-cert.pem" > "${TMP_DIR}/${cluster}-cert-chain.pem"
  done

  store_secret_file root-key-pem "${TMP_DIR}/root-key.pem"
  store_secret_file root-cert-pem "${TMP_DIR}/root-cert.pem"
  for cluster in infra workload-alice workload-bob; do
    store_secret_file "${cluster}-ca-key-pem" "${TMP_DIR}/${cluster}-ca-key.pem"
    store_secret_file "${cluster}-ca-cert-pem" "${TMP_DIR}/${cluster}-ca-cert.pem"
    store_secret_file "${cluster}-cert-chain-pem" "${TMP_DIR}/${cluster}-cert-chain.pem"
  done
}

if keyvault_secret_exists root-cert-pem && [[ "${ROTATE_CA:-0}" != "1" ]]; then
  echo "==> CA hierarchy already exists in Key Vault ${AKS_KEYVAULT_NAME}"
else
  generate_ca_hierarchy
fi

TENANT_ID="$(az account show --query tenantId --output tsv)"

apply_tenant_cacerts() {
  local namespace="$1"
  local ca_cluster="$2"
  local identity_client_id
  identity_client_id="$(kv_identity_client_id "${namespace}")"

  kubectl --context aks-infra create namespace "${namespace}" --dry-run=client -o yaml \
    | kubectl --context aks-infra apply -f - >/dev/null

  cat <<EOF | kubectl --context aks-infra apply -f - >/dev/null
apiVersion: v1
kind: ServiceAccount
metadata:
  name: istiod-keyvault
  namespace: ${namespace}
  annotations:
    azure.workload.identity/client-id: ${identity_client_id}
---
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: cacerts-keyvault
  namespace: ${namespace}
spec:
  provider: azure
  secretObjects:
    - secretName: cacerts
      type: Opaque
      data:
        - objectName: ca-key.pem
          key: ca-key.pem
        - objectName: ca-cert.pem
          key: ca-cert.pem
        - objectName: root-cert.pem
          key: root-cert.pem
        - objectName: cert-chain.pem
          key: cert-chain.pem
  parameters:
    usePodIdentity: "false"
    clientID: ${identity_client_id}
    keyvaultName: ${AKS_KEYVAULT_NAME}
    tenantId: ${TENANT_ID}
    objects: |
      array:
        - |
          objectName: ${ca_cluster}-ca-key-pem
          objectType: secret
          objectAlias: ca-key.pem
        - |
          objectName: ${ca_cluster}-ca-cert-pem
          objectType: secret
          objectAlias: ca-cert.pem
        - |
          objectName: root-cert-pem
          objectType: secret
          objectAlias: root-cert.pem
        - |
          objectName: ${ca_cluster}-cert-chain-pem
          objectType: secret
          objectAlias: cert-chain.pem
EOF
}

apply_workload_root_cert() {
  local context="$1"
  local cluster="$2"
  local identity_client_id
  identity_client_id="$(kv_identity_client_id "${cluster}")"

  kubectl --context "${context}" create namespace istio-system --dry-run=client -o yaml \
    | kubectl --context "${context}" apply -f - >/dev/null

  cat <<EOF | kubectl --context "${context}" apply -f - >/dev/null
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: istio-root-cert-keyvault
  namespace: istio-system
spec:
  provider: azure
  parameters:
    usePodIdentity: "false"
    clientID: ${identity_client_id}
    keyvaultName: ${AKS_KEYVAULT_NAME}
    tenantId: ${TENANT_ID}
    objects: |
      array:
        - |
          objectName: root-cert-pem
          objectType: secret
          objectAlias: root-cert.pem
EOF
}

apply_tenant_cacerts tenant-alice workload-alice
apply_tenant_cacerts tenant-bob workload-bob
apply_workload_root_cert aks-workload-alice workload-alice
apply_workload_root_cert aks-workload-bob workload-bob

echo "CA SecretProviderClass resources applied from Key Vault ${AKS_KEYVAULT_NAME}"
