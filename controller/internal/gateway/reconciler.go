// Package gateway handles Gateway API reconciliation for a single tenant.
// It is NOT a controller-runtime controller at the manager level; the
// membercluster package invokes Reconcile from its Gateway event handler.
package gateway

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"gateway.appnet.azure.com/controller/controller/internal/provisioner"
	"gateway.appnet.azure.com/controller/controller/internal/status"
)

// ControllerName identifies which GatewayClasses this controller handles.
const ControllerName = "gateway.azure.com/appnet-gateway.controller"

// Deps bundles the inputs needed by Reconcile.
type Deps struct {
	Tenant         string
	InfraNamespace string
	Platform       string
	MemberClient   client.Client
	InfraClient    client.Client

	// Istiod re-render inputs. The gateway reconciler must re-apply the
	// per-tenant istiod Deployment whenever the union of
	// `appnet.azure.com/tls-cert` annotations across Gateways changes, so
	// that mounted infra-cluster Secrets stay in sync with declared HTTPS
	// listeners. These fields mirror what the membercluster cache set.
	IstiodImage          string
	IstiodReplicas       int32
	MemberKubeconfigName string
	MemberKubeconfigKey  string
}

// Reconcile reconciles a single Gateway by namespaced name.
func Reconcile(ctx context.Context, d Deps, key types.NamespacedName) error {
	logger := log.FromContext(ctx).WithName("gateway").WithValues("tenant", d.Tenant, "gw", key)

	var gw gwapiv1.Gateway
	if err := d.MemberClient.Get(ctx, key, &gw); err != nil {
		if apierrors.IsNotFound(err) {
			return deleteAgentGateway(ctx, d, key)
		}
		return fmt.Errorf("get gateway: %w", err)
	}

	// Only handle Gateways referencing our GatewayClass.
	handled, err := isOurs(ctx, d.MemberClient, &gw)
	if err != nil {
		return err
	}
	if !handled {
		logger.V(1).Info("skipping; not ours")
		return nil
	}

	// Provision agentgateway Deployment + Service in the infra cluster.
	params := provisioner.GatewayParams{
		Tenant:         d.Tenant,
		InfraNamespace: d.InfraNamespace,
		Platform:       d.Platform,
		Gateway:        &gw,
	}
	if err := provisioner.Apply(ctx, d.InfraClient, provisioner.AgentGatewayServiceAccountObject(params)); err != nil {
		return fmt.Errorf("apply agentgateway serviceaccount: %w", err)
	}
	if err := ensureAgentGatewayMemberToken(ctx, d, &gw); err != nil {
		return err
	}
	if err := provisioner.Apply(ctx, d.InfraClient, provisioner.AgentGatewayDeployment(params)); err != nil {
		return fmt.Errorf("apply agentgateway deployment: %w", err)
	}
	if err := provisioner.Apply(ctx, d.InfraClient, provisioner.AgentGatewayService(params)); err != nil {
		return fmt.Errorf("apply agentgateway service: %w", err)
	}

	// Re-render istiod with the union of TLS cert annotations across all our
	// Gateways. Cheap (a List + Apply) and ensures HTTPS Gateway adds/removes
	// roll the infra-side Secret mounts.
	if err := RefreshIstiod(ctx, d); err != nil {
		logger.Error(err, "refresh istiod tls mounts")
	}

	// Write Accepted=True. Programmed depends on agentgateway pod readiness.
	if err := status.MarkAccepted(ctx, d.MemberClient, &gw); err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	// Promote to Programmed=True once the agentgateway Deployment has at least
	// one available replica. Re-fetch after Apply since Apply only knows about
	// spec, not status.
	var dep appsv1.Deployment
	depKey := types.NamespacedName{
		Namespace: d.InfraNamespace,
		Name:      provisioner.AgentGatewayObjectName(&gw),
	}
	if err := d.InfraClient.Get(ctx, depKey, &dep); err == nil && dep.Status.AvailableReplicas >= 1 {
		// Re-fetch the Gateway to get the listener statuses we just wrote, so
		// MarkProgrammed can stamp the listener-level Programmed condition.
		var fresh gwapiv1.Gateway
		if err := d.MemberClient.Get(ctx, key, &fresh); err == nil {
			if err := status.MarkProgrammed(ctx, d.MemberClient, &fresh); err != nil {
				logger.Error(err, "mark programmed")
			}
		}
	}

	logger.Info("reconciled gateway")
	return nil
}

func ensureAgentGatewayMemberToken(ctx context.Context, d Deps, gw *gwapiv1.Gateway) error {
	labels := map[string]string{
		provisioner.ManagedByLabel: provisioner.ManagedByValue,
		provisioner.TenantLabel:    d.Tenant,
	}
	memberSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      provisioner.AgentGatewayServiceAccount,
			Namespace: gw.Namespace,
			Labels:    labels,
		},
	}
	if err := provisioner.Apply(ctx, d.MemberClient, memberSA); err != nil {
		return fmt.Errorf("apply member agentgateway serviceaccount: %w", err)
	}

	expirationSeconds := int64(43200)
	tokenRequest := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			Audiences:         []string{"istio-ca"},
			ExpirationSeconds: &expirationSeconds,
		},
	}
	if err := d.MemberClient.SubResource("token").Create(ctx, memberSA, tokenRequest); err != nil {
		return fmt.Errorf("create member agentgateway token: %w", err)
	}
	if tokenRequest.Status.Token == "" {
		return fmt.Errorf("create member agentgateway token: empty token")
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      provisioner.AgentGatewayTokenSecretName(gw),
			Namespace: d.InfraNamespace,
			Labels:    labels,
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"token": tokenRequest.Status.Token},
	}
	if err := provisioner.Apply(ctx, d.InfraClient, secret); err != nil {
		return fmt.Errorf("apply infra agentgateway token secret: %w", err)
	}
	return nil
}

func deleteAgentGateway(ctx context.Context, d Deps, key types.NamespacedName) error {
	// The Gateway is gone in member cluster; delete the infra-cluster Deployment/Service by label.
	stubGw := &gwapiv1.Gateway{}
	stubGw.Namespace = key.Namespace
	stubGw.Name = key.Name
	params := provisioner.GatewayParams{InfraNamespace: d.InfraNamespace, Platform: d.Platform, Gateway: stubGw}
	dep := provisioner.AgentGatewayDeployment(params)
	svc := provisioner.AgentGatewayService(params)
	if err := d.InfraClient.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete deployment: %w", err)
	}
	if err := d.InfraClient.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete service: %w", err)
	}
	tokenSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: provisioner.AgentGatewayTokenSecretName(stubGw), Namespace: d.InfraNamespace}}
	if err := d.InfraClient.Delete(ctx, tokenSecret); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete token secret: %w", err)
	}
	return nil
}

func isOurs(ctx context.Context, c client.Client, gw *gwapiv1.Gateway) (bool, error) {
	var gc gwapiv1.GatewayClass
	if err := c.Get(ctx, types.NamespacedName{Name: string(gw.Spec.GatewayClassName)}, &gc); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get gatewayclass %q: %w", gw.Spec.GatewayClassName, err)
	}
	return string(gc.Spec.ControllerName) == ControllerName, nil
}

// RefreshIstiod re-renders the per-tenant Istiod Deployment with the union of
// `appnet.azure.com/tls-cert` annotations across every Gateway in the member
// cluster that targets our GatewayClass. Missing kubeconfig fields short-circuit
// to a no-op so older callers (e.g. tests) still work.
func RefreshIstiod(ctx context.Context, d Deps) error {
	if d.MemberKubeconfigName == "" {
		return nil
	}
	var gws gwapiv1.GatewayList
	if err := d.MemberClient.List(ctx, &gws); err != nil {
		return fmt.Errorf("list gateways: %w", err)
	}
	var certs []string
	for i := range gws.Items {
		gw := &gws.Items[i]
		ours, err := isOurs(ctx, d.MemberClient, gw)
		if err != nil {
			return err
		}
		if !ours {
			continue
		}
		if name := gw.Annotations[provisioner.TLSCertAnnotation]; name != "" {
			certs = append(certs, name)
		}
	}
	params := provisioner.IstiodParams{
		Tenant:               d.Tenant,
		Namespace:            d.InfraNamespace,
		Platform:             d.Platform,
		Image:                d.IstiodImage,
		Replicas:             d.IstiodReplicas,
		MemberKubeconfigName: d.MemberKubeconfigName,
		MemberKubeconfigKey:  d.MemberKubeconfigKey,
		TLSCertNames:         certs,
	}
	if err := provisioner.Apply(ctx, d.InfraClient, provisioner.IstiodDeployment(params)); err != nil {
		return fmt.Errorf("apply istiod deployment: %w", err)
	}
	return nil
}
