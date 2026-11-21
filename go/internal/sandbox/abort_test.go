// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// TestNoNodeCallCancelledByCaller runs every op against a node that cuts
// a mutating call whose request is cancelled (today's feosd), with
// callers that give up early: the ops' calls run on regardless.
func TestNoNodeCallCancelledByCaller(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1", func(c *testnode.Config) { c.AbortOnCancel = true })
	e := h.eng()
	sb := h.sandbox(512)
	// giveUp makes method slow, runs f, and cancels f's context once the
	// node got the op's call: the caller hangs up while the node works.
	giveUp := func(method string, f func(ctx context.Context) error) {
		t.Helper()
		n.Script().SetDelay(method, 700*time.Millisecond)
		arrived := make(chan struct{}, 1)
		n.Script().SetHook(method, func(context.Context, string, any) error {
			select {
			case arrived <- struct{}{}:
			default:
			}
			return nil
		})
		defer n.Script().SetHook(method, nil)
		ctx, cancel := context.WithCancel(bg)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- f(ctx) }()
		select {
		case <-arrived:
		case err := <-done:
			t.Fatalf("%s: returned before the node got the call: %v", method, err)
		case <-time.After(15 * time.Second):
			t.Fatalf("%s: the node got no call", method)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("a caller that gave up got %v", err)
		}
	}
	ensure := func(ctx context.Context) error {
		_, err := e.Ensure(ctx, sb.ID)
		return err
	}
	op := func(kind OpKind) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			hd, err := e.runner.Start(ctx, sb.ID, OpIntent{Kind: kind, Trigger: TriggerExplicit, Target: "n1",
				Incarnation: sb.Incarnation})
			if err != nil {
				return err
			}
			_, err = hd.Wait(ctx)
			return err
		}
	}

	giveUp(podv1.PodService_CreatePod_FullMethodName, ensure)
	h.waitState(sb.ID, model.StateRunning, 15*time.Second)
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)

	// A cancelled SuspendPod would leave the record RUNNING.
	giveUp(podv1.PodService_SuspendPod_FullMethodName, op(OpPause))
	h.waitState(sb.ID, model.StatePaused, 15*time.Second)
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_SUSPENDED)

	giveUp(podv1.PodService_ResumePod_FullMethodName, ensure)
	h.waitState(sb.ID, model.StateRunning, 15*time.Second)
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)

	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	giveUp(podv1.PodService_HibernatePod_FullMethodName, op(OpHibernate))
	h.waitState(sb.ID, model.StateHibernated, 15*time.Second)

	// A REST client that disconnects during a slow RestorePod: the restore
	// completes and the sandbox runs (a cancelled one would leave the pod
	// PENDING without a bring-up).
	giveUp(podv1.PodService_RestorePod_FullMethodName, ensure)
	h.waitState(sb.ID, model.StateRunning, 15*time.Second)
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)

	if err := n.Script().FailPod(sb.ID, "crashed"); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StateFailed, 10*time.Second)
	giveUp(podv1.PodService_StartPod_FullMethodName, ensure)
	h.waitState(sb.ID, model.StateRunning, 15*time.Second)

	// The tombstone of a delete: its DeletePod runs detached too.
	n.Script().SetDelay(podv1.PodService_DeletePod_FullMethodName, 700*time.Millisecond)
	if _, _, err := e.Delete(bg, testProject, sb.ID); err != nil {
		t.Fatal(err)
	}
	h.noPod("n1", sb.ID)
}
