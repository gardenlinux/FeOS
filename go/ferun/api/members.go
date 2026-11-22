// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

// Members is GET /api/v1/project/members: the people and the integrations
// that hold a grant in the project (FeOS ADR 0031, 0056). Project tokens
// are not listed (GET /api/v1/project/tokens, for its admins).
type Members struct {
	Members      []Member           `json:"members"`
	Integrations []IntegrationGrant `json:"integrations"`
}

// Member is a person with a grant in the project: Issuer and Subject name
// them at the provider, so that software on it finds its own users. Role
// is their grant on the project ("" with grants on environments alone),
// Environments their grants on the environments the caller reads. MadeBy
// is the project grant's principal as made_by records it, MadeByName its
// name.
type Member struct {
	Person       string     `json:"person"`
	Name         string     `json:"name"`
	Email        string     `json:"email"`
	Issuer       string     `json:"issuer"`
	Subject      string     `json:"subject"`
	Disabled     bool       `json:"disabled,omitempty"`
	Role         string     `json:"role"`
	MadeBy       string     `json:"madeBy"`
	MadeByName   string     `json:"madeByName"`
	Environments []EnvGrant `json:"environments"`
}

// EnvGrant is a grant on an environment, with its environment's name.
type EnvGrant struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Role       string `json:"role"`
	MadeBy     string `json:"madeBy"`
	MadeByName string `json:"madeByName"`
}

// IntegrationGrant is an integration with a grant in the project, as
// Member is a person.
type IntegrationGrant struct {
	Integration  string     `json:"integration"`
	Title        string     `json:"title"`
	URL          string     `json:"url"`
	Disabled     bool       `json:"disabled"`
	Role         string     `json:"role"`
	MadeBy       string     `json:"madeBy"`
	MadeByName   string     `json:"madeByName"`
	Environments []EnvGrant `json:"environments"`
}
