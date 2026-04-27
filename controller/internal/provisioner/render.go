// Package provisioner renders Kubernetes objects for per-tenant Istiod
// and per-Gateway agentgateway Deployments/Services.
//
// Invariants:
//   - All rendered pods set `automountServiceAccountToken: false` — per project rules,
//     neither Istiod nor agentgateway is allowed to reach the infra cluster's apiserver.
//   - Objects are labeled with `app.kubernetes.io/managed-by=appnet-gateway-controller`
//     so the controller can safely garbage-collect them via label selector.
package provisioner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TLSCertAnnotation is the workload Gateway annotation that names an
// infra-cluster Secret to mount into istiod and use for HTTPS termination.
// The Secret itself never lives in the member cluster.
const TLSCertAnnotation = "appnet.azure.com/tls-cert"

// TLSMountRoot is the directory under which per-cert Secrets are mounted in
// the istiod pod. Per-cert files live at TLSMountRoot/<name>/{tls.crt,tls.key}.
const TLSMountRoot = "/var/run/tls"

const (
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "appnet-gateway-controller"

	TenantLabel  = "appnet.azure.com/tenant"
	GatewayLabel = "appnet.azure.com/gateway"

	IstiodServicePort            = 15010 // plaintext xDS for agentgateway
	IstiodSecureXDSPort          = 15012 // mTLS xDS/CA for ztunnel and HBONE gateways
	AgentGatewayListen           = 80
	AgentGatewayServiceAccount   = "agentgateway"
	IstiodKeyVaultServiceAccount = "istiod-keyvault"
	PlatformAKS                  = "aks"

	AzureInternalLoadBalancerAnnotation = "service.beta.kubernetes.io/azure-load-balancer-internal"
	// AgentGatewayNode is kept as a hint for kind extraPortMappings on the FIRST
	// Gateway provisioned per cluster. We no longer pin Service.spec.ports.nodePort
	// because multiple Gateways would collide on the same NodePort.
	AgentGatewayNode = 30080
)

// IstiodImage is the default image for per-tenant Istiod (forked, with the
// agentgateway xDS generator).
const IstiodImage = "akstraffic.azurecr.io/mgdgtw/istiod:agentgw"

// AgentGatewayImage is the default data-plane image.
const AgentGatewayImage = "akstraffic.azurecr.io/mgdgtw/agentgateway:dev"

// IstiodParams are the inputs to render per-tenant Istiod.
type IstiodParams struct {
	Tenant               string
	Namespace            string
	Platform             string
	Image                string
	Replicas             int32
	MemberKubeconfigName string // Secret name in Namespace
	MemberKubeconfigKey  string // default "kubeconfig"
	// TLSCertNames is the union of `appnet.azure.com/tls-cert` annotation
	// values across every Gateway the controller renders for this tenant.
	// Each name N is mounted at /var/run/tls/<N>/{tls.crt,tls.key} via the
	// infra-cluster Secret of the same name. Member cluster never sees these.
	TLSCertNames []string
}

func sortedUniq(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// tlsCertHash returns a stable hash of the cert-name set for stamping on the
// pod template annotation so that membership changes roll the Deployment.
func tlsCertHash(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	h := sha256.Sum256([]byte(strings.Join(sortedUniq(names), ",")))
	return hex.EncodeToString(h[:8])
}

func defaultIstiodParams(p IstiodParams) IstiodParams {
	if p.Image == "" {
		p.Image = IstiodImage
	}
	if p.Replicas == 0 {
		p.Replicas = 1
	}
	if p.MemberKubeconfigKey == "" {
		p.MemberKubeconfigKey = "kubeconfig"
	}
	return p
}

func istiodLabels(tenant string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "istiod",
		"app.kubernetes.io/component": "control-plane",
		ManagedByLabel:                ManagedByValue,
		TenantLabel:                   tenant,
	}
}

func isAKS(platform string) bool {
	return strings.EqualFold(strings.TrimSpace(platform), PlatformAKS)
}

func IstiodDeployment(p IstiodParams) *appsv1.Deployment {
	p = defaultIstiodParams(p)
	labels := istiodLabels(p.Tenant)
	podLabels := maps.Clone(labels)
	no := false
	yes := true
	runAsUser := int64(1337)

	certNames := sortedUniq(p.TLSCertNames)
	tlsVolumes := make([]corev1.Volume, 0, len(certNames))
	tlsMounts := make([]corev1.VolumeMount, 0, len(certNames))
	optional := true
	for _, n := range certNames {
		volName := "tls-" + n
		tlsVolumes = append(tlsVolumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: n,
					Optional:   &optional,
					Items: []corev1.KeyToPath{
						{Key: "tls.crt", Path: "tls.crt"},
						{Key: "tls.key", Path: "tls.key"},
					},
				},
			},
		})
		tlsMounts = append(tlsMounts, corev1.VolumeMount{
			Name:      volName,
			MountPath: fmt.Sprintf("%s/%s", TLSMountRoot, n),
			ReadOnly:  true,
		})
	}

	serviceAccountName := ""
	cacertsVolumeSource := corev1.VolumeSource{
		Secret: &corev1.SecretVolumeSource{
			SecretName: "cacerts",
			Optional:   &optional,
		},
	}
	if isAKS(p.Platform) {
		serviceAccountName = IstiodKeyVaultServiceAccount
		podLabels["azure.workload.identity/use"] = "true"
		cacertsVolumeSource = corev1.VolumeSource{
			CSI: &corev1.CSIVolumeSource{
				Driver:   "secrets-store.csi.k8s.io",
				ReadOnly: &yes,
				VolumeAttributes: map[string]string{
					"secretProviderClass": "cacerts-keyvault",
				},
			},
		}
	}

	volumes := []corev1.Volume{{
		Name: "member-kubeconfig",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: p.MemberKubeconfigName,
				Items: []corev1.KeyToPath{{
					Key:  p.MemberKubeconfigKey,
					Path: "kubeconfig",
				}},
			},
		},
	}, {
		Name:         "cacerts",
		VolumeSource: cacertsVolumeSource,
	}}
	volumes = append(volumes, tlsVolumes...)

	mounts := []corev1.VolumeMount{{
		Name:      "member-kubeconfig",
		MountPath: "/var/run/member",
		ReadOnly:  true,
	}, {
		Name:      "cacerts",
		MountPath: "/etc/cacerts",
		ReadOnly:  true,
	}}
	mounts = append(mounts, tlsMounts...)

	podAnnotations := map[string]string{
		"appnet.azure.com/tls-cert-hash": tlsCertHash(certNames),
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "istiod",
			Namespace: p.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &p.Replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels, Annotations: podAnnotations},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: &no,
					ServiceAccountName:           serviceAccountName,
					Containers: []corev1.Container{{
						Name:            "discovery",
						Image:           p.Image,
						ImagePullPolicy: corev1.PullAlways,
						Args: []string{
							"discovery",
							"--kubeconfig=/var/run/member/kubeconfig",
							"--keepaliveMaxServerConnectionAge=30m",
						},
						Env: []corev1.EnvVar{
							{Name: "CLUSTER_ID", Value: p.Tenant},
							{Name: "PILOT_ENABLE_GATEWAY_API", Value: "true"},
							{Name: "PILOT_ENABLE_GATEWAY_API_DEPLOYMENT_CONTROLLER", Value: "false"},
							{Name: "EXTERNAL_ISTIOD", Value: "true"},
							// ztunnel in the member cluster dials mTLS xDS on 15012.
							// Istiod acts as its own CA using the cacerts Secret.
							{Name: "ENABLE_CA_SERVER", Value: "true"},
							{Name: "PILOT_CERT_PROVIDER", Value: "istiod"},
							// ztunnel uses the workload-cluster shim DNS name; infra gateways use tenant-local DNS.
							{Name: "ISTIOD_CUSTOM_HOST", Value: fmt.Sprintf("istiod.istio-system.svc,istiod.istio-system.svc.cluster.local,istiod.%s.svc,istiod.%s.svc.cluster.local", p.Namespace, p.Namespace)},
							{Name: "PILOT_ENABLE_AMBIENT", Value: "true"},
							{Name: "CA_TRUSTED_NODE_ACCOUNTS", Value: "istio-system/ztunnel"},
						},
						Ports: []corev1.ContainerPort{
							{Name: "grpc-xds", ContainerPort: IstiodServicePort, Protocol: corev1.ProtocolTCP},
							{Name: "grpc-xds-tls", ContainerPort: IstiodSecureXDSPort, Protocol: corev1.ProtocolTCP},
						},
						VolumeMounts: mounts,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &no,
							ReadOnlyRootFilesystem:   &yes,
							RunAsNonRoot:             &yes,
							RunAsUser:                &runAsUser,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

func IstiodService(p IstiodParams) *corev1.Service {
	labels := istiodLabels(p.Tenant)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "istiod",
			Namespace: p.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       "grpc-xds",
				Port:       IstiodServicePort,
				TargetPort: intstr.FromInt(IstiodServicePort),
				Protocol:   corev1.ProtocolTCP,
			}, {
				Name:       "grpc-xds-tls",
				Port:       IstiodSecureXDSPort,
				TargetPort: intstr.FromInt(IstiodSecureXDSPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
	if isAKS(p.Platform) {
		svc.Annotations = map[string]string{AzureInternalLoadBalancerAnnotation: "true"}
		svc.Spec.Type = corev1.ServiceTypeLoadBalancer
	}
	return svc
}

// GatewayParams are inputs for a per-Gateway agentgateway Deployment.
type GatewayParams struct {
	Tenant         string
	InfraNamespace string
	Platform       string
	Gateway        *gwapiv1.Gateway
	Image          string
	IstiodDNS      string // e.g. istiod.tenant-alice.svc.cluster.local
	IstiodPort     int32
}

func defaultGatewayParams(p GatewayParams) GatewayParams {
	if p.Image == "" {
		p.Image = AgentGatewayImage
	}
	if p.IstiodPort == 0 {
		p.IstiodPort = IstiodServicePort
	}
	if p.IstiodDNS == "" {
		p.IstiodDNS = fmt.Sprintf("istiod.%s.svc.cluster.local", p.InfraNamespace)
	}
	return p
}

func agentGatewayLabels(tenant, gwNS, gwName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "agentgateway",
		"app.kubernetes.io/component": "data-plane",
		ManagedByLabel:                ManagedByValue,
		TenantLabel:                   tenant,
		GatewayLabel:                  fmt.Sprintf("%s.%s", gwName, gwNS),
	}
}

// AgentGatewayObjectName builds a stable infra-cluster name for a Gateway
// defined in the member cluster: `gw-<gwNS>-<gwName>`.
func AgentGatewayObjectName(gw *gwapiv1.Gateway) string {
	return fmt.Sprintf("gw-%s-%s", gw.Namespace, gw.Name)
}

func AgentGatewayTokenSecretName(gw *gwapiv1.Gateway) string {
	return AgentGatewayObjectName(gw) + "-token"
}

func AgentGatewayServiceAccountObject(p GatewayParams) *corev1.ServiceAccount {
	labels := map[string]string{
		"app.kubernetes.io/name":      "agentgateway",
		"app.kubernetes.io/component": "data-plane",
		ManagedByLabel:                ManagedByValue,
		TenantLabel:                   p.Tenant,
	}
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentGatewayServiceAccount,
			Namespace: p.InfraNamespace,
			Labels:    labels,
		},
	}
}

func AgentGatewayDeployment(p GatewayParams) *appsv1.Deployment {
	p = defaultGatewayParams(p)
	labels := agentGatewayLabels(p.Tenant, p.Gateway.Namespace, p.Gateway.Name)
	name := AgentGatewayObjectName(p.Gateway)
	no := false
	yes := true
	replicas := int32(1)
	runAsRoot := int64(0)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: p.InfraNamespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: &no,
					ServiceAccountName:           AgentGatewayServiceAccount,
					Containers: []corev1.Container{{
						Name:            "agentgateway",
						Image:           p.Image,
						ImagePullPolicy: corev1.PullAlways,
						Env: []corev1.EnvVar{
							{Name: "XDS_ADDRESS", Value: fmt.Sprintf("http://%s:%d", p.IstiodDNS, p.IstiodPort)},
							{Name: "NAMESPACE", Value: p.Gateway.Namespace},
							{Name: "GATEWAY", Value: p.Gateway.Name},
							{Name: "CLUSTER_ID", Value: p.Tenant},
							{Name: "CA_ADDRESS", Value: fmt.Sprintf("https://istiod.%s.svc.cluster.local:%d", p.InfraNamespace, IstiodSecureXDSPort)},
							{Name: "CA_ROOT_CA", Value: "/var/run/secrets/istio/root-cert.pem"},
							{Name: "SERVICE_ACCOUNT", Value: AgentGatewayServiceAccount},
							{Name: "NETWORK", Value: "infra"},
							{Name: "IPV6_ENABLED", Value: "false"},
						},
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: AgentGatewayListen, Protocol: corev1.ProtocolTCP},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &no,
							RunAsNonRoot:             &no,
							RunAsUser:                &runAsRoot,
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
								Add:  []corev1.Capability{"NET_BIND_SERVICE"},
							},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "istio-token",
							MountPath: "/var/run/secrets/tokens",
							ReadOnly:  true,
						}, {
							Name:      "istiod-ca-cert",
							MountPath: "/var/run/secrets/istio",
							ReadOnly:  true,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "istio-token",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName: AgentGatewayTokenSecretName(p.Gateway),
								Items: []corev1.KeyToPath{{
									Key:  "token",
									Path: "istio-token",
								}},
							},
						},
					}, {
						Name: "istiod-ca-cert",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName: "cacerts",
								Optional:   &yes,
								Items: []corev1.KeyToPath{{
									Key:  "root-cert.pem",
									Path: "root-cert.pem",
								}},
							},
						},
					}},
				},
			},
		},
	}
}

func AgentGatewayService(p GatewayParams) *corev1.Service {
	p = defaultGatewayParams(p)
	labels := agentGatewayLabels(p.Tenant, p.Gateway.Namespace, p.Gateway.Name)

	// Expose one Service port per unique listener port. agentgateway binds
	// listener ports dynamically from xDS, so the Service is the only piece
	// that needs to advertise them ahead of time.
	type portKey struct {
		port int32
	}
	seen := map[portKey]struct{}{}
	ports := []corev1.ServicePort{}
	for _, l := range p.Gateway.Spec.Listeners {
		port := int32(l.Port)
		key := portKey{port: port}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		name := fmt.Sprintf("p-%d", port)
		ports = append(ports, corev1.ServicePort{
			Name:       name,
			Port:       port,
			TargetPort: intstr.FromInt(int(port)),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	if len(ports) == 0 {
		// Default to the legacy HTTP listener port so the Service is never
		// portless (which would fail validation).
		ports = append(ports, corev1.ServicePort{
			Name:       "http",
			Port:       AgentGatewayListen,
			TargetPort: intstr.FromInt(AgentGatewayListen),
			Protocol:   corev1.ProtocolTCP,
		})
	}

	svcType := corev1.ServiceTypeNodePort
	if isAKS(p.Platform) {
		svcType = corev1.ServiceTypeLoadBalancer
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentGatewayObjectName(p.Gateway),
			Namespace: p.InfraNamespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Type:     svcType,
			Ports:    ports,
		},
	}
}
