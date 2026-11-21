// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	toolv1 "github.com/gardenlinux/feos/go/gen/feos/tool/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/testnode"
	"github.com/gardenlinux/feos/go/internal/tools/toolstest"
)

// Every error out of Acquire reaches the client as "ensure sandbox: <err>",
// and nothing is called on the node.
func TestEnsureSandboxWrapping(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	sb := env.NewSandbox()
	for _, tc := range []struct {
		err       error
		want      string
		retryable bool
	}{
		{model.ErrLost, "ensure sandbox: sandbox lost", false},
		{model.ErrNotFound, "ensure sandbox: no such sandbox", false},
		{fmt.Errorf("%w (retryable): no feasible node; n1: slots full (50 of 50)", model.ErrNoCapacity),
			"ensure sandbox: no capacity (retryable): no feasible node; n1: slots full (50 of 50)", true},
		{fmt.Errorf("%w: node n1 is down", model.ErrNodeUnavailable), "ensure sandbox: node unavailable: node n1 is down", true},
		{fmt.Errorf("instance i1 is stopping: %w", model.ErrStopping), "ensure sandbox: instance i1 is stopping: instance is stopping", true},
		{errors.New("resolve image alpine: 401 Unauthorized"), "ensure sandbox: resolve image alpine: 401 Unauthorized", false},
		{errors.New("create on node n1 failed: no space left"), "ensure sandbox: create on node n1 failed: no space left", false},
		{context.DeadlineExceeded, "ensure sandbox: context deadline exceeded", false},
		{hibernating{sb.ID}, "ensure sandbox: sandbox " + sb.ID.String() + " is hibernating; retry in a minute", true},
	} {
		env.Engine.FailAcquire(sb.ID, tc.err)
		res, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"true"}`), nil)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%v: got %v, want %q", tc.err, err, tc.want)
		}
		if !errors.Is(err, tc.err) {
			t.Errorf("%v: the cause is not kept", tc.err)
		}
		if Retryable(err) != tc.retryable {
			t.Errorf("%v: retryable %v", tc.err, Retryable(err))
		}
		if res.Text != "" {
			t.Errorf("output %q", res.Text)
		}
	}
	if _, err := env.PodState(bg, sb.ID); status.Code(err) != codes.NotFound {
		t.Errorf("a pod was made: %v", err)
	}

	// An unknown tool is refused before anything is acquired.
	_, err := r.Call(bg, sb, "nope", nil, nil)
	if err == nil || err.Error() != `unknown tool "nope"` {
		t.Errorf("unknown tool: %v", err)
	}
	if n := env.Engine.Calls("Acquire", sb.ID); n != 9 {
		t.Errorf("%d Acquires", n)
	}
}

// hibernating is the engine's answer for a sandbox whose hibernation did
// not end within Ensure's wait: a text of its own that is model.ErrBusy,
// and a retry after a minute.
type hibernating struct{ id podid.ID }

func (e hibernating) Error() string {
	return "sandbox " + e.id.String() + " is hibernating; retry in a minute"
}

func (e hibernating) Unwrap() error { return model.ErrBusy }

func (e hibernating) RetryAfter() time.Duration { return time.Minute }

// An op in flight that Ensure does not wait for (a hibernation) is worth
// a retry, however it is wrapped.
func TestRetryableBusy(t *testing.T) {
	for _, err := range []error{model.ErrBusy, hibernating{podid.Mint()},
		fmt.Errorf("ensure sandbox: %w", hibernating{podid.Mint()})} {
		if !Retryable(err) {
			t.Errorf("%v is not retryable", err)
		}
	}
}

// warm brings the sandbox's pod up with a call.
func warm(t *testing.T, r *Runner, sb auth.Sandbox) {
	t.Helper()
	if _, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"true"}`), nil); err != nil {
		t.Fatal(err)
	}
}

// A call that finds the pod not running (an idle pause it raced) or gone
// from the node syncs the sandbox, acquires it again and is made once
// more; a second refusal is the call's error.
func TestRetryNotOnPod(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	sb := env.NewSandbox()
	warm(t, r, sb)
	pods, err := env.Pods(bg)
	if err != nil {
		t.Fatal(err)
	}

	// FAILED_PRECONDITION: suspended under a lease that said running.
	if _, err := pods.SuspendPod(bg, &podv1.SuspendPodRequest{PodId: sb.ID.String()}); err != nil {
		t.Fatal(err)
	}
	env.Engine.Stale(sb.ID, 1)
	acquires := env.Engine.Calls("Acquire", sb.ID)
	res, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"echo back"}`), nil)
	if err != nil || res.Text != "back\n" {
		t.Fatalf("%+v %v", res, err)
	}
	if env.Engine.Calls("Sync", sb.ID) != 1 || env.Engine.Calls("Acquire", sb.ID) != acquires+2 {
		t.Errorf("syncs %d, acquires %d", env.Engine.Calls("Sync", sb.ID), env.Engine.Calls("Acquire", sb.ID)-acquires)
	}

	// NOT_FOUND: the pod went from the node; a unary tool.
	if _, err := pods.DeletePod(bg, &podv1.DeletePodRequest{PodId: sb.ID.String()}); err != nil {
		t.Fatal(err)
	}
	env.Engine.Stale(sb.ID, 1)
	res, err = r.Call(bg, sb, ToolLs, json.RawMessage(`{"path":"/"}`), nil)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if env.Engine.Calls("Sync", sb.ID) != 2 {
		t.Errorf("syncs %d", env.Engine.Calls("Sync", sb.ID))
	}

	// Once only.
	if _, err := pods.SuspendPod(bg, &podv1.SuspendPodRequest{PodId: sb.ID.String()}); err != nil {
		t.Fatal(err)
	}
	env.Engine.Stale(sb.ID, 2)
	_, err = r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"true"}`), nil)
	want := fmt.Sprintf("node n1: pod %s is POD_STATE_SUSPENDED", sb.ID)
	if err == nil || err.Error() != want || status.Code(err) != codes.FailedPrecondition {
		t.Errorf("second refusal: %v, want %q", err, want)
	}

	// A Sync or a second Acquire that fails is the ensure step's error.
	env.Engine.Stale(sb.ID, 1)
	env.Engine.FailSync(errors.New("database is down"))
	_, err = r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"true"}`), nil)
	if err == nil || err.Error() != "ensure sandbox: database is down" {
		t.Errorf("sync failure: %v", err)
	}
	env.Engine.FailSync(nil)
	env.Engine.Stale(sb.ID, 1)
	env.Engine.FailAcquire(sb.ID, nil, model.ErrLost)
	_, err = r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"true"}`), nil)
	if err == nil || err.Error() != "ensure sandbox: sandbox lost" {
		t.Errorf("acquire failure: %v", err)
	}
	if env.Engine.Holds(sb.ID) != 0 {
		t.Errorf("%d holds left", env.Engine.Holds(sb.ID))
	}
}

// upOnWait ends the bring-up of the sandbox's pod (closes up) once the
// runner, having found the pod still starting, waits for it to run
// (WaitRunning). A runner that does not wait gets the pod after 10 s, and
// the test's count of the waits fails.
func upOnWait(env *toolstest.Env, id podid.ID, up chan struct{}) {
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for env.Engine.Calls("WaitRunning", id) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		close(up)
	}()
}

// A guest that does not answer while its pod starts is waited for and the
// call made once more; one that does not answer while the pod runs is
// reported unreachable and fails the call.
func TestGuestUnreachable(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	sb := env.NewSandbox()

	// A pod whose bring-up waits for the test.
	up := make(chan struct{})
	env.Node.Script().SetBringUp(func(ctx context.Context, _ podid.ID) error {
		select {
		case <-up:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	pods, err := env.Pods(bg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pods.CreatePod(bg, &podv1.CreatePodRequest{PodId: sb.ID.String(), Spec: &podv1.PodSpec{
		Containers: []*podv1.ContainerSpec{{Name: "main", ImageRef: "test"}},
	}}); err != nil {
		t.Fatal(err)
	}
	env.Engine.Stale(sb.ID, 1)
	upOnWait(env, sb.ID, up)
	res, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"echo up"}`), nil)
	if err != nil || res.Text != "up\n" {
		t.Fatalf("%+v %v", res, err)
	}
	if env.Engine.Calls("WaitRunning", sb.ID) != 1 || env.Engine.Calls("Unreachable", sb.ID) != 0 || env.Engine.Calls("Reachable", sb.ID) != 1 {
		t.Errorf("waits %d, unreachable %d, reachable %d", env.Engine.Calls("WaitRunning", sb.ID),
			env.Engine.Calls("Unreachable", sb.ID), env.Engine.Calls("Reachable", sb.ID))
	}
	env.Node.Script().SetBringUp(nil)

	env.Node.Script().SetGuestUnreachable(sb.ID, true)
	_, err = r.Call(bg, sb, ToolRead, json.RawMessage(`{"path":"/a"}`), nil)
	want := fmt.Sprintf("node n1: the guest of pod %s: connection reset by peer", sb.ID)
	if err == nil || err.Error() != want {
		t.Errorf("unreachable guest: %v, want %q", err, want)
	}
	if !Retryable(err) {
		t.Error("an unreachable guest is worth a retry")
	}
	if env.Engine.Calls("Unreachable", sb.ID) != 1 || env.Engine.Calls("WaitRunning", sb.ID) != 1 {
		t.Errorf("unreachable %d", env.Engine.Calls("Unreachable", sb.ID))
	}
	env.Node.Script().SetGuestUnreachable(sb.ID, false)
	if _, err := r.Call(bg, sb, ToolRead, json.RawMessage(`{"path":"/a"}`), nil); err != nil {
		t.Fatal(err)
	}
	if env.Engine.Calls("Reachable", sb.ID) != 2 {
		t.Errorf("reachable %d", env.Engine.Calls("Reachable", sb.ID))
	}

	// A guest that drops a running exec after output: the call is not
	// made again, and the running pod is reported.
	env.Node.Script().SetExec(func(ctx context.Context, _ podid.ID, _ *containerv1.ExecRequest, out testnode.ExecOutput) (int32, bool, error) {
		_ = out.Stdout([]byte("partial\n"))
		return 0, true, status.Errorf(codes.Unavailable, "the guest of pod %s: connection reset by peer", sb.ID)
	})
	res, err = r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"x"}`), nil)
	if err == nil || err.Error() != want || res.Text != "partial\n" {
		t.Errorf("dropped exec: %+v %v", res, err)
	}
	if env.Engine.Calls("Unreachable", sb.ID) != 2 {
		t.Errorf("unreachable %d after a dropped exec", env.Engine.Calls("Unreachable", sb.ID))
	}
	env.Node.Script().SetExec(nil)
	if env.Engine.Holds(sb.ID) != 0 {
		t.Errorf("%d holds left", env.Engine.Holds(sb.ID))
	}

	// A guest that does not answer once its starting pod runs: the retry
	// fails, and the running pod is reported.
	other := env.NewSandbox()
	up = make(chan struct{})
	env.Node.Script().SetBringUp(func(ctx context.Context, _ podid.ID) error {
		select {
		case <-up:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if _, err := pods.CreatePod(bg, &podv1.CreatePodRequest{PodId: other.ID.String(), Spec: &podv1.PodSpec{
		Containers: []*podv1.ContainerSpec{{Name: "main", ImageRef: "test"}},
	}}); err != nil {
		t.Fatal(err)
	}
	env.Engine.Stale(other.ID, 1)
	env.Node.Script().SetGuestUnreachable(other.ID, true)
	upOnWait(env, other.ID, up)
	_, err = r.Call(bg, other, ToolRead, json.RawMessage(`{"path":"/a"}`), nil)
	want = fmt.Sprintf("node n1: the guest of pod %s: connection reset by peer", other.ID)
	if err == nil || err.Error() != want {
		t.Errorf("unreachable after the wait: %v, want %q", err, want)
	}
	if env.Engine.Calls("WaitRunning", other.ID) != 1 || env.Engine.Calls("Unreachable", other.ID) != 1 {
		t.Errorf("waits %d, unreachable %d", env.Engine.Calls("WaitRunning", other.ID), env.Engine.Calls("Unreachable", other.ID))
	}
	env.Node.Script().SetGuestUnreachable(other.ID, false)
	env.Node.Script().SetBringUp(nil)
	if env.Engine.Holds(other.ID) != 0 {
		t.Errorf("%d holds left", env.Engine.Holds(other.ID))
	}
}

// blockExec makes every Exec of the node wait until release is closed, and
// counts the ones begun on started.
func blockExec(env *toolstest.Env) (started chan struct{}, release chan struct{}) {
	started, release = make(chan struct{}, 1000), make(chan struct{})
	env.Node.Script().SetExec(func(ctx context.Context, _ podid.ID, _ *containerv1.ExecRequest, out testnode.ExecOutput) (int32, bool, error) {
		started <- struct{}{}
		select {
		case <-release:
			return 0, true, nil
		case <-ctx.Done():
			return 0, true, status.FromContextError(ctx.Err()).Err()
		}
	})
	return started, release
}

func waitN(t *testing.T, ch chan struct{}, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d began", i, n)
		}
	}
}

// The nodes' port's stream and tool budgets answer at once, retryable; so
// does the per-sandbox exec cap.
func TestBudgets(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{MaxStreams: 8}) // 4 long streams, 2 tool calls
	r := newRunner(env)
	sb := env.NewSandbox()
	warm(t, r, sb)

	started, release := blockExec(env)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"wait"}`), nil); err != nil {
				t.Errorf("blocked call: %v", err)
			}
		}()
	}
	waitN(t, started, 4)
	_, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"one more"}`), nil)
	if err == nil || err.Error() != "node n1: stream budget exhausted" || !Retryable(err) || status.Code(err) != codes.ResourceExhausted {
		t.Errorf("past the stream budget: %v", err)
	}
	close(release)
	wg.Wait()

	gate := make(chan struct{})
	entered := make(chan struct{}, 10)
	env.Node.Script().SetHook(toolv1.ToolService_Read_FullMethodName, func(ctx context.Context, _ string, _ any) error {
		entered <- struct{}{}
		<-gate
		return nil
	})
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.Call(bg, sb, ToolRead, json.RawMessage(`{"path":"/x"}`), nil)
		}()
	}
	waitN(t, entered, 2)
	_, err = r.Call(bg, sb, ToolRead, json.RawMessage(`{"path":"/x"}`), nil)
	if err == nil || err.Error() != "node n1: tool budget exhausted" || !Retryable(err) {
		t.Errorf("past the tool budget: %v", err)
	}
	close(gate)
	wg.Wait()
	env.Node.Script().SetHook(toolv1.ToolService_Read_FullMethodName, nil)

	// The exec cap: per sandbox, on this instance, before the budget.
	r.execCap = 2
	started, release = blockExec(env)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"wait"}`), nil)
		}()
	}
	waitN(t, started, 2)
	_, err = r.Call(bg, sb, ToolProcessList, nil, nil)
	want := fmt.Sprintf("sandbox %s: too many execs at once (at most 2 on this instance)", sb.ID)
	if err == nil || err.Error() != want || !Retryable(err) {
		t.Errorf("past the exec cap: %v", err)
	}
	// Another sandbox has a cap of its own.
	other := env.NewSandbox()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := r.Call(bg, other, ToolBash, json.RawMessage(`{"command":"wait"}`), nil); err != nil {
			t.Errorf("another sandbox: %v", err)
		}
	}()
	waitN(t, started, 1)
	close(release)
	wg.Wait()
	if env.Engine.Holds(sb.ID) != 0 || env.Engine.Holds(other.ID) != 0 {
		t.Errorf("holds left %d %d", env.Engine.Holds(sb.ID), env.Engine.Holds(other.ID))
	}
}

// A caller that goes away cancels the call; the guest sees the stream end.
// ferun_cancel of kind exec cancels a sandbox's calls on every instance.
func TestCancellation(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	sb := env.NewSandbox()
	warm(t, r, sb)
	started, _ := blockExec(env)

	ctx, cancel := context.WithCancel(bg)
	go func() {
		<-started
		cancel()
	}()
	_, err := r.Call(ctx, sb, ToolBash, json.RawMessage(`{"command":"wait"}`), nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled call: %v", err)
	}

	rctx, stop := context.WithCancel(bg)
	defer stop()
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(rctx) }()
	if err := env.DB.WaitListening(bg); err != nil {
		t.Fatal(err)
	}
	// Run subscribes to ferun_cancel before it records calls with an id.
	waitFor(t, "Run to subscribe", func() bool {
		r.k.mu.Lock()
		defer r.k.mu.Unlock()
		return r.k.renewing
	})
	calls := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"wait"}`), nil)
			calls <- err
		}()
	}
	waitN(t, started, 2)
	// Another sandbox's cancel and a malformed payload change nothing.
	for _, p := range []string{`{"kind":"exec","sandbox":"` + podid.Mint().String() + `"}`, `{"kind":"mcp","key":"x"}`, `nonsense`} {
		if err := db.Notify(bg, env.DB.Pool, db.ChanCancel, p); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-calls:
		t.Fatalf("a call ended early: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := db.Notify(bg, env.DB.Pool, db.ChanCancel, `{"kind":"exec","sandbox":"`+sb.ID.String()+`"}`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case err := <-calls:
			if !errors.Is(err, ErrCancelled) || !Retryable(err) {
				t.Errorf("cancelled by the control plane: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("ferun_cancel did not cancel the calls")
		}
	}

	// A call still waiting for its sandbox is cancelled too.
	waiting := env.NewSandbox()
	entered := make(chan struct{}, 1)
	env.Node.Script().SetBringUp(func(ctx context.Context, _ podid.ID) error {
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	go func() {
		_, err := r.Call(bg, waiting, ToolBash, json.RawMessage(`{"command":"wait"}`), nil)
		calls <- err
	}()
	waitN(t, entered, 1)
	if err := db.Notify(bg, env.DB.Pool, db.ChanCancel, `{"kind":"exec","sandbox":"`+waiting.ID.String()+`"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-calls:
		if !errors.Is(err, ErrCancelled) || !Retryable(err) || !strings.HasPrefix(err.Error(), "ensure sandbox: ") {
			t.Errorf("cancelled while acquiring: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ferun_cancel did not cancel the call in Acquire")
	}
	env.Node.Script().SetBringUp(nil)
	if env.Engine.Holds(waiting.ID) != 0 {
		t.Errorf("%d holds left", env.Engine.Holds(waiting.ID))
	}
	stop()
	if err := <-runDone; err != nil {
		t.Errorf("Run: %v", err)
	}
	rows := env.Activity(sb.ID, 8)
	cancelled := 0
	for _, row := range rows {
		if row.Kind == activity.KindToolEnd && row.Name == "bash: cancelled" && string(row.Detail) == `{"cancelled": true}` {
			cancelled++
		}
	}
	if cancelled != 3 {
		t.Errorf("%d cancelled tool_end rows", cancelled)
	}
	if env.Engine.Holds(sb.ID) != 0 {
		t.Errorf("%d holds left", env.Engine.Holds(sb.ID))
	}
}

// Every call that ran has a tool_start and a tool_end row, children of the
// sandbox's open run.
func TestActivityRows(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	sb := env.NewSandbox()
	if _, err := env.DB.Pool.Exec(bg, "UPDATE sandboxes SET run_id = 'run-1' WHERE id = $1", sb.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"echo hi\necho more","timeout":5}`), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"exit 4"}`), nil); err != nil {
		t.Fatal(err)
	}
	_, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"echo slow; sleep 60","timeout":1}`), nil)
	if err == nil {
		t.Fatal("no timeout")
	}
	big := strings.Repeat("x", 100<<10)
	if _, err := r.Call(bg, sb, ToolWrite, json.RawMessage(`{"path":"/big","content":"`+big+`"}`), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"head -c 70000 /dev/zero | tr '\\0' y"}`), nil); err != nil {
		t.Fatal(err)
	}
	rows := env.Activity(sb.ID, 10)
	if len(rows) != 10 {
		t.Fatalf("%d rows", len(rows))
	}
	start, end := rows[0], rows[1]
	if start.Kind != "tool_start" || start.Category != "tool" || start.Source != "ferund" || start.InstanceID != toolstest.InstanceID ||
		start.SpanID == "" || start.RequestID != start.SpanID || start.ParentSpanID != "run-1" || start.Tool != "bash" ||
		start.Name != "bash: echo hi …" || start.ProjectID != toolstest.Project || start.Node != toolstest.NodeName {
		t.Errorf("tool_start %+v", start)
	}
	var params map[string]any
	if err := json.Unmarshal(start.Params, &params); err != nil || params["command"] != "echo hi\necho more" || params["timeout"] != float64(5) {
		t.Errorf("params %s", start.Params)
	}
	if end.Kind != "tool_end" || end.SpanID != start.SpanID || end.Name != "bash: ok" || *end.ExitCode != 0 ||
		end.DurationNs == nil || *end.DurationNs <= 0 || end.Output != "hi\nmore\n" || end.OutputTruncated || string(end.Detail) != "{}" {
		t.Errorf("tool_end %+v", end)
	}
	if e := rows[3]; e.Name != "bash: failed (exit 4)" || *e.ExitCode != 4 {
		t.Errorf("failed tool_end %+v", e)
	}
	if e := rows[5]; e.Name != "bash: failed (exit -1)" || *e.ExitCode != -1 || string(e.Detail) != `{"error": "timeout after 1s"}` || e.Output != "slow\n" {
		t.Errorf("timed out tool_end %+v %s", e, e.Detail)
	}
	if s := rows[6]; len(s.Params) > 4<<10 || !strings.Contains(string(s.Params), "… (102400 bytes)") || s.Name != "write: /big" {
		t.Errorf("a large write's params: %d bytes, name %q", len(s.Params), s.Name)
	}
	if e := rows[9]; !strings.HasSuffix(e.Output, streamMarker) || len(e.Output) > 50000+len(streamMarker) || e.OutputTruncated {
		t.Errorf("audit output %d bytes, truncated %v", len(e.Output), e.OutputTruncated)
	}
	// The audit copy is cut at the activity log's cap and says so.
	scriptExec(env, func(_ *containerv1.ExecRequest, out testnode.ExecOutput) int32 {
		_ = out.Stdout([]byte(strings.Repeat("o", 40000)))
		_ = out.Stderr([]byte(strings.Repeat("e", 40000)))
		return 0
	})
	if _, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"x"}`), nil); err != nil {
		t.Fatal(err)
	}
	rows = env.Activity(sb.ID, 12)
	if e := rows[11]; len(e.Output) != activity.OutputCap || !e.OutputTruncated {
		t.Errorf("audit output %d bytes, truncated %v", len(e.Output), e.OutputTruncated)
	}
	if toolName(ToolRg, json.RawMessage(`{"pattern":"x","path":"/src"}`)) != "rg: /src" ||
		toolName(ToolProcessList, json.RawMessage(`{}`)) != "process_list" ||
		toolName(ToolEdit, json.RawMessage(`{"edits":[],"zeta":"z","alpha":"a"}`)) != "edit: a" {
		t.Error("names")
	}
}

// pythonRoot makes the node run the process scripts with their records
// under root, a directory of the test, instead of the container's.
func pythonRoot(env *toolstest.Env, root string) {
	env.Node.Script().SetExec(func(_ context.Context, _ podid.ID, req *containerv1.ExecRequest, _ testnode.ExecOutput) (int32, bool, error) {
		if len(req.Command) > 2 && req.Command[0] == "python3" {
			req.Command[2] = strings.ReplaceAll(req.Command[2], processRoot, root)
		}
		return 0, false, nil
	})
}

// Background processes: a started process is held (for its timeout, or
// without a limit), a stopped one released, and so is one that
// process_list shows ended or process_logs does not know.
// RunningProcesses lists them without a hold and without activity.
func TestProcessTools(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	sb := env.NewSandbox()
	pythonRoot(env, t.TempDir())

	res, err := r.Call(bg, sb, ToolProcessStart, json.RawMessage(`{"command":"echo served; sleep 60","timeout":0,"name":"dev"}`), nil)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	forever, ok := parseHandle(res.Text)
	if !ok {
		t.Fatalf("no handle in %q", res.Text)
	}
	res, err = r.Call(bg, sb, ToolProcessStart, json.RawMessage(`{"command":"sleep 0.2","timeout":30,"name":"short"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	short, _ := parseHandle(res.Text)
	if got := env.Engine.Processes(sb.ID); len(got) != 2 || got[forever] != 0 || got[short] != 30*time.Second {
		t.Fatalf("holds %v", got)
	}

	// RunningProcesses: no hold, no activity, no Acquire.
	rows := len(env.Activity(sb.ID, 4))
	acquires := env.Engine.Calls("Acquire", sb.ID)
	lease := model.Lease{SandboxID: sb.ID, Node: toolstest.NodeName}
	running, err := r.RunningProcesses(bg, lease)
	if err != nil || !running[forever] {
		t.Fatalf("running %v %v", running, err)
	}
	if env.Engine.Calls("Acquire", sb.ID) != acquires || env.Engine.Holds(sb.ID) != 0 {
		t.Error("RunningProcesses acquired the sandbox")
	}
	waitFor(t, "the short process's end", func() bool {
		running, err := r.RunningProcesses(bg, lease)
		return err == nil && !running[short] && running[forever]
	})
	time.Sleep(200 * time.Millisecond)
	if n := len(env.Activity(sb.ID, rows)); n != rows {
		t.Errorf("RunningProcesses wrote %d activity rows", n-rows)
	}

	// process_list shows the short one ended: its hold goes.
	res, err = r.Call(bg, sb, ToolProcessList, nil, nil)
	if err != nil || !strings.HasPrefix(res.Text, "[") {
		t.Fatalf("%+v %v", res, err)
	}
	if got := env.Engine.Processes(sb.ID); len(got) != 1 || got[forever] != 0 {
		t.Errorf("holds after the list %v", got)
	}

	res, err = r.Call(bg, sb, ToolProcessLogs, json.RawMessage(`{"id":"`+forever+`"}`), nil)
	if err != nil || res.Text != "served\n" {
		t.Errorf("logs %+v %v", res, err)
	}
	res, err = r.Call(bg, sb, ToolProcessLogs, json.RawMessage(`{"id":"000000-gone"}`), nil)
	if err != nil || res.Text != "no such process: 000000-gone\n" || res.ExitCode != 1 {
		t.Errorf("logs of an unknown process %+v %v", res, err)
	}
	if rel := env.Engine.Released(sb.ID); rel[len(rel)-1] != "000000-gone" {
		t.Errorf("released %v", rel)
	}

	res, err = r.Call(bg, sb, ToolProcessStop, json.RawMessage(`{"id":"`+forever+`"}`), nil)
	if err != nil || res.Text != "process "+forever+" stopped\n" {
		t.Errorf("stop %+v %v", res, err)
	}
	if got := env.Engine.Processes(sb.ID); len(got) != 0 {
		t.Errorf("holds after the stop %v", got)
	}
	running, err = r.RunningProcesses(bg, lease)
	if err != nil || len(running) != 0 {
		t.Errorf("running after the stop %v %v", running, err)
	}

	// A process_start that printed no handle holds nothing; a list that is
	// not one releases nothing.
	scriptExec(env, func(_ *containerv1.ExecRequest, out testnode.ExecOutput) int32 {
		_ = out.Stderr([]byte("python3: not found\n"))
		return 127
	})
	if res, err := r.Call(bg, sb, ToolProcessStart, json.RawMessage(`{"command":"x","timeout":5}`), nil); err != nil || res.ExitCode != 127 {
		t.Errorf("%+v %v", res, err)
	}
	if got := env.Engine.Processes(sb.ID); len(got) != 0 {
		t.Errorf("holds %v", got)
	}
	if _, err := r.RunningProcesses(bg, lease); err == nil || err.Error() != "process_list exited with 127: python3: not found" {
		t.Errorf("RunningProcesses without python: %v", err)
	}
}

// A guest that feosd could not reach because its VM is busy (its marker,
// as while Firecracker writes a late snapshot) is not reported
// unreachable, which would have the engine repair, and so restart, a pod
// that is only busy: the call did not run, and the caller comes back
// after busyRetry. An unmarked guest timeout of a running pod is still
// reported.
func TestBusyGuestIsNotUnreachable(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	sb := env.NewSandbox()
	msg := fmt.Sprintf("the guest of pod %s did not answer within 5s; the VM is busy: the pod is being suspended", sb.ID)
	busy := busyGuest(sb.ID)
	env.Node.Script().SetExec(func(context.Context, podid.ID, *containerv1.ExecRequest, testnode.ExecOutput) (int32, bool, error) {
		return 0, false, busy
	})
	_, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"x"}`), nil)
	var later interface{ RetryAfter() time.Duration }
	if err == nil || err.Error() != "node n1: "+msg || !errors.As(err, &later) || later.RetryAfter() != 30*time.Second {
		t.Fatalf("busy guest: %v, want %q with a retry after 30s", err, "node n1: "+msg)
	}
	if !Retryable(err) {
		t.Error("a busy guest is worth a retry")
	}
	if n := env.Engine.Calls("Unreachable", sb.ID); n != 0 {
		t.Errorf("a busy guest reported unreachable %d times", n)
	}

	// The same timeout without the marker: unreachable, as before.
	env.Node.Script().SetExec(func(context.Context, podid.ID, *containerv1.ExecRequest, testnode.ExecOutput) (int32, bool, error) {
		return 0, false, status.Error(codes.Unavailable, fmt.Sprintf("the guest of pod %s did not answer within 5s", sb.ID))
	})
	_, err = r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"x"}`), nil)
	var be *BusyError
	if err == nil || errors.As(err, &be) {
		t.Fatalf("unmarked timeout: %v", err)
	}
	if n := env.Engine.Calls("Unreachable", sb.ID); n != 1 {
		t.Errorf("an unmarked timeout reported unreachable %d times", n)
	}
	env.Node.Script().SetExec(nil)
	if env.Engine.Holds(sb.ID) != 0 {
		t.Errorf("%d holds left", env.Engine.Holds(sb.ID))
	}
}
