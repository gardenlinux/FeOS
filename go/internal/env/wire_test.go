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
			`"image":"registry.test/expense@sha256:0123"}]}`,
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
			`"finishedAt":"2026-10-18T12:00:00Z","detail":""}}`,
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
		Hosts: []string{}, Access: model.AccessAnyone, Objects: []ObjectStatus{}, Routes: []RouteStatus{},
		CreatedAt: "2026-10-18T12:00:00Z"}
	const want = `{"id":"0193a000-0000-7000-8000-0000000000e1","name":"stage","labels":{},"deployment":0,"deployed":0,` +
		`"deleting":false,"public":false,"hosts":[],"access":"anyone","provider":"","rollout":null,"objects":[],"routes":[],"openIncidents":0,` +
		`"createdAt":"2026-10-18T12:00:00Z"}`
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
		Hosts: []string{"stage-acme.apps.test"}, Access: model.AccessVisitors, Provider: "fecode", CreatedAt: st.CreatedAt}
	const wantSum = `{"id":"0193a000-0000-7000-8000-0000000000e1","name":"stage","labels":{},"deployment":0,"deployed":0,` +
		`"state":"none","public":true,"hosts":["stage-acme.apps.test"],"access":"visitors","provider":"fecode",` +
		`"openIncidents":0,"createdAt":"2026-10-18T12:00:00Z"}`
	if got := marshal(t, sum); got != wantSum {
		t.Fatalf("summary\n got %s\nwant %s", got, wantSum)
	}
	if got := marshal(t, PublicAnswer{Hosts: []string{}, Access: model.AccessAnyone}); got !=
		`{"public":false,"hosts":[],"rolling":false,"access":"anyone","provider":""}` {
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
		Base: 7, Current: 7, Images: map[string]string{}, Reachability: spec.Reachability{Edges: []spec.Edge{},
			Egress: []spec.ObjectEgress{}, Routes: []spec.RouteClients{}}}}
	if got := marshal(t, plan); got != `{"valid":true,"findings":[],"diff":[],"base":7,"current":7,"images":{},`+
		`"reachability":{"edges":[],"egress":[],"routes":[]}}` ||
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
