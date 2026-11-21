// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"testing"
)

// Carried to another place, a spec keeps what its code expects of each
// secret and leaves the place's versions and upstreams behind; the spec
// it was made from is not changed.
func TestPortable(t *testing.T) {
	const raw = `{
  "services": {"web": {"image": "a", "env": {"S4": {"secret": "s4", "key": "url"}, "T": {"secret": "token"}}}},
  "bindings": {
    "erp": {"upstream": "https://erp.example", "mode": "name", "credentialRef": {"name": "erp", "version": 3}},
    "plain": {"upstream": "https://plain.example", "credential": {"kind": "headers", "headers": {"K": "v"}}}
  },
  "secrets": {
    "s4": {"version": 2, "type": "http", "upstream": "https://api.s4.example", "mode": "name", "forwardedHost": true},
    "token": {"version": 1, "type": "value"},
    "old": {"version": 4}
  }
}`
	s, fs := Parse([]byte(raw))
	if len(fs) > 0 {
		t.Fatal(fs)
	}
	p := Portable(s)
	got, _ := json.Marshal(struct {
		Secrets  map[string]SecretRef `json:"secrets"`
		Bindings map[string]Binding   `json:"bindings"`
	}{p.Secrets, p.Bindings})
	want := `{"secrets":{"old":{},"s4":{"type":"http","mode":"name","forwardedHost":true},"token":{"type":"value"}},` +
		`"bindings":{"erp":{"upstream":"","credentialRef":{"name":"erp"}},"plain":{"upstream":"https://plain.example",` +
		`"credential":{"kind":"headers","headers":{"K":"v"}}}}}`
	if string(got) != want {
		t.Fatalf("portable\n%s\nwant\n%s", got, want)
	}
	if s.Secrets["s4"].Version != 2 || s.Bindings["erp"].CredentialRef.Version != 3 || s.Bindings["erp"].Upstream == "" {
		t.Fatalf("the original changed: %+v %+v", s.Secrets, s.Bindings)
	}
	if p.Services["web"].Image != "a" {
		t.Fatalf("services %+v", p.Services)
	}
	if e := Portable(Spec{}); e.Secrets != nil || e.Bindings != nil {
		t.Fatalf("empty %+v", e)
	}
}
