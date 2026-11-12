// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/model"
)

// wantCode checks an error's status and code.
func wantCode(t *testing.T, err error, status int, code, msg string) {
	t.Helper()
	ae, ok := err.(*apierr.Error)
	if !ok || ae.Status != status || ae.Code != code || !strings.Contains(ae.Msg, msg) {
		t.Fatalf("error %v (%T), want %d %s %q", err, err, status, code, msg)
	}
}

// provider inserts a provider of tenantA with SQL.
func (h *harness) provider(name, kind string) {
	h.t.Helper()
	if kind == model.ProviderKindTenant {
		h.exec(`INSERT INTO access_providers (tenant, name, kind, url) VALUES ($1, $2, 'tenant', 'https://p.example/visit')`,
			tenantA, name)
		return
	}
	h.exec(`INSERT INTO access_providers (tenant, name, kind, issuer, client_id, client_secret_sealed, rule)
		VALUES ($1, $2, 'oidc', 'https://login.example/', 'c', '\x01'::bytea, '{"anyone":true}')`, tenantA, name)
}

// openVisit inserts an open visit of an environment of tenantA.
func (h *harness) openVisit(env uuid.UUID, provider, kind string) uuid.UUID {
	h.t.Helper()
	id := uuid.New()
	var person any
	if kind == model.ProviderKindFerun {
		p, err := h.st.UpsertPerson(h.ctx(), "https://id.test/", "sub-"+id.String(), "", "")
		if err != nil {
			h.t.Fatal(err)
		}
		person = p.ID
	}
	h.exec(`INSERT INTO visits (id, env_id, tenant, provider, provider_kind, host, subject, person_id, ends_at)
		VALUES ($1, $2, $3, $4, $5, 'h.apps.test', 'u-1', $6, now() + interval '1 hour')`, id, env, tenantA, provider,
		kind, person)
	return id
}

// visitEnd is a visit's end reason; "" while it is open.
func (h *harness) visitEnd(id uuid.UUID) string {
	h.t.Helper()
	var reason string
	if err := h.d.Pool.QueryRow(h.ctx(), "SELECT end_reason FROM visits WHERE id = $1", id).Scan(&reason); err != nil {
		h.t.Fatal(err)
	}
	return reason
}

func TestSetPublicAccessRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{appsZone: "apps.test", visitKinds: []string{model.ProviderKindTenant}})
	ctx := h.ctx()
	h.create("stage", nil)
	h.provider("fecode", model.ProviderKindTenant)
	h.provider("sso", model.ProviderKindOIDC)
	for _, c := range []struct {
		req  PublicRequest
		text string
	}{
		{PublicRequest{Public: true, Access: "members"}, "access is anyone or visitors"},
		{PublicRequest{Public: true, Access: "anyone", Provider: "fecode"}, "a provider goes with access visitors"},
		{PublicRequest{Public: true, Provider: "fecode"}, "a provider goes with access visitors"},
		{PublicRequest{Access: "anyone"}, "access goes with public true"},
		{PublicRequest{Access: "visitors", Provider: "fecode"}, "access goes with public true"},
	} {
		_, err := h.svc.SetPublic(ctx, tenantA, "stage", c.req)
		wantCode(t, err, http.StatusBadRequest, CodeInvalidAccess, c.text)
	}
	// Without ferun-auth the default (visitors of ferun) is refused, and
	// so is an oidc provider; a tenant provider and anyone are not.
	_, err := h.svc.SetPublic(ctx, tenantA, "stage", PublicRequest{Public: true})
	wantCode(t, err, http.StatusConflict, CodeFerunAuthRequired,
		"visitors of ferun need ferun-auth, which this installation does not run")
	_, err = h.svc.SetPublic(ctx, tenantA, "stage", PublicRequest{Public: true, Access: "visitors", Provider: "ferun"})
	wantCode(t, err, http.StatusConflict, CodeFerunAuthRequired, "visitors of ferun")
	_, err = h.svc.SetPublic(ctx, tenantA, "stage", PublicRequest{Public: true, Access: "visitors", Provider: "sso"})
	wantCode(t, err, http.StatusConflict, CodeFerunAuthRequired, "visitors of sso")
	_, err = h.svc.SetPublic(ctx, tenantA, "stage", PublicRequest{Public: true, Access: "visitors", Provider: "nope"})
	wantCode(t, err, http.StatusUnprocessableEntity, CodeUnknownProvider, "no provider nope in this tenant")
	if e := h.env("stage"); e.Public || e.Access != model.AccessAnyone {
		t.Fatalf("a refusal wrote %+v", e)
	}
	a, err := h.svc.SetPublic(ctx, tenantA, "stage", PublicRequest{Public: true, Access: "visitors", Provider: "fecode"})
	if err != nil || a.Access != "visitors" || a.Provider != "fecode" || !a.Public {
		t.Fatalf("tenant provider: %+v %v", a, err)
	}
	a, err = h.svc.SetPublic(ctx, tenantA, "stage", PublicRequest{Public: true, Access: "anyone"})
	if err != nil || a.Access != "anyone" || a.Provider != "" {
		t.Fatalf("anyone: %+v %v", a, err)
	}

	// Without a visits service every visitors is refused, anyone is not.
	h2 := newHarness(t, harnessOptions{appsZone: "apps.test", noVisits: true})
	h2.create("stage", nil)
	_, err = h2.svc.SetPublic(ctx, tenantA, "stage", PublicRequest{Public: true})
	wantCode(t, err, http.StatusConflict, CodeVisitsNeedSecretKey,
		"this installation guards no environments: ferund runs without --secret-key-file")
	if a, err := h2.svc.SetPublic(ctx, tenantA, "stage", PublicRequest{Public: true, Access: "anyone"}); err != nil ||
		a.Access != "anyone" {
		t.Fatalf("anyone without visits: %+v %v", a, err)
	}
	// A public environment that keeps its access anyone needs none.
	if a, err := h2.svc.SetPublic(ctx, tenantA, "stage", PublicRequest{Public: true}); err != nil || a.Access != "anyone" {
		t.Fatalf("kept anyone: %+v %v", a, err)
	}
	if _, err := h2.svc.SetPublic(ctx, tenantA, "stage", PublicRequest{}); err != nil {
		t.Fatal(err)
	}
}

// The access after a publish, row by row of the contract's table, and
// the visits a change ends.
func TestSetPublicAccess(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{appsZone: "apps.test"})
	ctx := h.ctx()
	st := h.create("stage", nil)
	id := uuid.MustParse(st.ID)
	h.provider("fecode", model.ProviderKindTenant)
	h.provider("sso", model.ProviderKindOIDC)
	if st.Access != "anyone" || st.Provider != "" {
		t.Fatalf("a new environment: %+v", st)
	}
	check := func(what string, req PublicRequest, access, provider, stored string) {
		t.Helper()
		a, err := h.svc.SetPublic(ctx, tenantA, "stage", req)
		if err != nil || a.Access != access || a.Provider != provider {
			t.Fatalf("%s: %+v %v, want %s %q", what, a, err, access, provider)
		}
		if e := h.env("stage"); e.Access != access || e.AccessProvider != stored {
			t.Fatalf("%s: row %s %q", what, e.Access, e.AccessProvider)
		}
		s, err := h.svc.GetEnv(ctx, tenantA, "stage")
		if err != nil || s.Access != access || s.Provider != provider {
			t.Fatalf("%s: status %s %q %v", what, s.Access, s.Provider, err)
		}
		l, err := h.svc.ListEnvs(ctx, tenantA, nil)
		if err != nil || l.Environments[0].Access != access || l.Environments[0].Provider != provider {
			t.Fatalf("%s: list %+v %v", what, l, err)
		}
	}
	// not public, "" "": visitors of ferun.
	check("first publish", PublicRequest{Public: true}, "visitors", "ferun", "")
	v1 := h.openVisit(id, "ferun", "ferun")
	// public, "" "": unchanged, and the visit stays.
	check("again", PublicRequest{Public: true}, "visitors", "ferun", "")
	if r := h.visitEnd(v1); r != "" {
		t.Fatalf("an unchanged publish ended a visit: %s", r)
	}
	// any, visitors "ferun": ferun, unchanged.
	check("visitors ferun", PublicRequest{Public: true, Access: "visitors", Provider: "ferun"}, "visitors", "ferun", "")
	// any, visitors name: that provider, the visits of ferun ended.
	check("visitors fecode", PublicRequest{Public: true, Access: "visitors", Provider: "fecode"}, "visitors", "fecode", "fecode")
	if r := h.visitEnd(v1); r != model.VisitProviderChanged {
		t.Fatalf("visit after the change: %q", r)
	}
	v2 := h.openVisit(id, "fecode", "tenant")
	// any, visitors "": ferun.
	check("visitors", PublicRequest{Public: true, Access: "visitors"}, "visitors", "ferun", "")
	if r := h.visitEnd(v2); r != model.VisitProviderChanged {
		t.Fatalf("visit after the change back: %q", r)
	}
	// any, anyone: anyone.
	check("anyone", PublicRequest{Public: true, Access: "anyone"}, "anyone", "", "")
	check("anyone kept", PublicRequest{Public: true}, "anyone", "", "")
	check("oidc", PublicRequest{Public: true, Access: "visitors", Provider: "sso"}, "visitors", "sso", "sso")

	// Unpublishing ends the open visits and keeps the access; publishing
	// again without access is ferun's (the row was not public).
	v3 := h.openVisit(id, "sso", "oidc")
	a, err := h.svc.SetPublic(ctx, tenantA, "stage", PublicRequest{})
	if err != nil || a.Public || a.Access != "visitors" || a.Provider != "sso" {
		t.Fatalf("unpublish: %+v %v", a, err)
	}
	if r := h.visitEnd(v3); r != model.VisitUnpublished {
		t.Fatalf("visit after unpublishing: %q", r)
	}
	check("publish again", PublicRequest{Public: true}, "visitors", "ferun", "")

	// The answer's JSON carries both, always.
	a, err = h.svc.SetPublic(ctx, tenantA, "stage", PublicRequest{Public: true, Access: "anyone"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(a)
	if !strings.HasSuffix(string(b), `"rolling":false,"access":"anyone","provider":""}`) {
		t.Fatalf("answer %s", b)
	}
	// A provider an environment names cannot go.
	if _, err := h.d.Pool.Exec(ctx, "DELETE FROM access_providers WHERE name = 'fecode'"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SetPublic(ctx, tenantA, "stage", PublicRequest{Public: true, Access: "visitors",
		Provider: "sso"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Pool.Exec(ctx, "DELETE FROM access_providers WHERE name = 'sso'"); err == nil {
		t.Fatal("deleted a provider an environment names")
	}
}
