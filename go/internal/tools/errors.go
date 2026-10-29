// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// NodeError is a call into a pod's guest that failed on its node: the gRPC
// status of the call. It reads "node <n>: <message>", unless the message
// names the node already (the nodes' port's own refusals do).
type NodeError struct {
	Node   string
	Status *status.Status
}

func (e *NodeError) Error() string {
	msg := e.Status.Message()
	prefix := "node " + e.Node
	if strings.HasPrefix(msg, prefix+":") || strings.HasPrefix(msg, prefix+" ") {
		return msg
	}
	return prefix + ": " + msg
}

// GRPCStatus lets status.Code see the call's code through the error.
func (e *NodeError) GRPCStatus() *status.Status { return e.Status }

// nodeError makes the NodeError of a failed call on node.
func nodeError(node string, err error) *NodeError {
	return &NodeError{Node: node, Status: status.Convert(err)}
}

// ErrExecCap is a call refused because the sandbox has as many execs
// running on this instance as it may (§4: 64).
var ErrExecCap = errors.New("too many execs at once")

// ErrCancelled is a call the control plane cancelled (ferun_cancel of kind
// exec): the sandbox's node is being drained by force.
var ErrCancelled = errors.New("the call was cancelled by the control plane: the sandbox's node is being drained")

// TimeoutError is a call into the guest that ran past its deadline. It
// reads "timeout after <N>s", the deadline in seconds.
type TimeoutError struct{ Seconds int }

func (e *TimeoutError) Error() string { return fmt.Sprintf("timeout after %ds", e.Seconds) }

// The answers of the calls with an id (ADR 0023, keyed.go).
var (
	// ErrBadCallKey is a call id outside the alphabet or the length.
	ErrBadCallKey = errors.New("a call id is 1 to 255 characters of A-Z a-z 0-9 . _ ~ : @ -")
	// ErrCallCap is a call refused because the sandbox has as many calls
	// with an id in flight on this instance as it may (64), or as many
	// requests waiting for such calls (256). It is worth a retry.
	ErrCallCap = errors.New("too many calls with an id at once")
	// ErrCancelledByCaller is a call with an id that a client cancelled
	// (REST DELETE …/calls/{key}, or MCP notifications/cancelled). It is
	// final even when the call had not reached the sandbox yet.
	ErrCancelledByCaller = errors.New("the call was cancelled by its caller")
	// ErrRecord is a call whose row could not be written or read. Nothing
	// ran because of this request.
	ErrRecord = errors.New("the call could not be recorded")
	// ErrCallNotRun is a call that did not run after its row was replaced
	// or deleted under it again and again.
	ErrCallNotRun = errors.New("the call did not run; repeat it with the same id")
	// ErrNoCall is a call id that names no call of the sandbox (never
	// made, or past its retention).
	ErrNoCall = errors.New("no call with this id in the sandbox")
)

// CallIDConflictError is a call id that names another call in the
// sandbox: another tool, or the same tool with other arguments. Its text
// never contains the id.
type CallIDConflictError struct{ Tool string }

func (e *CallIDConflictError) Error() string {
	return fmt.Sprintf("call id conflict: the id names another call in this sandbox (%s with other arguments); "+
		"a new call needs a new id", e.Tool)
}

// KeyedTimeoutError is a bash call with an id whose timeout is longer
// than a call with an id may run (MaxKeyedTimeout). Nothing is recorded.
type KeyedTimeoutError struct{ Max time.Duration }

func (e *KeyedTimeoutError) Error() string {
	return fmt.Sprintf("a call with an id may run at most %d s: lower its timeout, or start longer work with process_start",
		int(e.Max/time.Second))
}

// AskAgainError answers a request that waited for another instance's call
// while this instance stops: the call still runs there.
type AskAgainError struct{ Instance string }

func (e *AskAgainError) Error() string {
	return fmt.Sprintf("the call is still running on instance %s, and this instance is stopping: ask again with the same id",
		e.Instance)
}

// The causes with which a call with an id is cancelled from inside the
// Runner. callRun answers them in place of the context's error.
var (
	// errStopping: the drain cut the call at the shutdown grace.
	errStopping = errors.New("the instance is stopping")
	// errRunnerStopped: Run ended, and with it the renewal of the rows.
	errRunnerStopped = errors.New("the instance's runner stopped")
	// errCallLost: the renewal found the call's row ended by another
	// writer (the leader, a waiter or a restart marked it).
	errCallLost = errors.New("the call's record was ended while it ran")
	// errCallBudget: the call's whole life ran past its budget.
	errCallBudget = errors.New("the call ran past its budget (the sandbox's preparation, the tool's timeout and a minute)")
)

// localRefusal reports whether err is the nodes' port's own refusal of a
// call (a *nodelink.RefusalError: the node not connected to this
// instance, its connection closing, a stream or tool budget exhausted):
// the call never left this instance. The same code and text from a node
// is not one.
func localRefusal(err error) bool {
	var re *nodelink.RefusalError
	return errors.As(err, &re)
}

// feosdRefusal reports whether err is feosd's own answer, sent before it
// routes a call to the pod's guest: the pod is not running
// (FAILED_PRECONDITION "pod <id> is <STATE>", check_reachable,
// crates/services/pod-service/src/pods.rs:1608-1628) or not on the node
// (NOT_FOUND "no pod <id>", pods.rs:1644-1666). A call that got such an
// answer has not run in the guest. Any other FAILED_PRECONDITION or
// NOT_FOUND may come from the guest's services.
//
// guestUnreachable is not a refusal: feosd sends UNAVAILABLE "the guest of
// pod <id>: ..." both when it cannot connect to the guest
// (guest_channel, pods.rs:2520-2524) and when the call fails after it
// handed the request to the guest (forward.rs:118-124), as when the guest
// agent or the VM dies while the command runs. The two texts are the same,
// so such a call may have run. One marked busy (nodelink.VMBusy) is a
// refusal: feosd gave up on the connect to the guest of a VM that writes
// a snapshot, before the call reached the guest.
func feosdRefusal(err error, id podid.ID) bool {
	if nodelink.VMBusy(err) {
		return true
	}
	s, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch s.Code() {
	case codes.FailedPrecondition:
		return strings.HasPrefix(s.Message(), "pod "+id.String()+" is ")
	case codes.NotFound:
		return s.Message() == "no pod "+id.String()
	}
	return false
}

// Retryable reports whether a failed call is worth repeating shortly, as
// the REST tool routes tell with Retry-After: no capacity, a node or
// instance that is unavailable or stopping, an image's registry that did
// not answer the question before a create, an op in flight that the
// sandbox's preparation does not wait for (a hibernation), a stream or
// tool budget, the exec cap or the cap of calls with an id exhausted, a
// call cut by a forced drain, a call with an id that could not be
// recorded or did not run, or one still running on a stopping instance.
func Retryable(err error) bool {
	switch {
	case errors.Is(err, model.ErrNoCapacity), errors.Is(err, model.ErrNodeUnavailable),
		errors.Is(err, model.ErrRegistryUnavailable),
		errors.Is(err, model.ErrStopping), errors.Is(err, model.ErrBusy), errors.Is(err, ErrExecCap),
		errors.Is(err, ErrCancelled), errors.Is(err, ErrCallCap), errors.Is(err, ErrRecord),
		errors.Is(err, ErrCallNotRun):
		return true
	}
	var ae *AskAgainError
	if errors.As(err, &ae) {
		return true
	}
	var ne *NodeError
	if errors.As(err, &ne) {
		switch ne.Status.Code() {
		case codes.ResourceExhausted, codes.Unavailable:
			return true
		}
	}
	return false
}

// busyRetry is when a caller told that the sandbox's VM is busy should
// come back, as the sandbox engine tells it.
const busyRetry = 30 * time.Second

// BusyError is a call that feosd could not hand to the guest because the
// pod's VM is busy, as while Firecracker writes a snapshot (feosd's marker,
// nodelink.VMBusy): the guest is not unreachable, the call did not run,
// and the caller comes back after busyRetry. It reads as the NodeError it
// wraps.
type BusyError struct{ Err error }

func (e *BusyError) Error() string { return e.Err.Error() }
func (e *BusyError) Unwrap() error { return e.Err }

// RetryAfter is when to come back (apierr.RetryAfterOf).
func (e *BusyError) RetryAfter() time.Duration { return busyRetry }

// guestUnreachable reports whether err is feosd's answer for a pod whose
// guest does not answer: UNAVAILABLE "the guest of pod <id>: ...".
func guestUnreachable(err error) bool {
	s, ok := status.FromError(err)
	return ok && s.Code() == codes.Unavailable && strings.HasPrefix(s.Message(), "the guest of pod")
}

// notOnPod reports whether err says the pod is not where the lease put it,
// or not running: FAILED_PRECONDITION "pod <id> is <state>" or NOT_FOUND
// "no pod <id>". The sandbox is synchronised and the call made once more.
func notOnPod(err error) bool {
	switch status.Code(err) {
	case codes.FailedPrecondition, codes.NotFound:
		return true
	}
	return false
}

// statusOf is the gRPC status of err, if it carries one.
func statusOf(err error) (*status.Status, bool) {
	return status.FromError(err)
}
