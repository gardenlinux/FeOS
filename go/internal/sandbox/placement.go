// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The placement gates of §9 and the pin of ADR 0018, numbered as they
// came; the lists below evaluate them in their order.
const (
	gateStatus     = 1  // status ready
	gateDraining   = 2  // not draining
	gateForked     = 3  // identity not forked
	gateConnected  = 4  // connected to this instance
	gateTombstone  = 5  // no tombstone for this pod on the node
	gateAvoid      = 6  // not in avoid_nodes
	gateTAP        = 7  // no other pod with the same TAP prefix
	gateStore      = 8  // restore: the checkpoint's object store
	gateSlots      = 9  // a free slot
	gateCPU        = 10 // booked vCPUs within floor(cores × overcommit)
	gateMemory     = 11 // booked memory within the total less the headroom
	gateFreeMemory = 12 // live memory less the headroom
	gateDisk       = 13 // live disk for the disk budget and the pod's own disk
	gatePin        = 14 // create: the node the row is pinned to
	gateDiskBooked = 15 // booked disks within the disk total less the disk budget
	// gatePeers refuses a claim whose node got another instance of the
	// replica's service since the snapshot (peersCame). It is in no list
	// and decides only the order: the begin tries the node again last.
	gatePeers = 16
)

// The gates of each check: a placement's snapshot, the claim of a create
// or restore under the node's lock, the admission on a pinned node (start,
// resume), and a booking that reality makes (syncBooking).
var (
	placementGates = []int{gateStatus, gatePin, gateDraining, gateForked, gateConnected, gateTombstone, gateAvoid, gateTAP,
		gateStore, gateSlots, gateCPU, gateMemory, gateDiskBooked, gateFreeMemory, gateDisk}
	claimGates = []int{gateStatus, gatePin, gateDraining, gateForked, gateConnected, gateTombstone, gateTAP, gateStore,
		gateSlots, gateCPU, gateMemory, gateDiskBooked}
	admitGates    = []int{gateStatus, gateDraining, gateForked, gateConnected, gateSlots, gateCPU, gateMemory, gateDiskBooked}
	capacityGates = []int{gateSlots, gateCPU, gateMemory, gateDiskBooked}
)

// gateInput is what the gates look at for one node and one pod.
type gateInput struct {
	node         store.Node
	telemetry    store.Telemetry
	hasTelemetry bool
	booked       store.BookingSums // without the pod itself
	tombstoned   bool
	// tap is the sandbox whose pod's TAP device has the pod's name on the
	// node (only a pod with a UUID can share one: ADR 0019); "": none.
	tap       podid.ID
	connected bool

	pod           podid.ID
	vcpus, memMiB int64
	// diskGiB is the pod's disk of its own size; 0: the guest's, which
	// books nothing.
	diskGiB         int64
	pin             string // the only node a create may go to; "": any
	avoid           []string
	restore         bool
	checkpointStore string
	diskBudget      int64
}

// check evaluates gates in order and returns the first that refuses, or
// nil.
func (in *gateInput) check(gates []int) *refusal {
	for _, g := range gates {
		if text := in.gate(g); text != "" {
			return in.refuse(g, text)
		}
	}
	return nil
}

func (in *gateInput) refuse(g int, text string) *refusal {
	return &refusal{node: in.node.Name, gate: g, text: text}
}

// gate is the refusal's reason of gate g, or "" when it passes.
func (in *gateInput) gate(g int) string {
	n := &in.node
	switch g {
	case gateStatus:
		if n.Status != model.NodeReady {
			return "node " + n.Status
		}
	case gateDraining:
		if n.Draining {
			return "draining"
		}
	case gateForked:
		if n.Forked() {
			return "identity forked"
		}
	case gateConnected:
		if !in.connected {
			return "not connected to this instance"
		}
	case gateTombstone:
		if in.tombstoned {
			return "deleting an earlier pod of this ID"
		}
	case gatePin:
		if in.pin != "" && n.Name != in.pin {
			return "pinned to node " + in.pin + ": its volume is there"
		}
	case gateAvoid:
		if slices.Contains(in.avoid, n.Name) {
			return "a create failed here"
		}
	case gateTAP:
		if !in.tap.IsZero() {
			return "TAP prefix taken by sandbox " + in.tap.String()
		}
	case gateStore:
		if !in.restore {
			return ""
		}
		if in.checkpointStore != "" && n.ObjectStoreID != in.checkpointStore {
			return "another object store"
		}
		if in.checkpointStore == "" && n.ObjectStoreID == "" && !n.ObjectStore {
			return "another object store"
		}
	case gateSlots:
		if n.Slots <= 0 {
			return "no slots"
		}
		if in.booked.Count >= int64(n.Slots) {
			return fmt.Sprintf("slots full (%d of %d)", in.booked.Count, n.Slots)
		}
	case gateCPU:
		if c := n.VCPUCapacity(); c > 0 && in.booked.VCPUs+in.vcpus > c {
			return fmt.Sprintf("cpu gate (%d+%d of %d vCPUs)", in.booked.VCPUs, in.vcpus, c)
		}
	case gateMemory:
		if c := n.MemoryCapacityMiB(); in.booked.MemoryMiB+in.memMiB > c {
			return fmt.Sprintf("memory gate (%dMiB+%dMiB of %dMiB MiB)", in.booked.MemoryMiB, in.memMiB, c)
		}
	case gateFreeMemory:
		free := (in.telemetry.MemoryAvailableBytes - n.MemHeadroomBytes) >> 20
		if free < in.memMiB {
			return fmt.Sprintf("free memory gate (%d MiB available)", max(free, 0))
		}
	case gateDisk:
		need := in.diskBudget + in.diskGiB<<30
		if in.telemetry.DiskAvailableBytes >= need {
			return ""
		}
		if in.diskGiB == 0 {
			return "disk gate"
		}
		return fmt.Sprintf("disk gate (%d GiB available, %d GiB for the pod's disk and the budget)",
			max(in.telemetry.DiskAvailableBytes, 0)>>30, need>>30)
	case gateDiskBooked:
		if in.diskGiB == 0 {
			return ""
		}
		if c := diskCapacityGiB(n, in.diskBudget); in.booked.DiskGiB+in.diskGiB > c {
			return fmt.Sprintf("disk booking gate (%dGiB+%dGiB of %dGiB)", in.booked.DiskGiB, in.diskGiB, c)
		}
	}
	return ""
}

// diskCapacityGiB is what the node's pods may book of its disk for disks
// of their own size: its total less the disk budget, never below 0. A
// disk of the guest's size books nothing: the slots and the budget cover
// those.
func diskCapacityGiB(n *store.Node, budget int64) int64 {
	return max(n.DiskTotalBytes-budget, 0) >> 30
}

// score is the free share of the node's scarcest resource after the pod
// (the old freeShare): CPU when it is gated, the memory capacity, the
// live memory, and the disk's capacity for disks of their own size once
// any is booked or asked for.
func (in *gateInput) score() float64 {
	n := &in.node
	s := 1.0
	if c := diskCapacityGiB(n, in.diskBudget); c > 0 && in.booked.DiskGiB+in.diskGiB > 0 {
		s = min(s, float64(c-in.booked.DiskGiB-in.diskGiB)/float64(c))
	}
	if c := n.VCPUCapacity(); c > 0 {
		s = min(s, float64(c-in.booked.VCPUs-in.vcpus)/float64(c))
	}
	if c := n.MemoryCapacityMiB(); c > 0 {
		s = min(s, float64(c-in.booked.MemoryMiB-in.memMiB)/float64(c))
	}
	if total := n.MemoryTotalBytes; total > 0 {
		s = min(s, float64(in.telemetry.MemoryAvailableBytes-in.memMiB<<20)/float64(total))
	}
	return s
}

// placement is a placement decision from a snapshot: the feasible nodes in
// the order the policy prefers them, every node's verdict for the audit,
// and the nodes that only a tombstone of the pod keeps it off.
type placement struct {
	candidates    []string
	verdicts      []store.PlacementCandidate
	tombstoneOnly []string
}

// noCapacity is the description and the error of a placement that found
// no node.
func (p *placement) noCapacity() string {
	var b strings.Builder
	b.WriteString("no capacity (retryable): no feasible node")
	for _, v := range p.verdicts {
		if !v.Feasible {
			fmt.Fprintf(&b, "; %s: %s", v.Node, v.Reason)
		}
	}
	return b.String()
}

// place scores every node for the sandbox's pod from a snapshot read (the
// nodes, their telemetry and bookings, this pod's tombstones, TAP
// conflicts and peers). The op's begin claims the first candidate that
// passes the gates again under its lock. The nodes that pass rank by the
// pod's peers first, so that a service's replicas go to different nodes
// under either policy (ADR 0018): fewer of the service's other instances
// of its template, then fewer of another; then a restore's record node,
// then the policy's score, then the name. The peers rank and never
// refuse: a replica goes beside another when every node that passes holds
// one. A pod that is no service's instance has no peers.
func (e *Engine) place(ctx context.Context, sb store.Sandbox, kind OpKind) (placement, error) {
	snaps, err := e.st.PlacementSnapshot(ctx, sb.ID)
	if err != nil {
		return placement{}, fmt.Errorf("placement of sandbox %s: %w", sb.ID, err)
	}
	type scored struct {
		name  string
		peers store.Peers
		score float64
	}
	var p placement
	var feasible []scored
	for _, ns := range snaps {
		in := e.gateInput(sb, kind, ns.Node)
		in.telemetry, in.hasTelemetry = ns.Telemetry, ns.HasTelemetry
		in.booked, in.tombstoned, in.tap = ns.Booked, ns.Tombstoned, ns.TAPConflict
		v := store.PlacementCandidate{Node: ns.Node.Name, Peers: ns.Peers}
		if r := in.check(placementGates); r != nil {
			v.Reason = r.text
			if r.gate == gateTombstone {
				in.tombstoned = false
				if in.check(placementGates) == nil {
					p.tombstoneOnly = append(p.tombstoneOnly, ns.Node.Name)
				}
			}
		} else {
			v.Feasible, v.Score = true, in.score()
			feasible = append(feasible, scored{ns.Node.Name, ns.Peers, v.Score})
		}
		p.verdicts = append(p.verdicts, v)
	}
	record := ""
	if kind == OpRestore {
		record = sb.Node
	}
	slices.SortFunc(feasible, func(a, b scored) int {
		if c := cmp.Compare(a.peers.Same, b.peers.Same); c != 0 {
			return c
		}
		if c := cmp.Compare(a.peers.Other, b.peers.Other); c != 0 {
			return c
		}
		if record != "" && (a.name == record) != (b.name == record) {
			if a.name == record {
				return -1
			}
			return 1
		}
		if a.score != b.score {
			less := a.score < b.score
			if e.cfg.Placement == PlacementSpread {
				less = !less
			}
			if less {
				return -1
			}
			return 1
		}
		return strings.Compare(a.name, b.name)
	})
	for _, f := range feasible {
		p.candidates = append(p.candidates, f.name)
	}
	return p, nil
}

// peersCame refuses target to a create or restore of a service's replica
// when another instance of the service came there since the placement's
// snapshot, whose verdicts say what it saw: replicas placed at once rank
// the nodes from the same snapshot and would all claim the first. The
// caller holds the node's row lock, which every claim of the node takes,
// so the count is the latest. A target without a verdict passes.
func peersCame(ctx context.Context, tx *store.Store, sb store.Sandbox, target string,
	verdicts []store.PlacementCandidate) error {
	i := slices.IndexFunc(verdicts, func(v store.PlacementCandidate) bool { return v.Node == target })
	if !sb.Instance || i < 0 {
		return nil
	}
	now, err := tx.NodePeers(ctx, sb.ID, target)
	if err != nil {
		return err
	}
	if seen := verdicts[i].Peers; now.Same > seen.Same || now.Other > seen.Other {
		return &refusal{node: target, gate: gatePeers, text: "another instance of the service came since the placement"}
	}
	return nil
}

// gateInput is the pod's side of the gates for a sandbox's op on node. A
// pinned row's create goes only to its pin_node (an addon's volume is
// there); its restore goes anywhere, since the checkpoint carries the
// volume; a start or resume goes to the row's own node, where its pod and
// the volume attached to it are, whatever pin_node says (a restore
// elsewhere may not have moved it yet). A pinned row avoids no node: a
// create that failed on its only node must not lock it out of it.
func (e *Engine) gateInput(sb store.Sandbox, kind OpKind, n store.Node) gateInput {
	in := gateInput{
		node: n, connected: e.conns.Connected(n.Name), pod: sb.ID,
		vcpus: int64(sb.VCPUs), memMiB: int64(sb.MemoryMiB), diskGiB: int64(sb.DiskGiB), avoid: sb.AvoidNodes,
		restore: kind == OpRestore, checkpointStore: sb.CheckpointStore, diskBudget: e.cfg.DiskBudget,
	}
	if sb.PinNode != "" {
		in.avoid = nil
		if kind == OpCreate {
			in.pin = sb.PinNode
		}
	}
	return in
}

// claim rechecks the gates of an op's begin on the target node, whose row
// it locks FOR UPDATE, against the bookings without the pod: the gates of
// a placement's claim for a create or restore, those of the admission on a
// pinned node for a start or resume. The caller holds the sandbox's row.
func (e *Engine) claim(ctx context.Context, tx *store.Store, sb store.Sandbox, kind OpKind, target string) error {
	n, err := tx.LockNode(ctx, target, store.ForUpdate)
	if err != nil {
		if err == store.ErrNoRow {
			return &refusal{node: target, gate: gateStatus, text: "node unknown"}
		}
		return err
	}
	in := e.gateInput(sb, kind, n)
	if in.booked, err = tx.SumBookings(ctx, target, sb.ID); err != nil {
		return err
	}
	gates := admitGates
	if kind == OpCreate || kind == OpRestore {
		gates = claimGates
		if in.tombstoned, err = tx.HasTombstone(ctx, sb.ID, target); err != nil {
			return err
		}
		if in.tap, err = tx.TAPConflict(ctx, target, sb.ID); err != nil {
			return err
		}
	}
	if r := in.check(gates); r != nil {
		return r
	}
	return nil
}

// capacityRefusal evaluates the capacity gates of a booking that reality
// makes, under the node's row lock the caller took; "" when they pass.
func capacityRefusal(n store.Node, sb store.Sandbox, booked store.BookingSums, diskBudget int64) string {
	in := gateInput{node: n, booked: booked, vcpus: int64(sb.VCPUs), memMiB: int64(sb.MemoryMiB),
		diskGiB: int64(sb.DiskGiB), diskBudget: diskBudget}
	if r := in.check(capacityGates); r != nil {
		return r.text
	}
	return ""
}

// admissionError is the error of a refused admission on a pinned node: a
// capacity or draining gate is ErrNoCapacity, a node that is not ready,
// forked or not connected is ErrNodeUnavailable.
func admissionError(r *refusal, id podid.ID) error {
	switch r.gate {
	case gateStatus, gateForked, gateConnected:
		return &admission{refusal: r, err: errorf(model.ErrNodeUnavailable, "node %s is unavailable: %s", r.node, r.text)}
	case gateDraining:
		return &admission{refusal: r, err: errorf(model.ErrNoCapacity, "node %s is draining", r.node)}
	}
	return &admission{refusal: r, err: errorf(model.ErrNoCapacity, "no capacity on node %s for sandbox %s (retryable): %s",
		r.node, id, r.text)}
}

// admission is a refused admission: the error a caller gets, and the gate
// that refused.
type admission struct {
	refusal *refusal
	err     error
}

func (a *admission) Error() string { return a.err.Error() }
func (a *admission) Unwrap() error { return a.err }
