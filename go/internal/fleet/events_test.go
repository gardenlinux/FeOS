// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

func podEvent(seq uint64, pod string, state podv1.PodState, reason string) *fleetv1.Event {
	return &fleetv1.Event{Seq: seq, Time: timestamppb.Now(), Body: &fleetv1.Event_Pod{Pod: &podv1.PodEvent{PodId: pod, State: state, Reason: reason}}}
}

func batch(start uuid.UUID, dropped uint64, events ...*fleetv1.Event) *fleetv1.EventBatch {
	return &fleetv1.EventBatch{StartId: start.String(), Events: events, Dropped: dropped}
}

func seqs(from, to uint64, pod string) []*fleetv1.Event {
	var out []*fleetv1.Event
	for s := from; s <= to; s++ {
		out = append(out, podEvent(s, pod, podv1.PodState_POD_STATE_RUNNING, ""))
	}
	return out
}

func ackSeqs(acks []*fleetv1.EventAck) []uint64 {
	out := make([]uint64, len(acks))
	for i, a := range acks {
		out[i] = a.GetSeq()
	}
	return out
}

func equalSeqs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (h *harness) count(sql string, args ...any) int {
	h.t.Helper()
	var n int
	h.scalar(&n, sql, args...)
	return n
}

func (h *harness) gaps(start uuid.UUID) [][2]int64 {
	h.t.Helper()
	rows, err := h.d.Pool.Query(bg, "SELECT from_seq, to_seq FROM event_gaps WHERE start_id = $1 ORDER BY from_seq", start)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out [][2]int64
	for rows.Next() {
		var g [2]int64
		if err := rows.Scan(&g[0], &g[1]); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, g)
	}
	return out
}

func TestPublishEventsDedupeAndGaps(t *testing.T) {
	h := newHarness(t)
	serial := h.joined("n1")
	start := uuid.New()
	pod := uuid.NewString()

	// An empty batch asks where the control plane is: 0 for a new start.
	acks, err := h.publish("n1", serial, batch(start, 0))
	if err != nil || !equalSeqs(ackSeqs(acks), []uint64{0}) || acks[0].GetStartId() != start.String() {
		t.Fatalf("acks %v, %v", acks, err)
	}
	acks, err = h.publish("n1", serial,
		batch(start, 0, seqs(1, 3, pod)...),
		batch(start, 0, seqs(2, 5, pod)...), // 2 and 3 again: dropped
		batch(start, 0),
	)
	if err != nil || !equalSeqs(ackSeqs(acks), []uint64{3, 5, 5}) {
		t.Fatalf("acks %v, %v", ackSeqs(acks), err)
	}
	if n := h.count("SELECT count(*) FROM activity WHERE start_id = $1 AND kind = 'pod_state'", start); n != 5 {
		t.Fatalf("%d pod rows; want 5, each number once", n)
	}
	// Every pod event goes to the engine, duplicates included.
	if got := len(h.hooks.changes()); got != 7 {
		t.Fatalf("%d PodChanged calls; want 7", got)
	}

	// Gaps: the newly dropped count explains skipped numbers first, each
	// counted once; the rest is recorded.
	acks, err = h.publish("n1", serial,
		batch(start, 1, podEvent(9, pod, podv1.PodState_POD_STATE_RUNNING, "")),      // 6–8 missing, 1 dropped: gap 7–8
		batch(start, 3, podEvent(12, pod, podv1.PodState_POD_STATE_RUNNING, "")),     // 10–11 missing, 2 more dropped: no gap
		batch(start, 3, seqs(14, 14, pod)[0], seqs(17, 17, pod)[0]),                  // 13 and 15–16 missing: gaps
		batch(start, 2, podEvent(18, pod, podv1.PodState_POD_STATE_RUNNING, "")),     // a lower count changes nothing
		batch(start, 10, podEvent(20, pod, podv1.PodState_POD_STATE_SUSPENDED, "x")), // 19 explained
	)
	if err != nil || !equalSeqs(ackSeqs(acks), []uint64{9, 12, 17, 18, 20}) {
		t.Fatalf("acks %v, %v", ackSeqs(acks), err)
	}
	want := [][2]int64{{7, 8}, {13, 13}, {15, 16}}
	got := h.gaps(start)
	if len(got) != len(want) {
		t.Fatalf("gaps %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("gaps %v; want %v", got, want)
		}
	}
	var lastSeq, dropped int64
	if err := h.d.Pool.QueryRow(bg, "SELECT last_seq, dropped FROM node_starts WHERE node = 'n1' AND start_id = $1", start).
		Scan(&lastSeq, &dropped); err != nil {
		t.Fatal(err)
	}
	if lastSeq != 20 || dropped != 10 {
		t.Fatalf("node start: last_seq %d, dropped %d", lastSeq, dropped)
	}

	// Another start of the node counts on its own.
	start2 := uuid.New()
	acks, err = h.publish("n1", serial, batch(start2, 0, seqs(1, 2, pod)...), batch(start, 0))
	if err != nil || !equalSeqs(ackSeqs(acks), []uint64{2, 20}) {
		t.Fatalf("acks %v, %v", ackSeqs(acks), err)
	}
}

func TestPublishEventsMalformed(t *testing.T) {
	h := newHarness(t)
	serial := h.joined("n1")
	start := uuid.New()
	pod := uuid.NewString()
	huge := &fleetv1.Event{Seq: 1, Body: &fleetv1.Event_Audit{Audit: &fleetv1.AuditRecord{
		PodId: pod, Kind: "kube_request", DetailJson: strings.Repeat("x", 1<<20)}}}
	tooMany := batch(start, 0, seqs(1, 513, pod)...)
	for name, b := range map[string]*fleetv1.EventBatch{
		"start not a UUID": {StartId: "start-1", Events: seqs(1, 1, pod)},
		"seq 0":            batch(start, 0, podEvent(0, pod, podv1.PodState_POD_STATE_RUNNING, "")),
		"seq too large":    batch(start, 0, podEvent(math.MaxInt64+1, pod, podv1.PodState_POD_STATE_RUNNING, "")),
		"not ascending":    batch(start, 0, seqs(2, 2, pod)[0], seqs(1, 1, pod)[0]),
		"repeated":         batch(start, 0, seqs(1, 1, pod)[0], seqs(1, 1, pod)[0]),
		"513 events":       tooMany,
		"over 1 MiB":       batch(start, 0, huge),
	} {
		// A valid batch first is stored and acknowledged; the malformed one
		// ends the stream, and the batch after it is never looked at.
		other := uuid.New()
		acks, err := h.publish("n1", serial, batch(other, 0, seqs(1, 1, pod)...), b, batch(other, 0, seqs(2, 2, pod)...))
		wantCode(t, err, codes.InvalidArgument, "")
		if !equalSeqs(ackSeqs(acks), []uint64{1}) {
			t.Fatalf("%s: acks %v", name, ackSeqs(acks))
		}
		if n := h.count("SELECT count(*) FROM activity WHERE start_id = $1", other); n != 1 {
			t.Fatalf("%s: %d rows stored of the stream", name, n)
		}
	}
	if n := h.count("SELECT count(*) FROM activity WHERE start_id = $1", start); n != 0 {
		t.Fatalf("%d rows of malformed batches stored", n)
	}
	_, err := h.publish("", "", batch(start, 0))
	wantCode(t, err, codes.Unauthenticated, "")
}

func TestPublishEventsStorageFailure(t *testing.T) {
	h := newHarness(t)
	serial := h.joined("n1")
	start := uuid.New()
	pod := uuid.NewString()
	acks, err := h.publish("n1", serial, batch(start, 0, seqs(1, 1, pod)...))
	if err != nil || len(acks) != 1 {
		t.Fatalf("acks %v, %v", acks, err)
	}
	// Today's partition of activity is gone: the batch cannot be stored.
	var today time.Time
	h.scalar(&today, "SELECT (now() AT TIME ZONE 'utc')::date")
	h.exec("DROP TABLE " + db.PartitionName(db.TableActivity, today))
	acks, err = h.publish("n1", serial, batch(start, 0, seqs(2, 3, pod)...), batch(start, 0, seqs(4, 4, pod)...))
	wantCode(t, err, codes.Unavailable, "store events")
	if len(acks) != 0 {
		t.Fatalf("acknowledged %v of what was not stored", ackSeqs(acks))
	}
	var lastSeq int64
	h.scalar(&lastSeq, "SELECT last_seq FROM node_starts WHERE start_id = $1", start)
	if lastSeq != 1 {
		t.Fatalf("last_seq %d after a failed batch", lastSeq)
	}
	// Restored, the node sends the batch again and it is stored.
	if err := h.d.EnsurePartitions(bg, today, today.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	acks, err = h.publish("n1", serial, batch(start, 0, seqs(2, 4, pod)...))
	if err != nil || !equalSeqs(ackSeqs(acks), []uint64{4}) {
		t.Fatalf("acks after the repair %v, %v", ackSeqs(acks), err)
	}
}

func TestPublishEventsContent(t *testing.T) {
	h := newHarness(t)
	serial := h.joined("n1")
	start := uuid.New()
	pod := uuid.NewString()
	events := []*fleetv1.Event{
		{Seq: 1, Time: timestamppb.New(time.Unix(1700000000, 5)), Body: &fleetv1.Event_Pod{Pod: &podv1.PodEvent{
			PodId: pod, State: podv1.PodState_POD_STATE_FAILED, Reason: "boom\x00 \xffbad"}}},
		{Seq: 2, Body: &fleetv1.Event_Audit{Audit: &fleetv1.AuditRecord{PodId: pod, Kind: "kube_request",
			DetailJson: `{"binding":"prod","method":"GET","path":"/api","status":200}`}}},
		{Seq: 3, Body: &fleetv1.Event_Audit{Audit: &fleetv1.AuditRecord{PodId: pod, Kind: "ssh_signature",
			DetailJson: `{"binding":"git"}`}}},
		{Seq: 4, Body: &fleetv1.Event_Audit{Audit: &fleetv1.AuditRecord{PodId: pod, Kind: "kube_deny",
			DetailJson: `{"reason":"port closed"}`}}},
		{Seq: 5, Body: &fleetv1.Event_Audit{Audit: &fleetv1.AuditRecord{PodId: pod, Kind: "Weird-Kind!" + strings.Repeat("z", 100),
			DetailJson: `[1,2]`}}},
		{Seq: 6, Body: &fleetv1.Event_Audit{Audit: &fleetv1.AuditRecord{PodId: pod, Kind: "tls_signature",
			DetailJson: `{"a":"b\u0000c"}`}}},
		{Seq: 7, Body: &fleetv1.Event_Audit{Audit: &fleetv1.AuditRecord{PodId: pod, Kind: "!!!", DetailJson: `{"a":1} trailing`}}},
		{Seq: 8}, // no body
		{Seq: 9, Body: &fleetv1.Event_Traffic{Traffic: &networkv1.TrafficRecord{PodId: pod}}}, // no record
		{Seq: 10, Body: &fleetv1.Event_Node{Node: &fleetv1.NodeEvent{Kind: "started"}}},
		{Seq: 11, Body: &fleetv1.Event_Node{Node: &fleetv1.NodeEvent{Kind: "rebooting", Message: "kernel update"}}},
		{Seq: 12, Time: timestamppb.New(time.Unix(1700000001, 0)), Body: &fleetv1.Event_Traffic{Traffic: &networkv1.TrafficRecord{
			PodId: pod, Missed: math.MaxUint64, Time: timestamppb.New(time.Unix(1700000002, 0)),
			Record: &networkv1.TrafficRecord_Dns{Dns: &networkv1.DnsQuery{}}}}},
	}
	acks, err := h.publish("n1", serial, batch(start, 0, events...))
	if err != nil || !equalSeqs(ackSeqs(acks), []uint64{12}) {
		t.Fatalf("acks %v, %v", ackSeqs(acks), err)
	}
	type row struct {
		source, kind, category, name, detail string
		tsNs                                 int64
	}
	rows := map[int64]row{}
	rs, err := h.d.Pool.Query(bg, "SELECT seq, source, kind, category, name, detail::text, ts_ns FROM activity WHERE start_id = $1", start)
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var seq int64
		var r row
		if err := rs.Scan(&seq, &r.source, &r.kind, &r.category, &r.name, &r.detail, &r.tsNs); err != nil {
			t.Fatal(err)
		}
		rows[seq] = r
	}
	rs.Close()
	check := func(seq int64, source, kind, category, name string) row {
		t.Helper()
		r, ok := rows[seq]
		if !ok || r.source != source || r.kind != kind || r.category != category || r.name != name {
			t.Fatalf("row %d = %+v; want %s %s %s %q", seq, r, source, kind, category, name)
		}
		return r
	}
	r := check(1, "feosd", "pod_state", "lifecycle", "pod FAILED (boom �bad)")
	var detail map[string]any
	if err := json.Unmarshal([]byte(r.detail), &detail); err != nil || detail["state"] != "POD_STATE_FAILED" ||
		detail["reason"] != "boom �bad" || detail["deleted"] != false || r.tsNs != 1700000000000000005 {
		t.Fatalf("pod row %+v: %v", r, err)
	}
	r = check(2, "authn", "kube_request", "credential", "GET /api 200")
	if !strings.Contains(r.detail, `"binding": "prod"`) {
		t.Fatalf("audit detail %s", r.detail)
	}
	check(3, "authn", "ssh_signature", "credential", "ssh signature (git)")
	check(4, "authn", "kube_deny", "credential", "denied: port closed")
	r = check(5, "authn", "eirdind"+strings.Repeat("z", 57), "credential", "eirdind"+strings.Repeat("z", 57))
	if r.detail != `{"raw": "[1,2]"}` {
		t.Fatalf("a detail that is no object: %s", r.detail)
	}
	r = check(6, "authn", "tls_signature", "credential", "tls_signature")
	if r.detail != `{"raw": "{\"a\":\"b\\u0000c\"}"}` {
		t.Fatalf("a detail with \\u0000: %s", r.detail)
	}
	r = check(7, "authn", "unknown", "credential", "unknown")
	if !strings.HasPrefix(r.detail, `{"raw": `) {
		t.Fatalf("a detail with trailing text: %s", r.detail)
	}
	for _, seq := range []int64{8, 9} {
		r := check(seq, "feosd", "node_event", "lifecycle", "unknown event")
		if r.detail != `{"unknown": true}` {
			t.Fatalf("unknown event %d detail %s", seq, r.detail)
		}
	}
	check(10, "feosd", "node_started", "lifecycle", "feosd started")
	r = check(11, "feosd", "node_event", "lifecycle", "node event rebooting: kernel update")
	if r.detail != `{"kind": "rebooting", "message": "kernel update"}` {
		t.Fatalf("node event detail %s", r.detail)
	}
	if h.s.unknownEvents.Load() != 2 {
		t.Fatalf("%d unknown events counted", h.s.unknownEvents.Load())
	}
	var kind, detail12 string
	var missed, tsNs int64
	if err := h.d.Pool.QueryRow(bg, "SELECT kind, missed, ts_ns, detail::text FROM traffic_records WHERE start_id = $1 AND seq = 12", start).
		Scan(&kind, &missed, &tsNs, &detail12); err != nil {
		t.Fatal(err)
	}
	if kind != "dns" || missed != math.MaxInt64 || tsNs != 1700000002000000000 || !strings.Contains(detail12, `"pod_id"`) {
		t.Fatalf("traffic record %s %d %d %s", kind, missed, tsNs, detail12)
	}
}

func TestPublishEventsAttribution(t *testing.T) {
	h := newHarness(t)
	serial := h.joined("n1")
	h.joined("n2")
	h.links.setConnected("n1", true)
	if _, _, err := h.st.CreateTenant(bg, "acme"); err != nil {
		t.Fatal(err)
	}
	// Short IDs and, made before ADR 0019, UUIDs.
	mkWith := func(id podid.ID) podid.ID {
		if _, _, err := h.st.CreateSandbox(bg, store.NewSandbox{ID: id, Owner: "acme", Image: "alpine", VCPUs: 1, MemoryMiB: 512}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mk := func() podid.ID { return mkWith(podid.Mint()) }
	legacy := func() podid.ID { return mkWith(podid.ID(uuid.NewString())) }
	onNode, elsewhere, target, lost, tomb, cert, oldCert := mk(), mk(), mk(), legacy(), legacy(), mk(), legacy()
	legacyOnNode, legacyElsewhere := legacy(), legacy()
	h.exec("UPDATE sandboxes SET node = 'n1', client_state = 'running' WHERE id = $1", onNode)
	h.exec("UPDATE sandboxes SET node = 'n1', client_state = 'running' WHERE id = $1", legacyOnNode)
	h.exec("UPDATE sandboxes SET node = 'n2', client_state = 'running' WHERE id = $1", legacyElsewhere)
	h.exec("UPDATE sandboxes SET node = 'n2', client_state = 'running' WHERE id = $1", elsewhere)
	h.exec(`UPDATE sandboxes SET node = 'n2', op = 'create', op_id = gen_random_uuid(), op_owner = 'i1',
		op_deadline = now() + interval '1 minute', op_target = 'n1', client_state = 'creating' WHERE id = $1`, target)
	h.exec("UPDATE sandboxes SET lost_from = 'n1', client_state = 'lost' WHERE id = $1", lost)
	h.exec("INSERT INTO pod_tombstones (pod_id, node, incarnation) VALUES ($1, 'n1', gen_random_uuid())", tomb)
	for id, age := range map[podid.ID]string{cert: "1 hour", oldCert: "2 days"} {
		h.exec(`INSERT INTO issued_certs (serial, pod_id, incarnation, node, spiffe_id, hpke_public_key, not_after, issued_at)
			VALUES ($1, $2, gen_random_uuid(), 'n1', 'spiffe://feos.test/x', '\x00', now(), now() - $3::interval)`,
			id.String(), id, age)
	}
	start := uuid.New()
	unknown, unknownLegacy := podid.Mint(), uuid.New()
	var events []*fleetv1.Event
	pods := []string{onNode.String(), elsewhere.String(), target.String(), lost.String(), tomb.String(),
		cert.String(), oldCert.String(), unknown.String(), "not-a-uuid", legacyOnNode.String(),
		strings.ToUpper(legacyOnNode.String()), legacyElsewhere.String(), unknownLegacy.String(), "Abcdefghjkm"}
	for i, p := range pods {
		events = append(events, podEvent(uint64(i+1), p, podv1.PodState_POD_STATE_RUNNING, ""))
	}
	events = append(events,
		&fleetv1.Event{Seq: 20, Body: &fleetv1.Event_Traffic{Traffic: &networkv1.TrafficRecord{PodId: onNode.String(),
			Record: &networkv1.TrafficRecord_Flow{Flow: &networkv1.Flow{BytesSent: 7}}}}},
		&fleetv1.Event{Seq: 21, Body: &fleetv1.Event_Traffic{Traffic: &networkv1.TrafficRecord{PodId: elsewhere.String(),
			Record: &networkv1.TrafficRecord_Drop{Drop: &networkv1.Drop{}}}}},
		&fleetv1.Event{Seq: 22, Body: &fleetv1.Event_Audit{Audit: &fleetv1.AuditRecord{PodId: elsewhere.String(), Kind: "kube_request", DetailJson: `{}`}}},
	)
	acks, err := h.publish("n1", serial, batch(start, 0, events...))
	if err != nil || len(acks) != 1 {
		t.Fatalf("acks %v, %v", acks, err)
	}
	attributed := map[podid.ID]bool{onNode: true, target: true, lost: true, tomb: true, cert: true, legacyOnNode: true}
	for i, p := range pods {
		var sandbox *podid.ID
		var tenant string
		var foreign bool
		if err := h.d.Pool.QueryRow(bg, "SELECT sandbox_id, tenant_id, foreign_pod FROM activity WHERE start_id = $1 AND seq = $2",
			start, i+1).Scan(&sandbox, &tenant, &foreign); err != nil {
			t.Fatal(err)
		}
		id, _ := podid.Parse(p)
		if attributed[id] {
			if sandbox == nil || *sandbox != id || tenant != "acme" || foreign {
				t.Fatalf("pod %s: sandbox %v tenant %q foreign %v; want attributed", p, sandbox, tenant, foreign)
			}
		} else if sandbox != nil || tenant != "" || !foreign {
			t.Fatalf("pod %s: sandbox %v tenant %q foreign %v; want a foreign record", p, sandbox, tenant, foreign)
		}
	}
	var sb *podid.ID
	var foreign bool
	h.scalar(&foreign, "SELECT foreign_pod FROM traffic_records WHERE seq = 20")
	if err := h.d.Pool.QueryRow(bg, "SELECT sandbox_id, foreign_pod FROM traffic_records WHERE seq = 20").Scan(&sb, &foreign); err != nil ||
		sb == nil || *sb != onNode || foreign {
		t.Fatalf("traffic of a pod on the node: %v %v %v", sb, foreign, err)
	}
	if err := h.d.Pool.QueryRow(bg, "SELECT sandbox_id, foreign_pod FROM traffic_records WHERE seq = 21").Scan(&sb, &foreign); err != nil ||
		sb != nil || !foreign {
		t.Fatalf("traffic of a pod elsewhere: %v %v %v", sb, foreign, err)
	}
	if n := h.count("SELECT count(*) FROM activity WHERE seq = 22 AND foreign_pod AND sandbox_id IS NULL AND source = 'authn'"); n != 1 {
		t.Fatal("an audit record of a pod elsewhere is attributed")
	}
	// A pod event about a row placed elsewhere asks for a reconcile of
	// the reporting node.
	h.expectReconcile("n1")
	if got := len(h.hooks.changes()); got != 12 {
		t.Fatalf("%d PodChanged calls; want one per pod event with a pod ID", got)
	}
}

func TestPublishEventsReconcileOnlyForRowsElsewhere(t *testing.T) {
	h := newHarness(t)
	serial := h.joined("n1")
	h.links.setConnected("n1", true)
	if _, _, err := h.st.CreateTenant(bg, "acme"); err != nil {
		t.Fatal(err)
	}
	onNode := podid.Mint()
	if _, _, err := h.st.CreateSandbox(bg, store.NewSandbox{ID: onNode, Owner: "acme", Image: "alpine", VCPUs: 1, MemoryMiB: 512}); err != nil {
		t.Fatal(err)
	}
	h.exec("UPDATE sandboxes SET node = 'n1' WHERE id = $1", onNode)
	start := uuid.New()
	// Rows on this node, and pods without a row (unlabelled pods and
	// orphans look alike): no reconcile.
	if _, err := h.publish("n1", serial, batch(start, 0,
		podEvent(1, onNode.String(), podv1.PodState_POD_STATE_RUNNING, ""),
		podEvent(2, uuid.NewString(), podv1.PodState_POD_STATE_RUNNING, ""))); err != nil {
		t.Fatal(err)
	}
	h.expectNoReconcile()
	// A row without a node: the node holds a pod the registry places
	// nowhere.
	h.exec("UPDATE sandboxes SET node = NULL WHERE id = $1", onNode)
	if _, err := h.publish("n1", serial, batch(start, 0, podEvent(3, onNode.String(), podv1.PodState_POD_STATE_RUNNING, ""))); err != nil {
		t.Fatal(err)
	}
	h.expectReconcile("n1")
	// A duplicate is handed to the engine and checked like a new one.
	h.allowReconcile("n1")
	before := len(h.hooks.changes())
	if _, err := h.publish("n1", serial, batch(start, 0, podEvent(3, onNode.String(), podv1.PodState_POD_STATE_RUNNING, ""))); err != nil {
		t.Fatal(err)
	}
	h.expectReconcile("n1")
	if got := h.hooks.changes(); len(got) != before+1 || got[len(got)-1] != "n1 "+onNode.String() {
		t.Fatalf("PodChanged %v after a duplicate", got)
	}
}

// TestPublishEventsAckLost covers a stream that breaks after a batch
// committed: the ack does not reach the node, but the batch is stored, so
// its pod events still go to the engine, and a pod placed elsewhere still
// requests a reconcile.
func TestPublishEventsAckLost(t *testing.T) {
	h := newHarness(t)
	serial := h.joined("n1")
	h.joined("n2")
	h.links.setConnected("n1", true)
	if _, _, err := h.st.CreateTenant(bg, "acme"); err != nil {
		t.Fatal(err)
	}
	pod := podid.Mint()
	if _, _, err := h.st.CreateSandbox(bg, store.NewSandbox{ID: pod, Owner: "acme", Image: "alpine", VCPUs: 1, MemoryMiB: 512}); err != nil {
		t.Fatal(err)
	}
	h.exec("UPDATE sandboxes SET node = 'n2', client_state = 'running' WHERE id = $1", pod)
	start := uuid.New()
	ctx, cancel := context.WithCancel(uplinkCtx("n1", serial))
	defer cancel()
	fs := &fakeStream{ctx: ctx, in: make(chan *fleetv1.EventBatch, 1), acks: make(chan *fleetv1.EventAck, 1),
		breakOnSend: cancel, sendErr: status.Error(codes.Unavailable, "transport is closing")}
	fs.in <- batch(start, 0, podEvent(1, pod.String(), podv1.PodState_POD_STATE_RUNNING, ""))
	close(fs.in)
	err := h.s.UplinkServer().PublishEvents(fs)
	wantCode(t, err, codes.Unavailable, "transport is closing")
	if n := h.count("SELECT count(*) FROM activity WHERE start_id = $1 AND seq = 1", start); n != 1 {
		t.Fatalf("%d rows of the batch stored", n)
	}
	if got := h.hooks.changes(); !contains(got, "n1 "+pod.String()) {
		t.Fatalf("PodChanged %v; want the pod of the stored batch", got)
	}
	h.expectReconcile("n1")
}

// TestPublishEventsUnknownCountedOnce covers the count of events without a
// known body: once per stored event, also when the transaction ran twice,
// and not at all for a batch that was not stored.
func TestPublishEventsUnknownCountedOnce(t *testing.T) {
	h := newHarness(t)
	serial := h.joined("n1")
	start := uuid.New()
	// The first attempt to advance the start fails as a serialization
	// failure would: db.Tx runs the transaction again.
	h.exec("CREATE SEQUENCE attempts")
	h.exec(`CREATE FUNCTION fail_first() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF nextval('attempts') = 1 THEN
				RAISE EXCEPTION 'injected serialization failure' USING ERRCODE = 'serialization_failure';
			END IF;
			RETURN NEW;
		END $$`)
	h.exec("CREATE TRIGGER fail_first BEFORE UPDATE ON node_starts FOR EACH ROW EXECUTE FUNCTION fail_first()")
	acks, err := h.publish("n1", serial, batch(start, 0, &fleetv1.Event{Seq: 1}, &fleetv1.Event{Seq: 2}))
	if err != nil || !equalSeqs(ackSeqs(acks), []uint64{2}) {
		t.Fatalf("acks %v, %v", ackSeqs(acks), err)
	}
	if n := h.count("SELECT last_value FROM attempts"); n != 2 {
		t.Fatalf("%d attempts; want the retry", n)
	}
	if got := h.s.UnknownEvents(); got != 2 {
		t.Fatalf("%d unknown events counted; want the 2 stored", got)
	}

	// A batch that cannot be stored counts nothing; its resend counts once.
	h.exec("DROP TRIGGER fail_first ON node_starts")
	h.exec(`CREATE FUNCTION fail_always() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected failure'; END $$`)
	h.exec("CREATE TRIGGER fail_always BEFORE UPDATE ON node_starts FOR EACH ROW EXECUTE FUNCTION fail_always()")
	acks, err = h.publish("n1", serial, batch(start, 0, &fleetv1.Event{Seq: 3}))
	wantCode(t, err, codes.Unavailable, "store events")
	if len(acks) != 0 || h.s.UnknownEvents() != 2 {
		t.Fatalf("acks %v, %d unknown events counted after a failed batch", ackSeqs(acks), h.s.UnknownEvents())
	}
	h.exec("DROP TRIGGER fail_always ON node_starts")
	if acks, err = h.publish("n1", serial, batch(start, 0, &fleetv1.Event{Seq: 3})); err != nil || !equalSeqs(ackSeqs(acks), []uint64{3}) {
		t.Fatalf("acks %v, %v", ackSeqs(acks), err)
	}
	if got := h.s.UnknownEvents(); got != 3 {
		t.Fatalf("%d unknown events counted; want 3", got)
	}
}

func TestPublishEventsStopping(t *testing.T) {
	h := newHarness(t)
	serial := h.joined("n1")
	ch, unsubscribe := h.d.Subscribe(db.ChanNode)
	defer unsubscribe()
	start := uuid.New()
	_, err := h.publish("n1", serial, batch(start, 0,
		&fleetv1.Event{Seq: 1, Body: &fleetv1.Event_Node{Node: &fleetv1.NodeEvent{Kind: "stopping", Message: "upgrade"}}}))
	if err != nil {
		t.Fatal(err)
	}
	expectPayload(t, ch, "n1")
	var left time.Duration
	var until time.Time
	h.scalar(&until, "SELECT stopping_until FROM nodes WHERE name = 'n1'")
	var now time.Time
	h.scalar(&now, "SELECT now()")
	if left = until.Sub(now); left < 25*time.Second || left > 31*time.Second {
		t.Fatalf("stopping_until %v ahead; want the rollout grace of 30 s", left)
	}
	if n := h.count("SELECT count(*) FROM activity WHERE kind = 'node_stopping' AND name = 'feosd stopping: upgrade' AND sandbox_id IS NULL"); n != 1 {
		t.Fatalf("%d node_stopping rows", n)
	}
	// The same event again (a resend) sets nothing.
	h.exec("UPDATE nodes SET stopping_until = NULL WHERE name = 'n1'")
	if _, err := h.publish("n1", serial, batch(start, 0,
		&fleetv1.Event{Seq: 1, Body: &fleetv1.Event_Node{Node: &fleetv1.NodeEvent{Kind: "stopping"}}})); err != nil {
		t.Fatal(err)
	}
	if n := h.node("n1"); !n.StoppingUntil.IsZero() {
		t.Fatal("a duplicate stopping event set the rollout deadline")
	}
}

// A pod's requests through its HTTP bindings are no activity: their http
// records are counted per pod, binding and minute, the newest kept as
// evidence, once each when a batch comes again; their http_end, a foreign
// pod's and the pod's own mistakes count for nothing; the other records of
// feos-authn stay activity.
func TestPublishEventsEgress(t *testing.T) {
	h := newHarness(t)
	serial := h.joined("n1")
	if _, _, err := h.st.CreateTenant(bg, "acme"); err != nil {
		t.Fatal(err)
	}
	pod := podid.Mint()
	if _, _, err := h.st.CreateSandbox(bg, store.NewSandbox{ID: pod, Owner: "acme", Image: "alpine", VCPUs: 1,
		MemoryMiB: 512}); err != nil {
		t.Fatal(err)
	}
	h.exec("UPDATE sandboxes SET node = 'n1', client_state = 'running' WHERE id = $1", pod)
	start := uuid.New()
	audit := func(seq uint64, podID, kind, detail string) *fleetv1.Event {
		return &fleetv1.Event{Seq: seq, Body: &fleetv1.Event_Audit{Audit: &fleetv1.AuditRecord{PodId: podID, Kind: kind,
			DetailJson: detail}}}
	}
	http := func(status int, errJSON string) string {
		return fmt.Sprintf(`{"type":"http","binding":"erp","method":"GET","path":"/odata","status":%d,"error":%s}`,
			status, errJSON)
	}
	var events []*fleetv1.Event
	seq := uint64(0)
	add := func(podID, kind, detail string) {
		seq++
		events = append(events, audit(seq, podID, kind, detail))
	}
	for range 12 {
		add(pod.String(), "http_request", http(200, "null"))
	}
	add(pod.String(), "http_request", http(401, "null"))
	add(pod.String(), "http_request", http(503, "null"))
	add(pod.String(), "http", http(502, `{"code":"unreachable","message":"x"}`))
	add(pod.String(), "http_request", http(503, `{"code":"credential_unavailable","message":"x"}`))
	add(pod.String(), "http_request", http(403, `{"code":"denied_path","message":"x"}`))
	add(pod.String(), "http_end", `{"type":"http_end","binding":"erp","status":200}`)
	add(pod.String(), "http_request", `{"type":"http","binding":null,"status":404,"error":{"code":"unknown_binding"}}`)
	add(podid.Mint().String(), "http_request", http(200, "null"))
	add(pod.String(), "token", `{"type":"token","binding":"erp"}`)
	b := batch(start, 0, events...)
	acks, err := h.publish("n1", serial, b, b)
	if err != nil || len(acks) != 2 {
		t.Fatalf("acks %v, %v", ackSeqs(acks), err)
	}
	if n := h.count("SELECT count(*) FROM activity WHERE kind IN ('http_request', 'http', 'http_end')"); n != 0 {
		t.Fatalf("%d activity rows of requests", n)
	}
	if n := h.count("SELECT count(*) FROM activity WHERE kind = 'token' AND sandbox_id = $1", pod); n != 1 {
		t.Fatalf("token records: %d", n)
	}
	var requests, refused, failing, tokenFailed int64
	if err := h.d.Pool.QueryRow(bg, `SELECT sum(requests), sum(refused), sum(failing), sum(token_failed)
		FROM http_egress WHERE sandbox_id = $1 AND binding = 'erp'`, pod).Scan(&requests, &refused, &failing,
		&tokenFailed); err != nil {
		t.Fatal(err)
	}
	if requests != 16 || refused != 1 || failing != 2 || tokenFailed != 1 {
		t.Fatalf("counts %d %d %d %d", requests, refused, failing, tokenFailed)
	}
	if n := h.count("SELECT count(*) FROM http_egress"); n != 1 {
		t.Fatalf("%d count rows", n)
	}
	if n := h.count("SELECT count(*) FROM http_egress_records WHERE sandbox_id = $1", pod); n != store.EgressKeep {
		t.Fatalf("%d records kept", n)
	}
	var newest string
	h.scalar(&newest, "SELECT detail->'error'->>'code' FROM http_egress_records ORDER BY id DESC LIMIT 1")
	if newest != "credential_unavailable" {
		t.Fatalf("the newest record: %s", newest)
	}
	// Swept by the database's clock.
	h.exec("UPDATE http_egress SET minute = minute - interval '11 minutes'")
	h.exec("UPDATE http_egress_records SET arrived_at = arrived_at - interval '11 minutes'")
	if err := h.st.SweepEgress(bg, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := h.count("SELECT (SELECT count(*) FROM http_egress) + (SELECT count(*) FROM http_egress_records)"); n != 0 {
		t.Fatalf("%d rows after the sweep", n)
	}
}
