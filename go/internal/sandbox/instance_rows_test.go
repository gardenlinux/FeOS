// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
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

// plainTemplate is an instance's template without secrets: the env group
// and a command.
func plainTemplate(env uuid.UUID) *model.PodTemplate {
	return &model.PodTemplate{Command: []string{"/bin/svc"}, SecurityGroups: []string{model.EnvGroupID(env)},
		Labels: map[string]string{model.LabelObject: "service/svc"}}
}

// sequence records the prepare hook's calls and the node's pod calls in
// the order they happen.
type sequence struct {
	mu    sync.Mutex
	steps []string
	fail  map[OpKind]error
	kinds []OpKind
}

func (s *sequence) add(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, step)
}

func (s *sequence) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.steps
	s.steps = nil
	return out
}

func (s *sequence) failOn(kind OpKind, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail == nil {
		s.fail = map[OpKind]error{}
	}
	s.fail[kind] = err
}

func (s *sequence) hook(_ context.Context, node string, sb store.Sandbox, kind OpKind) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, "hook "+string(kind)+" "+node)
	s.kinds = append(s.kinds, kind)
	if sb.PodTemplate == nil {
		return errors.New("the hook ran for a row without a template")
	}
	return s.fail[kind]
}

// recordCalls records the pod calls of node that bring a pod up or stop
// it into seq.
func (h *harness) recordCalls(node string, seq *sequence) {
	for _, m := range []string{podv1.PodService_CreatePod_FullMethodName, podv1.PodService_StartPod_FullMethodName,
		podv1.PodService_ResumePod_FullMethodName, podv1.PodService_RestorePod_FullMethodName,
		podv1.PodService_StopPod_FullMethodName, podv1.PodService_SuspendPod_FullMethodName,
		podv1.PodService_HibernatePod_FullMethodName, podv1.PodService_DeletePod_FullMethodName} {
		name := m[strings.LastIndex(m, "/")+1:]
		h.node(node).Script().SetHook(m, func(context.Context, string, any) error {
			seq.add(name)
			return nil
		})
	}
}

func wantSteps(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("steps %q, want %q", got, want)
	}
}

// The prepare hook runs for a templated row before the pod call of a
// create, resume, restore and start, inside a repair before its pod
// calls, and never before a pause or hibernate, or for a row without a
// template.
func TestPrepareHookOrder(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	seq := &sequence{}
	h.eng().SetPrepareHook(seq.hook)
	h.recordCalls("n1", seq)
	env := h.environment()
	sb := h.instance(env, plainTemplate(env))

	h.ensure(sb.ID)
	wantSteps(t, seq.take(), "hook create n1", "CreatePod")
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	wantSteps(t, seq.take(), "SuspendPod")
	h.ensure(sb.ID)
	wantSteps(t, seq.take(), "hook resume n1", "ResumePod")
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	h.startOp(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1"})
	wantSteps(t, seq.take(), "SuspendPod", "HibernatePod")
	h.ensure(sb.ID)
	wantSteps(t, seq.take(), "hook restore n1", "RestorePod")
	if _, err := h.pods("n1").StopPod(bg, &podv1.StopPodRequest{PodId: sb.ID.String()}); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StateStopped, 10*time.Second)
	seq.take()
	h.ensure(sb.ID)
	wantSteps(t, seq.take(), "hook start n1", "StartPod")
	if err := h.eng().Restart(bg, sb.ID, "container main exited with code 1"); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StateRunning, 10*time.Second)
	wantSteps(t, seq.take(), "hook repair n1", "StopPod", "StartPod")

	// A sandbox without a template is never prepared.
	plain := h.running(512)
	h.startOp(plain.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	h.ensure(plain.ID)
	wantSteps(t, seq.take(), "CreatePod", "SuspendPod", "ResumePod")
}

// A prepare hook that fails before a create sends nothing: the row is
// pending again with the hook's error as the op's error and description.
// An error that may pass leaves the node to the next try; one that says
// the node cannot take the pod (ErrNodeUnfit) avoids it on an unpinned
// create; a pinned create does not avoid it, and a pinned row is placed
// on its node whatever it avoids.
func TestPrepareHookFailureRollsBack(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	seq := &sequence{}
	h.eng().SetPrepareHook(seq.hook)
	h.recordCalls("n1", seq)
	env := h.environment()
	seq.failOn(OpCreate, errors.New("security group env-x: unavailable"))

	sb := h.instance(env, plainTemplate(env))
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err := h.eng().Ensure(ctx, sb.ID)
	wantErr(t, err, nil, "create on node n1 failed: prepare node n1 for create: security group env-x: unavailable")
	row := h.row(sb.ID)
	wantState(t, row, model.StatePending)
	if row.StatusDescription != "prepare node n1 for create: security group env-x: unavailable; nothing was sent" ||
		len(row.AvoidNodes) != 0 || row.Node != "" {
		t.Fatalf("row after the failed hook %q %v %q", row.StatusDescription, row.AvoidNodes, row.Node)
	}
	if _, ok := h.booking(sb.ID); ok {
		t.Fatal("a rolled back create is booked")
	}
	wantSteps(t, seq.take(), "hook create n1")
	h.activityNamed(sb.ID, activity.KindLifecycleEnd, "create on node n1: pending")

	// The node cannot take the pod: avoided from now on.
	unfit := fmt.Errorf("volume data: %w: no space left", ErrNodeUnfit)
	seq.failOn(OpCreate, unfit)
	_, err = h.eng().Ensure(ctx, sb.ID)
	wantErr(t, err, ErrNodeUnfit, "create on node n1 failed: prepare node n1 for create: "+unfit.Error())
	row = h.row(sb.ID)
	wantState(t, row, model.StatePending)
	if row.StatusDescription != "prepare node n1 for create: "+unfit.Error()+"; nothing was sent; placed elsewhere on the next use" ||
		!slices.Equal(row.AvoidNodes, []string{"n1"}) || row.Node != "" {
		t.Fatalf("row after the unfit node %q %v %q", row.StatusDescription, row.AvoidNodes, row.Node)
	}
	wantSteps(t, seq.take(), "hook create n1")

	// Pinned: the node is not avoided, and a pinned row passes the node it
	// avoids.
	pinned := h.instance(env, plainTemplate(env), func(n *store.NewSandbox) { n.PinNode = "n1" })
	_, err = h.eng().Ensure(ctx, pinned.ID)
	wantErr(t, err, ErrNodeUnfit, "prepare node n1 for create")
	if row := h.row(pinned.ID); len(row.AvoidNodes) != 0 ||
		row.StatusDescription != "prepare node n1 for create: "+unfit.Error()+"; nothing was sent" {
		t.Fatalf("pinned row %q %v", row.StatusDescription, row.AvoidNodes)
	}
	seq.failOn(OpCreate, nil)
	h.exec("UPDATE sandboxes SET avoid_nodes = '{n1}' WHERE id = $1", pinned.ID)
	h.ensure(pinned.ID)
	if row := h.row(pinned.ID); row.Node != "n1" || row.ClientState != model.StateRunning {
		t.Fatalf("pinned row %+v", row)
	}
	// The unpinned one has no node left to go to.
	_, err = h.eng().Ensure(ctx, sb.ID)
	wantErr(t, err, model.ErrNoCapacity, "n1: a create failed here")
	seq.take()

	// A hook that fails before a resume leaves the row paused.
	h.startOp(pinned.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	seq.failOn(OpResume, errors.New("no group"))
	_, err = h.eng().Ensure(ctx, pinned.ID)
	wantErr(t, err, nil, "resume on node n1 failed: prepare node n1 for resume: no group")
	if row := h.row(pinned.ID); row.ClientState != model.StatePaused ||
		row.StatusDescription != "prepare node n1 for resume: no group; nothing was sent" {
		t.Fatalf("row after a failed resume hook %s %q", row.ClientState, row.StatusDescription)
	}
	h.podState("n1", pinned.ID, podv1.PodState_POD_STATE_SUSPENDED)
	wantSteps(t, seq.take(), "SuspendPod", "hook resume n1")
}

// A prepare hook whose node went away (its error wraps
// model.ErrNodeUnavailable, as nodelink's does after its wait) never
// avoids the node, not even when it says unfit as well: on a single node
// the next Ensure places the row there again and its pod runs.
func TestPrepareHookNodeUnavailable(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	seq := &sequence{}
	h.eng().SetPrepareHook(seq.hook)
	h.recordCalls("n1", seq)
	env := h.environment()
	sb := h.instance(env, plainTemplate(env))
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	for _, cause := range []error{
		fmt.Errorf("security group %s: %w", model.EnvGroupID(env), model.ErrNodeUnavailable),
		fmt.Errorf("volume data: %w: %w", ErrNodeUnfit, model.ErrNodeUnavailable),
	} {
		seq.failOn(OpCreate, cause)
		_, err := h.eng().Ensure(ctx, sb.ID)
		wantErr(t, err, model.ErrNodeUnavailable, "create on node n1 failed: prepare node n1 for create: "+cause.Error())
		row := h.row(sb.ID)
		wantState(t, row, model.StatePending)
		if row.StatusDescription != "prepare node n1 for create: "+cause.Error()+"; nothing was sent" ||
			len(row.AvoidNodes) != 0 {
			t.Fatalf("row after %q: %q %v", cause, row.StatusDescription, row.AvoidNodes)
		}
		wantSteps(t, seq.take(), "hook create n1")
	}
	seq.failOn(OpCreate, nil)
	if lease := h.ensure(sb.ID); lease.Node != "n1" {
		t.Fatalf("lease %+v", lease)
	}
	wantState(t, h.row(sb.ID), model.StateRunning)
	wantSteps(t, seq.take(), "hook create n1", "CreatePod")
}

// A node that is no longer connected when the op gets its connection
// rolls the create back without a cause, before the prepare hook could
// be called: the description names the missing connection, and the node
// is not avoided.
func TestPrepareHookNeedsTheConnection(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	seq := &sequence{}
	h.eng().SetPrepareHook(seq.hook)
	h.recordCalls("n1", seq)
	env := h.environment()
	sb := h.instance(env, plainTemplate(env))
	connected := func() bool { return h.ins[0].srv.Connected("n1") }
	var once sync.Once
	gone := make(chan bool, 1)
	h.eng().connHook = func(*OpHandle) error {
		once.Do(func() {
			// The node goes away between the begin's claim and the op's
			// connection.
			n.Partition(true)
			deadline := time.Now().Add(10 * time.Second)
			for connected() && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			gone <- !connected()
		})
		return nil
	}
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err := h.eng().Ensure(ctx, sb.ID)
	if !<-gone {
		t.Fatal("n1 stayed connected")
	}
	wantErr(t, err, model.ErrNodeUnavailable, "create on node n1 failed: node n1 is not connected to instance i1")
	row := h.row(sb.ID)
	wantState(t, row, model.StatePending)
	if row.StatusDescription != "node n1 is not connected to instance i1; nothing was sent" || len(row.AvoidNodes) != 0 {
		t.Fatalf("row after the lost connection %q %v", row.StatusDescription, row.AvoidNodes)
	}
	wantSteps(t, seq.take())

	// Back: the next Ensure prepares the node and creates the pod there.
	n.Partition(false)
	eventually(t, 20*time.Second, "n1 back", connected)
	h.ensure(sb.ID)
	wantSteps(t, seq.take(), "hook create n1", "CreatePod")
}

// A secret that does not resolve fails a create before anything reaches
// the node, the hook included; the node is not avoided.
func TestResolverFailureRollsBack(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	seq := &sequence{}
	h.eng().SetPrepareHook(seq.hook)
	h.recordCalls("n1", seq)
	env := h.environment()
	sec := &secrets{fail: errors.New("the secret does not open")}
	h.eng().SetSecretResolver(sec.resolve)
	sb := h.instance(env, webTemplate(env))
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err := h.eng().Ensure(ctx, sb.ID)
	wantErr(t, err, nil, "create on node n1 failed: build the pod spec for create: env ")
	wantErr(t, err, nil, "the secret does not open")
	row := h.row(sb.ID)
	wantState(t, row, model.StatePending)
	if !strings.HasPrefix(row.StatusDescription, "build the pod spec for create: env ") ||
		!strings.HasSuffix(row.StatusDescription, "the secret does not open; nothing was sent") || len(row.AvoidNodes) != 0 {
		t.Fatalf("row %q %v", row.StatusDescription, row.AvoidNodes)
	}
	wantSteps(t, seq.take())

	// Without a resolver at all.
	h.eng().SetSecretResolver(nil)
	_, err = h.eng().Ensure(ctx, sb.ID)
	wantErr(t, err, nil, "no secret resolver")
	// Once it resolves, the create goes.
	h.eng().SetSecretResolver(webSecrets(env).resolve)
	h.ensure(sb.ID)
	wantSteps(t, seq.take(), "hook create n1", "CreatePod")
}

// A prepare hook that fails inside a repair counts as sent and as no call
// made: no pod call is made, the observation decides (the pod still
// runs), the description says that the repair was not done, and the
// process holds survive; a repair that restarts the pod ends them.
func TestPrepareHookFailureInRepair(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	seq := &sequence{}
	h.eng().SetPrepareHook(seq.hook)
	h.recordCalls("n1", seq)
	// A sandbox: a tenant's row with a template, held by its background
	// processes.
	id := podid.Mint()
	sb := h.instance(h.boxEnv(testTenant), &model.PodTemplate{Command: []string{"/bin/sh"},
		SecurityGroups: []string{model.SandboxGroupID(id)}}, func(n *store.NewSandbox) { n.ID, n.Instance = id, false })
	h.ensure(sb.ID)
	seq.take()
	if err := h.eng().HoldProcess(bg, sb.ID, "p1", 0); err != nil {
		t.Fatal(err)
	}
	processHolds := func() int {
		n := 0
		for _, hd := range h.holds(sb.ID) {
			if hd.Kind == model.HoldProcess {
				n++
			}
		}
		return n
	}

	seq.failOn(OpRepair, errors.New("no group"))
	if err := h.eng().Restart(bg, sb.ID, "exited"); err != nil {
		t.Fatal(err)
	}
	h.activityNamed(sb.ID, activity.KindLifecycleEnd, "repair on node n1: running")
	row := h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	if row.StatusDescription != "repair not done: prepare node n1 for repair: no group" {
		t.Fatalf("description after the failed repair %q", row.StatusDescription)
	}
	wantSteps(t, seq.take(), "hook repair n1")
	if n := processHolds(); n != 1 {
		t.Fatalf("%d process holds after a repair that made no call, want 1", n)
	}

	// A restart that happens ends the processes, and their holds.
	seq.failOn(OpRepair, nil)
	if err := h.eng().Restart(bg, sb.ID, "exited again"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the restart's end", func() bool {
		row := h.row(sb.ID)
		return row.Op == "" && row.ClientState == model.StateRunning && row.StatusDescription == ""
	})
	wantSteps(t, seq.take(), "hook repair n1", "StopPod", "StartPod")
	if n := processHolds(); n != 0 {
		t.Fatalf("%d process holds after a restart, want 0", n)
	}
}

// Restart restarts a running row's pod (StopPod and StartPod) with its
// reason as the description while it runs; it refuses a row that does
// not run, and one with an op in flight.
func TestRestart(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	seq := &sequence{}
	h.recordCalls("n1", seq)
	sb := h.running(512)
	seq.take()
	stop := h.watchCalls("n1", podv1.PodService_StopPod_FullMethodName, true)
	if err := h.eng().Restart(bg, sb.ID, "container main exited with code 2"); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, stop.arrived, "the restart's StopPod")
	row := h.row(sb.ID)
	if row.Op != string(OpRepair) || row.ClientState != model.StateCreating || row.StatusDescription != "container main exited with code 2" {
		t.Fatalf("row while restarting %+v", row)
	}
	// Another restart while it runs is busy.
	wantErr(t, h.eng().Restart(bg, sb.ID, "again"), model.ErrBusy, "in flight")
	stop.release()
	row = h.waitState(sb.ID, model.StateRunning, 10*time.Second)
	if row.StatusDescription != "" {
		t.Fatalf("description after the restart %q", row.StatusDescription)
	}
	d := h.activityNamed(sb.ID, activity.KindLifecycleStart, "repair on node n1")
	if d["trigger"] != string(TriggerRestart) || d["reason"] != "container main exited with code 2" {
		t.Fatalf("start %v", d)
	}
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)

	// Not running: refused.
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	wantErr(t, h.eng().Restart(bg, sb.ID, "x"), model.ErrPrecondition, "is paused")
	wantErr(t, h.eng().Restart(bg, podid.Mint(), "x"), model.ErrNotFound, "")
}

// The pin gate: a create and a start go only to the pinned node; a restore
// goes anywhere (the checkpoint carries the volume).
func TestPinGate(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	h.addNode("n2")
	env := h.environment()
	// Binpack would put a small pod beside a big one; the pin wins.
	big := h.running(8192)
	other := map[string]string{"n1": "n2", "n2": "n1"}[big.Node]
	pinned := h.instance(env, plainTemplate(env), func(n *store.NewSandbox) { n.PinNode = other })
	h.ensure(pinned.ID)
	if row := h.row(pinned.ID); row.Node != other {
		t.Fatalf("pinned to %s, placed on %s", other, row.Node)
	}

	// Pinned to a node that does not exist: no capacity, every node saying
	// why.
	nowhere := h.instance(env, plainTemplate(env), func(n *store.NewSandbox) { n.PinNode = "n9" })
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err := h.eng().Ensure(ctx, nowhere.ID)
	wantErr(t, err, model.ErrNoCapacity, "n1: pinned to node n9: its volume is there")
	wantErr(t, err, model.ErrNoCapacity, "n2: pinned to node n9: its volume is there")

	// A hibernated pinned row restores elsewhere.
	h.startOp(pinned.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: other})
	h.startOp(pinned.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: other})
	h.exec("UPDATE sandboxes SET pin_node = 'n9' WHERE id = $1", pinned.ID)
	h.ensure(pinned.ID)
	wantState(t, h.row(pinned.ID), model.StateRunning)

	// A row restored elsewhere and stopped there before its pin moved
	// starts on its own node, where its pod and volume are: the pin gates
	// creates only.
	restored := h.row(pinned.ID).Node
	if _, err := h.pods(restored).StopPod(bg, &podv1.StopPodRequest{PodId: pinned.ID.String()}); err != nil {
		t.Fatal(err)
	}
	h.waitState(pinned.ID, model.StateStopped, 10*time.Second)
	h.ensure(pinned.ID)
	if row := h.row(pinned.ID); row.ClientState != model.StateRunning || row.Node != restored || row.PinNode != "n9" {
		t.Fatalf("after the start: %s on %s, pinned to %s", row.ClientState, row.Node, row.PinNode)
	}

	// The gate unit: create only.
	in := readyNode("n1")
	in.pin = "n2"
	if r := in.check(placementGates); r == nil || r.gate != gatePin {
		t.Fatalf("refusal %+v", r)
	}
	if r := in.check(admitGates); r != nil {
		t.Fatalf("admission refusal %+v", r)
	}
	sb := store.Sandbox{PinNode: "n2", AvoidNodes: []string{"n2"}}
	for kind, pin := range map[OpKind]string{OpCreate: "n2", OpStart: "", OpRestore: "", OpResume: ""} {
		g := h.eng().gateInput(sb, kind, store.Node{Name: "n1"})
		if g.pin != pin || g.avoid != nil {
			t.Fatalf("%s: pin %q avoid %v", kind, g.pin, g.avoid)
		}
	}
}

// Per-row idle: idle_after_s 0 never idles, n idles after n seconds, NULL
// after --idle-pause (never when it is 0).
func TestIdleAfterPerRow(t *testing.T) {
	t.Run("global off", func(t *testing.T) {
		h := newHarness(t)
		h.addNode("n1")
		env := h.environment()
		zero, one := 0, 1
		forever := h.instance(env, plainTemplate(env), func(n *store.NewSandbox) { n.IdleAfterS = &zero })
		soon := h.instance(env, plainTemplate(env), func(n *store.NewSandbox) { n.IdleAfterS = &one })
		global := h.running(512)
		for _, id := range []podid.ID{forever.ID, soon.ID} {
			h.ensure(id)
		}
		for _, id := range []podid.ID{forever.ID, soon.ID, global.ID} {
			h.settle(id)
			h.idleSince(id)
		}
		if err := h.eng().idlePause(bg); err != nil {
			t.Fatal(err)
		}
		h.waitState(soon.ID, model.StatePaused, 10*time.Second)
		never(t, 1500*time.Millisecond, "a row that never idles paused", func() bool {
			return h.row(forever.ID).ClientState != model.StateRunning || h.row(global.ID).ClientState != model.StateRunning
		})
		// The begin's recheck heeds the row's interval too.
		ok, err := h.st.IdleRecheck(bg, forever.ID, time.Hour)
		if err != nil || ok {
			t.Fatalf("recheck of a row that never idles: %v %v", ok, err)
		}
	})
	t.Run("global on", func(t *testing.T) {
		h := newHarness(t, func(c *Config) { c.IdlePause = time.Hour })
		h.addNode("n1")
		env := h.environment()
		minute := 60
		short := h.instance(env, plainTemplate(env), func(n *store.NewSandbox) { n.IdleAfterS = &minute })
		h.ensure(short.ID)
		global := h.running(512)
		for _, id := range []podid.ID{short.ID, global.ID} {
			h.settle(id)
			h.exec("UPDATE sandboxes SET last_active_at = now() - interval '10 minutes' WHERE id = $1", id)
		}
		if err := h.eng().idlePause(bg); err != nil {
			t.Fatal(err)
		}
		h.waitState(short.ID, model.StatePaused, 10*time.Second)
		wantState(t, h.row(global.ID), model.StateRunning)
		h.idleSince(global.ID)
		if err := h.eng().idlePause(bg); err != nil {
			t.Fatal(err)
		}
		h.waitState(global.ID, model.StatePaused, 10*time.Second)
	})
}

// The address of an instance's pod is written as the create's answer
// gives it, while the pod still comes up, with the env-groups hint in the
// same transaction; observations keep it, and a hibernation clears it.
// An ordinary sandbox's address is kept without a hint.
func TestAddressAndGroupsHint(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	env := h.environment()
	hints, unsubscribe := h.d.Subscribe(db.ChanLeader)
	defer unsubscribe()

	// Hold the bring-up: the pod stays PENDING, the create's op waits.
	release := make(chan struct{})
	h.node("n1").Script().SetBringUp(func(ctx context.Context, _ podid.ID) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	sb := h.instance(env, plainTemplate(env))
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(bg, 60*time.Second)
		defer cancel()
		_, err := h.eng().Ensure(ctx, sb.ID)
		done <- err
	}()
	var row store.Sandbox
	eventually(t, 10*time.Second, "the address written while the create runs", func() bool {
		row = h.row(sb.ID)
		return row.Address != "" && row.Op == string(OpCreate)
	})
	expectHint(t, hints, store.HintEnvGroups)
	info, err := h.pods("n1").GetPod(bg, &podv1.GetPodRequest{PodId: sb.ID.String()})
	if err != nil || info.GetIpv6Address() != row.Address || info.GetState() != podv1.PodState_POD_STATE_PENDING {
		t.Fatalf("pod %v %v, row address %q", info, err, row.Address)
	}
	close(release)
	h.node("n1").Script().SetBringUp(nil)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	addr := row.Address
	if row = h.row(sb.ID); row.Address != addr {
		t.Fatalf("address after the create %q, want %q", row.Address, addr)
	}

	// Hibernated: no address; restored: the new one, with the hint.
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	if row = h.row(sb.ID); row.Address != addr {
		t.Fatalf("a paused pod keeps its address: %q", row.Address)
	}
	drain(hints)
	h.startOp(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1"})
	if row = h.row(sb.ID); row.Address != "" {
		t.Fatalf("a hibernated pod has the address %q", row.Address)
	}
	expectHint(t, hints, store.HintEnvGroups)
	h.ensure(sb.ID)
	if row = h.row(sb.ID); row.Address == "" {
		t.Fatal("no address after the restore")
	}
	expectHint(t, hints, store.HintEnvGroups)

	// A sync that finds the pod gone clears it (this one, or the one that
	// the deletion's pod event triggers first).
	drain(hints)
	if _, err := h.pods("n1").DeletePod(bg, &podv1.DeletePodRequest{PodId: sb.ID.String()}); err != nil {
		t.Fatal(err)
	}
	if err := h.eng().Sync(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	if row = h.row(sb.ID); row.Address != "" || row.ClientState == model.StateRunning {
		t.Fatalf("after the pod went: %s %q", row.ClientState, row.Address)
	}
	expectHint(t, hints, store.HintEnvGroups)

	// A sandbox: the address, and the hint, as every pod is in its
	// environment's groups (ADR 0035).
	drain(hints)
	plain := h.running(512)
	if h.row(plain.ID).Address == "" {
		t.Fatal("a sandbox has no address")
	}
	expectHint(t, hints, store.HintEnvGroups)
}

func expectHint(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case p := <-ch:
			if p == want {
				return
			}
		case <-timer.C:
			t.Fatalf("no hint %q", want)
		}
	}
}

func drain(ch <-chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
