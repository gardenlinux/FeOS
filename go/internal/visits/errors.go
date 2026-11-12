// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package visits

import (
	"net/http"

	"github.com/gardenlinux/feos/go/internal/apierr"
)

// The codes of the refusals of the providers', challenges' and visits'
// routes (ADR 0054).
const (
	CodeInvalidProvider    = "invalid_provider"
	CodeReservedName       = "reserved_name"
	CodeDestinationDenied  = "destination_denied"
	CodeFerunAuthRequired  = "ferun_auth_required"
	CodeTooManyProviders   = "too_many_providers"
	CodeProviderInUse      = "provider_in_use"
	CodeInvalidAccept      = "invalid_accept"
	CodeChallengeExpired   = "challenge_expired"
	CodeChallengeAnswered  = "challenge_answered"
	CodeInvalidRequest     = "invalid_request"
	textNoChallenge        = "no such challenge"
	textNoVisit            = "no such visit"
	textNoProvider         = "no such provider"
	textReservedName       = "ferun is every tenant's provider and cannot be made"
	textDestinationDenied  = "the address of %s is not reachable from the control plane"
	textOIDCNeedsFerunAuth = "providers of kind oidc need ferun-auth, which this installation does not run"
	textTooManyProviders   = "a tenant has at most %d providers"
	textProviderInUse      = "provider %s is named by %d environments"
	textChallengeExpired   = "the challenge has expired: send the browser to its restart address"
	textChallengeAnswered  = "the challenge was answered before: send the browser to its restart address"
	// textFailed is the text of every failure inside the service: its
	// cause is logged, never answered (it may hold what a log must not).
	textFailed = "the visits service failed"
)

// maxProviders is how many providers a tenant has at most.
const maxProviders = 50

func errNoChallenge() error { return apierr.New(http.StatusNotFound, textNoChallenge) }
func errNoVisit() error     { return apierr.New(http.StatusNotFound, textNoVisit) }
func errNoProvider() error  { return apierr.New(http.StatusNotFound, textNoProvider) }

// invalidProvider is a 400 invalid_provider.
func invalidProvider(format string, args ...any) error {
	return apierr.WithCode(http.StatusBadRequest, CodeInvalidProvider, format, args...)
}

// invalidAccept is a 400 invalid_accept.
func invalidAccept(format string, args ...any) error {
	return apierr.WithCode(http.StatusBadRequest, CodeInvalidAccept, format, args...)
}

// failed logs a failure inside the service and returns the 500 that
// answers it, whose text is fixed.
func (s *Service) failed(what string, err error) error {
	s.log.Error("visits: "+what, "err", err)
	return apierr.New(http.StatusInternalServerError, textFailed)
}
