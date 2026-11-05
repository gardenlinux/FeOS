// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// The secret values of the bindings below, which no answer may carry.
const (
	httpAPIKey       = "erp-api-key-PLAINTEXT-4711"
	httpPassword     = "basic-password-PLAINTEXT-0815"
	httpClientSecret = "oauth-client-secret-PLAINTEXT-42"
)

func headersBinding(name, upstream string) map[string]any {
	return map[string]any{"name": name, "upstream": upstream,
		"credential": map[string]any{"kind": "headers", "headers": map[string]any{"X-API-Key": httpAPIKey}}}
}

func (r *rig) putHTTP(body string) httpSetOut {
	r.t.Helper()
	code, raw := r.sandboxCall(http.MethodPut, "/http", body)
	if code != http.StatusOK {
		r.t.Fatalf("PUT http: %d %s", code, raw)
	}
	var out httpSetOut
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// pushes records the pods a PUT had sent their clear part.
type pushes struct {
	mu  sync.Mutex
	ids []podid.ID
}

func (p *pushes) PushHTTPBindings(id podid.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids = append(p.ids, id)
}

func (p *pushes) n() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.ids)
}

func TestHTTPPutValidationCodes(t *testing.T) {
	r := newRig(t)
	ca := newTestCA(t, "erp-ca")
	other := newTestCA(t, "other")
	certPEM, _, _ := ca.issue(t, "client", nil)
	_, otherKey, _ := other.issue(t, "client", nil)
	with := func(mod func(b map[string]any)) map[string]any {
		b := headersBinding("erp", "https://api.erp.example.test")
		mod(b)
		return b
	}
	cred := func(c map[string]any) func(map[string]any) {
		return func(b map[string]any) { b["credential"] = c }
	}
	for _, c := range []struct {
		name string
		b    map[string]any
		code string
		msg  string
	}{
		{"plain http", with(func(b map[string]any) { b["upstream"] = "http://api.erp.example.test" }),
			codeInsecureUpstream, "https"},
		{"path", with(func(b map[string]any) { b["upstream"] = "https://api.erp.example.test/odata" }),
			codeInvalidBinding, "pathPrefixes"},
		{"query", with(func(b map[string]any) { b["upstream"] = "https://api.erp.example.test?x=1" }),
			codeInvalidBinding, "query"},
		{"userinfo", with(func(b map[string]any) { b["upstream"] = "https://u:p@api.erp.example.test" }),
			codeInvalidBinding, "credentials"},
		{"bad name", with(func(b map[string]any) { b["name"] = "-erp" }), codeInvalidBinding, "DNS label"},
		{"upper name", with(func(b map[string]any) { b["name"] = "ERP" }), codeInvalidBinding, "DNS label"},
		{"long name", with(func(b map[string]any) { b["name"] = strings.Repeat("a", 64) }), codeInvalidBinding, "DNS label"},
		{"unknown mode", with(func(b map[string]any) { b["mode"] = "proxy" }), codeInvalidBinding, "unknown"},
		{"intercept port", with(func(b map[string]any) {
			b["mode"], b["upstream"] = "intercept", "https://api.erp.example.test:8443"
		}), codeInvalidBinding, "port 443"},
		{"intercept address", with(func(b map[string]any) {
			b["mode"], b["upstream"] = "intercept", "https://203.0.113.40"
		}), codeInvalidBinding, "host name"},
		{"prefix", with(func(b map[string]any) { b["pathPrefixes"] = []string{"odata/"} }), codeInvalidBinding, "begin"},
		{"dotdot prefix", with(func(b map[string]any) { b["pathPrefixes"] = []string{"/a/../b"} }), codeInvalidBinding, ".."},
		{"bad ca", with(func(b map[string]any) { b["caPem"] = "not a pem" }), codeBadCA, "no PEM"},
		{"burst alone", with(func(b map[string]any) { b["rateLimit"] = map[string]any{"burst": 5} }),
			codeInvalidBinding, "perSecond"},
		{"no credential", with(func(b map[string]any) { delete(b, "credential") }), codeInvalidBinding, "credential is required"},
		{"no kind", with(cred(map[string]any{})), codeInvalidBinding, "kind is required"},
		{"host header", with(cred(map[string]any{"kind": "headers", "headers": map[string]any{"Host": "x"}})),
			codeBadHeader, "cannot set"},
		{"hop-by-hop", with(cred(map[string]any{"kind": "headers", "headers": map[string]any{"Connection": "x"}})),
			codeBadHeader, "cannot set"},
		{"proxy header", with(cred(map[string]any{"kind": "headers",
			"headers": map[string]any{"Proxy-Authorization": "x"}})), codeBadHeader, "cannot set"},
		{"header token", with(cred(map[string]any{"kind": "headers", "headers": map[string]any{"X Key": "x"}})),
			codeBadHeader, "not a header name"},
		{"header value", with(cred(map[string]any{"kind": "headers", "headers": map[string]any{"X-Key": "a\nb"}})),
			codeBadHeader, "control"},
		{"headers twice", with(cred(map[string]any{"kind": "headers",
			"headers": map[string]any{"x-key": "a", "X-Key": "b"}})), codeBadHeader, "one header"},
		{"other fields", with(cred(map[string]any{"kind": "headers", "headers": map[string]any{"X-Key": "a"},
			"password": "p"})), codeInvalidBinding, "takes no password"},
		{"basic colon", with(cred(map[string]any{"kind": "basic", "user": "a:b", "password": "p"})),
			codeInvalidBinding, "':'"},
		{"basic password", with(cred(map[string]any{"kind": "basic", "user": "a"})), codeInvalidBinding, "required"},
		{"oauth http", with(cred(map[string]any{"kind": "oauth2", "tokenUrl": "http://auth.erp.example.test/token",
			"clientId": "c", "clientSecret": "s"})), codeInsecureUpstream, "https"},
		{"oauth secret", with(cred(map[string]any{"kind": "oauth2", "tokenUrl": "https://auth.erp.example.test/token",
			"clientId": "c"})), codeInvalidBinding, "clientSecret is required"},
		{"client cert mismatch", with(cred(map[string]any{"kind": "clientCertificate", "certPem": certPEM,
			"keyPem": otherKey})), codeBadClientCert, "private key"},
		{"unknown kind", with(cred(map[string]any{"kind": "jwt"})), codeInvalidBinding, "not supported"},
		{"internal upstream", with(func(b map[string]any) { b["upstream"] = "https://internal.test" }),
			codeDestinationDenied, "internal"},
		{"internal address", with(func(b map[string]any) { b["upstream"] = "https://10.0.0.1" }),
			codeDestinationDenied, "internal"},
		{"unresolvable", with(func(b map[string]any) { b["upstream"] = "https://nowhere.test" }),
			codeDestinationDenied, "does not resolve"},
		{"internal token url", with(cred(map[string]any{"kind": "oauth2", "tokenUrl": "https://internal.test/token",
			"clientId": "c", "clientSecret": "s"})), codeDestinationDenied, "internal"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, raw := r.sandboxCall(http.MethodPut, "/http", setBody(c.b))
			msg, got := codeOf(raw)
			if code != http.StatusBadRequest || got != c.code || !strings.Contains(msg, c.msg) {
				t.Fatalf("PUT: %d %s; want 400 %s with %q", code, raw, c.code, c.msg)
			}
		})
	}

	dup := setBody(headersBinding("erp", "https://api.erp.example.test"), headersBinding("erp", "https://pay.example.test"))
	if code, raw := r.sandboxCall(http.MethodPut, "/http", dup); code != http.StatusBadRequest ||
		!strings.Contains(raw, "duplicate name") {
		t.Fatalf("duplicate: %d %s", code, raw)
	}
	twice := setBody(
		map[string]any{"name": "a", "upstream": "https://api.erp.example.test", "mode": "intercept",
			"credential": map[string]any{"kind": "basic", "user": "u", "password": "p"}},
		map[string]any{"name": "b", "upstream": "https://api.erp.example.test", "mode": "intercept",
			"credential": map[string]any{"kind": "basic", "user": "u", "password": "p"}})
	if code, raw := r.sandboxCall(http.MethodPut, "/http", twice); code != http.StatusBadRequest ||
		!strings.Contains(raw, "resolve api.erp.example.test") {
		t.Fatalf("one host twice: %d %s", code, raw)
	}
	if code, raw := r.sandboxCall(http.MethodPut, "/http", `{}`); code != http.StatusBadRequest ||
		!strings.Contains(raw, "bindings is required") {
		t.Fatalf("no bindings: %d %s", code, raw)
	}
}

// allKinds is a set with a binding of every credential kind.
func allKinds(t *testing.T) []map[string]any {
	ca := newTestCA(t, "client-ca")
	certPEM, keyPEM, _ := ca.issue(t, "fecode", nil)
	return []map[string]any{
		{"name": "erp", "upstream": "https://api.erp.example.test:443/", "pathPrefixes": []string{"/odata/"},
			"forwardedHost": true, "rateLimit": map[string]any{"perSecond": 10},
			"credential": map[string]any{"kind": "headers", "headers": map[string]any{"x-api-key": httpAPIKey}}},
		{"name": "basic", "upstream": "https://pay.example.test:8443",
			"credential": map[string]any{"kind": "basic", "user": "fecode", "password": httpPassword}},
		{"name": "graph", "upstream": "https://api.erp.example.test", "mode": "intercept",
			"credential": map[string]any{"kind": "oauth2", "tokenUrl": "https://auth.erp.example.test/oauth/token",
				"clientId": "fecode-client", "clientSecret": httpClientSecret, "scope": "read", "clientAuthInBody": true}},
		{"name": "mtls", "upstream": "https://pay.example.test",
			"credential": map[string]any{"kind": "clientCertificate", "certPem": certPEM, "keyPem": keyPEM}},
	}
}

func TestHTTPPutGetDelete(t *testing.T) {
	r := newRig(t)
	p := &pushes{}
	r.svc.SetPusher(p)
	set := allKinds(t)
	out := r.putHTTP(setBody(set...))
	if out.Version == "" || len(out.Bindings) != 4 {
		t.Fatalf("PUT answered %+v", out)
	}
	if p.n() != 1 {
		t.Fatalf("pushes after the PUT: %d", p.n())
	}
	code, raw := r.sandboxCall(http.MethodGet, "/http", "")
	if code != http.StatusOK {
		t.Fatalf("GET: %d %s", code, raw)
	}
	for _, secret := range []string{httpAPIKey, httpPassword, httpClientSecret, "PRIVATE KEY"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("GET carries a secret %q: %s", secret, raw)
		}
	}
	var got struct {
		Version  string           `json:"version"`
		Bindings []map[string]any `json:"bindings"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != out.Version {
		t.Fatalf("GET's version %s, PUT's %s", got.Version, out.Version)
	}
	byName := map[string]map[string]any{}
	for _, b := range got.Bindings {
		byName[b["name"].(string)] = b
	}
	erp := byName["erp"]
	if erp["upstream"] != "https://api.erp.example.test" || erp["mode"] != "name" || erp["forwardedHost"] != true {
		t.Fatalf("erp: %v", erp)
	}
	if rl := erp["rateLimit"].(map[string]any); rl["perSecond"] != float64(10) || rl["burst"] != float64(10) {
		t.Fatalf("erp's rate limit: %v", rl)
	}
	if h := erp["credential"].(map[string]any)["headers"].(map[string]any); h["X-Api-Key"].(map[string]any)["set"] != true {
		t.Fatalf("erp's headers: %v", h)
	}
	if c := byName["basic"]["credential"].(map[string]any); c["user"] != "fecode" ||
		c["password"].(map[string]any)["set"] != true {
		t.Fatalf("basic: %v", c)
	}
	if byName["basic"]["upstream"] != "https://pay.example.test:8443" {
		t.Fatalf("basic's upstream: %v", byName["basic"]["upstream"])
	}
	if c := byName["graph"]["credential"].(map[string]any); c["clientId"] != "fecode-client" ||
		c["clientSecret"].(map[string]any)["set"] != true || c["scope"] != "read" || c["clientAuthInBody"] != true ||
		c["tokenUrl"] != "https://auth.erp.example.test/oauth/token" {
		t.Fatalf("graph: %v", c)
	}
	if c := byName["mtls"]["credential"].(map[string]any); !strings.Contains(c["certPem"].(string), "CERTIFICATE") ||
		c["keyPem"].(map[string]any)["set"] != true {
		t.Fatalf("mtls: %v", c)
	}

	// The same set again changes nothing: the version stays, nothing is
	// pushed.
	if again := r.putHTTP(setBody(set...)); again.Version != out.Version || p.n() != 1 {
		t.Fatalf("the same set again: version %s (was %s), %d pushes", again.Version, out.Version, p.n())
	}
	set[0]["pathPrefixes"] = []string{"/odata/v4/"}
	changed := r.putHTTP(setBody(set...))
	if changed.Version == out.Version || p.n() != 2 {
		t.Fatalf("a changed set: version %s (was %s), %d pushes", changed.Version, out.Version, p.n())
	}

	// Sealed at rest: no secret in the rows.
	var rows string
	if err := r.d.Pool.QueryRow(bg, `SELECT string_agg(row_to_json(b)::text, '') FROM http_bindings b`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{httpAPIKey, httpPassword, httpClientSecret, "PRIVATE KEY"} {
		if strings.Contains(rows, secret) {
			t.Fatalf("a row carries a secret %q", secret)
		}
	}

	if code, raw := r.sandboxCall(http.MethodDelete, "/http", ""); code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", code, raw)
	}
	if p.n() != 3 {
		t.Fatalf("pushes after the DELETE: %d", p.n())
	}
	code, raw = r.sandboxCall(http.MethodGet, "/http", "")
	if code != http.StatusOK || raw != `{"version":"","bindings":[]}`+"\n" {
		t.Fatalf("GET after DELETE: %d %s", code, raw)
	}

	// Another sandbox's token does not reach sb1's set.
	if code, _ := r.call(http.MethodGet, "/api/v1/sandboxes/"+r.sb1.String()+"/http", "Bearer "+sb2Token, ""); code != http.StatusNotFound {
		t.Fatalf("another sandbox's token: %d", code)
	}
	off := newRigWith(t, false)
	if code, raw := off.sandboxCall(http.MethodGet, "/http", ""); code != http.StatusNotFound ||
		!strings.Contains(raw, "no such route") {
		t.Fatalf("off: %d %s", code, raw)
	}
}

// openSealed seals the pod's bindings to a fresh run key as
// IssuePodCredentials does and opens them again.
func openSealed(t *testing.T, r *rig, id podid.ID) (*fleetv1.PodBindings, string) {
	t.Helper()
	b, v, err := r.svc.PodBindings(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil {
		return nil, v
	}
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := pki.Seal(priv.PublicKey().Bytes(), id, b, v)
	if err != nil {
		t.Fatal(err)
	}
	hp, err := pki.HPKEKEM().NewPrivateKey(priv.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	opened, err := pki.Open(hp, id, sealed)
	if err != nil {
		t.Fatal(err)
	}
	return opened, sealed.GetVersion()
}

func TestHTTPPodBindingsSealed(t *testing.T) {
	r := newRig(t)
	r.putHTTP(setBody(allKinds(t)...))
	b, v := openSealed(t, r, r.sb1)
	if b == nil || len(b.GetHttp()) != 4 || len(b.GetKube()) != 0 {
		t.Fatalf("bindings: %v", b)
	}
	byName := map[string]*fleetv1.HttpBinding{}
	for _, h := range b.GetHttp() {
		byName[h.GetName()] = h
	}
	erp := byName["erp"]
	if erp.GetUpstream() != "https://api.erp.example.test" || erp.GetMode() != fleetv1.HttpBindingMode_HTTP_BINDING_MODE_NAME ||
		!erp.GetForwardedHost() || !slices.Equal(erp.GetPathPrefixes(), []string{"/odata/"}) ||
		erp.GetRateLimit().GetPerSecond() != 10 || erp.GetRateLimit().GetBurst() != 10 ||
		erp.GetHeaders().GetHeaders()["X-Api-Key"] != httpAPIKey {
		t.Fatalf("erp: %v", erp)
	}
	if bs := byName["basic"].GetBasic(); bs.GetUser() != "fecode" || bs.GetPassword() != httpPassword {
		t.Fatalf("basic: %v", bs)
	}
	g := byName["graph"]
	if g.GetMode() != fleetv1.HttpBindingMode_HTTP_BINDING_MODE_INTERCEPT || g.GetOauth2().GetClientSecret() != httpClientSecret ||
		!g.GetOauth2().GetClientAuthInBody() || g.GetOauth2().GetScope() != "read" {
		t.Fatalf("graph: %v", g)
	}
	if c := byName["mtls"].GetClientCertificate(); !strings.Contains(c.GetKeyPem(), "PRIVATE KEY") {
		t.Fatalf("mtls: %v", c)
	}
	if len(b.GetInterceptCas()) != 1 || !strings.Contains(b.GetInterceptCas()[0].GetKeyPem(), "PRIVATE KEY") {
		t.Fatalf("intercept CAs: %v", b.GetInterceptCas())
	}

	// The clear part names what the pod resolves, carries the CA's
	// certificate and the version sealed.
	row, err := r.st.GetSandbox(bg, r.sb1)
	if err != nil {
		t.Fatal(err)
	}
	part, err := r.svc.PodHTTP(bg, row)
	if err != nil {
		t.Fatal(err)
	}
	if part.GetVersion() != v || !slices.Equal(part.GetNames(), []string{"basic", "erp", "mtls"}) ||
		!slices.Equal(part.GetInterceptHosts(), []string{"api.erp.example.test"}) ||
		len(part.GetInterceptCaPems()) != 1 || part.GetInterceptCaPems()[0] != b.GetInterceptCas()[0].GetCertPem() {
		t.Fatalf("clear part %v, sealed version %s", part, v)
	}
	for _, pem := range part.GetInterceptCaPems() {
		if strings.Contains(pem, "PRIVATE") {
			t.Fatal("the clear part carries a key")
		}
	}

	// A Kubernetes binding moves the pod's version, and the clear part's
	// with it.
	r.putKube(setBody(tokenBinding("prod", 6443, "https://k8s.example.test")))
	_, v2 := openSealed(t, r, r.sb1)
	part2, err := r.svc.PodHTTP(bg, row)
	if err != nil {
		t.Fatal(err)
	}
	if v2 == v || part2.GetVersion() != v2 {
		t.Fatalf("after a kube PUT: sealed %s (was %s), clear part %s", v2, v, part2.GetVersion())
	}

	// Without HTTP bindings the pod has no clear part, and its version is
	// that of its other sets alone.
	r.sandboxCall(http.MethodDelete, "/http", "")
	_, v3 := openSealed(t, r, r.sb1)
	if part3, err := r.svc.PodHTTP(bg, row); err != nil || part3 != nil {
		t.Fatalf("clear part without HTTP bindings: %v %v", part3, err)
	}
	kv, _ := r.kubeRows()
	if v3 != podVersion(kv, "", nil) {
		t.Fatalf("version without HTTP bindings %s", v3)
	}
	// Another sandbox has nothing.
	if b, v, err := r.svc.PodBindings(bg, r.sb2); err != nil || b != nil || v != "" {
		t.Fatalf("sb2: %v %q %v", b, v, err)
	}
}

// interceptBinding is an intercept binding of host.
func interceptBinding(name, host string) map[string]any {
	return map[string]any{"name": name, "upstream": "https://" + host, "mode": "intercept",
		"credential": map[string]any{"kind": "headers", "headers": map[string]any{"Authorization": "Bearer " + httpAPIKey}}}
}

// leafOf issues a leaf for name from an interception CA, as feos-authn
// does.
func leafOf(t *testing.T, ca *fleetv1.InterceptCa, name string) *x509.Certificate {
	t.Helper()
	pair, err := tls.X509KeyPair([]byte(ca.GetCertPem()), []byte(ca.GetKeyPem()))
	if err != nil {
		t.Fatalf("the CA's key does not match its certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

func verifies(leaf *x509.Certificate, caPEMs []string, name string) error {
	pool := x509.NewCertPool()
	for _, p := range caPEMs {
		pool.AppendCertsFromPEM([]byte(p))
	}
	_, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: name})
	return err
}

func TestHTTPInterceptCA(t *testing.T) {
	r := newRig(t)
	r.putHTTP(setBody(interceptBinding("erp", "api.erp.example.test")))
	b, _ := openSealed(t, r, r.sb1)
	if len(b.GetInterceptCas()) != 1 {
		t.Fatalf("CAs: %d", len(b.GetInterceptCas()))
	}
	ca := b.GetInterceptCas()[0]
	blk, _ := pem.Decode([]byte(ca.GetCertPem()))
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Fatalf("the CA's key is %T, not ECDSA P-256", cert.PublicKey)
	}
	if !cert.IsCA || !cert.MaxPathLenZero || cert.MaxPathLen != 0 || !cert.PermittedDNSDomainsCritical ||
		!slices.Equal(cert.PermittedDNSDomains, []string{"api.erp.example.test"}) || len(cert.ExcludedIPRanges) != 2 ||
		cert.CheckSignatureFrom(cert) != nil || cert.NotAfter.Before(time.Now().Add(365*24*time.Hour)) {
		t.Fatalf("the CA: %+v", cert)
	}
	if blk, _ := pem.Decode([]byte(ca.GetKeyPem())); blk == nil || blk.Type != "PRIVATE KEY" {
		t.Fatal("the CA's key is not PKCS#8 PEM")
	}
	if _, err := x509.ParsePKCS8PrivateKey(mustPEM(t, ca.GetKeyPem())); err != nil {
		t.Fatal(err)
	}
	// Its name constraints: a leaf for the intercepted name verifies, one
	// for another name does not.
	if err := verifies(leafOf(t, ca, "api.erp.example.test"), []string{ca.GetCertPem()}, "api.erp.example.test"); err != nil {
		t.Fatalf("a leaf for the intercepted name: %v", err)
	}
	if err := verifies(leafOf(t, ca, "bank.example.test"), []string{ca.GetCertPem()}, "bank.example.test"); err == nil {
		t.Fatal("a leaf for another name verifies")
	}

	// The same names keep the CA; other names add a new one beside it,
	// which the newest of them issues for.
	r.putHTTP(setBody(interceptBinding("erp", "api.erp.example.test"), headersBinding("other", "https://pay.example.test")))
	if b, _ := openSealed(t, r, r.sb1); len(b.GetInterceptCas()) != 1 || b.GetInterceptCas()[0].GetCertPem() != ca.GetCertPem() {
		t.Fatal("the same intercepted names made a new CA")
	}
	r.putHTTP(setBody(interceptBinding("erp", "api.erp.example.test"), interceptBinding("pay", "pay.example.test")))
	b2, _ := openSealed(t, r, r.sb1)
	if len(b2.GetInterceptCas()) != 2 || b2.GetInterceptCas()[0].GetCertPem() != ca.GetCertPem() {
		t.Fatalf("CAs after a new name: %d", len(b2.GetInterceptCas()))
	}
	newest := b2.GetInterceptCas()[1]
	if err := verifies(leafOf(t, newest, "pay.example.test"), []string{newest.GetCertPem()}, "pay.example.test"); err != nil {
		t.Fatalf("the newest CA for the new name: %v", err)
	}
	if err := verifies(leafOf(t, ca, "pay.example.test"), []string{ca.GetCertPem()}, "pay.example.test"); err == nil {
		t.Fatal("the old CA issues for the new name")
	}
	row, _ := r.st.GetSandbox(bg, r.sb1)
	part, err := r.svc.PodHTTP(bg, row)
	if err != nil {
		t.Fatal(err)
	}
	if len(part.GetInterceptCaPems()) != 2 || len(part.GetNames()) != 0 ||
		!slices.Equal(part.GetInterceptHosts(), []string{"api.erp.example.test", "pay.example.test"}) {
		t.Fatalf("clear part: %v", part)
	}
	// Sealed at rest: no CA key in the rows.
	var n int
	if err := r.d.Pool.QueryRow(bg, `SELECT count(*) FROM pod_intercept_cas WHERE sandbox_id = $1
		AND encode(sealed_key, 'escape') LIKE '%PRIVATE%'`, r.sb1).Scan(&n); err != nil || n != 0 {
		t.Fatalf("CA keys in the clear: %d %v", n, err)
	}

	// Without intercept bindings the pod gets no CA, in neither part.
	r.putHTTP(setBody(headersBinding("other", "https://pay.example.test")))
	if b, _ := openSealed(t, r, r.sb1); len(b.GetInterceptCas()) != 0 {
		t.Fatal("CAs without intercept bindings")
	}
	if part, _ := r.svc.PodHTTP(bg, row); len(part.GetInterceptCaPems()) != 0 || len(part.GetInterceptHosts()) != 0 {
		t.Fatalf("clear part without intercept bindings: %v", part)
	}
}

func mustPEM(t *testing.T, s string) []byte {
	t.Helper()
	blk, _ := pem.Decode([]byte(s))
	if blk == nil {
		t.Fatal("no PEM")
	}
	return blk.Bytes
}

func TestInterceptCAsBounded(t *testing.T) {
	r := newRig(t)
	for i := range maxCAs + 3 {
		hosts := []string{"h" + string(rune('a'+i)) + ".example.test"}
		if _, err := r.svc.ensureCAs(bg, r.st, r.sb1, hosts); err != nil {
			t.Fatal(err)
		}
	}
	cas, err := r.st.InterceptCAs(bg, r.sb1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cas) != maxCAs || cas[len(cas)-1].Seq != maxCAs+3 || cas[len(cas)-1].Hosts[0] != "hk.example.test" {
		t.Fatalf("CAs: %d, newest %d", len(cas), cas[len(cas)-1].Seq)
	}
	// Asked again for the newest's names: no new CA.
	again, err := r.svc.ensureCAs(bg, r.st, r.sb1, []string{"hk.example.test"})
	if err != nil || len(again) != maxCAs {
		t.Fatalf("again: %d %v", len(again), err)
	}
	// A name more: a new CA. Then, by turns, a part of its names and all of
	// them: none.
	pair := []string{"hk.example.test", "hz.example.test"}
	if _, err := r.svc.ensureCAs(bg, r.st, r.sb1, pair); err != nil {
		t.Fatal(err)
	}
	for _, hosts := range [][]string{{"hz.example.test"}, pair, {"hk.example.test"}, pair} {
		cas, err := r.svc.ensureCAs(bg, r.st, r.sb1, hosts)
		if err != nil {
			t.Fatal(err)
		}
		if newest := cas[len(cas)-1]; newest.Seq != maxCAs+4 || !slices.Equal(newest.Hosts, pair) {
			t.Fatalf("%v: newest %d %v", hosts, newest.Seq, newest.Hosts)
		}
	}
}
