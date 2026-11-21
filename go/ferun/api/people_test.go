// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"
)

// The people answers keep the contract's JSON: what is left out when
// empty, and what is always there.
func TestPeopleJSON(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
	}{
		{Me{Kind: "basic", Username: "ops", Admin: true, Method: "basic", Projects: []MeProject{}, Accounts: []MeAccount{}},
			`{"kind":"basic","username":"ops","admin":true,"method":"basic","projects":[],"accounts":[]}`},
		{Me{Kind: "integration", Integration: &MeIntegration{Name: "fecode"}, Named: &MeNamed{Sub: "x"},
			Projects: []MeProject{{Project: "kino", Role: "write", IntegrationRole: "admin", Environments: []MeEnvironment{
				{ID: "e", Name: "prod", Role: "read", IntegrationRole: "admin", Protected: true, Plan: true}}}},
			Accounts: []MeAccount{}},
			`{"kind":"integration","integration":{"name":"fecode","title":"","url":"","disabled":false},` +
				`"named":{"sub":"x","known":false},"projects":[{"project":"kino","role":"write","integrationRole":"admin",` +
				`"environments":[{"id":"e","name":"prod","role":"read","integrationRole":"admin","protected":true,"plan":true,` +
				`"delete":false}]}],"accounts":[]}`},
		{Me{Kind: "token", Token: &MeToken{Role: "write"}, Projects: []MeProject{}, Accounts: []MeAccount{}},
			`{"kind":"token","token":{"label":"","role":"write","environment":""},"projects":[],"accounts":[]}`},
		{Member{Person: "p", Role: "admin", Environments: []EnvGrant{}},
			`{"person":"p","name":"","email":"","issuer":"","subject":"","role":"admin","madeBy":"","madeByName":"","environments":[]}`},
		{Principal{Kind: "token", ID: "h", ProjectRole: "write", LosesWrite: true},
			`{"kind":"token","id":"h","label":"","projectRole":"write","environmentRole":"","losesWrite":true}`},
		{ProtectedAnswer{ID: "e", Name: "prod", Protected: true}, `{"id":"e","name":"prod","protected":true,"changed":false}`},
		{Project{Project: "kino", Environments: []string{}}, `{"project":"kino","role":"","environments":[]}`},
	} {
		b, err := json.Marshal(c.v)
		if err != nil || string(b) != c.want {
			t.Errorf("%T:\n got %s\nwant %s", c.v, b, c.want)
		}
	}
}
