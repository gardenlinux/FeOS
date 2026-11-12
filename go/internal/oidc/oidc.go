// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package oidc is the OpenID Connect relying party of FeRun's sign-ins:
// ferund's human sign-in (§11) and ferun-auth's visitors (ADR 0054). It
// holds the provider's discovery document, its key set, the code exchange
// at the token endpoint, userinfo, and the checks of an ID token and of a
// back-channel logout token. Confidential client, client_secret_basic,
// authorization code flow with PKCE; no refresh tokens.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The provider's timings.
const (
	// discoveryTTL is how long the discovery document is cached.
	discoveryTTL = time.Hour
	// jwksRefetchEvery bounds the refetches of the key set for an unknown
	// key id.
	jwksRefetchEvery = time.Minute
	// providerTimeout bounds every call to the provider.
	providerTimeout = 10 * time.Second
	// clockSkew is the leeway of exp and iat.
	clockSkew = 60 * time.Second
	// MaxBody bounds what is read of a provider's answer.
	MaxBody = 1 << 20
)

// Config is an OIDC relying party's client.
type Config struct {
	// Issuer is compared exactly with the discovery document's issuer and
	// every ID token's iss.
	Issuer, ClientID, ClientSecret string
	Scopes                         []string
	// AdminGroup makes a person an admin when the groups claim holds it.
	AdminGroup string
	// HTTPClient talks to the provider; nil: a client with a 10 s timeout.
	HTTPClient *http.Client
	// Now is the clock of the token checks and the caches; nil: time.Now
	// (tests set it).
	Now func() time.Time
}

// Discovery is what a client reads of the discovery document.
type Discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
}

// ErrProvider is a failure to talk to the provider (unreachable, an
// answer that is not what OIDC prescribes): the sign-in page says 502.
type ErrProvider struct{ err error }

func (e *ErrProvider) Error() string { return e.err.Error() }
func (e *ErrProvider) Unwrap() error { return e.err }

func providerErr(format string, args ...any) error {
	return &ErrProvider{err: fmt.Errorf(format, args...)}
}

// ErrRefused is a sign-in that the checks refuse: an ID token or a
// provider's answer that is not valid. Reason is a fixed text, the short
// reason the sign-in page shows; Detail holds what was found (values of
// the token among them) and goes to the log only.
type ErrRefused struct{ Reason, Detail string }

func (e *ErrRefused) Error() string {
	if e.Detail == "" {
		return e.Reason
	}
	return e.Reason + ": " + e.Detail
}

// Refused is a refusal for a fixed reason.
func Refused(reason string) error { return &ErrRefused{Reason: reason} }

// refusedf is a refusal for a fixed reason with the detail of what was
// found, for the log.
func refusedf(reason, format string, args ...any) error {
	return &ErrRefused{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// Provider is the OIDC provider of one client, with its caches.
type Provider struct {
	cfg    Config
	client *http.Client

	mu     sync.Mutex
	disc   *Discovery
	discAt time.Time
	keys   []verifyKey
	keysAt time.Time // the last fetch of the key set, successful or not
}

// New returns the provider of cfg's client.
func New(cfg Config) *Provider {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: providerTimeout}
	}
	return &Provider{cfg: cfg, client: client}
}

// now is the clock of the token checks and the caches.
func (p *Provider) now() time.Time {
	if p.cfg.Now != nil {
		return p.cfg.Now()
	}
	return time.Now()
}

// Discovery returns the discovery document, fetched lazily and cached for
// an hour. Its issuer must be the configured one, exactly.
func (p *Provider) Discovery(ctx context.Context) (*Discovery, error) {
	p.mu.Lock()
	if p.disc != nil && p.now().Sub(p.discAt) < discoveryTTL {
		d := p.disc
		p.mu.Unlock()
		return d, nil
	}
	p.mu.Unlock()
	u := strings.TrimSuffix(p.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	var d Discovery
	if err := p.getJSON(ctx, u, "", &d); err != nil {
		return nil, providerErr("discovery: %w", err)
	}
	if d.Issuer != p.cfg.Issuer {
		return nil, providerErr("discovery names the issuer %q, not %q", d.Issuer, p.cfg.Issuer)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.JWKSURI == "" {
		return nil, providerErr("discovery lacks the authorization, token or JWKS endpoint")
	}
	p.mu.Lock()
	p.disc, p.discAt = &d, p.now()
	p.mu.Unlock()
	return &d, nil
}

// keySet returns the provider's keys, fetched when there are none. With
// refresh (a token of an unknown key id: the provider may have rotated),
// the set is fetched again unless that happened less than a minute ago.
func (p *Provider) keySet(ctx context.Context, d *Discovery, refresh bool) ([]verifyKey, error) {
	p.mu.Lock()
	keys, at := p.keys, p.keysAt
	p.mu.Unlock()
	if len(keys) > 0 && (!refresh || p.now().Sub(at) < jwksRefetchEvery) {
		return keys, nil
	}
	var raw json.RawMessage
	err := p.getJSON(ctx, d.JWKSURI, "", &raw)
	if err == nil {
		var parsed []verifyKey
		if parsed, err = parseKeySet(raw); err == nil {
			keys = parsed
		}
	}
	p.mu.Lock()
	p.keysAt = p.now()
	if err == nil {
		p.keys = keys
	}
	p.mu.Unlock()
	if err != nil {
		return keys, providerErr("key set: %w", err)
	}
	return keys, nil
}

// getJSON fetches u (with a bearer when given) and decodes its JSON.
func (p *Provider) getJSON(ctx context.Context, u, bearer string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("GET %s: not JSON: %w", u, err)
	}
	return nil
}

// Tokens is the token endpoint's answer.
type Tokens struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	TokenType   string `json:"token_type"`
}

// Exchange trades an authorization code for tokens (client_secret_basic,
// PKCE verifier).
func (p *Provider) Exchange(ctx context.Context, d *Discovery, code, redirectURI, verifier string) (Tokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, providerErr("token endpoint: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// RFC 6749 §2.3.1: the client id and secret are form-encoded first.
	req.SetBasicAuth(url.QueryEscape(p.cfg.ClientID), url.QueryEscape(p.cfg.ClientSecret))
	resp, err := p.client.Do(req)
	if err != nil {
		return Tokens{}, providerErr("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	if err != nil {
		return Tokens{}, providerErr("token endpoint: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error != "" {
			return Tokens{}, providerErr("the token endpoint refused the code: %s: %s", e.Error, e.Description)
		}
		return Tokens{}, providerErr("token endpoint: HTTP %d", resp.StatusCode)
	}
	var t Tokens
	if err := json.Unmarshal(body, &t); err != nil {
		return Tokens{}, providerErr("token endpoint: not JSON: %w", err)
	}
	if t.IDToken == "" {
		return Tokens{}, providerErr("the token endpoint returned no ID token")
	}
	return t, nil
}

// Claims is a JSON object of claims, read field by field and tolerant of
// their types where providers differ.
type Claims map[string]json.RawMessage

// Str returns a string claim ("" when absent or not a string).
func (c Claims) Str(name string) string {
	var s string
	if raw, ok := c[name]; ok && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// Strs returns a claim that is an array of strings or one string.
func (c Claims) Strs(name string) []string {
	raw, ok := c[name]
	if !ok {
		return nil
	}
	var ss []string
	if json.Unmarshal(raw, &ss) == nil {
		return ss
	}
	if s := c.Str(name); s != "" {
		return []string{s}
	}
	return nil
}

// num returns a numeric claim (seconds since the epoch for times).
func (c Claims) num(name string) (float64, bool) {
	var f float64
	if raw, ok := c[name]; ok && json.Unmarshal(raw, &f) == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
		return f, true
	}
	return 0, false
}

// numText returns a numeric claim as its text (github_id), or a string
// claim as it is.
func (c Claims) numText(name string) string {
	if f, ok := c.num(name); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return c.Str(name)
}

// IDToken is a verified ID token: the token as received and its claims.
type IDToken struct {
	Raw    string
	Claims Claims
}

// Verify checks an ID token (§11): its signature by the provider's keys
// (the key set fetched again once for an unknown key id), iss exactly the
// issuer, aud holding the client id, azp (when present) the client id,
// exp not more than 60 s past, iat not more than 60 s ahead, nonce the
// sign-in's, and a sub.
func (p *Provider) Verify(ctx context.Context, d *Discovery, raw, nonce string) (IDToken, error) {
	keys, err := p.keySet(ctx, d, false)
	if err != nil {
		return IDToken{}, err
	}
	payload, err := verifyJWS(raw, keys)
	if errors.Is(err, errUnknownKey) {
		if keys, err = p.keySet(ctx, d, true); err != nil {
			return IDToken{}, err
		}
		payload, err = verifyJWS(raw, keys)
	}
	if err != nil {
		return IDToken{}, err
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return IDToken{}, Refused("the token's claims are not a JSON object")
	}
	if iss := c.Str("iss"); iss != p.cfg.Issuer {
		return IDToken{}, refusedf("the token's issuer is not the configured one", "%q is not %q", iss, p.cfg.Issuer)
	}
	if !slices.Contains(c.Strs("aud"), p.cfg.ClientID) {
		return IDToken{}, Refused("the token is not for this client (aud)")
	}
	if azp := c.Str("azp"); azp != "" && azp != p.cfg.ClientID {
		return IDToken{}, Refused("the token was issued to another party (azp)")
	}
	now := p.now()
	exp, ok := c.num("exp")
	if !ok || !now.Add(-clockSkew).Before(unixSeconds(exp)) {
		return IDToken{}, Refused("the token has expired")
	}
	if iat, ok := c.num("iat"); !ok || !unixSeconds(iat).Before(now.Add(clockSkew)) {
		return IDToken{}, Refused("the token is issued in the future")
	}
	if c.Str("nonce") != nonce || nonce == "" {
		return IDToken{}, Refused("the token's nonce is not the sign-in's")
	}
	if c.Str("sub") == "" {
		return IDToken{}, Refused("the token names no subject")
	}
	return IDToken{Raw: raw, Claims: c}, nil
}

// The back-channel logout token's fixed parts (OpenID Connect Back-Channel
// Logout 1.0, §2.4).
const (
	// backchannelEvent is the member of the events claim a logout token
	// must have.
	backchannelEvent = "http://schemas.openid.net/event/backchannel-logout"
	// logoutTokenMaxAge is how old a logout token's iat may be: the
	// provider sends it as the person signs out.
	logoutTokenMaxAge = 10 * time.Minute
)

// VerifyLogout checks a back-channel logout token (§2.6): its signature by
// the provider's keys (fetched again once for an unknown key id), iss
// exactly the issuer, aud holding the client id, iat within the last ten
// minutes (and not ahead by more than the skew), exp (when present) not
// past, events holding the back-channel logout event as an object, no
// nonce, and a sid. It returns the sid and the sub ("" when absent).
func (p *Provider) VerifyLogout(ctx context.Context, d *Discovery, raw string) (sid, sub string, err error) {
	keys, err := p.keySet(ctx, d, false)
	if err != nil {
		return "", "", err
	}
	payload, err := verifyJWS(raw, keys)
	if errors.Is(err, errUnknownKey) {
		if keys, err = p.keySet(ctx, d, true); err != nil {
			return "", "", err
		}
		payload, err = verifyJWS(raw, keys)
	}
	if err != nil {
		return "", "", err
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return "", "", Refused("the token's claims are not a JSON object")
	}
	if iss := c.Str("iss"); iss != p.cfg.Issuer {
		return "", "", refusedf("the token's issuer is not the configured one", "%q is not %q", iss, p.cfg.Issuer)
	}
	if !slices.Contains(c.Strs("aud"), p.cfg.ClientID) {
		return "", "", Refused("the token is not for this client (aud)")
	}
	now := p.now()
	iat, ok := c.num("iat")
	if !ok || !unixSeconds(iat).Before(now.Add(clockSkew)) || unixSeconds(iat).Before(now.Add(-logoutTokenMaxAge)) {
		return "", "", Refused("the token's iat is not recent")
	}
	if exp, ok := c.num("exp"); ok && !now.Add(-clockSkew).Before(unixSeconds(exp)) {
		return "", "", Refused("the token has expired")
	}
	var events map[string]json.RawMessage
	if raw, ok := c["events"]; !ok || json.Unmarshal(raw, &events) != nil {
		return "", "", Refused("the token has no events")
	}
	var event map[string]json.RawMessage
	if raw, ok := events[backchannelEvent]; !ok || json.Unmarshal(raw, &event) != nil || event == nil {
		return "", "", Refused("the token is not a back-channel logout")
	}
	if _, ok := c["nonce"]; ok {
		return "", "", Refused("a logout token has no nonce")
	}
	if sid = c.Str("sid"); sid == "" {
		return "", "", Refused("the token names no session (sid)")
	}
	return sid, c.Str("sub"), nil
}

// unixSeconds converts a NumericDate.
func unixSeconds(f float64) time.Time {
	if f > float64(math.MaxInt64/int64(time.Second)) {
		return time.Unix(math.MaxInt64/int64(time.Second), 0)
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9))
}

// Userinfo fetches the userinfo of an access token, when the provider has
// the endpoint; its sub must be the ID token's.
func (p *Provider) Userinfo(ctx context.Context, d *Discovery, accessToken, sub string) (Claims, error) {
	if d.UserinfoEndpoint == "" || accessToken == "" {
		return Claims{}, nil
	}
	var c Claims
	if err := p.getJSON(ctx, d.UserinfoEndpoint, accessToken, &c); err != nil {
		return nil, providerErr("userinfo: %w", err)
	}
	if c.Str("sub") != sub {
		return nil, Refused("userinfo names another subject than the ID token")
	}
	return c, nil
}

// Person is who signed in: the claims of userinfo, falling back to the ID
// token's.
type Person struct {
	Sub, Email, Name, Username, Picture, GitHubID string
	Groups                                        []string
}

// PersonOf is the person of an ID token's and userinfo's claims.
func PersonOf(id, info Claims) Person {
	pick := func(name string) string {
		if v := info.Str(name); v != "" {
			return v
		}
		return id.Str(name)
	}
	groups := info.Strs("groups")
	if _, ok := info["groups"]; !ok {
		groups = id.Strs("groups")
	}
	gh := info.numText("github_id")
	if gh == "" {
		gh = id.numText("github_id")
	}
	return Person{
		Sub: id.Str("sub"), Email: pick("email"), Name: pick("name"), Username: pick("preferred_username"),
		Picture: pick("picture"), GitHubID: gh, Groups: groups,
	}
}
