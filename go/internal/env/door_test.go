// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/env/envdeps"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// previewSpec has an attached service in sb beside stageSpec's objects.
func previewSpec(sb podid.ID) string {
	return strings.Replace(stageSpec, `"services": {`, `"services": {
    "dev": {"runtime": "attached", "sandbox": "`+sb.String()+`", "command": ["npm", "run", "dev"], "cwd": "/workspace/app",
      "ports": [{"name": "http", "port": 5173}], "env": {"API": {"service": "api", "port": "http", "key": "url"}}},`, 1)
}

func TestClone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	sb := h.sandbox(tenantA, true)
	src := h.create("preview", nil)
	srcID := uuid.MustParse(src.ID)
	h.putSecret("preview", "token", "v1")
	h.putSecret("preview", "token", "v2")
	withDev := strings.Replace(previewSpec(sb), `"root": {"path": "/", "service": "web", "port": "http"}`,
		`"root": {"path": "/", "service": "web", "port": "http"}, "dev": {"path": "/dev", "service": "dev", "port": "http"}`, 1)
	if _, err := h.svc.Apply(ctx, tenantA, "preview", ApplyRequest{Spec: json.RawMessage(withDev),
		Annotations: map[string]string{"fecode.dev/revision": "r7"}}); err != nil {
		t.Fatal(describe(err))
	}

	// Without the secret, and asking for data: both refused at once.
	_, err := h.svc.Clone(ctx, tenantA, "preview", CloneRequest{Name: "demo", Data: true})
	var ise *InvalidSpecError
	if !errors.As(err, &ise) || !hasFinding(ise.Findings, spec.SeverityError, "data", "copying data is not available") ||
		!hasFinding(ise.Findings, spec.SeverityError, "services.api.env.TOKEN", "the new environment has no secret token") ||
		!hasFinding(ise.Findings, spec.SeverityInfo, "services.dev", "attached service dev is not cloned") ||
		!hasFinding(ise.Findings, spec.SeverityInfo, "routes.dev", "route dev is not cloned") || len(ise.Findings) != 4 {
		t.Fatalf("clone without the secret: %v", describe(err))
	}
	if ise.Findings[2].Remedy != `pass secrets: {"token": {"copy": true}} or a value` ||
		ise.Error() != "the spec has 2 errors, the first: data: copying data is not available" {
		t.Fatalf("remedy %q, %v", ise.Findings[2].Remedy, ise)
	}
	if _, err := h.svc.GetEnv(ctx, tenantA, "demo"); err == nil {
		t.Fatal("a refused clone left an environment")
	}

	// Copied: the latest version, sealed again for the new environment.
	id := uuid.New()
	st, err := h.svc.Clone(ctx, tenantA, "preview", CloneRequest{Name: "demo", ID: id.String(),
		Labels: map[string]string{"fecode.class": "demo"}, Secrets: map[string]CloneSecret{"token": {Copy: true}},
		Author: "a"})
	if err != nil {
		t.Fatal(describe(err))
	}
	if st.ID != id.String() || st.Release != 1 || st.Rollout.State != model.ReleaseWaiting || st.Labels["fecode.class"] != "demo" {
		t.Fatalf("clone status %+v", st)
	}
	r, err := h.st.GetRelease(ctx, id, 1)
	if err != nil || r.Origin != model.OriginClone || r.OriginRef != srcID.String()+"/1" ||
		r.Annotations["fecode.dev/revision"] != "r7" || r.Author != "a" {
		t.Fatalf("release 1 %+v %v", r, err)
	}
	sp, _ := parseStored(r.Spec)
	if _, ok := sp.Services["dev"]; ok || sp.Routes["dev"].Path != "" || sp.Secrets["token"].Version != 1 ||
		sp.Services["api"].ImageAsWritten != "ghcr.io/acme/api:1" {
		t.Fatalf("cloned spec %s", r.Spec)
	}
	sec, err := h.st.GetSecret(ctx, id, "token", 1)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := h.sealer.Open(sec.Sealed, envdeps.SecretAAD(id, "token", 1)); err != nil || string(v) != "v2" {
		t.Fatalf("copied secret %q %v", v, err)
	}
	if _, err := h.sealer.Open(sec.Sealed, envdeps.SecretAAD(srcID, "token", 1)); err == nil {
		t.Fatal("the copy opens under the source's AAD")
	}

	// A given spec, a set value, another release; a taken name.
	given := strings.Replace(stageSpec, `"replicas": 2`, `"replicas": 1`, 1)
	one := 1
	value := "fresh"
	st, err = h.svc.Clone(ctx, tenantA, src.ID, CloneRequest{Name: "demo2", Release: &one, Spec: json.RawMessage(given),
		Secrets: map[string]CloneSecret{"token": {Value: &value}}})
	if err != nil {
		t.Fatal(describe(err))
	}
	e2 := h.env("demo2")
	sec, _ = h.st.GetSecret(ctx, e2.ID, "token", 1)
	if v, err := h.sealer.Open(sec.Sealed, envdeps.SecretAAD(e2.ID, "token", 1)); err != nil || string(v) != "fresh" {
		t.Fatalf("set secret %q %v", v, err)
	}
	sp2, _ := h.svc.releaseSpec(ctx, e2, 1)
	if *sp2.Services["api"].Replicas != 1 {
		t.Fatalf("given spec %+v", sp2.Services["api"])
	}
	_, err = h.svc.Clone(ctx, tenantA, "preview", CloneRequest{Name: "demo", Secrets: map[string]CloneSecret{"token": {Copy: true}}})
	wantStatus(t, err, http.StatusConflict, "an environment named demo exists")
	_, err = h.svc.Clone(ctx, tenantA, "preview", CloneRequest{Name: "demo3", Secrets: map[string]CloneSecret{
		"token": {Copy: true, Value: &value}, "absent": {Copy: true}}})
	if !errors.As(err, &ise) || !hasFinding(ise.Findings, spec.SeverityError, "secrets.token", "is {\"copy\": true} or") ||
		!hasFinding(ise.Findings, spec.SeverityError, "secrets.absent", "has no secret absent to copy") {
		t.Fatalf("bad secrets %v", describe(err))
	}
	// Another tenant's environment's ID: refused as taken before the spec
	// is planned under it, so nothing of what that environment's volumes
	// hold shows; planned for an environment that is not there yet, no
	// volume is read.
	if _, err := h.st.InsertEnvVolume(ctx, store.EnvVolume{EnvID: srcID, Object: "addon/db", VolumeID: uuid.New(),
		SizeMiB: 1024, AddonType: "postgres", DataVersion: "16"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.CreateEnv(ctx, tenantB, CreateEnvRequest{Name: "own"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Apply(ctx, tenantB, "own", ApplyRequest{Spec: json.RawMessage(
		`{"services": {"web": {"image": "ghcr.io/acme/web:1"}}}`)}); err != nil {
		t.Fatal(describe(err))
	}
	probe := `{"addons": {"db": {"type": "postgres", "version": "17"}}, "routes": {"bad": {"path": "x", "service": "nope"}}}`
	_, err = h.svc.Clone(ctx, tenantB, "own", CloneRequest{Name: "probe", ID: src.ID, Spec: json.RawMessage(probe)})
	if errors.As(err, &ise) {
		t.Fatalf("a foreign ID was planned: %v", describe(err))
	}
	wantStatus(t, err, http.StatusConflict, "an environment of id "+src.ID+" exists with another name")
	p, err := h.svc.plan(ctx, tenantB, store.Environment{ID: srcID, Owner: tenantB, Name: "probe"}, json.RawMessage(probe),
		nil, spec.ModeApply, map[string]spec.SecretInfo{})
	if err != nil || p.valid() {
		t.Fatalf("plan %+v %v", p, err)
	}
	for _, f := range p.findings {
		if strings.HasPrefix(f.Path, "addons.db") {
			t.Fatalf("a finding from a foreign volume: %+v", f)
		}
	}

	nine := 9
	_, err = h.svc.Clone(ctx, tenantA, "preview", CloneRequest{Name: "demo3", Release: &nine})
	wantStatus(t, err, http.StatusNotFound, "no such release")
	_, err = h.svc.Clone(ctx, tenantB, "preview", CloneRequest{Name: "demo3"})
	wantStatus(t, err, http.StatusNotFound, "no such environment")
	h.create("bare", nil)
	_, err = h.svc.Clone(ctx, tenantA, "bare", CloneRequest{Name: "demo3"})
	if !errors.As(err, &ise) || !hasFinding(ise.Findings, spec.SeverityError, "release", "has no release to clone") {
		t.Fatalf("clone of nothing %v", describe(err))
	}
}

func TestStatus(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	sb := h.sandbox(tenantA, true)
	st := h.create("stage", map[string]string{"k": "v"})
	e := h.env("stage")
	h.putSecret("stage", "token", "x")
	h.apply("stage", previewSpec(sb), 0)
	// The earlier words, as the version of migration 0021 still writes
	// them (store/env_words.go): 'realised' is deployed, 'rolling'
	// deploying, 'retiring' stopping.
	h.exec("UPDATE env_releases SET state = 'realised', started_at = now(), finished_at = now() WHERE env_id = $1", e.ID)
	h.exec("UPDATE environments SET deployed = 1 WHERE id = $1", e.ID)

	api0 := h.instance(e.ID, spec.KindService, "api", 0, 1, model.PhaseReady, model.StateRunning, "n1", "fd6b::11")
	h.instance(e.ID, spec.KindService, "api", 1, 1, model.PhaseExited, model.StateRunning, "n2", "fd6b::12")
	// Ready counts are of live instances: a ready phase on a row with an
	// op in flight, or one out of touch, does not count; a paused replica
	// of a service that idles (web) does.
	pausing := h.instance(e.ID, spec.KindService, "api", 2, 1, model.PhaseReady, model.StateRunning, "n1", "fd6b::13")
	h.exec(`UPDATE sandboxes SET op = 'pause', op_id = gen_random_uuid(), op_owner = 'i1',
		op_deadline = now() + interval '1 minute' WHERE id = $1`, pausing)
	h.instance(e.ID, spec.KindService, "api", 3, 1, model.PhaseReady, model.StateUnknown, "n2", "fd6b::14")
	h.instance(e.ID, spec.KindService, "web", 0, 1, model.PhaseReady, model.StatePaused, "n1", "")
	pg := h.instance(e.ID, spec.KindAddon, "pg", 0, 1, model.PhaseReady, model.StateRunning, "n1", "fd6b::5")
	h.exec("UPDATE env_instances SET provisioned_hash = 'p', restarts = 2, last_exit = 1, ready_since = now() WHERE sandbox_id = $1", pg)
	h.exec("UPDATE sandboxes SET status_description = 'no capacity' WHERE id = $1", api0)
	pending := h.instance(e.ID, spec.KindAddon, "q", 0, 1, model.PhaseStarting, model.StatePending, "", "")
	h.exec("UPDATE sandboxes SET status_description = 'no capacity on any node' WHERE id = $1", pending)
	if _, err := h.st.InsertAttached(ctx, store.EnvAttached{EnvID: e.ID, Service: "dev", SandboxID: sb,
		Phase: model.PhaseStarting, Detail: "cwd /workspace/app does not exist yet"}); err != nil {
		t.Fatal(err)
	}
	code := 0
	if _, err := h.st.InsertJobRun(ctx, store.EnvJobRun{EnvID: e.ID, Job: "migrate", TemplateHash: "h", Release: 1,
		Attempt: 1, Phase: model.RunFailed}); err != nil {
		t.Fatal(err)
	}
	run, err := h.st.InsertJobRun(ctx, store.EnvJobRun{EnvID: e.ID, Job: "migrate", TemplateHash: "h", Release: 1,
		Attempt: 2, Phase: model.RunSucceeded, ExitCode: &code})
	if err != nil {
		t.Fatal(err)
	}
	h.exec("UPDATE env_job_runs SET finished_at = now() WHERE id = $1", run.ID)
	if _, _, err := h.st.OpenIncident(ctx, store.EnvIncident{EnvID: e.ID, Object: "service/api", Release: 1,
		Condition: model.ConditionCrashLoop, Summary: "api exited 3 times",
		Evidence: json.RawMessage(`{"exits":[{"at":"2026-10-18T14:05:01Z","code":1}],"logTail":["boom"]}`)}); err != nil {
		t.Fatal(err)
	}

	got, err := h.svc.GetEnv(ctx, tenantA, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Release != 1 || got.Deployed != 1 || got.Rollout.State != model.ReleaseDeployed || got.Rollout.StartedAt == "" ||
		got.OpenIncidents != 1 || got.Labels["k"] != "v" {
		t.Fatalf("status %+v", got)
	}
	names := []string{}
	for _, o := range got.Objects {
		names = append(names, o.Kind+"/"+o.Name)
	}
	if strings.Join(names, " ") != "service/api service/dev service/web addon/pg addon/q job/migrate" {
		t.Fatalf("objects %v", names)
	}
	api := got.Objects[0]
	if api.Runtime != spec.RuntimeImage || api.Desired != 2 || api.Ready != 1 || len(api.Instances) != 4 ||
		api.Instances[0].ID != api0.String() || api.Instances[0].Node != "n1" || api.Instances[0].Address != "fd6b::11" ||
		api.Instances[0].Detail != "" || api.Instances[1].Phase != model.PhaseExited ||
		api.Instances[0].Image != "registry.test/img@sha256:"+strings.Repeat("0", 64) {
		t.Fatalf("api %+v", api)
	}
	dev := got.Objects[1]
	if dev.Runtime != spec.RuntimeAttached || dev.Sandbox != sb.String() || dev.Phase != model.PhaseStarting ||
		dev.Detail != "cwd /workspace/app does not exist yet" {
		t.Fatalf("dev %+v", dev)
	}
	if web := got.Objects[2]; web.Ready != 1 || web.Desired != 1 || web.Instances[0].State != model.StatePaused {
		t.Fatalf("web %+v", web)
	}
	pgo := got.Objects[3]
	if pgo.Type != "postgres" || pgo.Version != "17" || pgo.Plan != "small" || pgo.Phase != model.PhaseReady ||
		!pgo.Provisioned || pgo.Instances[0].Restarts != 2 || *pgo.Instances[0].LastExit != 1 || pgo.Instances[0].ReadySince == "" {
		t.Fatalf("pg %+v", pgo)
	}
	if q := got.Objects[4]; q.Phase != model.PhaseStarting || q.Provisioned || q.Instances[0].Detail != "no capacity on any node" {
		t.Fatalf("q %+v", q)
	}
	job := got.Objects[5]
	if job.LastRun == nil || job.LastRun.Attempt != 2 || job.LastRun.Phase != model.RunSucceeded || *job.LastRun.ExitCode != 0 ||
		job.LastRun.FinishedAt == "" {
		t.Fatalf("job %+v", job.LastRun)
	}
	if len(got.Routes) != 2 || got.Routes[0] != (RouteStatus{Name: "api", Path: "/api", Service: "api", Port: 8080, Door: DoorRoute}) ||
		got.Routes[1].Port != 3000 {
		t.Fatalf("routes %+v", got.Routes)
	}
	// The JSON has every field of its kind.
	b, _ := json.Marshal(got)
	for _, want := range []string{`"lastRun":{"release":1,"attempt":2,"phase":"succeeded","exitCode":0`,
		`"kind":"service","name":"dev","runtime":"attached","sandbox":"` + sb.String() + `","phase":"starting","restarts":0,"lastExit":null`,
		`"openIncidents":1`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("status JSON lacks %s:\n%s", want, b)
		}
	}

	// Incidents.
	list, err := h.svc.Incidents(ctx, tenantA, "stage", false)
	if err != nil || len(list.Incidents) != 1 || list.Incidents[0].Object != "service/api" ||
		list.Incidents[0].Evidence.LogTail[0] != "boom" || *list.Incidents[0].Evidence.Exits[0].Code != 1 {
		t.Fatalf("incidents %+v %v", list, err)
	}
	inc, err := h.svc.Incident(ctx, tenantA, "stage", mustInt64(t, list.Incidents[0].ID))
	if err != nil || inc.Condition != model.ConditionCrashLoop || inc.ClosedAt != "" {
		t.Fatalf("incident %+v %v", inc, err)
	}
	_, err = h.svc.Incident(ctx, tenantA, "stage", 999)
	wantStatus(t, err, http.StatusNotFound, "no such incident")
	h.exec("UPDATE env_incidents SET closed_at = now() WHERE env_id = $1", e.ID)
	if list, _ := h.svc.Incidents(ctx, tenantA, "stage", true); len(list.Incidents) != 0 {
		t.Fatalf("open %+v", list)
	}

	// A failed latest release: the objects are the deployed release's,
	// the rollout the latest's.
	changed := strings.Replace(previewSpec(sb), `"replicas": 2`, `"replicas": 4`, 1)
	h.apply("stage", changed, 1)
	h.exec("UPDATE env_releases SET state = 'failed', reason = 'rollout timed out' WHERE env_id = $1 AND number = 2", e.ID)
	got, _ = h.svc.GetEnv(ctx, tenantA, "stage")
	if got.Rollout.Release != 2 || got.Rollout.State != model.ReleaseFailed || got.Rollout.Reason != "rollout timed out" ||
		got.Objects[0].Desired != 2 {
		t.Fatalf("failed latest %+v", got)
	}
	h.exec("UPDATE env_releases SET state = 'rolling' WHERE env_id = $1 AND number = 2", e.ID)
	if got, _ := h.svc.GetEnv(ctx, tenantA, "stage"); got.Objects[0].Desired != 4 {
		t.Fatalf("rolling latest %+v", got.Objects[0])
	}
	list2, _ := h.svc.ListEnvs(ctx, tenantA, nil)
	if list2.Environments[0].State != model.ReleaseDeploying || list2.Environments[0].OpenIncidents != 0 {
		t.Fatalf("list %+v", list2)
	}
}

// An incident's answer has every part of the evidence the controller
// stores (ctl's evidence): a release past its deadline's pending objects
// too.
func TestIncidentEvidence(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	h.create("stage", nil)
	stored := `{"exits":[{"at":"2026-10-18T14:05:01Z","code":1}],"logTail":["boom"],` +
		`"diff":[{"kind":"addon","name":"postgres","change":"add","fields":[],"effect":"new pod"}],` +
		`"placement":"no capacity","pending":["addon postgres is not ready",` +
		`"addon/postgres replica 0: provisioning step provision exited 1"]}`
	// Every key of it is one the answer knows.
	dec := json.NewDecoder(strings.NewReader(stored))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&IncidentEvidence{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.st.OpenIncident(ctx, store.EnvIncident{EnvID: h.env("stage").ID, Object: "addon/postgres", Release: 2,
		Condition: model.ConditionRolloutFailed, Summary: "release 2 did not roll out within 10m0s: addon postgres is not ready",
		Evidence: json.RawMessage(stored)}); err != nil {
		t.Fatal(err)
	}
	list, err := h.svc.Incidents(ctx, tenantA, "stage", true)
	if err != nil || len(list.Incidents) != 1 {
		t.Fatalf("incidents %+v %v", list, err)
	}
	inc, err := h.svc.Incident(ctx, tenantA, "stage", mustInt64(t, list.Incidents[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	ev := inc.Evidence
	if len(ev.Pending) != 2 || ev.Pending[1] != "addon/postgres replica 0: provisioning step provision exited 1" ||
		len(ev.Exits) != 1 || len(ev.LogTail) != 1 || len(ev.Diff) != 1 || ev.Placement != "no capacity" {
		t.Fatalf("evidence %+v", ev)
	}
	b, _ := json.Marshal(list.Incidents[0].Evidence)
	if !strings.Contains(string(b), `"pending":["addon postgres is not ready",`) {
		t.Fatalf("evidence JSON %s", b)
	}
}

func mustInt64(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	if err := json.Unmarshal([]byte(s), &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// doorMux serves the route door at its pattern.
func doorMux(svc *Service) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/environments/{env}/http/{path...}", svc.ServeRoute)
	return mux
}

func routeRequest(mux http.Handler, method, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Host = "stage.boxed.example"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func wantPlain(t *testing.T, w *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if w.Code != status || w.Body.String() != body {
		t.Fatalf("%d %q, want %d %q", w.Code, w.Body.String(), status, body)
	}
	if status >= 400 && w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type %q", w.Header().Get("Content-Type"))
	}
}

func TestRouteDoor(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	mux := doorMux(h.svc)
	// A sandbox made before ADR 0019, with a UUID: its attached service
	// is routed to as a short ID's is.
	sb := h.legacySandbox(tenantA, true)
	st := h.create("stage", nil)
	e := h.env("stage")
	h.create("prod", nil)
	tok, err := h.svc.CreateRouteToken(ctx, tenantA, "stage")
	if err != nil {
		t.Fatal(err)
	}
	prodTok, _ := h.svc.CreateRouteToken(ctx, tenantA, "prod")

	// The token: required, the environment's own; a tenant token is no
	// route token.
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/stage/http/", ""), 401, "route token required\n")
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/stage/http/", "not-a-token"), 401, "route token required\n")
	tenantTok := "tenant-token"
	if _, err := h.st.CreateTenantToken(ctx, store.HashToken(tenantTok), tenantA, "t"); err != nil {
		t.Fatal(err)
	}
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/stage/http/", tenantTok), 401, "route token required\n")
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/stage/http/", prodTok.Token), 404, "no such environment\n")
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/nope/http/", tok.Token), 404, "no such environment\n")
	// No release, no route.
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/stage/http/x", tok.Token), 404, "no route for /x\n")

	h.putSecret("stage", "token", "x")
	h.apply("stage", previewSpec(sb), 0)
	// Nothing runs: 503, Retry-After 5.
	w := routeRequest(mux, "GET", "/api/v1/environments/stage/http/api/v", tok.Token)
	wantPlain(t, w, 503, "no ready instance of api\n")
	if w.Header().Get("Retry-After") != "5" {
		t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
	}

	a0 := h.instance(e.ID, spec.KindService, "api", 0, 1, model.PhaseReady, model.StateRunning, "n1", "fd6b::11")
	a1 := h.instance(e.ID, spec.KindService, "api", 1, 1, model.PhaseReady, model.StateRunning, "n2", "fd6b::12")
	h.instance(e.ID, spec.KindService, "api", 2, 1, model.PhaseStopping, model.StateRunning, "n2", "fd6b::13")
	h.instance(e.ID, spec.KindService, "api", 3, 1, model.PhaseStarting, model.StateRunning, "n2", "fd6b::14")
	web := h.instance(e.ID, spec.KindService, "web", 0, 1, model.PhaseReady, model.StatePaused, "n1", "")

	// /api strips its prefix; ready instances take turns; the query and
	// the Host stay, the bearer goes to the door, which strips it.
	seen := map[podid.ID]int{}
	for range 4 {
		w := routeRequest(mux, "GET", "/api/v1/environments/stage/http/api/reports/2026?month=10", tok.Token)
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		f := h.ports.last(t)
		if f.port != 8080 || f.path != "/reports/2026" || f.rawPath != "" || f.query != "month=10" ||
			f.host != "stage.boxed.example" {
			t.Fatalf("forward %+v", f)
		}
		seen[f.sandbox]++
	}
	if seen[a0] != 2 || seen[a1] != 2 {
		t.Fatalf("round robin %v (want %s and %s twice)", seen, a0, a1)
	}
	if f := h.ports.last(t); !slices.Equal(f.credentials, []string{"Authorization", RouteTokenHeader}) {
		t.Fatalf("a bearer route token: the door strips %q", f.credentials)
	}
	// The route token in X-FeRun-Route-Token: that header stops at the
	// door, the client's Authorization goes on; it wins over a bearer
	// token, and a tenant token is no route token there either.
	withHeader := func(routeTok, authz string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/v1/environments/stage/http/api/me", nil)
		r.Header.Set(RouteTokenHeader, routeTok)
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if w := withHeader(tok.Token, "Bearer client-jwt"); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if f := h.ports.last(t); f.authz != "Bearer client-jwt" || !slices.Equal(f.credentials, []string{RouteTokenHeader}) {
		t.Fatalf("a route token header: %+v", f)
	}
	if w := withHeader(tok.Token, ""); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	wantPlain(t, withHeader(tenantTok, ""), 401, "route token required\n")
	wantPlain(t, withHeader(tenantTok, "Bearer "+tok.Token), 401, "route token required\n")
	wantPlain(t, withHeader("not-a-token", "Bearer "+tok.Token), 401, "route token required\n")
	// Beside the header, the Authorization that would go on may hold no
	// FeRun credential: a tenant's or a sandbox's token as its bearer is
	// refused and nothing forwarded; any other passes, as above.
	sandboxTok := "sandbox-token"
	if _, err := h.st.CreateSandboxToken(ctx, store.HashToken(sandboxTok), sb, "s"); err != nil {
		t.Fatal(err)
	}
	forwards := func() int {
		h.ports.mu.Lock()
		defer h.ports.mu.Unlock()
		return len(h.ports.calls)
	}
	for _, authz := range []string{"Bearer " + tenantTok, "bearer " + tenantTok, "Bearer " + sandboxTok} {
		before := forwards()
		wantPlain(t, withHeader(tok.Token, authz), 401, "route token required\n")
		if forwards() != before {
			t.Fatalf("%.12s…: forwarded %+v", authz, h.ports.last(t))
		}
	}
	if w := withHeader(tok.Token, "Bearer client-jwt"); w.Code != 200 || h.ports.last(t).authz != "Bearer client-jwt" {
		t.Fatalf("a client's own bearer beside the header: %d %s", w.Code, w.Body)
	}
	// Nor in a second Authorization behind the client's own.
	before := forwards()
	r2 := httptest.NewRequest("GET", "/api/v1/environments/stage/http/api/me", nil)
	r2.Header.Set(RouteTokenHeader, tok.Token)
	r2.Header.Add("Authorization", "Bearer client-jwt")
	r2.Header.Add("Authorization", "Bearer "+tenantTok)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, r2)
	wantPlain(t, w2, 401, "route token required\n")
	if forwards() != before {
		t.Fatalf("a second Authorization: forwarded %+v", h.ports.last(t))
	}
	// Segment-aware: /apix is the root's; the root does not strip; a
	// paused instance is woken by the forward (scale from zero).
	for _, c := range []struct{ path, up string }{
		{"/apix", "/apix"}, {"/", "/"}, {"/api", "/"}, {"/api/", "/"}, {"/static/app.js", "/static/app.js"},
	} {
		w := routeRequest(mux, "GET", "/api/v1/environments/"+st.ID+"/http"+c.path, tok.Token)
		f := h.ports.last(t)
		if w.Code != 200 || f.path != c.up {
			t.Fatalf("%s: %d %+v", c.path, w.Code, f)
		}
		if strings.HasPrefix(c.path, "/apix") || c.path == "/" || strings.HasPrefix(c.path, "/static") {
			if f.sandbox != web || f.port != 3000 {
				t.Fatalf("%s went to %+v", c.path, f)
			}
		}
	}
	// An escaped segment stays escaped upstream.
	routeRequest(mux, "GET", "/api/v1/environments/stage/http/api/a%2Fb/c%20d", tok.Token)
	if f := h.ports.last(t); f.path != "/a/b/c d" || f.rawPath != "/a%2Fb/c%20d" {
		t.Fatalf("escaped %+v", f)
	}
	// A route to an attached service goes to its sandbox, once the
	// controller has its row.
	h.apply("stage", strings.Replace(previewSpec(sb), `"root": {"path": "/", "service": "web", "port": "http"}`,
		`"root": {"path": "/", "service": "web", "port": "http"}, "dev": {"path": "/dev", "service": "dev", "port": "http"}`, 1), 1)
	w = routeRequest(mux, "POST", "/api/v1/environments/stage/http/dev/src/main.ts", tok.Token)
	wantPlain(t, w, 503, "no ready instance of dev\n")
	if _, err := h.st.InsertAttached(ctx, store.EnvAttached{EnvID: e.ID, Service: "dev", SandboxID: sb}); err != nil {
		t.Fatal(err)
	}
	routeRequest(mux, "POST", "/api/v1/environments/stage/http/dev/src/main.ts", tok.Token)
	if f := h.ports.last(t); f.sandbox != sb || f.port != 5173 || f.path != "/dev/src/main.ts" {
		t.Fatalf("attached %+v", f)
	}
	// The deployed release's routes serve while a later one rolls.
	h.exec("UPDATE environments SET deployed = 1 WHERE id = $1", e.ID)
	if w := routeRequest(mux, "GET", "/api/v1/environments/stage/http/dev/x", tok.Token); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if f := h.ports.last(t); f.sandbox != web {
		t.Fatalf("deployed release's routes: %+v", f)
	}

	// .. segments, also escaped (the mux redirects plain ones itself).
	r := httptest.NewRequest("GET", "/api/v1/environments/stage/http/api/%2e%2e/admin", nil)
	r.Header.Set("Authorization", "Bearer "+tok.Token)
	r.SetPathValue("env", "stage")
	w = httptest.NewRecorder()
	h.svc.ServeRoute(w, r)
	wantPlain(t, w, 400, "the path has a .. segment\n")
	r = httptest.NewRequest("GET", "/api/v1/environments/stage/http/api/%2e/admin", nil)
	r.Header.Set("Authorization", "Bearer "+tok.Token)
	r.SetPathValue("env", "stage")
	w = httptest.NewRecorder()
	h.svc.ServeRoute(w, r)
	wantPlain(t, w, 400, "the path has a . segment\n")
	// A percent sign is a character like any other.
	routeRequest(mux, "GET", "/api/v1/environments/stage/http/api/q/100%25", tok.Token)
	if f := h.ports.last(t); f.path != "/q/100%" || f.rawPath != "" {
		t.Fatalf("percent %+v", f)
	}

	// The token was used; revoked, it opens nothing.
	rt, _ := h.st.ResolveRouteToken(ctx, store.HashToken(tok.Token))
	if rt.LastUsedAt.IsZero() {
		t.Fatal("last_used_at not set")
	}
	if err := h.svc.RevokeRouteTokens(ctx, tenantA, "stage"); err != nil {
		t.Fatal(err)
	}
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/stage/http/", tok.Token), 401, "route token required\n")
	// A deleting environment is gone for the door.
	if err := h.svc.DeleteEnv(ctx, tenantA, "prod"); err != nil {
		t.Fatal(err)
	}
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/prod/http/", prodTok.Token), 404, "no such environment\n")

	// Without the port door: 503.
	off := newHarness(t, harnessOptions{noPorts: true})
	off.create("stage", nil)
	off.putSecret("stage", "token", "x")
	off.apply("stage", stageSpec, 0)
	offTok, _ := off.svc.CreateRouteToken(off.ctx(), tenantA, "stage")
	wantPlain(t, routeRequest(doorMux(off.svc), "GET", "/api/v1/environments/stage/http/", offTok.Token), 503,
		"not available on this instance\n")
}

// The route door serves anonymous clients: a failure inside ferund is
// logged, and its answer names no cause.
func TestRouteDoorFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	mux := doorMux(h.svc)
	h.create("stage", nil)
	h.putSecret("stage", "token", "x")
	h.apply("stage", stageSpec, 0)
	tok, err := h.svc.CreateRouteToken(h.ctx(), tenantA, "stage")
	if err != nil {
		t.Fatal(err)
	}
	// The lookup of a bearer token beside the route token header.
	h.exec(`ALTER TABLE tenant_tokens RENAME TO tenant_tokens_away`)
	r := httptest.NewRequest("GET", "/api/v1/environments/stage/http/api/x", nil)
	r.Header.Set(RouteTokenHeader, tok.Token)
	r.Header.Set("Authorization", "Bearer client-jwt")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	wantPlain(t, w, 500, "the route door failed\n")
	h.exec(`ALTER TABLE tenant_tokens_away RENAME TO tenant_tokens`)
	// A stored spec that does not parse.
	h.exec(`UPDATE env_releases SET spec = '{"services": 5}' WHERE env_id = $1`, h.env("stage").ID)
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/stage/http/api/x", tok.Token), 500,
		"the route door failed\n")
	// A database that is gone.
	h.d.Close()
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/stage/http/api/x", tok.Token), 500,
		"the route door failed\n")
}

func TestRoutePaths(t *testing.T) {
	routes := map[string]spec.Route{
		"root": {Path: "/"}, "api": {Path: "/api"}, "deep": {Path: "/api/v2/"}, "x": {Path: "/a b"},
	}
	for _, c := range []struct {
		raw  string
		want string
		n    int
	}{
		{"/", "/", 0}, {"/api", "/api", 1}, {"/api/", "/api", 1}, {"/apis", "/", 0}, {"/api/v2", "/api/v2/", 2},
		{"/api/v2/x", "/api/v2/", 2}, {"/api/v3", "/api", 1}, {"/a%20b/c", "/a b", 1}, {"/API", "/", 0},
	} {
		r, n, ok := matchRoute(routes, c.raw)
		if !ok || r.Path != c.want || n != c.n {
			t.Errorf("%s: %+v %d %v, want %s %d", c.raw, r, n, ok, c.want, c.n)
		}
	}
	if _, _, ok := matchRoute(map[string]spec.Route{"api": {Path: "/api"}}, "/web"); ok {
		t.Error("matched without a root")
	}
	for _, c := range []struct {
		raw, path, rawPath string
		n                  int
	}{
		{"/api/x", "/x", "", 1}, {"/api", "/", "", 1}, {"/api/a%2Fb", "/a/b", "/a%2Fb", 1}, {"/a/b/c", "/a/b/c", "", 0},
	} {
		p, rp, err := stripSegments(c.raw, c.n)
		if err != nil || p != c.path || rp != c.rawPath {
			t.Errorf("strip %s %d: %q %q %v", c.raw, c.n, p, rp, err)
		}
	}
	// Only a segment that is, or decodes once or twice to, . or .. is
	// refused; a % that starts no escape after a decode is a character.
	for _, c := range []struct{ raw, want string }{
		{"/a/../b", ".."}, {"/a/%2e%2e/b", ".."}, {"/a/%252e%252e/b", ".."}, {"/a%2F..%2Fb", ".."},
		{"/%252e%252e/x%25", ".."}, {"/%252e%2e/x", ".."}, {"/a/%2e/b", "."}, {"/a/%252E", "."},
		{"/a/..b/c", ""}, {"/a/b", ""}, {"/q/100%25", ""}, {"/search/50%25off", ""}, {"/a%2541", ""},
		{"/a/%25252e%25252e/b", ""}, {"/%2e%2e%zz", ""}, {"/files/a%25zz/..x", ""},
	} {
		if got := dotSegment(c.raw); got != c.want {
			t.Errorf("dot segment of %s: %q, want %q", c.raw, got, c.want)
		}
	}
}

// The route door forwards to an attached service's sandbox only while it
// is the environment owner's and not an instance row: a sandbox ID is
// free again once deleted, and another tenant may take it. The owner's
// sandbox created again under the same ID (FeCode's replaceLostSandbox)
// is a new incarnation, which is routed to (§13.6).
func TestRouteDoorAttachedOwner(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	mux := doorMux(h.svc)
	sb := h.sandbox(tenantA, true)
	h.create("preview", nil)
	e := h.env("preview")
	tok, err := h.svc.CreateRouteToken(ctx, tenantA, "preview")
	if err != nil {
		t.Fatal(err)
	}
	h.putSecret("preview", "token", "x")
	h.apply("preview", strings.Replace(previewSpec(sb), `"root": {"path": "/", "service": "web", "port": "http"}`,
		`"root": {"path": "/", "service": "web", "port": "http"}, "dev": {"path": "/dev", "service": "dev", "port": "http"}`, 1), 0)
	first, err := h.st.GetSandbox(ctx, sb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.InsertAttached(ctx, store.EnvAttached{EnvID: e.ID, Service: "dev", SandboxID: sb,
		SandboxIncarnation: first.Incarnation, ProcessID: "p1", Phase: model.PhaseReady}); err != nil {
		t.Fatal(err)
	}
	const path = "/api/v1/environments/preview/http/dev/"
	if w := routeRequest(mux, "GET", path, tok.Token); w.Code != 200 || h.ports.last(t).sandbox != sb {
		t.Fatalf("own sandbox: %d %s", w.Code, w.Body)
	}
	calls := func() int {
		h.ports.mu.Lock()
		defer h.ports.mu.Unlock()
		return len(h.ports.calls)
	}
	refused := func(why string) {
		t.Helper()
		before := calls()
		w := routeRequest(mux, "GET", path, tok.Token)
		wantPlain(t, w, 503, "sandbox "+sb.String()+" of service dev is gone or not this tenant's\n")
		if calls() != before {
			t.Fatalf("%s: forwarded %+v", why, h.ports.last(t))
		}
	}
	recreate := func(n store.NewSandbox) {
		t.Helper()
		h.exec("DELETE FROM sandboxes WHERE id = $1", sb)
		if n.Owner != "" {
			n.ID, n.Image, n.VCPUs, n.MemoryMiB = sb, "img", 1, 512
			if _, _, err := h.st.CreateSandbox(ctx, n); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Gone.
	recreate(store.NewSandbox{})
	refused("gone")
	// Another tenant's sandbox of the same ID, attachable.
	recreate(store.NewSandbox{Owner: tenantB, PodTemplate: &model.PodTemplate{Command: []string{"sleep", "infinity"},
		SecurityGroups: []string{model.SandboxGroupID(sb)}}})
	refused("another tenant's")
	// An instance row of the same ID.
	zero := 0
	recreate(store.NewSandbox{Owner: tenantA, EnvironmentID: e.ID, IdleAfterS: &zero,
		PodTemplate: &model.PodTemplate{SecurityGroups: []string{model.EnvGroupID(e.ID)}}})
	refused("an instance row")
	// The owner's own sandbox again, a new incarnation under the same ID
	// (the row still records the first one): routed to.
	recreate(store.NewSandbox{Owner: tenantA, PodTemplate: &model.PodTemplate{Command: []string{"sleep", "infinity"},
		SecurityGroups: []string{model.SandboxGroupID(sb)}}})
	if again, err := h.st.GetSandbox(ctx, sb); err != nil || again.Incarnation == first.Incarnation {
		t.Fatalf("the recreated sandbox %+v %v", again, err)
	}
	if a, err := h.st.GetAttached(ctx, e.ID, "dev"); err != nil || a.SandboxIncarnation != first.Incarnation {
		t.Fatalf("attached %+v %v", a, err)
	}
	if w := routeRequest(mux, "GET", path, tok.Token); w.Code != 200 || h.ports.last(t).sandbox != sb {
		t.Fatalf("recreated own sandbox: %d %s", w.Code, w.Body)
	}
	// Another tenant's again: refused.
	recreate(store.NewSandbox{Owner: tenantB, PodTemplate: &model.PodTemplate{Command: []string{"sleep", "infinity"},
		SecurityGroups: []string{model.SandboxGroupID(sb)}}})
	refused("another tenant's after the owner's")
}

// The port of a request comes from the release of the instance it goes
// to: while a release that changes the port rolls, its ready instances
// get the new one, the old release's the old one.
func TestRouteDoorRollingPort(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	mux := doorMux(h.svc)
	h.create("stage", nil)
	e := h.env("stage")
	tok, err := h.svc.CreateRouteToken(ctx, tenantA, "stage")
	if err != nil {
		t.Fatal(err)
	}
	specOf := func(name string, port int) string {
		return fmt.Sprintf(`{"services": {"api": {"image": "ghcr.io/acme/api:1", "env": {"PORT": "%d"},
		  "ports": [{"name": "%s", "port": %d}]}}, "routes": {"api": {"path": "/", "service": "api", "port": "%s"}}}`,
			port, name, port, name)
	}
	h.apply("stage", specOf("http", 8080), 0)
	h.exec("UPDATE environments SET deployed = 1 WHERE id = $1", e.ID)
	h.apply("stage", specOf("http", 9090), 1)
	port := func() int {
		t.Helper()
		if w := routeRequest(mux, "GET", "/api/v1/environments/stage/http/x", tok.Token); w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		return h.ports.last(t).port
	}
	old := h.instance(e.ID, spec.KindService, "api", 0, 1, model.PhaseReady, model.StateRunning, "n1", "fd6b::11")
	if p := port(); p != 8080 {
		t.Fatalf("release 1's instance on %d", p)
	}
	h.exec("UPDATE env_instances SET phase = 'retiring' WHERE sandbox_id = $1", old)
	h.instance(e.ID, spec.KindService, "api", 0, 2, model.PhaseReady, model.StateRunning, "n1", "fd6b::12")
	if p := port(); p != 9090 {
		t.Fatalf("release 2's instance on %d", p)
	}
	// A release that has no such port falls back to the matched one's.
	h.apply("stage", specOf("web", 7070), 2)
	h.exec("UPDATE env_instances SET phase = 'retiring' WHERE env_id = $1", e.ID)
	h.instance(e.ID, spec.KindService, "api", 0, 3, model.PhaseReady, model.StateRunning, "n1", "fd6b::13")
	if p := port(); p != 8080 {
		t.Fatalf("release 3's instance on %d", p)
	}
}
