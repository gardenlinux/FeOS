// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	otlptracev1 "github.com/gardenlinux/feos/go/gen/otlp/collector/trace/v1"
	commonv1 "github.com/gardenlinux/feos/go/gen/otlp/common/v1"
	tracev1 "github.com/gardenlinux/feos/go/gen/otlp/trace/v1"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/sink/kafka"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// newTestTraces is the spans over a new database, the trace index in
// memory and the topics in memory; idx.sync(lg) is a commit of the
// index's source.
func newTestTraces(t *testing.T, cfg Config) (*Traces, *memTraceIndex, *memLog) {
	t.Helper()
	idx, lg := newMemTraceIndex(), newMemLog()
	tr := NewTraces(newTestDB(t), idx, lg, cfg, dbtest.Logger(t))
	if err := tr.EnsureIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	return tr, idx, lg
}

// ingestSpans parses a request of a node's and stores it.
func ingestSpans(t *testing.T, tr *Traces, req *otlptracev1.ExportTraceServiceRequest) error {
	t.Helper()
	spans, _, err := parseTraceExport(req, ingestPeer{node: "node-1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tr.Ingest(context.Background(), spans)
}

// topicSpans are the spans of the trace topic's records, keyed by trace
// ID in its partition, by project.
func topicSpans(t *testing.T, tr *Traces, lg *memLog) map[string]int {
	t.Helper()
	id, _ := tr.indexID()
	parts := int32(tr.cfg.KafkaPartitions)
	out := map[string]int{}
	for _, r := range lg.records(id) {
		var d quickwit.SpanDoc
		if err := json.Unmarshal(r.Value, &d); err != nil {
			t.Fatal(err)
		}
		if string(r.Key) != d.TraceID || r.Partition != kafka.Partition(d.TraceID, parts) {
			t.Errorf("span %s: key %q, partition %d", d.SpanID, r.Key, r.Partition)
		}
		out[d.Project]++
	}
	return out
}

func spanIDs(spans []quickwit.SpanDoc) string {
	s := ""
	for _, d := range spans {
		s += d.SpanID[12:] + " "
	}
	return s
}

func TestTraceStore(t *testing.T) {
	tr, idx, lg := newTestTraces(t, testConfig())
	ctx := context.Background()
	start := time.Now().Add(-10 * time.Minute).UnixNano()
	root := testSpan(traceA, "000000000000000a", "", "GET /", start, 20*time.Millisecond)
	root.Attributes = []*commonv1.KeyValue{kv("http.response.status_code", &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: 500}})}
	child := testSpan(traceA, "000000000000000b", "000000000000000a", "query", start+int64(time.Millisecond), 5*time.Millisecond)
	if err := ingestSpans(t, tr, traceRequest(podAttrs("acme", "env1", "api"), root, child)); err != nil {
		t.Fatal(err)
	}
	other := testSpan(traceB, "000000000000000c", "", "POST /x", start+int64(time.Minute), time.Second)
	if err := ingestSpans(t, tr, traceRequest(podAttrs("acme", "env2", "web"), other)); err != nil {
		t.Fatal(err)
	}
	notAcme := testSpan(traceA, "000000000000000d", "", "GET /", start, time.Millisecond)
	if err := ingestSpans(t, tr, traceRequest(podAttrs("other", "env1", "api"), notAcme)); err != nil {
		t.Fatal(err)
	}
	if id, _ := tr.indexID(); id != "ferun-traces-v4-30d" || fmt.Sprint(topicSpans(t, tr, lg)) != "map[acme:3 other:1]" {
		t.Fatalf("topic %s: %v", id, topicSpans(t, tr, lg))
	}
	var used int64
	if err := tr.db.Pool.QueryRow(ctx, `SELECT bytes FROM trace_usage WHERE project = 'acme'`).Scan(&used); err != nil || used < 300 {
		t.Errorf("usage %d, %v", used, err)
	}

	// reads answers the same before and after the index took the spans.
	reads := func(when string) {
		t.Helper()
		spans, err := tr.Trace(ctx, quickwit.SpanQuery{Project: "acme", TraceID: traceA})
		if err != nil || spanIDs(spans) != "000a 000b " {
			t.Errorf("%s: trace A: %s %v", when, spanIDs(spans), err)
		}
		if spans, err := tr.Trace(ctx, quickwit.SpanQuery{Project: "acme", TraceID: traceA, Environment: "env2"}); err != nil || spans != nil {
			t.Errorf("%s: trace A of env2: %s %v", when, spanIDs(spans), err)
		}
		since := start - int64(time.Hour)
		for _, c := range []struct {
			q    quickwit.SpanQuery
			want string
		}{
			{quickwit.SpanQuery{}, traceB + " " + traceA},
			{quickwit.SpanQuery{Service: "api"}, traceA},
			{quickwit.SpanQuery{Operation: "query"}, traceA},
			{quickwit.SpanQuery{Tags: map[string]string{"http.response.status_code": "500"}}, traceA},
			{quickwit.SpanQuery{Tags: map[string]string{"ferun.environment": "env2"}}, traceB},
			{quickwit.SpanQuery{MinDurationNs: int64(500 * time.Millisecond)}, traceB},
			{quickwit.SpanQuery{MaxDurationNs: int64(10 * time.Millisecond)}, traceA},
			{quickwit.SpanQuery{Environment: "env1"}, traceA},
			{quickwit.SpanQuery{Source: SourceIngress}, ""},
			{quickwit.SpanQuery{UntilNs: start + int64(time.Second)}, traceA},
		} {
			q := c.q
			q.Project, q.SinceNs = "acme", since
			if q.UntilNs == 0 {
				q.UntilNs = time.Now().UnixNano()
			}
			found, err := tr.Find(ctx, q, 20)
			got := ""
			for i, spans := range found {
				if i > 0 {
					got += " "
				}
				got += spans[0].TraceID
			}
			if err != nil || got != c.want {
				t.Errorf("%s: find %+v: %q %v, want %q", when, c.q, got, err, c.want)
			}
		}
		if found, err := tr.Find(ctx, quickwit.SpanQuery{Project: "acme", SinceNs: since, UntilNs: time.Now().UnixNano()}, 1); err != nil ||
			len(found) != 1 || found[0][0].TraceID != traceB {
			t.Errorf("%s: find 1: %v", when, err)
		}
		// A trace found by one span answers its spans of the selectors.
		if found, err := tr.Find(ctx, quickwit.SpanQuery{Project: "acme", Operation: "query", SinceNs: since}, 20); err != nil ||
			len(found) != 1 || spanIDs(found[0]) != "000a 000b " {
			t.Errorf("%s: whole trace: %v", when, err)
		}
		svcs, err := tr.Services(ctx, quickwit.SpanQuery{Project: "acme"}, since)
		if err != nil || fmt.Sprint(svcs) != "[api web]" {
			t.Errorf("%s: services %v %v", when, svcs, err)
		}
		ops, err := tr.Operations(ctx, quickwit.SpanQuery{Project: "acme", Service: "api"}, since)
		if err != nil || fmt.Sprint(ops) != "[GET / query]" {
			t.Errorf("%s: operations %v %v", when, ops, err)
		}
	}
	// Before the index's commit, a trace is read by its ID from the
	// topic's tail; a search reads the index alone and finds none.
	if spans, err := tr.Trace(ctx, quickwit.SpanQuery{Project: "acme", TraceID: traceA}); err != nil || spanIDs(spans) != "000a 000b " {
		t.Errorf("tail: trace A: %s %v", spanIDs(spans), err)
	}
	if spans, err := tr.Trace(ctx, quickwit.SpanQuery{Project: "acme", TraceID: traceA, Environment: "env2"}); err != nil || spans != nil {
		t.Errorf("tail: trace A of env2: %s %v", spanIDs(spans), err)
	}
	if found, err := tr.Find(ctx, quickwit.SpanQuery{Project: "acme", SinceNs: start - int64(time.Hour), UntilNs: time.Now().UnixNano()}, 20); err != nil ||
		len(found) != 0 {
		t.Errorf("tail: find %d, %v", len(found), err)
	}
	if svcs, err := tr.Services(ctx, quickwit.SpanQuery{Project: "acme"}, start-int64(time.Hour)); err != nil || len(svcs) != 0 {
		t.Errorf("tail: services %v %v", svcs, err)
	}
	idx.sync(lg)
	if idx.count() != 4 {
		t.Fatalf("%d indexed", idx.count())
	}
	reads("index")
	// A span sent again is in the index and in the tail: read once.
	if err := ingestSpans(t, tr, traceRequest(podAttrs("acme", "env1", "api"), child)); err != nil {
		t.Fatal(err)
	}
	reads("twice")
	idx.sync(lg)
	if idx.count() != 5 {
		t.Fatalf("%d indexed", idx.count())
	}
	reads("indexed twice")

	// The usage of the days before yesterday goes.
	if _, err := tr.db.Pool.Exec(ctx, `INSERT INTO trace_usage VALUES ('acme', (now() AT TIME ZONE 'utc')::date - 2, 5)`); err != nil {
		t.Fatal(err)
	}
	if err := tr.sweepUsage(ctx); err != nil {
		t.Fatal(err)
	}
	var days int
	if err := tr.db.Pool.QueryRow(ctx, `SELECT count(*) FROM trace_usage WHERE project = 'acme'`).Scan(&days); err != nil || days != 1 {
		t.Errorf("usage days %d, %v", days, err)
	}
}

func TestTraceQuota(t *testing.T) {
	cfg := testConfig()
	cfg.TraceQuota = 100
	tr, _, lg := newTestTraces(t, cfg)
	start := time.Now().UnixNano()
	sp := func(id string) *tracev1.Span { return testSpan(traceA, id, "", "x", start, time.Millisecond) }
	// The request that crosses the quota is stored; the next is refused.
	if err := ingestSpans(t, tr, traceRequest(podAttrs("acme", "e", "api"), sp("0000000000000001"))); err != nil {
		t.Fatal(err)
	}
	if err := ingestSpans(t, tr, traceRequest(podAttrs("acme", "e", "api"), sp("0000000000000002"))); !errors.Is(err, ErrTraceQuota) {
		t.Errorf("over the quota: %v", err)
	}
	if err := ingestSpans(t, tr, traceRequest(podAttrs("other", "e", "api"), sp("0000000000000003"))); err != nil {
		t.Errorf("another project: %v", err)
	}
	if got := fmt.Sprint(topicSpans(t, tr, lg)); got != "map[acme:1 other:1]" {
		t.Errorf("spans stored %s", got)
	}
}

// A produce that fails stores nothing, not even the usage; the sender
// sends again.
func TestTraceProduceFailure(t *testing.T) {
	tr, _, lg := newTestTraces(t, testConfig())
	ctx := context.Background()
	req := traceRequest(podAttrs("acme", "e", "api"), testSpan(traceA, "0000000000000001", "", "x", time.Now().UnixNano(), time.Millisecond))
	lg.failProduce = errors.New("down")
	if err := ingestSpans(t, tr, req); err == nil {
		t.Fatal("no error")
	}
	var days int
	if err := tr.db.Pool.QueryRow(ctx, `SELECT count(*) FROM trace_usage`).Scan(&days); err != nil || days != 0 {
		t.Errorf("usage of %d days: %v", days, err)
	}
	lg.failProduce = nil
	if err := ingestSpans(t, tr, req); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(topicSpans(t, tr, lg)); got != "map[acme:1]" {
		t.Errorf("spans stored %s", got)
	}
	if spans, err := tr.Trace(ctx, quickwit.SpanQuery{Project: "acme", TraceID: traceA}); err != nil || len(spans) != 1 {
		t.Errorf("trace: %d spans, %v", len(spans), err)
	}
}
