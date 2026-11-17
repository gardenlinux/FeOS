// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// hostState is FeCode as one environment with several hosts (ADR 0045):
// fecode.dev's API and docs go to backend and the rest to ui,
// git.fecode.dev and every name under *.boxed.fecode.dev to backend; a
// route without a host, /healthz, answers on every host, and git.fecode.dev's
// /admin only to 192.0.2.0/24.
func hostState() *wire.State {
	s := testState(svc("backend", ep("b1", "fd00::1", "n1")), svc("ui", ep("u1", "fd00::2", "n1")))
	e := s.Envs[testEnv]
	e.Hosts = []string{"fecode.dev", "git.fecode.dev", "*.boxed.fecode.dev"}
	e.Routes = []wire.Route{
		{Name: "admin", Host: "git.fecode.dev", Path: "/admin", Service: "backend", Port: "http", From: []string{"192.0.2.0/24"}},
		{Name: "api", Host: "fecode.dev", Path: "/api/v1", Service: "backend", Port: "http"},
		{Name: "boxed", Host: "*.boxed.fecode.dev", Path: "/", Service: "backend", Port: "http"},
		{Name: "docs", Host: "fecode.dev", Path: "/docs", Service: "backend", Port: "http"},
		{Name: "git", Host: "git.fecode.dev", Path: "/", Service: "backend", Port: "http"},
		{Name: "health", Path: "/healthz", Service: "ui", Port: "http"},
		{Name: "ui", Host: "fecode.dev", Path: "/", Service: "ui", Port: "http"},
	}
	s.Envs[testEnv] = e
	return s
}

// The ingress serves a route on its host alone: of the routes that answer
// on a request's host the longest prefix wins, a wildcard's route on
// every name it serves, a route without a host on every host; a route's
// from holds on its host.
func TestRouteHosts(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "backend", "fd00::1")
	upstream(t, fn, "ui", "fd00::2")
	p, _ := newTestProxy(t, Config{}, Deps{}, fn, "n1")
	p.SetState(hostState())

	tb := p.cur.Load().t
	for _, c := range []struct{ host, path, route string }{
		{"fecode.dev", "/", "ui"},
		{"fecode.dev", "/projects/kino", "ui"},
		{"fecode.dev", "/api/v1/projects", "api"},
		{"fecode.dev", "/docs/intro", "docs"},
		{"fecode.dev", "/healthz", "health"},
		{"fecode.dev", "/admin", "ui"},
		{"git.fecode.dev", "/", "git"},
		{"git.fecode.dev", "/api/v1/projects", "git"},
		{"git.fecode.dev", "/healthz", "health"},
		{"8080--abc.boxed.fecode.dev", "/", "boxed"},
		{"8080--abc.boxed.fecode.dev", "/docs", "boxed"},
	} {
		tg, rep := tb.route(c.host, c.path, netip.MustParseAddr("192.0.2.7"))
		if rep != nil || tg.route == nil || tg.route.Name != c.route {
			t.Errorf("%s%s: %+v %+v, want route %s", c.host, c.path, tg.route, rep, c.route)
		}
	}

	serve := func(host, path, remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "https://"+host+path, nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		p.ServeHTTPS().ServeHTTP(rec, req)
		return rec
	}
	for _, c := range []struct{ host, path, upstream string }{
		{"fecode.dev", "/", "ui"},
		{"FeCode.Dev.", "/api/v1/x", "backend"},
		{"git.fecode.dev", "/", "backend"},
		{"git.fecode.dev:443", "/healthz", "ui"},
		{"x.boxed.fecode.dev", "/", "backend"},
	} {
		rec := serve(c.host, c.path, "192.0.2.7:4711")
		var e echo
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Upstream != c.upstream {
			t.Errorf("%s%s: %d %q, want %s", c.host, c.path, rec.Code, rec.Body.String(), c.upstream)
		}
	}
	if rec := serve("git.fecode.dev", "/admin", "198.51.100.1:4711"); rec.Code != http.StatusForbidden {
		t.Errorf("admin from outside: %d %q", rec.Code, rec.Body.String())
	}
	if rec := serve("fecode.dev", "/admin", "198.51.100.1:4711"); rec.Code != http.StatusOK {
		t.Errorf("fecode.dev's /admin is ui's: %d %q", rec.Code, rec.Body.String())
	}
	// A host the environment has, without a route that answers on it.
	s := hostState()
	e := s.Envs[testEnv]
	e.Routes = e.Routes[:1]
	s.Envs[testEnv] = e
	p.SetState(s)
	if rec := serve("fecode.dev", "/x", "192.0.2.7:4711"); rec.Code != http.StatusNotFound || rec.Body.String() != "no route for /x\n" {
		t.Errorf("no route on the host: %d %q", rec.Code, rec.Body.String())
	}
}
