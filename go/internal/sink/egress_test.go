// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	commonv1 "github.com/gardenlinux/feos/go/gen/otlp/common/v1"
	tracev1 "github.com/gardenlinux/feos/go/gen/otlp/trace/v1"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// egressT0 is the start of the egress tests' range.
var egressT0 = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

// egressPod are a pod's resource attributes on its egress spans, as its
// node stamps them: kind and object its labels', the source egress.
func egressPod(env, kind, object string) map[string]string {
	a := podAttrs("acme", env, object)
	a[attrKind], a[attrSource], a[attrContainer] = kind, SourceEgress, "feos-authn"
	return a
}

// egressSpan is the client span of the n-th request through binding (""
// for none), at egressT0+n s, answered status (0: none written), with
// feos-authn's code for a refusal of its own.
func egressSpan(n int, method, binding string, status int64, code string) *tracev1.Span {
	name := method
	if binding != "" {
		name += " " + binding
	}
	s := testSpan(fmt.Sprintf("%032x", 7000+n), fmt.Sprintf("%016x", 7000+n), "", name,
		egressT0.Add(time.Duration(n)*time.Second).UnixNano(), 20*time.Millisecond)
	s.Kind = tracev1.Span_SPAN_KIND_CLIENT
	s.Attributes = []*commonv1.KeyValue{kv("http.request.method", str(method)), kv("ferun.binding", str(binding))}
	if status != 0 {
		s.Attributes = append(s.Attributes, kv("http.response.status_code",
			&commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: status}}))
	}
	if code != "" {
		s.Attributes = append(s.Attributes, kv("ferun.error.code", str(code)))
	}
	if code != "" || status >= 500 {
		s.Status = &tracev1.Status{Code: tracev1.Status_STATUS_CODE_ERROR, Message: code}
	}
	return s
}

// egressRequests are the egress tests' requests of environment e1: api's
// through erp, two answered, one refused by the upstream (401), three
// failing (503 and 429 answered, one unreachable) and one denied a path
// by feos-authn (blocked, the newest problem); sandbox sbx1's through
// erp, answered, and through crm, without a token; job migrate's through
// crm, answered; one of api that no binding matched. Not counted: a
// request of api before the range and one of e2, a server span of api,
// a request of another project.
func egressRequests(t *testing.T, tr *Traces) {
	t.Helper()
	for _, r := range []struct {
		attrs map[string]string
		spans []*tracev1.Span
	}{
		{egressPod("e1", "service", "api"), []*tracev1.Span{
			egressSpan(1, "GET", "erp", 200, ""), egressSpan(2, "GET", "erp", 200, ""),
			egressSpan(3, "POST", "erp", 401, ""), egressSpan(4, "GET", "erp", 503, ""),
			egressSpan(5, "GET", "erp", 502, "unreachable"), egressSpan(6, "GET", "erp", 429, ""),
			egressSpan(7, "GET", "erp", 403, "denied_path"), egressSpan(20, "GET", "", 404, "unknown_binding"),
			egressSpan(-1, "GET", "erp", 401, "")}},
		{egressPod("e1", "sandbox", "sbx1"), []*tracev1.Span{
			egressSpan(8, "GET", "erp", 204, ""), egressSpan(9, "GET", "crm", 503, "credential_unavailable")}},
		{egressPod("e1", "job", "migrate"), []*tracev1.Span{egressSpan(10, "PUT", "crm", 200, "")}},
		{egressPod("e2", "service", "api"), []*tracev1.Span{egressSpan(11, "GET", "erp", 401, "")}},
		{podAttrs("acme", "e1", "api"), []*tracev1.Span{egressSpan(12, "GET", "erp", 401, "")}},
	} {
		if err := ingestSpans(t, tr, traceRequest(r.attrs, r.spans...)); err != nil {
			t.Fatal(err)
		}
	}
	other := egressPod("e1", "service", "api")
	other[attrProject] = "other"
	if err := ingestSpans(t, tr, traceRequest(other, egressSpan(13, "GET", "erp", 401, ""))); err != nil {
		t.Fatal(err)
	}
}

// egressWant is what the egress tests' requests count in [egressT0,
// egressT0+1m), without the messages, which the API adds.
func egressWant() Egress {
	at := func(n int) int64 { return egressT0.Add(time.Duration(n) * time.Second).UnixNano() }
	return Egress{Bindings: []BindingEgress{
		{Binding: "crm", Requests: 2, TokenFailed: 1, LastNs: at(10),
			LastProblem: &EgressProblem{TsNs: at(9), Class: quickwit.EgressTokenFailed, Status: 503,
				Code: "credential_unavailable", TraceID: fmt.Sprintf("%032x", 7009), SpanID: fmt.Sprintf("%016x", 7009)},
			Users: []EgressUser{{"job", "migrate", 1}, {"sandbox", "sbx1", 1}}},
		{Binding: "erp", Requests: 8, Refused: 1, Failing: 3, Blocked: 1, LastNs: at(8),
			LastProblem: &EgressProblem{TsNs: at(7), Class: quickwit.EgressBlocked, Status: 403, Code: "denied_path",
				TraceID: fmt.Sprintf("%032x", 7007), SpanID: fmt.Sprintf("%016x", 7007)},
			Users: []EgressUser{{"service", "api", 7}, {"sandbox", "sbx1", 1}}},
	}}
}

// The requests through each binding of the environment, from its pods'
// egress spans: every class counted, the users by kind and object, the
// newest problem; requests no binding matched, of another environment
// or project, outside the range or not egress are none.
func TestEgress(t *testing.T) {
	tr, idx, lg := newTestTraces(t, testConfig())
	ctx := context.Background()
	egressRequests(t, tr)
	q := EgressQuery{Project: "acme", Environment: "e1", SinceNs: egressT0.UnixNano(), UntilNs: egressT0.Add(time.Minute).UnixNano()}
	got, err := tr.Egress(ctx, q)
	if err != nil || len(got.Bindings) != 0 || got.Bindings == nil || got.Truncated {
		t.Fatalf("before the index's commit: %+v %v", got, err)
	}
	idx.sync(lg)
	got, err = tr.Egress(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if want := egressWant(); !reflect.DeepEqual(got, want) {
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		t.Errorf("egress\n%s\nwant\n%s", gb, wb)
	}
	// Another environment has its own.
	q.Environment = "e2"
	if got, err := tr.Egress(ctx, q); err != nil || len(got.Bindings) != 1 || got.Bindings[0].Refused != 1 ||
		got.Bindings[0].Requests != 1 {
		t.Errorf("e2: %+v %v", got, err)
	}
}

// A span's class is the query's: feos-authn's codes first, then the
// upstream's status; a code it does not know counts by the status.
func TestEgressClass(t *testing.T) {
	for _, c := range []struct {
		status int64
		code   string
		want   string
	}{
		{200, "", ""}, {404, "", ""}, {401, "", quickwit.EgressRefused}, {403, "", quickwit.EgressRefused},
		{429, "", quickwit.EgressFailing}, {500, "", quickwit.EgressFailing}, {0, "", ""},
		{502, "unreachable", quickwit.EgressFailing}, {502, "tls", quickwit.EgressFailing},
		{503, "credential_unavailable", quickwit.EgressTokenFailed}, {403, "denied_path", quickwit.EgressBlocked},
		{429, "rate_limited", quickwit.EgressBlocked}, {503, "not_ready", quickwit.EgressBlocked},
		{403, "denied_destination", quickwit.EgressBlocked}, {503, "a_new_one", quickwit.EgressFailing},
	} {
		req, _, err := parseTraceExport(traceRequest(egressPod("e1", "service", "api"), egressSpan(1, "GET", "erp", c.status,
			c.code)), ingestPeer{node: "node-1"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		d := &req[0].doc
		if got := quickwit.EgressClass(d); got != c.want {
			t.Errorf("%d %q: %q, want %q", c.status, c.code, got, c.want)
		}
		for _, class := range append([]string{quickwit.EgressProblem}, quickwit.EgressClasses...) {
			want := c.want != "" && (class == quickwit.EgressProblem || class == c.want)
			if q := (quickwit.SpanQuery{Egress: class}); q.Match(d) != want {
				t.Errorf("%d %q as %s: %v", c.status, c.code, class, !want)
			}
		}
	}
	if q := (quickwit.SpanQuery{Egress: "other"}); q.String() != "NOT *" || q.Match(&quickwit.SpanDoc{}) {
		t.Error("an unknown class selects something")
	}
}

func TestParseEgressQuery(t *testing.T) {
	until := egressT0.Add(time.Hour)
	ok := url.Values{"environment": {"e"}, "since": {"2026-11-19T10:00:00Z"}, "until": {fmt.Sprint(until.UnixNano())}}
	if q, err := parseEgressQuery(ok, "acme"); err != nil || q.Project != "acme" || q.Environment != "e" ||
		q.SinceNs != egressT0.UnixNano() || q.UntilNs != until.UnixNano() {
		t.Fatalf("%+v %v", q, err)
	}
	for _, c := range []struct {
		set  map[string][]string
		want string
	}{
		{map[string][]string{"environment": nil}, "environment is required"},
		{map[string][]string{"since": nil}, "since is required"},
		{map[string][]string{"until": {"x"}}, "until must be RFC 3339 or Unix nanoseconds"},
		{map[string][]string{"since": {"0"}}, "since must be after the epoch"},
		{map[string][]string{"since": {"2026-11-19T11:00:00Z"}}, "since must be before until"},
		{map[string][]string{"since": {"2026-11-01T10:00:00Z"}}, "the range is at most 168h0m0s"},
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
		if _, err := parseEgressQuery(v, "acme"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.set, err, c.want)
		}
	}
}

// GET /api/v1/egress answers a readable environment's bindings, a
// refusal of feos-authn's with the message of its egress record (found
// by its trace and span), 404 for another's, 400 for a bad range.
func TestEgressAPI(t *testing.T) {
	s, sidx, slg := newTestStore(t, testConfig())
	tr, tidx, tlg := newTestTraces(t, testConfig())
	ctx := context.Background()
	egressRequests(t, tr)
	tidx.sync(tlg)
	// The egress records of api's requests 6 and 7, the latter's with
	// its message; another span of the same trace says something else.
	rec := func(n int, span int, msg string) string {
		e := "null"
		if msg != "" {
			e = `{"code":"denied_path","message":"` + msg + `"}`
		}
		return fmt.Sprintf(`{"ts":"%s","type":"http","binding":"erp","method":"GET","status":403,"traceId":"%032x",`+
			`"spanId":"%016x","error":%s}`, egressT0.Add(time.Duration(n)*time.Second).Format(time.RFC3339Nano), 7000+n,
			7000+span, e)
	}
	texts := []string{rec(6, 6, ""), rec(7, 99, "another span"), rec(7, 7, "path /admin is not allowed by binding erp")}
	labels := envLabels("acme", "e1", 0)
	labels.Source = SourceEgress
	if err := s.Ingest(ctx, batch(StreamKey{"pe", "feos-authn", "l"}, labels,
		lines(0, egressT0.Add(6*time.Second).UnixNano(), len(texts), func(i int) string { return texts[i] }))); err != nil {
		t.Fatal(err)
	}
	sidx.sync(slg)

	ferund := httptest.NewServer(http.HandlerFunc(accessFerund))
	defer ferund.Close()
	ready := make(chan struct{})
	close(ready)
	srv := httptest.NewServer(newAPI(s, tr, NewProjects(ferund.URL, nil, time.Minute), ready, dbtest.Logger(t)))
	defer srv.Close()
	get := func(who string, v url.Values) (int, []byte) {
		t.Helper()
		r, _ := http.NewRequest("GET", srv.URL+"/api/v1/egress?"+v.Encode(), nil)
		r.Header.Set("Authorization", "Bearer "+who)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	v := url.Values{"environment": {"e1"}, "since": {egressT0.Format(time.RFC3339)},
		"until": {fmt.Sprint(egressT0.Add(time.Minute).UnixNano())}}
	code, b := get("e1", v)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, b)
	}
	var got Egress
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := egressWant()
	want.Bindings[1].LastProblem.Message = "path /admin is not allowed by binding erp"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("answer %s", b)
	}
	if !strings.Contains(string(b), `"lastNs":"`) || !strings.Contains(string(b), `"tsNs":"`) {
		t.Errorf("times not as strings: %s", b)
	}
	v.Set("environment", "e2")
	if code, b := get("e1", v); code != http.StatusNotFound {
		t.Errorf("another's environment: %d %s", code, b)
	}
	if code, _ := get("all", v); code != http.StatusOK {
		t.Errorf("the project's reader: %d", code)
	}
	v.Del("since")
	if code, b := get("all", v); code != http.StatusBadRequest || !strings.Contains(string(b), "since is required") {
		t.Errorf("no since: %d %s", code, b)
	}
}

// TestEgressQuickwit runs against the Quickwit at
// FERUN_TEST_QUICKWIT_URL: of the egress tests' spans in a trace index
// (made without a source, filled through the ingest API), the groupings
// and the newest spans answer what the tests' index in memory answers.
func TestEgressQuickwit(t *testing.T) {
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
	prefix := fmt.Sprintf("test-egress-%d", time.Now().UnixNano())
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

	// The spans of the egress tests, through a Traces over the memory
	// index, which keeps them; then into Quickwit as they are.
	tr, idx, lg := newTestTraces(t, testConfig())
	egressRequests(t, tr)
	idx.sync(lg)
	var raws []json.RawMessage
	mem := newMemTraceIndex()
	for _, docs := range idx.docs {
		for _, d := range docs {
			b, _ := json.Marshal(d)
			raws = append(raws, b)
			mem.docs[id] = append(mem.docs[id], d)
		}
	}
	if err := ingestSpansForTest(ctx, u, id, raws); err != nil {
		t.Fatal(err)
	}
	pattern := prefix + "-*"
	base := quickwit.SpanQuery{Project: "acme", Environment: "e1", Source: SourceEgress, SinceNs: egressT0.UnixNano(),
		UntilNs: egressT0.Add(time.Minute).UnixNano() - 1}
	queries := []quickwit.SpanQuery{base}
	for _, k := range egressKinds {
		q := base
		q.ObjectKind = k
		queries = append(queries, q)
	}
	for _, class := range append([]string{quickwit.EgressProblem}, quickwit.EgressClasses...) {
		q := base
		q.Egress = class
		queries = append(queries, q)
	}
	for _, q := range queries {
		for _, fields := range [][]string{{"span_name"}, {"span_name", "service_name"}} {
			got, more, err := c.SpanGroups(ctx, pattern, q, fields, 100)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			want, _, _ := mem.SpanGroups(ctx, pattern, q, fields, 100)
			// Quickwit answers a newest start to about a microsecond.
			for i := range got {
				got[i].LastNs = got[i].LastNs / 1000
			}
			for i := range want {
				want[i].LastNs = want[i].LastNs / 1000
			}
			if more || fmt.Sprint(sortedGroups(got)) != fmt.Sprint(sortedGroups(want)) {
				t.Errorf("%s by %v: %v (more %v), want %v", q, fields, got, more, want)
			}
		}
	}
	// The bounds: fewer values than there are leave some out.
	if got, more, err := c.SpanGroups(ctx, pattern, base, []string{"span_name"}, 1); err != nil || len(got) != 1 || !more {
		t.Errorf("one name: %v %v %v", got, more, err)
	}
	// The newest problem of erp.
	q := base
	q.Egress, q.Tags, q.Max = quickwit.EgressProblem, map[string]string{quickwit.AttrBinding: "erp"}, 1
	got, err := c.NewestSpans(ctx, pattern, q)
	if err != nil || len(got) != 1 || got[0].SpanID != fmt.Sprintf("%016x", 7007) {
		t.Errorf("the newest problem: %+v %v", got, err)
	}
}

// sortedGroups is groups in the order of their keys.
func sortedGroups(gs []quickwit.SpanGroup) []string {
	var out []string
	for _, g := range gs {
		out = append(out, fmt.Sprintf("%s=%d@%d", strings.Join(g.Keys, "/"), g.Count, g.LastNs))
	}
	slices.Sort(out)
	return out
}
