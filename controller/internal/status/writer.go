// Package status writes Gateway.status conditions back to the member cluster.
package status

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// MarkAccepted stamps Accepted=True on the Gateway and every listener.
// Retries on optimistic-concurrency conflicts: when multiple reconcilers
// (or a reconcile racing with an external status write) target the same
// Gateway, the loser of the resourceVersion race re-fetches and re-applies.
func MarkAccepted(ctx context.Context, c client.Client, gw *gwapiv1.Gateway) error {
	key := types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh gwapiv1.Gateway
		if err := c.Get(ctx, key, &fresh); err != nil {
			return err
		}
		now := metav1.Now()
		fresh.Status.Conditions = upsertCondition(fresh.Status.Conditions, metav1.Condition{
			Type:               string(gwapiv1.GatewayConditionAccepted),
			Status:             metav1.ConditionTrue,
			Reason:             string(gwapiv1.GatewayReasonAccepted),
			Message:            "Accepted by appnet-gateway.controller",
			LastTransitionTime: now,
			ObservedGeneration: fresh.Generation,
		})

		// Build listener statuses mirroring spec.listeners. Preserve any
		// existing listener conditions (e.g. Programmed) so we don't clobber
		// them with a fresh Accepted-only slice.
		existing := map[gwapiv1.SectionName]gwapiv1.ListenerStatus{}
		for _, l := range fresh.Status.Listeners {
			existing[l.Name] = l
		}
		listenerStatuses := make([]gwapiv1.ListenerStatus, 0, len(fresh.Spec.Listeners))
		for _, l := range fresh.Spec.Listeners {
			ls, ok := existing[l.Name]
			if !ok {
				ls = gwapiv1.ListenerStatus{
					Name:           l.Name,
					SupportedKinds: []gwapiv1.RouteGroupKind{{Kind: "HTTPRoute"}},
				}
			}
			ls.Conditions = upsertCondition(ls.Conditions, metav1.Condition{
				Type:               string(gwapiv1.ListenerConditionAccepted),
				Status:             metav1.ConditionTrue,
				Reason:             string(gwapiv1.ListenerReasonAccepted),
				Message:            "Accepted",
				LastTransitionTime: now,
				ObservedGeneration: fresh.Generation,
			})
			listenerStatuses = append(listenerStatuses, ls)
		}
		fresh.Status.Listeners = listenerStatuses

		return c.Status().Update(ctx, &fresh)
	})
}

// MarkProgrammed flips Programmed=True on the Gateway and on each listener.
// Called once the agentgateway Deployment for this Gateway has at least one
// available replica. Retries on conflict.
func MarkProgrammed(ctx context.Context, c client.Client, gw *gwapiv1.Gateway) error {
	key := types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh gwapiv1.Gateway
		if err := c.Get(ctx, key, &fresh); err != nil {
			return err
		}
		now := metav1.Now()
		fresh.Status.Conditions = upsertCondition(fresh.Status.Conditions, metav1.Condition{
			Type:               string(gwapiv1.GatewayConditionProgrammed),
			Status:             metav1.ConditionTrue,
			Reason:             string(gwapiv1.GatewayReasonProgrammed),
			Message:            "agentgateway data plane ready",
			LastTransitionTime: now,
			ObservedGeneration: fresh.Generation,
		})
		for i := range fresh.Status.Listeners {
			fresh.Status.Listeners[i].Conditions = upsertCondition(
				fresh.Status.Listeners[i].Conditions,
				metav1.Condition{
					Type:               string(gwapiv1.ListenerConditionProgrammed),
					Status:             metav1.ConditionTrue,
					Reason:             string(gwapiv1.ListenerReasonProgrammed),
					Message:            "Programmed",
					LastTransitionTime: now,
					ObservedGeneration: fresh.Generation,
				},
			)
		}
		return c.Status().Update(ctx, &fresh)
	})
}

func upsertCondition(existing []metav1.Condition, newC metav1.Condition) []metav1.Condition {
	for i, c := range existing {
		if c.Type == newC.Type {
			if c.Status == newC.Status && c.Reason == newC.Reason && c.Message == newC.Message {
				// No change; keep old LastTransitionTime.
				existing[i].ObservedGeneration = newC.ObservedGeneration
				return existing
			}
			existing[i] = newC
			return existing
		}
	}
	return append(existing, newC)
}
