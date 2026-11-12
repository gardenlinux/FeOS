// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/visit"
)

// The visits' timing at the ingress (ADR 0054, spec §1.1).
const (
	// renewInline bounds a renewal on a request whose token is still
	// valid: on any failure it is served with the token it has.
	renewInline = 2 * time.Second
	// renewExpired bounds one whose token is past its end and leeway.
	renewExpired = 5 * time.Second
	// renewBackground bounds one of the background renewal.
	renewBackground = 5 * time.Second
	// renewRetry is how long after a failed renewal a visit's requests
	// go on with the last failure instead of calling again.
	renewRetry = 5 * time.Second
	// visitEvery is the period of the background renewal.
	visitEvery = 10 * time.Second
	// maxBackground is how many background renewals run at once: half
	// the agent's renewal slots, so that the ingress's own background
	// work never fills them for the requests' renewals.
	maxBackground = 4
)

// The bounds of what the proxy remembers of visits.
const (
	maxVisitEntries = 1 << 16
	maxEndedVisits  = 1 << 16
)

// textVisitRequired answers a request of a guarded environment without a
// visit that is no navigation.
const textVisitRequired = "this site needs a visit: open it in a browser"

// errBadRenewal is a renewal whose answer the ingress cannot take: a
// token that does not verify or is not the visit's, or another outcome.
var errBadRenewal = errors.New("the control plane's renewal is not the visit's")

// visitReq is the visit of a request that passed the visit check: the
// token the application gets (the newest this instance has), the visit
// cookie's secret and the token's claims. cookie: the answer sets the
// visit cookie anew, with that token and the same secret.
type visitReq struct {
	token  string
	secret string
	claims visit.Token
	cookie bool
}

// navigation reports whether a request is a browser's navigation, which
// a guarded environment sends to sign in: GET, no Upgrade, no prefetch,
// and Sec-Fetch-Mode navigate, or without that header an Accept that
// names text/html.
func navigation(r *http.Request) bool {
	if r.Method != http.MethodGet || len(r.Header.Values("Upgrade")) > 0 {
		return false
	}
	for _, v := range r.Header.Values("Sec-Purpose") {
		if strings.Contains(strings.ToLower(v), "prefetch") {
			return false
		}
	}
	if modes := r.Header.Values("Sec-Fetch-Mode"); len(modes) > 0 {
		return len(modes) == 1 && strings.EqualFold(strings.TrimSpace(modes[0]), "navigate")
	}
	for _, v := range r.Header.Values("Accept") {
		if strings.Contains(strings.ToLower(v), "text/html") {
			return true
		}
	}
	return false
}

// fromAnotherSite reports whether a request says it comes from another
// site or another host of the same site (Sec-Fetch-Site same-site or
// cross-site). Hosts under one zone are one site, so a fetch of another
// tenant's page carries the visit cookie: it counts as without a visit
// unless it is a navigation.
func fromAnotherSite(r *http.Request) bool {
	for _, v := range r.Header.Values("Sec-Fetch-Site") {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "same-site", "cross-site":
			return true
		}
	}
	return false
}

// validHeaderValue reports whether s may stand as a header's value as it
// is: no control character but a tab, no DEL.
func validHeaderValue(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 && c != '\t' || c == 0x7f {
			return false
		}
	}
	return true
}

// setVisitor drops every visitor header a client sent, on every host
// (visit.DropVisitorHeaders), and sets the visit's when the request has
// one: the subject, the email where the provider gave one and the token,
// never the cookie's secret.
func setVisitor(h http.Header, v *visitReq) {
	visit.DropVisitorHeaders(h)
	if v == nil {
		return
	}
	h.Set(visit.HeaderSubject, v.claims.Subject)
	if v.claims.Email != "" {
		h.Set(visit.HeaderEmail, v.claims.Email)
	}
	h.Set(visit.HeaderToken, v.token)
}

// answerAs notes, for the access log, where an answer of the ingress's
// own about a visit went: the environment and the route's service.
func (q *request) answerAs(tg target) {
	q.env, q.tenant, q.service = tg.env.id, tg.env.tenant, tg.route.Service
}

// checkVisit is the visit check of a guarded environment (ADR 0054),
// after the route's from and before its service: it reports whether the
// request goes on, with q.visit set when it has a visit; otherwise it
// answered. An environment that cannot sign anyone in fails closed: a
// navigation gets 503, anything else 401; a request without a valid
// visit gets the sign-in when it is a navigation, 401 otherwise. Neither
// wakes, holds nor counts anything.
func (q *request) checkVisit(v *view, tg target) bool {
	q.visit = nil
	e := tg.env
	if !e.guarded() {
		return true
	}
	p := q.p
	nav := navigation(q.r)
	if e.access != wire.AccessVisitors || v.t.keys == nil || e.signIn == "" || p.d.Visits == nil {
		q.answerAs(tg)
		q.reason = wire.ReasonVisitUnavailable
		p.m.visitUnavailable.Add(1)
		p.rl.warn("visit-unavailable", "a guarded environment cannot sign anyone in; it serves nobody",
			"host", q.host, "env", e.id, "access", e.access, "keys", v.t.keys != nil, "signIn", e.signIn != "",
			"visits", p.d.Visits != nil)
		if nav {
			writePage(q.w, http.StatusServiceUnavailable, page{Text: pageNotHere}, 30)
		} else {
			q.writeVisitRequired(false)
		}
		return false
	}
	vr, clear := q.visitOf(v.t, e, nav)
	if vr != nil {
		q.visit = vr
		if vr.cookie {
			q.w.final = q.ownVisitCookie
		}
		return true
	}
	q.answerAs(tg)
	if nav {
		q.signIn(v.t, e, clear)
	} else {
		q.needVisit(clear)
	}
	return false
}

// needVisit answers 401 a request of a guarded environment that has no
// visit and is no navigation, and counts it; clear: its visit ended, and
// the answer clears the cookie.
func (q *request) needVisit(clear bool) {
	if q.reason == "" {
		q.reason = wire.ReasonVisitRequired
	}
	q.p.m.visitRequired.Add(1)
	q.writeVisitRequired(clear)
}

// writeVisitRequired writes needVisit's 401, also for an environment
// that cannot sign anyone in, which counts as unavailable instead.
func (q *request) writeVisitRequired(clear bool) {
	h := q.w.Header()
	if clear {
		clearVisitCookie(h)
	}
	h.Set("WWW-Authenticate", `FeRun-Visit realm="`+q.host+`"`)
	h.Set("Cache-Control", "no-store")
	reply{http.StatusUnauthorized, textVisitRequired, 0}.write(q.w)
}

// signIn sends a navigation without a visit to its environment's provider
// with a challenge the ingress sealed: the environment, the host, the
// path and query to return to and the hash of the challenge cookie's
// secret, which the answer sets (reused when the request carries one,
// so that two tabs signing in at once share it).
func (q *request) signIn(t *table, e *envRoutes, clear bool) {
	p := q.p
	q.reason = wire.ReasonVisitSignIn
	secret := ""
	for _, c := range q.r.CookiesNamed(visit.CookieChallenge) {
		if visit.ValidBindingSecret(c.Value) {
			secret = c.Value
			break
		}
	}
	var err error
	if secret == "" {
		if secret, err = visit.NewBindingSecret(); err != nil {
			q.signInFailed(err)
			return
		}
	}
	id, err := visit.NewChallengeID()
	if err != nil {
		q.signInFailed(err)
		return
	}
	path := q.raw
	if q.r.URL.RawQuery != "" {
		path += "?" + q.r.URL.RawQuery
	}
	if len(path) > visit.MaxPath {
		path = "/"
	}
	now := time.Now()
	sealed, err := t.keys.SealChallenge(visit.Challenge{ID: id, Env: e.id, Host: q.host, Path: path,
		Binding: visit.BindingHash(secret), IssuedAt: now.Unix(), Expires: now.Add(visit.ChallengeLifetime).Unix()})
	if err != nil {
		q.signInFailed(err)
		return
	}
	sep := "?"
	if strings.Contains(e.signIn, "?") {
		sep = "&"
	}
	p.m.visitSignIns.Add(1)
	h := q.w.Header()
	if clear {
		clearVisitCookie(h)
	}
	setCookie(h, visit.CookieChallenge, secret, int(visit.ChallengeLifetime/time.Second))
	ferunHeaders(h)
	h.Set("Location", e.signIn+sep+"challenge="+sealed)
	q.w.WriteHeader(http.StatusSeeOther)
}

// signInFailed answers a sign-in the ingress could not make (no random
// bytes, a challenge it could not seal).
func (q *request) signInFailed(err error) {
	q.reason = wire.ReasonVisitUnavailable
	q.p.m.visitUnavailable.Add(1)
	q.p.rl.warn("visit-sign-in", "make a challenge failed", "host", q.host, "err", err)
	writePage(q.w, http.StatusServiceUnavailable, page{Text: pageNotNow}, 5)
}

// visitOf returns the request's visit of the guarded environment e, when
// it has a valid one: a visit cookie that splits into a token and a
// secret, whose token verifies with the table's keys and names the
// secret's hash, the environment, the host and the environment's
// provider, of a visit this instance did not see end, and that is in its
// time (timely). A request from another site or host that is no
// navigation has none (fromAnotherSite). clear: a cookie held a visit
// that ended, and the answer clears it.
func (q *request) visitOf(t *table, e *envRoutes, nav bool) (vr *visitReq, clear bool) {
	if !nav && fromAnotherSite(q.r) {
		return nil, false
	}
	p := q.p
	for _, c := range q.r.CookiesNamed(visit.CookieVisit) {
		token, secret, ok := visit.ParseVisitCookie(c.Value)
		if !ok {
			continue
		}
		claims, err := t.keys.VerifyToken(token)
		if err != nil {
			p.log.Debug("a visit cookie's token does not verify", "host", q.host, "err", err)
			continue
		}
		if !visitFor(claims, secret, e, q.host) {
			continue
		}
		if p.visits.isEnded(claims.Visit, time.Now()) {
			clear = true
			continue
		}
		got, ended := q.timely(t.keys, &visitReq{token: token, secret: secret, claims: claims})
		if got != nil {
			return got, false
		}
		clear = clear || ended
	}
	return nil, clear
}

// visitFor reports whether a token's claims are those of a visit of the
// environment e on host, held with secret, and fit into the application's
// headers.
func visitFor(c visit.Token, secret string, e *envRoutes, host string) bool {
	return c.Binding == visit.BindingHash(secret) && c.Env == e.id && c.Host == host && c.Provider == e.provider &&
		validHeaderValue(c.Subject) && validHeaderValue(c.Email)
}

// timely returns the visit when its token counts now: a newer token this
// instance got for it is taken first; one valid up to its end and the
// leeway counts, and is renewed in line within renewInline when it has
// less than visit.RenewBefore left, served on when that fails; one past
// it is renewed within renewExpired and counts until the visit's end
// only when the control plane is away (the outage rule). ended: the
// renewal said the visit ended.
func (q *request) timely(keys *visit.Keys, vr *visitReq) (*visitReq, bool) {
	p := q.p
	now := time.Now()
	if raw, claims, ok := p.visits.latest(vr.claims, now); ok {
		vr = &visitReq{token: raw, secret: vr.secret, claims: claims, cookie: true}
	}
	exp := time.Unix(vr.claims.Expires, 0)
	if !now.After(exp.Add(visit.Leeway)) {
		if exp.Sub(now) >= visit.RenewBefore {
			return vr, false
		}
		next, ended, err := p.renew(context.Background(), keys, vr, renewInline)
		switch {
		case err != nil:
			return vr, false
		case ended:
			return nil, true
		}
		return next, false
	}
	if !now.Before(time.Unix(vr.claims.VisitEnds, 0)) {
		return nil, false
	}
	next, ended, err := p.renew(context.Background(), keys, vr, renewExpired)
	switch {
	case err == nil && ended:
		return nil, true
	case err == nil:
		return next, false
	case away(err):
		p.log.Debug("served a visit past its token's end: the control plane is away", "host", q.host,
			"visit", vr.claims.Visit, "err", err)
		return vr, false
	}
	return nil, false
}

// away reports whether a failed call means the control plane is away:
// not connected, unavailable, without the method, or not in time.
func away(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.Unimplemented, codes.DeadlineExceeded:
		return true
	}
	return false
}

// ownVisitCookie sets the visit cookie of a renewed token on an answer
// of the ingress's own after the visit check passed (the recorder's
// final hook), unless the answer carries it already (ModifyResponse put
// it on the application's).
func (q *request) ownVisitCookie(h http.Header) {
	if q.visit == nil || !q.visit.cookie {
		return
	}
	for _, v := range h["Set-Cookie"] {
		if strings.HasPrefix(v, visit.CookieVisit+"=") {
			return
		}
	}
	setVisitCookie(h, q.visit.token, q.visit.secret, q.visit.claims.VisitEnds, time.Now())
}

// visitCookieOn sets the visit cookie of a renewed token on the
// application's answer (ModifyResponse, so also on a 101, and after any
// 1xx, which clears what was set before the call).
func (q *request) visitCookieOn(h http.Header) {
	if q.visit == nil || !q.visit.cookie {
		return
	}
	setVisitCookie(h, q.visit.token, q.visit.secret, q.visit.claims.VisitEnds, time.Now())
}

// renew renews a visit's token at the control plane within bound, one
// call at a time per visit cookie on this instance (others wait for it,
// within their bound), and after a failure not again for renewRetry,
// unless the failure was a local busy. A call it makes ends with ctx: a
// request's renewal passes one that its own cancellation does not end,
// the background renewal its loop's. It returns the visit with the next
// token, or ended, or the failure.
func (p *Proxy) renew(ctx context.Context, keys *visit.Keys, vr *visitReq, bound time.Duration) (*visitReq, bool, error) {
	b := p.visits
	key := vr.claims.Binding
	now := time.Now()
	b.mu.Lock()
	e := b.entryLocked(key, vr.claims.Visit, false)
	if e.flight == nil && now.Before(e.retryAfter) {
		err := e.lastErr
		b.mu.Unlock()
		return nil, false, err
	}
	f := e.flight
	first := f == nil
	if first {
		f = &renewFlight{done: make(chan struct{})}
		e.flight = f
	}
	b.mu.Unlock()
	if first {
		p.fly(ctx, e, f, keys, vr, bound)
	} else {
		t := time.NewTimer(bound)
		defer t.Stop()
		select {
		case <-f.done:
		case <-t.C:
			return nil, false, status.Error(codes.DeadlineExceeded, "the visit's renewal took too long")
		}
	}
	if f.err != nil {
		return nil, false, f.err
	}
	if f.ended {
		return nil, true, nil
	}
	return &visitReq{token: f.token, secret: vr.secret, claims: f.claims, cookie: true}, false, nil
}

// fly makes the call of a renewal flight and records its answer.
func (p *Proxy) fly(ctx context.Context, e *visitEntry, f *renewFlight, keys *visit.Keys, vr *visitReq,
	bound time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, bound)
	res, err := p.d.Visits.RenewVisit(ctx, wire.RenewVisitRequest{Token: vr.token, Secret: vr.secret})
	cancel()
	switch {
	case err != nil:
		f.err = err
	case res.Outcome == wire.OutcomeRenewed:
		c, verr := keys.VerifyToken(res.Token)
		if verr != nil || c.Visit != vr.claims.Visit || c.Binding != vr.claims.Binding || c.Env != vr.claims.Env ||
			c.Host != vr.claims.Host || c.Provider != vr.claims.Provider || !validHeaderValue(c.Subject) ||
			!validHeaderValue(c.Email) {
			f.err = errBadRenewal
		} else {
			f.token, f.claims = res.Token, c
		}
	case res.Outcome == wire.OutcomeEnded:
		f.ended, f.reason = true, res.Reason
	default:
		f.err = errBadRenewal
	}
	now := time.Now()
	var cut []*liveAttempt
	b := p.visits
	b.mu.Lock()
	e.flight = nil
	switch {
	case f.err != nil:
		if status.Code(f.err) != codes.ResourceExhausted {
			e.retryAfter, e.lastErr = now.Add(renewRetry), f.err
		}
	case f.ended:
		cut = b.endLocked(vr.claims.Visit, vr.claims.Binding, f.reason, vr.claims.VisitEnds, now)
	default:
		e.retryAfter, e.lastErr = time.Time{}, nil
		if f.claims.Expires > e.latestClaims.Expires {
			e.latest, e.latestClaims = f.token, f.claims
		}
	}
	b.mu.Unlock()
	close(f.done)
	switch {
	case f.err != nil:
		p.m.visitFailed.Add(1)
		p.rl.warn("visit-renew", "renew a visit failed; it goes on with its token", "visit", vr.claims.Visit,
			"env", vr.claims.Env, "err", f.err)
	case f.ended:
		p.m.visitEnded.Add(1)
		p.log.Debug("a visit ended at its renewal", "visit", vr.claims.Visit, "env", vr.claims.Env, "reason", f.reason)
		closeAttempts(cut, errVisitEnded)
	default:
		p.m.visitRenewed.Add(1)
	}
}

// visitBook is what the proxy knows of visits: per visit cookie (by the
// token's binding) the renewal in flight, the last failure, the newest
// token this instance got and the live attempts; and the visits it saw
// end, until their end, so that no outage revives them. Beside them, under
// openMu, the live attempts without a visit, by environment, so that an
// environment that turns guarded closes what it let in while it was open.
type visitBook struct {
	mu      sync.Mutex
	entries map[string]*visitEntry
	ended   map[string]time.Time

	openMu sync.Mutex
	open   map[string]map[*liveAttempt]struct{}
}

// visitEntry is one visit cookie's: its visit, the renewal in flight,
// when a renewal may be tried again after lastErr, the newest token and
// the live attempts.
type visitEntry struct {
	visit        string
	flight       *renewFlight
	retryAfter   time.Time
	lastErr      error
	latest       string
	latestClaims visit.Token
	live         map[*liveAttempt]struct{}
}

// renewFlight is a renewal in flight, which concurrent requests of the
// same visit cookie wait for: the next token and its claims, or ended
// with the reason, or the failure.
type renewFlight struct {
	done   chan struct{}
	token  string
	claims visit.Token
	ended  bool
	reason string
	err    error
}

// liveAttempt is one forwarded attempt: cancel ends it (its upgraded
// connection or streaming answer among it). An attempt of a visit has the
// token and secret the background renewal renews with; one without a
// visit (visit empty) was let in by an environment that was open.
type liveAttempt struct {
	cancel   context.CancelCauseFunc
	binding  string
	visit    string
	env      string
	host     string
	provider string
	token    string
	secret   string
	claims   visit.Token
}

// The causes an attempt is cancelled with: its visit ended, or its
// environment no longer admits it.
var (
	errVisitEnded    = errors.New("the visit ended")
	errAccessChanged = errors.New("the environment no longer admits the request")
)

func newVisitBook() *visitBook {
	return &visitBook{entries: map[string]*visitEntry{}, ended: map[string]time.Time{},
		open: map[string]map[*liveAttempt]struct{}{}}
}

// entryLocked returns the entry of a visit cookie, made when missing.
// When the book is full and nothing can be pruned, a new entry is not
// kept, unless force (an attempt, which the in-flight cap bounds).
func (b *visitBook) entryLocked(binding, vid string, force bool) *visitEntry {
	if e := b.entries[binding]; e != nil {
		return e
	}
	e := &visitEntry{visit: vid}
	if len(b.entries) >= maxVisitEntries {
		b.pruneLocked(time.Now())
	}
	if force || len(b.entries) < maxVisitEntries {
		b.entries[binding] = e
	}
	return e
}

// latest returns the newest token this instance got for a visit cookie
// when it is newer than the claims' token and still counts.
func (b *visitBook) latest(c visit.Token, now time.Time) (string, visit.Token, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[c.Binding]
	if e == nil || e.latest == "" || e.latestClaims.Visit != c.Visit || e.latestClaims.Expires <= c.Expires ||
		now.After(time.Unix(e.latestClaims.Expires, 0).Add(visit.Leeway)) {
		return "", visit.Token{}, false
	}
	return e.latest, e.latestClaims, true
}

// isEnded reports whether this instance saw the visit end.
func (b *visitBook) isEnded(vid string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	end, ok := b.ended[vid]
	return ok && now.Before(end)
}

// endLocked records that a visit ended and returns the live attempts to
// close: with reason "unknown" only the cookie of binding held no visit,
// so only its attempts end and nothing is remembered; any other reason
// ends the visit, remembered until its end vend.
func (b *visitBook) endLocked(vid, binding, reason string, vend int64, now time.Time) []*liveAttempt {
	var cut []*liveAttempt
	take := func(e *visitEntry) {
		e.latest, e.latestClaims = "", visit.Token{}
		for a := range e.live {
			cut = append(cut, a)
		}
	}
	if reason == "unknown" {
		if e := b.entries[binding]; e != nil {
			take(e)
		}
		return cut
	}
	for _, e := range b.entries {
		if e.visit == vid {
			take(e)
		}
	}
	end := time.Unix(vend, 0)
	if now.Before(end) {
		if _, ok := b.ended[vid]; !ok && len(b.ended) >= maxEndedVisits {
			maps.DeleteFunc(b.ended, func(_ string, at time.Time) bool { return !now.Before(at) })
			if len(b.ended) >= maxEndedVisits {
				// Forget the visit that ends first: the outage rule
				// would let it count the shortest.
				first := ""
				for id, at := range b.ended {
					if first == "" || at.Before(b.ended[first]) {
						first = id
					}
				}
				delete(b.ended, first)
			}
		}
		b.ended[vid] = end
	}
	return cut
}

// end records that a visit ended, as endLocked, and closes its live
// attempts.
func (b *visitBook) end(vid, binding, reason string, vend int64) int {
	b.mu.Lock()
	cut := b.endLocked(vid, binding, reason, vend, time.Now())
	b.mu.Unlock()
	closeAttempts(cut, errVisitEnded)
	return len(cut)
}

func closeAttempts(cut []*liveAttempt, cause error) {
	for _, a := range cut {
		a.cancel(cause)
	}
}

// admits reports whether the table t admits a live attempt still: its
// host serves its environment, which admits anyone, or, for an attempt
// of a visit, the visitors of the same provider. An attempt without a
// visit came in while the environment was open, so a guarded one ends
// it.
func admits(t *table, a *liveAttempt) bool {
	er := t.env(a.host)
	if er == nil || er.id != a.env {
		return false
	}
	if !er.guarded() {
		return true
	}
	return a.visit != "" && er.access == wire.AccessVisitors && er.provider == a.provider
}

// attach records a forwarded attempt to the environment e on host, of
// the visit vr or without one (vr nil), which cancel ends. A table that
// came after the request's visit check and no longer admits the attempt
// ends it at once: retable runs under the same lock after the table is
// stored, so it sees the attempt, or the attempt sees the table (cur).
func (b *visitBook) attach(vr *visitReq, e *envRoutes, host string, cancel context.CancelCauseFunc,
	cur func() *table) *liveAttempt {
	a := &liveAttempt{cancel: cancel, env: e.id, host: host}
	var ok bool
	if vr == nil {
		b.openMu.Lock()
		set := b.open[a.env]
		if set == nil {
			set = map[*liveAttempt]struct{}{}
			b.open[a.env] = set
		}
		set[a] = struct{}{}
		ok = admits(cur(), a)
		b.openMu.Unlock()
	} else {
		a.binding, a.visit, a.provider = vr.claims.Binding, vr.claims.Visit, e.provider
		a.token, a.secret, a.claims = vr.token, vr.secret, vr.claims
		b.mu.Lock()
		en := b.entryLocked(a.binding, a.visit, true)
		if en.live == nil {
			en.live = map[*liveAttempt]struct{}{}
		}
		en.live[a] = struct{}{}
		ok = admits(cur(), a)
		b.mu.Unlock()
	}
	if !ok {
		cancel(errAccessChanged)
	}
	return a
}

// detach forgets an attempt that ended.
func (b *visitBook) detach(a *liveAttempt) {
	if a.visit == "" {
		b.openMu.Lock()
		defer b.openMu.Unlock()
		if set := b.open[a.env]; set != nil {
			delete(set, a)
			if len(set) == 0 {
				delete(b.open, a.env)
			}
		}
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.entries[a.binding]; e != nil {
		delete(e.live, a)
	}
}

// retable closes the live attempts that the new table t no longer
// admits (admits): their host is gone or serves another environment;
// their environment turned guarded while they came in without a visit;
// or, of a visit, their environment admits nobody, or the visitors of
// another provider. An environment that admits anyone now keeps them.
// It returns how many it closed.
func (b *visitBook) retable(t *table) int {
	var cut []*liveAttempt
	b.mu.Lock()
	for _, e := range b.entries {
		for a := range e.live {
			if !admits(t, a) {
				cut = append(cut, a)
			}
		}
	}
	b.mu.Unlock()
	b.openMu.Lock()
	for _, set := range b.open {
		for a := range set {
			if !admits(t, a) {
				cut = append(cut, a)
			}
		}
	}
	b.openMu.Unlock()
	closeAttempts(cut, errAccessChanged)
	return len(cut)
}

// pruneLocked forgets the visits whose end passed and the entries that
// hold nothing that counts any more.
func (b *visitBook) pruneLocked(now time.Time) {
	maps.DeleteFunc(b.ended, func(_ string, at time.Time) bool { return !now.Before(at) })
	maps.DeleteFunc(b.entries, func(_ string, e *visitEntry) bool {
		return e.flight == nil && len(e.live) == 0 && !now.Before(e.retryAfter) &&
			(e.latest == "" || now.After(time.Unix(e.latestClaims.Expires, 0).Add(visit.Leeway)))
	})
}

// due is a visit cookie whose live attempts need its next token: the
// newest token of the attempts or of the book.
type due struct {
	vr   *visitReq
	cut  []*liveAttempt
	ends bool
}

// visitLoop prunes the book and renews the visits with live attempts
// every visitEvery until ctx ends.
func (p *Proxy) visitLoop(ctx context.Context) {
	t := time.NewTicker(visitEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.renewLive(ctx)
			p.redeems.prune(time.Now())
		}
	}
}

// renewLive is one turn of the background renewal: every visit cookie
// with live attempts on this instance whose newest token has less than
// visit.RenewBefore left is renewed (renewBackground, sharing the
// request path's single flight), the soonest to expire first, and an
// ended visit's attempts are closed; those past their visit's end are
// closed at once. A renewal that fails while the token is past its end
// and leeway closes its attempts unless the control plane is away (the
// outage rule, as for a request). A turn starts no renewal after
// visitEvery, so that it ends within visitEvery and renewBackground; what
// is left waits for the next turn. A page that only holds a WebSocket
// open sends no request that would renew it.
func (p *Proxy) renewLive(ctx context.Context) {
	start := time.Now()
	keys := p.cur.Load().t.visitKeys()
	var work []due
	b := p.visits
	b.mu.Lock()
	b.pruneLocked(start)
	for _, e := range b.entries {
		if len(e.live) == 0 || e.flight != nil {
			continue
		}
		var newest *liveAttempt
		for a := range e.live {
			if newest == nil || a.claims.Expires > newest.claims.Expires {
				newest = a
			}
		}
		vr := &visitReq{token: newest.token, secret: newest.secret, claims: newest.claims}
		if e.latest != "" && e.latestClaims.Expires > vr.claims.Expires {
			vr.token, vr.claims = e.latest, e.latestClaims
		}
		if !start.Before(time.Unix(vr.claims.VisitEnds, 0)) {
			work = append(work, due{ends: true, cut: slices.Collect(maps.Keys(e.live))})
			continue
		}
		if time.Unix(vr.claims.Expires, 0).Sub(start) < visit.RenewBefore {
			work = append(work, due{vr: vr})
		}
	}
	b.mu.Unlock()
	for _, w := range work {
		if w.ends {
			closeAttempts(w.cut, errVisitEnded)
		}
	}
	if keys == nil || p.d.Visits == nil {
		return
	}
	work = slices.DeleteFunc(work, func(w due) bool { return w.ends })
	slices.SortFunc(work, func(a, b due) int { return cmp.Compare(a.vr.claims.Expires, b.vr.claims.Expires) })
	sem := make(chan struct{}, maxBackground)
	var wg sync.WaitGroup
	defer wg.Wait()
	for _, w := range work {
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}
		if time.Since(start) >= visitEvery {
			<-sem
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			_, _, err := p.renew(ctx, keys, w.vr, renewBackground)
			if err != nil && !away(err) && ctx.Err() == nil {
				if n := b.cutExpired(w.vr.claims.Binding, time.Now()); n > 0 {
					p.log.Debug("closed the connections of a visit whose token ended and could not be renewed",
						"visit", w.vr.claims.Visit, "env", w.vr.claims.Env, "attempts", n, "err", err)
				}
			}
		}()
	}
}

// cutExpired closes the live attempts of a visit cookie when even its
// newest token, of its attempts or of the book, is past its end and
// leeway, and returns how many it closed.
func (b *visitBook) cutExpired(binding string, now time.Time) int {
	var cut []*liveAttempt
	b.mu.Lock()
	if e := b.entries[binding]; e != nil {
		newest := int64(0)
		if e.latest != "" {
			newest = e.latestClaims.Expires
		}
		for a := range e.live {
			newest = max(newest, a.claims.Expires)
		}
		if now.After(time.Unix(newest, 0).Add(visit.Leeway)) {
			cut = slices.Collect(maps.Keys(e.live))
		}
	}
	b.mu.Unlock()
	closeAttempts(cut, errVisitEnded)
	return len(cut)
}

// visitKeys are the table's keys of visits; nil without a table.
func (t *table) visitKeys() *visit.Keys {
	if t == nil {
		return nil
	}
	return t.keys
}
