// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/codes"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The clear part of a pod's HTTP bindings (ADR 0028), PodSpec.http_bindings:
// the names its resolver answers with the node address and its
// interception CAs' certificates. A create carries it in its spec; a pod
// whose part changes afterwards, a sandbox's PUT or an environment's new
// secret version, gets SetPodHttpBindings, without a restart:
//
//   - at once after the change (PushHTTPBindings), in the background;
//   - before the StartPod or ResumePod of an op, and after the RestorePod
//     of a restore or a repair, when the row's part is not the one its node
//     took last (sandboxes.http_pushed);
//   - by the leader's loop (LoopHTTPBindings), for every row on a
//     connected node whose part is not the one its node took last: a push
//     that was lost.
//
// A node that answers UNIMPLEMENTED (a feosd from before ADR 0028) is
// recorded (an activity event, a warning) and not asked again for
// httpUnimplementedFor; the pod gets its part with its next create. A
// failed push never fails an op.

// LoopHTTPBindings is the leader's loop of lost pushes.
const LoopHTTPBindings = "sandbox-http-bindings"

const (
	// httpPushTimeout bounds one push, connection included.
	httpPushTimeout = 20 * time.Second
	// httpPushRetries is how often a push in the background is tried; the
	// loop takes over after.
	httpPushRetries = 4
	// httpUnimplementedFor is how long a node that answered UNIMPLEMENTED
	// is not asked again.
	httpUnimplementedFor = 10 * time.Minute
	// httpLoopInterval and httpLoopBatch are the loop's pace.
	httpLoopInterval = 15 * time.Second
	httpLoopBatch    = 256
)

// HTTPBindings gives the clear part of a pod's HTTP bindings as its row
// has them now (*bindings.Service): nil for none.
type HTTPBindings interface {
	PodHTTP(ctx context.Context, row store.Sandbox) (*podv1.PodHttpBindings, error)
}

// SetHTTPBindings sets the source of the pods' HTTP bindings; nil: pods
// have none.
func (e *Engine) SetHTTPBindings(src HTTPBindings) {
	e.hooks.mu.Lock()
	defer e.hooks.mu.Unlock()
	e.hooks.http = src
}

func (e *Engine) httpSource() HTTPBindings {
	e.hooks.mu.Lock()
	defer e.hooks.mu.Unlock()
	return e.hooks.http
}

// httpPart is the row's clear part, nil without a source or bindings.
func (e *Engine) httpPart(ctx context.Context, row store.Sandbox) (*podv1.PodHttpBindings, error) {
	src := e.httpSource()
	if src == nil {
		return nil, nil
	}
	part, err := src.PodHTTP(ctx, row)
	if err != nil {
		return nil, fmt.Errorf("the pod's HTTP bindings: %w", err)
	}
	return part, nil
}

// httpPushes coalesces the background pushes: one per pod at a time, and
// one more when asked meanwhile.
type httpPushes struct {
	mu sync.Mutex
	// running: a push of the pod runs; again: it was asked once more.
	running map[podid.ID]bool
	again   map[podid.ID]bool
	// unimplemented are the nodes that answered UNIMPLEMENTED, until when.
	unimplemented map[string]time.Time
}

// PushHTTPBindings has the pod's node sent the pod's clear part now, in the
// background, when the pod is on a node without an op and its part is not
// the one its node took last. It never waits.
func (e *Engine) PushHTTPBindings(id podid.ID) {
	p := &e.httpPush
	p.mu.Lock()
	if p.running == nil {
		p.running, p.again = map[podid.ID]bool{}, map[podid.ID]bool{}
	}
	if p.running[id] {
		p.again[id] = true
		p.mu.Unlock()
		return
	}
	p.running[id] = true
	p.mu.Unlock()
	started := e.goBackground(func(ctx context.Context) {
		for {
			e.pushWithRetries(ctx, id)
			p.mu.Lock()
			if !p.again[id] || ctx.Err() != nil {
				delete(p.running, id)
				delete(p.again, id)
				p.mu.Unlock()
				return
			}
			delete(p.again, id)
			p.mu.Unlock()
		}
	})
	if !started {
		p.mu.Lock()
		delete(p.running, id)
		delete(p.again, id)
		p.mu.Unlock()
	}
}

func (e *Engine) pushWithRetries(ctx context.Context, id podid.ID) {
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		err := e.pushHTTP(ctx, id)
		if err == nil || errors.Is(err, errHTTPUnimplemented) || ctx.Err() != nil {
			return
		}
		if attempt >= httpPushRetries {
			e.rl.warn("http-push", "cannot send a pod its HTTP bindings; the loop tries again", "sandbox", id, "err", err)
			return
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff *= 2
	}
}

// pushable reports whether the row's pod is on its node for a push: a
// materialised pod, running, paused or stopped, without an op (an op that
// brings it up pushes itself).
func pushable(row store.Sandbox) bool {
	if row.Node == "" || !row.Materialised || row.Op != "" {
		return false
	}
	switch row.ClientState {
	case model.StateRunning, model.StatePaused, model.StateStopped:
		return true
	}
	return false
}

// pushHTTP sends the row's pod its part when it is pushable and its part
// is not the one its node took last.
func (e *Engine) pushHTTP(ctx context.Context, id podid.ID) error {
	ctx, cancel := context.WithTimeout(ctx, httpPushTimeout)
	defer cancel()
	row, err := e.st.GetSandbox(ctx, id)
	if errors.Is(err, model.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !pushable(row) {
		return nil
	}
	return e.pushRow(ctx, row, nil)
}

// pushRow sends the row's pod its part, through pc when given (an op's
// client), unless its node took it already; row.HTTPPushed is what was
// read before.
func (e *Engine) pushRow(ctx context.Context, row store.Sandbox, pc podv1.PodServiceClient) error {
	part, err := e.httpPart(ctx, row)
	if err != nil {
		return err
	}
	if part.GetVersion() == row.HTTPPushed {
		return nil
	}
	if until, ok := e.httpUnimplemented(row.Node); ok && time.Now().Before(until) {
		return errHTTPUnimplemented
	}
	if pc == nil {
		if pc, err = e.podClient(ctx, row.Node); err != nil {
			return err
		}
	}
	req := &podv1.SetPodHttpBindingsRequest{PodId: row.ID.String(), HttpBindings: part}
	if req.HttpBindings == nil {
		req.HttpBindings = &podv1.PodHttpBindings{}
	}
	_, err = pc.SetPodHttpBindings(ctx, req)
	switch grpcCode(err) {
	case codes.OK:
	case codes.Unimplemented:
		e.markHTTPUnimplemented(row.Node)
		e.rl.warn("http-unimplemented", "the node does not take SetPodHttpBindings; its pods get their HTTP "+
			"bindings' names and CAs at their next create", "node", row.Node, "sandbox", row.ID)
		e.recordHTTPPush(row, part.GetVersion(), "unimplemented")
		return errHTTPUnimplemented
	case codes.NotFound:
		// The pod is gone from the node: its next create carries its part.
		return nil
	default:
		return fmt.Errorf("SetPodHttpBindings on node %s: %w", row.Node, err)
	}
	// Against the http_pushed read before the push: a concurrent push (the
	// loop's, another instance's) leaves it unknown, and the loop pushes
	// again.
	if _, err := e.st.SetHTTPPushed(ctx, row.ID, row.Incarnation, row.HTTPPushed, part.GetVersion()); err != nil {
		return fmt.Errorf("record the pushed HTTP bindings: %w", err)
	}
	e.recordHTTPPush(row, part.GetVersion(), "sent")
	return nil
}

// errHTTPUnimplemented is a node without SetPodHttpBindings.
var errHTTPUnimplemented = errors.New("the node does not implement SetPodHttpBindings")

func (e *Engine) httpUnimplemented(node string) (time.Time, bool) {
	p := &e.httpPush
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.unimplemented[node]
	return t, ok
}

func (e *Engine) markHTTPUnimplemented(node string) {
	p := &e.httpPush
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unimplemented == nil {
		p.unimplemented = map[string]time.Time{}
	}
	p.unimplemented[node] = time.Now().Add(httpUnimplementedFor)
}

// recordHTTPPush records a push's outcome in the activity log.
func (e *Engine) recordHTTPPush(row store.Sandbox, version, result string) {
	e.record(activity.Event{SandboxID: row.ID, TenantID: row.Owner, Node: row.Node, Kind: activity.KindPodHTTPBindings,
		Name:   fmt.Sprintf("HTTP bindings %s to node %s", result, row.Node),
		Detail: map[string]any{"node": row.Node, "version": version, "result": result}})
}

// prepareHTTP puts the row's part into a create's spec: a part that cannot
// be read fails the create, which sends nothing then.
func (e *Engine) prepareHTTP(ctx context.Context, h *OpHandle) error {
	part, err := e.httpPart(ctx, h.row)
	if err != nil {
		return err
	}
	h.spec.HttpBindings = part
	return nil
}

// httpAroundCall pushes an op's pod its part where the op's call does not
// carry it: before the StartPod or ResumePod (before is true), after a
// restore's or a repair's calls (before false). A create records no part:
// a feosd from before ADR 0028 drops the spec's unread, so whether the pod
// holds it is not known until a push is answered, and the loop sends it
// once (the same part again changes nothing on a node that took it). Best
// effort: a failure is logged, and the loop tries again.
func (e *Engine) httpAroundCall(ctx context.Context, pc podv1.PodServiceClient, h *OpHandle, before bool) {
	if e.httpSource() == nil {
		return
	}
	var err error
	switch {
	case !before && h.kind == OpCreate:
		err = e.st.ResetHTTPPushed(ctx, h.id, h.inc)
	case before && (h.kind == OpStart || h.kind == OpResume),
		!before && (h.kind == OpRestore || h.kind == OpRepair):
		row := h.row
		row.Node = h.target
		if cur, gerr := e.st.GetSandbox(ctx, h.id); gerr == nil && cur.Incarnation == h.inc {
			row.HTTPPushed = cur.HTTPPushed
		}
		err = e.pushRow(ctx, row, pc)
	}
	if err != nil && !errors.Is(err, errHTTPUnimplemented) && ctx.Err() == nil {
		e.rl.warn("http-op", "cannot send a pod its HTTP bindings around its op; the loop tries again",
			"sandbox", h.id, "op", h.kind, "err", err)
	}
}

// httpBindingsLoop pushes the rows on connected nodes whose part is not
// the one their node took last.
func (e *Engine) httpBindingsLoop(ctx context.Context) error {
	if e.httpSource() == nil {
		return nil
	}
	nodes := e.conns.Nodes()
	if len(nodes) == 0 {
		return nil
	}
	rows, err := e.st.HTTPPushCandidates(ctx, nodes, httpLoopBatch)
	if err != nil {
		return fmt.Errorf("http bindings: %w", err)
	}
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, row := range rows {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil
		}
		wg.Add(1)
		go func() {
			defer func() {
				<-sem
				wg.Done()
			}()
			pctx, cancel := context.WithTimeout(ctx, httpPushTimeout)
			defer cancel()
			if err := e.pushRow(pctx, row, nil); err != nil && !errors.Is(err, errHTTPUnimplemented) && ctx.Err() == nil {
				e.rl.warn("http-loop", "cannot send a pod its HTTP bindings", "sandbox", row.ID, "err", err)
			}
		}()
	}
	wg.Wait()
	return nil
}
