// Package gateway handles Gateway API reconciliation for a single tenant.
// It is NOT a controller-runtime controller at the manager level; the
// membercluster package invokes Reconcile from its Gateway event handler.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

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

const (
	agentGatewayTokenExpiration = 12 * time.Hour
	agentGatewayTokenRefresh    = 4 * time.Hour
)

// Deps bundles the inputs needed by Reconcile.
type Deps struct {
	Tenant         string
	InfraNamespace string
	Platform       string
	MemberClient   client.Client
	InfraClient    client.Client

	// Istiod re-render inputs. The gateway reconciler must re-apply the
	// per-tenant istiod Deployment whenever the union of Gateway listener
	// certificateRefs changes, so mounted infra-cluster Secrets stay in sync
	// with declared HTTPS listeners. These fields mirror what the membercluster cache set.
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
			if err := deleteAgentGateway(ctx, d, key); err != nil {
				return err
			}
			return RefreshIstiod(ctx, d)
		}
		return fmt.Errorf("get gateway: %w", err)
	}

	// Only handle Gateways referencing our GatewayClass.
	handled, err := isOurs(ctx, d.MemberClient, &gw)
	if err != nil {
		return err
	}
	if !handled {
		logger.V(1).Info("cleaning up gateway no longer handled by this controller")
		if err := deleteAgentGateway(ctx, d, key); err != nil {
			return err
		}
		return RefreshIstiod(ctx, d)
	}

	if err := status.MarkAccepted(ctx, d.MemberClient, &gw); err != nil {
		return fmt.Errorf("write accepted status: %w", err)
	}
	if err := validateGatewayTLSAssets(ctx, d, &gw); err != nil {
		var listenerErr *listenerRefsError
		if errors.As(err, &listenerErr) {
			if statusErr := status.MarkListenerResolvedRefs(ctx, d.MemberClient, &gw, listenerErr.listener, metav1.ConditionFalse, listenerErr.reason, listenerErr.message); statusErr != nil {
				return fmt.Errorf("write listener resolved refs status: %w", statusErr)
			}
		}
		return err
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
	tokenExpiration, err := ensureAgentGatewayMemberToken(ctx, d, &gw)
	if err != nil {
		return err
	}
	params.TokenExpiration = tokenExpiration
	if err := provisioner.Apply(ctx, d.InfraClient, provisioner.AgentGatewayDeployment(params)); err != nil {
		return fmt.Errorf("apply agentgateway deployment: %w", err)
	}
	if err := provisioner.Apply(ctx, d.InfraClient, provisioner.AgentGatewayService(params)); err != nil {
		return fmt.Errorf("apply agentgateway service: %w", err)
	}

	// Re-render istiod with the union of listener certificateRefs across all our
	// Gateways. Cheap (a List + Apply) and ensures HTTPS Gateway adds/removes
	// roll the infra-side Secret mounts.
	if err := RefreshIstiod(ctx, d); err != nil {
		logger.Error(err, "refresh istiod tls mounts")
	}

	depKey := types.NamespacedName{
		Namespace: d.InfraNamespace,
		Name:      provisioner.AgentGatewayObjectName(&gw),
	}
	ready, err := waitForAgentGatewayAvailable(ctx, d.InfraClient, depKey)
	if err != nil {
		return fmt.Errorf("wait for agentgateway deployment: %w", err)
	}
	if !ready {
		return fmt.Errorf("agentgateway deployment %s not ready", depKey)
	}
	svcKey := types.NamespacedName{
		Namespace: d.InfraNamespace,
		Name:      provisioner.AgentGatewayObjectName(&gw),
	}
	addresses, err := waitForAgentGatewayAddresses(ctx, d.InfraClient, svcKey)
	if err != nil {
		return fmt.Errorf("wait for agentgateway service address: %w", err)
	}
	if len(addresses) == 0 {
		return fmt.Errorf("agentgateway service %s has no loadbalancer address", svcKey)
	}
	var fresh gwapiv1.Gateway
	if err := d.MemberClient.Get(ctx, key, &fresh); err == nil {
		if err := status.MarkProgrammed(ctx, d.MemberClient, &fresh, addresses...); err != nil {
			logger.Error(err, "mark programmed")
		}
		if err := status.MarkRoutesAccepted(ctx, d.MemberClient, &fresh); err != nil {
			logger.Error(err, "mark routes accepted")
		}
	}

	logger.Info("reconciled gateway")
	return nil
}

func waitForAgentGatewayAvailable(ctx context.Context, c client.Client, key types.NamespacedName) (bool, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		var dep appsv1.Deployment
		err := c.Get(ctx, key, &dep)
		if err == nil && dep.Status.AvailableReplicas >= 1 {
			return true, nil
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		select {
		case <-ctx.Done():
			return false, nil
		case <-ticker.C:
		}
	}
}

func waitForAgentGatewayAddresses(ctx context.Context, c client.Client, key types.NamespacedName) ([]gwapiv1.GatewayStatusAddress, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		var svc corev1.Service
		err := c.Get(ctx, key, &svc)
		if err == nil {
			addresses := serviceGatewayAddresses(&svc)
			if len(addresses) > 0 {
				return addresses, nil
			}
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-ticker.C:
		}
	}
}

func serviceGatewayAddresses(svc *corev1.Service) []gwapiv1.GatewayStatusAddress {
	var addresses []gwapiv1.GatewayStatusAddress
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			addresses = append(addresses, gwapiv1.GatewayStatusAddress{Type: addressType(gwapiv1.IPAddressType), Value: ingress.IP})
		}
		if ingress.Hostname != "" {
			addresses = append(addresses, gwapiv1.GatewayStatusAddress{Type: addressType(gwapiv1.HostnameAddressType), Value: ingress.Hostname})
		}
	}
	return addresses
}

func addressType(t gwapiv1.AddressType) *gwapiv1.AddressType {
	return &t
}

type listenerRefsError struct {
	listener gwapiv1.SectionName
	reason   gwapiv1.ListenerConditionReason
	message  string
}

func (e *listenerRefsError) Error() string {
	return e.message
}

func validateGatewayTLSAssets(ctx context.Context, d Deps, gw *gwapiv1.Gateway) error {
	for _, listener := range gw.Spec.Listeners {
		refs, required := listenerCertificateRefs(listener)
		if !required {
			continue
		}
		if len(refs) == 0 {
			return &listenerRefsError{listener: listener.Name, reason: gwapiv1.ListenerReasonInvalidCertificateRef, message: "Listener requires a certificateRef"}
		}
		for _, ref := range refs {
			name, ok := secretObjectReferenceName(ref)
			if !ok {
				return &listenerRefsError{listener: listener.Name, reason: gwapiv1.ListenerReasonInvalidCertificateRef, message: fmt.Sprintf("certificateRef %s must reference a core Secret", ref.Name)}
			}
			if !gatewaySecretReferencePermitted(ctx, d.MemberClient, gw, ref) {
				return &listenerRefsError{listener: listener.Name, reason: gwapiv1.ListenerReasonRefNotPermitted, message: fmt.Sprintf("certificateRef %s is not permitted", ref.Name)}
			}
			if err := requireInfraSecretKeys(ctx, d.InfraClient, d.InfraNamespace, name, "tls.crt", "tls.key"); err != nil {
				return &listenerRefsError{listener: listener.Name, reason: gwapiv1.ListenerReasonInvalidCertificateRef, message: fmt.Sprintf("certificateRef %s: %v", ref.Name, err)}
			}
		}
	}
	if gw.Spec.TLS != nil && gw.Spec.TLS.Backend != nil && gw.Spec.TLS.Backend.ClientCertificateRef != nil {
		ref := *gw.Spec.TLS.Backend.ClientCertificateRef
		name, ok := secretObjectReferenceName(ref)
		if !ok {
			return fmt.Errorf("backend clientCertificateRef %s must reference a core Secret", ref.Name)
		}
		if !gatewaySecretReferencePermitted(ctx, d.MemberClient, gw, ref) {
			return fmt.Errorf("backend clientCertificateRef %s is not permitted", ref.Name)
		}
		if err := requireInfraSecretKeys(ctx, d.InfraClient, d.InfraNamespace, name, "tls.crt", "tls.key"); err != nil {
			return fmt.Errorf("backend clientCertificateRef %s: %w", ref.Name, err)
		}
	}
	return nil
}

func listenerCertificateRefs(listener gwapiv1.Listener) ([]gwapiv1.SecretObjectReference, bool) {
	if listener.Protocol != gwapiv1.HTTPSProtocolType && listener.Protocol != gwapiv1.TLSProtocolType {
		return nil, false
	}
	if listener.Protocol == gwapiv1.TLSProtocolType && listener.TLS == nil {
		return nil, false
	}
	if listener.TLS != nil && listener.TLS.Mode != nil && *listener.TLS.Mode == gwapiv1.TLSModePassthrough {
		return nil, false
	}
	if listener.TLS == nil {
		return nil, true
	}
	return listener.TLS.CertificateRefs, true
}

func requireInfraSecretKeys(ctx context.Context, c client.Client, namespace, name string, keys ...string) error {
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("infra Secret not found")
		}
		return err
	}
	for _, key := range keys {
		if len(secret.Data[key]) == 0 {
			return fmt.Errorf("infra Secret missing %s", key)
		}
	}
	return nil
}

func gatewaySecretReferencePermitted(ctx context.Context, c client.Client, gw *gwapiv1.Gateway, ref gwapiv1.SecretObjectReference) bool {
	namespace := gwapiv1.Namespace(gw.Namespace)
	if ref.Namespace != nil {
		namespace = *ref.Namespace
	}
	if namespace == gwapiv1.Namespace(gw.Namespace) {
		return true
	}
	var grants gwapiv1.ReferenceGrantList
	if err := c.List(ctx, &grants, client.InNamespace(string(namespace))); err != nil {
		return false
	}
	for _, grant := range grants.Items {
		if gatewayReferenceGrantAllows(grant, gwapiv1.Namespace(gw.Namespace), ref.Name) {
			return true
		}
	}
	return false
}

func gatewayReferenceGrantAllows(grant gwapiv1.ReferenceGrant, fromNamespace gwapiv1.Namespace, toName gwapiv1.ObjectName) bool {
	for _, from := range grant.Spec.From {
		if from.Group != gwapiv1.Group(gwapiv1.GroupVersion.Group) || from.Kind != "Gateway" || from.Namespace != fromNamespace {
			continue
		}
		for _, to := range grant.Spec.To {
			if to.Group == "" && to.Kind == "Secret" && (to.Name == nil || *to.Name == toName) {
				return true
			}
		}
	}
	return false
}

func ensureAgentGatewayMemberToken(ctx context.Context, d Deps, gw *gwapiv1.Gateway) (string, error) {
	labels := provisioner.AgentGatewayLabels(d.Tenant, gw)
	memberSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      provisioner.AgentGatewayServiceAccount,
			Namespace: gw.Namespace,
			Labels: map[string]string{
				provisioner.ManagedByLabel: provisioner.ManagedByValue,
				provisioner.TenantLabel:    d.Tenant,
			},
		},
	}
	if err := provisioner.Apply(ctx, d.MemberClient, memberSA); err != nil {
		return "", fmt.Errorf("apply member agentgateway serviceaccount: %w", err)
	}

	secretKey := types.NamespacedName{Namespace: d.InfraNamespace, Name: provisioner.AgentGatewayTokenSecretName(gw)}
	var existing corev1.Secret
	if err := d.InfraClient.Get(ctx, secretKey, &existing); err == nil {
		if tokenSecretCurrent(&existing, time.Now()) {
			return existing.Annotations[provisioner.MemberTokenExpirationAnnotation], nil
		}
	} else if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("get infra agentgateway token secret: %w", err)
	}

	expirationSeconds := int64(agentGatewayTokenExpiration.Seconds())
	tokenRequest := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			Audiences:         []string{"istio-ca"},
			ExpirationSeconds: &expirationSeconds,
		},
	}
	if err := d.MemberClient.SubResource("token").Create(ctx, memberSA, tokenRequest); err != nil {
		return "", fmt.Errorf("create member agentgateway token: %w", err)
	}
	if tokenRequest.Status.Token == "" {
		return "", fmt.Errorf("create member agentgateway token: empty token")
	}
	if tokenRequest.Status.ExpirationTimestamp.IsZero() {
		return "", fmt.Errorf("create member agentgateway token: empty expiration")
	}

	expires := tokenRequest.Status.ExpirationTimestamp.UTC().Format(time.RFC3339)
	annotations := provisioner.AgentGatewayAnnotations(gw)
	annotations[provisioner.MemberTokenExpirationAnnotation] = expires
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        secretKey.Name,
			Namespace:   secretKey.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"token": tokenRequest.Status.Token},
	}
	if err := provisioner.Apply(ctx, d.InfraClient, secret); err != nil {
		return "", fmt.Errorf("apply infra agentgateway token secret: %w", err)
	}
	return expires, nil
}

func tokenSecretCurrent(secret *corev1.Secret, now time.Time) bool {
	if len(secret.Data["token"]) == 0 {
		return false
	}
	expires, ok := secret.Annotations[provisioner.MemberTokenExpirationAnnotation]
	if !ok || expires == "" {
		return false
	}
	expiration, err := time.Parse(time.RFC3339, expires)
	if err != nil {
		return false
	}
	return expiration.After(now.Add(agentGatewayTokenRefresh))
}

func deleteAgentGateway(ctx context.Context, d Deps, key types.NamespacedName) error {
	stubGw := &gwapiv1.Gateway{}
	stubGw.Namespace = key.Namespace
	stubGw.Name = key.Name
	params := provisioner.GatewayParams{InfraNamespace: d.InfraNamespace, Platform: d.Platform, Gateway: stubGw}
	if err := deleteManagedGatewayObject(ctx, d.InfraClient, provisioner.AgentGatewayDeployment(params), key); err != nil {
		return fmt.Errorf("delete deployment: %w", err)
	}
	if err := deleteManagedGatewayObject(ctx, d.InfraClient, provisioner.AgentGatewayService(params), key); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	tokenSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: provisioner.AgentGatewayTokenSecretName(stubGw), Namespace: d.InfraNamespace}}
	if err := deleteManagedGatewayObject(ctx, d.InfraClient, tokenSecret, key); err != nil {
		return fmt.Errorf("delete token secret: %w", err)
	}
	return nil
}

func deleteManagedGatewayObject(ctx context.Context, c client.Client, obj client.Object, key types.NamespacedName) error {
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if obj.GetLabels()[provisioner.ManagedByLabel] != provisioner.ManagedByValue {
		return nil
	}
	annotations := obj.GetAnnotations()
	if annotations[provisioner.GatewayNSAnnotation] != "" && annotations[provisioner.GatewayNSAnnotation] != key.Namespace {
		return nil
	}
	if annotations[provisioner.GatewayNameAnnotation] != "" && annotations[provisioner.GatewayNameAnnotation] != key.Name {
		return nil
	}
	return c.Delete(ctx, obj)
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
// listener certificateRefs across every Gateway in the member cluster that
// targets our GatewayClass. Missing kubeconfig fields short-circuit to a no-op
// so older callers (e.g. tests) still work.
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
		certs = append(certs, gatewayTLSCertNames(gw)...)
		certs = append(certs, gatewayBackendTLSCertNames(gw)...)
	}
	certs = append(certs, backendTLSPolicyCACertNames(ctx, d.MemberClient)...)
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

func gatewayTLSCertNames(gw *gwapiv1.Gateway) []string {
	var out []string
	for _, l := range gw.Spec.Listeners {
		if l.Protocol != gwapiv1.HTTPSProtocolType && l.Protocol != gwapiv1.TLSProtocolType {
			continue
		}
		if l.TLS == nil || (l.TLS.Mode != nil && *l.TLS.Mode == gwapiv1.TLSModePassthrough) {
			continue
		}
		for _, ref := range l.TLS.CertificateRefs {
			if name, ok := secretObjectReferenceName(ref); ok {
				out = append(out, name)
			}
		}
	}
	return out
}

func gatewayBackendTLSCertNames(gw *gwapiv1.Gateway) []string {
	if gw.Spec.TLS == nil || gw.Spec.TLS.Backend == nil || gw.Spec.TLS.Backend.ClientCertificateRef == nil {
		return nil
	}
	if name, ok := secretObjectReferenceName(*gw.Spec.TLS.Backend.ClientCertificateRef); ok {
		return []string{name}
	}
	return nil
}

func backendTLSPolicyCACertNames(ctx context.Context, c client.Client) []string {
	var policies gwapiv1.BackendTLSPolicyList
	if err := c.List(ctx, &policies); err != nil {
		return nil
	}
	var out []string
	for _, policy := range policies.Items {
		for _, ref := range policy.Spec.Validation.CACertificateRefs {
			if ref.Group == "" && ref.Kind == "ConfigMap" {
				out = append(out, string(ref.Name))
			}
		}
	}
	return out
}

func secretObjectReferenceName(ref gwapiv1.SecretObjectReference) (string, bool) {
	group := ""
	if ref.Group != nil {
		group = string(*ref.Group)
	}
	kind := "Secret"
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}
	if group == "" && kind == "Secret" {
		return string(ref.Name), true
	}
	return "", false
}
