// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ingress

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/agent"
	"github.com/gardenlinux/feos/go/internal/ingress/wg"
)

// outageMesh is a mesh device on a network that keeps a broken UDP flow
// broken: a flow (the device's port and a peer) that carried packets
// during an outage passes nothing afterwards, for as long as packets keep
// it alive, which WireGuard's keepalive and retries do. A peer handshakes
// while its flow passes; one whose flow does not reads as having had no
// handshake for ten minutes, past the data path's freshness, as after a
// long outage.
type outageMesh struct {
	mu     sync.Mutex
	port   int
	peers  []wg.PeerState
	down   bool
	broken map[outageFlow]bool
}

type outageFlow struct {
	port int
	key  wg.Key
}

func (m *outageMesh) Configure(c agent.MeshConfig) error {
	m.mu.Lock()
	m.port = c.ListenPort
	m.mu.Unlock()
	return nil
}

func (m *outageMesh) SetPeers(ps []wg.Peer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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

func (m *outageMesh) Status() (wg.Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for i := range m.peers {
		p := &m.peers[i]
		f := outageFlow{m.port, p.PublicKey}
		if m.down {
			m.broken[f] = true
		}
		switch {
		case !m.broken[f]:
			p.LastHandshake = now
		case !p.LastHandshake.IsZero() && now.Sub(p.LastHandshake) < 10*time.Minute:
			p.LastHandshake = now.Add(-10 * time.Minute)
		}
	}
	return wg.Device{Name: agent.DefaultMeshDevice, ListenPort: m.port, Peers: slices.Clone(m.peers)}, nil
}

func (m *outageMesh) setDown(down bool) {
	m.mu.Lock()
	m.down = down
	m.mu.Unlock()
}

// The second failure of 2026-11-05: a network outage left the instance's
// flows to the nodes broken after it ended, and the instance stayed not
// ready ("none of the 2 nodes with endpoints has a fresh handshake") for
// hours, until its pod was deleted. Now it is ready again on its own a
// repair after the outage: its device moves to a port the outage did not
// break, and the control plane learns the new endpoint.
func TestReadinessRecoversAfterAnOutage(t *testing.T) {
	t.Parallel()
	h := newProgHarness(t)
	h.cfg.meshRepairEvery = 200 * time.Millisecond
	dev := &outageMesh{broken: map[outageFlow]bool{}}
	h.mesh = dev
	h.upstream(http.HandlerFunc(echoHandler))
	h.start()
	h.waitReady()
	c := h.client()
	if code, body, err := get(c, "https://"+progHost+"/"); err != nil || code != http.StatusOK {
		t.Fatalf("before the outage: %d %q %v", code, body, err)
	}

	dev.setDown(true)
	h.waitNotReady("none of the 1 nodes with endpoints has a fresh handshake")
	// The repairs during the outage (the agent looks every 2 s) move the
	// device to ports that the outage breaks as well.
	time.Sleep(5 * time.Second)
	if code, _ := h.health("/readyz"); code != http.StatusServiceUnavailable {
		t.Fatal("ready during the outage")
	}
	during := h.prog.Status().MeshRepairs

	dev.setDown(false)
	h.waitReady()
	st := h.prog.Status()
	if during == 0 || st.MeshRepairs <= during {
		t.Errorf("%d repairs during the outage, %d in all", during, st.MeshRepairs)
	}
	if st.MeshPort == agent.DefaultMeshPort || st.MeshEndpoint != fmt.Sprintf("127.0.0.1:%d", st.MeshPort) {
		t.Errorf("port %d, endpoint %q after the outage", st.MeshPort, st.MeshEndpoint)
	}
	if len(st.MeshPeers) != 1 || !st.MeshPeers[0].Fresh {
		t.Errorf("mesh peers %+v", st.MeshPeers)
	}
	if code, body, err := get(c, "https://"+progHost+"/"); err != nil || code != http.StatusOK {
		t.Fatalf("after the outage: %d %q %v", code, body, err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for !slices.ContainsFunc(h.fake.Hellos(), func(hl agent.FakeHello) bool { return hl.Request.Endpoint == st.MeshEndpoint }) {
		if time.Now().After(deadline) {
			t.Fatalf("the control plane never took the endpoint %s", st.MeshEndpoint)
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, body := h.health("/metrics")
	for _, want := range []string{"ferun_ingress_mesh_repairs_total ", fmt.Sprintf("ferun_ingress_mesh_port %d", st.MeshPort)} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q:\n%s", want, body)
		}
	}
}
