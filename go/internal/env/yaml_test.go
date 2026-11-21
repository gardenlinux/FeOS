// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/gardenlinux/feos/go/internal/env/spec"
)

// A spec in YAML plans, applies and clones as the JSON it stands for
// (ADR 0057); what does not read answers as a JSON spec's parse errors
// do: a plan's findings, an apply's and a clone's 422. Nothing stores
// YAML.
func TestYAMLSpecs(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	h.create("stage", nil)
	h.putSecret("stage", "token", "s3cret")
	// JSON is YAML; the comment makes it no JSON.
	yamlSpec := "# stage, as YAML\n" + stageSpec

	pj, err := h.svc.Plan(ctx, projectA, "stage", PlanRequest{Spec: json.RawMessage(stageSpec)}, spec.FormatJSON)
	if err != nil || !pj.Valid {
		t.Fatalf("JSON plan %+v %v", pj, err)
	}
	py, err := h.svc.Plan(ctx, projectA, "stage", PlanRequest{Spec: []byte(yamlSpec)}, spec.FormatYAML)
	if err != nil || !reflect.DeepEqual(py, pj) {
		t.Fatalf("YAML plan %+v %v\nwant %+v", py, err, pj)
	}
	ps, err := h.svc.PlanSpec(ctx, projectA, PlanSpecRequest{Spec: []byte(yamlSpec)}, spec.FormatYAML)
	if err != nil || !ps.Valid {
		t.Fatalf("YAML plan without an environment %+v %v", ps, err)
	}

	// The apply stores the normalised JSON: the same spec in JSON is
	// unchanged.
	a, err := h.svc.Apply(ctx, projectA, "stage", ApplyRequest{Spec: []byte(yamlSpec), Author: "tester"}, spec.FormatYAML)
	if err != nil || a.Number != 1 || a.Unchanged {
		t.Fatalf("YAML apply %+v %v", a, err)
	}
	stored, err := h.st.GetDeployment(ctx, h.env("stage").ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseStored(stored.Spec); err != nil || !json.Valid(stored.Spec) {
		t.Fatalf("stored %s %v", stored.Spec, err)
	}
	again, err := h.svc.Apply(ctx, projectA, "stage", ApplyRequest{Spec: json.RawMessage(stageSpec), Base: 1}, spec.FormatJSON)
	if err != nil || !again.Unchanged {
		t.Fatalf("JSON apply after the YAML one %+v %v", again, err)
	}

	// Findings at the JSON paths.
	const unknown = "services:\n  api:\n    image: ghcr.io/acme/api:1\n    replica: 1\n"
	p, err := h.svc.Plan(ctx, projectA, "stage", PlanRequest{Spec: []byte(unknown)}, spec.FormatYAML)
	if err != nil || p.Valid || p.Current != 1 || len(p.Diff) != 0 ||
		!hasFinding(p.Findings, spec.SeverityError, "services.api.replica", `unknown field "replica"`) {
		t.Fatalf("unknown field %+v %v", p, err)
	}
	p, err = h.svc.Plan(ctx, projectA, "stage", PlanRequest{Spec: []byte("services: [a\n")}, spec.FormatYAML)
	if err != nil || p.Valid || !hasFinding(p.Findings, spec.SeverityError, "", "not valid YAML") {
		t.Fatalf("syntax %+v %v", p, err)
	}
	ps, err = h.svc.PlanSpec(ctx, projectA, PlanSpecRequest{Spec: []byte("b: &b {image: a}\nservices:\n  x:\n    <<: *b\n")},
		spec.FormatYAML)
	if err != nil || ps.Valid || !hasFinding(ps.Findings, spec.SeverityError, "services.x.<<", "merge keys") {
		t.Fatalf("merge key %+v %v", ps, err)
	}
	_, err = h.svc.Apply(ctx, projectA, "stage", ApplyRequest{Spec: []byte(unknown), Base: 1}, spec.FormatYAML)
	var ise *InvalidSpecError
	if !errors.As(err, &ise) || !hasFinding(ise.Findings, spec.SeverityError, "services.api.replica", "unknown field") {
		t.Fatalf("apply %v", err)
	}

	// A clone takes a YAML spec; one that does not read is refused, and
	// an empty one clones the source's deployment.
	st, err := h.svc.Clone(ctx, projectA, "stage", CloneRequest{Name: "copy", Spec: []byte(yamlSpec),
		Secrets: map[string]CloneSecret{"token": {Copy: true}}}, spec.FormatYAML)
	if err != nil || st.Deployment != 1 {
		t.Fatalf("YAML clone %+v %v", st, err)
	}
	_, err = h.svc.Clone(ctx, projectA, "stage", CloneRequest{Name: "broken", Spec: []byte("services: !x {}\n")},
		spec.FormatYAML)
	if !errors.As(err, &ise) || !hasFinding(ise.Findings, spec.SeverityError, "services", "tags are not read") ||
		!hasFinding(ise.Findings, spec.SeverityError, "spec", "cannot be read") {
		t.Fatalf("unreadable YAML clone %v", err)
	}
	_, err = h.svc.Clone(ctx, projectA, "stage", CloneRequest{Name: "plain"}, spec.FormatYAML)
	if !errors.As(err, &ise) || !hasFinding(ise.Findings, spec.SeverityError, "services.api.env.TOKEN", "no secret token") {
		t.Fatalf("empty YAML clone %v", err)
	}
	for _, f := range ise.Findings {
		if f.Path == "services.api.env.TOKEN" &&
			f.Remedy != `a YAML clone copies no secret: clone with JSON and pass secrets: {"token": {"copy": true}} or a value` {
			t.Fatalf("empty YAML clone: remedy %q", f.Remedy)
		}
	}
}
