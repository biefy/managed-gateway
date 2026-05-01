package status

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestMarkGatewayClassAcceptedSetsStatus(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := gwapiv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	gc := &gwapiv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "appnet", Generation: 2},
		Spec:       gwapiv1.GatewayClassSpec{ControllerName: "gateway.azure.com/appnet-gateway.controller"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gc).WithStatusSubresource(&gwapiv1.GatewayClass{}).Build()

	if err := MarkGatewayClassAccepted(ctx, c, gc); err != nil {
		t.Fatal(err)
	}

	var got gwapiv1.GatewayClass
	if err := c.Get(ctx, clientKey("", "appnet"), &got); err != nil {
		t.Fatal(err)
	}
	accepted := condition(got.Status.Conditions, string(gwapiv1.GatewayClassConditionStatusAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionTrue || accepted.ObservedGeneration != 2 {
		t.Fatalf("GatewayClass Accepted condition = %#v", accepted)
	}
	wantFeatures := []gwapiv1.SupportedFeature{
		{Name: "GRPCRoute"},
		{Name: "Gateway"},
		{Name: "HTTPRoute"},
		{Name: "ReferenceGrant"},
		{Name: "TLSRoute"},
	}
	if !supportedFeaturesEqual(got.Status.SupportedFeatures, wantFeatures) {
		t.Fatalf("supported features = %#v, want %#v", got.Status.SupportedFeatures, wantFeatures)
	}
}

func TestMarkAcceptedSetsSupportedKinds(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := gwapiv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app", Generation: 3},
		Spec: gwapiv1.GatewaySpec{Listeners: []gwapiv1.Listener{
			{Name: "http", Port: 80, Protocol: gwapiv1.HTTPProtocolType},
			{Name: "https", Port: 443, Protocol: gwapiv1.HTTPSProtocolType},
			{Name: "tls", Port: 15443, Protocol: gwapiv1.TLSProtocolType},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gw).WithStatusSubresource(&gwapiv1.Gateway{}).Build()

	if err := MarkAccepted(ctx, c, gw); err != nil {
		t.Fatal(err)
	}

	var got gwapiv1.Gateway
	if err := c.Get(ctx, clientKey("app", "gw"), &got); err != nil {
		t.Fatal(err)
	}
	if conditionStatus(got.Status.Conditions, string(gwapiv1.GatewayConditionAccepted)) != metav1.ConditionTrue {
		t.Fatalf("gateway Accepted condition not true: %#v", got.Status.Conditions)
	}
	programmed := condition(got.Status.Conditions, string(gwapiv1.GatewayConditionProgrammed))
	if programmed == nil || programmed.Status != metav1.ConditionUnknown || programmed.ObservedGeneration != 3 {
		t.Fatalf("gateway Programmed condition = %#v", programmed)
	}
	if len(got.Status.Listeners) != 3 {
		t.Fatalf("got %d listener statuses, want 3", len(got.Status.Listeners))
	}
	want := map[gwapiv1.SectionName][]gwapiv1.RouteGroupKind{
		"http":  {{Kind: "HTTPRoute"}, {Kind: "GRPCRoute"}},
		"https": {{Kind: "HTTPRoute"}, {Kind: "GRPCRoute"}},
		"tls":   {{Kind: "TLSRoute"}},
	}
	for _, listener := range got.Status.Listeners {
		if conditionStatus(listener.Conditions, string(gwapiv1.ListenerConditionAccepted)) != metav1.ConditionTrue {
			t.Fatalf("listener %s Accepted condition not true: %#v", listener.Name, listener.Conditions)
		}
		programmed := condition(listener.Conditions, string(gwapiv1.ListenerConditionProgrammed))
		if programmed == nil || programmed.Status != metav1.ConditionUnknown || programmed.ObservedGeneration != 3 {
			t.Fatalf("listener %s Programmed condition = %#v", listener.Name, programmed)
		}
		resolvedRefs := condition(listener.Conditions, string(gwapiv1.ListenerConditionResolvedRefs))
		if resolvedRefs == nil || resolvedRefs.Status != metav1.ConditionTrue || resolvedRefs.ObservedGeneration != 3 {
			t.Fatalf("listener %s ResolvedRefs condition = %#v", listener.Name, resolvedRefs)
		}
		if !routeKindsEqual(listener.SupportedKinds, want[listener.Name]) {
			t.Fatalf("listener %s supported kinds = %#v, want %#v", listener.Name, listener.SupportedKinds, want[listener.Name])
		}
	}
}

func TestMarkListenerResolvedRefsSetsCondition(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := gwapiv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app", Generation: 1},
		Spec:       gwapiv1.GatewaySpec{Listeners: []gwapiv1.Listener{{Name: "https", Port: 443, Protocol: gwapiv1.HTTPSProtocolType}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gw).WithStatusSubresource(&gwapiv1.Gateway{}).Build()
	if err := MarkAccepted(ctx, c, gw); err != nil {
		t.Fatal(err)
	}

	if err := MarkListenerResolvedRefs(ctx, c, gw, "https", metav1.ConditionFalse, gwapiv1.ListenerReasonInvalidCertificateRef, "missing certificate"); err != nil {
		t.Fatal(err)
	}

	var got gwapiv1.Gateway
	if err := c.Get(ctx, clientKey("app", "gw"), &got); err != nil {
		t.Fatal(err)
	}
	resolvedRefs := condition(got.Status.Listeners[0].Conditions, string(gwapiv1.ListenerConditionResolvedRefs))
	if resolvedRefs == nil || resolvedRefs.Status != metav1.ConditionFalse || resolvedRefs.Reason != string(gwapiv1.ListenerReasonInvalidCertificateRef) {
		t.Fatalf("listener ResolvedRefs condition = %#v", resolvedRefs)
	}
}

func TestMarkProgrammedPreservesAccepted(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := gwapiv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app", Generation: 1},
		Spec:       gwapiv1.GatewaySpec{Listeners: []gwapiv1.Listener{{Name: "http", Port: 80, Protocol: gwapiv1.HTTPProtocolType}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gw).WithStatusSubresource(&gwapiv1.Gateway{}).Build()

	if err := MarkAccepted(ctx, c, gw); err != nil {
		t.Fatal(err)
	}
	ipType := gwapiv1.IPAddressType
	if err := MarkProgrammed(ctx, c, gw, gwapiv1.GatewayStatusAddress{Type: &ipType, Value: "1.2.3.4"}); err != nil {
		t.Fatal(err)
	}

	var got gwapiv1.Gateway
	if err := c.Get(ctx, clientKey("app", "gw"), &got); err != nil {
		t.Fatal(err)
	}
	if conditionStatus(got.Status.Conditions, string(gwapiv1.GatewayConditionAccepted)) != metav1.ConditionTrue {
		t.Fatalf("gateway Accepted condition not preserved: %#v", got.Status.Conditions)
	}
	if conditionStatus(got.Status.Conditions, string(gwapiv1.GatewayConditionProgrammed)) != metav1.ConditionTrue {
		t.Fatalf("gateway Programmed condition not true: %#v", got.Status.Conditions)
	}
	if len(got.Status.Addresses) != 1 || got.Status.Addresses[0].Value != "1.2.3.4" || got.Status.Addresses[0].Type == nil || *got.Status.Addresses[0].Type != gwapiv1.IPAddressType {
		t.Fatalf("gateway addresses = %#v", got.Status.Addresses)
	}
	if len(got.Status.Listeners) != 1 {
		t.Fatalf("got %d listener statuses, want 1", len(got.Status.Listeners))
	}
	listener := got.Status.Listeners[0]
	if conditionStatus(listener.Conditions, string(gwapiv1.ListenerConditionAccepted)) != metav1.ConditionTrue {
		t.Fatalf("listener Accepted condition not preserved: %#v", listener.Conditions)
	}
	if conditionStatus(listener.Conditions, string(gwapiv1.ListenerConditionProgrammed)) != metav1.ConditionTrue {
		t.Fatalf("listener Programmed condition not true: %#v", listener.Conditions)
	}
	if err := MarkAccepted(ctx, c, gw); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, clientKey("app", "gw"), &got); err != nil {
		t.Fatal(err)
	}
	if conditionStatus(got.Status.Conditions, string(gwapiv1.GatewayConditionProgrammed)) != metav1.ConditionTrue {
		t.Fatalf("gateway Programmed condition downgraded: %#v", got.Status.Conditions)
	}
	if conditionStatus(got.Status.Listeners[0].Conditions, string(gwapiv1.ListenerConditionProgrammed)) != metav1.ConditionTrue {
		t.Fatalf("listener Programmed condition downgraded: %#v", got.Status.Listeners[0].Conditions)
	}
}

func TestMarkRoutesAcceptedSetsHTTPRouteParent(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := gwapiv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app", Generation: 5},
		Spec:       gwapiv1.GatewaySpec{Listeners: []gwapiv1.Listener{{Name: "http", Port: 80, Protocol: gwapiv1.HTTPProtocolType}}},
	}
	route := &gwapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "app", Generation: 7},
		Spec: gwapiv1.HTTPRouteSpec{CommonRouteSpec: gwapiv1.CommonRouteSpec{ParentRefs: []gwapiv1.ParentReference{
			{Name: "gw"},
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gw, route).WithStatusSubresource(&gwapiv1.HTTPRoute{}).Build()

	if err := MarkRoutesAccepted(ctx, c, gw); err != nil {
		t.Fatal(err)
	}

	var got gwapiv1.HTTPRoute
	if err := c.Get(ctx, clientKey("app", "route"), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Parents) != 1 {
		t.Fatalf("got %d route parents, want 1: %#v", len(got.Status.Parents), got.Status.Parents)
	}
	parent := got.Status.Parents[0]
	if parent.ControllerName != "gateway.azure.com/appnet-gateway.controller" {
		t.Fatalf("controller name = %q", parent.ControllerName)
	}
	if parent.ParentRef.Name != "gw" || parent.ParentRef.Namespace == nil || *parent.ParentRef.Namespace != "app" {
		t.Fatalf("parent ref = %#v", parent.ParentRef)
	}
	accepted := condition(parent.Conditions, string(gwapiv1.RouteConditionAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionTrue || accepted.Reason != string(gwapiv1.RouteReasonAccepted) || accepted.ObservedGeneration != 7 {
		t.Fatalf("route Accepted condition = %#v", accepted)
	}
	resolvedRefs := condition(parent.Conditions, string(gwapiv1.RouteConditionResolvedRefs))
	if resolvedRefs == nil || resolvedRefs.Status != metav1.ConditionTrue || resolvedRefs.Reason != string(gwapiv1.RouteReasonResolvedRefs) || resolvedRefs.ObservedGeneration != 7 {
		t.Fatalf("route ResolvedRefs condition = %#v", resolvedRefs)
	}

	before := append([]gwapiv1.RouteParentStatus(nil), got.Status.Parents...)
	if err := MarkRoutesAccepted(ctx, c, gw); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, clientKey("app", "route"), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Status.Parents, before) {
		t.Fatalf("route parents changed on second update: %#v", got.Status.Parents)
	}
}

func TestMarkRoutesAcceptedRejectsDisallowedNamespace(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := gwapiv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app", Generation: 1},
		Spec:       gwapiv1.GatewaySpec{Listeners: []gwapiv1.Listener{{Name: "http", Port: 80, Protocol: gwapiv1.HTTPProtocolType}}},
	}
	gwNS := gwapiv1.Namespace("app")
	route := &gwapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "other", Generation: 1},
		Spec: gwapiv1.HTTPRouteSpec{CommonRouteSpec: gwapiv1.CommonRouteSpec{ParentRefs: []gwapiv1.ParentReference{{
			Name:      "gw",
			Namespace: &gwNS,
		}}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gw, route).WithStatusSubresource(&gwapiv1.HTTPRoute{}).Build()

	if err := MarkRoutesAccepted(ctx, c, gw); err != nil {
		t.Fatal(err)
	}

	var got gwapiv1.HTTPRoute
	if err := c.Get(ctx, clientKey("other", "route"), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Parents) != 1 {
		t.Fatalf("got %d route parents, want 1: %#v", len(got.Status.Parents), got.Status.Parents)
	}
	accepted := condition(got.Status.Parents[0].Conditions, string(gwapiv1.RouteConditionAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionFalse || accepted.Reason != string(gwapiv1.RouteReasonNotAllowedByListeners) {
		t.Fatalf("route Accepted condition = %#v", accepted)
	}
}

func TestMarkRoutesAcceptedAllowsNamespaceSelector(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := gwapiv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	from := gwapiv1.NamespacesFromSelector
	gwNS := gwapiv1.Namespace("app")
	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app", Generation: 1},
		Spec: gwapiv1.GatewaySpec{Listeners: []gwapiv1.Listener{{
			Name:     "http",
			Port:     80,
			Protocol: gwapiv1.HTTPProtocolType,
			AllowedRoutes: &gwapiv1.AllowedRoutes{Namespaces: &gwapiv1.RouteNamespaces{
				From:     &from,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "allowed"}},
			}},
		}}},
	}
	route := &gwapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "other", Generation: 1},
		Spec: gwapiv1.HTTPRouteSpec{CommonRouteSpec: gwapiv1.CommonRouteSpec{ParentRefs: []gwapiv1.ParentReference{{
			Name:      "gw",
			Namespace: &gwNS,
		}}}},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other", Labels: map[string]string{"team": "allowed"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gw, route, ns).WithStatusSubresource(&gwapiv1.HTTPRoute{}).Build()

	if err := MarkRoutesAccepted(ctx, c, gw); err != nil {
		t.Fatal(err)
	}

	var got gwapiv1.HTTPRoute
	if err := c.Get(ctx, clientKey("other", "route"), &got); err != nil {
		t.Fatal(err)
	}
	accepted := condition(got.Status.Parents[0].Conditions, string(gwapiv1.RouteConditionAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionTrue || accepted.Reason != string(gwapiv1.RouteReasonAccepted) {
		t.Fatalf("route Accepted condition = %#v", accepted)
	}
}

func TestMarkRoutesAcceptedAllowsBackendWithReferenceGrant(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := gwapiv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app", Generation: 1},
		Spec:       gwapiv1.GatewaySpec{Listeners: []gwapiv1.Listener{{Name: "http", Port: 80, Protocol: gwapiv1.HTTPProtocolType}}},
	}
	backendNS := gwapiv1.Namespace("backend")
	backendPort := gwapiv1.PortNumber(8080)
	route := &gwapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "app", Generation: 1},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{ParentRefs: []gwapiv1.ParentReference{{Name: "gw"}}},
			Rules: []gwapiv1.HTTPRouteRule{{
				BackendRefs: []gwapiv1.HTTPBackendRef{{
					BackendRef: gwapiv1.BackendRef{BackendObjectReference: gwapiv1.BackendObjectReference{
						Name:      "api",
						Namespace: &backendNS,
						Port:      &backendPort,
					}},
				}},
			}},
		},
	}
	toName := gwapiv1.ObjectName("api")
	grant := &gwapiv1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "allow", Namespace: "backend"},
		Spec: gwapiv1.ReferenceGrantSpec{
			From: []gwapiv1.ReferenceGrantFrom{{Group: gwapiv1.Group(gwapiv1.GroupVersion.Group), Kind: "HTTPRoute", Namespace: "app"}},
			To:   []gwapiv1.ReferenceGrantTo{{Group: "", Kind: "Service", Name: &toName}},
		},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "backend"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gw, route, grant, svc).WithStatusSubresource(&gwapiv1.HTTPRoute{}).Build()

	if err := MarkRoutesAccepted(ctx, c, gw); err != nil {
		t.Fatal(err)
	}

	var got gwapiv1.HTTPRoute
	if err := c.Get(ctx, clientKey("app", "route"), &got); err != nil {
		t.Fatal(err)
	}
	resolved := condition(got.Status.Parents[0].Conditions, string(gwapiv1.RouteConditionResolvedRefs))
	if resolved == nil || resolved.Status != metav1.ConditionTrue || resolved.Reason != string(gwapiv1.RouteReasonResolvedRefs) {
		t.Fatalf("route ResolvedRefs condition = %#v", resolved)
	}
}

func TestMarkRoutesAcceptedReportsMissingBackend(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := gwapiv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app", Generation: 1},
		Spec:       gwapiv1.GatewaySpec{Listeners: []gwapiv1.Listener{{Name: "http", Port: 80, Protocol: gwapiv1.HTTPProtocolType}}},
	}
	backendPort := gwapiv1.PortNumber(8080)
	route := &gwapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "app", Generation: 1},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{ParentRefs: []gwapiv1.ParentReference{{Name: "gw"}}},
			Rules: []gwapiv1.HTTPRouteRule{{
				BackendRefs: []gwapiv1.HTTPBackendRef{{
					BackendRef: gwapiv1.BackendRef{BackendObjectReference: gwapiv1.BackendObjectReference{
						Name: "missing",
						Port: &backendPort,
					}},
				}},
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gw, route).WithStatusSubresource(&gwapiv1.HTTPRoute{}).Build()

	if err := MarkRoutesAccepted(ctx, c, gw); err != nil {
		t.Fatal(err)
	}

	var got gwapiv1.HTTPRoute
	if err := c.Get(ctx, clientKey("app", "route"), &got); err != nil {
		t.Fatal(err)
	}
	resolved := condition(got.Status.Parents[0].Conditions, string(gwapiv1.RouteConditionResolvedRefs))
	if resolved == nil || resolved.Status != metav1.ConditionFalse || resolved.Reason != string(gwapiv1.RouteReasonBackendNotFound) {
		t.Fatalf("route ResolvedRefs condition = %#v", resolved)
	}
}

func clientKey(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}

func conditionStatus(conditions []metav1.Condition, conditionType string) metav1.ConditionStatus {
	if condition := condition(conditions, conditionType); condition != nil {
		return condition.Status
	}
	return metav1.ConditionUnknown
}

func condition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func supportedFeaturesEqual(a, b []gwapiv1.SupportedFeature) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func routeKindsEqual(a, b []gwapiv1.RouteGroupKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
