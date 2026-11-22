// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The preflight against a local TLS server that plays a Kubernetes API
// server: /version and the SelfSubjectReview. The server listens on
// 127.0.0.1, which the destination rule denies, so these tests swap in a
// policy that lets loopback through, and one shows that the production
// policy refuses it, at validation and at dial.

// fakeAPIServer answers like a Kubernetes API server: /version, and a
// SelfSubjectReview naming the bearer token's or client certificate's
// user. review sets the review's status code (0: 201).
type fakeAPIServer struct {
	srv    *httptest.Server
	review atomic.Int64
	hits   atomic.Int64
	agents chan string
}

const goodToken = "good-token"

func (f *fakeAPIServer) handler(w http.ResponseWriter, r *http.Request) {
	f.hits.Add(1)
	select {
	case f.agents <- r.Header.Get("User-Agent"):
	default:
	}
	user := ""
	switch {
	case r.Header.Get("Authorization") == "Bearer "+goodToken:
		user = "system:serviceaccount:smelter:fecode"
	case r.TLS != nil && len(r.TLS.PeerCertificates) > 0:
		user = r.TLS.PeerCertificates[0].Subject.CommonName
	}
	switch {
	case user == "":
		http.Error(w, `{"kind":"Status","code":401}`, http.StatusUnauthorized)
	case r.URL.Path == "/version" && r.Method == http.MethodGet:
		_, _ = io.WriteString(w, `{"major":"1","minor":"31","gitVersion":"v1.31.2"}`)
	case r.URL.Path == "/apis/authentication.k8s.io/v1/selfsubjectreviews" && r.Method == http.MethodPost:
		var body struct{ APIVersion, Kind string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Kind != "SelfSubjectReview" || body.APIVersion != "authentication.k8s.io/v1" {
			http.Error(w, "bad review", http.StatusBadRequest)
			return
		}
		if code := f.review.Load(); code != 0 {
			http.Error(w, "nope", int(code))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"userInfo": map[string]any{
			"username": user, "groups": []string{"system:serviceaccounts", "system:authenticated"}}}})
	default:
		http.NotFound(w, r)
	}
}

// newFakeAPIServer starts it with httptest's own certificate (valid for
// 127.0.0.1) and returns it with that certificate as a CA PEM.
func newFakeAPIServer(t *testing.T) (*fakeAPIServer, string) {
	f := &fakeAPIServer{agents: make(chan string, 16)}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.handler))
	f.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}))
}

func (r *rig) kubePreflight(body map[string]any) (int, string, string) {
	r.t.Helper()
	b, _ := json.Marshal(body)
	code, raw := r.projectCall(http.MethodPost, "/api/v1/kube/preflight", string(b))
	_, c := codeOf(raw)
	return code, raw, c
}

func kubePreflightBody(server, ca, token string) map[string]any {
	b := map[string]any{"server": server, "auth": map[string]any{"type": "token", "token": token}}
	if ca != "" {
		b["caPem"] = ca
	}
	return b
}

func (r *rig) allowLoopback() {
	r.svc.dest.allowDest, r.svc.dest.allowDial = allowLoopback, allowLoopback
}

func TestKubePreflightToken(t *testing.T) {
	r := newRig(t)
	r.allowLoopback()
	api, ca := newFakeAPIServer(t)

	code, raw, _ := r.kubePreflight(kubePreflightBody(api.srv.URL, ca, goodToken))
	if code != http.StatusOK {
		t.Fatalf("preflight: %d %s", code, raw)
	}
	var res struct {
		ServerVersion string
		User          *struct {
			Username string
			Groups   []string
		}
	}
	_ = json.Unmarshal([]byte(raw), &res)
	if res.ServerVersion != "v1.31.2" || res.User == nil ||
		res.User.Username != "system:serviceaccount:smelter:fecode" || len(res.User.Groups) != 2 {
		t.Fatalf("preflight answer: %s", raw)
	}
	if ua := <-api.agents; ua != "ferund-kube-preflight" {
		t.Fatalf("User-Agent %q", ua)
	}
	// name and port are optional and ignored; a trailing slash is fine.
	b := kubePreflightBody(api.srv.URL+"/", ca, goodToken)
	b["name"], b["port"] = "Not A Label", 1
	if code, raw, _ := r.kubePreflight(b); code != http.StatusOK {
		t.Fatalf("preflight with ignored fields: %d %s", code, raw)
	}
	// Nothing is stored.
	if v, rows := r.kubeRows(); v != "" || len(rows) != 0 {
		t.Fatalf("preflight stored: %q %+v", v, rows)
	}

	// A review the server refuses (old cluster, missing RBAC): the
	// credential works, identity unknown.
	api.review.Store(http.StatusNotFound)
	code, raw, _ = r.kubePreflight(kubePreflightBody(api.srv.URL, ca, goodToken))
	if code != http.StatusOK || !strings.Contains(raw, `"user":null`) || !strings.Contains(raw, "v1.31.2") {
		t.Fatalf("review 404: %d %s", code, raw)
	}
	api.review.Store(http.StatusForbidden)
	if code, raw, _ = r.kubePreflight(kubePreflightBody(api.srv.URL, ca, goodToken)); code != http.StatusOK ||
		!strings.Contains(raw, `"user":null`) {
		t.Fatalf("review 403: %d %s", code, raw)
	}
}

func TestKubePreflightClientCert(t *testing.T) {
	r := newRig(t)
	r.allowLoopback()
	ca := newTestCA(t, "cluster-ca")
	_, _, serverPair := ca.issue(t, "kube-apiserver", []net.IP{net.ParseIP("127.0.0.1")})
	clientCert, clientKey, _ := ca.issue(t, "fecode-admin", nil)
	f := &fakeAPIServer{agents: make(chan string, 16)}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.handler))
	f.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverPair},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)

	body := func(cert, key string) map[string]any {
		return map[string]any{"server": f.srv.URL, "caPem": ca.caPEM,
			"auth": map[string]any{"type": "client_cert", "certPem": cert, "keyPem": key}}
	}
	code, raw, _ := r.kubePreflight(body(clientCert, clientKey))
	if code != http.StatusOK || !strings.Contains(raw, `"username":"fecode-admin"`) {
		t.Fatalf("client cert preflight: %d %s", code, raw)
	}
	// A certificate the cluster does not trust fails the handshake.
	other := newTestCA(t, "other-ca")
	oc, ok, _ := other.issue(t, "intruder", nil)
	if code, raw, c := r.kubePreflight(body(oc, ok)); code != http.StatusUnprocessableEntity || c != codeTLSFailed {
		t.Fatalf("untrusted client cert: %d %s", code, raw)
	}
	// So does a server whose CA was not given (system roots).
	b := body(clientCert, clientKey)
	delete(b, "caPem")
	if code, raw, c := r.kubePreflight(b); code != http.StatusUnprocessableEntity || c != codeTLSFailed {
		t.Fatalf("unknown server CA: %d %s", code, raw)
	}
	// And a server name the certificate does not carry.
	b = body(clientCert, clientKey)
	b["serverName"] = "kubernetes.example"
	if code, raw, c := r.kubePreflight(b); code != http.StatusUnprocessableEntity || c != codeTLSFailed {
		t.Fatalf("wrong server name: %d %s", code, raw)
	}
}

func TestKubePreflightFailures(t *testing.T) {
	r := newRig(t)
	r.allowLoopback()
	api, ca := newFakeAPIServer(t)

	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "https://" + l.Addr().String()
	l.Close()
	notK8s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"hello":"world"}`)
	}))
	notK8s.Config.ErrorLog = log.New(io.Discard, "", 0)
	notK8s.StartTLS()
	t.Cleanup(notK8s.Close)
	notK8sCA := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: notK8s.Certificate().Raw}))

	for _, c := range []struct {
		name   string
		body   map[string]any
		status int
		code   string
	}{
		{"wrong token", kubePreflightBody(api.srv.URL, ca, "bad-token"), http.StatusUnprocessableEntity, codeUnauthorized},
		{"no CA, system roots", kubePreflightBody(api.srv.URL, "", goodToken), http.StatusUnprocessableEntity, codeTLSFailed},
		{"nothing listening", kubePreflightBody(closed, ca, goodToken), http.StatusUnprocessableEntity, codeUnreachable},
		{"not kubernetes", kubePreflightBody(notK8s.URL, notK8sCA, goodToken), http.StatusUnprocessableEntity, codeUnreachable},
		{"plaintext server", kubePreflightBody(strings.Replace(api.srv.URL, "https", "http", 1), ca, goodToken),
			http.StatusUnprocessableEntity, codeInsecureServer},
		{"bad CA", kubePreflightBody(api.srv.URL, "garbage", goodToken), http.StatusUnprocessableEntity, codeBadCA},
		{"internal destination", kubePreflightBody("https://10.0.0.10:6443", "", goodToken),
			http.StatusUnprocessableEntity, codeDestinationDenied},
		{"malformed: no auth", map[string]any{"server": api.srv.URL}, http.StatusBadRequest, codeInvalidBinding},
	} {
		code, raw, got := r.kubePreflight(c.body)
		if code != c.status || got != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, code, strings.TrimSpace(raw), c.status, c.code)
		}
	}
	if code, raw := r.projectCall(http.MethodPost, "/api/v1/kube/preflight", `{"server":`); code != http.StatusBadRequest ||
		!strings.Contains(raw, codeInvalidBinding) {
		t.Errorf("bad JSON: %d %s", code, raw)
	}
}

// Redirects are not followed: the credential goes to the server the
// binding names and nowhere else.
func TestKubePreflightNoRedirects(t *testing.T) {
	r := newRig(t)
	r.allowLoopback()
	api, _ := newFakeAPIServer(t)
	redirector := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, api.srv.URL+"/version", http.StatusFound)
	}))
	redirector.Config.ErrorLog = log.New(io.Discard, "", 0)
	redirector.StartTLS()
	t.Cleanup(redirector.Close)
	rca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: redirector.Certificate().Raw}))

	code, raw, c := r.kubePreflight(kubePreflightBody(redirector.URL, rca, goodToken))
	if code != http.StatusUnprocessableEntity || c != codeUnreachable {
		t.Fatalf("redirect: %d %s", code, raw)
	}
	if api.hits.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

// The destination rule twice: the production policy refuses the loopback
// server at validation; and with validation fooled, the dialer's own
// check still refuses the address it would connect to: the server never
// sees a request.
func TestKubePreflightDestinationRuleAtDial(t *testing.T) {
	r := newRig(t)
	api, ca := newFakeAPIServer(t)

	if code, raw, c := r.kubePreflight(kubePreflightBody(api.srv.URL, ca, goodToken)); code != http.StatusUnprocessableEntity ||
		c != codeDestinationDenied {
		t.Fatalf("production policy at validation: %d %s", code, raw)
	}
	r.svc.dest.allowDest = allowLoopback // validation passes; allowDial stays the deny policy
	code, raw, c := r.kubePreflight(kubePreflightBody(api.srv.URL, ca, goodToken))
	if code != http.StatusUnprocessableEntity || c != codeDestinationDenied {
		t.Fatalf("production policy at dial: %d %s", code, raw)
	}
	if api.hits.Load() != 0 {
		t.Fatal("the dial check let a request through")
	}
}

// The budget (shortened here) covers the whole exchange.
func TestKubePreflightBudget(t *testing.T) {
	r := newRig(t)
	r.allowLoopback()
	r.svc.budget = 200 * time.Millisecond
	release := make(chan struct{})
	hang := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-req.Context().Done():
		case <-release:
		}
	}))
	hang.Config.ErrorLog = log.New(io.Discard, "", 0)
	hang.StartTLS()
	t.Cleanup(hang.Close)
	t.Cleanup(func() { close(release) })
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: hang.Certificate().Raw}))
	start := time.Now()
	code, raw, c := r.kubePreflight(kubePreflightBody(hang.URL, ca, goodToken))
	if code != http.StatusUnprocessableEntity || c != codeUnreachable {
		t.Fatalf("hanging server: %d %s", code, raw)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the budget did not hold: %s", d)
	}
}
