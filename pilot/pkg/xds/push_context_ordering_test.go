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
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"

	meshconfig "istio.io/api/mesh/v1alpha1"
	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/core"
	v3 "istio.io/istio/pilot/pkg/xds/v3"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/config/schema/kind"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/util/sets"
)

// Pause a context-reusing push after it captures the global context, before fan-out.
// This models a goroutine being descheduled at that point without production hooks.
type addressPushBarrier struct {
	model.DisabledCache
	captured chan struct{}
	resume   chan struct{}
	kind     kind.Kind
}

type orderingSotwStream struct {
	grpc.ServerStream
	responses []*discovery.DiscoveryResponse
}

func (s *orderingSotwStream) Context() context.Context                   { return context.Background() }
func (s *orderingSotwStream) Recv() (*discovery.DiscoveryRequest, error) { return nil, io.EOF }
func (s *orderingSotwStream) Send(r *discovery.DiscoveryResponse) error {
	s.responses = append(s.responses, r)
	return nil
}

type orderingDeltaStream struct {
	grpc.ServerStream
	responses []*discovery.DeltaDiscoveryResponse
}

func (s *orderingDeltaStream) Context() context.Context                        { return context.Background() }
func (s *orderingDeltaStream) Recv() (*discovery.DeltaDiscoveryRequest, error) { return nil, io.EOF }
func (s *orderingDeltaStream) Send(r *discovery.DeltaDiscoveryResponse) error {
	s.responses = append(s.responses, r)
	return nil
}

// Exercise actual CDS generation and response sending, including normal proxy filtering,
// sidecar-scope recomputation, XDS caching, and ACK processing.
func TestReusedPushContextOrderingCDS(t *testing.T) {
	for _, updateKind := range []kind.Kind{kind.Endpoints, kind.Address} {
		t.Run(updateKind.String(), func(t *testing.T) {
			for _, delta := range []bool{false, true} {
				for _, state := range []string{"in-order", "pending", "delivered", "delivered-cache-miss", "before-config-delivery"} {
					t.Run(fmt.Sprintf("delta=%v/%s", delta, state), func(t *testing.T) {
						alreadyDelivered := strings.HasPrefix(state, "delivered")
						buildContext := func(timeout int) *core.ConfigGenTest {
							return core.NewConfigGenTest(t, core.TestOptions{
								Services: []*model.Service{{
									Hostname:   host.Name("app.default.svc.cluster.local"),
									Ports:      model.PortList{&model.Port{Name: "http", Port: 80, Protocol: protocol.HTTP}},
									Attributes: model.ServiceAttributes{Name: "app", Namespace: "default"},
								}},
								ConfigString: fmt.Sprintf(`
apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: app
  namespace: default
spec:
  host: app.default.svc.cluster.local
  trafficPolicy:
    connectionPool:
      tcp:
        connectTimeout: %ds
`, timeout),
							})
						}
						oldState, newState := buildContext(1), buildContext(9)
						old, latest := oldState.PushContext(), newState.PushContext()
						old.PushVersion, latest.PushVersion = "old", "new"
						old.Generation, latest.Generation = 1, 2
						proxy := oldState.SetupProxy(nil)
						proxy.LastPushContext = old
						proxy.WatchedResources = map[string]*model.WatchedResource{
							v3.ClusterType: {TypeUrl: v3.ClusterType, Wildcard: true, ResourceNames: sets.New[string]()},
						}
						sotwStream, deltaStream := &orderingSotwStream{}, &orderingDeltaStream{}
						con := newConnection("", sotwStream)
						if delta {
							con = newDeltaConnection("", deltaStream)
						}
						con.SetID("test")
						con.proxy = proxy
						cache := model.NewXdsCache()
						barrier := &addressPushBarrier{captured: make(chan struct{}), resume: make(chan struct{}), kind: updateKind}
						s := &DiscoveryServer{
							Env: oldState.Env(), Cache: barrier,
							pushQueue: NewPushQueue(), adsClients: map[string]*Connection{con.ID(): con},
							ProxyNeedsPush: DefaultProxyNeedsPush,
							Generators: map[string]model.XdsResourceGenerator{
								v3.ClusterType: &CdsGenerator{ConfigGenerator: core.NewConfigGenerator(cache)},
							},
						}
						defer s.pushQueue.ShutDown()
						done := make(chan struct{})
						go func() {
							defer close(done)
							s.Push(&model.PushRequest{ConfigsUpdated: sets.New(model.ConfigKey{Kind: updateKind, Name: "pod"})}, false)
						}()
						<-barrier.captured
						if state == "in-order" {
							close(barrier.resume)
							<-done
						}
						s.Env.SetPushContext(latest)
						cache.ClearAll()
						configPush := func() {
							s.AdsPushAll(&model.PushRequest{
								Push:           latest,
								ConfigsUpdated: sets.New(model.ConfigKey{Kind: kind.DestinationRule, Name: "app", Namespace: "default"}),
							})
						}
						if state != "before-config-delivery" {
							configPush()
						}
						deliver := func() {
							_, req, _ := s.pushQueue.Dequeue()
							if delta {
								assert.NoError(t, s.pushConnectionDelta(con, &Event{pushRequest: req}))
							} else {
								assert.NoError(t, s.pushConnection(con, &Event{pushRequest: req}))
							}
							s.pushQueue.MarkDone(con)
						}
						responseCount := func() int {
							if delta {
								return len(deltaStream.responses)
							}
							return len(sotwStream.responses)
						}
						checkResponse := func() {
							t.Helper()
							var resources []*anypb.Any
							var version string
							if delta {
								r := deltaStream.responses[len(deltaStream.responses)-1]
								version = r.SystemVersionInfo
								for _, resource := range r.Resources {
									resources = append(resources, resource.Resource)
								}
							} else {
								r := sotwStream.responses[len(sotwStream.responses)-1]
								version, resources = r.VersionInfo, r.Resources
							}
							for _, resource := range resources {
								c := &cluster.Cluster{}
								assert.NoError(t, resource.UnmarshalTo(c))
								if c.Name == "outbound|80||app.default.svc.cluster.local" {
									t.Logf("sent version=%s connectTimeout=%s", version, c.ConnectTimeout.AsDuration())
									if got := c.ConnectTimeout.AsDuration(); got != 9*time.Second {
										t.Errorf("sent stale connectTimeout: got %s, want 9s", got)
									}
									return
								}
							}
							t.Fatal("missing app cluster")
						}
						requestClusters := func() {
							if delta {
								proxy.DeleteWatchedResource(v3.ClusterType)
								assert.NoError(t, s.processDeltaRequest(&discovery.DeltaDiscoveryRequest{
									TypeUrl: v3.ClusterType, ResourceNamesSubscribe: []string{"outbound|80||app.default.svc.cluster.local"},
								}, con))
							} else {
								assert.NoError(t, s.processRequest(&discovery.DiscoveryRequest{TypeUrl: v3.ClusterType}, con))
							}
						}
						if alreadyDelivered {
							deliver()
							checkResponse()
						}
						if state != "in-order" {
							close(barrier.resume)
							<-done
						}
						deliver()
						if state == "before-config-delivery" {
							// Global B has invalidated the cache, but this proxy still has A. The delayed
							// bypass gets a newer Start without changing contexts at delivery.
							assert.Equal(t, proxy.LastPushContext.Generation, old.Generation)
							assert.Equal(t, proxy.LastPushSkipCacheWrite, true)
							requestClusters()
							assert.Equal(t, responseCount(), 1)
							assert.Equal(t, len(cache.Keys(model.CDSType)), 0)
							configPush()
							deliver()
							assert.Equal(t, proxy.LastPushSkipCacheWrite, false)
							if len(cache.Keys(model.CDSType)) == 0 {
								t.Fatal("new normal push did not resume cache writes")
							}
						}
						if alreadyDelivered {
							// Sidecars skip CDS for endpoint/address-only pushes, but a subsequent client request
							// uses LastPushContext. Request CDS again to see if the stale context is repaired.
							assert.Equal(t, responseCount(), 1)
							if state == "delivered-cache-miss" {
								// A cache hit may mask stale context selection, but entries can be evicted.
								cache.ClearAll()
							}
							requestClusters()
							if state == "delivered-cache-miss" {
								assert.Equal(t, len(cache.Keys(model.CDSType)), 0)
							}
						}
						if state == "pending" || state == "in-order" {
							assert.Equal(t, len(cache.Keys(model.CDSType)), 0)
						}
						checkResponse()
						// A valid ACK is accepted, with no compensating push queued or response generated.
						count := responseCount()
						nonce := proxy.GetWatchedResource(v3.ClusterType).NonceSent
						if delta {
							assert.NoError(t, s.processDeltaRequest(&discovery.DeltaDiscoveryRequest{
								TypeUrl: v3.ClusterType, ResponseNonce: nonce,
							}, con))
						} else {
							assert.NoError(t, s.processRequest(&discovery.DiscoveryRequest{
								TypeUrl: v3.ClusterType, ResponseNonce: nonce,
							}, con))
						}
						assert.Equal(t, responseCount(), count)
						assert.Equal(t, proxy.GetWatchedResource(v3.ClusterType).NonceAcked, nonce)
						assert.Equal(t, s.pushQueue.Pending(), 0)
						t.Log("ACK accepted without a corrective response or queued push")
					})
				}
			}
		})
	}
}

func TestDirectPushSkipsCacheWrites(t *testing.T) {
	for _, broadcast := range []bool{false, true} {
		t.Run(fmt.Sprint(broadcast), func(t *testing.T) {
			env := model.NewEnvironment()
			con := newConnection("", nil)
			con.SetID("test")
			con.proxy = &model.Proxy{Metadata: &model.NodeMetadata{}, IPAddresses: []string{"1.2.3.4"}}
			con.MarkInitialized()
			s := &DiscoveryServer{Env: env, pushQueue: NewPushQueue(), adsClients: map[string]*Connection{con.ID(): con}}
			defer s.pushQueue.ShutDown()
			if broadcast {
				AdsPushAll(s)
			} else {
				s.ProxyUpdate("", "1.2.3.4")
			}
			_, req, shutdown := s.pushQueue.Dequeue()
			assert.Equal(t, shutdown, false)
			assert.Equal(t, req.SkipCacheWrite, true)
			assert.Equal(t, req.Forced, true)
			if req.Push != env.PushContext() {
				t.Fatal("direct push did not reuse the global context")
			}
		})
	}
}

func (c *addressPushBarrier) Clear(configs sets.Set[model.ConfigKey]) {
	blockedKind := c.kind
	if blockedKind == 0 {
		blockedKind = kind.Address
	}
	if model.OnlyHasConfigsOfKind(configs, blockedKind) {
		close(c.captured)
		<-c.resume
	}
}

func TestConcurrentEDSReusePreservesDestinationRule(t *testing.T) {
	f := core.NewConfigGenTest(t, core.TestOptions{
		Services: []*model.Service{{
			Hostname:   host.Name("app.default.svc.cluster.local"),
			Ports:      model.PortList{&model.Port{Name: "http", Port: 80, Protocol: protocol.HTTP}},
			Attributes: model.ServiceAttributes{Name: "app", Namespace: "default"},
		}},
		ConfigString: `
apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: app
  namespace: default
spec:
  host: app.default.svc.cluster.local
  trafficPolicy:
    connectionPool:
      tcp:
        connectTimeout: 1s
`,
	})
	proxy := f.SetupProxy(nil)
	barrier := &addressPushBarrier{captured: make(chan struct{}), resume: make(chan struct{}), kind: kind.Endpoints}
	s := &DiscoveryServer{Env: f.Env(), Cache: barrier, pushQueue: NewPushQueue()}
	defer s.pushQueue.ShutDown()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The debounce-disabled EDS branch calls Push concurrently with config pushes.
		s.Push(&model.PushRequest{ConfigsUpdated: sets.New(model.ConfigKey{
			Kind: kind.Endpoints, Name: "app.default.svc.cluster.local", Namespace: "default",
		})}, false)
	}()
	<-barrier.captured // EDS has captured the old context, but has not enqueued it yet.
	cfg := f.Store().Get(gvk.DestinationRule, "app", "default").DeepCopy()
	cfg.Spec.(*networking.DestinationRule).TrafficPolicy.ConnectionPool.Tcp.ConnectTimeout.Seconds = 9
	_, err := f.Store().Update(cfg)
	assert.NoError(t, err)
	s.Push(&model.PushRequest{ConfigsUpdated: sets.New(model.ConfigKey{
		Kind: kind.DestinationRule, Name: "app", Namespace: "default",
	})}, true)
	timeout := func(push *model.PushContext) time.Duration {
		proxy.SetSidecarScope(push)
		dr := proxy.SidecarScope.DestinationRule(model.TrafficDirectionOutbound, proxy, "app.default.svc.cluster.local")
		return dr.GetRule().Spec.(*networking.DestinationRule).TrafficPolicy.ConnectionPool.Tcp.ConnectTimeout.AsDuration()
	}
	assert.Equal(t, timeout(s.Env.PushContext()), 9*time.Second)
	close(barrier.resume)
	<-done
	assert.Equal(t, s.Env.PushContext().Generation, uint64(1))
	if got := timeout(s.Env.PushContext()); got != 9*time.Second {
		t.Errorf("EDS publication reverted destination rule: got %s, want 9s", got)
	}

	// Subsequent builds must continue to use the updated index.
	s.Push(&model.PushRequest{ConfigsUpdated: sets.New(model.ConfigKey{Kind: kind.Telemetry, Name: "unrelated"})}, true)
	if got := timeout(s.Env.PushContext()); got != 9*time.Second {
		t.Errorf("unrelated push retained stale destination rule: got %s, want 9s", got)
	}
}

func TestPushRequestForProxy(t *testing.T) {
	old := &model.PushContext{Generation: 1}
	latest := &model.PushContext{Generation: 2}
	lastPushTime := time.Now()
	proxy := &model.Proxy{LastPushContext: latest, LastPushTime: lastPushTime}
	for _, start := range []time.Time{lastPushTime.Add(-time.Second), lastPushTime.Add(time.Second)} {
		req := &model.PushRequest{
			Push: old, Start: start,
			ConfigsUpdated:   sets.New(model.ConfigKey{Kind: kind.Address, Name: "pod"}),
			AddressesUpdated: sets.New("pod"),
		}
		got := pushRequestForProxy(proxy, req)
		if got == req || got.Push != latest || req.Push != old {
			t.Fatal("must use a proxy-local copy with the newer context")
		}
		assert.Equal(t, req.Start, start)
		assert.Equal(t, got.Start, start)
		assert.Equal(t, got.SkipCacheWrite, true)
		assert.Equal(t, req.SkipCacheWrite, false)
		assert.Equal(t, got.ConfigsUpdated, req.ConfigsUpdated)
		assert.Equal(t, got.AddressesUpdated, req.AddressesUpdated)
	}
}

func TestCacheWriteSuppressionAfterEndpointPush(t *testing.T) {
	for _, delta := range []bool{false, true} {
		t.Run(fmt.Sprint(delta), func(t *testing.T) {
			old := &model.PushContext{Generation: 1}
			latest := &model.PushContext{Generation: 2}
			con := newConnection("", nil)
			con.proxy = &model.Proxy{Type: model.Ztunnel, LastPushContext: old}
			s := &DiscoveryServer{ProxyNeedsPush: DefaultProxyNeedsPush}
			deliver := func(req *model.PushRequest) {
				if delta {
					assert.NoError(t, s.pushConnectionDelta(con, &Event{pushRequest: req}))
				} else {
					assert.NoError(t, s.pushConnection(con, &Event{pushRequest: req}))
				}
			}
			// Endpoint-only delivery skips computeProxyState. Suppression must still stick.
			deliver(&model.PushRequest{
				Push: old, Start: time.Now(), SkipCacheWrite: true,
				ConfigsUpdated: sets.New(model.ConfigKey{Kind: kind.Endpoints}),
			})
			assert.Equal(t, con.proxy.LastPushSkipCacheWrite, true)
			// Another request reusing that context cannot restore cache writes.
			req := &model.PushRequest{Push: old, Start: time.Now(), ConfigsUpdated: sets.New(model.ConfigKey{Kind: kind.Address})}
			deliver(req)
			assert.Equal(t, con.proxy.LastPushSkipCacheWrite, true)
			assert.Equal(t, req.SkipCacheWrite, false) // shared request was not changed
			// A new normal context delivery can restore writes.
			deliver(&model.PushRequest{
				Push: latest, Start: time.Now(),
				ConfigsUpdated: sets.New(model.ConfigKey{Kind: kind.AuthorizationPolicy}),
			})
			assert.Equal(t, con.proxy.LastPushSkipCacheWrite, false)
			assert.Equal(t, con.proxy.LastPushContext.Generation, uint64(2))
		})
	}
}

func TestAddressPushContextOrdering(t *testing.T) {
	for _, delta := range []bool{false, true} {
		protocol := "sotw"
		if delta {
			protocol = "delta"
		}
		for _, state := range []string{"in-order", "pending", "processing", "delivered"} {
			t.Run(protocol+"/"+state, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					old := model.NewPushContext()
					old.PushVersion = "old"
					old.Generation = 1
					old.Mesh = &meshconfig.MeshConfig{TrustDomain: "old.example"}
					latest := model.NewPushContext()
					latest.PushVersion = "new"
					latest.Generation = 2
					latest.Mesh = &meshconfig.MeshConfig{TrustDomain: "new.example"}
					env := model.NewEnvironment()
					env.SetPushContext(old)
					barrier := &addressPushBarrier{captured: make(chan struct{}), resume: make(chan struct{})}
					con := newConnection("", nil)
					con.SetID("test")
					con.proxy = &model.Proxy{Type: model.Ztunnel, LastPushContext: old}
					s := &DiscoveryServer{
						Env:        env,
						Cache:      barrier,
						pushQueue:  NewPushQueue(),
						adsClients: map[string]*Connection{con.ID(): con},
						ProxyNeedsPush: func(_ *model.Proxy, req *model.PushRequest) (*model.PushRequest, bool) {
							// This is the context presented to downstream push filtering/generation.
							if req.Push.Mesh.TrustDomain != "new.example" {
								t.Errorf("delivery uses stale config: trust domain %q", req.Push.Mesh.TrustDomain)
							}
							return req, true
						},
					}
					defer s.pushQueue.ShutDown()
					address := model.ConfigKey{Kind: kind.Address, Name: "pod"}
					config := model.ConfigKey{Kind: kind.AuthorizationPolicy, Name: "policy"}
					go s.Push(&model.PushRequest{
						ConfigsUpdated:   sets.New(address),
						AddressesUpdated: sets.New("pod"),
					}, false)
					<-barrier.captured
					if state == "in-order" {
						// Control: the old request reaches the queue before the new config push.
						close(barrier.resume)
						synctest.Wait()
					}

					if state == "processing" {
						// Hold an earlier push in progress so both new events merge in processing.
						s.pushQueue.Enqueue(con, &model.PushRequest{Push: old})
						s.pushQueue.Dequeue()
					}
					// Model the publication and fan-out at the end of a concurrent config push.
					env.SetPushContext(latest)
					s.AdsPushAll(&model.PushRequest{Push: latest, ConfigsUpdated: sets.New(config)})
					deliver := func() *model.PushRequest {
						_, req, shutdown := s.pushQueue.Dequeue()
						assert.Equal(t, shutdown, false)
						ev := &Event{pushRequest: req}
						if delta {
							assert.NoError(t, s.pushConnectionDelta(con, ev))
						} else {
							assert.NoError(t, s.pushConnection(con, ev))
						}
						s.pushQueue.MarkDone(con)
						return req
					}
					if state == "delivered" {
						deliver()
					}
					if state != "in-order" {
						close(barrier.resume)
						synctest.Wait()
					}
					if state == "processing" {
						s.pushQueue.MarkDone(con)
					}
					got := deliver()
					assert.Equal(t, got.AddressesUpdated, sets.New("pod"))
					if state != "delivered" {
						assert.Equal(t, got.ConfigsUpdated, sets.New(config, address))
					}
					assert.Equal(t, s.pushQueue.Pending(), 0)
					assert.Equal(t, env.PushContext().PushVersion, "new")
					assert.Equal(t, con.proxy.LastPushContext.PushVersion, "new")
				})
			})
		}
	}
}
