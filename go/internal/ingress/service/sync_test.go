// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/ingress/table"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// webSpec is a deployment whose route / goes to port http (8080) of the
// image service web.
const webSpec = `{"services": {"web": {"runtime": "image", "image": "web@sha256:1",
	"ports": [{"name": "http", "port": 8080}]}}, "routes": {"root": {"path": "/", "service": "web", "port": "http"}}}`

// world makes the tenant acme and the node n1, which holds version 2 of
// the group ferun-ingress.
func (h *harness) world() {
	h.t.Helper()
	if _, _, err := h.st.CreateTenant(bg, "acme"); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.st.InsertNode(bg, "n1"); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.st.SetNodeIngressGroupVersion(bg, "n1", 2); err != nil {
		h.t.Fatal(err)
	}
}

// mkEnv makes an environment of acme at deployment 1 with spec, public
// when public.
func (h *harness) mkEnv(name, spec string, public bool) store.Environment {
	h.t.Helper()
	e, err := h.st.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: "acme", Name: name})
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.st.InsertDeployment(bg, store.EnvDeployment{EnvID: e.ID, Number: 1, Spec: json.RawMessage(spec),
		Origin: model.OriginApply}); err != nil {
		h.t.Fatal(err)
	}
	after := e
	after.Deployment = 1
	if e, err = h.st.UpdateEnvironment(bg, e, after); err != nil {
		h.t.Fatal(err)
	}
	if public {
		h.publish(e.ID, true)
	}
	return e
}

func (h *harness) publish(id uuid.UUID, public bool) {
	h.t.Helper()
	if _, err := h.st.SetEnvironmentPublic(bg, id, public); err != nil {
		h.t.Fatal(err)
	}
}

// mkInstance makes an instance of service in env, of phase, whose sandbox
// is in state on n1, with an address when it runs, and whose node admits
// ports.
func (h *harness) mkInstance(env store.Environment, service, phase, state string, ports ...int) podid.ID {
	h.t.Helper()
	sb, created, err := h.st.CreateSandbox(bg, store.NewSandbox{ID: podid.Mint(), Owner: "acme", Image: service, VCPUs: 1,
		MemoryMiB: 256, EnvironmentID: env.ID, PodTemplate: &model.PodTemplate{SecurityGroups: []string{model.EnvGroupID(env.ID)}}})
	if err != nil || !created {
		h.t.Fatalf("create sandbox: %v %v", err, created)
	}
	h.setSandbox(sb.ID, func(s *store.Sandbox) { s.Node, s.ClientState = "n1", state })
	if state == model.StateRunning {
		sb, err = h.st.GetSandbox(bg, sb.ID)
		if err != nil {
			h.t.Fatal(err)
		}
		if ok, err := h.st.SetSandboxAddress(bg, sb.ID, sb.Incarnation, "fd00:0:0:1::5"); err != nil || !ok {
			h.t.Fatalf("address: %v %v", ok, err)
		}
	}
	if _, err := h.st.InsertEnvInstance(bg, store.EnvInstance{SandboxID: sb.ID, EnvID: env.ID, Kind: "service",
		Object: service, TemplateHash: "h1", Deployment: 1, Phase: phase}); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.st.SetIngressPorts(bg, sb.ID, ports); err != nil {
		h.t.Fatal(err)
	}
	return sb.ID
}

// setSandbox changes the sandbox row with UpdateSandbox, which sends
// ferun_sandbox for a lifecycle column.
func (h *harness) setSandbox(id podid.ID, change func(s *store.Sandbox)) {
	h.t.Helper()
	before, err := h.st.GetSandbox(bg, id)
	if err != nil {
		h.t.Fatal(err)
	}
	after := before
	change(&after)
	if _, err := h.st.UpdateSandbox(bg, before, after); err != nil {
		h.t.Fatal(err)
	}
}

// setPhase changes the instance's phase with UpdateEnvInstance.
func (h *harness) setPhase(id podid.ID, phase string) {
	h.t.Helper()
	before, err := h.st.LockEnvInstance(bg, id, store.NoLock)
	if err != nil {
		h.t.Fatal(err)
	}
	after := before
	after.Phase = phase
	if _, err := h.st.UpdateEnvInstance(bg, before, after); err != nil {
		h.t.Fatal(err)
	}
}

// meshNode puts node name on the pods' mesh with WireGuard key k, as its
// Hello does.
func (h *harness) meshNode(name string, k byte) {
	h.t.Helper()
	if _, err := h.st.InsertNode(bg, name); err != nil {
		h.t.Fatal(err)
	}
	err := h.st.InTx(bg, func(tx *store.Store) error {
		n, err := tx.LockNode(bg, name, store.ForUpdate)
		if err != nil {
			return err
		}
		_, err = tx.JoinMesh(bg, n, testULA, wgKey(k), "10.0.1.1:51820")
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

// syncer reads a Sync stream in the background; the test applies its
// messages to a wire.State, which checks their sequence.
type syncer struct {
	t     *testing.T
	msgs  chan *wire.Table
	end   chan error
	state *wire.State
}

// sync opens a Sync stream on c.
func (h *harness) sync(c *client) *syncer {
	h.t.Helper()
	ctx, cancel := context.WithCancel(bg)
	h.t.Cleanup(cancel)
	s, err := c.api.Sync(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	sy := &syncer{t: h.t, msgs: make(chan *wire.Table, 64), end: make(chan error, 1), state: wire.NewState()}
	go func() {
		for {
			tbl, err := s.Recv()
			if err != nil {
				sy.end <- err
				return
			}
			select {
			case sy.msgs <- tbl:
			case <-ctx.Done():
				return
			}
		}
	}()
	return sy
}

// next returns the next message within d, applied to the state.
func (sy *syncer) next(d time.Duration) *wire.Table {
	sy.t.Helper()
	select {
	case tbl := <-sy.msgs:
		if _, err := sy.state.Apply(tbl); err != nil {
			sy.t.Fatalf("apply %+v: %v", tbl, err)
		}
		return tbl
	case err := <-sy.end:
		sy.t.Fatalf("the stream ended: %v", err)
	case <-time.After(d):
		sy.t.Fatalf("no message within %v", d)
	}
	return nil
}

// until applies the messages until ok accepts one, within d, and
// returns that one.
func (sy *syncer) until(d time.Duration, what string, ok func(t *wire.Table) bool) *wire.Table {
	sy.t.Helper()
	deadline := time.Now().Add(d)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			sy.t.Fatalf("no message with %s within %v; state %+v", what, d, sy.state)
		}
		select {
		case tbl := <-sy.msgs:
			if _, err := sy.state.Apply(tbl); err != nil {
				sy.t.Fatalf("apply %+v: %v", tbl, err)
			}
			if ok(tbl) {
				return tbl
			}
		case err := <-sy.end:
			sy.t.Fatalf("the stream ended waiting for %s: %v", what, err)
		case <-time.After(left):
		}
	}
}

// ended returns the error that ended the stream within d.
func (sy *syncer) ended(d time.Duration) error {
	sy.t.Helper()
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-sy.msgs:
		case err := <-sy.end:
			return err
		case <-timer.C:
			sy.t.Fatalf("the stream did not end within %v", d)
			return nil
		}
	}
}

// empty reports whether t is a heartbeat: nothing but its seq and time.
func empty(t *wire.Table) bool {
	return !t.Whole && len(t.Envs) == 0 && len(t.Removed) == 0 && t.Mesh == nil && t.Self == nil && t.Challenges == nil
}

// The stream: whole first, with the ingress itself and the mesh's nodes;
// an environment published with an instance is put within a second, its
// stopping instance taken out, and the unpublished environment removed; a
// new node comes as the mesh; a change of the ingress's row named on
// ferun_ingress comes as Self.
func TestSync(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.world()
	h.meshNode("n1", 9)
	env := h.mkEnv("web", webSpec, false)
	sb := h.mkInstance(env, "web", model.PhaseReady, model.StateRunning, 8080)
	_, c, hello := h.connect("in-0", 1, "10.0.0.1:51820")
	g := h.ingress("in-0")

	sy := h.sync(c)
	first := sy.next(10 * time.Second)
	if first.Seq != 1 || !first.Whole || len(first.Envs) != 0 || first.Mesh == nil || first.Self == nil {
		t.Fatalf("first %+v", first)
	}
	if *first.Self != (wire.Self{Name: "in-0", Address: hello.Address, AppsZone: testZone,
		AdmittedVersion: uint64(g.GroupVersion)}) || g.GroupVersion == 0 {
		t.Fatalf("self %+v, row %+v", first.Self, g)
	}
	// The mesh has the nodes only, not the ingress itself.
	if m := first.Mesh; len(m.Peers) != 1 || m.Peers[0].Name != "n1" || m.Version == 0 {
		t.Fatalf("mesh %+v", m)
	}

	// Published: put, with the instance as an endpoint.
	h.publish(env.ID, true)
	start := time.Now()
	put := sy.until(time.Second, "the published environment", func(t *wire.Table) bool { return len(t.Envs) == 1 })
	e := put.Envs[0]
	if e.ID != env.ID.String() || len(e.Hosts) != 1 || len(e.Services) != 1 || len(e.Services[0].Endpoints) != 1 ||
		e.Services[0].Endpoints[0].Instance != sb.String() || e.Services[0].Endpoints[0].Ports["http"] != 8080 {
		t.Fatalf("put %+v", put)
	}
	t.Logf("published to put: %v", time.Since(start))

	// Stopping: put again, without the endpoint.
	h.setPhase(sb, model.PhaseStopping)
	sy.until(time.Second, "the stopped endpoint gone", func(t *wire.Table) bool {
		return len(t.Envs) == 1 && len(t.Envs[0].Services) == 1 && len(t.Envs[0].Services[0].Endpoints) == 0
	})

	// A new node: the mesh.
	h.meshNode("n2", 8)
	m := sy.until(time.Second, "the new node", func(t *wire.Table) bool { return t.Mesh != nil }).Mesh
	if len(m.Peers) != 2 || m.Version <= first.Mesh.Version {
		t.Fatalf("mesh %+v", m)
	}

	// The ingress's row, named on ferun_ingress: Self.
	h.exec("UPDATE ingresses SET group_version = group_version + 5 WHERE name = 'in-0'")
	h.exec("SELECT pg_notify($1, $2)", db.ChanIngress, store.IngressNamePayload("in-0"))
	self := sy.until(time.Second, "the new self", func(t *wire.Table) bool { return t.Self != nil }).Self
	if self.AdmittedVersion != uint64(g.GroupVersion)+5 || self.Address != hello.Address {
		t.Fatalf("self %+v", self)
	}

	// Unpublished: removed.
	h.publish(env.ID, false)
	gone := sy.until(time.Second, "the environment removed", func(t *wire.Table) bool { return len(t.Removed) == 1 })
	if gone.Removed[0] != env.ID.String() || len(sy.state.Envs) != 0 {
		t.Fatalf("removed %+v, state %+v", gone, sy.state.Envs)
	}
}

// F13: a table larger than one message goes in parts, each of its own
// seq, the first with whole, the mesh and Self, every one but the last
// with more: the whole table, and a delta later. The receiver applies
// the parts together, and the sequence goes on from the last part.
func TestSyncTableInParts(t *testing.T) {
	h := newHarness(t, harnessOpts{partBytes: 1}) // every item in a part of its own
	h.world()
	h.meshNode("n1", 9)
	var ids []string
	for i := range 3 {
		env := h.mkEnv(fmt.Sprintf("web%d", i), webSpec, true)
		h.mkInstance(env, "web", model.PhaseReady, model.StateRunning, 8080)
		ids = append(ids, env.ID.String())
	}
	_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")

	sy := h.sync(c)
	for seq := uint64(1); seq <= 3; seq++ {
		part := sy.next(10 * time.Second)
		first := seq == 1
		if part.Seq != seq || part.More != (seq < 3) || part.Whole != first || (part.Mesh != nil) != first ||
			(part.Self != nil) != first || len(part.Envs) != 1 || len(part.Removed) != 0 {
			t.Fatalf("part %d: %+v", seq, part)
		}
		if seq < 3 && len(sy.state.Envs) != 0 {
			t.Fatalf("part %d applied alone: %d environments", seq, len(sy.state.Envs))
		}
	}
	if len(sy.state.Envs) != 3 || sy.state.Seq != 3 || sy.state.Self.Name != "in-0" || len(sy.state.Mesh.Peers) != 1 {
		t.Fatalf("state %+v", sy.state)
	}

	// Two environments unpublished in one read: a delta of two removals,
	// in two parts.
	h.exec("UPDATE environments SET public = false WHERE id = ANY($1::uuid[])", ids[:2])
	h.exec("SELECT pg_notify($1, '*')", db.ChanIngress)
	for seq := uint64(4); seq <= 5; seq++ {
		part := sy.next(2 * time.Second)
		if part.Seq != seq || part.More != (seq == 4) || part.Whole || len(part.Removed) != 1 || len(part.Envs) != 0 {
			t.Fatalf("part %d: %+v", seq, part)
		}
		if seq == 4 && len(sy.state.Envs) != 3 {
			t.Fatalf("part %d applied alone: %d environments", seq, len(sy.state.Envs))
		}
	}
	if _, ok := sy.state.Envs[ids[2]]; !ok || len(sy.state.Envs) != 1 || sy.state.Seq != 5 {
		t.Fatalf("state %+v", sy.state.Envs)
	}
}

// The heartbeat sends an empty table and records the ingress as seen.
func TestSyncHeartbeat(t *testing.T) {
	h := newHarness(t, harnessOpts{heartbeat: 200 * time.Millisecond})
	_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	h.exec("UPDATE ingresses SET last_seen = now() - interval '1 hour' WHERE name = 'in-0'")
	before := h.ingress("in-0").LastSeen
	sy := h.sync(c)
	if first := sy.next(10 * time.Second); !first.Whole {
		t.Fatalf("first %+v", first)
	}
	for seq := uint64(2); seq <= 3; seq++ {
		beat := sy.next(time.Second)
		if beat.Seq != seq || !empty(beat) || beat.SentAt.IsZero() {
			t.Fatalf("heartbeat %+v", beat)
		}
	}
	eventually(t, 2*time.Second, "seen", func() bool { return h.ingress("in-0").LastSeen.After(before.Add(time.Minute)) })
}

// Sync needs a Hello on its connection first.
func TestSyncBeforeHello(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	id, _ := h.mustJoin("", "in-0")
	err := h.sync(h.dial(id)).ended(5 * time.Second)
	wantCode(t, "Sync", err, codes.FailedPrecondition, helloFirstText)
}

// RemoveIngress ends the ingress's streams with PERMISSION_DENIED, the
// text on which it declares its identity lost.
func TestSyncRemoved(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	sy := h.sync(c)
	sy.next(10 * time.Second)
	if found, err := h.s.RemoveIngress(bg, "in-0"); err != nil || !found {
		t.Fatalf("remove: %v %v", found, err)
	}
	wantCode(t, "the stream", sy.ended(5*time.Second), codes.PermissionDenied, "ingress in-0 was removed; join it again")
	if n := len(h.s.streamsOf("", true)); n != 0 {
		t.Fatalf("%d streams left", n)
	}
}

// A removal written by another instance reaches the stream on
// ferun_ingress: it ends, refused or closed.
func TestSyncRemovedElsewhere(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	sy := h.sync(c)
	sy.next(10 * time.Second)
	if found, err := h.st.RemoveIngress(bg, "in-0"); err != nil || !found {
		t.Fatalf("remove: %v %v", found, err)
	}
	err := sy.ended(5 * time.Second)
	if code := status.Code(err); code != codes.PermissionDenied && code != codes.Unavailable {
		t.Fatalf("the stream ended with %v", err)
	}
	t.Logf("the stream ended with %v", err)
}

// BeginStopping ends every stream with UNAVAILABLE, closes new ingress
// connections at once, and has Wake and Hold answer UNAVAILABLE on the
// connections that stay.
func TestBeginStopping(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	id, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	sy := h.sync(c)
	sy.next(10 * time.Second)

	h.s.BeginStopping()
	h.s.BeginStopping() // twice is harmless
	wantCode(t, "the stream", sy.ended(5*time.Second), codes.Unavailable, stoppingText)
	_, err := c.api.Wake(bg, &wire.WakeRequest{Env: uuid.NewString(), Service: "web"})
	wantCode(t, "Wake", err, codes.Unavailable, stoppingText)
	_, err = c.api.Hold(bg, &wire.HoldRequest{})
	wantCode(t, "Hold", err, codes.Unavailable, stoppingText)
	wantCode(t, "a new stream", h.sync(c).ended(5*time.Second), codes.Unavailable, stoppingText)

	nc := h.dial(id)
	ctx, cancel := context.WithTimeout(bg, 3*time.Second)
	defer cancel()
	if _, err := nc.api.Hello(ctx, &wire.HelloRequest{PublicKey: wgKey(1), Endpoint: "10.0.0.1:51820"}); err == nil {
		t.Fatal("a Hello on a new connection succeeded")
	}
	if n := h.links.IngressConnections("in-0"); n != 1 {
		t.Fatalf("connections %d, want the established one", n)
	}
}

// A table that cannot be read for its health window ends the streams
// with UNAVAILABLE.
func TestSyncUnhealthy(t *testing.T) {
	fq := &flaky{}
	h := newHarness(t, harnessOpts{tableStore: store.On(fq), tableMod: func(b *table.Builder) {
		b.Full, b.Health = 50*time.Millisecond, 300*time.Millisecond
	}})
	fq.q = h.d.Pool
	fq.ready.Store(true)
	_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	sy := h.sync(c)
	sy.next(10 * time.Second)
	fq.down.Store(true)
	wantCode(t, "the stream", sy.ended(5*time.Second), codes.Unavailable, unhealthyText)
}

// lockIngresses holds ACCESS EXCLUSIVE on the table ingresses, so that
// every statement on it waits, until the returned function (or the
// test's end) releases it.
func (h *harness) lockIngresses() (unlock func()) {
	h.t.Helper()
	tx, err := h.d.Pool.Begin(bg)
	if err != nil {
		h.t.Fatal(err)
	}
	var once sync.Once
	unlock = func() { once.Do(func() { _ = tx.Rollback(bg) }) }
	h.t.Cleanup(unlock)
	if _, err := tx.Exec(bg, "LOCK TABLE ingresses IN ACCESS EXCLUSIVE MODE"); err != nil {
		h.t.Fatal(err)
	}
	return unlock
}

// A database that stalls longer than the row's timeout while the table
// stays healthy keeps the stream (D18: only the table's health ends
// streams): the heartbeats go on with the last Self, and once the row
// can be read again a change of it comes as Self.
func TestSyncRowStalls(t *testing.T) {
	h := newHarness(t, harnessOpts{heartbeat: 100 * time.Millisecond, dbTimeout: 200 * time.Millisecond})
	_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	g := h.ingress("in-0")
	sy := h.sync(c)
	if first := sy.next(10 * time.Second); !first.Whole {
		t.Fatalf("first %+v", first)
	}

	unlock := h.lockIngresses()
	// Each heartbeat now waits out SeenIngress and the read of the row.
	for stalled := time.Now().Add(1500 * time.Millisecond); time.Now().Before(stalled); {
		if beat := sy.next(2 * time.Second); !empty(beat) {
			t.Fatalf("heartbeat %+v", beat)
		}
	}
	if cur := h.b.Current(); cur == nil || !cur.Healthy {
		t.Fatalf("the table is not healthy: %+v", cur)
	}
	unlock()

	h.exec("UPDATE ingresses SET group_version = group_version + 5 WHERE name = 'in-0'")
	h.exec("SELECT pg_notify($1, $2)", db.ChanIngress, store.IngressNamePayload("in-0"))
	self := sy.until(2*time.Second, "the new self", func(t *wire.Table) bool { return t.Self != nil }).Self
	if self.AdmittedVersion != uint64(g.GroupVersion)+5 {
		t.Fatalf("self %+v", self)
	}
}

// dbError answers a failed step UNAVAILABLE while the call goes on, its
// own timeout included, and the call's own end as such.
func TestDBError(t *testing.T) {
	step, cancel := context.WithTimeout(bg, time.Nanosecond)
	defer cancel()
	<-step.Done()
	timedOut := fmt.Errorf("timeout: %w", step.Err())
	ended, end := context.WithCancel(bg)
	end()
	expired, cancelExpired := context.WithTimeout(bg, time.Nanosecond)
	defer cancelExpired()
	<-expired.Done()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		code codes.Code
	}{
		{"the step's timeout", bg, timedOut, codes.Unavailable},
		{"the database", bg, errDown, codes.Unavailable},
		{"a status", bg, status.Error(codes.PermissionDenied, "no"), codes.PermissionDenied},
		{"the call cancelled", ended, fmt.Errorf("x: %w", context.Canceled), codes.Canceled},
		{"the call's deadline", expired, timedOut, codes.DeadlineExceeded},
	} {
		if got := status.Code(dbError(tc.ctx, tc.err, "step")); got != tc.code {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.code)
		}
	}
	if err := dbError(bg, nil, "step"); err != nil {
		t.Errorf("nil: %v", err)
	}
}

// errDown is the error of a database that cannot be reached.
var errDown = errors.New("the database cannot be reached")

// flaky runs statements on q once ready; while down is set, each fails
// with errDown.
type flaky struct {
	q     db.Querier
	ready atomic.Bool
	down  atomic.Bool
}

func (f *flaky) failing() bool { return !f.ready.Load() || f.down.Load() }

func (f *flaky) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if f.failing() {
		return pgconn.CommandTag{}, errDown
	}
	return f.q.Exec(ctx, sql, args...)
}

func (f *flaky) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if f.failing() {
		return nil, errDown
	}
	return f.q.Query(ctx, sql, args...)
}

func (f *flaky) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if f.failing() {
		return downRow{}
	}
	return f.q.QueryRow(ctx, sql, args...)
}

type downRow struct{}

func (downRow) Scan(...any) error { return errDown }
