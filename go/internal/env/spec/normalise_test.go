// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The digests of the fixtures' images.
const (
	digestApproval12 = "sha256:11cdf88d1c2c3802016199fb7e7e2a5377ed51b81855411f3f5840fe9d703e93"
	digestExpense13  = "sha256:2780e52e5ffc077629f9b26f1469ccc2b0bc37d1778d4038d4a9fc587ff89ab5"
	digestExpense14  = "sha256:863e2ebe9c3820e6d1b20ef70fdb2bebe152f5f0fbbddb7c7e5f5144c438658a"
	digestWeb13      = "sha256:171e8d334efeb5b7d6961f1f2ff14eee166a978b3fa0f3d116c66526e1eda9d2"
	digestWeb14      = "sha256:de2feb13b0b780dd0dfd7aa32bcd02b306a4680f4958f896b3347f3ba3521a81"
)

// stagePins pin the fixtures' stage spec as deployment 7 was.
func stagePins() Pins {
	return Pins{
		Images: map[string]string{
			"ghcr.io/acme/approval:1.2":    "ghcr.io/acme/approval@" + digestApproval12,
			"ghcr.io/acme/expense:1.4":     "ghcr.io/acme/expense@" + digestExpense14,
			"ghcr.io/acme/expensa-web:1.4": "ghcr.io/acme/expensa-web@" + digestWeb14,
		},
		Secrets: map[string]int{"internal-token": 3},
		Plans:   map[string]string{"postgres": "small", "nats": "small", "valkey": "small"},
	}
}

func marshalSpec(t *testing.T, s Spec) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The fixtures' request normalises to the fixtures' deployment 7, byte for
// byte; a normalised spec normalises to itself.
func TestNormaliseFixture(t *testing.T) {
	s := mustParse(t, string(readFile(t, "testdata/expensa-stage-request.json")))
	got, fs := Normalise(s, stagePins())
	if len(fs) != 0 {
		t.Fatalf("findings %+v", fs)
	}
	want := canonical(t, readFile(t, "testdata/expensa-stage-deployment7.json"))
	if g := canonical(t, []byte(marshalSpec(t, got))); g != want {
		t.Fatalf("normalised\n%s\nwant\n%s", g, want)
	}
	again, fs := Normalise(got, Pins{})
	if len(fs) != 0 || !EqualNormalised(again, got) {
		t.Fatalf("not idempotent: %+v\n%s", fs, marshalSpec(t, again))
	}
	// The input is left as it was.
	if s.Services["web"].Replicas != nil || s.Services["expense-service"].Image != "ghcr.io/acme/expense:1.4" {
		t.Fatalf("input changed: %+v", s.Services)
	}
}

func TestNormaliseRules(t *testing.T) {
	s := mustParse(t, expensa)
	s.Addons["cache"] = Addon{Type: "valkey", Version: "8"}
	s.Secrets["unused"] = SecretRef{Version: 2}
	s.Secrets["also-unused"] = SecretRef{}
	s.Services["digest"] = Service{Image: "docker.io/library/nginx@sha256:" + strings.Repeat("b", 64),
		ImageAsWritten: "nginx:1.27"}
	s.Services["unpinned"] = Service{Image: "ghcr.io/acme/unresolved:1"}
	s.Services["tuned"] = Service{Image: "ghcr.io/acme/approval:1.2", Replicas: intp(0),
		Resources: &Resources{MemoryMib: 1024}, Idle: "1h30m", Rollout: &Rollout{Timeout: "90s"},
		Readiness: &Readiness{TCP: &TCPCheck{Port: PortRef{Number: 80}}, Period: "5000ms", FailureThreshold: 5}}
	s.Services["attached"] = Service{Runtime: RuntimeAttached, Sandbox: webSandbox, Command: []string{"x"},
		Replicas: intp(1), Readiness: &Readiness{Exec: &ExecCheck{Command: []string{"true"}}}, Idle: "900s"}
	s.Services["attached-never"] = Service{Runtime: RuntimeAttached, Sandbox: webSandbox, Command: []string{"x"}, Idle: IdleNever}
	s.Jobs["seed"] = Job{Image: "ghcr.io/acme/expense:1.4", On: JobOnManual, Timeout: "1h"}
	s.Jobs["deploy"] = Job{Image: "ghcr.io/acme/expense:1.4", On: JobOnDeploy}
	s.Jobs["release"] = Job{Image: "ghcr.io/acme/expense:1.4", On: JobOnRelease}
	s.Volumes = []byte(`{}`)
	pins := stagePins()
	got, fs := Normalise(s, pins)
	checkFindings(t, fs, []expect{i("secrets.also-unused", "not referenced: dropped"), i("secrets.unused", "dropped")})

	exp := got.Services["expense-service"]
	if exp.Image != "ghcr.io/acme/expense@"+digestExpense14 || exp.ImageAsWritten != "ghcr.io/acme/expense:1.4" ||
		*exp.Replicas != 2 || exp.Idle != "15m0s" || exp.Rollout.Timeout != "10m0s" || exp.Readiness.Period != "2s" ||
		exp.Readiness.InitialDelay != "0s" || exp.Readiness.FailureThreshold != 3 || exp.Runtime != RuntimeImage {
		t.Fatalf("expense-service %+v", exp)
	}
	if d := got.Services["digest"]; d.Image != s.Services["digest"].Image || d.ImageAsWritten != "nginx:1.27" {
		t.Fatalf("a digest reference changed: %+v", d)
	}
	if u := got.Services["unpinned"]; u.Image != "ghcr.io/acme/unresolved:1" || u.ImageAsWritten != "" ||
		*u.Replicas != 1 || *u.Resources != (Resources{VCPUs: 1, MemoryMib: 512}) {
		t.Fatalf("unpinned %+v", u)
	}
	tuned := got.Services["tuned"]
	if *tuned.Replicas != 0 || *tuned.Resources != (Resources{VCPUs: 1, MemoryMib: 1024}) || tuned.Idle != "1h30m0s" ||
		tuned.Rollout.Timeout != "1m30s" || tuned.Readiness.Period != "5s" || tuned.Readiness.FailureThreshold != 5 {
		t.Fatalf("tuned %+v", tuned)
	}
	att := got.Services["attached"]
	if att.Replicas != nil || att.Resources != nil || att.Rollout != nil || att.Readiness.Period != "2s" || att.Idle != "15m0s" {
		t.Fatalf("attached %+v", att)
	}
	if n := got.Services["attached-never"]; n.Idle != IdleNever {
		t.Fatalf("attached never %+v", n)
	}
	if web := got.Services["web"]; web.Runtime != RuntimeAttached || web.Replicas != nil {
		t.Fatalf("web %+v", web)
	}
	if j := got.Jobs["migrate"]; j.Timeout != "10m0s" || j.On != JobOnStored || j.Retries != 1 ||
		j.Image != "ghcr.io/acme/expense@"+digestExpense14 {
		t.Fatalf("migrate %+v", j)
	}
	// Both words of a deploy job, and none, normalise to the stored one.
	for _, name := range []string{"deploy", "release"} {
		if j := got.Jobs[name]; j.On != JobOnStored {
			t.Fatalf("%s: on %q, want %q", name, j.On, JobOnStored)
		}
	}
	if j := got.Jobs["seed"]; j.Timeout != "1h0m0s" || j.On != JobOnManual || *j.Resources != (Resources{VCPUs: 1, MemoryMib: 512}) {
		t.Fatalf("seed %+v", j)
	}
	if got.Addons["cache"].Plan != "small" || got.Addons["postgres"].Plan != "small" {
		t.Fatalf("addons %+v", got.Addons)
	}
	if !reflect.DeepEqual(got.Secrets, map[string]SecretRef{"internal-token": {Version: 3}}) || got.Volumes != nil {
		t.Fatalf("secrets %+v volumes %s", got.Secrets, got.Volumes)
	}

	// A secret without a version is pinned to the latest; one with
	// keeps it.
	s = mustParse(t, expensa)
	delete(s.Secrets, "internal-token")
	got, _ = Normalise(s, Pins{Secrets: map[string]int{"internal-token": 5}})
	if got.Secrets["internal-token"].Version != 5 {
		t.Fatalf("pinned %+v", got.Secrets)
	}
	// One nothing pins is left out (Validate refused it).
	got, _ = Normalise(s, Pins{})
	if got.Secrets != nil {
		t.Fatalf("pinned %+v", got.Secrets)
	}
	// An invalid spec normalises without a panic.
	bad := Spec{Services: map[string]Service{"x": {Idle: "soon", Readiness: &Readiness{Period: "x"}}},
		Jobs: map[string]Job{"j": {Timeout: "never"}}}
	got, _ = Normalise(bad, Pins{})
	if got.Services["x"].Idle != "soon" || got.Jobs["j"].Timeout != "never" {
		t.Fatalf("%+v", got)
	}
	if empty, fs := Normalise(Spec{}, Pins{}); fs != nil || marshalSpec(t, empty) != "{}" {
		t.Fatalf("empty %+v %v", empty, fs)
	}
}

// A spec with a plan's pins written in (image the pin, imageAsWritten the
// image as written; ADR 0032) normalises to what the plan normalised,
// without pins: what the plan showed is what an apply runs, even once a
// tag moved.
func TestNormaliseWrittenPins(t *testing.T) {
	const tag, pinned = "ghcr.io/acme/expense:1.4", "ghcr.io/acme/expense@" + digestExpense14
	draft := Spec{
		Services: map[string]Service{"api": {Image: tag}, "web": {Image: "nginx:1.27"}},
		Jobs:     map[string]Job{"migrate": {Image: tag}},
	}
	planned, _ := Normalise(draft, Pins{Images: map[string]string{tag: pinned, "nginx:1.27": "docker.io/library/nginx@" +
		digestWeb13}})
	// The client writes every pin into its draft.
	written := Spec{Services: map[string]Service{}, Jobs: map[string]Job{}}
	for name, svc := range planned.Services {
		written.Services[name] = Service{Image: svc.Image, ImageAsWritten: svc.ImageAsWritten}
	}
	written.Jobs["migrate"] = Job{Image: planned.Jobs["migrate"].Image, ImageAsWritten: tag}
	// The tag moved since: the apply's pins name another digest.
	moved := Pins{Images: map[string]string{tag: "ghcr.io/acme/expense@" + digestExpense13}}
	applied, _ := Normalise(written, moved)
	if !EqualNormalised(applied, planned) || Hash(applied) != Hash(planned) {
		t.Fatalf("applied %s\nplanned %s", marshalSpec(t, applied), marshalSpec(t, planned))
	}
	if api := applied.Services["api"]; api.Image != pinned || api.ImageAsWritten != tag {
		t.Fatalf("api %+v", api)
	}
	// "Check for a newer image" writes the image as written back: the
	// tag resolves again, to where it moved.
	back := written
	back.Services = map[string]Service{"api": {Image: tag, ImageAsWritten: tag}}
	if api, _ := Normalise(back, moved); api.Services["api"].Image != "ghcr.io/acme/expense@"+digestExpense13 ||
		api.Services["api"].ImageAsWritten != tag {
		t.Fatalf("checked %+v", api.Services["api"])
	}
	// One that does not resolve keeps no stale image as written.
	stale := Spec{Services: map[string]Service{"api": {Image: "ghcr.io/acme/expense:1.5", ImageAsWritten: tag}}}
	if got, _ := Normalise(stale, Pins{}); got.Services["api"].Image != "ghcr.io/acme/expense:1.5" ||
		got.Services["api"].ImageAsWritten != "" {
		t.Fatalf("unresolved %+v", got.Services["api"])
	}
}

func TestMergePatch(t *testing.T) {
	// RFC 7396, appendix A.
	cases := []struct{ doc, patch, want string }{
		{`{"a":"b"}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":"b"}`, `{"b":"c"}`, `{"a":"b","b":"c"}`},
		{`{"a":"b"}`, `{"a":null}`, `{}`},
		{`{"a":"b","b":"c"}`, `{"a":null}`, `{"b":"c"}`},
		{`{"a":["b"]}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":"c"}`, `{"a":["b"]}`, `{"a":["b"]}`},
		{`{"a":{"b":"c"}}`, `{"a":{"b":"d","c":null}}`, `{"a":{"b":"d"}}`},
		{`{"a":[{"b":"c"}]}`, `{"a":[1]}`, `{"a":[1]}`},
		{`["a","b"]`, `["c","d"]`, `["c","d"]`},
		{`{"a":"b"}`, `["c"]`, `["c"]`},
		{`{"a":"foo"}`, `null`, `null`},
		{`{"a":"foo"}`, `"bar"`, `"bar"`},
		{`{"e":null}`, `{"a":1}`, `{"a":1,"e":null}`},
		{`[1,2]`, `{"a":"b","c":null}`, `{"a":"b"}`},
		{`{}`, `{"a":{"bb":{"ccc":null}}}`, `{"a":{"bb":{}}}`},
		{``, `{"a":1}`, `{"a":1}`},
		{`{"n":12345678901234567890}`, `{"m":1.50}`, `{"m":1.50,"n":12345678901234567890}`},
	}
	for _, c := range cases {
		got, err := MergePatch([]byte(c.doc), []byte(c.patch))
		if err != nil || string(got) != c.want {
			t.Errorf("MergePatch(%s, %s) = %s, %v; want %s", c.doc, c.patch, got, err, c.want)
		}
	}
	for _, c := range [][2]string{{`{`, `{}`}, {`{}`, `{`}, {`{}`, `{} {}`}, {`{}`, ``}} {
		if _, err := MergePatch([]byte(c[0]), []byte(c[1])); err == nil {
			t.Errorf("MergePatch(%s, %s): no error", c[0], c[1])
		}
	}
}
