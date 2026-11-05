// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

var bg = context.Background()

const testTD = "feos.test"

// issuerDir holds one platform CA for every test of the package, made by
// TestMain.
var issuerDir string

func TestMain(m *testing.M) {
	base, err := os.MkdirTemp("", "fleet-test-ca-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	issuerDir = filepath.Join(base, "ca")
	err = pki.InitCA(pki.InitCAOptions{
		RootDir: filepath.Join(base, "root"), Dir: issuerDir, TrustDomain: testTD, DNSDomains: []string{testTD},
	})
	code := 1
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	} else {
		code = m.Run()
	}
	os.RemoveAll(base)
	os.Exit(code)
}

// testIssuer loads the package's platform CA.
func testIssuer(t *testing.T) *pki.Issuer {
	t.Helper()
	iss, err := pki.LoadIssuer(issuerDir, testTD)
	if err != nil {
		t.Fatal(err)
	}
	return iss
}

// fakeLinks is the nodes' port as fleet sees it: which nodes are
// connected, and the re-checks and rollout deadlines fleet asked for.
type fakeLinks struct {
	mu         sync.Mutex
	connected  map[string]bool
	conns      map[string]grpc.ClientConnInterface // a node's connection; none: UNAVAILABLE
	rechecks   []string
	recheckAll int
	stopping   map[string]time.Time
	stopCalls  int
}

func newFakeLinks() *fakeLinks {
	return &fakeLinks{connected: map[string]bool{}, conns: map[string]grpc.ClientConnInterface{},
		stopping: map[string]time.Time{}}
}

// setConn connects the node with conn as its connection.
func (f *fakeLinks) setConn(node string, conn grpc.ClientConnInterface) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected[node] = true
	f.conns[node] = conn
}

func (f *fakeLinks) setConnected(node string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected[node] = on
}

func (f *fakeLinks) Conn(_ context.Context, node string) (grpc.ClientConnInterface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.conns[node]; c != nil {
		return c, nil
	}
	return nil, status.Error(codes.Unavailable, "no connections in this test")
}
func (f *fakeLinks) WaitConnected(context.Context, string) error {
	return status.Error(codes.Unavailable, "no connections in this test")
}
func (f *fakeLinks) Connected(node string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected[node]
}
func (f *fakeLinks) Connections(node string) int {
	if f.Connected(node) {
		return 1
	}
	return 0
}
func (f *fakeLinks) Nodes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n, on := range f.connected {
		if on {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
func (f *fakeLinks) Subscribe() (<-chan nodelink.ConnEvent, func()) {
	return make(chan nodelink.ConnEvent), func() {}
}
func (f *fakeLinks) Recheck(_ context.Context, node string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rechecks = append(f.rechecks, node)
}
func (f *fakeLinks) RecheckAll(context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recheckAll++
}
func (f *fakeLinks) SetStoppingUntil(node string, until time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopping[node] = until
	f.stopCalls++
}

func (f *fakeLinks) snapshot() (rechecks []string, all int, stopping map[string]time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stopping = map[string]time.Time{}
	for k, v := range f.stopping {
		stopping[k] = v
	}
	return append([]string(nil), f.rechecks...), f.recheckAll, stopping
}

var _ Links = (*fakeLinks)(nil)

// fakeHooks is the sandbox engine as fleet sees it.
type fakeHooks struct {
	mu         sync.Mutex
	changed    []string // "<node> <pod>"
	hashes     map[string]string
	hashErr    error
	reconciles chan string
	// gate, when set, holds every ReconcileNode that starts after it was
	// set until it is closed or receives. A run reads it before it reports
	// itself on reconciles, so that a run the test has seen already never
	// blocks on a gate set after that.
	gate chan struct{}
}

func newFakeHooks() *fakeHooks {
	return &fakeHooks{hashes: map[string]string{}, reconciles: make(chan string, 100)}
}

func (f *fakeHooks) PodChanged(node string, podID podid.ID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changed = append(f.changed, node+" "+podID.String())
}
func (f *fakeHooks) ReconcileNode(ctx context.Context, node string) error {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	f.reconciles <- node
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (f *fakeHooks) RegistryHash(_ context.Context, node string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hashes[node], f.hashErr
}
func (f *fakeHooks) setHash(node, h string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hashes[node] = h
}
func (f *fakeHooks) changes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.changed...)
}

var _ SandboxHooks = (*fakeHooks)(nil)

// fakeBindings is the bindings service as fleet sees it.
type fakeBindings struct {
	mu      sync.Mutex
	b       *fleetv1.PodBindings
	version string
	err     error
	// calls counts the PodBindings calls.
	calls int
}

func (f *fakeBindings) PodBindings(context.Context, podid.ID) (*fleetv1.PodBindings, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.b, f.version, f.err
}

func (f *fakeBindings) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeBindings) set(b *fleetv1.PodBindings, version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.b, f.version = b, version
}

type harness struct {
	t        *testing.T
	d        *db.DB
	st       *store.Store
	iss      *pki.Issuer
	links    *fakeLinks
	hooks    *fakeHooks
	bindings *fakeBindings
	s        *Service
}

// testSink is the sink's ingest every telemetry answer names.
const testSink = "ferun-sink.feos.test:4317"

// newHarness gives the test a database and a Service of its own: the
// node TTL is 1 s, the drain timeout 5 minutes (the superseded grace 15
// minutes), the rollout grace 30 s.
func newHarness(t *testing.T) *harness {
	t.Helper()
	t.Parallel()
	d := dbtest.New(t)
	h := &harness{t: t, d: d, st: store.New(d), iss: testIssuer(t), links: newFakeLinks(), hooks: newFakeHooks(), bindings: &fakeBindings{}}
	h.s = New(Config{
		InstanceID: "i1", TrustDomain: testTD, NodeTTL: time.Second, ConnDrainTimeout: 5 * time.Minute,
		RolloutGrace: 30 * time.Second, SinkEndpoint: testSink, Log: dbtest.Logger(t),
	}, Deps{DB: d, Issuer: h.iss, Links: h.links, Hooks: h.hooks, Bindings: h.bindings})
	t.Cleanup(h.s.stop)
	return h
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

func (h *harness) node(name string) store.Node {
	h.t.Helper()
	n, err := h.st.GetNode(bg, name)
	if err != nil {
		h.t.Fatalf("node %s: %v", name, err)
	}
	return n
}

// newCSR returns a signing request for a new Ed25519 key.
func newCSR(t *testing.T) []byte {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.CreateCSR(key, "ignored.example.com")
	if err != nil {
		t.Fatal(err)
	}
	return csr
}

// token issues a join token, pinned to node unless "", and returns what
// the node sends.
func (h *harness) token(node string) string {
	h.t.Helper()
	_, token, _, err := h.s.IssueJoinToken(bg, "test", node, time.Hour)
	if err != nil {
		h.t.Fatal(err)
	}
	sent, pin, err := pki.SplitJoinToken(token)
	if err != nil {
		h.t.Fatal(err)
	}
	if pin != h.iss.JoinPin() {
		h.t.Fatalf("token pin %s, issuer pin %s", pin, h.iss.JoinPin())
	}
	return sent
}

const joinPeer = "192.0.2.7:40000"

func joinCtx() context.Context {
	addr, _ := net.ResolveTCPAddr("tcp", joinPeer)
	return peer.NewContext(bg, &peer.Peer{Addr: addr})
}

func (h *harness) join(name, sent string, csr []byte) (*fleetv1.JoinResponse, error) {
	return h.s.JoinServer().Join(joinCtx(), &fleetv1.JoinRequest{Token: sent, Name: name, Csr: csr})
}

// joined joins a node with an unpinned token, presents its certificate
// and returns its serial.
func (h *harness) joined(name string) string {
	h.t.Helper()
	resp, err := h.join(name, h.token(""), newCSR(h.t))
	if err != nil {
		h.t.Fatalf("join %s: %v", name, err)
	}
	serial := serialOf(h.t, resp.GetCertificate())
	h.s.OnPresented(name, serial, "198.51.100.1:5000")
	return serial
}

func serialOf(t *testing.T, c *fleetv1.Certificate) string {
	t.Helper()
	if c == nil || len(c.GetChain()) == 0 {
		t.Fatal("no certificate")
	}
	leaf, err := x509.ParseCertificate(c.GetChain()[0])
	if err != nil {
		t.Fatal(err)
	}
	return pki.Serial(leaf)
}

func uplinkCtx(node, serial string) context.Context {
	return nodelink.ContextWithPeer(bg, nodelink.Peer{Node: node, Serial: serial, Addr: "198.51.100.1:5000"})
}

func wantCode(t *testing.T, err error, code codes.Code, text string) {
	t.Helper()
	st, _ := status.FromError(err)
	if st.Code() != code || (text != "" && !strings.Contains(st.Message(), text)) {
		t.Fatalf("got %v; want %v containing %q", err, code, text)
	}
}

func statusOf(err error) (codes.Code, string) {
	st, _ := status.FromError(err)
	return st.Code(), st.Message()
}

func waitFor(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", within, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// expectReconcile waits for an inventory reconcile of node.
func (h *harness) expectReconcile(node string) {
	h.t.Helper()
	select {
	case got := <-h.hooks.reconciles:
		if got != node {
			h.t.Fatalf("reconcile of %s; want %s", got, node)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatalf("no reconcile of %s", node)
	}
}

// expectNoReconcile checks that no reconcile runs within a while.
func (h *harness) expectNoReconcile() {
	h.t.Helper()
	select {
	case got := <-h.hooks.reconciles:
		h.t.Fatalf("unexpected reconcile of %s", got)
	case <-time.After(300 * time.Millisecond):
	}
}

// allowReconcile clears the cross-instance spacing of a node's reconciles.
func (h *harness) allowReconcile(node string) {
	h.exec("UPDATE nodes SET reconcile_requested_at = NULL WHERE name = $1", node)
}

func expectPayload(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case p := <-ch:
			if p == want {
				return
			}
		case <-deadline:
			t.Fatalf("no notification %q", want)
		}
	}
}

func expectNoPayload(t *testing.T, ch <-chan string, unwanted string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case p := <-ch:
			if p == unwanted {
				t.Fatalf("unexpected notification %q", p)
			}
		case <-deadline:
			return
		}
	}
}

// fakeStream is the server side of a PublishEvents stream.
type fakeStream struct {
	grpc.ServerStream
	ctx  context.Context
	in   chan *fleetv1.EventBatch
	acks chan *fleetv1.EventAck
	// breakOnSend, when set, makes Send fail as on a stream that broke:
	// it is called (to end ctx), and Send returns sendErr.
	breakOnSend func()
	sendErr     error
}

func (f *fakeStream) Context() context.Context { return f.ctx }

func (f *fakeStream) Recv() (*fleetv1.EventBatch, error) {
	select {
	case b, ok := <-f.in:
		if !ok {
			return nil, io.EOF
		}
		return b, nil
	case <-f.ctx.Done():
		return nil, status.FromContextError(f.ctx.Err()).Err()
	}
}

func (f *fakeStream) Send(a *fleetv1.EventAck) error {
	if f.breakOnSend != nil {
		f.breakOnSend()
		return f.sendErr
	}
	f.acks <- a
	return nil
}

// publish sends batches on one stream from node and returns the acks and
// the stream's end.
func (h *harness) publish(node, serial string, batches ...*fleetv1.EventBatch) ([]*fleetv1.EventAck, error) {
	h.t.Helper()
	fs := &fakeStream{ctx: uplinkCtx(node, serial), in: make(chan *fleetv1.EventBatch, len(batches)),
		acks: make(chan *fleetv1.EventAck, len(batches))}
	for _, b := range batches {
		fs.in <- b
	}
	close(fs.in)
	done := make(chan error, 1)
	go func() { done <- h.s.UplinkServer().PublishEvents(fs) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		h.t.Fatal("PublishEvents did not return")
	}
	close(fs.acks)
	var acks []*fleetv1.EventAck
	for a := range fs.acks {
		acks = append(acks, a)
	}
	return acks, err
}
