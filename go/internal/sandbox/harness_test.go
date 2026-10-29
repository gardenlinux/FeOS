// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/fleet"
	"github.com/gardenlinux/feos/go/internal/leader"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

var bg = context.Background()

const (
	testTD     = "feos.test"
	testTenant = "t1"
)

// issuerDir holds the platform CA of every test of the package.
var issuerDir string

func TestMain(m *testing.M) {
	base, err := os.MkdirTemp("", "sandbox-test-ca-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	issuerDir = filepath.Join(base, "ca")
	err = pki.InitCA(pki.InitCAOptions{RootDir: filepath.Join(base, "root"), Dir: issuerDir, TrustDomain: testTD,
		DNSDomains: []string{testTD}})
	code := 1
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	} else {
		code = m.Run()
	}
	os.RemoveAll(base)
	os.Exit(code)
}

// instance is one ferund instance of a test: its nodes' port, fleet,
// engine, elector and heartbeat.
type instance struct {
	id   string
	srv  *nodelink.Server
	fl   *fleet.Service
	eng  *Engine
	el   *leader.Elector
	hb   *leader.Heartbeat
	stop func()
}

type harness struct {
	t    *testing.T
	d    *db.DB
	st   *store.Store
	iss  *pki.Issuer
	objs *testnode.ObjectStore
	ins  []*instance

	mu    sync.Mutex
	nodes map[string]*testnode.Node
}

// newHarness gives the test a database with the tenant t1 and one
// instance; mod changes the engine's configuration.
func newHarness(t *testing.T, mod ...func(*Config)) *harness {
	t.Helper()
	t.Parallel()
	d := dbtest.New(t)
	iss, err := pki.LoadIssuer(issuerDir, testTD)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, d: d, st: store.New(d), iss: iss, objs: testnode.NewObjectStore("store-1"),
		nodes: map[string]*testnode.Node{}}
	if _, _, err := h.st.CreateTenant(bg, testTenant); err != nil {
		t.Fatal(err)
	}
	h.addInstance("i1", mod...)
	return h
}

// addInstance starts an instance; its engine has the tests' timings.
func (h *harness) addInstance(id string, mod ...func(*Config)) *instance {
	t := h.t
	t.Helper()
	log := dbtest.Logger(t)
	in := &instance{id: id}
	src, err := pki.NewServerCertSource(h.iss, []string{h.iss.ControlPlaneDomain()}, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := nodelink.New(nodelink.Config{
		Addr: "127.0.0.1:0", TrustDomain: testTD, InstanceID: id, Bundle: h.iss.Bundle(),
		ServerCert: src.GetCertificate, NodeWait: 500 * time.Millisecond, ConnDrainTimeout: time.Minute,
		CheckNode: func(ctx context.Context, node, serial, addr string, atAccept bool) (time.Time, error) {
			return in.fl.CheckNode(ctx, node, serial, addr, atAccept)
		},
		OnPresented: func(node, serial, addr string) { in.fl.OnPresented(node, serial, addr) },
		OnNodeConn:  func(node string) { in.fl.OnNodeConn(node) },
		Log:         log,
	})
	if err != nil {
		t.Fatal(err)
	}
	in.srv = srv
	in.el = leader.NewElector(h.d, id, log)
	cfg := Config{
		InstanceID: id, TrustDomain: testTD, Placement: PlacementBinpack, DiskBudget: 1 << 30,
		NodeWait: 500 * time.Millisecond, Log: log,
	}
	for _, m := range mod {
		m(&cfg)
	}
	in.eng, err = New(cfg, Deps{DB: h.d, Conns: srv, Elector: in.el})
	if err != nil {
		t.Fatal(err)
	}
	in.eng.resolveImage = fakeResolve
	// The images of the tests name no registry that answers (ADR 0025).
	in.eng.checkImage = func(context.Context, string) error { return nil }
	in.eng.t.waitPoll = 100 * time.Millisecond
	in.eng.t.deferredEvery = 500 * time.Millisecond
	// The tests look for orphaned ops themselves (Engine.orphans).
	in.eng.t.orphanEvery = time.Hour
	// The tests call the loops directly; TestLoopsWaitAfterOutage covers the guard.
	in.eng.t.outageGuard = 0
	in.fl = fleet.New(fleet.Config{InstanceID: id, TrustDomain: testTD, RolloutGrace: 30 * time.Second,
		NodeTTL: time.Hour, ConnDrainTimeout: time.Minute, Log: log},
		fleet.Deps{DB: h.d, Issuer: h.iss, Links: srv, Hooks: in.eng, Elector: in.el})
	fleetv1.RegisterFleetServiceServer(srv.Uplink(), in.fl.UplinkServer())
	fleetv1.RegisterFleetServiceServer(srv.Join(), in.fl.JoinServer())
	in.hb = leader.NewHeartbeat(h.d, leader.InstanceInfo{ID: id, NodeAddress: srv.Addr().String()}, log)

	ctx, cancel := context.WithCancel(bg)
	var wg sync.WaitGroup
	run := func(f func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f(ctx)
		}()
	}
	run(srv.Serve)
	run(in.fl.Run)
	run(in.eng.Run)
	run(in.el.Run)
	run(in.hb.Run)
	var once sync.Once
	in.stop = func() {
		once.Do(func() {
			sctx, scancel := context.WithTimeout(bg, 10*time.Second)
			srv.Shutdown(sctx)
			scancel()
			cancel()
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Errorf("instance %s did not stop", id)
			}
		})
	}
	t.Cleanup(in.stop)
	h.ins = append(h.ins, in)
	if len(h.ins) == 1 {
		eventually(t, 10*time.Second, "instance "+id+" leads", in.el.IsLeader)
	}
	return in
}

// failNextOp makes the next op of the first instance find no connection
// to its target after its begin, running before first (when set).
func (h *harness) failNextOp(before func()) {
	var once sync.Once
	h.eng().connHook = func(hd *OpHandle) error {
		var err error
		once.Do(func() {
			if before != nil {
				before()
			}
			err = status.Errorf(codes.Unavailable, "node %s is not connected (injected)", hd.target)
		})
		return err
	}
}

// settle waits until the nodes' events reached the engines and the syncs
// and tombstone runs of the sandbox that they and the ended ops asked for
// ran, and syncs the sandbox once more: nothing from earlier ops arrives
// after it. Ops still in flight are not waited for.
func (h *harness) settle(id podid.ID) {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		h.eventsHandedOver(time.Until(deadline))
		if !h.syncPending(id) {
			break
		}
		// What runs may make more events: wait for it, then look again.
		eventually(h.t, time.Until(deadline), "the syncs and tombstone runs of sandbox "+id.String(), func() bool {
			return !h.syncPending(id)
		})
	}
	if err := h.eng().Sync(bg, id); err != nil {
		h.t.Fatal(err)
	}
}

// eventsHandedOver waits until every event that the nodes sent so far
// reached an engine. Fleet takes a node's batches one at a time and hands
// a batch's pod events to the engine (PodChanged) after its ack, before it
// reads the next batch: a marker acknowledged after another acknowledged
// marker comes after the PodChanged of every event before the first. A
// node that no instance is connected to gets no marker; its events must
// be acknowledged all the same.
func (h *harness) eventsHandedOver(within time.Duration) {
	h.t.Helper()
	h.mu.Lock()
	nodes := make(map[string]*testnode.Node, len(h.nodes))
	maps.Copy(nodes, h.nodes)
	h.mu.Unlock()
	acked := func() bool {
		for _, n := range nodes {
			if n.EventsPending() > 0 {
				return false
			}
		}
		return true
	}
	for range 2 {
		for name, n := range nodes {
			if h.connected(name) {
				n.Publish(settleMarker())
			}
		}
		eventually(h.t, within, "events acknowledged", acked)
	}
}

// settleMarker is the event that settle publishes: fleet stores it as an
// activity row and does nothing else with it.
func settleMarker() *fleetv1.Event {
	return &fleetv1.Event{Body: &fleetv1.Event_Node{Node: &fleetv1.NodeEvent{Kind: "test_settle"}}}
}

// connected reports whether a running instance is connected to node.
func (h *harness) connected(node string) bool {
	for _, in := range h.ins {
		if !in.eng.ended() && in.srv.Connected(node) {
			return true
		}
	}
	return false
}

// syncPending reports whether an engine that runs still has work of the
// sandbox to do: a sync running or queued, a sync deferred behind an op
// that has ended (the next look at the deferred runs it), or a run of a
// tombstone of its pod.
func (h *harness) syncPending(id podid.ID) bool {
	h.t.Helper()
	for _, in := range h.ins {
		e := in.eng
		if e.ended() {
			continue
		}
		e.syncs.mu.Lock()
		_, syncing := e.syncs.m[id]
		e.syncs.mu.Unlock()
		e.tombsMu.Lock()
		tombs := false
		for k := range e.tombs {
			tombs = tombs || k.PodID == id
		}
		e.tombsMu.Unlock()
		e.mu.Lock()
		_, deferred := e.deferred[id]
		e.mu.Unlock()
		if syncing || tombs {
			return true
		}
		if deferred {
			row, err := h.st.GetSandbox(bg, id)
			if err != nil && !errors.Is(err, model.ErrNotFound) {
				h.t.Fatal(err)
			}
			if err != nil || row.Op == "" {
				return true
			}
		}
	}
	return false
}

// ended reports whether the engine's Run ended: it runs nothing more.
func (e *Engine) ended() bool {
	e.bgMu.Lock()
	defer e.bgMu.Unlock()
	return e.bgDone
}

// eng is the first instance's engine.
func (h *harness) eng() *Engine { return h.ins[0].eng }

// fakeResolve pins an image without a registry: a digest reference as it
// is, a tag to a digest made from its name.
func fakeResolve(_ context.Context, image string) (string, error) {
	ref, err := parseImageRef(image)
	if err != nil {
		return "", err
	}
	if ref.digest() {
		return ref.pinned(ref.ref), nil
	}
	return ref.pinned(fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(image)))), nil
}

// addNode joins a test node and waits until every instance has its node
// connection, it is ready, and its first inventory reconcile is done.
func (h *harness) addNode(name string, mod ...func(*testnode.Config)) *testnode.Node {
	t := h.t
	t.Helper()
	_, token, _, err := h.ins[0].fl.IssueJoinToken(bg, "test", name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testnode.Config{
		Name: name, JoinToken: token, ServerName: h.iss.ControlPlaneDomain(), TrustDomain: testTD,
		Addrs: []string{h.ins[0].srv.Addr().String()}, ObjectStore: h.objs, PendingDelay: 20 * time.Millisecond,
		Log: dbtest.Logger(t),
	}
	for _, m := range mod {
		m(&cfg)
	}
	n, err := testnode.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Errorf("node %s did not stop", name)
		}
	})
	h.mu.Lock()
	h.nodes[name] = n
	h.mu.Unlock()
	eventually(t, 20*time.Second, "node "+name+" connected and ready", func() bool {
		for _, in := range h.ins {
			if !in.srv.Connected(name) {
				return false
			}
		}
		nd, err := h.st.GetNode(bg, name)
		return err == nil && nd.Status == model.NodeReady && !nd.ReconciledAt.IsZero()
	})
	return n
}

func (h *harness) node(name string) *testnode.Node {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nodes[name]
}

// pods is a PodService client on the first instance's connection to node.
func (h *harness) pods(node string) podv1.PodServiceClient {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	cc, err := h.ins[0].srv.Conn(ctx, node)
	if err != nil {
		h.t.Fatal(err)
	}
	return podv1.NewPodServiceClient(cc)
}

// podSpec is the spec the engine builds for a row without a secret
// resolver (an ordinary sandbox's, or a template's without secrets).
func podSpec(sb store.Sandbox) *podv1.PodSpec {
	spec, err := buildPodSpec(bg, sb, nil)
	if err != nil {
		panic(err)
	}
	return spec
}

// testImage is a pinned image: no registry is asked.
const testImage = "docker.io/library/alpine@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1"

// sandbox inserts a sandbox of the test tenant.
func (h *harness) sandbox(memoryMiB int) store.Sandbox {
	return h.sandboxWithID(podid.Mint(), memoryMiB)
}

// legacyID is a new UUID as a pod ID: the ID of a sandbox made before ADR
// 0019, which keeps it.
func legacyID() podid.ID { return podid.ID(uuid.NewString()) }

func (h *harness) sandboxWithID(id podid.ID, memoryMiB int) store.Sandbox {
	h.t.Helper()
	sb, created, err := h.st.CreateSandbox(bg, store.NewSandbox{ID: id, Owner: testTenant, Image: testImage, VCPUs: 1,
		MemoryMiB: memoryMiB})
	if err != nil || !created {
		h.t.Fatalf("create sandbox: %v (created %v)", err, created)
	}
	return sb
}

func (h *harness) row(id podid.ID) store.Sandbox {
	h.t.Helper()
	sb, err := h.st.GetSandbox(bg, id)
	if err != nil {
		h.t.Fatalf("sandbox %s: %v", id, err)
	}
	return sb
}

// ensure runs Ensure on the first instance and fails the test on an
// error.
func (h *harness) ensure(id podid.ID) model.Lease {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(bg, 60*time.Second)
	defer cancel()
	lease, err := h.eng().Ensure(ctx, id)
	if err != nil {
		h.t.Fatalf("ensure %s: %v", id, err)
	}
	return lease
}

// running creates a sandbox and ensures it.
func (h *harness) running(memoryMiB int) store.Sandbox {
	h.t.Helper()
	sb := h.sandbox(memoryMiB)
	h.ensure(sb.ID)
	return h.row(sb.ID)
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.d.Pool.Exec(bg, sql, args...); err != nil {
		h.t.Fatalf("%s: %v", sql, err)
	}
}

func (h *harness) scalar(dst any, sql string, args ...any) {
	h.t.Helper()
	if err := h.d.Pool.QueryRow(bg, sql, args...).Scan(dst); err != nil {
		h.t.Fatalf("%s: %v", sql, err)
	}
}

// waitState waits until the sandbox is in state without an op.
func (h *harness) waitState(id podid.ID, state string, within time.Duration) store.Sandbox {
	h.t.Helper()
	var sb store.Sandbox
	eventually(h.t, within, fmt.Sprintf("sandbox %s %s", id, state), func() bool {
		var err error
		sb, err = h.st.GetSandbox(bg, id)
		return err == nil && sb.ClientState == state && sb.Op == ""
	})
	return sb
}

// podState waits until node holds the pod in state.
func (h *harness) podState(node string, id podid.ID, state podv1.PodState) *podv1.PodInfo {
	h.t.Helper()
	var info *podv1.PodInfo
	eventually(h.t, 15*time.Second, fmt.Sprintf("pod %s %s on %s", id, state, node), func() bool {
		var err error
		info, err = h.pods(node).GetPod(bg, &podv1.GetPodRequest{PodId: id.String()})
		return err == nil && info.State == state
	})
	return info
}

// noPod waits until node does not hold the pod.
func (h *harness) noPod(node string, id podid.ID) {
	h.t.Helper()
	eventually(h.t, 15*time.Second, fmt.Sprintf("pod %s gone from %s", id, node), func() bool {
		_, err := h.pods(node).GetPod(bg, &podv1.GetPodRequest{PodId: id.String()})
		return status.Code(err) == codes.NotFound
	})
}

// booking returns the sandbox's booking, and false without one.
func (h *harness) booking(id podid.ID) (store.Booking, bool) {
	h.t.Helper()
	b, err := h.st.GetBooking(bg, id)
	if errors.Is(err, store.ErrNoRow) {
		return store.Booking{}, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return b, true
}

func (h *harness) tombstones(id podid.ID) []store.Tombstone {
	h.t.Helper()
	ts, err := h.st.TombstonesOfPod(bg, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return ts
}

// deadInstance makes an instance row that is not live: seen long ago.
func (h *harness) deadInstance(id string) {
	h.exec(`INSERT INTO instances (id, last_seen) VALUES ($1, now() - interval '1 hour')
		ON CONFLICT (id) DO UPDATE SET last_seen = excluded.last_seen`, id)
}

// liveInstance makes an instance row that is live.
func (h *harness) liveInstance(id string) {
	h.exec(`INSERT INTO instances (id, last_seen) VALUES ($1, now())
		ON CONFLICT (id) DO UPDATE SET last_seen = now(), stopping = NULL`, id)
}

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", within, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func never(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("happened: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func wantState(t *testing.T, sb store.Sandbox, state string) {
	t.Helper()
	if sb.ClientState != state {
		t.Fatalf("sandbox %s is %s (%q), want %s", sb.ID, sb.ClientState, sb.StatusDescription, state)
	}
}

func wantErr(t *testing.T, err, kind error, text string) {
	t.Helper()
	if err == nil || (kind != nil && !errors.Is(err, kind)) || !strings.Contains(err.Error(), text) {
		t.Fatalf("got error %v, want %v containing %q", err, kind, text)
	}
}

// gate holds the first call of method on node until release is called;
// arrived is closed when that call reached the node. Later calls pass.
func (h *harness) gate(node, method string) (arrived <-chan struct{}, release func()) {
	in := make(chan struct{})
	out := make(chan struct{})
	var first, once sync.Once
	h.node(node).Script().SetHook(method, func(ctx context.Context, _ string, _ any) error {
		held := false
		first.Do(func() {
			held = true
			close(in)
		})
		if !held {
			return nil
		}
		select {
		case <-out:
		case <-ctx.Done():
		}
		return nil
	})
	return in, func() { once.Do(func() { close(out) }) }
}

// holdPodReads holds every ListPods of node, and its GetPods of pods (of
// every pod when none is named), until release (or until the call's ctx
// ends): meanwhile no sync or reconcile observes those pods, so a state
// that they resolve (unknown) can be read before they do. The test's end
// releases them.
func (h *harness) holdPodReads(node string, pods ...podid.ID) (release func()) {
	out := make(chan struct{})
	hold := func(ctx context.Context, _ string, _ any) error {
		select {
		case <-out:
			return nil
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
	getPod := hold
	if len(pods) > 0 {
		getPod = func(ctx context.Context, method string, req any) error {
			if r, ok := req.(*podv1.GetPodRequest); ok && !slices.Contains(pods, podid.ID(r.GetPodId())) {
				return nil
			}
			return hold(ctx, method, req)
		}
	}
	s := h.node(node).Script()
	s.SetHook(podv1.PodService_GetPod_FullMethodName, getPod)
	s.SetHook(podv1.PodService_ListPods_FullMethodName, hold)
	var once sync.Once
	release = func() {
		once.Do(func() {
			s.SetHook(podv1.PodService_GetPod_FullMethodName, nil)
			s.SetHook(podv1.PodService_ListPods_FullMethodName, nil)
			close(out)
		})
	}
	h.t.Cleanup(release)
	return release
}

// waitClosed waits for ch, or fails the test.
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(15 * time.Second):
		t.Fatalf("not within 15s: %s", what)
	}
}

// calls watches the calls of one method of a node, or of one of its script
// points: it counts them, keeps their requests, and with hold holds the
// first until release, or until its ctx ends (a hibernation's upload
// point's ends when a resume abandons it). Later calls pass.
type calls struct {
	arrived chan struct{}
	out     chan struct{}
	once    sync.Once

	mu   sync.Mutex
	reqs []any
}

// watchCalls hooks method on node with a calls; with hold it holds the
// first call. The test's end releases it.
func (h *harness) watchCalls(node, method string, hold bool) *calls {
	c := &calls{arrived: make(chan struct{}), out: make(chan struct{})}
	h.node(node).Script().SetHook(method, func(ctx context.Context, _ string, req any) error {
		c.mu.Lock()
		c.reqs = append(c.reqs, req)
		first := len(c.reqs) == 1
		c.mu.Unlock()
		if !first {
			return nil
		}
		close(c.arrived)
		if hold {
			select {
			case <-c.out:
			case <-ctx.Done():
			}
		}
		return nil
	})
	h.t.Cleanup(c.release)
	return c
}

func (c *calls) release() { c.once.Do(func() { close(c.out) }) }

func (c *calls) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

// hibernateRequests are the HibernatePodRequests among the calls.
func (c *calls) hibernateRequests() []*podv1.HibernatePodRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*podv1.HibernatePodRequest
	for _, r := range c.reqs {
		if hr, ok := r.(*podv1.HibernatePodRequest); ok {
			out = append(out, hr)
		}
	}
	return out
}

// pausedLong makes a running sandbox on node, paused an hour ago: a
// hibernate-after's recheck passes. The syncs of the create's and the
// pause's pod events have run: none comes after it.
func (h *harness) pausedLong(node string) store.Sandbox {
	h.t.Helper()
	sb := h.running(512)
	if sb.Node != node {
		h.t.Fatalf("placed on %s, not %s", sb.Node, node)
	}
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: node})
	h.settle(sb.ID)
	h.exec("UPDATE sandboxes SET paused_at = now() - interval '1 hour' WHERE id = $1", sb.ID)
	return h.row(sb.ID)
}

// hibernatingAtUpload makes a paused sandbox on node and begins its
// hibernate on the first instance, as hibernate-after does; the node holds
// the hibernation at its upload, where a resume abandons it (upload
// releases it).
func (h *harness) hibernatingAtUpload(node string) (sb store.Sandbox, hd *OpHandle, upload *calls) {
	h.t.Helper()
	sb = h.pausedLong(node)
	upload = h.watchCalls(node, testnode.HibernateUpload, true)
	hd, err := h.begin(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerHibernateAfter, Target: node})
	if err != nil {
		h.t.Fatal(err)
	}
	waitClosed(h.t, upload.arrived, "the hibernation's upload")
	return h.row(sb.ID), hd, upload
}

// hibernation is the hibernation in flight that node reports of the pod;
// nil without one.
func (h *harness) hibernation(node string, id podid.ID) *podv1.Hibernation {
	h.t.Helper()
	info, err := h.pods(node).GetPod(bg, &podv1.GetPodRequest{PodId: id.String()})
	if err != nil {
		h.t.Fatal(err)
	}
	return info.GetHibernation()
}

// wantHibernation checks the hibernation in flight that node reports of
// the pod.
func (h *harness) wantHibernation(node string, id podid.ID, phase podv1.HibernationPhase, abandonable bool, requestIDs ...string) {
	h.t.Helper()
	hb := h.hibernation(node, id)
	if hb == nil || hb.GetPhase() != phase || hb.GetAbandonable() != abandonable {
		h.t.Fatalf("hibernation %v, want phase %s, abandonable %v", hb, phase, abandonable)
	}
	for _, r := range requestIDs {
		if !slices.Contains(hb.GetRequestIds(), r) {
			h.t.Fatalf("hibernation %v lacks the request id %s", hb, r)
		}
	}
}

// runOrigin waits for the start of the sandbox's open run in the activity
// log and returns its origin.
func (h *harness) runOrigin(id podid.ID) string {
	h.t.Helper()
	run := h.row(id).RunID
	if run == "" {
		h.t.Fatalf("sandbox %s has no open run", id)
	}
	var origin string
	eventually(h.t, 10*time.Second, "the run's start recorded", func() bool {
		err := h.d.Pool.QueryRow(bg, `SELECT detail->>'origin' FROM activity WHERE kind = 'run_start' AND span_id = $1`,
			run).Scan(&origin)
		return err == nil
	})
	return origin
}

// placements waits until the sandbox's activity has want rows of kind
// placement written by instance ("": by any), the audit of its placements.
func (h *harness) placements(id podid.ID, instance string, want int) {
	h.t.Helper()
	got := -1
	eventually(h.t, 10*time.Second, fmt.Sprintf("%d placements of sandbox %s", want, id), func() bool {
		h.scalar(&got, `SELECT count(*) FROM activity WHERE sandbox_id = $1 AND kind = 'placement'
			AND ($2 = '' OR instance_id = $2)`, id, instance)
		return got == want
	})
}

// activityNamed waits until the sandbox's activity has a row of kind named
// name, and returns its detail.
func (h *harness) activityNamed(id podid.ID, kind, name string) map[string]any {
	h.t.Helper()
	var detail map[string]any
	eventually(h.t, 10*time.Second, fmt.Sprintf("activity %s %q", kind, name), func() bool {
		err := h.d.Pool.QueryRow(bg, `SELECT detail FROM activity WHERE sandbox_id = $1 AND kind = $2 AND name = $3`,
			id, kind, name).Scan(&detail)
		return err == nil
	})
	return detail
}
