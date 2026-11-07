// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package quickwit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A failed request is one of a status of 500 or more, or of none written
// unless the client went away, as the ingress records them: its status
// as http.response.status_code (left out when 0), its class as
// ferun.outcome.
func TestFailed(t *testing.T) {
	for _, c := range []struct {
		status, outcome string
		want            bool
	}{
		{"200", `"2xx"`, false},
		{"503", `"5xx"`, true},
		{"999", `"9xx"`, true},
		{"", `"aborted"`, false},
		{"", `"2xx"`, true},
		{"502", `"aborted"`, true},
		{"200", `"aborted"`, false},
		{"404", `"4xx"`, false},
		{"101", `"upgraded"`, false},
		{"", "", false},
	} {
		d := SpanDoc{SpanAttributes: map[string]json.RawMessage{}}
		if c.status != "" {
			d.SpanAttributes["http.response.status_code"] = json.RawMessage(c.status)
		}
		if c.outcome != "" {
			d.SpanAttributes["ferun.outcome"] = json.RawMessage(c.outcome)
		}
		if got := Failed(&d); got != c.want {
			t.Errorf("status %s, outcome %s: %v", c.status, c.outcome, got)
		}
		q := SpanQuery{Failed: true}
		if q.Match(&d) != c.want {
			t.Errorf("status %s, outcome %s: Match disagrees", c.status, c.outcome)
		}
	}
	got := SpanQuery{Object: "web", Failed: true}.String()
	want := `object:"web" AND (span_attributes.http.response.status_code:IN [500 501 `
	if !strings.HasPrefix(got, want) || !strings.Contains(got, ` 998 999] OR (span_attributes.ferun.outcome:"2xx" AND `+
		`NOT span_attributes.http.response.status_code:IN [200 201 `) || !strings.HasSuffix(got, " 298 299]))") {
		t.Errorf("query %s", got)
	}
}

// ServerFailed tells a failed request from an application's server span:
// its status an error, or a status of 500 to 599 written under the
// stable attribute or the one before it; a client's error (4xx) is not.
func TestServerFailed(t *testing.T) {
	for _, c := range []struct {
		status uint64
		attrs  map[string]string
		want   bool
	}{
		{0, nil, false},
		{1, map[string]string{"http.response.status_code": "200"}, false},
		{2, nil, true},
		{0, map[string]string{"http.response.status_code": "503"}, true},
		{0, map[string]string{"http.status_code": "500"}, true},
		{0, map[string]string{"http.response.status_code": "404"}, false},
		{0, map[string]string{"http.status_code": "600"}, false},
	} {
		d := SpanDoc{StatusCode: c.status, SpanAttributes: map[string]json.RawMessage{}}
		for k, v := range c.attrs {
			d.SpanAttributes[k] = json.RawMessage(v)
		}
		if got := ServerFailed(&d); got != c.want {
			t.Errorf("status %d %v: %v", c.status, c.attrs, got)
		}
		if (&SpanQuery{ServerFailed: true}).Match(&d) != c.want {
			t.Errorf("status %d %v: Match disagrees", c.status, c.attrs)
		}
	}
	got := SpanQuery{Object: "web", Kind: SpanKindServer, ServerFailed: true, NotPaths: []string{"/readyz"}}.String()
	for _, part := range []string{`object:"web" AND span_kind:2 AND (status_code:2 OR span_attributes.http.response.status_code:IN [500 501 `,
		` 598 599] OR span_attributes.http.status_code:IN [500 `,
		` 599]) AND NOT span_attributes.url.path:"/readyz" AND NOT span_attributes.http.target:"/readyz"`} {
		if !strings.Contains(got, part) {
			t.Errorf("query %s\nlacks %s", got, part)
		}
	}
	// A span to a path left out, by either attribute, is not counted; of another kind neither.
	for _, c := range []struct {
		d    SpanDoc
		want bool
	}{
		{SpanDoc{SpanKind: 2, SpanAttributes: map[string]json.RawMessage{"url.path": json.RawMessage(`"/readyz"`)}}, false},
		{SpanDoc{SpanKind: 2, SpanAttributes: map[string]json.RawMessage{"http.target": json.RawMessage(`"/readyz"`)}}, false},
		{SpanDoc{SpanKind: 2, SpanAttributes: map[string]json.RawMessage{"url.path": json.RawMessage(`"/shows"`)}}, true},
		{SpanDoc{SpanKind: 3}, false},
	} {
		q := SpanQuery{Kind: SpanKindServer, NotPaths: []string{"/readyz"}}
		if got := q.Match(&c.d); got != c.want {
			t.Errorf("%+v: %v", c.d, got)
		}
	}
}

// SpanHistogram asks for a date histogram of ts in the step, and with
// durations for the percentiles overall and per step; it reads Quickwit
// 0.9's answer, of the steps those with spans.
func TestSpanHistogram(t *testing.T) {
	var asked map[string]any
	answer := `{"num_hits": 0, "hits": [], "errors": [], "aggregations": {
	  "durations": {"values": [{"key": 50.0, "value": 32950830.7}, {"key": 95.0, "value": 56544869.6}]},
	  "steps": {"buckets": [
	    {"key_as_string": "2026-11-02T00:18:00Z", "key": 1793578680000.0, "doc_count": 7, "p95": {"values": [{"key": 95.0, "value": 15104466.1}]}},
	    {"key_as_string": "2026-11-02T00:19:00Z", "key": 1793578740000.0, "doc_count": 0, "p95": {"values": [{"key": 95.0, "value": null}]}},
	    {"key_as_string": "2026-11-02T00:20:00Z", "key": 1793578800000.0, "doc_count": 2, "p95": {"values": [{"key": 95.0, "value": 9000000.0}]}}]}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/ferun-traces-*/search" || json.Unmarshal(b, &asked) != nil {
			http.Error(w, `{"message":"no"}`, http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	c, err := New(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC).UnixNano()
	q := SpanQuery{Tenant: "acme", Environment: "e", Object: "web", Source: "ingress", SinceNs: since,
		UntilNs: since + int64(time.Hour) - 1}
	h, err := c.SpanHistogram(context.Background(), "ferun-traces-*", q, time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(asked)
	if want := `{"aggs":{"durations":{"percentiles":{"field":"duration_ns","keyed":false,"percents":[50,95]}},` +
		`"steps":{"aggs":{"p95":{"percentiles":{"field":"duration_ns","keyed":false,"percents":[95]}}},` +
		`"date_histogram":{"field":"ts","fixed_interval":"60000ms","min_doc_count":1}}},"count_all":false,` +
		`"end_timestamp":1793581200,"format":"json","max_hits":0,"query":"tenant:\"acme\" AND environment:\"e\" AND ` +
		`object:\"web\" AND source:\"ingress\" AND ts:[2026-11-02T00:00:00.000000000Z TO 2026-11-02T00:59:59.999999999Z]",` +
		`"start_timestamp":1793577600}`; string(b) != want {
		t.Errorf("asked\n%s\nwant\n%s", b, want)
	}
	if h.Count != 9 || *h.P50Ns != 32950830.7 || *h.P95Ns != 56544869.6 || len(h.Buckets) != 2 ||
		h.Buckets[0].StartNs != 1793578680000000000 || h.Buckets[0].Count != 7 || *h.Buckets[0].P95Ns != 15104466.1 ||
		h.Buckets[1].StartNs != 1793578800000000000 || h.Buckets[1].Count != 2 {
		t.Errorf("histogram %+v", h)
	}

	// Without durations nothing but the steps is asked; no spans: no
	// percentiles.
	answer = `{"num_hits": 0, "hits": [], "errors": [], "aggregations": {"steps": {"buckets": []}}}`
	h, err = c.SpanHistogram(context.Background(), "ferun-traces-*", q, 15*time.Minute, false)
	if err != nil || h.Count != 0 || h.P50Ns != nil || len(h.Buckets) != 0 {
		t.Errorf("empty: %+v %v", h, err)
	}
	if aggs, _ := json.Marshal(asked["aggs"]); string(aggs) != `{"steps":{"date_histogram":{"field":"ts","fixed_interval":"900000ms","min_doc_count":1}}}` {
		t.Errorf("aggs %s", aggs)
	}
	answer = `{"num_hits": 0, "hits": [], "errors": ["split x failed"], "aggregations": {"steps": {"buckets": []}}}`
	if _, err := c.SpanHistogram(context.Background(), "ferun-traces-*", q, time.Minute, false); err == nil {
		t.Error("a split's error is none")
	}
	if _, err := c.SpanHistogram(context.Background(), "ferun-traces-*", q, time.Microsecond, false); err == nil {
		t.Error("a step under a millisecond")
	}
}
