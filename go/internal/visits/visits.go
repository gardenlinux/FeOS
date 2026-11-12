// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package visits

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/visit"
)

// errNotBuilt is every body's answer until the package is built.
var errNotBuilt = errors.New("visits: not built yet")

// The cluster secrets of the keys (S1): the token key's seed, sealed under
// the AAD "cluster_secrets|visit-token", and the challenge key in plaintext.
const (
	SecretToken     = "visit-token"
	SecretChallenge = "visit-challenge"
)

// Sealer seals the token key's seed and the oidc providers' client
// secrets at rest (--secret-key-file).
type Sealer interface {
	Seal(plaintext, aad []byte) ([]byte, error)
	Open(sealed, aad []byte) ([]byte, error)
}

// Config is the installation's side of visits.
type Config struct {
	AuthURL string         // --visitor-auth-url without a trailing slash; "": no ferun-auth
	Issuer  string         // --oidc-issuer exactly: the issuer of kind ferun's persons; "": kind ferun not served
	Zones   model.Zones    // the zones of environments' default hosts
	Deny    []netip.Prefix // --deny-prefix; nil: egress.DefaultDeny()
	Log     *slog.Logger
}

// Deps are what the service works with.
type Deps struct {
	Store  *store.Store
	Sealer Sealer // required: New refuses nil (S1); ferund without --secret-key-file makes no Service
}

// Service is ferund's visits service.
type Service struct {
	cfg Config
	d   Deps
	// roots verify the providers' certificates; nil (the system's) but
	// in tests.
	roots *x509.CertPool
}

// New returns the service of cfg and d.
func New(cfg Config, d Deps) (*Service, error) { return nil, errNotBuilt }

// Kinds are the kinds of providers this installation serves, in the order
// tenant, ferun, oidc.
func (s *Service) Kinds() []string { return nil }

// Loops are the service's leader loops: "visits-sweep", every 10 minutes.
func (s *Service) Loops() []model.Loop { return nil }

// Keys are the installation's keys (they can sign), made on first use: read
// both cluster secrets; one that is missing is made (random 32 bytes; the
// seed sealed) with INSERT … ON CONFLICT DO NOTHING and read back, so
// every instance ends with the winner's. Cached once loaded; an error is
// returned and retried at the next call, never fatal at start. The table
// builder takes its VisitKeys from here.
func (s *Service) Keys(ctx context.Context) (*visit.Keys, error) { return nil, errNotBuilt }

// Caller is who calls a provider route.
type Caller struct {
	Auth   bool                     // ferun-auth's service token
	Tenant string                   // a tenant principal's tenant; "" with Auth
	Admin  func(env uuid.UUID) bool // whether the tenant principal is admin on env; nil with Auth
	MadeBy string                   // the principal, for answered_by
}

// Rule is an oidc provider's rule over the ID token's claims: exactly one
// of its fields is set.
type Rule struct {
	Anyone  bool     `json:"anyone,omitempty"`
	Emails  []string `json:"emails,omitempty"`
	Domains []string `json:"domains,omitempty"`
	Groups  []string `json:"groups,omitempty"`
}

// Secret is a client secret as answered: whether one is stored.
type Secret struct {
	Set bool `json:"set"`
}

// EnvRef names an environment.
type EnvRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ProviderIn is the body of PUT /api/v1/providers/{provider}.
type ProviderIn struct {
	Kind         string          `json:"kind"`
	URL          string          `json:"url,omitempty"`
	Issuer       string          `json:"issuer,omitempty"`
	ClientID     string          `json:"clientId,omitempty"`
	ClientSecret json.RawMessage `json:"clientSecret,omitempty"` // a string, or {"set": true}: keep
	Rule         *Rule           `json:"rule,omitempty"`
}

// Provider is a provider as answered.
type Provider struct {
	Name           string   `json:"name"`
	Kind           string   `json:"kind"`
	BuiltIn        bool     `json:"builtIn"`
	URL            string   `json:"url,omitempty"`
	Issuer         string   `json:"issuer,omitempty"`
	ClientID       string   `json:"clientId,omitempty"`
	ClientSecret   *Secret  `json:"clientSecret,omitempty"`
	Rule           *Rule    `json:"rule,omitempty"`
	BackchannelURI string   `json:"backchannelUri,omitempty"`
	Environments   []EnvRef `json:"environments"`
	CreatedAt      string   `json:"createdAt,omitempty"`
	UpdatedAt      string   `json:"updatedAt,omitempty"`
}

// ProviderList is GET /api/v1/providers' answer.
type ProviderList struct {
	Providers   []Provider `json:"providers"`
	Kinds       []string   `json:"kinds"`
	RedirectURI string     `json:"redirectUri"`
}

// ListProviders lists a tenant's providers, ferun first.
func (s *Service) ListProviders(ctx context.Context, tenant string) (ProviderList, error) {
	return ProviderList{}, errNotBuilt
}

// GetProvider answers one of a tenant's providers.
func (s *Service) GetProvider(ctx context.Context, tenant, name string) (Provider, error) {
	return Provider{}, errNotBuilt
}

// PutProvider makes or changes a tenant's provider.
func (s *Service) PutProvider(ctx context.Context, tenant, name string, in ProviderIn, madeBy string) (p Provider, created bool, err error) {
	return Provider{}, false, errNotBuilt
}

// DeleteProvider deletes a tenant's provider no environment names.
func (s *Service) DeleteProvider(ctx context.Context, tenant, name string) error { return errNotBuilt }

// EnvView is an environment as a provider sees it.
type EnvView struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Tenant string            `json:"tenant"`
	Labels map[string]string `json:"labels"`
}

// ProviderRef names a provider and its kind.
type ProviderRef struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// OIDC is an oidc provider's client, for ferun-auth.
type OIDC struct {
	Issuer       string `json:"issuer"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	Rule         Rule   `json:"rule"`
}

// Challenge is an opened challenge as answered.
type Challenge struct {
	Environment EnvView     `json:"environment"`
	Host        string      `json:"host"`
	Path        string      `json:"path"`
	ExpiresAt   string      `json:"expiresAt"`
	Expired     bool        `json:"expired"`
	Answered    bool        `json:"answered"`
	Restart     string      `json:"restart"`
	Provider    ProviderRef `json:"provider"`
	OIDC        *OIDC       `json:"oidc,omitempty"`
}

// Accept is the body of POST /api/v1/challenges/accept.
type Accept struct {
	Challenge string `json:"challenge"`
	Subject   string `json:"subject"`
	Email     string `json:"email,omitempty"`
	Name      string `json:"name,omitempty"`
	Session   string `json:"session,omitempty"`
	EndsAt    string `json:"endsAt,omitempty"`
}

// Answer is where a provider sends the browser after its answer.
type Answer struct {
	Redirect string `json:"redirect"`
	Admitted bool   `json:"admitted"`
	Visit    string `json:"visit,omitempty"`
}

// Visit is a visit as a provider reads it.
type Visit struct {
	ID          string      `json:"id"`
	Environment EnvView     `json:"environment"`
	Host        string      `json:"host"`
	Provider    ProviderRef `json:"provider"`
	Subject     string      `json:"subject"`
	Email       string      `json:"email"`
	Name        string      `json:"name"`
	Session     string      `json:"session"`
	CreatedAt   string      `json:"createdAt"`
	EndsAt      string      `json:"endsAt"`
	EndedAt     string      `json:"endedAt"`
	EndReason   string      `json:"endReason"`
}

// EndSession is the body of POST /api/v1/visits/end.
type EndSession struct {
	Tenant   string `json:"tenant,omitempty"`
	Provider string `json:"provider"`
	Session  string `json:"session"`
}

// OIDCProvider is a tenant's oidc provider for ferun-auth's back-channel
// logout.
type OIDCProvider struct {
	Tenant       string `json:"tenant"`
	Name         string `json:"name"`
	Issuer       string `json:"issuer"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	Rule         Rule   `json:"rule"`
}

// OpenChallenge opens a sealed challenge for a provider.
func (s *Service) OpenChallenge(ctx context.Context, c Caller, sealed string) (Challenge, error) {
	return Challenge{}, errNotBuilt
}

// AcceptChallenge records a provider's yes and makes the visit.
func (s *Service) AcceptChallenge(ctx context.Context, c Caller, in Accept) (Answer, error) {
	return Answer{}, errNotBuilt
}

// RejectChallenge records a provider's no.
func (s *Service) RejectChallenge(ctx context.Context, c Caller, sealed string) (Answer, error) {
	return Answer{}, errNotBuilt
}

// GetVisit answers a visit to its provider.
func (s *Service) GetVisit(ctx context.Context, c Caller, id string) (Visit, error) {
	return Visit{}, errNotBuilt
}

// EndBySession ends the open visits of a provider's session.
func (s *Service) EndBySession(ctx context.Context, in EndSession) (int, error) {
	return 0, errNotBuilt
}

// OIDCProvider answers a tenant's oidc provider to ferun-auth.
func (s *Service) OIDCProvider(ctx context.Context, tenant, name string) (OIDCProvider, error) {
	return OIDCProvider{}, errNotBuilt
}

// ErrInvalid marks a request on the ingress's connection that is malformed
// (INVALID_ARGUMENT); every other error is a failure (UNAVAILABLE).
var ErrInvalid = errors.New("invalid request")

// Redeem redeems an accepted code for the visit's first token.
func (s *Service) Redeem(ctx context.Context, req wire.RedeemVisitRequest) (wire.RedeemVisitResponse, error) {
	return wire.RedeemVisitResponse{}, errNotBuilt
}

// Renew gives a visit's next token, asking its provider (D7, §1.9).
func (s *Service) Renew(ctx context.Context, req wire.RenewVisitRequest) (wire.RenewVisitResponse, error) {
	return wire.RenewVisitResponse{}, errNotBuilt
}

// End ends a visit whose visitor signed out.
func (s *Service) End(ctx context.Context, req wire.EndVisitRequest) (wire.EndVisitResponse, error) {
	return wire.EndVisitResponse{}, errNotBuilt
}
