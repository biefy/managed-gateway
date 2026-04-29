//go:build conformance

package conformance_test

import (
	"io/fs"
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/gateway-api/conformance"
	"sigs.k8s.io/gateway-api/conformance/utils/suite"
	"sigs.k8s.io/gateway-api/pkg/features"
)

func TestGatewayAPIStandardConformance(t *testing.T) {
	options := conformance.DefaultOptions(t)
	options.ManifestFS = []fs.FS{conformanceManifests()}
	if options.GatewayClassName == "" {
		options.GatewayClassName = "appnet"
	}
	if options.ConformanceProfiles.Len() == 0 {
		options.ConformanceProfiles = sets.New(
			suite.GatewayHTTPConformanceProfileName,
			suite.GatewayGRPCConformanceProfileName,
			suite.GatewayTLSConformanceProfileName,
		)
	}
	if options.SupportedFeatures.Len() == 0 && !options.EnableAllSupportedFeatures {
		options.SupportedFeatures = features.SetsToNamesSet(
			features.GatewayCoreFeatures,
			features.ReferenceGrantCoreFeatures,
			features.HTTPRouteCoreFeatures,
			features.GRPCRouteCoreFeatures,
			features.TLSRouteCoreFeatures,
		)
	}
	if options.ExemptFeatures == nil {
		options.ExemptFeatures = suite.FeaturesSet{}
	}
	for _, feature := range features.AllFeatures.UnsortedList() {
		if feature.Channel == features.FeatureChannelExperimental {
			options.ExemptFeatures.Insert(feature.Name)
		}
	}

	conformance.RunConformanceWithOptions(t, options)
}

type manifestImageFS struct {
	base         fs.FS
	replacements map[string]string
}

func conformanceManifests() fs.FS {
	echoImage := os.Getenv("GATEWAY_API_CONFORMANCE_ECHO_IMAGE")
	if echoImage == "" {
		echoImage = "akstraffic.azurecr.io/mgdgtw/gateway-api/echo-basic:v20260204-monthly-2026.01-60-g28382302"
	}
	coreDNSImage := os.Getenv("GATEWAY_API_CONFORMANCE_COREDNS_IMAGE")
	if coreDNSImage == "" {
		coreDNSImage = "akstraffic.azurecr.io/mgdgtw/gateway-api/coredns:v1.12.2"
	}
	return manifestImageFS{
		base: conformance.Manifests,
		replacements: map[string]string{
			"gcr.io/k8s-staging-gateway-api/echo-basic:v20260204-monthly-2026.01-60-g28382302": echoImage,
			"registry.k8s.io/coredns/coredns:v1.12.2":                                          coreDNSImage,
		},
	}
}

func (m manifestImageFS) Open(name string) (fs.File, error) {
	return m.base.Open(name)
}

func (m manifestImageFS) ReadFile(name string) ([]byte, error) {
	data, err := fs.ReadFile(m.base, name)
	if err != nil {
		return nil, err
	}
	content := string(data)
	for oldImage, newImage := range m.replacements {
		content = strings.ReplaceAll(content, oldImage, newImage)
	}
	content = strings.ReplaceAll(content, "    gateway-conformance: infra\n", "    gateway-conformance: infra\n    istio.io/dataplane-mode: ambient\n")
	content = strings.ReplaceAll(content, "    gateway-conformance: backend\n", "    gateway-conformance: backend\n    istio.io/dataplane-mode: ambient\n")
	content = strings.ReplaceAll(content, "\n  replicas: 2\n", "\n  replicas: 1\n")
	return []byte(content), nil
}
