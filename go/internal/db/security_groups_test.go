// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
)

// 0031 makes the table of security groups with each environment's
// default, and writes today's open default (in from default, any
// protocol) into the latest and the deployed deployments' specs, every
// image service, job and addon in default; older deployments, and a spec
// that has groups, are left as they were. (0033 takes it all out again.)
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
	const full = `{"services": {"web": {"image": "x@sha256:1", "replicas": 1, "ports": [{"name": "http", "port": 8080}]},
		"dev": {"runtime": "attached", "sandbox": "k3x9m2p7q4r", "command": ["vite"]}},
		"jobs": {"migrate": {"image": "x@sha256:1"}},
		"addons": {"db": {"type": "postgres", "version": "17", "plan": "small"}},
		"routes": {"root": {"path": "/", "service": "web", "port": "http"}}}`
	exec(`INSERT INTO env_deployments (env_id, number, spec, base, origin, state) VALUES
		('` + stage + `', 1, '` + full + `', 0, 'apply', 'replaced'),
		('` + stage + `', 2, '` + full + `', 1, 'apply', 'deployed'),
		('` + stage + `', 3, '{}', 2, 'apply', 'deploying')`)
	migrateRange(t, d, 31, 32)

	type group struct {
		In []struct {
			Protocol string `json:"protocol"`
			From     struct {
				Group string `json:"group"`
			} `json:"from"`
		} `json:"in"`
		Out []struct {
			Protocol string `json:"protocol"`
			To       string `json:"to"`
		} `json:"out"`
	}
	type member struct {
		SecurityGroups []string `json:"securityGroups"`
	}
	type stored struct {
		SecurityGroups map[string]group  `json:"securityGroups"`
		Services       map[string]member `json:"services"`
		Jobs           map[string]member `json:"jobs"`
		Addons         map[string]member `json:"addons"`
	}
	specOf := func(n int) (string, stored) {
		t.Helper()
		var raw []byte
		if err := d.Pool.QueryRow(ctx, "SELECT spec FROM env_deployments WHERE env_id = $1 AND number = $2", stage, n).
			Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var s stored
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("deployment %d: %v", n, err)
		}
		return string(raw), s
	}
	raw1, _ := specOf(1)
	if strings.Contains(raw1, "securityGroups") {
		t.Fatalf("an older deployment changed: %s", raw1)
	}
	raw2, deployed := specOf(2)
	open := deployed.SecurityGroups["default"]
	if len(deployed.SecurityGroups) != 1 || len(open.In) != 1 || open.In[0].Protocol != "any" ||
		open.In[0].From.Group != "default" || len(open.Out) != 1 || open.Out[0].Protocol != "any" ||
		open.Out[0].To != "internet" {
		t.Fatalf("default %s", raw2)
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
	// Nothing else of the spec changed.
	var same bool
	if err := d.Pool.QueryRow(ctx, `SELECT (spec - 'securityGroups') #- '{services,web,securityGroups}'
			#- '{jobs,migrate,securityGroups}' #- '{addons,db,securityGroups}' = $1::jsonb
		FROM env_deployments WHERE env_id = $2 AND number = 2`, full, stage).Scan(&same); err != nil || !same {
		t.Fatalf("the rest of the spec changed (%v): %s", err, raw2)
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
