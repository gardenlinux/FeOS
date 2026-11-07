// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
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
		VCPUs: 1, MemoryMiB: 512, EnvironmentID: envRow.ID, Instance: true,
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

// sandboxAnswer is a sandbox's view as the tests read it.
type sandboxAnswer struct {
	SandboxID   string `json:"sandboxId"`
	Environment struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"environment"`
	SecurityGroups []string `json:"securityGroups"`
	Attachable     *bool    `json:"attachable"`
}

// deployGroups gives environment id a deployed deployment with the
// groups names (ADR 0034).
func (e *env) deployGroups(id uuid.UUID, names ...string) {
	e.t.Helper()
	ctx := context.Background()
	groups := map[string]any{}
	for _, n := range names {
		groups[n] = map[string]any{}
	}
	raw, err := json.Marshal(map[string]any{"securityGroups": groups})
	if err != nil {
		e.t.Fatal(err)
	}
	var n int
	if err := e.st.Q().QueryRow(ctx, "SELECT deployment + 1 FROM environments WHERE id = $1", id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.InsertDeployment(ctx, store.EnvDeployment{EnvID: id, Number: n, Origin: model.OriginApply,
		Spec: raw}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.Q().Exec(ctx, "UPDATE environments SET deployment = $2, deployed = $2 WHERE id = $1", id, n); err != nil {
		e.t.Fatal(err)
	}
}

// A sandbox is made in an environment (ADR 0035), required, by name or
// ID, in [default] unless the create names groups of its deployed
// deployment; attachable is refused; the views name the environment and
// the groups; the row has the template every sandbox's pod is made from
// and log labels of its environment; the list filters by environment.
func TestSandboxEnvironment(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	tok := bearerOf(e.tenantToken)
	create := func(body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		return e.do("POST", "/api/v1/sandboxes", tok, body)
	}
	expect(t, create(map[string]any{}), http.StatusBadRequest,
		errBody("environment is required: the name or ID of the environment the sandbox is in (ADR 0035)"))
	gone := errBody("attachable is gone with ADR 0035: every sandbox of an environment can run its attached services; " +
		"leave it out and name the sandbox's environment")
	for _, v := range []any{true, false} {
		expect(t, create(map[string]any{"environment": "box", "attachable": v}), http.StatusBadRequest, gone)
	}
	expect(t, create(map[string]any{"environment": "nope"}), http.StatusNotFound, errBody("no such environment"))
	expect(t, create(map[string]any{"environment": "box", "securityGroups": []string{}}), http.StatusBadRequest,
		errBody("securityGroups is empty: name at least one group (leave it out for default)"))
	expect(t, create(map[string]any{"environment": "box", "securityGroups": []string{"a", "b", "c", "d", "e", "f"}}),
		http.StatusBadRequest, errBody("securityGroups names 6 groups; at most 5 are allowed"))
	expect(t, create(map[string]any{"environment": "box", "securityGroups": []string{"web"}}),
		http.StatusUnprocessableEntity,
		errBody("securityGroups: the environment's deployed deployment has no group web; it has default"))

	w := create(map[string]any{"environment": "box"})
	expect(t, w, http.StatusCreated, "")
	a := decode[struct {
		Status sandboxAnswer `json:"status"`
	}](t, w).Status
	if a.Environment.ID != e.box.String() || a.Environment.Name != "box" || !slices.Equal(a.SecurityGroups, []string{"default"}) ||
		a.Attachable != nil {
		t.Fatalf("status %+v", a)
	}
	id := podid.ID(a.SandboxID)
	row, err := e.st.GetSandbox(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	tpl := row.PodTemplate
	if row.EnvironmentID != e.box || !row.IsOwn() || tpl == nil || !slices.Equal(tpl.Command, []string{"sleep", "infinity"}) ||
		row.LogLabels[model.LogEnvironment] != e.box.String() || row.LogLabels[model.LogKind] != model.LogKindSandbox ||
		row.LogLabels[model.LogObject] != a.SandboxID || row.GroupsPending {
		t.Fatalf("row %+v template %+v", row, tpl)
	}

	// Another environment, by its ID, with groups of its deployed
	// deployment, sorted.
	dev := e.mkEnv(testTenant, "dev")
	e.deployGroups(dev, "web", "db")
	w = create(map[string]any{"environment": dev.String(), "securityGroups": []string{"web", "db", "default"}})
	expect(t, w, http.StatusCreated, "")
	b := decode[struct {
		Status sandboxAnswer `json:"status"`
	}](t, w).Status
	if b.Environment.Name != "dev" || !slices.Equal(b.SecurityGroups, []string{"db", "default", "web"}) {
		t.Fatalf("in dev %+v", b)
	}
	get := decode[sandboxAnswer](t, e.do("GET", "/api/v1/sandboxes/"+b.SandboxID, tok, nil))
	if get.Environment.ID != dev.String() || !slices.Equal(get.SecurityGroups, b.SecurityGroups) {
		t.Fatalf("get %+v", get)
	}

	list := func(q string) []sandboxAnswer {
		t.Helper()
		w := e.do("GET", "/api/v1/sandboxes"+q, tok, nil)
		expect(t, w, http.StatusOK, "")
		return decode[struct {
			Sandboxes []sandboxAnswer `json:"sandboxes"`
		}](t, w).Sandboxes
	}
	if l := list(""); len(l) != 2 || l[0].Environment.Name != "box" || l[1].Environment.Name != "dev" {
		t.Fatalf("all %+v", l)
	}
	if l := list("?environment=dev"); len(l) != 1 || l[0].SandboxID != b.SandboxID {
		t.Fatalf("dev's %+v", l)
	}
	if l := list("?environment=" + e.box.String()); len(l) != 1 || l[0].SandboxID != a.SandboxID {
		t.Fatalf("box's %+v", l)
	}
	expect(t, e.do("GET", "/api/v1/sandboxes?environment=nope", tok, nil), http.StatusNotFound, errBody("no such environment"))

	// An environment being deleted takes no sandbox.
	if _, err := e.st.Q().Exec(context.Background(), "UPDATE environments SET deleting = true WHERE id = $1", dev); err != nil {
		t.Fatal(err)
	}
	expect(t, create(map[string]any{"environment": "dev"}), http.StatusConflict, errBody("environment dev is being deleted"))
}

// PUT /api/v1/sandboxes/{id}/security-groups records the sandbox's new
// groups, checked as a create's, and marks a sandbox with a pod for its
// move (202); one without a pod takes them at its next restore or create;
// the same groups again are 200 and change nothing.
func TestSandboxSecurityGroups(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	tok := bearerOf(e.tenantToken)
	e.deployGroups(e.box, "web", "db")
	id, _ := e.createSandbox()
	path := "/api/v1/sandboxes/" + id + "/security-groups"
	put := func(groups any) *httptest.ResponseRecorder {
		t.Helper()
		return e.do("PUT", path, tok, map[string]any{"securityGroups": groups})
	}
	row := func() store.Sandbox {
		t.Helper()
		sb, err := e.st.GetSandbox(context.Background(), podid.ID(id))
		if err != nil {
			t.Fatal(err)
		}
		return sb
	}

	// Never started: no pod to move.
	w := put([]string{"web"})
	expect(t, w, http.StatusAccepted, "")
	if v := decode[sandboxAnswer](t, w); !slices.Equal(v.SecurityGroups, []string{"web"}) || v.Environment.Name != "box" {
		t.Fatalf("view %+v", v)
	}
	if r := row(); !slices.Equal(r.SecurityGroups, []string{"web"}) || r.GroupsPending {
		t.Fatalf("pending row %+v", r)
	}
	// Running: the move is pending.
	if _, err := e.st.Q().Exec(context.Background(), "UPDATE sandboxes SET client_state = 'running' WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	expect(t, put([]string{"db", "web"}), http.StatusAccepted, "")
	if r := row(); !slices.Equal(r.SecurityGroups, []string{"db", "web"}) || !r.GroupsPending {
		t.Fatalf("running row %+v", r)
	}
	// The same list in another order: nothing to do.
	if _, err := e.st.Q().Exec(context.Background(), "UPDATE sandboxes SET groups_pending = false WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	expect(t, put([]string{"web", "db"}), http.StatusOK, "")
	if r := row(); r.GroupsPending {
		t.Fatalf("unchanged row %+v", r)
	}
	// Hibernated: its restore takes them.
	if _, err := e.st.Q().Exec(context.Background(), "UPDATE sandboxes SET client_state = 'hibernated' WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	expect(t, put([]string{"default"}), http.StatusAccepted, "")
	if r := row(); !slices.Equal(r.SecurityGroups, []string{"default"}) || r.GroupsPending {
		t.Fatalf("hibernated row %+v", r)
	}

	expect(t, put([]string{}), http.StatusBadRequest, errBody("securityGroups is empty: name at least one group (leave it out for default)"))
	expect(t, e.do("PUT", path, tok, map[string]any{}), http.StatusBadRequest, errBody("securityGroups is required"))
	expect(t, put([]string{"cache"}), http.StatusUnprocessableEntity,
		errBody("securityGroups: the environment's deployed deployment has no group cache; it has db, default, web"))
	expect(t, e.do("PUT", "/api/v1/sandboxes/"+podid.Mint().String()+"/security-groups", tok,
		map[string]any{"securityGroups": []string{"web"}}), http.StatusNotFound, "")
}

// An environment's delete is refused while a sandbox or a running job is
// in it (409 environment_in_use, naming them, ADR 0035).
func TestEnvironmentInUse(t *testing.T) {
	t.Parallel()
	w := apierr.EnvironmentInUse("dev", []string{"a0000000001"}, nil)
	rec := httptest.NewRecorder()
	apierr.Write(rec, w)
	expect(t, rec, http.StatusConflict, `{"error":"environment dev has 1 sandboxes and 0 running jobs: delete them first",`+
		`"code":"environment_in_use","sandboxes":["a0000000001"],"jobs":[]}`+"\n")
}
