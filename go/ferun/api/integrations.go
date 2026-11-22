// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

// The codes of an integration's refusals (FeOS ADR 0056).
const (
	// CodeIntegrationRefused: an integration changes no principal of a
	// project (403), and is no person and no provider of visitors.
	CodeIntegrationRefused = "integration_refused"
	// CodePersonHeaderRefused: FeRun-Person from a session or a project
	// token, or a value that names no one person (400).
	CodePersonHeaderRefused = "person_header_refused"
	// CodeTokenRefused: an integration's bearer fails verification (401).
	CodeTokenRefused = "token_refused"
	// CodeUnknownIntegration: a verified bearer of no registered
	// integration (401).
	CodeUnknownIntegration = "unknown_integration"
	// CodePersonRequired: a project token moves a project (403, ADR 0059)
	// or deletes it (ADR 0031).
	CodePersonRequired = "person_required"
	// CodeProviderUnreachable: the provider's keys cannot be read (502).
	CodeProviderUnreachable = "provider_unreachable"
	// CodeIntegrationsUnavailable: an installation without an issuer has
	// no integrations (503).
	CodeIntegrationsUnavailable = "integrations_unavailable"
	// CodeSubjectTaken: another integration has the (issuer, subject)
	// (409).
	CodeSubjectTaken = "subject_taken"
	// CodeIssuerRefused: the installation verifies tokens of its issuer
	// only (422).
	CodeIssuerRefused = "issuer_refused"
)

// IntegrationList is GET /api/v1/integrations: the enabled integrations,
// by name, which every signed-in person reads.
type IntegrationList struct {
	Integrations []IntegrationInfo `json:"integrations"`
}

// IntegrationInfo is a registered integration as people see it.
type IntegrationInfo struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	URL   string `json:"url"`
}
