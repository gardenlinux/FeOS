// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// instanceRow inserts an environment of the env's tenant and an instance
// row of it, with a sandbox token in the table as if one had been minted.
func (e *env) instanceRow() (store.Sandbox, string) {
	e.t.Helper()
	ctx := context.Background()
	envRow, err := e.st.InsertEnvironment(ctx, store.NewEnvironment{ID: uuid.New(), Owner: testTenant, Name: "stage"})
	if err != nil {
		e.t.Fatal(err)
	}
	sb, created, err := e.st.CreateSandbox(ctx, store.NewSandbox{ID: podid.Mint(), Owner: testTenant, Image: "img",
		VCPUs: 1, MemoryMiB: 512, EnvironmentID: envRow.ID,
		PodTemplate: &model.PodTemplate{SecurityGroups: []string{model.EnvGroupID(envRow.ID)}}})
	if err != nil || !created {
		e.t.Fatalf("instance row: %v %v", err, created)
	}
	tok, hash, err := mintSandboxToken()
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.CreateSandboxToken(ctx, hash, sb.ID, "smuggled", ""); err != nil {
		e.t.Fatal(err)
	}
	return sb, tok
}

// An environment's instance row is no sandbox to the sandbox routes: the
// tenant's routes answer as for an absent sandbox, its ID is taken for a
// create, the listing leaves it out, and a token of it opens no sandbox
// door.
func TestInstanceRowsAreNoSandboxes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	own, _ := e.createSandbox()
	inst, instToken := e.instanceRow()
	id := inst.ID.String()
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/sandboxes/" + id},
		{"DELETE", "/api/v1/sandboxes/" + id},
		{"POST", "/api/v1/sandboxes/" + id + "/pause"},
		{"POST", "/api/v1/sandboxes/" + id + "/resume"},
		{"GET", "/api/v1/sandboxes/" + id + "/tokens"},
		{"POST", "/api/v1/sandboxes/" + id + "/tokens"},
		{"DELETE", "/api/v1/sandboxes/" + id + "/tokens/abc"},
		{"GET", "/api/v1/sandboxes/" + id + "/access"},
	} {
		w := e.do(c.method, c.path, bearerOf(e.tenantToken), nil)
		expect(t, w, http.StatusNotFound, errBody("unknown sandbox "+id))
	}
	if _, err := e.st.GetSandbox(context.Background(), inst.ID); err != nil {
		t.Fatalf("the instance row went: %v", err)
	}
	if e.fake.paused != nil || e.fake.resumed != nil {
		t.Fatalf("the engine was asked: %v %v", e.fake.paused, e.fake.resumed)
	}
	toks, err := e.st.ListSandboxTokens(context.Background(), inst.ID)
	if err != nil || len(toks) != 1 {
		t.Fatalf("a token was minted: %v %v", toks, err)
	}

	// The listing has the tenant's own sandbox only.
	w := e.do("GET", "/api/v1/sandboxes", bearerOf(e.tenantToken), nil)
	list := decode[struct {
		Sandboxes []map[string]any `json:"sandboxes"`
	}](t, w)
	if len(list.Sandboxes) != 1 || list.Sandboxes[0]["sandboxId"] != own {
		t.Fatalf("list %+v", list)
	}

	// Its token opens no sandbox door: the tools, the watches, the
	// forward and MCP all resolve it as no token.
	w = e.do("POST", "/api/v1/sandboxes/"+id+"/exec", bearerOf(instToken), map[string]any{"command": "true"})
	expect(t, w, http.StatusNotFound, errBody("no such sandbox"))
	w = e.do("POST", "/api/v1/sandboxes/"+id+"/watches", bearerOf(instToken), map[string]any{})
	expect(t, w, http.StatusNotFound, errBody("no such sandbox"))
	if _, err := ResolveSandboxToken(context.Background(), e.st.Q(), instToken); !errors.Is(err, model.ErrUnknownToken) {
		t.Fatalf("the instance row's token resolved: %v", err)
	}
}

// POST /api/v1/sandboxes with attachable: the row has the attachable
// template, named after the minted ID, and the status says so.
func TestCreateAttachableSandbox(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	type answer struct {
		Status struct {
			SandboxID  string `json:"sandboxId"`
			Attachable bool   `json:"attachable"`
		} `json:"status"`
	}
	w := e.do("POST", "/api/v1/sandboxes", bearerOf(e.tenantToken), map[string]any{"attachable": true})
	expect(t, w, http.StatusCreated, "")
	a := decode[answer](t, w)
	if !a.Status.Attachable {
		t.Fatalf("status %+v", a.Status)
	}
	id, err := podid.Parse(a.Status.SandboxID)
	if err != nil || id.IsLegacy() {
		t.Fatalf("minted %q: %v", a.Status.SandboxID, err)
	}
	row, err := e.st.GetSandbox(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	tpl := row.PodTemplate
	if tpl == nil || !slices.Equal(tpl.Command, []string{"sleep", "infinity"}) || len(tpl.Env) != 0 ||
		!slices.Equal(tpl.SecurityGroups, []string{"sbx-" + id.String()}) || len(tpl.Labels) != 0 || len(tpl.Volumes) != 0 ||
		row.IsInstance() {
		t.Fatalf("row %+v template %+v", row, tpl)
	}
	for _, path := range []string{"/api/v1/sandboxes/" + id.String()} {
		w = e.do("GET", path, bearerOf(e.tenantToken), nil)
		if st := decode[map[string]any](t, w); st["attachable"] != true {
			t.Fatalf("GET %v", st)
		}
	}
	// An ordinary sandbox is not attachable.
	plain, _ := e.createSandbox()
	w = e.do("GET", "/api/v1/sandboxes/"+plain, bearerOf(e.tenantToken), nil)
	if st := decode[map[string]any](t, w); st["attachable"] != false {
		t.Fatalf("plain %v", st)
	}
	if row, _ := e.st.GetSandbox(context.Background(), podid.ID(plain)); row.PodTemplate != nil {
		t.Fatalf("plain template %+v", row.PodTemplate)
	}
}
