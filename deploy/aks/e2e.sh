#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
source "${SCRIPT_DIR}/common.sh"

require_azure_tools
require_cmd openssl
select_subscription

fail=0
ok()  { echo "  OK    $1"; }
bad() { echo "  FAIL  $1"; fail=1; }

kc_infra() { kubectl --context aks-infra "$@"; }
kc_alice() { kubectl --context aks-workload-alice "$@"; }
kc_bob()   { kubectl --context aks-workload-bob "$@"; }

is_private_ip() {
  local ip="$1"
  [[ "${ip}" == 10.* ]] || [[ "${ip}" == 192.168.* ]] || [[ "${ip}" =~ ^172\.(1[6-9]|2[0-9]|3[0-1])\. ]]
}

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
  return 1
}

wait_gateway_programmed() {
  local context="$1"
  local namespace="$2"
  local gateway="$3"
  local deadline=$((SECONDS + 300))
  local programmed=""
  while (( SECONDS < deadline )); do
    programmed="$(kubectl --context "${context}" -n "${namespace}" get gateway "${gateway}" -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null || true)"
    if [[ "${programmed}" == "True" ]]; then
      return 0
    fi
    sleep 5
  done
  echo "${programmed:-missing}"
  return 1
}

csi_mount_ready() {
  local context="$1"
  local namespace="$2"
  local pod_prefix="$3"
  local secret_provider_class="$4"
  kubectl --context "${context}" -n "${namespace}" get secretproviderclasspodstatus \
    -o jsonpath="{range .items[?(@.status.secretProviderClassName=='${secret_provider_class}')]}{.status.podName}{'\t'}{.status.mounted}{'\n'}{end}" \
    | grep -q "^${pod_prefix}.*[[:space:]]true$"
}

wait_csi_mount_ready() {
  local context="$1"
  local namespace="$2"
  local pod_prefix="$3"
  local secret_provider_class="$4"
  local deadline=$((SECONDS + 120))
  while (( SECONDS < deadline )); do
    if csi_mount_ready "${context}" "${namespace}" "${pod_prefix}" "${secret_provider_class}"; then
      return 0
    fi
    sleep 5
  done
  return 1
}

wait_curl_body() {
  local expected="$1"
  shift
  local deadline=$((SECONDS + 120))
  local body=""
  while (( SECONDS < deadline )); do
    body="$(curl "$@" 2>&1 || true)"
    if [[ "${body}" == "${expected}" ]]; then
      echo "${body}"
      return 0
    fi
    sleep 5
  done
  echo "${body}"
  return 1
}

wait_curl_code() {
  local expected="$1"
  shift
  local deadline=$((SECONDS + 120))
  local code=""
  while (( SECONDS < deadline )); do
    code="$(curl "$@" 2>&1 || true)"
    if [[ "${code}" == "${expected}" ]]; then
      echo "${code}"
      return 0
    fi
    sleep 5
  done
  echo "${code}"
  return 1
}

run_alice_no_client_probe() {
  local pod="mtls-probe"
  kc_alice -n demo delete pod "${pod}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  kc_alice -n demo run "${pod}" --restart=Never --image=curlimages/curl:8.10.1 --command -- sleep 3600 >/dev/null
  if ! kc_alice -n demo wait --for=condition=Ready "pod/${pod}" --timeout=120s >/dev/null 2>&1; then
    kc_alice -n demo delete pod "${pod}" --wait=false >/dev/null 2>&1 || true
    echo "probe-not-ready"
    return 0
  fi
  kc_alice -n demo exec "${pod}" -- curl -sk -o /dev/null -m 10 -w '%{http_code}' https://echo.demo.svc.cluster.local:8443/ 2>/dev/null || true
  kc_alice -n demo delete pod "${pod}" --wait=false >/dev/null 2>&1 || true
}

install_gateway_api_crds() {
  local url="https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml"
  for ctx in aks-workload-alice aks-workload-bob; do
    echo "==> Installing Gateway API ${GATEWAY_API_VERSION} Standard CRDs into ${ctx}"
    kubectl --context "${ctx}" apply -f "${url}" >/dev/null
    kubectl --context "${ctx}" wait --for=condition=Established --timeout=120s \
      crd/backendtlspolicies.gateway.networking.k8s.io \
      crd/gatewayclasses.gateway.networking.k8s.io \
      crd/gateways.gateway.networking.k8s.io \
      crd/grpcroutes.gateway.networking.k8s.io \
      crd/httproutes.gateway.networking.k8s.io \
      crd/listenersets.gateway.networking.k8s.io \
      crd/referencegrants.gateway.networking.k8s.io \
      crd/tlsroutes.gateway.networking.k8s.io >/dev/null
  done
}

ensure_member_access() {
  local context="$1"
  kubectl --context "${context}" create namespace appnet-system --dry-run=client -o yaml \
    | kubectl --context "${context}" apply -f - >/dev/null
  kubectl --context "${context}" -n appnet-system create serviceaccount appnet-gateway-controller --dry-run=client -o yaml \
    | kubectl --context "${context}" apply -f - >/dev/null
  kubectl --context "${context}" create clusterrolebinding appnet-gateway-controller \
    --clusterrole=cluster-admin \
    --serviceaccount=appnet-system:appnet-gateway-controller \
    --dry-run=client -o yaml \
    | kubectl --context "${context}" apply -f - >/dev/null
}

create_member_kubeconfig_secret() {
  local context="$1"
  local namespace="$2"
  local secret="$3"
  local tmp
  local server
  local ca_data
  local token
  tmp="$(mktemp)"
  ensure_member_access "${context}"
  server="$(kubectl config view --context "${context}" --raw --minify -o jsonpath='{.clusters[0].cluster.server}')"
  ca_data="$(kubectl config view --context "${context}" --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
  token="$(kubectl --context "${context}" -n appnet-system create token appnet-gateway-controller --duration=24h)"
  cat > "${tmp}" <<EOF
apiVersion: v1
kind: Config
clusters:
- name: member
  cluster:
    server: ${server}
    certificate-authority-data: ${ca_data}
users:
- name: appnet-gateway-controller
  user:
    token: ${token}
contexts:
- name: member
  context:
    cluster: member
    user: appnet-gateway-controller
current-context: member
EOF
  kc_infra -n "${namespace}" create secret generic "${secret}" \
    --from-file=kubeconfig="${tmp}" \
    --dry-run=client -o yaml \
    | kc_infra apply -f - >/dev/null
  rm -f "${tmp}"
}

ensure_ambient_namespace() {
  local context="$1"
  local namespace="$2"
  kubectl --context "${context}" create namespace "${namespace}" --dry-run=client -o yaml \
    | kubectl --context "${context}" apply -f - >/dev/null
  kubectl --context "${context}" label namespace "${namespace}" istio.io/dataplane-mode=ambient --overwrite >/dev/null
}

apply_alice_backend_mtls_assets() {
  local tmp
  tmp="$(mktemp -d)"

  openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout "${tmp}/ca.key" \
    -out "${tmp}/ca.crt" \
    -days 365 \
    -subj "/CN=alice-backend-mtls-ca" >/dev/null 2>&1

  cat > "${tmp}/server.ext" <<'EOF'
subjectAltName=DNS:echo.demo.svc.cluster.local,DNS:echo.demo.svc,DNS:echo
extendedKeyUsage=serverAuth
EOF
  openssl genrsa -out "${tmp}/server.key" 2048 >/dev/null 2>&1
  openssl req -new \
    -key "${tmp}/server.key" \
    -out "${tmp}/server.csr" \
    -subj "/CN=echo.demo.svc.cluster.local" >/dev/null 2>&1
  openssl x509 -req \
    -in "${tmp}/server.csr" \
    -CA "${tmp}/ca.crt" \
    -CAkey "${tmp}/ca.key" \
    -CAcreateserial \
    -out "${tmp}/server.crt" \
    -days 365 \
    -sha256 \
    -extfile "${tmp}/server.ext" >/dev/null 2>&1

  cat > "${tmp}/client.ext" <<'EOF'
extendedKeyUsage=clientAuth
EOF
  openssl genrsa -out "${tmp}/client.key" 2048 >/dev/null 2>&1
  openssl req -new \
    -key "${tmp}/client.key" \
    -out "${tmp}/client.csr" \
    -subj "/CN=managed-gateway-alice" >/dev/null 2>&1
  openssl x509 -req \
    -in "${tmp}/client.csr" \
    -CA "${tmp}/ca.crt" \
    -CAkey "${tmp}/ca.key" \
    -CAcreateserial \
    -out "${tmp}/client.crt" \
    -days 365 \
    -sha256 \
    -extfile "${tmp}/client.ext" >/dev/null 2>&1

  kc_alice -n demo create secret generic echo-mtls-server \
    --from-file=tls.crt="${tmp}/server.crt" \
    --from-file=tls.key="${tmp}/server.key" \
    --from-file=ca.crt="${tmp}/ca.crt" \
    --dry-run=client -o yaml | kc_alice apply -f - >/dev/null
  kc_alice -n demo create configmap alice-backend-mtls \
    --from-file=ca.crt="${tmp}/ca.crt" \
    --dry-run=client -o yaml | kc_alice apply -f - >/dev/null
  kc_infra -n tenant-alice create secret generic alice-backend-mtls \
    --from-file=tls.crt="${tmp}/client.crt" \
    --from-file=tls.key="${tmp}/client.key" \
    --from-file=ca.crt="${tmp}/ca.crt" \
    --dry-run=client -o yaml | kc_infra apply -f - >/dev/null

  rm -rf "${tmp}"
}

echo "==> Materialize CA bundle from Key Vault"
bash "${SCRIPT_DIR}/ca.sh"

install_gateway_api_crds

if kc_infra -n tenant-alice get secretproviderclass cacerts-keyvault >/dev/null 2>&1; then ok "tenant-alice cacerts SecretProviderClass exists"; else bad "tenant-alice cacerts SecretProviderClass missing"; fi
if kc_infra -n tenant-bob get secretproviderclass cacerts-keyvault >/dev/null 2>&1; then ok "tenant-bob cacerts SecretProviderClass exists"; else bad "tenant-bob cacerts SecretProviderClass missing"; fi
if kc_alice -n istio-system get secretproviderclass istio-root-cert-keyvault >/dev/null 2>&1; then ok "alice root cert SecretProviderClass exists"; else bad "alice root cert SecretProviderClass missing"; fi
if kc_bob -n istio-system get secretproviderclass istio-root-cert-keyvault >/dev/null 2>&1; then ok "bob root cert SecretProviderClass exists"; else bad "bob root cert SecretProviderClass missing"; fi
if [[ -n "$(kc_infra -n tenant-alice get sa istiod-keyvault -o jsonpath='{.metadata.annotations.azure\.workload\.identity/client-id}' 2>/dev/null || true)" ]]; then ok "tenant-alice istiod workload identity ServiceAccount exists"; else bad "tenant-alice istiod workload identity ServiceAccount missing annotation"; fi
if [[ -n "$(kc_infra -n tenant-bob get sa istiod-keyvault -o jsonpath='{.metadata.annotations.azure\.workload\.identity/client-id}' 2>/dev/null || true)" ]]; then ok "tenant-bob istiod workload identity ServiceAccount exists"; else bad "tenant-bob istiod workload identity ServiceAccount missing annotation"; fi

echo "==> Create member-cluster kubeconfig Secrets in infra"
create_member_kubeconfig_secret aks-workload-alice tenant-alice alice-member-kubeconfig
create_member_kubeconfig_secret aks-workload-bob tenant-bob bob-member-kubeconfig

echo "==> Stamp per-tenant controller manifests"
PER_TENANT="${REPO_ROOT}/deploy/manifests/controller/per-tenant.yaml"
sed -e 's/__TENANT_NAME__/alice/g' -e 's/__INFRA_NAMESPACE__/tenant-alice/g' "${PER_TENANT}" \
  | kc_infra apply -f - >/dev/null
sed -e 's/__TENANT_NAME__/bob/g' -e 's/__INFRA_NAMESPACE__/tenant-bob/g' "${PER_TENANT}" \
  | kc_infra apply -f - >/dev/null
kc_infra -n tenant-alice rollout restart deploy/appnet-gateway-controller >/dev/null
kc_infra -n tenant-bob rollout restart deploy/appnet-gateway-controller >/dev/null
kc_infra -n tenant-alice rollout status deploy/appnet-gateway-controller --timeout=300s >/dev/null
kc_infra -n tenant-bob rollout status deploy/appnet-gateway-controller --timeout=300s >/dev/null
kc_infra -n tenant-alice rollout restart deploy/istiod >/dev/null
kc_infra -n tenant-bob rollout restart deploy/istiod >/dev/null
kc_infra -n tenant-alice rollout status deploy/istiod --timeout=300s >/dev/null
kc_infra -n tenant-bob rollout status deploy/istiod --timeout=300s >/dev/null

if kc_infra -n tenant-alice get secret cacerts -o jsonpath='{.data.ca-cert\.pem}{.data.ca-key\.pem}{.data.root-cert\.pem}{.data.cert-chain\.pem}' | grep -q .; then ok "tenant-alice cacerts Secret synced"; else bad "tenant-alice cacerts Secret not synced"; fi
if kc_infra -n tenant-bob get secret cacerts -o jsonpath='{.data.ca-cert\.pem}{.data.ca-key\.pem}{.data.root-cert\.pem}{.data.cert-chain\.pem}' | grep -q .; then ok "tenant-bob cacerts Secret synced"; else bad "tenant-bob cacerts Secret not synced"; fi

bash "${SCRIPT_DIR}/ambient-up.sh"

if kc_alice -n istio-system get sa ztunnel -o jsonpath='{.metadata.annotations.azure\.workload\.identity/client-id}' | grep -q .; then ok "alice ztunnel workload identity ServiceAccount annotated"; else bad "alice ztunnel workload identity ServiceAccount missing annotation"; fi
if kc_bob -n istio-system get sa ztunnel -o jsonpath='{.metadata.annotations.azure\.workload\.identity/client-id}' | grep -q .; then ok "bob ztunnel workload identity ServiceAccount annotated"; else bad "bob ztunnel workload identity ServiceAccount missing annotation"; fi
if wait_csi_mount_ready aks-workload-alice istio-system ztunnel istio-root-cert-keyvault; then ok "alice ztunnel mounted Key Vault root cert"; else bad "alice ztunnel root cert mount missing"; fi
if wait_csi_mount_ready aks-workload-bob istio-system ztunnel istio-root-cert-keyvault; then ok "bob ztunnel mounted Key Vault root cert"; else bad "bob ztunnel root cert mount missing"; fi
if wait_csi_mount_ready aks-workload-alice istio-system eastwest-gateway istio-root-cert-keyvault; then ok "alice east-west gateway mounted Key Vault root cert"; else bad "alice east-west root cert mount missing"; fi
if wait_csi_mount_ready aks-workload-bob istio-system eastwest-gateway istio-root-cert-keyvault; then ok "bob east-west gateway mounted Key Vault root cert"; else bad "bob east-west root cert mount missing"; fi

echo "==> Apply tenant sample resources"
ensure_ambient_namespace aks-workload-alice demo
ensure_ambient_namespace aks-workload-bob bob-demo
kc_infra apply -f "${REPO_ROOT}/deploy/manifests/sample/tls/alice-https-cert.yaml" >/dev/null
apply_alice_backend_mtls_assets
kc_alice apply -f "${REPO_ROOT}/deploy/manifests/sample/workload-alice/all.yaml" >/dev/null
kc_alice apply -f "${REPO_ROOT}/deploy/manifests/sample/workload-alice/https.yaml" >/dev/null
kc_bob apply -f "${REPO_ROOT}/deploy/manifests/sample/workload-bob/gateway.yaml" >/dev/null
kc_alice -n demo rollout restart deploy/echo >/dev/null

if wait_gateway_programmed aks-workload-alice demo demo-gw >/dev/null; then ok "demo-gw Programmed=True"; else bad "demo-gw Programmed=False"; fi
if wait_gateway_programmed aks-workload-bob bob-demo bob-gw >/dev/null; then ok "bob-gw Programmed=True"; else bad "bob-gw Programmed=False"; fi

kc_alice -n istio-system rollout status deploy/eastwest-gateway --timeout=300s >/dev/null
kc_bob -n istio-system rollout status deploy/eastwest-gateway --timeout=300s >/dev/null
kc_alice -n demo rollout status deploy/echo --timeout=300s >/dev/null
kc_infra -n tenant-alice rollout status deploy/gw-demo-demo-gw --timeout=300s >/dev/null
kc_infra -n tenant-bob rollout status deploy/gw-bob-demo-bob-gw --timeout=300s >/dev/null
kc_infra -n tenant-alice rollout restart deploy/istiod >/dev/null
kc_infra -n tenant-alice rollout status deploy/istiod --timeout=300s >/dev/null
kc_infra -n tenant-alice rollout restart deploy/gw-demo-demo-gw >/dev/null
kc_infra -n tenant-alice rollout status deploy/gw-demo-demo-gw --timeout=300s >/dev/null

ALICE_EW_IP="$(wait_lb_ip aks-workload-alice istio-system eastwest-gateway || true)"
BOB_EW_IP="$(wait_lb_ip aks-workload-bob istio-system eastwest-gateway || true)"
if [[ -n "${ALICE_EW_IP}" ]] && is_private_ip "${ALICE_EW_IP}"; then ok "alice east-west gateway is internal (${ALICE_EW_IP})"; else bad "alice east-west gateway IP is not internal: ${ALICE_EW_IP:-missing}"; fi
if [[ -n "${BOB_EW_IP}" ]] && is_private_ip "${BOB_EW_IP}"; then ok "bob east-west gateway is internal (${BOB_EW_IP})"; else bad "bob east-west gateway IP is not internal: ${BOB_EW_IP:-missing}"; fi

ALICE_IP="$(wait_lb_ip aks-infra tenant-alice gw-demo-demo-gw || true)"
BOB_IP="$(wait_lb_ip aks-infra tenant-bob gw-bob-demo-bob-gw || true)"
if [[ -n "${ALICE_IP}" ]] && ! is_private_ip "${ALICE_IP}"; then ok "alice managed gateway is public (${ALICE_IP})"; else bad "alice managed gateway IP is not public: ${ALICE_IP:-missing}"; fi
if [[ -n "${BOB_IP}" ]] && ! is_private_ip "${BOB_IP}"; then ok "bob managed gateway is public (${BOB_IP})"; else bad "bob managed gateway IP is not public: ${BOB_IP:-missing}"; fi

echo "==> Verify alice backend requires client certificate"
no_client_code="$(run_alice_no_client_probe)"
if [[ "${no_client_code}" == "000" || "${no_client_code}" == "400" || "${no_client_code}" == "495" || "${no_client_code}" == "496" ]]; then ok "alice backend rejects clients without cert"; else bad "alice backend accepted no-cert request with HTTP ${no_client_code}"; fi

echo "==> Curl alice gateway: expect HTTP 200 + echo body over backend mTLS"
body="$(wait_curl_body "hello-from-alice" -sS -m 10 "http://${ALICE_IP}/" || true)"
if [[ "${body}" == "hello-from-alice" ]]; then ok "alice round-trip body OK"; else bad "alice body: ${body}"; fi

echo "==> Curl alice HTTPS gateway: expect HTTPS 200 + echo body"
https_body="$(wait_curl_body "hello-from-alice" -sSk -m 10 --resolve "demo.example.com:443:${ALICE_IP}" "https://demo.example.com/" || true)"
if [[ "${https_body}" == "hello-from-alice" ]]; then ok "alice HTTPS round-trip body OK"; else bad "alice https body: ${https_body}"; fi

echo "==> Negative isolation: alice TLS Secret must NOT exist in member cluster"
if kc_alice -n demo get secret alice-https-cert >/dev/null 2>&1; then
  bad "alice-https-cert leaked into member cluster"
elif kc_alice get secret -A 2>/dev/null | grep -q alice-https-cert; then
  bad "alice-https-cert leaked into member cluster"
else
  ok "alice-https-cert absent from member cluster"
fi

echo "==> Curl bob gateway: expect HTTP 404"
code="$(wait_curl_code "404" -sS -o /dev/null -m 10 -w '%{http_code}' "http://${BOB_IP}/" || true)"
if [[ "${code}" == "404" ]]; then ok "bob isolation OK (404)"; else bad "bob got HTTP ${code}, expected 404"; fi

echo "==> Check managed gateway logs for missing east-west gateway warnings"
alice_logs="$(kc_infra -n tenant-alice logs -l app.kubernetes.io/name=agentgateway --all-containers --since=10m 2>/dev/null || true)"
if grep -q "workload on remote network but no gateway configured" <<<"${alice_logs}"; then
  bad "alice managed gateway reported missing network gateway"
else
  ok "alice managed gateway did not report missing network gateway"
fi

echo "==> Kill alice controller leader; confirm second replica takes over"
leader="$(kc_infra -n tenant-alice get lease appnet-gateway.controller -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)"
if [[ -n "${leader}" ]]; then
  leader_pod="${leader%%_*}"
  kc_infra -n tenant-alice delete pod "${leader_pod}" --wait=false >/dev/null 2>&1 || true
  sleep 30
  new_leader="$(kc_infra -n tenant-alice get lease appnet-gateway.controller -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)"
  if [[ -n "${new_leader}" && "${new_leader}" != "${leader}" ]]; then ok "leader failover: ${leader} -> ${new_leader}"; else bad "leader failover did not occur (still ${new_leader})"; fi
else
  bad "no leader lease found in tenant-alice"
fi

if [[ "${fail}" -eq 0 ]]; then
  echo
  echo "AKS E2E: PASS"
  exit 0
fi

echo
echo "AKS E2E: FAIL"
exit 1
