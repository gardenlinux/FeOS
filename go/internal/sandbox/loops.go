// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/leader"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The names of the engine's leader loops. A ferun_leader hint wakes the
// loops whose name contains it ("down" after node expiry, "drain",
// "tombstone", "tenant").
const (
	LoopDownSweep        = "sandbox-down-sweep"
	LoopLostGrace        = "sandbox-lost-grace"
	LoopIdlePause        = "sandbox-idle-pause"
	LoopHibernateAfter   = "sandbox-hibernate-after"
	LoopDrain            = "sandbox-drain"
	LoopTombstones       = "sandbox-tombstones"
	LoopStaleOps         = "sandbox-stale-ops"
	LoopUnknownSweep     = "sandbox-unknown-sweep"
	LoopRepair           = "sandbox-repair"
	LoopPendingDescribe  = "sandbox-pending-redescription"
	LoopProcessHolds     = "sandbox-process-holds"
	LoopExpiredHolds     = "sandbox-expired-holds"
	LoopTenantDeletion   = "sandbox-tenant-deletion"
	fencedLockTimeout    = time.Second
	fencedStmtTimeout    = 3 * time.Second
	fencedBatch          = 256
	loopBatch            = 64
	unknownBatch         = 128
	idleConcurrency      = 16
	tombstoneConcurrency = 8
	drainPerNode         = 2
	// restoreTimeout bounds the restore of a drained sandbox elsewhere.
	restoreTimeout = 10 * time.Minute
)

// Loops returns the engine's leader loops (§9 "Policies"): the down sweep
// (5 s), lost grace (30 s), idle pause (1 s; a row's idle_after_s beside
// --idle-pause, so also when that is 0),
// hibernate-after (5 s, when set), drain (5 s), tombstones (10 s), stale
// ops (10 s), the unknown sweep (15 s), repair (30 s), the pending
// re-description (30 s), process holds (60 s), expired holds (60 s),
// tenant deletion (60 s) and the lost pushes of HTTP bindings (15 s). The loops that begin ops or call nodes consider
// only nodes the leader is connected to; the database-only loops are not
// filtered by connection. Idle pause, stale ops and expired holds act on
// leases that an outage lets expire, and wait until the leader has
// settled (settled).
func (e *Engine) Loops() []leader.Loop {
	loops := []leader.Loop{
		{Name: LoopDownSweep, Interval: 5 * time.Second, Run: e.downSweep},
		{Name: LoopLostGrace, Interval: 30 * time.Second, Run: e.lostGrace},
		{Name: LoopIdlePause, Interval: time.Second, Run: e.idlePause},
	}
	if e.cfg.HibernateAfter > 0 {
		loops = append(loops, leader.Loop{Name: LoopHibernateAfter, Interval: 5 * time.Second, Run: e.hibernateAfter})
	}
	return append(loops,
		leader.Loop{Name: LoopDrain, Interval: 5 * time.Second, Run: e.drain},
		leader.Loop{Name: LoopTombstones, Interval: 10 * time.Second, Run: e.tombstones},
		leader.Loop{Name: LoopStaleOps, Interval: 10 * time.Second, Run: e.staleOps},
		leader.Loop{Name: LoopUnknownSweep, Interval: 15 * time.Second, Run: e.unknownSweep},
		leader.Loop{Name: LoopRepair, Interval: 30 * time.Second, Run: e.repair},
		leader.Loop{Name: LoopPendingDescribe, Interval: 30 * time.Second, Run: e.pendingRedescription},
		leader.Loop{Name: LoopProcessHolds, Interval: time.Minute, Run: e.processHolds},
		leader.Loop{Name: LoopExpiredHolds, Interval: time.Minute, Run: e.expiredHolds},
		leader.Loop{Name: LoopTenantDeletion, Interval: time.Minute, Run: e.tenantDeletion},
		leader.Loop{Name: LoopHTTPBindings, Interval: httpLoopInterval, Run: e.httpBindingsLoop},
	)
}

// fenced runs fn in a fenced transaction of the leader (§7): short lock
// and statement timeouts, the lease checked first.
func (e *Engine) fenced(ctx context.Context, fn func(tx *store.Store) error) error {
	if e.el == nil {
		return leader.ErrNotLeader
	}
	return e.st.InTx(ctx, func(tx *store.Store) error {
		if err := e.fence(ctx, tx); err != nil {
			return err
		}
		return fn(tx)
	})
}

func (e *Engine) fence(ctx context.Context, tx *store.Store) error {
	if e.el == nil {
		return leader.ErrNotLeader
	}
	if err := tx.SetLocalTimeouts(ctx, fencedLockTimeout, fencedStmtTimeout); err != nil {
		return err
	}
	return e.el.Fence(ctx, tx.Q())
}

// downSweep moves the sandboxes without an op on down or removed nodes to
// unknown, every pass (level-triggered: a leader that crashed between a
// node's expiry and the sweep leaves nothing behind).
func (e *Engine) downSweep(ctx context.Context) error {
	for {
		var ids []podid.ID
		err := e.fenced(ctx, func(tx *store.Store) error {
			var err error
			ids, err = tx.DownSweep(ctx, fencedBatch)
			return err
		})
		if err != nil {
			return fmt.Errorf("down sweep: %w", err)
		}
		if len(ids) > 0 {
			e.log.Info("sandboxes of down nodes are unknown", "sandboxes", len(ids))
		}
		if len(ids) < fencedBatch {
			return nil
		}
	}
}

// lostGrace resolves the unknown sandboxes of nodes down for longer than
// the lost grace, or removed: hibernated with a checkpoint, else lost, or
// pending without a node when their pod was never seen (store.LostGrace);
// their bookings and process holds go, their runs close. No tombstone: the
// pod may still run there, and a returning node's pod is adopted (one of a
// pending sandbox is tombstoned by reconcile).
func (e *Engine) lostGrace(ctx context.Context) error {
	for {
		var lost []store.LostRow
		err := e.fenced(ctx, func(tx *store.Store) error {
			var err error
			if lost, err = tx.LostGrace(ctx, e.cfg.LostGrace, fencedBatch); err != nil {
				return err
			}
			var runs []podid.ID
			for _, l := range lost {
				if l.RunID != "" {
					runs = append(runs, l.ID)
				}
			}
			return tx.ClearRunIDs(ctx, runs)
		})
		if err != nil {
			return fmt.Errorf("lost grace: %w", err)
		}
		for _, l := range lost {
			c := &change{e: e, before: store.Sandbox{ID: l.ID, Owner: l.Owner, Node: l.Node}}
			if l.RunID != "" {
				e.record(c.runEnd(l.RunID, "lost"))
			}
			e.record(activity.Event{SandboxID: l.ID, TenantID: l.Owner, Node: l.Node, Kind: activity.KindSandboxState,
				ParentSpanID: l.RunID, Name: fmt.Sprintf("%s → %s", model.StateUnknown, l.State),
				Detail: map[string]any{"from": model.StateUnknown, "to": l.State, "previous": l.Previous}})
		}
		if len(lost) < fencedBatch {
			return nil
		}
	}
}

// startOps begins ops for rows, at most n begins at once, without waiting
// for the ops: a loop's ops never wait for a node, and their failed
// rechecks are skipped quietly.
func (e *Engine) startOps(ctx context.Context, rows []store.Sandbox, n int, intent func(store.Sandbox) OpIntent) {
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for _, row := range rows {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func() {
			defer func() {
				<-sem
				wg.Done()
			}()
			in := intent(row)
			if _, err := e.runner.start(ctx, row.ID, in, false); err != nil && !quiet(err) {
				e.rl.warn("loop-op", "a policy's op was not begun", "sandbox", row.ID, "op", in.Kind, "err", err)
			}
		}()
	}
	wg.Wait()
}

// quiet reports the errors a loop skips silently: the row changed, or an
// op runs.
func quiet(err error) bool {
	return errors.Is(err, model.ErrPrecondition) || errors.Is(err, model.ErrBusy) || errors.Is(err, model.ErrNotFound) ||
		errors.Is(err, model.ErrStopping)
}

// idlePause pauses running sandboxes without a live hold whose last
// activity is older than their idle interval, the row's idle_after_s or
// else --idle-pause, 0 for never (trigger idle: the begin rechecks both
// with fresh statements), only once the leader has settled (settled).
func (e *Engine) idlePause(ctx context.Context) error {
	if !e.settled() {
		return nil
	}
	rows, err := e.st.IdleSandboxes(ctx, e.cfg.IdlePause, e.conns.Nodes(), loopBatch)
	if err != nil {
		return fmt.Errorf("idle pause: %w", err)
	}
	e.startOps(ctx, rows, idleConcurrency, func(r store.Sandbox) OpIntent {
		return OpIntent{Kind: OpPause, Trigger: TriggerIdle, Target: r.Node, Incarnation: r.Incarnation}
	})
	return nil
}

// hibernateAfter hibernates sandboxes paused for longer than
// --hibernate-after on nodes with an object store.
func (e *Engine) hibernateAfter(ctx context.Context) error {
	rows, err := e.st.HibernateCandidates(ctx, e.cfg.HibernateAfter, e.conns.Nodes(), loopBatch)
	if err != nil {
		return fmt.Errorf("hibernate after: %w", err)
	}
	e.startOps(ctx, rows, 4, func(r store.Sandbox) OpIntent {
		return OpIntent{Kind: OpHibernate, Trigger: TriggerHibernateAfter, Target: r.Node, Incarnation: r.Incarnation}
	})
	return nil
}

// drain evacuates the draining nodes the leader is connected to: paused
// sandboxes are hibernated, running ones without a live exec hold are
// paused and hibernated, stopped and failed ones hibernate their disk; a
// forward does not keep a sandbox (the port door closes its upgraded
// connections once it leaves running); after --drain-timeout, minutes, the
// execs that still run are cancelled (ferun_cancel) and everything is
// paused and hibernated regardless of holds. A sandbox that ran when the
// drain began, running or paused since, by the drain or by a call that
// met it, is restored on another node at once, where the calls that
// waited for it go on. A node without an object store cannot drain; one that holds
// nothing but hibernated, lost and pending sandboxes is drained. A pass
// only starts evacuations, at most drainPerNode per node at a time across
// passes, and never waits for them (a hibernation takes minutes per GiB,
// up to --hibernate-timeout): every pass looks at every draining node,
// its overdue status and its held sandboxes again, and describes its
// progress.
func (e *Engine) drain(ctx context.Context) error {
	nodes, err := e.st.DrainingNodes(ctx, e.cfg.DrainTimeout)
	if err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	var errs []error
	for _, dn := range nodes {
		if !e.conns.Connected(dn.Node.Name) {
			continue
		}
		if err := e.drainNode(ctx, dn); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) drainNode(ctx context.Context, dn store.DrainingNode) error {
	node := dn.Node.Name
	if !hasObjectStore(dn.Node) {
		return e.st.SetNodeDescriptionIfDraining(ctx, node, "cannot drain: the node has no object store")
	}
	trigger := TriggerDrain
	if dn.Overdue {
		trigger = TriggerDrainForced
	}
	rows, err := e.st.SandboxesInState(ctx, node, []string{model.StatePaused, model.StateRunning, model.StateStopped,
		model.StateFailed}, loopBatch)
	if err != nil {
		return fmt.Errorf("drain %s: %w", node, err)
	}
	for _, row := range rows {
		if e.evacuating(row.ID) {
			continue
		}
		if row.ClientState == model.StateRunning && !dn.Overdue {
			if held, err := e.st.HasLiveHold(ctx, row.ID, model.HoldExec); err != nil || held {
				continue
			}
		}
		ran := row.ClientState == model.StateRunning ||
			(row.ClientState == model.StatePaused && !row.PausedAt.Before(dn.Node.DrainStartedAt))
		if !e.startEvacuation(ctx, row, trigger, dn.Overdue, ran) {
			break // the node's evacuations are at their limit: the rest on a later pass
		}
	}
	left, err := e.st.CountUnevacuated(ctx, node)
	if err != nil {
		return err
	}
	desc := "drained"
	if left > 0 {
		desc = fmt.Sprintf("draining: %d remain", left)
	}
	return e.st.SetNodeDescriptionIfDraining(ctx, node, desc)
}

// evacuating reports whether a drain worker moves the sandbox.
func (e *Engine) evacuating(id podid.ID) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.evacuations[id]
	return ok
}

// startEvacuation starts a worker of the engine that moves row off its
// draining node, unless drainPerNode workers move sandboxes off that node
// already (false). Past the drain timeout (forced) the execs that still
// run in a running sandbox are cancelled first. The worker outlives the
// loop's pass; it ends with the leader's term (ctx) or the engine.
func (e *Engine) startEvacuation(ctx context.Context, row store.Sandbox, trigger OpTrigger, forced, ran bool) bool {
	node := row.Node
	e.mu.Lock()
	if e.evacuatingOn[node] >= drainPerNode {
		e.mu.Unlock()
		return false
	}
	e.evacuations[row.ID] = node
	e.evacuatingOn[node]++
	e.mu.Unlock()
	done := func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		delete(e.evacuations, row.ID)
		if e.evacuatingOn[node]--; e.evacuatingOn[node] <= 0 {
			delete(e.evacuatingOn, node)
		}
	}
	if forced && row.ClientState == model.StateRunning {
		e.cancelExecs(ctx, row.ID)
	}
	started := e.goBackground(func(bg context.Context) {
		hibernated := func() bool {
			defer done()
			wctx, cancel := context.WithCancel(ctx)
			defer cancel()
			stop := context.AfterFunc(bg, cancel)
			defer stop()
			return e.evacuate(wctx, row, trigger)
		}()
		if hibernated && ran {
			e.restoreElsewhere(bg, row.ID)
		}
	})
	if !started {
		done()
	}
	return started
}

// evacuate moves one sandbox off a draining node: a running one is paused
// first, then hibernated. It reports whether the sandbox is hibernated
// now.
func (e *Engine) evacuate(ctx context.Context, row store.Sandbox, trigger OpTrigger) bool {
	in := OpIntent{Kind: OpHibernate, Trigger: trigger, Target: row.Node, Incarnation: row.Incarnation}
	if row.ClientState == model.StateRunning {
		in.Kind = OpPause
	}
	for {
		h, err := e.runner.start(ctx, row.ID, in, false)
		if err != nil {
			if !quiet(err) {
				e.rl.warn("drain", "a drain's op was not begun", "sandbox", row.ID, "op", in.Kind, "err", err)
			}
			return false
		}
		res, err := h.Wait(ctx)
		if err != nil || res.Err != nil {
			return false
		}
		if h.kind != OpPause {
			return res.State == model.StateHibernated
		}
		if res.State != model.StatePaused {
			return false
		}
		in.Kind = OpHibernate
	}
}

// restoreElsewhere brings a sandbox that ran when its node's drain began,
// and that the drain hibernated, back up on another node, as its next
// call would: the calls that waited for the move go on there, and its pod
// is not left cold. Without room elsewhere it stays hibernated until it
// is used.
func (e *Engine) restoreElsewhere(ctx context.Context, id podid.ID) {
	ctx, cancel := context.WithTimeout(ctx, restoreTimeout)
	defer cancel()
	lease, err := e.Ensure(ctx, id)
	if err != nil {
		if !quiet(err) {
			e.rl.warn("drain", "a drained sandbox stays hibernated", "sandbox", id, "err", err)
		}
		return
	}
	e.log.Info("drained sandbox restored", "sandbox", id, "node", lease.Node)
}

// cancelExecs tells every instance to cancel the execs of a sandbox
// (ferun_cancel), past the drain timeout.
func (e *Engine) cancelExecs(ctx context.Context, id podid.ID) {
	payload, _ := json.Marshal(map[string]string{"kind": "exec", "sandbox": id.String()})
	if err := db.Notify(ctx, e.d.Pool, db.ChanCancel, string(payload)); err != nil {
		e.rl.warn("drain", "cannot cancel a draining sandbox's execs", "sandbox", id, "err", err)
	}
}

// tombstones drops the tombstones of removed nodes (fenced, with an
// activity row) and processes the due tombstones of the nodes the leader
// is connected to, at most 64 a pass, 8 at once.
func (e *Engine) tombstones(ctx context.Context) error {
	var dropped []store.Tombstone
	if err := e.fenced(ctx, func(tx *store.Store) error {
		var err error
		dropped, err = tx.DropRemovedNodeTombstones(ctx, fencedBatch)
		return err
	}); err != nil {
		return fmt.Errorf("tombstones of removed nodes: %w", err)
	}
	for _, t := range dropped {
		e.record(activity.Event{SandboxID: t.PodID, Node: t.Node, Kind: activity.KindTombstone,
			Name:   fmt.Sprintf("tombstone on node %s dropped: the node was removed", t.Node),
			Detail: map[string]any{"node": t.Node, "incarnation": t.Incarnation.String(), "reason": t.Reason}})
	}
	due, err := e.st.DueTombstones(ctx, e.conns.Nodes(), loopBatch)
	if err != nil {
		return fmt.Errorf("tombstones: %w", err)
	}
	sem := make(chan struct{}, tombstoneConcurrency)
	var wg sync.WaitGroup
	for _, t := range due {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil
		}
		wg.Add(1)
		go func() {
			defer func() {
				<-sem
				wg.Done()
			}()
			_ = e.processTombstone(t.Key()).wait(ctx)
		}()
	}
	wg.Wait()
	return nil
}

// staleOps handles the ops whose owner is not live: taken over when the
// leader is connected to their target, else released (unknown, for Sync).
// A live owner's op more than 5 minutes past its deadline, which only a
// bug or a database outage explains, is released and logged as an error.
// It acts only once the leader has settled (settled).
func (e *Engine) staleOps(ctx context.Context) error {
	if !e.settled() {
		return nil
	}
	rows, err := e.st.StaleOps(ctx, e.t.staleOverdue, loopBatch)
	if err != nil {
		return fmt.Errorf("stale ops: %w", err)
	}
	var errs []error
	for _, row := range rows {
		live, err := e.st.InstanceLive(ctx, row.OpOwner)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !live && e.conns.Connected(row.OpTarget) {
			_, err := e.runner.start(ctx, row.ID, OpIntent{Kind: OpKind(row.Op), Trigger: TriggerRepair,
				Target: row.OpTarget, Incarnation: row.Incarnation, takeover: row.OpID}, false)
			if err == nil || quiet(err) {
				continue
			}
			e.rl.warn("stale-op", "cannot take over a stale op; releasing it", "sandbox", row.ID, "err", err)
		}
		if live {
			e.log.Error("an op of a live instance is past its deadline; releasing it", "sandbox", row.ID, "op", row.Op,
				"owner", row.OpOwner, "deadline", row.OpDeadline)
		}
		if err := e.releaseOp(ctx, row, true); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// followOrphans looks for this instance's orphaned ops (runner.runs) every
// orphanEvery until ctx ends, so that no instance waits for an op that
// nobody runs until its deadline: the leader's stale ops leave a live
// owner's alone.
func (e *Engine) followOrphans(ctx context.Context) {
	t := time.NewTicker(e.t.orphanEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := e.orphans(ctx); err != nil && ctx.Err() == nil {
			e.rl.warn("orphans", "cannot resolve the orphaned ops of this instance", "err", err)
		}
	}
}

// orphans takes over the ops the rows say are this instance's and that
// its runner does not run, when it is connected to their target, and
// releases the others (unknown, for Sync), as the leader does with a dead
// instance's. It reads them all, a page at a time, without the ops the
// runner ran when it began: an orphan is found however many ops of this
// instance run, whatever their deadlines. A release is decided under the
// sandbox's begin lock, where an op whose begin committed is registered
// with the runner.
func (e *Engine) orphans(ctx context.Context) error {
	if e.runner.isRefusing() {
		return nil // Stop releases the instance's ops
	}
	running := e.runner.opIDs()
	var after store.OpCursor
	var errs []error
	for {
		rows, err := e.st.OpsOf(ctx, e.id, running, after, e.t.orphanBatch)
		if err != nil {
			return errors.Join(append(errs, fmt.Errorf("orphaned ops: %w", err))...)
		}
		errs = append(errs, e.orphanPage(ctx, rows))
		if len(rows) == 0 || len(rows) < e.t.orphanBatch || ctx.Err() != nil {
			return errors.Join(errs...)
		}
		after = after.Next(rows)
	}
}

// orphanPage takes over or releases the orphaned ops among rows.
func (e *Engine) orphanPage(ctx context.Context, rows []store.Sandbox) error {
	var errs []error
	for _, row := range rows {
		if e.runner.runs(row) {
			continue
		}
		if e.conns.Connected(row.OpTarget) {
			h, err := e.runner.start(ctx, row.ID, OpIntent{Kind: OpKind(row.Op), Trigger: TriggerRepair,
				Target: row.OpTarget, Incarnation: row.Incarnation, takeover: row.OpID}, false)
			if err == nil && h.takeover {
				e.log.Warn("an op of this instance ran nowhere; took it over", "sandbox", row.ID, "op", row.Op,
					"op_id", row.OpID)
			}
			if err == nil || quiet(err) {
				continue
			}
			e.rl.warn("orphans", "cannot take over an orphaned op; releasing it", "sandbox", row.ID, "err", err)
		}
		if err := e.releaseOrphan(ctx, row); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// releaseOrphan releases an orphaned op of this instance's under the
// sandbox's begin lock, unless the runner runs it by then.
func (e *Engine) releaseOrphan(ctx context.Context, row store.Sandbox) error {
	unlock, err := e.runner.lockBegin(ctx, row.ID)
	if err != nil {
		return err
	}
	defer unlock()
	if e.runner.runs(row) {
		return nil
	}
	e.log.Warn("an op of this instance runs nowhere; releasing it", "sandbox", row.ID, "op", row.Op, "op_id", row.OpID)
	return e.releaseOp(ctx, row, false)
}

// releaseOp releases a stale or orphaned op, when the row still has it:
// the op columns cleared, unknown, node and booking kept. The leader's
// release of another instance's op is fenced.
func (e *Engine) releaseOp(ctx context.Context, read store.Sandbox, fenced bool) error {
	inTx := e.st.InTx
	if fenced {
		inTx = e.fenced
	}
	var events []activity.Event
	err := inTx(ctx, func(tx *store.Store) error {
		events = nil
		row, err := tx.LockSandbox(ctx, read.ID, store.ForUpdate)
		if errors.Is(err, model.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.OpID != read.OpID || row.Op == "" {
			return nil
		}
		c, err := e.newChange(ctx, tx, row)
		if err != nil {
			return err
		}
		clearOp(&c.after)
		c.after.ClientState = model.StateUnknown
		c.after.StatusDescription = fmt.Sprintf("op %s of instance %s was abandoned", row.Op, row.OpOwner)
		if err := c.commit(ctx); err != nil {
			return err
		}
		events = c.events
		return nil
	})
	if err != nil {
		return fmt.Errorf("release the op of sandbox %s: %w", read.ID, err)
	}
	e.record(events...)
	return nil
}

// unknownSweep syncs the unknown sandboxes without an op on ready nodes
// the leader is connected to.
func (e *Engine) unknownSweep(ctx context.Context) error {
	rows, err := e.st.UnknownSandboxes(ctx, e.conns.Nodes(), unknownBatch)
	if err != nil {
		return fmt.Errorf("unknown sweep: %w", err)
	}
	for _, row := range rows {
		if err := e.Sync(ctx, row.ID); err != nil && ctx.Err() == nil {
			e.rl.warn("sync", "a sync of the unknown sweep failed", "sandbox", row.ID, "err", err)
		}
	}
	return nil
}

// repair repairs the pods stuck PENDING without an op for longer than
// --pending-repair-after and the running sandboxes whose guest has been
// unreachable for longer than --unreachable-repair-after.
func (e *Engine) repair(ctx context.Context) error {
	rows, err := e.st.RepairCandidates(ctx, e.cfg.PendingRepairAfter, e.cfg.UnreachableRepairAfter, e.conns.Nodes(), loopBatch)
	if err != nil {
		return fmt.Errorf("repair: %w", err)
	}
	e.startOps(ctx, rows, 4, func(r store.Sandbox) OpIntent {
		return OpIntent{Kind: OpRepair, Trigger: TriggerRepair, Target: r.Node, Incarnation: r.Incarnation}
	})
	return nil
}

// pendingRedescription tells the pending sandboxes that found no capacity
// when a node now passes the gates (cosmetic, as old: they materialise on
// their next use).
func (e *Engine) pendingRedescription(ctx context.Context) error {
	rows, err := e.st.PendingWithoutCapacity(ctx, loopBatch)
	if err != nil {
		return fmt.Errorf("pending re-description: %w", err)
	}
	for _, row := range rows {
		p, err := e.place(ctx, row, OpCreate)
		if err != nil || len(p.candidates) == 0 {
			continue
		}
		desc := fmt.Sprintf("capacity available on %s: materialises on the next tool call", p.candidates[0])
		if err := e.st.SetStatusDescription(ctx, row.ID, row.Incarnation, desc); err != nil {
			return err
		}
	}
	return nil
}

// processHolds releases the process holds of processes that ended by
// themselves: in running sandboxes idle past the idle time whose only live
// holds are process holds, the processes are listed (no hold, no
// activity) and the holds of those not running are deleted; the idle loop
// then pauses a sandbox that has none left.
func (e *Engine) processHolds(ctx context.Context) error {
	lister := e.processLister()
	if lister == nil {
		return nil
	}
	held, err := e.st.ProcessHeldSandboxes(ctx, max(e.cfg.IdlePause, 0), e.conns.Nodes(), loopBatch)
	if err != nil {
		return fmt.Errorf("process holds: %w", err)
	}
	for _, ph := range held {
		sb := ph.Sandbox
		running, err := lister.RunningProcesses(ctx, model.Lease{SandboxID: sb.ID, Incarnation: sb.Incarnation, Node: sb.Node})
		if err != nil {
			e.rl.warn("process-holds", "cannot list a sandbox's processes", "sandbox", sb.ID, "err", err)
			continue
		}
		for _, pid := range ph.ProcessIDs {
			if running[pid] {
				continue
			}
			if _, err := e.st.ReleaseProcessHold(ctx, sb.ID, pid); err != nil {
				return err
			}
		}
	}
	return nil
}

// expiredHolds deletes the holds whose lease has passed, only once the
// leader has settled (settled).
func (e *Engine) expiredHolds(ctx context.Context) error {
	if !e.settled() {
		return nil
	}
	if _, err := e.st.DeleteExpiredHolds(ctx); err != nil {
		return fmt.Errorf("expired holds: %w", err)
	}
	return nil
}

// tenantDeletion continues the deletions of tenants that a DELETE began,
// in fenced batches.
func (e *Engine) tenantDeletion(ctx context.Context) error {
	tenants, err := e.st.DeletingTenants(ctx)
	if err != nil {
		return fmt.Errorf("tenant deletion: %w", err)
	}
	var errs []error
	for _, t := range tenants {
		if err := e.deleteTenantSandboxes(ctx, t, func(tx *store.Store) error { return e.fence(ctx, tx) }); err != nil {
			errs = append(errs, fmt.Errorf("delete tenant %s: %w", t, err))
		}
	}
	return errors.Join(errs...)
}
