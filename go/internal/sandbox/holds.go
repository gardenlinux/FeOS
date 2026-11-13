// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"sync"
	"time"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// holdKey names this instance's hold of one kind on one sandbox.
type holdKey struct {
	id   podid.ID
	kind model.HoldKind
}

// holdState counts this instance's holders of a key. mu serialises the
// key's database writes (0 → 1, renewals, 1 → 0); refs keeps the entry
// while anyone uses it.
type holdState struct {
	mu    sync.Mutex
	count int
	refs  int
}

func (e *Engine) holdRef(k holdKey) *holdState {
	e.holdsMu.Lock()
	defer e.holdsMu.Unlock()
	hs := e.holds[k]
	if hs == nil {
		hs = &holdState{}
		e.holds[k] = hs
	}
	hs.refs++
	return hs
}

func (e *Engine) holdUnref(k holdKey, hs *holdState) {
	e.holdsMu.Lock()
	defer e.holdsMu.Unlock()
	hs.refs--
	if hs.refs == 0 && e.holds[k] == hs {
		delete(e.holds, k)
	}
}

// Acquire ensures the sandbox runs and holds it awake with a hold of kind
// (exec or forward) until release is called (§9 "Holds and activity"). Its
// admission locks the row FOR NO KEY UPDATE, which a pause's begin (FOR
// UPDATE) excludes, and checks the locked version: running without an op
// (else it ensures again). This instance's first holder of the kind writes
// the hold and last_active_at in that transaction; its last one deletes
// the hold and sets last_active_at in one transaction, so that the idle
// loop never sees the sandbox without its hold and with an old activity.
func (e *Engine) Acquire(ctx context.Context, id podid.ID, kind model.HoldKind) (model.Lease, func(), error) {
	k := holdKey{id, kind}
	for {
		if _, err := e.Ensure(ctx, id); err != nil {
			return model.Lease{}, nil, err
		}
		hs := e.holdRef(k)
		lease, ok, err := e.admitHold(ctx, k, hs)
		if err != nil || !ok {
			e.holdUnref(k, hs)
			if err != nil {
				return model.Lease{}, nil, err
			}
			if ctx.Err() != nil {
				return model.Lease{}, nil, ctx.Err()
			}
			continue
		}
		var once sync.Once
		return lease, func() { once.Do(func() { e.releaseHold(k, hs) }) }, nil
	}
}

// admitHold is Acquire's transaction; ok is false when the sandbox does
// not run without an op any more.
func (e *Engine) admitHold(ctx context.Context, k holdKey, hs *holdState) (model.Lease, bool, error) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	var lease model.Lease
	var ok bool
	err := e.st.InTx(ctx, func(tx *store.Store) error {
		ok = false
		row, err := tx.LockSandbox(ctx, k.id, store.ForNoKeyUpdate)
		if err != nil {
			return err
		}
		if e.admitHook != nil {
			e.admitHook(ctx, k.id)
		}
		if row.ClientState != model.StateRunning || row.Op != "" {
			return nil
		}
		if hs.count == 0 {
			if err := tx.AdmitHold(ctx, k.id, k.kind, e.id, e.t.holdLease); err != nil {
				return err
			}
		}
		lease = model.Lease{SandboxID: row.ID, Incarnation: row.Incarnation, Node: row.Node, Workdir: row.Workdir}
		ok = true
		return nil
	})
	if err != nil || !ok {
		return model.Lease{}, false, err
	}
	hs.count++
	return lease, true, nil
}

// releaseHold ends one holder; the last one deletes the hold.
func (e *Engine) releaseHold(k holdKey, hs *holdState) {
	defer e.holdUnref(k, hs)
	hs.mu.Lock()
	defer hs.mu.Unlock()
	hs.count--
	if hs.count > 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(e.bg), 10*time.Second)
	defer cancel()
	err := e.st.InTx(ctx, func(tx *store.Store) error {
		return tx.ReleaseHold(ctx, k.id, k.kind, e.id)
	})
	if err != nil {
		// The hold's lease ends it: the leader deletes expired holds.
		e.rl.warn("hold", "cannot release a hold; it expires with its lease", "sandbox", k.id, "kind", k.kind, "err", err)
	}
}

// renewHolds renews this instance's holds every holdRenew while they are
// held, by an upsert that re-inserts a hold the leader already expired.
func (e *Engine) renewHolds(ctx context.Context) {
	tick := time.NewTicker(e.t.holdRenew)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		e.renewHoldsOnce(ctx)
	}
}

// renewHoldsOnce renews every hold this instance holds.
func (e *Engine) renewHoldsOnce(ctx context.Context) {
	e.holdsMu.Lock()
	keys := make([]holdKey, 0, len(e.holds))
	for k := range e.holds {
		keys = append(keys, k)
	}
	e.holdsMu.Unlock()
	for _, k := range keys {
		hs := e.holdRef(k)
		hs.mu.Lock()
		if hs.count > 0 {
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := e.st.RenewHold(rctx, k.id, k.kind, e.id, e.t.holdLease); err != nil && ctx.Err() == nil {
				e.rl.warn("hold", "cannot renew a hold", "sandbox", k.id, "kind", k.kind, "err", err)
			}
			cancel()
		}
		hs.mu.Unlock()
		e.holdUnref(k, hs)
	}
}

// HoldProcess holds the sandbox awake for a background process that
// process_start started: until timeout plus a slack has passed, or, for
// timeout 0, without a limit until the process ends, is stopped or a
// transition ends the sandbox's processes.
func (e *Engine) HoldProcess(ctx context.Context, id podid.ID, processID string, timeout time.Duration) error {
	lease := time.Duration(0)
	if timeout > 0 {
		lease = timeout + e.t.processSlack
	}
	return e.st.HoldProcess(ctx, id, processID, lease)
}

// ReleaseProcess ends a background process's hold: it was stopped, or a
// listing showed it not running.
func (e *Engine) ReleaseProcess(ctx context.Context, id podid.ID, processID string) error {
	_, err := e.st.ReleaseProcessHold(ctx, id, processID)
	return err
}
