// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// fakeHTTP is the source of a pod's clear part: one per sandbox, as the
// test sets it.
type fakeHTTP struct {
	mu    sync.Mutex
	parts map[string]*podv1.PodHttpBindings
}

func (f *fakeHTTP) PodHTTP(_ context.Context, row store.Sandbox) (*podv1.PodHttpBindings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.parts[row.ID.String()], nil
}

func (f *fakeHTTP) set(id string, version string, names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.parts == nil {
		f.parts = map[string]*podv1.PodHttpBindings{}
	}
	if version == "" {
		delete(f.parts, id)
		return
	}
	f.parts[id] = &podv1.PodHttpBindings{Names: names, InterceptHosts: []string{}, InterceptCaPems: []string{},
		Version: version}
}

// nodePart is the clear part in the node's record of the pod.
func (h *harness) nodePart(node string, id string) *podv1.PodHttpBindings {
	h.t.Helper()
	info, err := h.pods(node).GetPod(bg, &podv1.GetPodRequest{PodId: id})
	if err != nil {
		h.t.Fatal(err)
	}
	return info.GetSpec().GetHttpBindings()
}

func (h *harness) waitPushed(id store.Sandbox, version string) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h.row(id.ID).HTTPPushed == version {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("http_pushed is %q, not %q", h.row(id.ID).HTTPPushed, version)
}

// A create carries the clear part in its spec; a change reaches the pod
// at once, by a push, and the pushed version is recorded; the same part
// again is not sent; a part removed is sent empty.
func TestHTTPBindingsCreateAndPush(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	src := &fakeHTTP{}
	h.eng().SetHTTPBindings(src)
	sb := h.sandbox(512)
	src.set(sb.ID.String(), "v1", "erp")
	h.ensure(sb.ID)
	if part := h.nodePart("n1", sb.ID.String()); part.GetVersion() != "v1" || !slices.Equal(part.GetNames(), []string{"erp"}) {
		t.Fatalf("the create's part: %v", part)
	}
	h.waitPushed(sb, "v1")

	calls := h.watchCalls("n1", podv1.PodService_SetPodHttpBindings_FullMethodName, false)
	src.set(sb.ID.String(), "v2", "erp", "pay")
	h.eng().PushHTTPBindings(sb.ID)
	h.waitPushed(sb, "v2")
	if part := h.nodePart("n1", sb.ID.String()); part.GetVersion() != "v2" || len(part.GetNames()) != 2 {
		t.Fatalf("the pushed part: %v", part)
	}
	h.eng().PushHTTPBindings(sb.ID)
	time.Sleep(200 * time.Millisecond)
	if n := calls.count(); n != 1 {
		t.Fatalf("SetPodHttpBindings calls: %d", n)
	}
	src.set(sb.ID.String(), "")
	h.eng().PushHTTPBindings(sb.ID)
	h.waitPushed(sb, "")
	if part := h.nodePart("n1", sb.ID.String()); part != nil {
		t.Fatalf("the removed part: %v", part)
	}
	if ev := h.activityNamed(sb.ID, activity.KindPodHTTPBindings, "HTTP bindings sent to node n1"); ev["version"] == nil {
		t.Fatalf("the push's activity: %v", ev)
	}
}

// A part that changed while the pod was stopped reaches it before its
// start; a node without SetPodHttpBindings is recorded and its op goes on.
func TestHTTPBindingsAroundOps(t *testing.T) {
	h := newHarness(t)
	n1 := h.addNode("n1")
	src := &fakeHTTP{}
	h.eng().SetHTTPBindings(src)
	sb := h.running(512)
	if _, err := h.pods("n1").StopPod(bg, &podv1.StopPodRequest{PodId: sb.ID.String()}); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StateStopped, 10*time.Second)
	src.set(sb.ID.String(), "v1", "erp")
	h.ensure(sb.ID)
	if part := h.nodePart("n1", sb.ID.String()); part.GetVersion() != "v1" {
		t.Fatalf("the part after the start: %v", part)
	}
	h.waitPushed(sb, "v1")

	n1.Script().SetHook(podv1.PodService_SetPodHttpBindings_FullMethodName, func(context.Context, string, any) error {
		return status.Error(codes.Unimplemented, "unknown method")
	})
	src.set(sb.ID.String(), "v2", "erp")
	h.eng().PushHTTPBindings(sb.ID)
	h.activityNamed(sb.ID, activity.KindPodHTTPBindings, "HTTP bindings unimplemented to node n1")
	if got := h.row(sb.ID).HTTPPushed; got != "v1" {
		t.Fatalf("http_pushed after UNIMPLEMENTED: %q", got)
	}
}

// The leader's loop sends a pod the part its node did not take: a push
// that was lost.
func TestHTTPBindingsLoop(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	src := &fakeHTTP{}
	h.eng().SetHTTPBindings(src)
	sb := h.running(512)
	other := h.running(512)
	src.set(sb.ID.String(), "v1", "erp")
	// sb's row has a part its node never took; other's has none at all and
	// is no candidate.
	h.exec(`UPDATE sandboxes SET http_version = 'set' WHERE id = $1`, sb.ID)
	calls := h.watchCalls("n1", podv1.PodService_SetPodHttpBindings_FullMethodName, false)
	if err := h.eng().httpBindingsLoop(bg); err != nil {
		t.Fatal(err)
	}
	h.waitPushed(sb, "v1")
	if part := h.nodePart("n1", sb.ID.String()); part.GetVersion() != "v1" {
		t.Fatalf("the loop's part: %v", part)
	}
	if err := h.eng().httpBindingsLoop(bg); err != nil {
		t.Fatal(err)
	}
	if n := calls.count(); n != 1 {
		t.Fatalf("SetPodHttpBindings calls: %d", n)
	}
	if got := h.row(other.ID).HTTPPushed; got != "" {
		t.Fatalf("the other row: %q", got)
	}
}
