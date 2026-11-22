// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gardenlinux/feos/go/ferun/spec"
)

func TestRoles(t *testing.T) {
	for _, c := range []struct {
		have, want string
		ok         bool
	}{
		{RoleAdmin, RoleWrite, true}, {RoleAdmin, RoleAdmin, true}, {RoleWrite, RoleRead, true},
		{RoleRead, RoleWrite, false}, {RoleWrite, RoleAdmin, false}, {"", RoleRead, false},
		{RoleAdmin, "", false}, {"owner", RoleRead, false}, {RoleRead, "owner", false},
	} {
		if got := AtLeast(c.have, c.want); got != c.ok {
			t.Errorf("AtLeast(%q, %q) = %v", c.have, c.want, got)
		}
	}
	for _, c := range [][3]string{
		{RoleAdmin, RoleWrite, RoleWrite}, {RoleRead, RoleAdmin, RoleRead}, {RoleWrite, RoleWrite, RoleWrite},
		{"", RoleAdmin, ""}, {RoleAdmin, "", ""}, {"owner", RoleRead, ""},
	} {
		if got := Lower(c[0], c[1]); got != c[2] {
			t.Errorf("Lower(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

// A refusal's body has error and code, and the fields of its code only.
func TestErrorBody(t *testing.T) {
	limit, use, current := int64(3), int64(3), 7
	for _, c := range []struct {
		b    ErrorBody
		want string
	}{
		{ErrorBody{Error: "no such environment"}, `{"error":"no such environment"}`},
		{ErrorBody{Error: "e", Code: CodeRoleRequired, Role: RoleWrite, Scope: ScopeEnvironment},
			`{"error":"e","code":"role_required","role":"write","scope":"environment"}`},
		{ErrorBody{Error: "e", Code: CodeQuotaExceeded, Resource: ResourceSandboxes, Limit: &limit, Use: &use},
			`{"error":"e","code":"quota_exceeded","resource":"sandboxes","limit":3,"use":3}`},
		{ErrorBody{Error: "e", Code: CodeInvalidSpec, Findings: []spec.Finding{{Severity: spec.SeverityError,
			Path: "services.web", Problem: "p", Remedy: "r"}}},
			`{"error":"e","code":"invalid_spec","findings":[{"severity":"error","path":"services.web","problem":"p","remedy":"r"}]}`},
		{ErrorBody{Error: "e", Code: CodeBaseMismatch, Current: &current}, `{"error":"e","code":"base_mismatch","current":7}`},
	} {
		b, err := json.Marshal(c.b)
		if err != nil || string(b) != c.want {
			t.Errorf("%+v: %s %v, want %s", c.b, b, err, c.want)
		}
	}
}

// An apply's and an object's answer say their status; an object's JSON
// is its plan's or its apply's.
func TestAnswerStatus(t *testing.T) {
	if (ApplyAnswer{}).StatusCode() != http.StatusCreated || (ApplyAnswer{Unchanged: true}).StatusCode() != http.StatusOK {
		t.Fatal("apply status")
	}
	o := ObjectAnswer{DryRun: true, Plan: PlanAnswer{Valid: true}}
	if o.StatusCode() != http.StatusOK {
		t.Fatal("dry run status")
	}
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var p PlanAnswer
	if err := json.Unmarshal(b, &p); err != nil || !p.Valid {
		t.Fatalf("dry run %s %v", b, err)
	}
	o = ObjectAnswer{Apply: ApplyAnswer{DeploymentInfo: DeploymentInfo{Number: 4}}}
	if o.StatusCode() != http.StatusCreated {
		t.Fatal("apply status")
	}
	b, _ = json.Marshal(o)
	var a ApplyAnswer
	if err := json.Unmarshal(b, &a); err != nil || a.Number != 4 {
		t.Fatalf("apply %s %v", b, err)
	}
}

// An environment says whether it is protected, false or true, always.
func TestProtected(t *testing.T) {
	for _, v := range []any{EnvStatus{}, EnvSummary{}, EnvStatus{Protected: true}} {
		b, _ := json.Marshal(v)
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["protected"]; !ok {
			t.Fatalf("%T: %s", v, b)
		}
	}
}
