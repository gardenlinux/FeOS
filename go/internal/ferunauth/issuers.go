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
	// failedTTL is how long a failed discovery is answered from memory:
	// an issuer that does not answer costs a fetch at most this often,
	// whoever sends browsers to it.
	failedTTL = 30 * time.Second
	// maxFlights bounds the sign-ins talking to one issuer at once (its
	// discovery, token, key set and userinfo fetches); one more gets the
	// 502 page at once rather than a socket and a goroutine for as long
	// as the issuer stalls.
	maxFlights = 8
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
	// failed are the issuers whose last discovery failed, with when, at
	// most maxIssuers; flights count the sign-ins at each issuer.
	failed  map[string]failure
	flights map[string]int
}

// failure is a failed discovery: when, and its error.
type failure struct {
	at  time.Time
	err error
}

func newIssuers(client *http.Client, scopes []string, now func() time.Time) *issuers {
	return &issuers{client: client, scopes: scopes, now: now, entries: map[issuerKey]*issuerEntry{},
		failed: map[string]failure{}, flights: map[string]int{}}
}

// errBusy is an issuer with maxFlights sign-ins in flight.
var errBusy = errors.New("too many sign-ins are talking to the issuer at once")

// enter takes one of an issuer's flights; done gives it back. ok is false
// when all are taken.
func (c *issuers) enter(issuer string) (done func(), ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.flights[issuer] >= maxFlights {
		return nil, false
	}
	c.flights[issuer]++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.flights[issuer]--; c.flights[issuer] <= 0 {
				delete(c.flights, issuer)
			}
		})
	}, true
}

// discover is discover through the cache of failures: an issuer whose
// discovery failed less than failedTTL ago gets that error again without
// a fetch.
func (c *issuers) discover(ctx context.Context, rp relyingParty) (*oidc.Discovery, error) {
	now := c.now()
	c.mu.Lock()
	f, ok := c.failed[rp.issuer]
	c.mu.Unlock()
	if ok && now.Sub(f.at) < failedTTL {
		return nil, f.err
	}
	d, err := discover(ctx, rp.p)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		delete(c.failed, rp.issuer)
		return d, nil
	}
	if ctx.Err() != nil {
		// The browser left or the callback's time ran out: that says
		// nothing of the issuer.
		return nil, err
	}
	for k, old := range c.failed {
		if now.Sub(old.at) >= failedTTL {
			delete(c.failed, k)
		}
	}
	if len(c.failed) >= maxIssuers {
		var oldest string
		var at time.Time
		for k, old := range c.failed {
			if at.IsZero() || old.at.Before(at) {
				oldest, at = k, old.at
			}
		}
		delete(c.failed, oldest)
	}
	c.failed[rp.issuer] = failure{at: now, err: err}
	return nil, err
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
