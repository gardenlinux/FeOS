// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"testing"
	"time"

	"github.com/google/uuid"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// serviceInstance inserts an instance row of an image service of env,
// whose pod has model.ServiceStopGrace, and brings it up.
func (h *harness) serviceInstance(env uuid.UUID) store.Sandbox {
	h.t.Helper()
	sb := h.instance(env, plainTemplate(env), func(n *store.NewSandbox) {
		n.LogLabels = map[string]string{model.LogKind: "service", model.LogObject: "svc"}
	})
	h.ensure(sb.ID)
	return h.row(sb.ID)
}

// StopPod stops a running instance's pod for good: while the node's
// StopPod lets the containers drain, the row stays running with the op
// stop, booked, its description saying so and its deadline the stop's
// and the pod's grace; the stop's end leaves it stopped without a booking
// and wakes env-reconcile. A row with an op in flight is busy, one that
// does not run is refused.
func TestStopPod(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	env := h.environment()
	sb := h.serviceInstance(env)
	if g := sb.StopGrace(); g != model.ServiceStopGrace {
		t.Fatalf("a service's instance has the stop grace %v", g)
	}
	hints, unsubscribe := h.d.Subscribe(db.ChanLeader)
	defer unsubscribe()
	stop := h.watchCalls("n1", podv1.PodService_StopPod_FullMethodName, true)
	var before time.Time
	h.scalar(&before, "SELECT now()")
	if err := h.eng().StopPod(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, stop.arrived, "the stop's StopPod")
	row := h.row(sb.ID)
	if row.Op != string(OpStop) || row.ClientState != model.StateRunning || row.OpFromState != model.StateRunning ||
		row.StatusDescription != "stopping: its containers have SIGTERM and 660 s to exit" {
		t.Fatalf("row while it stops %+v", row)
	}
	if want := before.Add(OpDeadline(OpStop, sb.MemoryMiB) + model.ServiceStopGrace); row.OpDeadline.Before(want) {
		t.Fatalf("the stop's deadline %v is before %v", row.OpDeadline, want)
	}
	if b, ok := h.booking(sb.ID); !ok || b.Node != "n1" {
		t.Fatal("a pod that stops is not booked")
	}
	if row.Address == "" || row.Address != sb.Address {
		t.Fatalf("a pod that stops has the address %q, had %q", row.Address, sb.Address)
	}
	wantErr(t, h.eng().StopPod(bg, sb.ID), model.ErrBusy, "in flight")
	drain(hints)
	stop.release()
	row = h.waitState(sb.ID, model.StateStopped, 10*time.Second)
	if row.Op != "" {
		t.Fatalf("row after the stop %+v", row)
	}
	if _, ok := h.booking(sb.ID); ok {
		t.Fatal("a stopped pod stays booked")
	}
	expectHint(t, hints, store.HintEnvReconcile)
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_STOPPED)
	d := h.activityNamed(sb.ID, activity.KindLifecycleStart, "stop on node n1")
	if d["trigger"] != string(TriggerStop) {
		t.Fatalf("start %v", d)
	}

	// Not running: refused.
	wantErr(t, h.eng().StopPod(bg, sb.ID), model.ErrPrecondition, "is stopped")
	wantErr(t, h.eng().StopPod(bg, podid.Mint()), model.ErrNotFound, "")
}

// A stop whose owner died is taken over by the leader's stale ops: its
// StopPod waits for the stop the node runs already, and the row, running
// and booked meanwhile, ends stopped.
func TestStopTakenOver(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	env := h.environment()
	sb := h.serviceInstance(env)
	h.settle(sb.ID)
	h.deadInstance("dead")
	h.opOfDeadOwner(sb, OpStop, "dead")
	if row := h.row(sb.ID); row.ClientState != model.StateRunning {
		t.Fatalf("a stop holds the row %s", row.ClientState)
	}
	stop := h.watchCalls("n1", podv1.PodService_StopPod_FullMethodName, true)
	if err := h.eng().staleOps(bg); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, stop.arrived, "the taken-over stop's StopPod")
	row := h.row(sb.ID)
	if row.Op != string(OpStop) || row.OpOwner != "i1" || row.ClientState != model.StateRunning {
		t.Fatalf("not taken over: %+v", row)
	}
	if _, ok := h.booking(sb.ID); !ok {
		t.Fatal("a pod that stops is not booked")
	}
	stop.release()
	h.waitState(sb.ID, model.StateStopped, 10*time.Second)
}

// A stop whose StopPod finds the VM busy leaves the pod running: the row
// is running again, without its op, and says that the stop is postponed.
func TestStopBusyVM(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	env := h.environment()
	sb := h.serviceInstance(env)
	h.failWith("n1", podv1.PodService_StopPod_FullMethodName, errVMBusy)
	if err := h.eng().StopPod(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	var row store.Sandbox
	eventually(t, 10*time.Second, "the stop ended", func() bool {
		row = h.row(sb.ID)
		return row.Op == ""
	})
	wantPostponed(t, row, model.StateRunning, "stop")
	if _, ok := h.booking(sb.ID); !ok {
		t.Fatal("a running pod is not booked")
	}
}
