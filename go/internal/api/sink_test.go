// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
)

// GET /api/v1/tenant answers a tenant's token with its tenant and role,
// anything else 401.
func TestGetTenant(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	expect(t, e.do("GET", "/api/v1/tenant", bearerOf(e.tenantToken), nil), http.StatusOK,
		`{"tenant":"acme","role":"admin","environments":[]}`+"\n")
	expect(t, e.do("GET", "/api/v1/tenant", bearerOf("nope"), nil), http.StatusUnauthorized, errBody("unknown tenant token"))
	expect(t, e.do("GET", "/api/v1/tenant", "", nil), http.StatusUnauthorized, errBody("tenant bearer required"))
}

// testIssuer is a fresh platform CA's issuer.
func testIssuer(t *testing.T) *pki.Issuer {
	t.Helper()
	dir := t.TempDir()
	if err := pki.InitCA(pki.InitCAOptions{RootDir: filepath.Join(dir, "root"), Dir: filepath.Join(dir, "ca"),
		TrustDomain: "feos.test", DNSDomains: []string{"feos.test"}}); err != nil {
		t.Fatal(err)
	}
	iss, err := pki.LoadIssuer(filepath.Join(dir, "ca"), "feos.test")
	if err != nil {
		t.Fatal(err)
	}
	return iss
}

// A sink's service token gets a certificate of 24 h whose one URI names
// the token's sink, verified by the bundle as a server's; a wrong, a
// tenant's or a revoked token is 401, a bad CSR 400, and the eleventh
// certificate within an hour 429. The token's revoke is the admin's.
func TestServiceCertificates(t *testing.T) {
	t.Parallel()
	iss := testIssuer(t)
	e := newEnv(t, envOptions{issuer: iss})
	ctx := context.Background()
	token, hash, err := pki.NewServiceToken(model.ServiceSink)
	if err != nil {
		t.Fatal(err)
	}
	st, err := e.st.CreateServiceToken(ctx, hash, model.ServiceSink, "sink-0", "test")
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	der, err := pki.CreateCSR(key, "sink-0")
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]string{"csr": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))}

	before := time.Now()
	w := e.do("POST", "/api/v1/service-certificates", bearerOf(token), body)
	expect(t, w, http.StatusOK, "")
	got := decode[struct {
		Certificate, Bundle, NotAfter string
	}](t, w)
	var chain []*x509.Certificate
	for rest := []byte(got.Certificate); ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			break
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		chain = append(chain, c)
	}
	roots := x509.NewCertPool()
	if len(chain) != 2 || !roots.AppendCertsFromPEM([]byte(got.Bundle)) {
		t.Fatalf("chain %d, bundle %q", len(chain), got.Bundle)
	}
	leaf := chain[0]
	inter := x509.NewCertPool()
	inter.AddCert(chain[1])
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if name, err := pki.ParseSinkID("feos.test", leaf); err != nil || name != "sink-0" || len(leaf.DNSNames) != 0 ||
		len(leaf.IPAddresses) != 0 || !leaf.PublicKey.(ed25519.PublicKey).Equal(key.Public()) {
		t.Fatalf("the leaf: %q %v %v %v", name, err, leaf.DNSNames, leaf.IPAddresses)
	}
	if d := leaf.NotAfter.Sub(before.Add(24 * time.Hour)); d < -2*time.Second || d > 2*time.Second ||
		got.NotAfter != leaf.NotAfter.UTC().Format(time.RFC3339) {
		t.Fatalf("notAfter %v (%s)", leaf.NotAfter, got.NotAfter)
	}

	for _, authz := range []string{bearerOf("feos-sink-nope"), bearerOf(e.tenantToken), bearerOf(token + "x")} {
		expect(t, e.do("POST", "/api/v1/service-certificates", authz, body), http.StatusUnauthorized,
			errBody("unknown service token"))
	}
	expect(t, e.do("POST", "/api/v1/service-certificates", bearerOf(token), map[string]string{"csr": "nope"}),
		http.StatusBadRequest, errBody("csr must be a PEM CERTIFICATE REQUEST"))
	// The bad CSR counted: 2 of 10.
	for range serviceCertsPerHour - 2 {
		expect(t, e.do("POST", "/api/v1/service-certificates", bearerOf(token), body), http.StatusOK, "")
	}
	expect(t, e.do("POST", "/api/v1/service-certificates", bearerOf(token), body), http.StatusTooManyRequests, "")

	w = e.do("GET", "/api/v1/admin/service-tokens", adminAuth(), nil)
	expect(t, w, http.StatusOK, "")
	if !strings.Contains(w.Body.String(), `"name":"sink-0"`) || strings.Contains(w.Body.String(), hash) ||
		!strings.Contains(w.Body.String(), `"lastUsedAt":"2`) {
		t.Fatalf("list %s", w.Body.String())
	}
	expect(t, e.do("DELETE", "/api/v1/admin/service-tokens/"+st.ID.String(), adminAuth(), nil), http.StatusOK,
		`{"revoked":true}`+"\n")
	expect(t, e.do("DELETE", "/api/v1/admin/service-tokens/"+st.ID.String(), adminAuth(), nil), http.StatusOK,
		`{"revoked":false}`+"\n")
	expect(t, e.do("POST", "/api/v1/service-certificates", bearerOf(token), body), http.StatusUnauthorized,
		errBody("unknown service token"))
	var kinds []string
	rows, err := e.d.Pool.Query(ctx, "SELECT kind FROM activity WHERE kind LIKE 'service%' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, k)
	}
	// The certificates issued (the first and eight more), and the revoke.
	if issued := 1 + serviceCertsPerHour - 2; len(kinds) != issued+1 || kinds[0] != "service_cert" ||
		kinds[issued] != "service_token_revoked" {
		t.Fatalf("activity %v", kinds)
	}
}
