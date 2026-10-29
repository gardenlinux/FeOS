// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// routeClasses is every route of the table with its auth class: the
// contract of who may call what. A route missing here, or here but not in
// the table, fails TestRouteTable.
var routeClasses = map[string]class{
	"GET /api/v1/admin/tenants":                           classAdmin,
	"POST /api/v1/admin/tenants":                          classAdmin,
	"DELETE /api/v1/admin/tenants/{tenant}":               classAdmin,
	"GET /api/v1/admin/tenants/{tenant}/tokens":           classAdmin,
	"POST /api/v1/admin/tenants/{tenant}/tokens":          classAdmin,
	"DELETE /api/v1/admin/tenants/{tenant}/tokens/{hash}": classAdmin,
	"GET /api/v1/admin/nodes":                             classAdmin,
	"PATCH /api/v1/admin/nodes/{node}":                    classAdmin,
	"DELETE /api/v1/admin/nodes/{node}":                   classAdmin,
	"POST /api/v1/admin/nodes/{node}/drain":               classAdmin,
	"POST /api/v1/admin/nodes/{node}/undrain":             classAdmin,
	"GET /api/v1/admin/nodes/{node}/certs":                classAdmin,
	"GET /api/v1/admin/nodes/{node}/activity":             classAdmin,
	"GET /api/v1/admin/sandboxes/{id}/activity":           classAdmin,
	"POST /api/v1/admin/join-tokens":                      classAdmin,
	"GET /api/v1/admin/join-tokens":                       classAdmin,
	"DELETE /api/v1/admin/join-tokens/{id}":               classAdmin,
	"GET /api/v1/admin/ingresses":                         classAdmin,
	"DELETE /api/v1/admin/ingresses/{name}":               classAdmin,
	"GET /api/v1/admin/instances":                         classAdmin,

	"GET /api/v1/tools":                           classTenant,
	"POST /api/v1/sandboxes":                      classTenant,
	"GET /api/v1/sandboxes":                       classTenant,
	"GET /api/v1/sandboxes/{id}":                  classTenant,
	"DELETE /api/v1/sandboxes/{id}":               classTenant,
	"POST /api/v1/sandboxes/{id}/pause":           classTenant,
	"POST /api/v1/sandboxes/{id}/resume":          classTenant,
	"GET /api/v1/sandboxes/{id}/tokens":           classTenant,
	"POST /api/v1/sandboxes/{id}/tokens":          classTenant,
	"DELETE /api/v1/sandboxes/{id}/tokens/{hash}": classTenant,
	"GET /api/v1/sandboxes/{id}/access":           classTenant,

	"POST /api/v1/sandboxes/{id}/exec":                classSandbox,
	"GET /api/v1/sandboxes/{id}/files/{path...}":      classSandbox,
	"PUT /api/v1/sandboxes/{id}/files/{path...}":      classSandbox,
	"PATCH /api/v1/sandboxes/{id}/files/{path...}":    classSandbox,
	"GET /api/v1/sandboxes/{id}/dirs/{path...}":       classSandbox,
	"GET /api/v1/sandboxes/{id}/search":               classSandbox,
	"POST /api/v1/sandboxes/{id}/fetch":               classSandbox,
	"GET /api/v1/sandboxes/{id}/processes":            classSandbox,
	"POST /api/v1/sandboxes/{id}/processes":           classSandbox,
	"GET /api/v1/sandboxes/{id}/processes/{pid}/logs": classSandbox,
	"DELETE /api/v1/sandboxes/{id}/processes/{pid}":   classSandbox,
	"GET /api/v1/sandboxes/{id}/calls/{key}":          classSandbox,
	"DELETE /api/v1/sandboxes/{id}/calls/{key}":       classSandbox,
	"POST /api/v1/sandboxes/{id}/watches":             classSandbox,
	"PUT /api/v1/sandboxes/{id}/watches/{wid}":        classSandbox,
	"GET /api/v1/sandboxes/{id}/watches/{wid}/events": classSandbox,
	"DELETE /api/v1/sandboxes/{id}/watches/{wid}":     classSandbox,

	"/mcp":                       classMCP,
	"/fwd/{id}/{port}/{rest...}": classForward,
	"/fwd/{id}/{port}":           classForward,
	"/api/v1/sandboxes/{id}/ports/{port}/{rest...}": classForward,
	"/api/v1/sandboxes/{id}/ports/{port}":           classForward,
	"/fwd/":                                         classForward,

	"POST /api/v1/environments":                                 classTenant,
	"GET /api/v1/environments":                                  classTenant,
	"POST /api/v1/environments/plan":                            classTenant,
	"GET /api/v1/environments/{env}":                            classTenant,
	"DELETE /api/v1/environments/{env}":                         classTenant,
	"POST /api/v1/environments/{env}/plan":                      classTenant,
	"POST /api/v1/environments/{env}/releases":                  classTenant,
	"GET /api/v1/environments/{env}/releases":                   classTenant,
	"GET /api/v1/environments/{env}/releases/{n}":               classTenant,
	"POST /api/v1/environments/{env}/releases/{n}/revert":       classTenant,
	"PUT /api/v1/environments/{env}/objects/{kind}/{name}":      classTenant,
	"DELETE /api/v1/environments/{env}/objects/{kind}/{name}":   classTenant,
	"GET /api/v1/environments/{env}/objects/{kind}/{name}/logs": classTenant,
	"GET /api/v1/environments/{env}/tags":                       classTenant,
	"GET /api/v1/environments/{env}/tags/{tag}":                 classTenant,
	"PUT /api/v1/environments/{env}/tags/{tag}":                 classTenant,
	"DELETE /api/v1/environments/{env}/tags/{tag}":              classTenant,
	"POST /api/v1/environments/{env}/clone":                     classTenant,
	"GET /api/v1/environments/{env}/secrets":                    classTenant,
	"PUT /api/v1/environments/{env}/secrets/{name}":             classTenant,
	"DELETE /api/v1/environments/{env}/secrets/{name}":          classTenant,
	"GET /api/v1/environments/{env}/incidents":                  classTenant,
	"GET /api/v1/environments/{env}/incidents/{id}":             classTenant,
	"POST /api/v1/environments/{env}/services/{name}/restart":   classTenant,
	"POST /api/v1/environments/{env}/jobs/{name}/run":           classTenant,
	"POST /api/v1/environments/{env}/route-tokens":              classTenant,
	"DELETE /api/v1/environments/{env}/route-tokens":            classTenant,
	"PUT /api/v1/environments/{env}/public":                     classTenant,
	"/api/v1/environments/{env}/http/{path...}":                 classRoute,

	"POST /api/v1/domains":                    classTenant,
	"GET /api/v1/domains":                     classTenant,
	"GET /api/v1/domains/{id}":                classTenant,
	"DELETE /api/v1/domains/{id}":             classTenant,
	"PUT /api/v1/domains/{id}/environment":    classTenant,
	"DELETE /api/v1/domains/{id}/environment": classTenant,
	"POST /api/v1/domains/{id}/check":         classTenant,

	"/api/":             classPublic,
	"GET /healthz":      classPublic,
	"GET /readyz":       classPublic,
	"GET /auth/config":  classPublic,
	"GET /auth/me":      classPublic,
	"POST /auth/logout": classPublic,
	"/auth/":            classPublic,
	"/":                 classPublic,

	// A sandbox's image credentials (ADR 0025).
	"PUT /api/v1/sandboxes/{id}/image-credentials":    classTenant,
	"DELETE /api/v1/sandboxes/{id}/image-credentials": classTenant,
}

// bindingRoutes are the routes that exist only while bindings are on.
var bindingRoutes = map[string]class{
	"PUT /api/v1/sandboxes/{id}/kube":              classSandbox,
	"GET /api/v1/sandboxes/{id}/kube":              classSandbox,
	"DELETE /api/v1/sandboxes/{id}/kube":           classSandbox,
	"PUT /api/v1/sandboxes/{id}/ssh":               classSandbox,
	"GET /api/v1/sandboxes/{id}/ssh":               classSandbox,
	"DELETE /api/v1/sandboxes/{id}/ssh":            classSandbox,
	"POST /api/v1/sandboxes/{id}/ssh/{name}/check": classSandbox,
	"POST /api/v1/kube/preflight":                  classTenant,
	"POST /api/v1/ssh/preflight":                   classTenant,
}

// oidcRoutes exist only with OIDC.
var oidcRoutes = map[string]class{
	"GET /auth/login":    classPublic,
	"GET /auth/callback": classPublic,
}

// concrete turns a pattern into a request: its method (GET when it names
// none) and a path with its wildcards filled in.
func concrete(pattern string, id string) (string, string) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		method, path = http.MethodGet, pattern
	}
	r := strings.NewReplacer("{id}", id, "{tenant}", testTenant, "{node}", "n1", "{hash}", "abcdef0123",
		"{pid}", "p1", "{wid}", "w1", "{name}", "k1", "{port}", "3000", "{path...}", "etc/hosts", "{rest...}", "x/y",
		"{env}", "stage", "{n}", "7", "{kind}", "service", "{tag}", "v1", "{key}", "c1")
	return method, r.Replace(path)
}

// classAnswer checks the answer of a request without credentials on a
// route of class c.
func classAnswer(t *testing.T, e *env, pattern string, c class) {
	t.Helper()
	method, path := concrete(pattern, podid.Mint().String())
	w := e.do(method, path, "", nil)
	switch c {
	case classAdmin:
		expect(t, w, http.StatusUnauthorized, errBody("admin sign-in required"))
		if got := w.Header().Get("WWW-Authenticate"); got != `Basic realm="ferun"` {
			t.Fatalf("%s: WWW-Authenticate %q", pattern, got)
		}
	case classTenant:
		expect(t, w, http.StatusUnauthorized, errBody("tenant bearer required"))
		if got := w.Header().Get("WWW-Authenticate"); got != `Bearer realm="ferun"` {
			t.Fatalf("%s: WWW-Authenticate %q", pattern, got)
		}
	case classSandbox:
		expect(t, w, http.StatusUnauthorized, errBody("sandbox token required"))
		if got := w.Header().Get("WWW-Authenticate"); got != "" {
			t.Fatalf("%s: WWW-Authenticate %q", pattern, got)
		}
	case classMCP:
		if w.Header().Get("X-Door") != "mcp" {
			t.Fatalf("%s: not the MCP door: %d %s", pattern, w.Code, w.Body.String())
		}
	case classRoute:
		if w.Header().Get("X-Door") != "route" {
			t.Fatalf("%s: not the route door: %d %s", pattern, w.Code, w.Body.String())
		}
	case classForward:
		if pattern == "/fwd/" {
			expect(t, w, http.StatusNotFound, "expected /fwd/<sandbox>/<port>/<path>\n")
		} else if w.Header().Get("X-Door") != "ports" {
			t.Fatalf("%s: not the port door: %d %s", pattern, w.Code, w.Body.String())
		}
	}
}

// Every route of the table has the class this test states, answers a
// request without credentials as its class does, and nothing else is
// routed; with bindings off, their routes are unknown routes.
func TestRouteTable(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{false, true} {
		e := newEnv(t, envOptions{bindings: &fakeBindings{on: on}, mcp: marker("mcp"), ports: marker("ports"),
			watches: fakeWatches{}})
		want := map[string]class{}
		for p, c := range routeClasses {
			want[p] = c
		}
		if on {
			for p, c := range bindingRoutes {
				want[p] = c
			}
		}
		got := map[string]class{}
		for _, rt := range e.srv.routes() {
			if _, dup := got[rt.pattern]; dup {
				t.Fatalf("route %s twice", rt.pattern)
			}
			got[rt.pattern] = rt.class
		}
		var diff []string
		for p, c := range want {
			if got[p] != c {
				diff = append(diff, p+": table "+string(got[p])+", want "+string(c))
			}
		}
		for p := range got {
			if _, ok := want[p]; !ok {
				diff = append(diff, p+": in the table, not in the test")
			}
		}
		sort.Strings(diff)
		if len(diff) > 0 {
			t.Fatalf("bindings %v:\n%s", on, strings.Join(diff, "\n"))
		}
		for p, c := range want {
			classAnswer(t, e, p, c)
		}
		if !on {
			sandboxID, sbToken := e.createSandbox()
			for p := range bindingRoutes {
				method, path := concrete(p, sandboxID)
				authz := bearerOf(sbToken)
				if bindingRoutes[p] == classTenant {
					authz = bearerOf(e.tenantToken)
				}
				w := e.do(method, path, authz, nil)
				expect(t, w, http.StatusNotFound, errBody("no such route: "+method+" "+path))
			}
		}
	}
}

// The catch-all answers every other path under /api/ and /auth/ with 404
// "no such route", whatever the method, also a wrong method on a known
// path; never 405.
func TestNoSuchRoute(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/nope"},
		{"PUT", "/api/v1/tools"},
		{"PATCH", "/api/v1/sandboxes"},
		{"POST", "/api/v1/admin/nodes"},
		{"GET", "/api/v1/sandboxes/x/watches"},
		{"GET", "/auth/login"}, // no OIDC
		{"GET", "/auth/callback"},
		{"GET", "/auth/nope"},
	} {
		w := e.do(c.method, c.path, bearerOf(e.tenantToken), nil)
		expect(t, w, http.StatusNotFound, errBody("no such route: "+c.method+" "+c.path))
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("%s %s: Content-Type %q", c.method, c.path, ct)
		}
	}
}

// The doors of other packages get their path values; without them their
// routes answer 503 "not available on this instance" (plain text, and
// JSON on the sandbox class's watch routes).
func TestMounts(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{mcp: marker("mcp"), ports: marker("ports")})
	id := podid.Mint().String()
	for _, c := range []struct{ path, want string }{
		{"/fwd/" + id + "/3000/a/b?q=1", "ports " + id + " 3000 a/b"},
		{"/fwd/" + id + "/3000", "ports " + id + " 3000 "},
		{"/fwd/" + id + "/3000/", "ports " + id + " 3000 "},
		{"/api/v1/sandboxes/" + id + "/ports/5173/src/main.js", "ports " + id + " 5173 src/main.js"},
		{"/mcp", "mcp   "},
	} {
		w := e.do("POST", c.path, "", nil)
		expect(t, w, http.StatusTeapot, c.want)
	}

	bare := newEnv(t, envOptions{})
	for _, path := range []string{"/mcp", "/fwd/" + id + "/3000/x", "/api/v1/sandboxes/" + id + "/ports/80"} {
		w := bare.do("GET", path, "", nil)
		expect(t, w, http.StatusServiceUnavailable, "not available on this instance\n")
	}
	sbID, tok := bare.createSandbox()
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/v1/sandboxes/" + sbID + "/watches"},
		{"PUT", "/api/v1/sandboxes/" + sbID + "/watches/w1"},
		{"GET", "/api/v1/sandboxes/" + sbID + "/watches/w1/events"},
		{"DELETE", "/api/v1/sandboxes/" + sbID + "/watches/w1"},
	} {
		w := bare.do(c.method, c.path, bearerOf(tok), nil)
		expect(t, w, http.StatusServiceUnavailable, errBody("not available on this instance"))
	}

	// With the watch service, its handlers answer.
	ws := newEnv(t, envOptions{watches: fakeWatches{}})
	sbID, tok = ws.createSandbox()
	w := ws.do("GET", "/api/v1/sandboxes/"+sbID+"/watches/w1/events", bearerOf(tok), nil)
	expect(t, w, http.StatusOK, "Events")
}

// With bindings on, the binding routes reach the bindings with their
// classes; the access route always belongs to the owner and answers the
// empty object while bindings are off.
func TestBindingRoutes(t *testing.T) {
	t.Parallel()
	off := newEnv(t, envOptions{bindings: &fakeBindings{on: false}})
	id, tok := off.createSandbox()
	w := off.do("GET", "/api/v1/sandboxes/"+id+"/access", bearerOf(off.tenantToken), nil)
	expect(t, w, http.StatusOK, `{"kube":null,"ssh":null,"denies":[]}`+"\n")
	w = off.do("GET", "/api/v1/sandboxes/"+id+"/access", bearerOf(tok), nil)
	expect(t, w, http.StatusUnauthorized, errBody("unknown tenant token"))
	other := off.mintTenantToken(mustTenant(t, off, "other"))
	w = off.do("GET", "/api/v1/sandboxes/"+id+"/access", bearerOf(other), nil)
	expect(t, w, http.StatusNotFound, errBody("unknown sandbox "+id))

	none := newEnv(t, envOptions{})
	id2, _ := none.createSandbox()
	w = none.do("GET", "/api/v1/sandboxes/"+id2+"/access", bearerOf(none.tenantToken), nil)
	expect(t, w, http.StatusOK, `{"kube":null,"ssh":null,"denies":[]}`+"\n")

	on := newEnv(t, envOptions{bindings: &fakeBindings{on: true}})
	id3, tok3 := on.createSandbox()
	for _, c := range []struct{ method, path, authz, want string }{
		{"PUT", "/api/v1/sandboxes/" + id3 + "/kube", bearerOf(tok3), "PutKube"},
		{"GET", "/api/v1/sandboxes/" + id3 + "/kube", bearerOf(tok3), "GetKube"},
		{"DELETE", "/api/v1/sandboxes/" + id3 + "/kube", bearerOf(tok3), "DeleteKube"},
		{"PUT", "/api/v1/sandboxes/" + id3 + "/ssh", bearerOf(tok3), "PutSSH"},
		{"GET", "/api/v1/sandboxes/" + id3 + "/ssh", bearerOf(tok3), "GetSSH"},
		{"DELETE", "/api/v1/sandboxes/" + id3 + "/ssh", bearerOf(tok3), "DeleteSSH"},
		{"POST", "/api/v1/sandboxes/" + id3 + "/ssh/k1/check", bearerOf(tok3), "CheckSSH"},
		{"POST", "/api/v1/kube/preflight", bearerOf(on.tenantToken), "KubePreflight"},
		{"POST", "/api/v1/ssh/preflight", bearerOf(on.tenantToken), "SSHPreflight"},
		{"GET", "/api/v1/sandboxes/" + id3 + "/access", bearerOf(on.tenantToken), "Access"},
	} {
		w := on.do(c.method, c.path, c.authz, nil)
		expect(t, w, http.StatusOK, c.want)
	}
	// The classes hold: a tenant's token on a set, a sandbox's on a
	// preflight.
	expect(t, on.do("PUT", "/api/v1/sandboxes/"+id3+"/kube", bearerOf(on.tenantToken), nil), http.StatusNotFound,
		errBody("no such sandbox"))
	expect(t, on.do("POST", "/api/v1/kube/preflight", bearerOf(tok3), nil), http.StatusUnauthorized,
		errBody("unknown tenant token"))
}

// mustTenant creates a tenant and returns its name.
func mustTenant(t *testing.T, e *env, name string) string {
	t.Helper()
	if _, _, err := e.st.CreateTenant(t.Context(), name); err != nil {
		t.Fatal(err)
	}
	return name
}

// With OIDC, the sign-in routes join the table, public.
func TestRouteTableOIDC(t *testing.T) {
	t.Parallel()
	p := newTestProvider(t, algRS256)
	e := newEnv(t, envOptions{oidc: p.config(), publicURL: testPublicURL})
	got := map[string]class{}
	for _, rt := range e.srv.routes() {
		got[rt.pattern] = rt.class
	}
	for pt, c := range oidcRoutes {
		if got[pt] != c {
			t.Errorf("%s: %q, want %q", pt, got[pt], c)
		}
	}
	if len(got) != len(routeClasses)+len(oidcRoutes) {
		t.Fatalf("%d routes, want %d", len(got), len(routeClasses)+len(oidcRoutes))
	}
}
