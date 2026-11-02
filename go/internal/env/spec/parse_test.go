// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// mustParse parses a spec that has no findings.
func mustParse(t *testing.T, raw string) Spec {
	t.Helper()
	s, fs := Parse([]byte(raw))
	if len(fs) > 0 {
		t.Fatalf("findings %+v", fs)
	}
	return s
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The Expensa spec of §3 and the fixtures' request read without a
// finding, into what a decoder reads.
func TestParseExpensa(t *testing.T) {
	for _, raw := range [][]byte{[]byte(expensa), readFile(t, "testdata/expensa-stage-request.json"),
		readFile(t, "testdata/expensa-stage-deployment7.json")} {
		s, fs := Parse(raw)
		if len(fs) > 0 {
			t.Fatalf("findings %+v", fs)
		}
		var want Spec
		strictUnmarshal(t, raw, &want)
		if !reflect.DeepEqual(s, want) {
			t.Fatalf("parsed\n%+v\nwant\n%+v", s, want)
		}
	}
}

// pathsOf are the paths of the error findings.
func pathsOf(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		if f.Severity == SeverityError {
			out = append(out, f.Path)
		}
	}
	return out
}

func TestParseFindings(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		paths []string
		// problem is a part of the first finding's problem.
		problem string
	}{
		{"empty", ``, []string{""}, "no spec"},
		{"blank", " \n ", []string{""}, "no spec"},
		{"not an object", `[1]`, []string{""}, "a list, not an object"},
		{"null", `null`, []string{""}, "null, not an object"},
		{"syntax", `{"services": {`, []string{""}, "not valid JSON"},
		{"bad token", `{"services": x}`, []string{""}, "not valid JSON"},
		{"trailing", `{} {}`, []string{""}, "data after its end"},
		{"trailing garbage", `{}x`, []string{""}, "data after its end"},
		{"unknown top", `{"unknown": {}}`, []string{"unknown"}, `unknown field "unknown"`},
		{"unknown service field", `{"services": {"x": {"image": "a", "replica": 2}}}`, []string{"services.x.replica"},
			`unknown field "replica"`},
		{"unknown deep", `{"services": {"x": {"image": "a", "readiness": {"http": {"port": "http", "path": "/", "prt": 1}}}}}`,
			[]string{"services.x.readiness.http.prt"}, "unknown field"},
		{"unknown in port", `{"services": {"x": {"ports": [{"name": "a", "port": 1}, {"name": "b", "port": 2, "proto": "tcp"}]}}}`,
			[]string{"services.x.ports[1].proto"}, "unknown field"},
		{"unknown job", `{"jobs": {"j": {"image": "a", "foo": 1}}}`, []string{"jobs.j.foo"}, "unknown field"},
		{"unknown addon", `{"addons": {"a": {"type": "postgres", "versio": "17"}}}`, []string{"addons.a.versio"}, "unknown"},
		{"unknown route", `{"routes": {"r": {"path": "/", "service": "x", "port": "http", "extra": true}}}`,
			[]string{"routes.r.extra"}, "unknown"},
		{"unknown secret", `{"secrets": {"s": {"ver": 1}}}`, []string{"secrets.s.ver"}, "unknown"},
		{"wrong string", `{"services": {"x": {"image": 3}}}`, []string{"services.x.image"}, "want a string, not a number"},
		{"wrong int", `{"services": {"x": {"replicas": "two"}}}`, []string{"services.x.replicas"}, "want a number, not a string"},
		{"fraction", `{"services": {"x": {"replicas": 1.5}}}`, []string{"services.x.replicas"}, "not a whole number"},
		{"huge", `{"services": {"x": {"replicas": 99999999999}}}`, []string{"services.x.replicas"}, "not a whole number"},
		{"wrong list", `{"services": {"x": {"command": "run"}}}`, []string{"services.x.command"}, "want a list"},
		{"wrong list item", `{"services": {"x": {"command": ["a", 1]}}}`, []string{"services.x.command[1]"}, "want a string"},
		{"wrong bool", `{"routes": {"r": {"path": "/", "service": "x", "port": 1, "strip": "yes"}}}`,
			[]string{"routes.r.strip"}, "want true or false"},
		{"wrong map", `{"services": []}`, []string{"services"}, "want an object, not a list"},
		{"wrong struct", `{"services": {"x": "image"}}`, []string{"services.x"}, "want an object, not a string"},
		{"null string", `{"services": {"x": {"image": null}}}`, []string{"services.x.image"}, "want a string, not null"},
		{"env number", `{"services": {"x": {"env": {"A": 3}}}}`, []string{"services.x.env.A"}, "env value is a string"},
		{"env two forms", `{"services": {"x": {"env": {"A": {"value": "a", "secret": "b"}}}}}`, []string{"services.x.env.A"},
			"want exactly one"},
		{"port ref bool", `{"routes": {"r": {"path": "/", "service": "x", "port": true}}}`, []string{"routes.r.port"},
			"want a name or a number"},
		{"duplicate top", `{"services": {}, "services": {}}`, []string{"services"}, `"services" is given twice`},
		{"duplicate nested", `{"services": {"x": {"image": "a", "image": "b"}}}`, []string{"services.x.image"}, "twice"},
		{"duplicate env", `{"services": {"x": {"env": {"A": "1", "A": "2"}}}}`, []string{"services.x.env.A"}, "twice"},
		{"duplicate in options", `{"addons": {"a": {"type": "t", "options": {"k": 1, "k": 2}}}}`,
			[]string{"addons.a.options.k"}, "twice"},
		{"several", `{"services": {"x": {"image": 1, "replica": 2}}, "jobs": {"j": {"on": 5}}, "nope": 1}`,
			[]string{"jobs.j.on", "nope", "services.x.image", "services.x.replica"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, fs := Parse([]byte(c.raw))
			if got := pathsOf(fs); !reflect.DeepEqual(got, c.paths) {
				t.Fatalf("paths %q, want %q; findings %+v", got, c.paths, fs)
			}
			if len(fs) != len(c.paths) {
				t.Fatalf("findings %+v", fs)
			}
			if !strings.Contains(fs[0].Problem, c.problem) {
				t.Fatalf("problem %q, want it to contain %q", fs[0].Problem, c.problem)
			}
			for _, f := range fs {
				if f.Severity != SeverityError || f.Remedy == "" {
					t.Fatalf("finding %+v", f)
				}
			}
		})
	}
}

func TestParseLimitsAndForms(t *testing.T) {
	big := `{"routes": {"r": {"path": "/` + strings.Repeat("a", MaxSpecBytes) + `"}}}`
	_, fs := Parse([]byte(big))
	if len(fs) != 1 || !strings.Contains(fs[0].Problem, "at most 1048576") {
		t.Fatalf("a spec past the limit: %+v", fs)
	}
	deep := `{"addons": {"a": {"type": "t", "options": {"x": ` + strings.Repeat("[", 100) + strings.Repeat("]", 100) + `}}}}`
	if _, fs := Parse([]byte(deep)); len(fs) != 1 || !strings.Contains(fs[0].Problem, "nests deeper") {
		t.Fatalf("a deep spec: %+v", fs)
	}

	// A null is an absent list, map or object.
	s := mustParse(t, `{"services": {"x": {"image": "a", "replicas": null, "env": null, "command": null}}, "jobs": null}`)
	if x := s.Services["x"]; x.Replicas != nil || x.Env != nil || x.Command != nil || s.Jobs != nil {
		t.Fatalf("nulls read as %+v", s)
	}

	// The kinds that are not available yet are read for Validate.
	s = mustParse(t, `{"volumes": {"v": {}}, "services": {"x": {"image": "a", "mounts": [{"volume": "v"}]}}}`)
	if string(s.Volumes) != `{"v":{}}` || string(s.Services["x"].Mounts) != `[{"volume":"v"}]` {
		t.Fatalf("volumes %s, mounts %s", s.Volumes, s.Services["x"].Mounts)
	}

	// Options keep what a decoder reads, nested values and nulls too.
	s = mustParse(t, `{"addons": {"pg": {"type": "postgres", "options": {"roles": ["a", "b"], "n": 2, "x": null, "o": {"k": true}}}}}`)
	want := map[string]any{"roles": []any{"a", "b"}, "n": 2.0, "x": nil, "o": map[string]any{"k": true}}
	if got := s.Addons["pg"].Options; !reflect.DeepEqual(got, want) {
		t.Fatalf("options %#v", got)
	}

	// Port references and env values in their forms.
	s = mustParse(t, `{"services": {"x": {"image": "a", "env": {"A": "1", "B": {"value": "2"}, "C": {"secret": "s"},
		"D": {"addon": "pg", "key": "url", "role": "r"}, "E": {"service": "x", "port": 80, "key": "port"}}}},
		"routes": {"r": {"path": "/", "service": "x", "port": 80}}}`)
	env := s.Services["x"].Env
	if !reflect.DeepEqual(env["A"], String("1")) || env["B"].Kind != EnvLiteral || !reflect.DeepEqual(env["C"], SecretEnv("s")) ||
		env["D"].Fields["role"] != "r" || env["E"].Port.Number != 80 || s.Routes["r"].Port.Number != 80 {
		t.Fatalf("env %+v routes %+v", env, s.Routes)
	}
	// A value's own error text loses the repeated key.
	_, fs = Parse([]byte(`{"routes": {"r": {"path": "/", "service": "x", "port": ""}}}`))
	if len(fs) != 1 || fs[0].Problem != "empty name" {
		t.Fatalf("port finding %+v", fs)
	}
}

func TestSortFindings(t *testing.T) {
	fs := []Finding{{Path: "b", Problem: "1"}, {Path: "a"}, {Path: "b", Problem: "2"}, {Path: ""}}
	got := SortFindings(fs)
	var order []string
	for _, f := range got {
		order = append(order, f.Path+f.Problem)
	}
	if strings.Join(order, ",") != ",a,b1,b2" {
		t.Fatal(order)
	}
	if SortFindings(nil) != nil {
		t.Fatal("nil")
	}
}

func TestHash(t *testing.T) {
	a := mustParse(t, expensa)
	b := mustParse(t, expensa)
	if Hash(a) == "" || Hash(a) != Hash(b) || len(Hash(a)) != 64 {
		t.Fatal(Hash(a))
	}
	b.Routes["root"] = Route{Path: "/x", Service: "web", Port: PortRef{Name: "http"}}
	if Hash(a) == Hash(b) {
		t.Fatal("a change keeps the hash")
	}
	if _, err := json.Marshal(a); err != nil {
		t.Fatal(err)
	}
}
