// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// ReconcileNode reconciles the registry with a node's inventory (§9
// "Reconcile"); it runs only on an instance connected to the node. It
// snapshots the rows on the node first, then lists the pods, so that a row
// that changed meanwhile is told apart from one the list contradicts:
// only rows unchanged since the snapshot and without an op are written,
// each in a transaction of its own that locks it and checks again. Every
// pod of ferund's that no row places there gets a tombstone; a listed pod
// of a row's incarnation is applied to the row as Sync does, adopting a
// lost or hibernated row whose pod the node still has; a row the list
// lacks is NOT_FOUND. Finally the node's reported inventory hash at the
// snapshot is recorded as the one this reconcile saw.
func (e *Engine) ReconcileNode(ctx context.Context, node string) error {
	if !e.conns.Connected(node) {
		return errorf(model.ErrNodeUnavailable, "node %s is not connected to instance %s", node, e.id)
	}
	rows, err := e.st.SandboxesOfNode(ctx, node)
	if err != nil {
		return fmt.Errorf("reconcile %s: snapshot: %w", node, err)
	}
	snap := make(map[podid.ID]store.Sandbox, len(rows))
	for _, r := range rows {
		snap[r.ID] = r
	}
	hash := ""
	if t, err := e.st.GetTelemetry(ctx, node); err == nil {
		hash = t.InventoryHash
	} else if !errors.Is(err, store.ErrNoRow) {
		return fmt.Errorf("reconcile %s: telemetry: %w", node, err)
	}
	pc, err := e.podClient(ctx, node)
	if err != nil {
		return fmt.Errorf("reconcile %s: %w", node, err)
	}
	list, err := pc.ListPods(ctx, &podv1.ListPodsRequest{})
	if err != nil {
		return fmt.Errorf("reconcile %s: list pods: %w", node, err)
	}
	listed := map[podid.ID]bool{}
	var errs []error
	for _, p := range list.GetPods() {
		if !isFerunPod(p) {
			continue
		}
		id, err := podid.Parse(p.GetPodId())
		if err != nil {
			continue
		}
		listed[id] = true
		if err := e.reconcilePod(ctx, node, id, p, snap); err != nil {
			errs = append(errs, err)
		}
	}
	for _, r := range rows {
		if r.Node != node || listed[r.ID] {
			continue
		}
		if err := e.reconcileMissing(ctx, node, r); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("reconcile %s: %w", node, err)
	}
	if err := e.st.InTx(ctx, func(tx *store.Store) error { return tx.SetReconciled(ctx, node, hash) }); err != nil {
		return fmt.Errorf("reconcile %s: %w", node, err)
	}
	return nil
}

// reconcilePod decides about one of ferund's pods the node listed.
func (e *Engine) reconcilePod(ctx context.Context, node string, id podid.ID, p *podv1.PodInfo, snap map[podid.ID]store.Sandbox) error {
	inc := podIncarnation(p)
	var events []activity.Event
	var keys []store.TombstoneKey
	err := e.st.InTx(ctx, func(tx *store.Store) error {
		events, keys = nil, nil
		row, err := tx.LockSandbox(ctx, id, store.ForUpdate)
		gone := errors.Is(err, model.ErrNotFound)
		if err != nil && !gone {
			return err
		}
		orphan := ""
		switch {
		case gone:
			orphan = "no sandbox has this pod"
		case row.Node != node && row.OpTarget != node:
			orphan = "its sandbox is placed elsewhere"
		case row.Incarnation != inc && row.Op == "":
			orphan = fmt.Sprintf("stale record of incarnation %s", inc)
		}
		if orphan != "" {
			t := store.Tombstone{PodID: id, Node: node, Incarnation: inc, Reason: orphan}
			if _, err := tx.UpsertTombstone(ctx, t); err != nil {
				return err
			}
			keys = []store.TombstoneKey{t.Key()}
			owner := ""
			if !gone {
				owner = row.Owner
			}
			events = []activity.Event{{SandboxID: id, ProjectID: owner, Node: node, Kind: activity.KindTombstone,
				Name:   fmt.Sprintf("tombstone on node %s: %s", node, orphan),
				Detail: map[string]any{"node": node, "incarnation": inc.String(), "reason": orphan}}}
			return nil
		}
		s, inSnap := snap[id]
		if row.Incarnation != inc || row.Node != node || row.Op != "" || !inSnap || s.Gen != row.Gen {
			return nil // changed since the snapshot, or an op runs: left to Sync
		}
		c, err := e.newChange(ctx, tx, row)
		if err != nil {
			return err
		}
		c.observe(node, observation{answered: true, info: p})
		if _, err := tx.DeleteTombstone(ctx, store.TombstoneKey{PodID: id, Node: node, Incarnation: inc}); err != nil {
			return err
		}
		if err := c.commit(ctx); err != nil {
			return err
		}
		events, keys = c.events, c.tombstoneKeys()
		return nil
	})
	if err != nil {
		return err
	}
	e.record(events...)
	e.processAfterCommit(afterCommit{tombstones: keys})
	return nil
}

// reconcileMissing applies NOT_FOUND to a row on the node that the list
// lacks, if it is unchanged since the snapshot and has no op.
func (e *Engine) reconcileMissing(ctx context.Context, node string, s store.Sandbox) error {
	var events []activity.Event
	err := e.st.InTx(ctx, func(tx *store.Store) error {
		events = nil
		row, err := tx.LockSandbox(ctx, s.ID, store.ForUpdate)
		if errors.Is(err, model.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.Gen != s.Gen || row.Op != "" || row.Node != node || row.Incarnation != s.Incarnation {
			return nil
		}
		c, err := e.newChange(ctx, tx, row)
		if err != nil {
			return err
		}
		c.observe(node, observation{answered: true})
		if err := c.commit(ctx); err != nil {
			return err
		}
		events = c.events
		return nil
	})
	if err != nil {
		return err
	}
	e.record(events...)
	return nil
}

// RegistryHash is the registry's inventory hash of a node: the lowercase
// hex SHA-256 of one line "<id> POD_STATE_RUNNING\n" or "<id>
// POD_STATE_SUSPENDED\n" per sandbox on the node that is running or paused
// without an op, sorted by ID: the formula of Telemetry.inventory_hash.
func (e *Engine) RegistryHash(ctx context.Context, node string) (string, error) {
	pods, err := e.st.SteadyPods(ctx, node)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, p := range pods {
		state := podv1.PodState_POD_STATE_RUNNING
		if p.State == model.StatePaused {
			state = podv1.PodState_POD_STATE_SUSPENDED
		}
		h.Write([]byte(p.ID.String() + " " + state.String() + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
