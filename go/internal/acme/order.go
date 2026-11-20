// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package acme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	xacme "golang.org/x/crypto/acme"

	"github.com/gardenlinux/feos/go/internal/domains"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The failures of an order that are ferund's, not the CA's.
var (
	// errNoIngress: no ingress instance is connected to answer the
	// challenge, so it is not accepted.
	errNoIngress = errors.New("no ingress instance is connected to answer the HTTP-01 challenge")
	// errNoHTTP01: the CA offers no http-01 challenge for the name.
	errNoHTTP01 = errors.New("the CA offers no HTTP-01 challenge for the name")
)

// notDelivered is the failure of an order whose challenge the connected
// ingress instances did not all get in time.
type notDelivered struct {
	missing []string
	wait    time.Duration
}

func (e *notDelivered) Error() string {
	return fmt.Sprintf("the ingress instances %s did not get the HTTP-01 challenge within %v", strings.Join(e.missing, ", "),
		e.wait)
}

// tokenRE is what acme_challenges accepts as a token (base64url).
var tokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// issued is what an order obtained: the chain in DER, leaf first, the key
// in PKCS #8 DER, and the certificate's times and serial.
type issued struct {
	chain                           [][]byte
	key                             []byte
	notBefore, notAfter, renewAfter time.Time
	serial                          string
}

// order obtains a certificate for o's name with HTTP-01, a wildcard's
// with DNS-01 in the ACME zone, outside any transaction but the short
// ones that keep an HTTP-01 challenge.
func (s *Issuer) order(ctx context.Context, cl *xacme.Client, o store.ACMEOrder) (*issued, error) {
	var opts []xacme.OrderOption
	if s.cfg.Profile != "" {
		opts = append(opts, xacme.WithOrderIssuanceProfile(xacme.ProfileName(s.cfg.Profile)))
	}
	ord, err := cl.AuthorizeOrder(ctx, xacme.DomainIDs(o.Name), opts...)
	if err != nil {
		return nil, fmt.Errorf("new order: %w", err)
	}
	for _, url := range ord.AuthzURLs {
		if err := s.authorize(ctx, cl, o, url); err != nil {
			return nil, err
		}
	}
	if ord, err = cl.WaitOrder(ctx, ord.URI); err != nil {
		return nil, fmt.Errorf("wait for the order: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("make the certificate's key: %w", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{o.Name}}, key)
	if err != nil {
		return nil, fmt.Errorf("make the signing request: %w", err)
	}
	// CreateCertFromOrder, not CreateOrderCert: the finalize answer need
	// not carry the order's Location (Pebble's does not), and the wait
	// for the certificate polls the order's URL.
	chain, _, err := cl.CreateCertFromOrder(ctx, ord, csr, true)
	if err != nil {
		return nil, fmt.Errorf("finalize the order: %w", err)
	}
	return check(o.Name, chain, key)
}

// authorize satisfies the authorization at url with its http-01
// challenge: the answer goes into the ingress's table, and once every
// connected ingress instance holds it the challenge is accepted and the
// authorization awaited; a wildcard's with its dns-01 challenge
// (authorizeDNS01). An authorization valid already needs nothing.
func (s *Issuer) authorize(ctx context.Context, cl *xacme.Client, o store.ACMEOrder, url string) error {
	z, err := cl.GetAuthorization(ctx, url)
	if err != nil {
		return fmt.Errorf("read the authorization: %w", err)
	}
	switch z.Status {
	case xacme.StatusValid:
		return nil
	case xacme.StatusPending:
	default:
		return fmt.Errorf("the CA's authorization for %s is %s", z.Identifier.Value, z.Status)
	}
	if domains.IsWildcard(o.Name) {
		return s.authorizeDNS01(ctx, cl, o, z)
	}
	var chal *xacme.Challenge
	for _, c := range z.Challenges {
		if c.Type == "http-01" {
			chal = c
			break
		}
	}
	if chal == nil {
		return errNoHTTP01
	}
	if !tokenRE.MatchString(chal.Token) {
		return fmt.Errorf("the CA's challenge token %q is not base64url", chal.Token)
	}
	keyAuth, err := cl.HTTP01ChallengeResponse(chal.Token)
	if err != nil {
		return fmt.Errorf("the challenge's answer: %w", err)
	}
	if err := s.fenced(ctx, func(tx *store.Store) error {
		return tx.PutACMEChallenge(ctx, store.ACMEChallenge{Token: chal.Token, Host: o.Name, KeyAuth: keyAuth,
			DomainID: o.DomainID}, s.cfg.ChallengeTTL)
	}); err != nil {
		return fmt.Errorf("keep the challenge: %w", err)
	}
	if err := s.awaitIngress(ctx, chal.Token); err != nil {
		return err
	}
	if _, err := cl.Accept(ctx, chal); err != nil {
		return fmt.Errorf("accept the challenge: %w", err)
	}
	if _, err := cl.WaitAuthorization(ctx, z.URI); err != nil {
		return fmt.Errorf("wait for the authorization: %w", err)
	}
	return nil
}

// awaitIngress waits until every ingress instance connected now holds
// the challenge token (its stream sent it), and at least one is
// connected; at most DeliveryWait on the database's clock.
func (s *Issuer) awaitIngress(ctx context.Context, token string) error {
	var since time.Time
	for {
		missing, connected, now, err := s.d.Store.IngressChallengeHolders(ctx, token, liveIngress)
		if err != nil {
			return err
		}
		if connected > 0 && len(missing) == 0 {
			return nil
		}
		if since.IsZero() {
			since = now
		}
		if now.Sub(since) >= s.cfg.DeliveryWait {
			if connected == 0 {
				return errNoIngress
			}
			return &notDelivered{missing: missing, wait: s.cfg.DeliveryWait}
		}
		t := time.NewTimer(deliveryPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// check checks the chain the CA issued for name and key: it parses, its
// first certificate is for name and for key, and its lifetime is not
// empty. The certificate is renewed from half its lifetime on: a six-day
// certificate every three days and a bit, which leaves three days for
// failed renewals, and a 90-day one after 45.
func check(name string, chain [][]byte, key *ecdsa.PrivateKey) (*issued, error) {
	if len(chain) == 0 {
		return nil, errors.New("the CA issued an empty chain")
	}
	var leaf *x509.Certificate
	for i, der := range chain {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("the CA's certificate %d does not parse: %w", i, err)
		}
		if i == 0 {
			leaf = c
		}
	}
	if err := leaf.VerifyHostname(name); err != nil {
		return nil, fmt.Errorf("the CA's certificate: %w", err)
	}
	pub, ok := leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(key.Public()) {
		return nil, errors.New("the CA's certificate is not for the key of the signing request")
	}
	if !leaf.NotAfter.After(leaf.NotBefore) {
		return nil, fmt.Errorf("the CA's certificate has no lifetime (%v to %v)", leaf.NotBefore, leaf.NotAfter)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("the certificate's key: %w", err)
	}
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	return &issued{chain: chain, key: der, notBefore: leaf.NotBefore, notAfter: leaf.NotAfter,
		renewAfter: leaf.NotBefore.Add(life / 2), serial: strings.ToLower(leaf.SerialNumber.Text(16))}, nil
}

// The ACME problem types the texts name (RFC 8555 §6.7), without their
// prefix urn:ietf:params:acme:error:.
const (
	problemCAA       = "caa"
	problemRateLimit = "ratelimited"
	problemRejected  = "rejectedidentifier"
)

// describe turns an order's error into issue_error, readable for the
// domain's owner, and the wait the CA asks for (its Retry-After of a rate
// limit; 0: none).
func describe(name string, err error, timeout time.Duration) (string, time.Duration) {
	var nd *notDelivered
	var np *notPublished
	var ae *xacme.AuthorizationError
	var oe *xacme.OrderError
	var pe *xacme.Error
	text, wait := "", time.Duration(0)
	switch {
	case errors.Is(err, errNoIngress), errors.Is(err, errNoHTTP01), errors.As(err, &nd),
		errors.Is(err, errNoZone), errors.Is(err, errNoDNS01), errors.As(err, &np):
		text = err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		text = fmt.Sprintf("the order did not finish within %v", timeout)
	case errors.As(err, &ae):
		var details []string
		for _, e := range ae.Errors {
			var p *xacme.Error
			if errors.As(e, &p) && problem(p) == problemCAA {
				return bound(fmt.Sprintf("a CAA record of %s forbids the CA to issue a certificate: %s", name, p.Detail)), 0
			}
			details = append(details, problemText(e))
		}
		if len(details) == 0 {
			details = []string{"no detail"}
		}
		kind := "HTTP-01"
		if domains.IsWildcard(name) {
			kind = "DNS-01"
		}
		text = fmt.Sprintf("the CA's %s challenge for %s failed: %s", kind, name, strings.Join(details, "; "))
	case errors.As(err, &oe):
		if oe.Problem != nil {
			return describe(name, oe.Problem, timeout)
		}
		text = fmt.Sprintf("the CA's order for %s became %s", name, oe.Status)
	case errors.As(err, &pe):
		switch problem(pe) {
		case problemCAA:
			text = fmt.Sprintf("a CAA record of %s forbids the CA to issue a certificate: %s", name, pe.Detail)
		case problemRateLimit:
			wait, _ = xacme.RateLimit(pe)
			text = "rate limited by the CA: " + pe.Detail
		case problemRejected:
			text = fmt.Sprintf("the CA refuses to issue a certificate for %s: %s", name, pe.Detail)
		default:
			text = "the CA refused: " + problemText(pe)
		}
	default:
		text = "the order failed: " + err.Error()
	}
	return bound(text), wait
}

// problem is an ACME error's problem type without its prefix, lower
// case.
func problem(p *xacme.Error) string {
	t := strings.ToLower(p.ProblemType)
	if i := strings.LastIndex(t, ":"); i >= 0 {
		t = t[i+1:]
	}
	return t
}

// problemText is an ACME error's detail with its problem type.
func problemText(err error) string {
	var p *xacme.Error
	if !errors.As(err, &p) {
		return err.Error()
	}
	if p.Detail == "" {
		return problem(p)
	}
	return fmt.Sprintf("%s (%s)", p.Detail, problem(p))
}

// bound cuts text to maxErrorText bytes, on a rune boundary.
func bound(text string) string {
	if len(text) <= maxErrorText {
		return text
	}
	cut := maxErrorText - len("…")
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}
