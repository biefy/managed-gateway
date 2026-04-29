// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package xds

import (
	"testing"
	"time"

	k8s "sigs.k8s.io/gateway-api/apis/v1"

	"istio.io/istio/pkg/config"
)

func TestTranslateHTTPMatches(t *testing.T) {
	pathType := k8s.PathMatchExact
	pathValue := "/v1"
	headerType := k8s.HeaderMatchRegularExpression
	queryType := k8s.QueryParamMatchExact
	method := k8s.HTTPMethodPost

	matches := translateHTTPMatches([]k8s.HTTPRouteMatch{{
		Path: &k8s.HTTPPathMatch{Type: &pathType, Value: &pathValue},
		Headers: []k8s.HTTPHeaderMatch{{
			Type:  &headerType,
			Name:  "x-version",
			Value: "v[12]",
		}},
		QueryParams: []k8s.HTTPQueryParamMatch{{
			Type:  &queryType,
			Name:  "debug",
			Value: "true",
		}},
		Method: &method,
	}})

	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(matches))
	}
	match := matches[0]
	if got := match.GetPath().GetExact(); got != "/v1" {
		t.Fatalf("path exact = %q, want /v1", got)
	}
	if got := match.GetMethod().GetExact(); got != "POST" {
		t.Fatalf("method exact = %q, want POST", got)
	}
	if got := match.GetHeaders()[0].GetRegex(); got != "v[12]" {
		t.Fatalf("header regex = %q, want v[12]", got)
	}
	if got := match.GetQueryParams()[0].GetExact(); got != "true" {
		t.Fatalf("query exact = %q, want true", got)
	}
}

func TestMatchedListenersFiltersProtocolAndHostname(t *testing.T) {
	gatewayNS := k8s.Namespace("infra")
	gwSpec := &k8s.GatewaySpec{Listeners: []k8s.Listener{
		{Name: "web", Port: 80, Protocol: k8s.HTTPProtocolType, Hostname: hostnamePtr("*.example.com")},
		{Name: "secure", Port: 443, Protocol: k8s.TLSProtocolType, Hostname: hostnamePtr("secure.example.com")},
	}}
	parentRefs := []k8s.ParentReference{{Name: "gw", Namespace: &gatewayNS}}
	routeCfg := config.Config{Meta: config.Meta{Name: "route", Namespace: "app"}}

	httpMatched := matchedListeners(routeCfg, parentRefs, "HTTPRoute", []k8s.Hostname{"api.example.com"}, "infra", "gw", gwSpec)
	if len(httpMatched) != 1 || httpMatched[0].key != "infra/gw/web" {
		t.Fatalf("HTTPRoute matched %#v, want infra/gw/web", httpMatched)
	}

	tlsMatched := matchedListeners(routeCfg, parentRefs, "TLSRoute", []k8s.Hostname{"api.example.com"}, "infra", "gw", gwSpec)
	if len(tlsMatched) != 0 {
		t.Fatalf("TLSRoute matched %#v, want no hostname match", tlsMatched)
	}
}

func TestTranslateGRPCMatches(t *testing.T) {
	service := "echo.Echo"
	method := "Ping"

	matches := translateGRPCMatches([]k8s.GRPCRouteMatch{{Method: &k8s.GRPCMethodMatch{Service: &service, Method: &method}}})
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(matches))
	}
	if got := matches[0].GetPath().GetExact(); got != "/echo.Echo/Ping" {
		t.Fatalf("gRPC exact path = %q, want /echo.Echo/Ping", got)
	}

	matches = translateGRPCMatches([]k8s.GRPCRouteMatch{{Method: &k8s.GRPCMethodMatch{Service: &service}}})
	if got := matches[0].GetPath().GetPathPrefix(); got != "/echo.Echo/" {
		t.Fatalf("gRPC service prefix = %q, want /echo.Echo/", got)
	}
}

func TestTranslateBackendRefs(t *testing.T) {
	port := k8s.PortNumber(8080)
	weight := int32(7)
	backends := translateBackendRefs("app", []k8s.BackendRef{{
		BackendObjectReference: k8s.BackendObjectReference{Name: "svc", Port: &port},
		Weight:                 &weight,
	}}, nil, nil)

	if len(backends) != 1 {
		t.Fatalf("got %d backends, want 1", len(backends))
	}
	if got := backends[0].GetBackend().GetService().GetHostname(); got != "svc.app.svc.cluster.local" {
		t.Fatalf("hostname = %q, want svc.app.svc.cluster.local", got)
	}
	if got := backends[0].GetBackend().GetPort(); got != 8080 {
		t.Fatalf("port = %d, want 8080", got)
	}
	if got := backends[0].GetWeight(); got != 7 {
		t.Fatalf("weight = %d, want 7", got)
	}
}

func TestTranslateHTTPPolicies(t *testing.T) {
	requestTimeout := k8s.Duration("10s")
	rewriteHost := k8s.PreciseHostname("rewritten.example.com")
	pathType := k8s.PrefixMatchHTTPPathModifier
	pathReplacement := "/v2"
	percent := int32(25)
	mirrorPort := k8s.PortNumber(8081)

	policies := translateHTTPPolicies("app", []k8s.HTTPRouteFilter{
		{
			Type: k8s.HTTPRouteFilterURLRewrite,
			URLRewrite: &k8s.HTTPURLRewriteFilter{
				Hostname: &rewriteHost,
				Path:     &k8s.HTTPPathModifier{Type: pathType, ReplacePrefixMatch: &pathReplacement},
			},
		},
		{
			Type: k8s.HTTPRouteFilterRequestMirror,
			RequestMirror: &k8s.HTTPRequestMirrorFilter{
				BackendRef: k8s.BackendObjectReference{Name: "mirror", Port: &mirrorPort},
				Percent:    &percent,
			},
		},
	}, &k8s.HTTPRouteTimeouts{Request: &requestTimeout}, nil)

	if len(policies) != 3 {
		t.Fatalf("got %d policies, want 3", len(policies))
	}
	if got := policies[0].GetTimeout().GetRequest().AsDuration(); got != 10*time.Second {
		t.Fatalf("timeout = %s, want 10s", got)
	}
	if got := policies[1].GetUrlRewrite().GetHost(); got != "rewritten.example.com" {
		t.Fatalf("rewrite host = %q, want rewritten.example.com", got)
	}
	mirrors := policies[2].GetRequestMirror().GetMirrors()
	if len(mirrors) != 1 || mirrors[0].GetBackend().GetService().GetHostname() != "mirror.app.svc.cluster.local" || mirrors[0].GetPercentage() != 25 {
		t.Fatalf("mirror policy = %#v, want mirror.app.svc.cluster.local at 25 percent", mirrors)
	}
}

func TestTranslateBackendTLSPolicy(t *testing.T) {
	wellKnown := k8s.WellKnownCACertificatesSystem
	hostname := k8s.PreciseHostname("backend.example.com")
	uri := k8s.AbsoluteURI("spiffe://cluster.local/ns/app/sa/backend")

	policy := translateBackendTLSPolicy(&k8s.BackendTLSPolicySpec{
		Validation: k8s.BackendTLSPolicyValidation{
			WellKnownCACertificates: &wellKnown,
			Hostname:                hostname,
			SubjectAltNames: []k8s.SubjectAltName{{
				Type: k8s.URISubjectAltNameType,
				URI:  uri,
			}},
		},
	}, []byte("client-cert"), []byte("client-key"))

	if policy == nil || policy.GetBackendTls() == nil {
		t.Fatalf("got nil BackendTLS policy")
	}
	tls := policy.GetBackendTls()
	if got := tls.GetHostname(); got != "backend.example.com" {
		t.Fatalf("backend tls hostname = %q, want backend.example.com", got)
	}
	if got := tls.GetVerifySubjectAltNames(); len(got) != 1 || got[0] != string(uri) {
		t.Fatalf("backend tls SANs = %#v, want %q", got, uri)
	}
	if got := tls.GetRoot(); got != nil {
		t.Fatalf("backend tls root = %#v, want system roots", got)
	}
	if got := string(tls.GetCert()); got != "client-cert" {
		t.Fatalf("backend tls client cert = %q, want client-cert", got)
	}
	if got := string(tls.GetKey()); got != "client-key" {
		t.Fatalf("backend tls client key = %q, want client-key", got)
	}
}

func TestTranslateBackendRefsAppliesBackendTLSPolicy(t *testing.T) {
	port := k8s.PortNumber(8443)
	wellKnown := k8s.WellKnownCACertificatesSystem
	policy := translateBackendTLSPolicy(&k8s.BackendTLSPolicySpec{
		Validation: k8s.BackendTLSPolicyValidation{
			WellKnownCACertificates: &wellKnown,
			Hostname:                k8s.PreciseHostname("backend.example.com"),
		},
	}, []byte("client-cert"), []byte("client-key"))

	backends := translateBackendRefs("app", []k8s.BackendRef{{
		BackendObjectReference: k8s.BackendObjectReference{Name: "svc", Port: &port},
	}}, nil, backendTLSPolicyMap{"app/svc": policy})

	if len(backends) != 1 {
		t.Fatalf("got %d backends, want 1", len(backends))
	}
	policies := backends[0].GetBackendPolicies()
	if len(policies) != 1 || policies[0].GetBackendTls().GetHostname() != "backend.example.com" {
		t.Fatalf("backend policies = %#v, want backend.example.com BackendTLS", policies)
	}
}

func hostnamePtr(h k8s.Hostname) *k8s.Hostname {
	return &h
}
