// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// A row's workload is its object for an instance, by its log labels, and
// the row itself for a tenant's job and a sandbox; its set is the one the
// nodes' sets carry (ADR 0034, 0036).
func TestSandboxWorkload(t *testing.T) {
	env := uuid.New()
	id := podid.Mint()
	labels := func(kind, object string) map[string]string {
		return map[string]string{model.LogKind: kind, model.LogObject: object}
	}
	for _, c := range []struct {
		name                string
		row                 Sandbox
		kind, object, group string
	}{
		{"service", Sandbox{ID: id, EnvironmentID: env, Instance: true, LogLabels: labels("service", "api")},
			"service", "api", model.WorkloadGroupID(env, "service", "api")},
		{"job run", Sandbox{ID: id, EnvironmentID: env, Instance: true, LogLabels: labels("job", "migrate")},
			"job", "migrate", model.WorkloadGroupID(env, "job", "migrate")},
		{"addon", Sandbox{ID: id, EnvironmentID: env, Instance: true, LogLabels: labels("addon", "db")},
			"addon", "db", model.WorkloadGroupID(env, "addon", "db")},
		{"tenant's job", Sandbox{ID: id, EnvironmentID: env, TenantJob: true},
			pki.WorkloadTenantJob, id.String(), model.JobWorkloadGroupID(id.String())},
		{"sandbox", Sandbox{ID: id, EnvironmentID: env, LogLabels: labels(model.LogKindSandbox, id.String())},
			pki.WorkloadSandbox, id.String(), model.SandboxWorkloadGroupID(id.String())},
		{"outside an environment", Sandbox{ID: id}, "", "", ""},
		{"instance without labels", Sandbox{ID: id, EnvironmentID: env, Instance: true}, "", "", ""},
		{"instance of an unknown kind", Sandbox{ID: id, EnvironmentID: env, Instance: true, LogLabels: labels("x", "y")},
			"", "", ""},
	} {
		kind, object, group := c.row.Workload()
		if kind != c.kind || object != c.object || group != c.group {
			t.Errorf("%s: (%q, %q, %q), want (%q, %q, %q)", c.name, kind, object, group, c.kind, c.object, c.group)
		}
	}
}
