// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// fakeAccessLog hands each record to a channel.
type fakeAccessLog chan wire.AccessLogEntry

func (f fakeAccessLog) Record(e wire.AccessLogEntry) { f <- e }

func (f fakeAccessLog) next(t *testing.T) wire.AccessLogEntry {
	t.Helper()
	select {
	case e := <-f:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no access log record")
	}
	return wire.AccessLogEntry{}
}

func (f fakeAccessLog) none(t *testing.T) {
	t.Helper()
	select {
	case e := <-f:
		t.Fatalf("access log record %+v, want none", e)
	case <-time.After(200 * time.Millisecond):
	}
}

// sendHeaders sends GET path to host with the headers h and decodes the
// echo.
func sendHeaders(t *testing.T, srv *httptest.Server, host, path string, h map[string]string) echo {
	t.Helper()
	req, err := http.NewRequest("GET", srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, v := range h {
		req.Header.Set(k, v)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return decode(t, result{status: res.StatusCode, body: string(b)})
}

func TestParseTraceparent(t *testing.T) {
	const id, parent = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	for _, c := range []struct {
		name string
		vv   []string
		ok   bool
	}{
		{"valid", []string{"00-" + id + "-" + parent + "-01"}, true},
		{"not sampled", []string{"00-" + id + "-" + parent + "-00"}, true},
		{"later version, longer", []string{"01-" + id + "-" + parent + "-01-what-comes"}, true},
		{"later version, exact", []string{"01-" + id + "-" + parent + "-01"}, true},
		{"none", nil, false},
		{"two fields", []string{"00-" + id + "-" + parent + "-01", "00-" + id + "-" + parent + "-01"}, false},
		{"version 00, longer", []string{"00-" + id + "-" + parent + "-01-x"}, false},
		{"later version, no dash", []string{"01-" + id + "-" + parent + "-01x"}, false},
		{"version ff", []string{"ff-" + id + "-" + parent + "-01"}, false},
		{"upper case", []string{"00-" + strings.ToUpper(id) + "-" + parent + "-01"}, false},
		{"zero trace", []string{"00-" + strings.Repeat("0", 32) + "-" + parent + "-01"}, false},
		{"zero parent", []string{"00-" + id + "-" + strings.Repeat("0", 16) + "-01"}, false},
		{"short", []string{"00-" + id + "-" + parent}, false},
		{"not hex", []string{"00-" + id + "-" + parent + "-0g"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, _, _, ok := parseTraceparent(c.vv); ok != c.ok {
				t.Errorf("ok %v, want %v", ok, c.ok)
			}
		})
	}
}

// A request's traceparent is continued: the instance gets its trace with
// the ingress's span as the parent, and its tracestate; the record names
// the span, the client's span and where the request went.
func TestTraceContinued(t *testing.T) {
	const id, parent = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	al := make(fakeAccessLog, 8)
	p, srv := newTestProxy(t, Config{}, Deps{AccessLog: al}, fn, "n1")
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	e := sendHeaders(t, srv, testHost, "/x?secret=1", map[string]string{
		"Traceparent": "00-" + id + "-" + parent + "-01", "Tracestate": "congo=t61rcWkgMzE",
		"User-Agent": "test-agent"})
	tp := e.Headers["Traceparent"]
	if !strings.HasPrefix(tp, "00-"+id+"-") || !strings.HasSuffix(tp, "-01") || len(tp) != 55 {
		t.Fatalf("traceparent %q, want trace %s", tp, id)
	}
	span := tp[36:52]
	if span == parent {
		t.Errorf("the ingress's span is the client's: %s", span)
	}
	if e.Headers["Tracestate"] != "congo=t61rcWkgMzE" {
		t.Errorf("tracestate %q", e.Headers["Tracestate"])
	}
	r := al.next(t)
	if r.TraceID != id || r.SpanID != span || r.ParentSpanID != parent || r.TraceState != "congo=t61rcWkgMzE" ||
		r.TraceFlags != 0x01 {
		t.Errorf("record's span %s/%s/%s %q %x, want %s/%s/%s", r.TraceID, r.SpanID, r.ParentSpanID, r.TraceState,
			r.TraceFlags, id, span, parent)
	}
	if r.Env != testEnv || r.Project != "acme" || r.Service != "web" || r.Instance != "w1" || r.Host != testHost ||
		r.Method != "GET" || r.Path != "/x" || r.Status != http.StatusOK || r.Outcome != "2xx" || r.Attempts != 1 ||
		r.ClientIP != "127.0.0.1" || r.UserAgent != "test-agent" || r.BytesOut == 0 || r.DurationNs <= 0 ||
		r.Proto != "HTTP/1.1" || r.Time.IsZero() {
		t.Errorf("record %+v", r)
	}
}

// Without a valid traceparent the ingress starts a sampled trace and
// passes no tracestate on.
func TestTraceStarted(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	al := make(fakeAccessLog, 8)
	p, srv := newTestProxy(t, Config{}, Deps{AccessLog: al}, fn, "n1")
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	for _, h := range []map[string]string{
		{},
		{"Traceparent": "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01", "Tracestate": "congo=t61rcWkgMzE"},
	} {
		e := sendHeaders(t, srv, testHost, "/", h)
		tp := e.Headers["Traceparent"]
		if _, _, flags, ok := parseTraceparent([]string{tp}); !ok || flags != flagSampled {
			t.Errorf("traceparent %q: not a sampled one", tp)
		}
		if e.Headers["Tracestate"] != "" {
			t.Errorf("tracestate %q passed on without a trace", e.Headers["Tracestate"])
		}
		r := al.next(t)
		if r.TraceID != tp[3:35] || r.SpanID != tp[36:52] || r.ParentSpanID != "" || r.TraceState != "" ||
			r.TraceFlags != flagSampled {
			t.Errorf("record's span %s/%s/%s for %s", r.TraceID, r.SpanID, r.ParentSpanID, tp)
		}
	}
}

// A request that reaches no environment is not recorded.
func TestAccessLogUnrouted(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	al := make(fakeAccessLog, 8)
	p, srv := newTestProxy(t, Config{}, Deps{AccessLog: al}, fn, "n1")
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	if r := get(t, srv, "unknown."+testZone, "/"); r.status != http.StatusNotFound {
		t.Fatalf("status %d", r.status)
	}
	al.none(t)
}

func TestCutField(t *testing.T) {
	if got := cutField("aé", 2); got != "a" {
		t.Errorf("cut within a rune: %q", got)
	}
	if got := cutField("a\xffb", 10); got != "a\uFFFDb" {
		t.Errorf("invalid UTF-8: %q", got)
	}
}
