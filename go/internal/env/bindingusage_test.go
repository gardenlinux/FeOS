// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/auth"
)

// fakeEgressSink is the sink's GET /api/v1/egress: it records what it was
// asked and answers erp's requests and those of a sandbox's own binding.
type fakeEgressSink struct {
	mu    sync.Mutex
	asked []string
	since []int64
	until []int64
	down  bool
}

func (f *fakeEgressSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, r.URL.Path+"?environment="+r.URL.Query().Get("environment")+" "+r.Header.Get("Authorization"))
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	until, _ := strconv.ParseInt(r.URL.Query().Get("until"), 10, 64)
	f.since, f.until = append(f.since, since), append(f.until, until)
	if f.down {
		http.Error(w, `{"error":"the sink cannot answer now"}`, http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte(`{"truncated": true, "bindings": [
	  {"binding": "erp", "requests": 1204, "refused": 2, "failing": 0, "tokenFailed": 0, "blocked": 1,
	   "lastNs": "1795082523000000000",
	   "lastProblem": {"tsNs": "1795081277000000000", "class": "blocked", "status": 403, "code": "denied_path",
	     "message": "path /admin is not allowed", "traceId": "4bf92f3577b34da6a3ce929d0e0e4736", "spanId": "00f067aa0ba902b7"},
	   "users": [{"kind": "service", "object": "web", "requests": 1200}, {"kind": "sandbox", "object": "c3kv9mwq2hd", "requests": 4}]},
	  {"binding": "own", "requests": 3, "refused": 0, "failing": 0, "tokenFailed": 0, "blocked": 0,
	   "lastNs": "1795082400000000000", "lastProblem": null,
	   "users": [{"kind": "sandbox", "object": "c3kv9mwq2hd", "requests": 3}]}]}`))
}

// The bindings' requests: every binding of the latest deployment, with
// zeros where unused, and the sandboxes' own, by name, read from the sink
// with the caller's token over the window that ends now; without a sink,
// a token, or with the sink down, the bindings with zeros and why.
func TestBindingUsage(t *testing.T) {
	h := newHarness(t, harnessOptions{bindings: &fakeHTTPBindings{}})
	h.create("shop", nil)
	h.putSecret("shop", "erp-key", "erp-key-v1")
	h.putSecret("shop", "pay-pass", "pay-pass-v1")
	h.apply("shop", shopSpec, 0)
	envID := h.envID("shop")
	sink := &fakeEgressSink{}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	ctx := auth.WithTenant(h.ctx(), auth.Tenant{Name: tenantA, Token: "tok"})

	zeros := func(name string, u BindingUsage, why string) {
		t.Helper()
		b, _ := json.Marshal(u.Bindings)
		if u.Window != "24h" || !strings.Contains(u.Unavailable, why) || u.Truncated || string(b) !=
			`[{"name":"erp","requests":0,"refused":0,"failing":0,"tokenFailed":0,"blocked":0,"lastAt":"","lastProblem":null,"users":[]},`+
				`{"name":"pay","requests":0,"refused":0,"failing":0,"tokenFailed":0,"blocked":0,"lastAt":"","lastProblem":null,"users":[]}]` {
			t.Errorf("%s: %+v %s", name, u, b)
		}
	}
	u, err := h.svc.BindingUsage(ctx, tenantA, "shop", "24h")
	if err != nil {
		t.Fatal(err)
	}
	zeros("no --sink-url", u, "(--sink-url)")
	h.svc.d.SinkURL = srv.URL + "/"
	if u, err = h.svc.BindingUsage(h.ctx(), tenantA, "shop", "24h"); err != nil {
		t.Fatal(err)
	}
	zeros("no token", u, "the caller's token or session")
	if len(sink.asked) != 0 {
		t.Fatalf("asked %v", sink.asked)
	}
	if _, err := h.svc.BindingUsage(auth.WithTenant(h.ctx(), auth.Tenant{Name: tenantB, Token: "tok"}), tenantB, "shop",
		"24h"); err == nil {
		t.Error("another tenant's environment")
	} else {
		wantStatus(t, err, http.StatusNotFound, "no such environment")
	}
	_, err = h.svc.BindingUsage(ctx, tenantA, "shop", "7d")
	wantStatus(t, err, http.StatusBadRequest, `window must be 1h or 24h, got "7d"`)

	before := time.Now()
	if u, err = h.svc.BindingUsage(ctx, tenantA, envID.String(), "1h"); err != nil {
		t.Fatal(err)
	}
	if len(sink.asked) != 1 || sink.asked[0] != "/api/v1/egress?environment="+envID.String()+" Bearer tok" ||
		sink.until[0]-sink.since[0] != int64(time.Hour) || sink.until[0] < before.UnixNano() {
		t.Fatalf("asked %v [%v, %v)", sink.asked, sink.since, sink.until)
	}
	b, _ := json.Marshal(u)
	if want := `{"window":"1h","truncated":true,"bindings":[` +
		`{"name":"erp","requests":1204,"refused":2,"failing":0,"tokenFailed":0,"blocked":1,"lastAt":"2026-11-19T10:02:03Z",` +
		`"lastProblem":{"at":"2026-11-19T09:41:17Z","status":403,"code":"denied_path","message":"path /admin is not allowed",` +
		`"traceId":"4bf92f3577b34da6a3ce929d0e0e4736"},"users":[{"kind":"service","name":"web","requests":1200},` +
		`{"kind":"sandbox","name":"c3kv9mwq2hd","requests":4}]},` +
		`{"name":"own","requests":3,"refused":0,"failing":0,"tokenFailed":0,"blocked":0,"lastAt":"2026-11-19T10:00:00Z",` +
		`"lastProblem":null,"users":[{"kind":"sandbox","name":"c3kv9mwq2hd","requests":3}]},` +
		`{"name":"pay","requests":0,"refused":0,"failing":0,"tokenFailed":0,"blocked":0,"lastAt":"","lastProblem":null,"users":[]}]}`; string(b) != want {
		t.Errorf("usage\n%s\nwant\n%s", b, want)
	}

	// An environment without a deployment has its sandboxes' bindings.
	h.create("empty", nil)
	if u, err := h.svc.BindingUsage(ctx, tenantA, "empty", "24h"); err != nil || len(u.Bindings) != 2 || u.Bindings[0].Name != "erp" {
		t.Errorf("empty: %+v %v", u, err)
	}

	sink.mu.Lock()
	sink.down = true
	sink.mu.Unlock()
	if u, err = h.svc.BindingUsage(ctx, tenantA, "shop", "24h"); err != nil {
		t.Fatal(err)
	}
	zeros("the sink down", u, "the log store does not answer")
}
