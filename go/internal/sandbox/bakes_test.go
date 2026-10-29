// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// Every create of a new pod asks the image's registry first (ADR 0025): a
// refusal fails the Ensure and leaves the sandbox pending with the
// answer, no answer is no as well and retryable, and only a yes creates
// the pod. A resume is no create and asks nothing.
func TestEnsureAsksTheRegistryBeforeEveryCreate(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	var mu sync.Mutex
	var asked []string
	var answer error
	e.checkImage = func(_ context.Context, pinned string) error {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, pinned)
		return answer
	}
	set := func(err error) {
		mu.Lock()
		answer = err
		mu.Unlock()
	}
	questions := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}
	sb := h.sandbox(512)
	ensure := func() error {
		ctx, cancel := context.WithTimeout(bg, 10*time.Second)
		defer cancel()
		_, err := e.Ensure(ctx, sb.ID)
		return err
	}
	noPod := func() {
		t.Helper()
		if _, err := h.pods("n1").GetPod(bg, podReq(sb.ID)); grpcCode(err) != codes.NotFound {
			t.Fatalf("a pod without a yes: %v", err)
		}
	}

	set(errImageRefused)
	wantErr(t, ensure(), model.ErrImageRefused, "image "+testImage+": the registry refused it")
	row := h.row(sb.ID)
	wantState(t, row, model.StatePending)
	if !strings.Contains(row.StatusDescription, "the registry refused it") {
		t.Fatalf("status %q", row.StatusDescription)
	}
	noPod()

	set(errImageNotThere)
	wantErr(t, ensure(), model.ErrImageRefused, "the registry does not have it for this create")
	noPod()

	set(&noAnswerError{errors.New("registry docker.io unreachable")})
	err := ensure()
	wantErr(t, err, model.ErrRegistryUnavailable, "image access not confirmed (retryable): "+testImage+": registry docker.io unreachable")
	var later *laterError
	if !errors.As(err, &later) || later.RetryAfter() != registryRetry {
		t.Fatalf("no answer without Retry-After: %#v", err)
	}
	noPod()

	set(nil)
	if err := ensure(); err != nil {
		t.Fatal(err)
	}
	wantState(t, h.row(sb.ID), model.StateRunning)
	n := len(questions())
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	h.ensure(sb.ID)
	got := questions()
	if len(got) != n || n != 4 {
		t.Fatalf("questions %d then %d, want 4 and no more for a resume", n, len(got))
	}
	for _, q := range got {
		if q != testImage {
			t.Fatalf("asked for %q, want the pinned %q", q, testImage)
		}
	}
}

// With --image-bakes, a create names the baked layers of its image and
// lets its node bake it: the first pod of an image bakes it, the engine
// records the bake from the pod's report, and the next create of the
// image, once asked for, starts its pod's disk on the layer. The image's
// source is the key: the same digest at another registry is another image.
func TestImageBakes(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ImageBakes = true })
	h.addNode("n1")
	first := h.running(512)
	info, err := h.pods("n1").GetPod(bg, podReq(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	if l := info.GetImageLayers(); l.GetImageRef() != testImage || !l.GetBake() || len(l.GetLayers()) != 0 {
		t.Fatalf("first create's layers %v", l)
	}
	baked := "baked-" + first.ID.String()
	if info.GetBaked().GetLayerId() != baked || info.GetDisk().GetLayerId() != "" {
		t.Fatalf("first pod: disk %v, baked %v", info.GetDisk(), info.GetBaked())
	}
	bakes, err := h.st.ImageBakes(bg, testImage)
	if err != nil || len(bakes) != 1 || bakes[0].Layer != baked || bakes[0].Node != "n1" ||
		bakes[0].GuestVersion != testnode.GuestVersion || bakes[0].L0 != testnode.BlankLayer {
		t.Fatalf("bakes %+v %v", bakes, err)
	}

	second := h.running(512)
	info, err = h.pods("n1").GetPod(bg, podReq(second.ID))
	if err != nil {
		t.Fatal(err)
	}
	if ls := info.GetImageLayers().GetLayers(); len(ls) != 1 || ls[0].GetLayerId() != baked {
		t.Fatalf("second create's layers %v", ls)
	}
	if info.GetDisk().GetLayerId() != baked || info.GetBaked() != nil {
		t.Fatalf("second pod: disk %v, baked %v", info.GetDisk(), info.GetBaked())
	}

	// The same digest named at another registry is another image: no
	// layer of the first, and a bake of its own.
	digest := testImage[strings.Index(testImage, "@"):]
	spoof := h.sandbox(512)
	h.exec("UPDATE sandboxes SET image = $2 WHERE id = $1", spoof.ID, "evil.example/x"+digest)
	h.ensure(spoof.ID)
	info, err = h.pods("n1").GetPod(bg, podReq(spoof.ID))
	if err != nil {
		t.Fatal(err)
	}
	if l := info.GetImageLayers(); l.GetImageRef() != "evil.example/x"+digest || len(l.GetLayers()) != 0 ||
		info.GetDisk().GetLayerId() != "" {
		t.Fatalf("another registry's image: layers %v, disk %v", l, info.GetDisk())
	}
}

// Without --image-bakes a create names no layers, and nothing is baked.
func TestNoImageBakes(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.running(512)
	info, err := h.pods("n1").GetPod(bg, podReq(sb.ID))
	if err != nil {
		t.Fatal(err)
	}
	if info.GetImageLayers() != nil || info.GetBaked() != nil {
		t.Fatalf("layers %v, baked %v", info.GetImageLayers(), info.GetBaked())
	}
	if bakes, err := h.st.ImageBakes(bg, testImage); err != nil || len(bakes) != 0 {
		t.Fatalf("bakes %v %v", bakes, err)
	}
}
