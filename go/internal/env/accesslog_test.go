// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/ingress/sinklog"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// fakeAccessSink is the sink's GET /api/v1/logs over an environment's
// ingress lines, oldest first: until, trace, limit and cursor as the
// sink takes them, at most two lines a page.
type fakeAccessSink struct {
	t     *testing.T
	env   string
	mu    sync.Mutex
	lines []sinklog.Line
	down  bool
	asked int
}

// passedOn reports whether a request to the sink brings the caller's
// credentials: the token "tok", or the session "sess" with its tenant.
func passedOn(r *http.Request) bool {
	if a := r.Header.Get("Authorization"); a != "" {
		return a == "Bearer tok" && r.Header.Get(auth.TenantHeader) == ""
	}
	c, err := r.Cookie(auth.SessionCookie)
	return err == nil && c.Value == "sess" && r.Header.Get(auth.TenantHeader) == tenantA
}

func (f *fakeAccessSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	q := r.URL.Query()
	if f.down {
		http.Error(w, `{"error":"down"}`, http.StatusServiceUnavailable)
		return
	}
	if r.URL.Path != "/api/v1/logs" || !passedOn(r) || q.Get("environment") != f.env ||
		q.Get("source") != "ingress" || q.Get("direction") != "backward" || q.Get("since") != "0" {
		f.t.Errorf("the sink was asked %s %v", r.URL, r.Header)
		http.Error(w, "no", http.StatusBadRequest)
		return
	}
	until := int64(1 << 62)
	if u := q.Get("until"); u != "" {
		until, _ = strconv.ParseInt(u, 10, 64)
	}
	var sel []sinklog.Line
	for _, l := range f.lines {
		if l.TsNs <= until && (q.Get("trace") == "" || l.TraceID == q.Get("trace")) {
			sel = append(sel, l)
		}
	}
	end := len(sel)
	if c := q.Get("cursor"); c != "" {
		end, _ = strconv.Atoi(c)
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	start := max(end-min(limit, 2), 0)
	type line struct {
		TS   int64  `json:"ts,string"`
		Text string `json:"text"`
	}
	out := struct {
		Lines  []line `json:"lines"`
		Cursor string `json:"cursor"`
	}{Lines: []line{}}
	for _, l := range sel[start:end] {
		b, _ := json.Marshal(l)
		out.Lines = append(out.Lines, line{TS: l.TsNs, Text: string(b)})
	}
	if start > 0 {
		out.Cursor = strconv.Itoa(start)
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeAccessSink) setDown(down bool) {
	f.mu.Lock()
	f.down = down
	f.mu.Unlock()
}

// The access log is read from the sink with the caller's token, page by
// page; without --sink-url or a token, or with the sink down, it answers
// 503 with a code of its own.
func TestAccessLogsFromSink(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.create("stage", nil)
	e := h.env("stage")
	const trace = "4bf92f3577b34da6a3ce929d0e0e4736"
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	sink := &fakeAccessSink{t: t, env: e.ID.String()}
	for i := range 5 {
		rec := wire.AccessLogEntry{Time: t0.Add(time.Duration(10+i) * time.Second), DurationNs: 1500, Env: e.ID.String(),
			Tenant: tenantA, Service: "web", Instance: "i1", Host: "app.apps.example", Method: "GET",
			Path: "/new" + strconv.Itoa(i), Proto: "HTTP/2.0", Status: 503, Outcome: "5xx", BytesIn: 3, BytesOut: 12,
			ClientIP: "203.0.113.9", UserAgent: "ua", Attempts: 2, HeldNs: 7, SpanID: "00f067aa0ba902b7"}
		if i == 2 {
			rec.TraceID = trace
		}
		sink.lines = append(sink.lines, sinklog.NewLine("in-1", rec))
	}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	ctx := auth.WithTenant(h.ctx(), auth.Tenant{Name: tenantA, Token: "tok"})

	// refused checks that the read answers 503 with code.
	refused := func(name string, ctx context.Context, code string) {
		t.Helper()
		_, err := h.svc.AccessLogs(ctx, tenantA, "stage", AccessLogQuery{})
		var ae *apierr.Error
		if !errors.As(err, &ae) || ae.Status != http.StatusServiceUnavailable || ae.Code != code {
			t.Errorf("%s: %v, want 503 %s", name, err, code)
		}
	}
	refused("no --sink-url", ctx, CodeNoLogStore)
	h.svc.d.SinkURL = srv.URL + "/"
	refused("no token", h.ctx(), CodeNoLogStore)

	paths := func(ctx context.Context, q AccessLogQuery) []string {
		t.Helper()
		l, err := h.svc.AccessLogs(ctx, tenantA, "stage", q)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, r := range l.Records {
			out = append(out, r.Path)
		}
		return out
	}
	for _, c := range []struct {
		name string
		q    AccessLogQuery
		want []string
	}{
		{"the newest", AccessLogQuery{Limit: 3}, []string{"/new2", "/new3", "/new4"}},
		{"all, over pages", AccessLogQuery{}, []string{"/new0", "/new1", "/new2", "/new3", "/new4"}},
		{"before", AccessLogQuery{Limit: 3, BeforeTsNs: t0.Add(13 * time.Second).UnixNano()},
			[]string{"/new0", "/new1", "/new2"}},
		{"before the oldest", AccessLogQuery{BeforeTsNs: t0.Add(10 * time.Second).UnixNano()}, []string{}},
		{"a trace", AccessLogQuery{TraceID: trace}, []string{"/new2"}},
	} {
		if got := paths(ctx, c.q); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}

	// A person's session is passed on as the token is (ADR 0031).
	sess := auth.WithTenant(h.ctx(), auth.Tenant{Name: tenantA, Session: "sess"})
	if got := paths(sess, AccessLogQuery{Limit: 2}); !slices.Equal(got, []string{"/new3", "/new4"}) {
		t.Errorf("a session: %v", got)
	}

	// A record from the sink is the one the ingress recorded.
	l, err := h.svc.AccessLogs(ctx, tenantA, "stage", AccessLogQuery{Limit: 1})
	if err != nil || len(l.Records) != 1 {
		t.Fatalf("%+v, %v", l, err)
	}
	ts := t0.Add(14 * time.Second)
	want := AccessLogRecord{TsNs: ts.UnixNano(), Time: ts, DurationNs: 1500, Ingress: "in-1", Service: "web",
		Instance: "i1", Host: "app.apps.example", Method: "GET", Path: "/new4", Proto: "HTTP/2.0", Status: 503,
		Outcome: "5xx", Attempts: 2, HeldNs: 7, BytesIn: 3, BytesOut: 12, ClientIP: "203.0.113.9", UserAgent: "ua",
		SpanID: "00f067aa0ba902b7"}
	if l.Records[0] != want {
		t.Errorf("record %+v, want %+v", l.Records[0], want)
	}

	sink.setDown(true)
	refused("the sink down", ctx, CodeLogStoreUnavailable)
}
