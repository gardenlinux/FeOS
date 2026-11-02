// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
)

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Each kind of object shows its own fields, all of them (§4.9).
func TestObjectStatusJSON(t *testing.T) {
	zero := 0
	cases := []struct {
		o    ObjectStatus
		want string
	}{{
		ObjectStatus{Kind: spec.KindService, Name: "expense-service", Runtime: spec.RuntimeImage, Desired: 2, Ready: 1,
			Instances: []InstanceStatus{{ID: "0193a000-0000-7000-8000-000000000101", Replica: 0, Phase: model.PhaseReady,
				State: model.StateRunning, Node: "n1", Address: "fd6b::5", Deployment: 7, ReadySince: "2026-10-18T12:00:00Z",
				Image: "registry.test/expense@sha256:0123"}}},
		`{"kind":"service","name":"expense-service","runtime":"image","desired":2,"ready":1,"instances":[` +
			`{"id":"0193a000-0000-7000-8000-000000000101","replica":0,"phase":"ready","state":"running","node":"n1",` +
			`"address":"fd6b::5","deployment":7,"restarts":0,"lastExit":null,"readySince":"2026-10-18T12:00:00Z","detail":"",` +
			`"image":"registry.test/expense@sha256:0123","release":7}]}`,
	}, {
		ObjectStatus{Kind: spec.KindService, Name: "idle", Runtime: spec.RuntimeImage},
		`{"kind":"service","name":"idle","runtime":"image","desired":0,"ready":0,"instances":[]}`,
	}, {
		ObjectStatus{Kind: spec.KindService, Name: "web", Runtime: spec.RuntimeAttached,
			Sandbox: "0193a000-0000-7000-8000-00000000000a", Phase: model.PhaseReady},
		`{"kind":"service","name":"web","runtime":"attached","sandbox":"0193a000-0000-7000-8000-00000000000a",` +
			`"phase":"ready","restarts":0,"lastExit":null,"detail":""}`,
	}, {
		ObjectStatus{Kind: spec.KindAddon, Name: "postgres", Type: "postgres", Version: "17", Plan: "small",
			Phase: model.PhaseReady, Provisioned: true},
		`{"kind":"addon","name":"postgres","type":"postgres","version":"17","plan":"small","phase":"ready",` +
			`"provisioned":true,"instances":[]}`,
	}, {
		ObjectStatus{Kind: spec.KindJob, Name: "migrate", LastRun: &JobRunStatus{Deployment: 7, Attempt: 1,
			Phase: model.RunSucceeded, ExitCode: &zero, FinishedAt: "2026-10-18T12:00:00Z"}},
		`{"kind":"job","name":"migrate","lastRun":{"deployment":7,"attempt":1,"phase":"succeeded","exitCode":0,` +
			`"finishedAt":"2026-10-18T12:00:00Z","detail":"","release":7}}`,
	}, {
		ObjectStatus{Kind: spec.KindJob, Name: "seed"},
		`{"kind":"job","name":"seed","lastRun":null}`,
	}}
	for _, c := range cases {
		if got := marshal(t, c.o); got != c.want {
			t.Errorf("%s/%s:\n got %s\nwant %s", c.o.Kind, c.o.Name, got, c.want)
		}
		var back ObjectStatus
		if err := json.Unmarshal([]byte(c.want), &back); err != nil || marshal(t, back) != c.want {
			t.Errorf("%s/%s does not survive decoding: %v", c.o.Kind, c.o.Name, err)
		}
	}
}

func TestStatusJSON(t *testing.T) {
	st := EnvStatus{ID: "0193a000-0000-7000-8000-0000000000e1", Name: "stage", Labels: map[string]string{},
		Hosts: []string{}, Objects: []ObjectStatus{}, Routes: []RouteStatus{}, CreatedAt: "2026-10-18T12:00:00Z"}
	const want = `{"id":"0193a000-0000-7000-8000-0000000000e1","name":"stage","labels":{},"deployment":0,"deployed":0,` +
		`"deleting":false,"public":false,"hosts":[],"rollout":null,"objects":[],"routes":[],"openIncidents":0,` +
		`"createdAt":"2026-10-18T12:00:00Z","release":0}`
	if got := marshal(t, st); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// A route door route has no url; an ingress route has one.
	routes := []RouteStatus{{Name: "dev", Path: "/dev", Service: "dev", Port: 5173, Door: DoorRoute},
		{Name: "api", Path: "/api/", Service: "api", Port: 8080, Door: DoorIngress, URL: "https://stage-acme.apps.test/api"}}
	const wantRoutes = `[{"name":"dev","path":"/dev","service":"dev","port":5173,"door":"route"},` +
		`{"name":"api","path":"/api/","service":"api","port":8080,"door":"ingress","url":"https://stage-acme.apps.test/api"}]`
	if got := marshal(t, routes); got != wantRoutes {
		t.Fatalf("routes\n got %s\nwant %s", got, wantRoutes)
	}
	sum := EnvSummary{ID: st.ID, Name: "stage", Labels: map[string]string{}, State: model.EnvNone, Public: true,
		Hosts: []string{"stage-acme.apps.test"}, CreatedAt: st.CreatedAt}
	const wantSum = `{"id":"0193a000-0000-7000-8000-0000000000e1","name":"stage","labels":{},"deployment":0,"deployed":0,` +
		`"state":"none","public":true,"hosts":["stage-acme.apps.test"],"openIncidents":0,"createdAt":"2026-10-18T12:00:00Z",` +
		`"release":0}`
	if got := marshal(t, sum); got != wantSum {
		t.Fatalf("summary\n got %s\nwant %s", got, wantSum)
	}
	if got := marshal(t, PublicAnswer{Hosts: []string{}}); got != `{"public":false,"hosts":[],"rolling":false}` {
		t.Fatalf("public answer %s", got)
	}
}

func TestAnswersJSON(t *testing.T) {
	a := ApplyAnswer{DeploymentInfo: DeploymentInfo{Number: 8, State: model.DeploymentWaiting, Origin: model.OriginApply,
		Annotations: map[string]string{}, Base: 7, Tags: []string{}, CreatedAt: "2026-10-18T12:00:00Z"},
		Diff: []spec.DiffEntry{}, Findings: []spec.Finding{}}
	const want = `{"number":8,"state":"waiting","origin":"apply","originRef":"","author":"","message":"",` +
		`"annotations":{},"base":7,"reason":"","createdAt":"2026-10-18T12:00:00Z","startedAt":"","finishedAt":"",` +
		`"tags":[],"diff":[],"findings":[]}`
	if got := marshal(t, a); got != want {
		t.Fatalf("apply answer\n got %s\nwant %s", got, want)
	}
	if a.StatusCode() != http.StatusCreated {
		t.Fatal("a new deployment is 201")
	}
	a.Unchanged = true
	if got := marshal(t, a); !strings.Contains(got, `"unchanged":true`) || a.StatusCode() != http.StatusOK {
		t.Fatalf("unchanged answer %s, %d", got, a.StatusCode())
	}

	plan := ObjectAnswer{DryRun: true, Plan: PlanAnswer{Valid: true, Findings: []spec.Finding{}, Diff: []spec.DiffEntry{},
		Base: 7, Current: 7}}
	if got := marshal(t, plan); got != `{"valid":true,"findings":[],"diff":[],"base":7,"current":7}` ||
		plan.StatusCode() != http.StatusOK {
		t.Fatalf("dry run answer %s, %d", got, plan.StatusCode())
	}
	applied := ObjectAnswer{Apply: a}
	if got := marshal(t, applied); got != marshal(t, a) || applied.StatusCode() != http.StatusOK {
		t.Fatalf("object answer %s", got)
	}
}

func TestWriteError(t *testing.T) {
	cases := []struct {
		err    error
		status int
		body   string
	}{{
		&InvalidSpecError{Findings: []spec.Finding{
			{Severity: spec.SeverityError, Path: "services.web.ports[0].port", Problem: "port 0 is out of range", Remedy: "use 1..65535"},
			{Severity: spec.SeverityWarning, Path: "services.web.idle", Problem: "w", Remedy: ""},
		}},
		422,
		`{"error":"the spec has an error: services.web.ports[0].port: port 0 is out of range","code":"invalid_spec",` +
			`"findings":[{"severity":"error","path":"services.web.ports[0].port","problem":"port 0 is out of range",` +
			`"remedy":"use 1..65535"},{"severity":"warning","path":"services.web.idle","problem":"w","remedy":""}]}`,
	}, {
		&BaseMismatchError{Base: 6, Current: 7},
		409,
		`{"error":"base 6 is not the current deployment 7: plan again on deployment 7","code":"base_mismatch","current":7}`,
	}, {
		errNoEnvironment(), 404, `{"error":"no such environment"}`,
	}, {
		errNoSecrets(), 503, `{"error":"secrets need --secret-key-file"}`,
	}, {
		errors.New("boom"), 500, `{"error":"boom"}`,
	}}
	for _, c := range cases {
		w := httptest.NewRecorder()
		WriteError(w, c.err)
		if w.Code != c.status || strings.TrimSpace(w.Body.String()) != c.body {
			t.Errorf("%v: %d %s, want %d %s", c.err, w.Code, w.Body, c.status, c.body)
		}
	}
	var ae *apierr.Error
	if err := errNoObject("service", "x"); !errors.As(err, &ae) || ae.Status != http.StatusNotFound ||
		ae.Msg != "no such object: service/x" {
		t.Fatalf("no object is %v", err)
	}
	e := &InvalidSpecError{Findings: []spec.Finding{{Severity: spec.SeverityError, Problem: "a"},
		{Severity: spec.SeverityError, Path: "p", Problem: "b"}}}
	if e.Error() != "the spec has 2 errors, the first: a" {
		t.Fatal(e.Error())
	}
}

// The deployments' earlier word, for one version (ADR 0018, "The
// deployments' word"): every number an answer calls deployment is there
// as release too, a list of deployments as releases too; a request and an
// answer read either key, both only when they are equal.
func TestEarlierWordJSON(t *testing.T) {
	answers := []struct {
		v    any
		want string
	}{
		{RolloutStatus{Deployment: 7, State: model.DeploymentDeploying},
			`{"deployment":7,"state":"deploying","startedAt":"","reason":"","release":7}`},
		{Tag{Tag: "v1", Deployment: 3, MovedBy: "a", MovedAt: "2026-10-18T12:00:00Z"},
			`{"tag":"v1","deployment":3,"movedBy":"a","movedAt":"2026-10-18T12:00:00Z","release":3}`},
		{DeploymentList{Deployments: []DeploymentInfo{}}, `{"deployments":[],"releases":[]}`},
		{Incident{ID: "1", Object: "service/web", Deployment: 2, Condition: model.ConditionCrashLoop,
			Summary: "a <b> & c"}, `{"id":"1","object":"service/web","deployment":2,"condition":"crash_loop",` +
			`"openedAt":"","closedAt":"","summary":"a \u003cb\u003e \u0026 c","evidence":{},"release":2}`},
	}
	for _, c := range answers {
		if got := marshal(t, c.v); got != c.want {
			t.Errorf("%T\n got %s\nwant %s", c.v, got, c.want)
		}
	}

	var tag PutTagRequest
	if err := json.Unmarshal([]byte(`{"release":4,"author":"a"}`), &tag); err != nil || tag.Deployment != 4 || tag.Author != "a" {
		t.Fatalf("a tag under release: %+v %v", tag, err)
	}
	if err := json.Unmarshal([]byte(`{"deployment":5,"release":5}`), &tag); err != nil || tag.Deployment != 5 {
		t.Fatalf("a tag under both keys: %+v %v", tag, err)
	}
	var clone CloneRequest
	if err := json.Unmarshal([]byte(`{"name":"demo"}`), &clone); err != nil || clone.Deployment != nil {
		t.Fatalf("a clone without a number: %+v %v", clone, err)
	}
	if err := json.Unmarshal([]byte(`{"name":"demo","release":0}`), &clone); err != nil || clone.Deployment == nil ||
		*clone.Deployment != 0 || clone.Name != "demo" {
		t.Fatalf("a clone under release: %+v %v", clone, err)
	}
	for _, body := range []string{`{"name":"demo","deployment":3,"release":4}`} {
		if err := json.Unmarshal([]byte(body), &clone); err == nil || !strings.Contains(err.Error(), "differ") {
			t.Fatalf("%s: %v", body, err)
		}
	}
	if err := json.Unmarshal([]byte(`{"deployment":3,"release":4}`), &tag); err == nil {
		t.Fatal("a tag under two numbers")
	}
	var list DeploymentList
	if err := json.Unmarshal([]byte(`{"releases":[{"number":2}]}`), &list); err != nil || len(list.Deployments) != 1 ||
		list.Deployments[0].Number != 2 {
		t.Fatalf("a list under releases: %+v %v", list, err)
	}
	var st EnvStatus
	if err := json.Unmarshal([]byte(`{"name":"stage","release":6,"rollout":{"release":6,"state":"deployed"}}`), &st); err != nil ||
		st.Deployment != 6 || st.Name != "stage" || st.Rollout == nil || st.Rollout.Deployment != 6 {
		t.Fatalf("a status under release: %+v %v", st, err)
	}
}
