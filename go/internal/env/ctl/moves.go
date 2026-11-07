// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// A sandbox's groups change by a move (ADR 0035): PUT
// /api/v1/sandboxes/{id}/security-groups, and the migration of ADR 0035,
// write its groups and mark it (groups_pending). The begin of the create
// or restore that makes its pod next makes its template of them
// (store.ApplySandboxGroups), and the restore sends them. env-groups-moves
// moves the sandboxes whose pod runs under the old ones: it hibernates
// each (Engine.HibernateToMove, a hibernation no resume abandons) and
// restores one that was running, under a new address on any node. A
// paused, stopped or failed one stays hibernated until it is used. A
// sandbox without a pod (pending, hibernated, lost) needs nothing: its
// next create or restore takes its groups. Members thus never join a
// group while their pod runs, and a removal that arrives late opens
// nothing (addresses are never given twice).
//
// The loop reads the database only; each move runs in the pool (single
// flight per sandbox), its hibernation on the engine's context. A move
// that fails is tried again after a backoff; a leader that ends between
// the hibernation and the restore leaves the sandbox hibernated, and its
// next call restores it in its new groups.

// LoopMoves is the loop of the moves. Its name contains HintGroups, which
// store.SetSandboxGroups sends: a change wakes it.
const LoopMoves = LoopGroups + "-moves"

const (
	movesEvery = 5 * time.Second
	// movesBatch is how many sandboxes a pass looks at.
	movesBatch = 64
)

// moveKey is the pool's key of a sandbox's move.
func moveKey(id podid.ID) string { return "move/" + id.String() }

// movesLoop is env-groups-moves: it starts the move of each sandbox whose
// groups changed and whose pod is on a connected node with no op in
// flight (store.EnvCtlMoves), unless one runs or its backoff holds it.
func (c *Controller) movesLoop(ctx context.Context) error {
	if c.d.Conns == nil {
		return nil
	}
	nodes := c.d.Conns.Nodes()
	if len(nodes) == 0 {
		return nil
	}
	rows, err := c.st.EnvCtlMoves(ctx, nodes, movesBatch)
	if err != nil {
		return fmt.Errorf("moves: %w", err)
	}
	for _, sb := range rows {
		key := moveKey(sb.ID)
		if !c.cleanDue(key) {
			continue
		}
		c.pool.run(ctx, key, func(ctx context.Context) { c.move(ctx, sb) })
	}
	return nil
}

// move moves one sandbox, read as sb: hibernated, then restored when it
// was running.
func (c *Controller) move(ctx context.Context, sb store.Sandbox) {
	key := moveKey(sb.ID)
	hibernated, err := c.d.Engine.HibernateToMove(ctx, sb.ID)
	switch {
	case err != nil && (errors.Is(err, model.ErrBusy) || errors.Is(err, model.ErrPrecondition) ||
		errors.Is(err, model.ErrNotFound)):
		// An op came first, or the mark went: the next pass looks again.
		return
	case err != nil:
		if ctx.Err() == nil {
			c.cleanFailed(key)
			c.rl.warn("move/"+sb.ID.String(), "a sandbox's move to its new security groups failed; it is tried again",
				"sandbox", sb.ID, "node", sb.Node, "err", err)
		}
		return
	case !hibernated:
		return
	}
	c.cleanOK(key)
	c.log.Info("sandbox hibernated for its new security groups", "sandbox", sb.ID, "node", sb.Node,
		"groups", sb.SecurityGroups)
	if sb.ClientState != model.StateRunning {
		return
	}
	l, err := c.d.Engine.Ensure(ctx, sb.ID)
	if err != nil {
		if ctx.Err() == nil {
			c.rl.warn("move/"+sb.ID.String(), "a moved sandbox stays hibernated until it is used", "sandbox", sb.ID,
				"err", err)
		}
		return
	}
	c.log.Info("sandbox moved to its new security groups", "sandbox", sb.ID, "node", l.Node)
}
