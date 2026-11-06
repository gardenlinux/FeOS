// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The rig stands the Service up on a fresh database behind a small mux
// with api's auth classes: a sandbox route takes a sandbox token that must
// name the path's sandbox (401 without one, 404 for another), a tenant
// route a tenant token. Tenant acme owns sb1, sb2 and sb3; tenant other
// owns nothing. The resolver of the destination rule is fakeLookup.

const (
	tenantToken = "tenant-acme"
	otherToken  = "tenant-other"
	sb1Token    = "tok-1"
	sb2Token    = "tok-2"
	sb3Token    = "tok-3"
)

type rig struct {
	t             *testing.T
	d             *db.DB
	st            *store.Store
	svc           *Service
	srv           *httptest.Server
	key           []byte
	sb1, sb2, sb3 podid.ID
}

var bg = context.Background()

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeLookup is the resolver of the destination rule in these tests.
func fakeLookup(_ context.Context, host string) ([]netip.Addr, error) {
	switch host {
	case "k8s.example.test":
		return []netip.Addr{netip.MustParseAddr("203.0.113.10")}, nil
	case "git.example.test":
		return []netip.Addr{netip.MustParseAddr("203.0.113.30")}, nil
	case "api.erp.example.test":
		return []netip.Addr{netip.MustParseAddr("203.0.113.40")}, nil
	case "auth.erp.example.test":
		return []netip.Addr{netip.MustParseAddr("203.0.113.41")}, nil
	case "pay.example.test":
		return []netip.Addr{netip.MustParseAddr("203.0.113.42")}, nil
	case "internal.test":
		return []netip.Addr{netip.MustParseAddr("10.43.0.1")}, nil
	case "mixed.test":
		return []netip.Addr{netip.MustParseAddr("203.0.113.11"), netip.MustParseAddr("192.168.1.10")}, nil
	}
	return nil, fmt.Errorf("lookup %s: no such host", host)
}

// allowLoopback is the test policy: the default deny set without 127/8,
// so that a test server on loopback is a destination.
func allowLoopback(a netip.Addr) error {
	if a.Unmap().IsLoopback() {
		return nil
	}
	return denyPolicy(DefaultDenyPrefixes())(a)
}

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// newRig returns the rig with the surfaces on.
func newRig(t *testing.T) *rig { return newRigWith(t, true) }

func newRigWith(t *testing.T, enabled bool) *rig {
	t.Helper()
	d := dbtest.New(t)
	st := store.New(d)
	r := &rig{t: t, d: d, st: st, key: newKey(t), sb1: podid.Mint(), sb2: podid.Mint(), sb3: podid.ID(uuid.NewString())}
	for _, tn := range []string{"acme", "other"} {
		if _, _, err := st.CreateTenant(bg, tn); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []podid.ID{r.sb1, r.sb2, r.sb3} {
		if _, _, err := st.CreateSandbox(bg, store.NewSandbox{ID: id, Owner: "acme", Image: "img", VCPUs: 2, MemoryMiB: 512}); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := New(Config{Enabled: enabled, Key: r.key, Log: quietLog()}, st, d)
	if err != nil {
		t.Fatal(err)
	}
	svc.dest.lookup = fakeLookup
	r.svc = svc
	r.srv = httptest.NewServer(r.mux())
	t.Cleanup(r.srv.Close)
	return r
}

// on returns a copy of the rig whose server mounts svc, another instance
// on the same database.
func (r *rig) on(svc *Service) *rig {
	r.t.Helper()
	svc.dest.lookup = fakeLookup
	r2 := *r
	r2.svc = svc
	r2.srv = httptest.NewServer(r2.mux())
	r.t.Cleanup(r2.srv.Close)
	return &r2
}

// mux mounts the Service as api does.
func (r *rig) mux() http.Handler {
	sandboxes := map[string]podid.ID{sb1Token: r.sb1, sb2Token: r.sb2, sb3Token: r.sb3}
	tenants := map[string]string{tenantToken: "acme", otherToken: "other"}
	sandbox := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			tok := strings.TrimSpace(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
			if tok == "" {
				apierr.Write(w, apierr.New(http.StatusUnauthorized, "sandbox token required"))
				return
			}
			id, ok := sandboxes[tok]
			if !ok || id.String() != req.PathValue("id") {
				apierr.Write(w, apierr.New(http.StatusNotFound, "no such sandbox"))
				return
			}
			h(w, req.WithContext(auth.WithSandbox(req.Context(), auth.Sandbox{ID: id, Owner: "acme",
				TokenHash: store.HashToken(tok)})))
		}
	}
	tenant := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			tok := strings.TrimSpace(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
			if tok == "" {
				apierr.Write(w, apierr.New(http.StatusUnauthorized, "tenant bearer required"))
				return
			}
			name, ok := tenants[tok]
			if !ok {
				apierr.Write(w, apierr.New(http.StatusUnauthorized, "unknown tenant token"))
				return
			}
			h(w, req.WithContext(auth.WithTenant(req.Context(), auth.Tenant{Name: name})))
		}
	}
	mux := http.NewServeMux()
	s := r.svc
	if s.Enabled() {
		mux.HandleFunc("PUT /api/v1/sandboxes/{id}/kube", sandbox(s.PutKube))
		mux.HandleFunc("GET /api/v1/sandboxes/{id}/kube", sandbox(s.GetKube))
		mux.HandleFunc("DELETE /api/v1/sandboxes/{id}/kube", sandbox(s.DeleteKube))
		mux.HandleFunc("PUT /api/v1/sandboxes/{id}/ssh", sandbox(s.PutSSH))
		mux.HandleFunc("GET /api/v1/sandboxes/{id}/ssh", sandbox(s.GetSSH))
		mux.HandleFunc("DELETE /api/v1/sandboxes/{id}/ssh", sandbox(s.DeleteSSH))
		mux.HandleFunc("POST /api/v1/sandboxes/{id}/ssh/{name}/check", sandbox(s.CheckSSH))
		mux.HandleFunc("POST /api/v1/kube/preflight", tenant(s.KubePreflight))
		mux.HandleFunc("POST /api/v1/ssh/preflight", tenant(s.SSHPreflight))
		mux.HandleFunc("PUT /api/v1/sandboxes/{id}/http", sandbox(s.PutHTTP))
		mux.HandleFunc("GET /api/v1/sandboxes/{id}/http", sandbox(s.GetHTTP))
		mux.HandleFunc("DELETE /api/v1/sandboxes/{id}/http", sandbox(s.DeleteHTTP))
		mux.HandleFunc("POST /api/v1/http/preflight", tenant(s.HTTPPreflight))
		mux.HandleFunc("GET /api/v1/credentials", tenant(s.ListCredentials))
		mux.HandleFunc("GET /api/v1/credentials/{name}", tenant(s.GetCredential))
		mux.HandleFunc("PUT /api/v1/credentials/{name}", tenant(s.PutCredential))
		mux.HandleFunc("DELETE /api/v1/credentials/{name}", tenant(s.DeleteCredential))
		mux.HandleFunc("POST /api/v1/credentials/{name}/check", tenant(s.CheckCredential))
	}
	mux.HandleFunc("GET /api/v1/sandboxes/{id}/access", tenant(s.Access))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, req *http.Request) {
		apierr.Write(w, apierr.New(http.StatusNotFound, "no such route: %s %s", req.Method, req.URL.Path))
	})
	return mux
}

func (r *rig) call(method, path, authz, body string) (int, string) {
	r.t.Helper()
	code, raw, err := r.try(method, path, authz, body)
	if err != nil {
		r.t.Fatal(err)
	}
	return code, raw
}

// try is call without failing the test, for goroutines other than the
// test's own, which must not call t.Fatal.
func (r *rig) try(method, path, authz, body string) (int, string, error) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, r.srv.URL+path, rdr)
	if err != nil {
		return 0, "", err
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), err
}

// sandboxCall calls a route of sb1 (path relative to its /api/v1/sandboxes/<id>).
func (r *rig) sandboxCall(method, suffix, body string) (int, string) {
	r.t.Helper()
	return r.call(method, "/api/v1/sandboxes/"+r.sb1.String()+suffix, "Bearer "+sb1Token, body)
}

func (r *rig) tenantCall(method, path, body string) (int, string) {
	r.t.Helper()
	return r.call(method, path, "Bearer "+tenantToken, body)
}

// codeOf decodes an error body.
func codeOf(raw string) (msg, code string) {
	var e struct{ Error, Code string }
	_ = json.Unmarshal([]byte(raw), &e)
	return e.Error, e.Code
}

// setBody is a PUT body {"bindings": bs}.
func setBody(bs ...map[string]any) string {
	if bs == nil {
		bs = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"bindings": bs})
	return string(b)
}
