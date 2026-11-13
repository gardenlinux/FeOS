// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/store"
)

func noEnv(string) string { return "" }

// runCmd runs ferund with args and returns its exit code and output.
func runCmd(args []string, getenv func(string) string) (int, string, string) {
	var out, errOut strings.Builder
	code := run(args, getenv, &out, &errOut)
	return code, out.String(), errOut.String()
}

// init-ca makes the root and the issuer, which an instance loads; it
// refuses the root's directory as the issuer's and never overwrites.
func TestInitCA(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root, dir := filepath.Join(base, "root"), filepath.Join(base, "ca")
	code, out, errOut := runCmd([]string{"init-ca", "--root-dir", root, "--dir", dir, "--trust-domain", "feos.test",
		"--dns-domain", "example.test"}, noEnv)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "made the root in "+root) {
		t.Fatalf("output %q", out)
	}
	iss, err := pki.LoadIssuer(dir, "feos.test")
	if err != nil {
		t.Fatal(err)
	}
	if iss.ControlPlaneDomain() != "ferun.example.test" {
		t.Fatalf("control-plane domain %s", iss.ControlPlaneDomain())
	}
	if _, err := os.Stat(filepath.Join(dir, "root.key")); !os.IsNotExist(err) {
		t.Fatalf("root.key in the issuer's directory: %v", err)
	}
	code, _, errOut = runCmd([]string{"init-ca", "--root-dir", root, "--dir", dir, "--trust-domain", "feos.test",
		"--dns-domain", "example.test"}, noEnv)
	if code != 1 || errOut == "" {
		t.Fatalf("overwrite: exit %d %s", code, errOut)
	}
	other := filepath.Join(base, "same")
	code, _, _ = runCmd([]string{"init-ca", "--root-dir", other, "--dir", other, "--dns-domain", "example.test"}, noEnv)
	if code != 1 {
		t.Fatalf("root dir = dir: exit %d", code)
	}
	code, _, errOut = runCmd([]string{"init-ca", "--root-dir", root}, noEnv)
	if code != 2 || !strings.Contains(errOut, "--dns-domain is required") {
		t.Fatalf("usage: exit %d %s", code, errOut)
	}
}

// new-issuer makes a new issuer under init-ca's root, which an instance
// loads with the same pin; it never overwrites.
func TestNewIssuer(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root, dir, next := filepath.Join(base, "root"), filepath.Join(base, "ca"), filepath.Join(base, "next")
	args := []string{"--trust-domain", "feos.test", "--dns-domain", "example.test"}
	if code, _, errOut := runCmd(append([]string{"init-ca", "--root-dir", root, "--dir", dir}, args...), noEnv); code != 0 {
		t.Fatalf("init-ca: exit %d: %s", code, errOut)
	}
	code, out, errOut := runCmd(append([]string{"new-issuer", "--root-dir", root, "--dir", next}, args...), noEnv)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "made a new issuer under the root in "+root) {
		t.Fatalf("output %q", out)
	}
	old, err := pki.LoadIssuer(dir, "feos.test")
	if err != nil {
		t.Fatal(err)
	}
	iss, err := pki.LoadIssuer(next, "feos.test")
	if err != nil {
		t.Fatal(err)
	}
	if iss.JoinPin() != old.JoinPin() || iss.Certificate().Equal(old.Certificate()) {
		t.Fatal("the new issuer is not a new one under the same root")
	}
	if code, _, errOut := runCmd(append([]string{"new-issuer", "--root-dir", root, "--dir", next}, args...), noEnv); code != 1 || errOut == "" {
		t.Fatalf("overwrite: exit %d %s", code, errOut)
	}
	if code, _, errOut := runCmd([]string{"new-issuer", "--root-dir", root}, noEnv); code != 2 || !strings.Contains(errOut, "--dir is required") {
		t.Fatalf("usage: exit %d %s", code, errOut)
	}
}

// migrate applies the migrations to an empty database; join-token issues
// a token pinned to the issuer's root, with the trust domain of
// --trust-domain, straight into the database.
func TestMigrateAndJoinToken(t *testing.T) {
	t.Parallel()
	url := dbtest.NewDatabase(t)
	code, _, errOut := runCmd([]string{"migrate"}, func(k string) string {
		if k == "FERUN_DATABASE_URL" {
			return url
		}
		return ""
	})
	if code != 0 {
		t.Fatalf("migrate: exit %d %s", code, errOut)
	}
	ctx := context.Background()
	d, err := db.Open(ctx, url, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Every embedded migration is applied.
	files, err := os.ReadDir("../../internal/db/migrations")
	if err != nil {
		t.Fatal(err)
	}
	var versions int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&versions); err != nil || versions != len(files) {
		t.Fatalf("migrations %d of %d: %v", versions, len(files), err)
	}

	base := t.TempDir()
	dir := filepath.Join(base, "ca")
	if err := pki.InitCA(pki.InitCAOptions{RootDir: filepath.Join(base, "root"), Dir: dir, TrustDomain: "run.example.test",
		DNSDomains: []string{"example.test"}}); err != nil {
		t.Fatal(err)
	}
	iss, err := pki.LoadIssuer(dir, "run.example.test")
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCmd([]string{"join-token", "--database-url", url, "--ca-dir", dir, "--trust-domain",
		"run.example.test", "--node", "n7", "--ttl", "10m"}, noEnv)
	if code != 0 {
		t.Fatalf("join-token: exit %d %s", code, errOut)
	}
	token := strings.TrimSpace(out)
	sent, pin, err := pki.SplitJoinToken(token)
	if err != nil || pin != iss.JoinPin() {
		t.Fatalf("token %q: %v", token, err)
	}
	if !strings.Contains(errOut, "for node n7") {
		t.Fatalf("stderr %q", errOut)
	}
	toks, err := store.New(d).ListJoinTokens(ctx)
	if err != nil || len(toks) != 1 || toks[0].Hash != pki.HashJoinSecret(sent) || toks[0].NodeName != "n7" ||
		toks[0].CreatedBy != "ferund join-token" {
		t.Fatalf("tokens %+v %v", toks, err)
	}
	// The issuer of another trust domain is refused.
	code, _, _ = runCmd([]string{"join-token", "--database-url", url, "--ca-dir", dir, "--trust-domain", "feos.test"}, noEnv)
	if code != 1 {
		t.Fatalf("wrong trust domain: exit %d", code)
	}

	// --kind ingress issues a token for an instance of ferun-ingress.
	code, out, errOut = runCmd([]string{"join-token", "--database-url", url, "--ca-dir", dir, "--trust-domain",
		"run.example.test", "--kind", "ingress", "--node", "ferun-ingress-0"}, noEnv)
	if code != 0 {
		t.Fatalf("join-token --kind ingress: exit %d %s", code, errOut)
	}
	if !strings.HasPrefix(errOut, "ingress join token ") || !strings.Contains(errOut, "for ingress ferun-ingress-0") {
		t.Fatalf("stderr %q", errOut)
	}
	sent, _, err = pki.SplitIngressJoinToken(strings.TrimSpace(out))
	if err != nil {
		t.Fatal(err)
	}
	toks, err = store.New(d).ListJoinTokens(ctx)
	if err != nil || len(toks) != 2 || toks[0].Hash != pki.HashJoinSecret(sent) || toks[0].Kind != "ingress" ||
		toks[0].NodeName != "ferun-ingress-0" || toks[1].Kind != "node" {
		t.Fatalf("tokens %+v %v", toks, err)
	}
	if code, _, errOut = runCmd([]string{"join-token", "--database-url", url, "--ca-dir", dir, "--kind", "pod"}, noEnv); code != 2 ||
		!strings.Contains(errOut, "--kind") {
		t.Fatalf("--kind pod: exit %d %s", code, errOut)
	}

	// service-token issues a sink's token, printed once, stored by its
	// hash with an activity row.
	code, out, errOut = runCmd([]string{"service-token", "--database-url", url, "--kind", "sink", "--name", "sink-0"}, noEnv)
	if code != 0 || !strings.HasPrefix(errOut, "sink service token ") || !strings.Contains(errOut, "for sink-0") {
		t.Fatalf("service-token: exit %d %s", code, errOut)
	}
	tok := strings.TrimSpace(out)
	sts, err := store.New(d).ListServiceTokens(ctx)
	if !strings.HasPrefix(tok, "feos-sink-") || err != nil || len(sts) != 1 || sts[0].Hash != store.HashToken(tok) ||
		sts[0].Name != "sink-0" || sts[0].Kind != "sink" || sts[0].CreatedBy != "ferund service-token" {
		t.Fatalf("token %q, rows %+v %v", tok, sts, err)
	}
	var acts int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM activity WHERE kind = 'service_token_created'").Scan(&acts); err != nil ||
		acts != 1 {
		t.Fatalf("activity rows %d %v", acts, err)
	}
	// --kind auth issues ferun-auth's token (ADR 0054).
	code, out, errOut = runCmd([]string{"service-token", "--database-url", url, "--kind", "auth", "--name", "ferun-auth"}, noEnv)
	if code != 0 || !strings.HasPrefix(errOut, "auth service token ") || !strings.Contains(errOut, "for ferun-auth") {
		t.Fatalf("service-token --kind auth: exit %d %s", code, errOut)
	}
	tok = strings.TrimSpace(out)
	sts, err = store.New(d).ListServiceTokens(ctx)
	if !strings.HasPrefix(tok, "feos-auth-") || err != nil || len(sts) != 2 || sts[1].Hash != store.HashToken(tok) ||
		sts[1].Name != "ferun-auth" || sts[1].Kind != "auth" {
		t.Fatalf("token %q, rows %+v %v", tok, sts, err)
	}
	for _, args := range [][]string{{"--kind", "node", "--name", "a"}, {"--kind", "other", "--name", "a"},
		{"--name", "Bad_Name"}, {}} {
		if code, _, _ := runCmd(append([]string{"service-token", "--database-url", url}, args...), noEnv); code != 2 {
			t.Errorf("service-token %v: exit %d", args, code)
		}
	}
}

func TestCommandLine(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"--help"}, 0, "Commands:"},
		{[]string{"help", "join-token"}, 0, "--node (FERUN_NODE)"},
		{[]string{"help", "service-token"}, 0, `--kind (FERUN_KIND, default "sink")`},
		{[]string{"serve", "--help"}, 0, "--database-url (FERUN_DATABASE_URL)"},
		{[]string{"migrate", "-h"}, 0, "Applies the database migrations"},
		{[]string{"version"}, 0, ""},
		{[]string{"frobnicate"}, 2, ""},
		{[]string{"serve"}, 2, ""},
		{[]string{"--listen", ":1"}, 2, ""},
	} {
		code, out, errOut := runCmd(c.args, noEnv)
		if code != c.code || !strings.Contains(out, c.out) {
			t.Errorf("%v: exit %d, out %q, err %q", c.args, code, out, errOut)
		}
	}
}
