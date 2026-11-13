// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ferunauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gardenlinux/feos/go/internal/oidc"
	"github.com/gardenlinux/feos/go/internal/visit"
)

// This file is the sign-in: GET /visit takes the challenge the ingress
// sent the browser with and sends it on to the issuer, GET /callback/…
// takes the issuer's answer and answers the challenge at ferund.
//
// Every provider has a redirect URI of its own: <public-url>/callback/ferun
// for kind ferun, <public-url>/callback/<tenant>/<provider> for kind oidc.
// The flow is bound to it and to the issuer and client it began with, so
// that a code one issuer gave cannot be finished as another provider's
// flow: with one URI for all, a tenant's issuer could send the browser on
// to another issuer, have its code come back under the tenant's own state,
// and read it at its token endpoint (the OAuth mix-up attack).

// The provider kinds ferun-auth serves (ferund's model.ProviderKind…).
const (
	kindFerun = "ferun"
	kindOIDC  = "oidc"
)

// The sign-in in flight: a cookie of its own per flow on ferun-auth's
// host, sealed under --key-file.
const (
	flowCookiePrefix = "__Host-ferun_auth_"
	flowAAD          = "ferun-auth-flow/1\x00"
	flowLifetime     = 10 * time.Minute
	maxFlowCookie    = 3800
	maxFlows         = 8
	// maxState bounds the state a callback brings: ferun-auth's are 43
	// characters.
	maxState = 128
)

// The bounds of what ferund takes at an accept (ADR 0054, §1.8.3).
const (
	maxSubject = 255
	maxEmail   = 254
	maxName    = 200
	maxSession = 255
)

// flow is a sign-in in flight, sealed in its cookie: the challenge, the ID
// token's nonce, the PKCE verifier, its end (Unix seconds), the provider's
// callback path (callbackOf) and the relying party it began with
// (rpDigest).
type flow struct {
	Challenge string `json:"ch"`
	Nonce     string `json:"nonce"`
	Verifier  string `json:"ver"`
	Expires   int64  `json:"exp"`
	Callback  string `json:"cb"`
	RP        string `json:"rp"`
}

// callbackPrefix starts every redirect URI's path.
const callbackPrefix = "/callback/"

// callbackOf is the path after callbackPrefix of a challenge's provider:
// "ferun" for kind ferun, "<tenant>/<provider>" for kind oidc. ok is false
// for a name that is no DNS label (ferund makes none).
func callbackOf(ch challengeView) (string, bool) {
	switch ch.Provider.Kind {
	case kindFerun:
		return kindFerun, true
	case kindOIDC:
		t, n := ch.Environment.Tenant, ch.Provider.Name
		return t + "/" + n, nameRE.MatchString(t) && nameRE.MatchString(n)
	}
	return "", false
}

// validCallback reports whether cb can be a provider's callback path.
func validCallback(cb string) bool {
	if cb == kindFerun {
		return true
	}
	t, n, ok := strings.Cut(cb, "/")
	return ok && nameRE.MatchString(t) && nameRE.MatchString(n)
}

// rpDigest names a client at an issuer in a flow: base64url(sha256) of
// both, so that a long issuer does not crowd the cookie.
func rpDigest(rp relyingParty) string {
	sum := sha256.Sum256([]byte(rp.issuer + "\x00" + rp.clientID))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// flowKey is the cookie's k of a state: the first 16 characters of
// base64url(sha256(state)).
func flowKey(state string) string {
	sum := sha256.Sum256([]byte(state))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:16]
}

// randomString is 32 random bytes, base64url without padding.
func randomString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// flowCookie is the cookie of k: value "" with maxAge -1 clears it.
func flowCookie(k, value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: flowCookiePrefix + k, Value: value, Path: "/", MaxAge: maxAge, Secure: true,
		HttpOnly: true, SameSite: http.SameSiteLaxMode}
}

// sealFlow seals f for the cookie of k.
func (s *Server) sealFlow(k string, f flow) (string, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	sealed, err := s.sealer.Seal(b, []byte(flowAAD+k))
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// openFlow opens the cookie value of k; ok is false for one that does not
// open or has passed its end.
func (s *Server) openFlow(k, value string) (flow, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return flow{}, false
	}
	b, err := s.sealer.Open(raw, []byte(flowAAD+k))
	if err != nil {
		return flow{}, false
	}
	var f flow
	if json.Unmarshal(b, &f) != nil || f.Challenge == "" || f.Nonce == "" || f.Verifier == "" || f.Callback == "" || f.RP == "" {
		return flow{}, false
	}
	if s.now().Unix() > f.Expires {
		return flow{}, false
	}
	return f, true
}

// isNavigation reports whether a request is the browser's own top-level
// navigation, not a fetch, a frame or a prefetch: Sec-Fetch-Mode navigate
// and Sec-Fetch-Dest document, both required.
func isNavigation(r *http.Request) bool {
	h := r.Header
	if h.Get("Sec-Fetch-Mode") != "navigate" || h.Get("Sec-Fetch-Dest") != "document" {
		return false
	}
	for _, name := range []string{"Sec-Purpose", "Purpose"} {
		for _, v := range h.Values(name) {
			if strings.Contains(strings.ToLower(v), "prefetch") {
				return false
			}
		}
	}
	return true
}

// sameHostURL reports whether s is an https URL on host: where ferund
// sends the browser (redirect and restart) is the challenge's host.
func sameHostURL(s, host string) bool {
	u, err := url.Parse(s)
	return err == nil && isHTTPS(s) && strings.EqualFold(u.Host, host)
}

// ferundFailed answers a failed call to ferund other than the route's own
// refusals: 502, logged at error when ferund refused the token.
func (s *Server) ferundFailed(w http.ResponseWriter, what string, err error) {
	if refusedToken(err) {
		s.log.Error("ferund refused ferun-auth's token", "call", what, "err", err)
	} else {
		s.log.Warn("ferun-auth: ferund failed", "call", what, "err", err)
	}
	page(w, http.StatusBadGateway, msgUnavailable)
}

// openChallenge opens a challenge at ferund and answers the browser where
// that ends the request: ok is false then.
func (s *Server) openChallenge(ctx context.Context, w http.ResponseWriter, sealed string) (challengeView, bool) {
	ch, err := s.ferund.openChallenge(ctx, sealed)
	switch {
	case statusOf(err) == http.StatusNotFound:
		page(w, http.StatusBadRequest, msgNotValid)
		return ch, false
	case err != nil:
		s.ferundFailed(w, "open the challenge", err)
		return ch, false
	}
	if !sameHostURL(ch.Restart, ch.Host) {
		s.log.Error("ferun-auth: ferund's restart address is not on the challenge's host", "env", ch.Environment.ID)
		page(w, http.StatusBadGateway, msgUnavailable)
		return ch, false
	}
	if ch.Expired || ch.Answered {
		seeOther(w, ch.Restart)
		return ch, false
	}
	return ch, true
}

// relyingParty is a client at an issuer.
type relyingParty struct {
	p                *oidc.Provider
	issuer, clientID string
}

// client is the relying party a challenge's provider signs in with: the
// installation's for kind ferun, the tenant's for kind oidc. ok is false
// when the browser was answered.
func (s *Server) client(w http.ResponseWriter, ch challengeView) (relyingParty, bool) {
	switch ch.Provider.Kind {
	case kindFerun:
		if s.cfg.Issuer == "" {
			page(w, http.StatusServiceUnavailable, msgNoFerun)
			return relyingParty{}, false
		}
		return relyingParty{s.issuers.get(s.cfg.Issuer, s.cfg.ClientID, s.sec.clientSecret), s.cfg.Issuer, s.cfg.ClientID}, true
	case kindOIDC:
		o := ch.OIDC
		if o == nil || o.Issuer == "" || o.ClientID == "" || o.ClientSecret == "" || !isHTTPS(o.Issuer) {
			s.log.Error("ferun-auth: ferund gave a challenge of kind oidc without its issuer and client", "env", ch.Environment.ID)
			page(w, http.StatusBadGateway, msgUnavailable)
			return relyingParty{}, false
		}
		return relyingParty{s.issuers.get(o.Issuer, o.ClientID, o.ClientSecret), o.Issuer, o.ClientID}, true
	}
	// ferund answers ferun-auth only for its kinds.
	page(w, http.StatusBadRequest, msgNotValid)
	return relyingParty{}, false
}

// issuerFailed answers an issuer's failure: 400 for a sign-in the checks
// refuse, 502 for an issuer that cannot be used. The log has the reason,
// never a token or a code.
func (s *Server) issuerFailed(w http.ResponseWriter, ch challengeView, err error) {
	var re *oidc.ErrRefused
	if errors.As(err, &re) {
		s.log.Warn("ferun-auth: the sign-in did not verify", "env", ch.Environment.ID, "provider", ch.Provider.Name,
			"reason", re.Reason)
		page(w, http.StatusBadRequest, msgCannotFinish)
		return
	}
	s.log.Warn("ferun-auth: the issuer cannot be used", "env", ch.Environment.ID, "provider", ch.Provider.Name, "err", err)
	page(w, http.StatusBadGateway, msgUnavailable)
}

// visit is GET /visit?challenge=<sealed>: the browser comes from the
// ingress, and goes on to the issuer with a flow of its own.
func (s *Server) visit(w http.ResponseWriter, r *http.Request) {
	if !isNavigation(r) {
		page(w, http.StatusBadRequest, msgOpenInBrowser)
		return
	}
	sealed := r.URL.Query().Get("challenge")
	if sealed == "" || len(sealed) > visit.MaxSealed {
		page(w, http.StatusBadRequest, msgNotValid)
		return
	}
	ch, ok := s.openChallenge(r.Context(), w, sealed)
	if !ok {
		return
	}
	rp, ok := s.client(w, ch)
	if !ok {
		return
	}
	cb, ok := callbackOf(ch)
	if !ok {
		s.log.Error("ferun-auth: ferund gave a challenge whose provider has no callback path", "env", ch.Environment.ID)
		page(w, http.StatusBadGateway, msgUnavailable)
		return
	}
	done, ok := s.issuers.enter(rp.issuer)
	if !ok {
		s.issuerFailed(w, ch, errBusy)
		return
	}
	d, err := s.issuers.discover(r.Context(), rp)
	done()
	if err != nil {
		s.issuerFailed(w, ch, err)
		return
	}
	var state, nonce, verifier string
	for _, v := range []*string{&state, &nonce, &verifier} {
		if *v, err = randomString(); err != nil {
			s.log.Error("ferun-auth: random", "err", err)
			page(w, http.StatusInternalServerError, msgUnavailable)
			return
		}
	}
	k := flowKey(state)
	value, err := s.sealFlow(k, flow{Challenge: sealed, Nonce: nonce, Verifier: verifier,
		Expires: s.now().Add(flowLifetime).Unix(), Callback: cb, RP: rpDigest(rp)})
	if err != nil {
		s.log.Error("ferun-auth: seal the flow", "err", err)
		page(w, http.StatusInternalServerError, msgUnavailable)
		return
	}
	cookie := flowCookie(k, value, int(flowLifetime/time.Second))
	if len(cookie.String()) > maxFlowCookie {
		page(w, http.StatusBadRequest, msgNotValid)
		return
	}
	auth, err := url.Parse(d.AuthorizationEndpoint)
	if err != nil {
		s.issuerFailed(w, ch, errEndpoint)
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	q := auth.Query()
	q.Set("response_type", "code")
	q.Set("client_id", rp.clientID)
	q.Set("redirect_uri", s.redirectURI(cb))
	q.Set("scope", strings.Join(s.cfg.Scopes, " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("code_challenge_method", "S256")
	auth.RawQuery = q.Encode()
	for _, old := range s.oldFlows(r) {
		http.SetCookie(w, flowCookie(old, "", -1))
	}
	http.SetCookie(w, cookie)
	s.log.Info("ferun-auth: sign-in started", "env", ch.Environment.ID, "tenant", ch.Environment.Tenant,
		"provider", ch.Provider.Name, "kind", ch.Provider.Kind)
	seeOther(w, auth.String())
}

// oldFlows are the flow cookies of the request to drop so that, with the
// one about to be set, at most maxFlows remain: those that do not open
// first, then the oldest.
func (s *Server) oldFlows(r *http.Request) []string {
	type held struct {
		k   string
		exp int64
	}
	var flows []held
	seen := map[string]bool{}
	for _, c := range r.Cookies() {
		k, ok := strings.CutPrefix(c.Name, flowCookiePrefix)
		if !ok || seen[k] {
			continue
		}
		seen[k] = true
		f, ok := s.openFlow(k, c.Value)
		if !ok {
			f.Expires = 0
		}
		flows = append(flows, held{k, f.Expires})
	}
	n := len(flows) - (maxFlows - 1)
	if n <= 0 {
		return nil
	}
	sort.SliceStable(flows, func(i, j int) bool { return flows[i].exp < flows[j].exp })
	out := make([]string, 0, n)
	for _, f := range flows[:n] {
		out = append(out, f.k)
	}
	return out
}

// callbackBudget bounds a callback's calls together (ferund's open, the
// issuer's discovery, token, key set and userinfo, ferund's answer): the
// browser gets its answer before the Gateway's 15 s give up on it.
const callbackBudget = 12 * time.Second

// callback is GET /callback/<cb>?code=…&state=… (or error=…), cb the
// provider's callback path: the browser comes back from the issuer, and
// goes on to the ingress with ferund's code.
func (s *Server) callback(w http.ResponseWriter, r *http.Request, cb string) {
	ctx, cancel := context.WithTimeout(r.Context(), callbackBudget)
	defer cancel()
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" || len(state) > maxState {
		if q.Get("error") != "" {
			page(w, http.StatusBadRequest, msgIssuerRefused)
			return
		}
		page(w, http.StatusBadRequest, msgCannotFinish)
		return
	}
	k := flowKey(state)
	// The flow is used once, whatever follows.
	http.SetCookie(w, flowCookie(k, "", -1))
	if q.Get("error") != "" {
		page(w, http.StatusBadRequest, msgIssuerRefused)
		return
	}
	c, err := r.Cookie(flowCookiePrefix + k)
	if err != nil {
		page(w, http.StatusBadRequest, msgCannotFinish)
		return
	}
	f, ok := s.openFlow(k, c.Value)
	if !ok {
		page(w, http.StatusBadRequest, msgCannotFinish)
		return
	}
	// The issuer answered at another provider's redirect URI than the
	// flow's: a code of one provider brought to another's flow.
	if cb != f.Callback {
		s.log.Warn("ferun-auth: a callback at another provider's redirect URI than its flow's", "at", cb, "flow", f.Callback)
		page(w, http.StatusBadRequest, msgCannotFinish)
		return
	}
	ch, ok := s.openChallenge(ctx, w, f.Challenge)
	if !ok {
		return
	}
	rp, ok := s.client(w, ch)
	if !ok {
		return
	}
	// The provider changed since the flow began (its issuer or client, or
	// the environment's provider): finish nothing of the old one.
	if now, ok := callbackOf(ch); !ok || now != f.Callback || rpDigest(rp) != f.RP {
		s.log.Warn("ferun-auth: the challenge's provider is not the one its sign-in began with", "env", ch.Environment.ID,
			"provider", ch.Provider.Name)
		page(w, http.StatusBadRequest, msgCannotFinish)
		return
	}
	done, ok := s.issuers.enter(rp.issuer)
	if !ok {
		s.issuerFailed(w, ch, errBusy)
		return
	}
	defer done()
	d, err := s.issuers.discover(ctx, rp)
	if err != nil {
		s.issuerFailed(w, ch, err)
		return
	}
	// RFC 9207: an issuer that names itself must be the one asked, and one
	// that says it always does must have.
	iss, named := q["iss"]
	if named && (len(iss) != 1 || iss[0] != rp.issuer) || !named && d.IssParameterSupported {
		s.log.Warn("ferun-auth: the callback's iss is not the issuer's", "env", ch.Environment.ID, "provider", ch.Provider.Name,
			"named", named)
		page(w, http.StatusBadRequest, msgCannotFinish)
		return
	}
	code := q.Get("code")
	if code == "" {
		page(w, http.StatusBadRequest, msgCannotFinish)
		return
	}
	v, err := s.signIn(ctx, rp.p, d, code, f)
	done()
	if err != nil {
		s.issuerFailed(w, ch, err)
		return
	}
	if !validText(v.Sub, maxSubject) {
		s.log.Warn("ferun-auth: the issuer's subject cannot be a visit's", "env", ch.Environment.ID, "provider", ch.Provider.Name)
		page(w, http.StatusBadRequest, msgCannotFinish)
		return
	}
	var a answer
	if ch.Provider.Kind == kindOIDC && !ch.OIDC.Rule.admits(v) {
		a, err = s.ferund.reject(ctx, f.Challenge)
		s.log.Info("ferun-auth: the provider's rule does not admit the visitor", "env", ch.Environment.ID,
			"tenant", ch.Environment.Tenant, "provider", ch.Provider.Name)
	} else {
		a, err = s.ferund.accept(ctx, s.acceptOf(ch, f.Challenge, v))
	}
	switch {
	case statusOf(err) == http.StatusNotFound:
		page(w, http.StatusBadRequest, msgNotValid)
		return
	case statusOf(err) == http.StatusConflict && (codeOf(err) == "challenge_expired" || codeOf(err) == "challenge_answered"):
		if ch, ok := s.openChallenge(ctx, w, f.Challenge); ok {
			// Neither expired nor answered after all: begin again anyway.
			seeOther(w, ch.Restart)
		}
		return
	case err != nil:
		s.ferundFailed(w, "answer the challenge", err)
		return
	}
	if !sameHostURL(a.Redirect, ch.Host) {
		s.log.Error("ferun-auth: ferund's redirect is not on the challenge's host", "env", ch.Environment.ID)
		page(w, http.StatusBadGateway, msgUnavailable)
		return
	}
	s.log.Info("ferun-auth: challenge answered", "env", ch.Environment.ID, "tenant", ch.Environment.Tenant,
		"provider", ch.Provider.Name, "kind", ch.Provider.Kind, "admitted", a.Admitted)
	seeOther(w, a.Redirect)
}

// acceptOf is the accept of a visitor: the email only when verified, and
// what ferund would refuse left out, logged without its value: a dropped
// session means the issuer's back-channel logout cannot end the visit.
func (s *Server) acceptOf(ch challengeView, challenge string, v visitor) acceptBody {
	a := acceptBody{Challenge: challenge, Subject: v.Sub, Email: verifiedEmail(v), Name: clip(v.Name, maxName),
		Session: optional(v.Session, maxSession)}
	if v.Session != "" && a.Session == "" {
		s.log.Warn("ferun-auth: the issuer's session ID cannot be a visit's, so its back-channel logout cannot end the visit",
			"env", ch.Environment.ID, "provider", ch.Provider.Name, "bytes", len(v.Session))
	}
	if v.EmailVerified && v.Email != "" && a.Email == "" {
		s.log.Warn("ferun-auth: the issuer's email cannot be a visit's; the visit is made without it",
			"env", ch.Environment.ID, "provider", ch.Provider.Name, "bytes", len(v.Email))
	}
	return a
}

// signIn trades the issuer's code for the visitor: the code exchange with
// the flow's PKCE verifier, the ID token with its nonce, userinfo as
// ferund's own sign-in reads it.
func (s *Server) signIn(ctx context.Context, p *oidc.Provider, d *oidc.Discovery, code string, f flow) (visitor, error) {
	toks, err := p.Exchange(ctx, d, code, s.redirectURI(f.Callback), f.Verifier)
	if err != nil {
		return visitor{}, err
	}
	idt, err := p.Verify(ctx, d, toks.IDToken, f.Nonce)
	if err != nil {
		return visitor{}, err
	}
	info, err := p.Userinfo(ctx, d, toks.AccessToken, idt.Claims.Str("sub"))
	if err != nil {
		return visitor{}, err
	}
	return visitorOf(idt.Claims, info), nil
}

// verifiedEmail is the email ferund is told: only a verified one, and
// only one it takes.
func verifiedEmail(v visitor) string {
	if !v.EmailVerified {
		return ""
	}
	return optional(v.Email, maxEmail)
}

// validText reports whether s is 1 to max bytes of UTF-8 without control
// characters: what ferund takes at an accept.
func validText(s string, max int) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return false
		}
	}
	return true
}

// optional is s when ferund takes it, else "": the visit is made without
// it rather than not at all.
func optional(s string, max int) string {
	if !validText(s, max) {
		return ""
	}
	return s
}

// clip is a display name as ferund takes it: control characters dropped,
// cut to max bytes at a character's boundary.
func clip(s string, max int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > max {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
