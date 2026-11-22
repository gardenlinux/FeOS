// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	otlptracev1 "github.com/gardenlinux/feos/go/gen/otlp/collector/trace/v1"
	commonv1 "github.com/gardenlinux/feos/go/gen/otlp/common/v1"
	tracev1 "github.com/gardenlinux/feos/go/gen/otlp/trace/v1"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// accessFerund answers GET /api/v1/project as ferund does for its
// readers: "Bearer all" holds read on the project, "Bearer writer" write,
// "Bearer e1" and the session "s" (in acme) read on the environment e1,
// "Bearer none" nothing.
func accessFerund(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != pathProject {
		http.NotFound(w, r)
		return
	}
	who := r.Header.Get("Authorization")
	if c, err := r.Cookie(auth.SessionCookie); who == "" && err == nil {
		who = "session " + c.Value + " " + r.Header.Get("FeRun-Project")
	}
	switch who {
	case "Bearer all":
		_, _ = io.WriteString(w, `{"project":"acme","role":"read","environments":[]}`)
	case "Bearer writer":
		_, _ = io.WriteString(w, `{"project":"acme","role":"write","environments":[]}`)
	case "Bearer e1", "session s acme":
		_, _ = io.WriteString(w, `{"project":"acme","role":"","environments":["e1"]}`)
	case "Bearer none":
		_, _ = io.WriteString(w, `{"project":"acme","role":"","environments":[]}`)
	default:
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unknown project token"}`)
	}
}

// A reader of environments only (a person's grant on one, or a token of
// its scope) reads the streams and spans of those environments, none of
// no environment, and no other's traffic; a session reaches the sink by
// its cookie and FeRun-Project, and its changes need the CSRF header and
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
	if err := s.Ingest(ctx, batch(StreamKey{"pj", "main", "l"}, Labels{Project: "acme", Job: "j1", Attrs: map[string]string{}},
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
	srv := httptest.NewServer(newAPI(s, tr, NewProjects(ferund.URL, nil, time.Minute), ready, dbtest.Logger(t)))
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
			r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: who.session})
			r.Header.Set("FeRun-Project", "acme")
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
		if got := selects("acme", Selector{Within: c.within}, &quickwit.Doc{Project: "acme", Environment: c.env}); got != c.want {
			t.Errorf("a follow's line of %q within %v: %v", c.env, c.within, got)
		}
	}
	// And none of before the project's making.
	for ts, want := range map[int64]bool{99: false, 100: true} {
		if got := selects("acme", Selector{From: 100}, &quickwit.Doc{Project: "acme", TS: ts}); got != want {
			t.Errorf("a follow's line at %d from 100: %v", ts, got)
		}
	}

	// A session without its project is asked to name it (here under the
	// cookie's old name, which the sink takes for one release).
	r, _ := http.NewRequest("GET", srv.URL+"/api/v1/log-streams?environment=e1", nil)
	r.AddCookie(&http.Cookie{Name: auth.OldSessionCookie, Value: "s"})
	if resp, err := http.DefaultClient.Do(r); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a session without FeRun-Project: %v %v", resp, err)
	}

	// Deleting streams needs write on the project; a session's delete the
	// CSRF header and the same origin first.
	del := "/api/v1/log-streams?environment=e2"
	for _, c := range []struct {
		name string
		who  as
		code int
		want string
	}{
		{"e1", e1, 403, `"code":"role_required","role":"write","scope":"project"`},
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

// A project made under the name of a deleted one is another (ADR 0056):
// a reader of it reads nothing of before its making, whatever it names
// (here the old project's environment e0, its service old-api and its
// trace), in any read; a stream with a line of before (the old project's
// pod in its last seconds) is not its, one begun after is (the follow
// below: the bound is a time, so a pod of the old project that runs on
// after the making, on a node out of the leader's reach, is read as the
// new one's; sink.md, Readers; backlog). A kept answer of the old project
// reads on for its TTL by what it names, and a search of spans that names
// nothing asks ferund afresh, which refuses it. An answer without
// createdAt (a ferund of before) bounds nothing.
func TestProjectMadeAgain(t *testing.T) {
	cfg := testConfig()
	cfg.Heartbeat = 50 * time.Millisecond
	s, sidx, slg := newTestStore(t, cfg)
	tr, tidx, tlg := newTestTraces(t, cfg)
	ctx := context.Background()
	now := time.Now()
	made := now.Add(-10 * time.Minute).UTC().Truncate(time.Microsecond)
	before, after := made.Add(-5*time.Minute).UnixNano(), made.Add(time.Minute).UnixNano()
	const traceOld, traceNew, traceMix = "11111111111111111111111111111111", "22222222222222222222222222222222",
		"33333333333333333333333333333333"

	ingest := func(key StreamKey, env string, replica int, ts int64, off int64, texts ...string) {
		t.Helper()
		ls := lines(off, ts, len(texts), func(i int) string { return texts[i] })
		for i := range ls {
			if strings.HasPrefix(ls[i].Text, "mix") {
				ls[i].Trace = traceMix
			}
		}
		if err := s.Ingest(ctx, batch(key, envLabels("acme", env, replica), ls)); err != nil {
			t.Fatal(err)
		}
	}
	oldKey, newKey := StreamKey{"p-old", "main", "l"}, StreamKey{"p-new", "main", "l"}
	ingest(oldKey, "e0", 0, before, 0, "old0", "mix-old")
	ingest(newKey, "e1", 0, after, 0, "new0", "mix-new")
	sidx.sync(slg)
	egress := func(n int, start int64) *tracev1.Span {
		sp := testSpan(fmt.Sprintf("%032x", 9000+n), fmt.Sprintf("%016x", 9000+n), "", "GET erp", start, time.Millisecond)
		sp.Kind = tracev1.Span_SPAN_KIND_CLIENT
		sp.Attributes = []*commonv1.KeyValue{kv("http.request.method", str("GET")), kv("ferun.binding", str("erp"))}
		return sp
	}
	for _, r := range []*otlptracev1.ExportTraceServiceRequest{
		traceRequest(podAttrs("acme", "e0", "old-api"), testSpan(traceOld, "00000000000000a1", "", "GET /old", before, time.Millisecond),
			testSpan(traceMix, "00000000000000a2", "", "GET /mix-old", before, time.Millisecond)),
		traceRequest(podAttrs("acme", "e1", "api"), testSpan(traceNew, "00000000000000b1", "", "GET /new", after, time.Millisecond),
			testSpan(traceMix, "00000000000000b2", "00000000000000a2", "GET /mix-new", after, time.Millisecond)),
		traceRequest(egressPod("e0", "service", "old-api"), egress(1, before)),
		traceRequest(egressPod("e1", "service", "api"), egress(2, after)),
	} {
		if err := ingestSpans(t, tr, r); err != nil {
			t.Fatal(err)
		}
	}
	tidx.sync(tlg)

	var remade atomic.Bool
	ferund := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer := func(at time.Time, role, envs string) {
			_, _ = fmt.Fprintf(w, `{"project":"acme","createdAt":%q,"role":%q,"environments":%s}`, at.Format(time.RFC3339Nano), role, envs)
		}
		switch r.Header.Get("Authorization") {
		case "Bearer old":
			// The deleted project's token went with it.
			if remade.Load() {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"unknown project token"}`)
				return
			}
			answer(made.Add(-24*time.Hour), "read", "[]")
		case "Bearer new":
			answer(made, "write", "[]")
		case "Bearer new-envs":
			answer(made, "", `["e0","e1"]`)
		case "Bearer before":
			_, _ = io.WriteString(w, `{"project":"acme","role":"read","environments":[]}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer ferund.Close()
	ready := make(chan struct{})
	close(ready)
	srv := httptest.NewServer(newAPI(s, tr, NewProjects(ferund.URL, nil, time.Minute), ready, dbtest.Logger(t)))
	defer srv.Close()
	call := func(method, who, path string) (int, string) {
		t.Helper()
		r, _ := http.NewRequest(method, srv.URL+path, nil)
		r.Header.Set("Authorization", "Bearer "+who)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
	// read answers a read as a short text: the lines' texts, the
	// streams' replicas and first times, the names, the traces' spans,
	// the traffic's requests, the bindings' requests; or the status.
	read := func(who, path string) string {
		t.Helper()
		code, b := call("GET", who, path)
		if code != http.StatusOK {
			return fmt.Sprint(code)
		}
		var out struct {
			Lines    []LineJSON
			Streams  json.RawMessage
			Data     json.RawMessage
			Services []ServiceTraffic
			Bindings []BindingEgress
		}
		if err := json.Unmarshal([]byte(b), &out); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		var parts []string
		switch {
		case strings.HasPrefix(path, "/api/v1/logs"):
			for _, l := range out.Lines {
				parts = append(parts, l.Text)
			}
		case strings.HasPrefix(path, "/api/v1/log-streams"):
			var sts []StreamJSON
			_ = json.Unmarshal(out.Streams, &sts)
			for _, st := range sts {
				parts = append(parts, fmt.Sprintf("%s@%t", st.Instance, !st.FirstTime.Before(made)))
			}
		case strings.HasPrefix(path, "/api/v1/jaeger/api/traces"):
			var trs []struct {
				Spans []struct{ OperationName string }
			}
			_ = json.Unmarshal(out.Data, &trs)
			for _, tr := range trs {
				var ops []string
				for _, sp := range tr.Spans {
					ops = append(ops, sp.OperationName)
				}
				slices.Sort(ops)
				parts = append(parts, strings.Join(ops, "+"))
			}
			slices.Sort(parts)
		case strings.HasPrefix(path, "/api/v1/jaeger"):
			_ = json.Unmarshal(out.Data, &parts)
		case strings.HasPrefix(path, "/api/v1/traffic"):
			for _, st := range out.Services {
				parts = append(parts, fmt.Sprintf("%s:%d", st.Service, st.Requests))
			}
		case strings.HasPrefix(path, "/api/v1/egress"):
			for _, b := range out.Bindings {
				parts = append(parts, fmt.Sprintf("%s:%d", b.Binding, b.Requests))
			}
		}
		return strings.Join(parts, ",")
	}
	us := func(ns int64) string { return fmt.Sprint(ns / 1000) }
	minute := int64(time.Minute)
	window := fmt.Sprintf("since=%d&until=%d", before-before%minute-minute, now.UnixNano()-now.UnixNano()%minute+minute)
	traffic := "/api/v1/traffic?source=app&step=60s&service=api&service=old-api&" + window

	// A reader of the old project first, its answer kept.
	for _, c := range []struct{ path, want string }{
		{"/api/v1/log-streams?environment=e0", "inst0@false"},
		{"/api/v1/jaeger/api/services", "api,old-api"},
	} {
		if got := read("old", c.path); got != c.want {
			t.Errorf("old reader %s: %s, want %s", c.path, got, c.want)
		}
	}
	remade.Store(true)

	for _, c := range []struct{ who, path, want string }{
		// Logs and streams: of the new project only, whatever is named.
		{"new", "/api/v1/logs?environment=e0", ""},
		{"new", "/api/v1/logs?environment=e0&since=0", ""},
		{"new", "/api/v1/logs?environment=e0&direction=forward", ""},
		{"new", "/api/v1/logs?environment=e1&since=0", "new0,mix-new"},
		{"new", "/api/v1/logs?environment=e1&direction=forward", "new0,mix-new"},
		{"new", "/api/v1/logs?instance=inst0&since=0", "new0,mix-new"},
		{"new", "/api/v1/logs?environment=e0&trace=" + traceMix, ""},
		{"new", "/api/v1/logs?environment=e1&trace=" + traceMix, "mix-new"},
		{"new", "/api/v1/log-streams?environment=e0&since=0", ""},
		{"new", "/api/v1/log-streams?instance=inst0&since=0", "inst0@true"},
		{"new-envs", "/api/v1/logs?environment=e0&since=0", ""},
		{"new-envs", "/api/v1/log-streams?instance=inst0&since=0", "inst0@true"},
		// Spans: names, a trace, a search, the traffic, the bindings.
		{"new", "/api/v1/jaeger/api/services", "api"},
		{"new", "/api/v1/jaeger/api/services/old-api/operations", ""},
		{"new", "/api/v1/jaeger/api/services/api/operations", "GET /mix-new,GET /new,GET erp"},
		{"new", "/api/v1/jaeger/api/traces/" + traceOld, "404"},
		{"new", "/api/v1/jaeger/api/traces/" + traceMix, "GET /mix-new"},
		{"new", "/api/v1/jaeger/api/traces/" + traceNew, "GET /new"},
		{"new", "/api/v1/jaeger/api/traces?service=old-api&start=" + us(before-minute), ""},
		{"new", "/api/v1/jaeger/api/traces?service=api&start=" + us(before-minute), "GET /mix-new,GET /new,GET erp"},
		{"new-envs", "/api/v1/jaeger/api/services", "api"},
		{"new-envs", "/api/v1/jaeger/api/traces/" + traceMix, "GET /mix-new"},
		{"new", traffic + "&environment=e0", "api:0,old-api:0"},
		{"new", traffic + "&environment=e1", "api:2,old-api:0"},
		{"new", "/api/v1/egress?environment=e0&" + window, ""},
		{"new", "/api/v1/egress?environment=e1&" + window, "erp:1"},
		// A ferund of before: no bound.
		{"before", "/api/v1/logs?environment=e0&since=0", "old0,mix-old"},
		{"before", "/api/v1/jaeger/api/services", "api,old-api"},
		{"before", "/api/v1/jaeger/api/traces/" + traceMix, "GET /mix-new+GET /mix-old"},
		{"before", traffic + "&environment=e0", "api:0,old-api:2"},
		{"before", "/api/v1/egress?environment=e0&" + window, "erp:1"},
		// The old project's kept answer reads by what it names; a search of
		// spans that names nothing asks afresh, and is refused.
		{"old", "/api/v1/log-streams?environment=e0", "inst0@false"},
		{"old", "/api/v1/jaeger/api/services?environment=e0", "old-api"},
		{"old", "/api/v1/jaeger/api/services", "401"},
		{"old", "/api/v1/jaeger/api/services/api/operations", "401"},
		{"old", "/api/v1/jaeger/api/traces?service=api&start=" + us(before-minute), "401"},
		{"old", "/api/v1/jaeger/api/traces/" + traceNew, "401"},
	} {
		if got := read(c.who, c.path); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.who, c.path, got, c.want)
		}
	}

	// A follow: none of the old project's lines, not the ones its pod
	// sends late, nor one it sends after the making (its stream began
	// before); a stream begun since is followed, of the old project's
	// environment e0 too: that is the bound's residual (a pod of the old
	// project still running), which this pins until it is closed.
	fctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	fr, _ := http.NewRequestWithContext(fctx, http.MethodGet, srv.URL+"/api/v1/logs?environment=e0&kind=service&object=api&follow=1", nil)
	fr.Header.Set("Authorization", "Bearer new")
	fresp, err := http.DefaultClient.Do(fr)
	if err != nil || fresp.StatusCode != http.StatusOK {
		t.Fatalf("follow: %v %v", fresp, err)
	}
	defer func() { _ = fresp.Body.Close() }()
	sc := bufio.NewScanner(fresp.Body)
	// The follow beats once it reads its partitions.
	for sc.Scan() && sc.Text() != "{}" {
		t.Errorf("before any line: %s", sc.Text())
	}
	ingest(oldKey, "e0", 0, before+int64(time.Second), 100, "late-old")
	ingest(oldKey, "e0", 0, after, 200, "drain")
	ingest(StreamKey{"p-x", "main", "l"}, "e0", 1, after+int64(time.Second), 0, "fresh")
	for sc.Scan() {
		var o struct{ Line *LineJSON }
		if err := json.Unmarshal(sc.Bytes(), &o); err != nil {
			t.Fatal(err)
		}
		if o.Line == nil {
			continue
		}
		if o.Line.Text != "fresh" {
			t.Errorf("the follow gave %q", o.Line.Text)
		}
		break
	}
	cancel()

	// Deleting the environment's streams deletes the new project's alone.
	if code, b := call("DELETE", "new", "/api/v1/log-streams?environment=e0"); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, b)
	}
	if got := read("before", "/api/v1/log-streams?environment=e0&since=0"); got != "inst0@false" {
		t.Errorf("after the delete: %s", got)
	}
	if got := read("before", "/api/v1/logs?environment=e0&since=0"); !strings.HasPrefix(got, "old0,mix-old") {
		t.Errorf("the old project's lines after the delete: %s", got)
	}
}
