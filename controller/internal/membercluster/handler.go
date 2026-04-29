package membercluster

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clientgocache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	gwreconciler "gateway.appnet.azure.com/controller/controller/internal/gateway"
	"gateway.appnet.azure.com/controller/controller/internal/status"
)

func newGatewayProcessor(parentCtx context.Context, logger logr.Logger, deps gwreconciler.Deps) func(types.NamespacedName) {
	inflight := &sync.Map{} // key: types.NamespacedName, value: struct{}{}
	return func(key types.NamespacedName) {
		if _, loaded := inflight.LoadOrStore(key, struct{}{}); loaded {
			return
		}
		go func() {
			defer inflight.Delete(key)
			for attempt := 1; attempt <= 3; attempt++ {
				ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
				err := gwreconciler.Reconcile(ctx, deps, key)
				cancel()
				if err == nil {
					return
				}
				logger.Error(err, "reconcile gateway", "key", key, "attempt", attempt)
				select {
				case <-parentCtx.Done():
					return
				case <-time.After(time.Duration(attempt) * 2 * time.Second):
				}
			}
		}()
	}
}

func newGatewayClassProcessor(parentCtx context.Context, logger logr.Logger, c client.Client) func(string) {
	inflight := &sync.Map{}
	return func(name string) {
		if name == "" {
			return
		}
		if _, loaded := inflight.LoadOrStore(name, struct{}{}); loaded {
			return
		}
		go func() {
			defer inflight.Delete(name)
			for attempt := 1; attempt <= 3; attempt++ {
				ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
				err := reconcileGatewayClass(ctx, c, name)
				cancel()
				if err == nil {
					return
				}
				logger.Error(err, "reconcile gatewayclass", "name", name, "attempt", attempt)
				select {
				case <-parentCtx.Done():
					return
				case <-time.After(time.Duration(attempt) * time.Second):
				}
			}
		}()
	}
}

func reconcileGatewayClass(ctx context.Context, c client.Client, name string) error {
	var gc gwapiv1.GatewayClass
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &gc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if string(gc.Spec.ControllerName) != gwreconciler.ControllerName {
		return nil
	}
	return status.MarkGatewayClassAccepted(ctx, c, &gc)
}

func newGatewayHandler(process func(types.NamespacedName)) clientgocache.ResourceEventHandler {
	return clientgocache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if o, ok := obj.(client.Object); ok {
				process(types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()})
			}
		},
		UpdateFunc: func(_, newObj any) {
			if o, ok := newObj.(client.Object); ok {
				process(types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()})
			}
		},
		DeleteFunc: func(obj any) {
			// Unwrap DeletedFinalStateUnknown if necessary.
			if tombstone, ok := obj.(clientgocache.DeletedFinalStateUnknown); ok {
				obj = tombstone.Obj
			}
			if o, ok := obj.(client.Object); ok {
				process(types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()})
			}
		},
	}
}

func newGatewayClassHandler(process func(string)) clientgocache.ResourceEventHandler {
	return clientgocache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if o, ok := obj.(client.Object); ok {
				process(o.GetName())
			}
		},
		UpdateFunc: func(_, newObj any) {
			if o, ok := newObj.(client.Object); ok {
				process(o.GetName())
			}
		},
	}
}

func newGatewayRefreshHandler(parentCtx context.Context, logger logr.Logger, c client.Client, process func(types.NamespacedName)) clientgocache.ResourceEventHandler {
	enqueueAll := func() {
		ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
		defer cancel()
		var gateways gwapiv1.GatewayList
		if err := c.List(ctx, &gateways); err != nil {
			logger.Error(err, "list gateways for refresh")
			return
		}
		for i := range gateways.Items {
			gw := &gateways.Items[i]
			process(types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name})
		}
	}

	return clientgocache.ResourceEventHandlerFuncs{
		AddFunc: func(any) {
			enqueueAll()
		},
		UpdateFunc: func(any, any) {
			enqueueAll()
		},
		DeleteFunc: func(any) {
			enqueueAll()
		},
	}
}
