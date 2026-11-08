// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/env/spec"
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
	Reaches     json.RawMessage `json:"reaches"`
	Egress      json.RawMessage `json:"egress"`
	Attachable  *bool           `json:"attachable"`
	SecurityGrp *[]string       `json:"securityGroups"`
}

// deploySpec gives environment id a deployed deployment of spec raw: what
// a sandbox's reaches may name (ADR 0035, revised).
func (e *env) deploySpec(id uuid.UUID, raw []byte) {
	e.t.Helper()
	ctx := context.Background()
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
// ID, reaching nothing of it unless the create names entries of its
// deployed deployment (or "all"), its egress the internet unless it names
// one; attachable and securityGroups are refused; the views name the
// environment, the reaches and the egress; the row has the template every sandbox's pod is made from
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
	expect(t, create(map[string]any{"environment": "box", "securityGroups": []string{"default"}}), http.StatusBadRequest,
		errBody("securityGroups is gone (ADR 0034, revised); use reaches"))
	expect(t, create(map[string]any{"environment": "box", "reaches": []any{map[string]any{"service": "web"}}}),
		http.StatusBadRequest, errBody("reaches[0]: name the service's port"))
	expect(t, create(map[string]any{"environment": "box", "egress": map[string]any{"domains": []string{"Bad Name"}}}),
		http.StatusBadRequest, errBody(`egress.domains[0]: "Bad Name" is not a domain`))
	expect(t, create(map[string]any{"environment": "box", "reaches": []any{map[string]any{"addon": "db"}}}),
		http.StatusUnprocessableEntity,
		errBody(spec.UnknownReaches(spec.Reaches{Entries: []spec.Use{{Addon: "db"}}}, nil)))

	w := create(map[string]any{"environment": "box"})
	expect(t, w, http.StatusCreated, "")
	a := decode[struct {
		Status sandboxAnswer `json:"status"`
	}](t, w).Status
	if a.Environment.ID != e.box.String() || a.Environment.Name != "box" || string(a.Reaches) != `[]` ||
		string(a.Egress) != `"internet"` || a.Attachable != nil || a.SecurityGrp != nil {
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
		row.LogLabels[model.LogObject] != a.SandboxID ||
		!slices.Equal(tpl.SecurityGroups, []string{model.EnvGroupID(e.box), model.SandboxWorkloadGroupID(a.SandboxID)}) ||
		tpl.RestrictEgressNames {
		t.Fatalf("row %+v template %+v", row, tpl)
	}

	// Another environment, by its ID, reaching what its deployed
	// deployment has, with an egress of its own.
	dev := e.mkEnv(testTenant, "dev")
	e.deploySpec(dev, []byte(devSpec))
	w = create(map[string]any{"environment": dev.String(),
		"reaches": []any{map[string]any{"service": "web", "port": "http"}, map[string]any{"addon": "db"}},
		"egress":  map[string]any{"domains": []string{"api.stripe.com"}}})
	expect(t, w, http.StatusCreated, "")
	b := decode[struct {
		Status sandboxAnswer `json:"status"`
	}](t, w).Status
	if b.Environment.Name != "dev" || string(b.Reaches) != `[{"service":"web","port":"http"},{"addon":"db"}]` ||
		string(b.Egress) != `{"domains":["api.stripe.com"]}` {
		t.Fatalf("in dev %+v (%s, %s)", b, b.Reaches, b.Egress)
	}
	get := decode[sandboxAnswer](t, e.do("GET", "/api/v1/sandboxes/"+b.SandboxID, tok, nil))
	if get.Environment.ID != dev.String() || string(get.Reaches) != string(b.Reaches) {
		t.Fatalf("get %+v", get)
	}
	if row, err := e.st.GetSandbox(context.Background(), podid.ID(b.SandboxID)); err != nil ||
		!row.PodTemplate.RestrictEgressNames || !slices.Equal(row.PodTemplate.EgressDomains, []string{"api.stripe.com"}) {
		t.Fatalf("dev's template %+v %v", row.PodTemplate, err)
	}
	expect(t, create(map[string]any{"environment": "dev", "reaches": []any{map[string]any{"service": "web", "port": 9}}}),
		http.StatusUnprocessableEntity, errBody(spec.UnknownReaches(spec.Reaches{Entries: []spec.Use{{Service: "web",
			Port: spec.PortRef{Number: 9}}}}, mustSpec(t, devSpec))))

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

// devSpec is a deployed deployment's spec a sandbox's reaches may name.
const devSpec = `{"services": {"web": {"image": "img", "ports": [{"name": "http", "port": 8080}]}},
	"addons": {"db": {"type": "postgres"}}}`

// mustSpec reads a spec.
func mustSpec(t *testing.T, raw string) *spec.Spec {
	t.Helper()
	var sp spec.Spec
	if err := json.Unmarshal([]byte(raw), &sp); err != nil {
		t.Fatal(err)
	}
	return &sp
}

// PUT /api/v1/sandboxes/{id}/reaches changes what the sandbox reaches in
// place (ADR 0035, revised): 200 with the view, no move, whatever state
// it is in; checked as a create's. The old route is gone.
func TestSandboxReaches(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	tok := bearerOf(e.tenantToken)
	e.deploySpec(e.box, []byte(devSpec))
	id, _ := e.createSandbox()
	path := "/api/v1/sandboxes/" + id + "/reaches"
	row := func() store.Sandbox {
		t.Helper()
		sb, err := e.st.GetSandbox(context.Background(), podid.ID(id))
		if err != nil {
			t.Fatal(err)
		}
		return sb
	}
	before := row()
	if !reflect.DeepEqual(before.PodTemplate, model.SandboxTemplate(before.EnvironmentID, id, false, nil)) {
		t.Fatalf("a new sandbox's template %+v", before.PodTemplate)
	}
	put := func(reaches any) *httptest.ResponseRecorder {
		t.Helper()
		return e.do("PUT", path, tok, map[string]any{"reaches": reaches})
	}

	w := put([]any{map[string]any{"addon": "db"}})
	expect(t, w, http.StatusOK, "")
	if v := decode[sandboxAnswer](t, w); string(v.Reaches) != `[{"addon":"db"}]` || v.Environment.Name != "box" {
		t.Fatalf("view %+v", v)
	}
	if _, err := e.st.Q().Exec(context.Background(), "UPDATE sandboxes SET client_state = 'running' WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	w = put("all")
	expect(t, w, http.StatusOK, "")
	if v := decode[sandboxAnswer](t, w); string(v.Reaches) != `"all"` {
		t.Fatalf("all %+v", v)
	}
	// In place: the row's state, template and gen stay.
	if r := row(); !r.Reaches.All || r.ClientState != model.StateRunning || r.Gen != before.Gen ||
		!reflect.DeepEqual(r.PodTemplate, before.PodTemplate) {
		t.Fatalf("running row %+v", r)
	}
	expect(t, put([]any{}), http.StatusOK, "")
	if r := row(); r.Reaches.All || len(r.Reaches.Entries) != 0 {
		t.Fatalf("nothing %+v", r.Reaches)
	}

	expect(t, e.do("PUT", path, tok, map[string]any{}), http.StatusBadRequest,
		errBody(`reaches is required: "all" or a list of {"service": name, "port": port} and {"addon": name}`))
	expect(t, put("some"), http.StatusBadRequest, "")
	expect(t, put([]any{map[string]any{"service": "web", "port": "grpc"}}), http.StatusUnprocessableEntity,
		errBody(spec.UnknownReaches(spec.Reaches{Entries: []spec.Use{{Service: "web", Port: spec.PortRef{Name: "grpc"}}}},
			mustSpec(t, devSpec))))
	expect(t, e.do("PUT", "/api/v1/sandboxes/"+podid.Mint().String()+"/reaches", tok,
		map[string]any{"reaches": "all"}), http.StatusNotFound, "")
	expect(t, e.do("PUT", "/api/v1/sandboxes/"+id+"/security-groups", tok,
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
