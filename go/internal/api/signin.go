// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/google/uuid"

	fapi "github.com/gardenlinux/feos/go/ferun/api"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/oidc"
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
// (project and sandbox tokens, /mcp) are not touched by any of it.

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

// meProjects answers /auth/me's projects of a caller: for each project the
// principal it is there (its grants, the named person's, the project's
// protected environments), its effective role, and every environment of
// it the caller reads with its effective role, whether it may plan
// against it and delete it, all by auth.Project's one rule, which every
// check uses too (the contract's §2.2). A project the caller does not see
// is left out, and so is one no longer there. Each says when it was made
// (a name made again is another project). The projects come sorted by
// name; integration says to answer an integration's own roles beside.
func (s *server) meProjects(r *http.Request, ps []auth.Project, integration bool) ([]fapi.MeProject, error) {
	out := []fapi.MeProject{}
	if len(ps) == 0 {
		return out, nil
	}
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.Name
	}
	envs, err := s.st.ProjectEnvironments(r.Context(), names)
	if err != nil {
		return nil, internalf("environments: %v", err)
	}
	created, err := s.st.ProjectsCreated(r.Context(), names)
	if err != nil {
		return nil, internalf("projects: %v", err)
	}
	byProject := map[string][]store.EnvName{}
	for _, e := range envs {
		byProject[e.Project] = append(byProject[e.Project], e)
	}
	for _, t := range ps {
		for _, e := range byProject[t.Name] {
			if e.Protected && !e.Deleting {
				if t.Protected == nil {
					t.Protected = map[uuid.UUID]bool{}
				}
				t.Protected[e.ID] = true
			}
		}
		at, there := created[t.Name]
		if !t.Visible() || !there {
			continue
		}
		mp := fapi.MeProject{Project: t.Name, CreatedAt: at, Role: t.ProjectRole().String(), Environments: []fapi.MeEnvironment{}}
		if integration {
			mp.IntegrationRole = t.Role.String()
			if t.Named != nil {
				mp.PersonRole = t.Named.Project.String()
			}
		}
		for _, e := range byProject[t.Name] {
			role := t.EnvRole(e.ID)
			if role < auth.RoleRead {
				continue
			}
			me := fapi.MeEnvironment{ID: e.ID.String(), Name: e.Name, Role: role.String(), Protected: t.Protected[e.ID],
				Plan: t.PlanRole(e.ID) >= auth.RoleWrite, Delete: t.MayDelete(e.ID)}
			if integration {
				me.IntegrationRole = t.OwnEnvRole(e.ID).String()
				if t.Named != nil {
					me.PersonRole = t.PersonEnvRole(e.ID).String()
				}
			}
			mp.Environments = append(mp.Environments, me)
		}
		out = append(out, mp)
	}
	return out, nil
}

// grantsByProject groups grants of one principal by project, in their
// order (by project).
func grantsByProject(gs []store.PersonProjectGrant) ([]string, map[string]auth.Grants) {
	var order []string
	out := map[string]auth.Grants{}
	for _, g := range gs {
		cur, ok := out[g.Project]
		if !ok {
			order = append(order, g.Project)
		}
		role, _ := auth.ParseRole(g.Role)
		if g.EnvironmentID == uuid.Nil {
			cur.Project = max(cur.Project, role)
		} else {
			if cur.Envs == nil {
				cur.Envs = map[uuid.UUID]auth.Role{}
			}
			cur.Envs[g.EnvironmentID] = max(cur.Envs[g.EnvironmentID], role)
		}
		out[g.Project] = cur
	}
	return order, out
}

// sessionProjects are a session's principals in its projects: an
// operator's every live project, as admin; a person's those of their
// grants.
func (s *server) sessionProjects(r *http.Request, sess store.UISession) ([]auth.Project, error) {
	p := sessionPrincipal(sess)
	if sess.Admin {
		names, err := s.st.LiveProjectNames(r.Context())
		if err != nil {
			return nil, internalf("projects: %v", err)
		}
		out := make([]auth.Project, 0, len(names))
		for _, n := range names {
			out = append(out, auth.Project{Name: n, Principal: p, Role: auth.RoleAdmin})
		}
		return out, nil
	}
	if sess.PersonID == uuid.Nil {
		return nil, nil
	}
	gs, err := s.st.PersonGrants(r.Context(), sess.PersonID)
	if err != nil {
		return nil, internalf("grants: %v", err)
	}
	order, by := grantsByProject(gs)
	out := make([]auth.Project, 0, len(order))
	for _, n := range order {
		out = append(out, auth.Project{Name: n, Principal: p, Role: by[n].Project, Envs: by[n].Envs})
	}
	return out, nil
}

// authMe is GET /auth/me (the contract's §2.3): who asks, by the
// credential, and the projects it reaches with its effective roles.
// Basic credentials that pass win, as on the admin routes; then a bearer
// (an integration's, with the person it names, or a project token); then
// the session. FeRun-Project is not read.
func (s *server) authMe(w http.ResponseWriter, r *http.Request) error {
	if a, present, err := s.basicAdmin(r); present && err == nil {
		return writeJSON(w, http.StatusOK, fapi.Me{Kind: "basic", Username: a.User, Admin: true, Method: a.Method,
			Projects: []fapi.MeProject{}, Accounts: []fapi.MeAccount{}})
	}
	if tok := bearer(r); tok != "" {
		if isJWT(tok) {
			return s.integrationMe(w, r, tok)
		}
		return s.tokenMe(w, r, tok)
	}
	sess, ok, err := s.session(r)
	if err != nil {
		return err
	}
	if !ok {
		return apierr.New(http.StatusUnauthorized, "not signed in")
	}
	if _, sent, _ := personNamed(r); sent {
		return errPersonHeaderRefused()
	}
	ps, err := s.sessionProjects(r, sess)
	if err != nil {
		return err
	}
	projects, err := s.meProjects(r, ps, false)
	if err != nil {
		return err
	}
	if err := s.fillProjectAccounts(r.Context(), projects); err != nil {
		return err
	}
	accounts, err := s.myAccounts(r.Context(), sess)
	if err != nil {
		return err
	}
	person := ""
	if sess.PersonID != uuid.Nil {
		person = sess.PersonID.String()
	}
	me := fapi.Me{Kind: "session", Sub: sess.Sub, Person: person, Email: sess.Email, Name: sess.Name,
		Username: sess.Username, Picture: sess.Picture, Admin: sess.Admin, Method: auth.AdminOIDC, Projects: projects,
		Accounts: accounts}
	return writeJSON(w, http.StatusOK, me)
}

// tokenMe is /auth/me for a project token: its own grant, and its project
// with the roles of all its grants.
func (s *server) tokenMe(w http.ResponseWriter, r *http.Request, tok string) error {
	ctx := r.Context()
	hash := store.HashToken(tok)
	name, roles, err := s.st.TokenAccess(ctx, hash)
	if errors.Is(err, model.ErrUnknownToken) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`"`)
		return apierr.New(http.StatusUnauthorized, "unknown project token")
	}
	if err != nil {
		return fmt.Errorf("token lookup: %w", err)
	}
	if _, sent, _ := personNamed(r); sent {
		return errPersonHeaderRefused()
	}
	own, err := s.st.GetProjectToken(ctx, name, hash)
	if err != nil {
		return fmt.Errorf("token lookup: %w", err)
	}
	t := auth.Project{Name: name, Principal: auth.Principal{TokenHash: hash}}
	grantRoles(&t, roles)
	projects, err := s.meProjects(r, []auth.Project{t}, false)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, fapi.Me{Kind: "token",
		Token:    &fapi.MeToken{Label: own.Label, Role: own.Role, Environment: idOrEmpty(own.EnvironmentID)},
		Projects: projects, Accounts: []fapi.MeAccount{}})
}

// integrationMe is /auth/me for an integration (ADR 0056): itself, the
// person it names, and the projects that grant it with their roles; with
// a person named, those in which both hold a role, with the lower of
// both and each side's own. A disabled integration reaches none and learns
// no more of a person it names than the subject it sent, and one naming a
// person FeRun does not know or who is disabled reaches none.
func (s *server) integrationMe(w http.ResponseWriter, r *http.Request, tok string) error {
	ctx := r.Context()
	in, err := s.integrationOf(w, r, tok)
	if err != nil {
		return err
	}
	sub, sent, err := personNamed(r)
	if err != nil {
		return err
	}
	me := fapi.Me{Kind: "integration",
		Integration: &fapi.MeIntegration{Name: in.Name, Title: in.Title, URL: in.URL, Disabled: in.Disabled()},
		Projects:    []fapi.MeProject{}, Accounts: []fapi.MeAccount{}}
	var person *store.Person
	if sent {
		me.Named = &fapi.MeNamed{Sub: sub}
	}
	// A disabled integration learns nothing of the person it names.
	if sent && !in.Disabled() {
		p, err := s.st.PersonBySubject(ctx, in.Issuer, sub)
		if err != nil && !isNoRow(err) {
			return fmt.Errorf("person %s: %w", sub, err)
		}
		if err == nil && !p.Disabled() {
			person = &p
			me.Named = &fapi.MeNamed{Sub: sub, Known: true, Person: p.ID.String(), Email: p.Email, Name: p.Name}
		}
	}
	if in.Disabled() || (sent && person == nil) {
		return writeJSON(w, http.StatusOK, me)
	}
	gs, err := s.st.IntegrationGrants(ctx, in.ID)
	if err != nil {
		return fmt.Errorf("grants: %w", err)
	}
	order, by := grantsByProject(gs)
	var theirs map[string]auth.Grants
	if person != nil {
		pgs, err := s.st.PersonGrants(ctx, person.ID)
		if err != nil {
			return fmt.Errorf("grants: %w", err)
		}
		_, theirs = grantsByProject(pgs)
	}
	ps := make([]auth.Project, 0, len(order))
	for _, n := range order {
		t := auth.Project{Name: n, Principal: auth.Principal{Integration: in.ID}, Role: by[n].Project, Envs: by[n].Envs}
		if person != nil {
			g, ok := theirs[n]
			if !ok {
				continue
			}
			t.Named, t.Principal.For = &auth.NamedPerson{Sub: sub, Grants: g}, person.ID
		}
		ps = append(ps, t)
	}
	if me.Projects, err = s.meProjects(r, ps, true); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, me)
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
	d, err := s.oidc.Discovery(r.Context())
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
	q.Set("client_id", s.cfg.OIDC.ClientID)
	q.Set("redirect_uri", s.redirectURI())
	q.Set("scope", strings.Join(s.cfg.OIDC.Scopes, " "))
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
		var pe *oidc.ErrProvider
		var re *oidc.ErrRefused
		switch {
		case errors.As(err, &pe):
			status, msg = http.StatusBadGateway, "The sign-in provider could not be reached or gave an answer FeRun cannot use."
		case errors.As(err, &re):
			msg = "The sign-in did not verify: " + re.Reason + "."
		}
		s.log.Warn("sign-in failed", "err", err)
		page(w, status, "Sign-in failed", msg, again, "Start again")
	}
	if code == "" {
		fail(oidc.Refused("the provider sent no code"))
		return
	}
	d, err := s.oidc.Discovery(r.Context())
	if err != nil {
		fail(err)
		return
	}
	toks, err := s.oidc.Exchange(r.Context(), d, code, s.redirectURI(), login.Verifier)
	if err != nil {
		fail(err)
		return
	}
	idt, err := s.oidc.Verify(r.Context(), d, toks.IDToken, login.Nonce)
	if err != nil {
		fail(err)
		return
	}
	info, err := s.oidc.Userinfo(r.Context(), d, toks.AccessToken, idt.Claims.Str("sub"))
	if err != nil {
		fail(err)
		return
	}
	p := oidc.PersonOf(idt.Claims, info)
	operator := s.cfg.OIDC.AdminGroup != "" && slices.Contains(p.Groups, s.cfg.OIDC.AdminGroup)
	who := p.Email
	if who == "" {
		who = p.Username
	}
	pr, err := s.st.UpsertPerson(r.Context(), s.cfg.OIDC.Issuer, p.Sub, p.Email, p.Name)
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
			Picture: p.Picture, Groups: p.Groups, Admin: operator, IDToken: idt.Raw, PersonID: pr.ID,
			SID: idt.Claims.Str("sid"),
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
			if d, err := s.oidc.Discovery(r.Context()); err == nil && d.EndSessionEndpoint != "" {
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
	r.Body = http.MaxBytesReader(w, r.Body, oidc.MaxBody)
	raw := ""
	if err := r.ParseForm(); err == nil {
		raw = r.PostForm.Get("logout_token")
	}
	if raw == "" {
		bad("logout_token is required")
		return
	}
	d, err := s.oidc.Discovery(r.Context())
	if err != nil {
		s.log.Warn("back-channel logout: the provider's discovery failed", "err", err)
		bad("the provider cannot be reached")
		return
	}
	sid, sub, err := s.oidc.VerifyLogout(r.Context(), d, raw)
	if err != nil {
		s.log.Info("back-channel logout refused", "err", err)
		var re *oidc.ErrRefused
		if errors.As(err, &re) {
			bad(re.Reason)
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
