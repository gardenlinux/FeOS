// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"encoding/json"
	"testing"
)

// For one release the ingress takes the names of before ADR 0061 where a
// message lacks the new ones, and the new ones win where both are there.
func TestLegacyNames(t *testing.T) {
	var e Env
	if err := json.Unmarshal([]byte(`{"id":"e1","tenant":"acme","name":"prod"}`), &e); err != nil || e.Project != "acme" || e.Name != "prod" { // KEEP-TENANT: a ferund before ADR 0061
		t.Fatalf("env of before: %+v %v", e, err)
	}
	e = Env{}
	if err := json.Unmarshal([]byte(`{"id":"e1","project":"new","tenant":"old"}`), &e); err != nil || e.Project != "new" { // KEEP-TENANT: a ferund before ADR 0061
		t.Fatalf("env with both: %+v %v", e, err)
	}
	var h HelloResponse
	if err := json.Unmarshal([]byte(`{"name":"i1","appsZone":"apps.x","tenantZone":"p.x"}`), &h); err != nil || h.ProjectZone != "p.x" || h.AppsZone != "apps.x" { // KEEP-TENANT: a ferund before ADR 0061
		t.Fatalf("hello of before: %+v %v", h, err)
	}
	var s Table
	if err := json.Unmarshal([]byte(`{"seq":1,"self":{"name":"i1","tenantZone":"p.x"},"envs":[{"id":"e1","tenant":"acme"}]}`), &s); err != nil || // KEEP-TENANT: a ferund before ADR 0061
		s.Self == nil || s.Self.ProjectZone != "p.x" {
		t.Fatalf("table of before: %+v %v", s, err)
	}
	b, err := json.Marshal(Env{ID: "e1", Project: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil || back["project"] != "acme" || len(back) == 0 {
		t.Fatalf("an env is sent with its new name: %s", b)
	}
	if _, old := back["tenant"]; old { // KEEP-TENANT: never sent
		t.Fatalf("an env is sent with the earlier name: %s", b)
	}
}
