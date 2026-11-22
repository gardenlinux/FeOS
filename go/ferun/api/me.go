// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"time"
)

// Me is GET /auth/me: who the caller is, by its credential, and the
// projects it reaches with its roles there (FeOS ADR 0056, 0060). Kind is
// "session" (a person's or an operator's), "basic" (an operator's tooling),
// "token" (a project token) or "integration". The roles are the effective
// ones: what FeRun's checks decide, so that no caller computes a role. A
// session's answer carries all of the person's fields, Sub to Method,
// empty and false ones too (MarshalJSON); the other kinds leave out what
// they do not have.
type Me struct {
	Kind     string `json:"kind"`
	Sub      string `json:"sub,omitempty"`
	Person   string `json:"person,omitempty"` // a session's person ID
	Email    string `json:"email,omitempty"`
	Name     string `json:"name,omitempty"`
	Username string `json:"username,omitempty"`
	Picture  string `json:"picture,omitempty"`
	Admin    bool   `json:"admin,omitempty"`
	Method   string `json:"method,omitempty"`
	// Token is a project token's own grant.
	Token *MeToken `json:"token,omitempty"`
	// Integration is the integration that asks, and Named the person it
	// names in FeRun-Person.
	Integration *MeIntegration `json:"integration,omitempty"`
	Named       *MeNamed       `json:"named,omitempty"`
	// Projects are sorted by name, each one's environments by name;
	// always present, [] when none.
	Projects []MeProject `json:"projects"`
	// Accounts are a session's (ADR 0059); [] for every other kind.
	Accounts []MeAccount `json:"accounts"`
}

// MarshalJSON writes Me in the shape of its kind: a session's with sub,
// person, email, name, username, picture, admin and method always, as
// /auth/me has always answered a session (a person without a picture is
// "picture": "", one who is no operator "admin": false); the other kinds
// as the fields' tags say.
func (m Me) MarshalJSON() ([]byte, error) {
	type fields Me // Me's fields without this method
	if m.Kind != "session" {
		return json.Marshal(fields(m))
	}
	// The outer fields hide the embedded ones of the same names, and the
	// answer keeps Me's order.
	return json.Marshal(struct {
		Kind     string `json:"kind"`
		Sub      string `json:"sub"`
		Person   string `json:"person"`
		Email    string `json:"email"`
		Name     string `json:"name"`
		Username string `json:"username"`
		Picture  string `json:"picture"`
		Admin    bool   `json:"admin"`
		Method   string `json:"method"`
		fields
	}{m.Kind, m.Sub, m.Person, m.Email, m.Name, m.Username, m.Picture, m.Admin, m.Method, fields(m)})
}

// MeToken is a project token's own grant: its role on the project, or on
// Environment (its ID; "" for the project).
type MeToken struct {
	Label       string `json:"label"`
	Role        string `json:"role"`
	Environment string `json:"environment"`
}

// MeIntegration is the integration that asks. A disabled one reaches no
// project.
type MeIntegration struct {
	Name     string `json:"name"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Disabled bool   `json:"disabled"`
}

// MeNamed is the person an integration names: Known false (and no other
// field but Sub) for a subject FeRun does not know or a disabled person.
type MeNamed struct {
	Sub    string `json:"sub"`
	Known  bool   `json:"known"`
	Person string `json:"person,omitempty"`
	Email  string `json:"email,omitempty"`
	Name   string `json:"name,omitempty"`
}

// MeProject is a project the caller reaches: Role its effective role on
// the project ("" with roles on environments alone); for an integration
// IntegrationRole its own grant on the project and, with a person named,
// PersonRole the person's. CreatedAt is when the project was made: a
// name freed by a deleted project and made again is another project, so a
// caller that keeps something per project tells the two apart by it.
type MeProject struct {
	Project         string      `json:"project"`
	CreatedAt       time.Time   `json:"createdAt"`
	Role            string      `json:"role"`
	IntegrationRole string      `json:"integrationRole,omitempty"`
	PersonRole      string      `json:"personRole,omitempty"`
	Account         *AccountRef `json:"account,omitempty"` // a session's (ADR 0059)
	// Delete is whether the caller may delete the project (DELETE
	// /api/v1/project): its admin, an operator, or an integration as
	// admin itself for a person it names who is the project's admin;
	// never an integration that names nobody, never a project token.
	Delete       bool            `json:"delete"`
	Environments []MeEnvironment `json:"environments"`
}

// MeEnvironment is an environment the caller reads: Role its effective
// role there; IntegrationRole the integration's own (protection counted,
// not the cap of one that names nobody) and PersonRole the named person's;
// Plan whether it may plan against it; Delete whether it may delete and
// rename it.
type MeEnvironment struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Role            string `json:"role"`
	IntegrationRole string `json:"integrationRole,omitempty"`
	PersonRole      string `json:"personRole,omitempty"`
	Protected       bool   `json:"protected"`
	Plan            bool   `json:"plan"`
	Delete          bool   `json:"delete"`
}

// MeAccount is an account a person may put projects on or read (ADR
// 0059): Role "member" or "admin"; Explicit, the account's page is
// readable to the caller.
type MeAccount struct {
	Name     string `json:"name"`
	Title    string `json:"title"`
	Role     string `json:"role"`
	Explicit bool   `json:"explicit"`
	Closed   bool   `json:"closed"`
}

// AccountRef names an account.
type AccountRef struct {
	Name  string `json:"name"`
	Title string `json:"title"`
}

// Project is GET /api/v1/project, the sink's ask (ADR 0026): the
// caller's effective role on the project and the environments it reads
// beside it. CreatedAt is when the project was made, as MeProject's: the
// sink reads a project's logs and traces from then on, since a project
// made under a name again is another (ADR 0056); a ferund of before
// leaves it out (the zero time).
type Project struct {
	Project      string    `json:"project"`
	CreatedAt    time.Time `json:"createdAt"`
	Role         string    `json:"role"`
	Environments []string  `json:"environments"`
}
