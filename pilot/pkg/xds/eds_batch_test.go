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
	"testing"

	"go.uber.org/atomic"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/provider"
	"istio.io/istio/pkg/config/schema/kind"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/util/sets"
)

func TestEDSUpdateBatchPushRequests(t *testing.T) {
	endpoint := func(address, serviceAccount string) *model.IstioEndpoint {
		return &model.IstioEndpoint{
			Addresses:       []string{address},
			EndpointPort:    8080,
			ServicePortName: "http",
			ServiceAccount:  serviceAccount,
		}
	}
	endpointUpdate := func(hostname string, endpoints ...*model.IstioEndpoint) model.EndpointsUpdate {
		return model.EndpointsUpdate{
			Hostname:  hostname,
			Namespace: "default",
			Endpoints: endpoints,
		}
	}
	configKey := func(hostname string, k kind.Kind) model.ConfigKey {
		return model.ConfigKey{Kind: k, Name: hostname, Namespace: "default"}
	}
	// A ServiceEntry exposes one workload under two hostnames. Each batch models
	// a single workload event fanning out to those hosts, not separate workload events.
	workload := endpoint("10.0.0.1", "sa")
	movedWorkload := endpoint("10.0.1.1", "sa")
	addedWorkload := endpoint("10.0.0.2", "new-sa")
	// An overlapping ServiceEntry contributes this existing workload only to host a.
	overlappingWorkload := endpoint("10.0.0.3", "new-sa")
	initial := []model.EndpointsUpdate{
		endpointUpdate("a.example.com", workload),
		endpointUpdate("b.example.com", workload),
	}
	for _, tt := range []struct {
		name    string
		initial []model.EndpointsUpdate
		updates []model.EndpointsUpdate
		want    sets.Set[model.ConfigKey]
	}{
		{
			name: "workload address changes for both hosts",
			updates: []model.EndpointsUpdate{
				endpointUpdate("a.example.com", movedWorkload),
				endpointUpdate("b.example.com", movedWorkload),
			},
			want: sets.New(configKey("a.example.com", kind.Endpoints), configKey("b.example.com", kind.Endpoints)),
		},
		{
			name: "added workload introduces service account only for host b",
			initial: []model.EndpointsUpdate{
				endpointUpdate("a.example.com", workload, overlappingWorkload),
				endpointUpdate("b.example.com", workload),
			},
			updates: []model.EndpointsUpdate{
				endpointUpdate("a.example.com", workload, overlappingWorkload, addedWorkload),
				endpointUpdate("b.example.com", workload, addedWorkload),
			},
			want: sets.New(configKey("a.example.com", kind.Endpoints), configKey("b.example.com", kind.ServiceEntry)),
		},
		{
			name: "added workload introduces service account for both hosts",
			updates: []model.EndpointsUpdate{
				endpointUpdate("a.example.com", workload, addedWorkload),
				endpointUpdate("b.example.com", workload, addedWorkload),
			},
			want: sets.New(configKey("a.example.com", kind.ServiceEntry), configKey("b.example.com", kind.ServiceEntry)),
		},
		{
			name: "workload removed from both hosts",
			updates: []model.EndpointsUpdate{
				endpointUpdate("a.example.com"),
				endpointUpdate("b.example.com"),
			},
			want: sets.New(configKey("a.example.com", kind.Endpoints), configKey("b.example.com", kind.Endpoints)),
		},
		{
			name:    "all unchanged",
			updates: initial,
		},
		{
			name: "empty batch",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			index := model.NewEndpointIndex(model.DisabledCache{})
			shard := model.ShardKey{Provider: provider.External, Cluster: "cluster-1"}
			seed := tt.initial
			if seed == nil {
				seed = initial
			}
			for _, u := range seed {
				index.UpdateServiceEndpoints(shard, u.Hostname, u.Namespace, u.Endpoints, false)
			}
			// Inspect requests before debounce or the push queue can coalesce them and
			// hide a regression that emits one request per hostname or push type.
			s := &DiscoveryServer{
				Env:            &model.Environment{EndpointIndex: index},
				InboundUpdates: atomic.NewInt64(0),
				pushChannel:    make(chan *model.PushRequest, len(tt.updates)+1),
			}
			s.EDSUpdateBatch(shard, tt.updates)

			if len(tt.want) == 0 {
				assert.Equal(t, len(s.pushChannel), 0)
				return
			}
			if got := len(s.pushChannel); got != 1 {
				t.Fatalf("expected one push request, got %d", got)
			}
			assert.Equal(t, <-s.pushChannel, &model.PushRequest{
				ConfigsUpdated: tt.want,
				Reason:         model.NewReasonStats(model.EndpointUpdate),
			})
		})
	}
}
