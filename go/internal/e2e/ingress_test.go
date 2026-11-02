// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/env"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/ingress"
	"github.com/gardenlinux/feos/go/internal/ingress/wg"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/mesh"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	engine "github.com/gardenlinux/feos/go/internal/sandbox"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The ingress's scenarios (ADR 0021, §10 of its design): ferund
// instances, test nodes on the pods' mesh that check security groups, and
// ferun-ingress in the test's process (runIngress), which joins with a
// token, takes its place on the mesh, follows the table and serves the
// public environments' hosts under apps.test. Its WireGuard device is a
// fake whose peers always have a fresh handshake, and its dial hook plays
// the mesh and the nodes' firewalls (dialPod): a pod's port answers only
// when its node lets the ingress in. The image services' replicas are an
// HTTP server per pod (apps), which the readiness checks reach through
// the port forward and the ingress through the dial hook.

// appPort is the port the replicas listen on.
const appPort = 8080

// appSpec is a release spec with the image service web: replicas on
// appPort, ready by GET /healthz, with idle ("" for none) and the
// environment variable VERSION, whose change changes the template, and
// the route / to web.
func appSpec(replicas int, idle, version string) string {
	idleField := ""
	if idle != "" {
		idleField = fmt.Sprintf(`,"idle":%q`, idle)
	}
	return fmt.Sprintf(`{"services":{"web":{"runtime":"image","image":%q,"replicas":%d,
		"ports":[{"name":"http","port":%d}],"env":{"VERSION":%q},
		"readiness":{"http":{"port":"http","path":"/healthz"},"period":"1s"}%s}},
		"routes":{"root":{"path":"/","service":"web","port":"http"}}}`, testImage, replicas, appPort, version, idleField)
}

// apps are the replicas of the image services: an HTTP server per pod,
// started at the pod's first bring-up on a node and mapped as its port
// appPort there. A replica answers GET /healthz with 200, and anything
// else with its pod and what it was sent (podOf reads the pod), /slow
// only once the test lets it (releaseSlow).
type apps struct {
	mu      sync.Mutex
	servers map[podid.ID]*httptest.Server
	// slow is closed by releaseSlow; stop when the cluster ends.
	slow, stop chan struct{}
	slowOnce   sync.Once
	slowOpen   atomic.Int32
}

// serveApps serves the replicas of the pods of nodes until the cluster
// has stopped.
func (c *cluster) serveApps(nodes ...*node) *apps {
	a := &apps{servers: map[podid.ID]*httptest.Server{}, slow: make(chan struct{}), stop: make(chan struct{})}
	for _, n := range nodes {
		n.Script().SetBringUp(func(_ context.Context, pod podid.ID) error {
			n.Script().SetPodPortAddr(pod, appPort, a.server(pod))
			return nil
		})
	}
	c.onEnd(a.close)
	return a
}

// server returns the address of the pod's server, which it starts once.
func (a *apps) server(pod podid.ID) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	srv, ok := a.servers[pod]
	if !ok {
		srv = httptest.NewServer(a.handler(pod))
		a.servers[pod] = srv
	}
	return srv.Listener.Addr().String()
}

func (a *apps) handler(pod podid.ID) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			fmt.Fprintln(w, "ok")
			return
		case "/slow":
			a.slowOpen.Add(1)
			defer a.slowOpen.Add(-1)
			select {
			case <-a.slow:
			case <-a.stop:
			case <-r.Context().Done():
				return
			}
		}
		fmt.Fprintf(w, "pod=%s host=%s path=%s for=%s proto=%s", pod, r.Host, r.URL.Path,
			r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Forwarded-Proto"))
	})
}

// releaseSlow lets the requests to /slow answer, those open and later
// ones.
func (a *apps) releaseSlow() { a.slowOnce.Do(func() { close(a.slow) }) }

func (a *apps) close() {
	close(a.stop)
	a.mu.Lock()
	servers := a.servers
	a.servers = map[podid.ID]*httptest.Server{}
	a.mu.Unlock()
	for _, srv := range servers {
		srv.Close()
	}
}

// podOf is the pod that a replica's answer names; "" for none.
func podOf(body string) podid.ID {
	f, ok := strings.CutPrefix(body, "pod=")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(f, " ")
	return podid.ID(id)
}

// mkApp makes the tenant's environment name with appSpec's first release
// and waits until it is deployed.
func (c *cluster) mkApp(name string, replicas int, idle string) uuid.UUID {
	c.t.Helper()
	e, err := c.st.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: tenant, Name: name})
	if err != nil {
		c.t.Fatal(err)
	}
	n := c.applyRelease(e.ID, appSpec(replicas, idle, "1"))
	c.waitRelease(e.ID, n, model.ReleaseDeployed, 2*time.Minute)
	return e.ID
}

// webRows are the environment's instances of web that are not stopping.
func (c *cluster) webRows(envID uuid.UUID) []store.EnvInstanceRow {
	c.t.Helper()
	rows, err := c.st.ListEnvInstanceRows(bg, envID)
	if err != nil {
		c.t.Fatal(err)
	}
	var out []store.EnvInstanceRow
	for _, r := range rows {
		if r.Instance.Kind == spec.KindService && r.Instance.Object == "web" && r.Instance.Phase != model.PhaseStopping {
			out = append(out, r)
		}
	}
	return out
}

// waitWeb waits until the environment has exactly replicas instances of
// web, none stopping, each ready, running without an op, and as ok says,
// and returns them.
func (c *cluster) waitWeb(envID uuid.UUID, replicas int, what string, ok func(store.EnvInstanceRow) bool) []store.EnvInstanceRow {
	c.t.Helper()
	var rows []store.EnvInstanceRow
	eventually(c.t, 2*time.Minute, what, func() bool {
		all, err := c.st.ListEnvInstanceRows(bg, envID)
		if err != nil {
			return false
		}
		rows = rows[:0]
		for _, r := range all {
			if r.Instance.Kind != spec.KindService || r.Instance.Object != "web" {
				continue
			}
			if r.Instance.Phase != model.PhaseReady || r.Sandbox.ClientState != model.StateRunning || r.Sandbox.Op != "" || !ok(r) {
				return false
			}
			rows = append(rows, r)
		}
		return len(rows) == replicas
	})
	return rows
}

// namesIngGroup reports whether a row's template has the ingress flag and
// names the row's own group ing-<id>.
func namesIngGroup(r store.EnvInstanceRow) bool {
	t := r.Sandbox.PodTemplate
	return t != nil && t.IngressGroup && slices.Contains(t.SecurityGroups, model.IngressGroupID(r.Sandbox.ID))
}

// publishApp publishes an environment for the first time, checks the
// answer (its one host under the apps zone, rolling), and waits until its
// service rolled once: replicas instances that name their groups. It
// returns the host.
func (c *cluster) publishApp(envID uuid.UUID, replicas int) string {
	c.t.Helper()
	ans := c.setPublic(envID, true)
	if !ans.Rolling || len(ans.Hosts) != 1 || !strings.HasSuffix(ans.Hosts[0], "."+appsZone) {
		c.t.Fatalf("the first publish: %+v", ans)
	}
	c.waitWeb(envID, replicas, "the service rolled to name its ingress groups", namesIngGroup)
	return ans.Hosts[0]
}

// checkAdmitted checks what the nodes hold of the ingress's groups for
// the environment's instances of web, and what is recorded of it: each
// instance's group ing-<id> on its node lets the ingress in on appPort
// and has no members; ferun-ingress there lists the ingress's address, at
// the version at which it entered or later; the instance's ingress_ports
// are [appPort]; and its node's ingress_group_version is that version or
// later.
func (c *cluster) checkAdmitted(p *ingressProc, envID uuid.UUID) {
	c.t.Helper()
	g, err := c.st.GetIngress(bg, p.name)
	if err != nil {
		c.t.Fatal(err)
	}
	for _, row := range c.webRows(envID) {
		n := c.nodeNamed(row.Sandbox.Node)
		ing := nodeGroup(n, model.IngressGroupID(row.Sandbox.ID))
		if ing == nil || len(ing.GetRules()) != 1 || !admits(ing, appPort) || len(ing.GetMembers()) != 0 {
			c.t.Fatalf("%s's group on %s: %v", row.Sandbox.ID, n.name, ing)
		}
		peers := nodeGroup(n, model.IngressPeersGroup)
		if peers == nil || !slices.Equal(peers.GetMembers(), []string{g.MeshAddress}) || int64(peers.GetVersion()) < g.GroupVersion {
			c.t.Fatalf("%s on %s: %v; the ingress %s entered at %d", model.IngressPeersGroup, n.name, peers, g.MeshAddress, g.GroupVersion)
		}
		if !slices.Equal(row.Instance.IngressPorts, []int{appPort}) {
			c.t.Fatalf("%s: ingress ports %v", row.Sandbox.ID, row.Instance.IngressPorts)
		}
		nr, err := c.st.GetNode(bg, n.name)
		if err != nil || nr.IngressGroupVersion < g.GroupVersion {
			c.t.Fatalf("node %s holds %s at %d (%v); the ingress entered at %d", n.name, model.IngressPeersGroup,
				nr.IngressGroupVersion, err, g.GroupVersion)
		}
	}
}

// ingressHeld reports whether the ingress holds the sandbox at the
// control plane: a live hold of kind forward, holder ingress:<name>,
// renewed by the instance instanceID.
func (c *cluster) ingressHeld(p *ingressProc, id podid.ID, instanceID string) bool {
	c.t.Helper()
	return c.count(`SELECT count(*) FROM sandbox_holds WHERE sandbox_id = $1 AND holder = $2 AND kind = $3
		AND instance_id = $4 AND lease_until > now()`, id, store.IngressHolder(p.name), string(model.HoldForward), instanceID) == 1
}

// cachedService is the service name of the environment as the ingress's
// cached table (table.json) has it. The agent hands a table to the proxy
// before it writes the cache, so what the cache shows the proxy routes by
// already (or by a newer table).
func (p *ingressProc) cachedService(envID uuid.UUID, name string) (wire.Service, bool) {
	b, err := os.ReadFile(filepath.Join(p.cfg.StateDir, "table.json"))
	if err != nil {
		return wire.Service{}, false
	}
	var cache struct {
		State *wire.State `json:"state"`
	}
	if err := json.Unmarshal(b, &cache); err != nil || cache.State == nil {
		return wire.Service{}, false
	}
	for _, e := range cache.State.Envs {
		if e.ID != envID.String() {
			continue
		}
		for _, svc := range e.Services {
			if svc.Name == name {
				return svc, true
			}
		}
	}
	return wire.Service{}, false
}

// webPods are the sandboxes of rows.
func webPods(rows []store.EnvInstanceRow) map[podid.ID]bool {
	out := map[podid.ID]bool{}
	for _, r := range rows {
		out[r.Sandbox.ID] = true
	}
	return out
}

// nodeNamed is the cluster's test node of that name.
func (c *cluster) nodeNamed(name string) *node {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range c.nodes {
		if n.name == name {
			return n
		}
	}
	c.t.Fatalf("no test node %s", name)
	return nil
}

// served GETs host/ through the ingress n times and returns how often
// each pod answered; false at the first request that fails or is not
// answered 200 by a pod within 5 s.
func (p *ingressProc) served(host string, n int) (map[podid.ID]int, bool) {
	pods := map[podid.ID]int{}
	for range n {
		ctx, cancel := context.WithTimeout(bg, 5*time.Second)
		r, err := p.get(ctx, host, "/")
		cancel()
		if err != nil || r.code != http.StatusOK || podOf(r.body) == "" {
			return nil, false
		}
		pods[podOf(r.body)]++
	}
	return pods, true
}

// waitServing waits until the ingress has n endpoints and serves host from
// n pods, and returns them.
func (p *ingressProc) waitServing(host string, n int, within time.Duration) map[podid.ID]bool {
	p.c.t.Helper()
	var pods map[podid.ID]int
	eventually(p.c.t, within, fmt.Sprintf("the ingress serving %s from %d pods", host, n), func() bool {
		if p.status().DataPath.Endpoints != n {
			return false
		}
		var ok bool
		pods, ok = p.served(host, 4*n)
		return ok && len(pods) == n
	})
	out := map[podid.ID]bool{}
	for id := range pods {
		out[id] = true
	}
	return out
}

// hammering is a stream of GETs through the ingress (hammer).
type hammering struct {
	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}

	mu       sync.Mutex
	total    int
	pods     map[podid.ID]int
	failures []string
}

// hammer GETs host/ through the ingress every 20 ms, one request at a time,
// until stopAndCheck, or until the test ends: a test that fails before
// stopAndCheck stops the GETs in its cleanup, before the ingress stops,
// and ends the one in flight (stopAndCheck waits for it instead, so that
// it counts).
func (p *ingressProc) hammer(host string) *hammering {
	ctx, cancel := context.WithCancel(bg)
	h := &hammering{stopCh: make(chan struct{}), done: make(chan struct{}), pods: map[podid.ID]int{}}
	p.c.t.Cleanup(func() {
		h.stopOnce.Do(func() { close(h.stopCh) })
		cancel()
		<-h.done
	})
	go func() {
		defer close(h.done)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-h.stopCh:
				return
			case <-tick.C:
			}
			rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
			start := time.Now()
			r, err := p.get(rctx, host, "/")
			rcancel()
			h.mu.Lock()
			h.total++
			switch {
			case err != nil:
				h.failures = append(h.failures, fmt.Sprintf("%s: %v", start.Format("15:04:05.000"), err))
			case r.code/100 != 2:
				h.failures = append(h.failures, fmt.Sprintf("%s: %d %s", start.Format("15:04:05.000"), r.code, strings.TrimSpace(r.body)))
			default:
				h.pods[podOf(r.body)]++
			}
			h.mu.Unlock()
		}
	}()
	return h
}

// stopAndCheck stops the GETs and fails the test on any failure, or when
// fewer than least were sent; it returns the pods that answered.
func (h *hammering) stopAndCheck(t *testing.T, least int) map[podid.ID]int {
	t.Helper()
	h.stopOnce.Do(func() { close(h.stopCh) })
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.failures) > 0 {
		t.Fatalf("%d of %d requests failed: %s", len(h.failures), h.total, strings.Join(h.failures[:min(len(h.failures), 10)], "; "))
	}
	if h.total < least {
		t.Fatalf("only %d requests were sent (want at least %d); the test proves nothing", h.total, least)
	}
	return h.pods
}

// Scenario: two ingress instances join a cluster of two instances with
// tokens of kind ingress, take /96s of the pods' mesh and become peers of
// every node, and every node theirs; ferun-ingress's version counts their
// entries. The admin API lists them connected. A restart on the same
// state directory keeps the identity and the place on the mesh without a
// join.
func TestIngressJoinAndMesh(t *testing.T) {
	t.Parallel()
	c := newCluster(t, ingressCluster())
	c.start("a")
	c.start("b")
	n1 := c.join("n1", meshNode)
	n2 := c.join("n2", meshNode)
	nodes := []*node{n1, n2}
	ula, meshed, err := c.st.PodNetwork(bg)
	if err != nil || !meshed {
		t.Fatalf("the cluster's ULA %v, meshed %v: %v", ula, meshed, err)
	}
	p0 := c.runIngress("ferun-ingress-0")
	p1 := c.runIngress("ferun-ingress-1", func(cfg *ingress.Config) { cfg.MeshEndpoint = "127.0.0.10:51820" })
	procs := []*ingressProc{p0, p1}
	for _, p := range procs {
		if len(p.cfg.ControlPlane) != 2 {
			t.Fatalf("--control-plane %v", p.cfg.ControlPlane)
		}
		p.waitReady(60 * time.Second)
	}

	rows := map[string]store.Ingress{}
	prefixes := map[string]bool{}
	for _, p := range procs {
		g, err := c.st.GetIngress(bg, p.name)
		if err != nil {
			t.Fatal(err)
		}
		rows[p.name] = g
		mc, ok := p.device().config()
		if !ok {
			t.Fatalf("%s: the mesh device was never configured", p.name)
		}
		key := mc.PrivateKey.PublicKey()
		prefix, err := netip.ParsePrefix(g.MeshPrefix)
		st := p.status()
		switch {
		case g.Status != model.IngressJoined || err != nil || prefix.Bits() != mesh.PrefixBits || !ula.Contains(prefix.Addr()) || prefixes[g.MeshPrefix]:
			t.Fatalf("%s: row %+v (%v); the ULA %v", p.name, g, err, ula)
		case g.MeshAddress != mesh.HostAddress(prefix).String() || mc.Address != netip.PrefixFrom(mesh.HostAddress(prefix), 128):
			t.Fatalf("%s: address %s, the device's %v, in %s", p.name, g.MeshAddress, mc.Address, g.MeshPrefix)
		case mc.PodNetwork != ula || mc.ListenPort != p.cfg.MeshPort || mc.Endpoint.String() != p.cfg.MeshEndpoint:
			t.Fatalf("%s: the device's configuration %+v", p.name, mc)
		case !bytes.Equal(g.MeshPublicKey, key[:]) || g.MeshEndpoint != p.cfg.MeshEndpoint || g.Version != "e2e" || g.GroupVersion <= 0:
			t.Fatalf("%s: row %+v; the device's key %s", p.name, g, key)
		case !st.Connected || st.Address != g.MeshAddress || st.Prefix != g.MeshPrefix || st.PodNetwork != ula.String() ||
			st.AdmittedVersion != uint64(g.GroupVersion) || st.CertSerial != g.CurrentSerial || st.AppsZone != appsZone:
			t.Fatalf("%s: status %+v; row %+v", p.name, st.Status, g)
		}
		prefixes[g.MeshPrefix] = true
		// A token pinned to the name joins it as a join again does
		// (kind rejoin), whether or not it existed.
		if n := c.count(`SELECT count(*) FROM ingress_certs WHERE ingress = $1 AND kind IN ('join', 'rejoin') AND serial = $2
			AND revoked_at IS NULL`, p.name, st.CertSerial); n != 1 {
			t.Fatalf("%s: %d join certificates with serial %s", p.name, n, st.CertSerial)
		}
	}
	if rows[p0.name].GroupVersion == rows[p1.name].GroupVersion {
		t.Fatalf("both entered ferun-ingress at version %d", rows[p0.name].GroupVersion)
	}
	members, version, err := c.st.IngressGroup(bg)
	want := []string{rows[p0.name].MeshAddress, rows[p1.name].MeshAddress}
	slices.Sort(want)
	slices.Sort(members)
	if err != nil || !slices.Equal(members, want) || version < max(rows[p0.name].GroupVersion, rows[p1.name].GroupVersion) {
		t.Fatalf("ferun-ingress %v at %d (%v); want %v", members, version, err, want)
	}

	// Every node has both as peers, with their keys, endpoints and /96s,
	// beside the other node.
	eventually(t, 60*time.Second, "the nodes' peers", func() bool {
		for _, n := range nodes {
			peers := map[string]*networkv1.MeshPeer{}
			for _, mp := range n.Mesh().GetPeers() {
				peers[mp.GetNode()] = mp
			}
			for _, p := range procs {
				g := rows[p.name]
				mp := peers[mesh.IngressPeerName(p.name)]
				if mp == nil || mp.GetEndpoint() != g.MeshEndpoint || mp.GetPodPrefix() != g.MeshPrefix ||
					!bytes.Equal(mp.GetPublicKey(), g.MeshPublicKey) {
					return false
				}
			}
			if len(peers) != len(procs)+len(nodes)-1 {
				return false
			}
		}
		return true
	})
	// Each ingress's device has both nodes, with their keys and /96s, and
	// counts them fresh.
	eventually(t, 30*time.Second, "the ingresses' peers", func() bool {
		for _, p := range procs {
			peers := p.device().peerList()
			if len(peers) != len(nodes) {
				return false
			}
			for _, n := range nodes {
				m := n.Mesh()
				pfx, err := netip.ParsePrefix(m.GetPodPrefix())
				if err != nil || !slices.ContainsFunc(peers, func(peer wg.Peer) bool {
					return bytes.Equal(peer.PublicKey[:], m.GetPublicKey()) && slices.Equal(peer.AllowedIPs, []netip.Prefix{pfx}) &&
						peer.Endpoint.IsValid()
				}) {
					return false
				}
			}
			st := p.status()
			if len(st.MeshPeers) != len(nodes) {
				return false
			}
			for _, mp := range st.MeshPeers {
				if !mp.Fresh {
					return false
				}
			}
		}
		return true
	})

	// The admin API lists both, joined and connected.
	r := c.call(nil, "GET", "/api/v1/admin/ingresses", adminAuth, "")
	var list struct {
		Ingresses []struct {
			Name            string `json:"name"`
			Status          string `json:"status"`
			MeshAddress     string `json:"meshAddress"`
			Connected       bool   `json:"connected"`
			AdmittedVersion int64  `json:"admittedVersion"`
		} `json:"ingresses"`
	}
	if err := json.Unmarshal([]byte(r.body), &list); r.code != http.StatusOK || err != nil || len(list.Ingresses) != 2 {
		t.Fatalf("ingresses: %d %s (%v)", r.code, r.body, err)
	}
	for _, g := range list.Ingresses {
		row := rows[g.Name]
		if g.Status != model.IngressJoined || !g.Connected || g.MeshAddress != row.MeshAddress || g.AdmittedVersion != row.GroupVersion {
			t.Fatalf("ingresses: %s", r.body)
		}
	}

	// A restart keeps the certificate and the place on the mesh: no join,
	// the same /96 and address, and it is ready again.
	serial := p0.status().CertSerial
	p0.restart()
	p0.waitReady(60 * time.Second)
	eventually(t, 30*time.Second, "the restarted ingress connected", func() bool { return p0.status().Connected })
	st := p0.status()
	g, err := c.st.GetIngress(bg, p0.name)
	if err != nil || st.CertSerial != serial || st.Address != rows[p0.name].MeshAddress || g.MeshPrefix != rows[p0.name].MeshPrefix ||
		g.GroupVersion != rows[p0.name].GroupVersion {
		t.Fatalf("after the restart: status %+v, row %+v (%v); before %+v", st.Status, g, err, rows[p0.name])
	}
	if n := c.count(`SELECT count(*) FROM ingress_certs WHERE ingress = $1`, p0.name); n != 1 {
		t.Fatalf("%d certificates of %s after the restart", n, p0.name)
	}
}

// Scenario: an environment's image service is served by the ingress only
// once the tenant makes the environment public. The first publish gives
// its host, rolls the service once to name the groups ing-<id>, whose
// rules let ferun-ingress in on the routed port, and records what the
// nodes admit; the ingress then serves the host round robin over the
// replicas with X-Forwarded-For and -Proto. Unpublishing takes the host
// away at once and empties the rules; publishing again rolls nothing.
func TestIngressPublishAndRoute(t *testing.T) {
	t.Parallel()
	c := newCluster(t, ingressCluster(func(cfg *config.Config) { cfg.Placement = config.PlacementSpread }))
	c.start("a")
	n1 := c.join("n1", meshNode)
	n2 := c.join("n2", meshNode)
	c.serveApps(n1, n2)
	p := c.runIngress("ferun-ingress-0")
	p.waitReady(60 * time.Second)
	envID := c.mkApp("web", 2, "")

	// Not public: no pod names an ingress group, and nothing is served.
	for _, r := range c.webRows(envID) {
		if tmpl := r.Sandbox.PodTemplate; tmpl == nil || tmpl.IngressGroup || slices.ContainsFunc(tmpl.SecurityGroups, func(g string) bool {
			return strings.HasPrefix(g, "ing-")
		}) {
			t.Fatalf("an instance of an environment never published: template %+v", tmpl)
		}
	}
	host := "web-" + tenant + "." + appsZone
	if r, err := p.get(bg, host, "/"); err != nil || r.code != http.StatusNotFound || !strings.Contains(r.body, "no such host") {
		t.Fatalf("before the publish: %+v %v", r, err)
	}

	if got := c.publishApp(envID, 2); got != host {
		t.Fatalf("host %s, want %s", got, host)
	}
	rows := c.webRows(envID)
	pods := p.waitServing(host, 2, 60*time.Second)
	if !samePods(pods, webPods(rows)) {
		t.Fatalf("served by %v; the instances %v", pods, webPods(rows))
	}
	// What the replica got.
	r, err := p.get(bg, host, "/hello")
	if err != nil || r.code != http.StatusOK || !strings.Contains(r.body, " host="+host+" path=/hello for=127.0.0.1 proto=https") {
		t.Fatalf("GET /hello: %+v %v", r, err)
	}
	// Round robin.
	counts, ok := p.served(host, 20)
	if !ok || len(counts) != 2 {
		t.Fatalf("round robin: %v", counts)
	}
	for id, n := range counts {
		if n < 6 {
			t.Fatalf("round robin: %s answered %d of 20", id, n)
		}
	}

	c.checkAdmitted(p, envID)
	// The environment's status names the host and the door.
	st := c.call(nil, "GET", "/api/v1/environments/"+envID.String(), tenantAuth, "")
	var es env.EnvStatus
	if err := json.Unmarshal([]byte(st.body), &es); st.code != http.StatusOK || err != nil || !es.Public ||
		!slices.Equal(es.Hosts, []string{host}) || len(es.Routes) != 1 || es.Routes[0].Door != env.DoorIngress ||
		es.Routes[0].URL != "https://"+host+"/" {
		t.Fatalf("status: %d %s (%v)", st.code, st.body, err)
	}

	// Unpublished: the host is gone at once, the rules in the next pass,
	// and nothing rolls.
	if ans := c.setPublic(envID, false); ans.Rolling || len(ans.Hosts) != 0 {
		t.Fatalf("unpublish: %+v", ans)
	}
	eventually(t, 10*time.Second, "the host gone", func() bool {
		r, err := p.get(bg, host, "/")
		return err == nil && r.code == http.StatusNotFound && strings.Contains(r.body, "no such host")
	})
	eventually(t, 60*time.Second, "the rules gone", func() bool {
		for _, row := range rows {
			ing := nodeGroup(c.nodeNamed(row.Sandbox.Node), model.IngressGroupID(row.Sandbox.ID))
			if ing == nil || len(ing.GetRules()) != 0 {
				return false
			}
		}
		return true
	})

	// Published again: the same host and pods, no roll.
	if ans := c.setPublic(envID, true); ans.Rolling || !slices.Equal(ans.Hosts, []string{host}) {
		t.Fatalf("publish again: %+v", ans)
	}
	again := p.waitServing(host, 2, 60*time.Second)
	if !samePods(again, pods) || !samePods(webPods(c.webRows(envID)), pods) {
		t.Fatalf("after publishing again: served by %v, the instances %v; before %v", again, webPods(c.webRows(envID)), pods)
	}
}

// samePods reports whether two sets of pods are equal.
func samePods(a, b map[podid.ID]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if !b[id] {
			return false
		}
	}
	return true
}

// Scenario: a release that changes the template rolls a public service's
// replicas while a client GETs every 20 ms through the ingress: not one
// request fails, and the new replicas answer at the end.
func TestIngressRollout(t *testing.T) {
	t.Parallel()
	c := newCluster(t, ingressCluster())
	c.start("a")
	n1 := c.join("n1", meshNode)
	c.serveApps(n1)
	p := c.runIngress("ferun-ingress-0")
	envID := c.mkApp("web", 2, "")
	host := c.publishApp(envID, 2)
	old := p.waitServing(host, 2, 60*time.Second)

	h := p.hammer(host)
	time.Sleep(500 * time.Millisecond)
	n := c.applyRelease(envID, appSpec(2, "", "2"))
	c.waitRelease(envID, n, model.ReleaseDeployed, 3*time.Minute)
	rows := c.waitWeb(envID, 2, "the new replicas", func(r store.EnvInstanceRow) bool { return !old[r.Sandbox.ID] })
	fresh := webPods(rows)
	eventually(t, 60*time.Second, "the new replicas serving", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		seen := 0
		for id := range fresh {
			if h.pods[id] > 0 {
				seen++
			}
		}
		return seen == len(fresh)
	})
	time.Sleep(time.Second)
	served := h.stopAndCheck(t, 100)
	for id := range old {
		if served[id] == 0 {
			t.Fatalf("the old replica %s answered nothing; served %v", id, served)
		}
	}
	if got := p.waitServing(host, 2, 30*time.Second); !samePods(got, fresh) {
		t.Fatalf("after the rollout: served by %v, the instances %v", got, fresh)
	}
	c.checkAdmitted(p, envID)
}

// Scenario: a public idle service's replica paused by the engine is woken
// by a request through the ingress, which gets its answer and holds the
// replica; the replica stays admitted. Paused again with a slow resume, a
// request wakes it, and another that comes once the ingress's table shows
// the replica waking is held with the wake bound, not the (shortened)
// pending bound, and gets its answer too.
func TestIngressWake(t *testing.T) {
	t.Parallel()
	const pendingHold = 2 * time.Second
	c := newCluster(t, ingressCluster())
	a := c.start("a")
	n1 := c.join("n1", meshNode)
	c.serveApps(n1)
	p := c.runIngress("ferun-ingress-0", func(cfg *ingress.Config) { cfg.PendingHold = pendingHold })
	envID := c.mkApp("web", 1, "1m")
	host := c.publishApp(envID, 1)
	var id podid.ID
	for pod := range p.waitServing(host, 1, 60*time.Second) {
		id = pod
	}
	c.checkAdmitted(p, envID)

	pause := func() {
		t.Helper()
		if err := a.Engine.Pause(bg, id); err != nil {
			t.Fatalf("pause %s: %v", id, err)
		}
		c.waitState(id, model.StatePaused, 30*time.Second)
		eventually(t, 10*time.Second, "the ingress without endpoints", func() bool { return p.status().DataPath.Endpoints == 0 })
	}
	// woken checks the replica after a request woke it and was served:
	// running, held by the ingress for that request (the hold lasts at
	// least a renewal past the request), and admitted as before.
	woken := func(what string) {
		t.Helper()
		if sb := c.row(id); sb.ClientState != model.StateRunning {
			t.Fatalf("%s: %s %s", what, sb.ClientState, sb.Op)
		}
		if !c.ingressHeld(p, id, a.ID()) {
			t.Fatalf("%s: no hold of %s on %s", what, store.IngressHolder(p.name), id)
		}
		c.checkAdmitted(p, envID)
	}
	type result struct {
		r    reply
		err  error
		took time.Duration
	}
	get := func(results chan<- result) {
		go func() {
			ctx, cancel := context.WithTimeout(bg, 60*time.Second)
			defer cancel()
			start := time.Now()
			r, err := p.get(ctx, host, "/")
			results <- result{r, err, time.Since(start)}
		}()
	}

	pause()
	results := make(chan result, 2)
	get(results)
	res := <-results
	if res.err != nil || res.r.code != http.StatusOK || podOf(res.r.body) != id {
		t.Fatalf("a request to the paused replica: %+v %v", res.r, res.err)
	}
	woken("after the wake")

	pause()
	// Long enough that the request below, sent up to a few seconds into
	// the resume, still waits for longer than the pending bound.
	const resumeDelay = 8 * time.Second
	n1.Script().SetDelay(podv1.PodService_ResumePod_FullMethodName, resumeDelay)
	defer n1.Script().SetDelay(podv1.PodService_ResumePod_FullMethodName, 0)
	get(results)
	eventually(t, 30*time.Second, "the resume under way", func() bool {
		sb := c.row(id)
		return sb.Op == string(engine.OpResume) || sb.ClientState == model.StateResuming
	})
	// The next request must meet the replica waking in the ingress's
	// table, not still asleep there: an asleep service gets the wake
	// bound anyway.
	eventually(t, 5*time.Second, "the ingress's table showing web waking", func() bool {
		svc, ok := p.cachedService(envID, "web")
		return ok && svc.Waking == 1 && len(svc.Endpoints) == 0
	})
	during := make(chan result, 1)
	get(during)
	if res := <-results; res.err != nil || res.r.code != http.StatusOK || podOf(res.r.body) != id {
		t.Fatalf("the waking request: %+v %v", res.r, res.err)
	}
	res = <-during
	if res.err != nil || res.r.code != http.StatusOK || podOf(res.r.body) != id {
		t.Fatalf("a request during the resume: %+v %v (after %v)", res.r, res.err, res.took)
	}
	if res.took <= pendingHold {
		t.Fatalf("the request during the resume took %v, not longer than the pending bound %v; the test proves nothing", res.took, pendingHold)
	}
	woken("after the slow resume")
}

// Scenario: while a slow request through the ingress is open, its
// replica of an idle service is held by the ingress (a hold of kind
// forward, holder ingress:<name>) and never paused, however long ago its
// last activity looks to the idle loop, for longer than one admit's
// validity and lease; once the request ended and the ingress released the
// hold, the replica pauses when idle.
func TestIngressHoldsIdle(t *testing.T) {
	t.Parallel()
	c := newCluster(t, ingressCluster())
	a := c.start("a")
	n1 := c.join("n1", meshNode)
	replicas := c.serveApps(n1)
	p := c.runIngress("ferun-ingress-0")
	envID := c.mkApp("web", 1, "1m")
	host := c.publishApp(envID, 1)
	var id podid.ID
	for pod := range p.waitServing(host, 1, 60*time.Second) {
		id = pod
	}
	c.checkAdmitted(p, envID)
	held := func() bool { return c.ingressHeld(p, id, a.ID()) }
	// idleNow makes the replica look idle for ten minutes: the service's
	// idle (1m) is long past, and the idle loop, every second, pauses it
	// unless something keeps it.
	idleNow := func() {
		c.sql(`UPDATE sandboxes SET last_active_at = now() - interval '10 minutes' WHERE id = $1`, id)
	}

	// waitServing's requests hold the replica until the ingress finds it
	// quiet and releases it, within two renewals (10 s each); and the idle
	// loop acts only once the leader has led for the engine's outage guard
	// (15 s). The window below begins after both, so that only the open
	// request can keep the replica. (The release sets last_active_at, so
	// the replica stays running for its idle of 1m meanwhile.)
	eventually(t, 45*time.Second, "waitServing's hold released", func() bool { return !held() })
	eventually(t, 30*time.Second, "the leader settled", func() bool {
		since := a.Elector.LeadingSince()
		return !since.IsZero() && time.Since(since) > 16*time.Second
	})

	type result struct {
		r   reply
		err error
	}
	slow := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(bg, 2*time.Minute)
		defer cancel()
		r, err := p.get(ctx, host, "/slow")
		slow <- result{r, err}
	}()
	eventually(t, 10*time.Second, "the slow request at the replica", func() bool { return replicas.slowOpen.Load() == 1 })
	eventually(t, 10*time.Second, "the ingress's hold", held)
	// The admit is valid for 20 s (lease 30 s less the margin of 10 s) and
	// its row's lease runs 30 s; the window runs past both, over three
	// renewals, so only renewals for the open request keep the hold.
	const window = 32 * time.Second
	for deadline := time.Now().Add(window); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		idleNow()
		if sb := c.row(id); sb.ClientState != model.StateRunning || sb.Op != "" {
			t.Fatalf("the replica with a request open: %s %s", sb.ClientState, sb.Op)
		}
		if !held() {
			t.Fatal("the hold lapsed while the request is open")
		}
	}
	replicas.releaseSlow()
	if res := <-slow; res.err != nil || res.r.code != http.StatusOK || podOf(res.r.body) != id {
		t.Fatalf("the slow request: %+v %v", res.r, res.err)
	}

	// Quiet, the replica's hold is released within two renewals (10 s
	// each), and then it pauses once idle.
	eventually(t, 45*time.Second, "the hold released", func() bool { return !held() })
	idleNow()
	c.waitState(id, model.StatePaused, 30*time.Second)
}

// Scenario: cut off from the control plane, the ingress serves on from
// its table and stays ready; restarted while cut off, it serves from its
// cache at once. What changed meanwhile (a replica more) arrives when the
// cut heals, in the whole table of the new connection.
func TestIngressSurvivesControlPlane(t *testing.T) {
	t.Parallel()
	c := newCluster(t, ingressCluster())
	c.start("a")
	n1 := c.join("n1", meshNode)
	c.serveApps(n1)
	p := c.runIngress("ferun-ingress-0")
	envID := c.mkApp("web", 1, "")
	host := c.publishApp(envID, 1)
	pods := p.waitServing(host, 1, 60*time.Second)
	eventually(t, 30*time.Second, "the table cached", func() bool {
		b, err := os.ReadFile(filepath.Join(p.cfg.StateDir, "table.json"))
		return err == nil && bytes.Contains(b, []byte(host))
	})

	servedFrom := func(what string, want map[podid.ID]bool) {
		t.Helper()
		got, ok := p.served(host, 4)
		if !ok {
			t.Fatalf("%s: a request failed", what)
		}
		for id := range got {
			if !want[id] {
				t.Fatalf("%s: served by %v; want %v", what, got, want)
			}
		}
	}

	p.cutOff()
	eventually(t, 30*time.Second, "the ingress disconnected", func() bool { return !p.status().Connected })
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		servedFrom("cut off", pods)
		if ok, why := p.ready(); !ok {
			t.Fatalf("cut off, not ready: %s", why)
		}
	}

	// Restarted while cut off, it serves from the cache.
	p.restart()
	p.waitReady(30 * time.Second)
	if st := p.status(); st.Connected || !st.HasState {
		t.Fatalf("restarted while cut off: %+v", st.Status)
	}
	servedFrom("restarted while cut off", pods)

	// Meanwhile the service gets a second replica, which the ingress
	// cannot know yet.
	n := c.applyRelease(envID, appSpec(2, "", "1"))
	c.waitRelease(envID, n, model.ReleaseDeployed, 2*time.Minute)
	both := webPods(c.waitWeb(envID, 2, "two replicas", namesIngGroup))
	if ep := p.status().DataPath.Endpoints; ep != 1 {
		t.Fatalf("cut off, the ingress has %d endpoints", ep)
	}
	servedFrom("cut off after the scale-up", pods)

	// Healed, it connects again and serves both.
	p.heal()
	eventually(t, 60*time.Second, "the ingress connected", func() bool { return p.status().Connected })
	if got := p.waitServing(host, 2, 60*time.Second); !samePods(got, both) {
		t.Fatalf("after the cut: served by %v; the instances %v", got, both)
	}
	c.checkAdmitted(p, envID)
}

// Scenario: --control-plane lists both instances; when the one the
// ingress is connected to stops, the ingress moves to the other while a
// client GETs every 20 ms, and not one request fails. The table follows
// on the new connection: a scale-up and an unpublish arrive.
func TestIngressMovesInstance(t *testing.T) {
	t.Parallel()
	c := newCluster(t, ingressCluster())
	a := c.start("a")
	b := c.start("b")
	n1 := c.join("n1", meshNode)
	c.serveApps(n1)
	p := c.runIngress("ferun-ingress-0")
	if !slices.Equal(p.cfg.ControlPlane, []string{a.NodeAddr(), b.NodeAddr()}) {
		t.Fatalf("--control-plane %v", p.cfg.ControlPlane)
	}
	envID := c.mkApp("web", 2, "")
	host := c.publishApp(envID, 2)
	p.waitServing(host, 2, 60*time.Second)
	eventually(t, 30*time.Second, "the ingress connected", func() bool { return p.status().Connected })
	from, to := a, b
	if p.status().Instance == b.ID() {
		from, to = b, a
	}

	h := p.hammer(host)
	time.Sleep(500 * time.Millisecond)
	if err := from.stop(); err != nil {
		t.Fatalf("stop %s: %v", from.name, err)
	}
	eventually(t, 60*time.Second, "the ingress connected to "+to.name, func() bool {
		st := p.status()
		return st.Connected && st.Instance == to.ID()
	})
	time.Sleep(500 * time.Millisecond)
	h.stopAndCheck(t, 20)

	// The table follows on the new instance.
	n := c.applyRelease(envID, appSpec(3, "", "1"))
	c.waitRelease(envID, n, model.ReleaseDeployed, 2*time.Minute)
	all := webPods(c.waitWeb(envID, 3, "three replicas", namesIngGroup))
	if got := p.waitServing(host, 3, 60*time.Second); !samePods(got, all) {
		t.Fatalf("after the scale-up: served by %v; the instances %v", got, all)
	}
	c.checkAdmitted(p, envID)
	c.setPublic(envID, false)
	eventually(t, 10*time.Second, "the host gone", func() bool {
		r, err := p.get(bg, host, "/")
		return err == nil && r.code == http.StatusNotFound
	})
}

// Scenario: an ingress removed by the admin loses its certificates and
// its place: it leaves ferun-ingress at a new version and the nodes'
// peers and group, the agent reports the removal, and readiness fails.
func TestIngressRemoved(t *testing.T) {
	t.Parallel()
	c := newCluster(t, ingressCluster())
	c.start("a")
	n1 := c.join("n1", meshNode)
	c.serveApps(n1)
	p := c.runIngress("ferun-ingress-0")
	envID := c.mkApp("web", 1, "")
	host := c.publishApp(envID, 1)
	p.waitServing(host, 1, 60*time.Second)
	p.waitReady(30 * time.Second)
	g, err := c.st.GetIngress(bg, p.name)
	if err != nil {
		t.Fatal(err)
	}
	hasPeer := func() bool {
		return slices.ContainsFunc(n1.Mesh().GetPeers(), func(mp *networkv1.MeshPeer) bool {
			return mp.GetNode() == mesh.IngressPeerName(p.name)
		})
	}
	listed := func() bool {
		peers := nodeGroup(n1, model.IngressPeersGroup)
		return peers != nil && slices.Contains(peers.GetMembers(), g.MeshAddress)
	}
	eventually(t, 30*time.Second, "the ingress a peer of n1 and in its group", func() bool { return hasPeer() && listed() })
	_, before, err := c.st.IngressGroup(bg)
	if err != nil {
		t.Fatal(err)
	}

	if r := c.call(nil, "DELETE", "/api/v1/admin/ingresses/"+p.name, adminAuth, ""); r.code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", r.code, r.body)
	}
	if r := c.call(nil, "DELETE", "/api/v1/admin/ingresses/nope", adminAuth, ""); r.code != http.StatusNotFound {
		t.Fatalf("remove an unknown ingress: %d %s", r.code, r.body)
	}
	g, err = c.st.GetIngress(bg, p.name)
	if err != nil || g.Status != model.IngressRemoved || g.RemovedAt.IsZero() {
		t.Fatalf("the removed ingress's row: %+v %v", g, err)
	}
	if n := c.count(`SELECT count(*) FROM ingress_certs WHERE ingress = $1 AND (revoked_at IS NULL OR revoked_reason <> 'removed')`, p.name); n != 0 {
		t.Fatalf("%d certificates of the removed ingress are not revoked for the removal", n)
	}
	members, after, err := c.st.IngressGroup(bg)
	if err != nil || len(members) != 0 || after <= before {
		t.Fatalf("ferun-ingress after the removal: %v at %d (before %d): %v", members, after, before, err)
	}

	eventually(t, 60*time.Second, "n1 without the ingress", func() bool {
		peers := nodeGroup(n1, model.IngressPeersGroup)
		nr, err := c.st.GetNode(bg, n1.name)
		return !hasPeer() && peers != nil && len(peers.GetMembers()) == 0 && int64(peers.GetVersion()) >= after &&
			err == nil && nr.IngressGroupVersion >= after
	})
	eventually(t, 30*time.Second, "the agent's removal", func() bool { return p.status().IdentityRemoved })
	if ok, why := p.ready(); ok || !strings.Contains(why, "removed") {
		t.Fatalf("readiness after the removal: %v %q", ok, why)
	}
	if code, body := p.health("/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "removed") {
		t.Fatalf("/readyz after the removal: %d %q", code, body)
	}
	r := c.call(nil, "GET", "/api/v1/admin/ingresses", adminAuth, "")
	if r.code != http.StatusOK || !strings.Contains(r.body, `"status":"`+model.IngressRemoved+`"`) {
		t.Fatalf("ingresses after the removal: %d %s", r.code, r.body)
	}
}
