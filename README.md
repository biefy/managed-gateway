# appnet-gateway-controller

Out-of-cluster managed ingress for member Kubernetes clusters, built on a Go controller, a forked Istio control plane, and a forked agentgateway data plane.

The current end-to-end target is AKS:

```text
client
  -> public managed gateway LoadBalancer in the infra AKS cluster
  -> member-cluster east-west internal LoadBalancer
  -> ambient workload service/pods
```

## Architecture

```text
┌──────────────────────────── infra AKS cluster ────────────────────────────┐
│                                                                            │
│  tenant-alice namespace                         tenant-bob namespace        │
│    ├─ appnet-gateway-controller              ├─ appnet-gateway-controller  │
│    │   watches member Gateway API objects     │   watches member objects    │
│    ├─ istiod                                  ├─ istiod                    │
│    │   external control plane for member      │   external control plane    │
│    └─ agentgateway per Gateway                └─ agentgateway per Gateway  │
│        public Azure LoadBalancer                  public Azure LB           │
└──────────────┬────────────────────────────────────────────┬────────────────┘
               │                                            │
               ▼                                            ▼
┌──────────────────────── member AKS cluster ────────────────────────────────┐
│                                                                            │
│  Gateway / HTTPRoute / Service / EndpointSlice watched by tenant controller │
│  istio-system/ztunnel runs ambient dataplane                                │
│  istio-system/eastwest-gateway exposes an internal Azure LoadBalancer       │
│                                                                            │
└────────────────────────────────────────────────────────────────────────────┘
```

Security invariants:

- Only the controller talks to the infra apiserver.
- Tenant controllers use member-cluster kubeconfigs stored as infra-cluster Secrets.
- Istiod and agentgateway pods run with `automountServiceAccountToken: false`.
- agentgateway and member east-west gateways use TLS xDS on port `15012`; the rendered Services do not expose plaintext xDS port `15010`.
- agentgateway and east-west gateway containers run non-root with read-only root filesystems, dropped Linux capabilities, and `RuntimeDefault` seccomp.
- Tenant TLS Secrets remain in the infra cluster and are not copied into member clusters.
- Declared frontend/backend TLS assets must exist with required keys before a Gateway can become `Programmed=True`.
- Cross-namespace backend and Gateway certificate refs require Gateway API `ReferenceGrant`.
- CA material is stored in Azure Key Vault and mounted with Secrets Store CSI + AKS Workload Identity.

GatewayClass controllerName: `gateway.azure.com/appnet-gateway.controller`.

## AKS workflow

Prereqs: `az`, `kubectl`, `docker buildx`, `go`, and access to the `akstraffic.azurecr.io` registry.

The AKS scripts default to:

- Subscription: `AKS Traffic Subscription` (`c9e491c5-2dc4-4254-9c7f-cee19a234b2a`)
- Resource group: `rg-mgd-gtw`
- Region: `centralus`
- Clusters: `infra`, `workload-alice`, `workload-bob`
- VNet: `vnet-mgd-gtw-centralus`, with one subnet per cluster
- Node size: `Standard_D4ds_v5`
- Key Vault: `kvmgdgtw<subscription-prefix>cu`
- Public Internet NSG ingress: managed gateway ports `80` and `443` only; AKS load-balancer probe rules remain AzureLoadBalancer-scoped.

Build images locally by default, or set `PUSH=1` and registry-backed `IMAGE`/`IMG` values to publish multi-architecture images:

```sh
./hack/build-controller.sh
./hack/build-istiod.sh
./hack/build-agentgateway.sh
```

The AKS e2e scripts default to local image placeholders; set `CONTROLLER_IMAGE` and `AGENTGATEWAY_IMAGE` to the promoted image references before applying them to shared clusters.

Provision and validate AKS:

```sh
./deploy/aks/bootstrap-infra.sh
./deploy/aks/up.sh
./deploy/aks/ca.sh
./deploy/aks/e2e.sh
```

What each script does:

- `deploy/aks/bootstrap-infra.sh` creates the resource group, VNet/subnets, Key Vault, and Key Vault getter user-assigned identities.
- `deploy/aks/up.sh` creates the three AKS clusters with Azure CNI, OIDC issuer, Workload Identity, Secrets Store CSI, ACR attach, Gateway API CRDs, and federated identity credentials.
- `deploy/aks/ca.sh` generates/uploads the root and intermediate CA hierarchy to Key Vault, then applies `SecretProviderClass` resources.
- `deploy/aks/ambient-up.sh` installs istio-cni, ztunnel, and east-west gateways in the member clusters.
- `deploy/aks/e2e.sh` runs the full AKS flow and checks public managed gateway routing, HTTPS, tenant isolation, east-west internal LoadBalancers, and leader failover.

Local validation and CI use `hack/static-checks.sh`, which runs controller tests, `go vet`, conformance package compilation, shell syntax checks, and release/security greps for known regressions.

If Azure LoadBalancer Services stay pending, check AKS cluster managed identity permissions on the VNet/subnets. Workload Identity is used for pod access to Key Vault; it does not grant the AKS cloud provider permission to manage LoadBalancers.

## Key Vault CA layout

The shared root CA and member intermediate CAs are stored as Key Vault secrets:

- `root-key-pem`
- `root-cert-pem`
- `workload-alice-ca-key-pem`
- `workload-alice-ca-cert-pem`
- `workload-alice-cert-chain-pem`
- `workload-bob-ca-key-pem`
- `workload-bob-ca-cert-pem`
- `workload-bob-cert-chain-pem`

Tenant istiod pods mount `cacerts-keyvault` through Secrets Store CSI. The same CSI mount syncs the tenant `cacerts` Kubernetes Secret so infra-cluster agentgateway pods can read `root-cert.pem`.

Member ztunnel and east-west gateway pods mount `istio-root-cert-keyvault` directly at `/var/run/secrets/istio/root-cert.pem`.

The AKS MVP uses Key Vault access-policy mode. Workload identities receive vault-wide secret `get` permission for the named CSI objects, and the bootstrap principal receives `get`/`set` so `deploy/aks/ca.sh` can materialize CA secrets. Per-secret scoping and audited rotation workflows remain production hardening work.

## Forked upstreams

This repo does not vendor the Istio or agentgateway forks. Fork-specific changes live in their own repositories:

- Istio: `https://github.com/biefy/istio`
- agentgateway: `https://github.com/biefy/agentgateway`

The image build scripts expect local checkouts of those forks and default to sibling paths:

- `../biefy/istio` for `./hack/build-istiod.sh`; override with `ISTIO_DIR`.
- `../biefy/agentgateway` for `./hack/build-agentgateway.sh`; override with `AGENTGATEWAY_DIR`.

## Production readiness

This is an MVP validated by `deploy/aks/e2e.sh`, not a production-ready managed ingress service. See `docs/production-readiness.md` for the current gaps around APIs, security, operations, observability, scale, Gateway API coverage, and fork maintenance.

## Repository layout

```text
controller/                  Go controller and renderers
controller/internal/gateway/  Gateway API reconciliation
controller/internal/membercluster/
                             member-cluster cache/client wiring
controller/internal/provisioner/
                             rendered istiod and agentgateway resources
deploy/aks/                  AKS infrastructure, CA, ambient, and e2e scripts
deploy/manifests/ambient/    ambient mesh and east-west gateway manifests
deploy/manifests/controller/  per-tenant controller manifest template
deploy/manifests/sample/      Alice/Bob sample Gateway API resources
hack/                        image build and static-check scripts
.github/workflows/           CI entrypoint for static checks
```
