// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/bindings"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/env/envdeps"
	"github.com/gardenlinux/feos/go/internal/seal"
	"github.com/gardenlinux/feos/go/internal/store"
)

// convertSpec is a latest deployment as FeCode renders an HTTP API of
// fecode.yaml today: binding s4 whose header's value is the secret
// http.s4.header.apikey at version 2, and a binding that names a
// credential, which is an HTTP secret already.
const convertSpec = `{"services":{"app":{"runtime":"image","image":"ghcr.io/acme/app@sha256:` +
	`0000000000000000000000000000000000000000000000000000000000000000","bindings":["gh","s4"]}},
  "bindings":{
    "s4":{"upstream":"https://203.0.113.40","mode":"name","pathPrefixes":["/odata/"],
      "rateLimit":{"perSecond":5,"burst":10},
      "credential":{"kind":"headers","headers":{"APIKey":{"secret":"http.s4.header.apikey"}}}},
    "gh":{"upstream":"https://api.github.com","mode":"name","credentialRef":{"name":"gh","version":1}}},
  "secrets":{"http.s4.header.apikey":{"version":2}}}`

// convert-bindings makes an HTTP secret of each binding stated in an
// environment's latest deployment, its key from the version the
// deployment pins; it says what it would make without --apply, never a
// value, and leaves a name a secret holds alone.
func TestConvertBindings(t *testing.T) {
	t.Parallel()
	url := dbtest.NewDatabase(t)
	dir := t.TempDir()
	key := make([]byte, 32)
	key[3] = 9
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, key, 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"FERUN_DATABASE_URL": url, "FERUN_SECRET_KEY_FILE": keyFile,
		// The address is TEST-NET-3: allowed here, as an API on the
		// internet would be.
		"FERUN_DENY_PREFIX": "10.0.0.0/8"}
	getenv := func(k string) string { return vars[k] }
	if code, _, errOut := runCmd([]string{"migrate"}, getenv); code != 0 {
		t.Fatalf("migrate: exit %d %s", code, errOut)
	}
	ctx := context.Background()
	d, err := db.Open(ctx, url, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st := store.New(d)
	sealer, err := seal.New(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, "INSERT INTO projects (id) VALUES ('acme')"); err != nil {
		t.Fatal(err)
	}
	put := func(env, spec string, secrets map[string][]string) {
		t.Helper()
		id := dbtest.Environment(t, d.Pool, "acme", env)
		for name, values := range secrets {
			for i, v := range values {
				sealed, err := sealer.Seal([]byte(v), envdeps.SecretAAD(id, name, i+1))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.InsertSecret(ctx, store.EnvSecret{EnvID: id, Name: name, Version: i + 1, Sealed: sealed}); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, err := d.Pool.Exec(ctx, `INSERT INTO env_deployments (env_id, number, spec, base, state, origin)
			VALUES ($1, 1, $2, 0, 'deployed', 'apply')`, id, spec); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Pool.Exec(ctx, "UPDATE environments SET deployment = 1, deployed = 1 WHERE id = $1", id); err != nil {
			t.Fatal(err)
		}
	}
	put("dev", convertSpec, map[string][]string{"http.s4.header.apikey": {"old-key", "dev-key"}})
	// stage has a value secret of the binding's name: left alone.
	put("stage", convertSpec, map[string][]string{"http.s4.header.apikey": {"k1", "k2"}, "s4": {"x"}})

	code, out, errOut := runCmd([]string{"convert-bindings"}, getenv)
	if code != 0 {
		t.Fatalf("dry run: exit %d %s %s", code, out, errOut)
	}
	for _, want := range []string{"acme/dev", "binding s4, upstream https://203.0.113.40, mode name, credential headers: would make HTTP secret s4",
		"binding gh", "it names credential gh", "acme/stage", "a value secret of the name exists",
		"dry run (--apply writes): 4 bindings; 1 would be made, 0 made, 0 unchanged, 3 skipped, 0 failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run does not say %q:\n%s", want, out)
		}
	}
	for _, v := range []string{"dev-key", "old-key", "k2"} {
		if strings.Contains(out+errOut, v) {
			t.Fatalf("the output shows a value: %s %s", out, errOut)
		}
	}
	var n int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM env_credentials").Scan(&n); err != nil || n != 0 {
		t.Fatalf("the dry run wrote %d credentials: %v", n, err)
	}

	code, out, errOut = runCmd([]string{"convert-bindings", "--apply"}, getenv)
	if code != 0 || !strings.Contains(out, "made HTTP secret s4 version 1") ||
		!strings.Contains(out, "applied: 4 bindings; 0 would be made, 1 made") {
		t.Fatalf("apply: exit %d %s %s", code, out, errOut)
	}
	bs, err := bindings.New(bindings.Config{Enabled: true, Key: key}, st, d)
	if err != nil {
		t.Fatal(err)
	}
	dev := dbtest.Environment(t, d.Pool, "acme", "dev")
	b, err := bs.OpenCredential(ctx, dev, "s4", 1, "s4")
	if err != nil || b.GetHeaders().GetHeaders()["Apikey"] != "dev-key" || b.GetUpstream() != "https://203.0.113.40" ||
		len(b.GetPathPrefixes()) != 1 || b.GetRateLimit().GetPerSecond() != 5 {
		t.Fatalf("dev's HTTP secret s4: %v %v", b, err)
	}
	h, err := bs.HTTPSecret(ctx, dev, "s4")
	if err != nil || h.CreatedBy != "ferund convert-bindings" {
		t.Fatalf("who made it: %+v %v", h, err)
	}
	var deployments int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM env_deployments").Scan(&deployments); err != nil || deployments != 2 {
		t.Fatalf("deployments %d, want the two there were: %v", deployments, err)
	}

	// Once more: the HTTP secret is there.
	code, out, _ = runCmd([]string{"convert-bindings", "--apply"}, getenv)
	if code != 0 || !strings.Contains(out, "an HTTP secret of the name exists") {
		t.Fatalf("again: exit %d %s", code, out)
	}
}

func TestConvertBindingsFlags(t *testing.T) {
	t.Parallel()
	code, _, errOut := runCmd([]string{"convert-bindings"}, noEnv)
	if code != 2 || !strings.Contains(errOut, "--database-url is required") || !strings.Contains(errOut, "--secret-key-file is required") {
		t.Fatalf("no flags: exit %d %s", code, errOut)
	}
	code, out, _ := runCmd([]string{"help", "convert-bindings"}, noEnv)
	if code != 0 || !strings.Contains(out, "--apply (FERUN_APPLY)") || !strings.Contains(out, "--deny-prefix (FERUN_DENY_PREFIX)") {
		t.Fatalf("help: exit %d %s", code, out)
	}
}
