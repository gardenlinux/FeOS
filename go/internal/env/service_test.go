// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/env/addons"
	"github.com/gardenlinux/feos/go/internal/env/ctl"
	"github.com/gardenlinux/feos/go/internal/env/envdeps"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

func TestCreateListGetDelete(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	id := uuid.New().String()
	labels := map[string]string{"fecode.project": "p1", "fecode.class": "stage"}
	st, created, err := h.svc.CreateEnv(ctx, tenantA, CreateEnvRequest{Name: "stage", ID: id, Labels: labels})
	if err != nil || !created || st.ID != id || st.Name != "stage" || st.Deployment != 0 || st.Rollout != nil ||
		st.Labels["fecode.class"] != "stage" || st.CreatedAt == "" || st.Objects == nil || st.Routes == nil {
		t.Fatalf("create: %+v %v %v", st, created, err)
	}
	// The same ID and name again: the environment, not created.
	again, created, err := h.svc.CreateEnv(ctx, tenantA, CreateEnvRequest{Name: "stage", ID: strings.ToUpper(id)})
	if err != nil || created || again.ID != id {
		t.Fatalf("again: %+v %v %v", again, created, err)
	}
	var ae *apierr.Error
	_, _, err = h.svc.CreateEnv(ctx, tenantA, CreateEnvRequest{Name: "other", ID: id})
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict || ae.Code != CodeIDTaken ||
		ae.Msg != "an environment of id "+id+" exists with another name" {
		t.Fatalf("id taken: %v", err)
	}
	_, _, err = h.svc.CreateEnv(ctx, tenantA, CreateEnvRequest{Name: "stage"})
	if !errors.As(err, &ae) || ae.Code != CodeNameTaken || ae.Msg != "an environment named stage exists with another id" {
		t.Fatalf("name taken: %v", err)
	}
	// Another tenant's ID is taken too; its names are not.
	_, _, err = h.svc.CreateEnv(ctx, tenantB, CreateEnvRequest{Name: "stage", ID: id})
	if !errors.As(err, &ae) || ae.Code != CodeIDTaken {
		t.Fatalf("foreign id: %v", err)
	}
	if _, created, err := h.svc.CreateEnv(ctx, tenantB, CreateEnvRequest{Name: "stage"}); err != nil || !created {
		t.Fatalf("another tenant's name: %v", err)
	}
	for _, c := range []CreateEnvRequest{{Name: "Stage"}, {Name: ""}, {Name: id}, {Name: "x", ID: "nope"},
		{Name: "x", Labels: map[string]string{"": "v"}}} {
		_, _, err := h.svc.CreateEnv(ctx, tenantA, c)
		wantStatus(t, err, http.StatusBadRequest, "")
	}
	h.create("prod", map[string]string{"fecode.project": "p1", "fecode.class": "production"})
	h.create("alpha", map[string]string{"fecode.project": "p2"})

	// By UUID or name; another tenant's is absent.
	for _, ref := range []string{"stage", id, strings.ToUpper(id)} {
		if st, err := h.svc.GetEnv(ctx, tenantA, ref); err != nil || st.ID != id {
			t.Fatalf("get %s: %+v %v", ref, st, err)
		}
	}
	for _, ref := range []string{"nope", uuid.NewString(), "Bad Name", "prod%2f"} {
		_, err := h.svc.GetEnv(ctx, tenantA, ref)
		wantStatus(t, err, http.StatusNotFound, "no such environment")
	}
	_, err = h.svc.GetEnv(ctx, tenantB, id)
	wantStatus(t, err, http.StatusNotFound, "no such environment")

	// The listing: by name, labels ANDed.
	list, err := h.svc.ListEnvs(ctx, tenantA, nil)
	if err != nil || len(list.Environments) != 3 || list.Environments[0].Name != "alpha" ||
		list.Environments[2].Name != "stage" || list.Environments[2].State != model.EnvNone {
		t.Fatalf("list %+v %v", list, err)
	}
	list, _ = h.svc.ListEnvs(ctx, tenantA, map[string]string{"fecode.project": "p1"})
	if len(list.Environments) != 2 || list.Environments[0].Name != "prod" {
		t.Fatalf("list p1 %+v", list)
	}
	list, _ = h.svc.ListEnvs(ctx, tenantA, map[string]string{"fecode.project": "p1", "fecode.class": "stage"})
	if len(list.Environments) != 1 || list.Environments[0].ID != id {
		t.Fatalf("list p1 stage %+v", list)
	}
	if list, _ := h.svc.ListEnvs(ctx, tenantB, nil); len(list.Environments) != 1 {
		t.Fatalf("tenant B %+v", list)
	}

	// Delete: marked, the controller woken; again is no error.
	ch, unsubscribe := h.d.Subscribe(db.ChanLeader)
	defer unsubscribe()
	if err := h.svc.DeleteEnv(ctx, tenantA, "stage"); err != nil {
		t.Fatal(err)
	}
	waitHint(t, ch, ctl.LoopDelete)
	if err := h.svc.DeleteEnv(ctx, tenantA, id); err != nil {
		t.Fatal(err)
	}
	if st, _ := h.svc.GetEnv(ctx, tenantA, id); !st.Deleting {
		t.Fatalf("not deleting: %+v", st)
	}
	list, _ = h.svc.ListEnvs(ctx, tenantA, map[string]string{"fecode.class": "stage"})
	if list.Environments[0].State != model.EnvDeleting {
		t.Fatalf("state %+v", list)
	}
	err = h.svc.DeleteEnv(ctx, tenantB, "prod")
	wantStatus(t, err, http.StatusNotFound, "no such environment")
	_, err = h.svc.Apply(ctx, tenantA, "stage", ApplyRequest{Spec: json.RawMessage(`{}`)})
	wantStatus(t, err, http.StatusConflict, "environment stage is being deleted")
	_, err = h.svc.PutSecret(ctx, tenantA, "stage", "token", PutSecretRequest{Value: "x"})
	wantStatus(t, err, http.StatusConflict, "being deleted")

	// A deleting tenant creates nothing.
	h.exec("UPDATE tenants SET deleting_at = now() WHERE id = $1", tenantB)
	_, _, err = h.svc.CreateEnv(ctx, tenantB, CreateEnvRequest{Name: "new"})
	wantStatus(t, err, http.StatusConflict, "tenant other is being deleted")
}

func TestPlanApply(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	st := h.create("stage", nil)

	// No secret yet: an error finding, the diff all adds.
	p, err := h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(stageSpec)})
	if err != nil || p.Valid || p.Base != 0 || p.Current != 0 || len(p.Diff) != 7 ||
		!hasFinding(p.Findings, spec.SeverityError, "services.api.env.TOKEN", "no secret token") {
		t.Fatalf("plan %+v %v", p, err)
	}
	// The plan says what the spec admits (ADR 0034, revised).
	if r := p.Reachability; len(r.Edges) != 6 || r.Edges[0] != (spec.Edge{From: "ingress", To: "service/api",
		Port: "http", Number: 8080, Via: "route api"}) || len(r.Egress) != 3 || r.Egress[0].Mode != spec.EgressInternet ||
		len(r.Routes) != 2 {
		t.Fatalf("reachability %+v", r)
	}
	_, err = h.svc.Apply(ctx, tenantA, "stage", ApplyRequest{Spec: json.RawMessage(stageSpec)})
	var ise *InvalidSpecError
	if !errors.As(err, &ise) || !hasFinding(ise.Findings, spec.SeverityError, "services.api.env.TOKEN", "no secret") {
		t.Fatalf("apply without the secret: %v", err)
	}
	if v := h.putSecret("stage", "token", "s3cret"); v != 1 {
		t.Fatalf("version %d", v)
	}
	p, err = h.svc.Plan(ctx, tenantA, st.ID, PlanRequest{Spec: json.RawMessage(stageSpec)})
	if err != nil || !p.Valid || len(p.Findings) != 0 {
		t.Fatalf("plan %+v %v", p, err)
	}

	// Apply: deployment 1, waiting, the controller woken.
	ch, unsubscribe := h.d.Subscribe(db.ChanLeader)
	defer unsubscribe()
	a := h.apply("stage", stageSpec, 0)
	waitHint(t, ch, ctl.HintReconcile)
	if a.Number != 1 || a.State != model.DeploymentWaiting || a.Origin != model.OriginApply || a.Base != 0 ||
		a.Unchanged || a.StatusCode() != http.StatusCreated || len(a.Diff) != 8 || a.Author != "tester" {
		t.Fatalf("apply %+v", a)
	}
	e := h.env("stage")
	if e.Deployment != 1 || e.Deployed != 0 {
		t.Fatalf("env %+v", e)
	}
	stored, err := h.st.GetDeployment(ctx, e.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := parseStored(stored.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if api := sp.Services["api"]; api.Image != digestOf("ghcr.io/acme/api:1") || api.ImageAsWritten != "ghcr.io/acme/api:1" ||
		*api.Replicas != 2 || sp.Secrets["token"].Version != 1 || sp.Addons["pg"].Plan != "small" ||
		sp.Addons["q"].Plan != "small" || sp.Jobs["migrate"].Timeout != "10m0s" {
		t.Fatalf("stored %s", stored.Spec)
	}

	// The same spec again: unchanged, whatever the base.
	for _, base := range []int{1, 0} {
		a = h.apply("stage", stageSpec, base)
		if !a.Unchanged || a.Number != 1 || a.StatusCode() != http.StatusOK || len(a.Diff) != 0 ||
			!hasFinding(a.Findings, spec.SeverityInfo, "", "nothing changes: the spec equals deployment 1's") {
			t.Fatalf("unchanged %+v", a)
		}
	}
	// A change on a stale base: 409 with the current deployment.
	changed := strings.Replace(stageSpec, `"replicas": 2`, `"replicas": 3`, 1)
	_, err = h.svc.Apply(ctx, tenantA, "stage", ApplyRequest{Spec: json.RawMessage(changed), Base: 0})
	var bm *BaseMismatchError
	if !errors.As(err, &bm) || bm.Current != 1 || bm.Base != 0 {
		t.Fatalf("base mismatch: %v", err)
	}
	// The plan of a change against the current deployment and an older base.
	p, _ = h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(changed)})
	if len(p.Diff) != 1 || p.Diff[0].Effect != spec.EffectInPlace || p.Diff[0].Fields[0].Path != "replicas" {
		t.Fatalf("plan diff %+v", p.Diff)
	}
	zero, far := 0, 9
	p, _ = h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(changed), Base: &zero})
	if p.Base != 0 || p.Current != 1 || len(p.Diff) != 8 {
		t.Fatalf("plan on base 0 %+v", p)
	}
	_, err = h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(changed), Base: &far})
	wantStatus(t, err, http.StatusNotFound, "no such deployment")

	// On the current base: deployment 2, the waiting 1 replaced.
	a = h.apply("stage", changed, 1)
	if a.Number != 2 || a.Base != 1 {
		t.Fatalf("apply %+v", a)
	}
	if r1, _ := h.st.GetDeployment(ctx, e.ID, 1); r1.State != model.DeploymentReplaced || r1.FinishedAt.IsZero() {
		t.Fatalf("deployment 1 %+v", r1)
	}

	// A failed deployment is retried by the same spec.
	h.exec("UPDATE env_deployments SET state = 'failed', reason = 'rollout timed out' WHERE env_id = $1 AND number = 2", e.ID)
	a = h.apply("stage", changed, 2)
	if a.Unchanged || a.Number != 3 || !hasFinding(a.Findings, spec.SeverityInfo, "", "nothing changes") {
		t.Fatalf("retry %+v", a)
	}

	// A new secret version moves the pins: the dependents are replaced.
	h.putSecret("stage", "token", "next")
	a = h.apply("stage", changed, 3)
	if a.Number != 4 || len(a.Diff) != 2 || a.Diff[0].Name != "api" || a.Diff[0].Effect != "replaces 3 pods" ||
		a.Diff[1].Kind != spec.KindSecret {
		t.Fatalf("new version %+v", a.Diff)
	}

	// Images: unresolvable is a warning in a plan and an error in an apply.
	h.eng.mu.Lock()
	h.eng.bad["ghcr.io/acme/web:2"] = "manifest unknown"
	h.eng.mu.Unlock()
	broken := strings.Replace(changed, "ghcr.io/acme/web:1", "ghcr.io/acme/web:2", 1)
	p, _ = h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(broken)})
	if !p.Valid || !hasFinding(p.Findings, spec.SeverityWarning, "services.web.image", "manifest unknown") {
		t.Fatalf("plan %+v", p)
	}
	_, err = h.svc.Apply(ctx, tenantA, "stage", ApplyRequest{Spec: json.RawMessage(broken), Base: 4})
	if !errors.As(err, &ise) || !hasFinding(ise.Findings, spec.SeverityError, "services.web.image", "could not be resolved") {
		t.Fatalf("apply %v", err)
	}

	// The addons' effects come from the blueprints.
	reprov := strings.Replace(changed, `"roles": ["api_svc"]`, `"roles": ["api_svc", "report_svc"]`, 1)
	p, _ = h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(reprov)})
	if len(p.Diff) != 1 || p.Diff[0].Effect != spec.EffectReprovisions {
		t.Fatalf("reprovision %+v", p.Diff)
	}
	recreate := strings.Replace(changed, `"q": {"type": "nats", "version": "2"}`,
		`"q": {"type": "nats", "version": "2", "options": {"jetstream": false}}`, 1)
	p, _ = h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(recreate)})
	if len(p.Diff) != 1 || p.Diff[0].Effect != spec.EffectRecreates {
		t.Fatalf("recreate %+v", p.Diff)
	}
	// A major version change against the current deployment is refused.
	major := strings.Replace(changed, `"version": "17"`, `"version": "16"`, 1)
	p, _ = h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(major)})
	if p.Valid || !hasFinding(p.Findings, spec.SeverityError, "addons.pg.version", "") {
		t.Fatalf("major %+v", p)
	}

	// Parse errors: no diff.
	p, err = h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(`{"services": {"x": {"replica": 1}}}`)})
	if err != nil || p.Valid || len(p.Diff) != 0 || p.Diff == nil || !hasFinding(p.Findings, spec.SeverityError,
		"services.x.replica", "unknown field") {
		t.Fatalf("parse errors %+v %v", p, err)
	}
	// Annotations are bounded.
	_, err = h.svc.Apply(ctx, tenantA, "stage", ApplyRequest{Spec: json.RawMessage(changed), Base: 4,
		Annotations: map[string]string{"k": strings.Repeat("v", 5000)}})
	wantStatus(t, err, http.StatusBadRequest, "annotations take")
	// Another tenant's environment is absent.
	_, err = h.svc.Plan(ctx, tenantB, "stage", PlanRequest{Spec: json.RawMessage(changed)})
	wantStatus(t, err, http.StatusNotFound, "no such environment")
}

// A plan answers the pins it resolved (ADR 0032); a client that writes
// them into its spec applies what the plan showed, though the tag moved
// in between.
func TestPlanImages(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	h.create("stage", nil)
	const tag, other = "ghcr.io/acme/api:1", "ghcr.io/acme/tool:3"
	pinnedDigest := "docker.io/library/nginx@sha256:" + strings.Repeat("c", 64)
	h.eng.mu.Lock()
	h.eng.bad["ghcr.io/acme/broken:1"] = "manifest unknown"
	h.eng.mu.Unlock()
	raw := `{"services": {"api": {"image": "` + tag + `"}, "web": {"image": "` + pinnedDigest + `",
		"imageAsWritten": "nginx:1.27"}, "broken": {"image": "ghcr.io/acme/broken:1"}},
		"jobs": {"migrate": {"image": "` + tag + `"}, "tool": {"image": "` + other + `"}}}`
	p, err := h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(raw)})
	want := map[string]string{tag: digestOf(tag), other: digestOf(other)}
	if err != nil || !p.Valid || !reflect.DeepEqual(p.Images, want) ||
		!hasFinding(p.Findings, spec.SeverityWarning, "services.broken.image", "manifest unknown") {
		t.Fatalf("plan %+v %v", p, err)
	}
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"images":{"ghcr.io/acme/api:1":"ghcr.io/acme/api@sha256:`) {
		t.Fatalf("answer %s", b)
	}
	// The spec without parse errors only has images; one that does not
	// parse answers {}.
	p, _ = h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(`{"services": 1}`)})
	if p.Images == nil || len(p.Images) != 0 {
		t.Fatalf("unparsed %+v", p.Images)
	}
	ps, err := h.svc.PlanSpec(ctx, tenantA, PlanSpecRequest{Spec: json.RawMessage(raw)})
	if err != nil || !reflect.DeepEqual(ps.Images, want) {
		t.Fatalf("plan spec %+v %v", ps, err)
	}

	// The client writes the pins in; then the tag moves.
	draft := `{"services": {"api": {"image": "` + want[tag] + `", "imageAsWritten": "` + tag + `"}}}`
	p, _ = h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(draft)})
	if !p.Valid || len(p.Images) != 0 {
		t.Fatalf("plan of the pinned draft %+v", p)
	}
	h.eng.mu.Lock()
	h.eng.moved = map[string]string{tag: "ghcr.io/acme/api@sha256:" + strings.Repeat("d", 64)}
	h.eng.mu.Unlock()
	a := h.apply("stage", draft, 0)
	e := h.env("stage")
	stored, err := h.st.GetDeployment(ctx, e.ID, a.Number)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := parseStored(stored.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if api := sp.Services["api"]; api.Image != want[tag] || api.ImageAsWritten != tag {
		t.Fatalf("stored %s", stored.Spec)
	}
	// The tag as written resolves where it moved: an update.
	p, _ = h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(`{"services": {"api": {"image": "` +
		tag + `"}}}`)})
	if p.Images[tag] != h.eng.moved[tag] || len(p.Diff) != 1 || p.Diff[0].Name != "api" {
		t.Fatalf("checked %+v", p)
	}
}

// The catalog answers the registered blueprints as they declare
// themselves; a type Deps.Blueprint does not run is left out.
func TestAddonCatalog(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	c, err := h.svc.Addons(h.ctx(), tenantA)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, a := range c.Addons {
		types = append(types, a.Type)
	}
	if !slices.Equal(types, []string{"nats", "postgres", "valkey"}) {
		t.Fatalf("types %v", types)
	}
	pg := c.Addons[1]
	if !slices.Equal(pg.Versions, []string{"16", "17"}) || pg.DefaultVersion != "17" || pg.DefaultPlan != "small" ||
		len(pg.Plans) != 4 || pg.Plans[0] != (AddonPlan{Name: "dev", VCPUs: 1, MemoryMib: 256, VolumeMib: 1024}) ||
		pg.Plans[3].Name != "large" || !slices.Equal(pg.OutputFields, []string{"role", "database"}) ||
		len(pg.Options) != 2 || pg.Options[1].Name != "roles" || !slices.Equal(pg.Options[1].ReservedPrefixes, []string{"pg_"}) {
		t.Fatalf("postgres %+v", pg)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	// The API's fixture, which the UI's tests read, is the catalog.
	raw, err := os.ReadFile("../api/testdata/ferun/GET_addons.200.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	var want, got any
	_ = json.Unmarshal(fx.Response, &want)
	_ = json.Unmarshal(b, &got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the fixture GET_addons.200.json is not the catalog:\n%s", b)
	}
	// Every field is there, lists as [] when empty.
	for _, s := range []string{`"type":"nats"`, `"outputFields":[]`, `"options":[{"name":"jetstream","kind":"boolean",` +
		`"required":false,"default":true,`, `"values":[],"pattern":"","reserved":[],"reservedPrefixes":[],"unique":false}`,
		`"kind":"oneOf","required":false,"default":"noeviction"`, `"default":[]`} {
		if !strings.Contains(string(b), s) {
			t.Fatalf("catalog lacks %s:\n%s", s, b)
		}
	}
	// A ferund that does not run a type does not offer it.
	without := &Service{d: Deps{Blueprint: func(typ string) (addons.Blueprint, bool) {
		if typ == "nats" {
			return nil, false
		}
		return addons.Get(typ)
	}}}
	if c, _ := without.Addons(h.ctx(), tenantA); len(c.Addons) != 2 || c.Addons[0].Type != "postgres" {
		t.Fatalf("without nats %+v", c)
	}
}

func TestPlanSpec(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	sb := h.sandbox(tenantA, "box")
	foreign := h.sandbox(tenantB, "box")
	raw := `{"services": {"web": {"runtime": "attached", "sandbox": "` + sb.String() + `", "command": ["npm", "run", "dev"],
		"env": {"T": {"secret": "absent"}}, "ports": [{"name": "http", "port": 5173}]},
		"api": {"image": "ghcr.io/acme/api:9"}}}`
	h.eng.bad["ghcr.io/acme/api:9"] = "manifest unknown"
	a, err := h.svc.PlanSpec(ctx, tenantA, PlanSpecRequest{Spec: json.RawMessage(raw)})
	if err != nil || !a.Valid || len(a.Findings) != 1 ||
		!hasFinding(a.Findings, spec.SeverityWarning, "services.api.image", "manifest unknown") {
		t.Fatalf("plan spec %+v %v", a, err)
	}
	// Another tenant's sandbox is none of yours; an instance row neither.
	a, _ = h.svc.PlanSpec(ctx, tenantA, PlanSpecRequest{Spec: json.RawMessage(strings.Replace(raw, sb.String(),
		foreign.String(), 1))})
	if a.Valid || !hasFinding(a.Findings, spec.SeverityError, "services.web.sandbox", "no sandbox") {
		t.Fatalf("foreign sandbox %+v", a)
	}
	a, _ = h.svc.PlanSpec(ctx, tenantA, PlanSpecRequest{})
	if a.Valid || !hasFinding(a.Findings, spec.SeverityError, "", "no spec") {
		t.Fatalf("no spec %+v", a)
	}
}

func TestAttachedValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	h.create("one", nil)
	h.create("two", nil)
	sb := h.sandbox(tenantA, "one")
	plain := h.sandbox(tenantA, "two")
	attachedAs := func(sandbox podid.ID, name, port string) string {
		return `{"services": {"` + name + `": {"runtime": "attached", "sandbox": "` + sandbox.String() + `", "command": ["vite"],
			"ports": [{"name": "http", "port": ` + port + `}]}}, "routes": {"root": {"path": "/", "service": "` + name +
			`", "port": "http"}}}`
	}
	attached := func(sandbox podid.ID, port string) string { return attachedAs(sandbox, "vite", port) }
	h.apply("one", attached(sb, "5173"), 0)
	// The sandbox is environment one's (ADR 0035): another environment's
	// attached service cannot run in it.
	p, err := h.svc.Plan(ctx, tenantA, "two", PlanRequest{Spec: json.RawMessage(attachedAs(sb, "web", "5174"))})
	if err != nil || p.Valid || !hasFinding(p.Findings, spec.SeverityError, "services.web.sandbox",
		"sandbox "+sb.String()+" is in environment one: an attached service runs in a sandbox of its own environment") {
		t.Fatalf("other environment %+v %v", p, err)
	}
	// Its own environment's deployments do.
	if p, _ := h.svc.Plan(ctx, tenantA, "one", PlanRequest{Spec: json.RawMessage(attached(sb, "5174"))}); !p.Valid {
		t.Fatalf("own %+v", p)
	}
	// Environment two's sandbox is its own.
	if p, _ := h.svc.Plan(ctx, tenantA, "two", PlanRequest{Spec: json.RawMessage(attachedAs(plain, "web", "5173"))}); !p.Valid {
		t.Fatalf("separate sandboxes %+v", p)
	}
	// A sandbox with a UUID, made before ADR 0019, is a sandbox as a short
	// ID's is, in either case; a value that is neither is refused.
	legacy := h.legacySandbox(tenantA, "two")
	for _, id := range []podid.ID{legacy, podid.ID(strings.ToUpper(legacy.String()))} {
		if p, _ := h.svc.Plan(ctx, tenantA, "two", PlanRequest{Spec: json.RawMessage(attachedAs(id, "web", "5176"))}); !p.Valid {
			t.Fatalf("legacy sandbox %s: %+v", id, p)
		}
	}
	for _, bad := range []podid.ID{"nope", podid.ID(strings.ToUpper(plain.String()))} {
		if p, _ := h.svc.Plan(ctx, tenantA, "two", PlanRequest{Spec: json.RawMessage(attachedAs(bad, "web", "5176"))}); p.Valid ||
			!hasFinding(p.Findings, spec.SeverityError, "services.web.sandbox", "is not a sandbox ID") {
			t.Fatalf("sandbox %q: %+v", bad, p)
		}
	}
	// An attached service is its sandbox's pod: no egress of its own, and
	// securityGroups is gone.
	withEgress := strings.Replace(attached(sb, "5173"), `"command": ["vite"],`, `"command": ["vite"], "egress": "none",`, 1)
	if p, _ := h.svc.Plan(ctx, tenantA, "one", PlanRequest{Spec: json.RawMessage(withEgress)}); p.Valid ||
		!hasFinding(p.Findings, spec.SeverityError, "services.vite.egress", "and has its egress") {
		t.Fatalf("egress %+v", p)
	}
	withGroups := strings.Replace(attached(sb, "5173"), `"command": ["vite"],`, `"command": ["vite"], "securityGroups": ["default"],`, 1)
	if p, _ := h.svc.Plan(ctx, tenantA, "one", PlanRequest{Spec: json.RawMessage(withGroups)}); p.Valid ||
		!hasFinding(p.Findings, spec.SeverityError, "services.vite.securityGroups", "securityGroups is gone") {
		t.Fatalf("groups %+v", p)
	}
	// An instance row is no sandbox.
	inst := h.instance(h.env("one").ID, spec.KindService, "x", 0, 1, model.PhaseReady, model.StateRunning, "n1", "fd00::1")
	if p, _ := h.svc.Plan(ctx, tenantA, "two", PlanRequest{Spec: json.RawMessage(attached(inst, "5173"))}); p.Valid {
		t.Fatalf("an instance row as sandbox %+v", p)
	}
}

func TestDeploymentsAndRevert(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	h.create("stage", nil)
	h.putSecret("stage", "token", "one")
	if _, err := h.svc.Apply(ctx, tenantA, "stage", ApplyRequest{Spec: json.RawMessage(stageSpec), Author: "a1",
		Message: "first", Annotations: map[string]string{"fecode.dev/revision": "r1"}}); err != nil {
		t.Fatal(describe(err))
	}
	changed := strings.Replace(stageSpec, "ghcr.io/acme/web:1", "ghcr.io/acme/web:2", 1)
	h.apply("stage", changed, 1)
	if _, err := h.svc.PutTag(ctx, tenantA, "stage", "v1", PutTagRequest{Deployment: 1, Author: "a2"}); err != nil {
		t.Fatal(err)
	}

	list, err := h.svc.Deployments(ctx, tenantA, "stage", 0)
	if err != nil || len(list.Deployments) != 2 || list.Deployments[0].Number != 2 || list.Deployments[1].Tags[0] != "v1" ||
		list.Deployments[1].Message != "first" || list.Deployments[1].Annotations["fecode.dev/revision"] != "r1" ||
		list.Deployments[0].Tags == nil {
		t.Fatalf("deployments %+v %v", list, err)
	}
	if list, _ := h.svc.Deployments(ctx, tenantA, "stage", 1); len(list.Deployments) != 1 {
		t.Fatalf("limit %+v", list)
	}
	d, err := h.svc.Deployment(ctx, tenantA, "stage", 2)
	if err != nil || d.Number != 2 || len(d.Diff) != 1 || d.Diff[0].Name != "web" ||
		d.Diff[0].Fields[0].To != "ghcr.io/acme/web:2 ("+strings.SplitN(digestOf("ghcr.io/acme/web:2"), "@", 2)[1]+")" {
		t.Fatalf("deployment 2 %+v %v", d, err)
	}
	var sp spec.Spec
	if err := json.Unmarshal(d.Spec, &sp); err != nil || sp.Services["web"].ImageAsWritten != "ghcr.io/acme/web:2" {
		t.Fatalf("spec %s %v", d.Spec, err)
	}
	if d, _ := h.svc.Deployment(ctx, tenantA, "stage", 1); len(d.Diff) != 8 {
		t.Fatalf("deployment 1 diff %+v", d.Diff)
	}
	// What its spec admits (ADR 0034, revised): web → api, api and migrate
	// → pg, api → q, and the ingress to both routes.
	if d, _ := h.svc.Deployment(ctx, tenantA, "stage", 1); len(d.Reachability.Edges) != 6 ||
		d.Reachability.Edges[5] != (spec.Edge{From: "service/web", To: "service/api", Port: "http", Number: 8080,
			Via: "env API"}) || len(d.Reachability.Routes) != 2 || len(d.Reachability.Egress) != 3 {
		t.Fatalf("deployment 1 reachability %+v", d.Reachability)
	}
	_, err = h.svc.Deployment(ctx, tenantA, "stage", 3)
	wantStatus(t, err, http.StatusNotFound, "no such deployment")

	// Revert to 1: its spec, its annotations plus ferun.revertOf.
	a, err := h.svc.Revert(ctx, tenantA, "stage", 1, RevertRequest{Base: 2, Author: "a3", Message: "back"})
	if err != nil || a.Number != 3 || a.Origin != model.OriginRevert || a.OriginRef != "1" ||
		a.Annotations["ferun.revertOf"] != "1" || a.Annotations["fecode.dev/revision"] != "r1" || len(a.Diff) != 1 {
		t.Fatalf("revert %+v %v", a, err)
	}
	if a, err := h.svc.Revert(ctx, tenantA, "stage", 3, RevertRequest{Base: 3}); err != nil || !a.Unchanged {
		t.Fatalf("revert to the current %+v %v", a, err)
	}
	// A clone of the revert copies its annotations without ferun.revertOf,
	// whose number names one of the source's deployments.
	if _, err := h.svc.Clone(ctx, tenantA, "stage", CloneRequest{Name: "copy",
		Secrets: map[string]CloneSecret{"token": {Copy: true}}}); err != nil {
		t.Fatal(describe(err))
	}
	if r, err := h.svc.Deployment(ctx, tenantA, "copy", 1); err != nil || r.Annotations["fecode.dev/revision"] != "r1" ||
		len(r.Annotations) != 1 {
		t.Fatalf("the clone's deployment's annotations %v, %v", r.Annotations, err)
	}
	_, err = h.svc.Revert(ctx, tenantA, "stage", 1, RevertRequest{Base: 2})
	var bm *BaseMismatchError
	if errors.As(err, &bm) {
		// Deployment 1 equals the current 3: unchanged answers before the
		// base.
		t.Fatalf("revert %v", err)
	}
	_, err = h.svc.Revert(ctx, tenantA, "stage", 9, RevertRequest{Base: 3})
	wantStatus(t, err, http.StatusNotFound, "no such deployment")

	// A pinned version that was deleted refuses the revert.
	noSecret := strings.Replace(stageSpec, `"TOKEN": {"secret": "token"},`, ``, 1)
	h.apply("stage", noSecret, 3)
	if err := h.svc.DeleteSecret(ctx, tenantA, "stage", "token"); err != nil {
		t.Fatal(err)
	}
	_, err = h.svc.Revert(ctx, tenantA, "stage", 1, RevertRequest{Base: 4})
	var ise *InvalidSpecError
	if !errors.As(err, &ise) || !hasFinding(ise.Findings, spec.SeverityError, "secrets.token",
		"version 1 of secret token was deleted") {
		t.Fatalf("revert with a deleted version: %v", describe(err))
	}
}

func TestObjects(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	h.create("stage", nil)
	h.putSecret("stage", "token", "x")
	h.apply("stage", stageSpec, 0)

	web := json.RawMessage(`{"image": "ghcr.io/acme/web:1", "ports": [{"name": "http", "port": 3000}], "idle": "15m",
		"replicas": 3, "env": {"API": {"service": "api", "port": "http", "key": "url"}}}`)
	dry, err := h.svc.PutObject(ctx, tenantA, "stage", spec.KindService, "web", PutObjectRequest{Object: web, Base: 1,
		DryRun: true})
	if err != nil || !dry.DryRun || !dry.Plan.Valid || len(dry.Plan.Diff) != 1 || dry.Plan.Diff[0].Fields[0].Path != "replicas" ||
		dry.StatusCode() != http.StatusOK || dry.Plan.Base != 1 {
		t.Fatalf("dry run %+v %v", dry, err)
	}
	if e := h.env("stage"); e.Deployment != 1 {
		t.Fatalf("a dry run applied: %+v", e)
	}
	put, err := h.svc.PutObject(ctx, tenantA, "stage", spec.KindService, "web", PutObjectRequest{Object: web, Base: 1,
		Author: "a", Message: "three webs"})
	if err != nil || put.Apply.Number != 2 || put.Apply.Origin != model.OriginObject || put.Apply.OriginRef != "service/web" ||
		put.StatusCode() != http.StatusCreated {
		t.Fatalf("put %+v %v", put, err)
	}
	// A route goes; an absent object is 404; a bad kind 400.
	del, err := h.svc.DeleteObject(ctx, tenantA, "stage", spec.KindRoute, "api", DeleteObjectRequest{Base: 2})
	if err != nil || del.Apply.Number != 3 || del.Apply.OriginRef != "route/api" || len(del.Apply.Diff) != 1 ||
		del.Apply.Diff[0].Change != spec.ChangeRemove {
		t.Fatalf("delete %+v %v", del, err)
	}
	_, err = h.svc.DeleteObject(ctx, tenantA, "stage", spec.KindRoute, "api", DeleteObjectRequest{Base: 3})
	wantStatus(t, err, http.StatusNotFound, "no such object: route/api")
	_, err = h.svc.PutObject(ctx, tenantA, "stage", "volume", "v", PutObjectRequest{Object: json.RawMessage(`{}`), Base: 3})
	wantStatus(t, err, http.StatusBadRequest, "kind must be")
	_, err = h.svc.PutObject(ctx, tenantA, "stage", spec.KindService, "web", PutObjectRequest{Base: 3})
	wantStatus(t, err, http.StatusBadRequest, "object is required")
	// A dry run of a delete that breaks references: invalid, with the
	// diff.
	dry, err = h.svc.DeleteObject(ctx, tenantA, "stage", spec.KindAddon, "q", DeleteObjectRequest{Base: 3, DryRun: true})
	if err != nil || dry.Plan.Valid || !hasFinding(dry.Plan.Findings, spec.SeverityError, "services.api.env.Q", "no addon q") ||
		len(dry.Plan.Diff) != 1 || dry.Plan.Diff[0].Effect != spec.EffectDeletes {
		t.Fatalf("dry delete %+v %v", dry, err)
	}
	// Unknown fields in an object have their paths in the spec.
	_, err = h.svc.PutObject(ctx, tenantA, "stage", spec.KindJob, "seed", PutObjectRequest{
		Object: json.RawMessage(`{"image": "img", "schedule": "daily"}`), Base: 3})
	var ise *InvalidSpecError
	if !errors.As(err, &ise) || !hasFinding(ise.Findings, spec.SeverityError, "jobs.seed.schedule", "unknown field") {
		t.Fatalf("unknown field %v", describe(err))
	}
	// An object into an environment without a deployment.
	h.create("empty", nil)
	put, err = h.svc.PutObject(ctx, tenantA, "empty", spec.KindAddon, "q", PutObjectRequest{
		Object: json.RawMessage(`{"type": "nats", "version": "2"}`)})
	if err != nil || put.Apply.Number != 1 {
		t.Fatalf("first object %+v %v", put, describe(err))
	}
}

func TestTags(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	h.create("stage", nil)
	h.putSecret("stage", "token", "x")
	h.apply("stage", stageSpec, 0)
	tag, err := h.svc.PutTag(ctx, tenantA, "stage", "v1.4", PutTagRequest{Deployment: 1, Author: "a"})
	if err != nil || tag.Tag != "v1.4" || tag.Deployment != 1 || tag.MovedBy != "a" || tag.MovedAt == "" {
		t.Fatalf("put %+v %v", tag, err)
	}
	if tag, _ := h.svc.PutTag(ctx, tenantA, "stage", "stable", PutTagRequest{Deployment: 1}); tag.MovedBy != tenantA {
		t.Fatalf("moved by %+v", tag)
	}
	list, err := h.svc.Tags(ctx, tenantA, "stage")
	if err != nil || len(list.Tags) != 2 || list.Tags[0].Tag != "stable" {
		t.Fatalf("tags %+v %v", list, err)
	}
	if got, err := h.svc.Tag(ctx, tenantA, "stage", "v1.4"); err != nil || got.Deployment != 1 {
		t.Fatalf("tag %+v %v", got, err)
	}
	_, err = h.svc.Tag(ctx, tenantA, "stage", "nope")
	wantStatus(t, err, http.StatusNotFound, "no such tag")
	_, err = h.svc.PutTag(ctx, tenantA, "stage", "v2", PutTagRequest{Deployment: 7})
	wantStatus(t, err, http.StatusNotFound, "no such deployment")
	_, err = h.svc.PutTag(ctx, tenantA, "stage", ".bad", PutTagRequest{Deployment: 1})
	wantStatus(t, err, http.StatusBadRequest, "must match")
	if err := h.svc.DeleteTag(ctx, tenantA, "stage", "v1.4"); err != nil {
		t.Fatal(err)
	}
	err = h.svc.DeleteTag(ctx, tenantA, "stage", "v1.4")
	wantStatus(t, err, http.StatusNotFound, "no such tag")
	_, err = h.svc.Tags(ctx, tenantB, "stage")
	wantStatus(t, err, http.StatusNotFound, "no such environment")
}

func TestSecrets(t *testing.T) {
	t.Parallel()
	off := newHarness(t, harnessOptions{noSealer: true})
	off.create("stage", nil)
	ctx := off.ctx()
	_, err := off.svc.Secrets(ctx, tenantA, "stage")
	wantStatus(t, err, http.StatusServiceUnavailable, "secrets need --secret-key-file")
	_, err = off.svc.PutSecret(ctx, tenantA, "stage", "token", PutSecretRequest{Value: "x"})
	wantStatus(t, err, http.StatusServiceUnavailable, "secrets need --secret-key-file")
	err = off.svc.DeleteSecret(ctx, tenantA, "stage", "token")
	wantStatus(t, err, http.StatusServiceUnavailable, "secrets need --secret-key-file")
	if off.svc.SecretsEnabled() {
		t.Fatal("secrets without a sealer")
	}
	_, err = off.svc.ResolveSecret(ctx, uuid.New(), "token", 1)
	if err == nil || !strings.Contains(err.Error(), "--secret-key-file") {
		t.Fatalf("resolve without a sealer: %v", err)
	}

	h := newHarness(t, harnessOptions{})
	ctx = h.ctx()
	st := h.create("stage", nil)
	id := uuid.MustParse(st.ID)
	if v := h.putSecret("stage", "token", "one"); v != 1 {
		t.Fatal(v)
	}
	if v := h.putSecret(st.ID, "token", "two"); v != 2 {
		t.Fatal(v)
	}
	h.putSecret("stage", "other", "x")
	list, err := h.svc.Secrets(ctx, tenantA, "stage")
	if err != nil || len(list.Secrets) != 2 || list.Secrets[1].Name != "token" || list.Secrets[1].Version != 2 ||
		list.Secrets[1].CreatedBy != "tester" || list.Secrets[1].CreatedAt == "" {
		t.Fatalf("list %+v %v", list, err)
	}
	// Sealed under its own AAD: another version's does not open it.
	row, err := h.st.GetSecret(ctx, id, "token", 2)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := h.sealer.Open(row.Sealed, envdeps.SecretAAD(id, "token", 2)); err != nil || string(v) != "two" {
		t.Fatalf("open %q %v", v, err)
	}
	if _, err := h.sealer.Open(row.Sealed, envdeps.SecretAAD(id, "token", 1)); err == nil {
		t.Fatal("opened under another version's AAD")
	}
	if strings.Contains(string(row.Sealed), "two") {
		t.Fatal("stored in the clear")
	}
	for _, name := range []string{"addon.pg.password", "Token", ""} {
		_, err := h.svc.PutSecret(ctx, tenantA, "stage", name, PutSecretRequest{Value: "x"})
		wantStatus(t, err, http.StatusBadRequest, "secret name")
	}
	_, err = h.svc.PutSecret(ctx, tenantA, "stage", "big", PutSecretRequest{Value: strings.Repeat("x", 70000)})
	wantStatus(t, err, http.StatusBadRequest, "at most")

	// Pinned by the latest deployment: refused; then deleted.
	h.apply("stage", stageSpec, 0)
	err = h.svc.DeleteSecret(ctx, tenantA, "stage", "token")
	wantStatus(t, err, http.StatusConflict, "deployment 1 pins secret token: apply a deployment without it first")
	if err := h.svc.DeleteSecret(ctx, tenantA, "stage", "other"); err != nil {
		t.Fatal(err)
	}
	err = h.svc.DeleteSecret(ctx, tenantA, "stage", "other")
	wantStatus(t, err, http.StatusNotFound, "no such secret")
	err = h.svc.DeleteSecret(ctx, tenantA, "stage", "addon.pg.password")
	wantStatus(t, err, http.StatusNotFound, "no such secret")
	if list, _ := h.svc.Secrets(ctx, tenantA, "stage"); len(list.Secrets) != 1 {
		t.Fatalf("after delete %+v", list)
	}
	// A deleted secret's next version continues its numbers.
	if v := h.putSecret("stage", "other", "again"); v != 2 {
		t.Fatalf("version after delete %d", v)
	}
	// System secrets are never listed.
	sealed, _ := h.sealer.Seal([]byte("pw"), envdeps.SecretAAD(id, "addon.pg.password", 1))
	if _, err := h.st.InsertSecret(ctx, store.EnvSecret{EnvID: id, Name: "addon.pg.password", Version: 1, Sealed: sealed,
		System: true}); err != nil {
		t.Fatal(err)
	}
	if list, _ := h.svc.Secrets(ctx, tenantA, "stage"); len(list.Secrets) != 2 {
		t.Fatalf("system secret listed %+v", list)
	}
	// The S0 controller resolves no secret yet: an error, not a panic.
	if _, err := h.svc.ResolveSecret(ctx, id, "token", 1); err == nil {
		t.Log("the controller resolves secrets")
	}
}

func TestLogsRestartRunJob(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	h.create("stage", nil)
	_, err := h.svc.Logs(ctx, tenantA, "stage", "volume", "v", 0, 0)
	wantStatus(t, err, http.StatusNotFound, "no such object: volume/v")
	_, err = h.svc.Logs(ctx, tenantA, "stage", spec.KindService, "x", 0, -1)
	wantStatus(t, err, http.StatusBadRequest, "negative")
	_, err = h.svc.Logs(ctx, tenantB, "stage", spec.KindService, "x", 0, 0)
	wantStatus(t, err, http.StatusNotFound, "no such environment")
	// Whatever the controller answers maps to its status.
	for _, c := range []struct {
		err    error
		status int
		msg    string
	}{
		{ctl.ErrNoObject, http.StatusNotFound, "no such object: service/x"},
		{ctl.ErrPaused, http.StatusConflict, "paused"},
		{ctl.ErrNoContainerLogs, http.StatusNotImplemented, "this node does not serve container logs yet"},
		{fmt.Errorf("an op ensure of instance i2 is in flight: %w", model.ErrBusy), http.StatusConflict,
			"service x is busy with another operation; try again shortly"},
		{fmt.Errorf("sandbox s: %w", model.ErrNotFound), http.StatusConflict, "service x is not running: its sandbox is gone"},
		{fmt.Errorf("lock: %w", store.ErrNoRow), http.StatusConflict, "service x changed meanwhile; try again"},
		{fmt.Errorf("the instance is pending: no log to read (%w)", ctl.ErrNotRunning), http.StatusConflict,
			"the instance is pending: no log to read"},
		{fmt.Errorf("job x cannot run yet: addon pg has no address yet (%w)", ctl.ErrNotReady), http.StatusConflict,
			"job x cannot run yet"},
		{errors.New("the environment is being deleted"), http.StatusConflict, "the environment is being deleted"},
		{errors.New("the tenant is being deleted"), http.StatusConflict, "the tenant is being deleted"},
		{errors.New("process logs need the tools' runner, which this instance lacks"), http.StatusServiceUnavailable,
			"process logs need the tools' runner"},
		{errors.New("boom"), http.StatusInternalServerError, "boom"},
	} {
		wantStatus(t, ctlError(c.err, spec.KindService, "x"), c.status, c.msg)
	}
	if ctlError(nil, "", "") != nil {
		t.Fatal("nil")
	}
	if err := h.svc.DeleteEnv(ctx, tenantA, "stage"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, h.svc.RestartService(ctx, tenantA, "stage", "x"), http.StatusConflict, "being deleted")
	wantStatus(t, h.svc.RunJob(ctx, tenantA, "stage", "x"), http.StatusConflict, "being deleted")
}

func TestRouteTokens(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	st := h.create("stage", nil)
	a, err := h.svc.CreateRouteToken(ctx, tenantA, "stage")
	if err != nil || len(a.Token) != 43 {
		t.Fatalf("token %+v %v", a, err)
	}
	b, _ := h.svc.CreateRouteToken(ctx, tenantA, "stage")
	if a.Token == b.Token {
		t.Fatal("the same token twice")
	}
	rt, err := h.st.ResolveRouteToken(ctx, store.HashToken(a.Token))
	if err != nil || rt.EnvID.String() != st.ID {
		t.Fatalf("stored %+v %v", rt, err)
	}
	if err := h.svc.RevokeRouteTokens(ctx, tenantA, "stage"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.ResolveRouteToken(ctx, store.HashToken(b.Token)); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("not revoked: %v", err)
	}
	_, err = h.svc.CreateRouteToken(ctx, tenantB, "stage")
	wantStatus(t, err, http.StatusNotFound, "no such environment")
}

// An environment's ID is free again once env-delete removed it; the next
// environment of that ID, another tenant's here, never sees the old one's
// deployments through the parsed-spec cache.
func TestReusedEnvID(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	id := uuid.New()
	if _, _, err := h.svc.CreateEnv(ctx, tenantA, CreateEnvRequest{Name: "stage", ID: id.String()}); err != nil {
		t.Fatal(err)
	}
	h.putSecret("stage", "token", "x")
	h.apply("stage", stageSpec, 0)
	// Read deployment 1 every way the service caches it.
	if st, err := h.svc.GetEnv(ctx, tenantA, "stage"); err != nil || len(st.Objects) != 5 {
		t.Fatalf("status %+v %v", st, err)
	}
	if _, err := h.svc.Deployment(ctx, tenantA, "stage", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(stageSpec)}); err != nil {
		t.Fatal(err)
	}
	// env-delete removes the row, its deployments with it.
	h.exec("DELETE FROM environments WHERE id = $1", id)

	if _, _, err := h.svc.CreateEnv(ctx, tenantB, CreateEnvRequest{Name: "solo", ID: id.String()}); err != nil {
		t.Fatal(err)
	}
	own := `{"services": {"solo": {"image": "ghcr.io/b/solo:1", "ports": [{"name": "http", "port": 80}]}},
	  "routes": {"root": {"path": "/", "service": "solo", "port": "http"}}}`
	a, err := h.svc.Apply(ctx, tenantB, "solo", ApplyRequest{Spec: json.RawMessage(own)})
	if err != nil || a.Number != 1 {
		t.Fatalf("apply %+v %v", a, describe(err))
	}
	st, err := h.svc.GetEnv(ctx, tenantB, "solo")
	if err != nil || len(st.Objects) != 1 || st.Objects[0].Name != "solo" || len(st.Routes) != 1 || st.Routes[0].Port != 80 {
		t.Fatalf("the new environment's status %+v %v", st, err)
	}
	// The same spec again is unchanged; the old environment's is a change
	// that removes solo, with nothing of the old one's as "from".
	if a, err := h.svc.Apply(ctx, tenantB, "solo", ApplyRequest{Spec: json.RawMessage(own), Base: 1}); err != nil || !a.Unchanged {
		t.Fatalf("unchanged %+v %v", a, describe(err))
	}
	p, err := h.svc.Plan(ctx, tenantB, "solo", PlanRequest{Spec: json.RawMessage(strings.Replace(stageSpec,
		`"TOKEN": {"secret": "token"},`, "", 1))})
	if err != nil {
		t.Fatal(err)
	}
	removed := false
	for _, d := range p.Diff {
		if d.Kind == spec.KindService && d.Name == "solo" && d.Change == spec.ChangeRemove {
			removed = true
		}
		if d.Kind == spec.KindService && d.Name == "api" && d.Change != spec.ChangeAdd {
			t.Fatalf("api is %s: the old environment's spec leaked into the diff", d.Change)
		}
	}
	if !removed {
		t.Fatalf("diff %+v", p.Diff)
	}
	if d, err := h.svc.Deployment(ctx, tenantB, "solo", 1); err != nil || len(d.Diff) != 2 {
		t.Fatalf("deployment 1 %+v %v", d, err)
	}
}

// Plan, apply and revert validate an addon against the data its volume
// holds, which the controller keeps by the addon's name: a removal or a
// type change the controller has not acted on yet deletes nothing.
func TestAddonAgainstVolume(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	h.create("stage", nil)
	e := h.env("stage")
	addon := func(typ, version string) string {
		return `{"addons": {"db": {"type": "` + typ + `", "version": "` + version + `"}}}`
	}
	const none = `{"services": {"web": {"image": "ghcr.io/acme/web:1"}}}`
	refused := func(raw string, base int, path, problem string) {
		t.Helper()
		_, err := h.svc.Apply(ctx, tenantA, "stage", ApplyRequest{Spec: json.RawMessage(raw), Base: base})
		var ise *InvalidSpecError
		if !errors.As(err, &ise) || !hasFinding(ise.Findings, spec.SeverityError, path, problem) {
			t.Fatalf("apply %s on %d: %v", raw, base, describe(err))
		}
	}
	volume := func(typ, version string) store.EnvVolume {
		t.Helper()
		v, err := h.st.InsertEnvVolume(ctx, store.EnvVolume{EnvID: e.ID, Object: "addon/db", VolumeID: uuid.New(),
			SizeMiB: 1024, AddonType: typ, DataVersion: version})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	deleting := func(v store.EnvVolume) {
		t.Helper()
		after := v
		after.Deleting = true
		if _, err := h.st.UpdateEnvVolume(ctx, v, after); err != nil {
			t.Fatal(err)
		}
	}

	h.apply("stage", addon("postgres", "16"), 0)
	pg16 := volume("postgres", "16")

	// Removed, and added again as 17 before the controller deleted the
	// volume: postgres 17 would start on 16's data directory.
	h.apply("stage", none, 1)
	refused(addon("postgres", "17"), 2, "addons.db.version", "needs a data migration")
	if a := h.apply("stage", addon("postgres", "16"), 2); a.Number != 3 {
		t.Fatalf("re-added as it was %+v", a)
	}

	// A type round trip the controller has not acted on: valkey is told
	// the data goes, and back to postgres the data is still 16's.
	a := h.apply("stage", addon("valkey", "8"), 3)
	if !hasFinding(a.Findings, spec.SeverityWarning, "addons.db.type", "volume holds postgres data, not valkey: its data is deleted") {
		t.Fatalf("to valkey %+v", a.Findings)
	}
	refused(addon("postgres", "17"), 4, "addons.db.version", "needs a data migration")
	p, err := h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(addon("postgres", "16"))})
	if err != nil || !p.Valid || hasFinding(p.Findings, spec.SeverityWarning, "addons.db.type", "") {
		t.Fatalf("back to postgres 16 %+v %v", p, err)
	}
	// A revert to deployment 1 is checked the same way.
	if _, err := h.svc.Revert(ctx, tenantA, "stage", 1, RevertRequest{Base: 4}); err != nil {
		t.Fatalf("revert to postgres 16: %v", describe(err))
	}

	// Once the controller acts on valkey, postgres 16's volume is being
	// deleted and constrains nothing; once it is gone, valkey's new one
	// holds valkey data.
	h.apply("stage", addon("valkey", "8"), 5)
	deleting(pg16)
	p, err = h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(addon("postgres", "17"))})
	if err != nil || !p.Valid || !hasFinding(p.Findings, spec.SeverityWarning, "addons.db.type", "changes its type from valkey to postgres") {
		t.Fatalf("postgres 17 beside a deleting volume %+v %v", p, err)
	}
	h.exec("DELETE FROM env_volumes WHERE env_id = $1", e.ID)
	volume("valkey", "8")
	a = h.apply("stage", addon("postgres", "17"), 6)
	if a.Number != 7 || !hasFinding(a.Findings, spec.SeverityWarning, "addons.db.type", "volume holds valkey data, not postgres") {
		t.Fatalf("postgres 17 over valkey's volume %+v", a)
	}

	// The version that ran ready on the volume decides a downgrade, not
	// the latest deployment: an upgrade to 8 that never ran may go back.
	h.exec("DELETE FROM env_volumes WHERE env_id = $1", e.ID)
	h.apply("stage", addon("valkey", "7.2"), 7)
	vk72 := volume("valkey", "7.2")
	h.apply("stage", addon("valkey", "8"), 8)
	if _, err := h.svc.Revert(ctx, tenantA, "stage", 8, RevertRequest{Base: 9}); err != nil {
		t.Fatalf("revert of an upgrade that never ran: %v", describe(err))
	}
	// Once 8 ran ready on it, the data is 8's.
	h.apply("stage", addon("valkey", "8"), 10)
	after := vk72
	after.DataVersion = "8"
	if _, err := h.st.UpdateEnvVolume(ctx, vk72, after); err != nil {
		t.Fatal(err)
	}
	_, err = h.svc.Revert(ctx, tenantA, "stage", 8, RevertRequest{Base: 11})
	var ise *InvalidSpecError
	if !errors.As(err, &ise) || !hasFinding(ise.Findings, spec.SeverityError, "addons.db.version", "cannot go back from 8 to 7.2") {
		t.Fatalf("revert over 8's data: %v", describe(err))
	}
}

// A secret deleted between an apply's plan and its transaction is never
// pinned by the new deployment: the apply plans again and refuses it.
func TestApplyRacesDeleteSecret(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	h.create("stage", nil)
	h.putSecret("stage", "x", "v1")
	h.apply("stage", `{"services": {"web": {"image": "ghcr.io/acme/web:1"}}}`, 0)
	var once sync.Once
	h.svc.afterPlan = func() {
		once.Do(func() {
			if err := h.svc.DeleteSecret(ctx, tenantA, "stage", "x"); err != nil {
				t.Errorf("delete secret: %v", err)
			}
		})
	}
	_, err := h.svc.Apply(ctx, tenantA, "stage", ApplyRequest{Base: 1,
		Spec: json.RawMessage(`{"services": {"web": {"image": "ghcr.io/acme/web:1", "env": {"X": {"secret": "x"}}}}}`)})
	var ise *InvalidSpecError
	if !errors.As(err, &ise) || !hasFinding(ise.Findings, spec.SeverityError, "services.web.env.X", "secret x was deleted") {
		t.Fatalf("apply: %v", describe(err))
	}
	if e := h.env("stage"); e.Deployment != 1 {
		t.Fatalf("deployment %d was made", e.Deployment)
	}
}

// The controller's refusals of expected states answer 409 with a code,
// never 500: a restart while an op is in flight (Retry-After), a job that
// cannot run yet, the log of an instance that does not run.
func TestControllerRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	h.create("stage", nil)
	e := h.env("stage")
	h.putSecret("stage", "token", "x")
	sb := h.sandbox(tenantA, "stage")
	h.apply("stage", previewSpec(sb), 0)
	code := func(err error) string {
		var ae *apierr.Error
		if !errors.As(err, &ae) {
			return ""
		}
		return ae.Code
	}

	// The migrate job's addon has no address yet.
	err := h.svc.RunJob(ctx, tenantA, "stage", "migrate")
	wantStatus(t, err, http.StatusConflict, "job migrate cannot run yet: ")
	if code(err) != "not_ready" {
		t.Fatalf("code %q", code(err))
	}

	// A replica the deployment has but that has no instance yet (it waits
	// for its addons) has no log; one the object does not have is no
	// object, and the answer says which.
	for _, c := range []struct {
		kind, name string
		replica    int
		status     int
		msg, code  string
	}{
		{spec.KindService, "api", 1, http.StatusConflict, "service api has no instance of replica 1 yet: no log to read",
			"not_running"},
		{spec.KindAddon, "pg", 0, http.StatusConflict, "addon pg has no instance of replica 0 yet: no log to read", "not_running"},
		{spec.KindService, "api", 5, http.StatusNotFound, "service api has no replica 5 (it has 2)", ""},
		{spec.KindAddon, "pg", 1, http.StatusNotFound, "addon pg has no replica 1 (it has 1)", ""},
		{spec.KindService, "nope", 0, http.StatusNotFound, "no such object: service/nope", ""},
	} {
		_, err := h.svc.Logs(ctx, tenantA, "stage", c.kind, c.name, 0, c.replica)
		wantStatus(t, err, c.status, c.msg)
		if code(err) != c.code {
			t.Fatalf("%s/%s replica %d: code %q", c.kind, c.name, c.replica, code(err))
		}
	}
	// replicas: 0 runs none.
	h.apply("stage", strings.Replace(previewSpec(sb), `"replicas": 2`, `"replicas": 0`, 1), 1)
	_, err = h.svc.Logs(ctx, tenantA, "stage", spec.KindService, "api", 0, 0)
	wantStatus(t, err, http.StatusConflict, "service api runs no replica (replicas: 0): no log to read")
	h.apply("stage", previewSpec(sb), 2)

	// A pending instance has no log.
	h.instance(e.ID, spec.KindService, "api", 0, 1, model.PhaseStarting, model.StatePending, "", "")
	_, err = h.svc.Logs(ctx, tenantA, "stage", spec.KindService, "api", 0, 0)
	wantStatus(t, err, http.StatusConflict, "the instance is pending: no log to read")
	if code(err) != "not_running" {
		t.Fatalf("code %q", code(err))
	}
	// Nor has an attached service whose sandbox is gone.
	if _, err := h.st.InsertAttached(ctx, store.EnvAttached{EnvID: e.ID, Service: "dev", SandboxID: sb, ProcessID: "p1"}); err != nil {
		t.Fatal(err)
	}
	h.exec("DELETE FROM sandboxes WHERE id = $1", sb)
	_, err = h.svc.Logs(ctx, tenantA, "stage", spec.KindService, "dev", 0, 0)
	wantStatus(t, err, http.StatusConflict, "sandbox "+sb.String()+" is gone or not this tenant's: no log to read")

	// The replicas have an op in flight: the controller refuses before it
	// touches them.
	h.exec(`UPDATE sandboxes SET op = 'repair', op_id = gen_random_uuid(), op_owner = 'i2'
		WHERE environment_id = $1`, e.ID)
	err = h.svc.RestartService(ctx, tenantA, "stage", "api")
	wantStatus(t, err, http.StatusConflict, "service api is busy with another operation; try again shortly")
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != "busy" || ae.Header.Get("Retry-After") != "5" {
		t.Fatalf("busy %+v", ae)
	}
}

// Publishing (ADR 0021): with an apps zone, PUT public gives the
// environment its default host and rolls its image services once; the
// status, the listing and a plan say which door serves each route.
// Unpublishing keeps the host for the next publish, which rolls nothing.
// A clone is never public. Without a zone publishing is refused, and a
// public environment has no host; so is a change of a deleting
// environment or tenant.
func TestSetPublic(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{appsZone: "apps.test"})
	ctx := h.ctx()
	h.create("stage", nil)
	h.putSecret("stage", "token", "x")
	sb := h.sandbox(tenantA, "stage")
	withDev := strings.Replace(previewSpec(sb), `"root": {"path": "/", "service": "web", "port": "http"}`,
		`"root": {"path": "/", "service": "web", "port": "http"}, "dev": {"path": "/dev/", "service": "dev", "port": "http"}`, 1)
	h.apply("stage", withDev, 0)
	byDoor := []RouteStatus{
		{Name: "api", Path: "/api", Service: "api", Port: 8080, Door: DoorRoute},
		{Name: "dev", Path: "/dev/", Service: "dev", Port: 5173, Door: DoorRoute},
		{Name: "root", Path: "/", Service: "web", Port: 3000, Door: DoorRoute},
	}
	st, err := h.svc.GetEnv(ctx, tenantA, "stage")
	if err != nil || st.Public || st.Hosts == nil || len(st.Hosts) != 0 || !slices.Equal(st.Routes, byDoor) {
		t.Fatalf("before: %+v %v", st, err)
	}

	// Published: the host, a roll, the controller woken.
	hosts := []string{"stage-acme.apps.test"}
	ch, unsubscribe := h.d.Subscribe(db.ChanLeader)
	defer unsubscribe()
	a, err := h.svc.SetPublic(ctx, tenantA, "stage", true, "")
	if err != nil || !a.Public || !slices.Equal(a.Hosts, hosts) || !a.Rolling {
		t.Fatalf("publish: %+v %v", a, err)
	}
	waitHint(t, ch, ctl.HintReconcile)
	waitHint(t, ch, ctl.HintGroups)
	if e := h.env("stage"); !e.Public || !e.IngressGroup || e.HostLabel != "stage-acme" {
		t.Fatalf("row %+v", e)
	}
	// Again, by its ID: the same answer, and nothing rolls.
	a, err = h.svc.SetPublic(ctx, tenantA, st.ID, true, "")
	if err != nil || !a.Public || !slices.Equal(a.Hosts, hosts) || a.Rolling {
		t.Fatalf("again: %+v %v", a, err)
	}

	// The routes to image services are the ingress's too, at the host;
	// the attached service's are the route door's alone.
	ingress := slices.Clone(byDoor)
	ingress[0].Door, ingress[0].URL = DoorIngress, "https://stage-acme.apps.test/api"
	ingress[2].Door, ingress[2].URL = DoorIngress, "https://stage-acme.apps.test/"
	st, err = h.svc.GetEnv(ctx, tenantA, "stage")
	if err != nil || !st.Public || !slices.Equal(st.Hosts, hosts) || !slices.Equal(st.Routes, ingress) {
		t.Fatalf("status: %+v %v", st, err)
	}
	b, _ := json.Marshal(st)
	for _, want := range []string{`"public":true,"hosts":["stage-acme.apps.test"]`,
		`{"name":"api","path":"/api","service":"api","port":8080,"door":"ingress","url":"https://stage-acme.apps.test/api"}`,
		`{"name":"dev","path":"/dev/","service":"dev","port":5173,"door":"route"}`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("status JSON lacks %s:\n%s", want, b)
		}
	}
	list, err := h.svc.ListEnvs(ctx, tenantA, nil)
	if err != nil || len(list.Environments) != 1 || !list.Environments[0].Public ||
		!slices.Equal(list.Environments[0].Hosts, hosts) {
		t.Fatalf("list %+v %v", list, err)
	}
	p, err := h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(withDev)})
	if err != nil || !p.Valid ||
		!hasFinding(p.Findings, spec.SeverityInfo, "routes.api",
			"served by the ingress at https://stage-acme.apps.test/api and by the route door") ||
		!hasFinding(p.Findings, spec.SeverityInfo, "routes.root", "served by the ingress at https://stage-acme.apps.test/ and") ||
		!hasFinding(p.Findings, spec.SeverityInfo, "routes.dev", "served by the route door alone: an attached service") ||
		!hasFinding(p.Findings, spec.SeverityInfo, "", "nothing changes") || len(p.Findings) != 4 {
		t.Fatalf("plan %+v %v", p, err)
	}

	// A clone is not public, whatever its source is.
	if _, err := h.svc.Clone(ctx, tenantA, "stage", CloneRequest{Name: "demo",
		Secrets: map[string]CloneSecret{"token": {Copy: true}}}); err != nil {
		t.Fatal(describe(err))
	}
	if e := h.env("demo"); e.Public || e.IngressGroup || e.HostLabel != "" {
		t.Fatalf("clone row %+v", e)
	}
	if cst, err := h.svc.GetEnv(ctx, tenantA, "demo"); err != nil || cst.Public || len(cst.Hosts) != 0 ||
		cst.Routes[0].Door != DoorRoute || cst.Routes[0].URL != "" {
		t.Fatalf("clone %+v %v", cst, err)
	}

	// Unpublished: no host, the route door alone; the label and the group
	// stay, so publishing again names the same host and rolls nothing.
	a, err = h.svc.SetPublic(ctx, tenantA, "stage", false, "")
	if err != nil || a.Public || a.Hosts == nil || len(a.Hosts) != 0 || a.Rolling {
		t.Fatalf("unpublish: %+v %v", a, err)
	}
	st, err = h.svc.GetEnv(ctx, tenantA, "stage")
	if err != nil || st.Public || len(st.Hosts) != 0 || !slices.Equal(st.Routes, byDoor) {
		t.Fatalf("unpublished status: %+v %v", st, err)
	}
	p, err = h.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(withDev)})
	if err != nil || !hasFinding(p.Findings, spec.SeverityInfo, "routes.api",
		"served by the route door; the ingress serves it once the environment is public") {
		t.Fatalf("unpublished plan %+v %v", p, err)
	}
	if e := h.env("stage"); e.Public || !e.IngressGroup || e.HostLabel != "stage-acme" {
		t.Fatalf("unpublished row %+v", e)
	}
	a, err = h.svc.SetPublic(ctx, tenantA, "stage", true, "")
	if err != nil || !slices.Equal(a.Hosts, hosts) || a.Rolling {
		t.Fatalf("republish: %+v %v", a, err)
	}

	// A label the tenant chose: the host moves and nothing rolls; a label
	// not of the chosen form, one without public and one another tenant
	// holds are refused, and the environment keeps its host.
	a, err = h.svc.SetPublic(ctx, tenantA, "stage", true, "stage--shop")
	if err != nil || !slices.Equal(a.Hosts, []string{"stage--shop.apps.test"}) || a.Rolling {
		t.Fatalf("chosen label: %+v %v", a, err)
	}
	if e := h.env("stage"); !e.Public || e.HostLabel != "stage--shop" {
		t.Fatalf("chosen row %+v", e)
	}
	for _, bad := range []string{"stage-shop", "ab--shop", "Stage--shop", "x--0123456789abcdef", "stage--shop-"} {
		_, err = h.svc.SetPublic(ctx, tenantA, "stage", true, bad)
		wantStatus(t, err, http.StatusBadRequest, "a chosen host label is a DNS label")
	}
	_, err = h.svc.SetPublic(ctx, tenantA, "stage", false, "stage--shop")
	wantStatus(t, err, http.StatusBadRequest, textHostLabelNotPublic)
	h.exec("INSERT INTO host_labels (label, owner) VALUES ('taken--x', $1)", tenantB)
	_, err = h.svc.SetPublic(ctx, tenantA, "stage", true, "taken--x")
	wantStatus(t, err, http.StatusConflict, "host label taken--x is another tenant's")
	if e := h.env("stage"); !e.Public || e.HostLabel != "stage--shop" {
		t.Fatalf("kept %+v", e)
	}

	// A served domain is its host in place of the default host, in the
	// answer, the status, its routes and the listing; one being issued,
	// or one whose certificate expired, is not.
	stage := h.env("stage")
	for i, d := range []struct {
		name, state string
		notAfter    string
	}{{"shop.example", "served", "now() + interval '30 days'"}, {"issuing.example", "issuing", ""},
		{"old.example", "served", "now() - interval '1 hour'"}} {
		id := fmt.Sprintf("00000000-0000-4000-8000-00000000000%d", i+1)
		h.exec(`INSERT INTO domains (id, owner, name, token, env_id, bound_at, verification, verified_at, routed, was_routed, state)
			VALUES ($1, $2, $3, 'abcdefghijklmnopqrstuvwxyz234567', $4, now(), 'verified', now(), true, true, $5)`,
			id, tenantA, d.name, stage.ID, d.state)
		if d.notAfter != "" {
			h.exec(`INSERT INTO domain_certs (domain_id, chain, key_sealed, not_before, not_after, renew_after, serial)
				VALUES ($1, ARRAY['\x01'::bytea], '\x01', now() - interval '1 day', `+d.notAfter+`, now(), '1')`, id)
		}
	}
	shop := []string{"shop.example"}
	a, err = h.svc.SetPublic(ctx, tenantA, "stage", true, "")
	if err != nil || !slices.Equal(a.Hosts, shop) {
		t.Fatalf("with a domain: %+v %v", a, err)
	}
	st, err = h.svc.GetEnv(ctx, tenantA, "stage")
	if err != nil || !slices.Equal(st.Hosts, shop) || st.HostLabel != "stage--shop" || st.Routes[2].URL != "https://shop.example/" {
		t.Fatalf("status with a domain: %+v %v", st, err)
	}
	list, err = h.svc.ListEnvs(ctx, tenantA, map[string]string{})
	if err != nil || !slices.Equal(list.Environments[1].Hosts, shop) || list.Environments[1].HostLabel != "stage--shop" {
		t.Fatalf("list with a domain: %+v %v", list, err)
	}
	h.exec(`UPDATE domains SET state = 'inactive' WHERE env_id = $1`, stage.ID)
	if st, err = h.svc.GetEnv(ctx, tenantA, "stage"); err != nil ||
		!slices.Equal(st.Hosts, []string{"stage--shop.apps.test"}) {
		t.Fatalf("its domain inactive: %+v %v", st, err)
	}

	// Refused: another tenant's environment or none, a deleting
	// environment, a deleting tenant.
	_, err = h.svc.SetPublic(ctx, tenantB, "stage", true, "")
	wantStatus(t, err, http.StatusNotFound, "no such environment")
	_, err = h.svc.SetPublic(ctx, tenantA, "nope", true, "")
	wantStatus(t, err, http.StatusNotFound, "no such environment")
	if err := h.svc.DeleteEnv(ctx, tenantA, "demo"); err != nil {
		t.Fatal(err)
	}
	for _, public := range []bool{true, false} {
		_, err = h.svc.SetPublic(ctx, tenantA, "demo", public, "")
		wantStatus(t, err, http.StatusConflict, "environment demo is being deleted")
	}
	// An environment with a sandbox is not deleted (ADR 0035).
	err = h.svc.DeleteEnv(ctx, tenantA, "stage")
	wantStatus(t, err, http.StatusConflict, "environment stage has 1 sandboxes and 0 running jobs: delete them first")
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeEnvironmentInUse || ae.InUse == nil ||
		!slices.Equal(ae.InUse.Sandboxes, []string{sb.String()}) || len(ae.InUse.Jobs) != 0 {
		t.Fatalf("in use %+v", ae)
	}
	h.exec("DELETE FROM sandboxes WHERE id = $1", sb)
	// A public environment being deleted keeps its flag, but the ingress
	// no longer serves it: no host, the route door alone.
	if err := h.svc.DeleteEnv(ctx, tenantA, "stage"); err != nil {
		t.Fatal(err)
	}
	st, err = h.svc.GetEnv(ctx, tenantA, "stage")
	if err != nil || !st.Public || len(st.Hosts) != 0 || !slices.Equal(st.Routes, byDoor) {
		t.Fatalf("deleting: %+v %v", st, err)
	}
	if list, err := h.svc.ListEnvs(ctx, tenantA, nil); err != nil || !list.Environments[1].Public ||
		list.Environments[1].Name != "stage" || len(list.Environments[1].Hosts) != 0 {
		t.Fatalf("deleting list %+v %v", list, err)
	}
	if _, created, err := h.svc.CreateEnv(ctx, tenantB, CreateEnvRequest{Name: "x"}); err != nil || !created {
		t.Fatal(err)
	}
	h.exec("UPDATE tenants SET deleting_at = now() WHERE id = $1", tenantB)
	_, err = h.svc.SetPublic(ctx, tenantB, "x", true, "")
	wantStatus(t, err, http.StatusConflict, "tenant other is being deleted")

	// Without a zone (the kill switch): publishing is refused; a public
	// environment has no host and only the route door serves it;
	// unpublishing works.
	h2 := newHarness(t, harnessOptions{})
	h2.create("stage", nil)
	h2.putSecret("stage", "token", "x")
	h2.apply("stage", stageSpec, 0)
	_, err = h2.svc.SetPublic(ctx, tenantA, "stage", true, "")
	wantStatus(t, err, http.StatusConflict, "the platform serves no apps zone (--apps-zone); an environment cannot be public")
	if _, err := h2.st.SetEnvironmentPublic(ctx, h2.env("stage").ID, true); err != nil {
		t.Fatal(err)
	}
	st, err = h2.svc.GetEnv(ctx, tenantA, "stage")
	if err != nil || !st.Public || len(st.Hosts) != 0 || st.Routes[0].Door != DoorRoute || st.Routes[1].Door != DoorRoute {
		t.Fatalf("public without a zone: %+v %v", st, err)
	}
	if p, err := h2.svc.Plan(ctx, tenantA, "stage", PlanRequest{Spec: json.RawMessage(stageSpec)}); err != nil ||
		len(p.Findings) != 1 || p.Findings[0].Problem != "nothing changes: the spec equals deployment 1's" {
		t.Fatalf("plan without a zone %+v %v", p, err)
	}
	a, err = h2.svc.SetPublic(ctx, tenantA, "stage", false, "")
	if err != nil || a.Public || len(a.Hosts) != 0 || a.Rolling {
		t.Fatalf("unpublish without a zone: %+v %v", a, err)
	}
}

// A plain label (ADR 0021, 2026-11-01): a tenant of --apps-zone-owners may
// choose a host label without "--", as FeCode does for a project's
// production; any other tenant gets the 400 it got before. A plain label
// is first come as every label is, and one that is the natural label of
// an existing tenant's environment is refused; a tenant made later, whose
// natural label the owner holds, gets its fallback, and the owner keeps
// the label.
func TestSetPublicPlainLabel(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{appsZone: "apps.test", appsZoneOwners: []string{tenantA}})
	ctx := h.ctx()
	for _, n := range []string{"prod", "demo"} {
		h.create(n, nil)
	}
	if _, created, err := h.svc.CreateEnv(ctx, tenantB, CreateEnvRequest{Name: "prod"}); err != nil || !created {
		t.Fatalf("create %v %v", created, err)
	}
	code := func(err error) string {
		var ae *apierr.Error
		if errors.As(err, &ae) {
			return ae.Code
		}
		return ""
	}

	// The zone's owner: a plain label at its first publish, again without
	// a change, and a label with "--" as before.
	a, err := h.svc.SetPublic(ctx, tenantA, "prod", true, "kino")
	if err != nil || !slices.Equal(a.Hosts, []string{"kino.apps.test"}) || !a.Rolling {
		t.Fatalf("plain label: %+v %v", a, err)
	}
	if e := h.env("prod"); !e.Public || e.HostLabel != "kino" {
		t.Fatalf("row %+v", e)
	}
	a, err = h.svc.SetPublic(ctx, tenantA, "prod", true, "kino")
	if err != nil || !slices.Equal(a.Hosts, []string{"kino.apps.test"}) || a.Rolling {
		t.Fatalf("again: %+v %v", a, err)
	}
	a, err = h.svc.SetPublic(ctx, tenantA, "demo", true, "demo--kino")
	if err != nil || !slices.Equal(a.Hosts, []string{"demo--kino.apps.test"}) {
		t.Fatalf("chosen label of the owner: %+v %v", a, err)
	}
	a, err = h.svc.SetPublic(ctx, tenantA, "demo", true, "ironcore-dashboard")
	if err != nil || !slices.Equal(a.Hosts, []string{"ironcore-dashboard.apps.test"}) {
		t.Fatalf("a hyphenated plain label no tenant's: %+v %v", a, err)
	}

	// Not of the form: a fallback's end, R-LDH, upper case, too long.
	for _, bad := range []string{"kino-0123456789abcdef", "xn--kino", "Kino", "kino-", strings.Repeat("k", 64)} {
		_, err = h.svc.SetPublic(ctx, tenantA, "prod", true, bad)
		wantStatus(t, err, http.StatusBadRequest, fmt.Sprintf(textBadOwnedHostLabel, bad))
	}

	// Taken: another environment of the owner holds it, another tenant
	// owns it, it is the natural label of an existing tenant's
	// environment (the owner's own included). The environment keeps its
	// label.
	_, err = h.svc.SetPublic(ctx, tenantA, "demo", true, "kino")
	wantStatus(t, err, http.StatusConflict, "host label kino is another tenant's or another environment's")
	if code(err) != CodeHostLabelTaken {
		t.Fatalf("code %q", code(err))
	}
	h.exec("INSERT INTO host_labels (label, owner) VALUES ('shop', $1)", tenantB)
	_, err = h.svc.SetPublic(ctx, tenantA, "demo", true, "shop")
	wantStatus(t, err, http.StatusConflict, "host label shop is another tenant's or another environment's")
	for label, msg := range map[string]string{
		"web-other": "host label web-other is the natural label of environment web of tenant other",
		"web-acme":  "host label web-acme is the natural label of environment web of tenant acme",
	} {
		_, err = h.svc.SetPublic(ctx, tenantA, "demo", true, label)
		wantStatus(t, err, http.StatusConflict, msg)
		if code(err) != CodeHostLabelTaken {
			t.Fatalf("%s: code %q", label, code(err))
		}
	}
	if e := h.env("demo"); e.HostLabel != "ironcore-dashboard" {
		t.Fatalf("kept %+v", e)
	}

	// Another tenant: a plain label is the 400 of before, a label with
	// "--" is chosen.
	for _, bad := range []string{"kino2", "web-acme"} {
		_, err = h.svc.SetPublic(ctx, tenantB, "prod", true, bad)
		wantStatus(t, err, http.StatusBadRequest, fmt.Sprintf(textBadHostLabel, bad))
	}
	if a, err := h.svc.SetPublic(ctx, tenantB, "prod", true, "prod--x"); err != nil ||
		!slices.Equal(a.Hosts, []string{"prod--x.apps.test"}) {
		t.Fatalf("chosen label of another tenant: %+v %v", a, err)
	}

	// A tenant made later whose natural label the owner holds: its
	// environment gets its fallback, and the owner keeps the label, also
	// when it chooses it again.
	if _, err := h.svc.SetPublic(ctx, tenantA, "demo", true, "shop-later"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.st.CreateTenant(ctx, "later"); err != nil {
		t.Fatal(err)
	}
	shop, created, err := h.svc.CreateEnv(ctx, "later", CreateEnvRequest{Name: "shop"})
	if err != nil || !created {
		t.Fatalf("create %v %v", created, err)
	}
	_, _, fallback := model.HostLabels("shop", "later", uuid.MustParse(shop.ID))
	if a, err := h.svc.SetPublic(ctx, "later", "shop", true, ""); err != nil ||
		!slices.Equal(a.Hosts, []string{fallback + ".apps.test"}) {
		t.Fatalf("later tenant: %+v %v (fallback %s)", a, err, fallback)
	}
	if a, err := h.svc.SetPublic(ctx, tenantA, "demo", true, "shop-later"); err != nil ||
		!slices.Equal(a.Hosts, []string{"shop-later.apps.test"}) {
		t.Fatalf("the owner again: %+v %v", a, err)
	}
	// Its other environment may take it once the first gives it up.
	if _, err := h.svc.SetPublic(ctx, tenantA, "demo", true, "demo--kino"); err != nil {
		t.Fatal(err)
	}
	if a, err := h.svc.SetPublic(ctx, tenantA, "prod", true, "shop-later"); err != nil ||
		!slices.Equal(a.Hosts, []string{"shop-later.apps.test"}) {
		t.Fatalf("the owner's other environment: %+v %v", a, err)
	}
}

// The tenant zone (ADR 0031): a tenant there publishes under it, with its
// natural and its chosen labels, and the status and the listing say so; a
// zone owner moved there chooses no plain label; a tenant whose zone the
// platform does not serve publishes nothing, and a move back gives it its
// hosts again.
func TestSetPublicTenantZone(t *testing.T) {
	t.Parallel()
	const tz = "apps.run.test"
	h := newHarness(t, harnessOptions{appsZone: "apps.test", appsZoneOwners: []string{tenantA}, tenantZone: tz})
	ctx := h.ctx()
	h.create("web", nil)
	h.exec("UPDATE tenants SET zone = $1 WHERE id = $2", tz, tenantA)

	a, err := h.svc.SetPublic(ctx, tenantA, "web", true, "")
	if err != nil || !slices.Equal(a.Hosts, []string{"web-acme." + tz}) {
		t.Fatalf("natural label: %+v %v", a, err)
	}
	a, err = h.svc.SetPublic(ctx, tenantA, "web", true, "web--shop")
	if err != nil || !slices.Equal(a.Hosts, []string{"web--shop." + tz}) {
		t.Fatalf("chosen label: %+v %v", a, err)
	}
	st, err := h.svc.GetEnv(ctx, tenantA, "web")
	if err != nil || !slices.Equal(st.Hosts, []string{"web--shop." + tz}) {
		t.Fatalf("status %+v %v", st.Hosts, err)
	}
	l, err := h.svc.ListEnvs(ctx, tenantA, nil)
	if err != nil || len(l.Environments) != 1 || !slices.Equal(l.Environments[0].Hosts, []string{"web--shop." + tz}) {
		t.Fatalf("listing %+v %v", l, err)
	}
	// An owner of the apps zone, but in the tenant zone: no plain label.
	_, err = h.svc.SetPublic(ctx, tenantA, "web", true, "kino")
	wantStatus(t, err, http.StatusBadRequest, "a plain host label is only for the apps zone")
	if e := h.env("web"); e.HostLabel != "web--shop" {
		t.Fatalf("kept %+v", e)
	}
	// Another tenant takes the same chosen label in the apps zone.
	if _, created, err := h.svc.CreateEnv(ctx, tenantB, CreateEnvRequest{Name: "web"}); err != nil || !created {
		t.Fatalf("create %v %v", created, err)
	}
	if a, err := h.svc.SetPublic(ctx, tenantB, "web", true, "web--shop"); err != nil ||
		!slices.Equal(a.Hosts, []string{"web--shop.apps.test"}) {
		t.Fatalf("the same label in the apps zone: %+v %v", a, err)
	}

	// A zone the platform does not serve: no hosts, no publish.
	h.exec("UPDATE tenants SET zone = 'old.test' WHERE id = $1", tenantA)
	if st, err := h.svc.GetEnv(ctx, tenantA, "web"); err != nil || len(st.Hosts) != 0 {
		t.Fatalf("status in an unserved zone %+v %v", st.Hosts, err)
	}
	if _, err := h.svc.SetPublic(ctx, tenantA, "web", false, ""); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	_, err = h.svc.SetPublic(ctx, tenantA, "web", true, "")
	wantStatus(t, err, http.StatusConflict, "the platform serves no zone old.test")

	// An operator moves it to the apps zone (store.SetTenantZone): its
	// label moves along, and it is the owner's plain labels' zone again.
	if _, err := h.st.SetTenantZone(ctx, tenantA, "", true); err != nil {
		t.Fatal(err)
	}
	if e := h.env("web"); e.HostLabel == "web--shop" || e.HostLabel != "web-acme" {
		t.Fatalf("moved: web--shop is the other tenant's in the apps zone, so the natural label: %+v", e)
	}
	if a, err := h.svc.SetPublic(ctx, tenantA, "web", true, "kino"); err != nil ||
		!slices.Equal(a.Hosts, []string{"kino.apps.test"}) {
		t.Fatalf("plain label in the apps zone: %+v %v", a, err)
	}
}
