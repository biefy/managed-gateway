package gateway

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"gateway.appnet.azure.com/controller/controller/internal/provisioner"
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

func TestTokenSecretCurrentRequiresTokenAndFreshExpiration(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			provisioner.MemberTokenExpirationAnnotation: now.Add(agentGatewayTokenRefresh + time.Hour).Format(time.RFC3339),
		}},
		Data: map[string][]byte{"token": []byte("token")},
	}
	if !tokenSecretCurrent(secret, now) {
		t.Fatal("fresh token secret was not current")
	}

	secret.Annotations[provisioner.MemberTokenExpirationAnnotation] = now.Add(agentGatewayTokenRefresh).Format(time.RFC3339)
	if tokenSecretCurrent(secret, now) {
		t.Fatal("token inside refresh window was current")
	}
	secret.Annotations[provisioner.MemberTokenExpirationAnnotation] = now.Add(agentGatewayTokenRefresh + time.Hour).Format(time.RFC3339)
	delete(secret.Data, "token")
	if tokenSecretCurrent(secret, now) {
		t.Fatal("token secret without token data was current")
	}
}

func TestValidateGatewayTLSAssetsRequiresInfraSecretMaterial(t *testing.T) {
	ctx := context.Background()
	memberScheme := runtime.NewScheme()
	if err := gwapiv1.Install(memberScheme); err != nil {
		t.Fatal(err)
	}
	infraScheme := runtime.NewScheme()
	if err := corev1.AddToScheme(infraScheme); err != nil {
		t.Fatal(err)
	}
	gw := &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app"}}
	gw.Spec.Listeners = []gwapiv1.Listener{{
		Name:     "https",
		Port:     443,
		Protocol: gwapiv1.HTTPSProtocolType,
		TLS: &gwapiv1.ListenerTLSConfig{CertificateRefs: []gwapiv1.SecretObjectReference{{
			Name: "cert",
		}}},
	}}
	memberClient := fake.NewClientBuilder().WithScheme(memberScheme).Build()
	infraClient := fake.NewClientBuilder().WithScheme(infraScheme).Build()

	err := validateGatewayTLSAssets(ctx, Deps{InfraNamespace: "infra", MemberClient: memberClient, InfraClient: infraClient}, gw)
	if err == nil {
		t.Fatal("missing infra Secret was accepted")
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cert", Namespace: "infra"},
		Data: map[string][]byte{
			"tls.crt": []byte("cert"),
			"tls.key": []byte("key"),
		},
	}
	infraClient = fake.NewClientBuilder().WithScheme(infraScheme).WithObjects(secret).Build()
	if err := validateGatewayTLSAssets(ctx, Deps{InfraNamespace: "infra", MemberClient: memberClient, InfraClient: infraClient}, gw); err != nil {
		t.Fatal(err)
	}
}

func TestValidateGatewayTLSAssetsEnforcesReferenceGrant(t *testing.T) {
	ctx := context.Background()
	memberScheme := runtime.NewScheme()
	if err := gwapiv1.Install(memberScheme); err != nil {
		t.Fatal(err)
	}
	infraScheme := runtime.NewScheme()
	if err := corev1.AddToScheme(infraScheme); err != nil {
		t.Fatal(err)
	}
	other := gwapiv1.Namespace("other")
	gw := &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app"}}
	gw.Spec.Listeners = []gwapiv1.Listener{{
		Name:     "https",
		Port:     443,
		Protocol: gwapiv1.HTTPSProtocolType,
		TLS: &gwapiv1.ListenerTLSConfig{CertificateRefs: []gwapiv1.SecretObjectReference{{
			Name:      "cert",
			Namespace: &other,
		}}},
	}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cert", Namespace: "infra"},
		Data: map[string][]byte{
			"tls.crt": []byte("cert"),
			"tls.key": []byte("key"),
		},
	}
	infraClient := fake.NewClientBuilder().WithScheme(infraScheme).WithObjects(secret).Build()
	memberClient := fake.NewClientBuilder().WithScheme(memberScheme).Build()

	err := validateGatewayTLSAssets(ctx, Deps{InfraNamespace: "infra", MemberClient: memberClient, InfraClient: infraClient}, gw)
	if err == nil {
		t.Fatal("cross-namespace certificateRef without ReferenceGrant was accepted")
	}

	toName := gwapiv1.ObjectName("cert")
	grant := &gwapiv1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "allow", Namespace: "other"},
		Spec: gwapiv1.ReferenceGrantSpec{
			From: []gwapiv1.ReferenceGrantFrom{{
				Group:     gwapiv1.Group(gwapiv1.GroupVersion.Group),
				Kind:      "Gateway",
				Namespace: "app",
			}},
			To: []gwapiv1.ReferenceGrantTo{{
				Group: "",
				Kind:  "Secret",
				Name:  &toName,
			}},
		},
	}
	memberClient = fake.NewClientBuilder().WithScheme(memberScheme).WithObjects(grant).Build()
	if err := validateGatewayTLSAssets(ctx, Deps{InfraNamespace: "infra", MemberClient: memberClient, InfraClient: infraClient}, gw); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteAgentGatewaySkipsUnmanagedObjects(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "gw-app-gw", Namespace: "infra"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()

	if err := deleteAgentGateway(ctx, Deps{InfraNamespace: "infra", InfraClient: c}, types.NamespacedName{Namespace: "app", Name: "gw"}); err != nil {
		t.Fatal(err)
	}

	var got appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Namespace: "infra", Name: "gw-app-gw"}, &got); err != nil {
		t.Fatalf("unmanaged deployment was deleted: %v", err)
	}
}

func TestDeleteAgentGatewayDeletesMatchingManagedObjects(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gw := &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app"}}
	params := provisioner.GatewayParams{Tenant: "tenant", InfraNamespace: "infra", Gateway: gw}
	dep := provisioner.AgentGatewayDeployment(params)
	svc := provisioner.AgentGatewayService(params)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:        provisioner.AgentGatewayTokenSecretName(gw),
		Namespace:   "infra",
		Labels:      provisioner.AgentGatewayLabels("tenant", gw),
		Annotations: provisioner.AgentGatewayAnnotations(gw),
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, svc, secret).Build()

	if err := deleteAgentGateway(ctx, Deps{InfraNamespace: "infra", Platform: "", InfraClient: c}, types.NamespacedName{Namespace: "app", Name: "gw"}); err != nil {
		t.Fatal(err)
	}

	if err := c.Get(ctx, types.NamespacedName{Namespace: "infra", Name: dep.Name}, &appsv1.Deployment{}); err == nil {
		t.Fatal("deployment still exists")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "infra", Name: svc.Name}, &corev1.Service{}); err == nil {
		t.Fatal("service still exists")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "infra", Name: secret.Name}, &corev1.Secret{}); err == nil {
		t.Fatal("token secret still exists")
	}
}
