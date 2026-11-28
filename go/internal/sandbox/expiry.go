// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// LoopExpiry is the leader loop that expires the sandboxes idle for their
// time (ADR 0065).
const LoopExpiry = "sandbox-expiry"

// expiryInterval is how often the expiry loop looks: an idle time is
// hours at least, so a minute late is nothing.
const expiryInterval = time.Minute

// expiry expires every sandbox whose idle time has passed, a batch at a
// time: each in a fenced transaction of its own.
func (e *Engine) expiry(ctx context.Context) error {
	for {
		rows, err := e.st.ExpiryCandidates(ctx, loopBatch)
		if err != nil {
			return fmt.Errorf("expiry: %w", err)
		}
		n := 0
		for _, row := range rows {
			ok, err := e.expire(ctx, row)
			if err != nil {
				return fmt.Errorf("expiry: sandbox %s: %w", row.ID, err)
			}
			if ok {
				n++
			}
		}
		if n > 0 {
			e.log.Info("sandboxes expired", "sandboxes", n)
		}
		if len(rows) < loopBatch || n == 0 {
			return nil
		}
	}
}

// expire expires row if it still may (the recheck on the locked row): its
// pod goes by tombstones as a delete's does, its run ends, its booking and
// process holds go, and the row stays, expired, with no node and no
// checkpoint. It reports whether the row expired.
func (e *Engine) expire(ctx context.Context, row store.Sandbox) (bool, error) {
	var keys []store.TombstoneKey
	var events []activity.Event
	expired := false
	err := e.fenced(ctx, func(tx *store.Store) error {
		before, err := tx.LockSandbox(ctx, row.ID, store.ForUpdate)
		if errors.Is(err, model.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if before.Incarnation != row.Incarnation {
			return nil
		}
		due, err := tx.ExpiryDue(ctx, before.ID)
		if err != nil || !due {
			return err
		}
		for _, n := range []string{before.Node, before.LostFrom} {
			if n == "" || containsKey(keys, n) {
				continue
			}
			t := store.Tombstone{PodID: before.ID, Node: n, Incarnation: before.Incarnation, Reason: "expired"}
			if _, err := tx.UpsertTombstone(ctx, t); err != nil {
				return err
			}
			keys = append(keys, t.Key())
			events = append(events, activity.Event{SandboxID: before.ID, ProjectID: before.Owner, Node: n,
				Kind: activity.KindTombstone, Name: fmt.Sprintf("tombstone on node %s: expired", n),
				Detail: map[string]any{"node": n, "incarnation": before.Incarnation.String(), "reason": "expired"}})
		}
		if before.RunID != "" {
			c := &change{e: e, before: before}
			events = append(events, c.runEnd(before.RunID, "expired"))
		}
		desc := fmt.Sprintf("expired on %s after %s idle; its disk and memory are gone",
			time.Now().UTC().Format("2006-01-02"), model.IdleTimeText(before.ExpiresAfterIdleS))
		after, err := tx.MarkSandboxExpired(ctx, before.ID, before.Incarnation, desc)
		if errors.Is(err, store.ErrNoRow) {
			keys, events = nil, nil
			return nil
		}
		if err != nil {
			return err
		}
		ev, err := e.syncBooking(ctx, tx, before, after, true)
		if err != nil {
			return err
		}
		events = append(events, ev...)
		events = append(events, activity.Event{SandboxID: after.ID, ProjectID: after.Owner, Node: before.Node,
			Kind: activity.KindSandboxState, Name: fmt.Sprintf("%s → %s", before.ClientState, after.ClientState),
			Detail: map[string]any{"reason": "expired", "idleSeconds": *before.ExpiresAfterIdleS,
				"lastActiveAt": before.LastActiveAt.UTC().Format(time.RFC3339)}})
		expired = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if expired {
		e.record(events...)
		e.processAfterCommit(afterCommit{tombstones: keys})
	}
	return expired, nil
}
