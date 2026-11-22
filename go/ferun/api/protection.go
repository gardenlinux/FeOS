// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

// ProtectedRequest is PUT /api/v1/environments/{env}/protected (FeOS ADR
// 0060); Protected is required.
type ProtectedRequest struct {
	Protected bool `json:"protected"`
}

// ProtectedAnswer is its answer: Changed false when it was so already.
type ProtectedAnswer struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Protected bool   `json:"protected"`
	Changed   bool   `json:"changed"`
}

// Principals is GET /api/v1/environments/{env}/principals: every principal
// with a grant in the project, people not disabled, project tokens and
// integrations, for the dialog that protects the environment.
type Principals struct {
	Principals []Principal `json:"principals"`
}

// Principal is one of them: Kind "person" (ID the person's), "token" (ID
// its hash, the {hash} of the token routes) or "integration" (ID its
// name). ProjectRole and EnvironmentRole are its grants on the project and
// on the environment; LosesWrite, it changes the environment by its
// project grant alone and only reads it once it is protected.
type Principal struct {
	Kind            string `json:"kind"`
	ID              string `json:"id"`
	Label           string `json:"label"`
	ProjectRole     string `json:"projectRole"`
	EnvironmentRole string `json:"environmentRole"`
	LosesWrite      bool   `json:"losesWrite"`
}
