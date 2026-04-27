// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package xds

import (
	"istio.io/istio/pilot/pkg/model"
)

// AgentgatewayResourceType is the custom xDS type URL agentgateway subscribes to
// for its config (Bind/Listener/Route/Backend/Policy + reused istio.workload.*).
const AgentgatewayResourceType = "type.googleapis.com/agentgateway.dev.resource.Resource"

// AgentgatewayResourceGenerator emits agentgateway.dev.resource.Resource for
// proxies of NodeType=agentgateway. It keys per-Gateway delivery off
// node.metadata.role = "<workload-ns>~<gateway-name>".
//
// Skeleton: returns an empty resource set so the proxy ACKs but receives no
// listeners yet. The translation from Gateway/HTTPRoute -> Bind/Listener/Route
// is implemented in agentgateway_translate.go.
type AgentgatewayResourceGenerator struct {
	Server *DiscoveryServer
}

var _ model.XdsResourceGenerator = &AgentgatewayResourceGenerator{}

func (g *AgentgatewayResourceGenerator) Generate(
	proxy *model.Proxy,
	w *model.WatchedResource,
	req *model.PushRequest,
) (model.Resources, model.XdsLogDetails, error) {
	if proxy.Type != model.Agentgateway {
		return nil, model.DefaultXdsLogDetails, nil
	}
	resources := g.translateAgentgatewayResources(proxy, req)
	return resources, model.XdsLogDetails{
		AdditionalInfo: "agentgateway resources",
	}, nil
}

// AgentgatewayAddressGenerator emits istio.workload.Address for agentgateway
// proxies. Reuses Istio's existing WorkloadGenerator output but is registered
// under the agentgateway/<typeURL> key so we can scope it later if needed.
type AgentgatewayAddressGenerator struct {
	Inner model.XdsResourceGenerator
}

var _ model.XdsResourceGenerator = &AgentgatewayAddressGenerator{}

func (g *AgentgatewayAddressGenerator) Generate(
	proxy *model.Proxy,
	w *model.WatchedResource,
	req *model.PushRequest,
) (model.Resources, model.XdsLogDetails, error) {
	if g.Inner == nil {
		return nil, model.DefaultXdsLogDetails, nil
	}
	return g.Inner.Generate(proxy, w, req)
}

// translateAgentgatewayResources is implemented in agentgateway_translate.go.
