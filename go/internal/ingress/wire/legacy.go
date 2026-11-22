// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wire

import "encoding/json"

// For one release (ADR 0061) an ingress reads the names of before beside
// the new ones: a ferund of the release before sends them, and the table
// cache of before (table-v3.json) holds them. The ingress rolls before
// ferund, so it decodes project, else its earlier name, and projectZone,
// else its earlier name. Removed in the next release.

// UnmarshalJSON decodes an Env, taking the project from its earlier name
// when the message has no project.
func (e *Env) UnmarshalJSON(b []byte) error {
	type plain Env
	var v struct {
		plain
		Legacy string `json:"tenant"` // KEEP-TENANT: a ferund before ADR 0061 sends it
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*e = Env(v.plain)
	if e.Project == "" {
		e.Project = v.Legacy
	}
	return nil
}

// UnmarshalJSON decodes a HelloResponse, taking the project zone from
// the earlier name when the message has none.
func (h *HelloResponse) UnmarshalJSON(b []byte) error {
	type plain HelloResponse
	var v struct {
		plain
		LegacyZone string `json:"tenantZone"` // KEEP-TENANT: a ferund before ADR 0061 sends it
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*h = HelloResponse(v.plain)
	if h.ProjectZone == "" {
		h.ProjectZone = v.LegacyZone
	}
	return nil
}

// UnmarshalJSON decodes a Self, taking the project zone from the earlier
// name when the message has none.
func (s *Self) UnmarshalJSON(b []byte) error {
	type plain Self
	var v struct {
		plain
		LegacyZone string `json:"tenantZone"` // KEEP-TENANT: a ferund before ADR 0061 sends it
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*s = Self(v.plain)
	if s.ProjectZone == "" {
		s.ProjectZone = v.LegacyZone
	}
	return nil
}
