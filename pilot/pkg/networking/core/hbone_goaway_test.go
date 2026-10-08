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
	"testing"
	"time"

	udpa "github.com/cncf/xds/go/udpa/type/v1"
	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	leastrequest "github.com/envoyproxy/go-control-plane/envoy/extensions/load_balancing_policies/least_request/v3"
	xdstype "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/util/protomarshal"
)

func goawayChild(t *testing.T, c *cluster.Cluster) *cluster.LoadBalancingPolicy {
	t.Helper()
	assert.Equal(t, c.LbPolicy, cluster.Cluster_LOAD_BALANCING_POLICY_CONFIG)
	outer := c.LoadBalancingPolicy.GetPolicies()[0].TypedExtensionConfig
	assert.Equal(t, outer.Name, util.HBONEGoAwayLB)
	wrapped := &udpa.TypedStruct{}
	assert.NoError(t, outer.TypedConfig.UnmarshalTo(wrapped))
	assert.Equal(t, wrapped.TypeUrl, util.HBONEGoAwayLBType)
	assert.Equal(t, wrapped.Value.Fields["upstream_port_override"].GetNumberValue(), float64(model.HBoneInboundListenPort))
	child := &cluster.LoadBalancingPolicy{}
	assert.NoError(t, protomarshal.StructToMessageSlow(wrapped.Value.Fields["child_policy"].GetStructValue(), child))
	return child
}

func TestHBONEGoAwayPolicyConversion(t *testing.T) {
	cb := &ClusterBuilder{hboneGoAwayPreference: true}
	for _, policy := range []cluster.Cluster_LbPolicy{cluster.Cluster_ROUND_ROBIN, cluster.Cluster_LEAST_REQUEST, cluster.Cluster_RANDOM} {
		t.Run(policy.String(), func(t *testing.T) {
			common := &cluster.Cluster_CommonLbConfig{
				HealthyPanicThreshold: &xdstype.Percent{Value: 10},
				LocalityConfigSpecifier: &cluster.Cluster_CommonLbConfig_ZoneAwareLbConfig_{
					ZoneAwareLbConfig: &cluster.Cluster_CommonLbConfig_ZoneAwareLbConfig{
						MinClusterSize: wrapperspb.UInt64(2), RoutingEnabled: &xdstype.Percent{Value: 80}, FailTrafficOnPanic: true,
					},
				},
			}
			c := &cluster.Cluster{ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_EDS}, LbPolicy: policy, CommonLbConfig: common}
			if policy == cluster.Cluster_LEAST_REQUEST {
				c.LbConfig = &cluster.Cluster_LeastRequestLbConfig_{LeastRequestLbConfig: &cluster.Cluster_LeastRequestLbConfig{
					ChoiceCount: wrapperspb.UInt32(3), ActiveRequestBias: &core.RuntimeDouble{DefaultValue: 2},
					SlowStartConfig: &cluster.Cluster_SlowStartConfig{SlowStartWindow: durationpb.New(20 * time.Second)},
				}}
			}
			cb.applyHBONEGoAwayPreference(c)
			child := goawayChild(t, c)
			assert.Equal(t, c.CommonLbConfig.HealthyPanicThreshold, common.HealthyPanicThreshold)
			assert.Equal(t, c.CommonLbConfig.GetZoneAwareLbConfig() == nil, true)
			assert.Equal(t, common.GetZoneAwareLbConfig() != nil, true)
			assert.Equal(t, c.LbConfig == nil, true)
			if policy == cluster.Cluster_LEAST_REQUEST {
				lr := &leastrequest.LeastRequest{}
				assert.NoError(t, child.Policies[0].TypedExtensionConfig.TypedConfig.UnmarshalTo(lr))
				assert.Equal(t, lr.ChoiceCount.GetValue(), uint32(3))
				assert.Equal(t, lr.ActiveRequestBias.DefaultValue, float64(2))
				assert.Equal(t, lr.SlowStartConfig.SlowStartWindow.AsDuration(), 20*time.Second)
				assert.Equal(t, lr.LocalityLbConfig.GetZoneAwareLbConfig().MinClusterSize.GetValue(), uint64(2))
				assert.Equal(t, lr.LocalityLbConfig.GetZoneAwareLbConfig().FailTrafficOnPanic, true)
			}
		})
	}
	for _, policy := range []cluster.Cluster_LbPolicy{cluster.Cluster_RING_HASH, cluster.Cluster_MAGLEV, cluster.Cluster_CLUSTER_PROVIDED} {
		c := &cluster.Cluster{ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_EDS}, LbPolicy: policy}
		before := protomarshal.Clone(c)
		cb.applyHBONEGoAwayPreference(c)
		assert.Equal(t, c, before)
	}
	for _, c := range []*cluster.Cluster{
		{ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_STATIC}},
		{ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_EDS}, LbSubsetConfig: &cluster.Cluster_LbSubsetConfig{}},
		{ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_EDS}, LoadBalancingPolicy: &cluster.LoadBalancingPolicy{}},
	} {
		before := protomarshal.Clone(c)
		cb.applyHBONEGoAwayPreference(c)
		assert.Equal(t, c, before)
	}
}

func TestHBONEGoAwayOptInAndCacheIsolation(t *testing.T) {
	test.SetForTest(t, &features.EnableHBONESend, true)
	test.SetForTest(t, &features.PreferHBONESend, true)
	test.SetForTest(t, &features.EnableAmbientMultiNetwork, false)
	svc := &model.Service{Hostname: "app.default.svc.cluster.local", Resolution: model.ClientSideLB,
		Attributes: model.ServiceAttributes{Namespace: "default"}}
	port := &model.Port{Name: "http", Port: 80, Protocol: protocol.HTTP}
	cg := NewConfigGenTest(t, TestOptions{Services: []*model.Service{svc}})
	for _, kind := range []model.NodeType{model.Waypoint, model.Router} {
		var originalKey any
		for _, shim := range []bool{false, true} {
			for _, preference := range []bool{false, true} {
				proxy := shimTestProxy(cg, kind, shim)
				proxy.Metadata.EnableHBONEGoAwayPreference = model.StringBool(preference)
				cb := NewClusterBuilder(proxy, &model.PushRequest{Push: cg.PushContext()}, model.NewXdsCache())
				outer := cb.buildWaypointConnectOriginate(proxy, cg.PushContext())
				assert.Equal(t, outer.TypedExtensionProtocolOptions[util.HBONEGoAwayOptions] != nil, shim && preference)
				var c *cluster.Cluster
				if kind == model.Waypoint {
					c = cb.buildWaypointInboundVIPCluster(proxy, svc, *port, "http", cg.PushContext().Mesh, nil, nil)
				} else {
					// Exercise the same traffic-policy path used for gateway CDS.
					c = &cluster.Cluster{ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_EDS}}
					cb.applyTrafficPolicy(svc, buildClusterOpts{
						mutable: &clusterWrapper{cluster: c}, port: port,
						mesh: cg.PushContext().Mesh, direction: model.TrafficDirectionOutbound,
					})
				}
				assert.Equal(t, c.LoadBalancingPolicy != nil, shim && preference)
				entry := buildClusterKey(svc, port, cb, proxy, nil)
				key := entry.Key()
				if shim && !preference {
					originalKey = key
				}
				if shim && preference && key == originalKey {
					t.Fatal("GOAWAY preference shares the original shim CDS cache key")
				}
			}
		}
	}
}
