// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ferunauth

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/oidc/oidctest"
)

// The test's secrets: none of them may show in the logs.
const (
	testToken   = "feos-auth-t0ken-never-logged"
	testSession = "sess-7f3a9c-never-logged"
	testHost    = "stage--acme.apps.example.test"
	testPublic  = "https://auth.example.test"
)

// fakeFerund is ferund's API as ferun-auth calls it.
type fakeFerund struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// views are the challenges by their sealed text.
	views map[string]*challengeView
	// status, when not 0, is every answer's status, with code.
	status int
	code   string
	// acceptStatus, when not 0, answers the accepts (and rejects) with it
	// and acceptCode, and marks the challenge answered.
	acceptStatus int
	acceptCode   string
	endStatus    int
	providers    map[string]oidcProvider
	accepts      []acceptBody
	rejects      []string
	ends         []endBody
	calls        []string
	auth         []string
}

func newFakeFerund(t *testing.T) *fakeFerund {
	f := &fakeFerund{t: t, views: map[string]*challengeView{}, providers: map[string]oidcProvider{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFerund) fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "refused (fake)", "code": code})
}

func (f *fakeFerund) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.EscapedPath())
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	if f.status != 0 {
		f.fail(w, f.status, f.code)
		return
	}
	var in map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&in)
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(in[k], &s)
		return s
	}
	ok := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch p := r.Method + " " + r.URL.EscapedPath(); {
	case p == "POST /api/v1/challenges/open":
		v, found := f.views[str("challenge")]
		if !found {
			f.fail(w, 404, "not_found")
			return
		}
		ok(v)
	case p == "POST /api/v1/challenges/accept" || p == "POST /api/v1/challenges/reject":
		v, found := f.views[str("challenge")]
		if !found {
			f.fail(w, 404, "not_found")
			return
		}
		if f.acceptStatus != 0 {
			v.Answered = true
			f.fail(w, f.acceptStatus, f.acceptCode)
			return
		}
		if strings.HasSuffix(p, "accept") {
			var a acceptBody
			b, _ := json.Marshal(in)
			_ = json.Unmarshal(b, &a)
			f.accepts = append(f.accepts, a)
			ok(map[string]any{"redirect": "https://" + v.Host + "/.ferun/auth?code=accepted-code", "admitted": true, "visit": "v-1"})
			return
		}
		f.rejects = append(f.rejects, str("challenge"))
		ok(map[string]any{"redirect": "https://" + v.Host + "/.ferun/auth?code=rejected-code", "admitted": false})
	case strings.HasPrefix(p, "GET /api/v1/oidc-providers/"):
		pr, found := f.providers[strings.TrimPrefix(r.URL.Path, "/api/v1/oidc-providers/")]
		if !found {
			f.fail(w, 404, "not_found")
			return
		}
		ok(pr)
	case p == "POST /api/v1/visits/end":
		if f.endStatus != 0 {
			f.fail(w, f.endStatus, "")
			return
		}
		var e endBody
		b, _ := json.Marshal(in)
		_ = json.Unmarshal(b, &e)
		f.ends = append(f.ends, e)
		ok(map[string]int{"ended": 1})
	default:
		f.fail(w, 404, "")
	}
}

// view adds a challenge of kind kind under sealed.
func (f *fakeFerund) view(sealed string, kind string, o *oidcProvider) *challengeView {
	v := &challengeView{Host: testHost, Restart: "https://" + testHost + "/menu?x=1", OIDC: o}
	v.Environment.ID, v.Environment.Name, v.Environment.Tenant = "6f0c1d2e-0000-4000-8000-000000000001", "stage", "acme"
	v.Provider.Name, v.Provider.Kind = "sso", kind
	if kind == kindFerun {
		v.Provider.Name = "ferun"
	}
	f.mu.Lock()
	f.views[sealed] = v
	f.mu.Unlock()
	return v
}

func (f *fakeFerund) set(fn func(f *fakeFerund)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeFerund) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// clock is the tests' clock of the flows.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// syncBuffer is a log's buffer that the handler writes while tests read.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// harness is ferun-auth with a test issuer and a fake ferund.
type harness struct {
	t      *testing.T
	issuer *oidctest.Provider
	ferund *fakeFerund
	srv    *Server
	logs   *syncBuffer
	clock  *clock
	jar    map[string]string
}

type harnessOpt func(*Config)

// withFerunIssuer makes the test issuer the installation's, for kind
// ferun.
func withFerunIssuer(h *harness) harnessOpt {
	return func(c *Config) {
		c.Issuer, c.ClientID = h.issuer.Issuer, h.issuer.ClientID
		c.ClientSecretFile = writeFile(h.t, "client-secret", h.issuer.ClientSecret+"\n")
	}
}

// noAllow leaves loopback denied, as in production.
func noAllow(c *Config) { c.Allow = nil }

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func newHarness(t *testing.T, opts ...func(*harness) harnessOpt) *harness {
	t.Helper()
	h := &harness{t: t, issuer: oidctest.New(t, "RS256"), ferund: newFakeFerund(t), logs: &syncBuffer{},
		clock: &clock{t: time.Now()}, jar: map[string]string{}}
	roots := x509.NewCertPool()
	roots.AddCert(h.issuer.Srv.Certificate())
	cfg := Default()
	cfg.PublicURL = testPublic
	cfg.FerunURL = h.ferund.srv.URL
	cfg.TokenFile = writeFile(t, "token", testToken+"\n")
	cfg.KeyFile = writeFile(t, "key", "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f\n")
	cfg.Allow = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	cfg.roots = roots
	cfg.now = h.clock.now
	for _, o := range opts {
		o(h)(&cfg)
	}
	log := slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv, err := New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	return h
}

// opt adapts a plain option.
func opt(o harnessOpt) func(*harness) harnessOpt { return func(*harness) harnessOpt { return o } }

// navigation are a browser's headers of a top-level navigation.
var navigation = map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Sec-Fetch-Site": "cross-site",
	"Accept": "text/html"}

// do sends a request with the jar's cookies and keeps what it sets.
func (h *harness) do(method, target string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, testPublic+target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, testPublic+target, nil)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for k, v := range h.jar {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(h.jar, c.Name)
		} else {
			h.jar[c.Name] = c.Value
		}
	}
	return rec
}

// visit is the browser's GET /visit with the challenge.
func (h *harness) visit(sealed string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do("GET", "/visit?challenge="+url.QueryEscape(sealed), navigation, "")
}

// toIssuer checks a /visit's answer, the 303 to the issuer, and returns
// its Location and state.
func (h *harness) toIssuer(rec *httptest.ResponseRecorder) (string, string) {
	h.t.Helper()
	if rec.Code != http.StatusSeeOther {
		h.t.Fatalf("/visit: %d %s", rec.Code, rec.Body)
	}
	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil || u.Scheme+"://"+u.Host+u.Path != h.issuer.Srv.URL+"/oauth2/auth" {
		h.t.Fatalf("Location %q", loc)
	}
	return loc, u.Query().Get("state")
}

// callback is the browser's GET /callback after the issuer signed it in.
func (h *harness) callback(loc, state string, claims map[string]any) *httptest.ResponseRecorder {
	h.t.Helper()
	code := h.issuer.Issue(loc, claims)
	return h.do("GET", "/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), navigation, "")
}

// wantPage checks a page's status and text and its headers.
func wantPage(t *testing.T, rec *httptest.ResponseRecorder, status int, msg string) {
	t.Helper()
	if rec.Code != status || !strings.Contains(rec.Body.String(), msg) {
		t.Fatalf("got %d %q, want %d %q", rec.Code, rec.Body, status, msg)
	}
	h := rec.Header()
	if h.Get("Content-Security-Policy") != pageCSP || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Cache-Control") != "no-store" || !strings.HasPrefix(h.Get("Content-Type"), "text/html") {
		t.Fatalf("page headers %v", h)
	}
}

// claims are a visitor's claims at the test issuer.
func claims() map[string]any {
	c := oidctest.UserClaims()
	c["email"] = "bob@example.test"
	c["sid"] = testSession
	return c
}

// oidcOf is a provider of kind oidc at the test issuer with r.
func (h *harness) oidcOf(r rule) *oidcProvider {
	return &oidcProvider{Issuer: h.issuer.Issuer, ClientID: h.issuer.ClientID, ClientSecret: h.issuer.ClientSecret, Rule: r}
}
