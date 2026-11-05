// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/wg"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// netMesh is a mesh device on a fake network of UDP flows, one per port
// of the device and node: a peer handshakes (its last handshake becomes
// the time of a Status) while its flow passes. A flow that an outage
// carried packets in, or that a node's restart left unanswered, stays
// broken, as a stateful network keeps a flow that packets keep alive;
// only another flow, from another port, gets through. The keys are the
// peers'.
type netMesh struct {
	mu      sync.Mutex
	port    int
	peers   []wg.PeerState
	down    bool
	broken  map[netFlow]bool
	configs []MeshConfig
	sets    [][]wg.Peer
}

type netFlow struct {
	port int
	key  wg.Key
}

func newNetMesh() *netMesh { return &netMesh{broken: map[netFlow]bool{}} }

func (m *netMesh) Configure(c MeshConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configs = append(m.configs, c)
	m.port = c.ListenPort
	return nil
}

// SetPeers keeps the peers that stay with their handshakes, as the kernel
// does; a new peer has none.
func (m *netMesh) SetPeers(ps []wg.Peer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sets = append(m.sets, slices.Clone(ps))
	next := make([]wg.PeerState, 0, len(ps))
	for _, p := range ps {
		st := wg.PeerState{Peer: p}
		if i := slices.IndexFunc(m.peers, func(h wg.PeerState) bool { return h.PublicKey == p.PublicKey }); i >= 0 {
			st.LastHandshake = m.peers[i].LastHandshake
		}
		next = append(next, st)
	}
	m.peers = next
	return nil
}

func (m *netMesh) Status() (wg.Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for i := range m.peers {
		f := netFlow{m.port, m.peers[i].PublicKey}
		switch {
		case m.down:
			m.broken[f] = true
		case !m.broken[f]:
			m.peers[i].LastHandshake = now
		}
	}
	return wg.Device{Name: DefaultMeshDevice, ListenPort: m.port, Peers: slices.Clone(m.peers)}, nil
}

// breakFlow leaves the flow from the device's port to the peer k broken.
func (m *netMesh) breakFlow(k wg.Key) {
	m.mu.Lock()
	m.broken[netFlow{m.port, k}] = true
	m.mu.Unlock()
}

func (m *netMesh) setDown(down bool) {
	m.mu.Lock()
	m.down = down
	m.mu.Unlock()
}

func (m *netMesh) currentPort() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.port
}

// removedAndSetAgain reports whether some SetPeers left k out, and
// whether a later one set it again with endpoint ep.
func (m *netMesh) removedAndSetAgain(k wg.Key, ep string) (removed, again bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, set := range m.sets {
		i := slices.IndexFunc(set, func(p wg.Peer) bool { return p.PublicKey == k })
		switch {
		case i < 0:
			removed = true
		case i >= 0 && removed && set[i].Endpoint.String() == ep:
			again = true
		}
	}
	return removed, again
}

// repairTiming makes a node lost 300 ms after its last handshake, and
// repairs at most every 300 ms.
func repairTiming(tm *timing) {
	tm.meshStale, tm.meshRepairEvery = 300*time.Millisecond, 300*time.Millisecond
}

// twoNodes puts two nodes into the fake's mesh at version v and an
// environment with an endpoint on each.
func twoNodes(t *testing.T, h *harness, v uint64) (k1, k2 wg.Key) {
	t.Helper()
	k1, k2 = newWGKey(t).PublicKey(), newWGKey(t).PublicKey()
	h.fake.SetMesh(wire.Mesh{Version: v, Peers: []wire.Peer{
		{Name: "n1", PublicKey: k1[:], Endpoint: "10.250.0.2:51820", Prefix: "fd00:1:2::1:0:0/96"},
		{Name: "n2", PublicKey: k2[:], Endpoint: "10.250.0.3:51820", Prefix: "fd00:1:2::2:0:0/96"},
	}})
	e := testEnv("e1")
	e.Services[0].Endpoints = append(e.Services[0].Endpoints, wire.Endpoint{
		Instance: "i-e1-2", Address: "fd00:1:2::2:0:5", Node: "n2", Ports: map[string]int{"http": 8080}, GroupVersion: 1,
	})
	h.fake.PutEnv(e)
	return k1, k2
}

// fresh reports whether the agent's last report gives node a handshake
// within the last d.
func (ta *testAgent) fresh(node string, d time.Duration) bool {
	at, ok := ta.rec.lastMesh()[node]
	return ok && !at.IsZero() && time.Since(at) < d
}

// helloWith reports whether the fake control plane took a Hello with the
// endpoint ep.
func (h *harness) helloWith(ep string) bool {
	return slices.ContainsFunc(h.fake.Hellos(), func(hl FakeHello) bool { return hl.Request.Endpoint == ep })
}

// A node whose flow from the instance stopped being answered (its feosd
// restarted, or the network dropped the flow) is reached again without a
// restart: the device moves to the next port and sets the node's peer
// anew, the control plane learns the new endpoint, and the node that
// kept answering keeps its peer.
func TestMeshRepairReachesALostNodeAgain(t *testing.T) {
	h := newHarness(t)
	k1, k2 := twoNodes(t, h, 1)
	dev := newNetMesh()
	h.device = dev
	ta := h.joined(repairTiming)
	eventually(t, "both nodes handshake", func() bool { return ta.fresh("n1", time.Second) && ta.fresh("n2", time.Second) })
	if p := dev.currentPort(); p != DefaultMeshPort {
		t.Fatalf("the device is on port %d before any repair", p)
	}

	dev.breakFlow(k2)
	eventually(t, "n2 lost and found again on another port", func() bool {
		return ta.Status().MeshRepairs > 0 && ta.fresh("n2", 200*time.Millisecond)
	})
	st := ta.Status()
	if st.MeshPort != DefaultMeshPort+1 || dev.currentPort() != DefaultMeshPort+1 || st.MeshEndpoint != "10.0.0.5:51821" {
		t.Errorf("port %d (device %d), endpoint %q after the repair", st.MeshPort, dev.currentPort(), st.MeshEndpoint)
	}
	if removed, again := dev.removedAndSetAgain(k2, "10.250.0.3:51820"); !removed || !again {
		t.Errorf("n2's peer: removed %v, set again with its endpoint %v", removed, again)
	}
	if removed, _ := dev.removedAndSetAgain(k1, "10.250.0.2:51820"); removed {
		t.Error("the repair removed n1's peer, which answered")
	}
	eventually(t, "the new endpoint announced", func() bool { return h.helloWith("10.0.0.5:51821") })
	if !ta.logs.contains("gave the mesh device new paths") || !ta.logs.contains("nodes=[n2]") {
		t.Error("the repair was not logged with the node")
	}
	// The node answers on the new flow: no further repair.
	repairs := ta.Status().MeshRepairs
	time.Sleep(time.Second)
	if n := ta.Status().MeshRepairs; n != repairs {
		t.Errorf("%d repairs more once n2 answered again", n-repairs)
	}

	// A reconnect says Hello with the moved endpoint, and the device stays
	// on its port.
	hellos := len(h.fake.Hellos())
	h.fake.CloseStreams(nil)
	eventually(t, "a new session", func() bool { return len(h.fake.Hellos()) > hellos && ta.Connected() })
	if last := h.fake.Hellos()[len(h.fake.Hellos())-1]; last.Request.Endpoint != "10.0.0.5:51821" {
		t.Errorf("the reconnect's Hello sent %q", last.Request.Endpoint)
	}
	time.Sleep(200 * time.Millisecond)
	if p := dev.currentPort(); p != DefaultMeshPort+1 {
		t.Errorf("the device went back to port %d", p)
	}
}

// After an outage that left every flow it carried broken, the instance
// finds both nodes again on its own, once the network is back: the repairs
// during the outage move it to ports the outage broke as well, the first
// one after it to a flow that passes.
func TestMeshRepairAfterAnOutage(t *testing.T) {
	h := newHarness(t)
	twoNodes(t, h, 1)
	dev := newNetMesh()
	h.device = dev
	ta := h.joined(repairTiming)
	eventually(t, "both nodes handshake", func() bool { return ta.fresh("n1", time.Second) && ta.fresh("n2", time.Second) })

	dev.setDown(true)
	eventually(t, "repairs during the outage", func() bool { return ta.Status().MeshRepairs >= 2 })
	if ta.fresh("n1", 300*time.Millisecond) || ta.fresh("n2", 300*time.Millisecond) {
		t.Fatal("a node handshook during the outage")
	}
	dev.setDown(false)
	start := time.Now()
	eventually(t, "both nodes again after the outage", func() bool {
		return ta.fresh("n1", 200*time.Millisecond) && ta.fresh("n2", 200*time.Millisecond)
	})
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the nodes came back %s after the outage", d)
	}
	st := ta.Status()
	ep := fmt.Sprintf("10.0.0.5:%d", st.MeshPort)
	if st.MeshPort == DefaultMeshPort || st.MeshEndpoint != ep {
		t.Errorf("port %d, endpoint %q after the outage", st.MeshPort, st.MeshEndpoint)
	}
	eventually(t, "the last endpoint announced", func() bool { return h.helloWith(ep) })
}

// A node without endpoints carries no traffic, and one that is down has
// none: its lost flow moves nothing. Once it runs an endpoint, it is
// repaired.
func TestMeshRepairOnlyForNodesWithEndpoints(t *testing.T) {
	h := newHarness(t)
	k1, k2 := newWGKey(t).PublicKey(), newWGKey(t).PublicKey()
	h.fake.SetMesh(wire.Mesh{Version: 1, Peers: []wire.Peer{
		{Name: "n1", PublicKey: k1[:], Endpoint: "10.250.0.2:51820", Prefix: "fd00:1:2::1:0:0/96"},
		{Name: "n2", PublicKey: k2[:], Endpoint: "10.250.0.3:51820", Prefix: "fd00:1:2::2:0:0/96"},
	}})
	h.fake.PutEnv(testEnv("e1")) // an endpoint on n1 alone
	dev := newNetMesh()
	h.device = dev
	ta := h.joined(repairTiming)
	eventually(t, "both nodes handshake", func() bool { return ta.fresh("n1", time.Second) && ta.fresh("n2", time.Second) })
	dev.breakFlow(k2)
	time.Sleep(1500 * time.Millisecond)
	if st := ta.Status(); st.MeshRepairs != 0 || st.MeshPort != DefaultMeshPort {
		t.Fatalf("%d repairs, port %d for a node without endpoints", st.MeshRepairs, st.MeshPort)
	}
	twoNodes(t, h, 2) // new keys: n2 is a new peer, with an endpoint
	eventually(t, "both nodes handshake after the change", func() bool { return ta.fresh("n1", time.Second) && ta.fresh("n2", time.Second) })
	if st := ta.Status(); st.MeshRepairs != 0 {
		t.Errorf("%d repairs for new peers that answer", st.MeshRepairs)
	}
}

// With an endpoint whose port is not the device's (a port forwarded to
// it), a repair sets the lost peers anew and keeps the port.
func TestMeshRepairKeepsAForwardedPort(t *testing.T) {
	h := newHarness(t)
	_, k2 := twoNodes(t, h, 1)
	dev := newNetMesh()
	h.device = dev
	cfg := h.cfg
	cfg.MeshEndpoint = "192.0.2.7:40000"
	h.writeToken(h.mintToken())
	ta := h.start(cfg, repairTiming)
	eventually(t, "both nodes handshake", func() bool { return ta.fresh("n1", time.Second) && ta.fresh("n2", time.Second) })
	dev.breakFlow(k2)
	eventually(t, "a repair", func() bool { return ta.Status().MeshRepairs > 0 })
	if _, again := dev.removedAndSetAgain(k2, "10.250.0.3:51820"); !again {
		t.Error("n2's peer was not set anew")
	}
	if st := ta.Status(); st.MeshPort != DefaultMeshPort || st.MeshEndpoint != "192.0.2.7:40000" || dev.currentPort() != DefaultMeshPort {
		t.Errorf("port %d (device %d), endpoint %q", st.MeshPort, dev.currentPort(), st.MeshEndpoint)
	}
}
