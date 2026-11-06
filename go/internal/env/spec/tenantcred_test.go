// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"strings"
	"testing"
)

// credVC is the bindings' context with the tenant's credential github in
// two versions, the second intercepting.
type credVC struct{ bindingsVC }

func (c credVC) TenantCredential(name string) (CredentialInfo, bool) {
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
  "bindings": {"gh": {"tenantCredential": {"name": "github"}}}
}`

func TestTenantCredentialBinding(t *testing.T) {
	vc := credVC{newBindingsVC(ModeApply)}
	s := mustParse(t, credSpec)
	if fs := Validate(vc, s, nil); HasErrors(fs) {
		t.Fatalf("findings %+v", fs)
	}
	creds := map[string]CredentialInfo{}
	creds["github"], _ = vc.TenantCredential("github")
	norm, _ := Normalise(s, Pins{Credentials: creds})
	gh := norm.Bindings["gh"]
	if gh.TenantCredential == nil || gh.TenantCredential.Version != 2 || gh.Upstream != "https://api.github.com" ||
		gh.Mode != "intercept" {
		t.Fatalf("normalised: %+v %+v", gh, gh.TenantCredential)
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

func TestTenantCredentialBindingFindings(t *testing.T) {
	vc := credVC{newBindingsVC(ModeApply)}
	cases := map[string]struct {
		spec, path string
	}{
		"unknown":      {strings.Replace(credSpec, `"github"`, `"gitlab"`, 1), "bindings.gh.tenantCredential.name"},
		"no version":   {strings.Replace(credSpec, `{"name": "github"}`, `{"name": "github", "version": 7}`, 1), "bindings.gh.tenantCredential.version"},
		"own fields":   {strings.Replace(credSpec, `{"tenantCredential"`, `{"pathPrefixes": ["/x/"], "tenantCredential"`, 1), "bindings.gh.pathPrefixes"},
		"other origin": {strings.Replace(credSpec, `{"tenantCredential"`, `{"upstream": "https://example.org", "tenantCredential"`, 1), "bindings.gh.upstream"},
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
	// Without the tenant's credentials (a context that has none) it is an
	// error, not a crash.
	if fs := Validate(newBindingsVC(ModeApply), mustParse(t, credSpec), nil); !HasErrors(fs) {
		t.Fatal("a context without credentials took the binding")
	}
}
