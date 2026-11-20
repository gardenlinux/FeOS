// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// tombRun is the processing of one tombstone on this instance.
type tombRun struct {
	done chan struct{}
	err  error
}

// The reasons a tombstone was not processed (yet).
var (
	errNotDue       = errors.New("tombstone not due yet")
	errNotConnected = errors.New("node not connected to this instance")
)

// processTombstone starts the processing of tombstone k, or joins the one
// in flight on this instance (single flight per tombstone). It always
// runs detached (§9 "Tombstones"): a caller that stops waiting ends its
// wait, never the DeletePod, which feosd would abort at an arbitrary
// point.
func (e *Engine) processTombstone(k store.TombstoneKey) *tombRun {
	e.tombsMu.Lock()
	if r := e.tombs[k]; r != nil {
		e.tombsMu.Unlock()
		return r
	}
	r := &tombRun{done: make(chan struct{})}
	e.tombs[k] = r
	e.tombsMu.Unlock()
	end := func(err error) {
		e.tombsMu.Lock()
		delete(e.tombs, k)
		e.tombsMu.Unlock()
		r.err = err
		close(r.done)
	}
	if !e.goBackground(func(ctx context.Context) { end(e.runTombstone(ctx, k)) }) {
		end(context.Canceled)
	}
	return r
}

// tombstoneInFlight reports whether this instance processes tombstone k.
func (e *Engine) tombstoneInFlight(k store.TombstoneKey) bool {
	e.tombsMu.Lock()
	defer e.tombsMu.Unlock()
	return e.tombs[k] != nil
}

// wait waits for the run, or ctx.
func (r *tombRun) wait(ctx context.Context) error {
	select {
	case <-r.done:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// processAfterCommit processes the tombstones a committed transaction of
// this instance wrote, at once and without waiting (best effort; not on a
// stopping instance, and not for a node it is not connected to: the
// leader's loop is the backstop).
func (e *Engine) processAfterCommit(ac afterCommit) {
	if len(ac.tombstones) == 0 || e.isStopping() {
		return
	}
	for _, k := range ac.tombstones {
		if e.conns.Connected(k.Node) {
			e.processTombstone(k)
		}
	}
}

// runTombstone is process(t) of §9: on a node this instance is connected
// to, once the tombstone is due, GetPod: a pod the node does not hold
// takes the tombstone with it; one of the tombstone's incarnation, or
// without an incarnation label, is deleted, and the tombstone with it; one
// of another incarnation is left, and only the tombstone goes; a pod that
// is not ferund's is never touched. A failed call counts an attempt; the
// tombstone stays for a later run.
func (e *Engine) runTombstone(bg context.Context, k store.TombstoneKey) error {
	qctx, cancel := context.WithTimeout(bg, 10*time.Second)
	t, due, err := e.st.DueTombstone(qctx, k)
	cancel()
	if errors.Is(err, store.ErrNoRow) {
		return nil
	}
	if err != nil {
		return err
	}
	if !due {
		return errNotDue
	}
	if !e.conns.Connected(k.Node) {
		return errNotConnected
	}
	gctx, gcancel := e.detached(bg, time.Now().Add(e.t.tombstoneGet))
	obs, err := e.getPod(gctx, k.Node, k.PodID)
	gcancel()
	if err != nil {
		return e.noteAttempt(bg, k, fmt.Errorf("get pod: %w", err))
	}
	if !obs.found() {
		return e.dropTombstone(bg, t, "the pod is gone")
	}
	inc := podIncarnation(obs.info)
	if !isFerunPod(obs.info) || (inc != k.Incarnation && inc != uuid.Nil) {
		return e.dropTombstone(bg, t, fmt.Sprintf("tombstone dropped: node %s holds incarnation %s", k.Node, incarnationText(obs.info)))
	}
	mem := int(obs.info.GetSpec().GetResources().GetMemoryMib())
	grace := time.Duration(obs.info.GetSpec().GetStopGraceSeconds()) * time.Second
	dctx, dcancel := e.detached(bg, time.Now().Add(tombstoneDeadline(e.t.tombstoneBase, mem, grace)))
	pc, err := e.podClient(dctx, k.Node)
	if err == nil {
		_, err = pc.DeletePod(dctx, &podv1.DeletePodRequest{PodId: k.PodID.String()})
	}
	dcancel()
	if err != nil && grpcCode(err) != codes.NotFound {
		return e.noteAttempt(bg, k, fmt.Errorf("delete pod: %w", err))
	}
	return e.dropTombstone(bg, t, "the pod was deleted")
}

// incarnationText names a pod's incarnation for a description.
func incarnationText(info *podv1.PodInfo) string {
	if s, ok := info.GetSpec().GetLabels()[LabelIncarnation]; ok {
		return activity.Clean(s)
	}
	return "none"
}

// dropTombstone deletes a processed tombstone, with an activity row.
func (e *Engine) dropTombstone(bg context.Context, t store.Tombstone, why string) error {
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	if _, err := e.st.DeleteTombstone(ctx, t.Key()); err != nil {
		return err
	}
	e.record(activity.Event{SandboxID: t.PodID, Node: t.Node, Kind: activity.KindTombstone,
		Name:   fmt.Sprintf("tombstone on node %s done: %s", t.Node, why),
		Detail: map[string]any{"node": t.Node, "incarnation": t.Incarnation.String(), "reason": t.Reason, "result": why}})
	return nil
}

// noteAttempt records a failed processing; the tombstone is tried again
// later.
func (e *Engine) noteAttempt(bg context.Context, k store.TombstoneKey, cause error) error {
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	if err := e.st.NoteTombstoneAttempt(ctx, k, callText(cause)); err != nil {
		e.rl.warn("tombstone", "cannot record a failed tombstone", "pod", k.PodID, "node", k.Node, "err", err)
	}
	e.rl.warn("tombstone", "a tombstone's pod could not be deleted; retrying later", "pod", k.PodID, "node", k.Node, "err", cause)
	return cause
}

// Delete removes a sandbox (§9 "Delete") in one transaction: a tombstone
// for every node a pod of its incarnation may be on (not processed before
// the deadline of a live op that may still make it there), the open run
// closed, the row deleted (its tokens, holds, watches, bindings and
// booking cascade). owner "" deletes any owner's. It returns the row as
// read and whether a tombstone was written; the new tombstones are
// processed at once when this instance can.
func (e *Engine) Delete(ctx context.Context, owner string, id podid.ID) (store.Sandbox, bool, error) {
	var row store.Sandbox
	var keys []store.TombstoneKey
	var events []activity.Event
	err := e.st.InTx(ctx, func(tx *store.Store) error {
		var err error
		row, err = tx.LockSandbox(ctx, id, store.ForUpdate)
		if errors.Is(err, model.ErrNotFound) || (err == nil && owner != "" && row.Owner != owner) {
			return errorf(model.ErrNotFound, "unknown sandbox %s", id)
		}
		if err != nil {
			return err
		}
		keys, events, err = e.deleteLocked(ctx, tx, row)
		return err
	})
	if err != nil {
		return store.Sandbox{}, false, err
	}
	e.record(events...)
	e.processAfterCommit(afterCommit{tombstones: keys})
	return row, len(keys) > 0, nil
}

// deleteLocked deletes the locked row of a sandbox, as Delete does, in
// the caller's transaction.
func (e *Engine) deleteLocked(ctx context.Context, tx *store.Store, row store.Sandbox) ([]store.TombstoneKey, []activity.Event, error) {
	makes := false
	switch OpKind(row.Op) {
	case OpCreate, OpStart, OpResume, OpRestore, OpRepair:
		live, err := tx.InstanceLive(ctx, row.OpOwner)
		if err != nil {
			return nil, nil, err
		}
		makes = live
	}
	var keys []store.TombstoneKey
	var events []activity.Event
	for _, n := range []string{row.Node, row.OpTarget, row.LostFrom} {
		if n == "" || containsKey(keys, n) {
			continue
		}
		t := store.Tombstone{PodID: row.ID, Node: n, Incarnation: row.Incarnation, Reason: "deleted"}
		if makes && n == row.OpTarget {
			t.NotBefore = row.OpDeadline
		}
		if _, err := tx.UpsertTombstone(ctx, t); err != nil {
			return nil, nil, err
		}
		keys = append(keys, t.Key())
		events = append(events, activity.Event{SandboxID: row.ID, TenantID: row.Owner, Node: n, Kind: activity.KindTombstone,
			Name:   fmt.Sprintf("tombstone on node %s: deleted", n),
			Detail: map[string]any{"node": n, "incarnation": row.Incarnation.String(), "reason": "deleted"}})
	}
	if row.RunID != "" {
		c := &change{e: e, before: row}
		events = append(events, c.runEnd(row.RunID, "deleted"))
	}
	if _, err := tx.DeleteSandbox(ctx, row.ID); err != nil {
		return nil, nil, err
	}
	if len(keys) > 0 {
		if err := tx.Notify(ctx, db.ChanLeader, hintTombstone); err != nil {
			return nil, nil, err
		}
	}
	events = append(events, activity.Event{SandboxID: row.ID, TenantID: row.Owner, Node: row.Node,
		Kind: activity.KindSandboxState, ParentSpanID: row.RunID, Name: fmt.Sprintf("%s → deleted", row.ClientState),
		Detail: map[string]any{"from": row.ClientState, "to": "deleted"}})
	return keys, events, nil
}

// DeleteTenant deletes a tenant as §11 says: its deletion is marked and
// its tokens go in a first transaction, then its sandboxes in batches of
// at most 256, each as Delete does, then the tenant row once it has no
// sandbox left. existed is false for an unknown tenant. The batches are
// not fenced: the row locks serialise them with the leader's loop, which
// finishes a deletion this call did not.
func (e *Engine) DeleteTenant(ctx context.Context, tenant string) (bool, error) {
	err := e.st.InTx(ctx, func(tx *store.Store) error {
		if _, err := tx.LockTenant(ctx, tenant, store.ForUpdate); err != nil {
			return err
		}
		_, err := tx.MarkTenantDeleting(ctx, tenant)
		return err
	})
	if errors.Is(err, store.ErrNoRow) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := e.deleteTenantSandboxes(ctx, tenant, nil); err != nil {
		return true, err
	}
	return true, nil
}

// deleteTenantSandboxes deletes a deleting tenant's sandboxes in batches,
// then the tenant. fence, when set, runs first in every transaction.
func (e *Engine) deleteTenantSandboxes(ctx context.Context, tenant string, fence func(tx *store.Store) error) error {
	for {
		var n int
		var keys []store.TombstoneKey
		var events []activity.Event
		err := e.st.InTx(ctx, func(tx *store.Store) error {
			n, keys, events = 0, nil, nil
			if fence != nil {
				if err := fence(tx); err != nil {
					return err
				}
			}
			rows, err := tx.LockSandboxesOfOwner(ctx, tenant, tenantBatch)
			if err != nil {
				return err
			}
			n = len(rows)
			for _, row := range rows {
				ks, evs, err := e.deleteLocked(ctx, tx, row)
				if err != nil {
					return err
				}
				keys, events = append(keys, ks...), append(events, evs...)
			}
			return nil
		})
		if err != nil {
			return err
		}
		e.record(events...)
		e.processAfterCommit(afterCommit{tombstones: keys})
		if n < tenantBatch {
			break
		}
	}
	return e.st.InTx(ctx, func(tx *store.Store) error {
		if fence != nil {
			if err := fence(tx); err != nil {
				return err
			}
		}
		_, err := tx.DeleteTenantIfEmpty(ctx, tenant)
		return err
	})
}

// tenantBatch is the most sandboxes one transaction of a tenant's
// deletion deletes.
const tenantBatch = 256

// tombstoneDeadline is the deadline of a tombstone's DeletePod of a pod of
// memoryMiB whose spec names the stop grace grace (0: feosd's 10 s, which
// base covers). feosd's delete waits for the pod's lock, which a suspend
// (an idle pause of the sandbox deleted meanwhile) holds for up to feosd's
// suspend budget; the delete's own steps then take base, the pod's grace
// for its containers to exit after SIGTERM, and 4 s per GiB. The deadline
// covers all of them, and pauseMargin for the call's way there and back,
// so that the DeletePod answers before it: one that ran out while it
// waited would be tried again only after noteAttempt's backoff.
func tombstoneDeadline(base time.Duration, memoryMiB int, grace time.Duration) time.Duration {
	return feosdSuspendBudget(memoryMiB) + pauseMargin + base + grace + 4*time.Second*gib(memoryMiB)
}
