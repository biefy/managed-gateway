package provisioner

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestApplyPreservesServiceAssignedFields(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	family := corev1.IPv4Protocol
	policy := corev1.IPFamilyPolicySingleStack
	allocateNodePorts := true
	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gw",
			Namespace: "tenant",
			Labels:    map[string]string{ManagedByLabel: ManagedByValue},
		},
		Spec: corev1.ServiceSpec{
			Type:                          corev1.ServiceTypeLoadBalancer,
			ClusterIP:                     "10.0.0.10",
			ClusterIPs:                    []string{"10.0.0.10"},
			IPFamilies:                    []corev1.IPFamily{family},
			IPFamilyPolicy:                &policy,
			HealthCheckNodePort:           32000,
			AllocateLoadBalancerNodePorts: &allocateNodePorts,
			Ports: []corev1.ServicePort{{
				Name:     "http",
				Port:     80,
				Protocol: corev1.ProtocolTCP,
				NodePort: 30080,
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gw",
			Namespace: "tenant",
			Labels:    map[string]string{ManagedByLabel: ManagedByValue},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{
				Name:     "http",
				Port:     80,
				Protocol: corev1.ProtocolTCP,
			}},
		},
	}

	if err := Apply(ctx, c, desired); err != nil {
		t.Fatal(err)
	}

	var got corev1.Service
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tenant", Name: "gw"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.ClusterIP != "10.0.0.10" || len(got.Spec.ClusterIPs) != 1 || got.Spec.ClusterIPs[0] != "10.0.0.10" {
		t.Fatalf("cluster IP fields were not preserved: %#v", got.Spec)
	}
	if len(got.Spec.IPFamilies) != 1 || got.Spec.IPFamilies[0] != family || got.Spec.IPFamilyPolicy == nil || *got.Spec.IPFamilyPolicy != policy {
		t.Fatalf("IP family fields were not preserved: %#v", got.Spec)
	}
	if got.Spec.HealthCheckNodePort != 32000 {
		t.Fatalf("healthCheckNodePort = %d, want 32000", got.Spec.HealthCheckNodePort)
	}
	if got.Spec.AllocateLoadBalancerNodePorts == nil || *got.Spec.AllocateLoadBalancerNodePorts != allocateNodePorts {
		t.Fatalf("allocateLoadBalancerNodePorts = %#v", got.Spec.AllocateLoadBalancerNodePorts)
	}
	if len(got.Spec.Ports) != 1 || got.Spec.Ports[0].NodePort != 30080 {
		t.Fatalf("ports = %#v, want nodePort 30080", got.Spec.Ports)
	}
}

func TestApplyRefusesObjectManagedByAnotherController(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gw",
			Namespace: "tenant",
			Labels:    map[string]string{ManagedByLabel: "other-controller"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gw",
			Namespace: "tenant",
			Labels:    map[string]string{ManagedByLabel: ManagedByValue},
		},
	}

	if err := Apply(ctx, c, desired); err == nil {
		t.Fatal("Apply succeeded for object managed by another controller")
	}
}
