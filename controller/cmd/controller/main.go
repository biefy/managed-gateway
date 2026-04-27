// Per-tenant controller. One Deployment per tenant, deployed into the
// tenant's infra namespace. No cluster-scoped CRD watch — everything is
// configured via flags / env on the Deployment itself.
//
// Responsibilities (single tenant):
//   - Connect to the member cluster via a kubeconfig Secret in our own
//     namespace.
//   - Watch Gateways/HTTPRoutes there; provision per-Gateway agentgateway
//     Deployment + Service in our infra namespace.
//   - Maintain a per-tenant Istiod Deployment (with TLS Secret mounts driven
//     by Gateway annotations).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	gwreconciler "gateway.appnet.azure.com/controller/controller/internal/gateway"
	"gateway.appnet.azure.com/controller/controller/internal/membercluster"
	"gateway.appnet.azure.com/controller/controller/internal/provisioner"
)

const (
	leaderElectionID      = "appnet-gateway.controller"
	defaultMetricsAddress = ":8080"
	defaultProbeAddress   = ":8081"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gwapiv1.Install(scheme))
}

func main() {
	cfg := parseFlags()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&cfg.zapOpts)))
	klog.SetLogger(ctrl.Log)

	if err := cfg.validate(); err != nil {
		ctrl.Log.Error(err, "invalid configuration")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: cfg.metricsAddr},
		HealthProbeBindAddress:  cfg.probeAddr,
		LeaderElection:          cfg.enableLeaderElect,
		LeaderElectionID:        leaderElectionID,
		LeaderElectionNamespace: cfg.infraNamespace,
		// Constrain the infra-cluster cache to our own namespace — we only
		// touch Deployments/Services/Secrets in this one namespace.
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{cfg.infraNamespace: {}},
		},
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := mgr.Add(&bootstrapRunnable{cfg: cfg, infraClient: mgr.GetClient()}); err != nil {
		ctrl.Log.Error(err, "unable to register bootstrap runnable")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up readyz check")
		os.Exit(1)
	}

	ctrl.Log.Info("starting controller",
		"tenant", cfg.tenantName,
		"infraNamespace", cfg.infraNamespace,
		"memberKubeconfigSecret", cfg.kubeconfigSecretName,
		"platform", cfg.platform,
		"leaderElection", cfg.enableLeaderElect)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

type config struct {
	metricsAddr          string
	probeAddr            string
	enableLeaderElect    bool
	tenantName           string
	infraNamespace       string
	kubeconfigSecretName string
	kubeconfigSecretKey  string
	istiodImage          string
	istiodReplicas       int32
	istiodResyncPeriod   time.Duration
	platform             string
	zapOpts              zap.Options
}

func parseFlags() *config {
	c := &config{zapOpts: zap.Options{Development: true}}
	var istiodReplicas int
	flag.StringVar(&c.metricsAddr, "metrics-bind-address", defaultMetricsAddress, "Metrics endpoint")
	flag.StringVar(&c.probeAddr, "health-probe-bind-address", defaultProbeAddress, "Health probe endpoint")
	flag.BoolVar(&c.enableLeaderElect, "leader-elect", true, "Enable leader election (required for HA)")
	flag.StringVar(&c.tenantName, "tenant-name", os.Getenv("TENANT_NAME"),
		"Logical tenant name (also used as the istiod owner label).")
	flag.StringVar(&c.infraNamespace, "infra-namespace", os.Getenv("INFRA_NAMESPACE"),
		"Infra-cluster namespace this controller owns. Also holds the leader-election lease.")
	flag.StringVar(&c.kubeconfigSecretName, "member-kubeconfig-secret", os.Getenv("MEMBER_KUBECONFIG_SECRET"),
		"Name of the Secret in our namespace containing the member-cluster kubeconfig.")
	flag.StringVar(&c.kubeconfigSecretKey, "member-kubeconfig-key", "kubeconfig",
		"Key inside the Secret holding the kubeconfig bytes.")
	flag.StringVar(&c.istiodImage, "istiod-image", os.Getenv("ISTIOD_IMAGE"),
		"Container image for the per-tenant istiod (empty = provisioner default).")
	flag.IntVar(&istiodReplicas, "istiod-replicas", 1, "Replicas for the per-tenant istiod Deployment.")
	flag.DurationVar(&c.istiodResyncPeriod, "istiod-resync-period", 60*time.Second,
		"How often to re-apply the per-tenant istiod Deployment to absorb manual edits.")
	flag.StringVar(&c.platform, "platform", os.Getenv("APPNET_PLATFORM"),
		"Deployment platform. Set to aks to render Azure LoadBalancer Services.")
	c.zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()
	c.istiodReplicas = int32(istiodReplicas)
	return c
}

func (c *config) validate() error {
	if c.tenantName == "" {
		return errors.New("--tenant-name (or $TENANT_NAME) is required")
	}
	if c.infraNamespace == "" {
		return errors.New("--infra-namespace (or $INFRA_NAMESPACE) is required")
	}
	if c.kubeconfigSecretName == "" {
		return errors.New("--member-kubeconfig-secret (or $MEMBER_KUBECONFIG_SECRET) is required")
	}
	return nil
}

// bootstrapRunnable owns the per-tenant work: open the member cluster,
// register the Gateway handler, and periodically re-render istiod. It's
// gated on leader election so only one replica does work.
type bootstrapRunnable struct {
	cfg         *config
	infraClient client.Client
}

func (b *bootstrapRunnable) NeedLeaderElection() bool { return true }

func (b *bootstrapRunnable) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("bootstrap").WithValues("tenant", b.cfg.tenantName)

	kubeconfig, err := b.loadKubeconfig(ctx)
	if err != nil {
		return fmt.Errorf("load member kubeconfig: %w", err)
	}

	mc, err := membercluster.Start(ctx, membercluster.CacheConfig{
		TenantName:           b.cfg.tenantName,
		InfraNamespace:       b.cfg.infraNamespace,
		Platform:             b.cfg.platform,
		Kubeconfig:           kubeconfig,
		InfraClient:          b.infraClient,
		IstiodImage:          b.cfg.istiodImage,
		IstiodReplicas:       b.cfg.istiodReplicas,
		MemberKubeconfigName: b.cfg.kubeconfigSecretName,
		MemberKubeconfigKey:  b.cfg.kubeconfigSecretKey,
	})
	if err != nil {
		return fmt.Errorf("start member cluster cache: %w", err)
	}
	logger.Info("member cluster cache started")

	deps := gwreconciler.Deps{
		Tenant:               b.cfg.tenantName,
		InfraNamespace:       b.cfg.infraNamespace,
		Platform:             b.cfg.platform,
		MemberClient:         mc.Client,
		InfraClient:          b.infraClient,
		IstiodImage:          b.cfg.istiodImage,
		IstiodReplicas:       b.cfg.istiodReplicas,
		MemberKubeconfigName: b.cfg.kubeconfigSecretName,
		MemberKubeconfigKey:  b.cfg.kubeconfigSecretKey,
	}

	// Apply once on startup so the istiod Deployment exists even before any
	// Gateway events fire (and for the initial cert union when Gateways do
	// exist already).
	if err := provisioner.Apply(ctx, b.infraClient, provisioner.IstiodDeployment(initialIstiodParams(b.cfg))); err != nil {
		logger.Error(err, "initial istiod apply")
	}
	if err := provisioner.Apply(ctx, b.infraClient, provisioner.IstiodService(initialIstiodParams(b.cfg))); err != nil {
		logger.Error(err, "initial istiod service apply")
	}

	go b.istiodResyncLoop(ctx, deps, logger)

	<-ctx.Done()
	return nil
}

func (b *bootstrapRunnable) loadKubeconfig(ctx context.Context) ([]byte, error) {
	var secret corev1.Secret
	if err := b.infraClient.Get(ctx, types.NamespacedName{
		Namespace: b.cfg.infraNamespace,
		Name:      b.cfg.kubeconfigSecretName,
	}, &secret); err != nil {
		return nil, fmt.Errorf("get secret %s/%s: %w", b.cfg.infraNamespace, b.cfg.kubeconfigSecretName, err)
	}
	data, ok := secret.Data[b.cfg.kubeconfigSecretKey]
	if !ok || len(data) == 0 {
		return nil, fmt.Errorf("secret %s/%s missing key %q", b.cfg.infraNamespace, b.cfg.kubeconfigSecretName, b.cfg.kubeconfigSecretKey)
	}
	if _, err := clientcmd.RESTConfigFromKubeConfig(data); err != nil {
		return nil, fmt.Errorf("kubeconfig parse: %w", err)
	}
	return data, nil
}

func (b *bootstrapRunnable) istiodResyncLoop(ctx context.Context, deps gwreconciler.Deps, logger logr.Logger) {
	t := time.NewTicker(b.cfg.istiodResyncPeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := gwreconciler.RefreshIstiod(ctx, deps); err != nil {
				logger.Error(err, "periodic istiod refresh")
			}
		}
	}
}

func initialIstiodParams(c *config) provisioner.IstiodParams {
	return provisioner.IstiodParams{
		Tenant:               c.tenantName,
		Namespace:            c.infraNamespace,
		Platform:             c.platform,
		Image:                c.istiodImage,
		Replicas:             c.istiodReplicas,
		MemberKubeconfigName: c.kubeconfigSecretName,
		MemberKubeconfigKey:  c.kubeconfigSecretKey,
	}
}

// Local interface alias to avoid pulling logr just for the type.
var _ manager.Runnable = (*bootstrapRunnable)(nil)
