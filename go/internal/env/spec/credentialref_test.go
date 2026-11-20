// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"strings"
	"testing"
)

// credVC is the bindings' context with the environment's credential
// github in two versions, the second intercepting.
type credVC struct{ bindingsVC }

func (c credVC) Credential(name string) (CredentialInfo, bool) {
	if name != "github" {
		return CredentialInfo{}, false
	}
	return CredentialInfo{Latest: 2, Versions: map[int]CredentialTemplate{
		1: {Upstream: "https://api.github.com", Mode: "name"},
		2: {Upstream: "https://api.github.com", Mode: "intercept"},
	}}, true
}

const credSpec = `{
  "services": {"web": {"image": "ghcr.io/acme/web:1", "bindings": ["gh"]}},
  "bindings": {"gh": {"credentialRef": {"name": "github"}}}
}`

func TestCredentialRefBinding(t *testing.T) {
	vc := credVC{newBindingsVC(ModeApply)}
	s := mustParse(t, credSpec)
	if fs := Validate(vc, s, nil); HasErrors(fs) {
		t.Fatalf("findings %+v", fs)
	}
	creds := map[string]CredentialInfo{}
	creds["github"], _ = vc.Credential("github")
	norm, _ := Normalise(s, Pins{Credentials: creds})
	gh := norm.Bindings["gh"]
	if gh.CredentialRef == nil || gh.CredentialRef.Version != 2 || gh.Upstream != "https://api.github.com" ||
		gh.Mode != "intercept" {
		t.Fatalf("normalised: %+v %+v", gh, gh.CredentialRef)
	}
	// The normalised spec validates again, reads back and normalises to
	// itself without the pins.
	if fs := Validate(vc, norm, nil); HasErrors(fs) {
		t.Fatalf("normalised findings %+v", fs)
	}
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
	// A pin to version 1 keeps the name mode.
	s1 := mustParse(t, strings.Replace(credSpec, `{"name": "github"}`, `{"name": "github", "version": 1}`, 1))
	if n1, _ := Normalise(s1, Pins{Credentials: creds}); n1.Bindings["gh"].Mode != "name" {
		t.Fatalf("version 1: %+v", n1.Bindings["gh"])
	}
}

func TestCredentialRefBindingFindings(t *testing.T) {
	vc := credVC{newBindingsVC(ModeApply)}
	cases := map[string]struct {
		spec, path string
	}{
		"unknown":      {strings.Replace(credSpec, `"github"`, `"gitlab"`, 1), "bindings.gh.credentialRef.name"},
		"no version":   {strings.Replace(credSpec, `{"name": "github"}`, `{"name": "github", "version": 7}`, 1), "bindings.gh.credentialRef.version"},
		"own fields":   {strings.Replace(credSpec, `{"credentialRef"`, `{"pathPrefixes": ["/x/"], "credentialRef"`, 1), "bindings.gh.pathPrefixes"},
		"other origin": {strings.Replace(credSpec, `{"credentialRef"`, `{"upstream": "https://example.org", "credentialRef"`, 1), "bindings.gh.upstream"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fs := Validate(vc, mustParse(t, c.spec), nil)
			for _, f := range fs {
				if f.Severity == SeverityError && f.Path == c.path {
					return
				}
			}
			t.Fatalf("no error at %s: %+v", c.path, fs)
		})
	}
	// Without the environment's credentials (a spec without an
	// environment) it is the same error at the name, not a crash.
	fs := Validate(newBindingsVC(ModeApply), mustParse(t, credSpec), nil)
	found := false
	for _, f := range fs {
		found = found || (f.Severity == SeverityError && f.Path == "bindings.gh.credentialRef.name" &&
			strings.Contains(f.Problem, "the environment has no credential github"))
	}
	if !found {
		t.Fatalf("a context without credentials: %+v", fs)
	}
}
