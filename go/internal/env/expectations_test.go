// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/env/spec"
)

// The plan checks what a spec says its code expects of an HTTP secret
// against the environment's secret (ADR 0058), its forwarded host too; a
// normalised deployment keeps the expectation, and a clone carries it to
// the copy, whose secret is checked against it.
func TestSecretExpectationsPlanned(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.withCredentials()
	ctx := h.ctx()
	h.create("prod", nil)
	if _, err := h.svc.PutSecret(ctx, projectA, "prod", "s4", PutSecretRequest{Type: SecretHTTP,
		Upstream: "https://203.0.113.40", ForwardedHost: true, Author: "tester",
		Credential: json.RawMessage(`{"kind":"headers","headers":{"APIKey":"key-1"}}`)}); err != nil {
		t.Fatal(err)
	}
	withPin := func(pin string) string {
		return `{"services": {"shop": {"image": "ghcr.io/acme/shop:1", "env": {"S4_URL": {"secret": "s4", "key": "url"}}}},
		  "secrets": {"s4": ` + pin + `}}`
	}

	p, err := h.svc.Plan(ctx, projectA, "prod", PlanRequest{Spec: json.RawMessage(withPin(
		`{"mode": "name", "forwardedHost": false}`))}, spec.FormatJSON)
	if err != nil || p.Valid || !hasFinding(p.Findings, spec.SeverityError, "secrets.s4.forwardedHost",
		"HTTP secret s4 (version 1) forwards the host") {
		t.Fatalf("another forwarded host: %+v %v", p.Findings, err)
	}
	a := h.apply("prod", withPin(`{"type": "http", "mode": "name", "forwardedHost": true}`), 0)
	sp, err := h.svc.deploymentSpec(ctx, h.env("prod"), a.Number)
	if err != nil {
		t.Fatal(err)
	}
	if ref := sp.Secrets["s4"]; ref.Version != 1 || ref.Mode != "name" || ref.ForwardedHost == nil || !*ref.ForwardedHost {
		t.Fatalf("the stored pin %+v", ref)
	}

	// The spec's code expects the other mode.
	p, err = h.svc.Plan(ctx, projectA, "prod", PlanRequest{Spec: json.RawMessage(withPin(`{"mode": "intercept"}`))},
		spec.FormatJSON)
	if err != nil || p.Valid || !hasFinding(p.Findings, spec.SeverityError, "secrets.s4.mode",
		"is called in the name mode; the spec's code expects the intercept mode") {
		t.Fatalf("another mode: %+v %v", p.Findings, err)
	}

	// A new version that does not forward the host: the same spec is
	// refused, as it pins the latest.
	h.putHTTPSecret("prod", "s4", "key-2", "")
	expected := withPin(`{"type": "http", "mode": "name", "forwardedHost": true}`)
	p, err = h.svc.Plan(ctx, projectA, "prod", PlanRequest{Spec: json.RawMessage(expected)}, spec.FormatJSON)
	if err != nil || p.Valid || !hasFinding(p.Findings, spec.SeverityError, "secrets.s4.forwardedHost",
		"(version 2) does not forward the host; the spec's code expects one that forwards the host") {
		t.Fatalf("another forwarded host: %+v %v", p.Findings, err)
	}
	// A revert to deployment 1 is checked against its expectation, and
	// holds: it pins version 1.
	if _, err := h.svc.Revert(ctx, projectA, "prod", 1, RevertRequest{Base: a.Number}); err != nil {
		t.Fatalf("revert: %s", describe(err))
	}

	// A clone copies the latest version, which does not forward the
	// host: the copy's secret is not what the code expects.
	_, err = h.svc.Clone(ctx, projectA, "prod", CloneRequest{Name: "copy", Deployment: &a.Number,
		Secrets: map[string]CloneSecret{"s4": {Copy: true}}}, spec.FormatJSON)
	if f, ok := findingAt(err, "secrets.s4.forwardedHost"); !ok || !strings.Contains(f.Problem, "(version 1) does not forward") {
		t.Fatalf("clone: %s", describe(err))
	}
	// Not copied, the missing secret's finding says what to make.
	_, err = h.svc.Clone(ctx, projectA, "prod", CloneRequest{Name: "copy", Deployment: &a.Number,
		Spec: json.RawMessage(withPin(`{"mode": "name", "forwardedHost": true}`))}, spec.FormatJSON)
	if f, ok := findingAt(err, "services.shop.env.S4_URL"); !ok || !strings.Contains(f.Remedy, `secrets: {"s4": {"copy": true}}`) {
		t.Fatalf("clone without the secret: %s", describe(err))
	}
}
