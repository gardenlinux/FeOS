// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"strings"
	"testing"
)

// fhVC is httpVC whose HTTP secret s4 has a third version, in the name
// mode with the host forwarded, which is its latest.
type fhVC struct{ httpVC }

func (c fhVC) Credential(name string) (CredentialInfo, bool) {
	info, ok := c.httpVC.Credential(name)
	if !ok {
		return info, false
	}
	info.Versions[3] = CredentialTemplate{Upstream: "https://api.s4.example", Mode: SecretModeName, ForwardedHost: true}
	info.Latest = 3
	return info, true
}

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

// A spec states what its code expects of a secret (ADR 0058): the plan
// compares it with the version it would pin, and says each difference
// at the secret's field, with both ways out.
func TestSecretExpectations(t *testing.T) {
	vc := newHTTPVC(ModeApply)
	for name, c := range map[string]struct {
		vc     ValidateContext
		pins   map[string]SecretRef
		want   []expect
		remedy []string
	}{
		"as the latest is": {vc, map[string]SecretRef{"s4": {Type: SecretTypeHTTP, Mode: SecretModeIntercept},
			"token": {Type: SecretTypeValue}}, nil, nil},
		"as version 1 is": {vc, map[string]SecretRef{"s4": {Version: 1, Mode: SecretModeName, ForwardedHost: no()}},
			nil, nil},
		"another mode than the latest": {vc, map[string]SecretRef{"s4": {Type: SecretTypeHTTP, Mode: SecretModeName}},
			[]expect{e("secrets.s4.mode", "HTTP secret s4 (version 2) is called in the intercept mode; the spec's "+
				"code expects the name mode")},
			[]string{"make a new version of s4 in the name mode", "leave the pin's version out",
				"or change the spec's mode to intercept"}},
		"another mode than the pinned": {vc, map[string]SecretRef{"s4": {Version: 1, Mode: SecretModeIntercept}},
			[]expect{e("secrets.s4.mode", "(version 1) is called in the name mode")},
			[]string{"and pin that version", "or change the spec's mode to name"}},
		"the host forwarded otherwise": {fhVC{vc}, map[string]SecretRef{"s4": {Mode: SecretModeName, ForwardedHost: no()}},
			[]expect{e("secrets.s4.forwardedHost", "HTTP secret s4 (version 3) forwards the host; the spec's code "+
				"expects one that does not forward the host")},
			[]string{`"forwardedHost": false`, "its key stays", "or change the spec's forwardedHost to true"}},
		"the host expected forwarded": {vc, map[string]SecretRef{"s4": {Version: 1, ForwardedHost: yes()}},
			[]expect{e("secrets.s4.forwardedHost", "(version 1) does not forward the host")}, nil},
		"a forwarded host of an intercepting version": {vc, map[string]SecretRef{"s4": {ForwardedHost: yes()}},
			[]expect{e("secrets.s4.forwardedHost", "is called in the intercept mode; forwardedHost is the name mode's")},
			[]string{`"mode": "name"`, "or leave forwardedHost out"}},
		"a forwarded host in the intercept mode": {vc, map[string]SecretRef{"s4": {Mode: SecretModeIntercept,
			ForwardedHost: no()}}, []expect{e("secrets.s4.forwardedHost", "forwardedHost is the name mode's")}, nil},
		"an unknown type": {vc, map[string]SecretRef{"s4": {Type: "file"}},
			[]expect{e("secrets.s4.type", `type "file" is no secret's kind`)}, nil},
		"an unknown mode": {vc, map[string]SecretRef{"s4": {Mode: "env"}},
			[]expect{e("secrets.s4.mode", `mode "env" is no HTTP secret's mode`)}, nil},
		"a value with a mode": {vc, map[string]SecretRef{"token": {Type: SecretTypeValue, Mode: SecretModeName}},
			[]expect{e("secrets.token", "expected as a value, which has no upstream, mode or forwarded host")}, nil},
		"a value with a forwarded host": {vc, map[string]SecretRef{"token": {ForwardedHost: yes()}},
			[]expect{e("secrets.token", "is no HTTP secret's pin")}, nil},
		"a value referenced by a key": {vc, map[string]SecretRef{"s4": {Type: SecretTypeValue}},
			[]expect{e("secrets.s4.type", "the spec expects a value secret and a variable references s4 by its url "+
				"or host")}, []string{`reference it as {"secret": "s4"}, or change type to http`}},
		"an HTTP secret referenced as a value": {vc, map[string]SecretRef{"token": {Type: SecretTypeHTTP}},
			[]expect{e("secrets.token.type", "the spec expects an HTTP secret token and a variable references it as "+
				"a value")}, []string{`{"secret": "token", "key": "url"}`, "or change type to value"}},
	} {
		t.Run(name, func(t *testing.T) {
			s := mustParse(t, httpSpec)
			s.Secrets = c.pins
			fs := Validate(c.vc, s, nil)
			checkFindings(t, fs, c.want)
			for _, r := range c.remedy {
				if !strings.Contains(fs[0].Remedy, r) {
					t.Fatalf("remedy %q, want %q in it", fs[0].Remedy, r)
				}
			}
		})
	}
}

// The kind the code expects against the environment's secret of the
// name: the finding is the secret's, once, not the references'.
func TestSecretKindExpected(t *testing.T) {
	vc := newHTTPVC(ModeApply)
	// token is a value of the environment; the spec expects it as HTTP.
	s := mustParse(t, httpSpec)
	svc(&s, "web", func(v *Service) {
		v.Env["T1"] = HTTPSecretEnv("token", SecretKeyURL)
		v.Env["T2"] = HTTPSecretEnv("token", SecretKeyHost)
		delete(v.Env, "TOKEN")
	})
	s.Secrets = map[string]SecretRef{"token": {Type: SecretTypeHTTP}}
	fs := Validate(vc, s, nil)
	checkFindings(t, fs, []expect{e("secrets.token.type", "secret token of the environment is a value; the spec's "+
		"code expects an HTTP secret")})
	if !strings.Contains(fs[0].Remedy, "the kind does not change") || !strings.Contains(fs[0].Remedy, "type value") {
		t.Fatalf("remedy %q", fs[0].Remedy)
	}
	// Without the expectation it is the references' finding, as before.
	s.Secrets = nil
	checkFindings(t, Validate(vc, s, nil), []expect{e("services.web.env.T1", "secret token is a value"),
		e("services.web.env.T2", "secret token is a value")})

	// s4 is an HTTP secret of the environment; the spec expects a value.
	s = mustParse(t, httpSpec)
	svc(&s, "web", func(v *Service) { v.Env["S4_URL"] = SecretEnv("s4") })
	s.Jobs = nil
	s.Secrets = map[string]SecretRef{"s4": {Type: SecretTypeValue}}
	fs = Validate(vc, s, nil)
	checkFindings(t, fs, []expect{e("secrets.s4.type", "secret s4 of the environment is an HTTP secret; the spec's "+
		"code expects a value")})
	s.Secrets = nil
	checkFindings(t, Validate(vc, s, nil), []expect{e("services.web.env.S4_URL", "its key never reaches a container")})
}

// A missing HTTP secret's finding says what to make: the kind, the mode
// and the forwarded host the spec expects.
func TestMissingSecretSaysWhatToMake(t *testing.T) {
	vc := newHTTPVC(ModeApply)
	const put = `PUT /api/v1/environments/{env}/secrets/s9 {"type": "http", `
	for pin, want := range map[*SecretRef]string{
		nil:                         put + `"mode": "name", "upstream": …`,
		{Mode: SecretModeIntercept}: put + `"mode": "intercept", "upstream": …`,
		{Type: SecretTypeHTTP, ForwardedHost: yes()}:      put + `"mode": "name", "forwardedHost": true, "upstream": …`,
		{Mode: SecretModeName, ForwardedHost: no()}:       put + `"mode": "name", "forwardedHost": false, "upstream": …`,
		{Type: SecretTypeHTTP, Mode: SecretModeName}:      put + `"mode": "name", "upstream": …`,
		{Type: SecretTypeHTTP, Mode: SecretModeIntercept}: put + `"mode": "intercept", "upstream": …`,
	} {
		s := mustParse(t, httpSpec)
		svc(&s, "web", func(v *Service) { v.Env["S4_URL"] = HTTPSecretEnv("s9", SecretKeyURL) })
		s.Jobs = nil
		if pin != nil {
			s.Secrets = map[string]SecretRef{"s9": *pin}
		}
		fs := Validate(vc, s, nil)
		checkFindings(t, fs, []expect{e("services.web.env.S4_URL", "no HTTP secret s9")})
		if !strings.Contains(fs[0].Remedy, want) {
			t.Fatalf("%+v: remedy %q", pin, fs[0].Remedy)
		}
	}
}

// A normalised spec keeps what its code expects, and adds nothing the
// spec did not say: a stored deployment normalises to itself.
func TestNormalisedExpectations(t *testing.T) {
	vc := newHTTPVC(ModeApply)
	s := mustParse(t, httpSpec)
	s.Secrets = map[string]SecretRef{"s4": {Version: 1, Mode: SecretModeName, ForwardedHost: no()},
		"token": {Type: SecretTypeValue}}
	checkFindings(t, Validate(vc, s, nil), nil)
	norm, _ := Normalise(s, httpPins(vc))
	if got := norm.Secrets["s4"]; got.Version != 1 || got.Type != SecretTypeHTTP || got.Upstream != "https://api.s4.example" ||
		got.Mode != SecretModeName || got.ForwardedHost == nil || *got.ForwardedHost {
		t.Fatalf("the HTTP pin %+v", got)
	}
	if got := norm.Secrets["token"]; got != (SecretRef{Version: 1, Type: SecretTypeValue}) {
		t.Fatalf("the value pin %+v", got)
	}
	checkFindings(t, Validate(vc, norm, nil), nil)
	raw, err := json.Marshal(norm.Secrets)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"s4":{"version":1,"type":"http","upstream":"https://api.s4.example","mode":"name","forwardedHost":false},` +
		`"token":{"version":1,"type":"value"}}`; string(raw) != want {
		t.Fatalf("pins %s", raw)
	}

	// Without an expectation, a pin is normalised as before ADR 0058: no
	// forwardedHost, no type of a value, so that what was stored equals
	// what a plan of the same spec makes.
	s.Secrets = nil
	plain, _ := Normalise(s, httpPins(vc))
	stored := `{"version":2,"type":"http","upstream":"https://api.s4.example","mode":"intercept"}`
	if raw, _ := json.Marshal(plain.Secrets["s4"]); string(raw) != stored {
		t.Fatalf("plain pin %s", raw)
	}
	if raw, _ := json.Marshal(plain.Secrets["token"]); string(raw) != `{"version":1}` {
		t.Fatalf("plain value pin %s", raw)
	}
	var old Spec
	b, _ := json.Marshal(plain)
	old, fs := Parse(b)
	if len(fs) > 0 {
		t.Fatal(fs)
	}
	again, _ := Normalise(old, httpPins(vc))
	if !EqualNormalised(again, plain) {
		t.Fatalf("a stored spec does not normalise to itself:\n%s", b)
	}

	// A plan from another environment carries the expectation and leaves
	// the version and the upstream behind; normalised there, the pin is
	// that environment's.
	carried := Portable(norm)
	if got := carried.Secrets["s4"]; got.Version != 0 || got.Upstream != "" || got.Mode != SecretModeName ||
		got.Type != SecretTypeHTTP || got.ForwardedHost == nil {
		t.Fatalf("carried %+v", got)
	}
	checkFindings(t, Validate(vc, carried, nil), []expect{e("secrets.s4.mode", "(version 2) is called in the "+
		"intercept mode; the spec's code expects the name mode")})
}
