// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"strings"
	"testing"
)

// deployment7 is the fixtures' normalised stage deployment 7.
func deployment7(t *testing.T) Spec {
	t.Helper()
	return mustParse(t, string(readFile(t, "testdata/expensa-stage-deployment7.json")))
}

// deployment6 is deployment 7 with expense-service, web and migrate at 1.3.
func deployment6(t *testing.T) Spec {
	t.Helper()
	s := deployment7(t)
	svc(&s, "expense-service", func(v *Service) {
		v.Image, v.ImageAsWritten = "ghcr.io/acme/expense@"+digestExpense13, "ghcr.io/acme/expense:1.3"
	})
	svc(&s, "web", func(v *Service) {
		v.Image, v.ImageAsWritten = "ghcr.io/acme/expensa-web@"+digestWeb13, "ghcr.io/acme/expensa-web:1.3"
	})
	j := s.Jobs["migrate"]
	j.Image, j.ImageAsWritten = "ghcr.io/acme/expense@"+digestExpense13, "ghcr.io/acme/expense:1.3"
	s.Jobs["migrate"] = j
	return s
}

func diffJSON(t *testing.T, d []DiffEntry) string {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The diff of deployments 6 and 7 is the fixtures', byte for byte; a revert
// is its mirror.
func TestDiffFixture(t *testing.T) {
	r6, r7 := deployment6(t), deployment7(t)
	want := strings.TrimSpace(string(readFile(t, "testdata/expensa-stage-diff-6-7.json")))
	if got := diffJSON(t, Diff(&r6, &r7)); got != want {
		t.Fatalf("diff\n%s\nwant\n%s", got, want)
	}
	back := Diff(&r7, &r6)
	if len(back) != 3 || back[0].Fields[0].To != "ghcr.io/acme/expense:1.3 ("+digestExpense13+")" ||
		back[0].Effect != "replaces 2 pods" || back[2].Effect != EffectRestarts {
		t.Fatalf("revert diff %s", diffJSON(t, back))
	}
	if d := Diff(&r7, &r7); len(d) != 0 || diffJSON(t, d) != "[]" {
		t.Fatalf("no change: %s", diffJSON(t, d))
	}
}

type diffCase struct {
	name   string
	change func(s *Spec)
	want   string
}

func TestDiffEffects(t *testing.T) {
	cases := []diffCase{
		{"replicas", func(s *Spec) { svc(s, "expense-service", func(v *Service) { v.Replicas = intp(3) }) },
			`[{"kind":"service","name":"expense-service","change":"update","fields":[{"path":"replicas","from":2,"to":3}],"effect":"in place"}]`},
		{"readiness and idle", func(s *Spec) {
			svc(s, "web", func(v *Service) { v.Readiness.Period = "5s"; v.Idle = "15m0s" })
		}, `[{"kind":"service","name":"web","change":"update","fields":[{"path":"idle","from":null,"to":"15m0s"},` +
			`{"path":"readiness.period","from":"2s","to":"5s"}],"effect":"in place"}]`},
		{"command", func(s *Spec) { svc(s, "expense-service", func(v *Service) { v.Command = []string{"/x"} }) },
			`[{"kind":"service","name":"expense-service","change":"update","fields":[{"path":"command","from":["/app/expense-service"],"to":["/x"]}],"effect":"replaces 2 pods"}]`},
		{"env and resources", func(s *Spec) {
			svc(s, "web", func(v *Service) {
				v.Env = map[string]EnvValue{"A": String("1")}
				v.Resources = &Resources{VCPUs: 2, MemoryMib: 512}
			})
		}, `[{"kind":"service","name":"web","change":"update","fields":[{"path":"env.A","from":null,"to":"1"},` +
			`{"path":"resources.vcpus","from":1,"to":2}],"effect":"replaces 1 pod"}]`},
		{"secret version", func(s *Spec) { s.Secrets["internal-token"] = SecretRef{Version: 4} },
			`[{"kind":"service","name":"approval-service","change":"update","fields":[{"path":"env.APPROVAL_INTERNAL_TOKEN",` +
				`"from":{"secret":"internal-token","version":3},"to":{"secret":"internal-token","version":4}}],"effect":"replaces 1 pod"},` +
				`{"kind":"service","name":"expense-service","change":"update","fields":[{"path":"env.EXPENSA_INTERNAL_TOKEN",` +
				`"from":{"secret":"internal-token","version":3},"to":{"secret":"internal-token","version":4}}],"effect":"replaces 2 pods"},` +
				`{"kind":"secret","name":"internal-token","change":"update","fields":[{"path":"version","from":3,"to":4}],"effect":"in place"}]`},
		{"referenced port", func(s *Spec) {
			svc(s, "approval-service", func(v *Service) { v.Ports = []Port{{Name: "http", Port: 9090}} })
			r := s.Routes["root"]
			r.Strip = true
			s.Routes["root"] = r
		}, `[{"kind":"service","name":"approval-service","change":"update","fields":[{"path":"ports","from":[{"name":"http","port":8080}],` +
			`"to":[{"name":"http","port":9090}]}],"effect":"in place"},{"kind":"service","name":"expense-service","change":"update",` +
			`"fields":[{"path":"env.EXPENSA_APPROVAL_URL","from":{"key":"url","port":8080,"service":"approval-service"},` +
			`"to":{"key":"url","port":9090,"service":"approval-service"}}],"effect":"replaces 2 pods"},` +
			`{"kind":"route","name":"root","change":"update","fields":[{"path":"strip","from":null,"to":true}],"effect":"in place"}]`},
		{"runtime", func(s *Spec) {
			s.Services["web"] = Service{Runtime: RuntimeAttached, Sandbox: webSandbox, Command: []string{"npm", "run", "dev"},
				Ports: []Port{{Name: "http", Port: 8080}}}
		}, ""},
		{"job template and schedule", func(s *Spec) {
			j := s.Jobs["migrate"]
			j.Retries, j.On = 3, JobOnManual
			s.Jobs["migrate"] = j
		}, `[{"kind":"job","name":"migrate","change":"update","fields":[{"path":"on","from":"deploy","to":"manual"},` +
			`{"path":"retries","from":1,"to":3}],"effect":"in place"}]`},
		{"job env", func(s *Spec) {
			j := s.Jobs["migrate"]
			j.Env = map[string]EnvValue{"X": SecretEnv("internal-token")}
			s.Jobs["migrate"] = j
		}, `[{"kind":"job","name":"migrate","change":"update","fields":[{"path":"env.EXPENSA_DATABASE_URL","from":{"addon":"postgres",` +
			`"database":"expensa","key":"url","role":"expense_svc"},"to":null},{"path":"env.X","from":null,"to":{"secret":"internal-token",` +
			`"version":3}}],"effect":"restarts"}]`},
		{"addon version", func(s *Spec) {
			a := s.Addons["postgres"]
			a.Version = "18"
			s.Addons["postgres"] = a
		}, `[{"kind":"addon","name":"postgres","change":"update","fields":[{"path":"version","from":"17","to":"18"}],"effect":"recreates (downtime)"}]`},
		{"addon plan", func(s *Spec) {
			a := s.Addons["nats"]
			a.Plan = "dev"
			s.Addons["nats"] = a
		}, `[{"kind":"addon","name":"nats","change":"update","fields":[{"path":"plan","from":"small","to":"dev"}],"effect":"recreates (downtime)"}]`},
		{"addon options", func(s *Spec) {
			s.Addons["postgres"] = Addon{Type: "postgres", Version: "17", Plan: "small",
				Options: map[string]any{"databases": []any{"expensa", "audit"}, "roles": []any{"expense_svc", "approval_svc"}}}
		}, `[{"kind":"addon","name":"postgres","change":"update","fields":[{"path":"options.databases","from":["expensa"],` +
			`"to":["expensa","audit"]}],"effect":"recreates (downtime)"}]`},
		{"addon type", func(s *Spec) { s.Addons["nats"] = Addon{Type: "valkey", Version: "8", Plan: "small"} },
			`[{"kind":"addon","name":"nats","change":"update","fields":[{"path":"options.jetstream","from":true,"to":null},` +
				`{"path":"type","from":"nats","to":"valkey"},{"path":"version","from":"2","to":"8"}],"effect":"deletes data"}]`},
		{"adds and removes", func(s *Spec) {
			delete(s.Addons, "nats")
			delete(s.Routes, "api-approval")
			s.Services["extra"] = Service{Runtime: RuntimeImage, Image: "img"}
			s.Jobs["seed"] = Job{Image: "img"}
			s.Secrets["other"] = SecretRef{Version: 1}
		}, `[{"kind":"service","name":"extra","change":"add","fields":[],"effect":"in place"},` +
			`{"kind":"job","name":"seed","change":"add","fields":[],"effect":"in place"},` +
			`{"kind":"addon","name":"nats","change":"remove","fields":[],"effect":"deletes data"},` +
			`{"kind":"route","name":"api-approval","change":"remove","fields":[],"effect":"in place"},` +
			`{"kind":"secret","name":"other","change":"add","fields":[],"effect":"in place"}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prev, next := deployment7(t), deployment7(t)
			c.change(&next)
			got := Diff(&prev, &next)
			if c.want == "" {
				if len(got) != 1 || got[0].Effect != EffectRestarts || got[0].Kind != KindService {
					t.Fatalf("diff %s", diffJSON(t, got))
				}
				return
			}
			if g := diffJSON(t, got); g != c.want {
				t.Fatalf("diff\n%s\nwant\n%s", g, c.want)
			}
		})
	}
}

func TestDiffAttachedAndAddonEffect(t *testing.T) {
	att := func(cmd string) Spec {
		return Spec{Services: map[string]Service{"web": {Runtime: RuntimeAttached, Sandbox: webSandbox,
			Command: []string{cmd}, Ports: []Port{{Name: "http", Port: 5173}}}}}
	}
	a, b := att("a"), att("b")
	if d := Diff(&a, &b); len(d) != 1 || d[0].Effect != EffectRestarts {
		t.Fatalf("%s", diffJSON(t, d))
	}
	c := att("a")
	svc(&c, "web", func(v *Service) { v.Ports = []Port{{Name: "http", Port: 5174}} })
	if d := Diff(&a, &c); len(d) != 1 || d[0].Effect != EffectInPlace {
		t.Fatalf("%s", diffJSON(t, d))
	}

	// The blueprint tells an options change apart.
	prev, next := deployment7(t), deployment7(t)
	next.Addons["postgres"] = Addon{Type: "postgres", Version: "17", Plan: "small",
		Options: map[string]any{"databases": []any{"expensa"}, "roles": []any{"expense_svc"}}}
	var asked string
	d := DiffWith(&prev, &next, func(name string, p, n Addon) string {
		asked = name
		return EffectReprovisions
	})
	if asked != "postgres" || len(d) != 1 || d[0].Effect != EffectReprovisions {
		t.Fatalf("%s", diffJSON(t, d))
	}
	d = DiffWith(&prev, &next, func(string, Addon, Addon) string { return "" })
	if d[0].Effect != EffectRecreates {
		t.Fatalf("%s", diffJSON(t, d))
	}

	// Without a previous deployment every object is an add; without a next
	// one a remove.
	r7 := deployment7(t)
	adds := Diff(nil, &r7)
	if len(adds) != 3+1+2+3+1 {
		t.Fatalf("%s", diffJSON(t, adds))
	}
	for _, e := range adds {
		if e.Change != ChangeAdd || len(e.Fields) != 0 {
			t.Fatalf("%+v", e)
		}
	}
	removes := Diff(&r7, nil)
	for _, e := range removes {
		if e.Change != ChangeRemove {
			t.Fatalf("%+v", e)
		}
	}
	if diffJSON(t, Diff(nil, nil)) != "[]" {
		t.Fatal("empty diff")
	}

	// An image as written (a digest reference) shows as it is.
	p := Spec{Services: map[string]Service{"x": {Runtime: RuntimeImage, Image: "a@sha256:" + strings.Repeat("1", 64)}}}
	n := Spec{Services: map[string]Service{"x": {Runtime: RuntimeImage, Image: "a@sha256:" + strings.Repeat("2", 64)}}}
	if d := Diff(&p, &n); d[0].Fields[0].From != "a@sha256:"+strings.Repeat("1", 64) {
		t.Fatalf("%s", diffJSON(t, d))
	}
}

// A deploy job's on is the same in either word: two versions in a rolling
// deploy store different ones (JobOnStored), and neither may take the
// other's spec for a change (ADR 0018, "The deployments' word").
func TestJobOnWords(t *testing.T) {
	prev, next := deployment7(t), deployment7(t)
	j := prev.Jobs["migrate"]
	j.On = JobOnRelease
	prev.Jobs = map[string]Job{"migrate": j}
	j.On = JobOnDeploy
	next.Jobs = map[string]Job{"migrate": j}
	if !EqualNormalised(prev, next) {
		t.Fatal("on release and on deploy differ")
	}
	if d := Diff(&prev, &next); len(d) != 0 {
		t.Fatalf("diff %+v", d)
	}
	j.On = JobOnManual
	next.Jobs = map[string]Job{"migrate": j}
	if EqualNormalised(prev, next) {
		t.Fatal("on release and on manual are equal")
	}
	if prev.Jobs["migrate"].On != JobOnRelease {
		t.Fatal("the comparison changed its spec")
	}
}
