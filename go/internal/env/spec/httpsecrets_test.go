// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// An HTTP secret's reference reads and writes as {"secret", "key"}; a
// value's stays {"secret"}.
func TestHTTPSecretEnvValue(t *testing.T) {
	for raw, want := range map[string]EnvValue{
		`{"secret":"s4","key":"url"}`:  HTTPSecretEnv("s4", SecretKeyURL),
		`{"secret":"s4","key":"host"}`: HTTPSecretEnv("s4", SecretKeyHost),
		`{"secret":"token"}`:           SecretEnv("token"),
	} {
		var v EnvValue
		if err := json.Unmarshal([]byte(raw), &v); err != nil || !reflect.DeepEqual(v, want) {
			t.Fatalf("%s read as %+v %v", raw, v, err)
		}
		b, err := json.Marshal(v)
		if err != nil || string(b) != raw {
			t.Fatalf("%+v written as %s %v", v, b, err)
		}
	}
	for _, raw := range []string{`{"secret":"s4","port":1}`, `{"secret":"s4","key":"url","value":"x"}`,
		`{"secret":"s4","key":1}`} {
		var v EnvValue
		if err := json.Unmarshal([]byte(raw), &v); err == nil {
			t.Errorf("%s read as %+v", raw, v)
		}
	}
}

// httpVC is the bindings' context with the environment's HTTP secret s4
// in two versions, the second intercepting, and the value secret token.
type httpVC struct{ bindingsVC }

func (c httpVC) Credential(name string) (CredentialInfo, bool) {
	if name != "s4" {
		return CredentialInfo{}, false
	}
	return CredentialInfo{Latest: 2, Versions: map[int]CredentialTemplate{
		1: {Upstream: "https://api.s4.example", Mode: "name"},
		2: {Upstream: "https://api.s4.example", Mode: "intercept"},
	}}, true
}

func newHTTPVC(mode Mode) httpVC {
	vc := httpVC{newBindingsVC(mode)}
	vc.secrets["token"] = SecretInfo{Latest: 1, Versions: []int{1}}
	return vc
}

const httpSpec = `{
  "services": {"web": {"image": "ghcr.io/acme/web:1", "replicas": 2,
    "env": {"S4_URL": {"secret": "s4", "key": "url"}, "TOKEN": {"secret": "token"}}}},
  "jobs": {"sync": {"image": "ghcr.io/acme/web:1", "env": {"S4_HOST": {"secret": "s4", "key": "host"}}}}
}`

func httpPins(vc httpVC) Pins {
	info, _ := vc.Credential("s4")
	return Pins{Secrets: map[string]int{"token": 1}, Credentials: map[string]CredentialInfo{"s4": info}}
}

// A variable's HTTP secret is pinned with its version's upstream and
// mode; the normalised spec validates again and normalises to itself.
func TestHTTPSecretReference(t *testing.T) {
	vc := newHTTPVC(ModeApply)
	s := mustParse(t, httpSpec)
	checkFindings(t, Validate(vc, s, nil), nil)
	norm, _ := Normalise(s, httpPins(vc))
	want := SecretRef{Version: 2, Type: SecretTypeHTTP, Upstream: "https://api.s4.example", Mode: "intercept"}
	if got := norm.Secrets["s4"]; got != want {
		t.Fatalf("the pin: %+v", got)
	}
	if got := norm.Secrets["token"]; got != (SecretRef{Version: 1}) {
		t.Fatalf("a value's pin: %+v", got)
	}
	checkFindings(t, Validate(vc, norm, nil), nil)
	raw, err := json.Marshal(norm)
	if err != nil {
		t.Fatal(err)
	}
	again, fs := Parse(raw)
	if len(fs) > 0 {
		t.Fatalf("parse %+v", fs)
	}
	if n2, _ := Normalise(again, Pins{}); !EqualNormalised(n2, norm) {
		t.Fatalf("not idempotent:\n%s", raw)
	}
	if got := ReferencedHTTP(s); len(got) != 1 || got[0] != "s4" {
		t.Fatalf("ReferencedHTTP %v", got)
	}
	if got := Referenced(s); len(got) != 1 || got[0] != "token" {
		t.Fatalf("Referenced %v", got)
	}

	// Pinned to version 1, its name mode.
	s.Secrets = map[string]SecretRef{"s4": {Version: 1}}
	if n1, _ := Normalise(s, httpPins(vc)); n1.Secrets["s4"].Mode != "name" {
		t.Fatalf("version 1: %+v", n1.Secrets["s4"])
	}
}

func TestHTTPSecretReferenceFindings(t *testing.T) {
	vc := newHTTPVC(ModeApply)
	for name, c := range map[string]struct {
		change func(s *Spec)
		want   []expect
	}{
		"its key as a value": {func(s *Spec) { svc(s, "web", func(v *Service) { v.Env["S4_URL"] = SecretEnv("s4") }) },
			[]expect{e("services.web.env.S4_URL", "its key never reaches a container")}},
		"a key on a value": {func(s *Spec) {
			svc(s, "web", func(v *Service) { v.Env["TOKEN"] = HTTPSecretEnv("token", SecretKeyURL) })
		}, []expect{e("services.web.env.TOKEN", "secret token is a value")}},
		"an unknown key": {func(s *Spec) {
			svc(s, "web", func(v *Service) { v.Env["S4_URL"] = HTTPSecretEnv("s4", "token") })
		}, []expect{e("services.web.env.S4_URL", "key url or host")}},
		"none of the name": {func(s *Spec) {
			svc(s, "web", func(v *Service) { v.Env["S4_URL"] = HTTPSecretEnv("s5", SecretKeyURL) })
		}, []expect{e("services.web.env.S4_URL", "no HTTP secret s5")}},
		"a version it lacks": {func(s *Spec) { s.Secrets = map[string]SecretRef{"s4": {Version: 7}} },
			[]expect{e("secrets.s4.version", "has no version 7")}},
		"another upstream pinned": {func(s *Spec) {
			s.Secrets = map[string]SecretRef{"s4": {Version: 1, Type: SecretTypeHTTP, Upstream: "https://evil.example",
				Mode: "name"}}
		}, []expect{e("secrets.s4", "calls https://api.s4.example")}},
		"an image's login": {func(s *Spec) {
			svc(s, "web", func(v *Service) {
				v.ImageCredentials = &ImageCredentials{Username: String("u"), Password: SecretEnv("s4")}
			})
		}, []expect{e("services.web.imageCredentials.password", "its key never reaches a container")}},
		"a binding of its name": {func(s *Spec) {
			s.Bindings = map[string]Binding{"s4": {Upstream: "https://other.example",
				Credential: &BindingCredential{Kind: "headers", Headers: map[string]EnvValue{"K": SecretEnv("token")}}}}
			svc(s, "web", func(v *Service) { v.Bindings = []string{"s4"} })
		}, []expect{e("bindings.s4", "would both be the pods' binding s4")}},
		"the service of its name in the name mode": {func(s *Spec) {
			s.Secrets = map[string]SecretRef{"s4": {Version: 1}}
			s.Services["s4"] = Service{Image: "ghcr.io/acme/web:1"}
		}, []expect{e("services.web.env.S4_URL", "a service's name")}},
		"named as a binding": {func(s *Spec) { svc(s, "web", func(v *Service) { v.Bindings = []string{"s4"} }) },
			[]expect{e("services.web.bindings[0]", "s4 is an HTTP secret")}},
	} {
		t.Run(name, func(t *testing.T) {
			s := mustParse(t, httpSpec)
			c.change(&s)
			checkFindings(t, Validate(vc, s, nil), c.want)
		})
	}

	// Without an environment there is nothing to look up: a warning;
	// without the HTTP bindings an error.
	checkFindings(t, Validate(newHTTPVC(ModeSpec), mustParse(t, httpSpec), nil), []expect{
		w("jobs.sync.env.S4_HOST", "no HTTP secret s4 to look up"), w("services.web.env.S4_URL", "no HTTP secret s4 to look up")})
	off := newHTTPVC(ModeApply)
	off.off = true
	checkFindings(t, Validate(off, mustParse(t, httpSpec), nil), []expect{
		e("jobs.sync.env.S4_HOST", "not enabled"), e("services.web.env.S4_URL", "not enabled")})
}

// A new version of a variable's HTTP secret is a pin's change, in place
// for the objects that reference it; another mode replaces their pods.
func TestHTTPSecretDiff(t *testing.T) {
	vc := newHTTPVC(ModeApply)
	at := func(version int) *Spec {
		s := mustParse(t, httpSpec)
		s.Secrets = map[string]SecretRef{"s4": {Version: version}}
		n, _ := Normalise(s, httpPins(vc))
		return &n
	}
	one := at(1)
	moved := *one
	moved.Secrets = map[string]SecretRef{"s4": {Version: 3, Type: SecretTypeHTTP, Upstream: "https://api.s4.example",
		Mode: "name"}, "token": {Version: 1}}
	effects := map[string]string{}
	for _, d := range Diff(one, &moved) {
		effects[d.Kind+"/"+d.Name] = d.Effect
	}
	if effects["secret/s4"] != EffectInPlace || effects["service/web"] != EffectInPlace || effects["job/sync"] != EffectInPlace {
		t.Fatalf("a new version: %+v", Diff(one, &moved))
	}
	effects = map[string]string{}
	for _, d := range Diff(one, at(2)) {
		effects[d.Kind+"/"+d.Name] = d.Effect
	}
	if effects["service/web"] != EffectReplaces(2) || effects["job/sync"] != EffectRestarts {
		t.Fatalf("another mode: %+v", Diff(one, at(2)))
	}
}

// refusingVC is the bindings' context of a plan that refuses bindings
// stated in the spec (ADR 0055): a new plan's.
type refusingVC struct{ bindingsVC }

func (refusingVC) RefusesStatedBindings() bool { return true }

// A plan that refuses bindings stated in the spec refuses each, inline or
// by credentialRef, but one the current deployment has unchanged: a spec
// changed elsewhere keeps its bindings, a binding changed or added is
// refused.
func TestBindingsRefused(t *testing.T) {
	vc := refusingVC{newBindingsVC(ModeApply)}
	errorsAt := func(fs []Finding) map[string]bool {
		out := map[string]bool{}
		for _, f := range fs {
			if f.Problem == GivesWay && f.Severity == SeverityError {
				out[f.Path] = true
			}
		}
		return out
	}
	s := mustParse(t, bindingsSpec)
	if got := errorsAt(Validate(vc, s, nil)); !got["bindings.erp"] || !got["bindings.pay"] || len(got) != 2 {
		t.Fatalf("a new plan: %v", got)
	}
	prev, _ := Normalise(s, Pins{Secrets: map[string]int{"erp-key": 2, "pay-secret": 1}})
	again := mustParse(t, bindingsSpec)
	svc(&again, "web", func(v *Service) { v.Replicas = intp(3) })
	if got := errorsAt(Validate(vc, again, &prev)); len(got) != 0 {
		t.Fatalf("the current deployment's bindings again: %v", got)
	}
	binding(&again, "erp", func(b *Binding) { b.PathPrefixes = []string{"/other/"} })
	if got := errorsAt(Validate(vc, again, &prev)); !got["bindings.erp"] || len(got) != 1 {
		t.Fatalf("a changed binding: %v", got)
	}
	// Without the context's word, accepted with the warning.
	if got := errorsAt(Validate(newBindingsVC(ModeApply), s, nil)); len(got) != 0 {
		t.Fatalf("a context that accepts them: %v", got)
	}
}

// Every binding stated in the spec, inline or by credentialRef, is
// accepted with a warning by a context that does not refuse them (a
// stored deployment's plan).
func TestBindingsGiveWay(t *testing.T) {
	fs := Validate(newBindingsVC(ModeApply), mustParse(t, bindingsSpec), nil)
	got := map[string]bool{}
	for _, f := range fs {
		if f.Problem == GivesWay {
			if f.Severity != SeverityWarning || !strings.Contains(f.Remedy, `{"secret": "`) {
				t.Fatalf("the finding %+v", f)
			}
			got[f.Path] = true
		}
	}
	if !got["bindings.erp"] || !got["bindings.pay"] || len(got) != 2 {
		t.Fatalf("the warnings: %+v", fs)
	}
}
