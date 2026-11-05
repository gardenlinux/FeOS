// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The HTTP preflight against a local TLS server that plays an API and its
// OAuth token endpoint, on 127.0.0.1, which the tests' policy lets through
// (and the production policy refuses).

type fakeAPI struct {
	srv     *httptest.Server
	tokens  atomic.Int64
	lastTok atomic.Value
}

const (
	fakeToken     = "access-token-PLAINTEXT-99"
	fakeClientID  = "fecode-client"
	fakeClientSec = "client-secret-PLAINTEXT-7"
)

func (f *fakeAPI) handler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/oauth/token":
		f.tokens.Add(1)
		_ = r.ParseForm()
		id, secret, ok := r.BasicAuth()
		if !ok {
			id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		}
		if r.Method != http.MethodPost || r.PostForm.Get("grant_type") != "client_credentials" ||
			id != fakeClientID || secret != fakeClientSec {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
			return
		}
		f.lastTok.Store(r.PostForm.Get("scope"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"`+fakeToken+`","token_type":"Bearer","expires_in":3599}`)
	case "/odata/ok":
		switch {
		case r.Header.Get("X-API-Key") == httpAPIKey, r.Header.Get("Authorization") == "Bearer "+fakeToken:
		default:
			if u, p, ok := r.BasicAuth(); !ok || u != "fecode" || p != httpPassword {
				http.Error(w, "who are you", http.StatusUnauthorized)
				return
			}
		}
		_, _ = io.WriteString(w, `{"secret body":"never answered"}`)
	default:
		http.NotFound(w, r)
	}
}

func newFakeAPI(t *testing.T) (*fakeAPI, string) {
	t.Helper()
	f := &fakeAPI{}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.handler))
	f.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}))
}

func (r *rig) httpPreflight(body map[string]any) HTTPPreflightResult {
	r.t.Helper()
	b, _ := json.Marshal(body)
	code, raw := r.tenantCall(http.MethodPost, "/api/v1/http/preflight", string(b))
	if code != http.StatusOK {
		r.t.Fatalf("preflight: %d %s", code, raw)
	}
	if strings.Contains(raw, "never answered") || strings.Contains(raw, fakeToken) {
		r.t.Fatalf("the preflight answered a body or a token: %s", raw)
	}
	var res HTTPPreflightResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		r.t.Fatal(err)
	}
	return res
}

func TestHTTPPreflight(t *testing.T) {
	r := newRig(t)
	r.allowLoopback()
	f, ca := newFakeAPI(t)
	up := f.srv.URL // https://127.0.0.1:<port>
	probe := map[string]any{"method": "GET", "path": "/odata/ok"}

	// Headers, with a probe: the addresses, the certificate, the status.
	res := r.httpPreflight(map[string]any{"binding": map[string]any{"upstream": up, "caPem": ca,
		"credential": map[string]any{"kind": "headers", "headers": map[string]any{"X-API-Key": httpAPIKey}}},
		"probe": probe})
	if !res.OK || res.Error != nil || len(res.Addresses) != 1 || res.Addresses[0] != "127.0.0.1" || res.TLS == nil ||
		res.TLS.NotAfter.IsZero() || res.Probe == nil || res.Probe.Status != 200 || res.Token != nil {
		t.Fatalf("headers: %+v %+v", res, res.Error)
	}
	if !strings.Contains(res.TLS.Issuer, "Acme") {
		t.Fatalf("issuer %q", res.TLS.Issuer)
	}

	// Without a probe the credential is not sent, and the answer is ok.
	res = r.httpPreflight(map[string]any{"binding": map[string]any{"upstream": up, "caPem": ca,
		"credential": map[string]any{"kind": "headers", "headers": map[string]any{"X-API-Key": "wrong"}}}})
	if !res.OK || res.Probe != nil {
		t.Fatalf("no probe: %+v", res)
	}

	// A refused credential: not ok, unauthorized, the status kept.
	res = r.httpPreflight(map[string]any{"binding": map[string]any{"upstream": up, "caPem": ca,
		"credential": map[string]any{"kind": "headers", "headers": map[string]any{"X-API-Key": "wrong"}}},
		"probe": map[string]any{"method": "HEAD", "path": "/odata/ok"}})
	if res.OK || res.Error == nil || res.Error.Code != codeUnauthorized || res.Probe == nil || res.Probe.Status != 401 {
		t.Fatalf("refused: %+v %+v", res, res.Error)
	}

	// Basic.
	res = r.httpPreflight(map[string]any{"binding": map[string]any{"upstream": up, "caPem": ca,
		"credential": map[string]any{"kind": "basic", "user": "fecode", "password": httpPassword}}, "probe": probe})
	if !res.OK || res.Probe.Status != 200 {
		t.Fatalf("basic: %+v %+v", res, res.Error)
	}

	// OAuth 2.0 client credentials, the client in Basic and in the body:
	// the token's lifetime, never the token.
	for _, inBody := range []bool{false, true} {
		res = r.httpPreflight(map[string]any{"binding": map[string]any{"upstream": up, "caPem": ca,
			"credential": map[string]any{"kind": "oauth2", "tokenUrl": up + "/oauth/token", "clientId": fakeClientID,
				"clientSecret": fakeClientSec, "scope": "read", "clientAuthInBody": inBody}}, "probe": probe})
		if !res.OK || res.Token == nil || res.Token.ExpiresIn != 3599 || res.Probe.Status != 200 {
			t.Fatalf("oauth2 (in body %v): %+v %+v", inBody, res, res.Error)
		}
	}
	if f.lastTok.Load() != "read" {
		t.Fatalf("the token request's scope: %v", f.lastTok.Load())
	}
	res = r.httpPreflight(map[string]any{"binding": map[string]any{"upstream": up, "caPem": ca,
		"credential": map[string]any{"kind": "oauth2", "tokenUrl": up + "/oauth/token", "clientId": fakeClientID,
			"clientSecret": "wrong"}}})
	if res.OK || res.Error.Code != codeUnauthorized || !strings.Contains(res.Error.Message, "invalid_client") {
		t.Fatalf("a wrong client secret: %+v %+v", res, res.Error)
	}
	if strings.Contains(res.Error.Message, "wrong") {
		t.Fatalf("the error carries the secret: %s", res.Error.Message)
	}

	// TLS: the server's certificate is not the system's roots'.
	res = r.httpPreflight(map[string]any{"binding": map[string]any{"upstream": up,
		"credential": map[string]any{"kind": "headers", "headers": map[string]any{"X-API-Key": httpAPIKey}}}})
	if res.OK || res.Error.Code != codeTLSFailed || res.TLS != nil {
		t.Fatalf("an unknown CA: %+v %+v", res, res.Error)
	}

	// A probe outside the binding's prefixes, or with another method.
	res = r.httpPreflight(map[string]any{"binding": map[string]any{"upstream": up, "caPem": ca,
		"pathPrefixes": []string{"/api/"}, "credential": map[string]any{"kind": "headers",
			"headers": map[string]any{"X-API-Key": httpAPIKey}}}, "probe": probe})
	if res.OK || res.Error.Code != codeInvalidBinding || !strings.Contains(res.Error.Message, "outside") {
		t.Fatalf("outside the prefixes: %+v %+v", res, res.Error)
	}
	res = r.httpPreflight(map[string]any{"binding": map[string]any{"upstream": up, "caPem": ca,
		"credential": map[string]any{"kind": "headers", "headers": map[string]any{"X-API-Key": httpAPIKey}}},
		"probe": map[string]any{"method": "POST", "path": "/odata/ok"}})
	if res.OK || res.Error.Code != codeInvalidBinding {
		t.Fatalf("a POST probe: %+v %+v", res, res.Error)
	}

	// An invalid binding is a result too.
	res = r.httpPreflight(map[string]any{"binding": map[string]any{"upstream": "http://api.erp.example.test",
		"credential": map[string]any{"kind": "headers", "headers": map[string]any{"X-API-Key": httpAPIKey}}}})
	if res.OK || res.Error.Code != codeInsecureUpstream || len(res.Addresses) != 0 {
		t.Fatalf("plain http: %+v %+v", res, res.Error)
	}
	if code, _ := r.tenantCall(http.MethodPost, "/api/v1/http/preflight", "[1]"); code != http.StatusBadRequest {
		t.Fatalf("a body that is no object: %d", code)
	}
}

// The production policy refuses the loopback server at the resolution,
// and the dial refuses it even when the resolution let it through.
func TestHTTPPreflightDestinationRule(t *testing.T) {
	r := newRig(t)
	f, ca := newFakeAPI(t)
	body := map[string]any{"binding": map[string]any{"upstream": f.srv.URL, "caPem": ca,
		"credential": map[string]any{"kind": "headers", "headers": map[string]any{"X-API-Key": httpAPIKey}}}}
	res := r.httpPreflight(body)
	if res.OK || res.Error.Code != codeDestinationDenied || len(res.Addresses) != 0 {
		t.Fatalf("loopback: %+v %+v", res, res.Error)
	}
	r.svc.dest.allowDest = allowLoopback // the resolution passes; the dial's rule stays the deny policy
	res = r.httpPreflight(body)
	if res.OK || res.Error.Code != codeDestinationDenied || res.TLS != nil || len(res.Addresses) != 0 {
		t.Fatalf("loopback at the dial: %+v %+v", res, res.Error)
	}
	// The name of an internal host: no address it resolved to is answered.
	res = r.httpPreflight(map[string]any{"binding": map[string]any{"upstream": "https://mixed.test",
		"credential": map[string]any{"kind": "headers", "headers": map[string]any{"X-API-Key": httpAPIKey}}}})
	if res.OK || res.Error.Code != codeDestinationDenied || len(res.Addresses) != 0 ||
		strings.Contains(res.Error.Message, "192.168") || strings.Contains(res.Error.Message, "203.0.113") {
		t.Fatalf("a name with an internal address: %+v %+v", res, res.Error)
	}
}
