// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/bindings/httpcheck"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Limits of one EventBatch (fleet.proto).
const (
	maxBatchEvents = 512
	maxBatchBytes  = 1 << 20
	// auditKindCap bounds the kind of an audit record, in bytes of
	// [a-z0-9_].
	auditKindCap = 64
)

// The traffic record kinds: the names of TrafficRecord's oneof.
const (
	trafficFlow       = "flow"
	trafficDrop       = "drop"
	trafficDNS        = "dns"
	trafficServerName = "server_name"
)

// publishEvents is PublishEvents (§8). The batches of a stream are handled
// strictly in order, one at a time: checked for structure, sanitised,
// stored in one transaction under the lock of their start's row, and only
// then acknowledged. A malformed batch ends the stream with
// INVALID_ARGUMENT, one that cannot be stored with UNAVAILABLE, before any
// later batch is looked at. After the commit every pod event of the batch,
// duplicates included, goes to the sandbox engine, also when the ack no
// longer reaches the node: the batch is stored either way.
func (s *Service) publishEvents(stream grpc.BidiStreamingServer[fleetv1.EventBatch, fleetv1.EventAck]) error {
	ctx := stream.Context()
	p, err := nodePeer(ctx)
	if err != nil {
		return err
	}
	for {
		b, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		startID, err := checkBatch(b)
		if err != nil {
			s.rl.warn("events-malformed", "malformed event batch; ending the stream", "node", p.Node, "err", err)
			return status.Error(codes.InvalidArgument, err.Error())
		}
		last, unknown, err := s.ingest(ctx, p.Node, startID, b)
		if err != nil {
			s.rl.warn("events-store", "cannot store an event batch; ending the stream", "node", p.Node, "start_id", startID, "err", err)
			return status.Errorf(codes.Unavailable, "store events: %v", err)
		}
		if unknown > 0 {
			total := s.unknownEvents.Add(unknown)
			s.rl.warn("events-unknown", "events without a known body; stored as unknown",
				"node", p.Node, "start_id", startID, "events", unknown, "total", total)
		}
		sendErr := stream.Send(&fleetv1.EventAck{StartId: b.GetStartId(), Seq: uint64(last)})
		// The lookup of afterIngest must not end with a stream that broke
		// after the commit.
		s.afterIngest(context.WithoutCancel(ctx), p.Node, b)
		if sendErr != nil {
			return sendErr
		}
	}
}

// UnknownEvents is how many events without a known body this instance
// stored (a newer feosd's during a rolling upgrade), counted once each,
// after their batch committed.
func (s *Service) UnknownEvents() int64 { return s.unknownEvents.Load() }

// checkBatch checks a batch's structure, before anything is stored: the
// start is a UUID; at most 512 events and 1 MiB; every number is 1 to
// math.MaxInt64 and the numbers ascend strictly.
func checkBatch(b *fleetv1.EventBatch) (uuid.UUID, error) {
	startID, err := uuid.Parse(b.GetStartId())
	if err != nil {
		return uuid.Nil, fmt.Errorf("start_id %q is not a UUID", activity.Clean(b.GetStartId()))
	}
	if n := len(b.GetEvents()); n > maxBatchEvents {
		return uuid.Nil, fmt.Errorf("a batch of %d events; at most %d", n, maxBatchEvents)
	}
	if n := proto.Size(b); n > maxBatchBytes {
		return uuid.Nil, fmt.Errorf("a batch of %d bytes; at most %d", n, maxBatchBytes)
	}
	var prev uint64
	for _, ev := range b.GetEvents() {
		seq := ev.GetSeq()
		if seq == 0 || seq > math.MaxInt64 {
			return uuid.Nil, fmt.Errorf("sequence number %d is out of range", seq)
		}
		if seq <= prev {
			return uuid.Nil, fmt.Errorf("sequence number %d after %d: the numbers must ascend", seq, prev)
		}
		prev = seq
	}
	return startID, nil
}

// ingest stores a checked batch in one transaction and returns the start's
// last stored number, the acknowledgement, and how many of the stored
// events had no known body. Numbers already stored are dropped; numbers
// skipped are explained by the node's newly dropped count first, and the
// rest is recorded as gaps. An empty batch asks where the control plane is
// and is answered at once. The figures are those of the attempt that
// committed: the transaction may run more than once.
func (s *Service) ingest(ctx context.Context, node string, startID uuid.UUID, b *fleetv1.EventBatch) (last, unknown int64, err error) {
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		last, unknown = 0, 0
		ns, err := tx.LockNodeStart(ctx, node, startID)
		if err != nil {
			return err
		}
		last = ns.LastSeq
		if len(b.GetEvents()) == 0 {
			return nil
		}
		dropped := clampInt64(b.GetDropped())
		budget := max(0, dropped-ns.Dropped)
		in := ingestion{s: s, tx: tx, node: node, startID: startID, attr: map[podid.ID]attribution{}}
		prev := ns.LastSeq
		for _, ev := range b.GetEvents() {
			seq := int64(ev.GetSeq())
			if seq <= ns.LastSeq {
				continue
			}
			if seq > prev+1 {
				from, to := prev+1, seq-1
				if missing := to - from + 1; budget >= missing {
					budget -= missing
				} else {
					in.gaps = append(in.gaps, [2]int64{from + budget, to})
					budget = 0
				}
			}
			prev = seq
			if err := in.add(ctx, ev); err != nil {
				return err
			}
		}
		if err := in.store(ctx); err != nil {
			return err
		}
		if err := tx.AdvanceNodeStart(ctx, node, startID, prev, dropped); err != nil {
			return err
		}
		last, unknown = prev, in.unknown
		return nil
	})
	return last, unknown, err
}

// attribution is the sandbox a pod-scoped record of the batch belongs to.
type attribution struct {
	store.Attribution
	ok bool
}

// ingestion collects the rows of one batch inside its transaction.
type ingestion struct {
	s        *Service
	tx       *store.Store
	node     string
	startID  uuid.UUID
	attr     map[podid.ID]attribution
	acts     []store.ActivityRow
	traffic  []store.TrafficRow
	gaps     [][2]int64
	stopping bool
	// egress are the batch's counts of the pods' requests through their
	// HTTP bindings.
	egress map[egressKey]*store.EgressCount
	// unknown counts the events without a known body; they are added to
	// the Service's count once the batch committed.
	unknown int64
}

// attribute decides whether a record from the node about the pod named
// podID is its sandbox's (§8 "Attribution"): the row exists and the pod is
// or was on this node. Otherwise the record is foreign: kept for the
// node's audit, never shown on a sandbox.
func (in *ingestion) attribute(ctx context.Context, podID string) (podid.ID, attribution, error) {
	id, err := podid.Parse(podID)
	if err != nil {
		return "", attribution{}, nil
	}
	if a, ok := in.attr[id]; ok {
		return id, a, nil
	}
	a, ok, err := in.tx.AttributePod(ctx, id, in.node)
	if err != nil {
		return "", attribution{}, err
	}
	in.attr[id] = attribution{Attribution: a, ok: ok}
	return id, in.attr[id], nil
}

// row returns an activity row of the node's event ev, attributed.
func (in *ingestion) row(ev *fleetv1.Event, source, kind, category, name string, detail any) store.ActivityRow {
	return store.ActivityRow{
		TsNs: eventTime(ev.GetTime()), Node: in.node, StartID: in.startID, Seq: int64(ev.GetSeq()),
		Source: source, Kind: kind, Category: category, Name: activity.Name(name), Detail: detailJSON(detail),
	}
}

func (r *attribution) apply(row *store.ActivityRow, podID podid.ID) {
	row.PodID = podID
	if r.ok {
		row.SandboxID, row.TenantID = r.SandboxID, activity.Clean(r.TenantID)
	} else {
		row.ForeignPod = true
	}
}

// add maps one new event to its rows (§8 "Mapping").
func (in *ingestion) add(ctx context.Context, ev *fleetv1.Event) error {
	switch body := ev.GetBody().(type) {
	case *fleetv1.Event_Pod:
		if body.Pod == nil {
			break
		}
		pe := body.Pod
		state := pe.GetState().String()
		short := strings.TrimPrefix(state, "POD_STATE_")
		reason := activity.Clean(pe.GetReason())
		name := "pod " + short
		if pe.GetDeleted() {
			name = "pod deleted"
		}
		if reason != "" {
			name += " (" + reason + ")"
		}
		id, a, err := in.attribute(ctx, pe.GetPodId())
		if err != nil {
			return err
		}
		row := in.row(ev, store.SourceFeosd, activity.KindPodState, activity.CategoryLifecycle, name,
			map[string]any{"state": state, "reason": reason, "deleted": pe.GetDeleted()})
		a.apply(&row, id)
		in.acts = append(in.acts, row)
		return nil
	case *fleetv1.Event_Audit:
		if body.Audit == nil {
			break
		}
		rec := body.Audit
		kind := auditKind(rec.GetKind())
		detail, fields := auditDetail(rec.GetDetailJson())
		switch kind {
		case store.KindHTTPRequest, store.KindHTTP:
			return in.addEgress(ctx, rec.GetPodId(), detail, fields)
		case store.KindHTTPEnd:
			// A request's end: the sink's alone.
			return nil
		}
		id, a, err := in.attribute(ctx, rec.GetPodId())
		if err != nil {
			return err
		}
		row := in.row(ev, store.SourceAuthn, kind, activity.CategoryCredential, auditName(kind, fields), nil)
		row.Detail = detail
		a.apply(&row, id)
		in.acts = append(in.acts, row)
		return nil
	case *fleetv1.Event_Traffic:
		rec := body.Traffic
		kind := trafficKind(rec)
		if kind == "" {
			break
		}
		id, a, err := in.attribute(ctx, rec.GetPodId())
		if err != nil {
			return err
		}
		ts := eventTime(rec.GetTime())
		if rec.GetTime() == nil {
			ts = eventTime(ev.GetTime())
		}
		row := store.TrafficRow{
			TsNs: ts, Node: in.node, StartID: in.startID, Seq: int64(ev.GetSeq()), PodID: id,
			Kind: kind, Missed: clampInt64(rec.GetMissed()), Detail: trafficDetail(rec),
		}
		if a.ok {
			row.SandboxID, row.TenantID = a.SandboxID, activity.Clean(a.TenantID)
		} else {
			row.ForeignPod = true
		}
		in.traffic = append(in.traffic, row)
		return nil
	case *fleetv1.Event_Node:
		if body.Node == nil {
			break
		}
		ne := body.Node
		k, message := activity.Clean(ne.GetKind()), activity.Clean(ne.GetMessage())
		kind, name := activity.KindNodeEvent, "node event "+k
		switch k {
		case "started":
			kind, name = activity.KindNodeStarted, "feosd started"
		case "stopping":
			kind, name = activity.KindNodeStopping, "feosd stopping"
			in.stopping = true
		}
		if message != "" {
			name += ": " + message
		}
		in.acts = append(in.acts, in.row(ev, store.SourceFeosd, kind, activity.CategoryLifecycle, name,
			map[string]any{"kind": k, "message": message}))
		return nil
	}
	// No body this ferund knows: the oneof unset, or a member of a newer
	// feosd during a rolling upgrade. It is stored and acknowledged like
	// any other, never refused.
	in.unknown++
	in.acts = append(in.acts, in.row(ev, store.SourceFeosd, activity.KindNodeEvent, activity.CategoryLifecycle,
		"unknown event", map[string]any{"unknown": true}))
	return nil
}

// egressKey is a pod's binding.
type egressKey struct {
	sandbox podid.ID
	binding string
}

// addEgress counts the record of a pod's request through one of its HTTP
// bindings for the bindings' incidents (store.AddEgress): only when it is
// its sandbox's, is of type http, names a binding and counts for it. A
// pod's requests are the sink's records (ADR 0026): no activity row is
// kept of any of them, so that neither a busy pod's requests fill
// Postgres nor their retention keeps them for weeks.
func (in *ingestion) addEgress(ctx context.Context, podID string, detail json.RawMessage, f map[string]any) error {
	if f == nil {
		return nil
	}
	if t, ok := f["type"]; ok && t != store.KindHTTP {
		return nil
	}
	binding, _ := f["binding"].(string)
	if httpcheck.Name(binding) != nil {
		return nil
	}
	status := 0
	if n, ok := f["status"].(json.Number); ok {
		if v, err := strconv.Atoi(n.String()); err == nil && v >= 0 && v <= 999 {
			status = v
		}
	}
	var code string
	refusal := false
	if e, ok := f["error"]; ok && e != nil {
		refusal = true
		if m, ok := e.(map[string]any); ok {
			code, _ = m["code"].(string)
		}
	}
	class := store.ClassifyEgress(status, refusal, code)
	if !class.Counted {
		return nil
	}
	_, a, err := in.attribute(ctx, podID)
	if err != nil || !a.ok {
		return err
	}
	if in.egress == nil {
		in.egress = map[egressKey]*store.EgressCount{}
	}
	k := egressKey{sandbox: a.SandboxID, binding: binding}
	c := in.egress[k]
	if c == nil {
		c = &store.EgressCount{SandboxID: a.SandboxID, Binding: binding}
		in.egress[k] = c
	}
	c.Requests++
	c.Refused += b2i(class.Refused)
	c.Failing += b2i(class.Failing)
	c.TokenFailed += b2i(class.TokenFailed)
	c.Records = append(c.Records, detail)
	if len(c.Records) > store.EgressKeep {
		c.Records = c.Records[len(c.Records)-store.EgressKeep:]
	}
	return nil
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// store writes the batch's rows, its gaps and a "stopping" node event's
// rollout deadline.
func (in *ingestion) store(ctx context.Context) error {
	for _, g := range in.gaps {
		if err := in.tx.InsertEventGap(ctx, in.node, in.startID, g[0], g[1]); err != nil {
			return err
		}
	}
	if len(in.egress) > 0 {
		counts := make([]store.EgressCount, 0, len(in.egress))
		for _, c := range in.egress {
			counts = append(counts, *c)
		}
		if err := in.tx.AddEgress(ctx, counts); err != nil {
			return err
		}
	}
	if len(in.acts) > 0 {
		if err := in.tx.InsertActivity(ctx, in.acts...); err != nil {
			return err
		}
	}
	if len(in.traffic) > 0 {
		if err := in.tx.InsertTraffic(ctx, in.traffic...); err != nil {
			return err
		}
	}
	if in.stopping {
		if _, err := in.tx.SetNodeStopping(ctx, in.node, in.s.cfg.RolloutGrace); err != nil {
			return err
		}
	}
	return nil
}

// afterIngest hands every pod event of the stored batch, duplicates
// included, to the engine, and requests a reconcile of the node when one
// of them names a pod whose row exists but is placed on neither this node
// nor an op targeting it. Events about pods without a row request
// nothing: the inventory hash and the sweep catch ferund's orphans.
func (s *Service) afterIngest(ctx context.Context, node string, b *fleetv1.EventBatch) {
	var ids []podid.ID
	seen := map[podid.ID]bool{}
	for _, ev := range b.GetEvents() {
		pe := ev.GetPod()
		if pe == nil {
			continue
		}
		id, err := podid.Parse(pe.GetPodId())
		if err != nil {
			continue
		}
		if s.hooks != nil {
			s.hooks.PodChanged(node, id)
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	elsewhere, err := s.st.SandboxesElsewhere(qctx, node, ids)
	if err != nil {
		s.rl.warn("events-placement", "cannot look up the pods of an event batch", "node", node, "err", err)
		return
	}
	if len(elsewhere) > 0 {
		s.requestReconcile(node)
	}
}

// eventTime is a record's time in Unix nanoseconds, or now when it has
// none or one beyond what nanoseconds hold.
func eventTime(t *timestamppb.Timestamp) int64 {
	const maxSeconds = math.MaxInt64/int64(time.Second) - 1
	if t == nil || !t.IsValid() || t.GetSeconds() > maxSeconds || t.GetSeconds() < -maxSeconds {
		return time.Now().UnixNano()
	}
	return t.AsTime().UnixNano()
}

// detailJSON encodes a row's detail as a JSON object storable as jsonb.
func detailJSON(v any) json.RawMessage {
	if v == nil {
		return json.RawMessage("{}")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return activity.CleanJSON(b)
}

// auditKind cuts an audit record's kind to at most 64 bytes of [a-z0-9_];
// what remains of nothing is "unknown".
func auditKind(k string) string {
	var b strings.Builder
	for i := 0; i < len(k) && b.Len() < auditKindCap; i++ {
		c := k[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' {
			b.WriteByte(c)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// auditDetail stores an audit record's detail_json as a jsonb object when
// it parses as a JSON object without a \u0000 escape, otherwise as
// {"raw": "<sanitised text>"}. It also returns the object's fields, for the
// row's name.
func auditDetail(text string) (json.RawMessage, map[string]any) {
	raw := func() json.RawMessage {
		b, _ := json.Marshal(map[string]string{"raw": activity.Clean(text)})
		return activity.CleanJSON(b)
	}
	if strings.Contains(text, `\u0000`) {
		return raw(), nil
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return raw(), nil
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return raw(), nil
	}
	out := activity.CleanJSON([]byte(text))
	if len(out) == 0 || !bytes.HasPrefix(bytes.TrimSpace(out), []byte("{")) {
		return raw(), nil
	}
	return out, fields
}

// auditName is an audit record's row name: "<method> <path> <status>" of a
// kube_request, "ssh signature (<binding>)" of an ssh_signature,
// "denied: <reason>" of a kube_deny or ssh_deny, the kind otherwise.
func auditName(kind string, f map[string]any) string {
	field := func(k string) string {
		v, ok := f[k]
		if !ok || v == nil {
			return ""
		}
		if s, ok := v.(string); ok {
			return activity.Clean(s)
		}
		return activity.Clean(fmt.Sprint(v))
	}
	switch kind {
	case "kube_request":
		return strings.Join(strings.Fields(field("method")+" "+field("path")+" "+field("status")), " ")
	case "ssh_signature":
		return "ssh signature (" + field("binding") + ")"
	case "kube_deny", "ssh_deny":
		return "denied: " + field("reason")
	}
	return kind
}

// trafficKind is the name of a traffic record's oneof, "" when unset.
func trafficKind(rec *networkv1.TrafficRecord) string {
	switch rec.GetRecord().(type) {
	case *networkv1.TrafficRecord_Flow:
		return trafficFlow
	case *networkv1.TrafficRecord_Drop:
		return trafficDrop
	case *networkv1.TrafficRecord_Dns:
		return trafficDNS
	case *networkv1.TrafficRecord_ServerName:
		return trafficServerName
	}
	return ""
}

// trafficDetail is the whole record as protojson with the proto's field
// names, storable as jsonb.
func trafficDetail(rec *networkv1.TrafficRecord) json.RawMessage {
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(rec)
	if err != nil {
		return json.RawMessage("{}")
	}
	return activity.CleanJSON(b)
}
