// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/store"
)

// This file is human sign-in (§11 "Human sign-in", ADR 0031): people sign
// in to the UI and the API through the OIDC provider FeCode, FeRun and
// FeChat share. Everyone the provider lets in is a person (people) and gets
// a session, kept in ui_sessions and named by the cookie
// __Host-ferun_session (auth.SessionCookie); an
// operator's (the provider's groups claim holds --oidc-admin-group) is
// marked admin. A sign-in under way lives in auth_logins, so that it may
// complete on another instance than it began on. The provider's
// back-channel logout ends the sessions of its sid. Machine credentials
// (tenant and sandbox tokens, /mcp) are not touched by any of it.

// loginCookieAge is the life of the ferun_login cookie, the sign-in's
// limit.
const loginCookieAge = 600

// signInRoutes adds the routes of /auth/: the configuration, /auth/me and
// the sign-out always (the UI signs out the same way whatever signed it
// in), the OIDC flow only with OIDC (else they are unknown routes).
func (s *server) signInRoutes(public func(string, http.Handler)) {
	public("GET /auth/config", s.wrap(s.authConfig))
	public("GET /auth/me", s.wrap(s.authMe))
	public("POST /auth/logout", s.wrap(s.logout))
	if s.oidc != nil {
		public("GET /auth/login", http.HandlerFunc(s.login))
		public("GET /auth/callback", http.HandlerFunc(s.callback))
		public("POST /auth/backchannel-logout", http.HandlerFunc(s.backchannelLogout))
	}
}

// authConfig is GET /auth/config: the ways to sign in.
func (s *server) authConfig(w http.ResponseWriter, _ *http.Request) error {
	return writeJSON(w, http.StatusOK, struct {
		OIDC  bool `json:"oidc"`
		Basic bool `json:"basic"`
	}{s.oidc != nil, len(s.cfg.AdminUsers) > 0})
}

// meTenant is a tenant of /auth/me: the person's role on it ("" with
// grants on environments alone) and their grants on its environments.
type meTenant struct {
	Tenant       string          `json:"tenant"`
	Role         string          `json:"role"`
	Environments []meEnvironment `json:"environments"`
}

type meEnvironment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// myTenants lists a session's tenants: an operator's are every tenant, as
// admin; a person's those of their grants.
func (s *server) myTenants(r *http.Request, sess store.UISession) ([]meTenant, error) {
	out := []meTenant{}
	if sess.Admin {
		names, err := s.st.LiveTenantNames(r.Context())
		if err != nil {
			return nil, internalf("tenants: %v", err)
		}
		for _, n := range names {
			out = append(out, meTenant{Tenant: n, Role: auth.RoleAdmin.String(), Environments: []meEnvironment{}})
		}
		return out, nil
	}
	if sess.PersonID == uuid.Nil {
		return out, nil
	}
	gs, err := s.st.PersonGrants(r.Context(), sess.PersonID)
	if err != nil {
		return nil, internalf("grants: %v", err)
	}
	for _, g := range gs {
		if len(out) == 0 || out[len(out)-1].Tenant != g.Tenant {
			out = append(out, meTenant{Tenant: g.Tenant, Environments: []meEnvironment{}})
		}
		t := &out[len(out)-1]
		if g.EnvironmentID == uuid.Nil {
			t.Role = g.Role
		} else {
			t.Environments = append(t.Environments, meEnvironment{ID: g.EnvironmentID.String(), Name: g.EnvironmentName,
				Role: g.Role})
		}
	}
	return out, nil
}

// authMe is GET /auth/me: who is signed in, whether an operator, and the
// person's tenants with their roles. Basic credentials that pass win, as
// on the admin routes; then the session.
func (s *server) authMe(w http.ResponseWriter, r *http.Request) error {
	if a, present, err := s.basicAdmin(r); present && err == nil {
		return writeJSON(w, http.StatusOK, struct {
			Username string `json:"username"`
			Admin    bool   `json:"admin"`
			Method   string `json:"method"`
		}{a.User, true, a.Method})
	}
	sess, ok, err := s.session(r)
	if err != nil {
		return err
	}
	if !ok {
		return apierr.New(http.StatusUnauthorized, "not signed in")
	}
	tenants, err := s.myTenants(r, sess)
	if err != nil {
		return err
	}
	person := ""
	if sess.PersonID != uuid.Nil {
		person = sess.PersonID.String()
	}
	return writeJSON(w, http.StatusOK, struct {
		Sub      string     `json:"sub"`
		Person   string     `json:"person"`
		Email    string     `json:"email"`
		Name     string     `json:"name"`
		Username string     `json:"username"`
		Picture  string     `json:"picture"`
		Admin    bool       `json:"admin"`
		Method   string     `json:"method"`
		Tenants  []meTenant `json:"tenants"`
	}{sess.Sub, person, sess.Email, sess.Name, sess.Username, sess.Picture, sess.Admin, auth.AdminOIDC, tenants})
}

// randomToken returns 32 random bytes, base64url without padding.
func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// localPath returns next when it is a path of this site, else "/": it
// starts with one "/" (not "//", which browsers read as another host),
// holds no backslash and no control characters. Browsers read a backslash
// as a slash, and the redirect cleans the path ("/./\evil" becomes
// "/\evil", another host), so a backslash anywhere is refused.
func localPath(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsRune(next, '\\') {
		return "/"
	}
	for _, c := range next {
		if c < 0x20 || c == 0x7f {
			return "/"
		}
	}
	return next
}

// redirectURI is where the provider sends the browser back to.
func (s *server) redirectURI() string { return s.cfg.PublicURL + "/auth/callback" }

// cookie makes a cookie of the sign-in: HttpOnly, SameSite=Lax, Secure
// with an https public URL. maxAge < 0 clears it.
func (s *server) cookie(name, value, path string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: path, MaxAge: maxAge, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.secure}
}

// setSessionCookie sets the session's cookie, __Host-: Secure whatever
// the public URL (browsers take Secure cookies from http://localhost),
// Path=/, no Domain. It clears the cookie's old name
// (auth.OldSessionCookie), which a session of the release before has.
// maxAge < 0 clears both.
func (s *server) setSessionCookie(w http.ResponseWriter, value string, maxAge int) {
	c := s.cookie(cookieSession, value, "/", maxAge)
	c.Secure = true
	http.SetCookie(w, c)
	http.SetCookie(w, s.cookie(auth.OldSessionCookie, "", "/", -1))
}

// login is GET /auth/login?next=<path>: a new sign-in (state, nonce and a
// PKCE verifier), recorded, its state in the ferun_login cookie, and the
// browser sent to the provider.
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	next := localPath(r.URL.Query().Get("next"))
	hint := loginHint(r.URL.Query().Get("login_hint"))
	d, err := s.oidc.discovery(r.Context())
	if err != nil {
		s.log.Warn("sign-in: the provider's discovery failed", "err", err)
		page(w, http.StatusBadGateway, "Sign-in is not available",
			"The sign-in provider cannot be reached. Try again later.", "/auth/login?next="+url.QueryEscape(next), "Try again")
		return
	}
	var vals [3]string
	for i := range vals {
		if vals[i], err = randomToken(); err != nil {
			page(w, http.StatusInternalServerError, "Sign-in failed", "FeRun could not start the sign-in.", "", "")
			return
		}
	}
	state, nonce, verifier := vals[0], vals[1], vals[2]
	if err := s.st.InsertAuthLogin(r.Context(), store.AuthLogin{StateHash: store.HashToken(state), Verifier: verifier,
		Nonce: nonce, Next: next}); err != nil {
		s.log.Warn("sign-in: record the sign-in", "err", err)
		page(w, http.StatusInternalServerError, "Sign-in failed", "FeRun could not record the sign-in. Try again.",
			"/auth/login?next="+url.QueryEscape(next), "Try again")
		return
	}
	u, err := url.Parse(d.AuthorizationEndpoint)
	if err != nil {
		page(w, http.StatusBadGateway, "Sign-in is not available", "The sign-in provider's authorization endpoint is not a URL.", "", "")
		return
	}
	challenge := sha256.Sum256([]byte(verifier))
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", s.oidc.cfg.ClientID)
	q.Set("redirect_uri", s.redirectURI())
	q.Set("scope", strings.Join(s.oidc.cfg.Scopes, " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	q.Set("code_challenge_method", "S256")
	if hint != "" {
		q.Set("login_hint", hint)
	}
	u.RawQuery = q.Encode()
	http.SetCookie(w, s.cookie(cookieLogin, state, "/auth/", loginCookieAge))
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// loginHint is the address a person typed on the sign-in screen, passed on
// to the provider as OpenID Connect's login_hint so that it asks for the
// password or sends the person to their company without asking again: the
// address trimmed and lowercased, or "" for anything that is not one (at
// most 254 bytes, one "@" with something on both sides, no space or control
// character). The provider checks it again; FeRun keeps nothing of it.
func loginHint(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if len(v) > 254 || strings.Count(v, "@") != 1 || strings.HasPrefix(v, "@") || strings.HasSuffix(v, "@") {
		return ""
	}
	for _, c := range v {
		if c <= ' ' || c == 0x7f {
			return ""
		}
	}
	return v
}

// callback is GET /auth/callback: the provider's answer. The state must be
// the browser's (ferun_login) and a sign-in begun less than 10 minutes ago
// (taken once); the code is exchanged, the ID token verified, userinfo
// read, and the person recorded (people). Everyone gets a session, an
// operator's marked admin, and goes on to where the sign-in began; a
// person an operator disabled gets a page that says so, and no session.
func (s *server) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	again := "/auth/login"
	if e := q.Get("error"); e != "" {
		msg := "The sign-in provider refused the sign-in: " + e
		if d := q.Get("error_description"); d != "" {
			msg += ": " + d
		}
		s.log.Info("sign-in refused by the provider", "error", e, "description", q.Get("error_description"))
		page(w, http.StatusBadRequest, "Sign-in failed", msg, again, "Start again")
		return
	}
	state, code := q.Get("state"), q.Get("code")
	c, err := r.Cookie(cookieLogin)
	if err != nil || c.Value == "" || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		s.log.Info("sign-in: the state is not the browser's")
		page(w, http.StatusBadRequest, "Sign-in failed",
			"This sign-in did not start in this browser, or another one started since. Start again.", again, "Start again")
		return
	}
	login, err := s.st.TakeAuthLogin(r.Context(), store.HashToken(state))
	http.SetCookie(w, s.cookie(cookieLogin, "", "/auth/", -1))
	if isNoRow(err) {
		page(w, http.StatusBadRequest, "Sign-in expired", "This sign-in has expired. Start again.", again, "Start again")
		return
	}
	if err != nil {
		s.log.Warn("sign-in: read the sign-in", "err", err)
		page(w, http.StatusInternalServerError, "Sign-in failed", "FeRun could not read the sign-in. Try again.", again, "Start again")
		return
	}
	again = "/auth/login?next=" + url.QueryEscape(login.Next)
	// fail shows a short reason, the fixed text of a refusal, never the
	// token's values; the log has the whole error.
	fail := func(err error) {
		status, msg := http.StatusBadRequest, "The sign-in did not verify."
		var pe *errProvider
		var re *errRefused
		switch {
		case errors.As(err, &pe):
			status, msg = http.StatusBadGateway, "The sign-in provider could not be reached or gave an answer FeRun cannot use."
		case errors.As(err, &re):
			msg = "The sign-in did not verify: " + re.reason + "."
		}
		s.log.Warn("sign-in failed", "err", err)
		page(w, status, "Sign-in failed", msg, again, "Start again")
	}
	if code == "" {
		fail(refused("the provider sent no code"))
		return
	}
	d, err := s.oidc.discovery(r.Context())
	if err != nil {
		fail(err)
		return
	}
	toks, err := s.oidc.exchange(r.Context(), d, code, s.redirectURI(), login.Verifier)
	if err != nil {
		fail(err)
		return
	}
	idt, err := s.oidc.verify(r.Context(), d, toks.IDToken, login.Nonce)
	if err != nil {
		fail(err)
		return
	}
	info, err := s.oidc.userinfo(r.Context(), d, toks.AccessToken, idt.claims.str("sub"))
	if err != nil {
		fail(err)
		return
	}
	p := personOf(idt.claims, info)
	operator := s.oidc.cfg.AdminGroup != "" && slices.Contains(p.Groups, s.oidc.cfg.AdminGroup)
	who := p.Email
	if who == "" {
		who = p.Username
	}
	pr, err := s.st.UpsertPerson(r.Context(), s.oidc.cfg.Issuer, p.Sub, p.Email, p.Name)
	if err != nil {
		s.log.Warn("sign-in: record the person", "err", err)
		page(w, http.StatusInternalServerError, "Sign-in failed", "FeRun could not record the sign-in. Try again.", again, "Start again")
		return
	}
	if pr.Disabled() {
		s.log.Info("sign-in refused: the person is disabled", "person", pr.ID, "sub", p.Sub, "email", p.Email)
		out := "/"
		if d.EndSessionEndpoint != "" {
			out = d.EndSessionEndpoint
		}
		page(w, http.StatusForbidden, "No access", "Your account "+who+" is disabled in FeRun.", out, "Sign out")
		return
	}
	id, err := randomToken()
	if err == nil {
		_, err = s.st.InsertUISession(r.Context(), store.UISession{
			IDHash: store.HashToken(id), Sub: p.Sub, Email: p.Email, Name: p.Name, Username: p.Username,
			Picture: p.Picture, Groups: p.Groups, Admin: operator, IDToken: idt.raw, PersonID: pr.ID,
			SID: idt.claims.str("sid"),
		}, s.cfg.SessionTTL)
	}
	if err != nil {
		s.log.Warn("sign-in: record the session", "err", err)
		page(w, http.StatusInternalServerError, "Sign-in failed", "FeRun could not record the session. Try again.", again, "Start again")
		return
	}
	s.setSessionCookie(w, id, int(s.cfg.SessionTTL.Seconds()))
	s.log.Info("signed in", "person", pr.ID, "sub", p.Sub, "email", p.Email, "username", p.Username,
		"github_id", p.GitHubID, "operator", operator)
	if p.Username != "" {
		who = p.Username
	}
	s.record(r.Context(), store.ActivityRow{Kind: "sign_in", Category: activity.CategoryCredential, Name: "sign in: " + who,
		Detail: detail(map[string]any{"sub": p.Sub, "email": p.Email, "username": p.Username, "person": pr.ID.String(),
			"operator": operator})})
	http.Redirect(w, r, login.Next, http.StatusFound)
}

// logout is POST /auth/logout: the session ends (on every instance: its
// row goes) and its cookie is cleared. The answer names where the browser
// goes next: the provider's end-session endpoint, so that the person is
// signed out there too, or "/". Without OIDC no session is honoured, so
// the sign-out only clears the cookie and sends the browser home (the UI
// signs out of Basic the same way).
func (s *server) logout(w http.ResponseWriter, r *http.Request) error {
	redirect := "/"
	if session := auth.SessionOf(r); session != "" && s.oidc != nil && s.st != nil {
		if err := s.checkCSRF(r); err != nil {
			return err
		}
		sess, err := s.st.DeleteUISession(r.Context(), store.HashToken(session))
		if err != nil && !isNoRow(err) {
			return internalf("sign out: %v", err)
		}
		if sess.IDToken != "" {
			if d, err := s.oidc.discovery(r.Context()); err == nil && d.EndSessionEndpoint != "" {
				if u, err := url.Parse(d.EndSessionEndpoint); err == nil {
					q := u.Query()
					q.Set("id_token_hint", sess.IDToken)
					q.Set("post_logout_redirect_uri", s.cfg.PublicURL+"/")
					u.RawQuery = q.Encode()
					redirect = u.String()
				}
			}
		}
	}
	s.setSessionCookie(w, "", -1)
	return writeJSON(w, http.StatusOK, struct {
		Redirect string `json:"redirect"`
	}{redirect})
}

// backchannelLogout is POST /auth/backchannel-logout, the provider's
// back-channel logout (OpenID Connect Back-Channel Logout 1.0): the form's
// logout_token, verified (verifyLogout), ends every session of its sid
// (and sub, when it names one) on every instance, as their rows go. 200
// with an empty body; 400 {"error", "error_description"} for a token that
// does not verify; never cached.
func (s *server) backchannelLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	bad := func(desc string) {
		apierr.WriteJSON(w, http.StatusBadRequest, struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}{"invalid_request", desc})
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxProviderBody)
	raw := ""
	if err := r.ParseForm(); err == nil {
		raw = r.PostForm.Get("logout_token")
	}
	if raw == "" {
		bad("logout_token is required")
		return
	}
	d, err := s.oidc.discovery(r.Context())
	if err != nil {
		s.log.Warn("back-channel logout: the provider's discovery failed", "err", err)
		bad("the provider cannot be reached")
		return
	}
	sid, sub, err := s.oidc.verifyLogout(r.Context(), d, raw)
	if err != nil {
		s.log.Info("back-channel logout refused", "err", err)
		var re *errRefused
		if errors.As(err, &re) {
			bad(re.reason)
		} else {
			bad("the logout token did not verify")
		}
		return
	}
	n, err := s.st.DeleteUISessionsBySID(r.Context(), sid, sub)
	if err != nil {
		s.log.Warn("back-channel logout: end the sessions", "err", err)
		writeError(w, internalf("end the sessions: %v", err))
		return
	}
	s.log.Info("back-channel logout", "sub", sub, "sessions", n)
	w.WriteHeader(http.StatusOK)
}

// pageTemplate is the plain page of a sign-in that did not end in the UI.
var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>FeRun: {{.Title}}</title>
<style>body{font-family:system-ui,sans-serif;max-width:36rem;margin:4rem auto;padding:0 1rem;line-height:1.5}</style>
</head>
<body>
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
{{if .Link}}<p><a href="{{.Link}}">{{.LinkText}}</a></p>{{end}}
</body>
</html>
`))

// page answers a plain HTML page.
func page(w http.ResponseWriter, status int, title, message, link, linkText string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = pageTemplate.Execute(w, struct{ Title, Message, Link, LinkText string }{title, message, link, linkText})
}
