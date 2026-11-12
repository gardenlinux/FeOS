// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// routedEnv makes a public environment of owner at deployment 1.
func routedEnv(t *testing.T, s *store.Store, owner, name string) store.Environment {
	t.Helper()
	e := mkEnv(t, s, owner, name)
	after := e
	after.Deployment = 1
	if _, err := s.UpdateEnvironment(bg, e, after); err != nil {
		t.Fatal(err)
	}
	return publish(t, s, e.ID, true)
}

// routingEnvs returns the environments of rows, in order, each once.
func routingEnvs(rows []store.IngressRoutingRow) []uuid.UUID {
	var out []uuid.UUID
	for _, r := range rows {
		if len(out) == 0 || out[len(out)-1] != r.EnvID {
			out = append(out, r.EnvID)
		}
	}
	return out
}

// IngressRouting returns public environments with their service instances
// that are not stopping, environments without one as a single row, and
// leaves out what the ingress does not serve: environments not public,
// deleting, without a deployment, of a deleting tenant or without a host
// label; jobs and stopping instances.
func TestIngressRouting(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkTenant(t, s, "gone")
	mkNode(t, s, "n1")
	if _, err := s.SetNodeIngressGroupVersion(bg, "n1", 7); err != nil {
		t.Fatal(err)
	}

	web := routedEnv(t, s, "acme", "web")
	sb := mkService(t, s, web, "n1") // running on n1, idle after a minute
	if ok, err := s.SetSandboxAddress(bg, sb.ID, sb.Incarnation, "fd00::5"); err != nil || !ok {
		t.Fatalf("address: %v %v", ok, err)
	}
	if _, err := s.SetIngressPorts(bg, sb.ID, []int{8080, 80}); err != nil {
		t.Fatal(err)
	}
	inst, err := s.LockEnvInstance(bg, sb.ID, store.NoLock)
	must(t, err)
	after := inst
	after.Phase = model.PhaseReady
	if _, err := s.UpdateEnvInstance(bg, inst, after); err != nil {
		t.Fatal(err)
	}
	// A stopping service instance and a job are left out.
	stopping := mkService(t, s, web, "n1")
	inst, err = s.LockEnvInstance(bg, stopping.ID, store.NoLock)
	must(t, err)
	after = inst
	after.Phase = model.PhaseStopping
	if _, err := s.UpdateEnvInstance(bg, inst, after); err != nil {
		t.Fatal(err)
	}
	job := mkInstance(t, s, web)
	if _, err := s.InsertEnvInstance(bg, store.EnvInstance{SandboxID: job.ID, EnvID: web.ID, Kind: "job", Object: "migrate",
		TemplateHash: "j1", Deployment: 1}); err != nil {
		t.Fatal(err)
	}

	// Public, without instances: one row.
	bare := routedEnv(t, s, "acme", "bare")
	// Left out: never published (no label), unpublished, deleting, no
	// deployment, a deleting tenant's.
	mkEnv(t, s, "acme", "private")
	unpublished := routedEnv(t, s, "acme", "unpublished")
	publish(t, s, unpublished.ID, false)
	deleting := routedEnv(t, s, "acme", "deleting")
	cur, err := s.GetEnvironment(bg, deleting.ID)
	must(t, err)
	del := cur
	del.Deleting = true
	if _, err := s.UpdateEnvironment(bg, cur, del); err != nil {
		t.Fatal(err)
	}
	nodeployment := publish(t, s, mkEnv(t, s, "acme", "nodeployment").ID, true)
	routedEnv(t, s, "gone", "web")
	if _, err := s.MarkTenantDeleting(bg, "gone"); err != nil {
		t.Fatal(err)
	}

	rows, err := s.IngressRouting(bg, nil)
	must(t, err)
	want := []uuid.UUID{web.ID, bare.ID}
	slices.SortFunc(want, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	if got := routingEnvs(rows); !slices.Equal(got, want) || len(rows) != 2 {
		t.Fatalf("environments %v (%d rows), want %v", got, len(rows), want)
	}
	for _, r := range rows {
		switch r.EnvID {
		case web.ID:
			if !r.HasInstance || r.SandboxID != sb.ID || r.Object != "web" || r.Replica != 0 || r.InstanceDeployment != 1 ||
				r.Phase != model.PhaseReady || !slices.Equal(r.IngressPorts, []int{80, 8080}) || r.Address != "fd00::5" ||
				r.ClientState != model.StateRunning || r.Op != "" || r.Node != "n1" || r.NodeGroupVersion != 7 ||
				r.IdleAfterS == nil || *r.IdleAfterS != 60 {
				t.Fatalf("instance row %+v", r)
			}
			if r.Owner != "acme" || r.Name != "web" || r.HostLabel != "web-acme" || r.Deployment != 1 || r.Deployed != 0 ||
				!r.CreatedAt.Equal(web.CreatedAt) {
				t.Fatalf("environment of the row %+v", r)
			}
		case bare.ID:
			if r.HasInstance || r.SandboxID != "" || r.Object != "" || r.Phase != "" || r.IngressPorts != nil || r.Node != "" ||
				r.NodeGroupVersion != 0 || r.IdleAfterS != nil || r.HostLabel != "bare-acme" {
				t.Fatalf("row without an instance %+v", r)
			}
		}
	}

	// Limited to some environments; an empty list reads none, and an
	// environment it leaves out has no row.
	rows, err = s.IngressRouting(bg, []uuid.UUID{bare.ID, nodeployment.ID, unpublished.ID})
	must(t, err)
	if got := routingEnvs(rows); !slices.Equal(got, []uuid.UUID{bare.ID}) {
		t.Fatalf("limited: %v", got)
	}
	if rows, err := s.IngressRouting(bg, []uuid.UUID{}); err != nil || len(rows) != 0 {
		t.Fatalf("empty list: %v %v", rows, err)
	}

	// A public environment without a host label is left out too (the
	// schema's CHECK forbids one; the query does not rely on it).
	exec(t, s, "ALTER TABLE environments DROP CONSTRAINT environments_public_ready")
	exec(t, s, "UPDATE environments SET host_label = NULL WHERE id = $1", bare.ID)
	rows, err = s.IngressRouting(bg, nil)
	must(t, err)
	if got := routingEnvs(rows); !slices.Equal(got, []uuid.UUID{web.ID}) {
		t.Fatalf("without a label: %v", got)
	}
}

// ingProvider makes a provider of tenant by raw SQL (the store's own
// functions for providers are another package's): kind tenant at url, or
// kind oidc with a sealed secret when url is "".
func ingProvider(t *testing.T, s *store.Store, tenant, name, url string) {
	t.Helper()
	if url != "" {
		exec(t, s, "INSERT INTO access_providers (tenant, name, kind, url) VALUES ($1, $2, 'tenant', $3)", tenant, name, url)
		return
	}
	exec(t, s, `INSERT INTO access_providers (tenant, name, kind, issuer, client_id, client_secret_sealed, rule)
		VALUES ($1, $2, 'oidc', 'https://login.example.com/', 'abc', '\x01'::bytea, '{"anyone":true}')`, tenant, name)
}

// ingAccess sets who an environment admits by raw SQL: provider "" with
// visitors is ferun's.
func ingAccess(t *testing.T, s *store.Store, env uuid.UUID, access, provider string) {
	t.Helper()
	exec(t, s, "UPDATE environments SET access = $2, access_provider = nullif($3, '') WHERE id = $1", env, access, provider)
}

// IngressRouting reads who each environment admits (ADR 0054): anyone,
// visitors of ferun (no provider row), of a provider of kind tenant with
// its URL, and of one of kind oidc; a provider name without a row reads
// no kind and no URL.
func TestIngressRoutingAccess(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	ingProvider(t, s, "acme", "fecode", "https://fecode.example/api/v1/visit")
	ingProvider(t, s, "acme", "sso", "")
	open := routedEnv(t, s, "acme", "open")
	ferun := routedEnv(t, s, "acme", "ferun")
	ingAccess(t, s, ferun.ID, model.AccessVisitors, "")
	tenant := routedEnv(t, s, "acme", "tenant")
	ingAccess(t, s, tenant.ID, model.AccessVisitors, "fecode")
	oidc := routedEnv(t, s, "acme", "oidc")
	ingAccess(t, s, oidc.ID, model.AccessVisitors, "sso")

	type access struct{ access, provider, kind, url string }
	read := func() map[uuid.UUID]access {
		t.Helper()
		rows, err := s.IngressRouting(bg, nil)
		must(t, err)
		out := map[uuid.UUID]access{}
		for _, r := range rows {
			out[r.EnvID] = access{r.Access, r.AccessProvider, r.ProviderKind, r.ProviderURL}
		}
		return out
	}
	got := read()
	want := map[uuid.UUID]access{
		open.ID:   {model.AccessAnyone, "", "", ""},
		ferun.ID:  {model.AccessVisitors, "", "", ""},
		tenant.ID: {model.AccessVisitors, "fecode", model.ProviderKindTenant, "https://fecode.example/api/v1/visit"},
		oidc.ID:   {model.AccessVisitors, "sso", model.ProviderKindOIDC, ""},
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("environment %s: %+v, want %+v", id, got[id], w)
		}
	}

	// The foreign key keeps a named provider; without it, a name whose
	// row is gone reads no kind and no URL (the table then signs no one
	// in), and the environment stays guarded.
	exec(t, s, "ALTER TABLE environments DROP CONSTRAINT environments_access_provider_fkey")
	exec(t, s, "DELETE FROM access_providers WHERE tenant = 'acme' AND name = 'fecode'")
	if a := read()[tenant.ID]; a != (access{model.AccessVisitors, "fecode", "", ""}) {
		t.Fatalf("a missing provider row: %+v", a)
	}
}
