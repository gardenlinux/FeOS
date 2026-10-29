// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/sandbox"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/tools"
)

// The names of the tests' world.
const (
	testTenant   = "acme"
	testInstance = "i1"
	testAdmin    = "admin"
	testPassword = "s3cret"
)

// envOptions shape a test's API.
type envOptions struct {
	// noAdminUsers leaves Basic off (the default env has admin:s3cret).
	noAdminUsers bool
	// realEngine uses the sandbox engine instead of fakeEngine.
	realEngine bool
	oidc       *OIDCConfig
	publicURL  string
	bindings   Bindings
	watches    Watches
	mcp, ports http.Handler
	sandboxes  tools.Sandboxes
	fleet      interface {
		IssueJoinToken(ctx context.Context, createdBy, node string, ttl time.Duration) (uuid.UUID, string, time.Time, error)
	}
	ingress Ingress
	ready   func(context.Context) error
	// db shares another env's database (a second instance).
	db *db.DB
	// environments builds the environments service on the test's
	// database; nil: a fakeEnvs.
	environments func(d *db.DB, st *store.Store) Environments
	// domains builds the domains service on the test's database; nil: a
	// fakeDomains.
	domains func(d *db.DB, st *store.Store) Domains
}

// env is a test's API over a fresh database.
type env struct {
	t     *testing.T
	d     *db.DB
	st    *store.Store
	h     http.Handler
	srv   *server
	fake  *fakeEngine
	conns *fakeConns
	// envs is the fake environments service, when no other is given.
	envs *fakeEnvs
	// tenantToken is a token of testTenant.
	tenantToken string
}

func newEnv(t *testing.T, o envOptions) *env {
	t.Helper()
	d := o.db
	if d == nil {
		d = dbtest.New(t)
	}
	st := store.New(d)
	e := &env{t: t, d: d, st: st, conns: newFakeConns()}
	log := dbtest.Logger(t)
	var eng Engine
	if o.realEngine {
		real, err := sandbox.New(sandbox.Config{InstanceID: testInstance, TrustDomain: "feos.test",
			DefaultImage: "img", Placement: sandbox.PlacementBinpack, DefaultVCPUs: 2, DefaultMemoryMiB: 512, Log: log},
			sandbox.Deps{DB: d, Store: st, Conns: e.conns})
		if err != nil {
			t.Fatal(err)
		}
		// The test images name no registry that answers (ADR 0025).
		real.SetImageAccess(func(context.Context, string) error { return nil })
		eng = real
	} else {
		e.fake = &fakeEngine{st: st}
		eng = e.fake
	}
	cfg := Config{
		InstanceID: testInstance, PreparationWaitMs: 425000, PublicURL: o.publicURL, OIDC: o.oidc,
		SessionTTL: time.Hour, DefaultImage: "ghcr.io/maltej/debian-ai:latest", DefaultVCPUs: 2, DefaultMemoryMiB: 512,
		NodeTTL: 20 * time.Second, LostGrace: 7 * time.Minute, DrainTimeout: 90 * time.Minute,
		Log: log,
	}
	if !o.noAdminUsers {
		cfg.AdminUsers = map[string]string{testAdmin: testPassword}
	}
	sbs := o.sandboxes
	if sbs == nil {
		sbs = &fakeSandboxes{err: errors.New("no sandbox in this test")}
	}
	runner := tools.NewRunner(sbs, e.conns, nil, nil, testInstance, log)
	deps := Deps{
		DB: d, Store: st, Engine: eng, Conns: e.conns, Fleet: o.fleet, Tools: tools.NewREST(runner),
		Specs: tools.Specs, MCP: o.mcp, Ports: o.ports, Watches: o.watches, Bindings: o.bindings, Ready: o.ready,
		Ingress: o.ingress,
	}
	if o.environments != nil {
		deps.Environments = o.environments(d, st)
	} else {
		e.envs = &fakeEnvs{}
		deps.Environments = e.envs
	}
	if o.domains != nil {
		deps.Domains = o.domains(d, st)
	} else {
		deps.Domains = fakeDomains{}
	}
	e.srv = newServer(cfg, deps)
	e.h = e.srv.mux
	if _, _, err := st.CreateTenant(context.Background(), testTenant); err != nil {
		t.Fatal(err)
	}
	if o.db == nil {
		e.tenantToken = e.mintTenantToken(testTenant)
	}
	return e
}

// mintTenantToken stores a new token of tenant and returns it.
func (e *env) mintTenantToken(tenant string) string {
	e.t.Helper()
	tok, hash, err := mintTenantToken()
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.CreateTenantToken(context.Background(), hash, tenant, "test"); err != nil {
		e.t.Fatal(err)
	}
	return tok
}

// request is one request to the API.
type request struct {
	method, path string
	body         any // string, []byte or a value marshalled as JSON
	header       http.Header
	cookies      []*http.Cookie
}

func (e *env) serve(rq request) *httptest.ResponseRecorder {
	e.t.Helper()
	var body io.Reader
	switch b := rq.body.(type) {
	case nil:
	case string:
		body = strings.NewReader(b)
	case []byte:
		body = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(rq.method, rq.path, body)
	for k, vs := range rq.header {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	for _, c := range rq.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// do sends a request with an Authorization header ("" for none).
func (e *env) do(method, path, authz string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	h := http.Header{}
	if authz != "" {
		h.Set("Authorization", authz)
	}
	return e.serve(request{method: method, path: path, body: body, header: h})
}

func bearerOf(tok string) string { return "Bearer " + tok }

func basicOf(user, password string) string {
	r := httptest.NewRequest("GET", "/", nil)
	r.SetBasicAuth(user, password)
	return r.Header.Get("Authorization")
}

func adminAuth() string { return basicOf(testAdmin, testPassword) }

// expect checks a response's status and, when want is not "", its body
// exactly.
func expect(t *testing.T, w *httptest.ResponseRecorder, status int, want string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d; body %s", w.Code, status, w.Body.String())
	}
	if want != "" && w.Body.String() != want {
		t.Fatalf("body\n%s\nwant\n%s", w.Body.String(), want)
	}
}

// errBody is the API's error body.
func errBody(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b) + "\n"
}

// decode reads a JSON answer.
func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("answer %q: %v", w.Body.String(), err)
	}
	return v
}

// createSandbox creates a sandbox of the env's tenant over REST and returns
// the ID ferund minted and the token.
func (e *env) createSandbox() (string, string) {
	e.t.Helper()
	w := e.do("POST", "/api/v1/sandboxes", bearerOf(e.tenantToken), map[string]any{})
	expect(e.t, w, http.StatusCreated, "")
	out := decode[struct {
		Status struct {
			SandboxID string `json:"sandboxId"`
		} `json:"status"`
		Token string `json:"token"`
	}](e.t, w)
	return out.Status.SandboxID, out.Token
}

// fakeEngine is the sandbox engine of the tests that need no node.
type fakeEngine struct {
	st *store.Store

	mu        sync.Mutex
	pauseErr  error
	resumeErr error
	paused    []podid.ID
	resumed   []podid.ID
}

func (f *fakeEngine) Ensure(context.Context, podid.ID) (model.Lease, error) {
	return model.Lease{}, nil
}

func (f *fakeEngine) Pause(_ context.Context, id podid.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = append(f.paused, id)
	return f.pauseErr
}

func (f *fakeEngine) Resume(_ context.Context, id podid.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed = append(f.resumed, id)
	return f.resumeErr
}

func (f *fakeEngine) Delete(ctx context.Context, owner string, id podid.ID) (store.Sandbox, bool, error) {
	var row store.Sandbox
	err := f.st.InTx(ctx, func(tx *store.Store) error {
		var err error
		row, err = tx.LockSandbox(ctx, id, store.ForUpdate)
		if err != nil || (owner != "" && row.Owner != owner) {
			return model.ErrNotFound
		}
		_, err = tx.DeleteSandbox(ctx, id)
		return err
	})
	return row, row.Node != "", err
}

func (f *fakeEngine) DeleteTenant(ctx context.Context, tenant string) (bool, error) {
	ok, err := f.st.MarkTenantDeleting(ctx, tenant)
	if !ok || err != nil {
		return ok, err
	}
	_, err = f.st.DeleteTenantIfEmpty(ctx, tenant)
	return true, err
}

func (f *fakeEngine) Touch(podid.ID) {}

// fakeSandboxes is tools.Sandboxes failing every Acquire with err.
type fakeSandboxes struct {
	err   error
	lease model.Lease
}

func (f *fakeSandboxes) Acquire(_ context.Context, id podid.ID, _ model.HoldKind) (model.Lease, func(), error) {
	if f.err != nil {
		return model.Lease{}, nil, f.err
	}
	l := f.lease
	l.SandboxID = id
	return l, func() {}, nil
}
func (f *fakeSandboxes) Sync(context.Context, podid.ID) error { return nil }
func (f *fakeSandboxes) WaitRunning(context.Context, podid.ID) (model.Lease, error) {
	return model.Lease{}, model.ErrNotRunning
}
func (f *fakeSandboxes) Unreachable(podid.ID) {}
func (f *fakeSandboxes) Reachable(podid.ID)   {}
func (f *fakeSandboxes) HoldProcess(context.Context, podid.ID, string, time.Duration) error {
	return nil
}
func (f *fakeSandboxes) ReleaseProcess(context.Context, podid.ID, string) error { return nil }

// fakeConns is nodelink.Conns with settable connections; Conn fails with
// connErr (Unavailable when nil).
type fakeConns struct {
	mu          sync.Mutex
	connected   map[string]bool
	connections map[string]int
	connErr     error
}

func newFakeConns() *fakeConns {
	return &fakeConns{connected: map[string]bool{}, connections: map[string]int{}}
}

func (c *fakeConns) set(node string, connected bool, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected[node], c.connections[node] = connected, n
}

func (c *fakeConns) Conn(_ context.Context, node string) (grpc.ClientConnInterface, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connErr != nil {
		return nil, c.connErr
	}
	return nil, status.Errorf(codes.Unavailable, "node %s is not connected to instance %s", node, testInstance)
}

func (c *fakeConns) WaitConnected(ctx context.Context, node string) error {
	_, err := c.Conn(ctx, node)
	return err
}

func (c *fakeConns) Connected(node string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected[node]
}

func (c *fakeConns) Connections(node string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connections[node]
}

func (c *fakeConns) Nodes() []string { return nil }

func (c *fakeConns) Subscribe() (<-chan nodelink.ConnEvent, func()) {
	ch := make(chan nodelink.ConnEvent)
	return ch, func() {}
}

var _ nodelink.Conns = (*fakeConns)(nil)

// marker is a handler that answers its name: a mount of another door.
func marker(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Door", name)
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(name + " " + r.PathValue("id") + " " + r.PathValue("port") + " " + r.PathValue("rest")))
	})
}

// fakeBindings is the bindings with a switch; every handler answers its
// name.
type fakeBindings struct{ on bool }

func (b *fakeBindings) Enabled() bool { return b.on }
func (b *fakeBindings) answer(w http.ResponseWriter, name string) {
	w.Header().Set("X-Door", "bindings")
	_, _ = w.Write([]byte(name))
}
func (b *fakeBindings) PutKube(w http.ResponseWriter, _ *http.Request)    { b.answer(w, "PutKube") }
func (b *fakeBindings) GetKube(w http.ResponseWriter, _ *http.Request)    { b.answer(w, "GetKube") }
func (b *fakeBindings) DeleteKube(w http.ResponseWriter, _ *http.Request) { b.answer(w, "DeleteKube") }
func (b *fakeBindings) PutSSH(w http.ResponseWriter, _ *http.Request)     { b.answer(w, "PutSSH") }
func (b *fakeBindings) GetSSH(w http.ResponseWriter, _ *http.Request)     { b.answer(w, "GetSSH") }
func (b *fakeBindings) DeleteSSH(w http.ResponseWriter, _ *http.Request)  { b.answer(w, "DeleteSSH") }
func (b *fakeBindings) CheckSSH(w http.ResponseWriter, _ *http.Request)   { b.answer(w, "CheckSSH") }
func (b *fakeBindings) KubePreflight(w http.ResponseWriter, _ *http.Request) {
	b.answer(w, "KubePreflight")
}
func (b *fakeBindings) SSHPreflight(w http.ResponseWriter, _ *http.Request) {
	b.answer(w, "SSHPreflight")
}
func (b *fakeBindings) Access(w http.ResponseWriter, _ *http.Request) {
	if !b.on {
		// The real service's answer while off.
		_, _ = w.Write([]byte(`{"kube":null,"ssh":null,"denies":[]}` + "\n"))
		return
	}
	b.answer(w, "Access")
}

// fakeWatches answers each handler's name.
type fakeWatches struct{}

func (fakeWatches) Create(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("Create")) }
func (fakeWatches) Update(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("Update")) }
func (fakeWatches) Events(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("Events")) }
func (fakeWatches) Delete(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("Delete")) }

// newTenantRequest is a request made as tenant, past the tenant class.
func newTenantRequest(t *testing.T, method, path, tenant string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	return r.WithContext(auth.WithTenant(r.Context(), auth.Tenant{Name: tenant}))
}

func recorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }
