// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// bindingsSpec is a spec with an ERP system's API in the name mode, named
// by the web service and the migrate job, and a payment provider's API
// intercepted, named by the worker alone.
const bindingsSpec = `{
  "services": {
    "web": {"image": "ghcr.io/acme/web:1", "replicas": 2, "bindings": ["erp"]},
    "worker": {"image": "ghcr.io/acme/worker:1", "bindings": ["pay", "erp"]}
  },
  "jobs": {"migrate": {"image": "ghcr.io/acme/migrate:1", "bindings": ["erp"]}},
  "addons": {"db": {"type": "postgres", "version": "17"}},
  "bindings": {
    "erp": {"upstream": "https://api.erp.example:443/", "pathPrefixes": ["/odata/"], "rateLimit": {"perSecond": 5},
      "credential": {"kind": "headers", "headers": {"X-API-Key": {"secret": "erp-key"}}}},
    "pay": {"upstream": "https://api.pay.example", "mode": "intercept",
      "credential": {"kind": "oauth2", "tokenUrl": {"value": "https://auth.pay.example/token"},
        "clientId": "fecode", "clientSecret": {"secret": "pay-secret"}, "scope": {"value": "payments"}}}
  }
}`

// bindingsVC is defaultVC with the bindings' secrets, a destination rule
// that refuses internal.example, and HTTP bindings on or off.
type bindingsVC struct {
	fakeVC
	off bool
}

func (b bindingsVC) HTTPBindings() bool { return !b.off }
func (b bindingsVC) Destination(host string) error {
	if strings.HasSuffix(host, "internal.example") {
		return errors.New("host " + host + " resolves to 10.0.0.1, an internal address")
	}
	return nil
}

func newBindingsVC(mode Mode) bindingsVC {
	vc := defaultVC(mode)
	vc.secrets["erp-key"] = SecretInfo{Latest: 2, Versions: []int{1, 2}}
	vc.secrets["pay-secret"] = SecretInfo{Latest: 1, Versions: []int{1}}
	vc.secrets["cert"] = SecretInfo{Latest: 1, Versions: []int{1}}
	return bindingsVC{fakeVC: vc}
}

func validateBindings(t *testing.T, vc ValidateContext, change func(s *Spec)) []Finding {
	t.Helper()
	s := mustParse(t, bindingsSpec)
	if change != nil {
		change(&s)
	}
	return Validate(vc, s, nil)
}

func binding(s *Spec, name string, f func(*Binding)) {
	b := s.Bindings[name]
	f(&b)
	s.Bindings[name] = b
}

func lit(s string) *EnvValue    { return &EnvValue{Kind: EnvLiteral, Value: s} }
func secret(n string) *EnvValue { return &EnvValue{Kind: EnvSecret, Secret: n} }

func TestValidateBindings(t *testing.T) {
	checkFindings(t, validateBindings(t, newBindingsVC(ModeApply), nil), nil)

	for _, c := range []struct {
		name   string
		change func(s *Spec)
		want   []expect
	}{
		{"off", nil, nil},
		{"plain http", func(s *Spec) { binding(s, "erp", func(b *Binding) { b.Upstream = "http://api.erp.example" }) },
			[]expect{e("bindings.erp.upstream", "https")}},
		{"a path", func(s *Spec) { binding(s, "erp", func(b *Binding) { b.Upstream = "https://api.erp.example/odata" }) },
			[]expect{e("bindings.erp.upstream", "without a path")}},
		{"intercept port", func(s *Spec) {
			binding(s, "pay", func(b *Binding) { b.Upstream = "https://api.pay.example:8443" })
		}, []expect{e("bindings.pay.mode", "port 443")}},
		{"mode", func(s *Spec) { binding(s, "erp", func(b *Binding) { b.Mode = "tunnel" }) },
			[]expect{e("bindings.erp.mode", "unknown")}},
		{"internal upstream", func(s *Spec) {
			binding(s, "erp", func(b *Binding) { b.Upstream = "https://erp.internal.example" })
		}, []expect{e("bindings.erp.upstream", "internal address")}},
		{"internal token endpoint", func(s *Spec) {
			binding(s, "pay", func(b *Binding) { b.Credential.TokenURL = lit("https://auth.internal.example/token") })
		}, []expect{e("bindings.pay.credential.tokenUrl", "internal address")}},
		{"token url secret", func(s *Spec) { binding(s, "pay", func(b *Binding) { b.Credential.TokenURL = secret("erp-key") }) },
			[]expect{e("bindings.pay.credential.tokenUrl", "a literal")}},
		{"no secret", func(s *Spec) {
			binding(s, "erp", func(b *Binding) { b.Credential.Headers["X-API-Key"] = SecretEnv("missing") })
		}, []expect{e("bindings.erp.credential.headers.X-API-Key", "no secret missing")}},
		{"host header", func(s *Spec) {
			binding(s, "erp", func(b *Binding) { b.Credential.Headers = map[string]EnvValue{"Host": String("x")} })
		}, []expect{e("bindings.erp.credential.headers.Host", "cannot set")}},
		{"a field of another kind", func(s *Spec) { binding(s, "erp", func(b *Binding) { b.Credential.Password = lit("p") }) },
			[]expect{e("bindings.erp.credential.password", "has no password")}},
		{"no credential", func(s *Spec) { binding(s, "erp", func(b *Binding) { b.Credential = nil }) },
			[]expect{e("bindings.erp.credential", "needs a credential")}},
		{"basic", func(s *Spec) {
			binding(s, "erp", func(b *Binding) {
				b.Credential = &BindingCredential{Kind: "basic", User: lit("a:b")}
			})
		}, []expect{e("bindings.erp.credential.password", "needs password"), e("bindings.erp.credential.user", "':'")}},
		{"client certificate", func(s *Spec) {
			binding(s, "erp", func(b *Binding) {
				b.Credential = &BindingCredential{Kind: "clientCertificate", CertPem: lit("x"), KeyPem: lit("y")}
			})
		}, []expect{e("bindings.erp.credential", "credential")}},
		{"client certificate from secrets", func(s *Spec) {
			binding(s, "erp", func(b *Binding) {
				b.Credential = &BindingCredential{Kind: "clientCertificate", CertPem: secret("cert"), KeyPem: secret("cert")}
			})
		}, nil},
		{"bad prefix", func(s *Spec) { binding(s, "erp", func(b *Binding) { b.PathPrefixes = []string{"odata"} }) },
			[]expect{e("bindings.erp.pathPrefixes", "begin")}},
		{"bad ca", func(s *Spec) { binding(s, "erp", func(b *Binding) { b.CaPem = "nope" }) },
			[]expect{e("bindings.erp.caPem", "no PEM")}},
		{"named as a service", func(s *Spec) {
			s.Bindings["web"] = s.Bindings["erp"]
			svc(s, "worker", func(v *Service) { v.Bindings = append(v.Bindings, "web") })
		}, []expect{e("bindings.web", "a binding and a service are both named web")}},
		{"named as an addon", func(s *Spec) {
			s.Bindings["db"] = s.Bindings["erp"]
			svc(s, "worker", func(v *Service) { v.Bindings = append(v.Bindings, "db") })
		}, []expect{e("bindings.db", "a binding and an addon are both named db")}},
		{"one host intercepted twice", func(s *Spec) {
			s.Bindings["pay2"] = s.Bindings["pay"]
			svc(s, "worker", func(v *Service) { v.Bindings = append(v.Bindings, "pay2") })
		}, []expect{e("bindings.pay2", "both make the pods resolve api.pay.example")}},
		{"unknown binding", func(s *Spec) { svc(s, "web", func(v *Service) { v.Bindings = []string{"erp", "crm"} }) },
			[]expect{e("services.web.bindings[1]", "no binding crm")}},
		{"twice", func(s *Spec) { svc(s, "web", func(v *Service) { v.Bindings = []string{"erp", "erp"} }) },
			[]expect{e("services.web.bindings[1]", "named twice")}},
		{"a job's unknown binding", func(s *Spec) {
			j := s.Jobs["migrate"]
			j.Bindings = []string{"crm"}
			s.Jobs["migrate"] = j
		}, []expect{e("jobs.migrate.bindings[0]", "no binding crm")}},
		{"unnamed", func(s *Spec) { svc(s, "worker", func(v *Service) { v.Bindings = []string{"erp"} }) },
			[]expect{w("bindings.pay", "no service or job names binding pay")}},
		{"attached", func(s *Spec) {
			s.Services["dev"] = Service{Runtime: RuntimeAttached, Sandbox: webSandbox, Command: []string{"npm", "run", "dev"},
				Bindings: []string{"erp"}}
		}, []expect{e("services.dev.bindings", "attached services use their sandbox's bindings")}},
		{"forwardedHost intercepted", func(s *Spec) { binding(s, "pay", func(b *Binding) { b.ForwardedHost = true }) },
			[]expect{w("bindings.pay.forwardedHost", "no effect")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			vc := newBindingsVC(ModeApply)
			want := c.want
			if c.name == "off" {
				vc.off = true
				want = []expect{e("bindings", "not enabled")}
			}
			checkFindings(t, validateBindings(t, vc, c.change), want)
		})
	}

	// The names of a sandbox's own HTTP bindings are no names of an
	// environment there.
	vc := newBindingsVC(ModeApply)
	info := vc.sandboxes[webSandbox]
	info.HTTPNames = []string{"db"}
	vc.sandboxes[webSandbox] = info
	checkFindings(t, validateBindings(t, vc, func(s *Spec) {
		s.Services["dev"] = Service{Runtime: RuntimeAttached, Sandbox: webSandbox, Command: []string{"npm", "run", "dev"}}
	}), []expect{e("addons.db", "the name of an HTTP binding of sandbox")})
}

func TestNormaliseBindings(t *testing.T) {
	s := mustParse(t, bindingsSpec)
	norm, _ := Normalise(s, Pins{Secrets: map[string]int{"erp-key": 2, "pay-secret": 1}})
	erp := norm.Bindings["erp"]
	if erp.Upstream != "https://api.erp.example" || erp.Mode != "name" || erp.RateLimit.Burst != 5 {
		t.Fatalf("erp: %+v", erp)
	}
	if norm.Secrets["erp-key"].Version != 2 || norm.Secrets["pay-secret"].Version != 1 {
		t.Fatalf("secrets: %+v", norm.Secrets)
	}
	if got := norm.Services["worker"].Bindings; !slices.Equal(got, []string{"erp", "pay"}) {
		t.Fatalf("worker's bindings: %v", got)
	}
	if !slices.Equal(Referenced(s), []string{"erp-key", "pay-secret"}) {
		t.Fatalf("referenced: %v", Referenced(s))
	}
	// The normalised form reads back and normalises to itself.
	raw, err := json.Marshal(norm)
	if err != nil {
		t.Fatal(err)
	}
	again, fs := Parse(raw)
	if len(fs) > 0 {
		t.Fatalf("findings %+v", fs)
	}
	if n2, _ := Normalise(again, Pins{}); !EqualNormalised(n2, norm) {
		t.Fatalf("not idempotent:\n%s", raw)
	}
}

func TestDiffBindings(t *testing.T) {
	pins := Pins{Secrets: map[string]int{"erp-key": 1, "pay-secret": 1}}
	base, _ := Normalise(mustParse(t, bindingsSpec), pins)
	next := func(change func(s *Spec), secrets map[string]int) *Spec {
		s := mustParse(t, bindingsSpec)
		change(&s)
		p := pins
		if secrets != nil {
			p = Pins{Secrets: secrets}
		}
		n, _ := Normalise(s, p)
		return &n
	}
	byKind := func(d []DiffEntry) map[string]DiffEntry {
		out := map[string]DiffEntry{}
		for _, e := range d {
			out[e.Kind+"/"+e.Name] = e
		}
		return out
	}

	// A new version of a binding's secret: the binding and the secret in
	// place; no service or job changes.
	d := byKind(Diff(&base, next(func(*Spec) {}, map[string]int{"erp-key": 2, "pay-secret": 1})))
	if len(d) != 2 || d["binding/erp"].Effect != EffectInPlace || d["secret/erp-key"].Effect != EffectInPlace {
		t.Fatalf("a new secret version: %+v", d)
	}
	f := d["binding/erp"].Fields
	if len(f) != 1 || f[0].Path != "credential.headers.X-API-Key" {
		t.Fatalf("the binding's fields: %+v", f)
	}
	if raw, _ := json.Marshal(f[0]); !strings.Contains(string(raw), `"version":2`) {
		t.Fatalf("the field shows no version: %s", raw)
	}

	// Its options: in place.
	d = byKind(Diff(&base, next(func(s *Spec) {
		binding(s, "erp", func(b *Binding) { b.PathPrefixes = []string{"/odata/v4/"} })
	}, nil)))
	if len(d) != 1 || d["binding/erp"].Effect != EffectInPlace {
		t.Fatalf("new path prefixes: %+v", d)
	}

	// Its upstream: the pods of the services that name it are replaced,
	// the job runs again.
	d = byKind(Diff(&base, next(func(s *Spec) {
		binding(s, "erp", func(b *Binding) { b.Upstream = "https://api2.erp.example" })
	}, nil)))
	if d["binding/erp"].Effect != EffectReplaces(3) || d["service/web"].Effect != EffectReplaces(2) ||
		d["service/worker"].Effect != EffectReplaces(1) || d["job/migrate"].Effect != EffectRestarts {
		t.Fatalf("a new upstream: %+v", d)
	}
	if fs := d["service/web"].Fields; len(fs) != 1 || fs[0].Path != "bindings" {
		t.Fatalf("web's fields: %+v", fs)
	}

	// A service that names one more binding is replaced.
	d = byKind(Diff(&base, next(func(s *Spec) { svc(s, "web", func(v *Service) { v.Bindings = []string{"erp", "pay"} }) }, nil)))
	if len(d) != 1 || d["service/web"].Effect != EffectReplaces(2) {
		t.Fatalf("one more binding: %+v", d)
	}

	// Without bindings, a spec's template hashes as before.
	plain := Spec{Services: map[string]Service{"web": {Image: "x"}}}
	if d := Diff(&plain, &plain); len(d) != 0 {
		t.Fatalf("no change: %+v", d)
	}
}
