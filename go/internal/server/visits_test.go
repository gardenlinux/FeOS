// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/domains"
	"github.com/gardenlinux/feos/go/internal/ingress/service"
	"github.com/gardenlinux/feos/go/internal/visits"
)

// The visits service is what the ingress service takes for its calls.
var _ service.Visits = (*visits.Service)(nil)

// serveAPI sends a request to the instance's front doors' handler, as the
// bootstrap tenant's token, and returns the status and the body.
func serveAPI(t *testing.T, i *Instance, method, path, body string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+bootstrapToken)
	w := httptest.NewRecorder()
	i.handler.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// keyFile writes a --secret-key-file and returns its path.
func keyFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte(hex.EncodeToString(make([]byte, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Without --secret-key-file ferund runs no visits service (S1): the
// visitors' routes are unknown, and a publish that would guard an
// environment, the bare publish of a new one included, is 409
// visits_need_secret_key; one open to anyone goes on as before.
func TestVisitsNeedTheSecretKey(t *testing.T) {
	cfg := testConfig(dbtest.NewDatabase(t), testCA(t))
	cfg.AppsZone = "apps.example.test"
	i, err := New(context.Background(), cfg, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	if i.Visits != nil {
		t.Fatal("a visits service without --secret-key-file")
	}
	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/v1/providers"},
		{"PUT", "/api/v1/providers/fecode"},
		{"POST", "/api/v1/challenges/open"},
		{"GET", "/api/v1/visits/00000000-0000-4000-8000-0000000000a1"},
		{"POST", "/api/v1/visits/end"},
		{"GET", "/api/v1/oidc-providers/acme/sso"},
	} {
		code, body := serveAPI(t, i, rt.method, rt.path, "{}")
		if code != http.StatusNotFound || !strings.Contains(body, `"no such route: `+rt.method+" "+rt.path+`"`) {
			t.Errorf("%s %s: %d %s", rt.method, rt.path, code, body)
		}
	}
	if code, body := serveAPI(t, i, "POST", "/api/v1/environments", `{"name":"stage"}`); code/100 != 2 {
		t.Fatalf("create: %d %s", code, body)
	}
	code, body := serveAPI(t, i, "PUT", "/api/v1/environments/stage/public", `{"public":true}`)
	if code != http.StatusConflict || !strings.Contains(body, `"code":"visits_need_secret_key"`) {
		t.Fatalf("bare publish: %d %s", code, body)
	}
	code, body = serveAPI(t, i, "PUT", "/api/v1/environments/stage/public", `{"public":true,"access":"anyone"}`)
	if code != http.StatusOK || !strings.Contains(body, `"access":"anyone"`) {
		t.Fatalf("publish to anyone: %d %s", code, body)
	}
}

// With --secret-key-file the visits service runs: the providers' routes
// answer, with the kinds served (tenant alone without
// --visitor-auth-url; oidc beside it with one, and the redirect URI on
// ferun-auth's host); a bare publish of a new environment needs
// ferun-auth (409 ferun_auth_required); the installation's keys are made
// once; and ferun-auth's host can be neither a domain nor a tenant's
// name.
func TestVisitsWithTheSecretKey(t *testing.T) {
	caDir := testCA(t)
	cfg := testConfig(dbtest.NewDatabase(t), caDir)
	cfg.SecretKeyFile = keyFile(t)
	cfg.AppsZone, cfg.EdgeName = "apps.example.test", "edge.example.test"
	i, err := New(context.Background(), cfg, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	if i.Visits == nil {
		t.Fatal("no visits service with --secret-key-file")
	}
	code, body := serveAPI(t, i, "GET", "/api/v1/providers", "")
	if code != http.StatusOK {
		t.Fatalf("providers: %d %s", code, body)
	}
	var list visits.ProviderList
	if err := json.Unmarshal([]byte(body), &list); err != nil || !slices.Equal(list.Kinds, []string{"tenant"}) ||
		strings.Contains(body, "redirectUri") || len(list.Providers) != 1 || list.Providers[0].Name != "ferun" {
		t.Fatalf("providers %s (%v)", body, err)
	}
	if code, body := serveAPI(t, i, "POST", "/api/v1/environments", `{"name":"stage"}`); code/100 != 2 {
		t.Fatalf("create: %d %s", code, body)
	}
	code, body = serveAPI(t, i, "PUT", "/api/v1/environments/stage/public", `{"public":true}`)
	if code != http.StatusConflict || !strings.Contains(body, `"code":"ferun_auth_required"`) {
		t.Fatalf("bare publish: %d %s", code, body)
	}
	// The keys: made on first use, the same for a second instance.
	k1, err := i.Visits.Keys(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// A second instance on the same database, with ferun-auth's URL.
	cfg.VisitorAuthURL = "https://auth.example.test"
	cfg.Listen, cfg.NodeListen = "127.0.0.1:0", "127.0.0.1:0"
	j, err := New(context.Background(), cfg, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	k2, err := j.Visits.Keys(context.Background())
	if err != nil || k1.TokenKID() != k2.TokenKID() || k1.ChallengeKID() != k2.ChallengeKID() {
		t.Fatalf("keys %v", err)
	}
	code, body = serveAPI(t, j, "GET", "/api/v1/providers", "")
	if err := json.Unmarshal([]byte(body), &list); code != http.StatusOK || err != nil ||
		!slices.Equal(list.Kinds, []string{"tenant", "oidc"}) {
		t.Fatalf("providers with ferun-auth: %d %s (%v)", code, body, err)
	}
	// ferun-auth's host is reserved: no domain, no tenant of its name.
	if _, _, err := j.Store.CreateTenant(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"auth.example.test", "x.auth.example.test"} {
		_, err := j.Domains.CreateDomain(context.Background(), "acme", domains.CreateRequest{Name: name})
		var ae *apierr.Error
		if !errors.As(err, &ae) || ae.Status != http.StatusUnprocessableEntity || ae.Code != domains.CodeReserved {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := j.Domains.CreateDomain(context.Background(), "acme", domains.CreateRequest{Name: "example.test"}); err != nil {
		t.Errorf("a name beside ferun-auth's host: %v", err)
	}
	if !slices.Contains(reservedTenantNames(cfg), "auth") {
		t.Errorf("reserved tenant names %v", reservedTenantNames(cfg))
	}
}
