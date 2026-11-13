// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/visit"
)

// The visitors' scenario (ADR 0054): an environment that admits the
// visitors of a provider of kind tenant, the tenant's own service, here
// an HTTPS server of the test's. ferund calls that service at a
// renewal through its ruled client, which trusts the system's roots
// only; TestMain makes the test's CA one of them (SSL_CERT_FILE, read
// once per process, before any test), so that the service's certificate
// verifies as a public one would.

// providerCA signs the certificates of the tenants' services.
var providerCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ferun-e2e-roots-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := makeProviderCA(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// makeProviderCA makes providerCA and points SSL_CERT_FILE at a bundle
// of the system's roots and it.
func makeProviderCA(dir string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ferun e2e providers"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	if providerCA.cert, err = x509.ParseCertificate(der); err != nil {
		return err
	}
	providerCA.key = key
	bundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for _, f := range []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt"} {
		if b, err := os.ReadFile(f); err == nil {
			bundle = append(append(b, '\n'), bundle...)
			break
		}
	}
	file := filepath.Join(dir, "roots.pem")
	if err := os.WriteFile(file, bundle, 0o600); err != nil {
		return err
	}
	return os.Setenv("SSL_CERT_FILE", file)
}

// providerCert is a certificate of providerCA for 127.0.0.1.
func providerCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:   time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, providerCA.cert, &key.PublicKey, providerCA.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// tenantService is a provider of kind tenant, as FeCode is one: GET
// /visit?challenge=… is the browser sent to sign in, which it opens and
// accepts at ferund with the tenant's token and sends on to ferund's
// redirect; POST /visit {"visit"} is ferund asking at a renewal, which it
// answers with admit.
type tenantService struct {
	c   *cluster
	srv *httptest.Server
	url string
	// endsIn is the end the accepts give their visits.
	endsIn time.Duration

	mu       sync.Mutex
	admit    bool
	signIns  int
	visits   []string // the visits it accepted
	asks     []string // the visits ferund asked about
	askAuth  []string // the asks' Authorization headers
	failures []string
}

func (c *cluster) runTenantService(endsIn time.Duration) *tenantService {
	c.t.Helper()
	s := &tenantService{c: c, endsIn: endsIn, admit: true}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.srv.TLS = &tls.Config{Certificates: []tls.Certificate{providerCert(c.t)}}
	s.srv.StartTLS()
	c.t.Cleanup(s.srv.Close)
	s.url = s.srv.URL + "/visit"
	return s
}

func (s *tenantService) fail(w http.ResponseWriter, format string, args ...any) {
	s.mu.Lock()
	s.failures = append(s.failures, fmt.Sprintf(format, args...))
	s.mu.Unlock()
	http.Error(w, "the test's service failed", http.StatusInternalServerError)
}

func (s *tenantService) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/visit" {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPost {
		var in struct {
			Visit string `json:"visit"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in)
		s.mu.Lock()
		s.asks = append(s.asks, in.Visit)
		s.askAuth = append(s.askAuth, r.Header.Get("Authorization"))
		admit := s.admit
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"admitted": %t}`, admit)
		return
	}
	sealed := r.URL.Query().Get("challenge")
	s.mu.Lock()
	s.signIns++
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"challenge": sealed})
	op, err := s.c.callErr(ctx, nil, "POST", "/api/v1/challenges/open", tenantAuth, string(body))
	var ch struct {
		Environment struct {
			ID     string `json:"id"`
			Tenant string `json:"tenant"`
		} `json:"environment"`
		Host     string `json:"host"`
		Path     string `json:"path"`
		Expired  bool   `json:"expired"`
		Answered bool   `json:"answered"`
		Provider struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"provider"`
	}
	if err != nil || op.code != http.StatusOK || json.Unmarshal([]byte(op.body), &ch) != nil {
		s.fail(w, "open: %d %s (%v)", op.code, op.body, err)
		return
	}
	if ch.Provider.Name != "members" || ch.Provider.Kind != model.ProviderKindTenant || ch.Environment.Tenant != tenant ||
		ch.Expired || ch.Answered {
		s.fail(w, "the challenge opened as %s", op.body)
		return
	}
	body, _ = json.Marshal(map[string]string{"challenge": sealed, "subject": "u-ada", "email": "ada@example.test",
		"name": "Ada", "session": "s-1", "endsAt": time.Now().Add(s.endsIn).UTC().Format(time.RFC3339)})
	acc, err := s.c.callErr(ctx, nil, "POST", "/api/v1/challenges/accept", tenantAuth, string(body))
	var ans struct {
		Redirect string `json:"redirect"`
		Admitted bool   `json:"admitted"`
		Visit    string `json:"visit"`
	}
	if err != nil || acc.code != http.StatusOK || json.Unmarshal([]byte(acc.body), &ans) != nil || !ans.Admitted ||
		!strings.HasPrefix(ans.Redirect, "https://"+ch.Host+"/.ferun/auth?code=") {
		s.fail(w, "accept: %d %s (%v)", acc.code, acc.body, err)
		return
	}
	s.mu.Lock()
	s.visits = append(s.visits, ans.Visit)
	s.mu.Unlock()
	http.Redirect(w, r, ans.Redirect, http.StatusSeeOther)
}

func (s *tenantService) setAdmit(admit bool) {
	s.mu.Lock()
	s.admit = admit
	s.mu.Unlock()
}

// state is what the service saw: its sign-ins, its visits, ferund's asks
// with their Authorization headers, and its failures.
func (s *tenantService) state() (signIns int, visits, asks, askAuth, failures []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.signIns, append([]string(nil), s.visits...), append([]string(nil), s.asks...),
		append([]string(nil), s.askAuth...), append([]string(nil), s.failures...)
}

// browser is a browser of the test's: a cookie jar, the apps zone's hosts
// through the ingress and every other host as it is, the test's wildcard
// and the services' CA trusted, the Fetch Metadata of a navigation on
// every request it follows.
type browser struct {
	t      *testing.T
	jar    *cookiejar.Jar
	follow *http.Client
	once   *http.Client
}

func (p *ingressProc) browser() *browser {
	p.c.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		p.c.t.Fatal(err)
	}
	roots := p.tr.TLSClientConfig.RootCAs.Clone()
	roots.AddCert(providerCA.cert)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			if host, _, _ := net.SplitHostPort(addr); strings.HasSuffix(host, "."+appsZone) {
				return d.DialContext(ctx, network, p.program().Addrs().HTTPS)
			}
			return d.DialContext(ctx, network, addr)
		},
	}
	p.c.t.Cleanup(tr.CloseIdleConnections)
	follow := &http.Client{Transport: tr, Jar: jar, Timeout: time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("%d redirects", len(via))
			}
			site := "cross-site"
			if req.URL.Host == via[len(via)-1].URL.Host {
				site = "same-origin"
			}
			req.Header.Set("Sec-Fetch-Site", site)
			return nil
		}}
	once := &http.Client{Transport: tr, Jar: jar, Timeout: time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &browser{t: p.c.t, jar: jar, follow: follow, once: once}
}

// page is an answer the browser got, and where.
type page struct {
	reply
	at string
}

// do sends a request with the browser's headers of kind (navigate: a
// top-level navigation typed into the address bar; fetch: a script's of
// the same origin), following redirects or not.
func (b *browser) do(method, u, kind string, follow bool, hdr map[string]string) page {
	b.t.Helper()
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	switch kind {
	case "navigate":
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Site", "none")
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
	case "fetch":
		req.Header.Set("Sec-Fetch-Mode", "cors")
		req.Header.Set("Sec-Fetch-Dest", "empty")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Accept", "*/*")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c := b.once
	if follow {
		c = b.follow
	}
	resp, err := c.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, u, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return page{reply{code: resp.StatusCode, body: string(body), header: resp.Header}, resp.Request.URL.String()}
}

// cookie is the browser's cookie of that name for host; "" for none.
func (b *browser) cookie(host, name string) string {
	for _, c := range b.jar.Cookies(&url.URL{Scheme: "https", Host: host, Path: "/"}) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// wantApp checks that a page is the app's answer to the visitor: its
// visitor headers, and none of the ingress's cookies.
func wantApp(t *testing.T, what string, p page, host string) {
	t.Helper()
	if p.code != http.StatusOK || podOf(p.body) == "" || p.at != "https://"+host+"/" ||
		!strings.Contains(p.body, " visitor=u-ada email=ada@example.test token=true ") ||
		strings.Contains(p.body, "__Host-ferun") {
		t.Fatalf("%s: %d at %s: %s", what, p.code, p.at, p.body)
	}
}

// Scenario: the whole visit chain of a provider of kind tenant. The
// tenant makes the provider (its service's address) and publishes an
// environment to its visitors. A navigation without a visit goes to the
// service with a challenge the ingress sealed; the service opens and
// accepts it at ferund with the tenant's token and sends the browser to
// the host's /.ferun/auth, where the ingress redeems the code for the
// visit cookie and sends it on to the page: the app gets the visitor's
// headers and none of the ingress's cookies. A request that is no
// navigation without a visit gets 401. A renewal past the 8 minutes since
// the last asking asks the service, without a credential: its yes renews,
// its no ends the visit, and the next navigation goes to the service
// again. Signing out ends the visit.
func TestVisitorsOfATenantService(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "secret.key")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newCluster(t, ingressCluster(func(cfg *config.Config) {
		cfg.SecretKeyFile = keyFile
		// The tenant's service is on loopback: no block but TEST-NET-1
		// is denied, which ferund's rule then judges as any.
		cfg.DenyPrefixes = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	}))
	c.start("a")
	n1 := c.join("n1", meshNode)
	c.serveApps(n1)
	p := c.runIngress("ferun-ingress-0")
	p.waitReady(60 * time.Second)
	envID := c.mkApp("web", 1, "")

	// Visits end 4 minutes after the accept, so that every token has less
	// than visit.RenewBefore left and every request renews it in line.
	svc := c.runTenantService(4 * time.Minute)
	b, _ := json.Marshal(map[string]string{"kind": model.ProviderKindTenant, "url": svc.url})
	if r := c.call(nil, "PUT", "/api/v1/providers/members", tenantAuth, string(b)); r.code != http.StatusCreated {
		t.Fatalf("the provider: %d %s", r.code, r.body)
	}
	r := c.call(nil, "PUT", "/api/v1/environments/"+envID.String()+"/public", tenantAuth,
		`{"public":true,"access":"visitors","provider":"members"}`)
	var ans struct {
		Public   bool     `json:"public"`
		Hosts    []string `json:"hosts"`
		Access   string   `json:"access"`
		Provider string   `json:"provider"`
	}
	if err := json.Unmarshal([]byte(r.body), &ans); r.code != http.StatusOK || err != nil || !ans.Public || len(ans.Hosts) != 1 ||
		ans.Access != model.AccessVisitors || ans.Provider != "members" {
		t.Fatalf("publish: %d %s (%v)", r.code, r.body, err)
	}
	host := ans.Hosts[0]
	c.waitWeb(envID, 1, "the service rolled to name its ingress groups", namesIngGroup)
	home := "https://" + host + "/"

	// Without a visit: a fetch is 401, and nothing reaches the app.
	br := p.browser()
	eventually(t, 60*time.Second, "the ingress guarding the host", func() bool {
		pg := br.do("GET", home, "fetch", false, nil)
		return pg.code == http.StatusUnauthorized
	})
	pg := br.do("GET", home, "fetch", false, nil)
	if pg.header.Get("WWW-Authenticate") != `FeRun-Visit realm="`+host+`"` || pg.header.Get("Cache-Control") != "no-store" ||
		strings.TrimSpace(pg.body) != "this site needs a visit: open it in a browser" {
		t.Fatalf("a fetch without a visit: %d %v %s", pg.code, pg.header, pg.body)
	}
	// A navigation without one goes to the service with a challenge.
	pg = br.do("GET", home, "navigate", false, nil)
	if loc := pg.header.Get("Location"); pg.code != http.StatusSeeOther || !strings.HasPrefix(loc, svc.url+"?challenge=") ||
		br.cookie(host, visit.CookieChallenge) == "" {
		t.Fatalf("a navigation without a visit: %d %v", pg.code, pg.header)
	}

	// The sign-in, through the service and back.
	pg = br.do("GET", home, "navigate", true, nil)
	wantApp(t, "after the sign-in", pg, host)
	signIns, visits, _, _, failures := svc.state()
	if signIns != 1 || len(visits) != 1 || len(failures) != 0 || br.cookie(host, visit.CookieVisit) == "" {
		t.Fatalf("sign-ins %d, visits %v, failures %v, cookie %q", signIns, visits, failures, br.cookie(host, visit.CookieVisit))
	}
	vid := visits[0]
	if n := c.count(`SELECT count(*) FROM visits WHERE id = $1 AND env_id = $2 AND provider = 'members' AND subject = 'u-ada'
		AND host = $3 AND binding <> '' AND ended_at IS NULL`, vid, envID, host); n != 1 {
		t.Fatalf("the visit's row: %d", n)
	}
	// The visit's fetch reaches the app too, the token renewed in line.
	wantApp(t, "a fetch of the visit", br.do("GET", home, "fetch", false, nil), host)
	if _, _, asks, _, _ := svc.state(); len(asks) != 0 {
		t.Fatalf("asked within 8 minutes of the accept: %v", asks)
	}

	// The last asking 9 minutes ago: the next renewal asks, a yes
	// renews.
	c.sql(`UPDATE visits SET asked_at = now() - interval '9 minutes' WHERE id = $1`, vid)
	wantApp(t, "after a yes", br.do("GET", home, "navigate", true, nil), host)
	_, _, asks, askAuth, _ := svc.state()
	if len(asks) != 1 || asks[0] != vid || askAuth[0] != "" {
		t.Fatalf("asks %v, their Authorization %q", asks, askAuth)
	}
	if n := c.count(`SELECT count(*) FROM visits WHERE id = $1 AND asked_at > now() - interval '1 minute'`, vid); n != 1 {
		t.Fatal("the ask was not recorded")
	}

	// A no ends the visit, and the navigation goes to the service again.
	svc.setAdmit(false)
	c.sql(`UPDATE visits SET asked_at = now() - interval '9 minutes' WHERE id = $1`, vid)
	pg = br.do("GET", home, "navigate", false, nil)
	if loc := pg.header.Get("Location"); pg.code != http.StatusSeeOther || !strings.HasPrefix(loc, svc.url+"?challenge=") {
		t.Fatalf("after the service's no: %d %v %s", pg.code, pg.header, pg.body)
	}
	if br.cookie(host, visit.CookieVisit) != "" {
		t.Fatal("the ended visit's cookie was not cleared")
	}
	if n := c.count(`SELECT count(*) FROM visits WHERE id = $1 AND end_reason = $2`, vid, model.VisitProviderNo); n != 1 {
		t.Fatal("the visit did not end on the service's no")
	}
	if _, _, asks, _, _ := svc.state(); len(asks) != 2 {
		t.Fatalf("asks %v", asks)
	}

	// A new visit, and its sign-out: the visit ends, its cookie goes, and
	// the next navigation goes to the service again.
	svc.setAdmit(true)
	wantApp(t, "signed in again", br.do("GET", home, "navigate", true, nil), host)
	_, visits, _, _, _ = svc.state()
	if len(visits) != 2 || visits[1] == vid {
		t.Fatalf("visits %v", visits)
	}
	vid = visits[1]
	pg = br.do("POST", "https://"+host+"/.ferun/sign-out", "", false,
		map[string]string{"Origin": "https://" + host, "Sec-Fetch-Site": "same-origin"})
	if pg.code != http.StatusOK || !strings.Contains(pg.body, "You are signed out of "+host) || br.cookie(host, visit.CookieVisit) != "" {
		t.Fatalf("sign-out: %d %s; cookie %q", pg.code, pg.body, br.cookie(host, visit.CookieVisit))
	}
	if n := c.count(`SELECT count(*) FROM visits WHERE id = $1 AND end_reason = $2`, vid, model.VisitSignedOut); n != 1 {
		t.Fatal("the sign-out did not end the visit")
	}
	pg = br.do("GET", home, "navigate", false, nil)
	if loc := pg.header.Get("Location"); pg.code != http.StatusSeeOther || !strings.HasPrefix(loc, svc.url+"?challenge=") {
		t.Fatalf("after the sign-out: %d %v", pg.code, pg.header)
	}
	if _, _, _, _, failures := svc.state(); len(failures) != 0 {
		t.Fatalf("the service failed: %v", failures)
	}
}
