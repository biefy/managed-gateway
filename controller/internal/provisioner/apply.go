package provisioner

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EnsureNamespace creates the namespace if missing (idempotent).
func EnsureNamespace(ctx context.Context, c client.Client, name string) error {
	var ns corev1.Namespace
	err := c.Get(ctx, types.NamespacedName{Name: name}, &ns)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get namespace %s: %w", name, err)
	}
	ns = corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				ManagedByLabel: ManagedByValue,
			},
		},
	}
	if err := c.Create(ctx, &ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %s: %w", name, err)
	}
	return nil
}

func prepareForUpdate(existing, obj client.Object) error {
	existingLabels := existing.GetLabels()
	if owner := existingLabels[ManagedByLabel]; owner != "" && owner != ManagedByValue {
		return fmt.Errorf("refusing to update unmanaged object %s/%s", existing.GetNamespace(), existing.GetName())
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	if desired, ok := obj.(*corev1.Service); ok {
		current := existing.(*corev1.Service)
		preserveServiceFields(current, desired)
	}
	return nil
}

func preserveServiceFields(current, desired *corev1.Service) {
	desired.Spec.ClusterIP = current.Spec.ClusterIP
	desired.Spec.ClusterIPs = append([]string(nil), current.Spec.ClusterIPs...)
	desired.Spec.IPFamilies = append([]corev1.IPFamily(nil), current.Spec.IPFamilies...)
	desired.Spec.IPFamilyPolicy = current.Spec.IPFamilyPolicy
	desired.Spec.HealthCheckNodePort = current.Spec.HealthCheckNodePort
	desired.Spec.AllocateLoadBalancerNodePorts = current.Spec.AllocateLoadBalancerNodePorts
	for i := range desired.Spec.Ports {
		for _, currentPort := range current.Spec.Ports {
			if servicePortMatches(desired.Spec.Ports[i], currentPort) {
				desired.Spec.Ports[i].NodePort = currentPort.NodePort
				break
			}
		}
	}
}

func servicePortMatches(a, b corev1.ServicePort) bool {
	if a.Name != "" && b.Name != "" {
		return a.Name == b.Name
	}
	return a.Port == b.Port && a.Protocol == b.Protocol
}

func Apply(ctx context.Context, c client.Client, obj client.Object) error {
	// Read current by name/ns to decide create vs update.
	key := client.ObjectKeyFromObject(obj)
	// The Get needs a typed receiver of the same kind — use a shallow copy of obj.
	existing, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("object does not implement client.Object")
	}
	err := c.Get(ctx, key, existing)
	if apierrors.IsNotFound(err) {
		if createErr := c.Create(ctx, obj); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return fmt.Errorf("create %s: %w", key, createErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get %s: %w", key, err)
	}
	if err := prepareForUpdate(existing, obj); err != nil {
		return err
	}
	if err := c.Update(ctx, obj); err != nil {
		return fmt.Errorf("update %s: %w", key, err)
	}
	return nil
}
