// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/env/envdeps"
	"github.com/gardenlinux/feos/go/internal/fleet"
	"github.com/gardenlinux/feos/go/internal/leader"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/ports"
	"github.com/gardenlinux/feos/go/internal/sandbox"
	"github.com/gardenlinux/feos/go/internal/seal"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
	"github.com/gardenlinux/feos/go/internal/tools"
)

var bg = context.Background()

const (
	testTD     = "feos.test"
	testTenant = "t1"
	// testImage is pinned: no registry is asked.
	testImage = "example.test/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// issuerDir holds the platform CA of every test of the package.
var issuerDir string

func TestMain(m *testing.M) {
	base, err := os.MkdirTemp("", "ctl-test-ca-")
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

// testEngine is the engine with an image resolver that asks no registry.
type testEngine struct{ *sandbox.Engine }

func (testEngine) ResolveImage(_ context.Context, image string) (string, error) {
	return fakeResolve(image), nil
}

// fakeResolve pins an image without a registry: a digest reference as it
// is, a tag to a digest made from its name.
func fakeResolve(image string) string {
	if strings.Contains(image, "@sha256:") {
		return image
	}
	sum := sha256.Sum256([]byte(image))
	return "example.test/" + strings.NewReplacer(":", "-", "/", "-").Replace(image) + "@sha256:" + hex.EncodeToString(sum[:])
}

// harness is one ferund instance with the controller, on a fresh
// database with the tenant t1, and its test nodes.
type harness struct {
	t      *testing.T
	d      *db.DB
	st     *store.Store
	iss    *pki.Issuer
	srv    *nodelink.Server
	fl     *fleet.Service
	eng    *sandbox.Engine
	el     *leader.Elector
	c      *Controller
	runner *tools.Runner
	door   *ports.Handler
	sealer envdeps.Sealer
	objs   *testnode.ObjectStore

	mu    sync.Mutex
	nodes map[string]*testnode.Node
}

// options shape a harness.
type options struct {
	noSealer bool
	// timings change the controller's timings.
	timings func(*timings)
	// engine changes the engine's configuration.
	engine func(*sandbox.Config)
	// ports wraps the port door the controller probes through.
	ports func(envdeps.Ports) envdeps.Ports
}

// testTimings are the controller's timings for the tests.
func testTimings(t *timings) {
	t.retireGrace = 300 * time.Millisecond
	t.restartUnit = 200 * time.Millisecond
	t.restartMax = 2 * time.Second
	t.steadyAfter = 300 * time.Millisecond
	t.noCapacityAfter = time.Second
	t.provisionRetry = 300 * time.Millisecond
	t.attachedReady = 20 * time.Second
	t.restartReady = 30 * time.Second
}

func newHarness(t *testing.T, o options) *harness {
	t.Helper()
	t.Parallel()
	d := dbtest.New(t)
	iss, err := pki.LoadIssuer(issuerDir, testTD)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, d: d, st: store.New(d), iss: iss, objs: testnode.NewObjectStore("ctl"), nodes: map[string]*testnode.Node{}}
	if _, _, err := h.st.CreateTenant(bg, testTenant); err != nil {
		t.Fatal(err)
	}
	const id = "i1"
	log := dbtest.Logger(t)
	src, err := pki.NewServerCertSource(iss, []string{iss.ControlPlaneDomain()}, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	h.srv, err = nodelink.New(nodelink.Config{
		Addr: "127.0.0.1:0", TrustDomain: testTD, InstanceID: id, Bundle: iss.Bundle(),
		ServerCert: src.GetCertificate, NodeWait: 2 * time.Second, ConnDrainTimeout: time.Minute,
		CheckNode: func(ctx context.Context, node, serial, addr string, atAccept bool) (time.Time, error) {
			return h.fl.CheckNode(ctx, node, serial, addr, atAccept)
		},
		OnPresented: func(node, serial, addr string) { h.fl.OnPresented(node, serial, addr) },
		OnNodeConn:  func(node string) { h.fl.OnNodeConn(node) },
		Log:         log,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.el = leader.NewElector(d, id, log)
	cfg := sandbox.Config{InstanceID: id, TrustDomain: testTD, Placement: sandbox.PlacementBinpack, DiskBudget: 1 << 30,
		NodeWait: 2 * time.Second, Log: log}
	if o.engine != nil {
		o.engine(&cfg)
	}
	h.eng, err = sandbox.New(cfg, sandbox.Deps{DB: d, Conns: h.srv, Elector: h.el})
	if err != nil {
		t.Fatal(err)
	}
	// The test image names no registry that answers (ADR 0025).
	h.eng.SetImageAccess(func(context.Context, string) error { return nil })
	h.fl = fleet.New(fleet.Config{InstanceID: id, TrustDomain: testTD, RolloutGrace: 30 * time.Second, NodeTTL: time.Hour,
		ConnDrainTimeout: time.Minute, Log: log}, fleet.Deps{DB: d, Issuer: iss, Links: h.srv, Hooks: h.eng, Elector: h.el})
	fleetv1.RegisterFleetServiceServer(h.srv.Uplink(), h.fl.UplinkServer())
	fleetv1.RegisterFleetServiceServer(h.srv.Join(), h.fl.JoinServer())
	h.runner = tools.NewRunner(h.eng, h.srv, nil, d, id, log)
	h.door = ports.New(h.eng, h.srv, nil, nil, d, log)
	if !o.noSealer {
		key := sha256.Sum256([]byte(t.Name()))
		sl, err := seal.New(key[:])
		if err != nil {
			t.Fatal(err)
		}
		h.sealer = sl
	}
	var door envdeps.Ports = h.door
	if o.ports != nil {
		door = o.ports(h.door)
	}
	h.c, err = New(Deps{InstanceID: id, DB: d, Elector: h.el, Conns: h.srv, Engine: testEngine{h.eng}, Runner: h.runner,
		Ports: door, Sealer: h.sealer, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	testTimings(&h.c.t)
	if o.timings != nil {
		o.timings(&h.c.t)
	}
	h.eng.SetSecretResolver(h.c.ResolveSecret)
	h.eng.SetPrepareHook(h.c.PrepareHook)
	h.el.Register(h.fl.Loops()...)
	h.el.Register(h.eng.Loops()...)
	h.el.Register(h.c.Loops()...)
	hb := leader.NewHeartbeat(d, leader.InstanceInfo{ID: id, NodeAddress: h.srv.Addr().String()}, log)

	ctx, cancel := context.WithCancel(bg)
	var wg sync.WaitGroup
	run := func(f func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f(ctx)
		}()
	}
	run(h.srv.Serve)
	run(h.fl.Run)
	run(h.eng.Run)
	run(h.el.Run)
	run(hb.Run)
	run(h.door.Run)
	run(h.c.Run)
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(bg, 10*time.Second)
		h.srv.Shutdown(sctx)
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
			t.Error("the instance did not stop")
		}
	})
	eventually(t, 10*time.Second, "the instance leads", h.el.IsLeader)
	return h
}

// addNode joins a test node that checks security groups and name tables,
// and waits until it is connected and ready.
func (h *harness) addNode(name string, mod ...func(*testnode.Config)) *testnode.Node {
	t := h.t
	t.Helper()
	_, token, _, err := h.fl.IssueJoinToken(bg, "test", name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testnode.Config{
		Name: name, JoinToken: token, ServerName: h.iss.ControlPlaneDomain(), TrustDomain: testTD,
		Addrs: []string{h.srv.Addr().String()}, ObjectStore: h.objs, PendingDelay: 20 * time.Millisecond,
		CheckSecurityGroups: true, CheckNameTables: true, Log: dbtest.Logger(t),
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
		if !h.srv.Connected(name) {
			return false
		}
		nd, err := h.st.GetNode(bg, name)
		return err == nil && nd.Status == model.NodeReady && !nd.ReconciledAt.IsZero()
	})
	return n
}

func (h *harness) conn(node string) grpc.ClientConnInterface {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	cc, err := h.srv.Conn(ctx, node)
	if err != nil {
		h.t.Fatal(err)
	}
	return cc
}

// pod reads a pod from its node; nil when the node does not have it.
func (h *harness) pod(node string, id podid.ID) *podv1.PodInfo {
	h.t.Helper()
	info, err := podv1.NewPodServiceClient(h.conn(node)).GetPod(bg, &podv1.GetPodRequest{PodId: id.String()})
	if err != nil {
		return nil
	}
	return info
}

// group reads a security group from a node; nil when it has none.
func (h *harness) group(node, id string) *networkv1.SecurityGroup {
	h.t.Helper()
	g, err := networkv1.NewSecurityGroupServiceClient(h.conn(node)).GetSecurityGroup(bg, &networkv1.GetSecurityGroupRequest{Id: id})
	if err != nil {
		return nil
	}
	return g
}

// table reads a name table from a node; nil when it has none.
func (h *harness) table(node, id string) *networkv1.NameTable {
	h.t.Helper()
	t, err := networkv1.NewNameServiceClient(h.conn(node)).GetNameTable(bg, &networkv1.GetNameTableRequest{Id: id})
	if err != nil {
		return nil
	}
	return t
}

// resolves reports whether the pod resolves name, on its node as feosd's
// resolver would, to exactly addrs (none: the name is not the pod's).
func resolves(n *testnode.Node, pod podid.ID, name string, addrs ...string) bool {
	got, ok := n.Resolve(pod, name)
	return ok == (len(addrs) > 0) && slices.Equal(got, addrs)
}

// newEnv makes an environment of the tenant.
func (h *harness) newEnv(name string) store.Environment {
	h.t.Helper()
	e, err := h.st.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.Must(uuid.NewV7()), Owner: testTenant, Name: name})
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

// apply inserts a release of spec as the service's apply does: the next
// number, older pending and rolling releases superseded, env-reconcile
// and env-groups woken. It returns the release's number.
func (h *harness) apply(envID uuid.UUID, spec string) int {
	h.t.Helper()
	var compact json.RawMessage
	if err := json.Unmarshal([]byte(spec), &compact); err != nil {
		h.t.Fatalf("spec: %v", err)
	}
	var n int
	err := h.st.InTx(bg, func(tx *store.Store) error {
		e, err := tx.LockEnvironment(bg, envID, store.ForUpdate)
		if err != nil {
			return err
		}
		n = e.Release + 1
		if _, err := tx.InsertRelease(bg, store.EnvRelease{EnvID: envID, Number: n, Spec: compact, Base: e.Release,
			Origin: model.OriginApply}); err != nil {
			return err
		}
		after := e
		after.Release = n
		if _, err := tx.UpdateEnvironment(bg, e, after); err != nil {
			return err
		}
		if _, err := tx.SupersedeReleases(bg, envID, n, model.ReleasePending, model.ReleaseRolling); err != nil {
			return err
		}
		if err := tx.Notify(bg, db.ChanLeader, HintReconcile); err != nil {
			return err
		}
		return tx.Notify(bg, db.ChanLeader, HintGroups)
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

// putSecret adds a version of a tenant secret and returns its version.
func (h *harness) putSecret(envID uuid.UUID, name, value string) int {
	h.t.Helper()
	version := 1
	if last, err := h.st.LatestSecret(bg, envID, name); err == nil {
		version = last.Version + 1
	}
	sealed, err := h.sealer.Seal([]byte(value), envdeps.SecretAAD(envID, name, version))
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.st.InsertSecret(bg, store.EnvSecret{EnvID: envID, Name: name, Version: version, Sealed: sealed}); err != nil {
		h.t.Fatal(err)
	}
	return version
}

// release reads a release.
func (h *harness) release(envID uuid.UUID, n int) store.EnvRelease {
	h.t.Helper()
	r, err := h.st.GetRelease(bg, envID, n)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// waitRelease waits until a release is in state.
func (h *harness) waitRelease(envID uuid.UUID, n int, state string, within time.Duration) store.EnvRelease {
	h.t.Helper()
	var r store.EnvRelease
	eventually(h.t, within, fmt.Sprintf("release %d %s", n, state), func() bool {
		r = h.release(envID, n)
		return r.State == state
	})
	return r
}

// rows reads an environment's instances with their rows.
func (h *harness) rows(envID uuid.UUID) []store.EnvInstanceRow {
	h.t.Helper()
	rows, err := h.st.ListEnvInstanceRows(bg, envID)
	if err != nil {
		h.t.Fatal(err)
	}
	return rows
}

// rowsOf are the rows of an object, retiring ones left out.
func (h *harness) rowsOf(envID uuid.UUID, kind, name string) []store.EnvInstanceRow {
	var out []store.EnvInstanceRow
	for _, r := range h.rows(envID) {
		if r.Instance.Kind == kind && r.Instance.Object == name && r.Instance.Phase != model.PhaseRetiring {
			out = append(out, r)
		}
	}
	return out
}

// waitRows waits until an object has n rows (not retiring) that all
// satisfy ok, and returns them.
func (h *harness) waitRows(envID uuid.UUID, kind, name string, n int, within time.Duration, what string,
	ok func(store.EnvInstanceRow) bool) []store.EnvInstanceRow {
	h.t.Helper()
	var rows []store.EnvInstanceRow
	eventually(h.t, within, fmt.Sprintf("%d rows of %s/%s %s", n, kind, name, what), func() bool {
		rows = h.rowsOf(envID, kind, name)
		if len(rows) != n {
			return false
		}
		for _, r := range rows {
			if !ok(r) {
				return false
			}
		}
		return true
	})
	return rows
}

// ready reports a row that runs with its instance ready.
func ready(r store.EnvInstanceRow) bool {
	return r.Instance.Phase == model.PhaseReady && r.Sandbox.ClientState == model.StateRunning
}

// running reports a row whose pod runs.
func running(r store.EnvInstanceRow) bool {
	return r.Sandbox.ClientState == model.StateRunning && r.Sandbox.Op == "" && r.Sandbox.Node != ""
}

// incidents reads an environment's incidents.
func (h *harness) incidents(envID uuid.UUID, open bool) []store.EnvIncident {
	h.t.Helper()
	is, err := h.st.ListIncidents(bg, envID, open)
	if err != nil {
		h.t.Fatal(err)
	}
	return is
}

// incident waits for an open incident of condition of object.
func (h *harness) incident(envID uuid.UUID, object, condition string, within time.Duration) store.EnvIncident {
	h.t.Helper()
	var out store.EnvIncident
	eventually(h.t, within, "incident "+condition+" of "+object, func() bool {
		for _, i := range h.incidents(envID, true) {
			if i.Object == object && i.Condition == condition {
				out = i
				return true
			}
		}
		return false
	})
	return out
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.d.Pool.Exec(bg, sql, args...); err != nil {
		h.t.Fatalf("%s: %v", sql, err)
	}
}

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", within, what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func never(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("%s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
