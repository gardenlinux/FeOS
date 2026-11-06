// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gardenlinux/feos/go/internal/auth"
)

// pathTenant is ferund's call that names a reader's tenant and what it
// may read of it; askTenant asks it as a sink that filters by environment
// (ferund answers a reader of environments only no other).
const (
	pathTenant = "/api/v1/tenant"
	askTenant  = pathTenant + "?filter=environments"
)

// tenantCacheMax bounds the cached readers; beyond it the expired go, and
// then all.
const tenantCacheMax = 10000

// sessionTTL bounds how long a session's answer is kept: a person signs
// out, or loses a grant, more often than a token is revoked.
const sessionTTL = 10 * time.Second

// Credential is how a reader asks: a token (Authorization: Bearer …), or
// a person's session (its cookie's value) served under ferund's host; and
// the tenant it names in FeRun-Tenant (a session must, a token may).
type Credential struct {
	Authorization string
	Session       string
	Tenant        string
}

// key is the credential's cache key: the SHA-256 of what ferund is asked
// with.
func (c Credential) key() [32]byte {
	kind := "b"
	v := c.Authorization
	if v == "" {
		kind, v = "s", c.Session
	}
	return sha256.Sum256([]byte(kind + "\x00" + v + "\x00" + c.Tenant))
}

// Access is what a reader may read, as ferund answers GET /api/v1/tenant
// (ADR 0031): of Tenant, with All (a role on the tenant) every stream and
// span, else only those whose environment is one of Envs (its grants on
// environments); a stream or span of no environment (a sandbox's, a
// tenant's job's) needs All. Write (write on the tenant) may delete
// streams.
type Access struct {
	Tenant string
	All    bool
	Envs   []string
	Write  bool
}

// none reports a reader that may read nothing.
func (a Access) none() bool { return !a.All && len(a.Envs) == 0 }

// mayRead reports whether the reader may read the environment env's
// streams and spans ("": those of no environment).
func (a Access) mayRead(env string) bool { return a.All || (env != "" && slices.Contains(a.Envs, env)) }

// within is the environments a selection is bound to: nil for All (no
// bound), else Envs (never nil).
func (a Access) within() []string {
	if a.All {
		return nil
	}
	return append([]string{}, a.Envs...)
}

// Tenants resolves a reader's credential to its Access through ferund
// (GET /api/v1/tenant?filter=environments with the same Authorization,
// or the session's cookie, and FeRun-Tenant) and keeps the answer for TTL (a session's for
// at most sessionTTL), by the SHA-256 of the credential. Refusals are not
// kept.
type Tenants struct {
	ferunURL string
	hc       *http.Client
	ttl      time.Duration
	now      func() time.Time

	mu sync.Mutex
	m  map[[32]byte]tenantEntry
}

type tenantEntry struct {
	access  Access
	expires time.Time
}

// NewTenants returns the resolver of ferund at ferunURL.
func NewTenants(ferunURL string, hc *http.Client, ttl time.Duration) *Tenants {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Tenants{ferunURL: strings.TrimSuffix(ferunURL, "/"), hc: hc, ttl: ttl, now: time.Now, m: map[[32]byte]tenantEntry{}}
}

// refusal is ferund's answer that is not a tenant, passed on as it came.
type refusal struct {
	status      int
	contentType string
	body        []byte
}

func (r *refusal) Error() string {
	return fmt.Sprintf("ferund answered %d: %s", r.status, strings.TrimSpace(string(r.body)))
}

// tenantAnswer is ferund's answer: role and environments are left out by
// a ferund of before people (ADR 0031), whose every token was its
// tenant's.
type tenantAnswer struct {
	Tenant       string   `json:"tenant"`
	Role         *string  `json:"role"`
	Environments []string `json:"environments"`
}

// access is the answer as an Access.
func (a tenantAnswer) access() Access {
	if a.Role == nil {
		return Access{Tenant: a.Tenant, All: true, Write: true}
	}
	switch *a.Role {
	case "read":
		return Access{Tenant: a.Tenant, All: true}
	case "write", "admin":
		return Access{Tenant: a.Tenant, All: true, Write: true}
	}
	envs := slices.DeleteFunc(slices.Clone(a.Environments), func(e string) bool { return e == "" })
	return Access{Tenant: a.Tenant, Envs: envs}
}

// Resolve returns what the reader of c may read. A 4xx of ferund (401
// unknown, 404 no such tenant, 400) comes back as a *refusal; any other
// failure as an error.
func (t *Tenants) Resolve(ctx context.Context, c Credential) (Access, error) {
	h := c.key()
	now := t.now()
	t.mu.Lock()
	e, ok := t.m[h]
	t.mu.Unlock()
	if ok && now.Before(e.expires) {
		return e.access, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.ferunURL+askTenant, nil)
	if err != nil {
		return Access{}, err
	}
	ttl := t.ttl
	if c.Authorization != "" {
		req.Header.Set("Authorization", c.Authorization)
	} else {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: c.Session})
		ttl = min(ttl, sessionTTL)
	}
	if c.Tenant != "" {
		req.Header.Set(auth.TenantHeader, c.Tenant)
	}
	resp, err := t.hc.Do(req)
	if err != nil {
		return Access{}, fmt.Errorf("ask ferund for the tenant: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return Access{}, fmt.Errorf("ask ferund for the tenant: %w", err)
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
		return Access{}, &refusal{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: b}
	}
	if resp.StatusCode != http.StatusOK {
		return Access{}, fmt.Errorf("ask ferund for the tenant: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var a tenantAnswer
	if err := json.Unmarshal(b, &a); err != nil || a.Tenant == "" {
		return Access{}, fmt.Errorf("ask ferund for the tenant: no tenant in %q", b)
	}
	acc := a.access()
	t.mu.Lock()
	if len(t.m) >= tenantCacheMax {
		for k, v := range t.m {
			if !now.Before(v.expires) {
				delete(t.m, k)
			}
		}
		if len(t.m) >= tenantCacheMax {
			clear(t.m)
		}
	}
	t.m[h] = tenantEntry{access: acc, expires: now.Add(ttl)}
	t.mu.Unlock()
	return acc, nil
}
