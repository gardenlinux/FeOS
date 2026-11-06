// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The realm of the Basic and Bearer challenges.
const realm = "ferun"

// The cookies of human sign-in.
const (
	cookieSession = "ferun_session"
	cookieLogin   = "ferun_login"
)

// The header a request authenticated by a session cookie must carry
// unless it is a GET, HEAD or OPTIONS (a page of another origin cannot set
// it without CORS, which ferund never grants).
const (
	csrfHeader = "X-Requested-With"
	csrfValue  = "ferun"
)

// apiHandler is a handler that may fail: an error is the answer
// (apierr.Write), so that the 4xx and 5xx texts live at their causes.
type apiHandler func(http.ResponseWriter, *http.Request) error

// serve adapts a plain handler.
func serve(h func(http.ResponseWriter, *http.Request)) apiHandler {
	return func(w http.ResponseWriter, r *http.Request) error {
		h(w, r)
		return nil
	}
}

// wrap turns an apiHandler into an http.Handler; a 5xx answer is logged.
func (s *server) wrap(h apiHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			var e *apierr.Error
			if !errors.As(err, &e) || e.Status >= 500 {
				s.log.Warn("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
			}
			writeError(w, err)
		}
	})
}

// writeError answers err as the API does (apierr.Write).
func writeError(w http.ResponseWriter, err error) { apierr.Write(w, err) }

// unavailableText is the answer of a route whose service this instance
// does not run.
const unavailableText = "not available on this instance"

func errUnavailable() error { return apierr.New(http.StatusServiceUnavailable, unavailableText) }

func errNoRoute(r *http.Request) error {
	return apierr.New(http.StatusNotFound, "no such route: %s %s", r.Method, r.URL.Path)
}

// bearer returns the token of "Authorization: Bearer <token>" (the prefix
// exact, the token trimmed), or "".
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(h[len("Bearer "):])
}

// parseBasic returns the user and password of "Authorization: Basic …".
func parseBasic(h string) (user, password string, ok bool) {
	if !strings.HasPrefix(h, "Basic ") {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len("Basic "):]))
	if err != nil {
		return "", "", false
	}
	return strings.Cut(string(raw), ":")
}

// csvPasteHint names the one mistake Basic sign-in produces in the field:
// --admin-users is a comma-separated list of user:password pairs, and
// pasting all of it into `curl -u` sends the rest of the list as the
// password.
func csvPasteHint(password string) string {
	if i := strings.Index(password, ","); i >= 0 && strings.Contains(password[i+1:], ":") {
		return " — the password looks like the rest of the --admin-users list; " +
			"that value holds comma-separated user:password pairs, pass exactly one to curl -u"
	}
	return ""
}

// basicAdmin checks the request's Basic credentials against --admin-users.
// present is false when Basic is off or the request carries none; then
// the other ways apply.
func (s *server) basicAdmin(r *http.Request) (a auth.Admin, present bool, err error) {
	h := r.Header.Get("Authorization")
	if len(s.cfg.AdminUsers) == 0 || !strings.HasPrefix(h, "Basic ") {
		return auth.Admin{}, false, nil
	}
	u, p, ok := parseBasic(h)
	if !ok {
		return auth.Admin{}, true, apierr.New(http.StatusUnauthorized,
			"admin basic auth failed: the Authorization header is not Basic <base64 user:password>")
	}
	want, known := s.cfg.AdminUsers[u]
	if !known || want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(p)) != 1 {
		return auth.Admin{}, true, apierr.New(http.StatusUnauthorized, "admin basic auth failed for user %q%s", u, csvPasteHint(p))
	}
	return auth.Admin{User: u, Method: auth.AdminBasic}, true, nil
}

// session resolves the request's session cookie to a signed-in person,
// an operator or not. ok is false when there is no cookie, or no valid
// session of it (expired, ended, or its person disabled).
func (s *server) session(r *http.Request) (store.UISession, bool, error) {
	if s.oidc == nil || s.st == nil {
		return store.UISession{}, false, nil
	}
	c, err := r.Cookie(cookieSession)
	if err != nil || c.Value == "" {
		return store.UISession{}, false, nil
	}
	hash := store.HashToken(c.Value)
	sess, err := s.st.UISessionOf(r.Context(), hash)
	if errors.Is(err, store.ErrNoRow) {
		return store.UISession{}, false, nil
	}
	if err != nil {
		return store.UISession{}, false, fmt.Errorf("session lookup: %w", err)
	}
	if s.touches.due("u:" + hash) {
		if err := s.st.TouchUISession(r.Context(), hash); err != nil {
			s.log.Warn("touch a UI session", "err", err)
		}
	}
	return sess, true, nil
}

// adminOf is the identity of a signed-in admin.
func adminOf(sess store.UISession) auth.Admin {
	user := sess.Username
	if user == "" {
		user = sess.Email
	}
	return auth.Admin{User: user, Sub: sess.Sub, Method: auth.AdminOIDC, Person: sess.PersonID}
}

// checkCSRF refuses a request authenticated by a cookie that a page of
// another origin could have sent: a method other than GET, HEAD and
// OPTIONS needs the X-Requested-With header, and an Origin, when sent,
// must be the public URL's.
func (s *server) checkCSRF(r *http.Request) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	if r.Header.Get(csrfHeader) != csrfValue {
		return apierr.New(http.StatusForbidden, "missing %s header", csrfHeader)
	}
	if o := r.Header.Get("Origin"); o != "" && !strings.EqualFold(o, s.origin) {
		return apierr.New(http.StatusForbidden, "cross-origin request refused")
	}
	return nil
}

// basicCrossOrigin is net/http's cross-origin check: Sec-Fetch-Site, or
// without it Origin against Host. Its zero value trusts no origin;
// checkBasicOrigin adds the public URL's.
var basicCrossOrigin http.CrossOriginProtection

// checkBasicOrigin refuses a request with Basic credentials that a page of
// another origin could have sent. A browser caches the credentials typed
// into its own dialog (challengeBasic) and attaches them to every request
// to the origin, a cross-site form's POST among them. So a method other
// than GET, HEAD and OPTIONS is refused when the browser marks it as
// cross-origin: Sec-Fetch-Site other than same-origin or none, or,
// without it, an Origin that is neither the Host's nor the public URL's.
// A script (curl -u) sends neither header and passes; so does the UI,
// whose requests are same-origin.
func (s *server) checkBasicOrigin(r *http.Request) error {
	if err := basicCrossOrigin.Check(r); err != nil {
		if o := r.Header.Get("Origin"); s.origin != "" && strings.EqualFold(o, s.origin) {
			return nil
		}
		return apierr.New(http.StatusForbidden, "cross-origin request refused")
	}
	return nil
}

// admin is the admin class: an admin's UI session, or Basic credentials of
// --admin-users, which are checked first and win when present.
func (s *server) admin(h apiHandler) http.Handler {
	return s.wrap(func(w http.ResponseWriter, r *http.Request) error {
		if len(s.cfg.AdminUsers) == 0 && s.oidc == nil {
			return apierr.New(http.StatusServiceUnavailable, "no admin sign-in configured (--oidc-issuer or --admin-users)")
		}
		a, present, err := s.basicAdmin(r)
		if present {
			if err != nil {
				s.challengeBasic(w, r)
				return err
			}
			if err := s.checkBasicOrigin(r); err != nil {
				return err
			}
			return h(w, r.WithContext(auth.WithAdmin(r.Context(), a)))
		}
		sess, ok, err := s.session(r)
		if err != nil {
			return err
		}
		if ok && sess.Admin {
			if err := s.checkCSRF(r); err != nil {
				return err
			}
			return h(w, r.WithContext(auth.WithAdmin(r.Context(), adminOf(sess))))
		}
		if ok {
			// A person who is no operator: signed in, but not for this.
			return apierr.New(http.StatusForbidden, "operators only")
		}
		s.challengeBasic(w, r)
		return apierr.New(http.StatusUnauthorized, "admin sign-in required")
	})
}

// challengeBasic sets the Basic challenge of an admin route's 401, which
// makes a browser show its own login dialog: only where Basic is a way in
// (--admin-users), and never to the UI, which marks every request with
// X-Requested-With: ferun and shows its own sign-in screen on a 401 (a
// session that expired must not bring up the browser's dialog over it).
func (s *server) challengeBasic(w http.ResponseWriter, r *http.Request) {
	if len(s.cfg.AdminUsers) == 0 || r.Header.Get(csrfHeader) == csrfValue {
		return
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`"`)
}

// tenant is the tenant class: the bearer is a tenant's token.
func (s *server) tenant(h apiHandler) http.Handler {
	return s.wrap(func(w http.ResponseWriter, r *http.Request) error {
		tok := bearer(r)
		if tok == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`"`)
			return apierr.New(http.StatusUnauthorized, "tenant bearer required")
		}
		hash := store.HashToken(tok)
		name, err := s.st.ResolveTenantToken(r.Context(), hash)
		if errors.Is(err, model.ErrUnknownToken) {
			return apierr.New(http.StatusUnauthorized, "unknown tenant token")
		}
		if err != nil {
			return fmt.Errorf("token lookup: %w", err)
		}
		if s.touches.due("t:" + hash) {
			if err := s.st.TouchTenantToken(r.Context(), hash); err != nil {
				s.log.Warn("touch a tenant token", "err", err)
			}
		}
		return h(w, r.WithContext(auth.WithTenant(r.Context(), auth.Tenant{Name: name, Token: tok})))
	})
}

// sandbox is the sandbox class: the bearer is a sandbox's token, and the
// {id} of the path is a claim the token must back. A token of another
// sandbox, a tenant's token and an unknown one all get the answer of an
// absent sandbox.
func (s *server) sandbox(h apiHandler) http.Handler {
	return s.wrap(func(w http.ResponseWriter, r *http.Request) error {
		tok := bearer(r)
		if tok == "" {
			return apierr.New(http.StatusUnauthorized, "sandbox token required")
		}
		sb, err := ResolveSandboxToken(r.Context(), s.st.Q(), tok)
		if errors.Is(err, model.ErrUnknownToken) {
			return apierr.New(http.StatusNotFound, "no such sandbox")
		}
		if err != nil {
			return fmt.Errorf("token lookup: %w", err)
		}
		if id, err := podid.Parse(r.PathValue("id")); err != nil || id != sb.ID {
			return apierr.New(http.StatusNotFound, "no such sandbox")
		}
		if s.touches.due("s:" + sb.TokenHash) {
			if err := s.st.TouchSandboxToken(r.Context(), sb.TokenHash); err != nil {
				s.log.Warn("touch a sandbox token", "err", err)
			}
		}
		return h(w, r.WithContext(auth.WithSandbox(r.Context(), sb)))
	})
}

// ResolveSandboxToken resolves a bearer for the mcp and forward classes
// (and the sandbox class): the sandbox it is a token of, ignoring a tenant
// being deleted; model.ErrUnknownToken when it is none.
func ResolveSandboxToken(ctx context.Context, q db.Querier, bearer string) (auth.Sandbox, error) {
	if bearer == "" {
		return auth.Sandbox{}, model.ErrUnknownToken
	}
	hash := store.HashToken(bearer)
	id, owner, err := store.On(q).ResolveSandboxToken(ctx, hash)
	if err != nil {
		return auth.Sandbox{}, err
	}
	return auth.Sandbox{ID: id, Owner: owner, TokenHash: hash}, nil
}

// throttle lets a key through at most once per interval: the token
// touches, which would otherwise be a write per request.
type throttle struct {
	every time.Duration
	mu    sync.Mutex
	last  map[string]time.Time
}

// throttleKeys bounds the keys a throttle remembers; past it, those older
// than the interval are forgotten.
const throttleKeys = 4096

func newThrottle(every time.Duration) *throttle {
	return &throttle{every: every, last: map[string]time.Time{}}
}

// due reports whether key may act now, and if so records it.
func (t *throttle) due(key string) bool {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.last[key]; ok && now.Sub(last) < t.every {
		return false
	}
	if len(t.last) >= throttleKeys {
		for k, v := range t.last {
			if now.Sub(v) >= t.every {
				delete(t.last, k)
			}
		}
	}
	t.last[key] = now
	return true
}
