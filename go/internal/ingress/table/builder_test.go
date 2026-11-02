// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package table

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

var bg = context.Background()

// errDown is the error of a database that cannot be reached.
var errDown = errors.New("the database cannot be reached")

// flaky runs statements on q and counts them; while down is set, each
// fails with errDown.
type flaky struct {
	q       db.Querier
	queries atomic.Int64
	down    atomic.Bool
}

func (f *flaky) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.queries.Add(1)
	if f.down.Load() {
		return pgconn.CommandTag{}, errDown
	}
	return f.q.Exec(ctx, sql, args...)
}

func (f *flaky) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.queries.Add(1)
	if f.down.Load() {
		return nil, errDown
	}
	return f.q.Query(ctx, sql, args...)
}

func (f *flaky) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	f.queries.Add(1)
	if f.down.Load() {
		return downRow{}
	}
	return f.q.QueryRow(ctx, sql, args...)
}

type downRow struct{}

func (downRow) Scan(...any) error { return errDown }

// fixture is a database with a tenant, a node and a public environment
// whose deployment 1 routes / to the image service web's port http (8080),
// with one ready instance of it, running on n1 with an address, whose
// node admits 8080 and holds version 2 of ferun-ingress.
type fixture struct {
	d   *db.DB
	st  *store.Store
	q   *flaky
	env store.Environment
	sb  podid.ID
}

const webSpec = `{"services": {"web": {"runtime": "image", "image": "web@sha256:1",
	"ports": [{"name": "http", "port": 8080}]}}, "routes": {"root": {"path": "/", "service": "web", "port": "http"}}}`

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Parallel()
	d := dbtest.New(t)
	f := &fixture{d: d, st: store.New(d), q: &flaky{q: d.Pool}}
	if _, _, err := f.st.CreateTenant(bg, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.InsertNode(bg, "n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SetNodeIngressGroupVersion(bg, "n1", 2); err != nil {
		t.Fatal(err)
	}
	f.env = f.mkEnv(t, "web", webSpec)
	f.sb = f.mkInstance(t, f.env, "fd00:0:0:1::5", 8080)
	return f
}

// mkEnv makes a public environment of acme at deployment 1 with spec.
func (f *fixture) mkEnv(t *testing.T, name, spec string) store.Environment {
	t.Helper()
	e, err := f.st.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: "acme", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.InsertDeployment(bg, store.EnvDeployment{EnvID: e.ID, Number: 1, Spec: json.RawMessage(spec),
		Origin: model.OriginApply}); err != nil {
		t.Fatal(err)
	}
	after := e
	after.Deployment = 1
	if _, err := f.st.UpdateEnvironment(bg, e, after); err != nil {
		t.Fatal(err)
	}
	e, err = f.st.SetEnvironmentPublic(bg, e.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// mkInstance makes a ready instance of web in env, running on n1 at
// address, whose node admits ports.
func (f *fixture) mkInstance(t *testing.T, env store.Environment, address string, ports ...int) podid.ID {
	t.Helper()
	sb, created, err := f.st.CreateSandbox(bg, store.NewSandbox{ID: podid.Mint(), Owner: "acme", Image: "web", VCPUs: 1,
		MemoryMiB: 256, EnvironmentID: env.ID, PodTemplate: &model.PodTemplate{SecurityGroups: []string{model.EnvGroupID(env.ID)}}})
	if err != nil || !created {
		t.Fatalf("create sandbox: %v %v", err, created)
	}
	f.setSandbox(t, sb.ID, func(s *store.Sandbox) { s.Node, s.ClientState = "n1", model.StateRunning })
	sb, err = f.st.GetSandbox(bg, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.st.SetSandboxAddress(bg, sb.ID, sb.Incarnation, address); err != nil || !ok {
		t.Fatalf("address: %v %v", ok, err)
	}
	if _, err := f.st.InsertEnvInstance(bg, store.EnvInstance{SandboxID: sb.ID, EnvID: env.ID, Kind: "service", Object: "web",
		TemplateHash: "h1", Deployment: 1, Phase: model.PhaseReady}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SetIngressPorts(bg, sb.ID, ports); err != nil {
		t.Fatal(err)
	}
	return sb.ID
}

// setSandbox changes the sandbox row with UpdateSandbox, which sends
// ferun_sandbox for a lifecycle column.
func (f *fixture) setSandbox(t *testing.T, id podid.ID, change func(s *store.Sandbox)) {
	t.Helper()
	before, err := f.st.GetSandbox(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	after := before
	change(&after)
	if _, err := f.st.UpdateSandbox(bg, before, after); err != nil {
		t.Fatal(err)
	}
}

// setPhase changes the instance's phase with UpdateEnvInstance.
func (f *fixture) setPhase(t *testing.T, id podid.ID, phase string) {
	t.Helper()
	before, err := f.st.LockEnvInstance(bg, id, store.NoLock)
	if err != nil {
		t.Fatal(err)
	}
	after := before
	after.Phase = phase
	if _, err := f.st.UpdateEnvInstance(bg, before, after); err != nil {
		t.Fatal(err)
	}
}

// builder returns a builder on the fixture's flaky querier with a full
// read only every hour, so that only notifications bring changes, run
// until the test ends.
func (f *fixture) builder(t *testing.T, mod ...func(b *Builder)) *Builder {
	t.Helper()
	b := NewBuilder(f.d, store.On(f.q), "apps.example", dbtest.Logger(t))
	b.Debounce, b.Full = 10*time.Millisecond, time.Hour
	for _, m := range mod {
		m(b)
	}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := b.Run(ctx); err != nil {
			t.Errorf("run: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return b
}

// use uses b and waits for its first snapshot.
func use(t *testing.T, b *Builder) (*Snapshot, func()) {
	t.Helper()
	release := b.Use()
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	s, err := b.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	return s, release
}

// await waits up to within for a snapshot that ok accepts and returns it.
func await(t *testing.T, b *Builder, within time.Duration, what string, ok func(s *Snapshot) bool) *Snapshot {
	t.Helper()
	timer := time.NewTimer(within)
	defer timer.Stop()
	for {
		ch := b.Changed()
		if s := b.Current(); s != nil && ok(s) {
			return s
		}
		select {
		case <-ch:
		case <-timer.C:
			t.Fatalf("no snapshot with %s within %v; current %+v", what, within, b.Current())
		}
	}
}

func webService(s *Snapshot, env uuid.UUID) (wire.Service, bool) {
	e, ok := s.Envs[env.String()]
	if !ok || len(e.Services) != 1 {
		return wire.Service{}, false
	}
	return e.Services[0], true
}

// The builder follows the store's writes within a second each: the
// first snapshot has the endpoint; a sandbox change (ferun_sandbox) makes
// it asleep and back; stopping takes it out; unpublishing removes the
// environment (its read has no rows); publishing again brings it back,
// and the tenant's deletion removes it. An environment whose routing
// deployment does not parse is left out.
func TestBuilderFollows(t *testing.T) {
	f := newFixture(t)
	broken := f.mkEnv(t, "broken", `{"services": 5}`)
	b := f.builder(t)
	s, _ := use(t, b)

	e, ok := s.Envs[f.env.ID.String()]
	if !ok || len(s.Envs) != 1 {
		t.Fatalf("envs %+v", s.Envs)
	}
	if _, ok := s.Envs[broken.ID.String()]; ok {
		t.Fatal("an environment whose spec does not parse is served")
	}
	want := []wire.Endpoint{{Instance: f.sb.String(), Address: "fd00:0:0:1::5", Node: "n1",
		Ports: map[string]int{"http": 8080}, GroupVersion: 2}}
	if e.Deployment != 1 || len(e.Hosts) != 1 || e.Hosts[0] != "web-acme.apps.example" || len(e.Services) != 1 ||
		!reflect.DeepEqual(e.Services[0].Endpoints, want) {
		t.Fatalf("env %+v", e)
	}
	if !s.Healthy || s.At.IsZero() || s.bySandbox[f.sb] != f.env.ID {
		t.Fatalf("snapshot healthy %v at %v index %v", s.Healthy, s.At, s.bySandbox)
	}

	// The sandbox pauses: asleep, without the endpoint; it runs again.
	f.setSandbox(t, f.sb, func(s *store.Sandbox) { s.ClientState = model.StatePaused })
	await(t, b, time.Second, "the instance asleep", func(s *Snapshot) bool {
		svc, ok := webService(s, f.env.ID)
		return ok && svc.Asleep == 1 && len(svc.Endpoints) == 0
	})
	f.setSandbox(t, f.sb, func(s *store.Sandbox) { s.ClientState = model.StateRunning })
	await(t, b, time.Second, "the endpoint back", func(s *Snapshot) bool {
		svc, ok := webService(s, f.env.ID)
		return ok && len(svc.Endpoints) == 1
	})

	// Stopping: the environment stays, without the instance.
	f.setPhase(t, f.sb, model.PhaseStopping)
	s = await(t, b, time.Second, "the stopped endpoint gone", func(s *Snapshot) bool {
		svc, ok := webService(s, f.env.ID)
		return ok && len(svc.Endpoints) == 0 && svc.Asleep+svc.Waking+svc.Pending == 0
	})
	if _, ok := s.bySandbox[f.sb]; ok {
		t.Fatal("a stopped instance's sandbox is still indexed")
	}

	// A new instance comes with its environment's notification.
	sb2 := f.mkInstance(t, f.env, "fd00:0:0:1::6", 8080)
	await(t, b, time.Second, "the new endpoint", func(s *Snapshot) bool {
		svc, ok := webService(s, f.env.ID)
		return ok && len(svc.Endpoints) == 1 && svc.Endpoints[0].Instance == sb2.String()
	})

	// Unpublished: removed; published again: back.
	if _, err := f.st.SetEnvironmentPublic(bg, f.env.ID, false); err != nil {
		t.Fatal(err)
	}
	await(t, b, time.Second, "the unpublished environment removed", func(s *Snapshot) bool {
		_, ok := s.Envs[f.env.ID.String()]
		return !ok
	})
	if _, err := f.st.SetEnvironmentPublic(bg, f.env.ID, true); err != nil {
		t.Fatal(err)
	}
	await(t, b, time.Second, "the environment published again", func(s *Snapshot) bool {
		_, ok := s.Envs[f.env.ID.String()]
		return ok
	})

	// The tenant's deletion marks its environments deleting: removed.
	if _, err := f.st.MarkTenantEnvironmentsDeleting(bg, "acme"); err != nil {
		t.Fatal(err)
	}
	s = await(t, b, time.Second, "the deleting environment removed", func(s *Snapshot) bool { return len(s.Envs) == 0 })
	if len(s.bySandbox) != 0 || len(s.sandboxes) != 0 {
		t.Fatalf("index left: %v %v", s.bySandbox, s.sandboxes)
	}
}

// F13: an environment too large for the table (ErrTooLarge) is left out,
// on the whole read and on its own, as one without a usable spec is; the
// other environments stay, and a deployment that makes an environment too
// large takes it out.
func TestBuilderTooLarge(t *testing.T) {
	f := newFixture(t)
	raw, err := json.Marshal(routesSpec(spec.MaxRoutes, 20000))
	if err != nil {
		t.Fatal(err)
	}
	huge := f.mkEnv(t, "huge", string(raw))
	grow := f.mkEnv(t, "grow", webSpec)
	b := f.builder(t)
	s, _ := use(t, b)
	if _, ok := s.Envs[huge.ID.String()]; ok {
		t.Fatal("an environment too large for the table is served")
	}
	if _, ok := s.Envs[f.env.ID.String()]; !ok || len(s.Envs) != 2 {
		t.Fatalf("envs %v", s.Envs)
	}

	// Deployment 2 makes grow too large: it leaves the table.
	if _, err := f.st.InsertDeployment(bg, store.EnvDeployment{EnvID: grow.ID, Number: 2, Spec: raw,
		Origin: model.OriginApply}); err != nil {
		t.Fatal(err)
	}
	after := grow
	after.Deployment = 2
	if _, err := f.st.UpdateEnvironment(bg, grow, after); err != nil {
		t.Fatal(err)
	}
	await(t, b, time.Second, "the grown environment removed", func(s *Snapshot) bool {
		_, ok := s.Envs[grow.ID.String()]
		return !ok
	})
	// The others still follow their changes.
	f.setPhase(t, f.sb, model.PhaseStopping)
	s = await(t, b, time.Second, "the stopped endpoint gone", func(s *Snapshot) bool {
		svc, ok := webService(s, f.env.ID)
		return ok && len(svc.Endpoints) == 0
	})
	if _, ok := s.Envs[huge.ID.String()]; ok || len(s.Envs) != 1 {
		t.Fatalf("envs %v", s.Envs)
	}
}

// A write that sends no notification (an older ferund's) is read on
// NOTIFY ferun_ingress "*".
func TestBuilderFullRead(t *testing.T) {
	f := newFixture(t)
	b := f.builder(t)
	use(t, b)
	// The setup's own notifications may still be on their way: let the
	// builder take them first.
	time.Sleep(300 * time.Millisecond)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.d.Pool.Exec(bg, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec("UPDATE env_instances SET ingress_ports = '{}' WHERE sandbox_id = $1", f.sb)
	time.Sleep(200 * time.Millisecond)
	if svc, ok := webService(b.Current(), f.env.ID); !ok || len(svc.Endpoints) != 1 {
		t.Fatalf("a write without a notification was read: %+v", svc)
	}
	exec("SELECT pg_notify($1, '*')", db.ChanIngress)
	await(t, b, time.Second, "the instance pending", func(s *Snapshot) bool {
		svc, ok := webService(s, f.env.ID)
		return ok && len(svc.Endpoints) == 0 && svc.Pending == 1
	})
}

// The full read's backstop reads a write without any notification.
func TestBuilderBackstop(t *testing.T) {
	f := newFixture(t)
	b := f.builder(t, func(b *Builder) { b.Full = 100 * time.Millisecond })
	use(t, b)
	if _, err := f.d.Pool.Exec(bg, "UPDATE env_instances SET ingress_ports = '{}' WHERE sandbox_id = $1", f.sb); err != nil {
		t.Fatal(err)
	}
	await(t, b, time.Second, "the backstop's read", func(s *Snapshot) bool {
		svc, ok := webService(s, f.env.ID)
		return ok && len(svc.Endpoints) == 0
	})
}

// The mesh's nodes come with "mesh".
func TestBuilderMesh(t *testing.T) {
	f := newFixture(t)
	b := f.builder(t)
	s, _ := use(t, b)
	if len(s.Mesh.Peers) != 0 {
		t.Fatalf("peers before the mesh %+v", s.Mesh)
	}
	if _, err := f.d.Pool.Exec(bg, `INSERT INTO pod_network (id, ula, peers_version, ingress_group_version)
		VALUES (true, 'fd12:3456:789a::/64', 1, 4)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Pool.Exec(bg, `UPDATE nodes SET mesh_subnet = 1, mesh_prefix = 'fd12:3456:789a::1:0:0/96',
		mesh_public_key = $1, mesh_endpoint = '10.0.0.1:51820' WHERE name = 'n1'`, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Pool.Exec(bg, "SELECT pg_notify($1, 'mesh')", db.ChanIngress); err != nil {
		t.Fatal(err)
	}
	s = await(t, b, time.Second, "the node peer", func(s *Snapshot) bool { return len(s.Mesh.Peers) == 1 })
	if p := s.Mesh.Peers[0]; p.Name != "n1" || p.Endpoint != "10.0.0.1:51820" || p.Prefix != "fd12:3456:789a::1:0:0/96" ||
		s.Mesh.Version != 1 || s.GroupVersion != 4 {
		t.Fatalf("mesh %+v, group version %d", s.Mesh, s.GroupVersion)
	}
}

// While no one uses the builder it reads nothing; the next user waits
// for a new read.
func TestBuilderIdle(t *testing.T) {
	f := newFixture(t)
	b := f.builder(t, func(b *Builder) { b.Full = 20 * time.Millisecond })
	time.Sleep(200 * time.Millisecond)
	if n := f.q.queries.Load(); n != 0 || b.Current() != nil {
		t.Fatalf("%d statements before a use", n)
	}
	_, release := use(t, b)
	if f.q.queries.Load() == 0 {
		t.Fatal("no statement for a use")
	}
	release()
	time.Sleep(100 * time.Millisecond) // the loop notices
	n := f.q.queries.Load()
	time.Sleep(200 * time.Millisecond)
	if m := f.q.queries.Load(); m != n {
		t.Fatalf("%d statements after the last use", m-n)
	}

	// A change while unused; the next user's first snapshot has it.
	f.setPhase(t, f.sb, model.PhaseStopping)
	s, _ := use(t, b)
	if svc, ok := webService(s, f.env.ID); !ok || len(svc.Endpoints) != 0 {
		t.Fatalf("a stale snapshot after a pause: %+v", svc)
	}
}

// A builder that cannot read the database turns its snapshot unhealthy
// once the health window passed, and healthy again when it reads.
func TestBuilderHealth(t *testing.T) {
	f := newFixture(t)
	b := f.builder(t, func(b *Builder) { b.Full, b.Health = 50*time.Millisecond, 300*time.Millisecond })
	s, _ := use(t, b)
	if !s.Healthy {
		t.Fatal("the first snapshot is not healthy")
	}
	f.q.down.Store(true)
	start := time.Now()
	s = await(t, b, 2*time.Second, "an unhealthy snapshot", func(s *Snapshot) bool { return !s.Healthy })
	if time.Since(start) < 200*time.Millisecond {
		t.Fatalf("unhealthy after %v, before the window", time.Since(start))
	}
	if _, ok := s.Envs[f.env.ID.String()]; !ok {
		t.Fatal("the unhealthy snapshot lost the table")
	}
	f.q.down.Store(false)
	await(t, b, 2*time.Second, "a healthy snapshot", func(s *Snapshot) bool { return s.Healthy })
}

// Specs parses a deployment once and keeps it, apart for an environment
// made again with the same ID; a missing or unparsable deployment is
// ErrSpec; past maxSpecs it forgets one for each new one.
func TestSpecs(t *testing.T) {
	f := newFixture(t)
	broken := f.mkEnv(t, "broken", `{"services": 5}`)
	c := NewSpecs(store.On(f.q))
	sp, err := c.Get(bg, f.env.ID, f.env.CreatedAt, 1)
	if err != nil || sp.Services["web"].Ports[0].Port != 8080 || sp.Routes["root"].Path != "/" {
		t.Fatalf("spec %+v %v", sp, err)
	}
	n := f.q.queries.Load()
	if again, err := c.Get(bg, f.env.ID, f.env.CreatedAt, 1); err != nil || again != sp || f.q.queries.Load() != n {
		t.Fatalf("not cached: %v, %d statements", err, f.q.queries.Load()-n)
	}
	if other, err := c.Get(bg, f.env.ID, f.env.CreatedAt.Add(time.Second), 1); err != nil || other == sp {
		t.Fatalf("an environment made again shares the cache: %v", err)
	}
	for _, c2 := range []struct {
		env     uuid.UUID
		created time.Time
		number  int
	}{{f.env.ID, f.env.CreatedAt, 2}, {f.env.ID, f.env.CreatedAt, 0}, {broken.ID, broken.CreatedAt, 1}} {
		if _, err := c.Get(bg, c2.env, c2.created, c2.number); !errors.Is(err, ErrSpec) {
			t.Fatalf("%v deployment %d: %v", c2.env, c2.number, err)
		}
	}
	f.q.down.Store(true)
	if _, err := c.Get(bg, f.env.ID, f.env.CreatedAt, 3); err == nil || errors.Is(err, ErrSpec) {
		t.Fatalf("a read error: %v", err)
	}
	f.q.down.Store(false)

	// Full: one forgotten for each new one.
	c.mu.Lock()
	for i := len(c.m); i < maxSpecs; i++ {
		c.m[specKey{env: uuid.New(), number: 1}] = sp
	}
	c.mu.Unlock()
	if _, err := c.Get(bg, broken.ID, time.Time{}, 1); !errors.Is(err, ErrSpec) {
		t.Fatal(err)
	}
	e2 := f.mkEnv(t, "other", webSpec)
	if _, err := c.Get(bg, e2.ID, e2.CreatedAt, 1); err != nil {
		t.Fatal(err)
	}
	if got := c.size(); got != maxSpecs {
		t.Fatalf("%d specs kept, want %d", got, maxSpecs)
	}
	fn := c.For(bg, EnvRows{ID: e2.ID, CreatedAt: e2.CreatedAt})
	if got, err := fn(1); err != nil || got.Services["web"].Image != "web@sha256:1" {
		t.Fatalf("For: %+v %v", got, err)
	}
}
