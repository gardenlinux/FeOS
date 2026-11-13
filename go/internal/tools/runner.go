// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	toolv1 "github.com/gardenlinux/feos/go/gen/feos/tool/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Sandboxes is what the Runner needs of the sandbox engine
// (*sandbox.Engine): a running sandbox held awake for a call, and what the
// engine is told of the calls.
type Sandboxes interface {
	// Acquire ensures the sandbox runs and holds it awake with a hold of
	// kind until release is called.
	Acquire(ctx context.Context, id podid.ID, kind model.HoldKind) (model.Lease, func(), error)
	// Sync reads the sandbox's pod from its node and records what it finds.
	Sync(ctx context.Context, id podid.ID) error
	// WaitRunning waits until the sandbox's PENDING pod runs.
	WaitRunning(ctx context.Context, id podid.ID) (model.Lease, error)
	// Unreachable reports a running pod whose guest does not answer;
	// Reachable one whose guest did.
	Unreachable(id podid.ID)
	Reachable(id podid.ID)
	// HoldProcess holds the sandbox awake for a background process, for
	// timeout (0: without a limit); ReleaseProcess ends that hold.
	HoldProcess(ctx context.Context, id podid.ID, processID string, timeout time.Duration) error
	ReleaseProcess(ctx context.Context, id podid.ID, processID string) error
}

// Fixed bounds of the Runner (§4, §10).
const (
	// execCap is how many exec streams one sandbox may have at once on this
	// instance.
	execCap = 64
	// getPodTimeout bounds the GetPod that tells a starting pod from a
	// running one whose guest does not answer.
	getPodTimeout = 30 * time.Second
	// bookkeepingTimeout bounds the hold writes after a call and the
	// activity lookups around it.
	bookkeepingTimeout = 10 * time.Second
	// auditParamString is how much of a string argument the activity log
	// keeps when a call's arguments are larger than its output cap (a
	// write's content).
	auditParamString = 1 << 10
)

// Runner runs tool calls: the one entry point of REST and MCP (Call, and
// CallKeyed for calls with an id, keyed.go), the engine's listing of
// background processes (RunningProcesses), and the cancellation of a
// sandbox's calls from another instance and the renewal of the rows of
// calls with an id (Run).
type Runner struct {
	sb         Sandboxes
	conns      nodelink.Conns
	rec        *activity.Recorder
	d          *db.DB
	st         *store.Store
	instanceID string
	log        *slog.Logger
	rl         *rateLog

	// Fields, so that tests can shorten or lower them.
	defaultTimeout time.Duration
	execCap        int

	mu    sync.Mutex
	calls map[podid.ID]map[*running]struct{} // the calls in flight on this instance, by sandbox
	execs map[podid.ID]int                   // the exec streams open on this instance, by sandbox

	// k holds the calls with an id (keyed.go).
	k keyedState
}

// running is a call in flight, cancelled with ErrCancelled by a
// ferun_cancel of its sandbox.
type running struct {
	cancel context.CancelCauseFunc
}

// NewRunner makes a Runner. rec may be nil (no activity rows); d may be nil
// (no cancellation from other instances, no run spans).
func NewRunner(s Sandboxes, conns nodelink.Conns, rec *activity.Recorder, d *db.DB, instanceID string, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	r := &Runner{
		sb: s, conns: conns, rec: rec, d: d, instanceID: instanceID, log: log, rl: newRateLog(log),
		defaultTimeout: DefaultTimeout,
		execCap:        execCap,
		calls:          map[podid.ID]map[*running]struct{}{},
		execs:          map[podid.ID]int{},
	}
	r.k = keyedState{
		lease: config.CallLease, retention: config.CallRetention, cap: callCap, waiterCap: waiterCap, poll: callPoll,
		budget: func(timeout time.Duration) time.Duration {
			return config.PreparationBudget + timeout + callSlack
		},
		calls:      map[callRef]*keyedCall{},
		locks:      map[callRef]*refLock{},
		perSandbox: map[podid.ID]int{},
		waiters:    map[podid.ID]int{},
		release:    make(chan struct{}),
	}
	r.k.bg, r.k.bgCancel = context.WithCancelCause(context.Background())
	if d != nil {
		r.st = store.New(d)
		r.k.begin, r.k.finish, r.k.abandon = r.st.BeginToolCall, r.st.FinishToolCall, r.st.AbandonToolCall
	}
	return r
}

// Specs returns the tool baseline, in order.
func (r *Runner) Specs() []Spec { return Specs() }

// cancelPayload is the payload of ferun_cancel.
type cancelPayload struct {
	Kind    string `json:"kind"`
	Sandbox string `json:"sandbox"`
	Key     string `json:"key,omitempty"`
}

// Run serves this instance's part of other instances' cancellations and
// keeps the rows of its calls with an id alive, until ctx ends:
//   - ferun_cancel {"kind":"exec","sandbox":id} (a node drained by force)
//     cancels the sandbox's calls in flight here;
//   - ferun_cancel {"kind":"call","sandbox":id,"key":k} cancels the call
//     with the id k here (CancelCall on another instance), after a join of
//     that id in progress registered it;
//   - every third of the call lease, the rows of the calls with an id
//     running here are renewed; a call whose row another writer ended is
//     cancelled (errCallLost).
//
// Calls with an id are recorded only while Run runs. When it ends, the
// calls with an id still running here are cut (errRunnerStopped).
func (r *Runner) Run(ctx context.Context) error {
	if r.d == nil {
		<-ctx.Done()
		r.k.bgCancel(errRunnerStopped)
		return nil
	}
	ch, unsubscribe := r.d.Subscribe(db.ChanCancel)
	defer unsubscribe()
	var wg sync.WaitGroup
	r.k.mu.Lock()
	r.k.renewing = true
	r.k.mu.Unlock()
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.renewCalls(ctx)
	}()
	defer func() {
		r.k.mu.Lock()
		r.k.renewing = false
		r.k.mu.Unlock()
		r.k.bgCancel(errRunnerStopped)
		wg.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case payload := <-ch:
			var p cancelPayload
			if payload == "" || json.Unmarshal([]byte(payload), &p) != nil {
				continue
			}
			id, err := podid.Parse(p.Sandbox)
			if err != nil {
				continue
			}
			switch p.Kind {
			case "exec":
				if n := r.cancelSandbox(id); n > 0 {
					r.log.Info("tool calls cancelled by the control plane", "sandbox", id, "calls", n)
				}
			case "call":
				if !ValidCallKey(p.Key) {
					continue
				}
				// The per-key lock may wait for a join of the id that is
				// writing its row; the loop goes on meanwhile.
				wg.Add(1)
				go func() {
					defer wg.Done()
					if r.cancelLocal(callRef{sandbox: id, key: p.Key}, ErrCancelledByCaller) {
						r.log.Info("tool call cancelled by its caller through another instance", "sandbox", id)
					}
				}()
			}
		}
	}
}

// cancelSandbox cancels the sandbox's calls in flight on this instance and
// returns how many.
func (r *Runner) cancelSandbox(id podid.ID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for c := range r.calls[id] {
		c.cancel(ErrCancelled)
	}
	return len(r.calls[id])
}

// track registers a call in flight; the returned function removes it.
func (r *Runner) track(id podid.ID, cancel context.CancelCauseFunc) func() {
	c := &running{cancel: cancel}
	r.mu.Lock()
	set := r.calls[id]
	if set == nil {
		set = map[*running]struct{}{}
		r.calls[id] = set
	}
	set[c] = struct{}{}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(set, c)
		if len(r.calls[id]) == 0 {
			delete(r.calls, id)
		}
		r.mu.Unlock()
	}
}

// takeExec takes one of the sandbox's exec slots on this instance.
func (r *Runner) takeExec(id podid.ID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.execs[id] >= r.execCap {
		return fmt.Errorf("sandbox %s: %w (at most %d on this instance)", id, ErrExecCap, r.execCap)
	}
	r.execs[id]++
	return nil
}

func (r *Runner) giveExec(id podid.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.execs[id]--; r.execs[id] <= 0 {
		delete(r.execs, id)
	}
}

// Call runs one tool of the baseline in the sandbox sb, with the arguments
// args (a JSON object; empty means {}). progress, when not nil, is called
// with every chunk of output as it arrives.
//
// The sandbox is acquired first (Acquire: it runs, and an exec hold keeps
// it awake for the call); every error of that step is returned as "ensure
// sandbox: <err>". A call the node refuses because the pod is not where or
// not as the lease said (FAILED_PRECONDITION, NOT_FOUND) is made once more
// after a Sync and a new Acquire; one whose guest does not answer while
// the pod is still starting, once more when it runs. Other failures are
// "node <n>: <message>", a call past its deadline "timeout after <N>s".
//
// A tool that ran returns its Result and a nil error, whatever its exit
// code. A failed call returns the output gathered so far in the Result's
// Text, with the error. A call whose ctx ends returns ctx's error.
func (r *Runner) Call(ctx context.Context, sb auth.Sandbox, tool string, args json.RawMessage,
	progress func(chunk string)) (Result, error) {
	if !known(tool) {
		return Result{}, fmt.Errorf("unknown tool %q", tool)
	}
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	c, err := prepare(tool, args, r.defaultTimeout)
	if err != nil {
		return Result{}, err
	}
	return r.callPrepared(ctx, sb, c, args, progress)
}

// writeFileKeyed is the write tool with its content as bytes
// (prepareWrite), as CallKeyed runs it otherwise: with the id key, or as
// Call does without one (""). Its argument hash is writeHash's, so that
// REST's write of a body and MCP's write of the same bytes are the same
// call.
func (r *Runner) writeFileKeyed(ctx context.Context, sb auth.Sandbox, path string, content []byte, key string) (Result, CallInfo, error) {
	c, err := prepareWrite(path, content, r.defaultTimeout)
	if err != nil {
		return Result{}, CallInfo{}, err
	}
	if key != "" && !ValidCallKey(key) {
		return Result{}, CallInfo{}, ErrBadCallKey
	}
	args := writeArgs(path, content)
	if key == "" || r.d == nil {
		res, err := r.callPrepared(ctx, sb, c, args, nil)
		return res, CallInfo{}, err
	}
	return r.callKeyed(ctx, sb, c, args, key, writeHash(path, content), nil)
}

// callPrepared runs the call c on the caller's ctx, recorded in the
// activity log with args.
func (r *Runner) callPrepared(ctx context.Context, sb auth.Sandbox, c *call, args json.RawMessage,
	progress func(chunk string)) (Result, error) {
	res, _, err := r.callRun(ctx, sb, c, args, "", progress, nil)
	return res, err
}

// causeOf is the Runner's own cause with which ctx was cancelled
// (ErrCancelledByCaller, errStopping, errRunnerStopped, errCallLost,
// errCallBudget), or nil.
func causeOf(ctx context.Context) error {
	cause := context.Cause(ctx)
	for _, own := range []error{ErrCancelledByCaller, errStopping, errRunnerStopped, errCallLost, errCallBudget} {
		if errors.Is(cause, own) {
			return cause
		}
	}
	return nil
}

// callRun runs the call c on ctx, recorded in the activity log with args
// (and key, the call's id, "" for none). beforeSend, when not nil, runs
// before every request into the guest (output.beforeSend). sent reports
// that the call may have run in the guest. A ctx that ends with one of
// the Runner's own causes (causeOf) answers that cause in place of ctx's
// error.
func (r *Runner) callRun(ctx context.Context, sb auth.Sandbox, c *call, args json.RawMessage, key string,
	progress func(chunk string), beforeSend func(timeout time.Duration) error) (res Result, sent bool, err error) {
	tool := c.tool
	// The call is cancellable by ferun_cancel from here on, while Acquire
	// waits for the sandbox too: a forced drain must not miss a call that
	// is about to run on the node it drains.
	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	defer r.track(sb.ID, cancel)()

	lease, release, err := r.sb.Acquire(cctx, sb.ID, model.HoldExec)
	if err != nil {
		if cause := causeOf(cctx); cause != nil {
			err = cause
		} else if ctx.Err() == nil && errors.Is(context.Cause(cctx), ErrCancelled) {
			err = ErrCancelled
		}
		return Result{}, false, fmt.Errorf("ensure sandbox: %w", err)
	}
	defer func() { release() }()

	start := time.Now()
	spanID := r.toolStart(ctx, sb, lease, tool, args, key)
	o := &output{progress: progress, beforeSend: beforeSend}
	code, err := r.attempts(cctx, sb.ID, &lease, &release, c, o)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			if cause := causeOf(cctx); cause != nil {
				err = cause
			} else {
				err = ctx.Err()
			}
		case errors.Is(context.Cause(cctx), ErrCancelled):
			err = ErrCancelled
		}
	}
	r.toolEnd(ctx, sb, lease, tool, spanID, start, code, err, o)
	if err != nil {
		return o.partial(), o.sent, err
	}
	res = o.result(tool, code)
	r.processHolds(ctx, sb.ID, c, res)
	return res, o.sent, nil
}

// attempts makes the call into the guest, and once more when the first
// attempt found the pod elsewhere, not running or still starting. lease
// and release follow a new Acquire.
func (r *Runner) attempts(cctx context.Context, id podid.ID, lease *model.Lease, release *func(), c *call, o *output) (int, error) {
	for attempt := 0; ; attempt++ {
		before := o.sent
		code, received, err := r.invoke(cctx, *lease, c, o)
		if err == nil {
			r.sb.Reachable(id)
			return code, nil
		}
		if !received && feosdRefusal(err, id) {
			// feosd refused the call before routing it to the guest:
			// this attempt did not run. An earlier attempt that may have
			// run still counts.
			o.sent = before
		}
		if cctx.Err() != nil {
			return -1, r.callError(lease.Node, err)
		}
		if nodelink.VMBusy(err) {
			// The guest did not answer because its VM is busy (a late
			// snapshot): it is not unreachable, and a repair would
			// restart a pod that is only busy. The caller comes back.
			return -1, &BusyError{Err: r.callError(lease.Node, err)}
		}
		if attempt > 0 || received {
			// Not made again; a running pod whose guest stopped answering
			// is still reported, so that the engine repairs it.
			if guestUnreachable(err) {
				if state, gerr := r.podState(cctx, *lease); gerr == nil && state == podv1.PodState_POD_STATE_RUNNING {
					r.sb.Unreachable(id)
				}
			}
			return -1, r.callError(lease.Node, err)
		}
		switch {
		case notOnPod(err):
			if err := r.reacquire(cctx, id, lease, release); err != nil {
				return -1, err
			}
		case guestUnreachable(err):
			state, gerr := r.podState(cctx, *lease)
			switch {
			case gerr == nil && state == podv1.PodState_POD_STATE_PENDING:
				l, werr := r.sb.WaitRunning(cctx, id)
				if werr != nil {
					return -1, fmt.Errorf("ensure sandbox: %w", werr)
				}
				*lease = l
			case gerr == nil && state == podv1.PodState_POD_STATE_RUNNING:
				r.sb.Unreachable(id)
				return -1, r.callError(lease.Node, err)
			default:
				// The pod is gone from the node or no longer runs: as if
				// the node had said so.
				if err := r.reacquire(cctx, id, lease, release); err != nil {
					return -1, err
				}
			}
		default:
			return -1, r.callError(lease.Node, err)
		}
	}
}

// reacquire syncs the sandbox and acquires it anew, then releases the old
// hold: the sandbox stays held throughout.
func (r *Runner) reacquire(ctx context.Context, id podid.ID, lease *model.Lease, release *func()) error {
	if err := r.sb.Sync(ctx, id); err != nil {
		return fmt.Errorf("ensure sandbox: %w", err)
	}
	l, rel, err := r.sb.Acquire(ctx, id, model.HoldExec)
	if err != nil {
		return fmt.Errorf("ensure sandbox: %w", err)
	}
	old := *release
	*lease, *release = l, rel
	old()
	return nil
}

// callError is how a failed call reads: a timeout and the Runner's own
// errors as they are, a gRPC status as a NodeError.
func (r *Runner) callError(node string, err error) error {
	var te *TimeoutError
	if errors.As(err, &te) || errors.Is(err, ErrExecCap) {
		return err
	}
	if _, ok := statusOf(err); ok {
		return nodeError(node, err)
	}
	return err
}

// podState is the state of the lease's pod on its node.
func (r *Runner) podState(ctx context.Context, lease model.Lease) (podv1.PodState, error) {
	ctx, cancel := context.WithTimeout(ctx, getPodTimeout)
	defer cancel()
	conn, err := r.conns.Conn(ctx, lease.Node)
	if err != nil {
		return 0, err
	}
	pod, err := podv1.NewPodServiceClient(conn).GetPod(ctx, &podv1.GetPodRequest{PodId: lease.SandboxID.String()})
	if err != nil {
		return 0, err
	}
	return pod.GetState(), nil
}

// invoke makes one attempt of the call on the lease's node. received
// reports that output arrived before a failure: such a call is never made
// again.
func (r *Runner) invoke(ctx context.Context, lease model.Lease, c *call, o *output) (code int, received bool, err error) {
	if c.argv != nil {
		return r.exec(ctx, lease, c.argv, c.timeout, o)
	}
	conn, err := r.conns.Conn(ctx, lease.Node)
	if err != nil {
		return -1, false, err
	}
	prev, err := o.send(c.timeout)
	if err != nil {
		return -1, false, err
	}
	actx, cancel := withLimit(ctx, lease.SandboxID, c.timeout)
	defer cancel()
	code, err = c.unary(actx, toolv1.NewToolServiceClient(conn), lease.Workdir, o)
	if err != nil {
		if localRefusal(err) {
			o.sent = prev
		}
		return -1, false, deadline(ctx, actx, c.timeout, err)
	}
	return code, false, nil
}

// send runs the call's beforeSend, if any, and marks the call sent; prev
// is what sent was before, for a call the nodes' port refuses locally.
func (o *output) send(timeout time.Duration) (prev bool, err error) {
	if o.beforeSend != nil {
		if err := o.beforeSend(timeout); err != nil {
			return o.sent, err
		}
	}
	prev, o.sent = o.sent, true
	return prev, nil
}

// exec runs argv in the main container of the lease's pod, in the
// sandbox's working directory (the root when it has none, or while the
// directory does not exist: the guest's rule), emitting its output, each
// stream capped at MaxToolOutput with a marker. The stream is read to its
// end, past the cap, for the exit code. Leaving it (at the deadline, or
// when ctx ends) cancels it, and the guest kills the command's process
// group.
func (r *Runner) exec(ctx context.Context, lease model.Lease, argv []string, timeout time.Duration, o *output) (code int, received bool, err error) {
	if err := r.takeExec(lease.SandboxID); err != nil {
		return -1, false, err
	}
	defer r.giveExec(lease.SandboxID)
	conn, err := r.conns.Conn(ctx, lease.Node)
	if err != nil {
		return -1, false, err
	}
	prev, err := o.send(timeout)
	if err != nil {
		return -1, false, err
	}
	actx, cancel := withLimit(ctx, lease.SandboxID, timeout)
	defer cancel()
	stream, err := containerv1.NewContainerServiceClient(conn).Exec(actx, &containerv1.ExecRequest{
		ContainerId: mainContainer, Command: argv, WorkingDir: cmp.Or(lease.Workdir, "/"),
	})
	if err != nil {
		if localRefusal(err) {
			o.sent = prev
		}
		return -1, false, deadline(ctx, actx, timeout, err)
	}
	var stdout, stderr streamCap
	code, exited := -1, false
	for {
		f, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return -1, received, deadline(ctx, actx, timeout, err)
		}
		received = true
		switch fr := f.GetFrame().(type) {
		case *containerv1.ExecStreamFrame_Stdout:
			if s, ok := stdout.take(fr.Stdout); ok {
				o.emit(s)
			}
		case *containerv1.ExecStreamFrame_Stderr:
			if s, ok := stderr.take(fr.Stderr); ok {
				o.emit(s)
			}
		case *containerv1.ExecStreamFrame_ExitCode:
			code, exited = int(fr.ExitCode), true
		}
	}
	if !exited {
		return -1, true, errors.New("exec stream ended without exit code")
	}
	return code, true, nil
}

// errCallTimeout is the cause with which withLimit ends a call into the
// guest at its deadline.
var errCallTimeout = errors.New("the call's deadline passed")

// withLimit returns ctx, routed to the pod podID, cancelled with the cause
// errCallTimeout once timeout has passed; stop releases it. The context has
// no deadline of its own on purpose: gRPC would send a deadline to the
// node as grpc-timeout, and a node ending the call at the same moment can
// reset the stream before this side's timer fires, so that the call fails
// as cancelled instead of as a timeout. Here only the control plane ends
// the call, and its cause says why.
func withLimit(ctx context.Context, podID podid.ID, timeout time.Duration) (actx context.Context, stop func()) {
	actx, cancel := context.WithCancelCause(nodelink.PodContext(ctx, podID))
	t := time.AfterFunc(timeout, func() { cancel(errCallTimeout) })
	return actx, func() {
		t.Stop()
		cancel(nil)
	}
}

// deadline turns the failure of a call that withLimit ended at its
// deadline, while its caller still waits, into a TimeoutError.
func deadline(ctx, actx context.Context, timeout time.Duration, err error) error {
	if ctx.Err() == nil && errors.Is(context.Cause(actx), errCallTimeout) {
		return &TimeoutError{Seconds: int(timeout / time.Second)}
	}
	return err
}

// processHolds keeps the sandbox's background-process holds in step with a
// process tool's answer (§9 Holds): a started process is held, a stopped
// one released, and so is one that process_list shows as not running or
// process_logs does not know. The writes outlive the caller's request: the
// process has been started or stopped whatever the client does now.
func (r *Runner) processHolds(ctx context.Context, id podid.ID, c *call, res Result) {
	if c.tool != ToolProcessStart && c.tool != ToolProcessStop && c.tool != ToolProcessList && c.tool != ToolProcessLogs {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer cancel()
	release := func(pid string) {
		if err := r.sb.ReleaseProcess(ctx, id, pid); err != nil {
			r.rl.warn("release", "release a background process's hold", "sandbox", id, "process", pid, "err", err)
		}
	}
	switch c.tool {
	case ToolProcessStart:
		pid, ok := parseHandle(res.Text)
		if !ok {
			if res.ExitCode == 0 {
				r.rl.warn("handle", "process_start printed no handle; the process is not held", "sandbox", id)
			}
			return
		}
		if err := r.sb.HoldProcess(ctx, id, pid, c.startTimeout); err != nil {
			r.rl.warn("hold", "hold a background process", "sandbox", id, "process", pid, "err", err)
		}
	case ToolProcessStop:
		release(c.processID)
	case ToolProcessLogs:
		if res.ExitCode == 1 && strings.HasPrefix(res.Text, unknownProcess) {
			release(c.processID)
		}
	case ToolProcessList:
		if res.ExitCode != 0 || res.Truncated {
			return
		}
		rows, ok := parseProcessList(res.Text)
		if !ok {
			return
		}
		for _, row := range rows {
			if !row.Running && row.ID != "" {
				release(row.ID)
			}
		}
	}
}

// RunningProcesses lists the background processes running in the lease's
// sandbox: process_list run on its node, without a hold, without activity
// rows, without counting as activity of the sandbox. It is the engine's
// ProcessLister, which releases the holds of processes that ended by
// themselves.
func (r *Runner) RunningProcesses(ctx context.Context, lease model.Lease) (map[string]bool, error) {
	o := &output{}
	code, _, err := r.exec(ctx, lease, []string{"python3", "-c", processListScript}, r.defaultTimeout, o)
	if err != nil {
		return nil, r.callError(lease.Node, err)
	}
	res := o.result(ToolProcessList, code)
	if code != 0 {
		line, _, _ := strings.Cut(strings.TrimSpace(res.Text), "\n")
		return nil, fmt.Errorf("process_list exited with %d: %s", code, line)
	}
	if res.Truncated {
		return nil, errors.New("process_list: the output is longer than a tool's output may be")
	}
	rows, ok := parseProcessList(res.Text)
	if !ok {
		return nil, fmt.Errorf("process_list: unexpected output %q", firstLine(res.Text))
	}
	out := map[string]bool{}
	for _, row := range rows {
		if row.Running && row.ID != "" {
			out[row.ID] = true
		}
	}
	return out, nil
}

// toolStart records a call's tool_start row and returns its span id. The
// span is a child of the sandbox's open run. A call with an id (key) names
// it in the row's detail as callId.
func (r *Runner) toolStart(ctx context.Context, sb auth.Sandbox, lease model.Lease, tool string, args json.RawMessage, key string) string {
	if r.rec == nil {
		return ""
	}
	spanID := uuid.NewString()
	var detail map[string]any
	if key != "" {
		detail = map[string]any{"callId": key}
	}
	r.rec.Record(activity.Event{
		SandboxID: sb.ID, TenantID: sb.Owner, Node: lease.Node,
		Kind: activity.KindToolStart, Category: activity.CategoryTool,
		RequestID: spanID, SpanID: spanID, ParentSpanID: r.runID(ctx, sb.ID),
		Name: toolName(tool, args), Tool: tool, Params: auditParams(args), Detail: detail,
	})
	return spanID
}

// toolEnd records a call's tool_end row: exit code (-1 for a failure),
// duration, the audit copy of its output, and the error or the
// cancellation.
func (r *Runner) toolEnd(ctx context.Context, sb auth.Sandbox, lease model.Lease, tool, spanID string, start time.Time, code int, err error, o *output) {
	if r.rec == nil {
		return
	}
	exit := int64(code)
	dur := time.Since(start).Nanoseconds()
	cancelled := ctx.Err() != nil || errors.Is(err, ErrCancelled)
	var detail map[string]any
	switch {
	case cancelled:
		exit, detail = -1, map[string]any{"cancelled": true}
	case err != nil:
		exit, detail = -1, map[string]any{"error": err.Error()}
	}
	r.rec.Record(activity.Event{
		SandboxID: sb.ID, TenantID: sb.Owner, Node: lease.Node,
		Kind: activity.KindToolEnd, Category: activity.CategoryTool,
		RequestID: spanID, SpanID: spanID,
		Name: toolEndName(tool, exit, cancelled), Tool: tool,
		ExitCode: &exit, DurationNs: &dur,
		Output: o.audit.String(), OutputTruncated: o.auditCut, Detail: detail,
	})
}

// runID is the sandbox's open run span, or "" when it has none or the row
// cannot be read.
func (r *Runner) runID(ctx context.Context, id podid.ID) string {
	if r.st == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, bookkeepingTimeout)
	defer cancel()
	row, err := r.st.GetSandbox(ctx, id)
	if err != nil {
		return ""
	}
	return row.RunID
}

// gistKeys are the arguments worth naming a call by, in order; a call that
// has none of them is named by its first string argument.
var gistKeys = []string{"command", "cmd", "script", "query", "url", "path", "file", "pattern", "name"}

// toolName names a call by its tool and a one-line gist of its main
// argument ("bash: ls -la"); the arguments themselves go into params.
func toolName(tool string, args json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(args, &m) != nil {
		return tool
	}
	for _, k := range gistKeys {
		if s, ok := m[k].(string); ok && s != "" {
			return tool + ": " + firstLine(s)
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return tool + ": " + firstLine(s)
		}
	}
	return tool
}

// toolEndName names a call's end: ok, failed with the exit code, or
// cancelled.
func toolEndName(tool string, exit int64, cancelled bool) string {
	switch {
	case cancelled:
		return tool + ": cancelled"
	case exit == 0:
		return tool + ": ok"
	default:
		return fmt.Sprintf("%s: failed (exit %d)", tool, exit)
	}
}

// firstLine is the first line of s, trimmed, with an ellipsis when more
// lines follow.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return strings.TrimSpace(s)
}

// auditParams is a call's arguments as the activity log keeps them: as
// they are, unless they are larger than its output cap (a write of a large
// file), when every string longer than auditParamString is cut and says how
// long it was.
func auditParams(args json.RawMessage) json.RawMessage {
	if len(args) <= activity.OutputCap {
		return args
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		s, _ := json.Marshal(fmt.Sprintf("(%d bytes of arguments that are not JSON)", len(args)))
		return s
	}
	b, err := json.Marshal(shortenStrings(v))
	if err != nil {
		return nil
	}
	return b
}

func shortenStrings(v any) any {
	switch t := v.(type) {
	case string:
		if len(t) > auditParamString {
			cut := auditParamString
			for cut > 0 && !utf8.RuneStart(t[cut]) {
				cut--
			}
			return fmt.Sprintf("%s… (%d bytes)", t[:cut], len(t))
		}
		return t
	case []any:
		for i := range t {
			t[i] = shortenStrings(t[i])
		}
		return t
	case map[string]any:
		for k, x := range t {
			t[k] = shortenStrings(x)
		}
		return t
	}
	return v
}
