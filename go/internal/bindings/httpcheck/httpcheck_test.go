// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package httpcheck

import "testing"

func TestParseUpstream(t *testing.T) {
	for _, c := range []struct {
		in, origin, code string
		port             int
	}{
		{"https://API.erp.example", "https://api.erp.example", "", 443},
		{"https://api.erp.example:443/", "https://api.erp.example", "", 443},
		{"https://api.erp.example:8443", "https://api.erp.example:8443", "", 8443},
		{"https://203.0.113.4", "https://203.0.113.4", "", 443},
		{"http://api.erp.example", "", CodeInsecureUpstream, 0},
		{"https://api.erp.example/x", "", CodeInvalidBinding, 0},
		{"https://[2001:db8::1]", "", CodeInvalidBinding, 0},
		{"https://a_b.example", "", CodeInvalidBinding, 0},
		{"https://api.erp.example:0", "", CodeInvalidBinding, 0},
		{"ftp://api.erp.example", "", CodeInvalidBinding, 0},
	} {
		up, err := ParseUpstream("upstream", c.in)
		switch {
		case c.code == "" && (err != nil || up.Origin() != c.origin || up.Port != c.port):
			t.Errorf("%s: %+v %v", c.in, up, err)
		case c.code != "" && (err == nil || err.Code != c.code):
			t.Errorf("%s: %v, want %s", c.in, err, c.code)
		}
	}
}

func TestWithinPrefix(t *testing.T) {
	for _, c := range []struct {
		path, prefix string
		in           bool
	}{
		{"/api", "/api", true}, {"/api/x", "/api", true}, {"/apix", "/api", false},
		{"/api/x", "/api/", true}, {"/api", "/api/", false}, {"/", "/", true}, {"/x", "/", true},
	} {
		if got := WithinPrefix(c.path, c.prefix); got != c.in {
			t.Errorf("%s in %s: %v", c.path, c.prefix, got)
		}
	}
}

func TestPathPrefixes(t *testing.T) {
	if err := PathPrefixes("p", []string{"/odata/", "/api"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{{"api"}, {"/a/../b"}, {"/a/./b"}, {`/a\b`}, {"/a%2Fb"}, {"/a%2eb"}, {"/a?x"},
		{"/a b"}, {"/a", "/a"}} {
		if err := PathPrefixes("p", bad); err == nil || err.Code != CodeInvalidBinding {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestModeAndHeaders(t *testing.T) {
	up, _ := ParseUpstream("u", "https://api.erp.example:8443")
	if _, err := Mode("m", ModeIntercept, up); err == nil {
		t.Error("intercept on 8443")
	}
	if m, err := Mode("m", "", up); err != nil || m != ModeName {
		t.Errorf("default mode: %q %v", m, err)
	}
	for _, h := range []string{"Host", "host", "Connection", "Proxy-Authorization", "Transfer-Encoding", "X-Forwarded-For",
		"FeOS-Binding-Error", "X Key", ""} {
		if err := HeaderName("h", h); err == nil || err.Code != CodeBadHeader {
			t.Errorf("header %q: %v", h, err)
		}
	}
	for _, h := range []string{"Authorization", "X-API-Key", "apikey"} {
		if err := HeaderName("h", h); err != nil {
			t.Errorf("header %q: %v", h, err)
		}
	}
	if err := HeaderValue("v", "a\r\nb"); err == nil {
		t.Error("a value with a line break")
	}
	if _, b, err := RateLimit("r", 5, 0); err != nil || b != 5 {
		t.Errorf("burst: %d %v", b, err)
	}
}
