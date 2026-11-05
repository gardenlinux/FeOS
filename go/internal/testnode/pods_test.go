// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/podid"
)

func create(t *testing.T, pc podv1.PodServiceClient, id string, spec *podv1.PodSpec) (*podv1.PodInfo, error) {
	t.Helper()
	resp, err := pc.CreatePod(context.Background(), &podv1.CreatePodRequest{PodId: id, Spec: spec})
	return resp.GetPod(), err
}

func TestCreateIsIdempotentBySpec(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	e.start("n1", in)
	pc := pods(t, in, "n1")

	id := podid.Mint()
	spec := sandboxSpec(id, "inc-1")
	first, err := create(t, pc, id.String(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != podv1.PodState_POD_STATE_PENDING || first.Ipv6Address == "" || first.Ipv4Address == "" {
		t.Fatalf("created %v", first)
	}
	again, err := create(t, pc, id.String(), spec)
	if err != nil || again.Ipv6Address != first.Ipv6Address || !proto.Equal(again.Spec, first.Spec) {
		t.Fatalf("repeated create: %v, %v", again, err)
	}
	// Another incarnation's labels are another spec.
	_, err = create(t, pc, id.String(), sandboxSpec(id, "inc-2"))
	wantCode(t, err, codes.AlreadyExists, "pod "+id.String()+" exists with another spec")
	waitState(t, pc, id.String(), podv1.PodState_POD_STATE_RUNNING)
	// A running pod is answered as it is.
	running, err := create(t, pc, id.String(), spec)
	if err != nil || running.State != podv1.PodState_POD_STATE_RUNNING {
		t.Fatalf("create of a running pod: %v, %v", running, err)
	}

	// The defaults are part of the normalised spec.
	id2 := podid.Mint()
	bare := &podv1.PodSpec{Containers: []*podv1.ContainerSpec{{Name: "main", ImageRef: "alpine"}}}
	if _, err := create(t, pc, id2.String(), bare); err != nil {
		t.Fatal(err)
	}
	full := proto.Clone(bare).(*podv1.PodSpec)
	full.Resources = &podv1.Resources{Vcpus: 1, MemoryMib: 512}
	if _, err := create(t, pc, id2.String(), full); err != nil {
		t.Fatalf("the defaults differ: %v", err)
	}

	_, err = create(t, pc, "not-a-uuid", spec)
	wantCode(t, err, codes.InvalidArgument, `pod ID "not-a-uuid" is neither a short pod ID nor a UUID`)
	big := sandboxSpec(id, "x")
	big.Resources.Vcpus = 33
	_, err = create(t, pc, uuid.NewString(), big)
	wantCode(t, err, codes.InvalidArgument, "a pod has at most 32 vCPUs")
	noImage := sandboxSpec(id, "x")
	noImage.Containers[0].ImageRef = ""
	_, err = create(t, pc, uuid.NewString(), noImage)
	wantCode(t, err, codes.InvalidArgument, `container "main" has no image`)
}

// The clear part of a pod's HTTP bindings (ADR 0028) is the record's:
// SetPodHttpBindings sets it whole, running or not, and a create delivered
// again with another one takes it instead of refusing.
func TestSetPodHttpBindings(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	e.start("n1", in)
	pc := pods(t, in, "n1")

	id := podid.Mint()
	spec := sandboxSpec(id, "inc-1")
	spec.HttpBindings = &podv1.PodHttpBindings{Names: []string{"erp"}, Version: "v1"}
	if _, err := create(t, pc, id.String(), spec); err != nil {
		t.Fatal(err)
	}
	part := func() *podv1.PodHttpBindings {
		t.Helper()
		info, err := pc.GetPod(t.Context(), &podv1.GetPodRequest{PodId: id.String()})
		if err != nil {
			t.Fatal(err)
		}
		return info.GetSpec().GetHttpBindings()
	}
	if p := part(); p.GetVersion() != "v1" {
		t.Fatalf("the create's part: %v", p)
	}
	v2 := &podv1.PodHttpBindings{Names: []string{"erp", "pay"}, InterceptHosts: []string{"api.erp.example"},
		InterceptCaPems: []string{"-----BEGIN CERTIFICATE-----"}, Version: "v2"}
	if _, err := pc.SetPodHttpBindings(t.Context(), &podv1.SetPodHttpBindingsRequest{PodId: id.String(),
		HttpBindings: v2}); err != nil {
		t.Fatal(err)
	}
	if p := part(); !proto.Equal(p, v2) {
		t.Fatalf("the set part: %v", p)
	}
	again, err := create(t, pc, id.String(), spec)
	if err != nil || again.GetSpec().GetHttpBindings().GetVersion() != "v1" || part().GetVersion() != "v1" {
		t.Fatalf("a create again with another part: %v, %v", again, err)
	}
	if _, err := pc.SetPodHttpBindings(t.Context(), &podv1.SetPodHttpBindingsRequest{PodId: id.String(),
		HttpBindings: &podv1.PodHttpBindings{}}); err != nil {
		t.Fatal(err)
	}
	if p := part(); p != nil {
		t.Fatalf("an empty part: %v", p)
	}
	_, err = pc.SetPodHttpBindings(t.Context(), &podv1.SetPodHttpBindingsRequest{PodId: podid.Mint().String()})
	wantPrefix(t, err, codes.NotFound, "no pod ")
}

func TestTAPPrefixCollision(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	e.start("n1", in)
	pc := pods(t, in, "n1")
	a := "abcdef12-0000-4000-8000-000000000001"
	b := "abcdef12-1111-4000-8000-000000000002"
	if _, err := create(t, pc, a, sandboxSpec(podid.ID(a), "1")); err != nil {
		t.Fatal(err)
	}
	_, err := create(t, pc, b, sandboxSpec(podid.ID(b), "1"))
	wantCode(t, err, codes.AlreadyExists, "pod "+a+" has the TAP device pod-abcdef12 already: a short ID names its own, a UUID's first 8 hex digits name it")
	if _, err := pc.GetPod(context.Background(), &podv1.GetPodRequest{PodId: b}); status.Code(err) != codes.NotFound {
		t.Fatalf("the colliding pod was recorded: %v", err)
	}
	// Without a network there is no TAP device.
	none := sandboxSpec(podid.ID(b), "1")
	none.Network = &podv1.PodNetwork{None: true}
	info, err := create(t, pc, b, none)
	if err != nil || info.Ipv6Address != "" {
		t.Fatalf("a pod without a network: %v, %v", info, err)
	}
	// A short ID names its TAP device whole (ADR 0019): short IDs never
	// collide, also not one whose first 8 characters are a UUID's first 8
	// hex digits.
	for _, id := range []string{"abcdef12xyz", "abcdef12xyw", "abcdef12000"} {
		info, err := create(t, pc, id, sandboxSpec(podid.ID(id), "1"))
		if err != nil || info.Ipv6Address == "" {
			t.Fatalf("short ID %s: %v, %v", id, info, err)
		}
	}
	// An ID that is neither a short ID nor a UUID is refused.
	for _, id := range []string{"Abcdef12xyz", "abcdef12xyi", "1bcdef12xyz", "abcdef12xy"} {
		_, err := create(t, pc, id, sandboxSpec(podid.ID(id), "1"))
		wantCode(t, err, codes.InvalidArgument, fmt.Sprintf("pod ID %q is neither a short pod ID nor a UUID", id))
	}
}

func TestLifecyclePreconditions(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in)
	pc := pods(t, in, "n1")
	ctx := context.Background()
	uid, _ := createRunning(t, pc)
	id := uid.String()

	_, err := pc.ResumePod(ctx, &podv1.ResumePodRequest{PodId: id})
	wantCode(t, err, codes.FailedPrecondition, "pod "+id+" is POD_STATE_RUNNING, not POD_STATE_SUSPENDED")
	if _, err := pc.StartPod(ctx, &podv1.StartPodRequest{PodId: id}); err != nil {
		t.Fatalf("start of a running pod: %v", err)
	}

	s, err := pc.SuspendPod(ctx, &podv1.SuspendPodRequest{PodId: id})
	if err != nil || s.MemoryBytes != 1024<<20 {
		t.Fatalf("suspend: %v, %v", s, err)
	}
	if info := waitState(t, pc, id, podv1.PodState_POD_STATE_SUSPENDED); info.Reason != "suspended" {
		t.Fatalf("reason %q", info.Reason)
	}
	_, err = pc.SuspendPod(ctx, &podv1.SuspendPodRequest{PodId: id})
	wantCode(t, err, codes.FailedPrecondition, "pod "+id+" is POD_STATE_SUSPENDED, not POD_STATE_RUNNING")
	_, err = pc.StartPod(ctx, &podv1.StartPodRequest{PodId: id})
	wantCode(t, err, codes.FailedPrecondition, "pod "+id+" is suspended: resume it")
	if _, err := pc.ResumePod(ctx, &podv1.ResumePodRequest{PodId: id}); err != nil {
		t.Fatal(err)
	}
	resumed := waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING)

	if _, err := pc.StopPod(ctx, &podv1.StopPodRequest{PodId: id}); err != nil {
		t.Fatal(err)
	}
	if info := waitState(t, pc, id, podv1.PodState_POD_STATE_STOPPED); info.Reason != "stopped" {
		t.Fatalf("reason %q", info.Reason)
	}
	if _, err := pc.StopPod(ctx, &podv1.StopPodRequest{PodId: id}); err != nil {
		t.Fatalf("stop of a stopped pod: %v", err)
	}
	_, err = pc.SuspendPod(ctx, &podv1.SuspendPodRequest{PodId: id})
	wantCode(t, err, codes.FailedPrecondition, "pod "+id+" is POD_STATE_STOPPED, not POD_STATE_RUNNING")
	if _, err := pc.StartPod(ctx, &podv1.StartPodRequest{PodId: id}); err != nil {
		t.Fatal(err)
	}
	// A start boots a new microVM process; a resume kept the old one.
	if started := waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING); started.GetMicrovm().GetPid() == resumed.GetMicrovm().GetPid() ||
		resumed.GetMicrovm().GetPid() == 0 {
		t.Fatalf("the microVM's pid %d after the start, %d before", started.GetMicrovm().GetPid(), resumed.GetMicrovm().GetPid())
	}

	if _, err := pc.DeletePod(ctx, &podv1.DeletePodRequest{PodId: id}); err != nil {
		t.Fatal(err)
	}
	_, err = pc.GetPod(ctx, &podv1.GetPodRequest{PodId: id})
	wantCode(t, err, codes.NotFound, "no pod "+id)
	_, err = pc.DeletePod(ctx, &podv1.DeletePodRequest{PodId: id})
	wantCode(t, err, codes.NotFound, "no pod "+id)
	_, err = pc.StartPod(ctx, &podv1.StartPodRequest{PodId: id})
	wantCode(t, err, codes.NotFound, "no pod "+id)
	if _, err := os.Stat(n.PodDir(uid)); !os.IsNotExist(err) {
		t.Fatalf("the deleted pod's directory: %v", err)
	}
}

// A suspend of a pod that is still coming up waits behind its bring-up.
func TestSuspendWaitsBehindBringUp(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	e.start("n1", in, func(c *Config) { c.PendingDelay = 300 * time.Millisecond })
	pc := pods(t, in, "n1")
	id := podid.Mint()
	if _, err := create(t, pc, id.String(), sandboxSpec(id, "1")); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.SuspendPod(context.Background(), &podv1.SuspendPodRequest{PodId: id.String()}); err != nil {
		t.Fatalf("suspend behind the bring-up: %v", err)
	}
	waitState(t, pc, id.String(), podv1.PodState_POD_STATE_SUSPENDED)
}

func TestHibernateAndRestore(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	store := NewObjectStore("bucket")
	n := e.start("n1", in, func(c *Config) { c.ObjectStore = store })
	pc := pods(t, in, "n1")
	ctx := context.Background()
	uid, before := createRunning(t, pc)
	id := uid.String()
	if err := os.WriteFile(filepath.Join(n.PodDir(uid), "work.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}

	h, err := pc.HibernatePod(ctx, &podv1.HibernatePodRequest{PodId: id})
	if err != nil {
		t.Fatal(err)
	}
	cp := h.Checkpoint
	if !strings.HasPrefix(cp.HeadKey, "snapshots/"+id+"/") || !cp.Memory || h.MemoryBytes != 1024<<20 || h.DiskBytes != 4 {
		t.Fatalf("hibernate: %v", h)
	}
	info := waitState(t, pc, id, podv1.PodState_POD_STATE_HIBERNATED)
	if info.Reason != "hibernated" || info.Ipv6Address != "" || !proto.Equal(info.Checkpoint, cp) {
		t.Fatalf("hibernated: %v", info)
	}
	if _, err := os.Stat(n.PodDir(uid)); !os.IsNotExist(err) {
		t.Fatalf("the hibernated pod's directory stays: %v", err)
	}
	if got := store.Heads(); len(got) != 1 || got[0] != cp.HeadKey {
		t.Fatalf("heads %v", got)
	}
	again, err := pc.HibernatePod(ctx, &podv1.HibernatePodRequest{PodId: id})
	if err != nil || !proto.Equal(again.Checkpoint, cp) || again.MemoryBytes != 0 {
		t.Fatalf("hibernate of a hibernated pod: %v, %v", again, err)
	}
	_, err = pc.StartPod(ctx, &podv1.StartPodRequest{PodId: id})
	wantCode(t, err, codes.FailedPrecondition, "pod "+id+" is hibernated: resume or restore it, or stop it to boot it anew")

	// Its own head, with its memory.
	r, err := pc.RestorePod(ctx, &podv1.RestorePodRequest{PodId: id})
	if err != nil || !r.MemoryRestored || r.Pod.State != podv1.PodState_POD_STATE_RUNNING || r.Pod.Ipv6Address == "" {
		t.Fatalf("restore: %v, %v", r, err)
	}
	if data, err := os.ReadFile(filepath.Join(n.PodDir(uid), "work.txt")); err != nil || string(data) != "kept" {
		t.Fatalf("the restored disk: %q, %v", data, err)
	}
	// Restored already: a repeat whose answer was lost is answered.
	r2, err := pc.RestorePod(ctx, &podv1.RestorePodRequest{PodId: id, HeadKey: cp.HeadKey})
	if err != nil || r2.Pod.State != podv1.PodState_POD_STATE_RUNNING {
		t.Fatalf("repeated restore: %v, %v", r2, err)
	}
	_, err = pc.RestorePod(ctx, &podv1.RestorePodRequest{PodId: id, HeadKey: "snapshots/" + id + "/1/head.json"})
	wantCode(t, err, codes.FailedPrecondition, "pod "+id+" is POD_STATE_RUNNING, not hibernated")
	if !proto.Equal(before.Spec, r.Pod.Spec) {
		t.Fatal("the spec changed")
	}

	// A resume of a hibernated pod restores it.
	h2, err := pc.HibernatePod(ctx, &podv1.HibernatePodRequest{PodId: id})
	if err != nil || h2.Checkpoint.Epoch <= cp.Epoch || h2.Checkpoint.HeadKey == cp.HeadKey {
		t.Fatalf("second hibernation: %v, %v", h2, err)
	}
	if _, err := pc.ResumePod(ctx, &podv1.ResumePodRequest{PodId: id}); err != nil {
		t.Fatal(err)
	}
	waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING)

	// Cold: it boots anew on the checkpoint's disk.
	if _, err := pc.HibernatePod(ctx, &podv1.HibernatePodRequest{PodId: id}); err != nil {
		t.Fatal(err)
	}
	cold, err := pc.RestorePod(ctx, &podv1.RestorePodRequest{PodId: id, Cold: true})
	if err != nil || cold.MemoryRestored || cold.Reason != "asked to boot anew" ||
		cold.Pod.State != podv1.PodState_POD_STATE_PENDING || cold.Pod.Reason != "booting anew on its disk: asked to boot anew" {
		t.Fatalf("cold restore: %v, %v", cold, err)
	}
	waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING)

	// A node that cannot load the snapshot boots anew too.
	n.Script().SetUnrestorable("the CPU model differs")
	if _, err := pc.HibernatePod(ctx, &podv1.HibernatePodRequest{PodId: id}); err != nil {
		t.Fatal(err)
	}
	unr, err := pc.RestorePod(ctx, &podv1.RestorePodRequest{PodId: id})
	if err != nil || unr.MemoryRestored || unr.Pod.Reason != "booting anew on its disk: the CPU model differs" {
		t.Fatalf("restore here: %v, %v", unr, err)
	}
	n.Script().SetUnrestorable("")
	waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING)

	// A stopped hibernated pod boots on its checkpoint's disk.
	if _, err := pc.HibernatePod(ctx, &podv1.HibernatePodRequest{PodId: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.StopPod(ctx, &podv1.StopPodRequest{PodId: id}); err != nil {
		t.Fatal(err)
	}
	if info := waitState(t, pc, id, podv1.PodState_POD_STATE_STOPPED); info.Reason != "stopped: its memory is dropped, its disk is its checkpoint's" {
		t.Fatalf("reason %q", info.Reason)
	}
	if _, err := pc.StartPod(ctx, &podv1.StartPodRequest{PodId: id}); err != nil {
		t.Fatal(err)
	}
	waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING)
	if data, err := os.ReadFile(filepath.Join(n.PodDir(uid), "work.txt")); err != nil || string(data) != "kept" {
		t.Fatalf("the checkpoint's disk: %q, %v", data, err)
	}
	eventually(t, 5*time.Second, "the store in telemetry", func() bool {
		r, ok := e.f.lastReport()
		return ok && r.t.ObjectStore && r.t.ObjectStoreId == store.ID()
	})
}

func TestHibernateWithoutObjectStore(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	e.start("n1", in)
	pc := pods(t, in, "n1")
	uid, _ := createRunning(t, pc)
	_, err := pc.HibernatePod(context.Background(), &podv1.HibernatePodRequest{PodId: uid.String()})
	wantCode(t, err, codes.FailedPrecondition, "diskd has no object store: pods do not hibernate on this node")
	waitState(t, pc, uid.String(), podv1.PodState_POD_STATE_RUNNING)
}

// A node that does not have a pod adopts it from its checkpoint, with the
// spec, labels and disk it had.
func TestRestoreAdoptsCheckpoint(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	store := NewObjectStore("shared")
	a := e.start("na", in, func(c *Config) { c.ObjectStore = store })
	b := e.start("nb", in, func(c *Config) { c.ObjectStore = store })
	e.start("nc", in)
	pa, pb, pcNone := pods(t, in, "na"), pods(t, in, "nb"), pods(t, in, "nc")
	ctx := context.Background()
	uid, before := createRunning(t, pa)
	id := uid.String()
	if err := os.WriteFile(filepath.Join(a.PodDir(uid), "notes"), []byte("moved"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := pa.HibernatePod(ctx, &podv1.HibernatePodRequest{PodId: id})
	if err != nil {
		t.Fatal(err)
	}
	key := h.Checkpoint.HeadKey

	_, err = pb.RestorePod(ctx, &podv1.RestorePodRequest{PodId: id})
	wantCode(t, err, codes.NotFound, "no pod "+id+": name its checkpoint's head to restore it here")
	_, err = pb.RestorePod(ctx, &podv1.RestorePodRequest{PodId: id, HeadKey: "snapshots/" + id + "/1/head.json"})
	wantCode(t, err, codes.NotFound, "diskd: no head snapshots/"+id+"/1/head.json")
	other := uuid.NewString()
	_, err = pb.RestorePod(ctx, &podv1.RestorePodRequest{PodId: other, HeadKey: key})
	wantCode(t, err, codes.InvalidArgument, key+" is pod "+id+"'s, not "+other+"'s")
	_, err = pcNone.RestorePod(ctx, &podv1.RestorePodRequest{PodId: id, HeadKey: key})
	wantCode(t, err, codes.FailedPrecondition, "diskd: S3 not configured")

	r, err := pb.RestorePod(ctx, &podv1.RestorePodRequest{PodId: id, HeadKey: key})
	if err != nil || !r.MemoryRestored || r.Pod.State != podv1.PodState_POD_STATE_RUNNING {
		t.Fatalf("restore on another node: %v, %v", r, err)
	}
	if !proto.Equal(r.Pod.Spec, before.Spec) || r.Pod.Spec.Labels["ferun.incarnation"] != before.Spec.Labels["ferun.incarnation"] {
		t.Fatalf("adopted spec %v, was %v", r.Pod.Spec, before.Spec)
	}
	if data, err := os.ReadFile(filepath.Join(b.PodDir(uid), "notes")); err != nil || string(data) != "moved" {
		t.Fatalf("the adopted disk: %q, %v", data, err)
	}
	// Its record on the first node stays, hibernated.
	waitState(t, pa, id, podv1.PodState_POD_STATE_HIBERNATED)
}

// Every in-memory store has an ID of its own, since two share nothing; a
// store kept in a directory has one ID in every process that opens the
// directory.
func TestObjectStoreIDs(t *testing.T) {
	if a, b := NewObjectStore("bucket"), NewObjectStore("bucket"); a.ID() == b.ID() {
		t.Fatal("two in-memory stores report one ID")
	}
	dir := t.TempDir()
	d1, err := OpenObjectStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := OpenObjectStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	other, err := OpenObjectStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if d1.ID() != d2.ID() || d1.ID() == other.ID() || len(d1.ID()) != 64 {
		t.Fatalf("IDs %s, %s, other %s", d1.ID(), d2.ID(), other.ID())
	}
}

// Nodes whose stores open one directory, as test nodes in separate
// processes do, restore each other's checkpoints; a head key that names
// something outside the store is not found.
func TestRestoreFromADirectoryStore(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	dir := t.TempDir()
	sa, err := OpenObjectStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := OpenObjectStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := e.start("na", in, func(c *Config) { c.ObjectStore = sa })
	b := e.start("nb", in, func(c *Config) { c.ObjectStore = sb })
	pa, pb := pods(t, in, "na"), pods(t, in, "nb")
	ctx := context.Background()
	uid, before := createRunning(t, pa)
	id := uid.String()
	if err := os.MkdirAll(filepath.Join(a.PodDir(uid), "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.PodDir(uid), "work", "notes"), []byte("moved"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("work/notes", filepath.Join(a.PodDir(uid), "link")); err != nil {
		t.Fatal(err)
	}
	h, err := pa.HibernatePod(ctx, &podv1.HibernatePodRequest{PodId: id})
	if err != nil {
		t.Fatal(err)
	}
	key := h.Checkpoint.HeadKey
	if got := sb.Heads(); !slices.Equal(got, []string{key}) {
		t.Fatalf("heads seen by the other store: %v", got)
	}
	r, err := pb.RestorePod(ctx, &podv1.RestorePodRequest{PodId: id, HeadKey: key})
	if err != nil || !r.MemoryRestored || !proto.Equal(r.Pod.Spec, before.Spec) {
		t.Fatalf("restore on the other node: %v, %v", r, err)
	}
	if data, err := os.ReadFile(filepath.Join(b.PodDir(uid), "link")); err != nil || string(data) != "moved" {
		t.Fatalf("the restored disk: %q, %v", data, err)
	}
	if st, err := os.Stat(filepath.Join(b.PodDir(uid), "work", "notes")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the restored file: %v, %v", st, err)
	}
	other := uuid.NewString()
	for _, bad := range []string{"snapshots/../../" + other + "/1/head.json", "snapshots/" + other + "/1/../../../store-id", "store-id"} {
		_, err = pb.RestorePod(ctx, &podv1.RestorePodRequest{PodId: other, HeadKey: bad})
		wantCode(t, err, codes.NotFound, "diskd: no head "+bad)
	}
}

// With AbortOnCancel a cancelled call stops where feosd's does; without
// it, the call finishes.
func TestAbortOnCancel(t *testing.T) {
	for _, abort := range []bool{true, false} {
		t.Run(map[bool]string{true: "abort", false: "detached"}[abort], func(t *testing.T) {
			e := newEnv(t)
			in := e.instance("i1", caIss)
			store := NewObjectStore("s")
			n := e.start("n1", in, func(c *Config) { c.ObjectStore = store; c.AbortOnCancel = abort })
			pc := pods(t, in, "n1")
			uid, _ := createRunning(t, pc)
			id := uid.String()

			// SuspendPod cut while the snapshot is written.
			n.Script().SetDelay(podv1.PodService_SuspendPod_FullMethodName, time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			_, err := pc.SuspendPod(ctx, &podv1.SuspendPodRequest{PodId: id})
			cancel()
			wantCode(t, err, codes.DeadlineExceeded, "")
			if abort {
				time.Sleep(1500 * time.Millisecond)
				info := waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING)
				if info.Microvm.GuestAttached {
					t.Fatal("the guest of a cut suspend still answers")
				}
				_, err := execOut(t, in, "n1", id, "true")
				wantPrefix(t, err, codes.Unavailable, "the guest of pod "+id+": ")
				// The control plane repairs it: stop and start.
				if _, err := pc.StopPod(context.Background(), &podv1.StopPodRequest{PodId: id}); err != nil {
					t.Fatal(err)
				}
				if _, err := pc.StartPod(context.Background(), &podv1.StartPodRequest{PodId: id}); err != nil {
					t.Fatal(err)
				}
				waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING)
				if _, err := pc.SuspendPod(context.Background(), &podv1.SuspendPodRequest{PodId: id}); err != nil {
					t.Fatal(err)
				}
			} else {
				waitState(t, pc, id, podv1.PodState_POD_STATE_SUSPENDED)
			}
			n.Script().SetDelay(podv1.PodService_SuspendPod_FullMethodName, 0)
			if _, err := pc.HibernatePod(context.Background(), &podv1.HibernatePodRequest{PodId: id}); err != nil {
				t.Fatal(err)
			}

			// RestorePod cut while the memory loads.
			n.Script().SetDelay(podv1.PodService_RestorePod_FullMethodName, time.Second)
			ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
			_, err = pc.RestorePod(ctx, &podv1.RestorePodRequest{PodId: id})
			cancel()
			wantCode(t, err, codes.DeadlineExceeded, "")
			if abort {
				time.Sleep(1500 * time.Millisecond)
				info := waitState(t, pc, id, podv1.PodState_POD_STATE_PENDING)
				if !strings.HasPrefix(info.Reason, "restoring from snapshots/") {
					t.Fatalf("reason %q", info.Reason)
				}
				// No bring-up: it stays PENDING.
				never(t, 500*time.Millisecond, "the cut restore came up", func() bool {
					info, err := pc.GetPod(context.Background(), &podv1.GetPodRequest{PodId: id})
					return err != nil || info.State != podv1.PodState_POD_STATE_PENDING
				})
				// A repeated restore of its own head answers OK, and changes
				// nothing.
				r, err := pc.RestorePod(context.Background(), &podv1.RestorePodRequest{PodId: id})
				if err != nil || r.Pod.State != podv1.PodState_POD_STATE_PENDING {
					t.Fatalf("repeated restore: %v, %v", r, err)
				}

				// CreatePod cut after the pod is recorded: PENDING without a
				// bring-up, which a repeated create does not change.
				n.Script().SetDelay(podv1.PodService_CreatePod_FullMethodName, time.Second)
				fresh := podid.Mint()
				ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
				_, err = pc.CreatePod(ctx, &podv1.CreatePodRequest{PodId: fresh.String(), Spec: sandboxSpec(fresh, "1")})
				cancel()
				wantCode(t, err, codes.DeadlineExceeded, "")
				n.Script().SetDelay(podv1.PodService_CreatePod_FullMethodName, 0)
				if info, err := create(t, pc, fresh.String(), sandboxSpec(fresh, "1")); err != nil || info.State != podv1.PodState_POD_STATE_PENDING {
					t.Fatalf("repeated create: %v, %v", info, err)
				}
				never(t, 500*time.Millisecond, "the cut create came up", func() bool {
					info, err := pc.GetPod(context.Background(), &podv1.GetPodRequest{PodId: fresh.String()})
					return err != nil || info.State != podv1.PodState_POD_STATE_PENDING
				})
			} else {
				waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING)
			}
		})
	}
}

// podCtx is a context whose calls go to the pod's guest.
func podCtx(id string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), PodIDMetadata, id)
}

// execOut runs a command in the pod's guest and collects what it wrote.
func execOut(t *testing.T, in *instance, node, id string, argv ...string) (execResult, error) {
	t.Helper()
	s, err := containerv1.NewContainerServiceClient(conn(t, in, node)).Exec(podCtx(id), &containerv1.ExecRequest{ContainerId: "main", Command: argv, WorkingDir: "/"})
	if err != nil {
		return execResult{}, err
	}
	var r execResult
	for {
		f, err := s.Recv()
		if err == io.EOF {
			return r, nil
		}
		if err != nil {
			return r, err
		}
		switch fr := f.Frame.(type) {
		case *containerv1.ExecStreamFrame_Stdout:
			r.stdout += string(fr.Stdout)
		case *containerv1.ExecStreamFrame_Stderr:
			r.stderr += string(fr.Stderr)
		case *containerv1.ExecStreamFrame_ExitCode:
			r.code, r.exited = fr.ExitCode, true
		}
	}
}

type execResult struct {
	stdout, stderr string
	code           int32
	exited         bool
}

func TestStreamPodEvents(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in)
	pc := pods(t, in, "n1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	all, err := pc.StreamPodEvents(ctx, &podv1.StreamPodEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	other := podid.Mint()
	only, err := pc.StreamPodEvents(ctx, &podv1.StreamPodEventsRequest{PodId: other.String()})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // both subscribed before the events
	uid, _ := createRunning(t, pc)
	id := uid.String()
	for _, want := range []podv1.PodState{podv1.PodState_POD_STATE_PENDING, podv1.PodState_POD_STATE_RUNNING} {
		ev, err := all.Recv()
		if err != nil || ev.PodId != id || ev.State != want {
			t.Fatalf("event %v, %v; want %s", ev, err, want)
		}
	}
	if _, err := pc.DeletePod(context.Background(), &podv1.DeletePodRequest{PodId: id}); err != nil {
		t.Fatal(err)
	}
	if ev, err := all.Recv(); err != nil || !ev.Deleted || ev.State != podv1.PodState_POD_STATE_RUNNING {
		t.Fatalf("deleted event %v, %v", ev, err)
	}
	if _, err := create(t, pc, other.String(), sandboxSpec(other, "1")); err != nil {
		t.Fatal(err)
	}
	if ev, err := only.Recv(); err != nil || ev.PodId != other.String() {
		t.Fatalf("filtered stream: %v, %v", ev, err)
	}
	// A reader that fell behind gets DATA_LOSS.
	n.Script().LagPodEvents()
	for {
		_, err := all.Recv()
		if err != nil {
			wantCode(t, err, codes.DataLoss, "1 events were missed")
			break
		}
	}
	// Failed by the script, as a crashed microVM.
	if err := n.Script().FailPod(other, "the microVM crashed: signal 9"); err != nil {
		t.Fatal(err)
	}
	if info := waitState(t, pc, other.String(), podv1.PodState_POD_STATE_FAILED); info.Reason != "the microVM crashed: signal 9" {
		t.Fatalf("reason %q", info.Reason)
	}
}

// A scripted bring-up fails a pod, or keeps it PENDING until a stop ends
// it.
func TestScriptedBringUp(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in)
	pc := pods(t, in, "n1")
	ctx := context.Background()
	stuck := podid.Mint()
	n.Script().SetBringUp(func(ctx context.Context, pod podid.ID) error {
		if pod == stuck {
			<-ctx.Done()
			return ctx.Err()
		}
		return status.Error(codes.Internal, "container main: image not found")
	})
	failed := podid.Mint()
	if _, err := create(t, pc, failed.String(), sandboxSpec(failed, "1")); err != nil {
		t.Fatal(err)
	}
	if info := waitState(t, pc, failed.String(), podv1.PodState_POD_STATE_FAILED); !strings.Contains(info.Reason, "container main: image not found") {
		t.Fatalf("reason %q", info.Reason)
	}
	if _, err := create(t, pc, stuck.String(), sandboxSpec(stuck, "1")); err != nil {
		t.Fatal(err)
	}
	never(t, 300*time.Millisecond, "the stuck pod came up", func() bool {
		info, err := pc.GetPod(ctx, &podv1.GetPodRequest{PodId: stuck.String()})
		return err != nil || info.State != podv1.PodState_POD_STATE_PENDING
	})
	n.Script().SetBringUp(nil)
	if _, err := pc.StopPod(ctx, &podv1.StopPodRequest{PodId: stuck.String()}); err != nil {
		t.Fatal(err)
	}
	waitState(t, pc, stuck.String(), podv1.PodState_POD_STATE_STOPPED)
	if _, err := pc.StartPod(ctx, &podv1.StartPodRequest{PodId: stuck.String()}); err != nil {
		t.Fatal(err)
	}
	waitState(t, pc, stuck.String(), podv1.PodState_POD_STATE_RUNNING)

	// A hook fails a call before anything happens.
	n.Script().SetHook(podv1.PodService_CreatePod_FullMethodName, func(context.Context, string, any) error {
		return status.Error(codes.ResourceExhausted, "no address: the range is used up")
	})
	_, err := create(t, pc, uuid.NewString(), sandboxSpec(podid.Mint(), "1"))
	wantCode(t, err, codes.ResourceExhausted, "no address: the range is used up")
	n.Script().SetHook(podv1.PodService_CreatePod_FullMethodName, nil)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestInventoryHash(t *testing.T) {
	a := "3f2c0000-0000-4000-8000-000000000002"
	b := "1a2b0000-0000-4000-8000-000000000001"
	got := InventoryHash([]*podv1.PodInfo{
		{PodId: strings.ToUpper(a), State: podv1.PodState_POD_STATE_SUSPENDED},
		{PodId: b, State: podv1.PodState_POD_STATE_RUNNING},
		{PodId: uuid.NewString(), State: podv1.PodState_POD_STATE_PENDING},
		{PodId: uuid.NewString(), State: podv1.PodState_POD_STATE_HIBERNATED},
	})
	want := sha256Hex(b + " POD_STATE_RUNNING\n" + a + " POD_STATE_SUSPENDED\n")
	if got != want {
		t.Fatalf("hash %s, want %s", got, want)
	}
	if InventoryHash(nil) != sha256Hex("") {
		t.Fatal("empty hash")
	}
}

// holder holds the first call that reaches a hook point.
type holder struct {
	arrived chan struct{} // closed when the first call is held
	ended   chan struct{} // closed when it was let go
	byCtx   atomic.Bool   // its ctx ended before release
	out     chan struct{}
	once    sync.Once
}

// release lets the held call go.
func (h *holder) release() { h.once.Do(func() { close(h.out) }) }

// hold holds the first call that reaches the hook point name until
// release, or until its ctx ends; later calls pass.
func hold(n *Node, name string) *holder {
	h := &holder{arrived: make(chan struct{}), ended: make(chan struct{}), out: make(chan struct{})}
	var first sync.Once
	n.Script().SetHook(name, func(ctx context.Context, _ string, _ any) error {
		held := false
		first.Do(func() { held = true })
		if !held {
			return nil
		}
		close(h.arrived)
		defer close(h.ended)
		select {
		case <-h.out:
		case <-ctx.Done():
			h.byCtx.Store(true)
		}
		return nil
	})
	return h
}

// waitFor waits for ch, or fails the test.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("not within 10s: %s", what)
	}
}

type hibernated struct {
	resp *podv1.HibernatePodResponse
	err  error
}

// hibernateAsync sends a HibernatePod and delivers its answer.
func hibernateAsync(ctx context.Context, pc podv1.PodServiceClient, req *podv1.HibernatePodRequest) <-chan hibernated {
	ch := make(chan hibernated, 1)
	go func() {
		resp, err := pc.HibernatePod(ctx, req)
		ch <- hibernated{resp, err}
	}()
	return ch
}

// resumeAsync sends a ResumePod and delivers its error.
func resumeAsync(pc podv1.PodServiceClient, id string) <-chan error {
	ch := make(chan error, 1)
	go func() {
		_, err := pc.ResumePod(context.Background(), &podv1.ResumePodRequest{PodId: id})
		ch <- err
	}()
	return ch
}

func answer[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("not within 10s: %s", what)
	}
	var zero T
	return zero
}

// notYet checks that ch delivers nothing for d.
func notYet[T any](t *testing.T, ch <-chan T, d time.Duration, what string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("happened: %s (%v)", what, v)
	case <-time.After(d):
	}
}

// hibernationOf is the pod's hibernation in flight, as GetPod reports it.
func hibernationOf(t *testing.T, pc podv1.PodServiceClient, id string) *podv1.Hibernation {
	t.Helper()
	info, err := pc.GetPod(context.Background(), &podv1.GetPodRequest{PodId: id})
	if err != nil {
		t.Fatal(err)
	}
	return info.Hibernation
}

// waitHibernation waits until GetPod reports the pod's hibernation in
// phase, and returns it.
func waitHibernation(t *testing.T, pc podv1.PodServiceClient, id string, phase podv1.HibernationPhase) *podv1.Hibernation {
	t.Helper()
	var h *podv1.Hibernation
	eventually(t, 10*time.Second, fmt.Sprintf("pod %s hibernating in %s", id, phase), func() bool {
		h = hibernationOf(t, pc, id)
		return h.GetPhase() == phase
	})
	return h
}

const abandonedText = "pod %s is resumed: its hibernation was abandoned"

// A resume abandons a hibernation at its upload: the hibernation's call
// fails with ABORTED, the pod resumes from its snapshot on the node, and
// nothing is in the store.
func TestHibernationAbandonedByResume(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	store := NewObjectStore("s")
	n := e.start("n1", in, func(c *Config) { c.ObjectStore = store })
	pc := pods(t, in, "n1")
	uid, _ := createRunning(t, pc)
	id := uid.String()
	if err := os.WriteFile(filepath.Join(n.PodDir(uid), "work.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	up := hold(n, HibernateUpload)
	defer up.release()

	hib := hibernateAsync(context.Background(), pc, &podv1.HibernatePodRequest{PodId: id, RequestId: "a"})
	waitFor(t, up.arrived, "the upload")
	h := hibernationOf(t, pc, id)
	if h.GetPhase() != podv1.HibernationPhase_HIBERNATION_PHASE_UPLOADING_DISK || !h.Abandonable || h.Epoch == 0 || !slices.Equal(h.RequestIds, []string{"a"}) {
		t.Fatalf("hibernation %v", h)
	}
	list, err := pc.ListPods(context.Background(), &podv1.ListPodsRequest{})
	if err != nil || len(list.Pods) != 1 || !proto.Equal(list.Pods[0].Hibernation, h) {
		t.Fatalf("ListPods: %v, %v", list, err)
	}
	if info := waitState(t, pc, id, podv1.PodState_POD_STATE_SUSPENDED); info.Reason != "suspended" {
		t.Fatalf("reason %q", info.Reason)
	}

	if _, err := pc.ResumePod(context.Background(), &podv1.ResumePodRequest{PodId: id}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	waitFor(t, up.ended, "the upload's end")
	if !up.byCtx.Load() {
		t.Fatal("the upload's ctx did not end")
	}
	r := answer(t, hib, "the hibernation's answer")
	wantCode(t, r.err, codes.Aborted, fmt.Sprintf(abandonedText, id))
	info := waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING)
	if info.Hibernation != nil || info.Checkpoint != nil || info.Ipv6Address == "" {
		t.Fatalf("resumed: %v", info)
	}
	if heads := store.Heads(); len(heads) != 0 {
		t.Fatalf("heads %v", heads)
	}
	if data, err := os.ReadFile(filepath.Join(n.PodDir(uid), "work.txt")); err != nil || string(data) != "kept" {
		t.Fatalf("the pod's disk: %q, %v", data, err)
	}
	// A second resume finds it running.
	_, err = pc.ResumePod(context.Background(), &podv1.ResumePodRequest{PodId: id})
	wantCode(t, err, codes.FailedPrecondition, "pod "+id+" is POD_STATE_RUNNING, not POD_STATE_SUSPENDED")
	// A late call of the abandoned hibernation starts nothing.
	_, err = pc.HibernatePod(context.Background(), &podv1.HibernatePodRequest{PodId: id, RequestId: "a"})
	wantCode(t, err, codes.Aborted, fmt.Sprintf(abandonedText, id))
	if info := waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING); info.Hibernation != nil {
		t.Fatalf("a hibernation started: %v", info)
	}
}

// A HibernatePod that comes while a hibernation runs joins it: one
// hibernation, one head, the same answer to all; a request id is kept
// once.
func TestHibernationJoins(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	store := NewObjectStore("s")
	n := e.start("n1", in, func(c *Config) { c.ObjectStore = store })
	pc := pods(t, in, "n1")
	uid, _ := createRunning(t, pc)
	id := uid.String()
	up := hold(n, HibernateUpload)
	defer up.release()
	var headPoints atomic.Int32
	n.Script().SetHook(HibernateHead, func(context.Context, string, any) error {
		headPoints.Add(1)
		return nil
	})

	var calls atomic.Int32
	n.Script().SetHook(podv1.PodService_HibernatePod_FullMethodName, func(context.Context, string, any) error {
		calls.Add(1)
		return nil
	})

	first := hibernateAsync(context.Background(), pc, &podv1.HibernatePodRequest{PodId: id, RequestId: "a"})
	waitFor(t, up.arrived, "the upload")
	second := hibernateAsync(context.Background(), pc, &podv1.HibernatePodRequest{PodId: id, RequestId: "b"})
	third := hibernateAsync(context.Background(), pc, &podv1.HibernatePodRequest{PodId: id, RequestId: "c"})
	// The second and the third call race: their ids come in either order.
	eventually(t, 10*time.Second, "three request ids", func() bool {
		return slices.Equal(slices.Sorted(slices.Values(hibernationOf(t, pc, id).GetRequestIds())), []string{"a", "b", "c"})
	})
	// A fourth call with the first one's id joins and adds no id; the
	// hook runs just before the handler, which the wait leaves time for.
	fourth := hibernateAsync(context.Background(), pc, &podv1.HibernatePodRequest{PodId: id, RequestId: "a"})
	eventually(t, 10*time.Second, "the fourth call", func() bool { return calls.Load() == 4 })
	notYet(t, second, 200*time.Millisecond, "a joined call answered before the hibernation ended")
	if ids := slices.Sorted(slices.Values(hibernationOf(t, pc, id).GetRequestIds())); !slices.Equal(ids, []string{"a", "b", "c"}) {
		t.Fatalf("request ids %v", ids)
	}
	up.release()
	a, b := answer(t, first, "the first call"), answer(t, second, "the second call")
	c, d := answer(t, third, "the third call"), answer(t, fourth, "the fourth call")
	if a.err != nil || b.err != nil || c.err != nil || d.err != nil || a.resp.MemoryBytes == 0 ||
		!proto.Equal(a.resp, b.resp) || !proto.Equal(a.resp, c.resp) || !proto.Equal(a.resp, d.resp) {
		t.Fatalf("answers %v, %v, %v, %v", a, b, c, d)
	}
	if heads := store.Heads(); len(heads) != 1 || heads[0] != a.resp.Checkpoint.HeadKey || headPoints.Load() != 1 {
		t.Fatalf("heads %v, %d hibernations", heads, headPoints.Load())
	}
	info := waitState(t, pc, id, podv1.PodState_POD_STATE_HIBERNATED)
	if info.Hibernation != nil {
		t.Fatalf("the hibernation stays: %v", info.Hibernation)
	}
	// After its end a HibernatePod starts anew, and finds it hibernated.
	again, err := pc.HibernatePod(context.Background(), &podv1.HibernatePodRequest{PodId: id, RequestId: "a"})
	if err != nil || !proto.Equal(again.Checkpoint, a.resp.Checkpoint) || again.MemoryBytes != 0 {
		t.Fatalf("hibernate of a hibernated pod: %v, %v", again, err)
	}
}

// A hibernation runs on its own: a call that is cancelled leaves it
// running, also with AbortOnCancel.
func TestHibernationOutlivesItsCall(t *testing.T) {
	for _, abort := range []bool{false, true} {
		t.Run(map[bool]string{true: "abort", false: "detached"}[abort], func(t *testing.T) {
			e := newEnv(t)
			in := e.instance("i1", caIss)
			store := NewObjectStore("s")
			n := e.start("n1", in, func(c *Config) { c.ObjectStore = store; c.AbortOnCancel = abort })
			pc := pods(t, in, "n1")
			uid, _ := createRunning(t, pc)
			id := uid.String()
			up := hold(n, HibernateUpload)
			defer up.release()

			ctx, cancel := context.WithCancel(context.Background())
			hib := hibernateAsync(ctx, pc, &podv1.HibernatePodRequest{PodId: id})
			waitFor(t, up.arrived, "the upload")
			cancel()
			wantCode(t, answer(t, hib, "the cancelled call").err, codes.Canceled, "")
			never(t, 200*time.Millisecond, "the hibernation ended with its call", func() bool {
				return hibernationOf(t, pc, id).GetPhase() != podv1.HibernationPhase_HIBERNATION_PHASE_UPLOADING_DISK
			})
			up.release()
			waitFor(t, up.ended, "the upload's end")
			if up.byCtx.Load() {
				t.Fatal("the upload's ctx ended")
			}
			info := waitState(t, pc, id, podv1.PodState_POD_STATE_HIBERNATED)
			// The slot ends after the state (as on feosd), so a GetPod may
			// still show the hibernation leaving here.
			if info.Checkpoint == nil {
				t.Fatalf("hibernated: %v", info)
			}
			eventually(t, 5*time.Second, "no hibernation", func() bool { return hibernationOf(t, pc, id) == nil })
			if heads := store.Heads(); len(heads) != 1 || heads[0] != info.Checkpoint.HeadKey {
				t.Fatalf("heads %v", heads)
			}
		})
	}
}

// A hibernation started with request id "a" and joined with "b" is
// abandoned: a later HibernatePod with either answers ABORTED and starts
// nothing; one with another id hibernates the pod.
func TestHibernationAbandonedRequestID(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	store := NewObjectStore("s")
	n := e.start("n1", in, func(c *Config) { c.ObjectStore = store })
	pc := pods(t, in, "n1")
	uid, _ := createRunning(t, pc)
	id := uid.String()
	up := hold(n, HibernateUpload)
	defer up.release()
	var seen []string
	var mu sync.Mutex
	n.Script().SetHook(HibernateHead, func(_ context.Context, _ string, req any) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, req.(*podv1.HibernatePodRequest).GetRequestId())
		return nil
	})

	a := hibernateAsync(context.Background(), pc, &podv1.HibernatePodRequest{PodId: id, RequestId: "a"})
	waitFor(t, up.arrived, "the upload")
	b := hibernateAsync(context.Background(), pc, &podv1.HibernatePodRequest{PodId: id, RequestId: "b"})
	eventually(t, 10*time.Second, "both request ids", func() bool {
		return slices.Equal(hibernationOf(t, pc, id).GetRequestIds(), []string{"a", "b"})
	})
	if _, err := pc.ResumePod(context.Background(), &podv1.ResumePodRequest{PodId: id}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	for _, r := range []hibernated{answer(t, a, "a"), answer(t, b, "b")} {
		wantCode(t, r.err, codes.Aborted, fmt.Sprintf(abandonedText, id))
	}
	waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING)
	for _, rid := range []string{"b", "a"} {
		_, err := pc.HibernatePod(context.Background(), &podv1.HibernatePodRequest{PodId: id, RequestId: rid})
		wantCode(t, err, codes.Aborted, fmt.Sprintf(abandonedText, id))
	}
	if info := waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING); info.Hibernation != nil || len(store.Heads()) != 0 {
		t.Fatalf("a hibernation started: %v, heads %v", info, store.Heads())
	}
	// Another id hibernates it, past the hold, which held only the first.
	c, err := pc.HibernatePod(context.Background(), &podv1.HibernatePodRequest{PodId: id, RequestId: "c"})
	if err != nil || c.Checkpoint == nil {
		t.Fatalf("hibernate c: %v, %v", c, err)
	}
	waitState(t, pc, id, podv1.PodState_POD_STATE_HIBERNATED)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, []string{"c"}) {
		t.Fatalf("hibernations that put a head: %v", seen)
	}
}

// A hibernation that may not be abandoned, because its first call or one
// that joined it says so, makes a resume wait; the resume then restores
// the pod from the new checkpoint.
func TestHibernationNoAbandon(t *testing.T) {
	for _, joined := range []bool{false, true} {
		t.Run(map[bool]string{false: "started", true: "joined"}[joined], func(t *testing.T) {
			e := newEnv(t)
			in := e.instance("i1", caIss)
			store := NewObjectStore("s")
			n := e.start("n1", in, func(c *Config) { c.ObjectStore = store })
			pc := pods(t, in, "n1")
			uid, _ := createRunning(t, pc)
			id := uid.String()
			up := hold(n, HibernateUpload)
			defer up.release()

			hib := hibernateAsync(context.Background(), pc, &podv1.HibernatePodRequest{PodId: id, RequestId: "drain", NoAbandon: !joined})
			waitFor(t, up.arrived, "the upload")
			var join <-chan hibernated
			if joined {
				if h := hibernationOf(t, pc, id); !h.GetAbandonable() {
					t.Fatalf("abandonable before the join: %v", h)
				}
				join = hibernateAsync(context.Background(), pc, &podv1.HibernatePodRequest{PodId: id, RequestId: "drain2", NoAbandon: true})
				eventually(t, 10*time.Second, "the join", func() bool { return len(hibernationOf(t, pc, id).GetRequestIds()) == 2 })
			}
			if h := hibernationOf(t, pc, id); h.GetAbandonable() || h.GetPhase() != podv1.HibernationPhase_HIBERNATION_PHASE_UPLOADING_DISK {
				t.Fatalf("hibernation %v", h)
			}
			resumed := resumeAsync(pc, id)
			notYet(t, resumed, 300*time.Millisecond, "the resume did not wait")
			up.release()
			waitFor(t, up.ended, "the upload's end")
			if up.byCtx.Load() {
				t.Fatal("the upload was abandoned")
			}
			r := answer(t, hib, "the hibernation")
			if r.err != nil || r.resp.Checkpoint == nil {
				t.Fatalf("hibernate: %v, %v", r.resp, r.err)
			}
			if joined {
				if j := answer(t, join, "the joined call"); j.err != nil || !proto.Equal(j.resp, r.resp) {
					t.Fatalf("joined: %v, %v", j.resp, j.err)
				}
			}
			if err := answer(t, resumed, "the resume"); err != nil {
				t.Fatalf("resume: %v", err)
			}
			info := waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING)
			if !proto.Equal(info.Checkpoint, r.resp.Checkpoint) || info.Hibernation != nil {
				t.Fatalf("restored: %v", info)
			}
		})
	}
}

// A resume that comes after the point of no return waits for the
// hibernation and restores the pod from its new checkpoint.
func TestHibernationTooLate(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	store := NewObjectStore("s")
	n := e.start("n1", in, func(c *Config) { c.ObjectStore = store })
	pc := pods(t, in, "n1")
	uid, _ := createRunning(t, pc)
	id := uid.String()
	up := hold(n, HibernateHead)
	defer up.release()

	hib := hibernateAsync(context.Background(), pc, &podv1.HibernatePodRequest{PodId: id, RequestId: "a"})
	waitFor(t, up.arrived, "the head")
	h := hibernationOf(t, pc, id)
	if h.GetPhase() != podv1.HibernationPhase_HIBERNATION_PHASE_HEAD || h.Abandonable || h.Epoch == 0 {
		t.Fatalf("hibernation %v", h)
	}
	resumed := resumeAsync(pc, id)
	notYet(t, resumed, 300*time.Millisecond, "the resume did not wait")
	up.release()
	r := answer(t, hib, "the hibernation")
	if r.err != nil || r.resp.Checkpoint.GetEpoch() != h.Epoch || !r.resp.Checkpoint.Memory {
		t.Fatalf("hibernate: %v, %v", r.resp, r.err)
	}
	if err := answer(t, resumed, "the resume"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	info := waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING)
	if !proto.Equal(info.Checkpoint, r.resp.Checkpoint) || info.Hibernation != nil {
		t.Fatalf("restored: %v", info)
	}
}

// A hibernation whose pod has no memory to resume ignores an abandon that
// was asked while it waited for the pod's lock and did not know that yet:
// it hibernates the disk, and the resume then restores the pod from it.
func TestHibernationNoMemoryIgnoresAbandon(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	store := NewObjectStore("s")
	logs := &logRecords{}
	n := e.start("n1", in, func(c *Config) { c.ObjectStore = store; c.Log = slog.New(logs) })
	pc := pods(t, in, "n1")
	ctx := context.Background()
	uid, _ := createRunning(t, pc)
	id := uid.String()
	if _, err := pc.StopPod(ctx, &podv1.StopPodRequest{PodId: id}); err != nil {
		t.Fatal(err)
	}
	// A bring-up holds the pod's lock, and fails the pod when let go.
	inBringUp, letGo := make(chan struct{}), make(chan struct{})
	var once sync.Once
	n.Script().SetBringUp(func(ctx context.Context, _ podid.ID) error {
		once.Do(func() { close(inBringUp) })
		select {
		case <-letGo:
		case <-ctx.Done():
		}
		return status.Error(codes.Internal, "container main: exited 1")
	})
	if _, err := pc.StartPod(ctx, &podv1.StartPodRequest{PodId: id}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, inBringUp, "the bring-up")

	hib := hibernateAsync(ctx, pc, &podv1.HibernatePodRequest{PodId: id, RequestId: "a"})
	if h := waitHibernation(t, pc, id, podv1.HibernationPhase_HIBERNATION_PHASE_WAITING); !h.Abandonable || !slices.Equal(h.RequestIds, []string{"a"}) {
		t.Fatalf("waiting hibernation %v", h)
	}
	resumed := resumeAsync(pc, id)
	eventually(t, 10*time.Second, "the resume asks for the abandon", func() bool {
		return logs.has("test node: a resume abandons a hibernation", id, podv1.HibernationPhase_HIBERNATION_PHASE_WAITING)
	})
	notYet(t, resumed, 100*time.Millisecond, "the resume did not wait")
	n.Script().SetBringUp(nil)
	close(letGo)

	r := answer(t, hib, "the hibernation")
	if r.err != nil || r.resp.Checkpoint == nil || r.resp.Checkpoint.Memory || r.resp.MemoryBytes != 0 {
		t.Fatalf("hibernate: %v, %v", r.resp, r.err)
	}
	if err := answer(t, resumed, "the resume"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	info := waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING)
	if !proto.Equal(info.Checkpoint, r.resp.Checkpoint) || info.Hibernation != nil {
		t.Fatalf("restored: %v", info)
	}
	if heads := store.Heads(); len(heads) != 1 {
		t.Fatalf("heads %v", heads)
	}
	// The abandon was ignored for good: nothing was abandoned, and the
	// hibernation's request id hibernates the restored pod again.
	if logs.has("test node: a hibernation is abandoned for a resume", id, nil) {
		t.Fatal("the hibernation was abandoned")
	}
	if again, err := pc.HibernatePod(ctx, &podv1.HibernatePodRequest{PodId: id, RequestId: "a"}); err != nil || again.Checkpoint.GetEpoch() <= r.resp.Checkpoint.GetEpoch() {
		t.Fatalf("hibernate again: %v, %v", again, err)
	}
}

// ResumeWaitsForHibernation plays feosd before a resume could abandon a
// hibernation: no PodInfo.Hibernation, a resume waits and restores, and a
// cancelled call cuts the hibernation with AbortOnCancel.
func TestResumeWaitsForHibernation(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	store := NewObjectStore("s")
	n := e.start("n1", in, func(c *Config) { c.ObjectStore = store; c.ResumeWaitsForHibernation = true })
	pc := pods(t, in, "n1")
	uid, _ := createRunning(t, pc)
	id := uid.String()
	up := hold(n, HibernateUpload)
	defer up.release()
	var heads atomic.Int32
	n.Script().SetHook(HibernateHead, func(context.Context, string, any) error {
		heads.Add(1)
		return nil
	})

	hib := hibernateAsync(context.Background(), pc, &podv1.HibernatePodRequest{PodId: id, RequestId: "a"})
	waitFor(t, up.arrived, "the upload")
	if info := waitState(t, pc, id, podv1.PodState_POD_STATE_SUSPENDED); info.Hibernation != nil {
		t.Fatalf("hibernation reported: %v", info.Hibernation)
	}
	list, err := pc.ListPods(context.Background(), &podv1.ListPodsRequest{})
	if err != nil || len(list.Pods) != 1 || list.Pods[0].Hibernation != nil {
		t.Fatalf("ListPods: %v, %v", list, err)
	}
	resumed := resumeAsync(pc, id)
	notYet(t, resumed, 300*time.Millisecond, "the resume did not wait")
	up.release()
	r := answer(t, hib, "the hibernation")
	if r.err != nil || r.resp.Checkpoint == nil {
		t.Fatalf("hibernate: %v, %v", r.resp, r.err)
	}
	if err := answer(t, resumed, "the resume"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	info := waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING)
	if !proto.Equal(info.Checkpoint, r.resp.Checkpoint) || heads.Load() != 1 {
		t.Fatalf("restored: %v, %d heads", info, heads.Load())
	}

	// With AbortOnCancel a cancelled call cuts it at the upload: the pod
	// stays suspended, no head.
	e2 := newEnv(t)
	in2 := e2.instance("i1", caIss)
	store2 := NewObjectStore("s2")
	n2 := e2.start("n1", in2, func(c *Config) {
		c.ObjectStore = store2
		c.ResumeWaitsForHibernation = true
		c.AbortOnCancel = true
	})
	pc2 := pods(t, in2, "n1")
	uid2, _ := createRunning(t, pc2)
	id2 := uid2.String()
	up2 := hold(n2, HibernateUpload)
	defer up2.release()
	ctx, cancel := context.WithCancel(context.Background())
	hib2 := hibernateAsync(ctx, pc2, &podv1.HibernatePodRequest{PodId: id2})
	waitFor(t, up2.arrived, "the upload")
	cancel()
	wantCode(t, answer(t, hib2, "the cancelled call").err, codes.Canceled, "")
	waitFor(t, up2.ended, "the upload's end")
	if !up2.byCtx.Load() {
		t.Fatal("the upload's ctx did not end")
	}
	never(t, 300*time.Millisecond, "the cut hibernation went on", func() bool {
		info, err := pc2.GetPod(context.Background(), &podv1.GetPodRequest{PodId: id2})
		return err != nil || info.State != podv1.PodState_POD_STATE_SUSPENDED
	})
	if len(store2.Heads()) != 0 {
		t.Fatalf("heads %v", store2.Heads())
	}
	if _, err := pc2.ResumePod(context.Background(), &podv1.ResumePodRequest{PodId: id2}); err != nil {
		t.Fatal(err)
	}
	waitState(t, pc2, id2, podv1.PodState_POD_STATE_RUNNING)
}

// logRecords is a slog.Handler that keeps the node's log records.
type logRecords struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (l *logRecords) Enabled(context.Context, slog.Level) bool { return true }

func (l *logRecords) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, r.Clone())
	return nil
}

func (l *logRecords) WithAttrs([]slog.Attr) slog.Handler { return l }

func (l *logRecords) WithGroup(string) slog.Handler { return l }

// has reports whether a record msg was logged for the pod, in phase
// unless phase is nil.
func (l *logRecords) has(msg, pod string, phase any) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.recs {
		if r.Message != msg {
			continue
		}
		podOK, phaseOK := false, phase == nil
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "pod":
				podOK = a.Value.String() == pod
			case "phase":
				phaseOK = phaseOK || a.Value.Any() == phase
			}
			return true
		})
		if podOK && phaseOK {
			return true
		}
	}
	return false
}

// A hibernation keeps the last maxRequestIDs request ids of its calls,
// each once, as feosd does: the newest caller's id is always among them.
func TestHibernationRequestIDsKeepTheLast(t *testing.T) {
	h := &hibernation{}
	var want []string
	for i := range maxRequestIDs + 2 {
		id := fmt.Sprintf("r%d", i)
		h.addRequest(id)
		want = append(want, id)
	}
	h.addRequest("")
	h.addRequest(fmt.Sprintf("r%d", maxRequestIDs+1))
	want = want[2:]
	if !slices.Equal(h.requestIDs, want) {
		t.Fatalf("request ids %v, want %v", h.requestIDs, want)
	}
}

// An ABORTED from the upload (diskd's, say) fails the hibernation but does
// not abandon it: its request id hibernates the pod afterwards.
func TestHibernationAbortedIsNotAbandoned(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	store := NewObjectStore("s")
	n := e.start("n1", in, func(c *Config) { c.ObjectStore = store })
	pc := pods(t, in, "n1")
	uid, _ := createRunning(t, pc)
	id := uid.String()
	n.Script().SetHook(HibernateUpload, func(context.Context, string, any) error {
		return status.Error(codes.Aborted, "diskd: the upload is aborted")
	})
	_, err := pc.HibernatePod(context.Background(), &podv1.HibernatePodRequest{PodId: id, RequestId: "a"})
	wantCode(t, err, codes.Aborted, "diskd: the upload is aborted")
	waitState(t, pc, id, podv1.PodState_POD_STATE_SUSPENDED)
	n.Script().SetHook(HibernateUpload, nil)
	resp, err := pc.HibernatePod(context.Background(), &podv1.HibernatePodRequest{PodId: id, RequestId: "a"})
	if err != nil || resp.Checkpoint == nil || !resp.Checkpoint.Memory {
		t.Fatalf("hibernate again: %v, %v", resp, err)
	}
	waitState(t, pc, id, podv1.PodState_POD_STATE_HIBERNATED)
}

// A hibernation whose pod was deleted and created anew under its id
// before it took the lock ends NOT_FOUND and leaves the new pod alone.
func TestHibernationOfAReplacedPod(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	store := NewObjectStore("s")
	n := e.start("n1", in, func(c *Config) { c.ObjectStore = store })
	pc := pods(t, in, "n1")
	uid, info := createRunning(t, pc)
	id := uid.String()
	p := n.pods
	p.mu.Lock()
	old := p.pods[id]
	h := &hibernation{done: make(chan struct{}), abandon: make(chan struct{}), phase: podv1.HibernationPhase_HIBERNATION_PHASE_WAITING}
	old.hib = h
	p.mu.Unlock()
	if _, err := pc.DeletePod(context.Background(), &podv1.DeletePodRequest{PodId: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := create(t, pc, id, info.Spec); err != nil {
		t.Fatal(err)
	}
	waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING)

	p.runHibernation(context.Background(), old, h, &podv1.HibernatePodRequest{PodId: id, RequestId: "a"})
	waitFor(t, h.done, "the hibernation's end")
	wantCode(t, h.err, codes.NotFound, "")
	if info := waitState(t, pc, id, podv1.PodState_POD_STATE_RUNNING); info.Hibernation != nil || len(store.Heads()) != 0 {
		t.Fatalf("the new pod: %v, heads %v", info, store.Heads())
	}
}
