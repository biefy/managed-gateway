// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package xds

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	anypb "google.golang.org/protobuf/types/known/anypb"
	k8s "sigs.k8s.io/gateway-api/apis/v1"

	"istio.io/istio/pilot/pkg/model"
	agw "istio.io/istio/pilot/pkg/xds/agentgateway/api"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/schema/gvk"
)

// tlsCertAnnotation names a Secret in the istiod-tenant infra namespace whose
// `tls.crt`/`tls.key` are mounted under tlsMountRoot. The workload Gateway
// only carries the *name*; the bytes never travel through the workload cluster.
const (
	tlsCertAnnotation = "appnet.azure.com/tls-cert"
	tlsMountRoot      = "/var/run/tls"
)

// translateAgentgatewayResources is the entry point for translating Gateway-API
// state into agentgateway.dev.resource.Resource{Bind/Listener/Route}. It uses
// proxy.Metadata.Raw["role"]="<ns>~<gateway-name>" to scope the response to a
// single Gateway.
func (g *AgentgatewayResourceGenerator) translateAgentgatewayResources(proxy *model.Proxy, req *model.PushRequest) model.Resources {
	_ = req
	gwNS, gwName, ok := agentgatewayRole(proxy)
	if !ok {
		return nil
	}
	if g.Server == nil || g.Server.Env == nil {
		return nil
	}
	store := g.Server.Env.ConfigStore
	if store == nil {
		return nil
	}

	gwCfg := store.Get(gvk.KubernetesGateway, gwName, gwNS)
	if gwCfg == nil {
		return nil
	}
	gwSpec, ok := gwCfg.Spec.(*k8s.GatewaySpec)
	if !ok || gwSpec == nil {
		return nil
	}

	tlsCertName := ""
	if gwCfg.Annotations != nil {
		tlsCertName = strings.TrimSpace(gwCfg.Annotations[tlsCertAnnotation])
	}

	var out model.Resources
	// Track unique "ns/name" services referenced by HTTPRoute backends so we
	// can emit istio.workload.Service + istio.workload.Workload resources.
	refs := map[string]struct{}{}

	// 1) Per-listener Bind + Listener.
	for _, l := range gwSpec.Listeners {
		port := uint32(l.Port)
		bindKey := fmt.Sprintf("%s/%s/%d", gwNS, gwName, port)
		listenerKey := fmt.Sprintf("%s/%s/%s", gwNS, gwName, l.Name)

		bind := &agw.Bind{
			Key:      bindKey,
			Port:     port,
			Protocol: bindProtocolFor(l.Protocol),
		}
		out = append(out, mustWrap(bindKey, &agw.Resource{Kind: &agw.Resource_Bind{Bind: bind}}))

		hostname := ""
		if l.Hostname != nil {
			hostname = string(*l.Hostname)
		}
		ln := &agw.Listener{
			Key:      listenerKey,
			BindKey:  bindKey,
			Hostname: hostname,
			Protocol: listenerProtocolFor(l.Protocol),
			Name: &agw.ListenerName{
				GatewayName:      gwName,
				GatewayNamespace: gwNS,
				ListenerName:     string(l.Name),
			},
		}
		if l.Protocol == k8s.HTTPSProtocolType || l.Protocol == k8s.TLSProtocolType {
			if tls := loadTLSConfig(tlsCertName); tls != nil {
				ln.Tls = tls
			}
		}
		out = append(out, mustWrap(listenerKey, &agw.Resource{Kind: &agw.Resource_Listener{Listener: ln}}))
	}

	// 2) HTTPRoutes that attach to this Gateway.
	routes := store.List(gvk.HTTPRoute, "")
	for _, rc := range routes {
		rspec, ok := rc.Spec.(*k8s.HTTPRouteSpec)
		if !ok || rspec == nil {
			continue
		}
		matchedListeners := matchedListenerKeys(rc, rspec, gwNS, gwName, gwSpec)
		if len(matchedListeners) == 0 {
			continue
		}
		hostnames := make([]string, 0, len(rspec.Hostnames))
		for _, h := range rspec.Hostnames {
			hostnames = append(hostnames, string(h))
		}
		for ruleIdx, rule := range rspec.Rules {
			matches := translateMatches(rule.Matches)
			backends := translateBackends(rc.Namespace, rule.BackendRefs, refs)
			if len(backends) == 0 {
				continue
			}
			for _, lkey := range matchedListeners {
				routeKey := fmt.Sprintf("%s/%s/%s/%d/%s", rc.Namespace, rc.Name, lkey, ruleIdx, ruleNameOrEmpty(rule.Name))
				route := &agw.Route{
					Key:         routeKey,
					ListenerKey: lkey,
					Name: &agw.RouteName{
						Kind:      "HTTPRoute",
						Name:      rc.Name,
						Namespace: rc.Namespace,
					},
					Hostnames: hostnames,
					Matches:   matches,
					Backends:  backends,
				}
				if rule.Name != nil {
					rn := string(*rule.Name)
					route.Name.RuleName = &rn
				}
				out = append(out, mustWrap(routeKey, &agw.Resource{Kind: &agw.Resource_Route{Route: route}}))
			}
		}
	}
	_ = refs

	return out
}

// agentgatewayRole extracts (namespace, gatewayName) from
// proxy.Metadata.Raw["role"], which agentgateway sets to "<ns>~<gw>".
func agentgatewayRole(proxy *model.Proxy) (string, string, bool) {
	if proxy == nil || proxy.Metadata == nil {
		return "", "", false
	}
	raw, _ := proxy.Metadata.Raw["role"].(string)
	if raw == "" {
		return "", "", false
	}
	parts := strings.SplitN(raw, "~", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func bindProtocolFor(p k8s.ProtocolType) agw.Bind_Protocol {
	switch p {
	case k8s.HTTPSProtocolType, k8s.TLSProtocolType:
		return agw.Bind_TLS
	case k8s.TCPProtocolType:
		return agw.Bind_TCP
	default:
		return agw.Bind_HTTP
	}
}

func listenerProtocolFor(p k8s.ProtocolType) agw.Protocol {
	// agw.Protocol enum: UNKNOWN=0, HTTP=1, HTTPS=2, TLS=3, TCP=4 (per resource.proto).
	// We resolve symbolically via the enum value map to avoid hardcoding.
	switch p {
	case k8s.HTTPSProtocolType:
		return agw.Protocol(agw.Protocol_value["HTTPS"])
	case k8s.TLSProtocolType:
		return agw.Protocol(agw.Protocol_value["TLS"])
	case k8s.TCPProtocolType:
		return agw.Protocol(agw.Protocol_value["TCP"])
	case k8s.HTTPProtocolType:
		return agw.Protocol(agw.Protocol_value["HTTP"])
	default:
		return agw.Protocol_UNKNOWN
	}
}

func matchedListenerKeys(rc config.Config, rspec *k8s.HTTPRouteSpec, gwNS, gwName string, gwSpec *k8s.GatewaySpec) []string {
	var keys []string
	for _, p := range rspec.ParentRefs {
		group := "gateway.networking.k8s.io"
		if p.Group != nil {
			group = string(*p.Group)
		}
		kind := "Gateway"
		if p.Kind != nil {
			kind = string(*p.Kind)
		}
		if group != "gateway.networking.k8s.io" || kind != "Gateway" {
			continue
		}
		ns := rc.Namespace
		if p.Namespace != nil {
			ns = string(*p.Namespace)
		}
		if ns != gwNS || string(p.Name) != gwName {
			continue
		}
		for _, l := range gwSpec.Listeners {
			if p.SectionName != nil && *p.SectionName != l.Name {
				continue
			}
			if p.Port != nil && int32(*p.Port) != int32(l.Port) {
				continue
			}
			keys = append(keys, fmt.Sprintf("%s/%s/%s", gwNS, gwName, l.Name))
		}
	}
	return keys
}

func translateMatches(in []k8s.HTTPRouteMatch) []*agw.RouteMatch {
	if len(in) == 0 {
		// Match everything: a single empty match with PathPrefix "/".
		return []*agw.RouteMatch{{
			Path: &agw.PathMatch{Kind: &agw.PathMatch_PathPrefix{PathPrefix: "/"}},
		}}
	}
	out := make([]*agw.RouteMatch, 0, len(in))
	for _, m := range in {
		rm := &agw.RouteMatch{}
		if m.Path != nil {
			val := "/"
			if m.Path.Value != nil {
				val = *m.Path.Value
			}
			ptype := k8s.PathMatchPathPrefix
			if m.Path.Type != nil {
				ptype = *m.Path.Type
			}
			switch ptype {
			case k8s.PathMatchExact:
				rm.Path = &agw.PathMatch{Kind: &agw.PathMatch_Exact{Exact: val}}
			case k8s.PathMatchRegularExpression:
				rm.Path = &agw.PathMatch{Kind: &agw.PathMatch_Regex{Regex: val}}
			default:
				rm.Path = &agw.PathMatch{Kind: &agw.PathMatch_PathPrefix{PathPrefix: val}}
			}
		} else {
			rm.Path = &agw.PathMatch{Kind: &agw.PathMatch_PathPrefix{PathPrefix: "/"}}
		}
		out = append(out, rm)
	}
	return out
}

func translateBackends(routeNS string, refs []k8s.HTTPBackendRef, refSet map[string]struct{}) []*agw.RouteBackend {
	out := make([]*agw.RouteBackend, 0, len(refs))
	for _, b := range refs {
		group := ""
		if b.Group != nil {
			group = string(*b.Group)
		}
		kind := "Service"
		if b.Kind != nil {
			kind = string(*b.Kind)
		}
		if group != "" || kind != "Service" {
			continue
		}
		ns := routeNS
		if b.Namespace != nil {
			ns = string(*b.Namespace)
		}
		port := uint32(0)
		if b.Port != nil {
			port = uint32(*b.Port)
		}
		hostname := fmt.Sprintf("%s.%s.svc.cluster.local", b.Name, ns)
		if refSet != nil {
			refSet[ns+"/"+string(b.Name)] = struct{}{}
		}
		weight := int32(1)
		if b.Weight != nil {
			weight = *b.Weight
		}
		out = append(out, &agw.RouteBackend{
			Backend: &agw.BackendReference{
				Kind: &agw.BackendReference_Service_{
					Service: &agw.BackendReference_Service{Namespace: ns, Hostname: hostname},
				},
				Port: port,
			},
			Weight: weight,
		})
	}
	return out
}

func ruleNameOrEmpty(n *k8s.SectionName) string {
	if n == nil {
		return ""
	}
	return string(*n)
}

// mustWrap marshals a Resource into Any+Resource model entry. Errors here mean
// a programming bug in the proto wiring; panic so it surfaces in tests.
func mustWrap(name string, r *agw.Resource) *discovery.Resource {
	any, err := anypb.New(r)
	if err != nil {
		panic(fmt.Sprintf("agentgateway: failed to marshal Resource %q: %v", name, err))
	}
	return &discovery.Resource{Name: name, Resource: any}
}

// loadTLSConfig reads tls.crt + tls.key from the per-cert mount root. Returns
// nil if the annotation is empty or either file is unreadable, so a missing
// Secret degrades to a plaintext listener (handshake will fail at the proxy)
// rather than killing the entire xDS push.
func loadTLSConfig(name string) *agw.TLSConfig {
	if name == "" {
		return nil
	}
	dir := filepath.Join(tlsMountRoot, name)
	cert, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if err != nil {
		return nil
	}
	key, err := os.ReadFile(filepath.Join(dir, "tls.key"))
	if err != nil {
		return nil
	}
	return &agw.TLSConfig{Cert: cert, PrivateKey: key}
}
