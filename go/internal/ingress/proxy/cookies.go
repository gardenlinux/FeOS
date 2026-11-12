// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/visit"
)

// maxCookieLogHosts bounds the hosts whose last cookie log line the
// proxy remembers.
const maxCookieLogHosts = 4096

// hostOnlyCookies makes every cookie that a response sets on host (a
// canonical host under the apps zone or the tenant zone) host-only: each Domain attribute
// of a Set-Cookie header whose value, without a leading dot and in lower
// case, is not host itself is removed. So an app can set no cookie for
// other apps under the zone, nor for the platform's hosts beside it. A
// cookie without Domain, or with host as its Domain, stays as it is; the
// other attributes, their spelling and the headers' order are kept. It
// returns how many attributes it removed.
//
// A proxy cannot stop cookies an app's own JavaScript sets
// (document.cookie): the platform's hosts guard against those with
// __Host- cookies, and the zone belongs on the Public Suffix List.
func hostOnlyCookies(h http.Header, host string) int {
	removed := 0
	for i, v := range h["Set-Cookie"] {
		nv, n := cookieWithoutDomain(v, host)
		if n > 0 {
			h["Set-Cookie"][i] = nv
			removed += n
		}
	}
	return removed
}

// cookieWithoutDomain removes from one Set-Cookie value the Domain
// attributes that name anything but host.
func cookieWithoutDomain(v, host string) (string, int) {
	parts := strings.Split(v, ";")
	kept := make([]string, 1, len(parts))
	kept[0] = parts[0] // name=value; never an attribute
	removed := 0
	for _, part := range parts[1:] {
		name, value, _ := strings.Cut(part, "=")
		if strings.EqualFold(strings.TrimSpace(name), "domain") {
			d := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(value), "."))
			if d != host {
				removed++
				continue
			}
		}
		kept = append(kept, part)
	}
	if removed == 0 {
		return v, 0
	}
	return strings.Join(kept, ";"), removed
}

// cookiesRewritten counts removed Domain attributes and logs them at
// debug, once per host and minute.
func (p *Proxy) cookiesRewritten(host string, n int) {
	p.m.cookieDomains.Add(uint64(n))
	if !p.log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	now := time.Now()
	p.mu.Lock()
	if at, ok := p.cookieAt[host]; ok && now.Sub(at) < time.Minute {
		p.mu.Unlock()
		return
	}
	if len(p.cookieAt) >= maxCookieLogHosts {
		clear(p.cookieAt)
	}
	p.cookieAt[host] = now
	p.mu.Unlock()
	p.log.Debug("removed the Domain of cookies an app set; they are host-only now", "host", host, "removed", n)
}

// ourCookie reports whether a cookie name is one of the ingress's own
// (ADR 0054): the visit cookie or the challenge cookie, in any case.
func ourCookie(name string) bool {
	name = strings.TrimSpace(name)
	return strings.EqualFold(name, visit.CookieVisit) || strings.EqualFold(name, visit.CookieChallenge)
}

// stripOurCookies removes the ingress's cookies from a request's Cookie
// headers before the application sees them, on every host: each header
// is split at ";", the pairs whose name is ours are dropped, the rest
// keeps its spelling, and a header left empty is dropped. It returns how
// many pairs it removed.
func stripOurCookies(h http.Header) int {
	vals, ok := h["Cookie"]
	if !ok {
		return 0
	}
	removed := 0
	var out []string
	for _, v := range vals {
		parts := strings.Split(v, ";")
		kept := parts[:0]
		for _, part := range parts {
			name, _, _ := strings.Cut(part, "=")
			if ourCookie(name) {
				removed++
				continue
			}
			kept = append(kept, part)
		}
		if nv := strings.TrimLeft(strings.Join(kept, ";"), " \t"); strings.TrimSpace(nv) != "" {
			out = append(out, nv)
		}
	}
	if removed == 0 {
		return 0
	}
	if len(out) == 0 {
		delete(h, "Cookie")
	} else {
		h["Cookie"] = out
	}
	return removed
}

// dropOurSetCookies drops every Set-Cookie of a response (or of a 1xx
// before it) that sets one of the ingress's cookies: the cookies are the
// ingress's on every host, and no application sets, replaces or clears
// them. It returns how many it dropped.
func dropOurSetCookies(h http.Header) int {
	vals, ok := h["Set-Cookie"]
	if !ok {
		return 0
	}
	kept := vals[:0:0]
	for _, v := range vals {
		pair, _, _ := strings.Cut(v, ";")
		name, _, _ := strings.Cut(pair, "=")
		if ourCookie(name) {
			continue
		}
		kept = append(kept, v)
	}
	n := len(vals) - len(kept)
	if n == 0 {
		return 0
	}
	if len(kept) == 0 {
		delete(h, "Set-Cookie")
	} else {
		h["Set-Cookie"] = kept
	}
	return n
}

// setCookie adds a Set-Cookie of one of the ingress's cookies to h:
// host-only, Secure, HttpOnly, SameSite=Lax and Path=/ (what the __Host-
// prefix demands), alive maxAge seconds (0 clears it). Whatever sets or
// clears one is not cached.
func setCookie(h http.Header, name, value string, maxAge int) {
	h.Add("Set-Cookie", name+"="+value+"; Path=/; Max-Age="+strconv.Itoa(max(maxAge, 0))+
		"; Secure; HttpOnly; SameSite=Lax")
	h.Set("Cache-Control", "no-store")
}

// setVisitCookie sets the visit cookie, token "~" secret, until the
// visit's end vend (Unix seconds), at least a second.
func setVisitCookie(h http.Header, token, secret string, vend int64, now time.Time) {
	setCookie(h, visit.CookieVisit, visit.VisitCookie(token, secret), int(max(vend-now.Unix(), 1)))
}

// clearVisitCookie clears the visit cookie.
func clearVisitCookie(h http.Header) {
	setCookie(h, visit.CookieVisit, "", 0)
}
