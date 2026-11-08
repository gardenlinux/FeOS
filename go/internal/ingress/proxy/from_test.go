// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// fromState is testState with the root route limited to from.
func fromState(from []string, services ...wire.Service) *wire.State {
	s := testState(services...)
	e := s.Envs[testEnv]
	e.Routes[0].From = from
	s.Envs[testEnv] = e
	return s
}

// serveFrom serves GET path at testHost from the client address remote
// (RemoteAddr) with the headers h, straight through the HTTPS handler.
func serveFrom(p *Proxy, remote, path string, h map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "http://"+testHost+path, nil)
	req.RemoteAddr = remote
	for k, v := range h {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTPS().ServeHTTP(rec, req)
	return rec
}

// A route with blocks admits the clients whose address is in one of
// them, of either family, an IPv4-mapped one as IPv4, and answers the
// others 403 without forwarding; the address is the connection's,
// whatever a header claims. A route without blocks admits every client.
func TestRouteFrom(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	upstream(t, fn, "api", "fd00::2")
	al := make(fakeAccessLog, 32)
	p, _ := newTestProxy(t, Config{}, Deps{AccessLog: al}, fn, "n1")
	p.SetState(fromState([]string{"192.0.2.0/24", "2001:db8::/32"},
		svc("web", ep("w1", "fd00::1", "n1")), svc("api", ep("a1", "fd00::2", "n1"))))

	for _, c := range []struct {
		name, remote, path string
		header             map[string]string
		admitted           bool
		upstream           string
	}{
		{"ipv4 inside", "192.0.2.7:4711", "/x", nil, true, "web"},
		{"ipv4 outside", "198.51.100.1:4711", "/x", nil, false, ""},
		{"ipv6 inside", "[2001:db8::5]:4711", "/x", nil, true, "web"},
		{"ipv6 outside", "[2001:db9::5]:4711", "/x", nil, false, ""},
		{"ipv4-mapped inside", "[::ffff:192.0.2.9]:4711", "/x", nil, true, "web"},
		{"ipv4-mapped outside", "[::ffff:198.51.100.9]:4711", "/x", nil, false, ""},
		{"zoned inside", "[2001:db8::5%eth0]:4711", "/x", nil, true, "web"},
		{"headers claim inside", "198.51.100.1:4711", "/x", map[string]string{
			"X-Forwarded-For": "192.0.2.7", "X-Real-Ip": "192.0.2.7", "Forwarded": "for=192.0.2.7",
			"True-Client-Ip": "192.0.2.7"}, false, ""},
		{"no address", "pipe", "/x", nil, false, ""},
		{"route without from", "198.51.100.1:4711", "/api/x", nil, true, "api"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := serveFrom(p, c.remote, c.path, c.header)
			r := al.next(t)
			if !c.admitted {
				if rec.Code != http.StatusForbidden || rec.Body.String() != textRefused+"\n" {
					t.Fatalf("got %d %q, want 403 %q", rec.Code, rec.Body.String(), textRefused)
				}
				if r.Status != http.StatusForbidden || r.Reason != wire.ReasonRouteRefusedClient ||
					r.Env != testEnv || r.Tenant != "acme" || r.Service != "web" || r.Instance != "" ||
					r.Attempts != 0 || r.Outcome != "4xx" {
					t.Fatalf("record %+v", r)
				}
				return
			}
			var e echo
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Upstream != c.upstream {
				t.Fatalf("got %d %q, want %s", rec.Code, rec.Body.String(), c.upstream)
			}
			if r.Status != http.StatusOK || r.Reason != "" {
				t.Fatalf("record %+v", r)
			}
		})
	}
	if n := p.m.refusedClient.Load(); n != 5 {
		t.Errorf("refused %d, want 5", n)
	}
}

// A refused client wakes nothing: the route answers it before its
// service's instances count. A route whose blocks all do not parse
// admits no one.
func TestRouteFromBeforeWake(t *testing.T) {
	fn := newFakeNet()
	waker := &fakeWaker{}
	al := make(fakeAccessLog, 8)
	p, _ := newTestProxy(t, Config{}, Deps{Waker: waker, AccessLog: al}, fn, "n1")
	asleep := wire.Service{Name: "web", Asleep: 1}
	p.SetState(fromState([]string{"192.0.2.0/24"}, asleep))
	if rec := serveFrom(p, "198.51.100.1:4711", "/", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("asleep: %d %q", rec.Code, rec.Body.String())
	}
	if r := al.next(t); r.Reason != wire.ReasonRouteRefusedClient || r.HeldNs != 0 {
		t.Fatalf("asleep: record %+v", r)
	}
	if waker.count() != 0 {
		t.Errorf("a refused client woke the service")
	}

	// Without a service at all, the refusal comes first too.
	p.SetState(fromState([]string{"bogus"}))
	for _, remote := range []string{"192.0.2.1:1", "[2001:db8::1]:1"} {
		if rec := serveFrom(p, remote, "/", nil); rec.Code != http.StatusForbidden {
			t.Fatalf("bogus block, %s: %d %q", remote, rec.Code, rec.Body.String())
		}
		if r := al.next(t); r.Reason != wire.ReasonRouteRefusedClient {
			t.Fatalf("bogus block: record %+v", r)
		}
	}
}

// A held request is routed again when the table changes: a route that
// got blocks without its client refuses it then.
func TestRouteFromWhileHeld(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	waker := &fakeWaker{}
	al := make(fakeAccessLog, 8)
	p, _ := newTestProxy(t, Config{}, Deps{Waker: waker, AccessLog: al}, fn, "n1")
	p.SetState(fromState(nil, wire.Service{Name: "web", Asleep: 1}))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serveFrom(p, "198.51.100.1:4711", "/", nil) }()
	eventually(t, "the wake", func() bool { return waker.count() == 1 })
	p.SetState(fromState([]string{"192.0.2.0/24"}, svc("web", ep("w1", "fd00::1", "n1"))))
	if rec := <-done; rec.Code != http.StatusForbidden || rec.Body.String() != textRefused+"\n" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if r := al.next(t); r.Reason != wire.ReasonRouteRefusedClient || r.Status != http.StatusForbidden || r.HeldNs == 0 {
		t.Fatalf("record %+v", r)
	}
}
