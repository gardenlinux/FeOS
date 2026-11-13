// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package toolstest gives the tests of the tool doors (tools, mcpsrv) an
// instance's world to call into: a fresh database, a CA, the nodes' port
// with fleet behind it, a test node that joined over the wire and keeps its
// node connection, and a fake sandbox engine that hands out leases on pods
// of that node, so that every call travels the real path: nodelink, the
// node's forward layer, the guest's services.
package toolstest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/fleet"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// The names of the environment.
const (
	TrustDomain = "feos.test"
	Domain      = "example.test"
	InstanceID  = "i1"
	NodeName    = "n1"
	Tenant      = "acme"
)

// Options shape an environment.
type Options struct {
	// MaxStreams is the nodes' port's --node-max-streams, from which the
	// stream and tool budgets derive; 1024 when zero.
	MaxStreams int
	// PendingDelay is how long the node's pods take to start.
	PendingDelay time.Duration
	// FakeProcesses is the node's testnode.Config.FakeProcesses.
	FakeProcesses bool
}

// Env is a test's world.
type Env struct {
	t        testing.TB
	DB       *db.DB
	Store    *store.Store
	Port     *nodelink.Server
	Node     *testnode.Node
	Engine   *Engine
	Recorder *activity.Recorder
	Log      *slog.Logger
}

// New builds an environment, or skips the test without a database. It is
// torn down when the test ends.
func New(t testing.TB, o Options) *Env {
	t.Helper()
	d := dbtest.New(t)
	log := dbtest.Logger(t)
	st := store.New(d)
	e := &Env{t: t, DB: d, Store: st, Log: log}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	goRun := func(f func(ctx context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f(ctx)
		}()
	}

	base := t.TempDir()
	caDir := filepath.Join(base, "ca")
	if err := pki.InitCA(pki.InitCAOptions{RootDir: filepath.Join(base, "root"), Dir: caDir,
		TrustDomain: TrustDomain, DNSDomains: []string{Domain}}); err != nil {
		t.Fatal(err)
	}
	iss, err := pki.LoadIssuer(caDir, TrustDomain)
	if err != nil {
		t.Fatal(err)
	}
	serverName := iss.ControlPlaneDomain()
	certs, err := pki.NewServerCertSource(iss, []string{serverName}, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}

	e.Recorder = activity.NewRecorder(st, InstanceID, log)
	goRun(func(ctx context.Context) { _ = e.Recorder.Run(ctx) })

	var fl *fleet.Service
	port, err := nodelink.New(nodelink.Config{
		Addr: "127.0.0.1:0", TrustDomain: TrustDomain, InstanceID: InstanceID, Bundle: iss.Bundle(),
		ServerCert: certs.GetCertificate, NodeWait: 2 * time.Second, ConnDrainTimeout: time.Minute,
		MaxStreams: o.MaxStreams,
		CheckNode: func(ctx context.Context, node, serial, peer string, atAccept bool) (time.Time, error) {
			return fl.CheckNode(ctx, node, serial, peer, atAccept)
		},
		OnPresented: func(node, serial, peer string) { fl.OnPresented(node, serial, peer) },
		Log:         log,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.Port = port
	fl = fleet.New(fleet.Config{InstanceID: InstanceID, TrustDomain: TrustDomain, Log: log},
		fleet.Deps{DB: d, Store: st, Issuer: iss, Links: port, Hooks: hooks{}, Activity: e.Recorder})
	fleetv1.RegisterFleetServiceServer(port.Uplink(), fl.UplinkServer())
	fleetv1.RegisterFleetServiceServer(port.Join(), fl.JoinServer())
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = port.Serve(ctx)
	}()
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		port.Shutdown(sctx)
		<-served
	})

	_, token, _, err := fl.IssueJoinToken(ctx, "test", NodeName, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	node, err := testnode.New(testnode.Config{
		Name: NodeName, JoinToken: token, ServerName: serverName, TrustDomain: TrustDomain,
		Addrs: []string{port.Addr().String()}, PendingDelay: o.PendingDelay, Log: log, FakeProcesses: o.FakeProcesses,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.Node = node
	// The node goes first at the end, so that it does not redial a port
	// that is shutting down.
	nodeCtx, stopNode := context.WithCancel(ctx)
	nodeDone := make(chan struct{})
	go func() {
		defer close(nodeDone)
		if err := node.Run(nodeCtx); err != nil {
			log.Error("test node ended", "err", err)
		}
	}()
	t.Cleanup(func() {
		stopNode()
		<-nodeDone
	})
	deadline := time.Now().Add(30 * time.Second)
	for !port.Connected(NodeName) {
		if time.Now().After(deadline) {
			t.Fatal("the test node did not connect within 30 s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, _, err := st.CreateTenant(ctx, Tenant); err != nil {
		t.Fatal(err)
	}
	e.Engine = newEngine(e)
	return e
}

// hooks are fleet's calls into the engine, which this environment has
// none of.
type hooks struct{}

func (hooks) PodChanged(string, podid.ID)                          {}
func (hooks) ReconcileNode(context.Context, string) error          { return nil }
func (hooks) RegistryHash(context.Context, string) (string, error) { return "", nil }

// Sandbox is a request's sandbox that no database knows: for tests that
// need none.
func Sandbox() auth.Sandbox {
	id := podid.Mint()
	return auth.Sandbox{ID: id, Owner: Tenant, TokenHash: store.HashToken(id.String())}
}

// NewSandbox records a sandbox of the tenant in the database and returns
// how a request with its token names it.
func (e *Env) NewSandbox() auth.Sandbox {
	e.t.Helper()
	id := podid.Mint()
	if _, _, err := e.Store.CreateSandbox(context.Background(), store.NewSandbox{
		ID: id, Owner: Tenant, Image: "test", VCPUs: 1, MemoryMiB: 512,
		EnvironmentID: dbtest.Environment(e.t, e.Store.Q(), Tenant, "box"),
	}); err != nil {
		e.t.Fatal(err)
	}
	return auth.Sandbox{ID: id, Owner: Tenant, TokenHash: store.HashToken(id.String())}
}

// Pods is the node's PodService over the instance's node connection.
func (e *Env) Pods(ctx context.Context) (podv1.PodServiceClient, error) {
	conn, err := e.Port.Conn(ctx, NodeName)
	if err != nil {
		return nil, err
	}
	return podv1.NewPodServiceClient(conn), nil
}

// PodState is the state of the sandbox's pod on the node.
func (e *Env) PodState(ctx context.Context, id podid.ID) (podv1.PodState, error) {
	pods, err := e.Pods(ctx)
	if err != nil {
		return 0, err
	}
	p, err := pods.GetPod(ctx, &podv1.GetPodRequest{PodId: id.String()})
	if err != nil {
		return 0, err
	}
	return p.GetState(), nil
}

// Activity returns the sandbox's activity rows once at least n are stored,
// waiting up to 10 s for the recorder to write them.
func (e *Env) Activity(id podid.ID, n int) []store.ActivityRow {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, err := e.Store.ListSandboxActivity(context.Background(), id, store.ActivityQuery{Limit: 1000})
		if err != nil {
			e.t.Fatal(err)
		}
		if len(rows) >= n || time.Now().After(deadline) {
			return rows
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Engine is a fake sandbox engine (the tools.Sandboxes of a test): Acquire
// brings the sandbox's pod up on the test node over the node connection
// (create, resume or start, then wait until it runs) and hands out a lease
// on it. Errors and stale leases are scripted; every call is counted.
type Engine struct {
	env *Env

	mu           sync.Mutex
	acquireErrs  map[podid.ID][]error
	stale        map[podid.ID]int
	incarnations map[podid.ID]uuid.UUID
	workdirs     map[podid.ID]string
	holds        map[podid.ID]int
	calls        map[string]map[podid.ID]int
	processes    map[podid.ID]map[string]time.Duration
	released     map[podid.ID][]string
	syncErr      error
}

func newEngine(env *Env) *Engine {
	return &Engine{
		env:          env,
		acquireErrs:  map[podid.ID][]error{},
		stale:        map[podid.ID]int{},
		incarnations: map[podid.ID]uuid.UUID{},
		workdirs:     map[podid.ID]string{},
		holds:        map[podid.ID]int{},
		calls:        map[string]map[podid.ID]int{},
		processes:    map[podid.ID]map[string]time.Duration{},
		released:     map[podid.ID][]string{},
	}
}

func (g *Engine) count(what string, id podid.ID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.calls[what] == nil {
		g.calls[what] = map[podid.ID]int{}
	}
	g.calls[what][id]++
}

// Calls is how often the engine's method what ("Acquire", "Sync",
// "WaitRunning", "Unreachable", "Reachable") was called for the sandbox.
func (g *Engine) Calls(what string, id podid.ID) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[what][id]
}

// FailAcquire makes the next Acquires of the sandbox fail with errs, in
// turn; a nil error lets its Acquire succeed.
func (g *Engine) FailAcquire(id podid.ID, errs ...error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.acquireErrs[id] = append(g.acquireErrs[id], errs...)
}

// Stale makes the next n Acquires of the sandbox hand out a lease without
// looking at its pod: a lease that was true when the engine read it.
func (g *Engine) Stale(id podid.ID, n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stale[id] += n
}

// FailSync makes Sync fail with err; nil lets it succeed.
func (g *Engine) FailSync(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.syncErr = err
}

// Holds is the number of the sandbox's exec holds not yet released.
func (g *Engine) Holds(id podid.ID) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.holds[id]
}

// Processes are the sandbox's process holds and their timeouts.
func (g *Engine) Processes(id podid.ID) map[string]time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]time.Duration{}
	for k, v := range g.processes[id] {
		out[k] = v
	}
	return out
}

// Released are the process ids whose holds ReleaseProcess was asked to
// end, in order.
func (g *Engine) Released(id podid.ID) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.released[id]...)
}

// SetWorkdir makes dir the sandbox's working directory, which its leases
// carry from now on ("": none, the root).
func (g *Engine) SetWorkdir(id podid.ID, dir string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.workdirs[id] = dir
}

func (g *Engine) lease(id podid.ID) model.Lease {
	g.mu.Lock()
	defer g.mu.Unlock()
	inc, ok := g.incarnations[id]
	if !ok {
		inc = uuid.New()
		g.incarnations[id] = inc
	}
	return model.Lease{SandboxID: id, Incarnation: inc, Node: NodeName, Workdir: g.workdirs[id]}
}

func (g *Engine) Acquire(ctx context.Context, id podid.ID, kind model.HoldKind) (model.Lease, func(), error) {
	g.count("Acquire", id)
	g.mu.Lock()
	var scripted error
	if errs := g.acquireErrs[id]; len(errs) > 0 {
		scripted, g.acquireErrs[id] = errs[0], errs[1:]
	}
	stale := g.stale[id] > 0
	if stale {
		g.stale[id]--
	}
	g.mu.Unlock()
	if scripted != nil {
		return model.Lease{}, nil, scripted
	}
	if !stale {
		if err := g.bringUp(ctx, id); err != nil {
			return model.Lease{}, nil, err
		}
	}
	g.mu.Lock()
	g.holds[id]++
	g.mu.Unlock()
	var once sync.Once
	return g.lease(id), func() {
		once.Do(func() {
			g.mu.Lock()
			g.holds[id]--
			g.mu.Unlock()
		})
	}, nil
}

// bringUp makes the sandbox's pod run on the node.
func (g *Engine) bringUp(ctx context.Context, id podid.ID) error {
	pods, err := g.env.Pods(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", model.ErrNodeUnavailable, err)
	}
	p, err := pods.GetPod(ctx, &podv1.GetPodRequest{PodId: id.String()})
	switch {
	case status.Code(err) == codes.NotFound:
		_, err = pods.CreatePod(ctx, &podv1.CreatePodRequest{PodId: id.String(), Spec: &podv1.PodSpec{
			Containers: []*podv1.ContainerSpec{{Name: "main", ImageRef: "test"}},
			Labels:     map[string]string{"ferun.sandbox": id.String(), "ferun.incarnation": g.lease(id).Incarnation.String()},
		}})
	case err != nil:
	case p.GetState() == podv1.PodState_POD_STATE_SUSPENDED:
		_, err = pods.ResumePod(ctx, &podv1.ResumePodRequest{PodId: id.String()})
	case p.GetState() == podv1.PodState_POD_STATE_STOPPED, p.GetState() == podv1.PodState_POD_STATE_FAILED:
		_, err = pods.StartPod(ctx, &podv1.StartPodRequest{PodId: id.String()})
	}
	if err != nil {
		return err
	}
	_, err = g.waitRunning(ctx, id)
	return err
}

func (g *Engine) waitRunning(ctx context.Context, id podid.ID) (model.Lease, error) {
	for {
		state, err := g.env.PodState(ctx, id)
		if err != nil {
			return model.Lease{}, err
		}
		if state == podv1.PodState_POD_STATE_RUNNING {
			return g.lease(id), nil
		}
		select {
		case <-ctx.Done():
			return model.Lease{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (g *Engine) Sync(ctx context.Context, id podid.ID) error {
	g.count("Sync", id)
	g.mu.Lock()
	err := g.syncErr
	g.mu.Unlock()
	return err
}

func (g *Engine) WaitRunning(ctx context.Context, id podid.ID) (model.Lease, error) {
	g.count("WaitRunning", id)
	return g.waitRunning(ctx, id)
}

func (g *Engine) Unreachable(id podid.ID) { g.count("Unreachable", id) }
func (g *Engine) Reachable(id podid.ID)   { g.count("Reachable", id) }

func (g *Engine) HoldProcess(ctx context.Context, id podid.ID, processID string, timeout time.Duration) error {
	if processID == "" {
		return errors.New("no process id")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.processes[id] == nil {
		g.processes[id] = map[string]time.Duration{}
	}
	g.processes[id][processID] = timeout
	return nil
}

func (g *Engine) ReleaseProcess(ctx context.Context, id podid.ID, processID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.processes[id], processID)
	g.released[id] = append(g.released[id], processID)
	return nil
}
