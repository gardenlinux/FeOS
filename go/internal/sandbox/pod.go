// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// The labels of a sandbox's pod (§2, §9). A pod belongs to ferund when it
// carries LabelSandbox; whether it is the sandbox's current pod is decided
// by LabelIncarnation.
const (
	LabelSandbox     = "ferun.sandbox"
	LabelTenant      = "ferun.tenant"
	LabelIncarnation = "ferun.incarnation"
)

// mainContainer is the one container of a sandbox's pod.
const mainContainer = "main"

// podIncarnation is the incarnation a pod's label names: uuid.Nil for a pod
// without it, or with one that is not a UUID, which no row has.
func podIncarnation(info *podv1.PodInfo) uuid.UUID {
	s, ok := info.GetSpec().GetLabels()[LabelIncarnation]
	if !ok {
		return uuid.Nil
	}
	u, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil
	}
	return u
}

// isFerunPod reports whether the pod belongs to ferund: its spec carries
// LabelSandbox. ferund never touches another pod.
func isFerunPod(info *podv1.PodInfo) bool {
	_, ok := info.GetSpec().GetLabels()[LabelSandbox]
	return ok
}

// gib is a size's GiB for the deadlines that scale with memory:
// ceil(memory_mib / 1024).
func gib(memoryMiB int) time.Duration {
	if memoryMiB <= 0 {
		return 0
	}
	return time.Duration((memoryMiB + 1023) / 1024)
}

// observation is what a node said about a pod: nothing (answered false:
// the node never answered), NOT_FOUND (info nil) or the pod's record.
type observation struct {
	answered bool
	info     *podv1.PodInfo
}

func (o observation) found() bool { return o.answered && o.info != nil }

// ours reports whether the node holds the pod of incarnation inc.
func (o observation) ours(inc uuid.UUID) bool {
	return o.found() && podIncarnation(o.info) == inc
}

// stale reports whether the node holds a pod of this ID that is of another
// incarnation: a stale record.
func (o observation) stale(inc uuid.UUID) bool {
	return o.found() && podIncarnation(o.info) != inc
}

func (o observation) state() podv1.PodState { return o.info.GetState() }

// pendingOf reports whether the node holds the pod of inc, PENDING.
func (o observation) pendingOf(inc uuid.UUID) bool {
	return o.ours(inc) && o.state() == podv1.PodState_POD_STATE_PENDING
}

// podClient returns the node's PodService, through the node's current
// connection on this instance (waiting as Conns.Conn does).
func (e *Engine) podClient(ctx context.Context, node string) (podv1.PodServiceClient, error) {
	cc, err := e.conns.Conn(ctx, node)
	if err != nil {
		return nil, err
	}
	return podv1.NewPodServiceClient(cc), nil
}

// getPod asks node for the pod: NOT_FOUND is an answer (an observation
// without info); any other error is not.
func (e *Engine) getPod(ctx context.Context, node string, id podid.ID) (observation, error) {
	pc, err := e.podClient(ctx, node)
	if err != nil {
		return observation{}, err
	}
	info, err := pc.GetPod(ctx, &podv1.GetPodRequest{PodId: id.String()})
	if grpcCode(err) == codes.NotFound {
		return observation{answered: true}, nil
	}
	if err != nil {
		return observation{}, err
	}
	e.recordBake(ctx, node, info)
	return observation{answered: true, info: info}, nil
}

// podEvents is an open StreamPodEvents of one pod.
type podEvents struct {
	ch     chan *podv1.PodEvent
	ended  chan struct{}
	cancel context.CancelFunc
}

// openPodEvents opens StreamPodEvents{pod_id} on node; its events arrive
// on ch until the stream ends (ended is closed) or cancel is called.
func (e *Engine) openPodEvents(ctx context.Context, node string, id podid.ID) (*podEvents, error) {
	sctx, cancel := context.WithCancel(ctx)
	pc, err := e.podClient(sctx, node)
	if err != nil {
		cancel()
		return nil, err
	}
	stream, err := pc.StreamPodEvents(sctx, &podv1.StreamPodEventsRequest{PodId: id.String()})
	if err != nil {
		cancel()
		return nil, err
	}
	pe := &podEvents{ch: make(chan *podv1.PodEvent, 16), ended: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(pe.ended)
		for {
			ev, err := stream.Recv()
			if err != nil {
				return
			}
			if ev.GetPodId() != id.String() {
				continue
			}
			select {
			case pe.ch <- ev:
			case <-sctx.Done():
				return
			}
		}
	}()
	return pe, nil
}

// watchPod observes the pod id on node until the node holds it of
// incarnation inc in a state other than PENDING, holds one of another
// incarnation, does not hold it, or ctx ends. While the pod is PENDING it
// waits for the pod's events (StreamPodEvents, opened before each GetPod
// so that no change is missed between them), and asks GetPod again every
// pollEvery. Errors are transient (a reconnect, a feosd restart): the
// waiter goes on with Conn again, reopening a stream that ended
// (UNAVAILABLE, DATA_LOSS) and polling when one cannot be opened
// (RESOURCE_EXHAUSTED). onPending, if set, runs at the first observation
// of the pod of inc PENDING. It returns the last observation.
func (e *Engine) watchPod(ctx context.Context, node string, id podid.ID, inc uuid.UUID, onPending func()) observation {
	var last observation
	var events *podEvents
	defer func() {
		if events != nil {
			events.cancel()
		}
	}()
	pending := false
	backoff := e.t.retryMin
	for ctx.Err() == nil {
		obs, err := e.getPod(ctx, node, id)
		if err != nil {
			if !sleepCtx(ctx, backoff) {
				break
			}
			backoff = min(backoff*2, e.t.retryMax)
			continue
		}
		backoff = e.t.retryMin
		last = obs
		if !obs.pendingOf(inc) {
			return obs
		}
		if !pending {
			pending = true
			if onPending != nil {
				onPending()
			}
		}
		if events == nil || isClosed(events.ended) {
			if events != nil {
				// A stream that ended (a reconnect, DATA_LOSS) is reopened,
				// but not at once, so that one that keeps ending does not
				// spin the loop.
				events.cancel()
				events = nil
				if !sleepCtx(ctx, e.t.retryMin) {
					break
				}
			}
			if ev, err := e.openPodEvents(ctx, node, id); err == nil {
				events = ev
				// GetPod again: a change between the last one and the
				// subscription is seen there.
				continue
			}
			// No stream (RESOURCE_EXHAUSTED, a reconnect): poll.
		}
		e.awaitPodChange(ctx, events)
	}
	return last
}

// awaitPodChange waits for an event of the pod in a state other than
// PENDING (or its deletion), the end of the stream, pollEvery, or ctx.
func (e *Engine) awaitPodChange(ctx context.Context, events *podEvents) {
	timer := time.NewTimer(e.t.pollEvery)
	defer timer.Stop()
	var ch <-chan *podv1.PodEvent
	var ended <-chan struct{}
	if events != nil {
		ch, ended = events.ch, events.ended
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			return
		case <-ended:
			return
		case ev := <-ch:
			if ev.GetDeleted() || ev.GetState() != podv1.PodState_POD_STATE_PENDING {
				return
			}
		}
	}
}

// isClosed reports whether ch is closed.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// sleepCtx waits for d or ctx; false when ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
