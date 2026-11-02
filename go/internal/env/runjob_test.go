// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/env/ctl"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
)

// A job's run asked for while a run of the job runs answers 409 busy with
// the controller's own text (which deployment and attempt runs) and a
// Retry-After of 30 s; any other busy refusal keeps the generic text,
// which names no instance.
func TestRunJobBusy(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	h.create("stage", nil)
	h.apply("stage", `{"jobs": {"backfill": {"image": "ghcr.io/acme/api:1", "on": "manual"}}}`, 0)
	if err := h.svc.RunJob(ctx, tenantA, "stage", "backfill"); err != nil {
		t.Fatal(err)
	}
	err := h.svc.RunJob(ctx, tenantA, "stage", "backfill")
	wantStatus(t, err, http.StatusConflict, "job backfill runs already (deployment 1, attempt 1)")
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != CodeBusy || ae.Header.Get("Retry-After") != "30" ||
		ae.Error() != "job backfill runs already (deployment 1, attempt 1)" {
		t.Fatalf("busy %+v", ae)
	}

	err = ctlError(fmt.Errorf("job x runs already (deployment 3, attempt 2): %w", ctl.ErrJobRunning), spec.KindJob, "x")
	wantStatus(t, err, http.StatusConflict, "job x runs already (deployment 3, attempt 2)")
	err = ctlError(fmt.Errorf("an op ensure of instance i2 is in flight: %w", model.ErrBusy), spec.KindJob, "x")
	wantStatus(t, err, http.StatusConflict, "job x is busy with another operation; try again shortly")
}
