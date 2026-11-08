// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// The spec of the contract's §3 (the Expensa example).
const expensa = `{
  "services": {
    "expense-service": {
      "runtime": "image",
      "image": "ghcr.io/acme/expense:1.4",
      "command": ["/app/expense-service"],
      "env": {
        "EXPENSA_HTTP_ADDR": "[::]:8080",
        "EXPENSA_DATABASE_URL": {"addon": "postgres", "key": "url", "role": "expense_svc", "database": "expensa"},
        "EXPENSA_NATS_URL": {"addon": "nats", "key": "url"},
        "EXPENSA_INTERNAL_TOKEN": {"secret": "internal-token"},
        "EXPENSA_APPROVAL_URL": {"service": "approval-service", "port": "http", "key": "url"}
      },
      "ports": [{"name": "http", "port": 8080}],
      "replicas": 2,
      "resources": {"vcpus": 1, "memoryMib": 512},
      "readiness": {"http": {"port": "http", "path": "/readyz"}},
      "idle": "15m",
      "rollout": {"timeout": "10m"}
    },
    "web": {
      "runtime": "attached",
      "sandbox": "0193a000-0000-7000-8000-00000000000a",
      "command": ["npm", "run", "dev", "--", "--host", "::", "--port", "5173"],
      "cwd": "/workspace/expensa/web",
      "env": {},
      "ports": [{"name": "http", "port": 5173}],
      "readiness": {"tcp": {"port": "http"}}
    }
  },
  "jobs": {
    "migrate": {"image": "ghcr.io/acme/expense:1.4", "command": ["/app/expense-service", "migrate"],
                "env": {}, "on": "release", "timeout": "10m", "retries": 1,
                "resources": {"vcpus": 1, "memoryMib": 512}}
  },
  "addons": {
    "postgres": {"type": "postgres", "version": "17", "plan": "small",
                 "options": {"databases": ["expensa"], "roles": ["expense_svc", "approval_svc"]}},
    "nats": {"type": "nats", "version": "2", "plan": "small", "options": {"jetstream": true}}
  },
  "routes": {
    "api-expense": {"path": "/api/expense", "service": "expense-service", "port": "http", "strip": false},
    "root": {"path": "/", "service": "web", "port": "http"}
  },
  "secrets": {"internal-token": {"version": 3}}
}`

func strictUnmarshal(t *testing.T, b []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatal(err)
	}
}

// canonical is b decoded and encoded again: keys sorted, no space.
func canonical(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestSpecRoundTrip(t *testing.T) {
	var s Spec
	strictUnmarshal(t, []byte(expensa), &s)

	svc := s.Services["expense-service"]
	if svc.Runtime != RuntimeImage || *svc.Replicas != 2 || svc.Resources.MemoryMib != 512 ||
		svc.Readiness.HTTP.Port != (PortRef{Name: "http"}) || svc.Idle != "15m" || svc.Rollout.Timeout != "10m" {
		t.Fatalf("expense-service decoded as %+v", svc)
	}
	db := svc.Env["EXPENSA_DATABASE_URL"]
	if db.Kind != EnvAddon || db.Addon != "postgres" || db.Key != "url" ||
		!reflect.DeepEqual(db.Fields, map[string]string{"role": "expense_svc", "database": "expensa"}) {
		t.Fatalf("addon reference decoded as %+v", db)
	}
	if v := svc.Env["EXPENSA_APPROVAL_URL"]; v.Kind != EnvService || v.Service != "approval-service" ||
		v.Port.Name != "http" || v.Key != ServiceKeyURL {
		t.Fatalf("service reference decoded as %+v", v)
	}
	if v := svc.Env["EXPENSA_INTERNAL_TOKEN"]; !reflect.DeepEqual(v, SecretEnv("internal-token")) {
		t.Fatalf("secret reference decoded as %+v", v)
	}
	if v := svc.Env["EXPENSA_HTTP_ADDR"]; !reflect.DeepEqual(v, String("[::]:8080")) {
		t.Fatalf("plain value decoded as %+v", v)
	}
	if web := s.Services["web"]; web.Runtime != RuntimeAttached || web.Cwd != "/workspace/expensa/web" ||
		web.Readiness.TCP.Port.Name != "http" {
		t.Fatalf("web decoded as %+v", web)
	}
	if j := s.Jobs["migrate"]; j.On != JobOnRelease || j.Retries != 1 || j.Timeout != "10m" {
		t.Fatalf("migrate decoded as %+v", j)
	}
	if a := s.Addons["nats"]; a.Type != "nats" || a.Options["jetstream"] != true {
		t.Fatalf("nats decoded as %+v", a)
	}
	if r := s.Routes["root"]; r.Path != "/" || r.Service != "web" || r.Port.Name != "http" {
		t.Fatalf("root decoded as %+v", r)
	}
	if s.Secrets["internal-token"].Version != 3 {
		t.Fatalf("secrets decoded as %+v", s.Secrets)
	}

	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	// What the example writes and the encoding leaves out alike: an empty
	// env and a false strip.
	want := canonical(t, []byte(expensa))
	for _, drop := range []string{`"env":{},`, `,"strip":false`} {
		want = string(bytes.ReplaceAll([]byte(want), []byte(drop), nil))
	}
	if got := canonical(t, out); got != want {
		t.Fatalf("encoded\n%s\nwant\n%s", got, want)
	}
	var again Spec
	strictUnmarshal(t, out, &again)
	if !EqualNormalised(s, again) {
		t.Fatal("a spec does not survive its own encoding")
	}
}

func TestSpecStrict(t *testing.T) {
	for _, in := range []string{
		`{"services":{"x":{"image":"a","replica":2}}}`,
		`{"services":{"x":{"env":{"A":3}}}}`,
		`{"services":{"x":{"env":{"A":{"value":"a","secret":"b"}}}}}`,
		`{"routes":{"r":{"path":"/","service":"x","port":true}}}`,
		`{"unknown":{}}`,
	} {
		var s Spec
		dec := json.NewDecoder(bytes.NewReader([]byte(in)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err == nil {
			t.Errorf("%s: decoded, want an error", in)
		}
	}
	// The kinds that are not available yet are read, to be refused with
	// their own finding.
	var s Spec
	strictUnmarshal(t, []byte(`{"volumes":{"v":{}},"services":{"x":{"mounts":[{"volume":"v"}]}}}`), &s)
	if s.Volumes == nil || s.Services["x"].Mounts == nil {
		t.Fatalf("volumes and mounts not kept: %+v", s)
	}
}

func TestEnvValueForms(t *testing.T) {
	good := map[string]EnvValue{
		`"x"`:                {Value: "x"},
		`""`:                 {},
		`{"value":"x"}`:      {Kind: EnvLiteral, Value: "x"},
		`{"secret":"token"}`: {Kind: EnvSecret, Secret: "token"},
		`{"addon":"pg","database":"d","key":"url","role":"r"}`: {Kind: EnvAddon, Addon: "pg", Key: "url",
			Fields: map[string]string{"role": "r", "database": "d"}},
		`{"addon":"pg"}`: {Kind: EnvAddon, Addon: "pg"},
		`{"service":"api","port":"http","key":"url"}`: {Kind: EnvService, Service: "api", Port: PortRef{Name: "http"}, Key: "url"},
		`{"service":"api","port":8080,"key":"port"}`:  {Kind: EnvService, Service: "api", Port: PortRef{Number: 8080}, Key: "port"},
		`{"service":"api","key":"host"}`:              {Kind: EnvService, Service: "api", Key: "host"},
	}
	for in, want := range good {
		var v EnvValue
		if err := json.Unmarshal([]byte(in), &v); err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if !reflect.DeepEqual(v, want) {
			t.Errorf("%s: decoded %+v, want %+v", in, v, want)
		}
		b, err := json.Marshal(v)
		if err != nil || string(b) != in {
			t.Errorf("%s: encoded %s, %v", in, b, err)
		}
	}
	bad := []string{
		`3`, `true`, `null`, `[]`, `{}`, `{"value":3}`, `{"value":null}`, `{"secret":"a","value":"b"}`,
		`{"addon":"pg","service":"x"}`, `{"addon":"pg","port":"http"}`, `{"addon":"pg","role":1}`,
		`{"service":"api","role":"r"}`, `{"service":"api","port":true}`, `{"secret":"a","version":1}`, `{"other":"x"}`,
	}
	for _, in := range bad {
		var v EnvValue
		if err := json.Unmarshal([]byte(in), &v); err == nil {
			t.Errorf("%s: decoded as %+v, want an error", in, v)
		}
	}
}

func TestPortRef(t *testing.T) {
	for in, want := range map[string]PortRef{`"http"`: {Name: "http"}, `8080`: {Number: 8080}} {
		var p PortRef
		if err := json.Unmarshal([]byte(in), &p); err != nil || p != want {
			t.Errorf("%s: %+v, %v", in, p, err)
		}
		if b, _ := json.Marshal(p); string(b) != in {
			t.Errorf("%s: encoded %s", in, b)
		}
	}
	for _, in := range []string{`""`, `80.5`, `null`, `true`, `{}`} {
		var p PortRef
		if err := json.Unmarshal([]byte(in), &p); err == nil {
			t.Errorf("%s: decoded as %+v", in, p)
		}
	}
	if (PortRef{Number: 80}).String() != "80" || (PortRef{Name: "http"}).String() != "http" || !(PortRef{}).IsZero() {
		t.Error("PortRef's String or IsZero")
	}
}

func TestFindingAndDiffJSON(t *testing.T) {
	f := Finding{Severity: SeverityError, Path: "services.web.ports[0].port", Problem: "port 0 is out of range",
		Remedy: "use 1..65535"}
	b, _ := json.Marshal([]Finding{f})
	const wantF = `[{"severity":"error","path":"services.web.ports[0].port","problem":"port 0 is out of range","remedy":"use 1..65535"}]`
	if string(b) != wantF {
		t.Fatalf("finding %s", b)
	}
	var back []Finding
	strictUnmarshal(t, b, &back)
	if !reflect.DeepEqual(back, []Finding{f}) || !HasErrors(back) || HasErrors([]Finding{{Severity: SeverityWarning}}) {
		t.Fatalf("finding decoded as %+v", back)
	}

	d := DiffEntry{Kind: KindService, Name: "expense-service", Change: ChangeUpdate,
		Fields: []FieldChange{{Path: "replicas", From: 1.0, To: 2.0}, {Path: "env.X", From: nil, To: "y"}},
		Effect: EffectReplaces(2)}
	b, _ = json.Marshal(d)
	const wantD = `{"kind":"service","name":"expense-service","change":"update","fields":[{"path":"replicas","from":1,"to":2},` +
		`{"path":"env.X","from":null,"to":"y"}],"effect":"replaces 2 pods"}`
	if string(b) != wantD {
		t.Fatalf("diff entry %s", b)
	}
	var dback DiffEntry
	strictUnmarshal(t, b, &dback)
	if !reflect.DeepEqual(dback, d) {
		t.Fatalf("diff entry decoded as %+v", dback)
	}
	if EffectReplaces(1) != "replaces 1 pod" {
		t.Fatal(EffectReplaces(1))
	}
}
