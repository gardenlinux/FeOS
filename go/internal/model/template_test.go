// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The template of the contract's §2.3, as the controller stores it.
const exampleTemplate = `{"command":["/app/expense-service"],` +
	`"env":{"DATABASE_URL":{"parts":[{"value":"postgres://expense_svc:"},` +
	`{"secret":"addon.postgres.role.expense_svc.password","version":1,"escape":"url"},` +
	`{"value":"@[fd6b::5]:5432/expensa?sslmode=disable"}]},` +
	`"PGDATA":{"value":"/var/lib/postgresql/data/pgdata"},` +
	`"POSTGRES_PASSWORD":{"secret":"addon.postgres.password","version":1}},` +
	`"volumes":[{"name":"data","volumeId":"0193a000-0000-7000-8000-000000000001"}],` +
	`"mounts":[{"volume":"data","path":"/var/lib/postgresql/data"}],` +
	`"securityGroups":["env-0193a000-0000-7000-8000-0000000000e1"],` +
	`"labels":{"ferun.environment":"0193a000-0000-7000-8000-0000000000e1","ferun.object":"addon/postgres"}}`

func TestTemplateJSONRoundTrip(t *testing.T) {
	var tpl PodTemplate
	if err := json.Unmarshal([]byte(exampleTemplate), &tpl); err != nil {
		t.Fatal(err)
	}
	want := PodTemplate{
		Command: []string{"/app/expense-service"},
		Env: map[string]TemplateValue{
			"DATABASE_URL": {Parts: []TemplatePart{
				{Value: "postgres://expense_svc:"},
				{Secret: "addon.postgres.role.expense_svc.password", Version: 1, Escape: EscapeURL},
				{Value: "@[fd6b::5]:5432/expensa?sslmode=disable"},
			}},
			"PGDATA":            Literal("/var/lib/postgresql/data/pgdata"),
			"POSTGRES_PASSWORD": SecretValue("addon.postgres.password", 1, EscapeNone),
		},
		Volumes:        []TemplateVolume{{Name: "data", VolumeID: uuid.MustParse("0193a000-0000-7000-8000-000000000001")}},
		Mounts:         []TemplateMount{{Volume: "data", Path: "/var/lib/postgresql/data"}},
		SecurityGroups: []string{EnvGroupID(uuid.MustParse("0193a000-0000-7000-8000-0000000000e1"))},
		Labels: map[string]string{LabelEnvironment: "0193a000-0000-7000-8000-0000000000e1",
			LabelObject: "addon/postgres"},
	}
	if !reflect.DeepEqual(tpl, want) {
		t.Fatalf("decoded\n%+v\nwant\n%+v", tpl, want)
	}
	b, err := json.Marshal(tpl)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != exampleTemplate {
		t.Fatalf("encoded\n%s\nwant\n%s", b, exampleTemplate)
	}
	if tpl.HasCredential() {
		t.Fatal("a stored template has no credential")
	}
}

func TestTemplateValueForms(t *testing.T) {
	good := map[string]TemplateValue{
		`{"value":""}`:                              {},
		`{"value":"x"}`:                             Literal("x"),
		`{"secret":"s","version":2}`:                SecretValue("s", 2, EscapeNone),
		`{"secret":"s","version":2,"escape":"url"}`: SecretValue("s", 2, EscapeURL),
		`{"parts":[]}`:                              {Parts: []TemplatePart{}},
		`{"parts":[{"credential":"password","escape":"url"},{"value":"@h"}]}`: {Parts: []TemplatePart{
			{Credential: "password", Escape: EscapeURL}, {Value: "@h"}}},
	}
	for in, want := range good {
		var v TemplateValue
		if err := json.Unmarshal([]byte(in), &v); err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if !reflect.DeepEqual(v, want) {
			t.Errorf("%s: decoded %+v, want %+v", in, v, want)
		}
		b, _ := json.Marshal(v)
		if string(b) != in {
			t.Errorf("%s: encoded %s", in, b)
		}
	}
	if !CredentialValue("password").HasCredential() {
		t.Error("CredentialValue has a credential")
	}
	bad := []string{
		`"x"`, `{}`, `{"value":1}`, `{"value":"x","secret":"s","version":1}`, `{"secret":"s"}`,
		`{"secret":"","version":1}`, `{"secret":"s","version":1,"escape":"html"}`, `{"value":"x","other":1}`,
		`{"credential":"password"}`, `{"parts":[{"credential":""}]}`, `{"parts":[{"value":"x","secret":"s","version":1}]}`,
		`{"parts":[{"unknown":"x"}]}`, `null`,
	}
	for _, in := range bad {
		var v TemplateValue
		if err := json.Unmarshal([]byte(in), &v); err == nil {
			t.Errorf("%s: decoded as %+v, want an error", in, v)
		}
	}
}

func TestTemplateHash(t *testing.T) {
	var tpl PodTemplate
	if err := json.Unmarshal([]byte(exampleTemplate), &tpl); err != nil {
		t.Fatal(err)
	}
	const image = "docker.io/library/postgres@sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
	h := tpl.Hash(image, 1, 1024)
	if len(h) != 64 || strings.ToLower(h) != h {
		t.Fatalf("hash %q: want 64 lowercase hex digits", h)
	}
	if again := tpl.Hash(image, 1, 1024); again != h {
		t.Fatalf("hash not deterministic: %s, then %s", h, again)
	}

	// Labels are not in it, nor the order of the security groups, nor
	// whether an empty list is nil.
	same := tpl
	same.Labels = map[string]string{LabelTemplate: h, "other": "x"}
	same.SecurityGroups = []string{"sbx-b", "env-a"}
	tpl2 := tpl
	tpl2.SecurityGroups = []string{"env-a", "sbx-b"}
	if same.Hash(image, 1, 1024) != tpl2.Hash(image, 1, 1024) {
		t.Fatal("the security groups' order or the labels change the hash")
	}
	if (PodTemplate{}).Hash(image, 1, 512) != (PodTemplate{Command: []string{}, Env: map[string]TemplateValue{},
		SecurityGroups: []string{}}).Hash(image, 1, 512) {
		t.Fatal("nil and empty hash apart")
	}

	// Everything the pod runs is.
	changes := map[string]func(*PodTemplate) (string, int, int){
		"image":   func(*PodTemplate) (string, int, int) { return image + "0", 1, 1024 },
		"vcpus":   func(*PodTemplate) (string, int, int) { return image, 2, 1024 },
		"memory":  func(*PodTemplate) (string, int, int) { return image, 1, 2048 },
		"command": func(p *PodTemplate) (string, int, int) { p.Command = []string{"postgres"}; return image, 1, 1024 },
		"secret version": func(p *PodTemplate) (string, int, int) {
			p.Env = map[string]TemplateValue{"POSTGRES_PASSWORD": SecretValue("addon.postgres.password", 2, EscapeNone)}
			return image, 1, 1024
		},
		"volume": func(p *PodTemplate) (string, int, int) {
			p.Volumes = []TemplateVolume{{Name: "data", VolumeID: uuid.MustParse("0193a000-0000-7000-8000-000000000002")}}
			return image, 1, 1024
		},
		"mount": func(p *PodTemplate) (string, int, int) {
			p.Mounts = []TemplateMount{{Volume: "data", Path: "/var/lib/postgresql/data", ReadOnly: true}}
			return image, 1, 1024
		},
		"groups": func(p *PodTemplate) (string, int, int) { p.SecurityGroups = nil; return image, 1, 1024 },
	}
	for name, change := range changes {
		c := tpl
		c.Env = map[string]TemplateValue{}
		for k, v := range tpl.Env {
			c.Env[k] = v
		}
		img, cpu, mem := change(&c)
		if c.Hash(img, cpu, mem) == h {
			t.Errorf("a change of the %s leaves the hash", name)
		}
	}
}

// The hashes of templates without the fields added later (name tables,
// the ingress group flag) stay what they were: the instances of a deployment
// made before keep their pods. With the ingress group flag the hash
// differs, so that the first publish of an environment rolls its image
// services once.
func TestTemplateHashStable(t *testing.T) {
	var tpl PodTemplate
	if err := json.Unmarshal([]byte(exampleTemplate), &tpl); err != nil {
		t.Fatal(err)
	}
	const image = "docker.io/library/postgres@sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
	tables := PodTemplate{SecurityGroups: []string{"env-a"}, NameTables: []string{"env-a"}}
	golden := []struct {
		name string
		tpl  PodTemplate
		mem  int
		want string
	}{
		{"example", tpl, 1024, "322394c9a5fbe15d627b25b2b141a47e40de19615825747a6796c9bac2c3b665"},
		{"empty", PodTemplate{}, 512, "45b7088156a0299c9c5c443699bc4cc5bf44bd2b2cba80f5ab93031413564642"},
		{"name tables", tables, 512, "ffddb7b72ce9d7b3605a5d29c256f17e20d0a96b871dd94082da83f757f8a91d"},
	}
	for _, g := range golden {
		h := g.tpl.Hash(image, 1, g.mem)
		if h != g.want {
			t.Errorf("%s: hash %s, want %s", g.name, h, g.want)
		}
		flagged := g.tpl
		flagged.IngressGroup = true
		if flagged.Hash(image, 1, g.mem) == h {
			t.Errorf("%s: the ingress group flag leaves the hash", g.name)
		}
	}
	// The flag round-trips through the stored JSON, and is absent when
	// not set.
	b, err := json.Marshal(PodTemplate{IngressGroup: true})
	if err != nil || string(b) != `{"ingressGroup":true}` {
		t.Fatalf("encoded %s %v", b, err)
	}
	var back PodTemplate
	if err := json.Unmarshal(b, &back); err != nil || !back.IngressGroup {
		t.Fatalf("decoded %+v %v", back, err)
	}
	if b, _ := json.Marshal(PodTemplate{}); string(b) != `{}` {
		t.Fatalf("encoded %s", b)
	}
}

func TestGroupIDs(t *testing.T) {
	id := uuid.MustParse("0193a000-0000-7000-8000-0000000000e1")
	if g := EnvGroupID(id); g != "env-0193a000-0000-7000-8000-0000000000e1" || len(g) != 40 {
		t.Fatalf("EnvGroupID %q", g)
	}
	// The workload groups (ADR 0034, revised): the migration 0033 computes
	// the same in SQL.
	wl := WorkloadGroupID(id, "service", "api")
	sum := sha256.Sum256([]byte("0193a000-0000-7000-8000-0000000000e1/service/api"))
	if wl != "wl-"+hex.EncodeToString(sum[:])[:32] || len(wl) != 35 {
		t.Fatalf("WorkloadGroupID %q", wl)
	}
	if wl == WorkloadGroupID(id, "job", "api") || wl != WorkloadGroupID(id, "service", "api") {
		t.Fatal("WorkloadGroupID is not one per object")
	}
	if got := SandboxWorkloadGroupID("k3m9q7a2b4c"); got != "wl-s-k3m9q7a2b4c" {
		t.Fatalf("SandboxWorkloadGroupID %q", got)
	}
	if got := JobWorkloadGroupID("k3m9q7a2b4c"); got != "wl-j-k3m9q7a2b4c" {
		t.Fatalf("JobWorkloadGroupID %q", got)
	}
	if got := PodGroups(id, wl); !slices.Equal(got, []string{EnvGroupID(id), wl}) {
		t.Fatalf("PodGroups %v", got)
	}
	// A sandbox's pod: its groups and its environment's table, never an
	// own table (ADR 0035), its egress's names.
	tmpl := SandboxTemplate(id, "k3m9q7a2b4c", true, []string{"api.stripe.com"})
	if !slices.Equal(tmpl.Command, SandboxCommand) ||
		!slices.Equal(tmpl.SecurityGroups, PodGroups(id, "wl-s-k3m9q7a2b4c")) ||
		!slices.Equal(tmpl.NameTables, []string{EnvTableID(id)}) || !tmpl.RestrictEgressNames ||
		!slices.Equal(tmpl.EgressDomains, []string{"api.stripe.com"}) {
		t.Fatalf("SandboxTemplate %+v", tmpl)
	}
	tmpl.Command[0] = "x"
	if SandboxCommand[0] != "sleep" {
		t.Fatal("SandboxTemplate shares its command")
	}
}
