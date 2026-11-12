// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package egress is the rule for connections to addresses others name
// (ADR 0028, ADR 0054): IPv4 only, never into a denied block unless an
// operator allowed it, judged on the address actually dialled. It imports
// nothing but the standard library.
package egress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// DefaultDeny is the built-in deny set: the private, shared, link-local,
// loopback, "this network", multicast and reserved IPv4 ranges (10/8,
// 172.16/12, 192.168/16, 100.64/10, 169.254/16, 127/8, 0/8, 224/4,
// 240/4). An address others name is external by definition; the
// platform's own networks are infrastructure nobody may name.
func DefaultDeny() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("240.0.0.0/4"),
	}
}

// Rule judges an address: an IPv6 one is refused ("%s is not an IPv4
// address (the platform reaches servers over IPv4 only)"); one in Allow is
// allowed; one in Deny is refused ("%s is an internal address (%s)"); any
// other is allowed. An IPv4-mapped IPv6 address is judged as its IPv4
// address.
type Rule struct {
	Deny, Allow []netip.Prefix
}

// Judge returns nil for an address the rule allows, else an error saying
// why not.
func (r Rule) Judge(a netip.Addr) error {
	a = a.Unmap()
	if !a.Is4() {
		return fmt.Errorf("%s is not an IPv4 address (the platform reaches servers over IPv4 only)", a)
	}
	for _, p := range r.Allow {
		if p.Contains(a) {
			return nil
		}
	}
	for _, p := range r.Deny {
		if p.Contains(a) {
			return fmt.Errorf("%s is an internal address (%s)", a, p)
		}
	}
	return nil
}

// DeniedError is a dial the rule refused.
type DeniedError struct{ Err error }

func (e *DeniedError) Error() string { return "destination denied: " + e.Err.Error() }
func (e *DeniedError) Unwrap() error { return e.Err }

// IsDenied reports whether err is, or wraps, a *DeniedError.
func IsDenied(err error) bool {
	var d *DeniedError
	return errors.As(err, &d)
}

// Dialer is a net.Dialer whose Control judges every address it connects to.
func Dialer(judge func(netip.Addr) error, timeout time.Duration) *net.Dialer {
	return &net.Dialer{
		Timeout: timeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return &DeniedError{fmt.Errorf("unparseable address %q", address)}
			}
			if err := judge(ap.Addr()); err != nil {
				return &DeniedError{err}
			}
			return nil
		},
	}
}

// lookupTimeout bounds CheckHost's resolution.
const lookupTimeout = 5 * time.Second

// lookupIPv4 resolves a host's A records (tests replace it).
var lookupIPv4 = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
}

// CheckHost resolves host (IPv4, a 5 s bound) and refuses it when any
// address is refused; its error names no resolved address (a literal is
// named as given). One refused address is enough: a name that answers with
// both can be made to answer with the refused one only. A refusal is a
// *DeniedError; a name that does not resolve is a plain error.
func CheckHost(ctx context.Context, judge func(netip.Addr) error, host string) error {
	if a, err := netip.ParseAddr(host); err == nil {
		if err := judge(a); err != nil {
			return &DeniedError{err}
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	addrs, err := lookupIPv4(ctx, host)
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("%s does not resolve to an IPv4 address", host)
	}
	for _, a := range addrs {
		if judge(a) != nil {
			return &DeniedError{fmt.Errorf("%s resolves to an address the rule refuses", host)}
		}
	}
	return nil
}

// maxRedirects is how many redirects a client that follows them follows.
const maxRedirects = 10

// httpsOnly refuses a request whose URL is not https before its
// transport sees it, the first request and every redirect alike.
type httpsOnly struct{ next http.RoundTripper }

func (t httpsOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || req.URL.Scheme != "https" {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		scheme := ""
		if req.URL != nil {
			scheme = req.URL.Scheme
		}
		return nil, &DeniedError{fmt.Errorf("a URL of scheme %q, not https", scheme)}
	}
	return t.next.RoundTrip(req)
}

// Client is an HTTP client for addresses others name: no proxy, tcp4
// through Dialer(judge, 5 s), TLS 1.2 or later with roots (nil: the
// system's), HTTP/1.1, timeout the whole request's bound. It refuses every
// request whose URL scheme is not https, the first one included, with a
// *DeniedError. With redirects false it follows none
// (http.ErrUseLastResponse); with true it follows up to 10, and only when
// the first request (via[0]) was a GET, so a POST follows none. Each
// client holds its own pool of idle connections: make one per rule and
// reuse it, never one per request.
func Client(judge func(netip.Addr) error, timeout time.Duration, redirects bool, roots *x509.CertPool) *http.Client {
	d := Dialer(judge, 5*time.Second)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	t := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp4", addr)
		},
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		TLSHandshakeTimeout: 10 * time.Second,
		Protocols:           protocols,
		MaxIdleConns:        16,
		IdleConnTimeout:     90 * time.Second,
	}
	return &http.Client{
		Transport: httpsOnly{t},
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !redirects || via[0].Method != http.MethodGet {
				return http.ErrUseLastResponse
			}
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			if req.URL.Scheme != "https" {
				return &DeniedError{fmt.Errorf("a redirect to a URL of scheme %q, not https", req.URL.Scheme)}
			}
			return nil
		},
	}
}

// PrefixList is a flag.Value of comma-separated or repeated prefixes, where
// "default" expands to DefaultDeny(); a value given replaces the default
// unless it names "default".
type PrefixList []netip.Prefix

func (l *PrefixList) String() string {
	out := make([]string, len(*l))
	for i, p := range *l {
		out[i] = p.String()
	}
	return strings.Join(out, ",")
}

func (l *PrefixList) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if s == "default" {
			*l = append(*l, DefaultDeny()...)
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return err
		}
		*l = append(*l, p.Masked())
	}
	return nil
}
