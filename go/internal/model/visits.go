// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package model

// Who a public environment admits (ADR 0054): environments.access.
const (
	AccessAnyone   = "anyone"
	AccessVisitors = "visitors"
)

// ProviderFerun is the provider every project has without making it: the
// installation's issuer, admitting people with a grant on the environment.
// No project makes a provider of this name.
const ProviderFerun = "ferun"

// The kinds of providers (access_providers.kind; "ferun" has no row).
const (
	ProviderKindProject = "project"
	ProviderKindOIDC    = "oidc"
	ProviderKindFerun   = "ferun"
)

// Why a visit ended (visits.end_reason).
const (
	VisitExpired           = "expired"
	VisitProviderNo        = "provider_no"
	VisitSignedOut         = "signed_out"
	VisitUnpublished       = "unpublished"
	VisitProviderChanged   = "provider_changed"
	VisitBackchannelLogout = "backchannel_logout"
)
