// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/wg"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// MeshDevice is the instance's WireGuard device on the pods' mesh. The
// agent serializes Configure and SetPeers; Status may run beside them.
type MeshDevice interface {
	// Configure creates the device if it is missing and sets it up for
	// c: its MTU, up, the address c.Address and no other, the route of
	// c.PodNetwork through it with that address as the source, its key
	// and its port. The peers the device has stay.
	Configure(MeshConfig) error
	// SetPeers makes peers the device's peers; a peer that stays as it
	// is keeps its session.
	SetPeers([]wg.Peer) error
	// Status returns the device and its peers as the kernel has them.
	Status() (wg.Device, error)
}

// MeshConfig is the configuration of the mesh device.
type MeshConfig struct {
	Device     string
	PrivateKey wg.Key
	ListenPort int
	// MTU is the device's MTU; 0 is ComputeMTU of the interface that
	// holds Endpoint's address.
	MTU int
	// Endpoint is where the nodes reach the device.
	Endpoint netip.AddrPort
	// Address is the instance's address on the mesh, a /128.
	Address netip.Prefix
	// PodNetwork is the cluster's ULA, routed through the device.
	PodNetwork netip.Prefix
}

// The device's MTU (D5) and the peers' keepalive.
const (
	minMTU            = 1280 // IPv6's minimum
	maxMTU            = 1420 // WireGuard's default
	wireGuardOverhead = 80   // WireGuard 32, UDP 8, outer IPv6 40
	peerKeepalive     = 25 * time.Second
)

// The repair of the paths to the nodes (repairMesh).
const (
	// meshStale is how old a node's last handshake may be before the node
	// counts as lost: WireGuard's reject-after time, after which no
	// session is left; with a keepalive of 25 s a node that answers
	// handshakes about every 2 minutes. The data path's freshness is the
	// same.
	meshStale = 180 * time.Second
	// meshRepairEvery is the least time between two repairs, and how long
	// a node's peer, new or set anew, has to handshake before it counts.
	meshRepairEvery = 60 * time.Second
	// meshPorts is how many ports the device moves through: the
	// configured one and the 15 after it, so that a network policy can
	// name them as a range.
	meshPorts = 16
)

// ComputeMTU is the mesh device's MTU over an interface with MTU ifMTU:
// the interface's MTU less WireGuard's overhead over IPv6, at most 1420
// and at least 1280. The smaller MTU also clamps the MSS of the
// connections the ingress opens through the device.
func ComputeMTU(ifMTU int) int {
	return min(maxMTU, max(minMTU, ifMTU-wireGuardOverhead))
}

// wantMesh makes c the device's configuration and configures it unless it
// has it already. The port is the agent's: Config.MeshPort, or the one a
// repair moved the device to.
func (a *Agent) wantMesh(c MeshConfig) {
	a.mu.Lock()
	c.ListenPort = a.meshPort
	a.meshWant = c
	a.mu.Unlock()
	a.ensureMesh()
}

// ensureMesh configures the device as wanted unless it is so already, and
// then sets its peers again. A failure is logged; the mesh loop calls it
// again.
func (a *Agent) ensureMesh() {
	a.meshMu.Lock()
	a.mu.Lock()
	want := a.meshWant
	done := want.Device == "" || (a.meshOK && a.meshCfg == want)
	a.mu.Unlock()
	if done {
		a.meshMu.Unlock()
		return
	}
	err := a.deps.Mesh.Configure(want)
	a.mu.Lock()
	if err != nil {
		a.meshOK = false
	} else {
		a.meshCfg, a.meshOK, a.peersSet = want, true, false
	}
	a.mu.Unlock()
	a.meshMu.Unlock()
	if err != nil {
		a.rl.error("mesh", "ingress agent: configure the mesh device; retrying", "err", err, "device", want.Device, "address", want.Address)
		return
	}
	a.log.Info("ingress agent: configured the mesh device", "device", want.Device, "address", want.Address, "podNetwork", want.PodNetwork)
	a.syncPeers()
}

// syncPeers sets the device's peers from the table's mesh, unless the
// device already has that mesh version. A failure is retried by the mesh
// loop.
func (a *Agent) syncPeers() {
	a.meshMu.Lock()
	defer a.meshMu.Unlock()
	a.mu.Lock()
	if !a.meshOK || a.state == nil || (a.peersSet && a.peersVer == a.state.Mesh.Version) {
		a.mu.Unlock()
		return
	}
	m := a.state.Mesh
	a.mu.Unlock()
	peers, problems := wgPeers(m)
	if len(problems) > 0 {
		a.rl.warn("peers-skipped", "ingress agent: skipping mesh peers", "problems", problems, "meshVersion", m.Version)
	}
	err := a.deps.Mesh.SetPeers(peers)
	a.mu.Lock()
	a.peersSet = err == nil
	if err == nil {
		a.peersVer = m.Version
	}
	a.mu.Unlock()
	if err != nil {
		a.rl.error("peers", "ingress agent: set the mesh device's peers; retrying", "err", err, "meshVersion", m.Version)
		return
	}
	a.log.Info("ingress agent: set the mesh device's peers", "peers", len(peers), "meshVersion", m.Version)
	// Report the new peers' handshakes without waiting for the next poll.
	select {
	case a.meshKick <- struct{}{}:
	default:
	}
}

// wgPeers are the device's peers for a mesh: each node with its /96 as
// its only allowed prefix, its endpoint and keepalive 25 s. Nodes that
// cannot be peers are left out and described in problems.
func wgPeers(m wire.Mesh) (peers []wg.Peer, problems []string) {
	seen := map[wg.Key]bool{}
	for _, p := range m.Peers {
		if len(p.PublicKey) != wg.KeyLen {
			problems = append(problems, fmt.Sprintf("node %s: a public key of %d bytes", p.Name, len(p.PublicKey)))
			continue
		}
		k := wg.Key(p.PublicKey)
		if seen[k] {
			problems = append(problems, fmt.Sprintf("node %s: public key %s listed twice", p.Name, k))
			continue
		}
		var ep netip.AddrPort
		if p.Endpoint != "" {
			var err error
			if ep, err = netip.ParseAddrPort(p.Endpoint); err != nil {
				problems = append(problems, fmt.Sprintf("node %s: endpoint %q is not address:port", p.Name, p.Endpoint))
				continue
			}
		}
		pfx, err := netip.ParsePrefix(p.Prefix)
		if err != nil {
			problems = append(problems, fmt.Sprintf("node %s: prefix %q", p.Name, p.Prefix))
			continue
		}
		seen[k] = true
		peers = append(peers, wg.Peer{
			PublicKey:  k,
			Endpoint:   ep,
			AllowedIPs: []netip.Prefix{pfx.Masked()},
			Keepalive:  peerKeepalive,
		})
	}
	return peers, problems
}

// meshLoop reads the device's peers every meshPoll, and after the peers
// were set, and reports each node's last handshake to OnMesh; it also
// retries setting the peers, repairs the paths to the nodes it lost,
// tells the control plane a new endpoint, warns of an old table and
// forgets old Wake calls.
func (a *Agent) meshLoop(ctx context.Context) {
	t := time.NewTicker(a.tune.meshPoll)
	defer t.Stop()
	for {
		now := time.Now()
		a.ensureMesh()
		a.syncPeers()
		a.pollMesh()
		a.announce()
		a.checkTableAge(now)
		a.sweepWakes(now)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.meshKick:
		}
	}
}

// pollMesh reads the device's peers, maps their keys to node names
// through the table's mesh, and reports their handshakes. Without a
// configured device, or when it cannot be read, no node has one; a device
// that cannot be read is configured again. Nodes it lost it repairs
// (repairMesh).
func (a *Agent) pollMesh() {
	now := time.Now()
	a.mu.Lock()
	ok := a.meshOK
	names := map[wg.Key]string{}
	if a.state != nil {
		for _, p := range a.state.Mesh.Peers {
			if len(p.PublicKey) == wg.KeyLen {
				names[wg.Key(p.PublicKey)] = p.Name
			}
		}
	}
	for k := range names {
		if _, wanted := a.meshSince[k]; !wanted {
			a.meshSince[k] = now
		}
	}
	maps.DeleteFunc(a.meshSince, func(k wg.Key, _ time.Time) bool { _, wanted := names[k]; return !wanted })
	a.mu.Unlock()
	handshakes := map[string]time.Time{}
	var peers []MeshPeer
	read := false
	if ok {
		dev, err := a.deps.Mesh.Status()
		if err != nil {
			a.rl.warn("mesh-status", "ingress agent: read the mesh device; configuring it again", "err", err)
			a.mu.Lock()
			a.meshOK = false
			a.mu.Unlock()
			a.ensureMesh()
		} else {
			read = true
			for _, p := range dev.Peers {
				name, known := names[p.PublicKey]
				if !known {
					continue
				}
				handshakes[name] = p.LastHandshake
				mp := MeshPeer{Node: name, PublicKey: p.PublicKey.String(), LastHandshake: p.LastHandshake}
				if p.Endpoint.IsValid() {
					mp.Endpoint = p.Endpoint.String()
				}
				peers = append(peers, mp)
			}
			slices.SortFunc(peers, func(x, y MeshPeer) int { return strings.Compare(x.Node, y.Node) })
		}
	}
	a.mu.Lock()
	a.meshPeers = peers
	a.mu.Unlock()
	if a.deps.OnMesh != nil {
		a.deps.OnMesh(handshakes)
	}
	if read {
		if lost, names := a.lostNodes(now, handshakes); len(lost) > 0 {
			a.repairMesh(now, lost, names)
		}
	}
}

// lostNodes are the nodes the agent repairs the paths to, once
// meshRepairEvery passed since the last repair, as peers and by name: the
// nodes with endpoints in the table (the others carry no traffic, and a
// node that is down has none) whose last handshake on the device
// (handshakes; the zero time for none) is older than meshStale, and that
// have been wanted as peers for meshRepairEvery, so that a new peer, or
// one a repair set anew, has that long to handshake.
func (a *Agent) lostNodes(now time.Time, handshakes map[string]time.Time) (lost []wg.Peer, names []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state == nil || now.Sub(a.lastRepair) < a.tune.meshRepairEvery {
		return nil, nil
	}
	peers, _ := wgPeers(a.state.Mesh)
	var busy map[string]bool
	for _, p := range a.state.Mesh.Peers {
		if len(p.PublicKey) != wg.KeyLen {
			continue
		}
		k := wg.Key(p.PublicKey)
		i := slices.IndexFunc(peers, func(w wg.Peer) bool { return w.PublicKey == k })
		if i < 0 {
			continue // not a peer of the device
		}
		if hs := handshakes[p.Name]; !hs.IsZero() && now.Sub(hs) < a.tune.meshStale {
			continue
		}
		if since, wanted := a.meshSince[k]; !wanted || now.Sub(since) < a.tune.meshRepairEvery {
			continue
		}
		if busy == nil {
			busy = endpointNodes(a.state.Envs)
		}
		if busy[p.Name] {
			lost, names = append(lost, peers[i]), append(names, p.Name)
		}
	}
	return lost, names
}

// endpointNodes are the nodes that run endpoints of the table's
// environments.
func endpointNodes(envs map[string]wire.Env) map[string]bool {
	nodes := map[string]bool{}
	for _, e := range envs {
		for _, svc := range e.Services {
			for _, ep := range svc.Endpoints {
				nodes[ep.Node] = true
			}
		}
	}
	return nodes
}

// repairMesh gives the instance new paths to the nodes it lost
// (lostNodes), as a restart of its pod would, without one:
//
//   - The device moves to the next port of its range, so that every node
//     is reached in a UDP flow that is new to the network. The flow of a
//     WireGuard peer keeps its addresses and ports for the peer's life,
//     and the keepalive and the handshake's retries never let it rest, so
//     state that the network keeps for it, such as a flow an outage left
//     broken, would never expire. Where the endpoint's port is not the
//     device's own (a port forwarded to it), the device keeps its port.
//   - The lost nodes' peers are removed and set again: their sessions,
//     timers and the endpoints the device learnt go, their configured
//     endpoints come back, and the kernel handshakes at once.
//   - The control plane learns the new endpoint from the next Hello
//     (announce), and the nodes from it, as any new endpoint.
//
// The nodes that answer keep their sessions; they follow the new port with
// the instance's next packet. A failure is logged; the next repair, or
// syncPeers, tries again.
func (a *Agent) repairMesh(now time.Time, lost []wg.Peer, names []string) {
	a.meshMu.Lock()
	defer a.meshMu.Unlock()
	a.mu.Lock()
	cfg := a.meshCfg
	ok := a.meshOK && a.meshCfg == a.meshWant && a.state != nil
	var m wire.Mesh
	if ok {
		m = a.state.Mesh
	}
	a.lastRepair = now
	a.mu.Unlock()
	if !ok {
		return
	}
	from := cfg.ListenPort
	if a.portMoves() {
		for i := 1; i < meshPorts; i++ {
			next := cfg
			next.ListenPort = a.cfg.MeshPort + (from-a.cfg.MeshPort+i)%meshPorts
			if err := a.deps.Mesh.Configure(next); err != nil {
				a.rl.warn("mesh-port", "ingress agent: move the mesh device to another port; trying the next", "err", err, "port", next.ListenPort)
				continue
			}
			cfg = next
			break
		}
	}
	peers, _ := wgPeers(m)
	keep := slices.DeleteFunc(slices.Clone(peers), func(p wg.Peer) bool {
		return slices.ContainsFunc(lost, func(l wg.Peer) bool { return l.PublicKey == p.PublicKey })
	})
	err := a.deps.Mesh.SetPeers(keep)
	if err == nil {
		err = a.deps.Mesh.SetPeers(peers)
	}
	a.mu.Lock()
	a.meshCfg, a.meshWant, a.meshPort = cfg, cfg, cfg.ListenPort
	a.peersSet = err == nil
	if err == nil {
		a.peersVer = m.Version
	}
	for _, p := range lost {
		a.meshSince[p.PublicKey] = now
	}
	a.repairs++
	a.mu.Unlock()
	if err != nil {
		a.rl.error("peers", "ingress agent: set the mesh device's peers; retrying", "err", err, "meshVersion", m.Version)
	}
	a.log.Warn("ingress agent: no handshake with nodes that run endpoints; gave the mesh device new paths to them",
		"nodes", names, "after", a.tune.meshStale, "port", cfg.ListenPort, "fromPort", from)
	select {
	case a.meshKick <- struct{}{}:
	default:
	}
}

// portMoves reports whether a repair moves the device's port: when the
// nodes reach the device at its own port, and the range fits.
func (a *Agent) portMoves() bool {
	return int(a.endpoint.Port()) == a.cfg.MeshPort && a.cfg.MeshPort+meshPorts-1 <= 65535
}

// meshEndpointLocked is the endpoint the nodes reach the device at:
// Config.MeshEndpoint, at the port a repair moved the device to.
func (a *Agent) meshEndpointLocked() string {
	if a.meshPort == a.cfg.MeshPort || !a.portMoves() {
		return a.cfg.MeshEndpoint
	}
	return netip.AddrPortFrom(a.endpoint.Addr(), uint16(a.meshPort)).String()
}

// announce tells the control plane the device's endpoint when it changed
// since the last Hello: a Hello on the current session, on a goroutine of
// its own, one at a time. The control plane counts the mesh's peers up
// and sends the nodes the new endpoint. A failure is logged and tried
// again from the mesh loop; the next connection's Hello sends the
// endpoint anyway.
func (a *Agent) announce() {
	a.mu.Lock()
	s, ctx := a.cur, a.runCtx
	ep := a.meshEndpointLocked()
	if s == nil || ctx == nil || a.announcing || a.announced == ep {
		a.mu.Unlock()
		return
	}
	a.announcing = true
	pub := a.wgKey.PublicKey()
	a.goroutines.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.goroutines.Done()
		cctx, cancel := context.WithTimeout(ctx, a.tune.callTimeout)
		resp, err := s.client.Hello(cctx, &wire.HelloRequest{PublicKey: pub[:], Endpoint: ep, Version: a.cfg.Version})
		cancel()
		if err == nil && resp.Name != a.cfg.Name {
			err = fmt.Errorf("the control plane answered Hello for ingress %q, not %q", resp.Name, a.cfg.Name)
		}
		a.mu.Lock()
		a.announcing = false
		if err == nil {
			a.announced = ep
		}
		a.mu.Unlock()
		if err != nil {
			a.rl.warn("announce", "ingress agent: tell the control plane the mesh device's new endpoint; retrying", "err", err, "endpoint", ep)
			return
		}
		a.log.Info("ingress agent: the control plane took the mesh device's new endpoint", "endpoint", ep)
	}()
}
