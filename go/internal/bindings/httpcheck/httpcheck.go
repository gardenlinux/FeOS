// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package httpcheck holds the rules of an HTTP binding (ADR 0028) that
// need neither the network nor the database: its name, upstream, mode,
// path prefixes, rate limit, headers, CA and client certificate. The
// sandbox surface (internal/bindings) and the environments' spec
// (internal/env/spec) check a binding by the same rules; each failure
// carries a stable code, which the sandbox surface answers as "code".
package httpcheck

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The modes of a binding (ADR 0028 §3, §5).
const (
	// ModeName: the pod calls http://<name>/…; the default.
	ModeName = "name"
	// ModeIntercept: the pod calls https://<upstream host>/… under the
	// API's own name, through the pod's interception CA.
	ModeIntercept = "intercept"
)

// The kinds of a binding's credential (ADR 0028 §4).
const (
	KindHeaders           = "headers"
	KindBasic             = "basic"
	KindOAuth2            = "oauth2"
	KindClientCertificate = "clientCertificate"
)

// The stable codes of a failure.
const (
	CodeInvalidBinding   = "invalid_binding"
	CodeInsecureUpstream = "insecure_upstream"
	CodeBadCA            = "bad_ca"
	CodeBadClientCert    = "bad_client_cert"
	CodeBadHeader        = "bad_header"
)

// Bounds of a binding: sanity caps, not policy.
const (
	MaxBindings      = 32
	MaxPathPrefixes  = 32
	MaxPathPrefix    = 1024
	MaxHeaders       = 16
	MaxHeaderValue   = 8 << 10
	MaxRatePerSecond = 100000
	MaxRateBurst     = 100000
	// InterceptPort is the only upstream port the intercept mode takes
	// (ADR 0028 §5).
	InterceptPort = 443
)

// Error is a failure with its stable code.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// nameRe is a binding's name: a DNS label, as a service's.
var nameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// hostRe is a DNS name, loosely: labels of letters, digits and '-'
// joined by dots.
var hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// Name checks a binding's name: a DNS label of 1 to 63 lowercase
// letters, digits and '-', neither beginning nor ending with '-'.
func Name(s string) *Error {
	if !nameRe.MatchString(s) {
		return errorf(CodeInvalidBinding,
			"name %q must be a DNS label: 1 to 63 lowercase letters, digits and '-', not beginning or ending with '-'", s)
	}
	return nil
}

// Upstream is a parsed upstream: its host in lowercase (an IPv4 address
// or a DNS name) and its port (443 when the URL names none).
type Upstream struct {
	Host string
	Port int
	// IP: the host is an address.
	IP bool
}

// Origin is the upstream's canonical form, "https://host" or
// "https://host:port" for a port other than 443.
func (u Upstream) Origin() string {
	if u.Port == 443 {
		return "https://" + u.Host
	}
	return "https://" + u.Host + ":" + strconv.Itoa(u.Port)
}

// ParseUpstream checks an upstream: an https origin, "https://host[:port]",
// with nothing else: no path but "/", no query, fragment or userinfo.
// Plain http is refused with CodeInsecureUpstream: the credential would
// cross the network in the clear.
func ParseUpstream(field, s string) (Upstream, *Error) {
	if s == "" {
		return Upstream{}, errorf(CodeInvalidBinding, "%s is required", field)
	}
	u, err := url.Parse(s)
	if err != nil {
		return Upstream{}, errorf(CodeInvalidBinding, "%s %q is not a URL: %v", field, s, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		return Upstream{}, errorf(CodeInsecureUpstream,
			"%s %q must be an https:// URL: the credential would cross the network in the clear", field, s)
	default:
		return Upstream{}, errorf(CodeInvalidBinding, "%s %q must be an https:// URL", field, s)
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case u.Opaque != "" || host == "":
		return Upstream{}, errorf(CodeInvalidBinding, "%s %q names no host", field, s)
	case u.User != nil:
		return Upstream{}, errorf(CodeInvalidBinding, "%s must not carry credentials in the URL", field)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return Upstream{}, errorf(CodeInvalidBinding, "%s %q must not have a query or fragment", field, s)
	case u.Path != "" && u.Path != "/":
		return Upstream{}, errorf(CodeInvalidBinding,
			"%s %q must be an origin without a path; restrict paths with pathPrefixes", field, s)
	}
	out := Upstream{Host: host, Port: 443}
	if a, err := netip.ParseAddr(host); err == nil {
		if !a.Is4() {
			return Upstream{}, errorf(CodeInvalidBinding,
				"%s host %s is not an IPv4 address (the platform reaches servers over IPv4 only)", field, host)
		}
		out.IP = true
	} else if len(host) > 253 || !hostRe.MatchString(host) {
		return Upstream{}, errorf(CodeInvalidBinding, "%s host %q is not a host name or an IPv4 address", field, host)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return Upstream{}, errorf(CodeInvalidBinding, "%s port %q is not a port", field, p)
		}
		out.Port = n
	}
	return out, nil
}

// TokenURL checks an OAuth 2.0 token endpoint: an https URL with a host,
// a path and a query allowed (RFC 6749 §3.2), no userinfo or fragment. It
// returns the host, for the destination rule.
func TokenURL(field, s string) (string, *Error) {
	if s == "" {
		return "", errorf(CodeInvalidBinding, "%s is required", field)
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", errorf(CodeInvalidBinding, "%s %q is not a URL: %v", field, s, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		return "", errorf(CodeInsecureUpstream,
			"%s %q must be an https:// URL: the client secret would cross the network in the clear", field, s)
	default:
		return "", errorf(CodeInvalidBinding, "%s %q must be an https:// URL", field, s)
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case u.Opaque != "" || host == "":
		return "", errorf(CodeInvalidBinding, "%s %q names no host", field, s)
	case u.User != nil:
		return "", errorf(CodeInvalidBinding, "%s must not carry credentials in the URL", field)
	case u.Fragment != "":
		return "", errorf(CodeInvalidBinding, "%s %q must not have a fragment", field, s)
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if !a.Is4() {
			return "", errorf(CodeInvalidBinding, "%s host %s is not an IPv4 address", field, host)
		}
	} else if len(host) > 253 || !hostRe.MatchString(host) {
		return "", errorf(CodeInvalidBinding, "%s host %q is not a host name or an IPv4 address", field, host)
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", errorf(CodeInvalidBinding, "%s port %q is not a port", field, p)
		}
	}
	return host, nil
}

// Mode checks a mode ("" is ModeName) against the upstream and returns
// it: the intercept mode needs a host name (the pod resolves it) on port
// 443.
func Mode(field, mode string, up Upstream) (string, *Error) {
	switch mode {
	case "", ModeName:
		return ModeName, nil
	case ModeIntercept:
		if up.IP {
			return "", errorf(CodeInvalidBinding,
				"%s intercept needs an upstream named by a host name: the pod resolves that name to reach it", field)
		}
		if up.Port != InterceptPort {
			return "", errorf(CodeInvalidBinding,
				"%s intercept takes upstreams on port 443 only, not %d; use the name mode", field, up.Port)
		}
		return ModeIntercept, nil
	}
	return "", errorf(CodeInvalidBinding, "%s %q is unknown: name or intercept", field, mode)
}

// PathPrefixes checks the path prefixes a binding may reach: each begins
// with "/", has no query, fragment, whitespace or control character and no
// "." or ".." segment, at most MaxPathPrefix bytes; at most
// MaxPathPrefixes, no two the same.
func PathPrefixes(field string, ps []string) *Error {
	if len(ps) > MaxPathPrefixes {
		return errorf(CodeInvalidBinding, "%s: at most %d path prefixes", field, MaxPathPrefixes)
	}
	seen := map[string]bool{}
	for i, p := range ps {
		f := fmt.Sprintf("%s[%d]", field, i)
		switch {
		case !strings.HasPrefix(p, "/"):
			return errorf(CodeInvalidBinding, "%s %q must begin with \"/\"", f, p)
		case len(p) > MaxPathPrefix:
			return errorf(CodeInvalidBinding, "%s is longer than %d bytes", f, MaxPathPrefix)
		case strings.ContainsAny(p, "?#"):
			return errorf(CodeInvalidBinding, "%s %q must not have a query or fragment", f, p)
		case !utf8.ValidString(p) || hasControlOrSpace(p):
			return errorf(CodeInvalidBinding, "%s %q must not have whitespace or control characters", f, p)
		case seen[p]:
			return errorf(CodeInvalidBinding, "%s %q is listed twice", f, p)
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == "." || seg == ".." {
				return errorf(CodeInvalidBinding, "%s %q must not have a \".\" or \"..\" segment", f, p)
			}
		}
		seen[p] = true
	}
	return nil
}

// RateLimit checks a rate limit and returns it normalised: perSecond 0
// is none (and then burst must be 0 too); a burst of 0 with a rate is the
// rate (at least 1).
func RateLimit(field string, perSecond, burst int) (int, int, *Error) {
	switch {
	case perSecond < 0 || perSecond > MaxRatePerSecond:
		return 0, 0, errorf(CodeInvalidBinding, "%s.perSecond %d is out of range (0 to %d)", field, perSecond, MaxRatePerSecond)
	case burst < 0 || burst > MaxRateBurst:
		return 0, 0, errorf(CodeInvalidBinding, "%s.burst %d is out of range (0 to %d)", field, burst, MaxRateBurst)
	case perSecond == 0 && burst != 0:
		return 0, 0, errorf(CodeInvalidBinding, "%s.burst needs a perSecond", field)
	}
	if perSecond > 0 && burst == 0 {
		burst = perSecond
	}
	return perSecond, burst, nil
}

// tokenRe is an HTTP field name (RFC 9110 §5.1: a token).
var tokenRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// refusedHeaders are the header names a credential may not set: the
// request's target and framing, and the hop-by-hop headers (RFC 9110
// §7.6.1), which never reach the upstream.
var refusedHeaders = map[string]bool{
	"host": true, "connection": true, "keep-alive": true, "te": true, "trailer": true,
	"transfer-encoding": true, "upgrade": true, "content-length": true, "proxy-connection": true,
	"forwarded": true, "x-forwarded-for": true, "x-forwarded-host": true, "x-forwarded-proto": true,
	"feos-binding-error": true,
}

// HeaderName checks the name of a header a credential sets: a token, not
// Host, a hop-by-hop or framing header, a Proxy-* header (feos-authn
// drops them), the forwarding headers or FeOS-Binding-Error.
func HeaderName(field, name string) *Error {
	lower := strings.ToLower(name)
	switch {
	case !tokenRe.MatchString(name):
		return errorf(CodeBadHeader, "%s: %q is not a header name", field, name)
	case refusedHeaders[lower] || strings.HasPrefix(lower, "proxy-"):
		return errorf(CodeBadHeader, "%s: a credential cannot set %s", field, name)
	}
	return nil
}

// HeaderValue checks a header's value: not empty, at most MaxHeaderValue
// bytes, no control character but a tab, no leading or trailing
// whitespace.
func HeaderValue(field, v string) *Error {
	switch {
	case v == "":
		return errorf(CodeBadHeader, "%s is empty", field)
	case len(v) > MaxHeaderValue:
		return errorf(CodeBadHeader, "%s is longer than %d bytes", field, MaxHeaderValue)
	case !utf8.ValidString(v):
		return errorf(CodeBadHeader, "%s is not valid UTF-8", field)
	case strings.TrimSpace(v) != v:
		return errorf(CodeBadHeader, "%s has leading or trailing whitespace", field)
	}
	for _, r := range v {
		if (r < 0x20 && r != '\t') || r == 0x7f {
			return errorf(CodeBadHeader, "%s has a control character", field)
		}
	}
	return nil
}

// Text checks a credential's text field (a user, a client ID, a scope):
// valid UTF-8 without control characters. empty says whether it may be
// empty.
func Text(field, v string, empty bool) *Error {
	switch {
	case v == "" && !empty:
		return errorf(CodeInvalidBinding, "%s is required", field)
	case !utf8.ValidString(v):
		return errorf(CodeInvalidBinding, "%s is not valid UTF-8", field)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return errorf(CodeInvalidBinding, "%s has a control character", field)
		}
	}
	return nil
}

// BasicUser checks a Basic user: Text, and no ':' (RFC 7617 §2).
func BasicUser(field, v string) *Error {
	if err := Text(field, v, false); err != nil {
		return err
	}
	if strings.Contains(v, ":") {
		return errorf(CodeInvalidBinding, "%s must not contain ':'", field)
	}
	return nil
}

// CAPEM checks an upstream's CA bundle: at least one certificate and
// nothing that is not one, a bundle with a stray key in it being a
// pasting accident.
func CAPEM(field, s string) *Error {
	rest, n := []byte(s), 0
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			return errorf(CodeBadCA, "%s holds a %s block; only certificates belong here", field, blk.Type)
		}
		if _, err := x509.ParseCertificate(blk.Bytes); err != nil {
			return errorf(CodeBadCA, "%s: certificate %d: %v", field, n+1, err)
		}
		n++
	}
	if n == 0 {
		return errorf(CodeBadCA, "%s holds no PEM certificate", field)
	}
	return nil
}

// ClientCertificate checks a client certificate's chain and key: both
// PEM, and the key the certificate's.
func ClientCertificate(field, certPEM, keyPEM string) *Error {
	if certPEM == "" || keyPEM == "" {
		return errorf(CodeInvalidBinding, "%s needs certPem and keyPem", field)
	}
	if _, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err != nil {
		return errorf(CodeBadClientCert, "%s: %v", field, err)
	}
	return nil
}

func hasControlOrSpace(s string) bool {
	for _, r := range s {
		if r <= 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// AsError returns err's Error, or nil.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}
