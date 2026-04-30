package gateway

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestServiceGatewayAddresses(t *testing.T) {
	svc := &corev1.Service{Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{
		{IP: "1.2.3.4"},
		{Hostname: "gateway.example.com"},
	}}}}

	got := serviceGatewayAddresses(svc)

	if len(got) != 2 {
		t.Fatalf("got %d addresses, want 2: %#v", len(got), got)
	}
	if got[0].Value != "1.2.3.4" || got[0].Type == nil || *got[0].Type != gwapiv1.IPAddressType {
		t.Fatalf("IP address = %#v", got[0])
	}
	if got[1].Value != "gateway.example.com" || got[1].Type == nil || *got[1].Type != gwapiv1.HostnameAddressType {
		t.Fatalf("hostname address = %#v", got[1])
	}
}
