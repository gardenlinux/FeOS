// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/ports"
)

// hostSpec is FeCode as one environment with several hosts (ADR 0045):
// fecode.example's API and docs go to backend and the rest to ui,
// git.fecode.example and every name under *.boxed.fecode.example to
// backend; /healthz, without a host, to ui on every host.
const hostSpec = `{
  "services": {
    "backend": {"image": "ghcr.io/acme/backend:1", "ports": [{"name": "http", "port": 8080}]},
    "ui": {"image": "ghcr.io/acme/ui:1", "ports": [{"name": "http", "port": 3000}]}
  },
  "routes": {
    "api": {"host": "fecode.example", "path": "/api/v1", "service": "backend", "port": "http"},
    "docs": {"host": "fecode.example", "path": "/docs", "service": "backend", "port": "http"},
    "ui": {"host": "FeCode.example.", "path": "/", "service": "ui", "port": "http"},
    "git": {"host": "git.fecode.example", "path": "/", "service": "backend", "port": "http"},
    "boxed": {"host": "*.boxed.fecode.example", "path": "/", "service": "backend", "port": "http"},
    "health": {"path": "/healthz", "service": "ui", "port": "http"}
  }
}`

// hostEnv is a public environment web of projectA at web-acme.apps.test
// with the domains fecode.example and *.boxed.fecode.example served,
// git.fecode.example bound and being issued, and free.example claimed and
// unbound.
func hostEnv(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, harnessOptions{appsZone: "apps.test"})
	h.create("web", nil)
	h.create("other", nil)
	if _, err := h.svc.SetPublic(h.ctx(), projectA, "web", PublicRequest{Public: true}); err != nil {
		t.Fatal(err)
	}
	web, other := h.env("web"), h.env("other")
	for i, d := range []struct {
		name, state string
		env         any
		cert        bool
	}{{"fecode.example", "served", web.ID, true}, {"*.boxed.fecode.example", "served", web.ID, true},
		{"git.fecode.example", "issuing", web.ID, false}, {"free.example", "unbound", nil, false},
		{"other.example", "served", other.ID, true}} {
		id := fmt.Sprintf("00000000-0000-4000-8000-0000000000%02d", i+1)
		h.exec(`INSERT INTO domains (id, owner, name, token, env_id, bound_at, verification, verified_at, routed, was_routed, state)
			VALUES ($1, $2, $3, 'abcdefghijklmnopqrstuvwxyz234567', $4, now(), 'verified', now(), true, true, $5)`,
			id, projectA, d.name, d.env, d.state)
		if d.cert {
			h.exec(`INSERT INTO domain_certs (domain_id, chain, key_sealed, not_before, not_after, renew_after, serial)
				VALUES ($1, ARRAY['\x01'::bytea], '\x01', now() - interval '1 day', now() + interval '5 days', now(), '1')`, id)
		}
	}
	return h
}

// A route's host is a host of its environment: a domain bound to it,
// served or not, a wildcard of it or a name the wildcard serves, or its
// default host, also while a domain retires it. A domain claimed and not
// bound, or bound to another environment, is refused with a finding.
func TestRouteHostPlan(t *testing.T) {
	t.Parallel()
	h := hostEnv(t)
	ctx := h.ctx()
	plan := func(host string) PlanAnswer {
		t.Helper()
		raw := strings.Replace(hostSpec, `"host": "git.fecode.example"`, `"host": "`+host+`"`, 1)
		a, err := h.svc.Plan(ctx, projectA, "web", PlanRequest{Spec: json.RawMessage(raw)})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	for _, host := range []string{"git.fecode.example", "api.boxed.fecode.example", "web-acme.apps.test"} {
		if a := plan(host); !a.Valid {
			t.Errorf("host %s: %+v", host, a.Findings)
		}
	}
	a := plan("web-acme.apps.test")
	if !hasFinding(a.Findings, spec.SeverityInfo, "routes.git", "the ingress serves it at https://web-acme.apps.test/ "+
		"once the environment is served at web-acme.apps.test") {
		t.Errorf("the retired default host: %+v", a.Findings)
	}
	if a := plan("git.fecode.example"); !hasFinding(a.Findings, spec.SeverityInfo, "routes.git",
		"once the environment is served at git.fecode.example") ||
		!hasFinding(a.Findings, spec.SeverityInfo, "routes.ui", "served by the ingress at https://fecode.example/ and") {
		t.Errorf("a domain being issued: %+v", a.Findings)
	}
	for _, host := range []string{"free.example", "other.example", "boxed.fecode.example", "other-acme.apps.test"} {
		a := plan(host)
		if a.Valid || !hasFinding(a.Findings, spec.SeverityError, "routes.git.host", "host "+host+" is no host of this "+
			"environment") {
			t.Errorf("host %s: %+v", host, a.Findings)
		}
	}
	// An apply is refused the same way.
	raw := strings.Replace(hostSpec, "git.fecode.example", "free.example", 1)
	_, err := h.svc.Apply(ctx, projectA, "web", ApplyRequest{Spec: json.RawMessage(raw), Author: "tester"})
	var ise *InvalidSpecError
	if !errors.As(err, &ise) || !hasFinding(ise.Findings, spec.SeverityError, "routes.git.host", "is no host") {
		t.Fatalf("apply: %v", describe(err))
	}
	// Without an environment, a host is checked for its form alone.
	if a, err := h.svc.PlanSpec(ctx, projectA, PlanSpecRequest{Spec: json.RawMessage(raw)}); err != nil || !a.Valid {
		t.Fatalf("plan without an environment: %+v %v", a, err)
	}
}

// The status shows each route's host and its URL where the ingress serves
// it: at its host while the environment is served there, a route without
// a host at the first address, none for a wildcard. The route door picks
// the routes by the Host the guest sees, X-FeRun-Host first, as the
// ingress does by the request's.
func TestRouteHostStatusAndDoor(t *testing.T) {
	t.Parallel()
	h := hostEnv(t)
	ctx := h.ctx()
	h.apply("web", hostSpec, 0)
	e := h.env("web")
	st, err := h.svc.GetEnv(ctx, projectA, "web")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(st.Hosts, []string{"fecode.example", "*.boxed.fecode.example"}) {
		t.Fatalf("hosts %v", st.Hosts)
	}
	want := map[string][2]string{
		"api":    {"fecode.example", "https://fecode.example/api/v1"},
		"boxed":  {"*.boxed.fecode.example", ""},
		"docs":   {"fecode.example", "https://fecode.example/docs"},
		"git":    {"git.fecode.example", ""},
		"health": {"", "https://fecode.example/healthz"},
		"ui":     {"fecode.example", "https://fecode.example/"},
	}
	for _, r := range st.Routes {
		if w := want[r.Name]; r.Host != w[0] || r.URL != w[1] || r.Door != DoorIngress {
			t.Errorf("route %s: %+v, want host %q URL %q", r.Name, r, w[0], w[1])
		}
	}

	backend := h.instance(e.ID, spec.KindService, "backend", 0, 1, model.PhaseReady, model.StateRunning, "n1", "fd6b::11")
	ui := h.instance(e.ID, spec.KindService, "ui", 0, 1, model.PhaseReady, model.StateRunning, "n1", "fd6b::12")
	tok, err := h.svc.CreateRouteToken(ctx, projectA, "web")
	if err != nil {
		t.Fatal(err)
	}
	mux := doorMux(h.svc)
	door := func(host, guestHost, path string) (podid.ID, int, string) {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/v1/environments/web/http"+path, nil)
		r.Host = host
		if guestHost != "" {
			r.Header.Set(ports.HostHeader, guestHost)
		}
		r.Header.Set(RouteTokenHeader, tok.Token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 200 {
			return "", w.Code, w.Body.String()
		}
		return h.ports.last(t).sandbox, w.Code, ""
	}
	for _, c := range []struct {
		host, guest, path string
		want              podid.ID
	}{
		{"fecode.example", "", "/", ui},
		{"fecode.example", "", "/api/v1/projects", backend},
		{"FeCode.example:443", "", "/docs", backend},
		{"run.example", "fecode.example", "/projects", ui},
		{"run.example", "git.fecode.example", "/", backend},
		{"git.fecode.example", "", "/healthz", ui},
		{"run.example", "8080--abc.boxed.fecode.example", "/docs", backend},
		{"run.example", "", "/healthz", ui},
	} {
		if got, code, body := door(c.host, c.guest, c.path); got != c.want {
			t.Errorf("%s (%s) %s: %s %d %q, want %s", c.host, c.guest, c.path, got, code, body, c.want)
		}
	}
	// A host without a route of its own has the routes without a host.
	if _, code, body := door("run.example", "", "/"); code != 404 || body != "no route for /\n" {
		t.Errorf("another host: %d %q", code, body)
	}
}

// A clone leaves the routes with a host out, as their hosts are the
// source's: the clone has no domain and no label yet.
func TestCloneSpecRouteHosts(t *testing.T) {
	sp, fs := spec.Parse([]byte(hostSpec))
	if spec.HasErrors(fs) {
		t.Fatal(fs)
	}
	stored, err := json.Marshal(sp)
	if err != nil {
		t.Fatal(err)
	}
	raw, fs, err := cloneSpec(stored)
	if err != nil {
		t.Fatal(err)
	}
	var got spec.Spec
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Routes) != 1 || got.Routes["health"].Path != "/healthz" || len(fs) != 5 ||
		!hasFinding(fs, spec.SeverityInfo, "routes.git", "route git is not cloned: its host git.fecode.example is the source's") {
		t.Fatalf("clone %+v, findings %+v", got.Routes, fs)
	}
}
