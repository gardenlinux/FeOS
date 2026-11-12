// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/visit"
)

// The bounds of the calls under /.ferun/ (spec §1.1).
const (
	redeemTimeout = 5 * time.Second
	endTimeout    = 2 * time.Second
	// maxRedeemMemo bounds the codes whose outcome the instance
	// remembers.
	maxRedeemMemo = 10000
)

// textNoFerunPath answers a path under /.ferun/ that is not one of the
// ingress's.
const textNoFerunPath = "no such path"

// textCrossSiteSignOut answers a sign-out another site sent.
const textCrossSiteSignOut = "cross-site sign-out refused"

// ferunPath reports whether a request's escaped path is under /.ferun/,
// the ingress's own on every host (ADR 0054): the path or its unescaped
// form, each with runs of "/" made one, is /.ferun or starts with
// /.ferun/. sub is what follows /.ferun in the unescaped form ("/auth"),
// in the escaped one when it does not unescape.
func ferunPath(raw string) (sub string, ok bool) {
	under := func(p string) (string, bool) {
		p = collapseSlashes(p)
		if p == "/.ferun" {
			return "", true
		}
		if rest, ok := strings.CutPrefix(p, "/.ferun/"); ok {
			return "/" + rest, true
		}
		return "", false
	}
	if p, err := url.PathUnescape(raw); err == nil {
		if sub, ok := under(p); ok {
			return sub, true
		}
	}
	return under(raw)
}

// collapseSlashes makes every run of "/" in p one.
func collapseSlashes(p string) string {
	if !strings.Contains(p, "//") {
		return p
	}
	var b strings.Builder
	b.Grow(len(p))
	for i := 0; i < len(p); i++ {
		if p[i] == '/' && i > 0 && p[i-1] == '/' {
			continue
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

// ferun answers a request under /.ferun/ on a host of the environment e,
// whatever its access, before any route: it is never routed, wakes
// nothing and is not counted among the forwarded requests.
func (q *request) ferun(v *view, e *envRoutes, sub string) {
	q.env, q.tenant, q.reason = e.id, e.tenant, wire.ReasonFerunPath
	m := q.r.Method
	switch {
	case sub == "/auth" && m == http.MethodGet:
		q.auth(v.t, e)
	case sub == "/sign-out" && m == http.MethodGet:
		writePage(q.w, http.StatusOK, page{Text: pageSignOutAsk, SignOut: true}, 0)
	case sub == "/sign-out" && m == http.MethodPost:
		q.signOut(v.t)
	case sub == "/keys" && m == http.MethodGet && v.t.jwks != nil:
		h := q.w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Content-Length", strconv.Itoa(len(v.t.jwks)))
		h.Set("Cache-Control", "public, max-age=300")
		h.Set("X-Content-Type-Options", "nosniff")
		q.w.WriteHeader(http.StatusOK)
		_, _ = q.w.Write(v.t.jwks)
	default:
		ferunHeaders(q.w.Header())
		reply{http.StatusNotFound, textNoFerunPath, 0}.write(q.w)
	}
}

// back is the address to send a browser back to on the request's host:
// https, the host and the path made safe (visit.SafePath).
func (q *request) back(path string) string {
	return "https://" + q.host + visit.SafePath(path)
}

// auth is GET /.ferun/auth?code=…: a provider's answer, signed by the
// control plane, that the browser brought back. The code is checked here
// first, alone (a code nobody vouched for costs the control plane
// nothing), then an accepted one is redeemed with the challenge cookie of
// this browser, once per code on this instance, and the visit cookie set.
func (q *request) auth(t *table, e *envRoutes) {
	p := q.p
	raw := q.r.URL.Query().Get("code")
	c, ok := verifyCode(t, raw)
	if !ok || c.Env != e.id || c.Host != q.host {
		p.log.Debug("a sign-in code is not valid here", "host", q.host)
		writePage(q.w, http.StatusBadRequest, page{Text: pageBadLink}, 0)
		return
	}
	back := q.back(c.Path)
	if !e.guarded() {
		ferunHeaders(q.w.Header())
		q.w.Header().Set("Location", back)
		q.w.WriteHeader(http.StatusSeeOther)
		return
	}
	now := time.Now()
	until := time.Unix(c.Expires, 0).Add(visit.Leeway)
	if now.After(until) {
		writePage(q.w, http.StatusBadRequest, page{Text: pageTooLong, Retry: back}, 0)
		return
	}
	if c.Outcome == visit.OutcomeRejected {
		q.reason = wire.ReasonVisitRejected
		p.m.visitRejected.Add(1)
		writePage(q.w, http.StatusForbidden, page{Text: pageRejected}, 0)
		return
	}
	secret := ""
	cookies := q.r.CookiesNamed(visit.CookieChallenge)
	for _, ck := range cookies {
		if visit.ValidBindingSecret(ck.Value) && visit.BindingHash(ck.Value) == c.Binding {
			secret = ck.Value
			break
		}
	}
	if secret == "" {
		if len(cookies) == 0 {
			writePage(q.w, http.StatusBadRequest, page{Text: pageNoCookies}, 0)
		} else {
			writePage(q.w, http.StatusBadRequest, page{Text: pageOtherBrowser, Retry: back}, 0)
		}
		return
	}
	if e.access != wire.AccessVisitors || p.d.Visits == nil {
		q.reason = wire.ReasonVisitUnavailable
		p.m.visitUnavailable.Add(1)
		writePage(q.w, http.StatusServiceUnavailable, page{Text: pageNotNow, Retry: back}, 5)
		return
	}
	m, err := p.redeems.do(q.r.Context(), c.Challenge, until, func() (redeemMemo, error) {
		return q.redeem(t.keys, e, raw, secret, c)
	})
	switch {
	case err != nil:
		if q.r.Context().Err() != nil {
			q.w.aborted = true
			return
		}
		writePage(q.w, http.StatusServiceUnavailable, page{Text: pageNotNow, Retry: back}, 5)
	case !m.admitted:
		writePage(q.w, http.StatusBadRequest, page{Text: pageUsed, Retry: back}, 0)
	default:
		h := q.w.Header()
		h.Add("Set-Cookie", m.setCookie)
		ferunHeaders(h)
		h.Set("Location", back)
		q.w.WriteHeader(http.StatusSeeOther)
	}
}

// verifyCode checks a code by the table's keys alone.
func verifyCode(t *table, raw string) (visit.Code, bool) {
	if raw == "" || len(raw) > visit.MaxJWT || t.keys == nil {
		return visit.Code{}, false
	}
	c, err := t.keys.VerifyCode(raw)
	return c, err == nil
}

// redeem redeems an accepted code at the control plane with the
// challenge cookie's secret and a fresh visit secret, and checks the
// visit's first token; an error is no outcome.
func (q *request) redeem(keys *visit.Keys, e *envRoutes, code, binding string, c visit.Code) (redeemMemo, error) {
	p := q.p
	secret, err := visit.NewBindingSecret()
	if err != nil {
		return redeemMemo{}, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(q.r.Context()), redeemTimeout)
	defer cancel()
	res, err := p.d.Visits.RedeemVisit(ctx, wire.RedeemVisitRequest{Code: code, Binding: binding,
		VisitBinding: visit.BindingHash(secret), Host: q.host})
	if err != nil {
		p.m.visitFailed.Add(1)
		p.rl.warn("visit-redeem", "redeem a sign-in code failed", "host", q.host, "env", e.id, "err", err)
		return redeemMemo{}, err
	}
	switch res.Outcome {
	case wire.OutcomeRefused:
		p.log.Debug("the control plane refused a sign-in code", "host", q.host, "env", e.id, "reason", res.Reason)
		return redeemMemo{}, nil
	case wire.OutcomeAdmitted:
	default:
		p.m.visitFailed.Add(1)
		p.rl.warn("visit-redeem", "the control plane answered a redeem with an unknown outcome", "host", q.host,
			"outcome", res.Outcome)
		return redeemMemo{}, errBadRenewal
	}
	t, err := keys.VerifyToken(res.Token)
	if err != nil || t.Env != e.id || t.Host != q.host || t.Provider != e.provider ||
		t.Binding != visit.BindingHash(secret) || t.Visit != c.Visit || !validHeaderValue(t.Subject) ||
		!validHeaderValue(t.Email) {
		p.m.visitFailed.Add(1)
		p.rl.warn("visit-redeem", "the control plane's first token of a visit is not the visit's", "host", q.host,
			"env", e.id, "visit", c.Visit)
		return redeemMemo{}, errBadRenewal
	}
	p.m.visitRedeemed.Add(1)
	h := http.Header{}
	setVisitCookie(h, res.Token, secret, t.VisitEnds, time.Now())
	return redeemMemo{admitted: true, setCookie: h.Get("Set-Cookie")}, nil
}

// signOut is POST /.ferun/sign-out: from this site only, it ends the
// visit of the request's visit cookie at the control plane, when the
// cookie holds one, closes its live connections on this instance,
// remembers it ended, and always clears the cookie.
func (q *request) signOut(t *table) {
	p := q.p
	r := q.r
	if o := r.Header.Values("Origin"); len(o) > 0 && (len(o) != 1 || o[0] != "https://"+q.host) ||
		len(r.Header.Values("Sec-Fetch-Site")) > 0 && r.Header.Get("Sec-Fetch-Site") != "same-origin" {
		ferunHeaders(q.w.Header())
		reply{http.StatusForbidden, textCrossSiteSignOut, 0}.write(q.w)
		return
	}
	if t.keys != nil {
		for _, ck := range r.CookiesNamed(visit.CookieVisit) {
			token, secret, ok := visit.ParseVisitCookie(ck.Value)
			if !ok {
				continue
			}
			c, err := t.keys.VerifyToken(token)
			if err != nil || c.Binding != visit.BindingHash(secret) || c.Host != q.host {
				continue
			}
			if p.d.Visits != nil {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), endTimeout)
				if _, err := p.d.Visits.EndVisit(ctx, wire.EndVisitRequest{Token: token, Secret: secret}); err != nil {
					p.rl.warn("visit-end", "end a visit at the control plane failed; it ends here", "host", q.host,
						"visit", c.Visit, "err", err)
				}
				cancel()
			}
			p.visits.end(c.Visit, c.Binding, "signed_out", c.VisitEnds)
			p.m.visitEnded.Add(1)
		}
	}
	clearVisitCookie(q.w.Header())
	writePage(q.w, http.StatusOK, page{Text: pageSignedOutText + q.host + "."}, 0)
}

// redeemBook is the codes this instance redeemed: one call per code at a
// time, and the outcome kept until the code's end and the leeway, so
// that the same code brought twice (a reload, two tabs) gets the same
// answer without a call.
type redeemBook struct {
	mu      sync.Mutex
	flights map[string]*redeemFlight
	memo    map[string]redeemMemo
}

// redeemMemo is the outcome of a code: admitted with the Set-Cookie of
// the visit cookie, or refused; until is when it is forgotten.
type redeemMemo struct {
	until     time.Time
	admitted  bool
	setCookie string
}

type redeemFlight struct {
	done chan struct{}
	memo redeemMemo
	err  error
}

func newRedeemBook() *redeemBook {
	return &redeemBook{flights: map[string]*redeemFlight{}, memo: map[string]redeemMemo{}}
}

// do returns the outcome of the code cid: remembered, or of the call in
// flight, or of call, which it makes. An outcome is kept until until,
// while fewer than maxRedeemMemo are (the expired ones are dropped
// first); an error is not kept. ctx ends a wait for another's call.
func (b *redeemBook) do(ctx context.Context, cid string, until time.Time, call func() (redeemMemo, error)) (redeemMemo, error) {
	b.mu.Lock()
	if m, ok := b.memo[cid]; ok && time.Now().Before(m.until) {
		b.mu.Unlock()
		return m, nil
	}
	if f := b.flights[cid]; f != nil {
		b.mu.Unlock()
		select {
		case <-f.done:
			return f.memo, f.err
		case <-ctx.Done():
			return redeemMemo{}, ctx.Err()
		}
	}
	f := &redeemFlight{done: make(chan struct{})}
	b.flights[cid] = f
	b.mu.Unlock()
	f.memo, f.err = call()
	f.memo.until = until
	b.mu.Lock()
	delete(b.flights, cid)
	if f.err == nil {
		now := time.Now()
		if len(b.memo) >= maxRedeemMemo {
			for id, m := range b.memo {
				if !now.Before(m.until) {
					delete(b.memo, id)
				}
			}
		}
		if len(b.memo) < maxRedeemMemo {
			b.memo[cid] = f.memo
		}
	}
	b.mu.Unlock()
	close(f.done)
	return f.memo, f.err
}

// prune forgets the outcomes whose time passed.
func (b *redeemBook) prune(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, m := range b.memo {
		if !now.Before(m.until) {
			delete(b.memo, id)
		}
	}
}
