# Change Summary

This fork is based on upstream Istio commit `44d0e58e49d0dc89e27fc4f8679c68132d46b887` (`[1.24] ambient: fix ingress-use-waypoint with ServiceEntry`).

## Purpose

This fork adds the minimum Istio control-plane support needed for the managed out-of-cluster gateway MVP. Tenant Istiod instances run outside member clusters and program `agentgateway` data planes through xDS while still reusing Istio ambient workload discovery.

## Changes

- Added `agentgateway` as a recognized Istio `NodeType`.
- Registered xDS generators for `agentgateway` nodes:
  - reuse Istio workload `Address` resources for service/workload discovery;
  - emit `agentgateway.dev.resource.Resource` for Bind, Listener, Route, and related data-plane configuration.
- Added the agentgateway resource protobuf bindings under `pilot/pkg/xds/agentgateway/api`.
- Added Gateway API to agentgateway xDS translation for Gateway listeners, HTTPRoutes, backend refs, and TLS certificate references.
- Added support for infra-mounted TLS certificate references using the `appnet.azure.com/tls-cert` Gateway annotation.
- Improved Kubernetes network gateway discovery for HBONE east-west gateways:
  - recognizes `hbone` / `hbone-mtls` Service ports;
  - preserves `HBONEPort` through NodePort translation;
  - falls back to `NodeInternalIP` when ExternalIP is absent.

## MVP scope

This fork is intentionally narrow. It supports the current managed gateway MVP flow and is not a general-purpose Istio extension yet. Production hardening still needs a full API review, conformance coverage, broader Gateway API feature support, security review, and upstreamable design cleanup.
