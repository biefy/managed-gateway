# Change Summary

## Managed-gateway delta against upstream Istio

This file records the managed-gateway changes carried on top of `github.com/istio/istio` `master`.

- Adds the managed-gateway controller under `controller/`, including Gateway API reconciliation, member-cluster watches, provisioner rendering/apply logic, status writing, conformance compilation coverage, and `xds-echo`.
- Adds AKS deployment and validation assets under `deploy/aks/` and `deploy/manifests/` for infra/member clusters, ambient mesh, east-west gateways, per-tenant controllers, and sample workloads.
- Adds managed-gateway build, conformance, and static-check scripts under `hack/`, plus the repository CI workflow in `.github/workflows/ci.yml`.
- Adds managed-gateway design and production-readiness documentation under `docs/` and replaces the upstream Istio README with the managed-gateway README.
- Keeps the managed-gateway root Go module path (`gateway.appnet.azure.com/controller`) and dependency set in `go.mod`/`go.sum` instead of the upstream Istio root module.
- Combines local ignore rules with relevant upstream Istio generated/local-file ignore rules in `.gitignore`.

Validation for the current merge:

- `go test ./controller/...`
- `git diff --check`
