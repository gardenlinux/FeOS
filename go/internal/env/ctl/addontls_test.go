// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	"github.com/gardenlinux/feos/go/internal/env/envdeps"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/store"
)

// tlsSpec is a postgres addon with a service and a job that reference its
// roles (ADR 0038); job names the job's second role, "" none.
func tlsSpec(jobRole string) string {
	job := ""
	if jobRole != "" {
		job = fmt.Sprintf(`,"jobs":{"migrate":{"image":%q,"command":["/app","migrate"],"on":"manual","env":{
			"A":{"addon":"postgres","key":"url","role":"svc","database":"app"},
			"B":{"addon":"postgres","key":"host","role":%q}}}}`, testImage, jobRole)
	}
	return fmt.Sprintf(`{"addons":{"postgres":{"type":"postgres","version":"17","plan":"dev",
		"options":{"databases":["app"],"roles":["svc","ops"]}}},
		"services":{"web":{"image":%q,"command":["/app"],"ports":[{"name":"http","port":8080}],
			"env":{"DB":{"addon":"postgres","key":"url","role":"svc"}}}}%s}`, testImage, job)
}

// An addon that admits workloads by their certificates (ADR 0038): its
// tls step gets a server certificate for the addon's ID, its pod and its
// workload, for the key of its credential, under the platform's root; the
// map names each referencing workload by the ID its sets carry, with the
// role its reference names; a renewal and a removed reference run the
// step again; a role's password of before is deleted.
func TestAddonCertificate(t *testing.T) {
	h := newHarness(t, options{timings: func(t *timings) { t.addonCertTTL = 8 * time.Second }})
	n1 := h.addNode("n1")
	execs := &execLog{}
	n1.Script().SetExec(execs.fn)
	env := h.newEnv("stage")
	// What an addon of before has: a role's password.
	old := "addon.postgres.role.svc.password"
	sealed, err := h.sealer.Seal([]byte("0123456789abcdef0123456789abcdef"), envdeps.SecretAAD(env.ID, old, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.InsertSecret(bg, store.EnvSecret{EnvID: env.ID, Name: old, Version: 1, Sealed: sealed,
		System: true, CreatedBy: "ferund"}); err != nil {
		t.Fatal(err)
	}
	r1 := h.apply(env.ID, tlsSpec("ops"))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	row := h.waitRows(env.ID, spec.KindAddon, "postgres", 1, 10*time.Second, "ready", ready)[0]

	steps := execs.step("tls")
	if len(steps) == 0 {
		t.Fatal("the tls step never ran")
	}
	st := steps[len(steps)-1]
	leaf := certOf(t, st.Env["FERUN_TLS_CERT"])
	wantID := pki.WorkloadID(testTD, env.Owner, env.ID, spec.KindAddon, "postgres")
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != wantID || len(leaf.DNSNames) != 0 || len(leaf.IPAddresses) != 0 {
		t.Errorf("names: URIs %v, DNS %v, IP %v; want %s alone", leaf.URIs, leaf.DNSNames, leaf.IPAddresses, wantID)
	}
	wantSubject := "CN=" + testTD + "/" + row.Sandbox.ID.String() + ",OU=" + model.WorkloadGroupID(env.ID, spec.KindAddon, "postgres")
	if leaf.Subject.String() != wantSubject {
		t.Errorf("subject %q, want %q", leaf.Subject, wantSubject)
	}
	key, err := h.c.ResolveSecret(bg, env.ID, "addon.postgres.tls.key", 1)
	if err != nil || st.Env["FERUN_TLS_KEY"] != key {
		t.Fatalf("the step's key is not the credential: %v", err)
	}
	pub, err := publicOf(key)
	if err != nil {
		t.Fatal(err)
	}
	if !pub.(interface{ Equal(crypto.PublicKey) bool }).Equal(leaf.PublicKey) {
		t.Error("the certificate is not for the credential's key")
	}
	inter := x509.NewCertPool()
	inter.AddCert(h.iss.Certificate())
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: h.iss.Bundle(), Intermediates: inter,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Errorf("verify: %v", err)
	}
	if st.Env["FERUN_TLS_CA"] != string(h.iss.TrustPEM()) {
		t.Error("the step trusts something else than the platform's roots and issuer")
	}
	web := model.WorkloadGroupID(env.ID, spec.KindService, "web")
	migrate := model.WorkloadGroupID(env.ID, spec.KindJob, "migrate")
	ident := st.Env["FERUN_PG_IDENT"]
	for _, want := range []string{
		"# " + pki.WorkloadID(testTD, env.Owner, env.ID, spec.KindService, "web") + "\n",
		`feid  "/^CN=[^,]+,OU=` + web + `$"  svc` + "\n",
		`feid  "/^CN=[^,]+,OU=` + migrate + `$"  svc` + "\n",
		`feid  "/^CN=[^,]+,OU=` + migrate + `$"  ops` + "\n",
	} {
		if !strings.Contains(ident, want) {
			t.Errorf("pg_ident.conf has not %q:\n%s", want, ident)
		}
	}
	if strings.Count(ident, "\nfeid ") != 3 {
		t.Errorf("pg_ident.conf has other grants:\n%s", ident)
	}
	eventually(t, 10*time.Second, "the role's password of before deleted", func() bool {
		_, err := h.st.GetSecret(bg, env.ID, old, 1)
		return err != nil
	})

	// The renewal (three quarters of 8 s) runs the step again with a new
	// certificate, for the same key.
	eventually(t, 20*time.Second, "a renewed certificate", func() bool {
		s := execs.step("tls")
		last := s[len(s)-1]
		return last.Env["FERUN_TLS_CERT"] != st.Env["FERUN_TLS_CERT"] && last.Env["FERUN_TLS_KEY"] == key
	})

	// A removed reference takes its grant along.
	r2 := h.apply(env.ID, tlsSpec(""))
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 60*time.Second)
	eventually(t, 10*time.Second, "the job's grants gone", func() bool {
		s := execs.step("tls")
		id := s[len(s)-1].Env["FERUN_PG_IDENT"]
		return !strings.Contains(id, migrate) && strings.Contains(id, web)
	})
}

// certOf is the first certificate of a PEM chain.
func certOf(t *testing.T, chain string) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode([]byte(chain))
	if b == nil {
		t.Fatalf("no PEM in %q", chain)
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A service that references postgres addons' roles reaches each through
// its pod's TLS binding (ADR 0036): its template binds one loopback
// address per addon, numbered in the order of their names from
// 127.64.0.1, however many references and roles name one, to the addon by
// its name with ALPN postgresql; its env names the same address with the
// role and no password.
func TestAReferenceToARoleBindsTheAddon(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"addons":{
		"postgres":{"type":"postgres","version":"17","plan":"dev","options":{"databases":["app"],"roles":["svc","ops"]}},
		"archive":{"type":"postgres","version":"17","plan":"dev","options":{"databases":["old"],"roles":["svc"]}}},
		"services":{"web":{"image":%q,"command":["/app"],"ports":[{"name":"http","port":8080}],"env":{
			"DB":{"addon":"postgres","key":"url","role":"svc"},
			"OPS":{"addon":"postgres","key":"url","role":"ops"},
			"HOST":{"addon":"postgres","key":"host","role":"svc"},
			"ARCHIVE":{"addon":"archive","key":"url","role":"svc"}}}}}`, testImage))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	row := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0]
	tmpl := row.Sandbox.PodTemplate
	if tmpl == nil {
		t.Fatal("no template")
	}
	want := []model.TemplateTLSBinding{
		{Listen: "127.64.0.1:5432", Host: "archive", Port: 5432, ALPN: "postgresql", Addon: "archive"},
		{Listen: "127.64.0.2:5432", Host: "postgres", Port: 5432, ALPN: "postgresql", Addon: "postgres"},
	}
	if !slices.Equal(tmpl.TLSBindings, want) {
		t.Fatalf("TLS bindings %+v, want %+v", tmpl.TLSBindings, want)
	}
	for k, v := range map[string]string{
		"ARCHIVE": "postgres://svc@127.64.0.1:5432/old?sslmode=disable",
		"DB":      "postgres://svc@127.64.0.2:5432/app?sslmode=disable",
		"OPS":     "postgres://ops@127.64.0.2:5432/app?sslmode=disable",
		"HOST":    "127.64.0.2",
	} {
		if got := tmpl.Env[k]; len(got.Parts) != 1 || got.Parts[0].Value != v {
			t.Errorf("%s = %+v, want %s", k, got, v)
		}
	}
}

// kinoSpec is kino's shape (ADR 0039): a postgres and a valkey addon,
// services that reference both, and a job that references valkey;
// withJob false leaves the job out.
func kinoSpec(withJob bool) string {
	job := ""
	if withJob {
		job = fmt.Sprintf(`,"jobs":{"warm":{"image":%q,"command":["/app","warm"],"on":"manual","env":{
			"CACHE":{"addon":"valkey","key":"host"}}}}`, testImage)
	}
	return fmt.Sprintf(`{"addons":{
		"postgres":{"type":"postgres","version":"17","plan":"dev","options":{"databases":["kino"],"roles":["catalog"]}},
		"valkey":{"type":"valkey","version":"8","plan":"dev"}},
		"services":{
			"catalog":{"image":%q,"command":["/app"],"ports":[{"name":"http","port":8080}],"env":{
				"DATABASE_URL":{"addon":"postgres","key":"url","role":"catalog","database":"kino"},
				"VALKEY_URL":{"addon":"valkey","key":"url"},
				"VALKEY_HOST":{"addon":"valkey","key":"host"},
				"VALKEY_PORT":{"addon":"valkey","key":"port"}}},
			"notify":{"image":%q,"command":["/app"],"env":{"VALKEY_URL":{"addon":"valkey","key":"url"}}}}%s}`,
		testImage, testImage, job)
}

// A service that references valkey reaches it through its pod's TLS
// binding (ADR 0039), as postgres: one loopback address per addon,
// numbered by name, valkey's with no ALPN; its values name the loopback
// address, no user and no password.
func TestAReferenceToValkeyBindsTheAddon(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, kinoSpec(false))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	for svc, want := range map[string][]model.TemplateTLSBinding{
		"catalog": {
			{Listen: "127.64.0.1:5432", Host: "postgres", Port: 5432, ALPN: "postgresql", Addon: "postgres"},
			{Listen: "127.64.0.2:6379", Host: "valkey", Port: 6379, Addon: "valkey"},
		},
		// Alone in its pod, valkey has the first address.
		"notify": {{Listen: "127.64.0.1:6379", Host: "valkey", Port: 6379, Addon: "valkey"}},
	} {
		row := h.waitRows(env.ID, spec.KindService, svc, 1, 10*time.Second, "ready", ready)[0]
		tmpl := row.Sandbox.PodTemplate
		if tmpl == nil || !slices.Equal(tmpl.TLSBindings, want) {
			t.Fatalf("%s's TLS bindings %+v, want %+v", svc, tmpl, want)
		}
	}
	tmpl := h.waitRows(env.ID, spec.KindService, "catalog", 1, 10*time.Second, "ready", ready)[0].Sandbox.PodTemplate
	for k, v := range map[string]string{
		"DATABASE_URL": "postgres://catalog@127.64.0.1:5432/kino?sslmode=disable",
		"VALKEY_URL":   "redis://127.64.0.2:6379/0",
		"VALKEY_HOST":  "127.64.0.2",
		"VALKEY_PORT":  "6379",
	} {
		if got := tmpl.Env[k]; len(got.Parts) != 1 || got.Parts[0].Value != v {
			t.Errorf("%s = %+v, want %s", k, got, v)
		}
	}
}

// Valkey admits workloads by their certificates (ADR 0039): its tls step
// gets a certificate of the addon's ID that is a client's too, the
// addon's ID as its administrator, and a user per referencing workload,
// named by its SPIFFE ID; a removed reference takes its user along; the
// password of before is deleted once no instance names it.
func TestValkeyCertificateAndUsers(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	execs := &execLog{}
	n1.Script().SetExec(execs.fn)
	env := h.newEnv("stage")
	// What a valkey of before has: the default user's password.
	old := "addon.valkey.password"
	sealed, err := h.sealer.Seal([]byte("0123456789abcdef0123456789abcdef"), envdeps.SecretAAD(env.ID, old, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.InsertSecret(bg, store.EnvSecret{EnvID: env.ID, Name: old, Version: 1, Sealed: sealed,
		System: true, CreatedBy: "ferund"}); err != nil {
		t.Fatal(err)
	}
	r1 := h.apply(env.ID, kinoSpec(true))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	row := h.waitRows(env.ID, spec.KindAddon, "valkey", 1, 10*time.Second, "ready", ready)[0]
	if row.Sandbox.Image != "valkey/valkey:9-alpine" || len(row.Sandbox.PodTemplate.Env) != 0 {
		t.Fatalf("the addon runs %s with env %v", row.Sandbox.Image, row.Sandbox.PodTemplate.Env)
	}

	tlsSteps := func() []*containerv1.ExecRequest {
		var out []*containerv1.ExecRequest
		for _, s := range execs.step("tls") {
			if s.Env["FERUN_VALKEY_ID"] != "" {
				out = append(out, s)
			}
		}
		return out
	}
	var st *containerv1.ExecRequest
	eventually(t, 10*time.Second, "valkey's tls step", func() bool {
		s := tlsSteps()
		if len(s) == 0 {
			return false
		}
		st = s[len(s)-1]
		return true
	})
	self := pki.WorkloadID(testTD, env.Owner, env.ID, spec.KindAddon, "valkey")
	if st.Env["FERUN_VALKEY_ID"] != self {
		t.Errorf("the step's ID %q, want %s", st.Env["FERUN_VALKEY_ID"], self)
	}
	leaf := certOf(t, st.Env["FERUN_TLS_CERT"])
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != self || len(leaf.DNSNames) != 0 || len(leaf.IPAddresses) != 0 {
		t.Errorf("names: URIs %v, DNS %v, IP %v; want %s alone", leaf.URIs, leaf.DNSNames, leaf.IPAddresses, self)
	}
	inter := x509.NewCertPool()
	inter.AddCert(h.iss.Certificate())
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: h.iss.Bundle(), Intermediates: inter,
			KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
			t.Errorf("verify for %v: %v", usage, err)
		}
	}
	key, err := h.c.ResolveSecret(bg, env.ID, "addon.valkey.tls.key", 1)
	if err != nil || st.Env["FERUN_TLS_KEY"] != key {
		t.Fatalf("the step's key is not the credential: %v", err)
	}
	if st.Env["FERUN_TLS_CA"] != string(h.iss.TrustPEM()) {
		t.Error("the step trusts something else than the platform's roots and issuer")
	}
	catalog := pki.WorkloadID(testTD, env.Owner, env.ID, spec.KindService, "catalog")
	notify := pki.WorkloadID(testTD, env.Owner, env.ID, spec.KindService, "notify")
	warm := pki.WorkloadID(testTD, env.Owner, env.ID, spec.KindJob, "warm")
	acl := st.Env["FERUN_VALKEY_ACL"]
	head := "user default off resetpass resetkeys resetchannels -@all\nuser " + self + " on resetpass ~* &* +@all\n"
	if !strings.HasPrefix(acl, head) || strings.Count(acl, "\n") != 5 || strings.Contains(acl, "nopass") {
		t.Errorf("users:\n%s", acl)
	}
	for _, u := range []string{catalog, notify, warm} {
		if !strings.Contains(acl, "\nuser "+u+" on resetpass ~* &* +@all -@admin -migrate\n") {
			t.Errorf("no user %s:\n%s", u, acl)
		}
	}
	eventually(t, 10*time.Second, "the password of before deleted", func() bool {
		_, err := h.st.GetSecret(bg, env.ID, old, 1)
		return err != nil
	})

	// A removed reference takes its user along.
	r2 := h.apply(env.ID, kinoSpec(false))
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 60*time.Second)
	eventually(t, 10*time.Second, "the job's user gone", func() bool {
		s := tlsSteps()
		acl := s[len(s)-1].Env["FERUN_VALKEY_ACL"]
		return !strings.Contains(acl, warm) && strings.Contains(acl, catalog) && strings.Contains(acl, notify)
	})
}
