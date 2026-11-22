// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestReachForms(t *testing.T) {
	for _, tc := range []struct {
		in, mode string
		restrict bool
		domains  []string
	}{
		{`"internet"`, EgressInternet, false, nil},
		{`"none"`, EgressNone, true, nil},
		{`{}`, EgressNone, true, nil},
		{`{"domains": ["b.example.com", "a.example.com", "a.example.com"]}`, EgressRestricted, true,
			[]string{"a.example.com", "b.example.com"}},
		{`{"blocks": [{"cidr": "203.0.113.0/24", "except": ["203.0.113.8/29"]}]}`, EgressRestricted, true, nil},
	} {
		var e Egress
		if err := json.Unmarshal([]byte(tc.in), &e); err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if e.EffectiveMode() != tc.mode || e.RestrictsNames() != tc.restrict || !slices.Equal(e.NameDomains(), tc.domains) {
			t.Errorf("%s: %s %v %v", tc.in, e.EffectiveMode(), e.RestrictsNames(), e.NameDomains())
		}
		b, _ := json.Marshal(e)
		var again Egress
		if err := json.Unmarshal(b, &again); err != nil || again.EffectiveMode() != tc.mode {
			t.Errorf("%s round trip %s %v", tc.in, b, err)
		}
	}
	var nilEgress *Egress
	if nilEgress.EffectiveMode() != EgressInternet || nilEgress.RestrictsNames() {
		t.Error("an absent egress is not the internet")
	}
	for _, bad := range []string{`"open"`, `1`, `[]`, `{"domain": []}`, `{"domains": "x"}`} {
		var e Egress
		if err := json.Unmarshal([]byte(bad), &e); err == nil {
			t.Errorf("%s read as %+v", bad, e)
		}
	}
	var r Reaches
	if err := json.Unmarshal([]byte(`"all"`), &r); err != nil || !r.All {
		t.Errorf("all %+v %v", r, err)
	}
	if err := json.Unmarshal([]byte(`[{"service": "api", "port": "http"}, {"addon": "db"}, {"service": "x", "port": 80}]`),
		&r); err != nil || r.All || len(r.Entries) != 3 || r.Entries[2].Port.Number != 80 {
		t.Errorf("entries %+v %v", r, err)
	}
	for _, bad := range []string{`"some"`, `{}`, `[{"svc": "x"}]`, `3`} {
		var r Reaches
		if err := json.Unmarshal([]byte(bad), &r); err == nil {
			t.Errorf("%s read as %+v", bad, r)
		}
	}
	var body struct {
		Reaches Reaches `json:"reaches"`
		Egress  *Egress `json:"egress"`
	}
	if err := json.Unmarshal([]byte(`{"reaches": null, "egress": null}`), &body); err != nil || body.Reaches.All ||
		body.Egress != nil {
		t.Errorf("null %+v %v", body, err)
	}
	if b, _ := json.Marshal(Reaches{}); string(b) != `[]` {
		t.Errorf("nothing %s", b)
	}
	if b, _ := json.Marshal(Reaches{All: true}); string(b) != `"all"` {
		t.Errorf("all %s", b)
	}
	// In a spec, a wrong form is a finding at its path.
	_, fs := Parse([]byte(`{"services": {"a": {"image": "a", "egress": "open", "uses": [{"svc": "b"}]}}}`))
	if len(fs) != 2 || fs[0].Path != "services.a.egress" || fs[1].Path != "services.a.uses[0].svc" {
		t.Errorf("findings %+v", fs)
	}
}
