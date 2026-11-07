// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	commonv1 "github.com/gardenlinux/feos/go/gen/otlp/common/v1"
	tracev1 "github.com/gardenlinux/feos/go/gen/otlp/trace/v1"
	"github.com/gardenlinux/feos/go/internal/ingress/sinklog"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// trafficT0 is the start of the traffic tests' range, a whole minute.
var trafficT0 = time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)

// trafficRecords are the ingress's records of the traffic tests, of
// tenant acme's environment env1 unless said: web's in the range
// [t0, t0+5m) a request of each kind in its first minute (10 to 70 ms;
// three failed: 503, no status written, 502 to a client that went away)
// and two in its third, one before and one at its end; api's one in its
// fifth minute; and one failed request each of web of env2 and of
// another tenant's env9.
func trafficRecords() []wire.AccessLogEntry {
	var out []wire.AccessLogEntry
	add := func(tenant, env, service string, at time.Duration, ms int64, status int, outcome string) {
		n := len(out) + 1
		out = append(out, wire.AccessLogEntry{Time: trafficT0.Add(at), DurationNs: ms * int64(time.Millisecond), Env: env,
			Tenant: tenant, Service: service, Host: "app.apps.example", Method: "GET", Path: "/", Proto: "HTTP/1.1",
			Status: status, Outcome: outcome, Attempts: 1, TraceID: fmt.Sprintf("%032x", n), SpanID: fmt.Sprintf("%016x", n)})
	}
	for i, c := range []struct {
		status  int
		outcome string
	}{{200, "2xx"}, {503, "5xx"}, {0, "aborted"}, {0, "2xx"}, {502, "aborted"}, {404, "4xx"}, {101, "upgraded"}} {
		add("acme", "env1", "web", time.Duration(i)*time.Second, int64(10*(i+1)), c.status, c.outcome)
	}
	add("acme", "env1", "web", 2*time.Minute, 5, 200, "2xx")
	add("acme", "env1", "web", 2*time.Minute+30*time.Second, 15, 204, "2xx")
	add("acme", "env1", "web", -time.Nanosecond, 1, 500, "5xx")
	add("acme", "env1", "web", 5*time.Minute, 1, 500, "5xx")
	add("acme", "env1", "api", 4*time.Minute, 100, 200, "2xx")
	add("acme", "env2", "web", 0, 1, 503, "5xx")
	add("other", "env9", "web", 0, 1, 503, "5xx")
	return out
}

// trafficSpans are the records' spans as the ingress in-0 sends them and
// the sink stores them.
func trafficSpans(t *testing.T, recs []wire.AccessLogEntry) []traceSpan {
	t.Helper()
	req, skipped := sinklog.Spans("in-0", recs)
	if skipped != 0 {
		t.Fatalf("%d records skipped", skipped)
	}
	spans, _, err := parseTraceExport(req, ingestPeer{ingress: "in-0"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return spans
}

func ms(v float64) *float64 { v *= float64(time.Millisecond); return &v }

func TestTraffic(t *testing.T) {
	tr, idx, lg := newTestTraces(t, testConfig())
	ctx := context.Background()
	if err := tr.Ingest(ctx, trafficSpans(t, trafficRecords())); err != nil {
		t.Fatal(err)
	}
	// A pod's own span of web, not the ingress's, is not its traffic.
	if err := ingestSpans(t, tr, traceRequest(podAttrs("acme", "env1", "web"),
		testSpan(traceA, "000000000000000a", "", "GET /", trafficT0.UnixNano(), time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	q := TrafficQuery{Tenant: "acme", Environment: "env1", Services: []string{"web", "api", "idle"},
		SinceNs: trafficT0.UnixNano(), UntilNs: trafficT0.Add(5 * time.Minute).UnixNano(), Step: time.Minute}
	got, err := tr.Traffic(ctx, q)
	if err != nil || len(got) != 3 || got[0].Requests != 0 || len(got[0].Steps) != 0 {
		t.Fatalf("before the index's commit: %+v %v", got, err)
	}
	idx.sync(lg)
	got, err = tr.Traffic(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	t0 := trafficT0.UnixNano()
	want := []ServiceTraffic{
		{Service: "web", Requests: 9, Errors: 3, P50Ns: ms(30), P95Ns: ms(66), Steps: []TrafficStep{
			{TsNs: t0, Requests: 7, Errors: 3, P95Ns: ms(67)},
			{TsNs: t0 + int64(2*time.Minute), Requests: 2, Errors: 0, P95Ns: ms(14.5)}}},
		{Service: "api", Requests: 1, P50Ns: ms(100), P95Ns: ms(100), Steps: []TrafficStep{
			{TsNs: t0 + int64(4*time.Minute), Requests: 1, P95Ns: ms(100)}}},
		{Service: "idle", Steps: []TrafficStep{}},
	}
	// The percentiles to the nanosecond, past the interpolation's float.
	round := func(p *float64) {
		if p != nil {
			*p = math.Round(*p)
		}
	}
	for i := range got {
		round(got[i].P50Ns)
		round(got[i].P95Ns)
		for j := range got[i].Steps {
			round(got[i].Steps[j].P95Ns)
		}
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Errorf("traffic\n%s\nwant\n%s", gb, wb)
	}
	// A tenant reads its own spans only.
	q.Environment, q.Services = "env9", []string{"web"}
	if got, err := tr.Traffic(ctx, q); err != nil || got[0].Requests != 0 {
		t.Errorf("another tenant's environment: %+v %v", got, err)
	}
	q.Tenant = "other"
	if got, err := tr.Traffic(ctx, q); err != nil || got[0].Requests != 1 || got[0].Errors != 1 {
		t.Errorf("the other tenant's: %+v %v", got, err)
	}
}

// TestTrafficApp reads the traffic of the services' own server spans:
// web's in the range a request a second for five seconds (10 to 50 ms:
// one with an error status, one with 503 under the attribute before
// 1.21, one with a 404, which is the client's), two to its readiness
// check (by url.path and by http.target), a client span, and one request
// in the third minute; api's one in the fifth. The ingress's spans of
// the same services are not counted.
func TestTrafficApp(t *testing.T) {
	tr, idx, lg := newTestTraces(t, testConfig())
	ctx := context.Background()
	if err := tr.Ingest(ctx, trafficSpans(t, trafficRecords())); err != nil {
		t.Fatal(err)
	}
	n := 0
	span := func(at time.Duration, ms int64, kind tracev1.Span_SpanKind, status tracev1.Status_StatusCode, attrs ...*commonv1.KeyValue) *tracev1.Span {
		n++
		s := testSpan(fmt.Sprintf("%032x", 5000+n), fmt.Sprintf("%016x", 5000+n), "", "GET /x", trafficT0.Add(at).UnixNano(),
			time.Duration(ms)*time.Millisecond)
		s.Kind, s.Attributes, s.Status = kind, attrs, &tracev1.Status{Code: status}
		return s
	}
	code := func(k string, c int64) *commonv1.KeyValue {
		return kv(k, &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: c}})
	}
	server, client := tracev1.Span_SPAN_KIND_SERVER, tracev1.Span_SPAN_KIND_CLIENT
	unset, failed := tracev1.Status_STATUS_CODE_UNSET, tracev1.Status_STATUS_CODE_ERROR
	web := []*tracev1.Span{
		span(0, 10, server, unset, code("http.response.status_code", 200)),
		span(time.Second, 20, server, failed),
		span(2*time.Second, 30, server, unset, code("http.status_code", 503)),
		span(3*time.Second, 40, server, unset, code("http.response.status_code", 404)),
		span(4*time.Second, 1, server, unset, kv("url.path", str("/readyz"))),
		span(5*time.Second, 1, server, unset, kv("http.target", str("/readyz"))),
		span(6*time.Second, 5, client, failed),
		span(2*time.Minute, 5, server, unset),
	}
	if err := ingestSpans(t, tr, traceRequest(podAttrs("acme", "env1", "web"), web...)); err != nil {
		t.Fatal(err)
	}
	if err := ingestSpans(t, tr, traceRequest(podAttrs("acme", "env1", "api"), span(4*time.Minute, 100, server, unset))); err != nil {
		t.Fatal(err)
	}
	idx.sync(lg)
	q := TrafficQuery{Tenant: "acme", Environment: "env1", Services: []string{"web", "api", "idle"}, Source: TrafficApp,
		Skip: map[string][]string{"web": {"/readyz"}}, SinceNs: trafficT0.UnixNano(), UntilNs: trafficT0.Add(5 * time.Minute).UnixNano(),
		Step: time.Minute}
	got, err := tr.Traffic(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	t0 := trafficT0.UnixNano()
	type step struct{ ts, req, errs int64 }
	steps := func(st ServiceTraffic) []step {
		var out []step
		for _, s := range st.Steps {
			out = append(out, step{s.TsNs, s.Requests, s.Errors})
		}
		return out
	}
	if got[0].Service != "web" || got[0].Requests != 5 || got[0].Errors != 2 || got[0].P50Ns == nil ||
		!slices.Equal(steps(got[0]), []step{{t0, 4, 2}, {t0 + int64(2*time.Minute), 1, 0}}) {
		t.Errorf("web: %+v", got[0])
	}
	if got[1].Service != "api" || got[1].Requests != 1 || got[1].Errors != 0 || !slices.Equal(steps(got[1]), []step{{t0 + int64(4*time.Minute), 1, 0}}) {
		t.Errorf("api: %+v", got[1])
	}
	if got[2].Service != "idle" || got[2].Requests != 0 || len(got[2].Steps) != 0 {
		t.Errorf("idle: %+v", got[2])
	}
	// Without the skip, the readiness checks count.
	q.Skip = nil
	if got, err := tr.Traffic(ctx, q); err != nil || got[0].Requests != 7 {
		t.Errorf("without skip: %+v %v", got, err)
	}
	// The ingress's source counts its own spans, not the services'.
	q.Source = TrafficIngress
	if got, err := tr.Traffic(ctx, q); err != nil || got[0].Requests != 9 || got[1].Requests != 1 {
		t.Errorf("the ingress's: %+v %v", got, err)
	}
}

func TestParseTrafficQuery(t *testing.T) {
	ok := url.Values{"environment": {"e"}, "service": {"web", "api", "web"}, "since": {"2026-11-02T01:00:00Z"},
		"until": {"1793584800000000000"}, "step": {"60s"}}
	q, err := parseTrafficQuery(ok, "acme")
	if err != nil || q.Tenant != "acme" || q.Environment != "e" || strings.Join(q.Services, ",") != "web,api" ||
		q.SinceNs != 1793581200000000000 || q.UntilNs != 1793584800000000000 || q.Step != time.Minute ||
		q.Source != TrafficIngress || q.Skip != nil {
		t.Fatalf("%+v %v", q, err)
	}
	app := url.Values{"source": {"app"}, "skip": {"web:/readyz", "web:/readyz", "api:/healthz"}}
	for k, x := range ok {
		app[k] = x
	}
	if q, err := parseTrafficQuery(app, "acme"); err != nil || q.Source != TrafficApp ||
		strings.Join(q.Skip["web"], ",") != "/readyz" || strings.Join(q.Skip["api"], ",") != "/healthz" {
		t.Fatalf("app: %+v %v", q, err)
	}
	many := make([]string, maxTrafficServices+1)
	for i := range many {
		many[i] = fmt.Sprint("s", i)
	}
	for _, c := range []struct {
		set  map[string][]string
		want string
	}{
		{map[string][]string{"environment": nil}, "environment is required"},
		{map[string][]string{"service": nil}, "give 1 to 128 services"},
		{map[string][]string{"service": many}, "give 1 to 128 services"},
		{map[string][]string{"service": {""}}, "a service must not be empty"},
		{map[string][]string{"step": nil}, "step must be"},
		{map[string][]string{"step": {"1500ms"}}, "step must be"},
		{map[string][]string{"step": {"0s"}}, "step must be"},
		{map[string][]string{"since": nil}, "since is required"},
		{map[string][]string{"until": {"x"}}, "until must be RFC 3339 or Unix nanoseconds"},
		{map[string][]string{"since": {"2026-11-02T01:00:30Z"}}, "since must be after the epoch and a multiple of step"},
		{map[string][]string{"since": {"1793584800000000000"}}, "since must be before until"},
		{map[string][]string{"since": {"2026-10-31T01:00:00Z"}}, "at most 1440 steps"},
		{map[string][]string{"source": {"egress"}}, "source must be ingress or app"},
		{map[string][]string{"skip": {"web:/readyz"}}, "skip is of the source app only"},
		{map[string][]string{"source": {"app"}, "skip": {"web/readyz"}}, "a skip must be <service>:<path>"},
		{map[string][]string{"source": {"app"}, "skip": {":/readyz"}}, "a skip must be <service>:<path>"},
		{map[string][]string{"source": {"app"}, "skip": {"web:readyz"}}, "a skip must be <service>:<path>"},
	} {
		v := url.Values{}
		for k, x := range ok {
			v[k] = x
		}
		for k, x := range c.set {
			if x == nil {
				delete(v, k)
			} else {
				v[k] = x
			}
		}
		if _, err := parseTrafficQuery(v, "acme"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.set, err, c.want)
		}
	}
}

// TestTrafficQuickwit runs against the Quickwit at
// FERUN_TEST_QUICKWIT_URL: of the ingress's spans in a trace index (made
// without a source, filled through the ingest API), a histogram answers
// what the tests' index in memory answers: the same counts, and
// percentiles between the closest ranks, within Quickwit's estimate.
func TestTrafficQuickwit(t *testing.T) {
	u := os.Getenv("FERUN_TEST_QUICKWIT_URL")
	if u == "" {
		t.Skip("FERUN_TEST_QUICKWIT_URL is not set; skipping a test that needs Quickwit")
	}
	c, err := quickwit.New(u, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	prefix := fmt.Sprintf("test-traffic-%d", time.Now().UnixNano())
	id := quickwit.TraceIndexID(prefix, 1)
	cfg := quickwit.TraceIndexConfig(id, quickwit.IndexSettings{RetentionDays: 1, CommitSecs: 1, Partitions: 4})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u+"/api/v1/indexes", strings.NewReader(string(cfg)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create the index: %s", resp.Status)
	}
	defer func() {
		req, _ := http.NewRequest(http.MethodDelete, u+"/api/v1/indexes/"+id, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()

	recs := trafficRecords()
	// More of web, so that a step's percentiles have ranks to choose from.
	for i := range 200 {
		r := recs[0]
		r.Time = trafficT0.Add(3*time.Minute + time.Duration(i)*time.Millisecond)
		r.DurationNs = int64(i%37+1) * int64(time.Millisecond)
		r.TraceID, r.SpanID = fmt.Sprintf("%032x", 1000+i), fmt.Sprintf("%016x", 1000+i)
		if i%10 == 0 {
			r.Status, r.Outcome = 500, "5xx"
		}
		recs = append(recs, r)
	}
	spans := trafficSpans(t, recs)
	mem := newMemTraceIndex()
	raws := make([]json.RawMessage, 0, len(spans))
	for _, s := range spans {
		mem.docs[id] = append(mem.docs[id], s.doc)
		raws = append(raws, s.raw)
	}
	if err := ingestSpansForTest(ctx, u, id, raws); err != nil {
		t.Fatal(err)
	}
	pattern := prefix + "-*"
	// within reports whether Quickwit's estimate of the p-th percentile of
	// the durations of q's spans in [lo, hi) lies between their closest
	// ranks, give or take its 1 %.
	within := func(q quickwit.SpanQuery, lo, hi int64, p float64, got *float64) bool {
		var ds []int64
		for i := range mem.docs[id] {
			d := &mem.docs[id][i]
			if q.Match(d) && d.TS >= lo && d.TS < hi {
				ds = append(ds, d.DurationNs)
			}
		}
		if len(ds) == 0 || got == nil {
			return len(ds) == 0 && got == nil
		}
		slices.Sort(ds)
		r := p * float64(len(ds)-1)
		return *got >= 0.98*float64(ds[int(math.Floor(r))]) && *got <= 1.02*float64(ds[int(math.Ceil(r))])
	}
	since, until := trafficT0.UnixNano(), trafficT0.Add(5*time.Minute).UnixNano()
	for _, svc := range []string{"web", "api", "idle"} {
		for _, failed := range []bool{false, true} {
			q := quickwit.SpanQuery{Tenant: "acme", Environment: "env1", Object: svc, Source: SourceIngress, SinceNs: since,
				UntilNs: until - 1, Failed: failed}
			got, err := c.SpanHistogram(ctx, pattern, q, time.Minute, true)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			want, _ := mem.SpanHistogram(ctx, pattern, q, time.Minute, true)
			name := fmt.Sprintf("%s failed=%v", svc, failed)
			if got.Count != want.Count || len(got.Buckets) != len(want.Buckets) {
				t.Errorf("%s: %d spans in %d steps, want %d in %d", name, got.Count, len(got.Buckets), want.Count, len(want.Buckets))
				continue
			}
			if !within(q, since, until, 0.50, got.P50Ns) || !within(q, since, until, 0.95, got.P95Ns) {
				t.Errorf("%s: p50 %v, p95 %v", name, got.P50Ns, got.P95Ns)
			}
			for i, b := range got.Buckets {
				w := want.Buckets[i]
				if b.StartNs != w.StartNs || b.Count != w.Count || !within(q, b.StartNs, b.StartNs+int64(time.Minute), 0.95, b.P95Ns) {
					t.Errorf("%s: step %d: %+v, want %+v", name, i, b, w)
				}
			}
		}
	}
}
