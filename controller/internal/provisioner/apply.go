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

// Apply upserts the object in cluster. For M3 we use a simple Get/Create/Update cycle
// rather than SSA — controller-runtime's SSA helper needs a FieldManager configured
// at manager level which we can add in M6 hardening.
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
	// Preserve resourceVersion so Update succeeds.
	obj.SetResourceVersion(existing.GetResourceVersion())
	if err := c.Update(ctx, obj); err != nil {
		return fmt.Errorf("update %s: %w", key, err)
	}
	return nil
}
