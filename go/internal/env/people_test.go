// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/auth"
)

// A principal that writes an environment alone may deploy it while its
// attached services stay as they are, and not change them: they run in
// the tenant's sandboxes (ADR 0031). A writer of the tenant may. ferund
// records the principal of each deployment, beside the client's author.
func TestAttachedNeedsTenantWrite(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	sb := h.sandbox(tenantA, true)
	h.create("one", nil)
	e := h.env("one")
	attached := func(port, image string) string {
		return `{"services": {"vite": {"runtime": "attached", "sandbox": "` + sb.String() + `", "command": ["vite"],
			"ports": [{"name": "http", "port": ` + port + `}]}, "api": {"image": "` + image + `"}}}`
	}
	tenantWriter := auth.WithTenant(h.ctx(), auth.Tenant{Name: tenantA, Principal: auth.Principal{TokenHash: "t1"},
		Role: auth.RoleWrite})
	envWriter := auth.WithTenant(h.ctx(), auth.Tenant{Name: tenantA,
		Principal: auth.Principal{Person: uuid.MustParse("00000000-0000-4000-8000-000000000001")},
		Envs:      map[uuid.UUID]auth.Role{e.ID: auth.RoleWrite}})
	apply := func(ctx context.Context, raw string, base int) (ApplyAnswer, error) {
		return h.svc.Apply(ctx, tenantA, "one", ApplyRequest{Spec: json.RawMessage(raw), Base: base, Author: "fecode:ada"})
	}

	// The environment's writer may not add an attached service.
	_, err := apply(envWriter, attached("5173", "nginx:1"), 0)
	wantStatus(t, err, http.StatusForbidden, "changing attached services needs the role write on the tenant: "+
		"they run in its sandboxes")
	a, err := apply(tenantWriter, attached("5173", "nginx:1"), 0)
	if err != nil {
		t.Fatal(describe(err))
	}
	// It may change the rest, the attached services as they are.
	b, err := apply(envWriter, attached("5173", "nginx:2"), a.Number)
	if err != nil {
		t.Fatal(describe(err))
	}
	_, err = apply(envWriter, attached("5174", "nginx:2"), b.Number)
	wantStatus(t, err, http.StatusForbidden, "changing attached services needs the role write on the tenant: "+
		"they run in its sandboxes")
	// Nor remove them.
	_, err = apply(envWriter, `{"services": {"api": {"image": "nginx:2"}}}`, b.Number)
	wantStatus(t, err, http.StatusForbidden, "changing attached services needs the role write on the tenant: "+
		"they run in its sandboxes")

	list, err := h.svc.Deployments(h.ctx(), tenantA, "one", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Deployments) != 2 || list.Deployments[0].MadeBy != "person:00000000-0000-4000-8000-000000000001" ||
		list.Deployments[1].MadeBy != "token:t1" || list.Deployments[0].Author != "fecode:ada" {
		t.Fatalf("deployments %+v", list.Deployments)
	}
	// Without a principal (ferund's own callers), nothing is checked.
	if _, err := apply(h.ctx(), attached("5175", "nginx:2"), b.Number); err != nil {
		t.Fatal(describe(err))
	}
}
