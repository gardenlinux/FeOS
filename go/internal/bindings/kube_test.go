// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/seal"
	"github.com/gardenlinux/feos/go/internal/store"
)

const kubeTestToken = "eyJhbGciOiJSUzI1NiJ9.kube-test-token-PLAINTEXT-7d1f"

// ------------------------------------------------------------------ pki --

type testCA struct {
	caPEM string
	cert  *x509.Certificate
	key   *ecdsa.PrivateKey
}

var testSerial atomic.Int64

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(testSerial.Add(1)),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{caPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), cert: cert, key: key}
}

// issue signs a leaf for client and server use; ips become its IP SANs.
func (ca *testCA) issue(t *testing.T, cn string, ips []net.IP) (certPEM, keyPEM string, pair tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(testSerial.Add(1)),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}))
	pair, err = tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	return certPEM, keyPEM, pair
}

// -------------------------------------------------------------- helpers --

// tokenBinding is one valid token binding in the PUT shape.
func tokenBinding(name string, port int, server string) map[string]any {
	return map[string]any{"name": name, "port": port, "server": server,
		"auth": map[string]any{"type": "token", "token": kubeTestToken}}
}

// putKube sets sb1's Kubernetes bindings and returns the decoded answer.
func (r *rig) putKube(body string) kubeSetOut {
	r.t.Helper()
	code, raw := r.sandboxCall(http.MethodPut, "/kube", body)
	if code != http.StatusOK {
		r.t.Fatalf("PUT kube: HTTP %d %s", code, raw)
	}
	var out kubeSetOut
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		r.t.Fatal(err)
	}
	return out
}

func (r *rig) kubeRows() (string, []store.KubeBinding) {
	r.t.Helper()
	v, rows, err := r.st.KubeBindings(bg, r.sb1)
	if err != nil {
		r.t.Fatal(err)
	}
	return v, rows
}

// ----------------------------------------------------------------- tests --

// The wiring these tests assume, and api is expected to provide: the set
// behind a sandbox token that names the path's sandbox, the preflight
// behind a tenant token. It checks the harness's mux, not this package;
// the route table and its auth classes are api's to test.
func TestKubeHarnessWiring(t *testing.T) {
	r := newRig(t)
	own := "/api/v1/sandboxes/" + r.sb1.String() + "/kube"
	for _, c := range []struct {
		name, method, path, auth string
		want                     int
	}{
		{"set without token", http.MethodGet, own, "", http.StatusUnauthorized},
		{"set with tenant bearer", http.MethodGet, own, "Bearer " + tenantToken, http.StatusNotFound},
		{"another sandbox's set", http.MethodGet, "/api/v1/sandboxes/" + r.sb2.String() + "/kube",
			"Bearer " + sb1Token, http.StatusNotFound},
		{"own set", http.MethodGet, own, "Bearer " + sb1Token, http.StatusOK},
		{"preflight with sandbox token", http.MethodPost, "/api/v1/kube/preflight", "Bearer " + sb1Token,
			http.StatusUnauthorized},
	} {
		if code, body := r.call(c.method, c.path, c.auth, ""); code != c.want {
			t.Errorf("%s: HTTP %d %s, want %d", c.name, code, body, c.want)
		}
	}
}

// Validation, each failure with its stable code, the error body keeping
// its "error" next to the "code".
func TestKubePutValidationCodes(t *testing.T) {
	r := newRig(t)
	ok := func() map[string]any { return tokenBinding("prod", 6443, "https://k8s.example.test:6443") }
	with := func(k string, v any) map[string]any { b := ok(); b[k] = v; return b }
	authOf := func(a map[string]any) map[string]any { b := ok(); b["auth"] = a; return b }
	pkiA, pkiB := newTestCA(t, "ca-a"), newTestCA(t, "ca-b")
	certA, keyA, _ := pkiA.issue(t, "fecode", nil)
	_, keyB, _ := pkiB.issue(t, "other", nil)
	many := make([]map[string]any, 33)
	for i := range many {
		many[i] = tokenBinding("b"+strings.Repeat("x", i%3)+string(rune('a'+i%26))+string(rune('a'+i/26)), 2000+i,
			"https://k8s.example.test")
	}
	big := ok()
	big["caPem"] = strings.Repeat(pkiA.caPEM, (1<<20)/len(pkiA.caPEM)+1)

	for _, c := range []struct {
		name string
		body string
		code string
	}{
		{"no bindings field", `{}`, codeInvalidBinding},
		{"empty body", ``, codeInvalidBinding},
		{"not json", `{"bindings":`, codeInvalidBinding},
		{"not an object", `[]`, codeInvalidBinding},
		{"name not a label", setBody(with("name", "Prod")), codeInvalidBinding},
		{"empty name", setBody(with("name", "")), codeInvalidBinding},
		{"port below 1024", setBody(with("port", 80)), codeInvalidBinding},
		{"port above 65535", setBody(with("port", 70000)), codeInvalidBinding},
		{"vsock port", setBody(with("port", 10001)), codeInvalidBinding},
		{"port as string", setBody(with("port", "6443")), codeInvalidBinding},
		{"duplicate name", setBody(ok(), with("port", 7443)), codeInvalidBinding},
		{"duplicate port", setBody(ok(), with("name", "staging")), codeInvalidBinding},
		{"too many", setBody(many...), codeInvalidBinding},
		{"too large", setBody(big), codeInvalidBinding},
		{"no server", setBody(with("server", "")), codeInvalidBinding},
		{"http server", setBody(with("server", "http://k8s.example.test:6443")), codeInsecureServer},
		{"no scheme", setBody(with("server", "k8s.example.test:6443")), codeInsecureServer},
		{"path", setBody(with("server", "https://k8s.example.test/k8s")), codeInvalidBinding},
		{"query", setBody(with("server", "https://k8s.example.test?x=1")), codeInvalidBinding},
		{"fragment", setBody(with("server", "https://k8s.example.test#x")), codeInvalidBinding},
		{"userinfo", setBody(with("server", "https://u:p@k8s.example.test")), codeInvalidBinding},
		{"bad port", setBody(with("server", "https://k8s.example.test:0")), codeInvalidBinding},
		{"bad host", setBody(with("server", "https://k8s_example.test")), codeInvalidBinding},
		{"bad serverName", setBody(with("serverName", "a b")), codeInvalidBinding},
		{"not utf-8", setBody(with("serverName", "\xff")), codeInvalidBinding},
		{"ca garbage", setBody(with("caPem", "not a pem")), codeBadCA},
		{"ca with a key", setBody(with("caPem", pkiA.caPEM+keyA)), codeBadCA},
		{"no auth type", setBody(authOf(map[string]any{"token": "x"})), codeInvalidBinding},
		{"unknown auth type", setBody(authOf(map[string]any{"type": "exec"})), codeInvalidBinding},
		{"token missing", setBody(authOf(map[string]any{"type": "token"})), codeInvalidBinding},
		{"token with cert", setBody(authOf(map[string]any{"type": "token", "token": "t", "certPem": certA})),
			codeInvalidBinding},
		{"token with newline", setBody(authOf(map[string]any{"type": "token", "token": "a\nb"})), codeInvalidBinding},
		{"token with space", setBody(authOf(map[string]any{"type": "token", "token": "a b"})), codeInvalidBinding},
		{"cert missing key", setBody(authOf(map[string]any{"type": "client_cert", "certPem": certA})),
			codeInvalidBinding},
		{"cert with token", setBody(authOf(map[string]any{"type": "client_cert", "certPem": certA, "keyPem": keyA,
			"token": "t"})), codeInvalidBinding},
		{"cert and key mismatch", setBody(authOf(map[string]any{"type": "client_cert",
			"certPem": certA, "keyPem": keyB})), codeBadClientCert},
		{"cert garbage", setBody(authOf(map[string]any{"type": "client_cert", "certPem": "x", "keyPem": "y"})),
			codeBadClientCert},
		// The destination rule, without exceptions.
		{"platform cluster", setBody(with("server", "https://10.0.0.10:6443")), codeDestinationDenied},
		{"loopback", setBody(with("server", "https://127.0.0.1:6443")), codeDestinationDenied},
		{"metadata", setBody(with("server", "https://169.254.169.254")), codeDestinationDenied},
		{"cgnat", setBody(with("server", "https://100.64.1.1")), codeDestinationDenied},
		{"ipv6 loopback", setBody(with("server", "https://[::1]:6443")), codeDestinationDenied},
		{"ipv6 public", setBody(with("server", "https://[2001:db8::1]:6443")), codeDestinationDenied},
		{"v4-mapped internal", setBody(with("server", "https://[::ffff:10.0.0.10]:6443")), codeDestinationDenied},
		{"name resolving internal", setBody(with("server", "https://internal.test:6443")), codeDestinationDenied},
		{"name resolving mixed", setBody(with("server", "https://mixed.test")), codeDestinationDenied},
		{"name not resolving", setBody(with("server", "https://nxdomain.test")), codeDestinationDenied},
		{"second binding denied", setBody(ok(), map[string]any{"name": "evil", "port": 7443,
			"server": "https://internal.test", "auth": map[string]any{"type": "token", "token": "t"}}),
			codeDestinationDenied},
	} {
		code, raw := r.sandboxCall(http.MethodPut, "/kube", c.body)
		msg, got := codeOf(raw)
		if code != http.StatusBadRequest || got != c.code || msg == "" {
			t.Errorf("%s: HTTP %d %s, want 400 with code %s", c.name, code, strings.TrimSpace(raw), c.code)
		}
	}
	if v, rows := r.kubeRows(); v != "" || len(rows) != 0 {
		t.Fatalf("a rejected PUT stored something: %q %+v", v, rows)
	}

	// The texts clients see.
	for body, want := range map[string]string{
		`{}`:                                   "bindings is required: the complete set, [] for none",
		setBody(with("port", 10001)):           "bindings[0]: port 10001 is reserved (10000–10002 are the sandbox's own vsock ports)",
		setBody(ok(), with("name", "staging")): `bindings[1]: duplicate port 6443`,
		setBody(with("server", "https://internal.test:6443")): "bindings[0] (prod): server host internal.test resolves to " +
			"an internal address",
	} {
		_, raw := r.sandboxCall(http.MethodPut, "/kube", body)
		if msg, _ := codeOf(raw); msg != want {
			t.Errorf("message %q, want %q", msg, want)
		}
	}

	// And the valid forms: a public IP literal, a public name, a client
	// certificate with a CA, a trailing slash.
	cc := map[string]any{"name": "staging", "port": 7443, "server": "https://203.0.113.20/",
		"serverName": "api.staging", "caPem": pkiA.caPEM,
		"auth": map[string]any{"type": "client_cert", "certPem": certA, "keyPem": keyA}}
	out := r.putKube(setBody(ok(), cc))
	if len(out.Bindings) != 2 || out.Version == "" {
		t.Fatalf("valid set: %+v", out)
	}
}

// The version is the old system's: the SHA-256 of the snake_case JSON of
// the bindings sorted by port, so that a set has the same version whoever
// computes it.
func TestKubeVersionShape(t *testing.T) {
	bs := []kubeBinding{
		{Name: "staging", Port: 7443, Server: "https://203.0.113.20", CAPEM: "CA",
			Auth: kubeAuth{Type: "client_cert", CertPEM: "C", KeyPEM: "K"}},
		{Name: "prod", Port: 6443, Server: "https://k8s.example.test", Auth: kubeAuth{Type: "token", Token: "T"}},
	}
	want := `[{"name":"prod","port":6443,"server":"https://k8s.example.test","auth":{"type":"token","token":"T"}},` +
		`{"name":"staging","port":7443,"server":"https://203.0.113.20","ca_pem":"CA","auth":{"type":"client_cert","cert_pem":"C","key_pem":"K"}}]`
	sum := sha256.Sum256([]byte(want))
	if got := kubeVersion(bs); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("version %s, want the hash of %s", got, want)
	}
	if kubeVersion(nil) != "" {
		t.Fatal("the empty set's version is not empty")
	}
	if f := kubeFingerprint(kubeAuth{Type: "token", Token: "T"}); !regexp.MustCompile(`^sha256:[0-9a-f]{16}$`).MatchString(f) {
		t.Fatalf("fingerprint %q", f)
	}
}

// PUT, GET, DELETE: stored with the credential sealed, never echoed,
// idempotent; a smaller set replaces the whole one.
func TestKubePutGetDelete(t *testing.T) {
	r := newRig(t)
	ca := newTestCA(t, "ca")
	cert, key, _ := ca.issue(t, "fecode", nil)
	cc := map[string]any{"name": "staging", "port": 7443, "server": "https://203.0.113.20:6443",
		"caPem": ca.caPEM, "auth": map[string]any{"type": "client_cert", "certPem": cert, "keyPem": key}}
	body := setBody(tokenBinding("prod", 6443, "https://k8s.example.test:6443"), cc)

	out := r.putKube(body)
	want := kubeVersion([]kubeBinding{
		{Name: "prod", Port: 6443, Server: "https://k8s.example.test:6443", Auth: kubeAuth{Type: "token", Token: kubeTestToken}},
		{Name: "staging", Port: 7443, Server: "https://203.0.113.20:6443", CAPEM: ca.caPEM,
			Auth: kubeAuth{Type: "client_cert", CertPEM: cert, KeyPEM: key}},
	})
	if out.Version != want {
		t.Fatalf("version %q, want %q", out.Version, want)
	}
	fp := regexp.MustCompile(`^sha256:[0-9a-f]{16}$`)
	if len(out.Bindings) != 2 || out.Bindings[0].Name != "prod" || out.Bindings[0].AuthType != "token" ||
		out.Bindings[0].HasCa || !fp.MatchString(out.Bindings[0].Fingerprint) ||
		out.Bindings[1].AuthType != "client_cert" || !out.Bindings[1].HasCa ||
		out.Bindings[1].Fingerprint == out.Bindings[0].Fingerprint {
		t.Fatalf("PUT answer: %+v", out)
	}
	if v, rows := r.kubeRows(); v != want || len(rows) != 2 {
		t.Fatalf("stored: %q %+v", v, rows)
	}

	// GET: the set without its material.
	code, raw := r.sandboxCall(http.MethodGet, "/kube", "")
	if code != http.StatusOK {
		t.Fatalf("GET: %d %s", code, raw)
	}
	for _, secret := range []string{kubeTestToken, "PRIVATE KEY", "BEGIN CERTIFICATE"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("GET leaks %q: %s", secret, raw)
		}
	}
	var got kubeSetOut
	_ = json.Unmarshal([]byte(raw), &got)
	if got.Version != want || len(got.Bindings) != 2 || got.Bindings[1].Server != "https://203.0.113.20:6443" {
		t.Fatalf("GET: %+v", got)
	}

	// The same set again: no write (the sealed blobs stay as they are).
	_, before := r.kubeRows()
	if again := r.putKube(body); again.Version != want {
		t.Fatalf("repeat PUT version %q", again.Version)
	}
	_, after := r.kubeRows()
	for i := range before {
		if !bytes.Equal(before[i].Sealed, after[i].Sealed) {
			t.Fatal("a repeated PUT rewrote the set")
		}
	}

	// A smaller set replaces the whole one.
	r.putKube(setBody(tokenBinding("prod", 6443, "https://k8s.example.test:6443")))
	if _, rows := r.kubeRows(); len(rows) != 1 {
		t.Fatalf("after shrinking: %+v", rows)
	}

	// DELETE: 204, nothing left.
	if code, raw := r.sandboxCall(http.MethodDelete, "/kube", ""); code != http.StatusNoContent || raw != "" {
		t.Fatalf("DELETE: %d %s", code, raw)
	}
	code, raw = r.sandboxCall(http.MethodGet, "/kube", "")
	if code != http.StatusOK || strings.TrimSpace(raw) != `{"version":"","bindings":[]}` {
		t.Fatalf("GET after DELETE: %d %s", code, raw)
	}
	if code, raw := r.sandboxCall(http.MethodDelete, "/kube", ""); code != http.StatusNoContent {
		t.Fatalf("second DELETE: %d %s", code, raw)
	}
	// [] is a valid PUT too.
	if out := r.putKube(`{"bindings":[]}`); out.Version != "" || len(out.Bindings) != 0 {
		t.Fatalf("PUT []: %+v", out)
	}
}

// A sandbox that went away between the token check and the write: 404.
func TestKubePutMissingSandbox(t *testing.T) {
	r := newRig(t)
	if _, err := r.st.DeleteSandbox(bg, r.sb1); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{http.MethodPut, http.MethodGet, http.MethodDelete} {
		code, raw := r.sandboxCall(m, "/kube", setBody(tokenBinding("prod", 6443, "https://k8s.example.test")))
		if msg, _ := codeOf(raw); code != http.StatusNotFound || msg != "no such sandbox" {
			t.Errorf("%s on a deleted sandbox: %d %s", m, code, raw)
		}
	}
}

// The credential is sealed in the database, bound to its row: no
// plaintext in the table, another key does not open it, a blob moved to
// another sandbox does not open, and a PUT of the same set under a new
// key is the repair.
func TestKubeSecretsSealedAtRest(t *testing.T) {
	r := newRig(t)
	ca := newTestCA(t, "ca")
	cert, key, _ := ca.issue(t, "fecode", nil)
	body := setBody(tokenBinding("prod", 6443, "https://k8s.example.test"),
		map[string]any{"name": "staging", "port": 7443, "server": "https://203.0.113.20",
			"auth": map[string]any{"type": "client_cert", "certPem": cert, "keyPem": key}})
	v := r.putKube(body).Version

	var dump string
	if err := r.d.Pool.QueryRow(bg, "SELECT string_agg(kube_bindings::text, ' ') FROM kube_bindings").Scan(&dump); err != nil {
		t.Fatal(err)
	}
	keyLine := strings.Split(key, "\n")[1]
	for _, secret := range []string{kubeTestToken, keyLine, hex.EncodeToString([]byte(kubeTestToken))} {
		if strings.Contains(dump, secret) {
			t.Fatalf("kube_bindings holds secret material in plaintext")
		}
	}
	_, rows := r.kubeRows()
	for _, row := range rows {
		if row.Sealed[0] != seal.Version || len(row.Sealed) < 1+12+16 {
			t.Fatalf("blob shape: % x", row.Sealed[:4])
		}
	}
	if _, err := r.svc.openKube(r.sb1, v, rows); err != nil {
		t.Fatalf("the right key must open the set: %v", err)
	}
	other, err := New(Config{Enabled: true, Key: newKey(t), Log: quietLog()}, r.st, r.d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.openKube(r.sb1, v, rows); err == nil || !strings.Contains(err.Error(), "--secret-key-file") {
		t.Fatalf("another key must not open the set: %v", err)
	}
	if _, _, err := other.PodBindings(bg, r.sb1); err == nil {
		t.Fatal("PodBindings delivered a set that does not open")
	}
	if _, err := r.svc.openKubeRow(r.sb2, rows[0]); err == nil {
		t.Fatal("a sealed credential opened under another sandbox")
	}
	relabelled := rows[0]
	relabelled.Name = "other"
	if _, err := r.svc.openKubeRow(r.sb1, relabelled); err == nil {
		t.Fatal("a sealed credential opened under another name")
	}
	relabelled = rows[0]
	relabelled.AuthType = kubeAuthClientCert
	if _, err := r.svc.openKubeRow(r.sb1, relabelled); err == nil {
		t.Fatal("a sealed credential opened under another auth type")
	}

	// The same set PUT through a Service with a new key rewrites it.
	other.dest.lookup = fakeLookup
	r2 := *r
	r2.svc = other
	r2.srv = httptest.NewServer(r2.mux())
	t.Cleanup(r2.srv.Close)
	if again := r2.putKube(body); again.Version != v {
		t.Fatalf("repair PUT version %q, want %q", again.Version, v)
	}
	if _, rows := r.kubeRows(); func() error { _, err := other.openKube(r.sb1, v, rows); return err }() != nil {
		t.Fatal("the repair PUT did not reseal the set")
	}
}

// Deleting a sandbox deletes its bindings.
func TestKubeSandboxDeleteCascades(t *testing.T) {
	r := newRig(t)
	r.putKube(setBody(tokenBinding("prod", 6443, "https://k8s.example.test")))
	if _, err := r.st.DeleteSandbox(bg, r.sb1); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := r.d.Pool.QueryRow(bg, "SELECT count(*) FROM kube_bindings WHERE sandbox_id = $1", r.sb1).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d bindings survived their sandbox", n)
	}
}
