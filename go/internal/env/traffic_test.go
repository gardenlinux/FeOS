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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
)

// The window ends at now truncated to its step, a multiple of the step
// since the epoch, and starts its length before.
func TestTrafficRange(t *testing.T) {
	now := time.Date(2026, 11, 2, 2, 7, 31, 500, time.FixedZone("CET", 3600))
	for _, c := range []struct {
		window       string
		since, until string
	}{
		{"1h", "2026-11-02T00:07:00Z", "2026-11-02T01:07:00Z"},
		{"24h", "2026-11-01T01:00:00Z", "2026-11-02T01:00:00Z"},
	} {
		w := trafficWindows[c.window]
		since, until := trafficRange(now, w.length, w.step)
		if since.Format(time.RFC3339) != c.since || until.Format(time.RFC3339) != c.until || until.Location() != time.UTC ||
			until.UnixNano()%int64(w.step) != 0 || int(until.Sub(since)/w.step) != map[string]int{"1h": 60, "24h": 96}[c.window] {
			t.Errorf("%s: [%s, %s)", c.window, since, until)
		}
	}
	// On a step's start the window ends there.
	at := time.Date(2026, 11, 2, 1, 15, 0, 0, time.UTC)
	if _, until := trafficRange(at, 24*time.Hour, 15*time.Minute); !until.Equal(at) {
		t.Errorf("until %s", until)
	}
}

// The sink's steps with requests become every bucket of the window, zeros
// and null where it has none; services without requests are left out,
// the rest by name; durations in ms to a tenth.
func TestTrafficOf(t *testing.T) {
	since := time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)
	until := since.Add(5 * time.Minute)
	ns := func(v float64) *float64 { return &v }
	var got []sinkTraffic
	if err := json.Unmarshal([]byte(`[
	  {"service": "web", "requests": 9, "errors": 3, "p50Ns": 12345678.9, "p95Ns": 87049999, "steps": [
	    {"tsNs": "1793581200000000000", "requests": 7, "errors": 3, "p95Ns": 90100000},
	    {"tsNs": "1793581320000000000", "requests": 2, "errors": 0, "p95Ns": 14550000},
	    {"tsNs": "1793581500000000000", "requests": 5, "errors": 0, "p95Ns": 1}]},
	  {"service": "idle", "requests": 0, "errors": 0, "p50Ns": null, "p95Ns": null, "steps": []},
	  {"service": "api", "requests": 1, "errors": 0, "p50Ns": 40000, "p95Ns": 40000, "steps": [
	    {"tsNs": "1793581440000000000", "requests": 1, "errors": 0, "p95Ns": 40000}]}]`), &got); err != nil {
		t.Fatal(err)
	}
	// The source is the read's, which ferund sets.
	got[0].Source, got[2].Source = TrafficApp, TrafficIngress
	out := trafficOf(got, since, until, time.Minute)
	b, _ := json.Marshal(out)
	want := `[{"service":"api","source":"ingress","requests":1,"errors":0,"p50Ms":0,"p95Ms":0,"series":[` +
		`{"t":"2026-11-02T01:00:00Z","requests":0,"errors":0,"p95Ms":null},` +
		`{"t":"2026-11-02T01:01:00Z","requests":0,"errors":0,"p95Ms":null},` +
		`{"t":"2026-11-02T01:02:00Z","requests":0,"errors":0,"p95Ms":null},` +
		`{"t":"2026-11-02T01:03:00Z","requests":0,"errors":0,"p95Ms":null},` +
		`{"t":"2026-11-02T01:04:00Z","requests":1,"errors":0,"p95Ms":0}]},` +
		`{"service":"web","source":"app","requests":9,"errors":3,"p50Ms":12.3,"p95Ms":87,"series":[` +
		`{"t":"2026-11-02T01:00:00Z","requests":7,"errors":3,"p95Ms":90.1},` +
		`{"t":"2026-11-02T01:01:00Z","requests":0,"errors":0,"p95Ms":null},` +
		`{"t":"2026-11-02T01:02:00Z","requests":2,"errors":0,"p95Ms":14.6},` +
		`{"t":"2026-11-02T01:03:00Z","requests":0,"errors":0,"p95Ms":null},` +
		`{"t":"2026-11-02T01:04:00Z","requests":0,"errors":0,"p95Ms":null}]}]`
	if string(b) != want {
		t.Errorf("traffic\n%s\nwant\n%s", b, want)
	}
	if v := nsToMs(ns(150000)); *v != 0.2 {
		t.Errorf("0.15 ms is %v", *v)
	}
	if nsToMs(nil) != nil {
		t.Error("nil")
	}
	if out := trafficOf(nil, since, until, time.Minute); out == nil || len(out) != 0 {
		t.Errorf("none: %#v", out)
	}
}

// fakeTrafficSink is the sink's GET /api/v1/traffic: it records what it
// was asked and answers web's requests in the window's first step.
type fakeTrafficSink struct {
	t     *testing.T
	mu    sync.Mutex
	asked []string
	down  bool
}

func (f *fakeTrafficSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, r.URL.Path+"?"+r.URL.RawQuery+" "+r.Header.Get("Authorization"))
	if f.down {
		http.Error(w, `{"error":"down"}`, http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	type step struct {
		TsNs     string   `json:"tsNs"`
		Requests int64    `json:"requests"`
		Errors   int64    `json:"errors"`
		P95Ns    *float64 `json:"p95Ns"`
	}
	type svc struct {
		Service  string   `json:"service"`
		Requests int64    `json:"requests"`
		Errors   int64    `json:"errors"`
		P50Ns    *float64 `json:"p50Ns"`
		P95Ns    *float64 `json:"p95Ns"`
		Steps    []step   `json:"steps"`
	}
	p := 25e6
	out := struct {
		Services []svc `json:"services"`
	}{}
	// The services' own spans count api's requests, the ingress's web's.
	has := map[string]string{"app": "api", "ingress": "web"}[q.Get("source")]
	for _, s := range q["service"] {
		if s == has {
			out.Services = append(out.Services, svc{Service: s, Requests: 4, Errors: 1, P50Ns: &p, P95Ns: &p,
				Steps: []step{{TsNs: q.Get("since"), Requests: 4, Errors: 1, P95Ns: &p}}})
		} else {
			out.Services = append(out.Services, svc{Service: s, Steps: []step{}})
		}
	}
	_ = json.NewEncoder(w).Encode(out)
}

// The traffic is read from the sink with the caller's token, for the
// services the environment's desired and deployed deployments route to an
// image service; without --sink-url or a token, or with the sink down,
// it answers 503 as the access log does, and another tenant's
// environment is a 404.
func TestTrafficFromSink(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.create("stage", nil)
	h.putSecret("stage", "token", "v1")
	sb := h.sandbox(tenantA, "stage")
	// Deployment 1 routes / to web, /api to api and /dev to the attached
	// dev, which the route door alone serves; deployment 2 drops /api.
	withDev := strings.Replace(previewSpec(sb), `"root": {"path": "/", "service": "web", "port": "http"}`,
		`"root": {"path": "/", "service": "web", "port": "http"}, "dev": {"path": "/dev", "service": "dev", "port": "http"}`, 1)
	h.apply("stage", withDev, 0)
	h.apply("stage", strings.Replace(withDev, `"api": {"path": "/api", "service": "api", "port": "http", "strip": true},`, "", 1), 1)
	e := h.env("stage")
	sink := &fakeTrafficSink{t: t}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	ctx := auth.WithTenant(h.ctx(), auth.Tenant{Name: tenantA, Token: "tok"})

	refused := func(name string, ctx context.Context, tenant, window string, status int, code string) {
		t.Helper()
		_, err := h.svc.Traffic(ctx, tenant, "stage", window)
		var ae *apierr.Error
		if !errors.As(err, &ae) || ae.Status != status || ae.Code != code {
			t.Errorf("%s: %v, want %d %s", name, err, status, code)
		}
	}
	refused("no --sink-url", ctx, tenantA, "1h", http.StatusServiceUnavailable, CodeNoLogStore)
	h.svc.d.SinkURL = srv.URL + "/"
	refused("no token", h.ctx(), tenantA, "1h", http.StatusServiceUnavailable, CodeNoLogStore)
	refused("another tenant's", auth.WithTenant(h.ctx(), auth.Tenant{Name: tenantB, Token: "tok"}), tenantB, "1h",
		http.StatusNotFound, "")
	refused("another window", ctx, tenantA, "7d", http.StatusBadRequest, "")

	// Every image service's own spans are asked for, without api's
	// readiness check (the attached dev is none); the ingress for the
	// services that the routes send it to: the desired deployment routes
	// web alone, the deployed one adds api.
	read := func(window string) Traffic {
		t.Helper()
		tr, err := h.svc.Traffic(ctx, tenantA, "stage", window)
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}
	tr := read("1h")
	query := func(source, services, skip string, tr Traffic, step string) string {
		return "/api/v1/traffic?environment=" + e.ID.String() + services + "&since=" + itoa(tr.Since.UnixNano()) + skip +
			"&source=" + source + "&step=" + step + "&until=" + itoa(tr.Until.UnixNano()) + " Bearer tok"
	}
	want := []string{
		query("app", "&service=api&service=web", "&skip=api%3A%2Freadyz", tr, "60s"),
		query("ingress", "&service=web", "", tr, "60s"),
	}
	if !slices.Equal(sink.asked, want) {
		t.Errorf("asked\n%v\nwant\n%v", sink.asked, want)
	}
	h.exec(`UPDATE environments SET deployed = 1 WHERE id = $1`, e.ID)
	tr = read("24h")
	want = append(want, query("app", "&service=api&service=web", "&skip=api%3A%2Freadyz", tr, "900s"),
		query("ingress", "&service=api&service=web", "", tr, "900s"))
	if !slices.Equal(sink.asked, want) {
		t.Errorf("asked\n%v\nwant\n%v", sink.asked, want)
	}
	if tr.Window != "24h" || tr.Step != "15m" || tr.Until.Sub(tr.Since) != 24*time.Hour || len(tr.Services) != 2 {
		t.Fatalf("traffic %+v", tr)
	}
	// api's from its own spans; web sends none, so the ingress's.
	api, web := tr.Services[0], tr.Services[1]
	if api.Service != "api" || api.Source != TrafficApp || api.Requests != 4 {
		t.Errorf("api %+v", api)
	}
	if web.Service != "web" || web.Source != TrafficIngress || web.Requests != 4 || web.Errors != 1 || *web.P50Ms != 25 ||
		*web.P95Ms != 25 || len(web.Series) != 96 || !web.Series[0].T.Equal(tr.Since) || web.Series[0].Requests != 4 ||
		*web.Series[0].P95Ms != 25 || web.Series[95].Requests != 0 || web.Series[95].P95Ms != nil ||
		!web.Series[95].T.Equal(tr.Until.Add(-15*time.Minute)) {
		t.Errorf("web %+v", web)
	}

	// An environment without a deployment has no traffic, and the sink is
	// not asked.
	h.create("empty", nil)
	if tr, err := h.svc.Traffic(ctx, tenantA, "empty", "1h"); err != nil || len(tr.Services) != 0 || tr.Services == nil ||
		len(sink.asked) != 4 {
		t.Errorf("empty: %+v %v %v", tr, err, sink.asked)
	}

	sink.mu.Lock()
	sink.down = true
	sink.mu.Unlock()
	refused("the sink down", ctx, tenantA, "1h", http.StatusServiceUnavailable, CodeLogStoreUnavailable)
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
