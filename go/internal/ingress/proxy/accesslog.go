// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// AccessLog takes the record of each request the proxy answered for an
// environment (the agent ships them to the sink). Record must
// not block: it runs on the request's goroutine after its response.
type AccessLog interface {
	Record(wire.AccessLogEntry)
}

// The headers a client may send to claim another address, which the
// proxy drops: ReverseProxy drops Forwarded and X-Forwarded-For, -Host
// and -Proto before Rewrite, and SetXForwarded sets the last three anew.
// X-Real-IP is set anew too, to the client's address.
const (
	headerRealIP       = "X-Real-Ip"
	headerTrueClientIP = "True-Client-Ip"
)

// setForwarded sets the forwarding headers of an outbound request from
// the connection the request came on: X-Forwarded-For, -Host and -Proto
// (SetXForwarded) and X-Real-IP; it drops True-Client-IP.
func setForwarded(pr *httputil.ProxyRequest) {
	pr.SetXForwarded()
	pr.Out.Header.Del(headerTrueClientIP)
	if ip := clientIP(pr.In); ip != "" {
		pr.Out.Header.Set(headerRealIP, ip)
	} else {
		pr.Out.Header.Del(headerRealIP)
	}
}

// clientIP is the address of the request's client, "" when RemoteAddr
// has none.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	return host
}

// clientAddr is the address of the request's client, the connection's
// peer: never what a header claims. The zero Addr when RemoteAddr has
// none, which a route with blocks does not admit.
func clientAddr(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr()
}

// record hands the request's record to the access log, when the request
// was routed to an environment.
func (q *request) record() {
	p := q.p
	if p.d.AccessLog == nil || q.env == "" {
		return
	}
	end := time.Now()
	var in int64
	if q.body != nil {
		in = q.body.n.Load()
	}
	id, span, parent := q.trace.ids()
	r := q.r
	status := q.w.status
	if status == 0 && q.w.upgraded {
		status = http.StatusSwitchingProtocols
	}
	p.d.AccessLog.Record(wire.AccessLogEntry{
		Time:         q.wait.start.UTC(),
		DurationNs:   int64(end.Sub(q.wait.start)),
		Env:          q.env,
		Tenant:       q.tenant,
		Service:      q.service,
		Instance:     q.instance,
		Host:         cutField(q.host, wire.MaxAccessLogText),
		Method:       cutField(r.Method, wire.MaxAccessLogText),
		Path:         cutField(q.raw, wire.MaxAccessLogPath),
		Proto:        cutField(r.Proto, wire.MaxAccessLogText),
		Status:       status,
		Outcome:      q.w.class(),
		Reason:       q.reason,
		BytesIn:      in,
		BytesOut:     q.w.written,
		ClientIP:     clientIP(r),
		UserAgent:    cutField(r.UserAgent(), wire.MaxAccessLogUserAgent),
		Attempts:     q.attempts,
		HeldNs:       int64(q.held),
		TraceID:      id,
		SpanID:       span,
		ParentSpanID: parent,
		TraceState:   q.trace.state,
		TraceFlags:   q.trace.flags,
	})
}

// cutField bounds s to n bytes, at a rune's start, and replaces what is not
// UTF-8.
func cutField(s string, n int) string {
	if len(s) > n {
		i := n
		for i > 0 && !utf8.RuneStart(s[i]) {
			i--
		}
		s = s[:i]
	}
	return strings.ToValidUTF8(s, "�")
}
