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
	return newGatewayProcessorWithReconcile(parentCtx, logger, deps, gwreconciler.Reconcile)
}

type gatewayReconcileFunc func(context.Context, gwreconciler.Deps, types.NamespacedName) error

type gatewayProcessor struct {
	parentCtx context.Context
	logger    logr.Logger
	deps      gwreconciler.Deps
	reconcile gatewayReconcileFunc

	mu     sync.Mutex
	states map[types.NamespacedName]*gatewayWorkState
}

type gatewayWorkState struct {
	running bool
	dirty   bool
}

func newGatewayProcessorWithReconcile(parentCtx context.Context, logger logr.Logger, deps gwreconciler.Deps, reconcile gatewayReconcileFunc) func(types.NamespacedName) {
	p := &gatewayProcessor{
		parentCtx: parentCtx,
		logger:    logger,
		deps:      deps,
		reconcile: reconcile,
		states:    map[types.NamespacedName]*gatewayWorkState{},
	}
	return p.enqueue
}

func (p *gatewayProcessor) enqueue(key types.NamespacedName) {
	p.mu.Lock()
	state := p.states[key]
	if state == nil {
		state = &gatewayWorkState{}
		p.states[key] = state
	}
	if state.running {
		state.dirty = true
		p.mu.Unlock()
		return
	}
	state.running = true
	p.mu.Unlock()

	go p.run(key, state)
}

func (p *gatewayProcessor) run(key types.NamespacedName, state *gatewayWorkState) {
	for {
		p.reconcileWithRetries(key)

		p.mu.Lock()
		if !state.dirty {
			delete(p.states, key)
			p.mu.Unlock()
			return
		}
		state.dirty = false
		p.mu.Unlock()
	}
}

func (p *gatewayProcessor) reconcileWithRetries(key types.NamespacedName) {
	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(p.parentCtx, 30*time.Second)
		err := p.reconcile(ctx, p.deps, key)
		cancel()
		if err == nil {
			return
		}
		p.logger.Error(err, "reconcile gateway", "key", key, "attempt", attempt)
		select {
		case <-p.parentCtx.Done():
			return
		case <-time.After(time.Duration(attempt) * 2 * time.Second):
		}
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
		UpdateFunc: func(oldObj, newObj any) {
			if !objectChanged(oldObj, newObj) {
				return
			}
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
		UpdateFunc: func(oldObj, newObj any) {
			if !objectChanged(oldObj, newObj) {
				return
			}
			if o, ok := newObj.(client.Object); ok {
				process(o.GetName())
			}
		},
	}
}

func objectChanged(oldObj, newObj any) bool {
	oldMeta, oldOK := oldObj.(client.Object)
	newMeta, newOK := newObj.(client.Object)
	if !oldOK || !newOK {
		return true
	}
	if oldMeta.GetGeneration() != newMeta.GetGeneration() {
		return true
	}
	oldDeleting := oldMeta.GetDeletionTimestamp() != nil
	newDeleting := newMeta.GetDeletionTimestamp() != nil
	return oldDeleting != newDeleting
}

func enqueueAllGateways(parentCtx context.Context, logger logr.Logger, c client.Client, process func(types.NamespacedName)) {
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

func startGatewayPeriodicRefresh(parentCtx context.Context, logger logr.Logger, c client.Client, process func(types.NamespacedName)) {
	go func() {
		ticker := time.NewTicker(3 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-parentCtx.Done():
				return
			case <-ticker.C:
				enqueueAllGateways(parentCtx, logger, c, process)
			}
		}
	}()
}

func newGatewayRefreshHandler(parentCtx context.Context, logger logr.Logger, c client.Client, process func(types.NamespacedName)) clientgocache.ResourceEventHandler {
	enqueueAll := func() {
		enqueueAllGateways(parentCtx, logger, c, process)
	}

	return clientgocache.ResourceEventHandlerFuncs{
		AddFunc: func(any) {
			enqueueAll()
		},
		UpdateFunc: func(oldObj, newObj any) {
			if objectChanged(oldObj, newObj) {
				enqueueAll()
			}
		},
		DeleteFunc: func(any) {
			enqueueAll()
		},
	}
}
