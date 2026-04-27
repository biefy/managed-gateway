# Production readiness gaps

The current repository proves the managed gateway MVP on AKS. It is not yet a production-ready managed ingress service.

## MVP scope that is validated

- Three AKS clusters in one VNet: infra, member Alice, and member Bob.
- Tenant-scoped controller, Istiod, and managed agentgateway instances in the infra cluster.
- Member clusters running ambient mode with ztunnel and internal east-west HBONE gateways.
- Public Azure LoadBalancer for north-south managed gateway traffic.
- Internal Azure LoadBalancer for member east-west traffic.
- Shared root CA plus per-member intermediate CAs stored in Azure Key Vault.
- Runtime CA materialization through Secrets Store CSI and AKS Workload Identity.
- HTTP and HTTPS routing from the public managed gateway to an ambient member workload through the east-west gateway.
- Tenant TLS Secret isolation: infra TLS material is not copied into member clusters.
- Basic controller leader failover.

## Gaps before production use

### API and product model

- Tenant/member onboarding is still script-driven and sample-specific.
- GatewayClass parameters, tenant policy, member-cluster registration, and lifecycle APIs need a stable contract.
- Multi-region, multi-VNet, cross-subscription, and private-link topologies are not modeled.
- There is no customer-facing status or diagnostic API beyond Kubernetes status/logs.

### Security

- The controller currently uses broad member-cluster permissions in the e2e flow; production needs least-privilege RBAC.
- Kubeconfig bootstrap and rotation need a supported, audited lifecycle.
- The Istio and agentgateway changes need formal security review.
- CA issuance, intermediate rotation, revocation, and emergency rollover workflows are not productionized.
- Key Vault access policies are broad `get/list` grants for the MVP; production should scope identities and audit access.
- Network security rules are permissive enough for validation and should be narrowed to production ingress and health-probe requirements.

### Reliability and operations

- The scripts are not idempotent infrastructure-as-code with drift detection, rollback, or change review.
- Controller reconciliation needs stronger retry, backoff, conflict handling, and upgrade/downgrade behavior.
- Managed gateway and tenant Istiod sizing, autoscaling, PodDisruptionBudgets, and zonal placement need design.
- Image tags use mutable `:dev` tags; production needs immutable digests and release promotion.
- Disaster recovery for Key Vault, ACR, cluster recreation, and tenant namespace recovery is not covered.

### Observability

- Logs are sufficient for debugging the MVP, but production needs structured metrics, traces, dashboards, alerts, and SLOs.
- There is no standardized request correlation across public gateway, east-west gateway, ztunnel, and workload.
- Failure modes such as CA auth failure, xDS staleness, CSI mount failure, and LoadBalancer provisioning failure need first-class alerts.

### Scale and performance

- The MVP validates one or two tenants and sample routes; it has not been load-tested.
- xDS push scale, EndpointSlice volume, Gateway/HTTPRoute cardinality, and multi-tenant noisy-neighbor behavior need evaluation.
- East-west gateway throughput, connection pooling, mTLS handshake costs, and cross-cluster latency need benchmarking.

### Gateway API and traffic features

- The forked Istio translator implements only the subset required by the sample HTTP/HTTPS routes.
- Production needs a feature matrix for Gateway, HTTPRoute, TLSRoute/TCPRoute, filters, backend weights, timeouts, retries, header manipulation, and policy attachment.
- Status reporting must match Gateway API expectations for unsupported fields and partial failures.

### Upstream fork maintenance

- Istio and agentgateway are private forks for the MVP. They need an explicit rebase/upgrade process.
- The changes should be reduced to reviewable patches with tests before any long-term production dependency.
- Compatibility with future Istio ambient and agentgateway releases is unknown.

## Recommended next steps

1. Replace shell provisioning with reviewed infrastructure-as-code.
2. Define tenant/member registration APIs and least-privilege access boundaries.
3. Add integration and conformance tests for the supported Gateway API subset.
4. Add release automation that pins immutable image digests.
5. Build dashboards and alerts for xDS, CA, CSI, LoadBalancer, and traffic health.
6. Run scale and failure-injection tests before expanding beyond the MVP topology.
