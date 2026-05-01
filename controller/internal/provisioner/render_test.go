package provisioner

import (
	"strings"
	"testing"

	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestAgentGatewayNamesPreserveShortLegacyForm(t *testing.T) {
	gw := &gwapiv1.Gateway{}
	gw.Namespace = "app"
	gw.Name = "gw"

	if got := AgentGatewayObjectName(gw); got != "gw-app-gw" {
		t.Fatalf("object name = %q, want gw-app-gw", got)
	}
	if got := AgentGatewayTokenSecretName(gw); got != "gw-app-gw-token" {
		t.Fatalf("token secret name = %q, want gw-app-gw-token", got)
	}
}

func TestAgentGatewayRenderUsesSafeNamesAndSourceAnnotations(t *testing.T) {
	gw := &gwapiv1.Gateway{}
	gw.Namespace = strings.Repeat("namespace", 8)
	gw.Name = strings.Repeat("gateway", 9)
	params := GatewayParams{
		Tenant:          "tenant",
		InfraNamespace:  "infra",
		Gateway:         gw,
		TokenExpiration: "2026-05-01T00:00:00Z",
	}

	dep := AgentGatewayDeployment(params)
	svc := AgentGatewayService(params)
	secretName := AgentGatewayTokenSecretName(gw)

	for _, name := range []string{dep.Name, svc.Name, secretName} {
		if len(name) > 63 {
			t.Fatalf("name %q has length %d, want <= 63", name, len(name))
		}
		if strings.HasSuffix(name, "-") {
			t.Fatalf("name %q ends with dash", name)
		}
	}
	if dep.Name != svc.Name {
		t.Fatalf("deployment and service names differ: %q vs %q", dep.Name, svc.Name)
	}
	if dep.Annotations[GatewayNSAnnotation] != gw.Namespace || dep.Annotations[GatewayNameAnnotation] != gw.Name {
		t.Fatalf("deployment annotations = %#v", dep.Annotations)
	}
	if svc.Annotations[GatewayNSAnnotation] != gw.Namespace || svc.Annotations[GatewayNameAnnotation] != gw.Name {
		t.Fatalf("service annotations = %#v", svc.Annotations)
	}
	if got := dep.Spec.Template.Annotations[MemberTokenExpirationAnnotation]; got != params.TokenExpiration {
		t.Fatalf("token expiration annotation = %q", got)
	}
	if len(dep.Labels[GatewayLabel]) > 63 || len(dep.Labels[GatewayNamespaceLabel]) > 63 {
		t.Fatalf("gateway labels not length-safe: %#v", dep.Labels)
	}
	volumes := dep.Spec.Template.Spec.Volumes
	caVolume := volumes[1]
	if caVolume.Secret.Optional == nil || *caVolume.Secret.Optional {
		t.Fatalf("agentgateway CA volume is optional: %#v", caVolume.Secret.Optional)
	}
}

func TestIstiodTLSVolumeNamesAreLengthSafe(t *testing.T) {
	params := IstiodParams{
		Tenant:               "tenant",
		Namespace:            "infra",
		MemberKubeconfigName: "member-kubeconfig",
		TLSCertNames:         []string{strings.Repeat("secret", 20)},
	}

	dep := IstiodDeployment(params)
	volumes := dep.Spec.Template.Spec.Volumes
	got := volumes[len(volumes)-1].Name

	if len(got) > 63 {
		t.Fatalf("volume name = %q has length %d, want <= 63", got, len(got))
	}
	if !strings.HasPrefix(got, "tls-") {
		t.Fatalf("volume name = %q, want tls- prefix", got)
	}
	if volumes[len(volumes)-1].Secret.Optional == nil || *volumes[len(volumes)-1].Secret.Optional {
		t.Fatalf("TLS volume is optional: %#v", volumes[len(volumes)-1].Secret.Optional)
	}
}
