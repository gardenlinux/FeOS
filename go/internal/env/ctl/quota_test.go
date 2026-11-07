// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/store"
)

// A tenant job's create past the tenant's running jobs or its vCPUs is
// 422 quota_exceeded (ADR 0031); a finished job counts no more.
func TestTenantJobQuota(t *testing.T) {
	h := newHarness(t, options{})
	two, four := 2, 4
	if _, err := h.st.SetTenantLimits(bg, testTenant, store.TenantLimits{RunningJobs: &two, VCPUs: &four}); err != nil {
		t.Fatal(err)
	}
	quotaErr := func(err error, resource string) {
		t.Helper()
		var e *apierr.Error
		if !errors.As(err, &e) || e.Status != http.StatusUnprocessableEntity || e.Quota == nil || e.Quota.Resource != resource {
			t.Fatalf("err %v, want quota_exceeded of %s", err, resource)
		}
	}
	big := tenantJobSpecOf(time.Minute)
	big.VCPUs = 3
	first := h.createTenantJob(big)
	_, _, err := h.c.CreateTenantJob(bg, testTenant, h.jobSpecOf(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = h.c.CreateTenantJob(bg, testTenant, h.jobSpecOf(time.Minute))
	quotaErr(err, "runningJobs")
	if _, err := h.st.SetTenantLimits(bg, testTenant, store.TenantLimits{VCPUs: &four}); err != nil {
		t.Fatal(err)
	}
	_, _, err = h.c.CreateTenantJob(bg, testTenant, h.jobSpecOf(time.Minute))
	quotaErr(err, "vcpus")
	if _, err := h.st.Q().Exec(bg, `UPDATE jobs SET phase = 'cancelled', finished_at = now() WHERE id = $1`,
		first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.c.CreateTenantJob(bg, testTenant, h.jobSpecOf(time.Minute)); err != nil {
		t.Fatal(err)
	}
}
