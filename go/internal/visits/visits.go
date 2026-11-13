// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package visits

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/egress"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/visit"
)

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
	cfg   Config
	d     Deps
	st    *store.Store
	log   *slog.Logger
	rl    *rateLog
	kinds []string
	// judge is the rule of the addresses of the providers of kind tenant
	// (--deny-prefix, ADR 0028), at their PUT and at every call.
	judge func(netip.Addr) error
	// roots verify the providers' certificates; nil (the system's) but
	// in tests.
	roots *x509.CertPool
	// client calls the providers of kind tenant (§1.9): made once, with
	// roots.
	client *http.Client

	keysMu sync.Mutex
	keys   *visit.Keys
}

// New returns the service of cfg and d. It refuses a Deps without a
// Store or a Sealer: visits run only with --secret-key-file (S1).
func New(cfg Config, d Deps) (*Service, error) {
	return newService(cfg, d, nil)
}

// newService is New with the roots that verify the providers'
// certificates (nil: the system's; tests give their own).
func newService(cfg Config, d Deps, roots *x509.CertPool) (*Service, error) {
	if d.Store == nil {
		return nil, errors.New("visits: no store")
	}
	if d.Sealer == nil {
		return nil, errors.New("visits: no sealer: visits need --secret-key-file")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	cfg.AuthURL = strings.TrimRight(cfg.AuthURL, "/")
	deny := cfg.Deny
	if deny == nil {
		deny = egress.DefaultDeny()
	}
	s := &Service{cfg: cfg, d: d, st: d.Store, log: cfg.Log, rl: newRateLog(cfg.Log), roots: roots,
		judge: egress.Rule{Deny: deny}.Judge}
	s.client = egress.Client(s.judge, visit.AskTimeout, false, roots)
	s.kinds = []string{model.ProviderKindTenant}
	if cfg.AuthURL != "" && cfg.Issuer != "" {
		s.kinds = append(s.kinds, model.ProviderKindFerun)
	}
	if cfg.AuthURL != "" {
		s.kinds = append(s.kinds, model.ProviderKindOIDC)
	}
	return s, nil
}

// Kinds are the kinds of providers this installation serves, in the order
// tenant, ferun, oidc: tenant always, ferun with ferun-auth and the
// installation's issuer, oidc with ferun-auth.
func (s *Service) Kinds() []string { return slices.Clone(s.kinds) }

// serves reports whether the installation serves providers of kind.
func (s *Service) serves(kind string) bool { return slices.Contains(s.kinds, kind) }

// The sweep: every sweepInterval the leader ends the visits past their
// end, and deletes the visits that ended more than keepVisits ago and the
// answers of challenges that ended more than keepAnswers ago.
const (
	sweepInterval = 10 * time.Minute
	keepVisits    = 24 * time.Hour
	keepAnswers   = time.Hour
)

// Loops are the service's leader loops: "visits-sweep", every 10 minutes.
func (s *Service) Loops() []model.Loop {
	return []model.Loop{{Name: "visits-sweep", Interval: sweepInterval, Run: s.sweep}}
}

// sweep is a round of the loop visits-sweep.
func (s *Service) sweep(ctx context.Context) error {
	sw, err := s.st.SweepVisits(ctx, model.VisitExpired, keepVisits, keepAnswers)
	if err != nil {
		return fmt.Errorf("sweep visits: %w", err)
	}
	if sw.Ended > 0 || sw.Deleted > 0 || sw.Answers > 0 {
		s.log.Debug("visits swept", "ended", sw.Ended, "deleted", sw.Deleted, "answers", sw.Answers)
	}
	return nil
}

// tokenAAD is the AAD the token key's seed is sealed under.
var tokenAAD = []byte("cluster_secrets|" + SecretToken)

// Keys are the installation's keys (they can sign), made on first use: read
// both cluster secrets; one that is missing is made (random 32 bytes; the
// seed sealed) with INSERT … ON CONFLICT DO NOTHING and read back, so
// every instance ends with the winner's. Cached once loaded; an error is
// returned and retried at the next call, never fatal at start. The table
// builder takes its VisitKeys from here.
func (s *Service) Keys(ctx context.Context) (*visit.Keys, error) {
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	if s.keys != nil {
		return s.keys, nil
	}
	sealed, err := s.clusterSecret(ctx, SecretToken, func(seed []byte) ([]byte, error) {
		return s.d.Sealer.Seal(seed, tokenAAD)
	})
	if err != nil {
		return nil, err
	}
	seed, err := s.d.Sealer.Open(sealed, tokenAAD)
	if err != nil {
		return nil, fmt.Errorf("visits: the cluster secret %s: %w", SecretToken, err)
	}
	challenge, err := s.clusterSecret(ctx, SecretChallenge, func(key []byte) ([]byte, error) { return key, nil })
	if err != nil {
		return nil, err
	}
	keys, err := visit.NewKeys(seed, challenge)
	if err != nil {
		return nil, fmt.Errorf("visits: the keys: %w", err)
	}
	s.keys = keys
	return keys, nil
}

// JWKS is the token key's public half as a JWKS, the document the
// ingress serves at /.ferun/keys: what a provider of kind tenant verifies
// ferund's questions with (GET /api/v1/visit-keys).
func (s *Service) JWKS(ctx context.Context) ([]byte, error) {
	keys, err := s.Keys(ctx)
	if err != nil {
		return nil, s.failed("the keys", err)
	}
	return keys.JWKS(), nil
}

// clusterSecret reads the cluster secret name, making it when it is
// missing from 32 random bytes as stored makes them stored.
func (s *Service) clusterSecret(ctx context.Context, name string, stored func([]byte) ([]byte, error)) ([]byte, error) {
	v, err := s.st.ClusterSecret(ctx, name)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, store.ErrNoRow) {
		return nil, fmt.Errorf("visits: read the cluster secret %s: %w", name, err)
	}
	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("visits: %w", err)
	}
	if fresh, err = stored(fresh); err != nil {
		return nil, fmt.Errorf("visits: make the cluster secret %s: %w", name, err)
	}
	if v, err = s.st.EnsureClusterSecret(ctx, name, fresh); err != nil {
		return nil, fmt.Errorf("visits: %w", err)
	}
	return v, nil
}

// rateLog logs a warning of each kind at most once a minute (as the
// environments' controller does).
type rateLog struct {
	log  *slog.Logger
	mu   sync.Mutex
	last map[string]time.Time
}

func newRateLog(log *slog.Logger) *rateLog { return &rateLog{log: log, last: map[string]time.Time{}} }

func (r *rateLog) warn(kind, msg string, args ...any) {
	r.mu.Lock()
	now := time.Now()
	if t, ok := r.last[kind]; ok && now.Sub(t) < time.Minute {
		r.mu.Unlock()
		r.log.Debug(msg, args...)
		return
	}
	r.last[kind] = now
	r.mu.Unlock()
	r.log.Warn(msg, args...)
}

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
	Name         string  `json:"name"`
	Kind         string  `json:"kind"`
	BuiltIn      bool    `json:"builtIn"`
	URL          string  `json:"url,omitempty"`
	Issuer       string  `json:"issuer,omitempty"`
	ClientID     string  `json:"clientId,omitempty"`
	ClientSecret *Secret `json:"clientSecret,omitempty"`
	Rule         *Rule   `json:"rule,omitempty"`
	// RedirectURI and BackchannelURI, for kind oidc with ferun-auth, are
	// what the tenant registers at its issuer: ferun-auth's
	// /callback/<tenant>/<provider> and /backchannel/<tenant>/<provider>.
	RedirectURI    string   `json:"redirectUri,omitempty"`
	BackchannelURI string   `json:"backchannelUri,omitempty"`
	Environments   []EnvRef `json:"environments"`
	CreatedAt      string   `json:"createdAt,omitempty"`
	UpdatedAt      string   `json:"updatedAt,omitempty"`
}

// ProviderList is GET /api/v1/providers' answer.
type ProviderList struct {
	Providers []Provider `json:"providers"`
	Kinds     []string   `json:"kinds"`
}

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

// ErrInvalid marks a request on the ingress's connection that is malformed
// (INVALID_ARGUMENT); every other error is a failure (UNAVAILABLE).
var ErrInvalid = errors.New("invalid request")
