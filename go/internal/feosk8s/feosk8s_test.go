// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package feosk8s

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/kube"
)

const (
	ns     = "feos"
	prefix = "shoot--myproject--myshoot-worker-"
	// A node name as Gardener makes them: 51 characters, within a label
	// value's 63.
	node1 = "shoot--myproject--myshoot-worker-pool1-z1-cf876-l4bmn"
	node2 = "shoot--myproject--myshoot-worker-pool1-z1-cf876-x9q2z"
)

var feosLabels = map[string]string{"feos.fecode.dev/node": "true"}

// syncBuffer is a log sink the test reads while the controller writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// harness is a controller against fakes of the Kubernetes API and FeRun,
// with a clock of its own.
type harness struct {
	t     *testing.T
	kube  *fakeKube
	ferun *fakeFeRun
	logs  *syncBuffer
	dir   string
	cfg   Config

	kubeURL, ferunURL string

	mu    sync.Mutex
	clock time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, logs: &syncBuffer{}, dir: t.TempDir(), clock: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	h.kube = newFakeKube("sa-token")
	h.ferun = newFakeFeRun(h.now)
	ks := httptest.NewServer(h.kube)
	t.Cleanup(ks.Close)
	fs := httptest.NewServer(h.ferun)
	t.Cleanup(fs.Close)
	h.write("token", "sa-token\n")
	h.write("creds", "feos-k8s:s3cret\n")
	h.cfg = Config{NodeSelector: "feos.fecode.dev/node=true", NodeNamePrefix: prefix, Namespace: ns,
		JoinSecret: "feosd-join", GuardImage: "registry.k8s.io/pause:3.10", GuardPriorityClass: "system-node-critical",
		Interval: time.Second, JoinTokenTTL: time.Hour, RemoveAfter: 0}
	h.kubeURL, h.ferunURL = ks.URL, fs.URL
	return h
}

func (h *harness) write(name, content string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, name), []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clock
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clock = h.clock.Add(d)
}

// controller makes a controller, as a start of the process does.
func (h *harness) controller() *Controller {
	h.t.Helper()
	kc, err := kube.New(kube.Config{Server: h.kubeURL, TokenFile: filepath.Join(h.dir, "token"),
		HTTPClient: &http.Client{}})
	if err != nil {
		h.t.Fatal(err)
	}
	fr, err := NewFeRun(h.ferunURL, filepath.Join(h.dir, "creds"), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	c, err := New(h.cfg, kc, fr, slog.New(slog.NewTextHandler(h.logs, nil)))
	if err != nil {
		h.t.Fatal(err)
	}
	c.now = h.now
	return c
}

// pass runs one pass, which must succeed.
func (h *harness) pass(c *Controller) {
	h.t.Helper()
	if err := c.Reconcile(context.Background()); err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
}

// writes are the mutating calls since the last look, Kubernetes' then
// FeRun's.
func (h *harness) writes() []string {
	return append(h.kube.takeWrites(), h.ferun.takeWrites()...)
}

func (h *harness) guard(node string) (kube.Pod, bool) { return h.kube.pod(ns, GuardName(node)) }

func (h *harness) key(node string) string {
	s, ok := h.kube.secret(ns, "feosd-join")
	if !ok {
		return ""
	}
	return string(s.Data[node])
}

// steady checks that a pass changes nothing.
func (h *harness) steady(c *Controller) {
	h.t.Helper()
	h.writes()
	h.pass(c)
	if w := h.writes(); len(w) > 0 {
		h.t.Fatalf("a pass in a steady state wrote %q", w)
	}
}

func TestJoin(t *testing.T) {
	h := newHarness(t)
	h.kube.addNode(node1, feosLabels)
	h.kube.addNode("other", nil) // no FeOS node: no token, no guard
	c := h.controller()

	h.pass(c)
	key := h.key(node1)
	live := h.ferun.liveTokens(node1)
	if len(live) != 1 || key != live[0].secret {
		t.Fatalf("key %q, live tokens %+v; want one token pinned to the node, in the secret", key, live)
	}
	if exp, _ := time.Parse(time.RFC3339, live[0].ExpiresAt); exp.Sub(h.now()) != time.Hour {
		t.Fatalf("expires at %s; want in an hour", live[0].ExpiresAt)
	}
	if h.key("other") != "" || len(h.ferun.liveTokens("other")) != 0 {
		t.Fatal("a node outside the selector got a token")
	}
	if strings.Contains(h.logs.String(), key) {
		t.Fatalf("the token was logged: %s", h.logs.String())
	}

	// The token waits in the secret; a restart makes no second one.
	h.steady(c)
	h.steady(h.controller())

	// The node joins: FeRun has it joined, then ready.
	if !h.ferun.useToken(key) {
		t.Fatal("no token to use")
	}
	h.steady(c)
	if h.key(node1) == "" {
		t.Fatal("the key went before the node was ready")
	}
	h.ferun.setNode(FeRunNode{Node: node1, Status: nodeReady})
	h.pass(c)
	if h.key(node1) != "" {
		t.Fatal("the key stays after the node is ready")
	}
	if _, ok := h.kube.secret(ns, "feosd-join"); !ok {
		t.Fatal("the secret went")
	}
	h.steady(c)
}

func TestJoinReplacesExpiredToken(t *testing.T) {
	h := newHarness(t)
	h.kube.addNode(node1, feosLabels)
	c := h.controller()
	h.pass(c)
	first := h.key(node1)

	h.advance(59 * time.Minute)
	h.steady(c)
	h.advance(2 * time.Minute)
	h.pass(c)
	second := h.key(node1)
	if second == "" || second == first {
		t.Fatalf("the key after the token expired: %q (was %q)", second, first)
	}
	if live := h.ferun.liveTokens(node1); len(live) != 1 || live[0].secret != second {
		t.Fatalf("live tokens %+v", live)
	}
}

func TestJoinRemovedNode(t *testing.T) {
	h := newHarness(t)
	h.kube.addNode(node1, feosLabels)
	h.kube.addNode(node2, feosLabels)
	h.ferun.setNode(FeRunNode{Node: node1, Status: nodeRemoved})
	h.ferun.setNode(FeRunNode{Node: node2, Status: nodeDown, IdentityForked: true})
	c := h.controller()
	h.pass(c)
	if h.key(node1) == "" {
		t.Fatal("a node FeRun has removed got no token")
	}
	if h.key(node2) != "" {
		t.Fatal("a forked node got a token; that is an operator's call")
	}
}

func TestGuard(t *testing.T) {
	h := newHarness(t)
	h.kube.addNode(node1, feosLabels)
	h.ferun.setNode(FeRunNode{Node: node1, Status: nodeReady})
	c := h.controller()
	h.pass(c)
	g, ok := h.guard(node1)
	if !ok {
		t.Fatal("no guard")
	}
	s := g.Spec
	if s.NodeName != node1 || g.Metadata.Labels[LabelApp] != GuardApp || g.Metadata.Labels[LabelNode] != node1 ||
		s.PriorityClassName != "system-node-critical" || s.TerminationGracePeriodSeconds == nil ||
		*s.TerminationGracePeriodSeconds != 0 || len(s.Tolerations) != 1 || s.Tolerations[0].Operator != "Exists" ||
		s.Tolerations[0].Key != "" || len(s.Containers) != 1 || s.Containers[0].Image != "registry.k8s.io/pause:3.10" ||
		s.Containers[0].Resources.Requests["memory"] == "" {
		t.Fatalf("guard %+v", g)
	}
	if len(g.Metadata.Labels[LabelNode]) > 63 {
		t.Fatal("the node label's value is too long")
	}
	h.steady(c)

	// A guard that ended is replaced.
	g.Status.Phase = kube.PodFailed
	h.kube.putPod(g)
	h.pass(c)
	if _, ok := h.guard(node1); ok {
		t.Fatal("the ended guard stays")
	}
	h.pass(c)
	if g, ok := h.guard(node1); !ok || g.Status.Phase != "Running" {
		t.Fatal("no new guard")
	}
}

// drainState sets up a node that FeRun has ready, with its guard.
func drainState(t *testing.T) (*harness, *Controller) {
	h := newHarness(t)
	h.kube.addNode(node1, feosLabels)
	h.ferun.setNode(FeRunNode{Node: node1, Status: nodeReady})
	c := h.controller()
	h.pass(c)
	if _, ok := h.guard(node1); !ok {
		t.Fatal("no guard")
	}
	return h, c
}

func TestCordonDrainsAndReleases(t *testing.T) {
	h, c := drainState(t)
	h.kube.setUnschedulable(node1, true)
	h.pass(c)
	if n, _ := h.ferun.node(node1); !n.Draining {
		t.Fatal("the cordoned node does not drain in FeRun")
	}
	if h.kube.node(node1).Metadata.Annotations[AnnotationDrain] != "requested" {
		t.Fatal("no drain annotation")
	}
	if _, ok := h.guard(node1); !ok {
		t.Fatal("the guard went before the drain ended")
	}

	// Draining: the guard stays, and nothing is asked again.
	h.ferun.setNode(FeRunNode{Node: node1, Status: nodeReady, Draining: true, StatusDescription: "draining: 2 remain"})
	h.steady(c)
	h.steady(h.controller())

	h.ferun.setNode(FeRunNode{Node: node1, Status: nodeReady, Draining: true, StatusDescription: "drained"})
	h.pass(c)
	if _, ok := h.guard(node1); ok {
		t.Fatal("the guard stays after the node drained")
	}
	// No guard comes back on the cordoned node.
	h.steady(c)
	h.steady(h.controller())
}

func TestCannotDrainKeepsGuard(t *testing.T) {
	h, c := drainState(t)
	h.kube.setUnschedulable(node1, true)
	h.pass(c)
	h.ferun.setNode(FeRunNode{Node: node1, Status: nodeReady, Draining: true,
		StatusDescription: "cannot drain: the node has no object store"})
	h.steady(c)
	h.steady(c)
	if _, ok := h.guard(node1); !ok {
		t.Fatal("the guard went though the node cannot drain")
	}
	if n := strings.Count(h.logs.String(), "cannot drain"); n != 1 {
		t.Fatalf("logged %d times; want once:\n%s", n, h.logs.String())
	}
}

func TestCordonReleasesNodeFeRunCannotDrain(t *testing.T) {
	for _, tc := range []struct {
		name string
		node *FeRunNode
	}{
		{"unknown", nil},
		{"down", &FeRunNode{Node: node1, Status: nodeDown}},
		{"removed", &FeRunNode{Node: node1, Status: nodeRemoved}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.kube.addNode(node1, feosLabels)
			c := h.controller()
			h.pass(c)
			if tc.node != nil {
				h.ferun.setNode(*tc.node)
			}
			if _, ok := h.guard(node1); !ok {
				t.Fatal("no guard")
			}
			h.kube.setUnschedulable(node1, true)
			h.pass(c)
			if _, ok := h.guard(node1); ok {
				t.Fatal("the guard stays")
			}
			if n, _ := h.ferun.node(node1); n.Draining {
				t.Fatal("drained a node FeRun cannot drain")
			}
		})
	}
}

func TestCordonOfNodeGoingDown(t *testing.T) {
	h, c := drainState(t)
	h.kube.setUnschedulable(node1, true)
	// FeRun has the node down by the time the drain arrives: its answer
	// is 409.
	h.ferun.downOnDrain = true
	h.pass(c)
	if _, ok := h.guard(node1); ok {
		t.Fatal("the guard of a down node stays")
	}
}

func TestUncordonUndrains(t *testing.T) {
	h, c := drainState(t)
	h.kube.setUnschedulable(node1, true)
	h.pass(c)
	h.ferun.setNode(FeRunNode{Node: node1, Status: nodeReady, Draining: true, StatusDescription: "drained"})
	h.pass(c)
	if _, ok := h.guard(node1); ok {
		t.Fatal("the guard stays")
	}

	h.kube.setUnschedulable(node1, false)
	h.pass(h.controller()) // after a restart too
	n, _ := h.ferun.node(node1)
	if n.Draining || n.StatusDescription != "" {
		t.Fatalf("FeRun's node after the uncordon: %+v", n)
	}
	if _, ok := h.kube.node(node1).Metadata.Annotations[AnnotationDrain]; ok {
		t.Fatal("the drain annotation stays")
	}
	if _, ok := h.guard(node1); !ok {
		t.Fatal("no guard after the uncordon")
	}
	h.steady(c)
}

func TestOperatorDrainIsLeftAlone(t *testing.T) {
	h, c := drainState(t)
	// An operator drains the node in FeRun; Kubernetes knows nothing.
	h.ferun.setNode(FeRunNode{Node: node1, Status: nodeReady, Draining: true, StatusDescription: "drained"})
	h.steady(c)
	if n, _ := h.ferun.node(node1); !n.Draining {
		t.Fatal("the operator's drain was undone")
	}
	// A cordon now waits for that drain and asks for none; the uncordon
	// leaves it.
	h.kube.setUnschedulable(node1, true)
	h.pass(c)
	if _, ok := h.guard(node1); ok {
		t.Fatal("the guard stays")
	}
	if slices.ContainsFunc(h.writes(), func(w string) bool { return strings.HasSuffix(w, "/drain") }) {
		t.Fatal("asked to drain a draining node")
	}
	h.kube.setUnschedulable(node1, false)
	h.pass(c)
	if n, _ := h.ferun.node(node1); !n.Draining {
		t.Fatal("the uncordon undid the operator's drain")
	}
}

func TestRemoval(t *testing.T) {
	h := newHarness(t)
	h.cfg.RemoveAfter = time.Minute
	gone := prefix + "gone-1"
	h.kube.addNode(node1, feosLabels)
	h.ferun.setNode(FeRunNode{Node: node1, Status: nodeDown})                // its Kubernetes node is there
	h.ferun.setNode(FeRunNode{Node: gone, Status: nodeDown})                 // gone
	h.ferun.setNode(FeRunNode{Node: prefix + "ready", Status: nodeReady})    // reports, whatever Kubernetes says
	h.ferun.setNode(FeRunNode{Node: "other-cluster-node", Status: nodeDown}) // not this cluster's
	h.ferun.setNode(FeRunNode{Node: prefix + "old", Status: nodeRemoved})    // done
	c := h.controller()

	h.pass(c)
	if n, _ := h.ferun.node(gone); n.Status != nodeDown {
		t.Fatal("removed before --remove-after")
	}
	h.advance(time.Minute)
	h.pass(c)
	for name, want := range map[string]string{node1: nodeDown, gone: nodeRemoved, prefix + "ready": nodeReady,
		"other-cluster-node": nodeDown} {
		if n, _ := h.ferun.node(name); n.Status != want {
			t.Errorf("%s is %s; want %s", name, n.Status, want)
		}
	}
	if w := h.writes(); !slices.Contains(w, "DELETE /api/v1/admin/nodes/"+gone+"?force=true") {
		t.Fatalf("writes %q", w)
	}
	h.steady(c)
}

func TestCleanupOfGoneNodes(t *testing.T) {
	h, c := drainState(t)
	h.kube.addNode(node2, feosLabels)
	h.pass(c)
	if h.key(node2) == "" {
		t.Fatal("no key for the second node")
	}
	if _, ok := h.guard(node2); !ok {
		t.Fatal("no guard for the second node")
	}
	h.kube.deleteNode(node2)
	h.pass(c)
	if _, ok := h.guard(node2); ok {
		t.Fatal("the guard of a gone node stays")
	}
	if h.key(node2) != "" {
		t.Fatal("the key of a gone node stays")
	}
	if _, ok := h.guard(node1); !ok {
		t.Fatal("the other node's guard went")
	}
	h.steady(c)

	// A node that loses the label loses its guard too.
	h.kube.addNode(node1, nil)
	h.pass(c)
	if _, ok := h.guard(node1); ok {
		t.Fatal("the guard of a node outside the selector stays")
	}
}

func TestBadCredentials(t *testing.T) {
	h := newHarness(t)
	h.kube.addNode(node1, feosLabels)
	h.write("creds", "feos-k8s:wrong")
	c := h.controller()
	err := c.Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err %v; want FeRun's 401", err)
	}
	if w := h.writes(); len(w) > 0 {
		t.Fatalf("wrote %q without FeRun", w)
	}
	// The credentials are read again: a fixed secret works without a
	// restart.
	h.write("creds", "feos-k8s:s3cret")
	h.pass(c)
}

func TestRun(t *testing.T) {
	h := newHarness(t)
	h.kube.addNode(node1, feosLabels)
	h.ferun.setNode(FeRunNode{Node: node1, Status: nodeReady})
	h.cfg.Interval = 10 * time.Millisecond
	c := h.controller()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := h.guard(node1); ok && c.Healthy(time.Minute) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no guard from the loop")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestIsNodeName(t *testing.T) {
	for name, want := range map[string]bool{node1: true, "a": true, "": false, "-a": false, "a-": false,
		"A": false, "a.b": false, strings.Repeat("a", 63): true, strings.Repeat("a", 64): false} {
		if isNodeName(name) != want {
			t.Errorf("isNodeName(%q) != %v", name, want)
		}
	}
}

// EKS names its nodes by their private DNS name: FeRun knows a node by
// the first label, Kubernetes by the whole name.
func TestNodeNamesWithADomain(t *testing.T) {
	h := newHarness(t)
	h.cfg.NodeNamePrefix = "ip-10-0-"
	h.cfg.RemoveAfter = time.Minute
	const node, name = "ip-10-0-1-2.eu-west-1.compute.internal", "ip-10-0-1-2"
	h.kube.addNode(node, feosLabels)
	c := h.controller()

	h.pass(c)
	key := h.key(name)
	if live := h.ferun.liveTokens(name); len(live) != 1 || key != live[0].secret {
		t.Fatalf("key %q, live tokens %+v; want one token pinned to %s under that key", key, live, name)
	}
	if g, ok := h.guard(node); !ok || g.Spec.NodeName != node {
		t.Fatalf("guard %+v, %v; want one bound to %s", g, ok, node)
	}
	h.ferun.useToken(key)
	h.ferun.setNode(FeRunNode{Node: name, Status: nodeReady})
	h.pass(c)
	if h.key(name) != "" {
		t.Fatal("the key stays after the node is ready")
	}
	h.steady(c)

	// A cordon drains the FeRun node and annotates the Kubernetes one.
	h.kube.setUnschedulable(node, true)
	h.pass(c)
	if n, _ := h.ferun.node(name); !n.Draining {
		t.Fatal("the cordoned node does not drain in FeRun")
	}
	if h.kube.node(node).Metadata.Annotations[AnnotationDrain] != "requested" {
		t.Fatal("no drain annotation")
	}

	// Gone from Kubernetes and down in FeRun: removed after RemoveAfter.
	h.kube.deleteNode(node)
	h.ferun.setNode(FeRunNode{Node: name, Status: nodeDown})
	h.pass(c)
	h.advance(time.Minute)
	h.pass(c)
	if n, _ := h.ferun.node(name); n.Status != nodeRemoved {
		t.Fatalf("%s is %s; want removed", name, n.Status)
	}
}

// Two nodes of one first label: FeOS runs on neither.
func TestNodeNameClash(t *testing.T) {
	h := newHarness(t)
	h.kube.addNode("ip-10-0-1-2.eu-west-1.compute.internal", feosLabels)
	h.kube.addNode("ip-10-0-1-2.eu-central-1.compute.internal", feosLabels)
	c := h.controller()
	h.pass(c)
	if len(h.ferun.liveTokens("ip-10-0-1-2")) != 0 || h.key("ip-10-0-1-2") != "" {
		t.Fatal("a token for a name two nodes share")
	}
	if !strings.Contains(h.logs.String(), "have one FeRun name") {
		t.Fatalf("no warning: %s", h.logs.String())
	}
	h.steady(c)
}
