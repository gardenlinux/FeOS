// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	envs "github.com/gardenlinux/feos/go/internal/env"
	"github.com/gardenlinux/feos/go/internal/env/spec"
)

// Plan, plan without an environment, apply and clone take the spec as a
// YAML document, told by the content type, with the envelope's scalars
// in the query (ADR 0057); any other content type is the JSON envelope.
func TestSpecContentTypes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	tok := bearerOf(e.projectToken)
	const doc = "services:\n  web: {image: a}\n"
	two := 2
	cases := []struct {
		method, path, contentType string
		body                      string
		want                      envCall
		status                    int
	}{
		{"POST", "/api/v1/environments/plan", "application/yaml", doc,
			envCall{"PlanSpec", testProject, "", []any{envs.PlanSpecRequest{Spec: []byte(doc)}, spec.FormatYAML}}, 200},
		{"POST", "/api/v1/environments/stage/plan?base=2", "application/x-yaml", doc,
			envCall{"Plan", testProject, "stage", []any{envs.PlanRequest{Spec: []byte(doc), Base: &two}, spec.FormatYAML}}, 200},
		{"POST", "/api/v1/environments/stage/plan", "text/yaml; charset=utf-8", doc,
			envCall{"Plan", testProject, "stage", []any{envs.PlanRequest{Spec: []byte(doc)}, spec.FormatYAML}}, 200},
		{"POST", "/api/v1/environments/stage/deployments?base=7&author=ada&message=hi%20there", "Application/YAML", doc,
			envCall{"Apply", testProject, "stage", []any{envs.ApplyRequest{Spec: []byte(doc), Base: 7, Author: "ada",
				Message: "hi there"}, spec.FormatYAML}}, 201},
		{"POST", "/api/v1/environments/stage/clone?name=demo&id=0193a000-0000-7000-8000-0000000000e9&deployment=2&author=ada",
			"application/yaml", doc,
			envCall{"Clone", testProject, "stage", []any{envs.CloneRequest{Name: "demo", ID: "0193a000-0000-7000-8000-0000000000e9",
				Deployment: &two, Spec: []byte(doc), Author: "ada"}, spec.FormatYAML}}, 201},
		{"POST", "/api/v1/environments/stage/clone?name=demo", "application/yaml", "",
			envCall{"Clone", testProject, "stage", []any{envs.CloneRequest{Name: "demo", Spec: []byte{}}, spec.FormatYAML}}, 201},
		// JSON, whatever else the content type says.
		{"POST", "/api/v1/environments/stage/deployments?base=9", "application/json", `{"spec": {}, "base": 3}`,
			envCall{"Apply", testProject, "stage", []any{envs.ApplyRequest{Spec: json.RawMessage(`{}`), Base: 3}}}, 201},
		{"POST", "/api/v1/environments/stage/plan", "", `{"spec": {}}`,
			envCall{"Plan", testProject, "stage", []any{envs.PlanRequest{Spec: json.RawMessage(`{}`)}}}, 200},
		{"POST", "/api/v1/environments/plan", "text/plain", `{"spec": {}}`,
			envCall{"PlanSpec", testProject, "", []any{envs.PlanSpecRequest{Spec: json.RawMessage(`{}`)}}}, 200},
		{"POST", "/api/v1/environments/plan", "application/yaml-ish", `{"spec": {}}`,
			envCall{"PlanSpec", testProject, "", []any{envs.PlanSpecRequest{Spec: json.RawMessage(`{}`)}}}, 200},
	}
	for _, c := range cases {
		fe := &fakeEnvs{}
		e.srv.deps.Environments = fe
		h := http.Header{"Authorization": {tok}}
		if c.contentType != "" {
			h.Set("Content-Type", c.contentType)
		}
		w := e.serveWith(newServer(e.srv.cfg, e.srv.deps), request{method: c.method, path: c.path, body: c.body, header: h})
		if w.Code != c.status {
			t.Fatalf("%s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
		if got := fe.last(t); got.method != c.want.method || got.env != c.want.env || !reflect.DeepEqual(got.args, c.want.args) {
			t.Fatalf("%s %s (%s):\n%+v\nwant\n%+v", c.method, c.path, c.contentType, got, c.want)
		}
	}
	// A query that does not read is a 400 before the service is asked.
	for _, path := range []string{"/api/v1/environments/stage/plan?base=x", "/api/v1/environments/stage/deployments?base=x",
		"/api/v1/environments/stage/clone?name=demo&deployment=x"} {
		fe := &fakeEnvs{}
		e.srv.deps.Environments = fe
		w := e.serveWith(newServer(e.srv.cfg, e.srv.deps), request{method: "POST", path: path, body: doc,
			header: http.Header{"Authorization": {tok}, "Content-Type": {"application/yaml"}}})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "must be an integer") || len(fe.calls) != 0 {
			t.Fatalf("%s: %d %s %+v", path, w.Code, w.Body, fe.calls)
		}
	}
	// Other routes never take YAML.
	fe := &fakeEnvs{}
	e.srv.deps.Environments = fe
	w := e.serveWith(newServer(e.srv.cfg, e.srv.deps), request{method: "PUT", path: "/api/v1/environments/stage/public",
		body: "public: true\n", header: http.Header{"Authorization": {tok}, "Content-Type": {"application/yaml"}}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "must be a JSON object") || len(fe.calls) != 0 {
		t.Fatalf("PUT public in YAML: %d %s", w.Code, w.Body)
	}
}
