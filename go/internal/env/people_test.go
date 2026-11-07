// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/auth"
)

// A principal that writes an environment alone may change its attached
// services: they run in a sandbox of the environment (ADR 0035), which
// its grant covers. ferund records the principal of each deployment,
// beside the client's author.
func TestAttachedNeedsEnvWrite(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	h.create("one", nil)
	e := h.env("one")
	sb := h.sandbox(tenantA, "one")
	attached := func(port, image string) string {
		return `{"services": {"vite": {"runtime": "attached", "sandbox": "` + sb.String() + `", "command": ["vite"],
			"ports": [{"name": "http", "port": ` + port + `}]}, "api": {"image": "` + image + `"}}}`
	}
	tenantWriter := auth.WithTenant(h.ctx(), auth.Tenant{Name: tenantA, Principal: auth.Principal{TokenHash: "t1"},
		Role: auth.RoleWrite})
	envWriter := auth.WithTenant(h.ctx(), auth.Tenant{Name: tenantA,
		Principal: auth.Principal{Person: uuid.MustParse("00000000-0000-4000-8000-000000000001")},
		Envs:      map[uuid.UUID]auth.Role{e.ID: auth.RoleWrite}})
	apply := func(ctx context.Context, raw string, base int) ApplyAnswer {
		t.Helper()
		a, err := h.svc.Apply(ctx, tenantA, "one", ApplyRequest{Spec: json.RawMessage(raw), Base: base, Author: "fecode:ada"})
		if err != nil {
			t.Fatal(describe(err))
		}
		return a
	}

	// The environment's writer adds, changes and removes them.
	a := apply(envWriter, attached("5173", "nginx:1"), 0)
	b := apply(envWriter, attached("5174", "nginx:1"), a.Number)
	c := apply(tenantWriter, `{"services": {"api": {"image": "nginx:2"}}}`, b.Number)

	list, err := h.svc.Deployments(h.ctx(), tenantA, "one", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Deployments) != 3 || list.Deployments[0].Number != c.Number ||
		list.Deployments[0].MadeBy != "token:t1" || list.Deployments[1].MadeBy != "person:00000000-0000-4000-8000-000000000001" ||
		list.Deployments[1].Author != "fecode:ada" {
		t.Fatalf("deployments %+v", list.Deployments)
	}
}
