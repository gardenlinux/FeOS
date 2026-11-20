// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"strings"
	"testing"
)

// fecodeSpec is FeCode as one environment with several hosts (ADR
// 0045): fecode.dev serves the API and the docs from backend and the rest
// from ui, git.fecode.dev and every preview under *.boxed.fecode.dev are
// backend's.
const fecodeSpec = `{
  "services": {
    "backend": {"image": "ghcr.io/acme/backend:1", "ports": [{"name": "http", "port": 8080}]},
    "ui": {"image": "ghcr.io/acme/ui:1", "ports": [{"name": "http", "port": 8080}]}
  },
  "routes": {
    "api": {"host": "fecode.dev", "path": "/api/v1", "service": "backend", "port": "http"},
    "docs": {"host": "fecode.dev", "path": "/docs", "service": "backend", "port": "http"},
    "ui": {"host": "fecode.dev", "path": "/", "service": "ui", "port": "http"},
    "git": {"host": "git.fecode.dev", "path": "/", "service": "backend", "port": "http"},
    "boxed": {"host": "*.boxed.fecode.dev", "path": "/", "service": "backend", "port": "http"}
  }
}`

// fakeHostsVC also says the environment's hosts (RouteHosts) and its
// first host (IngressHost).
type fakeHostsVC struct {
	fakeVC
	hosts, served []string
	known         bool
	public        bool
}

func (f fakeHostsVC) RouteHosts() ([]string, []string, bool) { return f.hosts, f.served, f.known }

func (f fakeHostsVC) IngressHost() (string, bool, bool) {
	first := ""
	for _, h := range f.served {
		if !strings.HasPrefix(h, "*.") {
			first = h
			break
		}
	}
	return first, f.public, true
}

// fecodeVC is FeCode's environment: its default host, its domains
// fecode.dev, git.fecode.dev and *.boxed.fecode.dev, all served.
func fecodeVC(mode Mode) fakeHostsVC {
	return fakeHostsVC{fakeVC: defaultVC(mode),
		hosts:  []string{"fecode--fecode.apps.test", "fecode.dev", "git.fecode.dev", "*.boxed.fecode.dev"},
		served: []string{"fecode.dev", "git.fecode.dev", "*.boxed.fecode.dev"}, known: true, public: true}
}

func validateHosts(t *testing.T, vc ValidateContext, raw string, change func(*Spec)) []Finding {
	t.Helper()
	s := mustParse(t, raw)
	if change != nil {
		change(&s)
	}
	return Validate(vc, s, nil)
}

// FeCode's spec is valid: routes of one path on different hosts are no
// twins, and each is said to be served at its host.
func TestValidateRouteHostsFeCode(t *testing.T) {
	fs := validateHosts(t, fecodeVC(ModeApply), fecodeSpec, nil)
	checkFindings(t, fs, []expect{
		i("routes.api", "served by the ingress at https://fecode.dev/api/v1 and by the route door"),
		i("routes.boxed", "served by the ingress at https://*.boxed.fecode.dev/, on every name the wildcard serves"),
		i("routes.docs", "served by the ingress at https://fecode.dev/docs and by the route door"),
		i("routes.git", "served by the ingress at https://git.fecode.dev/ and by the route door"),
		i("routes.ui", "served by the ingress at https://fecode.dev/ and by the route door"),
	})
	// Without an environment, the hosts are checked for their form alone.
	fs = validateHosts(t, defaultVC(ModeSpec), fecodeSpec, nil)
	checkFindings(t, fs, nil)
}

// A route's host is a host of its environment: its default host, a
// domain bound to it in any state, a wildcard domain of it, or a name the
// wildcard serves; anything else is an error with the hosts it may name.
func TestValidateRouteHostOfEnvironment(t *testing.T) {
	route := func(host string) func(*Spec) {
		return func(s *Spec) {
			s.Routes = map[string]Route{"r": {Host: host, Path: "/x", Service: "backend", Port: PortRef{Name: "http"}}}
		}
	}
	foreign := "is no host of this environment"
	for _, c := range []struct {
		host string
		want []expect
	}{
		{"fecode.dev", []expect{i("routes.r", "https://fecode.dev/x")}},
		{"FeCode.Dev.", []expect{i("routes.r", "https://fecode.dev/x")}},
		{"api.boxed.fecode.dev", []expect{i("routes.r", "https://api.boxed.fecode.dev/x")}},
		{"*.boxed.fecode.dev", []expect{i("routes.r", "https://*.boxed.fecode.dev/x")}},
		// The default host, retired while a domain is served, may be
		// named: it is the environment's again once none is.
		{"fecode--fecode.apps.test", []expect{i("routes.r", "served by the route door; the ingress serves it at "+
			"https://fecode--fecode.apps.test/x once the environment is served at fecode--fecode.apps.test")}},
		{"example.com", []expect{e("routes.r.host", "host example.com "+foreign)}},
		{"boxed.fecode.dev", []expect{e("routes.r.host", "host boxed.fecode.dev "+foreign)}},
		{"a.b.boxed.fecode.dev", []expect{e("routes.r.host", foreign)}},
		{"*.fecode.dev", []expect{e("routes.r.host", "host *.fecode.dev "+foreign)}},
		{"other--fecode.apps.test", []expect{e("routes.r.host", foreign)}},
	} {
		t.Run(c.host, func(t *testing.T) {
			checkFindings(t, validateHosts(t, fecodeVC(ModePlan), fecodeSpec, route(c.host)), c.want)
		})
	}
	// The remedy names the hosts; without any, it says how to get one.
	fs := validateHosts(t, fecodeVC(ModePlan), fecodeSpec, route("example.com"))
	if want := "name one of its hosts (fecode--fecode.apps.test, fecode.dev, git.fecode.dev, *.boxed.fecode.dev)"; !strings.Contains(fs[0].Remedy, want) {
		t.Fatalf("remedy %q", fs[0].Remedy)
	}
	none := fakeHostsVC{fakeVC: defaultVC(ModeApply), known: true}
	fs = validateHosts(t, none, fecodeSpec, route("fecode.dev"))
	if len(fs) != 1 || !strings.Contains(fs[0].Remedy, "bind the domain to this environment first, or leave host out") {
		t.Fatalf("without hosts: %s", dump(fs))
	}
}

// A host is a name or a wildcard of letters, digits and hyphens of at
// least two labels, with no port, scheme, path or address.
func TestValidateRouteHostForm(t *testing.T) {
	for _, host := range []string{"fecode.dev:443", "https://fecode.dev", "fecode.dev/x", "a..fecode.dev", "dev",
		"*", "*.dev", "x*.fecode.dev", "*.*.fecode.dev", "10.0.0.1", "[fd00::1]", "-a.fecode.dev", "a_b.fecode.dev",
		"fecode.d", strings.Repeat("a", 64) + ".dev", strings.Repeat("abcdefgh.", 31) + "dev", "bücher.de"} {
		fs := validateHosts(t, defaultVC(ModeSpec), fecodeSpec, func(s *Spec) {
			r := s.Routes["git"]
			r.Host = host
			s.Routes["git"] = r
		})
		checkFindings(t, fs, []expect{e("routes.git.host", "is not a host name")})
	}
}

// Two routes of one path on one host are twins, the host compared as it
// is matched; one with a host and one without are not.
func TestValidateRouteHostTwins(t *testing.T) {
	fs := validateHosts(t, defaultVC(ModeSpec), fecodeSpec, func(s *Spec) {
		s.Routes["ui2"] = Route{Host: "FECODE.dev.", Path: "/", Service: "ui", Port: PortRef{Name: "http"}}
		s.Routes["all"] = Route{Path: "/", Service: "ui", Port: PortRef{Name: "http"}}
		s.Routes["all2"] = Route{Path: "/", Service: "ui", Port: PortRef{Name: "http"}}
	})
	checkFindings(t, fs, []expect{
		e("routes.all2.path", "path / is route all's too"),
		e("routes.ui2.path", "path / on host fecode.dev is route ui's too"),
	})
}

// The stored spec holds a route's host in lower case, without a trailing
// dot.
func TestNormaliseRouteHost(t *testing.T) {
	s := mustParse(t, fecodeSpec)
	r := s.Routes["git"]
	r.Host = "Git.FeCode.dev."
	s.Routes["git"] = r
	norm, _ := Normalise(s, Pins{})
	if got := norm.Routes["git"].Host; got != "git.fecode.dev" {
		t.Fatalf("host %q", got)
	}
	if got := IngressRoutes(&norm); got[3].Name != "git" || got[3].Host != "git.fecode.dev" || got[0].Host != "fecode.dev" {
		t.Fatalf("ingress routes %+v", got)
	}
}
