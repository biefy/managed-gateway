package membercluster

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgocache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	gwreconciler "gateway.appnet.azure.com/controller/controller/internal/gateway"
)

func TestGatewayHandlerEnqueuesObjectKey(t *testing.T) {
	var got []types.NamespacedName
	handler := newGatewayHandler(func(key types.NamespacedName) {
		got = append(got, key)
	})
	gw := &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app"}}

	handler.OnAdd(gw, false)
	handler.OnUpdate(nil, gw)
	handler.OnDelete(clientgocache.DeletedFinalStateUnknown{Obj: gw})

	want := []types.NamespacedName{
		{Namespace: "app", Name: "gw"},
		{Namespace: "app", Name: "gw"},
		{Namespace: "app", Name: "gw"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("processed keys = %#v, want %#v", got, want)
	}
}

func TestGatewayClassHandlerEnqueuesName(t *testing.T) {
	var got []string
	handler := newGatewayClassHandler(func(name string) {
		got = append(got, name)
	})
	gc := &gwapiv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "appnet"}}

	handler.OnAdd(gc, false)
	handler.OnUpdate(nil, gc)

	want := []string{"appnet", "appnet"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("processed names = %#v, want %#v", got, want)
	}
}

func TestReconcileGatewayClassMarksOwnedClassAccepted(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := gwapiv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	gc := &gwapiv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "appnet", Generation: 4},
		Spec:       gwapiv1.GatewayClassSpec{ControllerName: gwapiv1.GatewayController(gwreconciler.ControllerName)},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gc).WithStatusSubresource(&gwapiv1.GatewayClass{}).Build()

	if err := reconcileGatewayClass(ctx, c, "appnet"); err != nil {
		t.Fatal(err)
	}

	var got gwapiv1.GatewayClass
	if err := c.Get(ctx, types.NamespacedName{Name: "appnet"}, &got); err != nil {
		t.Fatal(err)
	}
	for _, condition := range got.Status.Conditions {
		if condition.Type == string(gwapiv1.GatewayClassConditionStatusAccepted) {
			if condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != 4 {
				t.Fatalf("Accepted condition = %#v", condition)
			}
			return
		}
	}
	t.Fatalf("GatewayClass Accepted condition missing: %#v", got.Status.Conditions)
}

func TestGatewayRefreshHandlerEnqueuesAllGateways(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := gwapiv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "one", Namespace: "app"}},
		&gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "two", Namespace: "other"}},
	).Build()
	var mu sync.Mutex
	var got []types.NamespacedName
	handler := newGatewayRefreshHandler(ctx, logr.Discard(), c, func(key types.NamespacedName) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, key)
	})

	handler.OnAdd(&gwapiv1.HTTPRoute{}, false)

	mu.Lock()
	defer mu.Unlock()
	want := map[types.NamespacedName]bool{
		{Namespace: "app", Name: "one"}:   true,
		{Namespace: "other", Name: "two"}: true,
	}
	if len(got) != len(want) {
		t.Fatalf("processed %d keys, want %d: %#v", len(got), len(want), got)
	}
	for _, key := range got {
		if !want[key] {
			t.Fatalf("unexpected key processed: %#v", key)
		}
	}
}
