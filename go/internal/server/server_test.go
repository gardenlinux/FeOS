// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/status"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
	"github.com/gardenlinux/feos/go/internal/tools"
)

// The tests of this package do not run in parallel: the streams hook is
// package-wide.

const (
	trustDomain    = "feos.test"
	dnsDomain      = "example.test"
	bootstrapToken = "bootstrap-token-0123456789"
	// testImage is pinned, so that no registry is asked.
	testImage = "example.test/img@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// testCA makes a CA and returns the issuer's directory.
func testCA(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "ca")
	if err := pki.InitCA(pki.InitCAOptions{RootDir: filepath.Join(base, "root"), Dir: dir, TrustDomain: trustDomain,
		DNSDomains: []string{dnsDomain}}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// testConfig is an instance's configuration on url with the CA in caDir,
// on free ports, with a short stop.
func testConfig(url, caDir string) config.Config {
	c := config.Default()
	c.DatabaseURL, c.CADir, c.TrustDomain = url, caDir, trustDomain
	c.Listen, c.NodeListen = "127.0.0.1:0", "127.0.0.1:0"
	c.BootstrapToken = bootstrapToken
	c.AdminUsers = map[string]string{"admin": "pw"}
	c.DefaultImage = testImage
	c.ReadinessDelay, c.ShutdownGrace = 0, 10*time.Second
	c.IdlePause = 0
	c.Version = "test"
	return c
}

// started runs an instance until the test ends (or stop is called) and
// returns a function that stops it and waits for Run.
func started(t *testing.T, i *Instance) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- i.Run(ctx) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cancel()
			err = <-done
		})
		return err
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

// waitServing waits until the instance answers /healthz: Run has
// started.
func waitServing(t *testing.T, i *Instance) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get("http://" + i.HTTPAddr() + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the instance does not serve: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// get fetches a URL of the instance.
func get(t *testing.T, i *Instance, path string, header http.Header) (int, string) {
	t.Helper()
	return call(t, i, "GET", path, header, "")
}

func call(t *testing.T, i *Instance, method, path string, header http.Header, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+i.HTTPAddr()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func bearer(tok string) http.Header { return http.Header{"Authorization": {"Bearer " + tok}} }

// Two instances starting together on an empty database: one applies the
// migrations and one bootstraps the operator, with exactly one token.
func TestBootstrapRace(t *testing.T) {
	url := dbtest.NewDatabase(t)
	caDir := testCA(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	insts := make([]*Instance, 2)
	errs := make([]error, 2)
	for n := range insts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			insts[n], errs[n] = New(ctx, testConfig(url, caDir), dbtest.Logger(t))
		}()
	}
	wg.Wait()
	for n, err := range errs {
		if err != nil {
			t.Fatalf("instance %d: %v", n, err)
		}
	}
	for _, i := range insts {
		started(t, i)
	}
	st := store.New(insts[0].DB)
	tenants, err := st.ListTenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tenants) != 1 || tenants[0].ID != "operator" || len(tenants[0].Tokens) != 1 ||
		tenants[0].Tokens[0].Hash != store.HashToken(bootstrapToken) || tenants[0].Tokens[0].Label != "bootstrap" {
		t.Fatalf("tenants %+v", tenants)
	}
	if insts[0].ID() == insts[1].ID() || !strings.Contains(insts[0].ID(), "-") {
		t.Fatalf("ids %s %s", insts[0].ID(), insts[1].ID())
	}
	// The token works on both.
	for _, i := range insts {
		if code, body := get(t, i, "/api/v1/sandboxes", bearer(bootstrapToken)); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
	}
}

// A node that is ready and reported since the start but has not dialled
// this instance holds /readyz back until the ready window ends; a stopping
// instance is not ready.
func TestReadyz(t *testing.T) {
	url := dbtest.NewDatabase(t)
	caDir := testCA(t)
	cfg := testConfig(url, caDir)
	cfg.ReadyWindow = 2 * time.Second
	i, err := New(context.Background(), cfg, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	stop := started(t, i)
	ctx := context.Background()
	waitServing(t, i)
	st := i.Store
	if _, err := st.InsertNode(ctx, "n9"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertTelemetry(ctx, "n9", store.TelemetryReport{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkNodeReady(ctx, "n9"); err != nil {
		t.Fatal(err)
	}
	code, body := get(t, i, "/readyz", nil)
	if code != 503 || body != "not ready: waiting for node n9 to connect\n" {
		t.Fatalf("%d %q", code, body)
	}
	if code, _ := get(t, i, "/healthz", nil); code != 200 {
		t.Fatalf("healthz %d", code)
	}
	for {
		code, body = get(t, i, "/readyz", nil)
		if code == 200 {
			break
		}
		if time.Since(t0) > 10*time.Second {
			t.Fatalf("still %d %q", code, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if time.Since(t0) < cfg.ReadyWindow {
		t.Fatalf("ready after %v, before the window", time.Since(t0))
	}
	i.stopping.Store(true)
	if code, body := get(t, i, "/readyz", nil); code != 503 || body != "not ready: the instance is stopping\n" {
		t.Fatalf("stopping: %d %q", code, body)
	}
	i.stopping.Store(false)
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	// With a node address, the instance waits until nodes had the time
	// to dial it.
	cfg = testConfig(dbtest.NewDatabase(t), caDir)
	cfg.NodeAdvertiseAddress = "127.0.0.1:7443"
	j, err := New(context.Background(), cfg, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	started(t, j)
	waitServing(t, j)
	if code, body := get(t, j, "/readyz", nil); code != 503 || body != "not ready: waiting for the nodes to dial this instance\n" {
		t.Fatalf("advertised: %d %q", code, body)
	}
}

// Ready may be called from the start on, while Run starts (e2e tests
// poll it): it says "starting" until Run has started, then passes. The
// race detector checks that Run's start and Ready do not race.
func TestReadyWhileRunStarts(t *testing.T) {
	i, err := New(context.Background(), testConfig(dbtest.NewDatabase(t), testCA(t)), dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := i.Ready(ctx); err == nil || err.Error() != "the instance is starting" {
		t.Fatalf("before Run: %v", err)
	}
	ready := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for {
			err := i.Ready(ctx)
			if err == nil || time.Now().After(deadline) {
				ready <- err
				return
			}
		}
	}()
	started(t, i)
	if err := <-ready; err != nil {
		t.Fatalf("never ready: %v", err)
	}
}

// Close releases an instance that never ran: its ports are free again,
// its pool is closed, and Run refuses to run it. After Run, Close does
// nothing.
func TestCloseWithoutRun(t *testing.T) {
	url := dbtest.NewDatabase(t)
	caDir := testCA(t)
	i, err := New(context.Background(), testConfig(url, caDir), dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	httpAddr, nodeAddr := i.HTTPAddr(), i.NodeAddr()
	i.Close()
	i.Close()
	for _, addr := range []string{httpAddr, nodeAddr} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("%s still bound: %v", addr, err)
		}
		ln.Close()
	}
	if err := i.DB.Pool.Ping(context.Background()); err == nil {
		t.Fatal("the pool is open")
	}
	if err := i.Run(context.Background()); err == nil || err.Error() != "an instance runs once, and not after Close" {
		t.Fatalf("Run after Close: %v", err)
	}

	j, err := New(context.Background(), testConfig(url, caDir), dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	stop := started(t, j)
	waitServing(t, j)
	j.Close()
	if code, _ := get(t, j, "/healthz", nil); code != 200 {
		t.Fatalf("healthz after Close during Run: %d", code)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	j.Close()
}

// stopRecord is what the streams hook saw of a stop, in order.
type stopRecord struct {
	mu     sync.Mutex
	events []string
}

func (r *stopRecord) add(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *stopRecord) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// The stop of §7 with a node: step 1 fails /readyz and resigns; a request
// in the readiness window that needs an op (a resume) succeeds; the
// streams hook's OnShutdown runs when the server's shutdown begins and
// AfterRequests once no request is left; when the shutdown begins, a new
// tool call with an id is refused (the drain began), and a call whose
// request left is waited for before step 3 (step 2b); after step 3 the
// engine refuses ops (ErrStopping); step 5 removes the row and releases
// the lease.
func TestShutdown(t *testing.T) {
	rec := &stopRecord{}
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	// The command of the call with an id ends 300 ms after the HTTP
	// shutdown: long after step 4 unless step 2b waits for it.
	drainRelease := make(chan struct{})
	// probeSB is the sandbox of the call that OnShutdown tries; probe gets
	// its answer.
	var probeSB auth.Sandbox
	type probeAnswer struct {
		info tools.CallInfo
		err  error
	}
	probe := make(chan probeAnswer, 1)
	streams = func(inst *Instance) Streams {
		return Streams{
			Ports: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				entered <- struct{}{}
				<-release
				rec.add("port request done")
				_, _ = w.Write([]byte("ok"))
			}),
			OnShutdown: func() {
				rec.add("OnShutdown")
				_, info, err := inst.Runner.CallKeyed(context.Background(), probeSB, tools.ToolBash,
					json.RawMessage(`{"command":"echo late"}`), "late", nil)
				probe <- probeAnswer{info, err}
			},
			AfterRequests: func() {
				rec.add("AfterRequests")
				time.AfterFunc(300*time.Millisecond, func() { close(drainRelease) })
			},
		}
	}
	t.Cleanup(func() { streams = nil })

	url := dbtest.NewDatabase(t)
	caDir := testCA(t)
	cfg := testConfig(url, caDir)
	// The readiness delay ends when the test has checked the window, not
	// by time; its length is only the bound of a test that fails.
	cfg.ReadinessDelay = 5 * time.Minute
	log := dbtest.Logger(t)
	i, err := New(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	// The test image names no registry that answers (ADR 0025).
	i.Engine.SetImageAccess(func(context.Context, string) error { return nil })
	i.endReadinessDelay = make(chan struct{})
	stop := started(t, i)
	var endOnce sync.Once
	endDelay := func() { endOnce.Do(func() { close(i.endReadinessDelay) }) }
	// A failed test does not wait for the delay (this runs before stop).
	t.Cleanup(endDelay)
	ctx := context.Background()

	// A test node joins.
	_, token, _, err := i.Fleet.IssueJoinToken(ctx, "test", "n1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	node, err := testnode.New(testnode.Config{Name: "n1", JoinToken: token, ServerName: i.Issuer.ControlPlaneDomain(),
		TrustDomain: trustDomain, Addrs: []string{i.NodeAddr()}, PendingDelay: 50 * time.Millisecond,
		Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	nodeCtx, stopNode := context.WithCancel(context.Background())
	nodeDone := make(chan struct{})
	go func() {
		defer close(nodeDone)
		_ = node.Run(nodeCtx)
	}()
	t.Cleanup(func() {
		stopNode()
		<-nodeDone
	})
	deadline := time.Now().Add(30 * time.Second)
	for !i.Links.Connected("n1") {
		if time.Now().After(deadline) {
			t.Fatal("the test node did not connect")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for {
		n, err := i.Store.GetNode(ctx, "n1")
		if err == nil && n.Status == model.NodeReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node is not ready: %+v %v", n, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Connected to every ready node: ready long before the window ends.
	if code, body := get(t, i, "/readyz", nil); code != 200 || body != "ready\n" {
		t.Fatalf("readyz with the node connected: %d %q", code, body)
	}

	// A sandbox runs, then is paused.
	code, body := call(t, i, "POST", "/api/v1/sandboxes", bearer(bootstrapToken), "{}")
	if code != 201 {
		t.Fatalf("create %d %s", code, body)
	}
	var created struct {
		Status struct {
			SandboxID string `json:"sandboxId"`
		} `json:"status"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	id, sbToken := created.Status.SandboxID, created.Token
	if probeSB, err = i.ResolveSandbox(ctx, sbToken); err != nil {
		t.Fatal(err)
	}
	exec := func() (int, string) {
		return call(t, i, "POST", "/api/v1/sandboxes/"+id+"/exec", bearer(sbToken), `{"command":"echo hi"}`)
	}
	if code, body := exec(); code != 200 || !strings.Contains(body, `"output":"hi\n"`) {
		t.Fatalf("exec %d %s", code, body)
	}
	if code, body := call(t, i, "POST", "/api/v1/sandboxes/"+id+"/pause", bearer(bootstrapToken), ""); code != 200 {
		t.Fatalf("pause %d %s", code, body)
	}
	row, err := i.Store.GetSandbox(ctx, idOf(t, id))
	if err != nil || row.ClientState != model.StatePaused {
		t.Fatalf("paused? %+v %v", row.ClientState, err)
	}

	// A request in flight through the port door when the stop begins.
	portDone := make(chan string, 1)
	portReq := mustRequest(t, "GET", "http://"+i.HTTPAddr()+"/fwd/"+id+"/3000/x", sbToken)
	go func() {
		resp, err := http.DefaultClient.Do(portReq)
		if err != nil {
			portDone <- err.Error()
			return
		}
		resp.Body.Close()
		portDone <- resp.Status
	}()
	select {
	case <-entered:
	case got := <-portDone:
		t.Fatalf("port request: %s", got)
	}

	// Step 3's refusal, seen right after step 4.
	var (
		afterStopErr error
		drained      store.ToolCall
		drainedErr   error
	)
	i.afterStop = func() {
		// The sandbox runs again (the readiness window's exec resumed it):
		// a pause needs an op.
		afterStopErr = i.Engine.Pause(ctx, idOf(t, id))
		drained, drainedErr = i.Store.GetToolCall(ctx, idOf(t, id), "drained")
		rec.add("after step 4")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()

	// Step 1: /readyz fails; the lease is released.
	for {
		code, _ := get(t, i, "/readyz", nil)
		if code == 503 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("/readyz never failed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		in, err := i.Store.GetInstance(ctx, i.ID())
		if err == nil && !in.Stopping.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("instance row %+v %v", in, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Step 2's readiness window: a request that needs a resume is served.
	if code, body := exec(); code != 200 || !strings.Contains(body, `"output":"hi\n"`) {
		t.Fatalf("exec in the readiness window: %d %s", code, body)
	}
	// A call with an id is accepted too; its request leaves while its
	// command runs on into the stop.
	drainStarted := make(chan struct{}, 1)
	node.Script().SetExec(func(ctx context.Context, _ podid.ID, req *containerv1.ExecRequest, out testnode.ExecOutput) (int32, bool, error) {
		if !slices.Contains(req.GetCommand(), "drain-me") {
			return 0, false, nil
		}
		drainStarted <- struct{}{}
		select {
		case <-drainRelease:
		case <-ctx.Done():
			return 0, true, status.FromContextError(ctx.Err()).Err()
		}
		return 0, true, out.Stdout([]byte("drained\n"))
	})
	callCtx, hangUp := context.WithCancel(ctx)
	called := make(chan error, 1)
	go func() {
		_, _, err := i.Runner.CallKeyed(callCtx, probeSB, tools.ToolBash, json.RawMessage(`{"command":"drain-me"}`), "drained", nil)
		called <- err
	}()
	select {
	case <-drainStarted:
	case err := <-called:
		t.Fatalf("the call with an id ended at once: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the call with an id did not reach the node")
	}
	hangUp()
	if err := <-called; !errors.Is(err, context.Canceled) {
		t.Fatalf("the call's request that left: %v", err)
	}
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("events before the server's shutdown: %v", got)
	}
	endDelay()
	// The shutdown begins; the port request is still in flight.
	for len(rec.list()) == 0 {
		if time.Now().After(deadline.Add(30 * time.Second)) {
			t.Fatal("no OnShutdown")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if got := strings.Join(rec.list(), ","); got != "OnShutdown" {
		t.Fatalf("events with a request in flight: %s", got)
	}
	close(release)
	if got := <-portDone; got != "200 OK" {
		t.Fatalf("port request %s", got)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rec.list(), ","); got != "OnShutdown,port request done,AfterRequests,after step 4" {
		t.Fatalf("events %s", got)
	}
	if !errors.Is(afterStopErr, model.ErrStopping) || !strings.Contains(afterStopErr.Error(), "instance "+i.ID()+" is stopping") {
		t.Fatalf("pause after step 3: %v", afterStopErr)
	}
	// Step 2 began the drain before the HTTP shutdown: the new call was
	// refused, not run, and nothing ran.
	if p := <-probe; !errors.Is(p.err, model.ErrStopping) || !strings.Contains(p.err.Error(), "instance "+i.ID()+" is stopping: ") ||
		p.info.State != tools.CallNotRun {
		t.Errorf("a call with an id when the shutdown began: %+v %v", p.info, p.err)
	}
	// Step 2b waited for the call whose request left.
	if drainedErr != nil || drained.State != store.ToolCallDone || string(drained.Output) != "drained\n" {
		t.Errorf("the call with an id after step 4: %+v %v", drained, drainedErr)
	}

	// Step 5: the row is gone, the lease released; the pool is closed, so
	// look through another connection.
	check, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close(context.Background())
	var rows int
	if err := check.QueryRow(ctx, "SELECT count(*) FROM instances WHERE id = $1", i.ID()).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("instance rows %d %v", rows, err)
	}
	var valid bool
	if err := check.QueryRow(ctx, "SELECT expires_at > now() FROM leases WHERE name = 'leader'").Scan(&valid); err != nil || valid {
		t.Fatalf("lease valid %v %v", valid, err)
	}
}

func idOf(t *testing.T, s string) podid.ID {
	t.Helper()
	id, err := podid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustRequest(t *testing.T, method, url, token string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}
