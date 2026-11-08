// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"
	"time"

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
