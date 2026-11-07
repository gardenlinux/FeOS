// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"fmt"
	"io"
	"net"
	"slices"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	portforwardv1 "github.com/gardenlinux/feos/go/gen/feos/portforward/v1"
	volumev1 "github.com/gardenlinux/feos/go/gen/feos/volume/v1"
	"github.com/gardenlinux/feos/go/internal/podid"
)

var bgctx = context.Background()

// envSpec is the spec of an environment's pod: the env label, a volume
// and security groups as given.
func envSpec(id podid.ID, volume string, groups ...string) *podv1.PodSpec {
	s := sandboxSpec(id, "inc-1")
	s.Labels["ferun.environment"] = "0193e3a0-0000-7000-8000-000000000001"
	s.Network = &podv1.PodNetwork{SecurityGroups: groups}
	s.Containers[0].Command = nil
	if volume != "" {
		s.Volumes = []*podv1.PodVolume{{Name: "data", VolumeId: volume}}
		s.Containers[0].VolumeMounts = []*podv1.VolumeMount{{Volume: "data", Path: "/var/lib/data"}}
	}
	return s
}

func getVolume(t *testing.T, vc volumev1.VolumeServiceClient, id string) *volumev1.Volume {
	t.Helper()
	v, err := vc.GetVolume(bgctx, &volumev1.GetVolumeRequest{VolumeId: id})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func wantVolume(t *testing.T, v *volumev1.Volume, state volumev1.VolumeState, pod string) {
	t.Helper()
	if v.State != state || v.PodId != pod {
		t.Fatalf("volume %s is %s of pod %q, want %s of pod %q", v.VolumeId, v.State, v.PodId, state, pod)
	}
}

// A volume is created once by its ID, claimed by one pod at its create,
// let go of at its deletion, checkpointed with its hibernation and taken
// over by its restore, on another node too; one a pod has is not deleted.
func TestVolumes(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	objs := NewObjectStore("vols")
	n1 := e.start("n1", in, func(c *Config) { c.ObjectStore = objs })
	n2 := e.start("n2", in, func(c *Config) { c.ObjectStore = objs })
	pc1, pc2 := pods(t, in, "n1"), pods(t, in, "n2")
	vc1, vc2 := volumev1.NewVolumeServiceClient(conn(t, in, "n1")), volumev1.NewVolumeServiceClient(conn(t, in, "n2"))

	vol := uuid.NewString()
	labels := map[string]string{"ferun.object": "addon/postgres"}
	v, err := vc1.CreateVolume(bgctx, &volumev1.CreateVolumeRequest{VolumeId: vol, SizeBytes: 1 << 30, Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	wantVolume(t, v, volumev1.VolumeState_VOLUME_STATE_AVAILABLE, "")
	if _, err := vc1.CreateVolume(bgctx, &volumev1.CreateVolumeRequest{VolumeId: vol, SizeBytes: 1 << 30, Labels: labels}); err != nil {
		t.Fatalf("the same create again: %v", err)
	}
	_, err = vc1.CreateVolume(bgctx, &volumev1.CreateVolumeRequest{VolumeId: vol, SizeBytes: 2 << 30, Labels: labels})
	wantCode(t, err, codes.AlreadyExists, "volume "+vol+" exists with another size or labels")
	_, err = vc1.CreateVolume(bgctx, &volumev1.CreateVolumeRequest{VolumeId: vol, SizeBytes: 1 << 30})
	wantCode(t, err, codes.AlreadyExists, "volume "+vol+" exists with another size or labels")
	_, err = vc1.CreateVolume(bgctx, &volumev1.CreateVolumeRequest{VolumeId: uuid.NewString(), SizeBytes: 1 << 20})
	wantPrefix(t, err, codes.InvalidArgument, "a volume has from 64 MiB")
	_, err = vc1.CreateVolume(bgctx, &volumev1.CreateVolumeRequest{VolumeId: "NOT-A-UUID", SizeBytes: 1 << 30})
	wantPrefix(t, err, codes.InvalidArgument, "volume ID")

	// A pod with a volume the node does not have.
	missing := uuid.NewString()
	_, err = create(t, pc1, uuid.NewString(), envSpec(podid.Mint(), missing))
	wantCode(t, err, codes.FailedPrecondition, "no volume "+missing+" on this node")

	a := podid.Mint()
	if _, err := create(t, pc1, a.String(), envSpec(a, vol)); err != nil {
		t.Fatal(err)
	}
	wantVolume(t, getVolume(t, vc1, vol), volumev1.VolumeState_VOLUME_STATE_ATTACHED, a.String())
	// The same create again finds the pod and its volume.
	if _, err := create(t, pc1, a.String(), envSpec(a, vol)); err != nil {
		t.Fatalf("a repeated create: %v", err)
	}
	b := podid.Mint()
	_, err = create(t, pc1, b.String(), envSpec(b, vol))
	wantCode(t, err, codes.FailedPrecondition, "volume "+vol+" is attached to pod "+a.String())
	_, err = vc1.DeleteVolume(bgctx, &volumev1.DeleteVolumeRequest{VolumeId: vol})
	wantCode(t, err, codes.FailedPrecondition, "volume "+vol+" is attached to pod "+a.String())
	waitState(t, pc1, a.String(), podv1.PodState_POD_STATE_RUNNING)
	if !getVolume(t, vc1, vol).Formatted {
		t.Error("a volume a pod booted with has no filesystem")
	}

	// Deleting the pod lets go of it; another pod takes it.
	if _, err := pc1.DeletePod(bgctx, &podv1.DeletePodRequest{PodId: a.String()}); err != nil {
		t.Fatal(err)
	}
	wantVolume(t, getVolume(t, vc1, vol), volumev1.VolumeState_VOLUME_STATE_AVAILABLE, "")
	if _, err := create(t, pc1, b.String(), envSpec(b, vol)); err != nil {
		t.Fatal(err)
	}
	waitState(t, pc1, b.String(), podv1.PodState_POD_STATE_RUNNING)

	// A hibernation checkpoints it with the pod.
	hib, err := pc1.HibernatePod(bgctx, &podv1.HibernatePodRequest{PodId: b.String()})
	if err != nil {
		t.Fatal(err)
	}
	v = getVolume(t, vc1, vol)
	wantVolume(t, v, volumev1.VolumeState_VOLUME_STATE_CHECKPOINTED, b.String())
	if v.GetCheckpoint().GetEpoch() != hib.GetCheckpoint().GetEpoch() || v.GetCheckpoint().GetHeadKey() == "" {
		t.Fatalf("the volume's checkpoint %v, the pod's %v", v.GetCheckpoint(), hib.GetCheckpoint())
	}
	_, err = vc1.DeleteVolume(bgctx, &volumev1.DeleteVolumeRequest{VolumeId: vol})
	wantCode(t, err, codes.FailedPrecondition, "volume "+vol+" is attached to pod "+b.String())

	// A node that has the volume with data of its own refuses the restore.
	if _, err := vc2.CreateVolume(bgctx, &volumev1.CreateVolumeRequest{VolumeId: vol, SizeBytes: 1 << 30, Labels: labels}); err != nil {
		t.Fatal(err)
	}
	_, err = pc2.RestorePod(bgctx, &podv1.RestorePodRequest{PodId: b.String(), HeadKey: hib.GetCheckpoint().GetHeadKey()})
	wantPrefix(t, err, codes.FailedPrecondition, "volume "+vol+" is on this node with data its checkpoint may not have")
	// The refused restore left nothing on n2, as on feosd.
	_, err = pc2.GetPod(bgctx, &podv1.GetPodRequest{PodId: b.String()})
	wantPrefix(t, err, codes.NotFound, "")
	if _, err := vc2.DeleteVolume(bgctx, &volumev1.DeleteVolumeRequest{VolumeId: vol}); err != nil {
		t.Fatal(err)
	}
	_, err = vc2.GetVolume(bgctx, &volumev1.GetVolumeRequest{VolumeId: vol})
	wantCode(t, err, codes.NotFound, "no volume "+vol)

	// Restored on n2, the volume comes along; n1's pod goes and leaves
	// its record checkpointed without a pod.
	if _, err := pc2.RestorePod(bgctx, &podv1.RestorePodRequest{PodId: b.String(), HeadKey: hib.GetCheckpoint().GetHeadKey()}); err != nil {
		t.Fatal(err)
	}
	waitState(t, pc2, b.String(), podv1.PodState_POD_STATE_RUNNING)
	wantVolume(t, getVolume(t, vc2, vol), volumev1.VolumeState_VOLUME_STATE_ATTACHED, b.String())
	if _, err := pc1.DeletePod(bgctx, &podv1.DeletePodRequest{PodId: b.String()}); err != nil {
		t.Fatal(err)
	}
	wantVolume(t, getVolume(t, vc1, vol), volumev1.VolumeState_VOLUME_STATE_CHECKPOINTED, "")
	// Such a record may go, and a pod may boot on it (from its checkpoint).
	c := podid.Mint()
	if _, err := create(t, pc1, c.String(), envSpec(c, vol)); err != nil {
		t.Fatal(err)
	}
	wantVolume(t, getVolume(t, vc1, vol), volumev1.VolumeState_VOLUME_STATE_CHECKPOINTED, c.String())
	waitState(t, pc1, c.String(), podv1.PodState_POD_STATE_RUNNING)
	wantVolume(t, getVolume(t, vc1, vol), volumev1.VolumeState_VOLUME_STATE_ATTACHED, c.String())
	if got := n1.Volumes(); len(got) != 1 || got[0].VolumeId != vol {
		t.Errorf("n1's volumes %v", got)
	}
	if got := n2.Volumes(); len(got) != 1 {
		t.Errorf("n2's volumes %v", got)
	}
	list, err := vc2.ListVolumes(bgctx, &volumev1.ListVolumesRequest{})
	if err != nil || len(list.GetVolumes()) != 1 {
		t.Errorf("list %v, %v", list, err)
	}
}

func egress(cidr string, except ...string) *networkv1.Rule {
	return &networkv1.Rule{Direction: networkv1.Direction_DIRECTION_EGRESS,
		Peer: &networkv1.Rule_Block{Block: &networkv1.AddressBlock{Cidr: cidr, Except: except}}}
}

// Security groups are put whole and updated by deltas; a group a pod names
// stays; with CheckSecurityGroups a pod whose group is missing fails at
// its bring-up, and starts once the group is there.
func TestSecurityGroups(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in, func(c *Config) { c.CheckSecurityGroups = true })
	pc := pods(t, in, "n1")
	gc := networkv1.NewSecurityGroupServiceClient(conn(t, in, "n1"))

	const id = "env-0193e3a0-0000-7000-8000-000000000001"
	g := &networkv1.SecurityGroup{Id: id, Version: 3,
		Rules:   []*networkv1.Rule{egress("2000::/3", "2001:db8::/32"), {Direction: networkv1.Direction_DIRECTION_INGRESS, Peer: &networkv1.Rule_Group{Group: id}}},
		Members: []string{"fd00::0005", "fd00::3", "fd00::3"}}
	if _, err := gc.PutSecurityGroup(bgctx, &networkv1.PutSecurityGroupRequest{Group: g}); err != nil {
		t.Fatal(err)
	}
	got, err := gc.GetSecurityGroup(bgctx, &networkv1.GetSecurityGroupRequest{Id: id})
	if err != nil || got.Version != 3 || !slices.Equal(got.Members, []string{"fd00::3", "fd00::5"}) || len(got.Rules) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range []*networkv1.SecurityGroup{
		{Id: "a b"},
		{Id: "x", Members: []string{"10.0.0.1"}},
		{Id: "x", Rules: []*networkv1.Rule{egress("10.0.0.0/8", "192.168.0.0/16")}},
		{Id: "x", Rules: []*networkv1.Rule{{Direction: networkv1.Direction_DIRECTION_EGRESS}}},
		{Id: "x", Rules: []*networkv1.Rule{{Peer: &networkv1.Rule_Group{Group: "x"}}}},
	} {
		_, err := gc.PutSecurityGroup(bgctx, &networkv1.PutSecurityGroupRequest{Group: bad})
		wantPrefix(t, err, codes.InvalidArgument, "")
	}
	_, err = gc.UpdateSecurityGroup(bgctx, &networkv1.UpdateSecurityGroupRequest{Id: id, FromVersion: 2, Version: 4, AddMembers: []string{"fd00::9"}})
	wantCode(t, err, codes.FailedPrecondition, "security group "+id+" is at version 3, not 2")
	if _, err := gc.UpdateSecurityGroup(bgctx, &networkv1.UpdateSecurityGroupRequest{Id: id, FromVersion: 3, Version: 4,
		AddMembers: []string{"fd00::9"}, RemoveMembers: []string{"fd00::3"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := gc.GetSecurityGroup(bgctx, &networkv1.GetSecurityGroupRequest{Id: id}); got.Version != 4 ||
		!slices.Equal(got.Members, []string{"fd00::5", "fd00::9"}) {
		t.Fatalf("after the update %v", got)
	}

	// A pod naming a group the node does not have: created, then failed.
	const other = "sbx-0193e3a0-0000-7000-8000-000000000002"
	p := podid.Mint()
	if _, err := create(t, pc, p.String(), envSpec(p, "", id, other)); err != nil {
		t.Fatal(err)
	}
	info := waitState(t, pc, p.String(), podv1.PodState_POD_STATE_FAILED)
	if info.Reason != "the node does not have the security groups "+other {
		t.Fatalf("reason %q", info.Reason)
	}
	// Another list of groups is another spec.
	_, err = create(t, pc, p.String(), envSpec(p, "", id))
	wantCode(t, err, codes.AlreadyExists, "pod "+p.String()+" exists with another spec")
	_, err = create(t, pc, uuid.NewString(), envSpec(podid.Mint(), "", "no spaces"))
	wantPrefix(t, err, codes.InvalidArgument, "invalid security group ID")
	if _, err := gc.PutSecurityGroup(bgctx, &networkv1.PutSecurityGroupRequest{Group: &networkv1.SecurityGroup{Id: other, Version: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.StartPod(bgctx, &podv1.StartPodRequest{PodId: p.String()}); err != nil {
		t.Fatal(err)
	}
	waitState(t, pc, p.String(), podv1.PodState_POD_STATE_RUNNING)

	_, err = gc.DeleteSecurityGroup(bgctx, &networkv1.DeleteSecurityGroupRequest{Id: other})
	wantCode(t, err, codes.FailedPrecondition, "a pod on the node names security group "+other)
	if _, err := pc.DeletePod(bgctx, &podv1.DeletePodRequest{PodId: p.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := gc.DeleteSecurityGroup(bgctx, &networkv1.DeleteSecurityGroupRequest{Id: other}); err != nil {
		t.Fatal(err)
	}
	_, err = gc.DeleteSecurityGroup(bgctx, &networkv1.DeleteSecurityGroupRequest{Id: other})
	wantCode(t, err, codes.NotFound, "security group "+other+" is not on the node")
	list, err := gc.ListSecurityGroups(bgctx, &networkv1.ListSecurityGroupsRequest{})
	if err != nil || len(list.Groups) != 1 || list.Groups[0].Id != id || len(n.SecurityGroups()) != 1 {
		t.Fatalf("list %v, %v", list, err)
	}

	// Without the check the groups a pod names are not looked at.
	n2 := e.start("n2", in)
	pc2 := pods(t, in, "n2")
	q := podid.Mint()
	if _, err := create(t, pc2, q.String(), envSpec(q, "", "env-unknown")); err != nil {
		t.Fatal(err)
	}
	waitState(t, pc2, q.String(), podv1.PodState_POD_STATE_RUNNING)
	_ = n2
}

// A container's exit shows in GetPod while the pod runs, until the pod
// boots again; its log lines stream with tail_lines.
func TestContainerExitAndLogs(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in)
	pc := pods(t, in, "n1")
	id := podid.Mint()
	if _, err := create(t, pc, id.String(), envSpec(id, "")); err != nil {
		t.Fatal(err)
	}
	info := waitState(t, pc, id.String(), podv1.PodState_POD_STATE_RUNNING)
	if info.Ipv6Address == "" || len(info.Containers) != 1 || info.Containers[0].State != podv1.ContainerState_CONTAINER_STATE_RUNNING {
		t.Fatalf("running %v", info)
	}
	if list, _ := pc.ListPods(bgctx, &podv1.ListPodsRequest{}); len(list.GetPods()) != 1 || list.GetPods()[0].Ipv6Address != info.Ipv6Address {
		t.Fatalf("listed %v", list)
	}
	if err := n.Script().ExitContainer(id, "main", 3); err != nil {
		t.Fatal(err)
	}
	info, _ = pc.GetPod(bgctx, &podv1.GetPodRequest{PodId: id.String()})
	if info.State != podv1.PodState_POD_STATE_RUNNING || info.Containers[0].State != podv1.ContainerState_CONTAINER_STATE_EXITED ||
		info.Containers[0].ExitCode == nil || *info.Containers[0].ExitCode != 3 {
		t.Fatalf("after the exit %v", info)
	}
	if err := n.Script().ExitContainer(id, "main", -1); err != nil {
		t.Fatal(err)
	}
	if info, _ = pc.GetPod(bgctx, &podv1.GetPodRequest{PodId: id.String()}); info.Containers[0].ExitCode != nil {
		t.Fatalf("an exit without a code %v", info)
	}
	if err := n.Script().ExitContainer(id, "sidecar", 1); err == nil {
		t.Error("a container the pod does not have exited")
	}
	// A restart (StopPod, StartPod) boots it anew.
	if _, err := pc.StopPod(bgctx, &podv1.StopPodRequest{PodId: id.String()}); err != nil {
		t.Fatal(err)
	}
	if err := n.Script().ExitContainer(id, "main", 1); err == nil {
		t.Error("a stopped pod's container exited")
	}
	if _, err := pc.StartPod(bgctx, &podv1.StartPodRequest{PodId: id.String()}); err != nil {
		t.Fatal(err)
	}
	info = waitState(t, pc, id.String(), podv1.PodState_POD_STATE_RUNNING)
	if info.Containers[0].State != podv1.ContainerState_CONTAINER_STATE_RUNNING || info.Containers[0].ExitCode != nil {
		t.Fatalf("after the restart %v", info)
	}

	for i := range 5 {
		if err := n.Script().AppendLog(id, "main", fmt.Sprintf("line %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	cc := containerv1.NewContainerServiceClient(conn(t, in, "n1"))
	read := func(req *containerv1.StreamContainerLogsRequest) []string {
		t.Helper()
		s, err := cc.StreamContainerLogs(podCtx(id.String()), req)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for {
			e, err := s.Recv()
			if err == io.EOF {
				return out
			}
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, string(e.Line))
		}
	}
	if got := read(&containerv1.StreamContainerLogsRequest{TailLines: 2}); !slices.Equal(got, []string{"line 3", "line 4"}) {
		t.Errorf("tail 2: %q", got)
	}
	if got := read(&containerv1.StreamContainerLogsRequest{TailLines: 50, ContainerId: "main"}); len(got) != 5 {
		t.Errorf("tail 50: %q", got)
	}
	if got := read(&containerv1.StreamContainerLogsRequest{Tail: true}); len(got) != 5 {
		t.Errorf("tail: %q", got)
	}
	if got := read(&containerv1.StreamContainerLogsRequest{}); len(got) != 0 {
		t.Errorf("neither: %q", got)
	}
}

// An Exec into an environment's pod exits 0 without running anything, as
// does every Exec with FakeExec; SetPodExec scripts one pod's; a port of
// one pod forwards to the address SetPodPortAddr gives it.
func TestEnvPodExecAndPorts(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in)
	pc := pods(t, in, "n1")
	cc := containerv1.NewContainerServiceClient(conn(t, in, "n1"))
	exec := func(pod podid.ID, argv ...string) int32 {
		t.Helper()
		s, err := cc.Exec(podCtx(pod.String()), &containerv1.ExecRequest{Command: argv})
		if err != nil {
			t.Fatal(err)
		}
		for {
			f, err := s.Recv()
			if err != nil {
				t.Fatalf("exec %q: %v", argv, err)
			}
			if c, ok := f.GetFrame().(*containerv1.ExecStreamFrame_ExitCode); ok {
				return c.ExitCode
			}
		}
	}
	envPod := podid.Mint()
	if _, err := create(t, pc, envPod.String(), envSpec(envPod, "")); err != nil {
		t.Fatal(err)
	}
	waitState(t, pc, envPod.String(), podv1.PodState_POD_STATE_RUNNING)
	plain, _ := createRunning(t, pc)
	if code := exec(envPod, "pg_isready-does-not-exist", "-h", "127.0.0.1"); code != 0 {
		t.Errorf("an environment's pod's exec exited %d", code)
	}
	if code := exec(plain, "sh", "-c", "exit 4"); code != 4 {
		t.Errorf("an ordinary pod's exec exited %d", code)
	}
	n.Script().SetPodExec(envPod, func(_ context.Context, _ podid.ID, req *containerv1.ExecRequest, _ ExecOutput) (int32, bool, error) {
		return 2, true, nil
	})
	if code := exec(envPod, "pg_isready"); code != 2 {
		t.Errorf("a scripted exec exited %d", code)
	}
	if code := exec(plain, "sh", "-c", "exit 4"); code != 4 {
		t.Errorf("another pod's exec exited %d", code)
	}
	n.Script().SetPodExec(envPod, nil)
	if code := exec(envPod, "pg_isready"); code != 0 {
		t.Errorf("after the script: %d", code)
	}

	fake := e.start("n2", in, func(c *Config) { c.FakeExec = true })
	pc2 := pods(t, in, "n2")
	p2, _ := createRunning(t, pc2)
	cc2 := containerv1.NewContainerServiceClient(conn(t, in, "n2"))
	s, err := cc2.Exec(podCtx(p2.String()), &containerv1.ExecRequest{Command: []string{"sh", "-c", "exit 9"}})
	if err != nil {
		t.Fatal(err)
	}
	if f, err := s.Recv(); err != nil || f.GetExitCode() != 0 {
		t.Errorf("a fake exec: %v %v", f, err)
	}
	_ = fake

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("hello"))
			c.Close()
		}
	}()
	n.Script().SetPodPortAddr(envPod, 5432, ln.Addr().String())
	fc := portforwardv1.NewPortForwardServiceClient(conn(t, in, "n1"))
	open := func(pod podid.ID) (string, error) {
		st, err := fc.Forward(podCtx(pod.String()))
		if err != nil {
			return "", err
		}
		if err := st.Send(&portforwardv1.ForwardRequest{Frame: &portforwardv1.ForwardRequest_Open{Open: &portforwardv1.Open{Port: 5432}}}); err != nil {
			return "", err
		}
		var got []byte
		for {
			r, err := st.Recv()
			if err == io.EOF {
				return string(got), nil
			}
			if err != nil {
				return string(got), err
			}
			got = append(got, r.GetData()...)
		}
	}
	if got, err := open(envPod); err != nil || got != "hello" {
		t.Errorf("the pod's port: %q %v", got, err)
	}
	// Another pod's port 5432 dials 127.0.0.1:5432, where nothing is
	// expected to listen.
	if got, err := open(plain); err == nil && got == "hello" {
		t.Error("another pod's port reached the first's address")
	}
}

// A restore that brings security groups and name tables puts them into the
// pod's spec, each unless empty, before the pod is placed (ADR 0035): on
// the node that has the pod as on one that adopts it from its checkpoint.
// With the checks on, the restore needs the new ones, not the old; an
// invalid list or a pod without a network changes nothing.
func TestRestoreBringsGroupsAndNameTables(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	objs := NewObjectStore("moves")
	checks := func(c *Config) { c.ObjectStore, c.CheckSecurityGroups, c.CheckNameTables = objs, true, true }
	n1 := e.start("n1", in, checks)
	n2 := e.start("n2", in, checks)
	pc1, pc2 := pods(t, in, "n1"), pods(t, in, "n2")
	const (
		old    = "sbx-0193e3a0-0000-7000-8000-000000000002"
		env    = "env-0193e3a0-0000-7000-8000-000000000001"
		group  = "sg-0193e3a0-0000-7000-8000-000000000003"
		group2 = "sg-0193e3a0-0000-7000-8000-000000000004"
	)
	for _, node := range []string{"n1", "n2"} {
		gc := networkv1.NewSecurityGroupServiceClient(conn(t, in, node))
		nc := networkv1.NewNameServiceClient(conn(t, in, node))
		for _, id := range []string{env, group, group2} {
			if _, err := gc.PutSecurityGroup(bgctx, &networkv1.PutSecurityGroupRequest{Group: &networkv1.SecurityGroup{Id: id, Version: 1}}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := nc.PutNameTable(bgctx, &networkv1.PutNameTableRequest{Table: &networkv1.NameTable{Id: env, Version: 1,
			Entries: []*networkv1.NameEntry{{Name: "api", Addresses: []string{"fd00::7"}}}}}); err != nil {
			t.Fatal(err)
		}
	}
	gc1 := networkv1.NewSecurityGroupServiceClient(conn(t, in, "n1"))
	nc1 := networkv1.NewNameServiceClient(conn(t, in, "n1"))
	if _, err := gc1.PutSecurityGroup(bgctx, &networkv1.PutSecurityGroupRequest{Group: &networkv1.SecurityGroup{Id: old, Version: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := nc1.PutNameTable(bgctx, &networkv1.PutNameTableRequest{Table: &networkv1.NameTable{Id: old, Version: 1}}); err != nil {
		t.Fatal(err)
	}
	network := func(pc podv1.PodServiceClient, id string) *podv1.PodNetwork {
		t.Helper()
		info, err := pc.GetPod(bgctx, &podv1.GetPodRequest{PodId: id})
		if err != nil {
			t.Fatal(err)
		}
		return info.GetSpec().GetNetwork()
	}
	wantNetwork := func(got *podv1.PodNetwork, groups, tables []string) {
		t.Helper()
		if !slices.Equal(got.GetSecurityGroups(), groups) || !slices.Equal(got.GetNameTables(), tables) {
			t.Fatalf("groups %v and tables %v, want %v and %v", got.GetSecurityGroups(), got.GetNameTables(), groups, tables)
		}
	}

	p := podid.Mint()
	spec := envSpec(p, "", old)
	spec.Network.NameTables = []string{old}
	spec.Network.DnsUpstreams = []string{"9.9.9.9"}
	if _, err := create(t, pc1, p.String(), spec); err != nil {
		t.Fatal(err)
	}
	waitState(t, pc1, p.String(), podv1.PodState_POD_STATE_RUNNING)
	if _, ok := n1.Resolve(p, "api"); ok {
		t.Fatal("the old table answers api")
	}
	if _, err := pc1.HibernatePod(bgctx, &podv1.HibernatePodRequest{PodId: p.String()}); err != nil {
		t.Fatal(err)
	}

	// Invalid lists are refused and change nothing.
	_, err := pc1.RestorePod(bgctx, &podv1.RestorePodRequest{PodId: p.String(), SecurityGroups: []string{group}, NameTables: []string{env, env}})
	wantCode(t, err, codes.InvalidArgument, "name table "+env+" is named twice")
	_, err = pc1.RestorePod(bgctx, &podv1.RestorePodRequest{PodId: p.String(), SecurityGroups: []string{"no spaces"}})
	wantPrefix(t, err, codes.InvalidArgument, "invalid security group ID")
	wantNetwork(network(pc1, p.String()), []string{old}, []string{old})

	// The node that has it: the new ones, and the old are free to go.
	r, err := pc1.RestorePod(bgctx, &podv1.RestorePodRequest{PodId: p.String(), SecurityGroups: []string{env, group}, NameTables: []string{env}})
	if err != nil || r.Pod.State != podv1.PodState_POD_STATE_RUNNING {
		t.Fatalf("restore: %v, %v", r, err)
	}
	wantNetwork(r.Pod.GetSpec().GetNetwork(), []string{env, group}, []string{env})
	if r.Pod.GetSpec().GetNetwork().GetDnsUpstreams()[0] != "9.9.9.9" {
		t.Fatalf("the rest of the network changed: %v", r.Pod.GetSpec().GetNetwork())
	}
	if addrs, ok := n1.Resolve(p, "api"); !ok || !slices.Equal(addrs, []string{"fd00::7"}) {
		t.Fatalf("api resolves to %v, %v", addrs, ok)
	}
	if _, err := gc1.DeleteSecurityGroup(bgctx, &networkv1.DeleteSecurityGroupRequest{Id: old}); err != nil {
		t.Fatalf("the old group: %v", err)
	}
	if _, err := nc1.DeleteNameTable(bgctx, &networkv1.DeleteNameTableRequest{Id: old}); err != nil {
		t.Fatalf("the old table: %v", err)
	}

	// Empty lists keep the spec's.
	if _, err := pc1.HibernatePod(bgctx, &podv1.HibernatePodRequest{PodId: p.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := pc1.RestorePod(bgctx, &podv1.RestorePodRequest{PodId: p.String()}); err != nil {
		t.Fatal(err)
	}
	wantNetwork(network(pc1, p.String()), []string{env, group}, []string{env})

	// A node that adopts it from its checkpoint: the checkpoint has the
	// groups it ran in, the restore brings others; its tables stay.
	h2, err := pc1.HibernatePod(bgctx, &podv1.HibernatePodRequest{PodId: p.String()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pc2.RestorePod(bgctx, &podv1.RestorePodRequest{PodId: p.String(), HeadKey: h2.Checkpoint.HeadKey, NameTables: []string{env, env}})
	wantCode(t, err, codes.InvalidArgument, "name table "+env+" is named twice")
	if _, err := pc2.GetPod(bgctx, &podv1.GetPodRequest{PodId: p.String()}); status.Code(err) != codes.NotFound {
		t.Fatalf("a refused adopt left the pod: %v", err)
	}
	r, err = pc2.RestorePod(bgctx, &podv1.RestorePodRequest{PodId: p.String(), HeadKey: h2.Checkpoint.HeadKey, SecurityGroups: []string{env, group2}})
	if err != nil || r.Pod.State != podv1.PodState_POD_STATE_RUNNING {
		t.Fatalf("restore on n2: %v, %v", r, err)
	}
	wantNetwork(network(pc2, p.String()), []string{env, group2}, []string{env})
	if _, ok := n2.Resolve(p, "api"); !ok {
		t.Fatal("api does not resolve on n2")
	}

	// A pod without a network takes none.
	q := podid.Mint()
	none := sandboxSpec(q, "inc-1")
	none.Network = &podv1.PodNetwork{None: true}
	if _, err := create(t, pc1, q.String(), none); err != nil {
		t.Fatal(err)
	}
	waitState(t, pc1, q.String(), podv1.PodState_POD_STATE_RUNNING)
	if _, err := pc1.HibernatePod(bgctx, &podv1.HibernatePodRequest{PodId: q.String()}); err != nil {
		t.Fatal(err)
	}
	_, err = pc1.RestorePod(bgctx, &podv1.RestorePodRequest{PodId: q.String(), SecurityGroups: []string{env}})
	wantCode(t, err, codes.InvalidArgument, "the pod has no network: it takes no security groups or name tables")
	if info := waitState(t, pc1, q.String(), podv1.PodState_POD_STATE_HIBERNATED); !info.GetSpec().GetNetwork().GetNone() {
		t.Fatalf("network %v", info.GetSpec().GetNetwork())
	}
}
