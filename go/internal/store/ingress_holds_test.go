// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// mkService makes a service instance of env (its sandbox row and its
// env_instances row), running on node, last active 10 minutes ago, with
// an idle interval of a minute.
func mkService(t *testing.T, s *store.Store, env store.Environment, node string) store.Sandbox {
	t.Helper()
	minute := 60
	sb := mkInstance(t, s, env, func(n *store.NewSandbox) { n.IdleAfterS = &minute })
	if _, err := s.InsertEnvInstance(bg, store.EnvInstance{SandboxID: sb.ID, EnvID: env.ID, Kind: "service", Object: "web",
		TemplateHash: "h1", Deployment: 1}); err != nil {
		t.Fatal(err)
	}
	place(t, s, sb.ID, node, model.StateRunning)
	exec(t, s, "UPDATE sandboxes SET last_active_at = now() - interval '10 minutes' WHERE id = $1", sb.ID)
	return get(t, s, sb.ID)
}

// mkMeshedIngress inserts a joined ingress instance with a mesh address, last
// seen ago.
func mkMeshedIngress(t *testing.T, s *store.Store, name string, subnet int, ago time.Duration) {
	t.Helper()
	exec(t, s, `INSERT INTO ingresses (name, mesh_subnet, mesh_prefix, mesh_address, last_seen)
		VALUES ($1, $2, $3, $4, now() - $5::interval)`, name, subnet, fmt.Sprintf("fd00:0:0:%x::/64", subnet),
		fmt.Sprintf("fd00:0:0:%x::1", subnet), fmt.Sprintf("%d microseconds", ago.Microseconds()))
}

func admit(t *testing.T, s *store.Store, id podid.ID, ingress string) string {
	t.Helper()
	refusal, err := s.AdmitIngressHold(bg, id, store.IngressHolder(ingress), "i1", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return refusal
}

func TestIngressHolds(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkNode(t, s, "n1")
	env := mkEnv(t, s, "acme", "stage")
	publish(t, s, env.ID, true)
	sb := mkService(t, s, env, "n1")
	if store.IngressHolder("x") != "ingress:x" {
		t.Fatalf("holder %s", store.IngressHolder("x"))
	}
	refusals := []string{store.IngressHoldGone, store.IngressHoldNotPublic, store.IngressHoldNotRunning, store.IngressHoldBusy}
	if !slices.Equal(refusals, []string{"gone", "not public", "not running", "busy"}) {
		t.Fatalf("refusals %q", refusals)
	}

	// Admitted: a forward hold of ingress:x, renewed by i1, for 30 s; the
	// sandbox was active now.
	if r := admit(t, s, sb.ID, "x"); r != "" {
		t.Fatalf("refused: %s", r)
	}
	now, err := s.Now(bg)
	must(t, err)
	holds, err := s.ListHolds(bg, sb.ID)
	if err != nil || len(holds) != 1 || holds[0].Holder != "ingress:x" || holds[0].Kind != model.HoldForward ||
		holds[0].InstanceID != "i1" || holds[0].LeaseUntil.Sub(now) < 25*time.Second || holds[0].LeaseUntil.Sub(now) > 31*time.Second {
		t.Fatalf("holds %+v %v", holds, err)
	}
	if got := get(t, s, sb.ID); got.LastActiveAt.Before(now.Add(-time.Minute)) {
		t.Fatalf("last_active_at %v, now %v", got.LastActiveAt, now)
	}
	// Admitting again renews the one hold.
	if r := admit(t, s, sb.ID, "x"); r != "" {
		t.Fatalf("renewal refused: %s", r)
	}
	if holds, _ := s.ListHolds(bg, sb.ID); len(holds) != 1 {
		t.Fatalf("renewed %+v", holds)
	}

	// Refusals: gone; not public (a private environment's service, a
	// public one's job, a plain sandbox, a deleting environment's
	// service); not running; busy.
	if r := admit(t, s, podid.Mint(), "x"); r != "gone" {
		t.Fatalf("gone: %q", r)
	}
	private := mkService(t, s, mkEnv(t, s, "acme", "private"), "n1")
	job := mkInstance(t, s, env)
	if _, err := s.InsertEnvInstance(bg, store.EnvInstance{SandboxID: job.ID, EnvID: env.ID, Kind: "job", Object: "migrate",
		TemplateHash: "j1", Deployment: 1}); err != nil {
		t.Fatal(err)
	}
	place(t, s, job.ID, "n1", model.StateRunning)
	plain := mkSandbox(t, s, "acme")
	place(t, s, plain.ID, "n1", model.StateRunning)
	for _, id := range []podid.ID{private.ID, job.ID, plain.ID} {
		if r := admit(t, s, id, "x"); r != "not public" {
			t.Fatalf("%s: %q", id, r)
		}
	}
	doomed := mkEnv(t, s, "acme", "doomed")
	publish(t, s, doomed.ID, true)
	dsb := mkService(t, s, doomed, "n1")
	cur, _ := s.GetEnvironment(bg, doomed.ID)
	after := cur
	after.Deleting = true
	if _, err := s.UpdateEnvironment(bg, cur, after); err != nil {
		t.Fatal(err)
	}
	if r := admit(t, s, dsb.ID, "x"); r != store.IngressHoldNotPublic {
		t.Fatalf("deleting: %q", r)
	}
	paused := mkService(t, s, env, "n1")
	place(t, s, paused.ID, "n1", model.StatePaused)
	if r := admit(t, s, paused.ID, "x"); r != "not running" {
		t.Fatalf("paused: %q", r)
	}
	busy := mkService(t, s, env, "n1")
	startOp(t, s, busy.ID, "pause", "i2", model.StatePaused, time.Minute)
	if r := admit(t, s, busy.ID, "x"); r != "busy" {
		t.Fatalf("busy: %q", r)
	}
	for _, id := range []podid.ID{private.ID, job.ID, plain.ID, dsb.ID, paused.ID, busy.ID} {
		if holds, _ := s.ListHolds(bg, id); len(holds) != 0 {
			t.Fatalf("a refused hold was written on %s: %+v", id, holds)
		}
	}

	// Release deletes the hold and touches the sandbox; a second release
	// finds none.
	exec(t, s, "UPDATE sandboxes SET last_active_at = now() - interval '10 minutes' WHERE id = $1", sb.ID)
	if ok, err := s.ReleaseIngressHold(bg, sb.ID, store.IngressHolder("x")); err != nil || !ok {
		t.Fatalf("release %v %v", ok, err)
	}
	now, _ = s.Now(bg)
	if holds, _ := s.ListHolds(bg, sb.ID); len(holds) != 0 {
		t.Fatalf("released %+v", holds)
	}
	if got := get(t, s, sb.ID); got.LastActiveAt.Before(now.Add(-time.Minute)) {
		t.Fatalf("last_active_at after the release %v", got.LastActiveAt)
	}
	exec(t, s, "UPDATE sandboxes SET last_active_at = now() - interval '10 minutes' WHERE id = $1", sb.ID)
	if ok, err := s.ReleaseIngressHold(bg, sb.ID, store.IngressHolder("x")); err != nil || ok {
		t.Fatalf("release again %v %v", ok, err)
	}
	if got := get(t, s, sb.ID); !got.LastActiveAt.Before(now.Add(-time.Minute)) {
		t.Fatalf("touched without a hold %v", got.LastActiveAt)
	}

	// LeaveIngress releases every hold of the ingress, touches their
	// sandboxes and sets left_at; another holder's hold stays.
	mkMeshedIngress(t, s, "x", 1, 0)
	second := mkService(t, s, env, "n1")
	for _, id := range []podid.ID{sb.ID, second.ID} {
		if r := admit(t, s, id, "x"); r != "" {
			t.Fatalf("admit %s: %s", id, r)
		}
		exec(t, s, "UPDATE sandboxes SET last_active_at = now() - interval '10 minutes' WHERE id = $1", id)
	}
	if r := admit(t, s, sb.ID, "y"); r != "" {
		t.Fatalf("admit y: %s", r)
	}
	must(t, s.LeaveIngress(bg, "x"))
	now, _ = s.Now(bg)
	for _, id := range []podid.ID{sb.ID, second.ID} {
		holds, _ := s.ListHolds(bg, id)
		if slices.ContainsFunc(holds, func(h store.Hold) bool { return h.Holder == "ingress:x" }) {
			t.Fatalf("left %s: %+v", id, holds)
		}
		if got := get(t, s, id); got.LastActiveAt.Before(now.Add(-time.Minute)) {
			t.Fatalf("last_active_at after leaving %v", got.LastActiveAt)
		}
	}
	if holds, _ := s.ListHolds(bg, sb.ID); len(holds) != 1 || holds[0].Holder != "ingress:y" {
		t.Fatalf("another ingress's hold %+v", holds)
	}
	var left bool
	must(t, s.Q().QueryRow(bg, "SELECT left_at IS NOT NULL FROM ingresses WHERE name = 'x'").Scan(&left))
	if !left {
		t.Fatal("left_at not set")
	}
	// Leaving an ingress without a row or holds is no error.
	must(t, s.LeaveIngress(bg, "nobody"))
}

// The idle fence: while an ingress that is joined, has an address and has
// not left was last seen 45 s to 15 min ago, a service instance of a
// public environment does not idle; other sandboxes do, and so does it
// once the ingress was seen recently, long ago, or left.
func TestIngressFence(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkNode(t, s, "n1")
	public := mkEnv(t, s, "acme", "stage")
	publish(t, s, public.ID, true)
	fenced := mkService(t, s, public, "n1")
	private := mkService(t, s, mkEnv(t, s, "acme", "private"), "n1")
	minute := 60
	plain := mkSandbox(t, s, "acme")
	exec(t, s, "UPDATE sandboxes SET idle_after_s = $2 WHERE id = $1", plain.ID, minute)
	place(t, s, plain.ID, "n1", model.StateRunning)
	exec(t, s, "UPDATE sandboxes SET last_active_at = now() - interval '10 minutes' WHERE id = $1", plain.ID)

	idle := func() []podid.ID {
		t.Helper()
		rows, err := s.IdleSandboxes(bg, 0, []string{"n1"}, 10)
		must(t, err)
		var out []podid.ID
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}
	recheck := func(id podid.ID) bool {
		t.Helper()
		ok, err := s.IdleRecheck(bg, id, 0)
		must(t, err)
		return ok
	}
	open := func(what string) {
		t.Helper()
		if got := idle(); len(got) != 3 || !slices.Contains(got, fenced.ID) {
			t.Fatalf("%s: idle %v", what, got)
		}
		if !recheck(fenced.ID) || !recheck(private.ID) || !recheck(plain.ID) {
			t.Fatalf("%s: recheck", what)
		}
	}
	closed := func(what string) {
		t.Helper()
		if got := idle(); len(got) != 2 || slices.Contains(got, fenced.ID) {
			t.Fatalf("%s: idle %v", what, got)
		}
		if recheck(fenced.ID) || !recheck(private.ID) || !recheck(plain.ID) {
			t.Fatalf("%s: recheck", what)
		}
	}

	open("no ingress")
	mkMeshedIngress(t, s, "g1", 1, 60*time.Second)
	closed("seen 60 s ago")
	exec(t, s, "UPDATE ingresses SET last_seen = now() - interval '10 seconds' WHERE name = 'g1'")
	open("seen 10 s ago")
	exec(t, s, "UPDATE ingresses SET last_seen = now() - interval '20 minutes' WHERE name = 'g1'")
	open("seen 20 min ago")
	exec(t, s, "UPDATE ingresses SET last_seen = now() - interval '60 seconds', left_at = now() WHERE name = 'g1'")
	open("left")
	exec(t, s, "UPDATE ingresses SET left_at = NULL, status = 'removed', removed_at = now() WHERE name = 'g1'")
	open("removed")
	exec(t, s, "UPDATE ingresses SET status = 'joined', removed_at = NULL WHERE name = 'g1'")
	closed("joined again")
	// Never seen, or without an address: no fence.
	exec(t, s, "UPDATE ingresses SET last_seen = NULL WHERE name = 'g1'")
	open("never seen")
	exec(t, s, `INSERT INTO ingresses (name, last_seen) VALUES ('g2', now() - interval '60 seconds')`)
	open("no address")
	exec(t, s, "UPDATE ingresses SET last_seen = now() - interval '60 seconds' WHERE name = 'g1'")
	closed("fenced again")
	// Unpublished, the environment's services idle.
	publish(t, s, public.ID, false)
	open("not public")
}
