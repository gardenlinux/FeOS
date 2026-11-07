// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"fmt"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// A sandbox's security groups change by a move (ADR 0035): its pod is
// hibernated and restored, which gives it new addresses, and the restore's
// begin makes its template of its new groups (store.ApplySandboxGroups),
// which RestorePod then sends (restoreRequest). A member therefore never
// joins a group while its pod runs. The environment controller decides
// when a sandbox moves and restores one that was running; the engine
// does the hibernation.

// HibernateToMove hibernates a sandbox whose groups changed
// (groups_pending) and whose pod is running, paused, stopped or failed, on
// its node (trigger move: never abandoned by a resume, no hold checks, the
// mark rechecked in the begin), and waits for it. It reports whether the
// sandbox is hibernated now; false, nil when it has nothing to move (no
// mark, no pod, an instance or a job's pod). An op in flight is
// ErrBusy: the caller tries again later. The op runs on the engine's
// context, not ctx: a caller that goes away stops waiting only.
func (e *Engine) HibernateToMove(ctx context.Context, id podid.ID) (bool, error) {
	row, err := e.st.GetSandbox(ctx, id)
	if err != nil {
		return false, err
	}
	if !row.GroupsPending || !row.IsOwn() || !movable(row.ClientState) {
		return false, nil
	}
	if row.Op != "" {
		return false, errorf(model.ErrBusy, "sandbox %s: an op %s is in flight", id, row.Op)
	}
	h, err := e.runner.start(ctx, id, OpIntent{Kind: OpHibernate, Trigger: TriggerMove, Target: row.Node,
		Incarnation: row.Incarnation}, false)
	if err != nil {
		return false, err
	}
	res, err := h.Wait(ctx)
	if err != nil {
		return false, err
	}
	if h.kind != OpHibernate {
		// This instance's op of another kind, joined: the next pass looks
		// at the sandbox again.
		return false, errorf(model.ErrBusy, "sandbox %s: an op %s was in flight", id, h.kind)
	}
	if res.Err != nil {
		return false, res.Err
	}
	if res.State != model.StateHibernated {
		return false, fmt.Errorf("hibernate on node %s for a move: %s", h.target, describe(res))
	}
	return true, nil
}
