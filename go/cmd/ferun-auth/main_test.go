// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

func runCmd(args []string, getenv func(string) string) (int, string, string) {
	var out, errOut strings.Builder
	code := run(args, getenv, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestCommands(t *testing.T) {
	if code, out, _ := runCmd([]string{"version"}, noEnv); code != 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("version: %d %q", code, out)
	}
	if code, out, _ := runCmd([]string{"help"}, noEnv); code != 0 || !strings.Contains(out, "FERUN_AUTH_") ||
		!strings.Contains(out, "-public-url") || !strings.Contains(out, "-client-secret-file") {
		t.Fatalf("help: %d %q", code, out)
	}
	if code, out, _ := runCmd([]string{"serve", "--help"}, noEnv); code != 0 || !strings.Contains(out, "Usage: ferun-auth") {
		t.Fatalf("--help: %d %q", code, out)
	}
	if code, _, errOut := runCmd([]string{"nonsense"}, noEnv); code != 2 || !strings.Contains(errOut, `unknown command "nonsense"`) {
		t.Fatalf("unknown command: %d %q", code, errOut)
	}
}

// A setting that is missing or wrong stops the start with an error that
// names the flag.
func TestStartErrors(t *testing.T) {
	dir := t.TempDir()
	token, key := filepath.Join(dir, "token"), filepath.Join(dir, "key")
	if err := os.WriteFile(token, []byte("feos-auth-x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte(strings.Repeat("ab", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"FERUN_AUTH_PUBLIC_URL": "https://auth.example.test", "FERUN_AUTH_FERUN_URL": "http://ferund:8100",
		"FERUN_AUTH_TOKEN_FILE": token, "FERUN_AUTH_KEY_FILE": key}
	getenv := func(k string) string { return env[k] }
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"--issuer", "https://id.example.test/", "--client-secret-file", key}, 2, "--client-id is required with --issuer"},
		{[]string{"--issuer", "https://id.example.test/", "--client-id", "c"}, 2, "--client-secret-file is required with --issuer"},
		{[]string{"--public-url", "https://auth.example.test/x"}, 2, "--public-url"},
		{[]string{"--key-file", token}, 1, "--key-file"},
	} {
		code, _, errOut := runCmd(tc.args, getenv)
		if code != tc.code || !strings.Contains(errOut, tc.want) {
			t.Errorf("%v: %d %q, want %d %q", tc.args, code, errOut, tc.code, tc.want)
		}
	}
	if code, _, errOut := runCmd(nil, noEnv); code != 2 || !strings.Contains(errOut, "--public-url is required") ||
		!strings.Contains(errOut, "--token-file is required") {
		t.Fatalf("nothing set: %d %q", code, errOut)
	}
}

// ferun-auth links no database driver and no gRPC (ADR 0054: no access to
// the database, no node connection), nor ferund's packages that would.
func TestDependencies(t *testing.T) {
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(goBin); err != nil {
		if goBin, err = exec.LookPath("go"); err != nil {
			t.Skip("no go tool")
		}
	}
	out, err := exec.Command(goBin, "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	const mod = "github.com/gardenlinux/feos/go/"
	allowed := map[string]bool{mod + "cmd/ferun-auth": true, mod + "internal/ferunauth": true, mod + "internal/oidc": true,
		mod + "internal/egress": true, mod + "internal/seal": true, mod + "internal/visit": true}
	for _, p := range strings.Fields(string(out)) {
		switch {
		case strings.Contains(p, "github.com/jackc/pgx"), strings.HasPrefix(p, "google.golang.org/grpc"):
			t.Errorf("ferun-auth links %s", p)
		case strings.HasPrefix(p, mod) && !allowed[p]:
			t.Errorf("ferun-auth links %s, which it does not need", p)
		case strings.Contains(strings.SplitN(p, "/", 2)[0], ".") && !strings.HasPrefix(p, mod):
			t.Errorf("ferun-auth links %s, outside the standard library", p)
		}
	}
}
