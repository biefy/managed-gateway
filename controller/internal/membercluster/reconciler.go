// Package membercluster builds the controller-runtime cache + client for the
// member cluster this per-tenant controller serves.
package membercluster

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	gwreconciler "gateway.appnet.azure.com/controller/controller/internal/gateway"
)

// CacheConfig configures a single member-cluster cache + handler.
type CacheConfig struct {
	TenantName           string
	InfraNamespace       string
	Platform             string
	Kubeconfig           []byte
	InfraClient          client.Client
	IstiodImage          string
	IstiodReplicas       int32
	MemberKubeconfigName string
	MemberKubeconfigKey  string
}

// Client is what callers receive once the cache is up.
type Client struct {
	Cache  cache.Cache
	Client client.Client
}

// Start opens a cache against the member cluster and registers the Gateway event handler.
func Start(parent context.Context, cfg CacheConfig) (*Client, error) {
	restCfg, err := clientcmd.RESTConfigFromKubeConfig(cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gwapiv1.Install(scheme))

	c, err := cache.New(restCfg, cache.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("new cache: %w", err)
	}

	for _, obj := range []client.Object{
		&gwapiv1.BackendTLSPolicy{},
		&gwapiv1.GRPCRoute{},
		&gwapiv1.Gateway{},
		&gwapiv1.GatewayClass{},
		&gwapiv1.HTTPRoute{},
		&gwapiv1.ListenerSet{},
		&gwapiv1.ReferenceGrant{},
		&gwapiv1.TLSRoute{},
	} {
		if _, err := c.GetInformer(parent, obj); err != nil {
			return nil, fmt.Errorf("get informer %T: %w", obj, err)
		}
	}

	cli, err := client.New(restCfg, client.Options{Scheme: scheme, Cache: &client.CacheOptions{Reader: c}})
	if err != nil {
		return nil, fmt.Errorf("new client: %w", err)
	}

	gwInformer, err := c.GetInformer(parent, &gwapiv1.Gateway{})
	if err != nil {
		return nil, err
	}
	logger := log.FromContext(parent).WithName("membercluster").WithValues("tenant", cfg.TenantName)
	deps := gwreconciler.Deps{
		Tenant:               cfg.TenantName,
		InfraNamespace:       cfg.InfraNamespace,
		Platform:             cfg.Platform,
		MemberClient:         cli,
		InfraClient:          cfg.InfraClient,
		IstiodImage:          cfg.IstiodImage,
		IstiodReplicas:       cfg.IstiodReplicas,
		MemberKubeconfigName: cfg.MemberKubeconfigName,
		MemberKubeconfigKey:  cfg.MemberKubeconfigKey,
	}
	processGateway := newGatewayProcessor(parent, logger, deps)
	if _, err := gwInformer.AddEventHandler(newGatewayHandler(processGateway)); err != nil {
		return nil, fmt.Errorf("add gateway handler: %w", err)
	}
	gcInformer, err := c.GetInformer(parent, &gwapiv1.GatewayClass{})
	if err != nil {
		return nil, err
	}
	processGatewayClass := newGatewayClassProcessor(parent, logger, cli)
	if _, err := gcInformer.AddEventHandler(newGatewayClassHandler(processGatewayClass)); err != nil {
		return nil, fmt.Errorf("add gatewayclass handler: %w", err)
	}
	refreshHandler := newGatewayRefreshHandler(parent, logger, cli, processGateway)
	for _, obj := range []client.Object{
		&gwapiv1.BackendTLSPolicy{},
		&gwapiv1.GRPCRoute{},
		&gwapiv1.GatewayClass{},
		&gwapiv1.HTTPRoute{},
		&gwapiv1.ListenerSet{},
		&gwapiv1.ReferenceGrant{},
		&gwapiv1.TLSRoute{},
	} {
		informer, err := c.GetInformer(parent, obj)
		if err != nil {
			return nil, fmt.Errorf("get informer %T: %w", obj, err)
		}
		if _, err := informer.AddEventHandler(refreshHandler); err != nil {
			return nil, fmt.Errorf("add refresh handler %T: %w", obj, err)
		}
	}

	go func() {
		if err := c.Start(parent); err != nil && parent.Err() == nil {
			logger.Error(err, "member cluster cache stopped with error")
		}
	}()

	return &Client{Cache: c, Client: cli}, nil
}
