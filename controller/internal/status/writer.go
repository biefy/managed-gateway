// Package status writes Gateway.status conditions back to the member cluster.
package status

import (
	"context"
	"reflect"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/pkg/features"
)

func MarkGatewayClassAccepted(ctx context.Context, c client.Client, gc *gwapiv1.GatewayClass) error {
	key := types.NamespacedName{Name: gc.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh gwapiv1.GatewayClass
		if err := c.Get(ctx, key, &fresh); err != nil {
			return err
		}
		beforeConditions := append([]metav1.Condition(nil), fresh.Status.Conditions...)
		beforeFeatures := append([]gwapiv1.SupportedFeature(nil), fresh.Status.SupportedFeatures...)
		now := metav1.Now()
		fresh.Status.Conditions = upsertCondition(fresh.Status.Conditions, metav1.Condition{
			Type:               string(gwapiv1.GatewayClassConditionStatusAccepted),
			Status:             metav1.ConditionTrue,
			Reason:             string(gwapiv1.GatewayClassReasonAccepted),
			Message:            "Accepted by appnet-gateway.controller",
			LastTransitionTime: now,
			ObservedGeneration: fresh.Generation,
		})
		fresh.Status.SupportedFeatures = supportedFeatures()
		if reflect.DeepEqual(beforeConditions, fresh.Status.Conditions) && reflect.DeepEqual(beforeFeatures, fresh.Status.SupportedFeatures) {
			return nil
		}
		return c.Status().Update(ctx, &fresh)
	})
}

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
		fresh.Status.Conditions = upsertCondition(fresh.Status.Conditions, metav1.Condition{
			Type:               string(gwapiv1.GatewayConditionProgrammed),
			Status:             metav1.ConditionUnknown,
			Reason:             string(gwapiv1.GatewayReasonPending),
			Message:            "Waiting for agentgateway data plane",
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
				ls = gwapiv1.ListenerStatus{Name: l.Name}
			}
			ls.SupportedKinds = supportedKindsForProtocol(l.Protocol)
			ls.Conditions = upsertCondition(ls.Conditions, metav1.Condition{
				Type:               string(gwapiv1.ListenerConditionAccepted),
				Status:             metav1.ConditionTrue,
				Reason:             string(gwapiv1.ListenerReasonAccepted),
				Message:            "Accepted",
				LastTransitionTime: now,
				ObservedGeneration: fresh.Generation,
			})
			ls.Conditions = upsertCondition(ls.Conditions, metav1.Condition{
				Type:               string(gwapiv1.ListenerConditionProgrammed),
				Status:             metav1.ConditionUnknown,
				Reason:             string(gwapiv1.ListenerReasonPending),
				Message:            "Waiting for agentgateway data plane",
				LastTransitionTime: now,
				ObservedGeneration: fresh.Generation,
			})
			ls.Conditions = upsertCondition(ls.Conditions, metav1.Condition{
				Type:               string(gwapiv1.ListenerConditionResolvedRefs),
				Status:             metav1.ConditionTrue,
				Reason:             string(gwapiv1.ListenerReasonResolvedRefs),
				Message:            "Resolved listener references",
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
// Called once the agentgateway Deployment is available and its Service has an address.
// Retries on conflict.
func MarkProgrammed(ctx context.Context, c client.Client, gw *gwapiv1.Gateway, addresses ...gwapiv1.GatewayStatusAddress) error {
	key := types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh gwapiv1.Gateway
		if err := c.Get(ctx, key, &fresh); err != nil {
			return err
		}
		now := metav1.Now()
		if len(addresses) > 0 {
			fresh.Status.Addresses = append([]gwapiv1.GatewayStatusAddress(nil), addresses...)
		}
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

func MarkRoutesAccepted(ctx context.Context, c client.Client, gw *gwapiv1.Gateway) error {
	if err := markHTTPRoutesAccepted(ctx, c, gw); err != nil {
		return err
	}
	if err := markGRPCRoutesAccepted(ctx, c, gw); err != nil {
		return err
	}
	return markTLSRoutesAccepted(ctx, c, gw)
}

func markHTTPRoutesAccepted(ctx context.Context, c client.Client, gw *gwapiv1.Gateway) error {
	var routes gwapiv1.HTTPRouteList
	if err := c.List(ctx, &routes); err != nil {
		return err
	}
	for _, route := range routes.Items {
		key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var fresh gwapiv1.HTTPRoute
			if err := c.Get(ctx, key, &fresh); err != nil {
				return err
			}
			parents := routeParentStatuses(fresh.Spec.ParentRefs, fresh.Namespace, fresh.Generation, gw, routeKindHTTP)
			updated := mergeRouteParents(fresh.Status.Parents, gw, parents)
			if reflect.DeepEqual(fresh.Status.Parents, updated) {
				return nil
			}
			fresh.Status.Parents = updated
			return c.Status().Update(ctx, &fresh)
		}); err != nil {
			return err
		}
	}
	return nil
}

func markGRPCRoutesAccepted(ctx context.Context, c client.Client, gw *gwapiv1.Gateway) error {
	var routes gwapiv1.GRPCRouteList
	if err := c.List(ctx, &routes); err != nil {
		return err
	}
	for _, route := range routes.Items {
		key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var fresh gwapiv1.GRPCRoute
			if err := c.Get(ctx, key, &fresh); err != nil {
				return err
			}
			parents := routeParentStatuses(fresh.Spec.ParentRefs, fresh.Namespace, fresh.Generation, gw, routeKindGRPC)
			updated := mergeRouteParents(fresh.Status.Parents, gw, parents)
			if reflect.DeepEqual(fresh.Status.Parents, updated) {
				return nil
			}
			fresh.Status.Parents = updated
			return c.Status().Update(ctx, &fresh)
		}); err != nil {
			return err
		}
	}
	return nil
}

func markTLSRoutesAccepted(ctx context.Context, c client.Client, gw *gwapiv1.Gateway) error {
	var routes gwapiv1.TLSRouteList
	if err := c.List(ctx, &routes); err != nil {
		return err
	}
	for _, route := range routes.Items {
		key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var fresh gwapiv1.TLSRoute
			if err := c.Get(ctx, key, &fresh); err != nil {
				return err
			}
			parents := routeParentStatuses(fresh.Spec.ParentRefs, fresh.Namespace, fresh.Generation, gw, routeKindTLS)
			updated := mergeRouteParents(fresh.Status.Parents, gw, parents)
			if reflect.DeepEqual(fresh.Status.Parents, updated) {
				return nil
			}
			fresh.Status.Parents = updated
			return c.Status().Update(ctx, &fresh)
		}); err != nil {
			return err
		}
	}
	return nil
}

type routeKind string

const (
	routeKindHTTP routeKind = "HTTPRoute"
	routeKindGRPC routeKind = "GRPCRoute"
	routeKindTLS  routeKind = "TLSRoute"
)

func routeParentStatuses(parentRefs []gwapiv1.ParentReference, routeNamespace string, routeGeneration int64, gw *gwapiv1.Gateway, kind routeKind) []gwapiv1.RouteParentStatus {
	var parents []gwapiv1.RouteParentStatus
	now := metav1.Now()
	for _, parentRef := range parentRefs {
		if !parentRefMatchesGateway(parentRef, routeNamespace, gw) || !gatewayHasMatchingListener(gw, parentRef, kind) {
			continue
		}
		parents = append(parents, gwapiv1.RouteParentStatus{
			ParentRef:      normalizedGatewayParentRef(parentRef, gw.Namespace),
			ControllerName: gwapiv1.GatewayController("gateway.azure.com/appnet-gateway.controller"),
			Conditions: []metav1.Condition{
				{
					Type:               string(gwapiv1.RouteConditionAccepted),
					Status:             metav1.ConditionTrue,
					Reason:             string(gwapiv1.RouteReasonAccepted),
					Message:            "Accepted",
					LastTransitionTime: now,
					ObservedGeneration: routeGeneration,
				},
				{
					Type:               string(gwapiv1.RouteConditionResolvedRefs),
					Status:             metav1.ConditionTrue,
					Reason:             string(gwapiv1.RouteReasonResolvedRefs),
					Message:            "Resolved route references",
					LastTransitionTime: now,
					ObservedGeneration: routeGeneration,
				},
			},
		})
	}
	return parents
}

func parentRefMatchesGateway(parentRef gwapiv1.ParentReference, routeNamespace string, gw *gwapiv1.Gateway) bool {
	group := gwapiv1.Group(gwapiv1.GroupVersion.Group)
	if parentRef.Group != nil {
		group = *parentRef.Group
	}
	kind := gwapiv1.Kind("Gateway")
	if parentRef.Kind != nil {
		kind = *parentRef.Kind
	}
	namespace := gwapiv1.Namespace(routeNamespace)
	if parentRef.Namespace != nil {
		namespace = *parentRef.Namespace
	}
	return group == gwapiv1.Group(gwapiv1.GroupVersion.Group) && kind == "Gateway" && namespace == gwapiv1.Namespace(gw.Namespace) && parentRef.Name == gwapiv1.ObjectName(gw.Name)
}

func gatewayHasMatchingListener(gw *gwapiv1.Gateway, parentRef gwapiv1.ParentReference, kind routeKind) bool {
	for _, listener := range gw.Spec.Listeners {
		if parentRef.SectionName != nil && *parentRef.SectionName != listener.Name {
			continue
		}
		if parentRef.Port != nil && *parentRef.Port != listener.Port {
			continue
		}
		if listenerSupportsRouteKind(listener.Protocol, kind) {
			return true
		}
	}
	return false
}

func listenerSupportsRouteKind(protocol gwapiv1.ProtocolType, kind routeKind) bool {
	switch kind {
	case routeKindHTTP, routeKindGRPC:
		return protocol == gwapiv1.HTTPProtocolType || protocol == gwapiv1.HTTPSProtocolType
	case routeKindTLS:
		return protocol == gwapiv1.TLSProtocolType
	default:
		return false
	}
}

func normalizedGatewayParentRef(parentRef gwapiv1.ParentReference, namespace string) gwapiv1.ParentReference {
	group := gwapiv1.Group(gwapiv1.GroupVersion.Group)
	kind := gwapiv1.Kind("Gateway")
	ns := gwapiv1.Namespace(namespace)
	parentRef.Group = &group
	parentRef.Kind = &kind
	parentRef.Namespace = &ns
	return parentRef
}

func mergeRouteParents(existing []gwapiv1.RouteParentStatus, gw *gwapiv1.Gateway, parents []gwapiv1.RouteParentStatus) []gwapiv1.RouteParentStatus {
	for i := range parents {
		for _, oldParent := range existing {
			if oldParent.ControllerName == parents[i].ControllerName && reflect.DeepEqual(oldParent.ParentRef, parents[i].ParentRef) {
				conditions := append([]metav1.Condition(nil), oldParent.Conditions...)
				for _, condition := range parents[i].Conditions {
					conditions = upsertCondition(conditions, condition)
				}
				parents[i].Conditions = conditions
				break
			}
		}
	}

	out := make([]gwapiv1.RouteParentStatus, 0, len(existing)+len(parents))
	for _, parent := range existing {
		if parent.ControllerName == gwapiv1.GatewayController("gateway.azure.com/appnet-gateway.controller") && parentStatusReferencesGateway(parent, gw) {
			continue
		}
		out = append(out, parent)
	}
	return append(out, parents...)
}

func parentStatusReferencesGateway(parent gwapiv1.RouteParentStatus, gw *gwapiv1.Gateway) bool {
	ref := parent.ParentRef
	group := gwapiv1.Group(gwapiv1.GroupVersion.Group)
	if ref.Group != nil {
		group = *ref.Group
	}
	kind := gwapiv1.Kind("Gateway")
	if ref.Kind != nil {
		kind = *ref.Kind
	}
	namespace := gwapiv1.Namespace(gw.Namespace)
	if ref.Namespace != nil {
		namespace = *ref.Namespace
	}
	return group == gwapiv1.Group(gwapiv1.GroupVersion.Group) && kind == "Gateway" && namespace == gwapiv1.Namespace(gw.Namespace) && ref.Name == gwapiv1.ObjectName(gw.Name)
}

func supportedKindsForProtocol(protocol gwapiv1.ProtocolType) []gwapiv1.RouteGroupKind {
	switch protocol {
	case gwapiv1.HTTPProtocolType, gwapiv1.HTTPSProtocolType:
		return []gwapiv1.RouteGroupKind{{Kind: "HTTPRoute"}, {Kind: "GRPCRoute"}}
	case gwapiv1.TLSProtocolType:
		return []gwapiv1.RouteGroupKind{{Kind: "TLSRoute"}}
	default:
		return nil
	}
}

func supportedFeatures() []gwapiv1.SupportedFeature {
	names := []gwapiv1.FeatureName{
		gwapiv1.FeatureName(features.SupportGRPCRoute),
		gwapiv1.FeatureName(features.SupportGateway),
		gwapiv1.FeatureName(features.SupportHTTPRoute),
		gwapiv1.FeatureName(features.SupportReferenceGrant),
		gwapiv1.FeatureName(features.SupportTLSRoute),
	}
	slices.Sort(names)
	out := make([]gwapiv1.SupportedFeature, 0, len(names))
	for _, name := range names {
		out = append(out, gwapiv1.SupportedFeature{Name: name})
	}
	return out
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
