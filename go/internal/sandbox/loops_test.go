// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/leader"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

func TestLoopsList(t *testing.T) {
	h := newHarness(t)
	names := func(ls []leader.Loop) map[string]time.Duration {
		m := map[string]time.Duration{}
		for _, l := range ls {
			m[l.Name] = l.Interval
		}
		return m
	}
	got := names(h.eng().Loops())
	// The idle pause runs without --idle-pause too: rows idle after their
	// own idle_after_s.
	if got[LoopIdlePause] != time.Second {
		t.Fatal("no idle pause without --idle-pause")
	}
	if _, ok := got[LoopHibernateAfter]; ok {
		t.Fatal("hibernate-after without --hibernate-after")
	}
	want := map[string]time.Duration{LoopDownSweep: 5 * time.Second, LoopLostGrace: 30 * time.Second, LoopDrain: 5 * time.Second,
		LoopTombstones: 10 * time.Second, LoopStaleOps: 10 * time.Second, LoopUnknownSweep: 15 * time.Second,
		LoopRepair: 30 * time.Second, LoopPendingDescribe: 30 * time.Second, LoopProcessHolds: time.Minute,
		LoopExpiredHolds: time.Minute, LoopProjectDeletion: time.Minute}
	for n, iv := range want {
		if got[n] != iv {
			t.Errorf("loop %s: %v, want %v", n, got[n], iv)
		}
	}
	h.eng().cfg.IdlePause, h.eng().cfg.HibernateAfter = time.Second, time.Second
	got = names(h.eng().Loops())
	if got[LoopIdlePause] != time.Second || got[LoopHibernateAfter] != 5*time.Second {
		t.Fatalf("loops %v", got)
	}
	// The hints of ferun_leader wake them by name.
	for _, hint := range []string{"down", "drain", "tombstone", "project"} {
		found := false
		for n := range got {
			found = found || strings.Contains(n, hint)
		}
		if !found {
			t.Errorf("no loop for the hint %q", hint)
		}
	}
}

func TestDownSweepAndLostGrace(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.LostGrace = time.Minute })
	n := h.addNode("n1")
	e := h.eng()
	running := h.running(512)
	paused := h.running(512)
	h.startOp(paused.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	withCheckpoint := h.running(512)
	h.startOp(withCheckpoint.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	h.startOp(withCheckpoint.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1"})
	h.ensure(withCheckpoint.ID)
	pending := h.sandbox(512)
	for _, id := range []podid.ID{running.ID, paused.ID, withCheckpoint.ID} {
		h.settle(id)
	}
	// The node expired (fleet's node expiry), and the leader crashed
	// before its sweep: the next leader's sweep still sweeps.
	n.Partition(true)
	eventually(t, 10*time.Second, "n1 disconnected", func() bool { return !h.ins[0].srv.Connected("n1") })
	h.exec("UPDATE nodes SET status = 'down', down_since = now(), status_description = 'heartbeat expired' WHERE name = 'n1'")
	if err := e.downSweep(bg); err != nil {
		t.Fatal(err)
	}
	for _, id := range []podid.ID{running.ID, paused.ID, withCheckpoint.ID} {
		row := h.row(id)
		wantState(t, row, model.StateUnknown)
		if !strings.HasPrefix(row.StatusDescription, "node n1 down since ") || row.Node != "n1" {
			t.Fatalf("swept %+v", row)
		}
	}
	if !strings.HasSuffix(h.row(paused.ID).StatusDescription, "; it was paused") {
		t.Fatalf("description %q", h.row(paused.ID).StatusDescription)
	}
	wantState(t, h.row(pending.ID), model.StatePending)
	// Within the grace, nothing more.
	if err := e.lostGrace(bg); err != nil {
		t.Fatal(err)
	}
	wantState(t, h.row(running.ID), model.StateUnknown)
	h.exec("UPDATE nodes SET down_since = now() - interval '2 minutes' WHERE name = 'n1'")
	if err := e.lostGrace(bg); err != nil {
		t.Fatal(err)
	}
	r := h.row(running.ID)
	wantState(t, r, model.StateLost)
	if r.LostFrom != "n1" || r.Node != "n1" {
		t.Fatalf("lost %+v", r)
	}
	c := h.row(withCheckpoint.ID)
	wantState(t, c, model.StateHibernated)
	if c.LostFrom != "n1" || !strings.Contains(c.StatusDescription, "restored from the checkpoint of") {
		t.Fatalf("hibernated by lost grace %+v", c)
	}
	// It is restored elsewhere on its next use; the node it was lost with
	// gets a tombstone for the pod it may still run.
	h.addNode("n2")
	lease := h.ensure(withCheckpoint.ID)
	if lease.Node != "n2" {
		t.Fatalf("restored on %s", lease.Node)
	}
	ts := h.tombstones(withCheckpoint.ID)
	if len(ts) != 1 || ts[0].Node != "n1" || ts[0].Incarnation != c.Incarnation {
		t.Fatalf("tombstones after the restore elsewhere %+v", ts)
	}
	// A removed node is past the grace at once.
	h.exec("UPDATE nodes SET status = 'removed', removed_at = now(), down_since = NULL WHERE name = 'n1'")
	h.exec("UPDATE sandboxes SET client_state = 'unknown', lost_from = NULL WHERE id = $1", running.ID)
	if err := e.lostGrace(bg); err != nil {
		t.Fatal(err)
	}
	wantState(t, h.row(running.ID), model.StateLost)
}

// A first create whose node went down with its outcome leaves a sandbox
// whose pod was never seen: lost grace makes it pending, not lost, and its
// next use creates it elsewhere. The pod the node holds when it returns
// belongs to no row there, and is tombstoned.
func TestLostGraceNeverSeen(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.LostGrace = time.Minute })
	n1 := h.addNode("n1")
	e := h.eng()
	sb := h.sandbox(512)
	h.exec(`UPDATE sandboxes SET client_state = 'unknown', node = 'n1', image_ref = $2, gen = gen + 1,
		status_description = 'create on node n1: outcome unknown (the node did not answer)' WHERE id = $1`, sb.ID, testImage)
	row := h.row(sb.ID)
	if row.Materialised {
		t.Fatal("materialised")
	}
	// The create made the pod after all; n1 goes down before a sync of
	// the pod's events sees it (it would make the row creating).
	releaseReads := h.holdPodReads("n1")
	if _, err := h.pods("n1").CreatePod(bg, &podv1.CreatePodRequest{PodId: sb.ID.String(), Spec: podSpec(row)}); err != nil {
		t.Fatal(err)
	}
	n1.Partition(true)
	eventually(t, 10*time.Second, "n1 disconnected", func() bool { return !h.ins[0].srv.Connected("n1") })
	releaseReads()
	h.exec("UPDATE nodes SET status = 'down', down_since = now() - interval '2 minutes' WHERE name = 'n1'")
	if err := e.lostGrace(bg); err != nil {
		t.Fatal(err)
	}
	r := h.row(sb.ID)
	wantState(t, r, model.StatePending)
	if r.Node != "" || r.LostFrom != "" || !strings.HasSuffix(r.StatusDescription, "; its pod was never seen there; created on the next use") {
		t.Fatalf("after lost grace %+v", r)
	}
	h.activityNamed(sb.ID, "sandbox_state", "unknown → pending")
	// Its next use creates it on another node.
	h.addNode("n2")
	if lease := h.ensure(sb.ID); lease.Node != "n2" {
		t.Fatalf("created on %s", lease.Node)
	}
	// n1 returns with the pod: it is tombstoned and deleted.
	n1.Partition(false)
	eventually(t, 10*time.Second, "n1 back", func() bool { return h.ins[0].srv.Connected("n1") })
	if err := e.ReconcileNode(bg, "n1"); err != nil {
		t.Fatal(err)
	}
	h.noPod("n1", sb.ID)
	wantState(t, h.row(sb.ID), model.StateRunning)
}

func TestIdlePauseLoop(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.IdlePause = 50 * time.Millisecond })
	h.addNode("n1")
	e := h.eng()
	idle := h.running(512)
	busy := h.running(512)
	h.settle(idle.ID)
	h.settle(busy.ID)
	_, release, err := e.Acquire(bg, busy.ID, model.HoldExec)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := e.idlePause(bg); err != nil {
		t.Fatal(err)
	}
	row := h.waitState(idle.ID, model.StatePaused, 10*time.Second)
	if row.RunID != "" {
		t.Fatal("the run stayed open")
	}
	eventually(t, 10*time.Second, "the run closed with the reason idle", func() bool {
		var c int
		h.scalar(&c, "SELECT count(*) FROM activity WHERE sandbox_id = $1 AND kind = 'run_end' AND detail->>'reason' = 'idle'", idle.ID)
		return c == 1
	})
	wantState(t, h.row(busy.ID), model.StateRunning)
	// The exec ends; the next pass pauses it.
	release()
	time.Sleep(100 * time.Millisecond)
	if err := e.idlePause(bg); err != nil {
		t.Fatal(err)
	}
	h.waitState(busy.ID, model.StatePaused, 10*time.Second)
}

// TestLoopsWaitAfterOutage: idle pause, stale ops and expired holds act
// only once the leader has led for the outage guard and the database has
// not failed within it (as fleet's node expiry), so that after an outage
// every live instance renews its holds and its heartbeat first.
func TestLoopsWaitAfterOutage(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.IdlePause = 50 * time.Millisecond })
	h.addNode("n1")
	e := h.eng()
	e.t.outageGuard = time.Hour
	idle := h.running(512)
	h.settle(idle.ID)
	// The hold of an exec in flight, expired during the outage; its
	// instance renews it once the database answers.
	h.liveInstance("other")
	h.exec(`INSERT INTO sandbox_holds (sandbox_id, holder, kind, instance_id, lease_until)
		VALUES ($1, 'other:exec', 'exec', 'other', now() - interval '1 second')`, idle.ID)
	// An op of an instance whose heartbeat went stale during the outage.
	h.deadInstance("dead")
	stale := h.opOfDeadOwner(h.running(512), OpPause, "dead")
	time.Sleep(100 * time.Millisecond)

	unsettled := func(what string) {
		t.Helper()
		if e.settled() {
			t.Fatalf("%s: settled", what)
		}
		for name, loop := range map[string]func(context.Context) error{
			"idle pause": e.idlePause, "stale ops": e.staleOps, "expired holds": e.expiredHolds,
		} {
			if err := loop(bg); err != nil {
				t.Fatalf("%s: %s: %v", what, name, err)
			}
		}
		if row := h.row(idle.ID); row.ClientState != model.StateRunning || row.Op != "" {
			t.Fatalf("%s: the idle sandbox was paused: %+v", what, row)
		}
		if hs := h.holds(idle.ID); len(hs) != 1 {
			t.Fatalf("%s: the expired hold went: %+v", what, hs)
		}
		if row := h.row(stale.ID); row.Op != string(OpPause) || row.OpOwner != "dead" || row.OpID != stale.OpID {
			t.Fatalf("%s: the stale op was touched: %+v", what, row)
		}
	}
	// Not leading.
	e.leadingSince = func() time.Time { return time.Time{} }
	unsettled("not leading")
	// Leading for less than the guard: after a stall or a failover.
	e.leadingSince = func() time.Time { return time.Now() }
	unsettled("leading since now")
	// Leading for long, but the database failed within the guard.
	e.leadingSince = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	e.lastFailure = func() time.Time { return time.Now() }
	unsettled("a failure now")

	// Settled: the loops act.
	e.lastFailure = func() time.Time { return time.Time{} }
	if !e.settled() {
		t.Fatal("not settled")
	}
	if err := e.expiredHolds(bg); err != nil {
		t.Fatal(err)
	}
	if hs := h.holds(idle.ID); len(hs) != 0 {
		t.Fatalf("expired hold kept once settled: %+v", hs)
	}
	if err := e.idlePause(bg); err != nil {
		t.Fatal(err)
	}
	h.waitState(idle.ID, model.StatePaused, 10*time.Second)
	if err := e.staleOps(bg); err != nil {
		t.Fatal(err)
	}
	h.waitState(stale.ID, model.StatePaused, 10*time.Second)
}

func TestHibernateAfterLoop(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.HibernateAfter = time.Minute })
	h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	// A paused sandbox on a node without an object store stays paused.
	h.addNode("bare", func(c *testnode.Config) { c.ObjectStore = nil })
	h.exec("UPDATE nodes SET draining = true WHERE name = 'n1'")
	other := h.running(512)
	h.exec("UPDATE nodes SET draining = false WHERE name = 'n1'")
	if other.Node != "bare" {
		t.Fatalf("placed on %s", other.Node)
	}
	h.startOp(other.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "bare"})
	// Paused recently: not yet.
	if err := e.hibernateAfter(bg); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	wantState(t, h.row(sb.ID), model.StatePaused)
	h.exec("UPDATE sandboxes SET paused_at = now() - interval '2 minutes' WHERE id = ANY($1)", []podid.ID{sb.ID, other.ID})
	if err := e.hibernateAfter(bg); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StateHibernated, 10*time.Second)
	time.Sleep(200 * time.Millisecond)
	wantState(t, h.row(other.ID), model.StatePaused)
}

// A policy's op that fails backs its policy off: the idle pause and the
// hibernate-after loops begin none for the sandbox before
// policy_not_before, 30 s after the first failure and doubling per
// consecutive one, instead of asking a node that cannot snapshot or
// upload again on their next pass; a pause or hibernation that succeeds
// clears it, and so does a move to another node.
func TestPolicyBackoff(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.IdlePause = 50 * time.Millisecond
		c.HibernateAfter = time.Minute
	})
	n := h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	h.settle(sb.ID)
	var suspends, hibernates atomic.Int64
	failSuspend := func(context.Context, string, any) error {
		suspends.Add(1)
		return status.Error(codes.Internal, "snapshot failed: no space left on device")
	}
	n.Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, failSuspend)
	// wantBackoff waits for the op's end and checks the sandbox's state and
	// backoff (the finish's clock is the row's updated_at).
	wantBackoff := func(state, policy string, failures int, backoff time.Duration) {
		t.Helper()
		eventually(t, 10*time.Second, "the op ended", func() bool { return h.row(sb.ID).Op == "" })
		r := h.row(sb.ID)
		wantState(t, r, state)
		if r.PolicyFailed != policy || r.PolicyFailures != failures || (backoff == 0) != r.PolicyNotBefore.IsZero() ||
			(backoff > 0 && r.PolicyNotBefore.Sub(r.UpdatedAt) != backoff) {
			t.Fatalf("%q: %d failures, not before %v (updated %v); want %q: %d, %v", r.PolicyFailed, r.PolicyFailures,
				r.PolicyNotBefore, r.UpdatedAt, policy, failures, backoff)
		}
	}
	pass := func(loop func(context.Context) error) {
		t.Helper()
		if err := loop(bg); err != nil {
			t.Fatal(err)
		}
	}
	pastBackoff := func() {
		h.exec("UPDATE sandboxes SET policy_not_before = now() - interval '1 second' WHERE id = $1", sb.ID)
	}

	// The idle pause fails: backed off for 30 s; the next pass leaves it.
	time.Sleep(100 * time.Millisecond)
	pass(e.idlePause)
	wantBackoff(model.StateRunning, store.PolicyIdle, 1, 30*time.Second)
	pass(e.idlePause)
	if r := h.row(sb.ID); r.Op != "" || suspends.Load() != 1 {
		t.Fatalf("the next pass began a pause: %d SuspendPod, op %q", suspends.Load(), r.Op)
	}
	// Past the backoff, it fails again: twice as long.
	pastBackoff()
	pass(e.idlePause)
	wantBackoff(model.StateRunning, store.PolicyIdle, 2, time.Minute)
	if suspends.Load() != 2 {
		t.Fatalf("%d SuspendPod", suspends.Load())
	}
	// It succeeds: cleared.
	n.Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, nil)
	pastBackoff()
	pass(e.idlePause)
	wantBackoff(model.StatePaused, "", 0, 0)

	// The hibernate-after fails: backed off; the next pass leaves it; past
	// the backoff it fails again.
	n.Script().SetHook(podv1.PodService_HibernatePod_FullMethodName, func(context.Context, string, any) error {
		hibernates.Add(1)
		return status.Error(codes.Unavailable, "upload failed: quota exceeded")
	})
	h.exec("UPDATE sandboxes SET paused_at = now() - interval '2 minutes' WHERE id = $1", sb.ID)
	pass(e.hibernateAfter)
	wantBackoff(model.StatePaused, store.PolicyHibernateAfter, 1, 30*time.Second)
	pass(e.hibernateAfter)
	if r := h.row(sb.ID); r.Op != "" || hibernates.Load() != 1 {
		t.Fatalf("the next pass began a hibernation: %d HibernatePod, op %q", hibernates.Load(), r.Op)
	}
	pastBackoff()
	pass(e.hibernateAfter)
	wantBackoff(model.StatePaused, store.PolicyHibernateAfter, 2, time.Minute)

	// Resumed, it idles: the hibernate-after's backoff does not hold the
	// idle pause back, whose failures count from 1.
	if res := h.startOp(sb.ID, OpIntent{Kind: OpResume, Trigger: TriggerExplicit, Target: "n1"}); res.State != model.StateRunning {
		t.Fatalf("resume: %+v", res)
	}
	if r := h.row(sb.ID); r.PolicyFailed != store.PolicyHibernateAfter || r.PolicyFailures != 2 {
		t.Fatalf("the resume changed the hibernate-after's backoff: %q %d", r.PolicyFailed, r.PolicyFailures)
	}
	n.Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, failSuspend)
	time.Sleep(100 * time.Millisecond)
	pass(e.idlePause)
	wantBackoff(model.StateRunning, store.PolicyIdle, 1, 30*time.Second)
	if suspends.Load() != 3 {
		t.Fatalf("%d SuspendPod", suspends.Load())
	}
	n.Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, nil)
	pastBackoff()
	pass(e.idlePause)
	wantBackoff(model.StatePaused, "", 0, 0)
	n.Script().SetHook(podv1.PodService_HibernatePod_FullMethodName, nil)
	h.exec("UPDATE sandboxes SET paused_at = now() - interval '2 minutes' WHERE id = $1", sb.ID)
	pass(e.hibernateAfter)
	wantBackoff(model.StateHibernated, "", 0, 0)

	// A move to another node clears a backoff (here: a restore of a row
	// whose node went).
	h.exec(`UPDATE sandboxes SET node = NULL, policy_failed = 'hibernate_after', policy_failures = 3,
		policy_not_before = now() + interval '1 hour' WHERE id = $1`, sb.ID)
	h.ensure(sb.ID)
	if r := h.row(sb.ID); r.PolicyFailed != "" || r.PolicyFailures != 0 || !r.PolicyNotBefore.IsZero() {
		t.Fatalf("after the restore: %q %d failures, not before %v", r.PolicyFailed, r.PolicyFailures, r.PolicyNotBefore)
	}
}

func TestDrainLoop(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.DrainTimeout = time.Hour })
	h.addNode("n1")
	e := h.eng()
	free := h.running(512)
	held := h.running(512)
	viewed := h.running(512)
	stopped := h.running(512)
	paused := h.running(512)
	h.startOp(paused.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	if _, err := h.pods("n1").StopPod(bg, &podv1.StopPodRequest{PodId: stopped.ID.String()}); err != nil {
		t.Fatal(err)
	}
	h.waitState(stopped.ID, model.StateStopped, 10*time.Second)
	_, release, err := e.Acquire(bg, held.ID, model.HoldExec)
	if err != nil {
		t.Fatal(err)
	}
	// An open WebSocket of a preview: the port door closes it once the
	// sandbox leaves running.
	_, releaseForward, err := e.Acquire(bg, viewed.ID, model.HoldForward)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseForward()
	if err := e.HoldProcess(bg, free.ID, "p", 0); err != nil {
		t.Fatal(err)
	}
	h.exec("UPDATE nodes SET draining = true, drain_started_at = now() WHERE name = 'n1'")
	// Everything without an exec hold goes to object storage (forward and
	// process holds do not keep a drain away); the one whose exec runs
	// stays, within the drain timeout. Without another node, what ran
	// stays hibernated.
	unheld := []podid.ID{free.ID, viewed.ID, stopped.ID, paused.ID}
	h.drainUntil("the sandboxes without an exec hibernated", func() bool {
		for _, id := range unheld {
			if row := h.row(id); row.ClientState != model.StateHibernated || row.Op != "" {
				return false
			}
		}
		return true
	})
	for _, id := range unheld {
		if row := h.row(id); row.CheckpointHead == "" {
			t.Fatalf("no checkpoint %+v", row)
		}
	}
	if !h.row(stopped.ID).CheckpointMemory == false {
		t.Fatal("a stopped pod's checkpoint holds memory")
	}
	wantState(t, h.row(held.ID), model.StateRunning)
	h.drainUntil("the node described", func() bool {
		nd, _ := h.st.GetNode(bg, "n1")
		return nd.StatusDescription == "draining: 1 remain"
	})
	// A new use of a hibernated sandbox restores it elsewhere.
	h.addNode("n2")
	if lease := h.ensure(free.ID); lease.Node != "n2" {
		t.Fatalf("restored on %s", lease.Node)
	}
	// Past the drain timeout, the execs that still run are cancelled and
	// the sandbox goes regardless of its holds; as it ran, it is restored
	// on another node at once.
	cancels, unsubscribe := h.d.Subscribe(db.ChanCancel)
	defer unsubscribe()
	h.exec("UPDATE nodes SET drain_started_at = now() - interval '2 hours' WHERE name = 'n1'")
	var cancel string
	h.drainUntil("a ferun_cancel past the drain timeout", func() bool {
		select {
		case cancel = <-cancels:
			return true
		default:
			return false
		}
	})
	var c struct{ Kind, Sandbox string }
	if err := json.Unmarshal([]byte(cancel), &c); err != nil || c.Kind != "exec" || c.Sandbox != held.ID.String() {
		t.Fatalf("ferun_cancel %q", cancel)
	}
	eventually(t, 20*time.Second, "the held sandbox restored on n2", func() bool {
		row := h.row(held.ID)
		return row.ClientState == model.StateRunning && row.Node == "n2" && row.Op == ""
	})
	release()
	h.drainUntil("the node drained", func() bool {
		nd, _ := h.st.GetNode(bg, "n1")
		return nd.StatusDescription == "drained"
	})
	// A running sandbox on a draining node is moved by Ensure too.
	h.exec("UPDATE nodes SET draining = false WHERE name = 'n1'")
	h.exec("UPDATE nodes SET draining = true, drain_started_at = now() WHERE name = 'n2'")
	if lease := h.ensure(free.ID); lease.Node != "n1" {
		t.Fatalf("Ensure left the sandbox on the draining node: %s", lease.Node)
	}
}

// TestDrainRestoresWhatRan: a sandbox that ran when the drain began is
// restored on another node once it is hibernated, also when a call that
// met the drain paused it (and gave up waiting); one that was paused
// before stays hibernated until it is used.
func TestDrainRestoresWhatRan(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	idle := h.running(512)
	h.startOp(idle.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	h.exec("UPDATE sandboxes SET paused_at = now() - interval '1 minute' WHERE id = $1", idle.ID)
	met := h.running(512)
	busy := h.running(512)
	h.exec("UPDATE nodes SET draining = true, drain_started_at = now() WHERE name = 'n1'")
	h.addNode("n2")
	h.startOp(met.ID, OpIntent{Kind: OpPause, Trigger: TriggerEnsure, Target: "n1"})
	for _, id := range []podid.ID{met.ID, busy.ID} {
		h.drainUntil("a sandbox that ran restored on n2", func() bool {
			row := h.row(id)
			return row.ClientState == model.StateRunning && row.Node == "n2" && row.Op == ""
		})
	}
	h.drainUntil("the idle one hibernated", func() bool {
		row := h.row(idle.ID)
		return row.ClientState == model.StateHibernated && row.Op == ""
	})
	never(t, 500*time.Millisecond, "the idle one restored", func() bool {
		return h.row(idle.ID).ClientState != model.StateHibernated
	})
}

// drainUntil runs drain passes until cond holds: a pass only starts the
// evacuations, at most drainPerNode per node at a time.
func (h *harness) drainUntil(what string, cond func() bool) {
	h.t.Helper()
	e := h.eng()
	eventually(h.t, 20*time.Second, what, func() bool {
		if err := e.drain(bg); err != nil {
			h.t.Fatal(err)
		}
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
		return false
	})
}

// TestDrainPassDoesNotWait: a drain pass starts evacuations and returns
// without waiting for them; at most drainPerNode run per node at a time,
// also across passes.
func TestDrainPassDoesNotWait(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	e := h.eng()
	var ids []podid.ID
	for range 3 {
		sb := h.running(512)
		h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
		ids = append(ids, sb.ID)
	}
	hold := make(chan struct{})
	var inFlight, most atomic.Int32
	n.Script().SetHook(podv1.PodService_HibernatePod_FullMethodName, func(ctx context.Context, _ string, _ any) error {
		c := inFlight.Add(1)
		defer inFlight.Add(-1)
		for m := most.Load(); c > m && !most.CompareAndSwap(m, c); m = most.Load() {
		}
		select {
		case <-hold:
		case <-ctx.Done():
		}
		return nil
	})
	h.exec("UPDATE nodes SET draining = true, drain_started_at = now() WHERE name = 'n1'")
	for range 3 {
		passed := make(chan error, 1)
		go func() { passed <- e.drain(bg) }()
		select {
		case err := <-passed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			close(hold)
			t.Fatal("a drain pass waited for the hibernations")
		}
	}
	eventually(t, 10*time.Second, "two hibernations under way", func() bool { return inFlight.Load() == drainPerNode })
	if err := e.drain(bg); err != nil {
		t.Fatal(err)
	}
	never(t, 500*time.Millisecond, "more hibernations at once than the limit", func() bool { return most.Load() > drainPerNode })
	if nd, _ := h.st.GetNode(bg, "n1"); nd.StatusDescription != "draining: 3 remain" {
		t.Fatalf("node description %q", nd.StatusDescription)
	}
	close(hold)
	h.drainUntil("the node drained", func() bool {
		nd, _ := h.st.GetNode(bg, "n1")
		return nd.StatusDescription == "drained"
	})
	for _, id := range ids {
		wantState(t, h.row(id), model.StateHibernated)
	}
	if m := most.Load(); m > drainPerNode {
		t.Fatalf("%d hibernations at once", m)
	}
}

func TestDrainWithoutObjectStore(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1", func(c *testnode.Config) { c.ObjectStore = nil })
	e := h.eng()
	sb := h.running(512)
	h.exec("UPDATE nodes SET draining = true, drain_started_at = now() WHERE name = 'n1'")
	if err := e.drain(bg); err != nil {
		t.Fatal(err)
	}
	nd, _ := h.st.GetNode(bg, "n1")
	if nd.StatusDescription != "cannot drain: the node has no object store" {
		t.Fatalf("description %q", nd.StatusDescription)
	}
	wantState(t, h.row(sb.ID), model.StateRunning)
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	_, err := e.Ensure(ctx, sb.ID)
	wantErr(t, err, model.ErrNoCapacity, "node n1 is draining")
}

func TestRepairLoop(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.PendingRepairAfter = time.Minute; c.UnreachableRepairAfter = time.Minute })
	n := h.addNode("n1")
	e := h.eng()

	// A pod stuck PENDING: its bring-up never ends (a cut create).
	stuck := h.sandbox(512)
	hang := make(chan struct{})
	n.Script().SetBringUp(func(ctx context.Context, id podid.ID) error {
		if id != stuck.ID {
			return nil
		}
		select {
		case <-hang:
		case <-ctx.Done():
		}
		return nil
	})
	e.t.opDeadline = func(OpKind, int) time.Duration { return time.Second }
	h.startOp(stuck.ID, OpIntent{Kind: OpCreate, Trigger: TriggerEnsure, Candidates: []string{"n1"}, ImageRef: testImage})
	e.t.opDeadline = OpDeadline
	wantState(t, h.row(stuck.ID), model.StateCreating)
	if err := e.repair(bg); err != nil {
		t.Fatal(err)
	}
	never(t, 300*time.Millisecond, "a repair before the threshold", func() bool { return h.row(stuck.ID).Op != "" })
	h.exec("UPDATE sandboxes SET pending_since = now() - interval '2 minutes' WHERE id = $1", stuck.ID)
	n.Script().SetBringUp(nil)
	if err := e.repair(bg); err != nil {
		t.Fatal(err)
	}
	h.waitState(stuck.ID, model.StateRunning, 15*time.Second)

	// A running sandbox whose guest stays unreachable: stopped and started
	// again, its processes gone.
	sick := h.running(512)
	if err := e.HoldProcess(bg, sick.ID, "p", 0); err != nil {
		t.Fatal(err)
	}
	e.Unreachable(sick.ID)
	eventually(t, 10*time.Second, "unreachable_since set", func() bool { return !h.row(sick.ID).UnreachableSince.IsZero() })
	h.exec("UPDATE sandboxes SET unreachable_since = now() - interval '2 minutes' WHERE id = $1", sick.ID)
	before := h.row(sick.ID)
	if err := e.repair(bg); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, "the repair ran", func() bool {
		row := h.row(sick.ID)
		return row.Gen > before.Gen && row.Op == "" && row.ClientState == model.StateRunning
	})
	row := h.row(sick.ID)
	if !row.UnreachableSince.IsZero() {
		t.Fatal("unreachable_since kept after the repair")
	}
	if hs := h.holds(sick.ID); len(hs) != 0 {
		t.Fatalf("process holds after a repair %+v", hs)
	}
	// Reachable again clears what Unreachable recorded.
	e.Unreachable(sick.ID)
	eventually(t, 10*time.Second, "marked", func() bool { return !h.row(sick.ID).UnreachableSince.IsZero() })
	e.Reachable(sick.ID)
	eventually(t, 10*time.Second, "cleared", func() bool { return h.row(sick.ID).UnreachableSince.IsZero() })

}

// TestRepairRechecks: a repair begins only for a sandbox still stuck, and
// one that finds the pod healthy restarts nothing.
func TestRepairRechecks(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.PendingRepairAfter = time.Minute; c.UnreachableRepairAfter = time.Minute })
	n := h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	h.settle(sb.ID)
	var stops atomic.Int32
	n.Script().SetHook(podv1.PodService_StopPod_FullMethodName, func(context.Context, string, any) error {
		stops.Add(1)
		return nil
	})
	repair := OpIntent{Kind: OpRepair, Trigger: TriggerRepair, Target: "n1"}
	// Running and reachable (Reachable cleared what the loop read): no
	// repair.
	_, err := h.begin(sb.ID, repair)
	wantErr(t, err, model.ErrPrecondition, "is running: no repair")
	// Unreachable for less than the threshold: no repair either.
	h.exec("UPDATE sandboxes SET unreachable_since = now() WHERE id = $1", sb.ID)
	_, err = h.begin(sb.ID, repair)
	wantErr(t, err, model.ErrPrecondition, "it is no longer stuck")
	// Read as creating and stuck, while its pod runs already (a Sync has
	// not applied RUNNING yet): the repair restarts nothing, the
	// observation decides, and the processes keep their holds.
	if err := e.HoldProcess(bg, sb.ID, "p", 0); err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE sandboxes SET client_state = 'creating', unreachable_since = NULL,
		pending_since = now() - interval '2 minutes', gen = gen + 1 WHERE id = $1`, sb.ID)
	res := h.startOp(sb.ID, repair)
	if res.State != model.StateRunning || res.Err != nil {
		t.Fatalf("repair of a running pod: %+v", res)
	}
	if s := stops.Load(); s != 0 {
		t.Fatalf("%d StopPod calls for a healthy pod", s)
	}
	if hs := h.holds(sb.ID); len(hs) != 1 {
		t.Fatalf("holds after a repair that restarted nothing %+v", hs)
	}
	row := h.row(sb.ID)
	if !row.PendingSince.IsZero() {
		t.Fatal("pending_since kept")
	}
	// Stuck past the threshold and really unreachable: repaired.
	h.exec("UPDATE sandboxes SET unreachable_since = now() - interval '2 minutes' WHERE id = $1", sb.ID)
	res = h.startOp(sb.ID, repair)
	if res.State != model.StateRunning || stops.Load() != 1 {
		t.Fatalf("repair of an unreachable guest: %+v, %d StopPod calls", res, stops.Load())
	}
	if hs := h.holds(sb.ID); len(hs) != 0 {
		t.Fatalf("holds after a restart %+v", hs)
	}
}

// TestReachabilityWritesInOrder: a mark and the clear of a later healthy
// call are written in the order of the calls, so that a healthy sandbox is
// never left marked.
func TestReachabilityWritesInOrder(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	idle := func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		r := e.unreachable[sb.ID]
		return r == nil || !r.busy
	}
	for i := range 20 {
		e.Unreachable(sb.ID)
		e.Reachable(sb.ID)
		eventually(t, 10*time.Second, "the writes done", idle)
		if row := h.row(sb.ID); !row.UnreachableSince.IsZero() {
			t.Fatalf("round %d: a healthy sandbox stayed marked", i)
		}
		e.mu.Lock()
		_, kept := e.unreachable[sb.ID]
		e.mu.Unlock()
		if kept {
			t.Fatalf("round %d: the cleared sandbox is still remembered", i)
		}
	}
	e.Reachable(sb.ID)
	e.Unreachable(sb.ID)
	eventually(t, 10*time.Second, "the writes done", idle)
	if row := h.row(sb.ID); row.UnreachableSince.IsZero() {
		t.Fatal("the last mark is not recorded")
	}
	e.Reachable(sb.ID)
	eventually(t, 10*time.Second, "cleared", func() bool { return idle() && h.row(sb.ID).UnreachableSince.IsZero() })
}

func TestRepairStuckRestore(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.PendingRepairAfter = time.Minute })
	// Today's feosd: a cancelled RestorePod stops where it is.
	n := h.addNode("n1", func(c *testnode.Config) { c.AbortOnCancel = true })
	e := h.eng()
	sb := h.running(512)
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	h.startOp(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1"})
	row := h.row(sb.ID)
	// A RestorePod cut halfway leaves the pod PENDING "restoring from …"
	// without a bring-up.
	n.Script().SetDelay(podv1.PodService_RestorePod_FullMethodName, time.Hour)
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.pods("n1").RestorePod(ctx, &podv1.RestorePodRequest{PodId: sb.ID.String(), HeadKey: row.CheckpointHead})
	}()
	info := h.podState("n1", sb.ID, podv1.PodState_POD_STATE_PENDING)
	if !strings.HasPrefix(info.GetReason(), "restoring from") {
		t.Fatalf("reason %q", info.GetReason())
	}
	n.Script().SetDelay(podv1.PodService_RestorePod_FullMethodName, 0)
	cancel()
	<-done
	h.waitState(sb.ID, model.StateCreating, 10*time.Second)
	h.exec("UPDATE sandboxes SET pending_since = now() - interval '2 minutes' WHERE id = $1", sb.ID)
	if err := e.repair(bg); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StateRunning, 15*time.Second)
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
}

func TestPendingRedescriptionAndNoCapacity(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	h.exec("UPDATE nodes SET slots = 0 WHERE name = 'n1'")
	sb := h.sandbox(512)
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	_, err := e.Ensure(ctx, sb.ID)
	wantErr(t, err, model.ErrNoCapacity, "no capacity (retryable): no feasible node; n1: no slots")
	row := h.row(sb.ID)
	wantState(t, row, model.StatePending)
	if row.StatusDescription != "no capacity (retryable): no feasible node; n1: no slots" {
		t.Fatalf("description %q", row.StatusDescription)
	}
	if err := e.pendingRedescription(bg); err != nil {
		t.Fatal(err)
	}
	if d := h.row(sb.ID).StatusDescription; !strings.HasPrefix(d, "no capacity") {
		t.Fatalf("re-described without capacity: %q", d)
	}
	h.exec("UPDATE nodes SET slots = 50 WHERE name = 'n1'")
	if err := e.pendingRedescription(bg); err != nil {
		t.Fatal(err)
	}
	if d := h.row(sb.ID).StatusDescription; d != "capacity available on n1: materialises on the next tool call" {
		t.Fatalf("description %q", d)
	}
	wantState(t, h.row(sb.ID), model.StatePending)
}

func TestUnknownSweep(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	h.settle(sb.ID)
	h.exec("UPDATE sandboxes SET client_state = 'unknown', gen = gen + 1 WHERE id = $1", sb.ID)
	if err := e.unknownSweep(bg); err != nil {
		t.Fatal(err)
	}
	wantState(t, h.row(sb.ID), model.StateRunning)
}
