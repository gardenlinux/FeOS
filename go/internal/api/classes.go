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

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/oidc"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The realm of the Basic and Bearer challenges.
const realm = "ferun"

// The cookies of human sign-in.
const (
	cookieSession = auth.SessionCookie
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
	session := auth.SessionOf(r)
	if session == "" {
		return store.UISession{}, false, nil
	}
	hash := store.HashToken(session)
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
			if _, sent, _ := personNamed(r); sent {
				return errPersonHeaderRefused()
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
			if _, sent, _ := personNamed(r); sent {
				return errPersonHeaderRefused()
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

// projectHeader names the project a person's session acts in (ADR 0031); a
// token's project is its own.
const projectHeader = auth.ProjectHeader

// scope is what a project-class route's role is checked on.
type scope int

const (
	// scopeProject: the principal's project grant.
	scopeProject scope = iota
	// scopeEnv: the path's {env}, the higher of the principal's project
	// grant and its grant on that environment.
	scopeEnv
	// scopeHandler: the handler checks, on what the request names (a
	// domain's binding: the environments it leaves and joins; a sandbox's
	// or a job's create: the environment of its body).
	scopeHandler
	// scopeSandbox: the environment of the path's sandbox {id} (ADR
	// 0035), as scopeEnv; a sandbox in one the principal may not read is
	// absent (404).
	scopeSandbox
	// scopeJob: the environment of the path's project's job {id}, as
	// scopeSandbox.
	scopeJob
	// scopeGrant: the scope of a grant or an invitation, the project or
	// the environment of the body or the query, which the handler checks
	// the role on (adminScope).
	scopeGrant
)

// need is the role and scope a project-class route needs of its principal,
// stated at the route (ADR 0031). A need of RoleNone on the project is a
// member's: any grant in the project, the answer filtered to what the
// principal may read.
type need struct {
	role  auth.Role
	scope scope
	// orProject is, on scopeEnv, a project role that suffices too while
	// the environment is not protected (an environment's deletion: its
	// admin, or write on the project; ADR 0060).
	orProject auth.Role
	// plan checks, on scopeEnv, the role the environment would give were
	// it not protected (ADR 0060: a plan is not held back).
	plan bool
	// noIntegration refuses an integration whatever its role (ADR 0056:
	// an integration changes no principal of a project): 403
	// integration_refused.
	noIntegration bool
	// ownIntegration lets an integration through on a route about one
	// integration ({integration}) when that is itself (giving up its own
	// grants); the handler checks that it is.
	ownIntegration bool
}

func onProject(r auth.Role) need { return need{role: r} }
func onEnv(r auth.Role) need     { return need{role: r, scope: scopeEnv} }
func onSandbox(r auth.Role) need { return need{role: r, scope: scopeSandbox} }
func onJob(r auth.Role) need     { return need{role: r, scope: scopeJob} }
func byHandler(r auth.Role) need { return need{role: r, scope: scopeHandler} }
func byScope(r auth.Role) need   { return need{role: r, scope: scopeGrant} }

// onEnvPlan is the need of an environment's plan: r on it, protection
// aside.
func onEnvPlan(r auth.Role) need { return need{role: r, scope: scopeEnv, plan: true} }

// onEnvOrProject is r on the environment, or p on the project while the
// environment is not protected.
func onEnvOrProject(r, p auth.Role) need { return need{role: r, scope: scopeEnv, orProject: p} }

// noIntegration is n refusing an integration (need.noIntegration).
func noIntegration(n need) need { n.noIntegration = true; return n }

// orOwnIntegration is n letting an integration through on its own
// {integration} (need.ownIntegration).
func orOwnIntegration(n need) need { n.ownIntegration = true; return n }

// member is the need of the routes every principal of the project may
// call, whose answers are filtered to what it may read.
var member = need{}

// String is the need as the route table's test states it.
func (n need) String() string {
	s := n.base()
	if n.noIntegration {
		s += ", no integration"
	}
	if n.ownIntegration {
		s += " or the integration itself"
	}
	return s
}

// base is String without what it says of integrations.
func (n need) base() string {
	switch {
	case n.scope == scopeHandler:
		return n.role.String() + " checked by the handler"
	case n.scope == scopeGrant:
		return n.role.String() + " by scope"
	case n.scope == scopeSandbox:
		return n.role.String() + " sandbox's environment"
	case n.scope == scopeJob:
		return n.role.String() + " job's environment"
	case n.scope == scopeEnv && n.orProject != auth.RoleNone:
		return n.role.String() + " environment or " + n.orProject.String() + " project unless protected"
	case n.scope == scopeEnv && n.plan:
		return n.role.String() + " environment, protection aside"
	case n.scope == scopeEnv:
		return n.role.String() + " environment"
	case n.role == auth.RoleNone:
		return "member"
	}
	return n.role.String() + " project"
}

// codeIntegrationRefused is the code of the 403 of a route or class an
// integration may not use whatever its role (ADR 0056).
const codeIntegrationRefused = "integration_refused"

// The codes of an integration's sign-in (ADR 0056) and of the header that
// names a person.
const (
	codePersonHeaderRefused = "person_header_refused"
	codeTokenRefused        = "token_refused"
	codeUnknownIntegration  = "unknown_integration"
	codeProviderUnreachable = "provider_unreachable"
)

// personHeader names the person an integration's call is made for.
const personHeader = auth.PersonHeader

// errPersonHeaderRefused is the answer to FeRun-Person from a session or a
// project token: only an integration names a person, and a caller that
// expects the narrowing must not get a wider answer.
func errPersonHeaderRefused() error {
	return apierr.WithCode(http.StatusBadRequest, codePersonHeaderRefused, "only an integration names a person")
}

// personNamed is the request's FeRun-Person: sent reports whether it has
// the header at all; a value that is not one subject of 1 to 255
// printable ASCII characters is 400.
func personNamed(r *http.Request) (sub string, sent bool, err error) {
	vs := r.Header.Values(personHeader)
	if len(vs) == 0 {
		return "", false, nil
	}
	bad := apierr.WithCode(http.StatusBadRequest, codePersonHeaderRefused,
		"%s names one person by their subject: 1 to 255 printable ASCII characters", personHeader)
	if len(vs) > 1 || len(vs[0]) == 0 || len(vs[0]) > 255 {
		return "", true, bad
	}
	for i := 0; i < len(vs[0]); i++ {
		if c := vs[0][i]; c < 0x20 || c > 0x7e {
			return "", true, bad
		}
	}
	return vs[0], true, nil
}

// errIntegrationRefused is the answer to an integration on a route that
// changes the principals of a project, or of a class it is no principal
// of (person, provider).
func errIntegrationRefused() error {
	return apierr.WithCode(http.StatusForbidden, codeIntegrationRefused, "an integration changes no principal of a project")
}

// isJWT reports whether a bearer is a JWT, an integration's (exactly two
// dots): project tokens are hex, service tokens feos-<kind>-<base64url>,
// sandbox and route tokens base64url, none with a dot.
func isJWT(tok string) bool { return strings.Count(tok, ".") == 2 }

// errNoProject is the answer for a project the principal is no member of,
// absent or foreign alike, and for a token's request that names another
// project than its own.
func errNoProject() error { return apierr.New(http.StatusNotFound, "no such project") }

// errNoEnvironment is the environments' answer for an environment the
// principal may not see (env.errNoEnvironment's text).
func errNoEnvironment() error { return apierr.New(http.StatusNotFound, "no such environment") }

// project is the project class: the principal is a project's token (the
// bearer), or a person's session that names its project in FeRun-Project,
// with the CSRF rules of a session. Then the route's need is checked
// (allow).
func (s *server) project(n need, h apiHandler) http.Handler {
	return s.wrap(func(w http.ResponseWriter, r *http.Request) error {
		t, err := s.projectPrincipal(w, r)
		if err != nil {
			return err
		}
		r = r.WithContext(auth.WithProject(r.Context(), t))
		r, err = s.allow(r, t, n)
		if err != nil {
			return err
		}
		return h(w, r)
	})
}

// projectPrincipal resolves the principal of a project-class request and
// its grants in the project.
func (s *server) projectPrincipal(w http.ResponseWriter, r *http.Request) (auth.Project, error) {
	return s.projectPrincipalOr(w, r, "project bearer required")
}

// projectPrincipalOr is projectPrincipal, whose 401 for a request with no
// credential at all says missing (the class's text). A bearer that is a
// JWT is an integration's (integrationPrincipal); FeRun-Person from any
// other caller is 400. The project's protected environments go with the
// principal (ADR 0060).
func (s *server) projectPrincipalOr(w http.ResponseWriter, r *http.Request, missing string) (auth.Project, error) {
	ctx := r.Context()
	named := r.Header.Get(projectHeader)
	if tok := bearer(r); tok != "" {
		if isJWT(tok) {
			return s.integrationPrincipal(w, r, tok, named)
		}
		hash := store.HashToken(tok)
		name, roles, err := s.st.TokenAccess(ctx, hash)
		if errors.Is(err, model.ErrUnknownToken) {
			return auth.Project{}, apierr.New(http.StatusUnauthorized, "unknown project token")
		}
		if err != nil {
			return auth.Project{}, fmt.Errorf("token lookup: %w", err)
		}
		if _, sent, _ := personNamed(r); sent {
			return auth.Project{}, errPersonHeaderRefused()
		}
		if named != "" && named != name {
			return auth.Project{}, errNoProject()
		}
		if s.touches.due("t:" + hash) {
			if err := s.st.TouchProjectToken(ctx, hash); err != nil {
				s.log.Warn("touch a project token", "err", err)
			}
		}
		t := auth.Project{Name: name, Token: tok, Principal: auth.Principal{TokenHash: hash}}
		grantRoles(&t, roles)
		return t, s.protect(ctx, &t)
	}
	sess, ok, err := s.session(r)
	if err != nil {
		return auth.Project{}, err
	}
	if !ok || sess.PersonID == uuid.Nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`"`)
		return auth.Project{}, apierr.New(http.StatusUnauthorized, "%s", missing)
	}
	if err := s.checkCSRF(r); err != nil {
		return auth.Project{}, err
	}
	if _, sent, _ := personNamed(r); sent {
		return auth.Project{}, errPersonHeaderRefused()
	}
	if named == "" {
		return auth.Project{}, apierr.New(http.StatusBadRequest, "a session names its project in the %s header", projectHeader)
	}
	roles, exists, err := s.st.PersonAccess(ctx, named, sess.PersonID)
	if err != nil {
		return auth.Project{}, fmt.Errorf("grants: %w", err)
	}
	if !exists || (!sess.Admin && len(roles) == 0) {
		return auth.Project{}, errNoProject()
	}
	t := auth.Project{Name: named, Principal: auth.Principal{Person: sess.PersonID, Operator: sess.Admin}}
	t.Session = auth.SessionOf(r)
	grantRoles(&t, roles)
	if sess.Admin {
		t.Role = auth.RoleAdmin
	}
	return t, s.protect(ctx, &t)
}

// protect sets the protected environments of t's project (ADR 0060).
func (s *server) protect(ctx context.Context, t *auth.Project) error {
	ids, err := s.st.ProtectedEnvironments(ctx, t.Name)
	if err != nil {
		return fmt.Errorf("protected environments: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	t.Protected = make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		t.Protected[id] = true
	}
	return nil
}

// integrationOf signs an integration in by its bearer (ADR 0056): the
// provider's access token for this API, verified (VerifyAccessToken), of
// a registered integration; 401 token_refused otherwise (the reason to the
// log only) or unknown_integration, 502 when the provider's keys cannot be
// read. Without --oidc-issuer every such bearer is refused. A disabled
// integration signs in; its grants have no effect (the caller decides).
func (s *server) integrationOf(w http.ResponseWriter, r *http.Request, tok string) (store.Integration, error) {
	refuse := func(code, text string) error {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`"`)
		return apierr.WithCode(http.StatusUnauthorized, code, "%s", text)
	}
	if s.oidc == nil {
		return store.Integration{}, refuse(codeTokenRefused, "bearer refused")
	}
	ctx := r.Context()
	c, err := s.oidc.VerifyAccessToken(ctx, tok, s.cfg.APIAudience)
	var pe *oidc.ErrProvider
	if errors.As(err, &pe) {
		s.log.Warn("an integration's bearer: the provider's keys", "err", err)
		return store.Integration{}, apierr.WithCode(http.StatusBadGateway, codeProviderUnreachable,
			"the provider's keys cannot be read")
	}
	if err != nil {
		s.log.Info("an integration's bearer refused", "err", err)
		return store.Integration{}, refuse(codeTokenRefused, "bearer refused")
	}
	in, err := s.st.IntegrationBySubject(ctx, s.cfg.OIDC.Issuer, c.Sub)
	if isNoRow(err) {
		s.log.Info("an access token of no registered integration", "sub", c.Sub)
		return store.Integration{}, refuse(codeUnknownIntegration, "unknown integration")
	}
	if err != nil {
		return store.Integration{}, fmt.Errorf("integration lookup: %w", err)
	}
	if s.touches.due("i:" + in.ID.String()) {
		if err := s.st.TouchIntegration(ctx, in.ID); err != nil {
			s.log.Warn("touch an integration", "err", err)
		}
	}
	return in, nil
}

// integrationPrincipal is an integration's principal in the project it
// names (FeRun-Project, required): its grants there, and, with
// FeRun-Person, the person it names, whose grants bound the call (the
// lower of two roles, ADR 0056). A project it holds no grant in, a
// disabled integration, and a named person FeRun does not know, who is
// disabled or holds no grant there, are an unknown project alike.
func (s *server) integrationPrincipal(w http.ResponseWriter, r *http.Request, tok, named string) (auth.Project, error) {
	ctx := r.Context()
	in, err := s.integrationOf(w, r, tok)
	if err != nil {
		return auth.Project{}, err
	}
	sub, sent, err := personNamed(r)
	if err != nil {
		return auth.Project{}, err
	}
	if named == "" {
		return auth.Project{}, apierr.New(http.StatusBadRequest, "an integration names its project in the %s header", projectHeader)
	}
	if in.Disabled() {
		return auth.Project{}, errNoProject()
	}
	roles, exists, err := s.st.IntegrationAccess(ctx, named, in.ID)
	if err != nil {
		return auth.Project{}, fmt.Errorf("grants: %w", err)
	}
	if !exists || len(roles) == 0 {
		return auth.Project{}, errNoProject()
	}
	t := auth.Project{Name: named, Token: tok, Principal: auth.Principal{Integration: in.ID}}
	grantRoles(&t, roles)
	if sent {
		p, err := s.st.PersonBySubject(ctx, in.Issuer, sub)
		if isNoRow(err) || (err == nil && p.Disabled()) {
			return auth.Project{}, errNoProject()
		}
		if err != nil {
			return auth.Project{}, fmt.Errorf("person %s: %w", sub, err)
		}
		proles, _, err := s.st.PersonAccess(ctx, named, p.ID)
		if err != nil {
			return auth.Project{}, fmt.Errorf("grants: %w", err)
		}
		if len(proles) == 0 {
			return auth.Project{}, errNoProject()
		}
		var pt auth.Project
		grantRoles(&pt, proles)
		t.Named = &auth.NamedPerson{Sub: sub, Grants: auth.Grants{Project: pt.Role, Envs: pt.Envs}}
		t.Principal.For = p.ID
	}
	if err := s.protect(ctx, &t); err != nil {
		return auth.Project{}, err
	}
	if !t.Visible() {
		return auth.Project{}, errNoProject()
	}
	return t, nil
}

// grantRoles sets a principal's roles from its grants: per scope the
// highest.
func grantRoles(t *auth.Project, roles []store.RoleAt) {
	for _, ra := range roles {
		role, ok := auth.ParseRole(ra.Role)
		if !ok {
			continue
		}
		if ra.EnvironmentID == uuid.Nil {
			t.Role = max(t.Role, role)
			continue
		}
		if t.Envs == nil {
			t.Envs = map[uuid.UUID]auth.Role{}
		}
		t.Envs[ra.EnvironmentID] = max(t.Envs[ra.EnvironmentID], role)
	}
}

// The texts of the provider and auth classes (ADR 0054).
const (
	textProviderRequired  = "provider credential required"
	textAuthRequired      = "ferun-auth token required"
	textUnknownService    = "unknown service token"
	textOutsideTheCluster = "ferun-auth's token used from outside the cluster"
)

// authTokenPrefix starts every token of ferun-auth (feos-auth-…).
var authTokenPrefix = pki.ServiceTokenPrefix(model.ServiceAuth)

// provider is the provider class (ADR 0054): a bearer that starts with
// feos-auth- is a service token, which must be ferun-auth's (kind auth;
// else 401 unknown service token); any other credential is a project's
// principal, taken exactly as the project class takes it (a token, or a
// session that names its project, with the CSRF rules of a session); an
// integration's bearer (a JWT) is refused, 403 integration_refused. The
// route states no need: the visits service decides, on the challenge or
// the visit, whether the caller answers for its environment.
func (s *server) provider(h apiHandler) http.Handler {
	return s.wrap(func(w http.ResponseWriter, r *http.Request) error {
		tok := bearer(r)
		if strings.HasPrefix(tok, authTokenPrefix) {
			svc, ok, err := s.authService(r, tok)
			if err != nil {
				return err
			}
			if !ok {
				return apierr.New(http.StatusUnauthorized, textUnknownService)
			}
			return h(w, r.WithContext(auth.WithService(r.Context(), svc)))
		}
		if isJWT(tok) {
			// ADR 0056 sets the integration aside as a provider of
			// visitors.
			return errIntegrationRefused()
		}
		t, err := s.projectPrincipalOr(w, r, textProviderRequired)
		if err != nil {
			return err
		}
		return h(w, r.WithContext(auth.WithProject(r.Context(), t)))
	})
}

// authOnly is the auth class (ADR 0054): ferun-auth's service token
// (kind auth) and nothing else.
func (s *server) authOnly(h apiHandler) http.Handler {
	return s.wrap(func(w http.ResponseWriter, r *http.Request) error {
		refuse := func() error {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`"`)
			return apierr.New(http.StatusUnauthorized, textAuthRequired)
		}
		tok := bearer(r)
		if !strings.HasPrefix(tok, authTokenPrefix) {
			return refuse()
		}
		svc, ok, err := s.authService(r, tok)
		if err != nil {
			return err
		}
		if !ok {
			return refuse()
		}
		return h(w, r.WithContext(auth.WithService(r.Context(), svc)))
	})
}

// authService resolves ferun-auth's token: ok is false for a token that
// is unknown, revoked or of another kind. Its last_used_at is touched at
// most once a minute. ferun-auth calls ferund inside the cluster only: a
// request with its token that carries X-Forwarded-For came through the
// Gateway, which is warned of, once a minute per token.
func (s *server) authService(r *http.Request, tok string) (auth.Service, bool, error) {
	ctx := r.Context()
	hash := store.HashToken(tok)
	t, err := s.st.ServiceTokenOf(ctx, hash)
	if errors.Is(err, store.ErrNoRow) || (err == nil && t.Kind != model.ServiceAuth) {
		return auth.Service{}, false, nil
	}
	if err != nil {
		return auth.Service{}, false, fmt.Errorf("service token lookup: %w", err)
	}
	if s.touches.due("v:" + hash) {
		if err := s.st.TouchServiceToken(ctx, hash); err != nil {
			s.log.Warn("touch a service token", "err", err)
		}
	}
	if _, forwarded := r.Header["X-Forwarded-For"]; forwarded && s.outside.due(t.ID.String()) {
		s.log.Warn(textOutsideTheCluster, "token", t.ID.String(), "name", t.Name)
	}
	return auth.Service{Kind: t.Kind, Name: t.Name, ID: t.ID, TokenHash: hash}, true, nil
}

// allow checks a route's need: an integration on a route that refuses
// one is 403 integration_refused, whatever its role; on the project, the
// principal's role there (auth.Project.ProjectRole); on an environment,
// its role there (auth.Project.EnvRole: the higher of the project grant,
// as protection counts it, and the grant there; the lower of two for an
// integration naming a person, ADR 0056, 0060), an environment it holds
// no role on being absent (404). A role too low is 403 role_required.
// The environment is resolved only when the project role does not decide
// (auth.Project.EveryEnv). An environment it resolved to check the grant
// there goes with the request it returns (checkedEnv), so that the
// handler acts on the environment that was checked, whatever its name
// names by then.
func (s *server) allow(r *http.Request, t auth.Project, n need) (*http.Request, error) {
	if n.noIntegration && t.Principal.IsIntegration() {
		return r, errIntegrationRefused()
	}
	switch n.scope {
	case scopeProject:
		if t.ProjectRole() >= n.role {
			return r, nil
		}
		return r, apierr.RoleRequired(n.role.String(), "project")
	case scopeEnv:
		switch {
		case n.plan && t.ProjectRole() >= n.role,
			!n.plan && t.EveryEnv(n.role),
			n.orProject != auth.RoleNone && len(t.Protected) == 0 && t.ProjectRole() >= n.orProject:
			return r, nil
		}
		if t.ProjectRole() == auth.RoleNone && len(t.EnvGrants()) == 0 {
			return r, errNoEnvironment()
		}
		ref := r.PathValue("env")
		id, err := s.envRef(r, t, ref)
		if err != nil {
			return r, err
		}
		have := t.EnvRole(id)
		if n.plan && have > auth.RoleNone {
			have = t.PlanRole(id)
		}
		switch {
		case have == auth.RoleNone:
			return r, errNoEnvironment()
		case have >= n.role, n.orProject != auth.RoleNone && !t.Protected[id] && t.ProjectRole() >= n.orProject:
		default:
			return r, apierr.RoleRequired(n.role.String(), "environment")
		}
		return r.WithContext(context.WithValue(r.Context(), checkedEnvKey{}, checkedEnvRef{ref: ref, id: id})), nil
	case scopeSandbox, scopeJob:
		if t.EveryEnv(n.role) {
			return r, nil
		}
		return r, s.allowPodEnv(r, t, n)
	}
	return r, nil
}

type checkedEnvKey struct{}

// checkedEnvRef is the environment allow resolved {env} to.
type checkedEnvRef struct {
	ref string
	id  uuid.UUID
}

// checkedEnv is the ID of the environment that allow resolved ref to for
// this request, false when it resolved none (a project grant sufficed).
func checkedEnv(r *http.Request, ref string) (uuid.UUID, bool) {
	c, ok := r.Context().Value(checkedEnvKey{}).(checkedEnvRef)
	return c.id, ok && c.ref == ref
}

// envRef is the ID of the environment of t's project that ref names (its
// ID or its name); none is 404.
func (s *server) envRef(r *http.Request, t auth.Project, ref string) (uuid.UUID, error) {
	id, err := s.st.EnvironmentRef(r.Context(), t.Name, ref)
	if isNoRow(err) {
		return uuid.Nil, errNoEnvironment()
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("environment %s: %w", ref, err)
	}
	return id, nil
}

// allowPodEnv checks n's role on the environment of the path's sandbox
// (scopeSandbox) or project's job (scopeJob) of t's project: one that is
// none of the project's, or in an environment the principal may not read,
// is absent (404); too low a role is 403.
func (s *server) allowPodEnv(r *http.Request, t auth.Project, n need) error {
	absent := func() error { return errUnknownSandbox(r.PathValue("id")) }
	if n.scope == scopeJob {
		absent = errNoProjectJob
	}
	if t.ProjectRole() == auth.RoleNone && len(t.EnvGrants()) == 0 {
		return absent()
	}
	id, err := podid.Parse(r.PathValue("id"))
	if err != nil {
		return absent()
	}
	var envID uuid.UUID
	if n.scope == scopeSandbox {
		sb, err := s.st.GetOwnSandbox(r.Context(), t.Name, id)
		if isNoRow(err) {
			return absent()
		}
		if err != nil {
			return fmt.Errorf("sandbox %s: %w", id, err)
		}
		envID = sb.EnvID
	} else {
		j, err := s.st.GetProjectJob(r.Context(), t.Name, id)
		if isNoRow(err) {
			return absent()
		}
		if err != nil {
			return fmt.Errorf("job %s: %w", id, err)
		}
		envID = j.EnvID
	}
	switch have := t.EnvRole(envID); {
	case have < auth.RoleRead:
		return absent()
	case have < n.role:
		return apierr.RoleRequired(n.role.String(), "environment")
	}
	return nil
}

// errNoProjectJob is the jobs' answer for a job the principal may not see
// (env.errNoProjectJob's text).
func errNoProjectJob() error { return apierr.New(http.StatusNotFound, "no such job") }

// allowEnv checks role on the environment of t's project that ref names
// (its ID or its name).
func (s *server) allowEnv(r *http.Request, t auth.Project, ref string, role auth.Role) error {
	if t.EveryEnv(role) {
		return nil
	}
	id, err := s.envRef(r, t, ref)
	if err != nil {
		return err
	}
	return allowEnvID(t, id, role)
}

// allowEnvID checks role on the environment id: none is 404, too low 403.
func allowEnvID(t auth.Project, id uuid.UUID, role auth.Role) error {
	switch have := t.EnvRole(id); {
	case have == auth.RoleNone:
		return errNoEnvironment()
	case have < role:
		return apierr.RoleRequired(role.String(), "environment")
	}
	return nil
}

// mayReadEnv reports whether the request's principal may read the
// environment of that ID (a list's filter).
func mayReadEnv(r *http.Request, id string) bool {
	t, _ := auth.ProjectFrom(r.Context())
	if t.MayOnProject(auth.RoleRead) {
		return true
	}
	u, err := uuid.Parse(id)
	return err == nil && t.EnvRole(u) >= auth.RoleRead
}

// sandbox is the sandbox class: the bearer is a sandbox's token, and the
// {id} of the path is a claim the token must back. A token of another
// sandbox, a project's token and an unknown one all get the answer of an
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
// (and the sandbox class): the sandbox it is a token of, ignoring a project
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
