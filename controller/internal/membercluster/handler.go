package membercluster

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	clientgocache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gwreconciler "gateway.appnet.azure.com/controller/controller/internal/gateway"
)

// newGatewayHandler returns a client-go ResourceEventHandler that enqueues
// reconciliations for Gateway events. Deduplicated by namespaced name.
// Retries on failure with a simple fixed backoff.
func newGatewayHandler(parentCtx context.Context, logger logr.Logger, deps gwreconciler.Deps) clientgocache.ResourceEventHandler {
	inflight := &sync.Map{} // key: types.NamespacedName, value: struct{}{}
	process := func(key types.NamespacedName) {
		if _, loaded := inflight.LoadOrStore(key, struct{}{}); loaded {
			return // already being reconciled; latest state will be fetched once it returns
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
