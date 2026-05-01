// Package status writes Gateway.status conditions back to the member cluster.
package status

import (
	"context"
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
		before := fresh.Status.DeepCopy()
		now := metav1.Now()
		fresh.Status.Conditions = upsertCondition(fresh.Status.Conditions, metav1.Condition{
			Type:               string(gwapiv1.GatewayConditionAccepted),
			Status:             metav1.ConditionTrue,
			Reason:             string(gwapiv1.GatewayReasonAccepted),
			Message:            "Accepted by appnet-gateway.controller",
			LastTransitionTime: now,
			ObservedGeneration: fresh.Generation,
		})
		programmed := conditionByType(fresh.Status.Conditions, string(gwapiv1.GatewayConditionProgrammed))
		if programmed == nil || programmed.Status != metav1.ConditionTrue || programmed.ObservedGeneration != fresh.Generation {
			fresh.Status.Conditions = upsertCondition(fresh.Status.Conditions, metav1.Condition{
				Type:               string(gwapiv1.GatewayConditionProgrammed),
				Status:             metav1.ConditionUnknown,
				Reason:             string(gwapiv1.GatewayReasonPending),
				Message:            "Waiting for agentgateway data plane",
				LastTransitionTime: now,
				ObservedGeneration: fresh.Generation,
			})
		}

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
			programmed := conditionByType(ls.Conditions, string(gwapiv1.ListenerConditionProgrammed))
			if programmed == nil || programmed.Status != metav1.ConditionTrue || programmed.ObservedGeneration != fresh.Generation {
				ls.Conditions = upsertCondition(ls.Conditions, metav1.Condition{
					Type:               string(gwapiv1.ListenerConditionProgrammed),
					Status:             metav1.ConditionUnknown,
					Reason:             string(gwapiv1.ListenerReasonPending),
					Message:            "Waiting for agentgateway data plane",
					LastTransitionTime: now,
					ObservedGeneration: fresh.Generation,
				})
			}
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
		if reflect.DeepEqual(*before, fresh.Status) {
			return nil
		}
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
		before := fresh.Status.DeepCopy()
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
		if reflect.DeepEqual(*before, fresh.Status) {
			return nil
		}
		return c.Status().Update(ctx, &fresh)
	})
}

func MarkListenerResolvedRefs(ctx context.Context, c client.Client, gw *gwapiv1.Gateway, name gwapiv1.SectionName, conditionStatus metav1.ConditionStatus, reason gwapiv1.ListenerConditionReason, message string) error {
	key := types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh gwapiv1.Gateway
		if err := c.Get(ctx, key, &fresh); err != nil {
			return err
		}
		before := fresh.Status.DeepCopy()
		now := metav1.Now()
		for i := range fresh.Status.Listeners {
			if fresh.Status.Listeners[i].Name != name {
				continue
			}
			fresh.Status.Listeners[i].Conditions = upsertCondition(fresh.Status.Listeners[i].Conditions, metav1.Condition{
				Type:               string(gwapiv1.ListenerConditionResolvedRefs),
				Status:             conditionStatus,
				Reason:             string(reason),
				Message:            message,
				LastTransitionTime: now,
				ObservedGeneration: fresh.Generation,
			})
			break
		}
		if reflect.DeepEqual(*before, fresh.Status) {
			return nil
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
			parents, err := routeParentStatuses(ctx, c, fresh.Spec.ParentRefs, fresh.Namespace, fresh.Generation, fresh.Spec.Hostnames, gw, routeKindHTTP, httpBackendObjectRefs(fresh.Spec.Rules))
			if err != nil {
				return err
			}
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
			parents, err := routeParentStatuses(ctx, c, fresh.Spec.ParentRefs, fresh.Namespace, fresh.Generation, fresh.Spec.Hostnames, gw, routeKindGRPC, grpcBackendObjectRefs(fresh.Spec.Rules))
			if err != nil {
				return err
			}
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
			parents, err := routeParentStatuses(ctx, c, fresh.Spec.ParentRefs, fresh.Namespace, fresh.Generation, fresh.Spec.Hostnames, gw, routeKindTLS, tlsBackendObjectRefs(fresh.Spec.Rules))
			if err != nil {
				return err
			}
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

func routeParentStatuses(ctx context.Context, c client.Client, parentRefs []gwapiv1.ParentReference, routeNamespace string, routeGeneration int64, routeHostnames []gwapiv1.Hostname, gw *gwapiv1.Gateway, kind routeKind, backendRefs []gwapiv1.BackendObjectReference) ([]gwapiv1.RouteParentStatus, error) {
	var parents []gwapiv1.RouteParentStatus
	now := metav1.Now()
	resolvedStatus, resolvedReason, resolvedMessage, err := backendRefsResolved(ctx, c, routeNamespace, kind, backendRefs)
	if err != nil {
		return nil, err
	}
	for _, parentRef := range parentRefs {
		if !parentRefMatchesGateway(parentRef, routeNamespace, gw) {
			continue
		}
		acceptedStatus, acceptedReason, acceptedMessage, err := routeAcceptedByGateway(ctx, c, parentRef, routeNamespace, routeHostnames, gw, kind)
		if err != nil {
			return nil, err
		}
		parents = append(parents, gwapiv1.RouteParentStatus{
			ParentRef:      normalizedGatewayParentRef(parentRef, gw.Namespace),
			ControllerName: gwapiv1.GatewayController("gateway.azure.com/appnet-gateway.controller"),
			Conditions: []metav1.Condition{
				{
					Type:               string(gwapiv1.RouteConditionAccepted),
					Status:             acceptedStatus,
					Reason:             string(acceptedReason),
					Message:            acceptedMessage,
					LastTransitionTime: now,
					ObservedGeneration: routeGeneration,
				},
				{
					Type:               string(gwapiv1.RouteConditionResolvedRefs),
					Status:             resolvedStatus,
					Reason:             string(resolvedReason),
					Message:            resolvedMessage,
					LastTransitionTime: now,
					ObservedGeneration: routeGeneration,
				},
			},
		})
	}
	return parents, nil
}

func routeAcceptedByGateway(ctx context.Context, c client.Client, parentRef gwapiv1.ParentReference, routeNamespace string, routeHostnames []gwapiv1.Hostname, gw *gwapiv1.Gateway, kind routeKind) (metav1.ConditionStatus, gwapiv1.RouteConditionReason, string, error) {
	matchedListener := false
	hostnameMismatch := false
	for _, listener := range gw.Spec.Listeners {
		if parentRef.SectionName != nil && *parentRef.SectionName != listener.Name {
			continue
		}
		if parentRef.Port != nil && *parentRef.Port != listener.Port {
			continue
		}
		if !listenerSupportsRouteKind(listener.Protocol, kind) {
			continue
		}
		matchedListener = true
		if !routeHostnamesIntersect(listener.Hostname, routeHostnames) {
			hostnameMismatch = true
			continue
		}
		allowed, err := listenerAllowsRoute(ctx, c, listener, gw.Namespace, routeNamespace, kind)
		if err != nil {
			return metav1.ConditionFalse, gwapiv1.RouteReasonPending, "Unable to evaluate listener route policy", err
		}
		if allowed {
			return metav1.ConditionTrue, gwapiv1.RouteReasonAccepted, "Accepted", nil
		}
	}
	if !matchedListener {
		return metav1.ConditionFalse, gwapiv1.RouteReasonNoMatchingParent, "No matching listener for parentRef", nil
	}
	if hostnameMismatch {
		return metav1.ConditionFalse, gwapiv1.RouteReasonNoMatchingListenerHostname, "Route hostnames do not intersect listener hostname", nil
	}
	return metav1.ConditionFalse, gwapiv1.RouteReasonNotAllowedByListeners, "Route is not allowed by listener allowedRoutes", nil
}

func listenerAllowsRoute(ctx context.Context, c client.Client, listener gwapiv1.Listener, gatewayNamespace, routeNamespace string, kind routeKind) (bool, error) {
	if listener.AllowedRoutes != nil && len(listener.AllowedRoutes.Kinds) > 0 && !allowedRouteKindsInclude(listener.AllowedRoutes.Kinds, kind) {
		return false, nil
	}
	if listener.AllowedRoutes == nil || listener.AllowedRoutes.Namespaces == nil || listener.AllowedRoutes.Namespaces.From == nil {
		return routeNamespace == gatewayNamespace, nil
	}
	switch *listener.AllowedRoutes.Namespaces.From {
	case gwapiv1.NamespacesFromAll:
		return true, nil
	case gwapiv1.NamespacesFromSame:
		return routeNamespace == gatewayNamespace, nil
	case gwapiv1.NamespacesFromSelector:
		if listener.AllowedRoutes.Namespaces.Selector == nil {
			return false, nil
		}
		selector, err := metav1.LabelSelectorAsSelector(listener.AllowedRoutes.Namespaces.Selector)
		if err != nil {
			return false, err
		}
		var ns corev1.Namespace
		if err := c.Get(ctx, types.NamespacedName{Name: routeNamespace}, &ns); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return selector.Matches(labels.Set(ns.Labels)), nil
	default:
		return false, nil
	}
}

func allowedRouteKindsInclude(kinds []gwapiv1.RouteGroupKind, kind routeKind) bool {
	for _, allowed := range kinds {
		group := gwapiv1.Group(gwapiv1.GroupVersion.Group)
		if allowed.Group != nil {
			group = *allowed.Group
		}
		if group == gwapiv1.Group(gwapiv1.GroupVersion.Group) && allowed.Kind == gwapiv1.Kind(kind) {
			return true
		}
	}
	return false
}

func routeHostnamesIntersect(listenerHostname *gwapiv1.Hostname, routeHostnames []gwapiv1.Hostname) bool {
	if listenerHostname == nil || len(routeHostnames) == 0 {
		return true
	}
	for _, routeHostname := range routeHostnames {
		if hostnamesIntersect(string(*listenerHostname), string(routeHostname)) {
			return true
		}
	}
	return false
}

func hostnamesIntersect(a, b string) bool {
	if a == b || a == "" || b == "" {
		return true
	}
	if strings.HasPrefix(a, "*.") && strings.HasSuffix(b, strings.TrimPrefix(a, "*")) && b != strings.TrimPrefix(a, "*.") {
		return true
	}
	if strings.HasPrefix(b, "*.") && strings.HasSuffix(a, strings.TrimPrefix(b, "*")) && a != strings.TrimPrefix(b, "*.") {
		return true
	}
	return false
}

func backendRefsResolved(ctx context.Context, c client.Client, routeNamespace string, routeKind routeKind, backendRefs []gwapiv1.BackendObjectReference) (metav1.ConditionStatus, gwapiv1.RouteConditionReason, string, error) {
	for _, ref := range backendRefs {
		group := gwapiv1.Group("")
		if ref.Group != nil {
			group = *ref.Group
		}
		kind := gwapiv1.Kind("Service")
		if ref.Kind != nil {
			kind = *ref.Kind
		}
		if group != "" || kind != "Service" {
			return metav1.ConditionFalse, gwapiv1.RouteReasonInvalidKind, "Only core Service backendRefs are supported", nil
		}
		namespace := gwapiv1.Namespace(routeNamespace)
		if ref.Namespace != nil {
			namespace = *ref.Namespace
		}
		if !referencePermitted(ctx, c, gwapiv1.Group(gwapiv1.GroupVersion.Group), gwapiv1.Kind(routeKind), gwapiv1.Namespace(routeNamespace), "", "Service", namespace, ref.Name) {
			return metav1.ConditionFalse, gwapiv1.RouteReasonRefNotPermitted, "Cross-namespace backendRef is not permitted", nil
		}
		if ref.Port == nil {
			return metav1.ConditionFalse, gwapiv1.RouteReasonBackendNotFound, "Service backendRef must include a port", nil
		}
		var svc corev1.Service
		if err := c.Get(ctx, types.NamespacedName{Namespace: string(namespace), Name: string(ref.Name)}, &svc); err != nil {
			if apierrors.IsNotFound(err) {
				return metav1.ConditionFalse, gwapiv1.RouteReasonBackendNotFound, "Referenced Service was not found", nil
			}
			return metav1.ConditionFalse, gwapiv1.RouteReasonPending, "Unable to resolve backendRef", err
		}
		if !serviceHasPort(&svc, *ref.Port) {
			return metav1.ConditionFalse, gwapiv1.RouteReasonBackendNotFound, "Referenced Service port was not found", nil
		}
	}
	return metav1.ConditionTrue, gwapiv1.RouteReasonResolvedRefs, "Resolved route references", nil
}

func serviceHasPort(svc *corev1.Service, port gwapiv1.PortNumber) bool {
	for _, svcPort := range svc.Spec.Ports {
		if svcPort.Port == int32(port) {
			return true
		}
	}
	return false
}

func referencePermitted(ctx context.Context, c client.Client, fromGroup gwapiv1.Group, fromKind gwapiv1.Kind, fromNamespace gwapiv1.Namespace, toGroup gwapiv1.Group, toKind gwapiv1.Kind, toNamespace gwapiv1.Namespace, toName gwapiv1.ObjectName) bool {
	if fromNamespace == toNamespace {
		return true
	}
	var grants gwapiv1.ReferenceGrantList
	if err := c.List(ctx, &grants, client.InNamespace(string(toNamespace))); err != nil {
		return false
	}
	for _, grant := range grants.Items {
		if referenceGrantAllows(grant, fromGroup, fromKind, fromNamespace, toGroup, toKind, toName) {
			return true
		}
	}
	return false
}

func referenceGrantAllows(grant gwapiv1.ReferenceGrant, fromGroup gwapiv1.Group, fromKind gwapiv1.Kind, fromNamespace gwapiv1.Namespace, toGroup gwapiv1.Group, toKind gwapiv1.Kind, toName gwapiv1.ObjectName) bool {
	fromAllowed := false
	for _, from := range grant.Spec.From {
		if from.Group == fromGroup && from.Kind == fromKind && from.Namespace == fromNamespace {
			fromAllowed = true
			break
		}
	}
	if !fromAllowed {
		return false
	}
	for _, to := range grant.Spec.To {
		if to.Group == toGroup && to.Kind == toKind && (to.Name == nil || *to.Name == toName) {
			return true
		}
	}
	return false
}

func httpBackendObjectRefs(rules []gwapiv1.HTTPRouteRule) []gwapiv1.BackendObjectReference {
	var out []gwapiv1.BackendObjectReference
	for _, rule := range rules {
		for _, ref := range rule.BackendRefs {
			out = append(out, ref.BackendObjectReference)
		}
	}
	return out
}

func grpcBackendObjectRefs(rules []gwapiv1.GRPCRouteRule) []gwapiv1.BackendObjectReference {
	var out []gwapiv1.BackendObjectReference
	for _, rule := range rules {
		for _, ref := range rule.BackendRefs {
			out = append(out, ref.BackendObjectReference)
		}
	}
	return out
}

func tlsBackendObjectRefs(rules []gwapiv1.TLSRouteRule) []gwapiv1.BackendObjectReference {
	var out []gwapiv1.BackendObjectReference
	for _, rule := range rules {
		for _, ref := range rule.BackendRefs {
			out = append(out, ref.BackendObjectReference)
		}
	}
	return out
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

func conditionByType(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
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
