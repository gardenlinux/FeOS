// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

const testDomain = "dashboard.fecore.dev"

// domainOf returns a domain with a self-signed certificate for cn.
func domainOf(t *testing.T, name, cn string) wire.Domain {
	t.Helper()
	certPEM, keyPEM := pairPEM(t, cn)
	c, _ := pem.Decode(certPEM)
	k, _ := pem.Decode(keyPEM)
	return wire.Domain{Name: name, Chain: [][]byte{c.Bytes}, Key: k.Bytes}
}

// withDomains is testState with the domains added to the environment's
// hosts and domains.
func withDomains(ds ...wire.Domain) *wire.State {
	s := testState(svc("web", ep("i1", "fd00::1", "n1")))
	e := s.Envs[testEnv]
	for _, d := range ds {
		e.Hosts = append(e.Hosts, d.Name)
	}
	e.Domains = ds
	s.Envs[testEnv] = e
	return s
}

// handshake dials srv with TLS for sni (none when empty) and returns the
// served certificate's DNS names.
func handshake(t *testing.T, addr, sni string) []string {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: sni, InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("handshake %q: %v", sni, err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].DNSNames
}

// TLS picks a domain's own certificate by SNI from the table and the
// wildcard otherwise: for the apps zone's hosts, an unknown name, no
// SNI, and a domain whose certificate does not load or is not for it. A
// renewed certificate replaces the old one; an unchanged one is reused
// parsed.
func TestDomainCertificateBySNI(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, "*."+testZone)
	certs, err := LoadPair(certFile, keyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{}, Deps{})
	ok := domainOf(t, testDomain, testDomain)
	wrongName := domainOf(t, "www.fecore.dev", "other.example")
	badKey := domainOf(t, "api.fecore.dev", "api.fecore.dev")
	badKey.Key = domainOf(t, "api.fecore.dev", "api.fecore.dev").Key
	p.SetState(withDomains(ok, wrongName, badKey))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := p.HTTPSServer(certs)
	go func() { _ = hs.ServeTLS(ln, "", "") }()
	defer hs.Close()
	addr := ln.Addr().String()

	if got := handshake(t, addr, testDomain); !slices.Equal(got, []string{testDomain}) {
		t.Fatalf("the domain got %v", got)
	}
	if got := handshake(t, addr, "DASHBOARD.fecore.dev."); !slices.Equal(got, []string{testDomain}) {
		t.Fatalf("the domain in upper case with a dot got %v", got)
	}
	for _, sni := range []string{testHost, "unknown.example", "", "www.fecore.dev", "api.fecore.dev"} {
		if got := handshake(t, addr, sni); !slices.Equal(got, []string{"*." + testZone}) {
			t.Errorf("SNI %q got %v, want the wildcard", sni, got)
		}
	}

	// The table again, unchanged but for the heartbeat: the parsed
	// certificate stays the same object; a renewal replaces it.
	before := p.cur.Load().t.cert(testDomain)
	s := withDomains(ok, wrongName, badKey)
	s.Envs[testEnv].Routes[0].Name = "root2" // another table, same domains
	p.SetState(s)
	if after := p.cur.Load().t.cert(testDomain); after != before {
		t.Fatal("an unchanged certificate was parsed again")
	}
	renewed := domainOf(t, testDomain, testDomain)
	p.SetState(withDomains(renewed))
	got := p.cur.Load().t.cert(testDomain)
	if got == before || !bytes.Equal(got.Certificate[0], renewed.Chain[0]) {
		t.Fatal("the renewed certificate is not served")
	}
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: testDomain, InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	served := conn.ConnectionState().PeerCertificates[0]
	conn.Close()
	leaf, _ := x509.ParseCertificate(renewed.Chain[0])
	if !served.Equal(leaf) {
		t.Fatal("the handshake serves the old certificate")
	}

	// A domain the environment does not serve as a host (not in Hosts)
	// gets no certificate.
	s = testState(svc("web", ep("i1", "fd00::1", "n1")))
	e := s.Envs[testEnv]
	e.Domains = []wire.Domain{ok}
	s.Envs[testEnv] = e
	p.SetState(s)
	if p.cur.Load().t.cert(testDomain) != nil {
		t.Fatal("a domain outside the hosts has a certificate")
	}
}

// Port 80 answers GET /.well-known/acme-challenge/<token> with the key
// authorization when the Host is the challenge's, before the redirect,
// also for a host the table does not serve yet; an unknown token, another
// host, or another method is 404; everything else still redirects.
func TestChallengeAnswer(t *testing.T) {
	p := New(Config{}, Deps{})
	s := testState()
	s.Challenges = []wire.Challenge{
		{Token: "tok-1", Host: testDomain, KeyAuth: "tok-1.thumbprint"},
		{Token: "tok-2", Host: testHost, KeyAuth: "tok-2.thumbprint"},
	}
	p.SetState(s)
	srv := httptest.NewServer(p.ServeHTTPRedirect())
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(method, host, path string) (int, http.Header, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		req.Host = host
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header, string(b)
	}
	const path = "/.well-known/acme-challenge/"
	for _, host := range []string{testDomain, "Dashboard.Fecore.Dev:80", testDomain + "."} {
		status, h, body := do("GET", host, path+"tok-1")
		if status != http.StatusOK || body != "tok-1.thumbprint" || h.Get("Content-Type") != "text/plain" {
			t.Fatalf("host %q: %d %q %v", host, status, body, h)
		}
	}
	if status, _, body := do("GET", testHost, path+"tok-2"); status != http.StatusOK || body != "tok-2.thumbprint" {
		t.Fatalf("a known host's challenge: %d %q", status, body)
	}
	if status, _, body := do("HEAD", testDomain, path+"tok-1"); status != http.StatusOK || body != "" {
		t.Fatalf("HEAD: %d %q", status, body)
	}
	for _, c := range []struct{ method, host, path string }{
		{"GET", testDomain, path + "unknown"},
		{"GET", testDomain, path + "tok-2"},
		{"GET", "other.example", path + "tok-1"},
		{"GET", testHost, path + "tok-1"},
		{"POST", testDomain, path + "tok-1"},
		{"GET", testDomain, path},
		{"GET", testDomain, path + "tok-1/x"},
	} {
		if status, _, body := do(c.method, c.host, c.path); status != http.StatusNotFound || body != textNoChallenge+"\n" {
			t.Errorf("%s %s%s: %d %q", c.method, c.host, c.path, status, body)
		}
	}
	// The rest of a known host still redirects, that of the domain is
	// 404 until the table serves it.
	if status, h, _ := do("GET", testHost, "/.well-known/other"); status != http.StatusPermanentRedirect ||
		h.Get("Location") != "https://"+testHost+"/.well-known/other" {
		t.Fatalf("redirect: %d %v", status, h)
	}
	if status, _, _ := do("GET", testDomain, "/"); status != http.StatusNotFound {
		t.Fatalf("a domain not served yet: %d", status)
	}

	// A table without challenges: gone.
	p.SetState(testState())
	if status, _, _ := do("GET", testDomain, path+"tok-1"); status != http.StatusNotFound {
		t.Fatalf("after the table dropped it: %d", status)
	}

	var b strings.Builder
	p.Metrics(&b)
	for _, want := range []string{`ferun_ingress_acme_answers_total{result="answered"} 5`,
		`ferun_ingress_acme_answers_total{result="unknown"} 8`, "ferun_ingress_acme_challenges 0"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, b.String())
		}
	}
}

// A heartbeat (the same state) keeps the table; a change of the
// challenges alone replaces it.
func TestChallengesChangeTable(t *testing.T) {
	p := New(Config{}, Deps{})
	s := testState()
	p.SetState(s)
	before := p.cur.Load().t
	p.SetState(s.Clone())
	if p.cur.Load().t != before {
		t.Fatal("a heartbeat replaced the table")
	}
	c := s.Clone()
	c.Challenges = []wire.Challenge{{Token: "t", Host: testDomain, KeyAuth: "t.k"}}
	p.SetState(c)
	if t2 := p.cur.Load().t; t2 == before || len(t2.challenges) != 1 {
		t.Fatal("new challenges kept the table")
	}
}

// With the tenant zone's wildcard (ADR 0031), TLS picks it for every name
// it covers, the apps zone's for the rest, and a domain's own before
// either.
func TestTenantZoneCertificateBySNI(t *testing.T) {
	const tenantZone = "apps.run.example"
	certFile, keyFile := writePair(t, t.TempDir(), "*."+testZone)
	apps, err := LoadPair(certFile, keyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = writePair(t, t.TempDir(), "*."+tenantZone)
	tenant, err := LoadPair(certFile, keyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{}, Deps{})
	p.SetState(withDomains(domainOf(t, testDomain, testDomain)))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := p.HTTPSServer(apps, tenant)
	go func() { _ = hs.ServeTLS(ln, "", "") }()
	defer hs.Close()
	addr := ln.Addr().String()
	for sni, want := range map[string]string{
		"web-acme." + tenantZone:        "*." + tenantZone,
		"WEB--Shop." + tenantZone + ".": "*." + tenantZone,
		testHost:                        "*." + testZone,
		"a.b." + tenantZone:             "*." + testZone, // a wildcard covers one label
		tenantZone:                      "*." + testZone,
		"unknown.example":               "*." + testZone,
		"":                              "*." + testZone,
		testDomain:                      testDomain,
	} {
		if got := handshake(t, addr, sni); !slices.Equal(got, []string{want}) {
			t.Errorf("SNI %q got %v, want %s", sni, got, want)
		}
	}
}
