// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"errors"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// The expiry loop (ADR 0065) expires a sandbox idle for longer than its
// idle time: its pod goes by a tombstone, its row stays, expired, without
// a node or a checkpoint, and Ensure answers ErrExpired. A sandbox
// without an idle time, one idle for less, and one that runs stay.
func TestExpiryLoop(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.HibernateAfter = time.Minute })
	h.addNode("n1")
	e := h.eng()
	pause := func() podid.ID {
		sb := h.running(512)
		h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
		h.waitState(sb.ID, model.StatePaused, 10*time.Second)
		return sb.ID
	}
	idle, never, recent := pause(), pause(), pause()
	running := h.running(512).ID
	hib := pause()
	h.startOp(hib, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1"})
	h.waitState(hib, model.StateHibernated, 10*time.Second)
	if h.row(hib).CheckpointHead == "" {
		t.Fatal("hibernated without a checkpoint")
	}
	h.exec(`UPDATE sandboxes SET expires_after_idle_s = 3600 WHERE id = ANY($1)`,
		[]podid.ID{idle, recent, running, hib})
	h.exec(`UPDATE sandboxes SET last_active_at = now() - interval '2 hours' WHERE id = ANY($1)`,
		[]podid.ID{idle, never, running, hib})

	if err := e.expiry(bg); err != nil {
		t.Fatal(err)
	}
	for _, id := range []podid.ID{idle, hib} {
		r := h.row(id)
		wantState(t, r, model.StateExpired)
		if r.Node != "" || r.CheckpointHead != "" || r.ExpiredAt.IsZero() {
			t.Fatalf("expired %s: node %q, head %q, expired at %v", id, r.Node, r.CheckpointHead, r.ExpiredAt)
		}
		if _, err := e.Ensure(bg, id); !errors.Is(err, model.ErrExpired) {
			t.Fatalf("ensure of an expired sandbox: %v", err)
		}
		if err := e.Pause(bg, id); err != nil {
			t.Fatalf("pause of an expired sandbox: %v", err)
		}
	}
	// The paused one's pod goes from its node.
	eventually(t, 10*time.Second, "the expired pod's tombstone was processed", func() bool {
		return len(h.tombstones(idle)) == 0
	})
	wantState(t, h.row(never), model.StatePaused)
	wantState(t, h.row(recent), model.StatePaused)
	wantState(t, h.row(running), model.StateRunning)
	if _, ok := h.row(never).ExpiresAt(); ok {
		t.Fatal("a sandbox without an idle time has an expiry")
	}
	if at, ok := h.row(recent).ExpiresAt(); !ok || time.Until(at) < 50*time.Minute {
		t.Fatalf("expires at %v, %v", at, ok)
	}
}
