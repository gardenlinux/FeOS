// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func prefixes(ss ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

func TestJudge(t *testing.T) {
	r := Rule{Deny: DefaultDeny(), Allow: prefixes("10.1.0.0/16")}
	for addr, want := range map[string]string{
		"93.184.215.14":    "",
		"10.0.0.1":         "10.0.0.1 is an internal address (10.0.0.0/8)",
		"10.1.2.3":         "", // the allowed block wins over the denied one
		"::ffff:10.0.0.1":  "10.0.0.1 is an internal address (10.0.0.0/8)",
		"::ffff:1.1.1.1":   "",
		"2001:db8::1":      "2001:db8::1 is not an IPv4 address (the platform reaches servers over IPv4 only)",
		"::1":              "::1 is not an IPv4 address (the platform reaches servers over IPv4 only)",
		"127.0.0.1":        "127.0.0.1 is an internal address (127.0.0.0/8)",
		"169.254.169.254":  "169.254.169.254 is an internal address (169.254.0.0/16)",
		"100.64.0.1":       "100.64.0.1 is an internal address (100.64.0.0/10)",
		"0.0.0.0":          "0.0.0.0 is an internal address (0.0.0.0/8)",
		"224.0.0.1":        "224.0.0.1 is an internal address (224.0.0.0/4)",
		"255.255.255.255":  "255.255.255.255 is an internal address (240.0.0.0/4)",
		"172.31.255.255":   "172.31.255.255 is an internal address (172.16.0.0/12)",
		"192.168.1.1":      "192.168.1.1 is an internal address (192.168.0.0/16)",
		"172.32.0.1":       "",
		"100.128.0.1":      "",
		"11.0.0.1":         "",
		"192.169.0.1":      "",
		"9.255.255.255":    "",
		"126.255.255.255":  "",
		"223.255.255.255":  "",
		"169.253.255.255":  "",
		"172.15.255.255":   "",
		"100.63.255.255":   "",
		"1.0.0.0":          "",
		"128.0.0.1":        "",
		"191.255.255.255":  "",
		"192.167.255.255":  "",
		"200.200.200.200":  "",
		"8.8.8.8":          "",
		"203.0.113.5":      "",
		"198.51.100.1":     "",
		"192.0.2.1":        "",
		"100.127.255.255":  "100.127.255.255 is an internal address (100.64.0.0/10)",
		"10.255.255.255":   "10.255.255.255 is an internal address (10.0.0.0/8)",
		"239.255.255.255":  "239.255.255.255 is an internal address (224.0.0.0/4)",
		"240.0.0.1":        "240.0.0.1 is an internal address (240.0.0.0/4)",
		"127.255.255.255":  "127.255.255.255 is an internal address (127.0.0.0/8)",
		"169.254.0.1":      "169.254.0.1 is an internal address (169.254.0.0/16)",
		"172.16.0.0":       "172.16.0.0 is an internal address (172.16.0.0/12)",
		"192.168.255.255":  "192.168.255.255 is an internal address (192.168.0.0/16)",
		"::ffff:127.0.0.1": "127.0.0.1 is an internal address (127.0.0.0/8)",
	} {
		err := r.Judge(netip.MustParseAddr(addr))
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != want {
			t.Errorf("%s: %q, want %q", addr, got, want)
		}
	}
	if err := (Rule{}).Judge(netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Errorf("an empty rule refuses %v", err)
	}
	if len(DefaultDeny()) != 9 {
		t.Errorf("DefaultDeny %v", DefaultDeny())
	}
}

func TestDeniedError(t *testing.T) {
	err := &DeniedError{errors.New("10.0.0.1 is an internal address (10.0.0.0/8)")}
	if err.Error() != "destination denied: 10.0.0.1 is an internal address (10.0.0.0/8)" {
		t.Errorf("text %q", err)
	}
	wrapped := &net.OpError{Op: "dial", Err: err}
	if !IsDenied(wrapped) || IsDenied(errors.New("x")) || IsDenied(nil) {
		t.Error("IsDenied")
	}
	if errors.Unwrap(err).Error() != "10.0.0.1 is an internal address (10.0.0.0/8)" {
		t.Error("Unwrap")
	}
}

// A name is refused when any of its addresses is, and the refusal names
// none of them; a literal is named as given.
func TestCheckHost(t *testing.T) {
	answers := map[string][]netip.Addr{
		"public.example": {netip.MustParseAddr("93.184.215.14")},
		"mixed.example":  {netip.MustParseAddr("93.184.215.14"), netip.MustParseAddr("10.20.30.40")},
		"empty.example":  nil,
	}
	saved := lookupIPv4
	lookupIPv4 = func(ctx context.Context, host string) ([]netip.Addr, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("a lookup without a deadline")
		}
		a, ok := answers[host]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return a, nil
	}
	t.Cleanup(func() { lookupIPv4 = saved })
	judge := Rule{Deny: DefaultDeny()}.Judge
	ctx := context.Background()
	if err := CheckHost(ctx, judge, "public.example"); err != nil {
		t.Errorf("public: %v", err)
	}
	err := CheckHost(ctx, judge, "mixed.example")
	if !IsDenied(err) || strings.Contains(err.Error(), "10.20") || strings.Contains(err.Error(), "93.184") {
		t.Errorf("mixed: %v", err)
	}
	for _, h := range []string{"missing.example", "empty.example"} {
		err := CheckHost(ctx, judge, h)
		if err == nil || IsDenied(err) || err.Error() != h+" does not resolve to an IPv4 address" {
			t.Errorf("%s: %v", h, err)
		}
	}
	if err := CheckHost(ctx, judge, "10.0.0.1"); !IsDenied(err) || !strings.Contains(err.Error(), "10.0.0.1") {
		t.Errorf("a denied literal: %v", err)
	}
	if err := CheckHost(ctx, judge, "2001:db8::1"); !IsDenied(err) {
		t.Errorf("an IPv6 literal: %v", err)
	}
	if err := CheckHost(ctx, judge, "1.1.1.1"); err != nil {
		t.Errorf("a public literal: %v", err)
	}
}

// The dialer refuses a denied address at Control, before connecting.
func TestDialerControl(t *testing.T) {
	lis, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	d := Dialer(Rule{Deny: DefaultDeny()}.Judge, time.Second)
	_, err = d.Dial("tcp4", lis.Addr().String())
	var de *DeniedError
	if !errors.As(err, &de) || !strings.Contains(err.Error(), "127.0.0.1 is an internal address (127.0.0.0/8)") {
		t.Fatalf("dial a denied literal: %v", err)
	}
	if d.Timeout != time.Second {
		t.Errorf("timeout %v", d.Timeout)
	}
	allowed := Dialer(Rule{Deny: DefaultDeny(), Allow: prefixes("127.0.0.0/8")}.Judge, time.Second)
	c, err := allowed.Dial("tcp4", lis.Addr().String())
	if err != nil {
		t.Fatalf("dial an allowed address: %v", err)
	}
	c.Close()
	// Control saw the refused dial before any connection: only the
	// allowed one arrived.
	deadline := time.Now().Add(2 * time.Second)
	for accepted.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := accepted.Load(); n != 1 {
		t.Errorf("%d connections accepted, want 1", n)
	}
}

// tlsServer is an httptest TLS server and the pool of its certificate.
func tlsServer(t *testing.T, h http.Handler) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool
}

// loopback allows the test servers' address.
var loopback = Rule{Deny: DefaultDeny(), Allow: prefixes("127.0.0.0/8")}.Judge

func TestClient(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.ProtoMajor != 1 {
			t.Errorf("protocol %s", r.Proto)
		}
		_, _ = io.WriteString(w, "ok")
	})
	srv, pool := tlsServer(t, mux)
	mux.HandleFunc("/to-ok", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/ok", http.StatusFound)
	})
	mux.HandleFunc("/to-http", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(srv.URL, "https://", "http://", 1)+"/ok", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/loop", http.StatusFound)
	})

	// The server's certificate verifies with its pool, and with the
	// system's roots it does not.
	c := Client(loopback, 5*time.Second, false, pool)
	resp, err := c.Get(srv.URL + "/ok")
	if err != nil {
		t.Fatalf("GET with the pool: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "ok" || resp.Proto != "HTTP/1.1" {
		t.Errorf("answer %q %s", b, resp.Proto)
	}
	if _, err := Client(loopback, 5*time.Second, false, nil).Get(srv.URL + "/ok"); err == nil || IsDenied(err) {
		t.Errorf("GET without the pool: %v", err)
	}

	// Without the allowed block the loopback server is refused at the
	// dial.
	before := hits.Load()
	if _, err := Client(Rule{Deny: DefaultDeny()}.Judge, 5*time.Second, false, pool).Get(srv.URL + "/ok"); !IsDenied(err) {
		t.Errorf("GET a denied address: %v", err)
	}
	if hits.Load() != before {
		t.Error("the denied GET arrived")
	}

	// An http URL is refused before anything is dialled.
	var judged atomic.Int32
	counting := func(a netip.Addr) error { judged.Add(1); return loopback(a) }
	plain := httptest.NewServer(mux)
	defer plain.Close()
	if _, err := Client(counting, 5*time.Second, true, pool).Get(plain.URL + "/ok"); !IsDenied(err) {
		t.Errorf("GET http: %v", err)
	}
	if judged.Load() != 0 || hits.Load() != before {
		t.Errorf("an http URL was dialled (%d) or served", judged.Load())
	}

	// No redirects: the redirect itself is the answer.
	resp, err = c.Get(srv.URL + "/to-ok")
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("no redirects: %v %v", resp, err)
	}
	resp.Body.Close()

	// Redirects of a GET are followed, to https only, at most 10.
	follow := Client(loopback, 5*time.Second, true, pool)
	resp, err = follow.Get(srv.URL + "/to-ok")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("following: %v %v", resp, err)
	}
	resp.Body.Close()
	if _, err := follow.Get(srv.URL + "/to-http"); !IsDenied(err) {
		t.Errorf("a redirect to http: %v", err)
	}
	if _, err := follow.Get(srv.URL + "/loop"); err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Errorf("a redirect loop: %v", err)
	}
	// A POST follows none, even when told to follow.
	resp, err = follow.Post(srv.URL+"/to-ok", "application/json", strings.NewReader("{}"))
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("POST: %v %v", resp, err)
	}
	resp.Body.Close()

	// The timeout bounds the whole request.
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	start := time.Now()
	if _, err := Client(loopback, 200*time.Millisecond, false, pool).Get(srv.URL + "/slow"); err == nil {
		t.Error("a slow answer within the timeout")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the timeout took %v", time.Since(start))
	}
}

func TestPrefixList(t *testing.T) {
	var l PrefixList
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Var(&l, "deny-prefix", "")
	if err := fs.Parse([]string{"--deny-prefix", "default, 100.144.0.0/13", "--deny-prefix", "100.152.1.2/13"}); err != nil {
		t.Fatal(err)
	}
	want := append(DefaultDeny(), prefixes("100.144.0.0/13", "100.152.0.0/13")...)
	if l.String() != (*PrefixList)(&want).String() {
		t.Errorf("list %s", l.String())
	}
	var only PrefixList
	if err := only.Set("192.0.2.0/24"); err != nil || only.String() != "192.0.2.0/24" {
		t.Errorf("a value without default: %s %v", only.String(), err)
	}
	if err := only.Set("not-a-prefix"); err == nil {
		t.Error("a bad prefix accepted")
	}
	var empty PrefixList
	if err := empty.Set(" , "); err != nil || len(empty) != 0 {
		t.Errorf("blanks: %v %v", empty, err)
	}
}
