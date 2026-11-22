// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// serverIDs are the ServerIds of the TLS bindings the node holds for the
// pod.
func (h *harness) serverIDs(node string, id podid.ID) []string {
	h.t.Helper()
	info, err := h.pods(node).GetPod(bg, &podv1.GetPodRequest{PodId: id.String()})
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, b := range info.GetSpec().GetTlsBindings() {
		out = append(out, b.GetServerId())
	}
	return out
}

// stale gives the pod on node a TLS binding pinned to an ID of before, as
// a pod made before a change of the workload path holds it.
func (h *harness) stale(node string, id podid.ID) {
	h.t.Helper()
	if _, err := h.pods(node).SetPodTlsBindings(bg, &podv1.SetPodTlsBindingsRequest{PodId: id.String(),
		TlsBindings: []*podv1.TlsBinding{{Listen: "127.88.1.2:5432", Host: "db", Port: 5432,
			ServerId: "spiffe://" + testTD + "/earlier/path"}}}); err != nil {
		h.t.Fatal(err)
	}
}

// A pod that runs again other than by a create gets the TLS bindings of
// now: a resume sends them after its run opens. The leader's sweep sends
// every running pod with TLS bindings its bindings once a node and
// leadership.
func TestTLSBindingsAgain(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.sandbox(256)
	bindings := []model.TemplateTLSBinding{{Listen: "127.88.1.2:5432", Host: "db", Port: 5432, ALPN: "postgresql", Addon: "db"}}
	if ok, err := h.st.SetSandboxTLSBindings(bg, sb.ID, sb.Incarnation, bindings); err != nil || !ok {
		t.Fatalf("set TLS bindings: %v %v", ok, err)
	}
	h.ensure(sb.ID)
	fresh := "spiffe://" + testTD + "/project/" + testProject + "/env/" + sb.EnvironmentID.String() + "/addon/db"
	if got := h.serverIDs("n1", sb.ID); len(got) != 1 || got[0] != fresh {
		t.Fatalf("created with %v, want %s", got, fresh)
	}

	// Paused, the pod is given an ID of before; its resume sends it the
	// bindings of now.
	if res := h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"}); res.State != model.StatePaused {
		t.Fatalf("pause: %+v", res)
	}
	h.stale("n1", sb.ID)
	h.ensure(sb.ID)
	eventually(t, 15*time.Second, "the bindings of now after the resume", func() bool {
		got := h.serverIDs("n1", sb.ID)
		return len(got) == 1 && got[0] == fresh
	})

	// The sweep sends them once a node in a leadership.
	h.stale("n1", sb.ID)
	term := context.Background()
	if err := h.eng().tlsBindingsLoop(term); err != nil {
		t.Fatal(err)
	}
	if got := h.serverIDs("n1", sb.ID); len(got) != 1 || got[0] != fresh {
		t.Fatalf("after the sweep %v, want %s", got, fresh)
	}
	h.stale("n1", sb.ID)
	if err := h.eng().tlsBindingsLoop(term); err != nil {
		t.Fatal(err)
	}
	if got := h.serverIDs("n1", sb.ID); len(got) != 1 || got[0] == fresh {
		t.Fatalf("swept twice in one leadership: %v", got)
	}
	next, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.eng().tlsBindingsLoop(next); err != nil {
		t.Fatal(err)
	}
	if got := h.serverIDs("n1", sb.ID); len(got) != 1 || got[0] != fresh {
		t.Fatalf("after the next leadership's sweep %v, want %s", got, fresh)
	}
}

// A push that follows a commit sends the bindings the row holds when the
// call goes out, not those of the row the commit wrote: a SetTLSBindings
// between the two is not undone. A lease of another incarnation sends
// nothing.
func TestTLSPushReadsTheRowOfNow(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.sandbox(256)
	first := []model.TemplateTLSBinding{{Listen: "127.88.1.2:5432", Host: "db", Port: 5432, ALPN: "postgresql", Addon: "db"}}
	if ok, err := h.st.SetSandboxTLSBindings(bg, sb.ID, sb.Incarnation, first); err != nil || !ok {
		t.Fatalf("set TLS bindings: %v %v", ok, err)
	}
	h.ensure(sb.ID)
	row, err := h.st.GetSandbox(bg, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := tlsPushOf(row, "snapshot")
	if p == nil {
		t.Fatal("no push for a resumed row with TLS bindings")
	}

	later := append(first, model.TemplateTLSBinding{Listen: "127.88.1.3:6379", Host: "cache", Port: 6379, Addon: "cache"})
	if ok, err := h.st.SetSandboxTLSBindings(bg, sb.ID, sb.Incarnation, later); err != nil || !ok {
		t.Fatalf("set TLS bindings: %v %v", ok, err)
	}
	if err := h.eng().pushCurrentTLSBindings(bg, p.lease); err != nil {
		t.Fatal(err)
	}
	if got := h.serverIDs("n1", sb.ID); len(got) != 2 {
		t.Fatalf("pushed %v, want the row's two bindings of now", got)
	}

	h.stale("n1", sb.ID)
	other := p.lease
	other.Incarnation = uuid.New()
	if err := h.eng().pushCurrentTLSBindings(bg, other); err != nil {
		t.Fatal(err)
	}
	if got := h.serverIDs("n1", sb.ID); len(got) != 1 || got[0] != "spiffe://"+testTD+"/earlier/path" {
		t.Fatalf("a lease of another incarnation sent %v", got)
	}
}
