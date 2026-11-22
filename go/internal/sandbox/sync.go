// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The ferun_leader hints the engine sends.
const (
	hintTombstone = "tombstone"
)

// syncer coalesces the syncs of one sandbox on this instance: one runs at
// a time, and requests that arrive while it runs make exactly one more run
// after it, which every one of them waits for.
type syncer struct {
	mu sync.Mutex
	m  map[podid.ID]*syncEntry
}

type syncEntry struct {
	running, queued *syncRun
}

type syncRun struct {
	done chan struct{}
	err  error
}

// requestSync asks for a sync of the sandbox and returns the run that
// covers the request; nil once the engine stopped.
func (e *Engine) requestSync(id podid.ID) *syncRun {
	s := &e.syncs
	s.mu.Lock()
	ent := s.m[id]
	if ent == nil {
		ent = &syncEntry{}
		s.m[id] = ent
	}
	if ent.running != nil {
		if ent.queued == nil {
			ent.queued = &syncRun{done: make(chan struct{})}
		}
		r := ent.queued
		s.mu.Unlock()
		return r
	}
	r := &syncRun{done: make(chan struct{})}
	ent.running = r
	s.mu.Unlock()
	started := e.goBackground(func(ctx context.Context) {
		cur := r
		for {
			sctx, cancel := context.WithTimeout(ctx, e.t.syncTimeout)
			cur.err = e.syncOnce(sctx, id)
			cancel()
			close(cur.done)
			s.mu.Lock()
			if ent.queued == nil || ctx.Err() != nil {
				q := ent.queued
				delete(s.m, id)
				s.mu.Unlock()
				if q != nil {
					q.err = ctx.Err()
					close(q.done)
				}
				return
			}
			cur, ent.running, ent.queued = ent.queued, ent.queued, nil
			s.mu.Unlock()
		}
	})
	if !started {
		s.mu.Lock()
		delete(s.m, id)
		q := ent.queued
		s.mu.Unlock()
		r.err = context.Canceled
		close(r.done)
		if q != nil {
			q.err = context.Canceled
			close(q.done)
		}
	}
	return r
}

// Sync applies what the sandbox's node holds of its pod to its row (§9
// "Sync"), and waits for it, or ctx. Syncs of one sandbox are coalesced
// per instance. A sandbox with an op in flight is synced after the op: by
// its finish when the op is this instance's, else when the op's end is
// seen on ferun_sandbox (or every 5 s). Without a connection to the node,
// the instances that have one are asked to reconcile it (ferun_reconcile).
func (e *Engine) Sync(ctx context.Context, id podid.ID) error {
	r := e.requestSync(id)
	select {
	case <-r.done:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PodChanged is fleet's hook for a pod event of node: a sync of the pod's
// sandbox, asynchronous and coalesced, deferred while an op runs. (A pod
// event for a row on another node makes fleet request a reconcile of the
// event's node.)
func (e *Engine) PodChanged(node string, podID podid.ID) {
	e.requestSync(podID)
}

// syncOnce reads the row, observes the pod on its node, and applies the
// observation when the row is unchanged since the read (gen, op,
// incarnation); when it changed, it reads and observes once more, then
// gives up (a later event or the sweeps catch up).
func (e *Engine) syncOnce(ctx context.Context, id podid.ID) error {
	for range 2 {
		row, err := e.st.GetSandbox(ctx, id)
		if errors.Is(err, model.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.Op != "" {
			e.deferSync(row)
			return nil
		}
		if row.Node == "" {
			return nil
		}
		if !e.conns.Connected(row.Node) {
			e.notifyReconcile(row.Node)
			return nil
		}
		obs, err := e.getPod(ctx, row.Node, id)
		if err != nil {
			return err
		}
		applied, deferred, err := e.applySync(ctx, row, obs)
		if err != nil || applied || deferred {
			return err
		}
	}
	return nil
}

// applySync applies an observation of the row's node in one transaction,
// when the row's gen, op and incarnation are those the observation was
// made for.
func (e *Engine) applySync(ctx context.Context, read store.Sandbox, obs observation) (applied, deferred bool, err error) {
	var events []activity.Event
	var keys []store.TombstoneKey
	var tls *tlsPush
	err = e.st.InTx(ctx, func(tx *store.Store) error {
		applied, deferred, events, keys, tls = false, false, nil, nil, nil
		row, err := tx.LockSandbox(ctx, read.ID, store.ForUpdate)
		if errors.Is(err, model.ErrNotFound) {
			applied = true
			return nil
		}
		if err != nil {
			return err
		}
		if row.Op != "" {
			deferred = true
			return nil
		}
		if row.Gen != read.Gen || row.Incarnation != read.Incarnation || row.Node != read.Node {
			return nil
		}
		c, err := e.newChange(ctx, tx, row)
		if err != nil {
			return err
		}
		c.observe(row.Node, obs)
		if obs.ours(row.Incarnation) {
			if _, err := tx.DeleteTombstone(ctx, store.TombstoneKey{PodID: row.ID, Node: row.Node, Incarnation: row.Incarnation}); err != nil {
				return err
			}
		}
		if err := c.commit(ctx); err != nil {
			return err
		}
		applied, events, keys, tls = true, c.events, c.tombstoneKeys(), c.tls
		return nil
	})
	if err != nil {
		return false, false, err
	}
	if deferred {
		row, rerr := e.st.GetSandbox(ctx, read.ID)
		if rerr == nil {
			e.deferSync(row)
		}
	}
	e.record(events...)
	e.processAfterCommit(afterCommit{tombstones: keys, tls: tls})
	return applied, deferred, nil
}

// deferSync marks a sandbox with an op in flight for a sync after the op:
// the op's own finish when it is this instance's, else this instance's
// follower of ferun_sandbox.
func (e *Engine) deferSync(row store.Sandbox) {
	if h := e.runner.Running(row.ID); h != nil && h.opID == row.OpID && h.markDirty() {
		return
	}
	e.mu.Lock()
	e.deferred[row.ID] = struct{}{}
	e.mu.Unlock()
}

// followSandboxes runs the deferred syncs: on a ferun_sandbox notification
// of a deferred sandbox (or "" and "*"), and every deferredEvery, a
// sandbox whose op ended (or that is gone) is synced.
func (e *Engine) followSandboxes(ctx context.Context) {
	ch, unsubscribe := e.d.Subscribe(db.ChanSandbox)
	defer unsubscribe()
	tick := time.NewTicker(e.t.deferredEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-ch:
			if p == "" || p == "*" {
				e.checkDeferred(ctx, nil)
				continue
			}
			id, err := podid.Parse(p)
			if err != nil {
				continue
			}
			e.mu.Lock()
			_, ok := e.deferred[id]
			e.mu.Unlock()
			if ok {
				e.checkDeferred(ctx, []podid.ID{id})
			}
		case <-tick.C:
			e.checkDeferred(ctx, nil)
		}
	}
}

// checkDeferred syncs those of ids (every deferred sandbox for nil) whose
// op ended.
func (e *Engine) checkDeferred(ctx context.Context, ids []podid.ID) {
	if ids == nil {
		e.mu.Lock()
		for id := range e.deferred {
			ids = append(ids, id)
		}
		e.mu.Unlock()
	}
	for _, id := range ids {
		qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		row, err := e.st.GetSandbox(qctx, id)
		cancel()
		if err != nil && !errors.Is(err, model.ErrNotFound) {
			continue
		}
		if err == nil && row.Op != "" {
			continue
		}
		e.mu.Lock()
		delete(e.deferred, id)
		e.mu.Unlock()
		if err == nil {
			e.requestSync(id)
		}
	}
}

// notifyReconcile asks the instances connected to node to reconcile it
// (ferun_reconcile), at most once per notifyEvery per node from this
// instance.
func (e *Engine) notifyReconcile(node string) {
	now := time.Now()
	e.mu.Lock()
	if last, ok := e.notified[node]; ok && now.Sub(last) < e.t.notifyEvery {
		e.mu.Unlock()
		return
	}
	e.notified[node] = now
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(e.bg, 10*time.Second)
	defer cancel()
	if err := db.Notify(ctx, e.d.Pool, db.ChanReconcile, node); err != nil {
		e.rl.warn("reconcile-notify", "cannot ask other instances for a reconcile", "node", node, "err", err)
	}
}
