// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// catcher is a TLS server that records every request it gets, its
// certificate of a CA of its own (ca), which a draft can trust it by.
type catcher struct {
	srv *httptest.Server
	ca  string
	mu  sync.Mutex
	got []string // each request's headers and body
}

func newCatcher(t *testing.T) *catcher {
	t.Helper()
	c := &catcher{}
	c.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var b strings.Builder
		_ = r.Header.Write(&b)
		b.Write(body)
		c.mu.Lock()
		c.got = append(c.got, b.String())
		c.mu.Unlock()
		_, _ = io.WriteString(w, `{"access_token":"caught","token_type":"Bearer"}`)
	}))
	c.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	ca := newTestCA(t, "catcher")
	_, _, pair := ca.issue(t, "127.0.0.1", []net.IP{net.IPv4(127, 0, 0, 1)})
	c.srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	c.srv.StartTLS()
	t.Cleanup(c.srv.Close)
	c.ca = ca.caPEM
	return c
}

func (c *catcher) requests() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.got...)
}

// A secret field {"set": true} keeps the latest version's value only for
// a definition that sends it where that version does. A check or a PUT
// whose upstream, CA or token URL is another is 422 and calls no one:
// write on the environment never makes ferund send a stored secret to a
// host of the writer's choice, nor trust a CA of the writer's for it. The
// same place written another way keeps, and so does another mode, which
// says how the code calls the API, not where the key goes (ADR 0055).
func TestKeptSecretStaysWhereItWent(t *testing.T) {
	r := newRig(t)
	r.allowLoopback()
	f, ca := newFakeAPI(t)
	evil := newCatcher(t)
	set := map[string]any{"set": true}
	tokenURL := f.srv.URL + "/oauth/token"
	defs := map[string]map[string]any{
		"api": {"upstream": f.srv.URL, "caPem": ca,
			"credential": map[string]any{"kind": "headers", "headers": map[string]any{"X-API-Key": httpAPIKey}}},
		"sso": {"upstream": f.srv.URL, "caPem": ca, "credential": map[string]any{"kind": "oauth2", "tokenUrl": tokenURL,
			"clientId": fakeClientID, "clientSecret": fakeClientSec}},
		"erp": {"upstream": "https://api.erp.example.test",
			"credential": map[string]any{"kind": "basic", "user": "fecode", "password": httpPassword}},
	}
	for name, def := range defs {
		if code, raw := r.projectCall("PUT", credPath("box", name), mustJSON(t, def)); code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, code, raw)
		}
	}
	keptKey := map[string]any{"kind": "headers", "headers": map[string]any{"X-API-Key": set}}
	keptPassword := map[string]any{"kind": "basic", "user": "fecode", "password": set}
	probe := map[string]any{"method": "GET", "path": "/odata/ok"}

	moves := []struct {
		what, cred, field string
		draft             map[string]any
	}{
		{"another upstream", "api", "upstream", map[string]any{"upstream": evil.srv.URL, "caPem": evil.ca, "credential": keptKey}},
		{"no upstream", "api", "upstream", map[string]any{"caPem": ca, "credential": keptKey}},
		{"an upstream that is none", "api", "upstream", map[string]any{"upstream": "evil.example", "credential": keptKey}},
		{"another CA", "api", "caPem", map[string]any{"upstream": f.srv.URL, "caPem": evil.ca, "credential": keptKey}},
		{"no CA", "api", "caPem", map[string]any{"upstream": f.srv.URL, "credential": keptKey}},
		{"another token URL", "sso", "tokenUrl", map[string]any{"upstream": f.srv.URL, "caPem": ca,
			"credential": map[string]any{"kind": "oauth2", "tokenUrl": evil.srv.URL + "/oauth/token",
				"clientId": fakeClientID, "clientSecret": set}}},
	}
	refused := func(what, call string, code int, raw, field string) {
		t.Helper()
		if code != http.StatusUnprocessableEntity || !strings.Contains(raw, "whose "+field+" is another") ||
			!strings.Contains(raw, "re-enter the secret fields when the upstream, CA or token URL change") {
			t.Errorf("%s, %s: %d %s", what, call, code, raw)
		}
		for _, secret := range []string{httpAPIKey, httpPassword, fakeClientSec} {
			if strings.Contains(raw, secret) {
				t.Errorf("%s, %s: the answer carries a secret: %s", what, call, raw)
			}
		}
	}
	for _, m := range moves {
		code, raw := r.projectCall("POST", credPath("box", m.cred)+"/check",
			mustJSON(t, map[string]any{"binding": m.draft, "probe": probe}))
		refused(m.what, "check", code, raw, m.field)
		code, raw = r.projectCall("PUT", credPath("box", m.cred), mustJSON(t, m.draft))
		refused(m.what, "put", code, raw, m.field)
		if c, err := r.st.EnvCredential(bg, r.box, m.cred, 0); err != nil || c.Version != 1 {
			t.Errorf("%s: the latest version is %+v %v, want version 1 alone", m.what, c, err)
		}
	}
	if got := evil.requests(); len(got) != 0 {
		t.Fatalf("the other server was called %d times: %q", len(got), got)
	}

	// The same place in another spelling keeps the value: the check
	// reaches the upstream with the stored key, and the PUT makes version
	// 2 with it.
	same := map[string]any{"upstream": "HTTPS://" + strings.TrimPrefix(f.srv.URL, "https://") + "/", "mode": "name",
		"caPem": "\n" + ca + "\n", "rateLimit": map[string]any{"perSecond": 5}, "credential": keptKey}
	code, raw := r.projectCall("POST", credPath("box", "api")+"/check", mustJSON(t, map[string]any{"binding": same, "probe": probe}))
	if code != http.StatusOK || !strings.Contains(raw, `"ok":true`) || !strings.Contains(raw, `"status":200`) {
		t.Fatalf("check in the same place: %d %s", code, raw)
	}
	if code, raw := r.projectCall("PUT", credPath("box", "api"), mustJSON(t, same)); code != http.StatusCreated {
		t.Fatalf("put in the same place: %d %s", code, raw)
	}
	b, err := r.svc.OpenCredential(bg, r.box, "api", 2, "api")
	if err != nil || b.GetHeaders().GetHeaders()["X-Api-Key"] != httpAPIKey {
		t.Fatalf("version 2 holds %v %v, want the kept key", b, err)
	}

	// Another mode, the forwarded host and the prefixes keep it too: the
	// key goes to the same API.
	moved := map[string]any{"upstream": "https://api.erp.example.test", "mode": "intercept",
		"pathPrefixes": []string{"/odata/"}, "credential": keptPassword}
	if code, raw := r.projectCall("PUT", credPath("box", "erp"), mustJSON(t, moved)); code != http.StatusCreated {
		t.Fatalf("put in another mode: %d %s", code, raw)
	}
	b, err = r.svc.OpenCredential(bg, r.box, "erp", 2, "erp")
	if err != nil || b.GetBasic().GetPassword() != httpPassword || b.GetMode().String() != "HTTP_BINDING_MODE_INTERCEPT" {
		t.Fatalf("erp version 2 holds %v %v, want the kept password in the intercept mode", b, err)
	}
}
