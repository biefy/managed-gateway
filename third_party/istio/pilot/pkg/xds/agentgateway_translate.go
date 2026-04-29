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
	"regexp"
	"sort"
	"strings"
	"time"

	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	anypb "google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	k8s "sigs.k8s.io/gateway-api/apis/v1"

	"istio.io/istio/pilot/pkg/model"
	agw "istio.io/istio/pilot/pkg/xds/agentgateway/api"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/schema/gvk"
)

const tlsMountRoot = "/var/run/tls"

type routeListener struct {
	key string
}

type backendTLSPolicyMap map[string]*agw.BackendPolicySpec

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

	var out model.Resources
	refs := map[string]struct{}{}
	backendTLS := backendTLSPolicies(store, gwSpec)

	for _, l := range gwSpec.Listeners {
		port := uint32(l.Port)
		bindKey := fmt.Sprintf("%s/%s/%d", gwNS, gwName, port)
		listenerKey := listenerKey(gwNS, gwName, l.Name)

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
			ln.Tls = listenerTLSConfig(l, gwSpec)
		}
		out = append(out, mustWrap(listenerKey, &agw.Resource{Kind: &agw.Resource_Listener{Listener: ln}}))
	}

	for _, rc := range store.List(gvk.HTTPRoute, "") {
		rspec, ok := rc.Spec.(*k8s.HTTPRouteSpec)
		if !ok || rspec == nil {
			continue
		}
		matched := matchedListeners(rc, rspec.ParentRefs, "HTTPRoute", rspec.Hostnames, gwNS, gwName, gwSpec)
		if len(matched) == 0 {
			continue
		}
		hostnames := routeHostnames(rspec.Hostnames)
		for ruleIdx, rule := range rspec.Rules {
			matches := translateHTTPMatches(rule.Matches)
			backends := translateHTTPBackends(rc.Namespace, rule.BackendRefs, refs, backendTLS)
			policies := translateHTTPPolicies(rc.Namespace, rule.Filters, rule.Timeouts, refs)
			if len(backends) == 0 && len(policies) == 0 {
				continue
			}
			for _, l := range matched {
				routeKey := fmt.Sprintf("%s/%s/%s/%d/%s", rc.Namespace, rc.Name, l.key, ruleIdx, ruleNameOrEmpty(rule.Name))
				route := &agw.Route{
					Key:             routeKey,
					ListenerKey:     l.key,
					Name:            routeName("HTTPRoute", rc, rule.Name),
					Hostnames:       hostnames,
					Matches:         matches,
					Backends:        backends,
					TrafficPolicies: policies,
				}
				out = append(out, mustWrap(routeKey, &agw.Resource{Kind: &agw.Resource_Route{Route: route}}))
			}
		}
	}

	for _, rc := range store.List(gvk.GRPCRoute, "") {
		rspec, ok := rc.Spec.(*k8s.GRPCRouteSpec)
		if !ok || rspec == nil {
			continue
		}
		matched := matchedListeners(rc, rspec.ParentRefs, "GRPCRoute", rspec.Hostnames, gwNS, gwName, gwSpec)
		if len(matched) == 0 {
			continue
		}
		hostnames := routeHostnames(rspec.Hostnames)
		for ruleIdx, rule := range rspec.Rules {
			matches := translateGRPCMatches(rule.Matches)
			backends := translateGRPCBackends(rc.Namespace, rule.BackendRefs, refs, backendTLS)
			policies := translateGRPCPolicies(rc.Namespace, rule.Filters, refs)
			if len(backends) == 0 && len(policies) == 0 {
				continue
			}
			for _, l := range matched {
				routeKey := fmt.Sprintf("%s/%s/%s/%d/%s", rc.Namespace, rc.Name, l.key, ruleIdx, ruleNameOrEmpty(rule.Name))
				route := &agw.Route{
					Key:             routeKey,
					ListenerKey:     l.key,
					Name:            routeName("GRPCRoute", rc, rule.Name),
					Hostnames:       hostnames,
					Matches:         matches,
					Backends:        backends,
					TrafficPolicies: policies,
				}
				out = append(out, mustWrap(routeKey, &agw.Resource{Kind: &agw.Resource_Route{Route: route}}))
			}
		}
	}

	for _, rc := range store.List(gvk.TLSRoute, "") {
		rspec, ok := rc.Spec.(*k8s.TLSRouteSpec)
		if !ok || rspec == nil {
			continue
		}
		matched := matchedListeners(rc, rspec.ParentRefs, "TLSRoute", rspec.Hostnames, gwNS, gwName, gwSpec)
		if len(matched) == 0 {
			continue
		}
		hostnames := routeHostnames(rspec.Hostnames)
		for ruleIdx, rule := range rspec.Rules {
			backends := translateBackendRefs(rc.Namespace, rule.BackendRefs, refs, backendTLS)
			if len(backends) == 0 {
				continue
			}
			for _, l := range matched {
				routeKey := fmt.Sprintf("%s/%s/%s/%d/%s", rc.Namespace, rc.Name, l.key, ruleIdx, ruleNameOrEmpty(rule.Name))
				route := &agw.TCPRoute{
					Key:         routeKey,
					ListenerKey: l.key,
					Name:        routeName("TLSRoute", rc, rule.Name),
					Hostnames:   hostnames,
					Backends:    backends,
				}
				out = append(out, mustWrap(routeKey, &agw.Resource{Kind: &agw.Resource_TcpRoute{TcpRoute: route}}))
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

func matchedListeners(rc config.Config, parentRefs []k8s.ParentReference, routeKind string, routeHostnames []k8s.Hostname, gwNS, gwName string, gwSpec *k8s.GatewaySpec) []routeListener {
	var out []routeListener
	seen := map[string]struct{}{}
	for _, p := range parentRefs {
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
			if !listenerSupportsRoute(l.Protocol, routeKind) || !listenerHostnameIntersects(l.Hostname, routeHostnames) {
				continue
			}
			key := listenerKey(gwNS, gwName, l.Name)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, routeListener{key: key})
		}
	}
	return out
}

func listenerSupportsRoute(protocol k8s.ProtocolType, routeKind string) bool {
	switch routeKind {
	case "HTTPRoute", "GRPCRoute":
		return protocol == k8s.HTTPProtocolType || protocol == k8s.HTTPSProtocolType
	case "TLSRoute":
		return protocol == k8s.TLSProtocolType
	default:
		return false
	}
}

func listenerHostnameIntersects(listener *k8s.Hostname, routeHostnames []k8s.Hostname) bool {
	if listener == nil || len(routeHostnames) == 0 {
		return true
	}
	for _, h := range routeHostnames {
		if hostnameIntersects(string(*listener), string(h)) {
			return true
		}
	}
	return false
}

func hostnameIntersects(a, b string) bool {
	if a == b || a == "*" || b == "*" {
		return true
	}
	if hostnameMatches(a, b) || hostnameMatches(b, a) {
		return true
	}
	if strings.HasPrefix(a, "*.") && strings.HasPrefix(b, "*.") {
		a = strings.TrimPrefix(a, "*.")
		b = strings.TrimPrefix(b, "*.")
		return strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a)
	}
	return false
}

func hostnameMatches(pattern, hostname string) bool {
	if !strings.HasPrefix(pattern, "*.") {
		return pattern == hostname
	}
	suffix := strings.TrimPrefix(pattern, "*.")
	return hostname != suffix && strings.HasSuffix(hostname, "."+suffix)
}

func translateHTTPMatches(in []k8s.HTTPRouteMatch) []*agw.RouteMatch {
	if len(in) == 0 {
		return []*agw.RouteMatch{{Path: pathPrefixMatch("/")}}
	}
	out := make([]*agw.RouteMatch, 0, len(in))
	for _, m := range in {
		rm := &agw.RouteMatch{
			Path:        translatePathMatch(m.Path),
			Headers:     translateHTTPHeaderMatches(m.Headers),
			QueryParams: translateHTTPQueryMatches(m.QueryParams),
		}
		if m.Method != nil {
			rm.Method = &agw.MethodMatch{Exact: string(*m.Method)}
		}
		out = append(out, rm)
	}
	return out
}

func translatePathMatch(m *k8s.HTTPPathMatch) *agw.PathMatch {
	if m == nil {
		return pathPrefixMatch("/")
	}
	val := "/"
	if m.Value != nil {
		val = *m.Value
	}
	ptype := k8s.PathMatchPathPrefix
	if m.Type != nil {
		ptype = *m.Type
	}
	switch ptype {
	case k8s.PathMatchExact:
		return &agw.PathMatch{Kind: &agw.PathMatch_Exact{Exact: val}}
	case k8s.PathMatchRegularExpression:
		return &agw.PathMatch{Kind: &agw.PathMatch_Regex{Regex: val}}
	default:
		return pathPrefixMatch(val)
	}
}

func translateHTTPHeaderMatches(in []k8s.HTTPHeaderMatch) []*agw.HeaderMatch {
	out := make([]*agw.HeaderMatch, 0, len(in))
	seen := map[string]struct{}{}
	for _, h := range in {
		name := string(h.Name)
		lower := strings.ToLower(name)
		if _, ok := seen[lower]; ok {
			continue
		}
		seen[lower] = struct{}{}
		match := &agw.HeaderMatch{Name: name}
		matchType := k8s.HeaderMatchExact
		if h.Type != nil {
			matchType = *h.Type
		}
		if matchType == k8s.HeaderMatchRegularExpression {
			match.Value = &agw.HeaderMatch_Regex{Regex: h.Value}
		} else {
			match.Value = &agw.HeaderMatch_Exact{Exact: h.Value}
		}
		out = append(out, match)
	}
	return out
}

func translateHTTPQueryMatches(in []k8s.HTTPQueryParamMatch) []*agw.QueryMatch {
	out := make([]*agw.QueryMatch, 0, len(in))
	seen := map[string]struct{}{}
	for _, q := range in {
		name := string(q.Name)
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		match := &agw.QueryMatch{Name: name}
		matchType := k8s.QueryParamMatchExact
		if q.Type != nil {
			matchType = *q.Type
		}
		if matchType == k8s.QueryParamMatchRegularExpression {
			match.Value = &agw.QueryMatch_Regex{Regex: q.Value}
		} else {
			match.Value = &agw.QueryMatch_Exact{Exact: q.Value}
		}
		out = append(out, match)
	}
	return out
}

func translateGRPCMatches(in []k8s.GRPCRouteMatch) []*agw.RouteMatch {
	if len(in) == 0 {
		return []*agw.RouteMatch{{Path: pathPrefixMatch("/")}}
	}
	out := make([]*agw.RouteMatch, 0, len(in))
	for _, m := range in {
		out = append(out, &agw.RouteMatch{
			Path:    translateGRPCMethodMatch(m.Method),
			Headers: translateGRPCHeaderMatches(m.Headers),
		})
	}
	return out
}

func translateGRPCMethodMatch(m *k8s.GRPCMethodMatch) *agw.PathMatch {
	if m == nil {
		return pathPrefixMatch("/")
	}
	service, method := "", ""
	if m.Service != nil {
		service = *m.Service
	}
	if m.Method != nil {
		method = *m.Method
	}
	matchType := k8s.GRPCMethodMatchExact
	if m.Type != nil {
		matchType = *m.Type
	}
	if matchType == k8s.GRPCMethodMatchRegularExpression {
		servicePattern := "[^/]+"
		if service != "" {
			servicePattern = service
		}
		methodPattern := "[^/]+"
		if method != "" {
			methodPattern = method
		}
		return &agw.PathMatch{Kind: &agw.PathMatch_Regex{Regex: "^/" + servicePattern + "/" + methodPattern + "$"}}
	}
	if service != "" && method != "" {
		return &agw.PathMatch{Kind: &agw.PathMatch_Exact{Exact: "/" + service + "/" + method}}
	}
	if service != "" {
		return pathPrefixMatch("/" + service + "/")
	}
	if method != "" {
		return &agw.PathMatch{Kind: &agw.PathMatch_Regex{Regex: "^/[^/]+/" + regexp.QuoteMeta(method) + "$"}}
	}
	return pathPrefixMatch("/")
}

func translateGRPCHeaderMatches(in []k8s.GRPCHeaderMatch) []*agw.HeaderMatch {
	out := make([]*agw.HeaderMatch, 0, len(in))
	seen := map[string]struct{}{}
	for _, h := range in {
		name := string(h.Name)
		lower := strings.ToLower(name)
		if _, ok := seen[lower]; ok {
			continue
		}
		seen[lower] = struct{}{}
		match := &agw.HeaderMatch{Name: name}
		matchType := k8s.GRPCHeaderMatchExact
		if h.Type != nil {
			matchType = *h.Type
		}
		if matchType == k8s.GRPCHeaderMatchRegularExpression {
			match.Value = &agw.HeaderMatch_Regex{Regex: h.Value}
		} else {
			match.Value = &agw.HeaderMatch_Exact{Exact: h.Value}
		}
		out = append(out, match)
	}
	return out
}

func translateHTTPPolicies(routeNS string, filters []k8s.HTTPRouteFilter, timeouts *k8s.HTTPRouteTimeouts, refSet map[string]struct{}) []*agw.TrafficPolicySpec {
	var out []*agw.TrafficPolicySpec
	if timeout := translateTimeout(timeouts); timeout != nil {
		out = append(out, &agw.TrafficPolicySpec{Kind: &agw.TrafficPolicySpec_Timeout{Timeout: timeout}})
	}
	for _, f := range filters {
		switch f.Type {
		case k8s.HTTPRouteFilterRequestHeaderModifier:
			if f.RequestHeaderModifier != nil {
				out = append(out, &agw.TrafficPolicySpec{Kind: &agw.TrafficPolicySpec_RequestHeaderModifier{RequestHeaderModifier: translateHeaderFilter(f.RequestHeaderModifier)}})
			}
		case k8s.HTTPRouteFilterResponseHeaderModifier:
			if f.ResponseHeaderModifier != nil {
				out = append(out, &agw.TrafficPolicySpec{Kind: &agw.TrafficPolicySpec_ResponseHeaderModifier{ResponseHeaderModifier: translateHeaderFilter(f.ResponseHeaderModifier)}})
			}
		case k8s.HTTPRouteFilterRequestRedirect:
			if f.RequestRedirect != nil {
				out = append(out, &agw.TrafficPolicySpec{Kind: &agw.TrafficPolicySpec_RequestRedirect{RequestRedirect: translateRequestRedirect(f.RequestRedirect)}})
			}
		case k8s.HTTPRouteFilterURLRewrite:
			if f.URLRewrite != nil {
				out = append(out, &agw.TrafficPolicySpec{Kind: &agw.TrafficPolicySpec_UrlRewrite{UrlRewrite: translateURLRewrite(f.URLRewrite)}})
			}
		case k8s.HTTPRouteFilterRequestMirror:
			if mirror := translateRequestMirror(routeNS, f.RequestMirror, refSet); mirror != nil {
				out = append(out, &agw.TrafficPolicySpec{Kind: &agw.TrafficPolicySpec_RequestMirror{RequestMirror: mirror}})
			}
		case k8s.HTTPRouteFilterCORS:
			if f.CORS != nil {
				out = append(out, &agw.TrafficPolicySpec{Kind: &agw.TrafficPolicySpec_Cors{Cors: translateCORS(f.CORS)}})
			}
		}
	}
	return out
}

func translateGRPCPolicies(routeNS string, filters []k8s.GRPCRouteFilter, refSet map[string]struct{}) []*agw.TrafficPolicySpec {
	var out []*agw.TrafficPolicySpec
	for _, f := range filters {
		switch f.Type {
		case k8s.GRPCRouteFilterRequestHeaderModifier:
			if f.RequestHeaderModifier != nil {
				out = append(out, &agw.TrafficPolicySpec{Kind: &agw.TrafficPolicySpec_RequestHeaderModifier{RequestHeaderModifier: translateHeaderFilter(f.RequestHeaderModifier)}})
			}
		case k8s.GRPCRouteFilterResponseHeaderModifier:
			if f.ResponseHeaderModifier != nil {
				out = append(out, &agw.TrafficPolicySpec{Kind: &agw.TrafficPolicySpec_ResponseHeaderModifier{ResponseHeaderModifier: translateHeaderFilter(f.ResponseHeaderModifier)}})
			}
		case k8s.GRPCRouteFilterRequestMirror:
			if mirror := translateRequestMirror(routeNS, f.RequestMirror, refSet); mirror != nil {
				out = append(out, &agw.TrafficPolicySpec{Kind: &agw.TrafficPolicySpec_RequestMirror{RequestMirror: mirror}})
			}
		}
	}
	return out
}

func translateTimeout(in *k8s.HTTPRouteTimeouts) *agw.Timeout {
	if in == nil {
		return nil
	}
	out := &agw.Timeout{
		Request:        parseGatewayDuration(in.Request),
		BackendRequest: parseGatewayDuration(in.BackendRequest),
	}
	if out.Request == nil && out.BackendRequest == nil {
		return nil
	}
	return out
}

func parseGatewayDuration(in *k8s.Duration) *durationpb.Duration {
	if in == nil {
		return nil
	}
	d, err := time.ParseDuration(string(*in))
	if err != nil {
		return nil
	}
	return durationpb.New(d)
}

func translateHeaderFilter(in *k8s.HTTPHeaderFilter) *agw.HeaderModifier {
	if in == nil {
		return nil
	}
	out := &agw.HeaderModifier{Remove: append([]string(nil), in.Remove...)}
	for _, h := range in.Add {
		out.Add = append(out.Add, &agw.Header{Name: string(h.Name), Value: h.Value})
	}
	for _, h := range in.Set {
		out.Set = append(out.Set, &agw.Header{Name: string(h.Name), Value: h.Value})
	}
	return out
}

func translateRequestRedirect(in *k8s.HTTPRequestRedirectFilter) *agw.RequestRedirect {
	out := &agw.RequestRedirect{}
	if in.Scheme != nil {
		out.Scheme = *in.Scheme
	}
	if in.Hostname != nil {
		out.Host = string(*in.Hostname)
	}
	if in.Port != nil {
		out.Port = uint32(*in.Port)
	}
	if in.StatusCode != nil {
		out.Status = uint32(*in.StatusCode)
	}
	if in.Path != nil {
		switch in.Path.Type {
		case k8s.FullPathHTTPPathModifier:
			if in.Path.ReplaceFullPath != nil {
				out.Path = &agw.RequestRedirect_Full{Full: *in.Path.ReplaceFullPath}
			}
		case k8s.PrefixMatchHTTPPathModifier:
			if in.Path.ReplacePrefixMatch != nil {
				out.Path = &agw.RequestRedirect_Prefix{Prefix: *in.Path.ReplacePrefixMatch}
			}
		}
	}
	return out
}

func translateURLRewrite(in *k8s.HTTPURLRewriteFilter) *agw.UrlRewrite {
	out := &agw.UrlRewrite{}
	if in.Hostname != nil {
		out.Host = string(*in.Hostname)
	}
	if in.Path != nil {
		switch in.Path.Type {
		case k8s.FullPathHTTPPathModifier:
			if in.Path.ReplaceFullPath != nil {
				out.Path = &agw.UrlRewrite_Full{Full: *in.Path.ReplaceFullPath}
			}
		case k8s.PrefixMatchHTTPPathModifier:
			if in.Path.ReplacePrefixMatch != nil {
				out.Path = &agw.UrlRewrite_Prefix{Prefix: *in.Path.ReplacePrefixMatch}
			}
		}
	}
	return out
}

func translateRequestMirror(routeNS string, in *k8s.HTTPRequestMirrorFilter, refSet map[string]struct{}) *agw.RequestMirrors {
	if in == nil {
		return nil
	}
	backend, ok := backendReferenceFromObject(routeNS, in.BackendRef, refSet)
	if !ok {
		return nil
	}
	return &agw.RequestMirrors{Mirrors: []*agw.RequestMirrors_Mirror{{Backend: backend, Percentage: mirrorPercentage(in)}}}
}

func mirrorPercentage(in *k8s.HTTPRequestMirrorFilter) float64 {
	if in.Percent != nil {
		return float64(*in.Percent)
	}
	if in.Fraction != nil {
		denominator := int32(100)
		if in.Fraction.Denominator != nil {
			denominator = *in.Fraction.Denominator
		}
		if denominator > 0 {
			return float64(in.Fraction.Numerator) * 100 / float64(denominator)
		}
	}
	return 100
}

func translateCORS(in *k8s.HTTPCORSFilter) *agw.CORS {
	out := &agw.CORS{}
	if in.AllowCredentials != nil {
		out.AllowCredentials = *in.AllowCredentials
	}
	for _, h := range in.AllowHeaders {
		out.AllowHeaders = append(out.AllowHeaders, string(h))
	}
	for _, m := range in.AllowMethods {
		out.AllowMethods = append(out.AllowMethods, string(m))
	}
	for _, o := range in.AllowOrigins {
		out.AllowOrigins = append(out.AllowOrigins, string(o))
	}
	for _, h := range in.ExposeHeaders {
		out.ExposeHeaders = append(out.ExposeHeaders, string(h))
	}
	if in.MaxAge > 0 {
		out.MaxAge = durationpb.New(time.Duration(in.MaxAge) * time.Second)
	}
	return out
}

func translateHTTPBackends(routeNS string, refs []k8s.HTTPBackendRef, refSet map[string]struct{}, backendTLS backendTLSPolicyMap) []*agw.RouteBackend {
	backends := make([]k8s.BackendRef, 0, len(refs))
	for _, r := range refs {
		backends = append(backends, r.BackendRef)
	}
	return translateBackendRefs(routeNS, backends, refSet, backendTLS)
}

func translateGRPCBackends(routeNS string, refs []k8s.GRPCBackendRef, refSet map[string]struct{}, backendTLS backendTLSPolicyMap) []*agw.RouteBackend {
	backends := make([]k8s.BackendRef, 0, len(refs))
	for _, r := range refs {
		backends = append(backends, r.BackendRef)
	}
	return translateBackendRefs(routeNS, backends, refSet, backendTLS)
}

func translateBackendRefs(routeNS string, refs []k8s.BackendRef, refSet map[string]struct{}, backendTLS backendTLSPolicyMap) []*agw.RouteBackend {
	out := make([]*agw.RouteBackend, 0, len(refs))
	for _, b := range refs {
		backend, ok := backendReferenceFromObject(routeNS, b.BackendObjectReference, refSet)
		if !ok {
			continue
		}
		weight := int32(1)
		if b.Weight != nil {
			weight = *b.Weight
		}
		rb := &agw.RouteBackend{Backend: backend, Weight: weight}
		if key, ok := backendServiceKey(routeNS, b.BackendObjectReference); ok {
			if policy := backendTLS[key]; policy != nil {
				rb.BackendPolicies = append(rb.BackendPolicies, policy)
			}
		}
		out = append(out, rb)
	}
	return out
}

func backendReferenceFromObject(routeNS string, b k8s.BackendObjectReference, refSet map[string]struct{}) (*agw.BackendReference, bool) {
	key, ok := backendServiceKey(routeNS, b)
	if !ok {
		return nil, false
	}
	parts := strings.SplitN(key, "/", 2)
	ns, name := parts[0], parts[1]
	port := uint32(0)
	if b.Port != nil {
		port = uint32(*b.Port)
	}
	hostname := fmt.Sprintf("%s.%s.svc.cluster.local", name, ns)
	if refSet != nil {
		refSet[key] = struct{}{}
	}
	return &agw.BackendReference{
		Kind: &agw.BackendReference_Service_{
			Service: &agw.BackendReference_Service{Namespace: ns, Hostname: hostname},
		},
		Port: port,
	}, true
}

func backendServiceKey(routeNS string, b k8s.BackendObjectReference) (string, bool) {
	group := ""
	if b.Group != nil {
		group = string(*b.Group)
	}
	kind := "Service"
	if b.Kind != nil {
		kind = string(*b.Kind)
	}
	if group != "" || kind != "Service" {
		return "", false
	}
	ns := routeNS
	if b.Namespace != nil {
		ns = string(*b.Namespace)
	}
	return ns + "/" + string(b.Name), true
}

func backendTLSPolicies(store model.ConfigStore, gwSpec *k8s.GatewaySpec) backendTLSPolicyMap {
	cfgs := store.List(gvk.BackendTLSPolicy, "")
	clientCert, clientKey := gatewayBackendTLS(gwSpec)
	sort.SliceStable(cfgs, func(i, j int) bool {
		if !cfgs[i].CreationTimestamp.Equal(cfgs[j].CreationTimestamp) {
			return cfgs[i].CreationTimestamp.Before(cfgs[j].CreationTimestamp)
		}
		return cfgs[i].Namespace+"/"+cfgs[i].Name < cfgs[j].Namespace+"/"+cfgs[j].Name
	})
	out := backendTLSPolicyMap{}
	for _, cfg := range cfgs {
		spec, ok := cfg.Spec.(*k8s.BackendTLSPolicySpec)
		if !ok || spec == nil {
			continue
		}
		policy := translateBackendTLSPolicy(spec, clientCert, clientKey)
		if policy == nil {
			continue
		}
		for _, target := range spec.TargetRefs {
			if !backendTLSTargetsService(target) {
				continue
			}
			key := cfg.Namespace + "/" + string(target.Name)
			if _, exists := out[key]; !exists {
				out[key] = policy
			}
		}
	}
	return out
}

func backendTLSTargetsService(target k8s.LocalPolicyTargetReferenceWithSectionName) bool {
	return target.Group == "" && target.Kind == "Service" && target.SectionName == nil
}

func translateBackendTLSPolicy(spec *k8s.BackendTLSPolicySpec, clientCert, clientKey []byte) *agw.BackendPolicySpec {
	root, ok := backendTLSRoot(spec.Validation)
	if !ok {
		return nil
	}
	hostname := string(spec.Validation.Hostname)
	tls := &agw.BackendPolicySpec_BackendTLS{
		Hostname:     stringPtr(hostname),
		Verification: agw.BackendPolicySpec_BackendTLS_STRICT,
	}
	if root != nil {
		tls.Root = root
	}
	if clientCert != nil && clientKey != nil {
		tls.Cert = clientCert
		tls.Key = clientKey
	}
	if len(spec.Validation.SubjectAltNames) == 0 {
		tls.VerifySubjectAltNames = append(tls.VerifySubjectAltNames, hostname)
	} else {
		for _, san := range spec.Validation.SubjectAltNames {
			switch san.Type {
			case k8s.HostnameSubjectAltNameType:
				tls.VerifySubjectAltNames = append(tls.VerifySubjectAltNames, string(san.Hostname))
			case k8s.URISubjectAltNameType:
				tls.VerifySubjectAltNames = append(tls.VerifySubjectAltNames, string(san.URI))
			}
		}
	}
	return &agw.BackendPolicySpec{Kind: &agw.BackendPolicySpec_BackendTls{BackendTls: tls}}
}

func backendTLSRoot(validation k8s.BackendTLSPolicyValidation) ([]byte, bool) {
	if validation.WellKnownCACertificates != nil && *validation.WellKnownCACertificates == k8s.WellKnownCACertificatesSystem {
		return nil, true
	}
	root := loadLocalCARefs(validation.CACertificateRefs)
	return root, root != nil
}

func gatewayBackendTLS(gwSpec *k8s.GatewaySpec) ([]byte, []byte) {
	if gwSpec == nil || gwSpec.TLS == nil || gwSpec.TLS.Backend == nil || gwSpec.TLS.Backend.ClientCertificateRef == nil {
		return nil, nil
	}
	name, ok := tlsSecretName(*gwSpec.TLS.Backend.ClientCertificateRef)
	if !ok {
		return nil, nil
	}
	return loadTLSKeyPair(name)
}

func loadLocalCARefs(refs []k8s.LocalObjectReference) []byte {
	var root []byte
	for _, ref := range refs {
		if ref.Group != "" || ref.Kind != "ConfigMap" {
			continue
		}
		root = append(root, loadCACert(string(ref.Name))...)
	}
	return emptyToNil(root)
}

func loadObjectCARefs(refs []k8s.ObjectReference) []byte {
	var root []byte
	for _, ref := range refs {
		if ref.Group != "" || ref.Kind != "ConfigMap" {
			continue
		}
		root = append(root, loadCACert(string(ref.Name))...)
	}
	return emptyToNil(root)
}

func emptyToNil(in []byte) []byte {
	if len(in) == 0 {
		return nil
	}
	return in
}

func stringPtr(s string) *string {
	return &s
}

func pathPrefixMatch(prefix string) *agw.PathMatch {
	return &agw.PathMatch{Kind: &agw.PathMatch_PathPrefix{PathPrefix: prefix}}
}

func routeName(kind string, rc config.Config, ruleName *k8s.SectionName) *agw.RouteName {
	out := &agw.RouteName{Kind: kind, Name: rc.Name, Namespace: rc.Namespace}
	if ruleName != nil {
		rn := string(*ruleName)
		out.RuleName = &rn
	}
	return out
}

func routeHostnames(in []k8s.Hostname) []string {
	out := make([]string, 0, len(in))
	for _, h := range in {
		out = append(out, string(h))
	}
	return out
}

func listenerKey(namespace, gateway string, name k8s.SectionName) string {
	return fmt.Sprintf("%s/%s/%s", namespace, gateway, name)
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

func listenerTLSConfig(l k8s.Listener, gwSpec *k8s.GatewaySpec) *agw.TLSConfig {
	if l.TLS == nil {
		return nil
	}
	if l.TLS.Mode != nil && *l.TLS.Mode == k8s.TLSModePassthrough {
		return nil
	}
	var out *agw.TLSConfig
	for _, ref := range l.TLS.CertificateRefs {
		name, ok := tlsSecretName(ref)
		if !ok {
			continue
		}
		out = loadTLSConfig(name)
		if out != nil {
			break
		}
	}
	if out == nil {
		return nil
	}
	if root, mode := frontendTLSValidation(l.Port, gwSpec); root != nil {
		out.Root = root
		out.MtlsMode = mode
	}
	return out
}

func tlsSecretName(ref k8s.SecretObjectReference) (string, bool) {
	group := ""
	if ref.Group != nil {
		group = string(*ref.Group)
	}
	kind := "Secret"
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}
	if group != "" || kind != "Secret" {
		return "", false
	}
	return string(ref.Name), true
}

func frontendTLSValidation(port k8s.PortNumber, gwSpec *k8s.GatewaySpec) ([]byte, agw.TLSConfig_MTLSMode) {
	if gwSpec == nil || gwSpec.TLS == nil || gwSpec.TLS.Frontend == nil {
		return nil, agw.TLSConfig_STRICT
	}
	tlsConfig := gwSpec.TLS.Frontend.Default
	for _, override := range gwSpec.TLS.Frontend.PerPort {
		if override.Port == port {
			tlsConfig = override.TLS
			break
		}
	}
	if tlsConfig.Validation == nil {
		return nil, agw.TLSConfig_STRICT
	}
	root := loadObjectCARefs(tlsConfig.Validation.CACertificateRefs)
	if root == nil {
		return nil, agw.TLSConfig_STRICT
	}
	if tlsConfig.Validation.Mode == k8s.AllowInsecureFallback {
		return root, agw.TLSConfig_ALLOW_INSECURE_FALLBACK
	}
	return root, agw.TLSConfig_STRICT
}

func loadTLSConfig(name string) *agw.TLSConfig {
	cert, key := loadTLSKeyPair(name)
	if cert == nil || key == nil {
		return nil
	}
	return &agw.TLSConfig{Cert: cert, PrivateKey: key}
}

func loadTLSKeyPair(name string) ([]byte, []byte) {
	if name == "" {
		return nil, nil
	}
	dir := filepath.Join(tlsMountRoot, name)
	cert, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if err != nil {
		return nil, nil
	}
	key, err := os.ReadFile(filepath.Join(dir, "tls.key"))
	if err != nil {
		return nil, nil
	}
	return cert, key
}

func loadCACert(name string) []byte {
	if name == "" {
		return nil
	}
	ca, err := os.ReadFile(filepath.Join(tlsMountRoot, name, "ca.crt"))
	if err != nil {
		return nil
	}
	return ca
}
