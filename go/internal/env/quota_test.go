// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/store"
)

func intPtr(v int) *int { return &v }

// A tenant's limits (ADR 0031): an environment's create and clone past
// the number of environments are 422 quota_exceeded, also when they come
// at once; a deployment that brings the vCPUs past the limit has an error
// finding in its plan and is not applied, nor cloned.
func TestQuotaEnvironments(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	if _, err := h.st.SetTenantLimits(ctx, tenantA, store.TenantLimits{Environments: intPtr(2), VCPUs: intPtr(4)}); err != nil {
		t.Fatal(err)
	}
	a := h.create("a", nil)
	h.create("b", nil)
	_, _, err := h.svc.CreateEnv(ctx, tenantA, CreateEnvRequest{Name: "c"})
	wantStatus(t, err, http.StatusUnprocessableEntity, "the tenant's limit of environments is 2, 2 are in use")
	// The same create again asks for nothing.
	if _, created, err := h.svc.CreateEnv(ctx, tenantA, CreateEnvRequest{Name: "a", ID: a.ID}); err != nil || created {
		t.Fatalf("again: %v %v", created, err)
	}
	// Another tenant's are its own.
	if _, _, err := h.svc.CreateEnv(ctx, tenantB, CreateEnvRequest{Name: "c"}); err != nil {
		t.Fatal(err)
	}

	replicas := func(n int) string {
		return fmt.Sprintf(`{"services": {"web": {"image": "nginx:1", "replicas": %d}}}`, n)
	}
	first := h.apply("a", replicas(3), 0)
	plan, err := h.svc.Plan(ctx, tenantA, "b", PlanRequest{Spec: json.RawMessage(replicas(2))})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Valid || !hasFinding(plan.Findings, spec.SeverityError, "", "brings the tenant to 5 of its limit 4") {
		t.Fatalf("plan %+v", plan)
	}
	_, err = h.svc.Apply(ctx, tenantA, "b", ApplyRequest{Spec: json.RawMessage(replicas(2))})
	wantStatus(t, err, http.StatusUnprocessableEntity, "")
	h.apply("b", replicas(1), 0)
	// Lowering what one asks for passes after the limit went down.
	if _, err := h.st.SetTenantLimits(ctx, tenantA, store.TenantLimits{Environments: intPtr(2), VCPUs: intPtr(1)}); err != nil {
		t.Fatal(err)
	}
	h.apply("a", replicas(2), first.Number)

	if _, err := h.st.SetTenantLimits(ctx, tenantA, store.TenantLimits{Environments: intPtr(2)}); err != nil {
		t.Fatal(err)
	}
	_, err = h.svc.Clone(ctx, tenantA, "a", CloneRequest{Name: "a2"})
	wantStatus(t, err, http.StatusUnprocessableEntity, "the tenant's limit of environments is 2")
	if _, err := h.st.SetTenantLimits(ctx, tenantA, store.TenantLimits{Environments: intPtr(9), VCPUs: intPtr(4)}); err != nil {
		t.Fatal(err)
	}
	_, err = h.svc.Clone(ctx, tenantA, "a", CloneRequest{Name: "a2"})
	if got, _ := statusOf(err); got != http.StatusUnprocessableEntity ||
		!hasFindingOf(err, "brings the tenant to 5 of its limit 4") {
		t.Fatalf("clone: %s", describe(err))
	}

	// Concurrent creates pass the limit no more than one by one.
	if _, err := h.st.SetTenantLimits(ctx, tenantA, store.TenantLimits{Environments: intPtr(4)}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = h.svc.CreateEnv(ctx, tenantA, CreateEnvRequest{Name: fmt.Sprintf("p%d", i)})
		}()
	}
	wg.Wait()
	made := 0
	for _, err := range errs {
		if err == nil {
			made++
		} else {
			wantStatus(t, err, http.StatusUnprocessableEntity, "the tenant's limit of environments is 4")
		}
	}
	if made != 2 {
		t.Fatalf("%d made: %v", made, errs)
	}
}

// hasFindingOf reports an InvalidSpecError with an error finding whose
// problem has problem.
func hasFindingOf(err error, problem string) bool {
	ise, ok := err.(*InvalidSpecError)
	return ok && hasFinding(ise.Findings, spec.SeverityError, "", problem)
}
