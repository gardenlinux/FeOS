// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// deploymentPart is the clear part of a pod's HTTP bindings as its
// instance's deployment has it: its version is the deployment's number,
// as a binding whose secret moves with each deployment would have it.
type deploymentPart struct{ st *store.Store }

func (d deploymentPart) PodHTTP(ctx context.Context, row store.Sandbox) (*podv1.PodHttpBindings, error) {
	if row.PodTemplate == nil || len(row.PodTemplate.HTTPBindings) == 0 {
		return nil, nil
	}
	n, err := d.st.EnvInstanceDeployment(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	return &podv1.PodHttpBindings{Names: []string{row.PodTemplate.HTTPBindings[0].Name}, Version: "d" + strconv.Itoa(n)}, nil
}

// bindingSpec is a stored spec whose web service names binding erp, its
// secret pinned at version.
func bindingSpec(version int, upstream string) string {
	return fmt.Sprintf(`{"services":{"web":{"image":%q,"command":["/app"],"replicas":1,"bindings":["erp"],
		"ports":[{"name":"http","port":8080}]}},
		"bindings":{"erp":{"upstream":%q,"mode":"name","credential":{"kind":"headers",
			"headers":{"X-API-Key":{"secret":"erp-key"}}}}},
		"secrets":{"erp-key":{"version":%d}}}`, testImage, upstream, version)
}

// A service's pods name its bindings in their template; a deployment that
// only pins another secret version keeps them and sends them their new
// bindings, while another upstream replaces them.
func TestServiceBindings(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	h.eng.SetHTTPBindings(deploymentPart{h.st})
	env := h.newEnv("stage")
	h.putSecret(env.ID, "erp-key", "one")
	h.putSecret(env.ID, "erp-key", "two")

	r1 := h.apply(env.ID, bindingSpec(1, "https://api.erp.example"))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	rows := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)
	want := []model.TemplateHTTPBinding{{Name: "erp", Upstream: "https://api.erp.example", Mode: "name"}}
	if got := rows[0].Sandbox.PodTemplate.HTTPBindings; !reflect.DeepEqual(got, want) {
		t.Fatalf("the template's bindings: %+v", got)
	}
	first := rows[0].Sandbox.ID
	if got := h.pod("n1", first).GetSpec().GetHttpBindings().GetVersion(); got != "d1" {
		t.Fatalf("the create's part: %q", got)
	}

	// Another secret version: the same pod, its part sent again.
	r2 := h.apply(env.ID, bindingSpec(2, "https://api.erp.example"))
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 30*time.Second)
	rows = h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "kept", ready)
	if rows[0].Sandbox.ID != first {
		t.Fatal("a new secret version replaced the pod")
	}
	deadline := time.Now().Add(10 * time.Second)
	for h.pod("n1", first).GetSpec().GetHttpBindings().GetVersion() != "d2" {
		if time.Now().After(deadline) {
			t.Fatalf("the kept pod's part: %v", h.pod("n1", first).GetSpec().GetHttpBindings())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Another upstream: another template, another pod.
	r3 := h.apply(env.ID, bindingSpec(2, "https://api2.erp.example"))
	h.waitDeployment(env.ID, r3, model.DeploymentDeployed, 30*time.Second)
	rows = h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "replaced", func(r store.EnvInstanceRow) bool {
		return ready(r) && r.Sandbox.ID != first
	})
	if got := rows[0].Sandbox.PodTemplate.HTTPBindings[0].Upstream; got != "https://api2.erp.example" {
		t.Fatalf("the new pod's upstream: %s", got)
	}
}

// auditHTTP records a pod's requests through a binding as ferund counts
// the http records feosd publishes (http_egress and its kept records), n
// of them with status, or refused with code when it is set.
func (h *harness) auditHTTP(sb store.Sandbox, binding string, n, status int, code string) {
	h.t.Helper()
	detail := fmt.Sprintf(`{"type":"http","binding":%q,"method":"GET","path":"/odata/Orders","status":%d,"error":null}`,
		binding, status)
	if code != "" {
		detail = fmt.Sprintf(`{"type":"http","binding":%q,"method":"GET","path":"/odata/Orders","status":%d,`+
			`"error":{"code":%q,"message":"no"}}`, binding, status, code)
	}
	cl := store.ClassifyEgress(status, code != "", code)
	if !cl.Counted {
		return
	}
	c := store.EgressCount{SandboxID: sb.ID, Binding: binding}
	for range n {
		c.Requests++
		for _, f := range []struct {
			on bool
			n  *int64
		}{{cl.Refused, &c.Refused}, {cl.Failing, &c.Failing}, {cl.TokenFailed, &c.TokenFailed}} {
			if f.on {
				*f.n++
			}
		}
		c.Records = append(c.Records, json.RawMessage(detail))
	}
	if len(c.Records) > store.EgressKeep {
		c.Records = c.Records[len(c.Records)-store.EgressKeep:]
	}
	if err := h.st.AddEgress(bg, []store.EgressCount{c}); err != nil {
		h.t.Fatal(err)
	}
}

// ageEgress moves the pods' requests out of the window.
func (h *harness) ageEgress() {
	h.t.Helper()
	h.exec(`UPDATE http_egress SET minute = minute - interval '6 minutes'`)
	h.exec(`UPDATE http_egress_records SET arrived_at = arrived_at - interval '6 minutes'`)
}

// ageIncidents moves the binding incidents' last holding (of cond, or all
// for "") a window back.
func (h *harness) ageIncidents(cond string) {
	h.t.Helper()
	h.exec(`UPDATE env_incidents SET opened_at = opened_at - interval '6 minutes',
		held_at = held_at - interval '6 minutes' WHERE object LIKE 'binding/%' AND ($1 = '' OR condition = $1)`, cond)
}

// The incidents of a binding: opened from its pods' requests in the last
// five minutes, one condition at a time; closed only after a whole window
// in which their condition did not hold, opened again when it holds again
// within the hour, deleted 30 days after they closed.
func TestBindingIncidents(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	h.putSecret(env.ID, "erp-key", "one")
	r1 := h.apply(env.ID, bindingSpec(1, "https://api.erp.example"))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	pod := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0].Sandbox
	pass := func() {
		t.Helper()
		if err := h.c.bindingsLoop(bg); err != nil {
			t.Fatal(err)
		}
	}
	openOf := func() map[string]store.EnvIncident {
		t.Helper()
		out := map[string]store.EnvIncident{}
		for _, i := range h.incidents(env.ID, true) {
			out[i.Object+" "+i.Condition] = i
		}
		return out
	}
	wantOpen := func(what string, conds ...string) map[string]store.EnvIncident {
		t.Helper()
		open := openOf()
		for _, c := range conds {
			if _, ok := open["binding/erp "+c]; !ok {
				t.Fatalf("%s: %s not open: %v", what, c, open)
			}
		}
		if len(open) != len(conds) {
			t.Fatalf("%s: open %v, want %v", what, open, conds)
		}
		return open
	}

	// Too few requests, or mostly fine: nothing.
	h.auditHTTP(pod, "erp", 3, 401, "")
	pass()
	wantOpen("3 requests")
	h.auditHTTP(pod, "erp", 4, 200, "")
	pass()
	wantOpen("3 of 7 refused")

	// Mostly 401 and 403: credential_refused, with the records.
	h.auditHTTP(pod, "erp", 3, 403, "")
	pass()
	refused := wantOpen("credential refused", model.ConditionCredentialRefused)["binding/erp credential_refused"]
	if refused.Deployment != r1 {
		t.Fatalf("on deployment %d", refused.Deployment)
	}
	var ev struct {
		Egress egressEvidence `json:"egress"`
	}
	if err := json.Unmarshal(refused.Evidence, &ev); err != nil || ev.Egress.Requests != 10 || ev.Egress.Refused != 6 ||
		len(ev.Egress.Records) != bindingEvidence {
		t.Fatalf("evidence %s: %v", refused.Evidence, err)
	}
	pass()
	if n := len(h.incidents(env.ID, false)); n != 1 {
		t.Fatalf("a second pass opened again: %d incidents", n)
	}

	// The window moves on: the old records age out, the upstream fails. The
	// refusals held within the window: their incident stays open until a
	// whole window passed without them.
	h.ageEgress()
	h.auditHTTP(pod, "erp", 4, 503, "")
	h.auditHTTP(pod, "erp", 1, 502, "unreachable")
	h.auditHTTP(pod, "erp", 1, 502, "tls")
	h.auditHTTP(pod, "erp", 1, 200, "")
	pass()
	wantOpen("upstream failing", model.ConditionCredentialRefused, model.ConditionUpstreamFailing)
	h.ageIncidents(model.ConditionCredentialRefused)
	pass()
	wantOpen("a window without refusals", model.ConditionUpstreamFailing)

	// Refused again within the hour: the same incident, open again.
	h.ageEgress()
	h.auditHTTP(pod, "erp", 6, 401, "")
	pass()
	again := wantOpen("refused again", model.ConditionCredentialRefused,
		model.ConditionUpstreamFailing)["binding/erp credential_refused"]
	if again.ID != refused.ID || len(h.incidents(env.ID, false)) != 2 {
		t.Fatalf("reopened %d, was %d; %d incidents", again.ID, refused.ID, len(h.incidents(env.ID, false)))
	}

	// The token endpoint fails; the pod's own mistakes count for nothing.
	h.ageEgress()
	h.auditHTTP(pod, "erp", 5, 503, "credential_unavailable")
	h.auditHTTP(pod, "erp", 20, 403, "denied_path")
	h.auditHTTP(pod, "erp", 20, 429, "rate_limited")
	pass()
	wantOpen("token failing", model.ConditionCredentialRefused, model.ConditionUpstreamFailing,
		model.ConditionTokenFailing)

	// Quiet: open while their conditions held within the window, closed
	// after a whole window without them.
	h.ageEgress()
	pass()
	wantOpen("quiet", model.ConditionCredentialRefused, model.ConditionUpstreamFailing, model.ConditionTokenFailing)
	h.ageIncidents("")
	pass()
	wantOpen("after the window")

	// Kept 30 days after they closed.
	h.exec(`UPDATE env_incidents SET closed_at = closed_at - interval '29 days'`)
	pass()
	if n := len(h.incidents(env.ID, false)); n != 3 {
		t.Fatalf("%d incidents within their retention", n)
	}
	h.exec(`UPDATE env_incidents SET closed_at = closed_at - interval '2 days'`)
	pass()
	if n := len(h.incidents(env.ID, false)); n != 0 {
		t.Fatalf("%d incidents past their retention", n)
	}

	// The counts and records are swept after twice the window.
	h.ageEgress()
	pass()
	var left int
	if err := h.d.Pool.QueryRow(bg, `SELECT (SELECT count(*) FROM http_egress) +
		(SELECT count(*) FROM http_egress_records)`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d rows after the sweep: %v", left, err)
	}
}
