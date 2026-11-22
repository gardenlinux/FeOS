// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	envs "github.com/gardenlinux/feos/go/internal/env"
	"github.com/gardenlinux/feos/go/internal/seal"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Cloning needs project write whatever the source's protection, but a
// clone that copies the source's secrets or credentials needs write on
// the source too: a project's writer, which only reads a protected
// environment (ADR 0060), clones it without its secrets and takes none
// of them, and nor does an integration with project write alone; a grant
// of write on the source lets them copy.
func TestCloneOfProtectedCopiesNothingForAReader(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{environments: func(d *db.DB, st *store.Store) Environments {
		sl, err := seal.New(make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		svc, err := envs.New(envs.Deps{InstanceID: testInstance, DB: d, Store: st, Engine: envEngine{}, Ports: envPorts{},
			Sealer: sl, Log: dbtest.Logger(t)})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}})
	e := w.e
	admin := bearerOf(e.projectToken)
	expect(t, e.do("POST", "/api/v1/environments", admin, map[string]any{"name": "live"}), http.StatusCreated, "")
	expect(t, e.do("PUT", "/api/v1/environments/live/secrets/token", admin, map[string]any{"value": "s3cret"}),
		http.StatusOK, "")
	spec := map[string]any{"services": map[string]any{"api": map[string]any{"image": "ghcr.io/acme/api:1",
		"env": map[string]any{"T": map[string]any{"secret": "token"}}}}}
	expect(t, e.do("POST", "/api/v1/environments/live/deployments", admin, map[string]any{"spec": spec, "base": 0}),
		http.StatusCreated, "")
	expect(t, e.do("PUT", "/api/v1/environments/live/protected", admin, map[string]any{"protected": true}),
		http.StatusOK, "")
	live, err := e.st.EnvironmentRef(context.Background(), testProject, "live")
	if err != nil {
		t.Fatal(err)
	}
	copying := func(name string) map[string]any {
		return map[string]any{"name": name, "secrets": map[string]any{"token": map[string]any{"copy": true}}}
	}
	refused := `{"error":"this needs the role write on the environment","code":"role_required","role":"write",` +
		`"scope":"environment"}` + "\n"
	writer := bearerOf(w.token(t, "write", uuid.Nil))
	expect(t, e.do("PUT", "/api/v1/environments/live/secrets/token", writer, map[string]any{"value": "x"}),
		http.StatusForbidden, refused)
	expect(t, e.do("POST", "/api/v1/environments/live/clone", writer, copying("leak")), http.StatusForbidden, refused)
	expect(t, e.do("POST", "/api/v1/environments/live/clone", writer, map[string]any{"name": "leak",
		"credentials": map[string]any{"api": map[string]any{"copy": true}}}), http.StatusForbidden, refused)
	if _, err := e.st.EnvironmentRef(context.Background(), testProject, "leak"); !isNoRow(err) {
		t.Fatalf("a refused clone was made: %v", err)
	}
	// Without a copy it clones, unprotected, with no secret.
	resp := e.do("POST", "/api/v1/environments/live/clone", writer, map[string]any{"name": "copy",
		"secrets": map[string]any{"token": map[string]any{"value": "own"}}})
	expect(t, resp, http.StatusCreated, "")
	if st := decode[envs.EnvStatus](t, resp); st.Protected {
		t.Fatalf("the clone is protected: %s", resp.Body)
	}
	// An integration with project write alone reads live too.
	jwt := w.integration(t, "fecode", store.Grant{Role: "write"})
	expect(t, e.serve(asIntegration("POST", "/api/v1/environments/live/clone", jwt, testProject, "", copying("leak"))),
		http.StatusForbidden, refused)
	// Write on live lets the writer copy.
	tok := w.token(t, "write", uuid.Nil)
	if _, err := e.st.PutGrant(context.Background(), store.Grant{ProjectID: testProject, TokenHash: store.HashToken(tok),
		EnvironmentID: live, Role: "write"}); err != nil {
		t.Fatal(err)
	}
	resp = e.do("POST", "/api/v1/environments/live/clone", bearerOf(tok), copying("granted"))
	expect(t, resp, http.StatusCreated, "")
	granted, err := e.st.EnvironmentRef(context.Background(), testProject, "granted")
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := e.st.ListSecrets(context.Background(), granted, false); err != nil || len(rows) != 1 {
		t.Fatalf("the granted clone's secrets: %v %v", rows, err)
	}
}
