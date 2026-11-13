// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/visit"
)

const (
	testSignIn   = "https://fecode.example/api/v1/visit"
	testProvider = "fecode"
	testVisit    = "0f2b7c9e-4f1d-4c0e-9d7a-5b6c7d8e9f01"
)

// newKeys makes signing keys of visits, as ferund holds them.
func newKeys(t *testing.T) *visit.Keys {
	t.Helper()
	seed, key := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(seed)
	_, _ = rand.Read(key)
	k, err := visit.NewKeys(seed, key)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func wireKeys(k *visit.Keys) *wire.VisitKeys {
	return &wire.VisitKeys{TokenKID: k.TokenKID(), TokenKey: k.TokenKey(), ChallengeKID: k.ChallengeKID(),
		ChallengeKey: k.ChallengeKey()}
}

// visitState is testState with the environment guarded: visitors of
// testProvider, signing in at signIn, with the keys k in the table.
func visitState(k *visit.Keys, signIn string, services ...wire.Service) *wire.State {
	s := testState(services...)
	e := s.Envs[testEnv]
	e.Access, e.Provider, e.SignIn = wire.AccessVisitors, testProvider, signIn
	s.Envs[testEnv] = e
	s.VisitKeys = wireKeys(k)
	return s
}

func newSecret(t *testing.T) string {
	t.Helper()
	s, err := visit.NewBindingSecret()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// mint signs a visit's token for secret as ferund would, valid for 15
// minutes of a 12-hour visit, changed by f.
func mint(t *testing.T, k *visit.Keys, secret string, f func(*visit.Token)) string {
	t.Helper()
	now := time.Now()
	tok := visit.Token{Subject: "u-1", Email: "a@example.com", Env: testEnv, Visit: testVisit, Host: testHost,
		Provider: testProvider, Binding: visit.BindingHash(secret), IssuedAt: now.Unix(),
		Expires: now.Add(visit.TokenLifetime).Unix(), VisitEnds: now.Add(visit.MaxVisit).Unix()}
	if f != nil {
		f(&tok)
	}
	raw, err := k.SignToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// fakeVisits is the control plane's side of visits, scriptable: it signs
// with keys, records the calls, and fails, waits or ends as told.
type fakeVisits struct {
	keys *visit.Keys

	mu         sync.Mutex
	redeems    []wire.RedeemVisitRequest
	renewals   []wire.RenewVisitRequest
	ends       []wire.EndVisitRequest
	redeemErr  error
	renewErr   error
	endErr     error
	refuse     string
	ended      map[string]string
	gate       chan struct{}
	renewToken func(visit.Token) visit.Token
}

func newFakeVisits(k *visit.Keys) *fakeVisits {
	return &fakeVisits{keys: k, ended: map[string]string{}}
}

func (f *fakeVisits) wait(ctx context.Context) error {
	f.mu.Lock()
	g := f.gate
	f.mu.Unlock()
	if g == nil {
		return nil
	}
	select {
	case <-g:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

func (f *fakeVisits) RedeemVisit(ctx context.Context, req wire.RedeemVisitRequest) (wire.RedeemVisitResponse, error) {
	f.mu.Lock()
	f.redeems = append(f.redeems, req)
	err, refuse := f.redeemErr, f.refuse
	f.mu.Unlock()
	if werr := f.wait(ctx); werr != nil {
		return wire.RedeemVisitResponse{}, werr
	}
	if err != nil {
		return wire.RedeemVisitResponse{}, err
	}
	if refuse != "" {
		return wire.RedeemVisitResponse{Outcome: wire.OutcomeRefused, Reason: refuse}, nil
	}
	c, err := f.keys.VerifyCode(req.Code)
	if err != nil || visit.BindingHash(req.Binding) != c.Binding || c.Host != req.Host {
		return wire.RedeemVisitResponse{Outcome: wire.OutcomeRefused, Reason: "unknown"}, nil
	}
	now := time.Now()
	tok, err := f.keys.SignToken(visit.Token{Subject: "u-1", Email: "a@example.com", Env: c.Env, Visit: c.Visit,
		Host: c.Host, Provider: testProvider, Binding: req.VisitBinding, IssuedAt: now.Unix(),
		Expires: now.Add(visit.TokenLifetime).Unix(), VisitEnds: now.Add(visit.MaxVisit).Unix()})
	if err != nil {
		return wire.RedeemVisitResponse{}, err
	}
	return wire.RedeemVisitResponse{Outcome: wire.OutcomeAdmitted, Token: tok}, nil
}

func (f *fakeVisits) RenewVisit(ctx context.Context, req wire.RenewVisitRequest) (wire.RenewVisitResponse, error) {
	f.mu.Lock()
	f.renewals = append(f.renewals, req)
	err := f.renewErr
	f.mu.Unlock()
	if werr := f.wait(ctx); werr != nil {
		return wire.RenewVisitResponse{}, werr
	}
	if err != nil {
		return wire.RenewVisitResponse{}, err
	}
	t, err := f.keys.VerifyToken(req.Token)
	if err != nil {
		return wire.RenewVisitResponse{}, status.Error(codes.InvalidArgument, "bad token")
	}
	if t.Binding != visit.BindingHash(req.Secret) {
		return wire.RenewVisitResponse{Outcome: wire.OutcomeEnded, Reason: "unknown"}, nil
	}
	f.mu.Lock()
	reason, ended := f.ended[t.Visit]
	change := f.renewToken
	f.mu.Unlock()
	if ended {
		return wire.RenewVisitResponse{Outcome: wire.OutcomeEnded, Reason: reason}, nil
	}
	now := time.Now()
	t.IssuedAt, t.Expires = now.Unix(), now.Add(visit.TokenLifetime).Unix()
	if change != nil {
		t = change(t)
	}
	raw, err := f.keys.SignToken(t)
	if err != nil {
		return wire.RenewVisitResponse{}, err
	}
	return wire.RenewVisitResponse{Outcome: wire.OutcomeRenewed, Token: raw}, nil
}

func (f *fakeVisits) EndVisit(ctx context.Context, req wire.EndVisitRequest) (wire.EndVisitResponse, error) {
	f.mu.Lock()
	f.ends = append(f.ends, req)
	err := f.endErr
	f.mu.Unlock()
	return wire.EndVisitResponse{}, err
}

func (f *fakeVisits) counts() (redeems, renewals, ends int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.redeems), len(f.renewals), len(f.ends)
}

func (f *fakeVisits) set(fn func(f *fakeVisits)) {
	f.mu.Lock()
	fn(f)
	f.mu.Unlock()
}

// call is one request straight through the HTTPS handler.
type call struct {
	method, path string
	header       map[string]string
	cookies      map[string]string
	remote       string
}

// nav is a browser's navigation to path.
func nav(path string) call {
	return call{method: "GET", path: path, header: map[string]string{
		"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Sec-Fetch-Site": "none",
		"Accept": "text/html,application/xhtml+xml"}}
}

// fetch is a script's request of path.
func fetch(path string) call {
	return call{method: "GET", path: path, header: map[string]string{"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Site": "same-origin", "Accept": "*/*"}}
}

func (c call) with(cookie, value string) call {
	m := map[string]string{}
	for k, v := range c.cookies {
		m[k] = v
	}
	m[cookie] = value
	c.cookies = m
	return c
}

func (c call) header2(k, v string) call {
	m := map[string]string{}
	for k, v := range c.header {
		m[k] = v
	}
	m[k] = v
	c.header = m
	return c
}

func serveCall(p *Proxy, c call) *httptest.ResponseRecorder {
	req := httptest.NewRequest(c.method, "http://"+testHost+c.path, nil)
	req.RemoteAddr = "192.0.2.7:4711"
	if c.remote != "" {
		req.RemoteAddr = c.remote
	}
	for k, v := range c.header {
		req.Header.Set(k, v)
	}
	for k, v := range c.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	rec := httptest.NewRecorder()
	p.ServeHTTPS().ServeHTTP(rec, req)
	return rec
}

// setCookies returns the Set-Cookie headers of an answer by cookie name.
func setCookies(h http.Header) map[string]string {
	out := map[string]string{}
	for _, v := range h.Values("Set-Cookie") {
		name, _, _ := strings.Cut(v, "=")
		out[name] = v
	}
	return out
}

// cookieValue is the value of a Set-Cookie header.
func cookieValue(sc string) string {
	_, v, _ := strings.Cut(sc, "=")
	v, _, _ = strings.Cut(v, ";")
	return v
}

// visitProxy is a proxy serving a guarded environment whose web service
// echoes, with its keys and fake visits.
func visitProxy(t *testing.T) (*Proxy, *visit.Keys, *fakeVisits, fakeAccessLog) {
	t.Helper()
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	k := newKeys(t)
	fv := newFakeVisits(k)
	al := make(fakeAccessLog, 64)
	p, _ := newTestProxy(t, Config{}, Deps{Visits: fv, AccessLog: al}, fn, "n1")
	p.SetState(visitState(k, testSignIn, svc("web", ep("w1", "fd00::1", "n1"))))
	return p, k, fv, al
}

func echoOf(t *testing.T, rec *httptest.ResponseRecorder) echo {
	t.Helper()
	var e echo
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &e) != nil {
		t.Fatalf("not forwarded: %d %q", rec.Code, rec.Body.String())
	}
	return e
}

func want401(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized || rec.Body.String() != textVisitRequired+"\n" ||
		rec.Header().Get("WWW-Authenticate") != `FeRun-Visit realm="`+testHost+`"` ||
		rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("got %d %q %v, want 401", rec.Code, rec.Body.String(), rec.Header())
	}
}

// A navigation without a visit goes to the provider with a challenge the
// ingress sealed, for the environment, the host and the path with its
// query, bound to the challenge cookie the answer sets; the cookie is
// reused when the browser has one, and set again each time.
func TestVisitSignIn(t *testing.T) {
	p, k, fv, al := visitProxy(t)
	rec := serveCall(p, nav("/menu/a%2Fb?x=1&y=%20"))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	sealed, ok := strings.CutPrefix(loc, testSignIn+"?challenge=")
	if !ok || strings.ContainsAny(sealed, "&?#") {
		t.Fatalf("Location %q", loc)
	}
	c, err := k.OpenChallenge(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != testEnv || c.Host != testHost || c.Path != "/menu/a%2Fb?x=1&y=%20" || len(c.ID) != 22 ||
		c.Expires-c.IssuedAt != int64(visit.ChallengeLifetime/time.Second) {
		t.Fatalf("challenge %+v", c)
	}
	sc := setCookies(rec.Header())[visit.CookieChallenge]
	secret := cookieValue(sc)
	if !visit.ValidBindingSecret(secret) || visit.BindingHash(secret) != c.Binding ||
		!strings.HasSuffix(sc, "; Path=/; Max-Age=600; Secure; HttpOnly; SameSite=Lax") {
		t.Fatalf("challenge cookie %q", sc)
	}
	for k, v := range map[string]string{"Cache-Control": "no-store", "Referrer-Policy": "no-referrer"} {
		if rec.Header().Get(k) != v {
			t.Errorf("%s %q", k, rec.Header().Get(k))
		}
	}
	if r := al.next(t); r.Reason != wire.ReasonVisitSignIn || r.Status != http.StatusSeeOther || r.Service != "web" ||
		r.Env != testEnv {
		t.Fatalf("record %+v", r)
	}

	// A second navigation of the same browser binds to the same secret,
	// and sets the cookie again for 600 s.
	rec = serveCall(p, nav("/other").with(visit.CookieChallenge, secret))
	c2, err := k.OpenChallenge(strings.TrimPrefix(rec.Header().Get("Location"), testSignIn+"?challenge="))
	if err != nil || c2.Binding != c.Binding || c2.ID == c.ID || c2.Path != "/other" {
		t.Fatalf("second challenge %+v %v", c2, err)
	}
	if sc := setCookies(rec.Header())[visit.CookieChallenge]; cookieValue(sc) != secret || !strings.Contains(sc, "Max-Age=600") {
		t.Fatalf("second cookie %q", sc)
	}
	al.next(t)
	// A bad cookie is not reused.
	rec = serveCall(p, nav("/").with(visit.CookieChallenge, "short"))
	if sc := setCookies(rec.Header())[visit.CookieChallenge]; cookieValue(sc) == "short" || !visit.ValidBindingSecret(cookieValue(sc)) {
		t.Fatalf("bad cookie reused: %q", sc)
	}
	al.next(t)
	// A long path returns to /.
	rec = serveCall(p, nav("/"+strings.Repeat("a", visit.MaxPath)))
	if c, _ := k.OpenChallenge(strings.TrimPrefix(rec.Header().Get("Location"), testSignIn+"?challenge=")); c.Path != "/" {
		t.Fatalf("long path %q", c.Path)
	}
	al.next(t)
	if r, n, e := fv.counts(); r+n+e != 0 || p.m.visitSignIns.Load() != 4 {
		t.Errorf("calls %d %d %d, sign-ins %d", r, n, e, p.m.visitSignIns.Load())
	}

	// A sign-in address with a query of its own gets "&".
	s := visitState(k, testSignIn+"?tenant=acme", svc("web", ep("w1", "fd00::1", "n1")))
	p.SetState(s)
	rec = serveCall(p, nav("/"))
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, testSignIn+"?tenant=acme&challenge=") {
		t.Fatalf("Location %q", loc)
	}
	// One that ends with "?" has a query too (§1.11, literally).
	p.SetState(visitState(k, testSignIn+"?", svc("web", ep("w1", "fd00::1", "n1"))))
	rec = serveCall(p, nav("/"))
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, testSignIn+"?&challenge=") {
		t.Fatalf("Location %q", loc)
	}
}

// What is no navigation gets 401: a script's fetch, curl's */*, a
// WebSocket handshake, a prefetch, a POST, a HEAD.
func TestVisitRequired(t *testing.T) {
	p, _, _, al := visitProxy(t)
	for name, c := range map[string]call{
		"fetch":        fetch("/api"),
		"curl":         {method: "GET", path: "/", header: map[string]string{"Accept": "*/*"}},
		"no accept":    {method: "GET", path: "/"},
		"websocket":    nav("/ws").header2("Upgrade", "websocket").header2("Connection", "Upgrade"),
		"prefetch":     nav("/").header2("Sec-Purpose", "prefetch;prerender"),
		"post":         {method: "POST", path: "/form", header: map[string]string{"Sec-Fetch-Mode": "navigate", "Accept": "text/html"}},
		"head":         {method: "HEAD", path: "/", header: map[string]string{"Accept": "text/html"}},
		"iframe-ish":   {method: "GET", path: "/", header: map[string]string{"Sec-Fetch-Mode": "no-cors", "Accept": "text/html"}},
		"old browser?": {method: "GET", path: "/", header: map[string]string{"Accept": "application/json"}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := serveCall(p, c)
			if c.method == "HEAD" {
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("got %d", rec.Code)
				}
			} else {
				want401(t, rec)
			}
			if _, ok := setCookies(rec.Header())[visit.CookieChallenge]; ok {
				t.Error("a 401 set a challenge cookie")
			}
			if r := al.next(t); r.Reason != wire.ReasonVisitRequired || r.Status != http.StatusUnauthorized {
				t.Fatalf("record %+v", r)
			}
		})
	}
	// Without Sec-Fetch-Mode, an Accept with text/html is a navigation.
	if rec := serveCall(p, call{method: "GET", path: "/", header: map[string]string{"Accept": "text/html"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("old browser: %d", rec.Code)
	}
}

// A guarded environment that cannot sign anyone in serves nobody: a
// navigation gets 503, anything else 401, a valid visit cookie included.
func TestVisitFailsClosed(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	k := newKeys(t)
	secret := newSecret(t)
	cookie := visit.VisitCookie(mint(t, k, secret, nil), secret)
	web := svc("web", ep("w1", "fd00::1", "n1"))
	for name, c := range map[string]struct {
		state  func() *wire.State
		visits bool
	}{
		"no keys": {func() *wire.State { s := visitState(k, testSignIn, web); s.VisitKeys = nil; return s }, true},
		"bad keys": {func() *wire.State {
			s := visitState(k, testSignIn, web)
			s.VisitKeys.TokenKID = "wrong"
			return s
		}, true},
		"http sign-in":    {func() *wire.State { return visitState(k, "http://fecode.example/visit", web) }, true},
		"no sign-in":      {func() *wire.State { return visitState(k, "", web) }, true},
		"relative":        {func() *wire.State { return visitState(k, "/visit", web) }, true},
		"with a fragment": {func() *wire.State { return visitState(k, testSignIn+"#x", web) }, true},
		"unknown access": {func() *wire.State {
			s := visitState(k, testSignIn, web)
			e := s.Envs[testEnv]
			e.Access = "members"
			s.Envs[testEnv] = e
			return s
		}, true},
		"no visit calls": {func() *wire.State { return visitState(k, testSignIn, web) }, false},
	} {
		t.Run(name, func(t *testing.T) {
			al := make(fakeAccessLog, 8)
			d := Deps{AccessLog: al}
			if c.visits {
				d.Visits = newFakeVisits(k)
			}
			p, _ := newTestProxy(t, Config{}, d, fn, "n1")
			p.SetState(c.state())
			rec := serveCall(p, nav("/").with(visit.CookieVisit, cookie))
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" ||
				!strings.Contains(rec.Body.String(), pageNotHere) {
				t.Fatalf("navigation: %d %q", rec.Code, rec.Body.String())
			}
			if r := al.next(t); r.Reason != wire.ReasonVisitUnavailable || r.Status != 503 {
				t.Fatalf("record %+v", r)
			}
			want401(t, serveCall(p, fetch("/").with(visit.CookieVisit, cookie)))
			if r := al.next(t); r.Reason != wire.ReasonVisitUnavailable {
				t.Fatalf("record %+v", r)
			}
			if fn.dialed("[fd00::1]:8080") != 0 {
				t.Error("forwarded")
			}
			if u, r := p.m.visitUnavailable.Load(), p.m.visitRequired.Load(); u != 2 || r != 0 {
				t.Errorf("unavailable %d, required %d: want 2, 0", u, r)
			}
		})
	}
}

// A valid visit is forwarded with the visitor headers, the token without
// its secret, and without the ingress's cookies; the application's other
// cookies pass as they were. A client's visitor headers never pass.
func TestVisitForwarded(t *testing.T) {
	p, k, fv, al := visitProxy(t)
	secret := newSecret(t)
	token := mint(t, k, secret, nil)
	c := fetch("/x").with(visit.CookieVisit, visit.VisitCookie(token, secret)).with("app", "1").
		with(visit.CookieChallenge, newSecret(t)).with("theme", "dark")
	c = c.header2("FeRun-Visitor", "admin").header2("ferun_visitor", "admin").header2("FeRun.Visitor", "admin").
		header2("FERUN-VISIT-TOKEN", "forged")
	e := echoOf(t, serveCall(p, c))
	if e.Headers["Ferun-Visitor"] != "u-1" || e.Headers["Ferun-Visitor-Email"] != "a@example.com" ||
		e.Headers["Ferun-Visit-Token"] != token {
		t.Fatalf("headers %v", e.Headers)
	}
	for k := range e.Headers {
		if visit.IsVisitorHeader(k) && k != "Ferun-Visitor" && k != "Ferun-Visitor-Email" && k != "Ferun-Visit-Token" {
			t.Errorf("a client's %s passed", k)
		}
	}
	if got := strings.Split(e.Headers["Cookie"], "; "); len(got) != 2 || !slices.Contains(got, "app=1") ||
		!slices.Contains(got, "theme=dark") {
		t.Fatalf("cookies %q", e.Headers["Cookie"])
	}
	if strings.Contains(e.Headers["Ferun-Visit-Token"], secret) {
		t.Fatal("the secret reached the app")
	}
	if r := al.next(t); r.Reason != "" || r.Status != http.StatusOK || r.Service != "web" {
		t.Fatalf("record %+v", r)
	}
	// A navigation of a visit is forwarded too, and wants no renewal.
	echoOf(t, serveCall(p, nav("/").with(visit.CookieVisit, visit.VisitCookie(token, secret))))
	al.next(t)
	// Without email, no email header.
	token2 := mint(t, k, secret, func(tk *visit.Token) { tk.Email = "" })
	if e := echoOf(t, serveCall(p, fetch("/").with(visit.CookieVisit, visit.VisitCookie(token2, secret)))); e.Headers["Ferun-Visitor-Email"] != "" {
		t.Fatalf("email %q", e.Headers["Ferun-Visitor-Email"])
	}
	al.next(t)
	if _, n, _ := fv.counts(); n != 0 {
		t.Errorf("%d renewals of fresh tokens", n)
	}
}

// What is no visit: the token alone, another secret, a token of another
// environment, host or provider, another key, a header value that is
// not one, a request from another host of the site that is no
// navigation.
func TestVisitRefusedCookies(t *testing.T) {
	p, k, _, al := visitProxy(t)
	secret := newSecret(t)
	good := mint(t, k, secret, nil)
	other := newKeys(t)
	for name, cookie := range map[string]string{
		"token alone":    good,
		"another secret": visit.VisitCookie(good, newSecret(t)),
		"another env": visit.VisitCookie(mint(t, k, secret, func(tk *visit.Token) {
			tk.Env = "1a6b3c62-6d4f-4b55-9a49-3a1c2c1b0f01"
		}), secret),
		"another host":     visit.VisitCookie(mint(t, k, secret, func(tk *visit.Token) { tk.Host = "other.apps.example" }), secret),
		"another provider": visit.VisitCookie(mint(t, k, secret, func(tk *visit.Token) { tk.Provider = "ferun" }), secret),
		"another key":      visit.VisitCookie(mint(t, other, secret, nil), secret),
		"a newline":        visit.VisitCookie(mint(t, k, secret, func(tk *visit.Token) { tk.Subject = "u\n1" }), secret),
		"a code":           "x~" + secret,
	} {
		t.Run(name, func(t *testing.T) {
			want401(t, serveCall(p, fetch("/").with(visit.CookieVisit, cookie)))
			al.next(t)
		})
	}
	cookie := visit.VisitCookie(good, secret)
	for site, ok := range map[string]bool{"same-site": false, "cross-site": false, "same-origin": true, "none": true} {
		rec := serveCall(p, fetch("/").header2("Sec-Fetch-Site", site).with(visit.CookieVisit, cookie))
		if ok {
			echoOf(t, rec)
		} else {
			want401(t, rec)
		}
		al.next(t)
	}
	// A navigation from another site keeps its visit, and so does one
	// without Sec-Fetch-Dest.
	echoOf(t, serveCall(p, nav("/").header2("Sec-Fetch-Site", "cross-site").with(visit.CookieVisit, cookie)))
	al.next(t)
	noDest := nav("/").header2("Sec-Fetch-Site", "same-site")
	delete(noDest.header, "Sec-Fetch-Dest")
	echoOf(t, serveCall(p, noDest.with(visit.CookieVisit, cookie)))
	al.next(t)
}

// A frame of the guarded app in another host's page of the site (an
// iframe, an embed) is no visit, though the browser sends the cookie: 401
// and no visitor header, with a cookie and without (no sign-in in a
// frame). A frame of the app's own origin keeps the visit.
func TestVisitFramedFromAnotherHost(t *testing.T) {
	p, k, fv, al := visitProxy(t)
	secret := newSecret(t)
	cookie := visit.VisitCookie(mint(t, k, secret, nil), secret)
	for _, dest := range []string{"iframe", "frame", "embed", "object"} {
		for _, site := range []string{"same-site", "cross-site"} {
			framed := nav("/settings").header2("Sec-Fetch-Dest", dest).header2("Sec-Fetch-Site", site)
			want401(t, serveCall(p, framed.with(visit.CookieVisit, cookie)))
			if e := al.next(t); e.Reason != wire.ReasonVisitRequired {
				t.Fatalf("%s %s: reason %q", dest, site, e.Reason)
			}
			want401(t, serveCall(p, framed))
			al.next(t)
		}
	}
	if n, _, _ := fv.counts(); n != 0 {
		t.Fatalf("%d calls", n)
	}
	e := echoOf(t, serveCall(p, nav("/settings").header2("Sec-Fetch-Dest", "iframe").header2("Sec-Fetch-Site", "same-origin").
		with(visit.CookieVisit, cookie)))
	if e.Headers["Ferun-Visitor"] == "" {
		t.Fatalf("a frame of the own origin lost its visitor: %v", e.Headers)
	}
	al.next(t)
	// The top-level navigation from another host of the site keeps it.
	e = echoOf(t, serveCall(p, nav("/settings").header2("Sec-Fetch-Site", "same-site").with(visit.CookieVisit, cookie)))
	if e.Headers["Ferun-Visitor"] == "" {
		t.Fatalf("a top-level navigation lost its visitor: %v", e.Headers)
	}
	al.next(t)
}

// On an environment open to anyone the visit cookie is not read, only
// taken out, and no visitor header passes, a client's least of all; an
// application's Set-Cookie of the ingress's cookies is dropped on every
// host.
func TestVisitAnyoneEnvironment(t *testing.T) {
	fn := newFakeNet()
	k := newKeys(t)
	handlerAt(t, fn, "fd00::1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		names := []string{}
		for k := range r.Header {
			if visit.IsVisitorHeader(k) {
				names = append(names, k)
			}
		}
		w.Header().Add("Set-Cookie", visit.CookieVisit+"=planted; Path=/; Secure")
		w.Header().Add("Set-Cookie", "app=1; Path=/")
		w.Header().Add("Set-Cookie", "__host-FERUN_challenge=x; Path=/; Secure")
		_ = json.NewEncoder(w).Encode(map[string]any{"visitor": names, "cookie": r.Header.Values("Cookie")})
	}))
	p, _ := newTestProxy(t, Config{}, Deps{Visits: newFakeVisits(k)}, fn, "n1")
	s := testState(svc("web", ep("w1", "fd00::1", "n1")))
	s.VisitKeys = wireKeys(k)
	p.SetState(s)
	secret := newSecret(t)
	rec := serveCall(p, fetch("/").with(visit.CookieVisit, visit.VisitCookie(mint(t, k, secret, nil), secret)).
		with("a", "b").header2("FeRun-Visitor", "admin").header2("ferun_visitor", "admin").
		header2("FeRun.Visitor", "admin").header2("FERUN-VISIT-TOKEN", "x"))
	var got struct {
		Visitor []string `json:"visitor"`
		Cookie  []string `json:"cookie"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if len(got.Visitor) != 0 || len(got.Cookie) != 1 || got.Cookie[0] != "a=b" {
		t.Fatalf("app got %+v", got)
	}
	if sc := rec.Header().Values("Set-Cookie"); len(sc) != 1 || sc[0] != "app=1; Path=/" {
		t.Fatalf("Set-Cookie %q", sc)
	}
	// Only our cookies: the Cookie header goes.
	rec = serveCall(p, fetch("/").with(visit.CookieVisit, "x"))
	if json.Unmarshal(rec.Body.Bytes(), &got) != nil || len(got.Cookie) != 0 {
		t.Fatalf("app got %+v", got)
	}
}

// stripOurCookies keeps the rest's spelling, every header of it.
func TestStripOurCookies(t *testing.T) {
	h := http.Header{"Cookie": {"a=1;" + visit.CookieVisit + "=x; b=2", visit.CookieChallenge + "=y", "c=3"}}
	if n := stripOurCookies(h); n != 2 || strings.Join(h["Cookie"], "|") != "a=1; b=2|c=3" {
		t.Fatalf("%d %q", n, h["Cookie"])
	}
	h = http.Header{"Cookie": {visit.CookieVisit + "=x"}}
	if stripOurCookies(h); h["Cookie"] != nil {
		t.Fatalf("%q", h["Cookie"])
	}
}

// A token with less than 5 minutes left is renewed in line, once for the
// requests of a visit cookie that come at once; the answer sets the new
// token with the same secret, and the app gets the new token. Requests
// that still carry the old token use the new one without a call.
func TestVisitRenewal(t *testing.T) {
	p, k, fv, al := visitProxy(t)
	secret := newSecret(t)
	old := mint(t, k, secret, func(tk *visit.Token) { tk.Expires = time.Now().Add(2 * time.Minute).Unix() })
	gate := make(chan struct{})
	fv.set(func(f *fakeVisits) { f.gate = gate })
	const n = 5
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recs[i] = serveCall(p, fetch("/").with(visit.CookieVisit, visit.VisitCookie(old, secret)))
		}()
	}
	eventually(t, "the renewal", func() bool { _, r, _ := fv.counts(); return r == 1 })
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	if _, r, _ := fv.counts(); r != 1 {
		t.Fatalf("%d renewals", r)
	}
	fv.mu.Lock()
	req := fv.renewals[0]
	fv.mu.Unlock()
	if req.Token != old || req.Secret != secret {
		t.Fatalf("renewal %+v", req)
	}
	var next string
	for _, rec := range recs {
		e := echoOf(t, rec)
		sc := setCookies(rec.Header())[visit.CookieVisit]
		tok, sec, ok := visit.ParseVisitCookie(cookieValue(sc))
		if !ok || sec != secret || tok == old || e.Headers["Ferun-Visit-Token"] != tok ||
			rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(sc, "; Secure; HttpOnly; SameSite=Lax") {
			t.Fatalf("answer %q %v", sc, e.Headers)
		}
		next = tok
		al.next(t)
	}
	// Later with the old cookie: the new token, no call.
	rec := serveCall(p, fetch("/").with(visit.CookieVisit, visit.VisitCookie(old, secret)))
	if e := echoOf(t, rec); e.Headers["Ferun-Visit-Token"] != next || cookieValue(setCookies(rec.Header())[visit.CookieVisit]) != visit.VisitCookie(next, secret) {
		t.Fatalf("old cookie later: %v", e.Headers)
	}
	al.next(t)
	// With the new one: nothing to set.
	rec = serveCall(p, fetch("/").with(visit.CookieVisit, visit.VisitCookie(next, secret)))
	if echoOf(t, rec); rec.Header().Values("Set-Cookie") != nil {
		t.Fatalf("Set-Cookie %q", rec.Header().Values("Set-Cookie"))
	}
	al.next(t)
	if _, r, _ := fv.counts(); r != 1 || p.m.visitRenewed.Load() != 1 {
		t.Fatalf("%d renewals, %d renewed", r, p.m.visitRenewed.Load())
	}
}

// The renewed cookie rides a 101 and the final answer after a 103, and
// the proxy's own answers.
func TestVisitRenewalCookieOnUpgradeAndEarlyHints(t *testing.T) {
	fn := newFakeNet()
	k := newKeys(t)
	fv := newFakeVisits(k)
	handlerAt(t, fn, "fd00::1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hints" {
			w.Header().Set("Link", "</a.css>; rel=preload")
			w.Header().Add("Set-Cookie", visit.CookieVisit+"=from-app")
			w.WriteHeader(http.StatusEarlyHints)
			w.Header().Del("Link")
			_, _ = io.WriteString(w, "ok")
			return
		}
		wsEcho().ServeHTTP(w, r)
	}))
	p, srv := newTestProxy(t, Config{}, Deps{Visits: fv}, fn, "n1")
	p.SetState(visitState(k, testSignIn, svc("web", ep("w1", "fd00::1", "n1")), svc("api")))
	secret := newSecret(t)
	short := func() string {
		return mint(t, k, secret, func(tk *visit.Token) { tk.Expires = time.Now().Add(time.Minute).Unix() })
	}

	// The 101.
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = io.WriteString(c, "GET /ws HTTP/1.1\r\nHost: "+testHost+"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Cookie: "+visit.CookieVisit+"="+visit.VisitCookie(short(), secret)+"\r\n\r\n")
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade %v %v", res, err)
	}
	if _, sec, ok := visit.ParseVisitCookie(cookieValue(setCookies(res.Header)[visit.CookieVisit])); !ok || sec != secret {
		t.Fatalf("101 Set-Cookie %q", res.Header.Values("Set-Cookie"))
	}

	// After a 103: the app's cookie of ours is dropped from both, ours is
	// on the final answer.
	tok := short()
	var hints []http.Header
	req, _ := http.NewRequestWithContext(httptraceHints(&hints), "GET", srv.URL+"/hints", nil)
	req.Host = testHost
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.AddCookie(&http.Cookie{Name: visit.CookieVisit, Value: visit.VisitCookie(tok, secret)})
	res, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(hints) != 1 || hints[0].Get("Set-Cookie") != "" {
		t.Fatalf("103 headers %v", hints)
	}
	scs := res.Header.Values("Set-Cookie")
	if len(scs) != 1 || cookieValue(scs[0]) == "from-app" || !strings.HasPrefix(scs[0], visit.CookieVisit+"=") {
		t.Fatalf("final Set-Cookie %q", scs)
	}

	// The proxy's own answer (no instance of api) carries it too.
	rec := serveCall(p, fetch("/api/x").with(visit.CookieVisit, visit.VisitCookie(short(), secret)))
	if rec.Code != http.StatusServiceUnavailable || len(rec.Header().Values("Set-Cookie")) != 1 {
		t.Fatalf("own answer %d %v", rec.Code, rec.Header())
	}
}

// When the renewal says the visit ended, the cookie is cleared, the
// visit's WebSocket and event stream are closed, another visit's are
// not, and this instance no longer takes the visit's tokens. An unknown
// cookie is cleared, and nothing is remembered.
func TestVisitEnded(t *testing.T) {
	fn := newFakeNet()
	handlerAt(t, fn, "fd00::1", wsEcho())
	k := newKeys(t)
	fv := newFakeVisits(k)
	p, srv := newTestProxy(t, Config{}, Deps{Visits: fv}, fn, "n1")
	p.SetState(visitState(k, testSignIn, svc("web", ep("w1", "fd00::1", "n1"))))
	secret, otherSecret := newSecret(t), newSecret(t)
	const otherVisit = "1f2b7c9e-4f1d-4c0e-9d7a-5b6c7d8e9f01"
	fresh := visit.VisitCookie(mint(t, k, secret, nil), secret)
	other := visit.VisitCookie(mint(t, k, otherSecret, func(tk *visit.Token) { tk.Visit = otherVisit }), otherSecret)

	ws, wsBr := upgradeWith(t, srv, testHost, "/ws", fresh)
	otherWS, otherBr := upgradeWith(t, srv, testHost, "/ws", other)
	events := stream(t, srv, "/events", fresh)
	if !echoes(ws, wsBr, "hi") || !echoes(otherWS, otherBr, "hi") {
		t.Fatal("no echo")
	}

	fv.set(func(f *fakeVisits) { f.ended[testVisit] = "provider_no" })
	short := mint(t, k, secret, func(tk *visit.Token) { tk.Expires = time.Now().Add(time.Minute).Unix() })
	rec := serveCall(p, nav("/").with(visit.CookieVisit, visit.VisitCookie(short, secret)))
	if rec.Code != http.StatusSeeOther || setCookies(rec.Header())[visit.CookieVisit] !=
		visit.CookieVisit+"=; Path=/; Max-Age=0; Secure; HttpOnly; SameSite=Lax" {
		t.Fatalf("ended: %d %v", rec.Code, rec.Header())
	}
	if !closed(ws, wsBr) {
		t.Error("the visit's WebSocket outlived its end")
	}
	select {
	case <-events:
	case <-time.After(2 * time.Second):
		t.Error("the visit's event stream outlived its end")
	}
	if !echoes(otherWS, otherBr, "still") {
		t.Error("another visit's WebSocket was closed")
	}
	// The visit's fresh token counts no more here, without a call.
	_, before, _ := fv.counts()
	rec = serveCall(p, fetch("/").with(visit.CookieVisit, fresh))
	want401(t, rec)
	if _, after, _ := fv.counts(); after != before {
		t.Error("a remembered end was asked again")
	}
	if setCookies(rec.Header())[visit.CookieVisit] == "" {
		t.Error("the cookie of an ended visit was not cleared")
	}

	// unknown: cleared, not remembered.
	s3 := newSecret(t)
	const v3 = "2f2b7c9e-4f1d-4c0e-9d7a-5b6c7d8e9f01"
	fv.set(func(f *fakeVisits) { f.ended[v3] = "unknown" })
	short3 := mint(t, k, s3, func(tk *visit.Token) { tk.Visit = v3; tk.Expires = time.Now().Add(time.Minute).Unix() })
	rec = serveCall(p, fetch("/").with(visit.CookieVisit, visit.VisitCookie(short3, s3)))
	want401(t, rec)
	if setCookies(rec.Header())[visit.CookieVisit] == "" {
		t.Error("unknown: not cleared")
	}
	if p.visits.isEnded(v3, time.Now()) {
		t.Error("unknown: remembered")
	}
	fv.set(func(f *fakeVisits) { delete(f.ended, v3) })
	stream(t, srv, "/events", visit.VisitCookie(mint(t, k, s3, func(tk *visit.Token) { tk.Visit = v3 }), s3))
}

// upgradeWith is upgrade with a visit cookie.
func upgradeWith(t *testing.T, srv *httptest.Server, host, path, cookie string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: "+host+"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Cookie: "+visit.CookieVisit+"="+cookie+"\r\n\r\n")
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", res, err)
	}
	return c, br
}

// stream opens an event stream with a visit cookie; the channel closes
// when it ends.
func stream(t *testing.T, srv *httptest.Server, path, cookie string) <-chan struct{} {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.Host = testHost
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.AddCookie(&http.Cookie{Name: visit.CookieVisit, Value: cookie})
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	br := bufio.NewReader(res.Body)
	if line, _ := br.ReadString('\n'); line != "data: one\n" {
		t.Fatalf("first event %q", line)
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.ReadAll(br)
		close(done)
	}()
	return done
}

// A renewal that fails serves on with the token the request has, and
// asks again only after renewRetry.
func TestVisitRenewalErrorServesOn(t *testing.T) {
	p, k, fv, _ := visitProxy(t)
	secret := newSecret(t)
	short := visit.VisitCookie(mint(t, k, secret, func(tk *visit.Token) { tk.Expires = time.Now().Add(time.Minute).Unix() }), secret)
	for _, err := range []error{status.Error(codes.Internal, "boom"), status.Error(codes.Unavailable, "away"),
		status.Error(codes.ResourceExhausted, "busy")} {
		p.visits = newVisitBook()
		fv.set(func(f *fakeVisits) { f.renewErr = err; f.renewals = nil })
		for range 2 {
			rec := serveCall(p, fetch("/").with(visit.CookieVisit, short))
			if echoOf(t, rec); rec.Header().Values("Set-Cookie") != nil {
				t.Fatalf("%v: Set-Cookie %q", err, rec.Header().Values("Set-Cookie"))
			}
		}
		_, n, _ := fv.counts()
		want := 1
		if status.Code(err) == codes.ResourceExhausted {
			want = 2 // a local busy does not back off
		}
		if n != want {
			t.Errorf("%v: %d renewals, want %d", err, n, want)
		}
	}
	// A renewal that is not the visit's is no answer either.
	p.visits = newVisitBook()
	fv.set(func(f *fakeVisits) {
		f.renewErr = nil
		f.renewToken = func(tk visit.Token) visit.Token { tk.Visit = "other"; return tk }
	})
	if rec := serveCall(p, fetch("/").with(visit.CookieVisit, short)); rec.Code != 200 || rec.Header().Values("Set-Cookie") != nil {
		t.Fatalf("a foreign renewal: %d %v", rec.Code, rec.Header())
	}
}

// A token past its end counts until its visit's end only while the
// control plane is away (not connected, unavailable, unimplemented, out
// of time); a local busy or an ended visit is no visit, and a visit this
// instance saw end is not revived.
func TestVisitOutageRule(t *testing.T) {
	p, k, fv, _ := visitProxy(t)
	secret := newSecret(t)
	expired := func(vend time.Duration) string {
		return visit.VisitCookie(mint(t, k, secret, func(tk *visit.Token) {
			tk.IssuedAt = time.Now().Add(-time.Hour).Unix()
			tk.Expires = time.Now().Add(-10 * time.Minute).Unix()
			tk.VisitEnds = time.Now().Add(vend).Unix()
		}), secret)
	}
	for _, c := range []struct {
		err    error
		served bool
	}{
		{status.Error(codes.Unavailable, "ingress agent: not connected to the control plane"), true},
		{status.Error(codes.Unimplemented, "method RenewVisit not implemented"), true},
		{status.Error(codes.DeadlineExceeded, "late"), true},
		{status.Error(codes.ResourceExhausted, "ingress agent: too many visit calls in flight"), false},
		{status.Error(codes.InvalidArgument, "bad"), false},
	} {
		p.visits = newVisitBook()
		fv.set(func(f *fakeVisits) { f.renewErr = c.err })
		rec := serveCall(p, fetch("/").with(visit.CookieVisit, expired(time.Hour)))
		if c.served {
			echoOf(t, rec)
		} else {
			want401(t, rec)
		}
		// Past the visit's end: never, and no call.
		_, before, _ := fv.counts()
		want401(t, serveCall(p, fetch("/").with(visit.CookieVisit, expired(-time.Second))))
		if _, after, _ := fv.counts(); after != before {
			t.Errorf("%v: a renewal past the visit's end", c.err)
		}
		// A navigation past the visit's end signs in again.
		if rec := serveCall(p, nav("/").with(visit.CookieVisit, expired(-time.Second))); rec.Code != http.StatusSeeOther {
			t.Errorf("navigation past the end: %d", rec.Code)
		}
	}
	// An expired token whose renewal works counts with the new one.
	p.visits = newVisitBook()
	fv.set(func(f *fakeVisits) { f.renewErr = nil })
	rec := serveCall(p, fetch("/").with(visit.CookieVisit, expired(time.Hour)))
	if e := echoOf(t, rec); setCookies(rec.Header())[visit.CookieVisit] == "" || e.Headers["Ferun-Visit-Token"] == "" {
		t.Fatalf("renewed after its end: %v", rec.Header())
	}
	// A visit this instance saw end stays ended through an outage.
	p.visits = newVisitBook()
	p.visits.end(testVisit, visit.BindingHash(secret), "signed_out", time.Now().Add(time.Hour).Unix())
	fv.set(func(f *fakeVisits) { f.renewErr = status.Error(codes.Unavailable, "away") })
	want401(t, serveCall(p, fetch("/").with(visit.CookieVisit, expired(time.Hour))))
	want401(t, serveCall(p, fetch("/").with(visit.CookieVisit,
		visit.VisitCookie(mint(t, k, secret, nil), secret))))
}

// The order: a client outside the route's blocks gets 403 before any
// sign-in; a request without a visit wakes nothing and is not held; a
// held request is checked again when its environment turns guarded.
func TestVisitOrder(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	k := newKeys(t)
	waker := &fakeWaker{}
	al := make(fakeAccessLog, 16)
	p, _ := newTestProxy(t, Config{}, Deps{Visits: newFakeVisits(k), Waker: waker, AccessLog: al}, fn, "n1")
	s := visitState(k, testSignIn, wire.Service{Name: "web", Asleep: 1})
	e := s.Envs[testEnv]
	e.Routes[0].From = []string{"192.0.2.0/24"}
	s.Envs[testEnv] = e
	p.SetState(s)
	c := nav("/")
	c.remote = "198.51.100.1:4711"
	if rec := serveCall(p, c); rec.Code != http.StatusForbidden {
		t.Fatalf("outside: %d", rec.Code)
	}
	al.next(t)
	if rec := serveCall(p, nav("/")); rec.Code != http.StatusSeeOther {
		t.Fatalf("inside: %d", rec.Code)
	}
	al.next(t)
	want401(t, serveCall(p, fetch("/")))
	al.next(t)
	// A service without instances: a client without a visit learns
	// nothing of it either.
	want401(t, serveCall(p, fetch("/api/x")))
	al.next(t)
	if waker.count() != 0 || p.held.count() != 0 || p.inflight.count() != 0 {
		t.Fatalf("wakes %d held %d inflight %d", waker.count(), p.held.count(), p.inflight.count())
	}

	// Held on an open environment, guarded while it waits.
	open := testState(wire.Service{Name: "web", Asleep: 1})
	open.VisitKeys = wireKeys(k)
	p.SetState(open)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serveCall(p, fetch("/")) }()
	eventually(t, "the wake", func() bool { return waker.count() == 1 })
	p.SetState(visitState(k, testSignIn, svc("web", ep("w1", "fd00::1", "n1"))))
	want401(t, <-done)
	if r := al.next(t); r.Reason != wire.ReasonVisitRequired || r.HeldNs == 0 {
		t.Fatalf("record %+v", r)
	}
	if fn.dialed("[fd00::1]:8080") != 0 {
		t.Error("forwarded")
	}
}

// signedCode signs a code for a challenge of this test's environment.
func signedCode(t *testing.T, k *visit.Keys, binding string, f func(*visit.Code)) string {
	t.Helper()
	now := time.Now()
	id, _ := visit.NewChallengeID()
	c := visit.Code{Challenge: id, Env: testEnv, Host: testHost, Path: "/menu?x=1", Binding: binding,
		Outcome: visit.OutcomeAccepted, Visit: testVisit, IssuedAt: now.Unix(), Expires: now.Add(visit.CodeLifetime).Unix()}
	if f != nil {
		f(&c)
	}
	raw, err := k.SignCode(c)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func wantPage(t *testing.T, rec *httptest.ResponseRecorder, code int, text, retry string) {
	t.Helper()
	body := rec.Body.String()
	if rec.Code != code || !strings.Contains(body, "<p>"+text+"</p>") {
		t.Fatalf("got %d %q, want %d %q", rec.Code, body, code, text)
	}
	h := rec.Header()
	if h.Get("Content-Type") != "text/html; charset=utf-8" || h.Get("Content-Security-Policy") != pageCSP ||
		h.Get("Cache-Control") != "no-store" || h.Get("Referrer-Policy") != "no-referrer" ||
		h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("page headers %v", h)
	}
	link := `<a href="` + retry + `">Try again</a>`
	if retry != "" && !strings.Contains(body, link) || retry == "" && strings.Contains(body, "Try again") {
		t.Fatalf("page %q, want link %q", body, retry)
	}
}

// /.ferun/auth: every branch.
func TestVisitAuth(t *testing.T) {
	p, k, fv, al := visitProxy(t)
	cs := newSecret(t)
	bnd := visit.BindingHash(cs)
	auth := func(code string, cookie bool) *httptest.ResponseRecorder {
		c := nav("/.ferun/auth?code=" + url.QueryEscape(code))
		if cookie {
			c = c.with(visit.CookieChallenge, cs)
		}
		return serveCall(p, c)
	}
	const retry = "https://" + testHost + "/menu?x=1"
	wantPage(t, auth("", true), 400, pageBadLink, "")
	wantPage(t, auth("garbage", true), 400, pageBadLink, "")
	wantPage(t, auth(strings.Repeat("a", visit.MaxJWT+1), true), 400, pageBadLink, "")
	wantPage(t, auth(signedCode(t, newKeys(t), bnd, nil), true), 400, pageBadLink, "")
	wantPage(t, auth(mint(t, k, cs, nil), true), 400, pageBadLink, "") // a token is no code
	wantPage(t, auth(signedCode(t, k, bnd, func(c *visit.Code) { c.Env = "other" }), true), 400, pageBadLink, "")
	wantPage(t, auth(signedCode(t, k, bnd, func(c *visit.Code) { c.Host = "other.apps.example" }), true), 400, pageBadLink, "")
	wantPage(t, auth(signedCode(t, k, bnd, func(c *visit.Code) {
		c.Expires = time.Now().Add(-2 * time.Minute).Unix()
	}), true), 400, pageTooLong, retry)
	// Within the leeway it still counts.
	late := auth(signedCode(t, k, bnd, func(c *visit.Code) { c.Expires = time.Now().Add(-30 * time.Second).Unix() }), true)
	if late.Code != http.StatusSeeOther {
		t.Fatalf("within the leeway: %d %q", late.Code, late.Body.String())
	}
	for range 9 {
		al.next(t)
	}
	rec := auth(signedCode(t, k, bnd, func(c *visit.Code) { c.Outcome, c.Visit = visit.OutcomeRejected, "" }), true)
	wantPage(t, rec, 403, pageRejected, "")
	if r := al.next(t); r.Reason != wire.ReasonVisitRejected || r.Status != 403 || r.Service != "" || r.Env != testEnv {
		t.Fatalf("record %+v", r)
	}
	wantPage(t, auth(signedCode(t, k, bnd, nil), false), 400, pageNoCookies, "")
	rec = serveCall(p, nav("/.ferun/auth?code="+signedCode(t, k, bnd, nil)).with(visit.CookieChallenge, newSecret(t)))
	wantPage(t, rec, 400, pageOtherBrowser, retry)
	if r := al.next(t); r.Reason != wire.ReasonFerunPath {
		t.Fatalf("record %+v", r)
	}
	al.next(t)
	if r, _, _ := fv.counts(); r != 1 {
		t.Fatalf("%d redeems before an accepted code with its cookie", r)
	}

	// Accepted: redeemed with the challenge cookie and a fresh visit
	// secret; the cookie is set and the browser goes back.
	code := signedCode(t, k, bnd, nil)
	rec = auth(code, true)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != retry ||
		rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("admitted: %d %v", rec.Code, rec.Header())
	}
	sc := setCookies(rec.Header())[visit.CookieVisit]
	tok, sec, ok := visit.ParseVisitCookie(cookieValue(sc))
	fv.mu.Lock()
	red := fv.redeems[len(fv.redeems)-1]
	fv.mu.Unlock()
	if !ok || red.Code != code || red.Binding != cs || red.Host != testHost || red.VisitBinding != visit.BindingHash(sec) ||
		!strings.HasSuffix(sc, "; Secure; HttpOnly; SameSite=Lax") || !strings.Contains(sc, "Max-Age=43") {
		t.Fatalf("redeem %+v, cookie %q", red, sc)
	}
	al.next(t)
	// The visit cookie admits.
	if e := echoOf(t, serveCall(p, fetch("/").with(visit.CookieVisit, cookieValue(sc)))); e.Headers["Ferun-Visit-Token"] != tok {
		t.Fatalf("visit %v", e.Headers)
	}
	al.next(t)
	// The same code again: the same cookie, no call.
	n, _, _ := fv.counts()
	if again := auth(code, true); setCookies(again.Header())[visit.CookieVisit] != sc || again.Code != 303 {
		t.Fatalf("again: %d %v", again.Code, again.Header())
	}
	al.next(t)
	if n2, _, _ := fv.counts(); n2 != n {
		t.Fatal("the memo called again")
	}

	// Refused: a page with the link; remembered too.
	fv.set(func(f *fakeVisits) { f.refuse = "used" })
	used := signedCode(t, k, bnd, nil)
	wantPage(t, auth(used, true), 400, pageUsed, retry)
	al.next(t)
	n, _, _ = fv.counts()
	wantPage(t, auth(used, true), 400, pageUsed, retry)
	al.next(t)
	if n2, _, _ := fv.counts(); n2 != n {
		t.Fatal("a refusal was asked again")
	}
	// Away: 503 with Retry-After, not remembered.
	fv.set(func(f *fakeVisits) { f.refuse = ""; f.redeemErr = status.Error(codes.Unavailable, "away") })
	away := signedCode(t, k, bnd, nil)
	rec = auth(away, true)
	wantPage(t, rec, 503, pageNotNow, retry)
	if rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("Retry-After %q", rec.Header().Get("Retry-After"))
	}
	al.next(t)
	fv.set(func(f *fakeVisits) { f.redeemErr = nil })
	if rec := auth(away, true); rec.Code != 303 {
		t.Fatalf("after the outage: %d", rec.Code)
	}
	al.next(t)

	// An environment open to anyone: back, no call.
	open := testState(svc("web", ep("w1", "fd00::1", "n1")))
	open.VisitKeys = wireKeys(k)
	p.SetState(open)
	n, _, _ = fv.counts()
	if rec := auth(signedCode(t, k, bnd, nil), false); rec.Code != 303 || rec.Header().Get("Location") != retry ||
		rec.Header().Values("Set-Cookie") != nil {
		t.Fatalf("anyone: %d %v", rec.Code, rec.Header())
	}
	if n2, _, _ := fv.counts(); n2 != n {
		t.Fatal("anyone: a call")
	}
}

// A code whose path would leave the host goes back to /.
func TestVisitAuthOpenRedirect(t *testing.T) {
	p, k, _, _ := visitProxy(t)
	cs := newSecret(t)
	for _, path := range []string{"//evil.example/x", "/\\evil.example", "/a\r\nb"} {
		code := signedCode(t, k, visit.BindingHash(cs), func(c *visit.Code) { c.Path = path })
		rec := serveCall(p, nav("/.ferun/auth?code="+code).with(visit.CookieChallenge, cs))
		if rec.Header().Get("Location") != "https://"+testHost+"/" {
			t.Fatalf("%q: Location %q", path, rec.Header().Get("Location"))
		}
	}
}

// The same code twice at once: one redeem, and both get the same cookie.
func TestVisitAuthSingleFlight(t *testing.T) {
	p, k, fv, _ := visitProxy(t)
	cs := newSecret(t)
	code := signedCode(t, k, visit.BindingHash(cs), nil)
	gate := make(chan struct{})
	fv.set(func(f *fakeVisits) { f.gate = gate })
	recs := make([]*httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := range recs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recs[i] = serveCall(p, nav("/.ferun/auth?code="+code).with(visit.CookieChallenge, cs))
		}()
	}
	eventually(t, "the redeem", func() bool { r, _, _ := fv.counts(); return r == 1 })
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	if r, _, _ := fv.counts(); r != 1 {
		t.Fatalf("%d redeems", r)
	}
	a, b := setCookies(recs[0].Header())[visit.CookieVisit], setCookies(recs[1].Header())[visit.CookieVisit]
	if a == "" || a != b {
		t.Fatalf("cookies %q %q", a, b)
	}
}

// /.ferun/ is the ingress's on every host before any route, in its
// escaped and doubled spellings; anything else under it is 404.
func TestFerunPath(t *testing.T) {
	for raw, want := range map[string]string{
		"/.ferun": "", "/.ferun/": "/", "/.ferun/auth": "/auth", "//.ferun/auth": "/auth", "/%2Eferun/auth": "/auth",
		"/%2eferun//keys": "/keys", "/.ferun%2Fauth": "/auth", "/.ferunx": "-", "/a/.ferun/auth": "-", "/": "-",
	} {
		sub, ok := ferunPath(raw)
		if want == "-" && ok || want != "-" && (!ok || sub != want) {
			t.Errorf("%q: %q %v, want %q", raw, sub, ok, want)
		}
	}
	p, k, _, al := visitProxy(t)
	cs := newSecret(t)
	code := signedCode(t, k, visit.BindingHash(cs), nil)
	for _, path := range []string{"/%2Eferun/auth?code=" + code, "//.ferun/auth?code=" + code} {
		if rec := serveCall(p, nav(path).with(visit.CookieChallenge, cs)); rec.Code != 303 {
			t.Fatalf("%s: %d %q", path, rec.Code, rec.Body.String())
		}
		al.next(t)
	}
	for _, c := range []call{nav("/.ferun/nothing"), nav("/.ferun"), {method: "POST", path: "/.ferun/auth"},
		{method: "DELETE", path: "/.ferun/sign-out"}} {
		rec := serveCall(p, c)
		if rec.Code != 404 || rec.Body.String() != textNoFerunPath+"\n" || rec.Header().Get("Cache-Control") != "no-store" ||
			rec.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s %s: %d %q", c.method, c.path, rec.Code, rec.Body.String())
		}
		if r := al.next(t); r.Reason != wire.ReasonFerunPath || r.Env != testEnv || r.Tenant != "acme" || r.Service != "" {
			t.Fatalf("record %+v", r)
		}
	}
	// An unknown host gets nothing of the ingress.
	req := httptest.NewRequest("GET", "http://nobody.apps.example/.ferun/keys", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTPS().ServeHTTP(rec, req)
	if rec.Code != 404 || rec.Body.String() != textNoHost+"\n" {
		t.Fatalf("unknown host: %d %q", rec.Code, rec.Body.String())
	}
}

// /.ferun/keys answers the token key's JWKS on every host, an open one
// too; 404 without keys.
func TestFerunKeys(t *testing.T) {
	fn := newFakeNet()
	k := newKeys(t)
	p, _ := newTestProxy(t, Config{}, Deps{}, fn, "n1")
	p.SetState(testState())
	if rec := serveCall(p, fetch("/.ferun/keys")); rec.Code != 404 {
		t.Fatalf("without keys: %d", rec.Code)
	}
	s := testState()
	s.VisitKeys = wireKeys(k)
	p.SetState(s)
	rec := serveCall(p, fetch("/.ferun/keys"))
	if rec.Code != 200 || rec.Body.String() != string(k.JWKS()) || rec.Header().Get("Content-Type") != "application/json" ||
		rec.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Fatalf("keys: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
}

// Only the visits' keys changed: the table is rebuilt.
func TestVisitKeysChangeTheTable(t *testing.T) {
	fn := newFakeNet()
	p, _ := newTestProxy(t, Config{}, Deps{}, fn, "n1")
	s := testState()
	p.SetState(s)
	k := newKeys(t)
	s2 := s.Clone()
	s2.VisitKeys = wireKeys(k)
	p.SetState(s2)
	if p.cur.Load().t.keys == nil || p.cur.Load().t.keys.TokenKID() != k.TokenKID() {
		t.Fatal("keys arriving alone were ignored")
	}
	// The same keys again keep the parsed ones.
	before := p.cur.Load().t
	s3 := s2.Clone()
	e := s3.Envs[testEnv]
	e.Deployment++
	s3.Envs[testEnv] = e
	p.SetState(s3)
	if p.cur.Load().t == before || p.cur.Load().t.keys != before.keys {
		t.Fatal("unchanged keys were parsed again")
	}
}

// Sign-out: a POST of this site's ends the visit with its secret, closes
// its connections here, remembers it and clears the cookie; another
// site's is refused; with the control plane away the cookie is cleared
// all the same. GET shows the form.
func TestVisitSignOut(t *testing.T) {
	fn := newFakeNet()
	handlerAt(t, fn, "fd00::1", wsEcho())
	k := newKeys(t)
	fv := newFakeVisits(k)
	al := make(fakeAccessLog, 16)
	p, srv := newTestProxy(t, Config{}, Deps{Visits: fv, AccessLog: al}, fn, "n1")
	p.SetState(visitState(k, testSignIn, svc("web", ep("w1", "fd00::1", "n1"))))
	secret := newSecret(t)
	token := mint(t, k, secret, nil)
	cookie := visit.VisitCookie(token, secret)

	rec := serveCall(p, nav("/.ferun/sign-out"))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `<form method="post" action="/.ferun/sign-out">`) {
		t.Fatalf("GET: %d %q", rec.Code, rec.Body.String())
	}
	if r := al.next(t); r.Reason != wire.ReasonFerunPath {
		t.Fatalf("record %+v", r)
	}
	for name, h := range map[string]map[string]string{
		"another origin":    {"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"},
		"same site":         {"Origin": "https://other.apps.example", "Sec-Fetch-Site": "same-site"},
		"null origin":       {"Origin": "null"},
		"fetch site only":   {"Sec-Fetch-Site": "same-site"},
		"http origin":       {"Origin": "http://" + testHost},
		"origin with ports": {"Origin": "https://" + testHost + ":443"},
	} {
		rec := serveCall(p, call{method: "POST", path: "/.ferun/sign-out", header: h, cookies: map[string]string{visit.CookieVisit: cookie}})
		if rec.Code != 403 || rec.Body.String() != textCrossSiteSignOut+"\n" {
			t.Fatalf("%s: %d %q", name, rec.Code, rec.Body.String())
		}
		al.next(t)
	}
	if _, _, e := fv.counts(); e != 0 {
		t.Fatal("a refused sign-out ended the visit")
	}

	ws, br := upgradeWith(t, srv, testHost, "/ws", cookie)
	rec = serveCall(p, call{method: "POST", path: "/.ferun/sign-out", header: map[string]string{
		"Origin": "https://" + testHost, "Sec-Fetch-Site": "same-origin"}, cookies: map[string]string{visit.CookieVisit: cookie}})
	wantPage(t, rec, 200, "You are signed out of "+testHost+".", "")
	if setCookies(rec.Header())[visit.CookieVisit] != visit.CookieVisit+"=; Path=/; Max-Age=0; Secure; HttpOnly; SameSite=Lax" {
		t.Fatalf("cookie %v", rec.Header())
	}
	fv.mu.Lock()
	end := fv.ends[0]
	fv.mu.Unlock()
	if end.Token != token || end.Secret != secret {
		t.Fatalf("end %+v", end)
	}
	if !closed(ws, br) {
		t.Error("the WebSocket outlived the sign-out")
	}
	want401(t, serveCall(p, fetch("/").with(visit.CookieVisit, cookie)))

	// Away, and without Origin (an old browser): the cookie goes anyway.
	fv.set(func(f *fakeVisits) { f.endErr = status.Error(codes.Unavailable, "away") })
	s2 := newSecret(t)
	rec = serveCall(p, call{method: "POST", path: "/.ferun/sign-out",
		cookies: map[string]string{visit.CookieVisit: visit.VisitCookie(mint(t, k, s2, nil), s2)}})
	if rec.Code != 200 || setCookies(rec.Header())[visit.CookieVisit] == "" {
		t.Fatalf("away: %d %v", rec.Code, rec.Header())
	}
	// Without a cookie: cleared, no call.
	_, _, before := fv.counts()
	if rec := serveCall(p, call{method: "POST", path: "/.ferun/sign-out"}); rec.Code != 200 {
		t.Fatalf("without a cookie: %d", rec.Code)
	}
	if _, _, after := fv.counts(); after != before {
		t.Error("a sign-out without a visit called")
	}
}

// The background renewal renews a visit that only holds a WebSocket
// open, and closes it when the visit ended or reached its end; a change
// of the environment's provider closes it, one to anyone does not.
func TestVisitBackgroundRenewal(t *testing.T) {
	fn := newFakeNet()
	handlerAt(t, fn, "fd00::1", wsEcho())
	k := newKeys(t)
	fv := newFakeVisits(k)
	p, srv := newTestProxy(t, Config{}, Deps{Visits: fv}, fn, "n1")
	web := svc("web", ep("w1", "fd00::1", "n1"))
	p.SetState(visitState(k, testSignIn, web))
	secret := newSecret(t)
	short := visit.VisitCookie(mint(t, k, secret, func(tk *visit.Token) {
		tk.Expires = time.Now().Add(visit.RenewBefore + time.Minute).Unix()
	}), secret)
	ws, br := upgradeWith(t, srv, testHost, "/ws", short)
	p.renewLive(context.Background())
	if _, n, _ := fv.counts(); n != 0 {
		t.Fatal("renewed a token with time left")
	}
	// Its time runs short: renewed, kept open.
	p.visits.mu.Lock()
	for _, e := range p.visits.entries {
		for a := range e.live {
			a.claims.Expires = time.Now().Add(time.Minute).Unix()
		}
	}
	p.visits.mu.Unlock()
	p.renewLive(context.Background())
	if _, n, _ := fv.counts(); n != 1 || !echoes(ws, br, "still") {
		t.Fatalf("%d renewals", n)
	}
	// The newest token is kept: a pass right after asks nothing.
	p.renewLive(context.Background())
	if _, n, _ := fv.counts(); n != 1 {
		t.Fatalf("%d renewals after the renewal", n)
	}
	// Ended: closed.
	fv.set(func(f *fakeVisits) { f.ended[testVisit] = "provider_no" })
	p.visits.mu.Lock()
	for _, e := range p.visits.entries {
		e.latestClaims.Expires = time.Now().Add(time.Minute).Unix()
	}
	p.visits.mu.Unlock()
	p.renewLive(context.Background())
	if !closed(ws, br) {
		t.Fatal("an ended visit's WebSocket stayed open")
	}

	// A provider change closes; anyone keeps.
	fv.set(func(f *fakeVisits) { delete(f.ended, testVisit) })
	s2 := newSecret(t)
	const v2 = "3f2b7c9e-4f1d-4c0e-9d7a-5b6c7d8e9f01"
	c2 := visit.VisitCookie(mint(t, k, s2, func(tk *visit.Token) { tk.Visit = v2 }), s2)
	ws2, br2 := upgradeWith(t, srv, testHost, "/ws", c2)
	open := testState(web)
	open.VisitKeys = wireKeys(k)
	p.SetState(open)
	if !echoes(ws2, br2, "open") {
		t.Fatal("turning open closed a visit's WebSocket")
	}
	p.SetState(visitState(k, testSignIn, web))
	ws3, br3 := upgradeWith(t, srv, testHost, "/ws", c2)
	other := visitState(k, testSignIn, web)
	e := other.Envs[testEnv]
	e.Provider = "ferun"
	other.Envs[testEnv] = e
	p.SetState(other)
	if !closed(ws3, br3) {
		t.Fatal("a provider change left a visit's WebSocket open")
	}
	// A visit past its end is closed without a call.
	p.SetState(visitState(k, testSignIn, web))
	s4 := newSecret(t)
	ws4, br4 := upgradeWith(t, srv, testHost, "/ws", visit.VisitCookie(mint(t, k, s4, func(tk *visit.Token) {
		tk.Visit = "4f2b7c9e-4f1d-4c0e-9d7a-5b6c7d8e9f01"
		tk.Expires = time.Now().Add(-30 * time.Second).Unix()
		tk.VisitEnds = time.Now().Add(2 * time.Second).Unix()
	}), s4))
	time.Sleep(2100 * time.Millisecond)
	_, before, _ := fv.counts()
	p.renewLive(context.Background())
	if !closed(ws4, br4) {
		t.Fatal("a visit past its end kept its WebSocket")
	}
	if _, after, _ := fv.counts(); after != before {
		t.Error("asked for a visit past its end")
	}
}

// A background renewal that fails while the newest token is past its end
// and leeway closes the visit's connections, unless the control plane is
// away (the outage rule, as for a request); one whose token still counts
// keeps them.
func TestVisitBackgroundRenewalFails(t *testing.T) {
	fn := newFakeNet()
	handlerAt(t, fn, "fd00::1", wsEcho())
	k := newKeys(t)
	fv := newFakeVisits(k)
	p, srv := newTestProxy(t, Config{}, Deps{Visits: fv}, fn, "n1")
	p.SetState(visitState(k, testSignIn, svc("web", ep("w1", "fd00::1", "n1"))))
	setExpires := func(at time.Time) {
		p.visits.mu.Lock()
		for _, e := range p.visits.entries {
			for a := range e.live {
				a.claims.Expires = at.Unix()
			}
			e.retryAfter = time.Time{}
		}
		p.visits.mu.Unlock()
	}
	for i, c := range []struct {
		err     error
		expired bool
		closes  bool
	}{
		{status.Error(codes.Internal, "boom"), false, false},
		{status.Error(codes.Unavailable, "away"), true, false},
		{status.Error(codes.Unimplemented, "old"), true, false},
		{status.Error(codes.Internal, "boom"), true, true},
		{status.Error(codes.InvalidArgument, "bad"), true, true},
		{status.Error(codes.ResourceExhausted, "busy"), true, true},
	} {
		p.visits = newVisitBook()
		fv.set(func(f *fakeVisits) { f.renewErr = c.err })
		secret := newSecret(t)
		ws, br := upgradeWith(t, srv, testHost, "/ws", visit.VisitCookie(mint(t, k, secret, nil), secret))
		at := time.Now().Add(time.Minute)
		if c.expired {
			at = time.Now().Add(-visit.Leeway - time.Minute)
		}
		setExpires(at)
		p.renewLive(context.Background())
		if got := closed(ws, br); got != c.closes {
			t.Errorf("%d %v expired=%v: closed %v, want %v", i, c.err, c.expired, got, c.closes)
		}
	}
	// A renewal that is not the visit's closes one past its end too.
	p.visits = newVisitBook()
	fv.set(func(f *fakeVisits) {
		f.renewErr = nil
		f.renewToken = func(tk visit.Token) visit.Token { tk.Visit = "other"; return tk }
	})
	secret := newSecret(t)
	ws, br := upgradeWith(t, srv, testHost, "/ws", visit.VisitCookie(mint(t, k, secret, nil), secret))
	setExpires(time.Now().Add(-visit.Leeway - time.Minute))
	p.renewLive(context.Background())
	if !closed(ws, br) {
		t.Error("a foreign renewal left a connection past its token's end open")
	}
}

// The background renewal's calls end with its context, so that Run ends
// at once.
func TestVisitBackgroundRenewalEndsWithItsContext(t *testing.T) {
	fn := newFakeNet()
	handlerAt(t, fn, "fd00::1", wsEcho())
	k := newKeys(t)
	fv := newFakeVisits(k)
	p, srv := newTestProxy(t, Config{}, Deps{Visits: fv}, fn, "n1")
	p.SetState(visitState(k, testSignIn, svc("web", ep("w1", "fd00::1", "n1"))))
	secret := newSecret(t)
	upgradeWith(t, srv, testHost, "/ws", visit.VisitCookie(mint(t, k, secret, func(tk *visit.Token) {
		tk.Expires = time.Now().Add(time.Minute).Unix()
	}), secret))
	gate := make(chan struct{})
	defer close(gate)
	fv.set(func(f *fakeVisits) { f.gate = gate })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.renewLive(ctx)
		close(done)
	}()
	for {
		if _, n, _ := fv.counts(); n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(renewBackground):
		t.Fatal("the background renewal outlived its context")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("ended %v after its context", d)
	}
}

// An environment that turns guarded closes what it let in while it was
// open: a WebSocket and an event stream without a visit. One that stays
// open keeps them.
func TestVisitGuardingClosesOpenConnections(t *testing.T) {
	fn := newFakeNet()
	handlerAt(t, fn, "fd00::1", wsEcho())
	k := newKeys(t)
	p, srv := newTestProxy(t, Config{}, Deps{Visits: newFakeVisits(k)}, fn, "n1")
	web := svc("web", ep("w1", "fd00::1", "n1"))
	open := testState(web)
	open.VisitKeys = wireKeys(k)
	p.SetState(open)
	ws, br := upgradeWith(t, srv, testHost, "/ws", "")
	events := stream(t, srv, "/events", "")
	other := testState(web)
	other.VisitKeys = wireKeys(k)
	other.Self.Name = "ingress-1"
	p.SetState(other)
	if !echoes(ws, br, "still") {
		t.Fatal("a table that keeps the environment open closed a WebSocket")
	}
	p.SetState(visitState(k, testSignIn, web))
	if !closed(ws, br) {
		t.Error("an open WebSocket outlived its environment turning guarded")
	}
	select {
	case <-events:
	case <-time.After(2 * time.Second):
		t.Error("an open event stream outlived its environment turning guarded")
	}
	// The attempts leave the book when their forward returns, which may
	// be just after the client saw its connection close.
	eventually(t, "the closed attempts to leave the book", func() bool {
		p.visits.openMu.Lock()
		defer p.visits.openMu.Unlock()
		return len(p.visits.open) == 0
	})
}

// A request let in open whose environment turns guarded while its dial
// is under way routes again and gets the visit check: 401, and the
// instance does not cool down.
func TestVisitGuardingDuringTheDial(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	k := newKeys(t)
	p, srv := newTestProxy(t, Config{}, Deps{Visits: newFakeVisits(k)}, fn, "n1")
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	p.up.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
		return fn.dial(ctx, network, addr)
	}
	web := svc("web", ep("w1", "fd00::1", "n1"))
	open := testState(web)
	open.VisitKeys = wireKeys(k)
	p.SetState(open)
	done := make(chan result, 1)
	go func() { done <- send(context.Background(), srv, "GET", testHost, "/", nil) }()
	<-entered
	p.SetState(visitState(k, testSignIn, web))
	close(gate)
	wantPlain(t, <-done, http.StatusUnauthorized, textVisitRequired)
	if p.m.dialErrors.Load() != 0 || p.m.responseErrors.Load() != 0 || len(p.cooling) != 0 {
		t.Errorf("dial errors %d, response errors %d, cooling %v", p.m.dialErrors.Load(), p.m.responseErrors.Load(),
			p.cooling)
	}
}

// An attempt whose visit check saw a table that a newer one replaced
// before it was recorded is checked against the newer one: it ends at
// once when that no longer admits it.
func TestVisitAttachSeesTheNewerTable(t *testing.T) {
	k := newKeys(t)
	fn := newFakeNet()
	p, _ := newTestProxy(t, Config{}, Deps{Visits: newFakeVisits(k)}, fn, "n1")
	web := svc("web", ep("w1", "fd00::1", "n1"))
	open := testState(web)
	open.VisitKeys = wireKeys(k)
	p.SetState(open)
	openEnv := p.cur.Load().t.env(testHost)
	p.SetState(visitState(k, testSignIn, web))
	guardedEnv := p.cur.Load().t.env(testHost)
	cur := func() *table { return p.cur.Load().t }
	ctx, cancel := context.WithCancelCause(context.Background())
	a := p.visits.attach(nil, openEnv, testHost, cancel, cur)
	if context.Cause(ctx) != errAccessChanged {
		t.Fatalf("an attempt without a visit on a guarded environment: %v", context.Cause(ctx))
	}
	p.visits.detach(a)

	secret := newSecret(t)
	claims, err := k.VerifyToken(mint(t, k, secret, nil))
	if err != nil {
		t.Fatal(err)
	}
	vr := &visitReq{token: "t", secret: secret, claims: claims}
	ctx, cancel = context.WithCancelCause(context.Background())
	a = p.visits.attach(vr, guardedEnv, testHost, cancel, cur)
	if ctx.Err() != nil {
		t.Fatal("a visit of the environment's provider ended")
	}
	p.visits.detach(a)
	ferun := visitState(k, testSignIn, web)
	e := ferun.Envs[testEnv]
	e.Provider = "ferun"
	ferun.Envs[testEnv] = e
	p.SetState(ferun)
	ctx, cancel = context.WithCancelCause(context.Background())
	a = p.visits.attach(vr, guardedEnv, testHost, cancel, cur)
	if context.Cause(ctx) != errAccessChanged {
		t.Fatalf("a visit of the former provider: %v", context.Cause(ctx))
	}
	p.visits.detach(a)
}

// The visit metrics are written.
func TestVisitMetrics(t *testing.T) {
	p, _, _, _ := visitProxy(t)
	serveCall(p, nav("/"))
	serveCall(p, fetch("/"))
	var b strings.Builder
	p.Metrics(&b)
	for _, want := range []string{"ferun_ingress_visit_sign_ins_total 1", "ferun_ingress_visit_required_total 1",
		"ferun_ingress_visit_rejected_total 0", "ferun_ingress_visit_redeemed_total 0", "ferun_ingress_visit_renewed_total 0",
		"ferun_ingress_visit_ended_total 0", "ferun_ingress_visit_failed_total 0", "ferun_ingress_visit_unavailable_total 0"} {
		if !strings.Contains(b.String(), want+"\n") {
			t.Errorf("metrics lack %q", want)
		}
	}
}

// The visit book stays bounded and forgets what no longer counts.
func TestVisitBookBounds(t *testing.T) {
	b := newVisitBook()
	now := time.Now()
	b.mu.Lock()
	for i := range maxEndedVisits + 10 {
		b.endLocked("v"+strconv.Itoa(i), "", "expired", now.Add(time.Duration(i+1)*time.Second).Unix(), now)
	}
	n := len(b.ended)
	b.mu.Unlock()
	if n != maxEndedVisits {
		t.Fatalf("%d ended", n)
	}
	b.mu.Lock()
	b.pruneLocked(now.Add(48 * time.Hour))
	n = len(b.ended)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d ended after their end", n)
	}
}

// httptraceHints is a context whose client trace collects the headers of
// the 1xx answers.
func httptraceHints(into *[]http.Header) context.Context {
	return httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
			*into = append(*into, http.Header(h).Clone())
			return nil
		},
	})
}
