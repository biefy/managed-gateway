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
- Gateway API route attachment checks for listener matching, `allowedRoutes`, hostname intersection, backend Service existence, and cross-namespace `ReferenceGrant`.
- Gateway TLS validation that blocks programming when declared certificate material is missing or unauthorized.
- Local release guardrails and CI static checks for tests, vet, conformance compile, shell syntax, mutable image defaults, tracked binaries, public NodePort exposure, plaintext xDS exposure, and credential literals.

## Gaps before production use

### API and product model

- Tenant/member onboarding is still script-driven and sample-specific.
- GatewayClass parameters, tenant policy, member-cluster registration, and lifecycle APIs need a stable contract.
- Multi-region, multi-VNet, cross-subscription, and private-link topologies are not modeled.
- There is no customer-facing status or diagnostic API beyond Kubernetes status/logs.

### Security

- The e2e flow now uses a dedicated member-cluster ClusterRole instead of `cluster-admin`, but production needs tenant-specific onboarding, review, and automated drift detection for that RBAC.
- Kubeconfig bootstrap and rotation need a supported, audited lifecycle.
- The Istio and agentgateway changes need formal security review.
- CA issuance, intermediate rotation, revocation, and emergency rollover workflows are not productionized.
- The AKS MVP still uses Key Vault access-policy mode, so identity grants are vault-wide secret `get` and bootstrap `get`/`set`; production should move to tighter scoping and audited access.
- Public Internet NSG ingress is limited to managed gateway ports `80` and `443`; production should still model customer-specific ingress, health probes, and private ingress requirements.

### Reliability and operations

- The scripts are not idempotent infrastructure-as-code with drift detection, rollback, or change review.
- Controller reconciliation needs stronger retry, backoff, conflict handling, and upgrade/downgrade behavior.
- Managed gateway and tenant Istiod sizing, autoscaling, PodDisruptionBudgets, and zonal placement need design.
- Build scripts and deploy templates no longer push or reference remote mutable `:dev` images by default, but production still needs immutable digest promotion and provenance.
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

- The forked Istio translator implements the subset required by the sample HTTP/HTTPS/TLS and backend mTLS routes, but it is not a complete Gateway API implementation.
- Production needs a feature matrix for Gateway, HTTPRoute, GRPCRoute, TLSRoute/TCPRoute, filters, backend weights, timeouts, retries, header manipulation, policy attachment, and ListenerSet semantics.
- Status reporting now covers key listener, Route, backend, and certificate-reference failures, but must be expanded for unsupported fields, partial failures, BackendTLSPolicy status, and the full conformance matrix.

### Upstream fork maintenance

- Istio and agentgateway are private forks for the MVP. They need an explicit rebase/upgrade process.
- The changes should be reduced to reviewable patches with tests before any long-term production dependency.
- Compatibility with future Istio ambient and agentgateway releases is unknown.

## Recommended next steps

1. Replace shell provisioning with reviewed infrastructure-as-code.
2. Define tenant/member registration APIs and least-privilege access boundaries.
3. Expand integration and conformance tests for unsupported-field, partial-invalid, policy, and ListenerSet behavior.
4. Add release automation that pins immutable image digests and records provenance.
5. Build dashboards and alerts for xDS, CA, CSI, LoadBalancer, token refresh, and traffic health.
6. Run scale and failure-injection tests before expanding beyond the MVP topology.
