// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/env/spec"
)

// 0031 makes the table of security groups with each environment's
// default, and writes today's open default (in from default, any
// protocol) into the latest and the deployed deployments' specs in the
// form Normalise gives them, every image service, job and addon in
// default; older deployments, and a spec that has groups, are left as
// they were.
func TestSecurityGroupsMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 31)

	exec := func(q string) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const stage = "00000000-0000-4000-8000-000000000001"
	const empty = "00000000-0000-4000-8000-000000000002"
	exec("INSERT INTO tenants (id) VALUES ('acme')")
	exec(`INSERT INTO environments (id, owner, name, deployment, deployed) VALUES
		('` + stage + `', 'acme', 'stage', 3, 2), ('` + empty + `', 'acme', 'empty', 0, 0)`)
	// A spec as the version before stored it: normalised, without groups.
	req, fs := spec.Parse([]byte(`{"services": {"web": {"image": "x@sha256:1", "ports": [{"name": "http", "port": 8080}]},
		"dev": {"runtime": "attached", "sandbox": "k3x9m2p7q4r", "command": ["vite"]}},
		"jobs": {"migrate": {"image": "x@sha256:1"}},
		"addons": {"db": {"type": "postgres", "version": "17", "plan": "small"}},
		"routes": {"root": {"path": "/", "service": "web", "port": "http"}}}`))
	if len(fs) != 0 {
		t.Fatalf("%+v", fs)
	}
	want, _ := spec.Normalise(req, spec.Pins{})
	want.SecurityGroups["default"] = spec.SecurityGroup{
		In:  []spec.Rule{{Protocol: spec.ProtocolAny, From: &spec.Peer{Group: spec.DefaultSecurityGroup}}},
		Out: want.SecurityGroups["default"].Out}
	old := want
	old.SecurityGroups = nil
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	full := strings.ReplaceAll(string(b), `,"securityGroups":["default"]`, "")
	if strings.Contains(full, "securityGroups") {
		t.Fatalf("the old form has groups: %s", full)
	}
	exec(`INSERT INTO env_deployments (env_id, number, spec, base, origin, state) VALUES
		('` + stage + `', 1, '` + full + `', 0, 'apply', 'replaced'),
		('` + stage + `', 2, '` + full + `', 1, 'apply', 'deployed'),
		('` + stage + `', 3, '{}', 2, 'apply', 'deploying')`)
	migrateRange(t, d, 31, 32)

	specOf := func(n int) (string, spec.Spec) {
		t.Helper()
		var raw []byte
		if err := d.Pool.QueryRow(ctx, "SELECT spec FROM env_deployments WHERE env_id = $1 AND number = $2", stage, n).
			Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var s spec.Spec
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("deployment %d: %v", n, err)
		}
		return string(raw), s
	}
	raw1, _ := specOf(1)
	if strings.Contains(raw1, "securityGroups") {
		t.Fatalf("an older deployment changed: %s", raw1)
	}
	_, deployed := specOf(2)
	open := deployed.SecurityGroups["default"]
	if len(open.In) != 1 || open.In[0].Protocol != "any" || open.In[0].From == nil || open.In[0].From.Group != "default" ||
		len(open.Out) != 1 || open.Out[0].To == nil || !open.Out[0].To.Internet {
		t.Fatalf("default %+v", open)
	}
	if g := deployed.Services["web"].SecurityGroups; len(g) != 1 || g[0] != "default" {
		t.Fatalf("web %v", g)
	}
	if g := deployed.Services["dev"].SecurityGroups; g != nil {
		t.Fatalf("an attached service got groups: %v", g)
	}
	if g := deployed.Jobs["migrate"].SecurityGroups; len(g) != 1 || g[0] != "default" {
		t.Fatalf("migrate %v", g)
	}
	if g := deployed.Addons["db"].SecurityGroups; len(g) != 1 || g[0] != "default" {
		t.Fatalf("db %v", g)
	}
	// The migrated spec is in its normalised form: normalised again, it
	// is the same deployment, so the next apply of the same spec changes
	// nothing.
	if !spec.EqualNormalised(want, deployed) {
		b, _ := json.Marshal(deployed)
		t.Fatalf("migrated\n%s\nwant the normalised\n%s", b, mustJSON(t, want))
	}
	if again, _ := spec.Normalise(deployed, spec.Pins{}); !spec.EqualNormalised(again, deployed) {
		t.Fatalf("not normalised:\n%s", mustJSON(t, again))
	}
	if raw3, latest := specOf(3); len(latest.SecurityGroups) != 1 || len(latest.SecurityGroups["default"].In) != 1 {
		t.Fatalf("latest %s", raw3)
	}

	var groups int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM security_groups WHERE name = 'default'").Scan(&groups); err != nil ||
		groups != 2 {
		t.Fatalf("default rows %d %v", groups, err)
	}
	// A group's row goes with its environment.
	exec("DELETE FROM env_deployments WHERE env_id = '" + empty + "'")
	exec("DELETE FROM environments WHERE id = '" + empty + "'")
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM security_groups").Scan(&groups); err != nil || groups != 1 {
		t.Fatalf("rows %d %v", groups, err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
