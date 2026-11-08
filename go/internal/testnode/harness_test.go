// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
)

const (
	testTD     = "feos.test"
	testDomain = "example.test"
	serverName = "ferun." + testDomain
)

// caIss is the platform CA of the tests; otherIss another CA with the same
// trust domain and domains, which a node's pin must tell apart.
var caIss, otherIss *pki.Issuer

func TestMain(m *testing.M) {
	base, err := os.MkdirTemp("", "testnode-ca-")
	if err == nil {
		caIss, err = makeCA(base, "a")
	}
	if err == nil {
		otherIss, err = makeCA(base, "b")
	}
	if err != nil {
		os.RemoveAll(base)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(base)
	os.Exit(code)
}

func makeCA(base, name string) (*pki.Issuer, error) {
	dir := filepath.Join(base, name, "ca")
	if err := pki.InitCA(pki.InitCAOptions{RootDir: filepath.Join(base, name, "root"), Dir: dir, TrustDomain: testTD, DNSDomains: []string{testDomain}}); err != nil {
		return nil, err
	}
	return pki.LoadIssuer(dir, testTD)
}

// fleet is the stub control plane behind every instance of a test: what
// ferund keeps in its database, in memory.
type fleet struct {
	iss *pki.Issuer
	ttl time.Duration

	mu         sync.Mutex
	instances  []*fleetv1.ControlPlaneInstance
	interval   uint32
	joins      []*fleetv1.JoinRequest
	joinErrs   []error           // answers of the next Joins, in turn
	joinBundle [][]byte          // a trust bundle Join answers with instead of the CA's
	refused    map[string]string // serial → the refusal's text
	checks     []check
	reports    []report
	renewals   int
	lastSeq    map[string]uint64
	dropped    map[string]uint64
	stored     []stored
	batches    []batch
	failStream int // the next batches with events end their stream with UNAVAILABLE, unacknowledged
	bindings   *fleetv1.PodBindings
	// onAccept, when set, is told of every check at accept, outside mu.
	onAccept func(check)
	// renewGate and credsGate, when set, hold RenewCertificate and
	// IssuePodCredentials until they are closed; each call is announced
	// on renewEntered or credsEntered first, without blocking.
	renewGate, renewEntered chan struct{}
	credsGate, credsEntered chan struct{}
}

type check struct {
	instance, node, serial string
	atAccept               bool
}

type report struct {
	instance, node, serial string
	t                      *fleetv1.Telemetry
}

type stored struct {
	start string
	ev    *fleetv1.Event
}

type batch struct {
	start   string
	events  int
	dropped uint64
	size    int
	first   uint64
}

func newFleet(iss *pki.Issuer) *fleet {
	return &fleet{iss: iss, ttl: time.Hour, interval: 1, refused: map[string]string{}, lastSeq: map[string]uint64{}, dropped: map[string]uint64{}}
}

// checker is the CheckNode of an instance.
func (f *fleet) checker(instance string) func(context.Context, string, string, string, bool) (time.Time, error) {
	return func(_ context.Context, node, serial, _ string, atAccept bool) (time.Time, error) {
		c := check{instance, node, serial, atAccept}
		f.mu.Lock()
		f.checks = append(f.checks, c)
		text, refused := f.refused[serial]
		onAccept := f.onAccept
		f.mu.Unlock()
		if atAccept && onAccept != nil {
			onAccept(c)
		}
		if refused {
			return time.Time{}, nodelink.Refusal("%s", text)
		}
		return time.Time{}, nil
	}
}

// gate announces a call on entered and holds it until gate is closed or
// the call ends.
func gate(ctx context.Context, f *fleet, gate, entered *chan struct{}) error {
	f.mu.Lock()
	g, e := *gate, *entered
	f.mu.Unlock()
	if g == nil {
		return nil
	}
	select {
	case e <- struct{}{}:
	default:
	}
	select {
	case <-g:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

func (f *fleet) refuse(serial, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refused[serial] = text
}

func (f *fleet) setInstances(list ...*fleetv1.ControlPlaneInstance) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances = list
}

func (f *fleet) acceptChecks(serial string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.checks {
		if c.serial == serial && c.atAccept {
			n++
		}
	}
	return n
}

// acceptsAt counts the connections with serial that instance accepted.
func (f *fleet) acceptsAt(instance, serial string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.checks {
		if c.instance == instance && c.serial == serial && c.atAccept {
			n++
		}
	}
	return n
}

func (f *fleet) joinCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.joins)
}

func (f *fleet) reportsFrom(serial string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reports {
		if r.serial == serial {
			n++
		}
	}
	return n
}

func (f *fleet) lastReport() (report, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reports) == 0 {
		return report{}, false
	}
	return f.reports[len(f.reports)-1], true
}

func (f *fleet) storedEvents() []stored {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]stored(nil), f.stored...)
}

func (f *fleet) batchesSeen() []batch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]batch(nil), f.batches...)
}

// joinStub is the join server's FleetService: Join only.
type joinStub struct {
	fleetv1.UnimplementedFleetServiceServer
	f *fleet
}

func (j *joinStub) Join(_ context.Context, req *fleetv1.JoinRequest) (*fleetv1.JoinResponse, error) {
	f := j.f
	f.mu.Lock()
	f.joins = append(f.joins, req)
	var answer error
	if len(f.joinErrs) > 0 {
		answer, f.joinErrs = f.joinErrs[0], f.joinErrs[1:]
	}
	bundle := f.joinBundle
	list := append([]*fleetv1.ControlPlaneInstance(nil), f.instances...)
	f.mu.Unlock()
	if answer != nil {
		return nil, answer
	}
	if !strings.HasPrefix(req.Token, pki.JoinTokenPrefix) || len(req.Token) != pki.JoinSecretLen {
		return nil, status.Error(codes.InvalidArgument, "malformed join token")
	}
	cert, _, err := f.iss.IssueNode(req.Csr, req.Name, f.ttl)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if bundle != nil {
		cert.TrustBundle = bundle
	}
	return &fleetv1.JoinResponse{Name: req.Name, Certificate: cert, Instances: list}, nil
}

// uplinkStub is the uplink server's FleetService of one instance.
type uplinkStub struct {
	fleetv1.UnimplementedFleetServiceServer
	f        *fleet
	instance string
}

func (u *uplinkStub) ReportTelemetry(ctx context.Context, t *fleetv1.Telemetry) (*fleetv1.TelemetryResponse, error) {
	p, ok := nodelink.NodeFromContext(ctx)
	if !ok {
		return nil, errors.New("no peer")
	}
	f := u.f
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, report{instance: u.instance, node: p.Node, serial: p.Serial, t: t})
	return &fleetv1.TelemetryResponse{IntervalSeconds: f.interval, Instances: append([]*fleetv1.ControlPlaneInstance(nil), f.instances...)}, nil
}

func (u *uplinkStub) RenewCertificate(ctx context.Context, req *fleetv1.RenewCertificateRequest) (*fleetv1.RenewCertificateResponse, error) {
	p, ok := nodelink.NodeFromContext(ctx)
	if !ok {
		return nil, errors.New("no peer")
	}
	f := u.f
	if err := gate(ctx, f, &f.renewGate, &f.renewEntered); err != nil {
		return nil, err
	}
	cert, _, err := f.iss.IssueNode(req.Csr, p.Node, f.ttl)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	f.mu.Lock()
	f.renewals++
	f.mu.Unlock()
	return &fleetv1.RenewCertificateResponse{Certificate: cert}, nil
}

// PublishEvents stores what it has not seen, as fleet does: numbers at or
// below the start's last are dropped, the acknowledgement names the last.
func (u *uplinkStub) PublishEvents(stream grpc.BidiStreamingServer[fleetv1.EventBatch, fleetv1.EventAck]) error {
	f := u.f
	for {
		b, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := uuid.Parse(b.StartId); err != nil {
			return status.Error(codes.InvalidArgument, "start ID is no UUID")
		}
		for i, ev := range b.Events {
			if ev.Seq == 0 || i > 0 && ev.Seq <= b.Events[i-1].Seq {
				return status.Error(codes.InvalidArgument, "numbers not ascending")
			}
		}
		if len(b.Events) > maxBatchEvents || proto.Size(b) > maxBatchBytes {
			return status.Error(codes.InvalidArgument, "batch too large")
		}
		f.mu.Lock()
		if len(b.Events) > 0 && f.failStream > 0 {
			f.failStream--
			f.mu.Unlock()
			return status.Error(codes.Unavailable, "storing the events failed")
		}
		rec := batch{start: b.StartId, events: len(b.Events), dropped: b.Dropped, size: proto.Size(b)}
		if len(b.Events) > 0 {
			rec.first = b.Events[0].Seq
		}
		f.batches = append(f.batches, rec)
		last := f.lastSeq[b.StartId]
		for _, ev := range b.Events {
			if ev.Seq <= last {
				continue
			}
			f.stored = append(f.stored, stored{start: b.StartId, ev: ev})
			last = ev.Seq
		}
		f.lastSeq[b.StartId] = last
		f.dropped[b.StartId] = max(f.dropped[b.StartId], b.Dropped)
		f.mu.Unlock()
		if err := stream.Send(&fleetv1.EventAck{StartId: b.StartId, Seq: last}); err != nil {
			return err
		}
	}
}

func (u *uplinkStub) IssuePodCredentials(ctx context.Context, req *fleetv1.IssuePodCredentialsRequest) (*fleetv1.IssuePodCredentialsResponse, error) {
	if _, ok := nodelink.NodeFromContext(ctx); !ok {
		return nil, errors.New("no peer")
	}
	pod, err := podid.Parse(req.PodId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	f := u.f
	if err := gate(ctx, f, &f.credsGate, &f.credsEntered); err != nil {
		return nil, err
	}
	cert, _, err := f.iss.IssuePod(req.Csr, pki.SandboxID(testTD, "t1", pod), pki.PodSubject(testTD, pod.String(), ""), nil, time.Hour)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	resp := &fleetv1.IssuePodCredentialsResponse{Certificate: cert}
	f.mu.Lock()
	b := f.bindings
	f.mu.Unlock()
	if b != nil {
		if resp.Bindings, err = pki.Seal(req.HpkePublicKey, pod, b, "v7"); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	return resp, nil
}

// instance is one control-plane instance's nodes' port.
type instance struct {
	id     string
	srv    *nodelink.Server
	cancel context.CancelFunc
	served chan error
	once   sync.Once
}

func (i *instance) addr() string { return i.srv.Addr().String() }

func (i *instance) listing() *fleetv1.ControlPlaneInstance {
	return &fleetv1.ControlPlaneInstance{Id: i.id, NodeAddress: i.addr()}
}

// shutdown closes the instance's port.
func (i *instance) shutdown(t *testing.T) {
	i.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		i.srv.Shutdown(ctx)
		i.cancel()
		select {
		case <-i.served:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return")
		}
	})
}

// env is a test's control plane: instances on one stub fleet.
type env struct {
	t   *testing.T
	iss *pki.Issuer
	f   *fleet
}

func newEnv(t *testing.T) *env {
	return &env{t: t, iss: caIss, f: newFleet(caIss)}
}

// instance starts an instance whose nodes' port shows a certificate of iss.
func (e *env) instance(id string, iss *pki.Issuer) *instance {
	e.t.Helper()
	src, err := pki.NewServerCertSource(iss, []string{serverName}, time.Hour, slog.New(slog.DiscardHandler))
	if err != nil {
		e.t.Fatal(err)
	}
	srv, err := nodelink.New(nodelink.Config{
		Addr:             "127.0.0.1:0",
		TrustDomain:      testTD,
		InstanceID:       id,
		Bundle:           iss.Bundle(),
		ServerCert:       src.GetCertificate,
		NodeWait:         2 * time.Second,
		ConnDrainTimeout: time.Minute,
		MaxStreams:       1024,
		CheckNode:        e.f.checker(id),
		Log:              slog.New(slog.DiscardHandler),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	fleetv1.RegisterFleetServiceServer(srv.Uplink(), &uplinkStub{f: e.f, instance: id})
	fleetv1.RegisterFleetServiceServer(srv.Join(), &joinStub{f: e.f})
	ctx, cancel := context.WithCancel(context.Background())
	in := &instance{id: id, srv: srv, cancel: cancel, served: make(chan error, 1)}
	go func() { in.served <- srv.Serve(ctx) }()
	e.t.Cleanup(func() { in.shutdown(e.t) })
	return in
}

// token makes a join token pinned to the test CA's root.
func (e *env) token() string {
	e.t.Helper()
	token, _, _, err := pki.NewJoinToken(caIss.JoinPin())
	if err != nil {
		e.t.Fatal(err)
	}
	return token
}

// config is a node's configuration against the instances.
func (e *env) config(name string, ins ...*instance) Config {
	addrs := make([]string, len(ins))
	for i, in := range ins {
		addrs[i] = in.addr()
	}
	return Config{
		Name:         name,
		JoinToken:    e.token(),
		ServerName:   serverName,
		TrustDomain:  testTD,
		Addrs:        addrs,
		PendingDelay: 50 * time.Millisecond,
		Log:          slog.New(slog.DiscardHandler),
	}
}

// newNode makes a node with the tests' short timings.
func (e *env) newNode(cfg Config) *Node {
	e.t.Helper()
	n, err := New(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	n.backoffMin = 50 * time.Millisecond
	n.backoffMax = 500 * time.Millisecond
	n.defaultInterval = time.Second
	n.gracefulTimeout = 3 * time.Second
	return n
}

// running is a node whose Run runs until the test ends.
type running struct {
	n      *Node
	cancel context.CancelFunc
	done   chan error
}

func (e *env) run(n *Node) *running {
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{n: n, cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- n.Run(ctx) }()
	e.t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(20 * time.Second):
			e.t.Error("Run did not return")
		}
	})
	return r
}

// wait waits for Run's result.
func (r *running) wait(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case err := <-r.done:
		r.done <- err
		return err
	case <-time.After(within):
		t.Fatalf("Run did not return within %v", within)
		return nil
	}
}

// start runs a node named name against in and waits until in has its
// node connection.
func (e *env) start(name string, in *instance, mod ...func(*Config)) *Node {
	e.t.Helper()
	return e.startRun(name, in, mod...).n
}

func (e *env) startRun(name string, in *instance, mod ...func(*Config)) *running {
	e.t.Helper()
	cfg := e.config(name, in)
	for _, m := range mod {
		m(&cfg)
	}
	r := e.run(e.newNode(cfg))
	eventually(e.t, 10*time.Second, "node "+name+" connected", func() bool { return in.srv.Connected(name) })
	return r
}

// newKeyCSR makes a node key and its signing request.
func newKeyCSR(t *testing.T, name string) (ed25519.PrivateKey, []byte) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.CreateCSR(key, name)
	if err != nil {
		t.Fatal(err)
	}
	return key, csr
}

// pods is a PodService client on the node's connection to in.
func pods(t *testing.T, in *instance, node string) podv1.PodServiceClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cc, err := in.srv.Conn(ctx, node)
	if err != nil {
		t.Fatal(err)
	}
	return podv1.NewPodServiceClient(cc)
}

func conn(t *testing.T, in *instance, node string) grpc.ClientConnInterface {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cc, err := in.srv.Conn(ctx, node)
	if err != nil {
		t.Fatal(err)
	}
	return cc
}

// rawConns counts the TCP connections the node's session holds to addr:
// its node connections and uplinks.
func rawConns(n *Node, addr string) int {
	n.lifeMu.Lock()
	s := n.sess
	n.lifeMu.Unlock()
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for c := range s.raw {
		if c.RemoteAddr().String() == addr {
			count++
		}
	}
	return count
}

// eventually polls cond until it holds or within passes.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", within, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// never checks that cond stays false for d.
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

// wantCode checks a call's gRPC code and message.
func wantCode(t *testing.T, err error, code codes.Code, msg string) {
	t.Helper()
	st := status.Convert(err)
	if err == nil || st.Code() != code || (msg != "" && st.Message() != msg) {
		t.Fatalf("got %v, want %s %q", err, code, msg)
	}
}

// wantPrefix checks a call's gRPC code and the beginning of its message.
func wantPrefix(t *testing.T, err error, code codes.Code, prefix string) {
	t.Helper()
	st := status.Convert(err)
	if err == nil || st.Code() != code || !strings.HasPrefix(st.Message(), prefix) {
		t.Fatalf("got %v, want %s %q…", err, code, prefix)
	}
}

// sandboxSpec is the spec ferund gives a sandbox's pod (§9 of the design).
func sandboxSpec(id podid.ID, incarnation string) *podv1.PodSpec {
	return &podv1.PodSpec{
		Resources:  &podv1.Resources{Vcpus: 2, MemoryMib: 1024},
		Containers: []*podv1.ContainerSpec{{Name: "main", ImageRef: "ghcr.io/maltej/debian-ai:latest", Command: []string{"sleep", "infinity"}}},
		Network:    &podv1.PodNetwork{},
		Labels:     map[string]string{"ferun.sandbox": id.String(), "ferun.tenant": "t1", "ferun.incarnation": incarnation},
	}
}

// waitState waits until the pod is in state.
func waitState(t *testing.T, pc podv1.PodServiceClient, id string, state podv1.PodState) *podv1.PodInfo {
	t.Helper()
	var info *podv1.PodInfo
	eventually(t, 10*time.Second, fmt.Sprintf("pod %s %s", id, state), func() bool {
		var err error
		info, err = pc.GetPod(context.Background(), &podv1.GetPodRequest{PodId: id})
		return err == nil && info.State == state
	})
	return info
}

// createRunning creates a sandbox pod and waits until it runs.
func createRunning(t *testing.T, pc podv1.PodServiceClient) (podid.ID, *podv1.PodInfo) {
	t.Helper()
	id := podid.Mint()
	if _, err := pc.CreatePod(context.Background(), &podv1.CreatePodRequest{PodId: id.String(), Spec: sandboxSpec(id, uuid.NewString())}); err != nil {
		t.Fatal(err)
	}
	return id, waitState(t, pc, id.String(), podv1.PodState_POD_STATE_RUNNING)
}
