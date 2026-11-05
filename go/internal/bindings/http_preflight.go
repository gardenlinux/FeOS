// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/bindings/httpcheck"
)

// The HTTP preflight (ADR 0028 §8) checks one binding before anything is
// stored, from this instance: it resolves the upstream and judges every
// address by the destination rule, dials an address it judged, checks the
// server's certificate (the binding's CA or the system's roots), fetches a
// token where the credential is OAuth 2.0 client credentials, and, when
// asked, sends one GET or HEAD to a path with the credential. It answers
// the addresses, the certificate's subject, issuer and end, the token's
// lifetime, the status and the latency; never a body, never a token.

// HTTPProbe is the one request a preflight sends when asked.
type HTTPProbe struct {
	// Method is GET or HEAD.
	Method string `json:"method"`
	// Path begins with "/"; it may have a query.
	Path string `json:"path"`
}

// HTTPPreflightTLS is the upstream's certificate.
type HTTPPreflightTLS struct {
	Subject  string    `json:"subject"`
	Issuer   string    `json:"issuer"`
	NotAfter time.Time `json:"notAfter"`
}

// HTTPPreflightToken is the token an OAuth credential got.
type HTTPPreflightToken struct {
	// ExpiresIn is the token's lifetime in seconds as its endpoint said;
	// 0 when it said none.
	ExpiresIn int `json:"expiresIn"`
}

// HTTPPreflightProbe is the probe's answer.
type HTTPPreflightProbe struct {
	Status    int   `json:"status"`
	LatencyMs int64 `json:"latencyMs"`
}

// HTTPPreflightError is why a preflight is not ok, with its stable code.
type HTTPPreflightError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// HTTPPreflightResult is the answer of POST /api/v1/http/preflight and of
// an environment's binding's check. Fields a failure left unchecked are
// null (the addresses empty).
type HTTPPreflightResult struct {
	OK        bool                `json:"ok"`
	Addresses []string            `json:"addresses"`
	TLS       *HTTPPreflightTLS   `json:"tls"`
	Token     *HTTPPreflightToken `json:"token"`
	Probe     *HTTPPreflightProbe `json:"probe"`
	Error     *HTTPPreflightError `json:"error"`
}

func (r *HTTPPreflightResult) fail(berr *bindingErr) *HTTPPreflightResult {
	r.OK = false
	r.Error = &HTTPPreflightError{Code: berr.code, Message: berr.msg}
	return r
}

// maxProbePath bounds a probe's path.
const maxProbePath = 4096

// HTTPPreflight is POST /api/v1/http/preflight: {"binding": B, "probe":
// {"method", "path"}?}. It answers 200 with the result, ok or not; 400
// only for a body that is not the request's JSON.
func (s *Service) HTTPPreflight(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	var body struct {
		Binding *httpBindingIn `json:"binding"`
		Probe   *HTTPProbe     `json:"probe"`
	}
	if _, err := decodeJSON(r, &body); err != nil {
		apierr.Write(w, bindingErrf(codeInvalidBinding, "%v", err).as(http.StatusBadRequest))
		return
	}
	res := &HTTPPreflightResult{Addresses: []string{}}
	if body.Binding == nil {
		apierr.WriteJSON(w, http.StatusOK, res.fail(bindingErrf(codeInvalidBinding, "binding is required")))
		return
	}
	apierr.WriteJSON(w, http.StatusOK, s.httpPreflight(r.Context(), *body.Binding, body.Probe))
}

// PreflightHTTP runs the preflight of a binding as a pod would receive it
// (an environment's binding, its secrets resolved), for the environments'
// check.
func (s *Service) PreflightHTTP(ctx context.Context, p *fleetv1.HttpBinding, probe *HTTPProbe) *HTTPPreflightResult {
	if !s.enabled {
		res := &HTTPPreflightResult{Addresses: []string{}}
		return res.fail(bindingErrf(codeUnreachable, "HTTP bindings are not enabled on this platform"))
	}
	return s.httpPreflight(ctx, inOf(httpBindingOf(p)), probe)
}

// inOf is a binding as a request would send it.
func inOf(b httpBinding) httpBindingIn {
	c := b.Credential
	in := httpBindingIn{Name: b.Name, Upstream: b.Upstream, Mode: b.Mode, CaPem: b.CAPEM, PathPrefixes: b.PathPrefixes,
		ForwardedHost: b.ForwardedHost, Credential: &httpCredentialIn{Kind: c.Kind, Headers: maps.Clone(c.Headers),
			User: c.User, Password: c.Password, TokenURL: c.TokenURL, ClientID: c.ClientID, ClientSecret: c.ClientSecret,
			Scope: c.Scope, Audience: c.Audience, ClientAuthInBody: c.ClientAuthInBody, CertPem: c.CertPEM, KeyPem: c.KeyPEM}}
	if b.RatePerSecond > 0 {
		in.RateLimit = &httpRateLimitIn{PerSecond: b.RatePerSecond, Burst: b.RateBurst}
	}
	return in
}

func (s *Service) httpPreflight(ctx context.Context, in httpBindingIn, probe *HTTPProbe) *HTTPPreflightResult {
	res := &HTTPPreflightResult{Addresses: []string{}}
	b, _, berr := validateHTTPBinding(in, false)
	if berr != nil {
		return res.fail(berr)
	}
	if probe != nil {
		if berr := checkProbe(*probe, b.PathPrefixes); berr != nil {
			return res.fail(berr)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, s.budget)
	defer cancel()

	up, cerr := httpcheck.ParseUpstream("upstream", b.Upstream)
	if cerr != nil {
		return res.fail(asBindingErr(cerr))
	}
	addrs, berr := s.resolveChecked(ctx, up.Host)
	for _, a := range addrs {
		res.Addresses = append(res.Addresses, a.String())
	}
	if berr != nil {
		return res.fail(berr)
	}
	conf, berr := upstreamTLS(b, up.Host)
	if berr != nil {
		return res.fail(berr)
	}
	target := net.JoinHostPort(addrs[0].String(), strconv.Itoa(up.Port))
	dialer := s.dest.dialer()
	dial := func(ctx context.Context) (net.Conn, error) { return dialer.DialContext(ctx, "tcp4", target) }

	// The TLS check, on a connection of its own.
	raw, err := dial(ctx)
	if err != nil {
		return res.fail(classifyHTTPErr(b.Upstream, err))
	}
	tc := tls.Client(raw, conf.Clone())
	err = tc.HandshakeContext(ctx)
	if err != nil {
		_ = raw.Close()
		return res.fail(classifyHTTPErr(b.Upstream, err))
	}
	if certs := tc.ConnectionState().PeerCertificates; len(certs) > 0 {
		res.TLS = &HTTPPreflightTLS{Subject: certs[0].Subject.String(), Issuer: certs[0].Issuer.String(),
			NotAfter: certs[0].NotAfter.UTC()}
	}
	_ = tc.Close()

	var token string
	if b.Credential.Kind == httpcheck.KindOAuth2 {
		tok, expiresIn, berr := s.fetchToken(ctx, b.Credential, b.CAPEM)
		if berr != nil {
			return res.fail(berr)
		}
		token = tok
		res.Token = &HTTPPreflightToken{ExpiresIn: expiresIn}
	}
	if probe == nil {
		res.OK = true
		return res
	}

	client := &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
			TLSClientConfig:     conf,
			TLSHandshakeTimeout: 5 * time.Second,
			MaxIdleConns:        1,
			DisableCompression:  true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, probe.Method, b.Upstream+probe.Path, nil)
	if err != nil {
		return res.fail(bindingErrf(codeInvalidBinding, "probe.path %q: %v", probe.Path, err))
	}
	req.Header.Set("User-Agent", "ferund-http-preflight")
	setCredential(req, b.Credential, token)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return res.fail(classifyHTTPErr(b.Upstream, err))
	}
	latency := time.Since(start)
	// No body is ever answered; a little is read so that the connection
	// closes cleanly.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	res.Probe = &HTTPPreflightProbe{Status: resp.StatusCode, LatencyMs: latency.Milliseconds()}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return res.fail(bindingErrf(codeUnauthorized, "%s refused the credential: HTTP %d on %s %s", b.Upstream,
			resp.StatusCode, probe.Method, probe.Path))
	}
	res.OK = true
	return res
}

// checkProbe checks a probe: GET or HEAD, a path beginning with "/"
// within the binding's prefixes, by whole segments as feos-authn holds
// them.
func checkProbe(p HTTPProbe, prefixes []string) *bindingErr {
	if p.Method != http.MethodGet && p.Method != http.MethodHead {
		return bindingErrf(codeInvalidBinding, "probe.method must be GET or HEAD, not %q", p.Method)
	}
	if !strings.HasPrefix(p.Path, "/") || len(p.Path) > maxProbePath || strings.ContainsAny(p.Path, "#") ||
		hasCtl(p.Path) {
		return bindingErrf(codeInvalidBinding, "probe.path %q must begin with \"/\", without a fragment, whitespace or "+
			"control characters", p.Path)
	}
	if len(prefixes) == 0 {
		return nil
	}
	path, _, _ := strings.Cut(p.Path, "?")
	for _, pre := range prefixes {
		if httpcheck.WithinPrefix(path, pre) {
			return nil
		}
	}
	return bindingErrf(codeInvalidBinding, "probe.path %q is outside the binding's pathPrefixes", p.Path)
}

func hasCtl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// resolveChecked resolves host (an address stands as it is) and judges
// every address by the destination rule; it returns the addresses, and
// an error when there is none or one is refused.
func (s *Service) resolveChecked(ctx context.Context, host string) ([]netip.Addr, *bindingErr) {
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		lctx, cancel := context.WithTimeout(ctx, lookupTimeout)
		addrs, err = s.dest.lookup(lctx, host)
		cancel()
		if err != nil || len(addrs) == 0 {
			why := "no IPv4 address"
			if err != nil {
				why = err.Error()
			}
			return nil, bindingErrf(codeDestinationDenied, "upstream host %s does not resolve (%s)", host, why)
		}
	}
	for _, a := range addrs {
		if err := s.dest.allowDest(a); err != nil {
			return addrs, bindingErrf(codeDestinationDenied, "upstream host %s resolves to %v", host, err)
		}
	}
	return addrs, nil
}

// upstreamTLS is the TLS a binding speaks to its upstream: at least 1.2,
// the upstream's name, its CA or the system's roots, its client
// certificate, HTTP/1.1.
func upstreamTLS(b httpBinding, host string) (*tls.Config, *bindingErr) {
	conf := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host, NextProtos: []string{"http/1.1"}}
	if b.CAPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(b.CAPEM)) {
			return nil, bindingErrf(codeBadCA, "caPem holds no certificate")
		}
		conf.RootCAs = pool
	}
	if b.Credential.Kind == httpcheck.KindClientCertificate {
		cert, err := tls.X509KeyPair([]byte(b.Credential.CertPEM), []byte(b.Credential.KeyPEM))
		if err != nil {
			return nil, bindingErrf(codeBadClientCert, "client certificate: %v", err)
		}
		conf.Certificates = []tls.Certificate{cert}
	}
	return conf, nil
}

// setCredential adds the credential to a request as feos-authn does:
// its headers, Basic, or the token as a Bearer.
func setCredential(req *http.Request, c httpCredential, token string) {
	switch c.Kind {
	case httpcheck.KindHeaders:
		for k, v := range c.Headers {
			req.Header.Set(k, v)
		}
	case httpcheck.KindBasic:
		req.SetBasicAuth(c.User, c.Password)
	case httpcheck.KindOAuth2:
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// fetchToken asks the token endpoint for a token (RFC 6749 §4.4): the
// endpoint's host judged by the destination rule, its addresses again at
// the dial, its certificate checked against the system's roots and the
// binding's CA.
func (s *Service) fetchToken(ctx context.Context, c httpCredential, caPEM string) (string, int, *bindingErr) {
	u, err := url.Parse(c.TokenURL)
	if err != nil {
		return "", 0, bindingErrf(codeInvalidBinding, "credential.tokenUrl %q: %v", c.TokenURL, err)
	}
	host := strings.ToLower(u.Hostname())
	if _, berr := s.resolveChecked(ctx, host); berr != nil {
		berr.msg = "token endpoint: " + berr.msg
		return "", 0, berr
	}
	conf := &tls.Config{MinVersion: tls.VersionTLS12}
	if caPEM != "" {
		// The system's roots and the binding's CA, as feos-authn checks
		// the token endpoint.
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(caPEM)) {
			return "", 0, bindingErrf(codeBadCA, "caPem holds no certificate")
		}
		conf.RootCAs = pool
	}
	dialer := s.dest.dialer()
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp4", addr)
			},
			TLSClientConfig:     conf,
			TLSHandshakeTimeout: 5 * time.Second,
			MaxIdleConns:        1,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	form := url.Values{"grant_type": {"client_credentials"}}
	if c.Scope != "" {
		form.Set("scope", c.Scope)
	}
	if c.Audience != "" {
		form.Set("audience", c.Audience)
	}
	if c.ClientAuthInBody {
		form.Set("client_id", c.ClientID)
		form.Set("client_secret", c.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, bindingErrf(codeInvalidBinding, "credential.tokenUrl %q: %v", c.TokenURL, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ferund-http-preflight")
	if !c.ClientAuthInBody {
		// RFC 6749 §2.3.1: both form-encoded before Basic.
		req.SetBasicAuth(url.QueryEscape(c.ClientID), url.QueryEscape(c.ClientSecret))
	}
	resp, err := client.Do(req)
	if err != nil {
		berr := classifyHTTPErr(c.TokenURL, err)
		if berr.code == codeUnreachable || berr.code == codeTLSFailed {
			berr.code = codeTokenFailed
		}
		berr.msg = "token endpoint: " + berr.msg
		return "", 0, berr
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, bindingErrf(codeTokenFailed, "token endpoint %s: %v", c.TokenURL, err)
	}
	var tok struct {
		AccessToken string          `json:"access_token"`
		TokenType   string          `json:"token_type"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
		Error       string          `json:"error"`
	}
	_ = json.Unmarshal(raw, &tok)
	if resp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		why := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if tok.Error != "" {
			why += " " + sanitize(tok.Error)
		}
		code := codeTokenFailed
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusBadRequest &&
			(tok.Error == "invalid_client" || tok.Error == "unauthorized_client") {
			code = codeUnauthorized
		}
		return "", 0, bindingErrf(code, "token endpoint %s gave no token: %s", c.TokenURL, why)
	}
	if tok.TokenType != "" && !strings.EqualFold(tok.TokenType, "bearer") {
		return "", 0, bindingErrf(codeTokenFailed, "token endpoint %s gave a token of type %s, not Bearer",
			c.TokenURL, sanitize(tok.TokenType))
	}
	return tok.AccessToken, expiresIn(tok.ExpiresIn), nil
}

// expiresIn reads expires_in, a number or (as some servers write it) a
// string; 0 when absent or neither.
func expiresIn(raw json.RawMessage) int {
	var n float64
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		return int(n)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			return v
		}
	}
	return 0
}

// sanitize bounds a server's word for a message: printable ASCII, at most
// 64 bytes.
func sanitize(s string) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < 64; i++ {
		if c := s[i]; c >= 0x20 && c < 0x7f {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// classifyHTTPErr turns a transport error into the codes: the destination
// rule at dial time, a TLS failure, else unreachable.
func classifyHTTPErr(target string, err error) *bindingErr {
	var denied *errDialDenied
	if errors.As(err, &denied) {
		return bindingErrf(codeDestinationDenied, "%s: %v", target, denied.err)
	}
	if isTLSError(err) {
		return bindingErrf(codeTLSFailed, "%s: TLS: %v", target, unwrapURLError(err))
	}
	return bindingErrf(codeUnreachable, "%s: %v", target, unwrapURLError(err))
}
