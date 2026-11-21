// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// A restore sends the row's log labels and the redactions the redactor
// gives anew (ADR 0026), so that a hibernated pod restored on another
// node, which has no record of it, ships its log as before; a redactor
// that fails fails the restore before its call.
func TestRestoreSendsLogLabels(t *testing.T) {
	h := newHarness(t)
	n1 := h.addNode("n1")
	n2 := h.addNode("n2")
	sb := h.hibernatedOn("n1")
	h.settle(sb.ID)
	labels := map[string]string{model.LogProject: testProject, model.LogInstance: sb.ID.String()}
	h.exec(`UPDATE sandboxes SET log_labels = '{"ferun.project":"`+testProject+`","ferun.instance":"`+sb.ID.String()+`"}'
		WHERE id = $1`, sb.ID)
	var asked int
	h.eng().SetLogRedactor(func(_ context.Context, row store.Sandbox) ([]string, error) {
		asked++
		if !maps.Equal(row.LogLabels, labels) {
			return nil, errors.New("not the row's labels")
		}
		if asked == 1 {
			return nil, errors.New("no secrets now")
		}
		return []string{"s3cr3t-value"}, nil
	})
	// The record holder lost the pod: the restore goes to n2.
	n1.Script().SetHook(podv1.PodService_RestorePod_FullMethodName, func(context.Context, string, any) error {
		if _, err := h.pods("n1").DeletePod(bg, &podv1.DeletePodRequest{PodId: sb.ID.String()}); err != nil {
			return err
		}
		return status.Error(codes.NotFound, "no pod")
	})
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	if _, err := h.eng().Ensure(ctx, sb.ID); err == nil || !strings.Contains(err.Error(), "no secrets now") {
		t.Fatalf("the restore without its redactions: %v", err)
	}
	lease := h.ensure(sb.ID)
	if lease.Node != "n2" {
		t.Fatalf("restored on %s", lease.Node)
	}
	if got := n2.LogLabels(sb.ID); !maps.Equal(got, labels) {
		t.Fatalf("n2's labels %v", got)
	}
	if got := n2.LogRedact(sb.ID); !slices.Equal(got, []string{"s3cr3t-value"}) {
		t.Fatalf("n2's redactions %q", got)
	}
}
