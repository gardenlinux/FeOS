// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ferunauth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gardenlinux/feos/go/internal/oidc"
)

// The issuer cache's bounds.
const (
	maxIssuers = 256
	issuerTTL  = time.Hour
)

// issuerKey names a client at an issuer.
type issuerKey struct{ issuer, clientID string }

type issuerEntry struct {
	p      *oidc.Provider
	secret string
	at     time.Time
}

// issuers caches the relying party of each client at each issuer, so that
// their discovery documents and key sets are fetched once an hour, not at
// every sign-in. Every one talks through the ruled client.
type issuers struct {
	client *http.Client
	scopes []string
	now    func() time.Time

	mu      sync.Mutex
	entries map[issuerKey]*issuerEntry
}

func newIssuers(client *http.Client, scopes []string, now func() time.Time) *issuers {
	return &issuers{client: client, scopes: scopes, now: now, entries: map[issuerKey]*issuerEntry{}}
}

// get returns the relying party of a client. An entry older than an hour,
// or one whose secret changed, is made again; at the bound the oldest
// entry gives way.
func (c *issuers) get(issuer, clientID, secret string) *oidc.Provider {
	k := issuerKey{issuer, clientID}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[k]; ok && e.secret == secret && now.Sub(e.at) < issuerTTL {
		return e.p
	}
	delete(c.entries, k)
	if len(c.entries) >= maxIssuers {
		var oldest issuerKey
		var at time.Time
		for key, e := range c.entries {
			if at.IsZero() || e.at.Before(at) {
				oldest, at = key, e.at
			}
		}
		delete(c.entries, oldest)
	}
	p := oidc.New(oidc.Config{Issuer: issuer, ClientID: clientID, ClientSecret: secret, Scopes: c.scopes,
		HTTPClient: c.client, Now: c.now})
	c.entries[k] = &issuerEntry{p: p, secret: secret, at: now}
	return p
}

// errEndpoint is a discovery document naming an endpoint that is not an
// absolute https URL.
var errEndpoint = errors.New("the issuer's discovery names an endpoint that is not an https URL")

// discover returns an issuer's discovery document, every endpoint ferun-auth
// uses an absolute https URL.
func discover(ctx context.Context, p *oidc.Provider) (*oidc.Discovery, error) {
	d, err := p.Discovery(ctx)
	if err != nil {
		return nil, err
	}
	for _, e := range []string{d.AuthorizationEndpoint, d.TokenEndpoint, d.JWKSURI} {
		if !isHTTPS(e) {
			return nil, errEndpoint
		}
	}
	if d.UserinfoEndpoint != "" && !isHTTPS(d.UserinfoEndpoint) {
		return nil, errEndpoint
	}
	return d, nil
}

// isHTTPS reports whether s is an absolute https URL with a host and no
// user information.
func isHTTPS(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.Hostname() != "" && u.User == nil && u.Opaque == ""
}
