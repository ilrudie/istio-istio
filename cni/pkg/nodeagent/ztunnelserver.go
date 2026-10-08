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

package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
	v1 "k8s.io/api/core/v1"

	istiolog "istio.io/istio/pkg/log"
	"istio.io/istio/pkg/monitoring"
	"istio.io/istio/pkg/util/sets"
	"istio.io/istio/pkg/zdsapi"
)

var readWriteDeadline = 5 * time.Second

var ztunnelConnected = monitoring.NewGauge("ztunnel_connected",
	"number of connections to ztunnel")

var (
	drainResultLabel = monitoring.CreateLabel("result")
	workloadDrains   = monitoring.NewSum(
		"istio_cni_workload_drains_total",
		"The total number of DrainWorkload messages for terminating pods, by result: acked, ack_error, "+
			"send_error, or skipped (no connected ztunnel supports draining).",
	)
	workloadDrainLatency = monitoring.NewDistribution(
		"istio_cni_workload_drain_latency_seconds",
		"Time from the CNI agent sending a DrainWorkload to ztunnel acking it.",
		[]float64{.001, .005, .01, .05, .1, .5, 1, 5},
		monitoring.WithUnit(monitoring.Seconds),
	)
)

type ZtunnelServer interface {
	Run(ctx context.Context)
	PodDeleted(ctx context.Context, uid string) error
	// PodDraining asks ztunnel to drain the inbound HBONE traffic of a pod that started
	// terminating. It does not wait for ztunnel.
	PodDraining(ctx context.Context, uid string) error
	PodAdded(ctx context.Context, pod *v1.Pod, netns Netns) error
	Close() error
}

/*
To clean up stale ztunnels

	we may need to ztunnel to send its (uid, bootid / boot time) to us
	so that we can remove stale entries when the ztunnel pod is deleted
	or when the ztunnel pod is restarted in the same pod (remove old entries when the same uid connects again, but with different boot id?)

	save a queue of what needs to be sent to the ztunnel pod and send it one by one when it connects.

	when a new ztunnel connects with different uid, only propagate deletes to older ztunnels.
*/

type connMgr struct {
	connectionSet []ZtunnelConnection
	// The optional protocol features each connection's ztunnel advertised in its hello. A
	// connection that has not sent its hello yet has no entry.
	capabilities map[ZtunnelConnection]sets.Set[zdsapi.Capability]
	mu           sync.Mutex
}

// setCapabilities records the capabilities a connection's ztunnel advertised in its hello.
func (c *connMgr) setCapabilities(conn ZtunnelConnection, capabilities []zdsapi.Capability) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capabilities == nil {
		c.capabilities = map[ZtunnelConnection]sets.Set[zdsapi.Capability]{}
	}
	c.capabilities[conn] = sets.New(capabilities...)
}

// hasCapability reports whether a connection's ztunnel advertised a capability.
func (c *connMgr) hasCapability(conn ZtunnelConnection, capability zdsapi.Capability) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capabilities[conn].Contains(capability)
}

func (c *connMgr) addConn(conn ZtunnelConnection) {
	c.mu.Lock()
	defer c.mu.Unlock()
	log := log.WithLabels("conn_uuid", conn.UUID())
	c.connectionSet = append(c.connectionSet, conn)
	log.Infof("new ztunnel connected, total connected: %v", len(c.connectionSet))
	ztunnelConnected.RecordInt(int64(len(c.connectionSet)))
}

func (c *connMgr) LatestConn() (ZtunnelConnection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.connectionSet) == 0 {
		return nil, fmt.Errorf("no connection")
	}
	lConn := c.connectionSet[len(c.connectionSet)-1]
	log.Debugf("latest ztunnel connection is %s, total connected: %v", lConn.UUID(), len(c.connectionSet))
	return lConn, nil
}

func (c *connMgr) deleteConn(conn ZtunnelConnection) {
	log.Debug("ztunnel disconnected")
	close(conn.Done())
	c.mu.Lock()
	defer c.mu.Unlock()
	log := log.WithLabels("conn_uuid", conn.UUID())

	// Loop over the slice, keeping non-deleted conn but
	// filtering out the deleted one.
	var retainedConns []ZtunnelConnection
	for _, existingConn := range c.connectionSet {
		// Not conn that was deleted? Keep it.
		if existingConn != conn {
			retainedConns = append(retainedConns, existingConn)
		}
	}
	c.connectionSet = retainedConns
	delete(c.capabilities, conn)
	log.Infof("ztunnel disconnected, total connected %s", len(c.connectionSet))
	ztunnelConnected.RecordInt(int64(len(c.connectionSet)))
}

// this is used in tests
// nolint: unused
func (c *connMgr) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.connectionSet)
}

type ztunnelServer struct {
	listener net.Listener

	// connections to pod delivered map
	// add pod goes to newest connection
	// delete pod goes to all connections
	conns             *connMgr
	pods              PodNetnsCache
	keepaliveInterval time.Duration

	// drainEnabled turns on DrainWorkload for terminating pods (see PodDraining). When it is off,
	// nothing is ever drained, exactly as before the feature.
	drainEnabled bool
	// draining holds the UIDs of the pods PodDraining was called for and PodDeleted was not, so
	// a ztunnel that connects while they terminate is told to drain them after its snapshot.
	draining   sets.String
	drainingMu sync.Mutex
}

var _ ZtunnelServer = &ztunnelServer{}

func (z *ztunnelServer) Close() error {
	return z.listener.Close()
}

func (z *ztunnelServer) Run(ctx context.Context) {
	context.AfterFunc(ctx, func() { _ = z.Close() })

	// Allow at most 5 requests per second. This is still a ridiculous amount; at most we should have 2 ztunnels on our node,
	// and they will only connect once and persist.
	// However, if they do get in a state where they call us in a loop, we will quickly OOM
	limit := rate.NewLimiter(rate.Limit(5), 1)
	for {
		log.Debug("accepting conn")
		if err := limit.Wait(ctx); err != nil {
			log.Errorf("failed to wait for ztunnel connection: %v", err)
			return
		}
		conn, err := z.accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				log.Debug("listener closed - returning")
				return
			}

			log.Errorf("failed to accept conn: %v", err)
			continue
		}
		log.Debug("connection accepted")
		go func() {
			log := log.WithLabels("conn_uuid", conn.UUID())
			log.Debug("handling conn")
			if err := z.handleConn(ctx, conn); err != nil {
				log.Errorf("failed to handle conn: %v", err)
			}
		}()
	}
}

// ZDS protocol is very simple, for every message sent, and ack is sent.
// the ack only has temporal correlation (i.e. it is the first and only ack msg after the message was sent)
// All this to say, that we want to make sure that message to ztunnel are sent from a single goroutine
// so we don't mix messages and acks.
// nolint: unparam
func (z *ztunnelServer) handleConn(ctx context.Context, conn ZtunnelConnection) error {
	defer conn.Close()

	// before doing anything, add the connection to the list of active connections
	z.conns.addConn(conn)
	defer z.conns.deleteConn(conn)

	log := log.WithLabels("conn_uuid", conn.UUID())

	m, err := conn.ReadHello()
	if err != nil {
		return err
	}

	log.WithLabels("version", m.Version, "capabilities", m.Capabilities).Infof("received hello from ztunnel")
	// Record the capabilities before reading the draining pods in redrainAfterSnapshot: a
	// PodDraining racing with this connection then either sees it as capable, or has already
	// recorded its pod for redrainAfterSnapshot to send.
	z.conns.setCapabilities(conn, m.Capabilities)
	log.Debug("sending snapshot to ztunnel")
	if err := z.sendSnapshot(ctx, conn); err != nil {
		return err
	}
	if err := z.redrainAfterSnapshot(conn); err != nil {
		return err
	}
	for {
		// listen for updates:
		select {
		case update, ok := <-conn.Updates():
			if !ok {
				log.Debug("update channel closed - returning")
				return nil
			}
			log.Debugf("got update to send to ztunnel")
			resp, err := conn.SendMsgAndWaitForAck(update.Update(), update.Fd())
			if err != nil {
				// Two possibilities
				// - we couldn't _write_ to the connection (in which case, this conn is dead)
				// (annoyingly, go's `net.OpErr` is not convertible?)
				if strings.Contains(err.Error(), "sendmsg: broken pipe") {
					log.Error("ztunnel connection broken/unwritable, disposing of this connection")
					update.Resp() <- updateResponse{
						err:  err,
						resp: nil,
					}
					return err
				}
				// if we timed out waiting for a (valid) response, mention and continue, connection may not be trashed
				log.Warnf("timed out waiting for valid ztunnel response: %s", err)

				if resp.GetAck().GetError() != "" {
					// - we wrote, got a response, but ztunnel responded with an `ack` error (in which case, this conn is not dead)
					log.Errorf("ztunnel responded with an ack error: ackErr %s", resp.GetAck().GetError())
				}
			}
			log.Debugf("ztunnel acked")
			// Safety: Resp is buffered, so this will not block
			update.Resp() <- updateResponse{
				err:  err,
				resp: resp,
			}

		case <-time.After(z.keepaliveInterval):
			// do a short read, just to see if the connection to ztunnel is
			// still alive. As ztunnel shouldn't send anything unless we send
			// something first, we expect to get an os.ErrDeadlineExceeded error
			// here if the connection is still alive.
			// note that unlike tcp connections, reading is a good enough test here.
			err := conn.CheckAlive(time.Second / 100)
			switch {
			case !errors.Is(err, os.ErrDeadlineExceeded):
				log.Debugf("ztunnel keepalive failed: %v", err)
				if errors.Is(err, io.EOF) {
					log.Debug("ztunnel EOF")
					return nil
				}
				return err
			case err == nil:
				log.Warn("ztunnel protocol error, unexpected message")
				return fmt.Errorf("ztunnel protocol error, unexpected message")
			default:
				// we get here if error is deadline exceeded, which means ztunnel is alive.
			}

		case <-ctx.Done():
			return nil
		}
	}
}

func podToWorkload(pod *v1.Pod) *zdsapi.WorkloadInfo {
	namespace := pod.ObjectMeta.Namespace
	name := pod.ObjectMeta.Name
	svcAccount := pod.Spec.ServiceAccountName
	return &zdsapi.WorkloadInfo{
		Namespace:      namespace,
		Name:           name,
		ServiceAccount: svcAccount,
	}
}

func (z *ztunnelServer) sendSnapshot(_ context.Context, conn ZtunnelConnection) error {
	snap := z.pods.ReadCurrentPodSnapshot()
	for uid, wl := range snap {
		var resp *zdsapi.WorkloadResponse
		var err error
		log := log.WithLabels("uid", uid)
		if wl.Workload != nil {
			log = log.WithLabels(
				"name", wl.Workload.Name,
				"namespace", wl.Workload.Namespace,
				"serviceAccount", wl.Workload.ServiceAccount)
		}
		if wl.Netns != nil {
			fd := int(wl.Netns.Fd())
			log.Infof("sending pod to ztunnel as part of snapshot")
			resp, err = conn.SendMsgAndWaitForAck(&zdsapi.WorkloadRequest{
				Payload: &zdsapi.WorkloadRequest_Add{
					Add: &zdsapi.AddWorkload{
						Uid:          uid,
						WorkloadInfo: wl.Workload,
					},
				},
			}, &fd)
		} else {
			log.Infof("netns is not available for pod, sending 'keep' to ztunnel")
			resp, err = conn.SendMsgAndWaitForAck(&zdsapi.WorkloadRequest{
				Payload: &zdsapi.WorkloadRequest_Keep{
					Keep: &zdsapi.KeepWorkload{
						Uid: uid,
					},
				},
			}, nil)
		}
		if err != nil {
			return err
		}
		if resp.GetAck().GetError() != "" {
			log.Errorf("add-workload: got ack error: %s", resp.GetAck().GetError())
		}
	}
	resp, err := conn.SendMsgAndWaitForAck(&zdsapi.WorkloadRequest{
		Payload: &zdsapi.WorkloadRequest_SnapshotSent{
			SnapshotSent: &zdsapi.SnapshotSent{},
		},
	}, nil)
	if err != nil {
		return err
	}
	log.Debugf("snapshot sent to ztunnel")
	if resp.GetAck().GetError() != "" {
		log.Errorf("snap-sent: got ack error: %s", resp.GetAck().GetError())
	}

	return nil
}

type updateResponse struct {
	err  error
	resp *zdsapi.WorkloadResponse
}

type UpdateRequest interface {
	Update() *zdsapi.WorkloadRequest
	Fd() *int

	Resp() chan updateResponse
}

type ZtunnelConnection interface {
	Close()
	UUID() uuid.UUID
	Updates() <-chan UpdateRequest
	CheckAlive(timeout time.Duration) error
	ReadHello() (*zdsapi.ZdsHello, error)
	Send(ctx context.Context, data *zdsapi.WorkloadRequest, fd *int) (*zdsapi.WorkloadResponse, error)
	SendMsgAndWaitForAck(msg *zdsapi.WorkloadRequest, fd *int) (*zdsapi.WorkloadResponse, error)
	Done() chan struct{}
}

func (c *connMgr) snapshotConns() []ZtunnelConnection {
	c.mu.Lock()
	defer c.mu.Unlock()
	// return a copy of the slice, so that the caller can iterate over it without holding the lock
	conns := make([]ZtunnelConnection, len(c.connectionSet))
	copy(conns, c.connectionSet)
	return conns
}

// snapshotConnsWith is snapshotConns, limited to connections whose ztunnel advertised a capability.
func (c *connMgr) snapshotConnsWith(capability zdsapi.Capability) []ZtunnelConnection {
	c.mu.Lock()
	defer c.mu.Unlock()
	var conns []ZtunnelConnection
	for _, conn := range c.connectionSet {
		if c.capabilities[conn].Contains(capability) {
			conns = append(conns, conn)
		}
	}
	return conns
}

// PodDeleted sends a pod deletion notification to connected ztunnels.
//
// Note that unlike PodAdded, this deletion event is broadcast to *all*
// currently-connected ztunnels - not just the latest.
// This is intentional, and critical to handle proper shutdown/reconnect
// cycles.
func (z *ztunnelServer) PodDeleted(ctx context.Context, uid string) error {
	r := &zdsapi.WorkloadRequest{
		Payload: &zdsapi.WorkloadRequest_Del{
			Del: &zdsapi.DelWorkload{
				Uid: uid,
			},
		},
	}

	log.Debugf("sending delete pod to all ztunnels: %s %v", uid, r)

	z.drainingMu.Lock()
	z.draining.Delete(uid)
	z.drainingMu.Unlock()

	var delErr []error

	for _, conn := range z.conns.snapshotConns() {
		log := log.WithLabels("conn_uuid", conn.UUID())
		log.Debug("sending msg to connected ztunnel")
		_, err := conn.Send(ctx, r, nil)
		if err != nil {
			delErr = append(delErr, err)
		}
	}
	return errors.Join(delErr...)
}

// PodDraining asks connected ztunnels to drain the inbound HBONE traffic of a pod that started
// terminating. Ztunnel sends a graceful GOAWAY on every inbound HBONE connection to the pod, lets
// the streams already running finish, and refuses (with REFUSED_STREAM) the new CONNECTs whose
// client marked them as retriable elsewhere. It serves every other CONNECT as usual, so a client
// that cannot retry, or has nowhere else to go, is not harmed.
//
// Like PodDeleted, this is broadcast to *all* connected ztunnels, as an older ztunnel that is
// shutting down may still hold connections to the pod. A ztunnel that did not advertise the
// DRAIN_WORKLOAD capability is skipped. One that connects later, before PodDeleted, is sent the
// drain after its snapshot (see redrainAfterSnapshot).
//
// It does not wait for ztunnel. The pod is leaving service discovery at the same moment, so a drain
// only helps if it lands promptly, and one slow ztunnel must neither delay the others nor hold up
// the informer. Failures are logged and counted, and not retried: a late drain is worth nothing,
// and RemovePodFromMesh still follows. It always returns nil.
func (z *ztunnelServer) PodDraining(ctx context.Context, uid string) error {
	if !z.drainEnabled {
		return nil
	}
	// Record the pod before looking for capable connections; see handleConn.
	z.drainingMu.Lock()
	if z.draining == nil {
		z.draining = sets.New[string]()
	}
	z.draining.Insert(uid)
	z.drainingMu.Unlock()

	conns := z.conns.snapshotConnsWith(zdsapi.Capability_DRAIN_WORKLOAD)
	if len(conns) == 0 {
		log.WithLabels("uid", uid).Debug("no connected ztunnel supports draining, skipping drain")
		workloadDrains.With(drainResultLabel.Value("skipped")).Increment()
		return nil
	}
	for _, conn := range conns {
		go z.sendDrain(ctx, conn, uid)
	}
	return nil
}

// sendDrain sends one DrainWorkload through a connection's update loop, and records the outcome.
func (z *ztunnelServer) sendDrain(ctx context.Context, conn ZtunnelConnection, uid string) {
	log := log.WithLabels("conn_uuid", conn.UUID(), "uid", uid)
	// The update loop waits up to readWriteDeadline for each ack, so allow for one message ahead of
	// this one. A drain still queued after that is too late to matter.
	ctx, cancel := context.WithTimeout(ctx, 2*readWriteDeadline)
	defer cancel()
	start := time.Now()
	resp, err := conn.Send(ctx, drainRequest(uid), nil)
	recordDrain(log, start, resp, err)
}

// redrainAfterSnapshot sends a newly connected, drain-capable ztunnel a DrainWorkload for every
// pod in its snapshot that is still draining. This covers a ztunnel that (re)connects, and the CNI
// agent restarting, while pods terminate. Pods no longer in the snapshot are forgotten. It runs
// on the connection's own goroutine, before the update loop, like sendSnapshot.
func (z *ztunnelServer) redrainAfterSnapshot(conn ZtunnelConnection) error {
	if !z.drainEnabled || !z.conns.hasCapability(conn, zdsapi.Capability_DRAIN_WORKLOAD) {
		return nil
	}
	snap := z.pods.ReadCurrentPodSnapshot()
	z.drainingMu.Lock()
	var uids []string
	for uid := range z.draining {
		if _, ok := snap[uid]; ok {
			uids = append(uids, uid)
		} else {
			z.draining.Delete(uid)
		}
	}
	z.drainingMu.Unlock()

	for _, uid := range uids {
		log := log.WithLabels("conn_uuid", conn.UUID(), "uid", uid)
		log.Debug("sending drain for terminating pod after snapshot")
		start := time.Now()
		resp, err := conn.SendMsgAndWaitForAck(drainRequest(uid), nil)
		recordDrain(log, start, resp, err)
		if err != nil {
			return err
		}
	}
	return nil
}

func drainRequest(uid string) *zdsapi.WorkloadRequest {
	return &zdsapi.WorkloadRequest{
		Payload: &zdsapi.WorkloadRequest_Drain{
			Drain: &zdsapi.DrainWorkload{
				Uid: uid,
			},
		},
	}
}

func recordDrain(log *istiolog.Scope, start time.Time, resp *zdsapi.WorkloadResponse, err error) {
	switch {
	case err != nil:
		log.Warnf("failed to send drain to ztunnel: %v", err)
		workloadDrains.With(drainResultLabel.Value("send_error")).Increment()
	case resp.GetAck().GetError() != "":
		log.Errorf("drain-workload: got ack error: %s", resp.GetAck().GetError())
		workloadDrains.With(drainResultLabel.Value("ack_error")).Increment()
	default:
		latency := time.Since(start)
		log.WithLabels("latency", latency).Debug("ztunnel acked pod drain")
		workloadDrains.With(drainResultLabel.Value("acked")).Increment()
		workloadDrainLatency.Record(latency.Seconds())
	}
}
