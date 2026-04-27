# Change Summary

This fork is based on upstream agentgateway commit `2cede80015e068617c014160d3e3273907bd5f99` (`Trust standalone EPP-selected destinations`).

## Purpose

This fork adds the data-plane behavior needed for the managed out-of-cluster gateway MVP on AKS with Istio ambient member clusters and east-west HBONE gateways.

## Changes

- Added multi-architecture image build support for `linux/amd64` and `linux/arm64`.
- Added `CA_CLUSTER_ID` support so the xDS cluster identity and CA token-auth cluster identity can differ.
- Starts gateway bind accept loops immediately, instead of waiting for xDS readiness, so local static binds such as the east-west HBONE gateway can accept while discovery initializes.
- Added hostname-target handling for HBONE gateway mode:
  - supports double-HBONE inner targets such as `echo.demo.svc.cluster.local:8080`;
  - resolves service hostnames through local discovery where possible;
  - falls back to DNS when needed.
- Added double-HBONE routing support for remote ambient workloads behind an east-west gateway.
- Uses the east-west gateway SPIFFE identity for synthetic Istio network-gateway workloads.
- Preserves IPv4-only binding through the existing `IPV6_ENABLED=false` configuration used by AKS manifests.

## MVP scope

This fork is scoped to the current managed gateway MVP. It validates north-south traffic through an infra gateway into member-cluster ambient workloads via east-west HBONE, but it is not yet a production-ready general-purpose data-plane fork. Production hardening still needs upstream API alignment, more tests, observability, retry/backoff tuning, security review, and broader protocol/Gateway API coverage.
