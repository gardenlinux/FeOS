// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	envspec "github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// tlsCallTimeout bounds SetPodTlsBindings: the guest listens on the
// bindings before the node answers.
const tlsCallTimeout = 30 * time.Second

// errNoTrustDomain is a TLS binding on an instance without a trust
// domain: no pod has an identity to present.
var errNoTrustDomain = errors.New("TLS bindings need a trust domain, which this instance has none of")

// podTLSBindings are a template's TLS bindings as a pod's spec names them,
// each pinned to its addon's ID in the environment envID of owner.
func podTLSBindings(td, owner string, envID uuid.UUID, bindings []model.TemplateTLSBinding) []*podv1.TlsBinding {
	out := make([]*podv1.TlsBinding, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, &podv1.TlsBinding{Listen: b.Listen, Host: b.Host, Port: uint32(b.Port), Alpn: b.ALPN,
			ServerId: pki.WorkloadID(td, owner, envID, envspec.KindAddon, b.Addon)})
	}
	return out
}

// SetTLSBindings gives the sandbox's pod the TLS bindings (ADR 0036) of the
// attached services in it, stated whole: written into its template, which
// every later create of its pod carries, and sent to the pod of lease
// (SetPodTlsBindings), whose guest listens on them when it answers. The
// sandbox is made before its attached services, so its create carried
// none of theirs. A node too old for the call is an error: the service's
// references would reach nothing.
func (e *Engine) SetTLSBindings(ctx context.Context, lease model.Lease, bindings []model.TemplateTLSBinding) error {
	if len(bindings) > 0 && e.cfg.TrustDomain == "" {
		return errNoTrustDomain
	}
	unlock := e.tlsLocks.lock(lease.SandboxID)
	defer unlock()
	row, err := e.st.GetSandbox(ctx, lease.SandboxID)
	if err != nil {
		return err
	}
	if row.Incarnation != lease.Incarnation {
		return fmt.Errorf("sandbox %s is in another incarnation", lease.SandboxID)
	}
	var have []model.TemplateTLSBinding
	if row.PodTemplate != nil {
		have = row.PodTemplate.TLSBindings
	}
	if !slices.Equal(have, bindings) {
		ok, err := e.st.SetSandboxTLSBindings(ctx, row.ID, row.Incarnation, bindings)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("sandbox %s is gone", row.ID)
		}
		e.log.Info("sandbox TLS bindings set", "sandbox", row.ID, "bindings", len(bindings))
	}
	return e.pushTLSBindings(ctx, lease, row.Owner, row.EnvironmentID, bindings)
}

// pushTLSBindings sends the pod of lease its TLS bindings; the node does
// nothing when it has them already. The call runs on a context of its
// own: a mutating pod call never runs on its caller's.
func (e *Engine) pushTLSBindings(ctx context.Context, lease model.Lease, owner string, envID uuid.UUID,
	bindings []model.TemplateTLSBinding) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tlsCallTimeout)
	defer cancel()
	pc, err := e.podClient(cctx, lease.Node)
	if err != nil {
		return err
	}
	req := &podv1.SetPodTlsBindingsRequest{PodId: lease.SandboxID.String()}
	if len(bindings) > 0 {
		req.TlsBindings = podTLSBindings(e.cfg.TrustDomain, owner, envID, bindings)
	}
	_, err = pc.SetPodTlsBindings(cctx, req)
	switch grpcCode(err) {
	case codes.OK:
		return nil
	case codes.Unimplemented:
		return fmt.Errorf("node %s does not take SetPodTlsBindings: its feos-node is older than TLS bindings "+
			"for attached services", lease.Node)
	default:
		return fmt.Errorf("SetPodTlsBindings on node %s: %w", lease.Node, err)
	}
}

// TLS bindings again (ADR 0061). A pod's TLS bindings pin each addon's
// workload ID, which the pod got when its spec was made: a pod that comes
// back from a snapshot, a checkpoint or its disk, or that the engine
// adopts, comes back with the bindings of then. When such a pod runs again
// (a run opens other than by a create, whose spec carries them fresh),
// the engine sends it the bindings of now; and the leader sends every
// running pod with TLS bindings its bindings once, on each node it is
// connected to, when it starts to lead. The node does nothing when it
// has them, so an ID that changed (as the project's path did) heals by
// itself, and one that did not costs a call.

// LoopTLSBindings is the leader's sweep of TLS bindings.
const LoopTLSBindings = "sandbox-tls-bindings"

const (
	// tlsLoopInterval is how often the sweep looks for nodes it has not
	// swept in this leadership.
	tlsLoopInterval = 30 * time.Second
	// tlsLoopBatch and tlsLoopConcurrency are the sweep's pace.
	tlsLoopBatch       = 256
	tlsLoopConcurrency = 8
)

// tlsPush is the push of a pod's TLS bindings that follows a commit: to
// the pod of lease, with the bindings its row holds when the call goes out.
type tlsPush struct {
	lease model.Lease
}

// tlsPushOf is the push a written row asks for when its commit opened a
// run of origin opened: none for a create's ("image"), whose spec carried
// the bindings, nor for a row without TLS bindings or without a node.
func tlsPushOf(w store.Sandbox, opened string) *tlsPush {
	if opened == "" || opened == "image" || w.Node == "" || w.PodTemplate == nil || len(w.PodTemplate.TLSBindings) == 0 {
		return nil
	}
	return &tlsPush{lease: model.Lease{SandboxID: w.ID, Incarnation: w.Incarnation, Node: w.Node}}
}

// beginTLSPush sends a pod its TLS bindings in the background, best
// effort: a failure is logged, and the leader's sweep of its next
// leadership sends them again.
func (e *Engine) beginTLSPush(p tlsPush) {
	if e.cfg.TrustDomain == "" || !e.conns.Connected(p.lease.Node) {
		return
	}
	e.goBackground(func(ctx context.Context) {
		if err := e.pushCurrentTLSBindings(ctx, p.lease); err != nil && ctx.Err() == nil {
			e.rl.warn("tls-push", "cannot send a pod its TLS bindings again", "sandbox", p.lease.SandboxID, "err", err)
		}
	})
}

// pushCurrentTLSBindings sends the pod of lease the TLS bindings its row
// holds now, read under the sandbox's TLS lock: a SetTLSBindings and this
// push of the same sandbox do not overlap in this instance, and the push
// that reaches the node last carries the bindings written last (the node
// keeps the set of the last call). Nothing is sent when the row is in
// another incarnation or on another node than lease.
func (e *Engine) pushCurrentTLSBindings(ctx context.Context, lease model.Lease) error {
	unlock := e.tlsLocks.lock(lease.SandboxID)
	defer unlock()
	row, err := e.st.GetSandbox(ctx, lease.SandboxID)
	if errors.Is(err, model.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.Incarnation != lease.Incarnation || row.Node != lease.Node {
		return nil
	}
	var bindings []model.TemplateTLSBinding
	if row.PodTemplate != nil {
		bindings = row.PodTemplate.TLSBindings
	}
	return e.pushTLSBindings(ctx, lease, row.Owner, row.EnvironmentID, bindings)
}

// tlsLocks serialise, per sandbox, the reads of its TLS bindings with the
// calls that send them.
type tlsLocks struct {
	mu sync.Mutex
	m  map[podid.ID]*tlsLock
}

type tlsLock struct {
	mu    sync.Mutex
	users int
}

// lock takes the sandbox's lock and returns its release.
func (l *tlsLocks) lock(id podid.ID) func() {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[podid.ID]*tlsLock{}
	}
	k := l.m[id]
	if k == nil {
		k = &tlsLock{}
		l.m[id] = k
	}
	k.users++
	l.mu.Unlock()
	k.mu.Lock()
	return func() {
		k.mu.Unlock()
		l.mu.Lock()
		if k.users--; k.users == 0 {
			delete(l.m, id)
		}
		l.mu.Unlock()
	}
}

// tlsSweep is the sweep's state: the nodes swept in the leadership whose
// loop context is term.
type tlsSweep struct {
	mu    sync.Mutex
	term  context.Context
	nodes map[string]bool
}

// tlsBindingsLoop sends every running pod with TLS bindings on a
// connected node its bindings, once a node in each leadership.
func (e *Engine) tlsBindingsLoop(ctx context.Context) error {
	if e.cfg.TrustDomain == "" {
		return nil
	}
	e.tlsSwept.mu.Lock()
	if e.tlsSwept.term != ctx {
		e.tlsSwept.term, e.tlsSwept.nodes = ctx, map[string]bool{}
	}
	var nodes []string
	for _, n := range e.conns.Nodes() {
		if !e.tlsSwept.nodes[n] {
			nodes = append(nodes, n)
		}
	}
	e.tlsSwept.mu.Unlock()
	if len(nodes) == 0 {
		return nil
	}
	sem := make(chan struct{}, tlsLoopConcurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	var after podid.ID
	for {
		rows, err := e.st.TLSPushCandidates(ctx, nodes, after, tlsLoopBatch)
		if err != nil {
			return fmt.Errorf("tls bindings: %w", err)
		}
		for _, row := range rows {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return nil
			}
			wg.Add(1)
			go func() {
				defer func() {
					<-sem
					wg.Done()
				}()
				lease := model.Lease{SandboxID: row.ID, Incarnation: row.Incarnation, Node: row.Node}
				if err := e.pushCurrentTLSBindings(ctx, lease); err != nil && ctx.Err() == nil {
					e.rl.warn("tls-loop", "cannot send a pod its TLS bindings again", "sandbox", row.ID, "err", err)
				}
			}()
		}
		if len(rows) < tlsLoopBatch {
			break
		}
		after = rows[len(rows)-1].ID
	}
	e.tlsSwept.mu.Lock()
	if e.tlsSwept.term == ctx {
		for _, n := range nodes {
			e.tlsSwept.nodes[n] = true
		}
	}
	e.tlsSwept.mu.Unlock()
	return nil
}
