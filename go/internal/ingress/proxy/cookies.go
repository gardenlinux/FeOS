// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
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
