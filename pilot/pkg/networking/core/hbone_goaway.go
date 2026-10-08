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

package core

import (
	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	lbcommon "github.com/envoyproxy/go-control-plane/envoy/extensions/load_balancing_policies/common/v3"
	leastrequest "github.com/envoyproxy/go-control-plane/envoy/extensions/load_balancing_policies/least_request/v3"
	randomlb "github.com/envoyproxy/go-control-plane/envoy/extensions/load_balancing_policies/random/v3"
	roundrobin "github.com/envoyproxy/go-control-plane/envoy/extensions/load_balancing_policies/round_robin/v3"
	"google.golang.org/protobuf/proto"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pilot/pkg/util/protoconv"
	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/util/protomarshal"
)

// applyHBONEGoAwayPreference wraps supported policies only after transport selection
// has opted the service into the shim. Keep unsupported policies unchanged.
func (cb *ClusterBuilder) applyHBONEGoAwayPreference(c *cluster.Cluster) {
	if !cb.hboneGoAwayPreference || c.GetType() != cluster.Cluster_EDS ||
		c.LoadBalancingPolicy != nil || c.LbSubsetConfig != nil || c.GetCommonLbConfig().GetConsistentHashingLbConfig() != nil {
		return
	}
	locality := hboneLocalityLB(c.CommonLbConfig)
	var child proto.Message
	var name string
	switch c.LbPolicy {
	case cluster.Cluster_ROUND_ROBIN:
		name = "round_robin"
		child = &roundrobin.RoundRobin{LocalityLbConfig: locality, SlowStartConfig: hboneSlowStart(c.GetRoundRobinLbConfig().GetSlowStartConfig())}
	case cluster.Cluster_LEAST_REQUEST:
		name = "least_request"
		legacy := c.GetLeastRequestLbConfig()
		child = &leastrequest.LeastRequest{
			LocalityLbConfig: locality, SlowStartConfig: hboneSlowStart(legacy.GetSlowStartConfig()),
			ChoiceCount: legacy.GetChoiceCount(), ActiveRequestBias: legacy.GetActiveRequestBias(),
		}
	case cluster.Cluster_RANDOM:
		name = "random"
		child = &randomlb.Random{LocalityLbConfig: locality}
	default:
		return
	}
	policy := &cluster.LoadBalancingPolicy{Policies: []*cluster.LoadBalancingPolicy_Policy{{
		TypedExtensionConfig: &core.TypedExtensionConfig{
			Name: "envoy.load_balancing_policies." + name, TypedConfig: protoconv.MessageToAny(child),
		},
	}}}
	fields, err := protomarshal.MessageToStructSlow(policy)
	if err != nil {
		log.Errorf("cannot configure HBONE GOAWAY preference for %s: %v", c.Name, err)
		return
	}
	c.LoadBalancingPolicy = &cluster.LoadBalancingPolicy{Policies: []*cluster.LoadBalancingPolicy_Policy{{
		TypedExtensionConfig: &core.TypedExtensionConfig{
			Name: util.HBONEGoAwayLB,
			TypedConfig: protoconv.TypedStructWithFields(util.HBONEGoAwayLBType, map[string]any{
				"child_policy":           fields.AsMap(),
				"upstream_port_override": model.HBoneInboundListenPort,
			}),
		},
	}}}
	c.LbPolicy = cluster.Cluster_LOAD_BALANCING_POLICY_CONFIG
	c.LbConfig = nil
	// Envoy rejects legacy locality fields alongside a typed LB policy. The child
	// now owns them; retain panic threshold and other cluster-wide settings.
	if c.CommonLbConfig != nil {
		c.CommonLbConfig = protomarshal.Clone(c.CommonLbConfig)
		c.CommonLbConfig.LocalityConfigSpecifier = nil
	}
}

// Match Envoy's legacy-to-typed policy conversion, including warmup and locality.
func hboneLocalityLB(common *cluster.Cluster_CommonLbConfig) *lbcommon.LocalityLbConfig {
	if common.GetLocalityWeightedLbConfig() != nil {
		return &lbcommon.LocalityLbConfig{LocalityConfigSpecifier: &lbcommon.LocalityLbConfig_LocalityWeightedLbConfig_{
			LocalityWeightedLbConfig: &lbcommon.LocalityLbConfig_LocalityWeightedLbConfig{},
		}}
	}
	if zone := common.GetZoneAwareLbConfig(); zone != nil {
		return &lbcommon.LocalityLbConfig{LocalityConfigSpecifier: &lbcommon.LocalityLbConfig_ZoneAwareLbConfig_{
			ZoneAwareLbConfig: &lbcommon.LocalityLbConfig_ZoneAwareLbConfig{
				RoutingEnabled: zone.RoutingEnabled, MinClusterSize: zone.MinClusterSize, FailTrafficOnPanic: zone.FailTrafficOnPanic,
			},
		}}
	}
	return nil
}

func hboneSlowStart(legacy *cluster.Cluster_SlowStartConfig) *lbcommon.SlowStartConfig {
	if legacy == nil {
		return nil
	}
	return &lbcommon.SlowStartConfig{
		SlowStartWindow: legacy.SlowStartWindow, Aggression: legacy.Aggression, MinWeightPercent: legacy.MinWeightPercent,
	}
}
