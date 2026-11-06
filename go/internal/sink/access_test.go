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
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// accessFerund answers GET /api/v1/tenant as ferund does for its
// readers: "Bearer all" holds read on the tenant, "Bearer writer" write,
// "Bearer e1" and the session "s" (in acme) read on the environment e1,
// "Bearer none" nothing.
func accessFerund(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != pathTenant {
		http.NotFound(w, r)
		return
	}
	who := r.Header.Get("Authorization")
	if c, err := r.Cookie("ferun_session"); who == "" && err == nil {
		who = "session " + c.Value + " " + r.Header.Get("FeRun-Tenant")
	}
	switch who {
	case "Bearer all":
		_, _ = io.WriteString(w, `{"tenant":"acme","role":"read","environments":[]}`)
	case "Bearer writer":
		_, _ = io.WriteString(w, `{"tenant":"acme","role":"write","environments":[]}`)
	case "Bearer e1", "session s acme":
		_, _ = io.WriteString(w, `{"tenant":"acme","role":"","environments":["e1"]}`)
	case "Bearer none":
		_, _ = io.WriteString(w, `{"tenant":"acme","role":"","environments":[]}`)
	default:
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unknown tenant token"}`)
	}
}

// A reader of environments only (a person's grant on one, or a token of
// its scope) reads the streams and spans of those environments, none of
// no environment, and no other's traffic; a session reaches the sink by
// its cookie and FeRun-Tenant, and its changes need the CSRF header and
// the same origin (ADR 0031).
func TestReaderAccess(t *testing.T) {
	s, _, _ := newTestStore(t, testConfig())
	tr, tidx, tlg := newTestTraces(t, testConfig())
	ctx := context.Background()
	text := func(i int) string { return fmt.Sprintf("line %d", i) }
	for i, env := range []string{"e1", "e2"} {
		if err := s.Ingest(ctx, batch(StreamKey{"p" + env, "main", "l"}, envLabels("acme", env, i), lines(0, base, 2, text))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Ingest(ctx, batch(StreamKey{"pj", "main", "l"}, Labels{Tenant: "acme", Job: "j1", Attrs: map[string]string{}},
		lines(0, base, 2, text))); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Minute).UnixNano()
	if err := ingestSpans(t, tr, traceRequest(podAttrs("acme", "e1", "api"),
		testSpan(traceA, "000000000000000a", "", "GET /", start, time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	if err := ingestSpans(t, tr, traceRequest(podAttrs("acme", "e2", "web"),
		testSpan(traceA, "000000000000000b", "000000000000000a", "GET /x", start, time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	tidx.sync(tlg)

	ferund := httptest.NewServer(http.HandlerFunc(accessFerund))
	defer ferund.Close()
	ready := make(chan struct{})
	close(ready)
	srv := httptest.NewServer(newAPI(s, tr, NewTenants(ferund.URL, nil, time.Minute), ready, dbtest.Logger(t)))
	defer srv.Close()

	type as struct {
		token, session string
		header         http.Header
	}
	call := func(method, path string, who as) (int, string) {
		t.Helper()
		r, _ := http.NewRequest(method, srv.URL+path, nil)
		if who.token != "" {
			r.Header.Set("Authorization", "Bearer "+who.token)
		}
		if who.session != "" {
			r.AddCookie(&http.Cookie{Name: "ferun_session", Value: who.session})
			r.Header.Set("FeRun-Tenant", "acme")
		}
		for k, v := range who.header {
			r.Header[k] = v
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
	// streams is what the reader finds of the selector: the streams'
	// environments, or "none" (an empty list), or the status.
	streams := func(sel string, who as) string {
		t.Helper()
		code, b := call("GET", "/api/v1/log-streams?"+sel, who)
		if code != http.StatusOK {
			return fmt.Sprint(code)
		}
		var out struct{ Streams []StreamJSON }
		if err := json.Unmarshal([]byte(b), &out); err != nil {
			t.Fatal(err)
		}
		envs := []string{}
		for _, st := range out.Streams {
			envs = append(envs, st.Environment+"/"+st.Job)
		}
		return fmt.Sprint(envs)
	}
	lineCount := func(sel string, who as) string {
		t.Helper()
		code, b := call("GET", "/api/v1/logs?since=0&"+sel, who)
		if code != http.StatusOK {
			return fmt.Sprint(code)
		}
		var page PageJSON
		if err := json.Unmarshal([]byte(b), &page); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(len(page.Lines))
	}
	all, e1, sess, none := as{token: "all"}, as{token: "e1"}, as{session: "s"}, as{token: "none"}
	for _, c := range []struct {
		name, got, want string
	}{
		{"all: e1", streams("environment=e1", all), "[e1/]"},
		{"all: the job", streams("job=j1", all), "[/j1]"},
		{"e1: e1", streams("environment=e1", e1), "[e1/]"},
		{"e1: e2", streams("environment=e2", e1), "[]"},
		{"e1: the job", streams("job=j1", e1), "[]"},
		{"e1: e2's instance", streams("instance=inst1", e1), "[]"},
		{"session: e1", streams("environment=e1", sess), "[e1/]"},
		{"session: e2", streams("environment=e2", sess), "[]"},
		{"none", streams("environment=e1", none), "404"},
		{"no one", streams("environment=e1", as{}), "401"},
		{"all: e2's lines", lineCount("environment=e2", all), "2"},
		{"e1: e1's lines", lineCount("environment=e1", e1), "2"},
		{"e1: e2's lines", lineCount("environment=e2", e1), "0"},
		{"e1: the job's lines", lineCount("job=j1", e1), "0"},
		{"session: e1's lines", lineCount("environment=e1", sess), "2"},
	} {
		if c.got != c.want {
			t.Errorf("%s: %s, want %s", c.name, c.got, c.want)
		}
	}

	// A follow gives a line of the reader's environments only.
	for _, c := range []struct {
		env    string
		within []string
		want   bool
	}{{"e1", nil, true}, {"", nil, true}, {"e1", []string{"e1"}, true}, {"e2", []string{"e1"}, false}, {"", []string{"e1"}, false}} {
		if got := selects("acme", Selector{Within: c.within}, &quickwit.Doc{Tenant: "acme", Environment: c.env}); got != c.want {
			t.Errorf("a follow's line of %q within %v: %v", c.env, c.within, got)
		}
	}

	// A session without its tenant is asked to name it.
	r, _ := http.NewRequest("GET", srv.URL+"/api/v1/log-streams?environment=e1", nil)
	r.AddCookie(&http.Cookie{Name: "ferun_session", Value: "s"})
	if resp, err := http.DefaultClient.Do(r); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a session without FeRun-Tenant: %v %v", resp, err)
	}

	// Deleting streams needs write on the tenant; a session's delete the
	// CSRF header and the same origin first.
	del := "/api/v1/log-streams?environment=e2"
	for _, c := range []struct {
		name string
		who  as
		code int
		want string
	}{
		{"e1", e1, 403, `"code":"role_required","role":"write","scope":"tenant"`},
		{"all", all, 403, `"code":"role_required"`},
		{"a session without the header", sess, 403, "missing X-Requested-With header"},
		{"a session of another origin", as{session: "s", header: http.Header{"X-Requested-With": {"ferun"},
			"Origin": {"https://evil.example"}}}, 403, "cross-origin request refused"},
		{"a session of the same origin", as{session: "s", header: http.Header{"X-Requested-With": {"ferun"},
			"Sec-Fetch-Site": {"same-origin"}}}, 403, `"code":"role_required"`},
		{"write", as{token: "writer"}, 204, ""},
	} {
		code, b := call("DELETE", del, c.who)
		if code != c.code || !strings.Contains(b, c.want) {
			t.Errorf("delete as %s: %d %s", c.name, code, b)
		}
	}

	// Traffic: of a readable environment only.
	from := start - start%int64(time.Minute) - int64(time.Hour)
	window := url.Values{"service": {"api"}, "step": {"60s"}, "since": {fmt.Sprint(from)},
		"until": {fmt.Sprint(from + 2*int64(time.Hour))}}
	for _, c := range []struct {
		env  string
		who  as
		code int
	}{{"e1", e1, 200}, {"e2", e1, 404}, {"e2", all, 200}, {"e1", sess, 200}} {
		window.Set("environment", c.env)
		if code, b := call("GET", "/api/v1/traffic?"+window.Encode(), c.who); code != c.code {
			t.Errorf("traffic of %s as %+v: %d %s", c.env, c.who, code, b)
		}
	}

	// Traces: a trace's spans of the readable environments; the services
	// of those.
	spans := func(who as) string {
		t.Helper()
		code, b := call("GET", "/api/v1/jaeger/api/traces/"+traceA, who)
		if code != http.StatusOK {
			return fmt.Sprint(code)
		}
		var out struct {
			Data []struct {
				Spans []struct{ OperationName string }
			}
		}
		if err := json.Unmarshal([]byte(b), &out); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, sp := range out.Data[0].Spans {
			names = append(names, sp.OperationName)
		}
		return strings.Join(names, ",")
	}
	if got := spans(all); got != "GET /,GET /x" {
		t.Errorf("the trace for all: %s", got)
	}
	if got := spans(e1); got != "GET /" {
		t.Errorf("the trace for e1: %s", got)
	}
	if got := spans(sess); got != "GET /" {
		t.Errorf("the trace for the session: %s", got)
	}
	if _, b := call("GET", "/api/v1/jaeger/api/services", e1); !strings.Contains(b, `"data":["api"]`) {
		t.Errorf("services for e1: %s", b)
	}
	if _, b := call("GET", "/api/v1/jaeger/api/traces?service=web", e1); !strings.Contains(b, `"data":[]`) {
		t.Errorf("e2's traces for e1: %s", b)
	}
}
