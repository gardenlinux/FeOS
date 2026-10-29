// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// feosd's limits of a pod spec (spec.rs).
const (
	defaultVCPUs     = 1
	defaultMemoryMiB = 512
	maxVCPUs         = 32
	minMemoryMiB     = 128
	maxVolumes       = 8
	// podEventsBuffer is the broadcast capacity of StreamPodEvents: a reader
	// that falls further behind gets DATA_LOSS.
	podEventsBuffer = 256
	// guestVersion is what the fake guest reports.
	guestVersion = "testnode"
)

// The pods' IPv4 range, feosd --pod-ipv4-range.
var podIPv4Range = netip.PrefixFrom(netip.AddrFrom4([4]byte{169, 254, 16, 0}), 20)

// podService is feosd's PodService over pods in memory, with feosd's rules
// (pods.rs): creates are idempotent by the normalised spec, the lifecycle
// calls have its preconditions and answers, and mutating calls of a pod
// are serialised by the pod's lock, which a bring-up holds while it runs.
type podService struct {
	podv1.UnimplementedPodServiceServer
	n *Node

	mu      sync.Mutex
	pods    map[string]*pod // by the ID as the caller gave it, as feosd keys them
	subs    map[*podSub]struct{}
	nextPID int64
}

// pod is a pod's record and what the fake knows of its microVM.
type pod struct {
	uid  podid.ID
	info *podv1.PodInfo
	// lock serialises the pod's mutating calls and its bring-up.
	lock chan struct{}
	// bringUp ends the running bring-up; nil when none runs.
	bringUp context.CancelFunc
	// guestDown: the microVM is gone or paused under a record that says
	// otherwise, after a call that was cut (AbortOnCancel).
	guestDown bool
	// snapshot: a suspended pod has its memory in a snapshot.
	snapshot bool
	pid      int64
	// hib is the hibernation in flight, from its HibernatePod until it
	// ends; never set with Config.ResumeWaitsForHibernation.
	hib *hibernation
	// abandonedIDs are the request ids of the pod's abandoned hibernations,
	// the last maxRequestIDs: a late HibernatePod with one of them is
	// answered ABORTED instead of starting a hibernation of the resumed pod.
	abandonedIDs []string
	// exited are the containers whose process exited while the pod runs
	// (Script.ExitContainer), until the pod boots again.
	exited map[string]containerExit
	// logs are the containers' log lines (Script.AppendLog).
	logs map[string][]string
}

// containerExit is how a container's process ended: its code, or none
// (the node reported no exit status).
type containerExit struct {
	code    int32
	hasCode bool
}

// maxRequestIDs is how many request ids a hibernation keeps of the calls
// that started or joined it, and a pod of its abandoned hibernations.
const maxRequestIDs = 16

// hibernation is a hibernation in flight (ADR 0008): it runs in a
// goroutine of the node that holds the pod's lock, and every HibernatePod
// that comes while it runs waits for its answer.
type hibernation struct {
	done chan struct{} // closed when it ended; resp and err are set then
	resp *podv1.HibernatePodResponse
	err  error
	// abandon is closed by a ResumePod that abandons it, which sets
	// abandoned under podService.mu.
	abandon   chan struct{}
	abandoned bool
	// noAbandon: a HibernatePod said it may not be abandoned (a drain).
	// Sticky: the OR of every call that started or joined it.
	noAbandon bool
	// requestIDs are the request ids of the calls that started or joined
	// it: non-empty ones, each once, the last maxRequestIDs.
	requestIDs []string
	// memory is whether the pod's memory goes with it; nil until decided.
	memory *bool
	phase  podv1.HibernationPhase
	// epoch is the checkpoint's epoch; 0 until chosen.
	epoch uint64
}

// abandonable reports whether a ResumePod abandons it now.
func (h *hibernation) abandonable() bool {
	return !h.noAbandon && (h.memory == nil || *h.memory) && h.phase < podv1.HibernationPhase_HIBERNATION_PHASE_HEAD
}

// addRequest records the request id of a call that started or joined it;
// past maxRequestIDs the oldest goes, as on feosd.
func (h *hibernation) addRequest(id string) {
	if id == "" || slices.Contains(h.requestIDs, id) {
		return
	}
	if len(h.requestIDs) >= maxRequestIDs {
		h.requestIDs = slices.Delete(h.requestIDs, 0, 1)
	}
	h.requestIDs = append(h.requestIDs, id)
}

// info is what GetPod and ListPods report of it.
func (h *hibernation) info() *podv1.Hibernation {
	return &podv1.Hibernation{
		Phase:       h.phase,
		Abandonable: h.abandonable(),
		Epoch:       h.epoch,
		RequestIds:  slices.Clone(h.requestIDs),
	}
}

// errAbandoned is what hibernate returns when a ResumePod abandoned the
// hibernation; runHibernation turns it into abandonedErr. Only it marks a
// hibernation abandoned, not any ABORTED (a hook may return one).
var errAbandoned = errors.New("the hibernation is abandoned for a resume")

// abandonedErr is the answer of an abandoned hibernation's calls.
func abandonedErr(id string) error {
	return status.Errorf(codes.Aborted, "pod %s is resumed: its hibernation was abandoned", id)
}

// podSub is a StreamPodEvents reader.
type podSub struct {
	ch     chan *podv1.PodEvent
	lagged chan struct{}
	missed int
}

func newPodService(n *Node) *podService {
	return &podService{n: n, pods: map[string]*pod{}, subs: map[*podSub]struct{}{}, nextPID: 1000}
}

func notFound(id string) error { return status.Errorf(codes.NotFound, "no pod %s", id) }

// checkID refuses an ID that is neither a short pod ID nor a UUID (ADR
// 0019), as feosd does.
func checkID(id string) error {
	if _, err := podid.Parse(id); err != nil {
		return status.Errorf(codes.InvalidArgument, "pod ID %q is neither a short pod ID nor a UUID", id)
	}
	return nil
}

func clonePod(info *podv1.PodInfo) *podv1.PodInfo {
	return proto.Clone(info).(*podv1.PodInfo)
}

func validContainerName(name string) bool {
	if name == "" || len(name) > 64 || name == "." || name == ".." {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func validVolumeName(name string) bool {
	if name == "" || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// normalize checks a spec and fills in feosd's defaults, so that two
// creates of a pod compare as they will run (spec::normalize).
func normalize(in *podv1.PodSpec) (*podv1.PodSpec, error) {
	spec := cloneSpec(in)
	if spec.Resources == nil {
		spec.Resources = &podv1.Resources{}
	}
	if spec.Resources.Vcpus == 0 {
		spec.Resources.Vcpus = defaultVCPUs
	}
	if spec.Resources.MemoryMib == 0 {
		spec.Resources.MemoryMib = defaultMemoryMiB
	}
	if spec.Resources.Vcpus > maxVCPUs {
		return nil, status.Errorf(codes.InvalidArgument, "a pod has at most %d vCPUs", maxVCPUs)
	}
	if spec.Resources.MemoryMib < minMemoryMiB {
		return nil, status.Errorf(codes.InvalidArgument, "a pod has at least %d MiB of memory", minMemoryMiB)
	}
	names := map[string]bool{}
	for _, c := range spec.Containers {
		if !validContainerName(c.Name) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid container name %q: 1 to 64 letters, digits, \"-\", \"_\" and \".\"", c.Name)
		}
		if names[c.Name] {
			return nil, status.Errorf(codes.InvalidArgument, "two containers are named %q", c.Name)
		}
		names[c.Name] = true
		if c.ImageRef == "" {
			return nil, status.Errorf(codes.InvalidArgument, "container %q has no image", c.Name)
		}
	}
	if len(spec.Volumes) > maxVolumes {
		return nil, status.Errorf(codes.InvalidArgument, "a pod has at most %d volumes", maxVolumes)
	}
	volumes, ids := map[string]bool{}, map[string]bool{}
	for _, v := range spec.Volumes {
		if !validVolumeName(v.Name) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid volume name %q: 1 to 63 lowercase letters, digits and \"-\", beginning and ending with a letter or a digit", v.Name)
		}
		if u, err := uuid.Parse(v.VolumeId); err != nil || u.String() != v.VolumeId {
			return nil, status.Errorf(codes.InvalidArgument, "volume ID %q is not a UUID in lowercase, with hyphens", v.VolumeId)
		}
		if volumes[v.Name] {
			return nil, status.Errorf(codes.InvalidArgument, "two volumes are named %q", v.Name)
		}
		if ids[v.VolumeId] {
			return nil, status.Errorf(codes.InvalidArgument, "volume %s is named twice", v.VolumeId)
		}
		volumes[v.Name], ids[v.VolumeId] = true, true
	}
	groups := map[string]bool{}
	for _, g := range spec.GetNetwork().GetSecurityGroups() {
		if !validGroupID(g) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid security group ID %q: 1 to 64 letters, digits, \"-\", \"_\" and \".\"", g)
		}
		if groups[g] {
			return nil, status.Errorf(codes.InvalidArgument, "security group %s is named twice", g)
		}
		groups[g] = true
	}
	tables := map[string]bool{}
	for _, t := range spec.GetNetwork().GetNameTables() {
		if !validGroupID(t) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid name table ID %q: 1 to 64 letters, digits, \"-\", \"_\" and \".\"", t)
		}
		if tables[t] {
			return nil, status.Errorf(codes.InvalidArgument, "name table %s is named twice", t)
		}
		tables[t] = true
	}
	for _, c := range spec.Containers {
		paths := map[string]bool{}
		for _, m := range c.VolumeMounts {
			if !volumes[m.Volume] {
				return nil, status.Errorf(codes.InvalidArgument, "container %q mounts volume %q, which the pod does not have", c.Name, m.Volume)
			}
			if !strings.HasPrefix(m.Path, "/") || m.Path == "/" || paths[m.Path] {
				return nil, status.Errorf(codes.InvalidArgument, "container %q cannot mount volume %q at %q: an absolute path other than \"/\", once", c.Name, m.Volume, m.Path)
			}
			paths[m.Path] = true
		}
	}
	return spec, nil
}

// hasNetwork reports whether a pod has a network: unless its spec says
// none.
func hasNetwork(spec *podv1.PodSpec) bool {
	return !spec.GetNetwork().GetNone()
}

// tapName is a pod's TAP device, as feosd names it (ADR 0019): "pod-" and
// a short ID, or the first 8 hex digits of a UUID, so that two pods with
// UUIDs that begin alike collide.
func tapName(id string) string {
	if pid, err := podid.Parse(id); err == nil {
		return pid.TAP()
	}
	hex := strings.ReplaceAll(id, "-", "")
	if len(hex) > 8 {
		hex = hex[:8]
	}
	return "pod-" + hex
}

// mutate runs the body of a mutating call as feosd does. With
// AbortOnCancel it runs on the request's context, so that a cancelled
// call stops at its next waiting point, as today's feosd does, which
// works inside the request. Otherwise it runs in a goroutine of the node
// that the request awaits (§19.2): a cancelled call or a dropped
// connection never cuts it.
func mutate[T any](p *podService, ctx context.Context, op func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	if p.n.cfg.AbortOnCancel {
		return op(ctx)
	}
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	if !p.n.goPods(func(pctx context.Context) {
		v, err := op(pctx)
		ch <- result{v, err}
	}) {
		return zero, status.Error(codes.Unavailable, "the node is stopping")
	}
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return zero, status.FromContextError(ctx.Err()).Err()
	}
}

// lock takes the pod's lock, waiting behind a bring-up and other calls of
// the pod, and makes sure the pod is still there.
func (p *podService) lock(ctx context.Context, id string) (func(), error) {
	p.mu.Lock()
	pd := p.pods[id]
	p.mu.Unlock()
	if pd == nil {
		return nil, notFound(id)
	}
	select {
	case pd.lock <- struct{}{}:
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	unlock := func() { <-pd.lock }
	p.mu.Lock()
	still := p.pods[id] == pd
	p.mu.Unlock()
	if !still {
		unlock()
		return nil, notFound(id)
	}
	return unlock, nil
}

// endBringUpAndLock aborts a running bring-up of the pod, then takes its
// lock: StopPod, DeletePod and RestorePod do not wait behind one.
func (p *podService) endBringUpAndLock(ctx context.Context, id string) (func(), error) {
	p.mu.Lock()
	if pd := p.pods[id]; pd != nil && pd.bringUp != nil {
		pd.bringUp()
		pd.bringUp = nil
	}
	p.mu.Unlock()
	return p.lock(ctx, id)
}

// dirOf is the directory of the pod's fake guest, its disk.
func (p *podService) dirOf(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pd := p.pods[id]; pd != nil {
		return p.n.podDir(pd.uid)
	}
	u, _ := podid.Parse(id)
	return p.n.podDir(u)
}

// get returns a copy of the pod's record.
func (p *podService) get(id string) (*podv1.PodInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pd := p.pods[id]
	if pd == nil {
		return nil, notFound(id)
	}
	return clonePod(pd.info), nil
}

// change changes the pod's record or state of its microVM, without an
// event.
func (p *podService) change(id string, f func(pd *pod)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pd := p.pods[id]; pd != nil {
		f(pd)
	}
}

// setState sets the pod's state and reason and announces it; a repeat of
// the same state and reason announces nothing.
func (p *podService) setState(id string, state podv1.PodState, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pd := p.pods[id]
	if pd == nil || pd.info.State == state && pd.info.Reason == reason {
		return
	}
	pd.info.State = state
	pd.info.Reason = reason
	p.announceLocked(pd.info, false)
}

// announceLocked sends a pod's state to the StreamPodEvents readers and
// into the node's event buffer.
func (p *podService) announceLocked(info *podv1.PodInfo, deleted bool) {
	ev := &podv1.PodEvent{PodId: info.PodId, State: info.State, Reason: info.Reason, Deleted: deleted}
	for sub := range p.subs {
		select {
		case sub.ch <- ev:
		default:
			p.lagLocked(sub)
		}
	}
	p.n.events.add(&fleetv1.Event{Body: &fleetv1.Event_Pod{Pod: proto.Clone(ev).(*podv1.PodEvent)}})
}

func (p *podService) lagLocked(sub *podSub) {
	if sub.missed == 0 {
		close(sub.lagged)
	}
	sub.missed++
}

func (p *podService) lagAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for sub := range p.subs {
		p.lagLocked(sub)
	}
}

// spawnBringUp starts the pod's bring-up: it waits for the pod's lock,
// then the PendingDelay, then the script's bring-up, and sets the pod
// RUNNING, or FAILED with the bring-up's error. A bring-up that is ended
// changes nothing.
func (p *podService) spawnBringUp(id string) {
	p.mu.Lock()
	pd := p.pods[id]
	if pd == nil {
		p.mu.Unlock()
		return
	}
	if pd.bringUp != nil {
		pd.bringUp()
	}
	ctx, cancel := context.WithCancel(p.n.podCtx)
	pd.bringUp = cancel
	p.mu.Unlock()
	if !p.n.goPods(func(context.Context) {
		defer cancel()
		p.bringUpPod(ctx, id, pd)
	}) {
		cancel()
	}
}

func (p *podService) bringUpPod(ctx context.Context, id string, pd *pod) {
	unlock, err := p.lock(ctx, id)
	if err != nil {
		return
	}
	defer unlock()
	p.mu.Lock()
	ours := p.pods[id] == pd && pd.info.State == podv1.PodState_POD_STATE_PENDING
	p.mu.Unlock()
	if !ours || !sleep(ctx, p.n.cfg.PendingDelay) {
		return
	}
	var failed error
	if f := p.n.script.bringUpFunc(); f != nil {
		failed = f(ctx, pd.uid)
	}
	if ctx.Err() != nil {
		return
	}
	if failed == nil {
		p.mu.Lock()
		groups := pd.info.GetSpec().GetNetwork().GetSecurityGroups()
		tables := pd.info.GetSpec().GetNetwork().GetNameTables()
		p.mu.Unlock()
		failed = p.n.groups.attachError(groups)
		if failed == nil {
			failed = p.n.names.attachError(tables)
		}
	}
	// What the boot schedules runs once p.mu is released.
	var scheduled func()
	defer func() {
		if scheduled != nil {
			scheduled()
		}
	}()
	p.mu.Lock()
	defer p.mu.Unlock()
	// Ended meanwhile: whoever ended it holds the say now. spawnBringUp and
	// endBringUpAndLock end bring-ups under p.mu, so this check is exact.
	if ctx.Err() != nil || p.pods[id] != pd || pd.info.State != podv1.PodState_POD_STATE_PENDING {
		return
	}
	pd.bringUp = nil
	if failed != nil {
		pd.info.State, pd.info.Reason = podv1.PodState_POD_STATE_FAILED, failed.Error()
	} else {
		pd.guestDown, pd.exited = false, nil
		bakeLocked(pd.info)
		pd.info.State, pd.info.Reason = podv1.PodState_POD_STATE_RUNNING, ""
		// A boot is a new microVM process, as on feosd.
		p.nextPID++
		pd.pid = p.nextPID
		p.n.volumes.booted(id)
		scheduled = p.bootedLocked(pd, true)
	}
	p.announceLocked(pd.info, false)
}

// The guest version and blank data volume of the test node's disks: what
// a baked layer is made for (ADR 0025).
const (
	GuestVersion = "testnode-guest"
	BlankLayer   = "testnode-l0"
)

// bakeLocked does at a pod's first bring-up what feosd's bake point does
// (ADR 0025): a pod whose create named its one container's image starts
// its disk on the first layer made for the node's guest and blank data
// volume, or, if none is and the create lets it, bakes the image; the
// bake is reported at once, as feosd reports it once the object store has
// it. A pod with a disk already, or of a checkpoint, is left as it is.
func bakeLocked(info *podv1.PodInfo) {
	layers := info.GetImageLayers()
	containers := info.GetSpec().GetContainers()
	if info.Disk != nil || info.Checkpoint != nil || layers.GetImageRef() == "" || len(containers) != 1 ||
		containers[0].GetImageRef() != layers.GetImageRef() {
		return
	}
	disk := &podv1.BakedLayer{ImageRef: layers.GetImageRef(), GuestVersion: GuestVersion, L0: BlankLayer}
	for _, l := range layers.GetLayers() {
		if l.GetImageRef() == layers.GetImageRef() && l.GetGuestVersion() == GuestVersion && l.GetL0() == BlankLayer {
			disk.LayerId = l.GetLayerId()
			break
		}
	}
	info.Disk = disk
	if disk.LayerId == "" && layers.GetBake() {
		baked := proto.Clone(disk).(*podv1.BakedLayer)
		baked.LayerId = "baked-" + info.PodId
		info.Baked = baked
	}
}

// allocateLocked gives a pod its addresses, refusing a TAP device that
// another pod with a network has (network.rs allocate).
func (p *podService) allocateLocked(info *podv1.PodInfo) error {
	tap := tapName(info.PodId)
	used := map[netip.Addr]bool{}
	for _, o := range p.pods {
		if o.info.PodId == info.PodId || !hasNetwork(o.info.Spec) {
			continue
		}
		if tapName(o.info.PodId) == tap {
			return status.Errorf(codes.AlreadyExists, "pod %s has the TAP device %s already: a short ID names its own, a UUID's first 8 hex digits name it", o.info.PodId, tap)
		}
		for _, s := range []string{o.info.Ipv6Address, o.info.Ipv4Address} {
			if a, err := netip.ParseAddr(s); err == nil {
				used[a] = true
			}
		}
	}
	v6, err := freeAddr(p.n.podPrefix, used)
	if err != nil {
		return err
	}
	v4, err := freeAddr(podIPv4Range, used)
	if err != nil {
		return err
	}
	info.Ipv6Address, info.Ipv4Address = v6.String(), v4.String()
	return nil
}

// freeAddr returns the first address of prefix from its second host on
// that is not used.
func freeAddr(prefix netip.Prefix, used map[netip.Addr]bool) (netip.Addr, error) {
	for a := prefix.Masked().Addr().Next().Next(); a.IsValid() && prefix.Contains(a); a = a.Next() {
		if !used[a] {
			return a, nil
		}
	}
	return netip.Addr{}, status.Errorf(codes.ResourceExhausted, "no address: %s is used up", prefix)
}

// head reads a checkpoint's head from the node's object store.
func (p *podService) head(key string) (*checkpoint, error) {
	store := p.n.cfg.ObjectStore
	if store == nil {
		return nil, status.Error(codes.FailedPrecondition, "diskd: S3 not configured")
	}
	c, ok, err := store.get(key)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "diskd: read head %s: %v", key, err)
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "diskd: no head %s", key)
	}
	return c, nil
}

// whyCold says why a restore boots the pod anew instead of restoring its
// memory, or "" when it restores it.
func whyCold(cold, memory bool, unrestorable string) string {
	switch {
	case cold:
		return "asked to boot anew"
	case !memory:
		return "its checkpoint holds no memory"
	default:
		return unrestorable
	}
}

// containerStatuses is what the guest reports of a running pod's
// containers: running, or exited (with its code when it has one).
func containerStatuses(spec *podv1.PodSpec, exited map[string]containerExit) []*podv1.ContainerStatus {
	var out []*podv1.ContainerStatus
	for _, c := range spec.GetContainers() {
		st := &podv1.ContainerStatus{Name: c.Name, State: podv1.ContainerState_CONTAINER_STATE_RUNNING}
		if e, ok := exited[c.Name]; ok {
			st.State = podv1.ContainerState_CONTAINER_STATE_EXITED
			if e.hasCode {
				code := e.code
				st.ExitCode = &code
			}
		}
		out = append(out, st)
	}
	return out
}

func (p *podService) CreatePod(ctx context.Context, req *podv1.CreatePodRequest) (*podv1.CreatePodResponse, error) {
	return mutate(p, ctx, func(ctx context.Context) (*podv1.CreatePodResponse, error) {
		info, err := p.create(ctx, req.GetPodId(), req.GetSpec(), req.GetImageLayers())
		if err != nil {
			return nil, err
		}
		return &podv1.CreatePodResponse{Pod: info}, nil
	})
}

func (p *podService) create(ctx context.Context, id string, spec *podv1.PodSpec, layers *podv1.ImageLayers) (*podv1.PodInfo, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	spec, err := normalize(spec)
	if err != nil {
		return nil, err
	}
	uid, _ := podid.Parse(id)
	p.mu.Lock()
	if pd := p.pods[id]; pd != nil {
		info := clonePod(pd.info)
		p.mu.Unlock()
		return sameSpec(info, spec)
	}
	info := &podv1.PodInfo{PodId: id, Spec: spec, State: podv1.PodState_POD_STATE_PENDING}
	if layers != nil {
		info.ImageLayers = proto.Clone(layers).(*podv1.ImageLayers)
	}
	if hasNetwork(spec) {
		if err := p.allocateLocked(info); err != nil {
			p.mu.Unlock()
			return nil, err
		}
	}
	// The volumes are claimed synchronously, all or none: one the node
	// does not have, or another pod holds, fails the create.
	var volumeIDs []string
	for _, v := range spec.Volumes {
		volumeIDs = append(volumeIDs, v.VolumeId)
	}
	if err := p.n.volumes.claim(id, volumeIDs); err != nil {
		p.mu.Unlock()
		return nil, err
	}
	if err := os.MkdirAll(p.n.podDir(uid), 0o755); err != nil {
		p.mu.Unlock()
		return nil, status.Errorf(codes.Internal, "keep pod %s: %v", id, err)
	}
	p.nextPID++
	pd := &pod{uid: uid, info: info, lock: make(chan struct{}, 1), pid: p.nextPID}
	p.pods[id] = pd
	out := clonePod(info)
	p.mu.Unlock()
	// feosd sets the pods' network rules here, between recording the pod
	// and announcing it: a create cut at this point leaves the pod PENDING
	// without a bring-up, for the control plane to repair.
	if err := p.n.script.wait(ctx, podv1.PodService_CreatePod_FullMethodName); err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.pods[id] == pd {
		p.announceLocked(pd.info, false)
	}
	p.mu.Unlock()
	p.spawnBringUp(id)
	return out, nil
}

// sameSpec answers a create of a pod that exists: the pod, if the spec is
// the same.
func sameSpec(existing *podv1.PodInfo, spec *podv1.PodSpec) (*podv1.PodInfo, error) {
	if proto.Equal(existing.Spec, spec) {
		return existing, nil
	}
	return nil, status.Errorf(codes.AlreadyExists, "pod %s exists with another spec", existing.PodId)
}

func (p *podService) StartPod(ctx context.Context, req *podv1.StartPodRequest) (*podv1.StartPodResponse, error) {
	return mutate(p, ctx, func(ctx context.Context) (*podv1.StartPodResponse, error) {
		return &podv1.StartPodResponse{}, p.start(ctx, req.GetPodId())
	})
}

func (p *podService) start(ctx context.Context, id string) error {
	unlock, err := p.lock(ctx, id)
	if err != nil {
		return err
	}
	info, err := p.get(id)
	if err != nil {
		unlock()
		return err
	}
	switch info.State {
	case podv1.PodState_POD_STATE_PENDING, podv1.PodState_POD_STATE_RUNNING:
		unlock()
		return nil
	case podv1.PodState_POD_STATE_SUSPENDED:
		unlock()
		return status.Errorf(codes.FailedPrecondition, "pod %s is suspended: resume it", id)
	case podv1.PodState_POD_STATE_HIBERNATED:
		unlock()
		return status.Errorf(codes.FailedPrecondition, "pod %s is hibernated: resume or restore it, or stop it to boot it anew", id)
	}
	if err := p.n.script.wait(ctx, podv1.PodService_StartPod_FullMethodName); err != nil {
		unlock()
		return err
	}
	p.setState(id, podv1.PodState_POD_STATE_PENDING, "")
	unlock()
	p.spawnBringUp(id)
	return nil
}

func (p *podService) StopPod(ctx context.Context, req *podv1.StopPodRequest) (*podv1.StopPodResponse, error) {
	return mutate(p, ctx, func(ctx context.Context) (*podv1.StopPodResponse, error) {
		return &podv1.StopPodResponse{}, p.stop(ctx, req.GetPodId())
	})
}

func (p *podService) stop(ctx context.Context, id string) error {
	unlock, err := p.endBringUpAndLock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	info, err := p.get(id)
	if err != nil {
		return err
	}
	switch info.State {
	case podv1.PodState_POD_STATE_STOPPED:
		return nil
	case podv1.PodState_POD_STATE_HIBERNATED:
		// Its next start boots on the disk of its checkpoint.
		if c, err := p.head(info.GetCheckpoint().GetHeadKey()); err == nil {
			if err := restoreDir(p.dirOf(id), c.files); err != nil {
				return status.Errorf(codes.Internal, "the disk of pod %s: %v", id, err)
			}
		}
		p.setState(id, podv1.PodState_POD_STATE_STOPPED, "stopped: its memory is dropped, its disk is its checkpoint's")
		return nil
	}
	if err := p.n.script.wait(ctx, podv1.PodService_StopPod_FullMethodName); err != nil {
		p.change(id, func(pd *pod) { pd.guestDown = true })
		return err
	}
	p.change(id, func(pd *pod) { pd.snapshot, pd.exited = false, nil })
	p.setState(id, podv1.PodState_POD_STATE_STOPPED, "stopped")
	return nil
}

func (p *podService) DeletePod(ctx context.Context, req *podv1.DeletePodRequest) (*podv1.DeletePodResponse, error) {
	return mutate(p, ctx, func(ctx context.Context) (*podv1.DeletePodResponse, error) {
		return &podv1.DeletePodResponse{}, p.delete(ctx, req.GetPodId())
	})
}

func (p *podService) delete(ctx context.Context, id string) error {
	unlock, err := p.endBringUpAndLock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	if err := p.n.script.wait(ctx, podv1.PodService_DeletePod_FullMethodName); err != nil {
		p.change(id, func(pd *pod) { pd.guestDown = true })
		return err
	}
	p.mu.Lock()
	pd := p.pods[id]
	if pd == nil {
		p.mu.Unlock()
		return notFound(id)
	}
	delete(p.pods, id)
	p.announceLocked(pd.info, true)
	p.mu.Unlock()
	p.n.volumes.release(id)
	p.n.procs.deleted(id)
	if err := os.RemoveAll(p.n.podDir(pd.uid)); err != nil {
		p.n.log.Warn("test node: the directory of a deleted pod stays", "pod", id, "err", err)
	}
	return nil
}

func (p *podService) SuspendPod(ctx context.Context, req *podv1.SuspendPodRequest) (*podv1.SuspendPodResponse, error) {
	return mutate(p, ctx, func(ctx context.Context) (*podv1.SuspendPodResponse, error) {
		return p.suspend(ctx, req.GetPodId())
	})
}

func (p *podService) expect(id string, state podv1.PodState) (*podv1.PodInfo, error) {
	info, err := p.get(id)
	if err != nil {
		return nil, err
	}
	if info.State != state {
		return nil, status.Errorf(codes.FailedPrecondition, "pod %s is %s, not %s", id, info.State, state)
	}
	return info, nil
}

func (p *podService) suspend(ctx context.Context, id string) (*podv1.SuspendPodResponse, error) {
	unlock, err := p.lock(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	info, err := p.expect(id, podv1.PodState_POD_STATE_RUNNING)
	if err != nil {
		return nil, err
	}
	// The microVM's actor writes the snapshot whatever happens to the
	// call; a call cut here leaves the record RUNNING over a paused guest.
	if err := p.n.script.wait(ctx, podv1.PodService_SuspendPod_FullMethodName); err != nil {
		p.change(id, func(pd *pod) { pd.guestDown = true })
		return nil, err
	}
	p.change(id, func(pd *pod) { pd.snapshot = true })
	p.setState(id, podv1.PodState_POD_STATE_SUSPENDED, "suspended")
	return &podv1.SuspendPodResponse{MemoryBytes: info.GetSpec().GetResources().GetMemoryMib() << 20}, nil
}

func (p *podService) ResumePod(ctx context.Context, req *podv1.ResumePodRequest) (*podv1.ResumePodResponse, error) {
	return mutate(p, ctx, func(ctx context.Context) (*podv1.ResumePodResponse, error) {
		return &podv1.ResumePodResponse{}, p.resume(ctx, req.GetPodId())
	})
}

func (p *podService) resume(ctx context.Context, id string) error {
	if err := p.abandonHibernation(ctx, id); err != nil {
		return err
	}
	unlock, err := p.lock(ctx, id)
	if err != nil {
		return err
	}
	if info, err := p.get(id); err == nil && info.State == podv1.PodState_POD_STATE_HIBERNATED {
		unlock()
		_, err := p.restore(ctx, id, "", false)
		return err
	}
	defer unlock()
	if _, err := p.expect(id, podv1.PodState_POD_STATE_SUSPENDED); err != nil {
		return err
	}
	if err := p.n.script.wait(ctx, podv1.PodService_ResumePod_FullMethodName); err != nil {
		return err
	}
	p.change(id, func(pd *pod) { pd.guestDown, pd.snapshot = false, false })
	p.setState(id, podv1.PodState_POD_STATE_RUNNING, "")
	return nil
}

// abandonHibernation is a ResumePod's look at the pod's hibernation in
// flight: it abandons one that may be abandoned, and waits for it to end
// either way. (feosd queues the resume for the pod's lock first, which is
// FIFO there; the test node's lock is not, so it waits for the end of the
// hibernation instead, before it takes the lock.)
func (p *podService) abandonHibernation(ctx context.Context, id string) error {
	if p.n.cfg.ResumeWaitsForHibernation {
		return nil
	}
	p.mu.Lock()
	pd := p.pods[id]
	if pd == nil || pd.hib == nil {
		p.mu.Unlock()
		return nil
	}
	h := pd.hib
	if h.abandonable() && !h.abandoned {
		h.abandoned = true
		close(h.abandon)
		p.n.log.Info("test node: a resume abandons a hibernation", "pod", id, "phase", h.phase)
	} else if !h.abandoned {
		p.n.log.Info("test node: a resume waits for a hibernation", "pod", id, "phase", h.phase)
	}
	p.mu.Unlock()
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

func (p *podService) HibernatePod(ctx context.Context, req *podv1.HibernatePodRequest) (*podv1.HibernatePodResponse, error) {
	if p.n.cfg.ResumeWaitsForHibernation {
		return mutate(p, ctx, func(ctx context.Context) (*podv1.HibernatePodResponse, error) {
			unlock, err := p.lock(ctx, req.GetPodId())
			if err != nil {
				return nil, err
			}
			defer unlock()
			return p.hibernate(ctx, req, nil)
		})
	}
	id := req.GetPodId()
	p.mu.Lock()
	pd := p.pods[id]
	if pd == nil {
		p.mu.Unlock()
		return nil, notFound(id)
	}
	if req.GetRequestId() != "" && slices.Contains(pd.abandonedIDs, req.GetRequestId()) {
		// A late call of an abandoned hibernation: the pod is resumed.
		p.mu.Unlock()
		return nil, abandonedErr(id)
	}
	h := pd.hib
	start := h == nil
	if start {
		h = &hibernation{
			done:      make(chan struct{}),
			abandon:   make(chan struct{}),
			noAbandon: req.GetNoAbandon(),
			phase:     podv1.HibernationPhase_HIBERNATION_PHASE_WAITING,
		}
		pd.hib = h
	} else {
		h.noAbandon = h.noAbandon || req.GetNoAbandon()
	}
	h.addRequest(req.GetRequestId())
	p.mu.Unlock()
	if start {
		first := proto.Clone(req).(*podv1.HibernatePodRequest)
		if !p.n.goPods(func(ctx context.Context) { p.runHibernation(ctx, pd, h, first) }) {
			p.endHibernation(pd, h, nil, status.Error(codes.Unavailable, "the node is stopping"), false)
		}
	}
	// The hibernation runs on its own: a call that goes leaves it running.
	select {
	case <-h.done:
		return h.resp, h.err
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
}

// runHibernation is the goroutine of a hibernation: it takes the pod's
// lock (the hibernation waits in HIBERNATION_PHASE_WAITING until then),
// runs it, and ends its slot before it releases the lock, so that a
// HibernatePod that comes afterwards starts a new one.
func (p *podService) runHibernation(ctx context.Context, pd *pod, h *hibernation, req *podv1.HibernatePodRequest) {
	id := req.GetPodId()
	unlock, err := p.lock(ctx, id)
	if err != nil {
		p.endHibernation(pd, h, nil, err, false)
		return
	}
	defer unlock()
	// The lock is that of the pod now under id: a pod deleted and created
	// anew since HibernatePod found pd is not the one hibernated here.
	p.mu.Lock()
	same := p.pods[id] == pd
	p.mu.Unlock()
	if !same {
		p.endHibernation(pd, h, nil, notFound(id), false)
		return
	}
	resp, err := p.hibernate(ctx, req, h)
	abandoned := errors.Is(err, errAbandoned)
	if abandoned {
		p.n.log.Info("test node: a hibernation is abandoned for a resume", "pod", id)
		err = abandonedErr(id)
	}
	p.endHibernation(pd, h, resp, err, abandoned)
}

// endHibernation sets a hibernation's answer and clears its slot, if the
// slot is still its own; an abandoned one leaves its request ids in the
// pod's abandonedIDs.
func (p *podService) endHibernation(pd *pod, h *hibernation, resp *podv1.HibernatePodResponse, err error, abandoned bool) {
	p.mu.Lock()
	h.resp, h.err = resp, err
	if pd.hib == h {
		pd.hib = nil
		if abandoned {
			pd.abandonedIDs = append(pd.abandonedIDs, h.requestIDs...)
			if over := len(pd.abandonedIDs) - maxRequestIDs; over > 0 {
				pd.abandonedIDs = slices.Delete(pd.abandonedIDs, 0, over)
			}
		}
	}
	p.mu.Unlock()
	close(h.done)
}

// hibernating changes a hibernation in flight under p.mu; nothing with
// Config.ResumeWaitsForHibernation, where h is nil.
func (p *podService) hibernating(h *hibernation, f func(h *hibernation)) {
	if h == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	f(h)
}

// isClosed reports whether ch is closed; never for a nil ch.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// untilClosed is a context that ends with ctx or when ch is closed.
func untilClosed(ctx context.Context, ch <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	if ch != nil {
		go func() {
			select {
			case <-ch:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return ctx, cancel
}

// hibernate is a hibernation, with the pod's lock held. h is its slot, nil
// with Config.ResumeWaitsForHibernation: there ctx is the request's or
// the node's as AbortOnCancel says, and nothing abandons it. Its phases
// are feosd's; an abandon stops it before the suspend (a running pod
// stays running), after it, or at the upload, leaves the pod suspended
// on the node with its snapshot, and returns errAbandoned.
func (p *podService) hibernate(ctx context.Context, req *podv1.HibernatePodRequest, h *hibernation) (*podv1.HibernatePodResponse, error) {
	id := req.GetPodId()
	info, err := p.get(id)
	if err != nil {
		return nil, err
	}
	store := p.n.cfg.ObjectStore
	if info.State != podv1.PodState_POD_STATE_HIBERNATED && store == nil {
		// Before anything changes: without one, the pod stays as it is.
		return nil, status.Error(codes.FailedPrecondition, "diskd has no object store: pods do not hibernate on this node")
	}
	var memory bool
	switch info.State {
	case podv1.PodState_POD_STATE_HIBERNATED:
		return &podv1.HibernatePodResponse{Checkpoint: info.Checkpoint}, nil
	case podv1.PodState_POD_STATE_RUNNING:
		memory = true
	case podv1.PodState_POD_STATE_SUSPENDED:
		p.mu.Lock()
		memory = p.pods[id] != nil && p.pods[id].snapshot
		p.mu.Unlock()
	case podv1.PodState_POD_STATE_STOPPED, podv1.PodState_POD_STATE_FAILED:
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "pod %s is %s", id, info.State)
	}
	// Only a hibernation that takes the pod's memory along may be
	// abandoned: without memory there is nothing to resume, and an abandon
	// asked while that was not known yet is ignored for good.
	var abandon chan struct{}
	if h != nil && memory {
		abandon = h.abandon
	}
	p.hibernating(h, func(h *hibernation) { h.memory = &memory })
	if isClosed(abandon) {
		return nil, errAbandoned
	}
	if info.State == podv1.PodState_POD_STATE_RUNNING {
		p.hibernating(h, func(h *hibernation) { h.phase = podv1.HibernationPhase_HIBERNATION_PHASE_SUSPENDING })
		p.setState(id, podv1.PodState_POD_STATE_RUNNING, "hibernating")
		p.change(id, func(pd *pod) { pd.snapshot = true })
		p.setState(id, podv1.PodState_POD_STATE_SUSPENDED, "suspended")
		if isClosed(abandon) {
			return nil, errAbandoned
		}
	}
	p.hibernating(h, func(h *hibernation) { h.phase = podv1.HibernationPhase_HIBERNATION_PHASE_SEALING })
	dir := p.dirOf(id)
	files, diskBytes, err := snapshotDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, status.Errorf(codes.Internal, "seal the disk of pod %s: %v", id, err)
	}
	epoch := uint64(time.Now().Unix())
	if last := info.GetCheckpoint().GetEpoch(); last+1 > epoch {
		epoch = last + 1
	}
	p.hibernating(h, func(h *hibernation) {
		h.phase, h.epoch = podv1.HibernationPhase_HIBERNATION_PHASE_UPLOADING_DISK, epoch
	})
	// The upload: a hibernation that fails, is cut or is abandoned here
	// leaves the pod suspended on the node (or as it was, without memory).
	actx, cancel := untilClosed(ctx, abandon)
	err = p.n.script.point(actx, HibernateUpload, req)
	if err == nil {
		err = p.n.script.wait(actx, podv1.PodService_HibernatePod_FullMethodName)
	}
	cancel()
	if isClosed(abandon) {
		return nil, errAbandoned
	}
	if err != nil {
		return nil, err
	}
	if memory {
		p.hibernating(h, func(h *hibernation) { h.phase = podv1.HibernationPhase_HIBERNATION_PHASE_UPLOADING_MEMORY })
	}
	// The point of no return: under p.mu, where a ResumePod decides too,
	// either the abandon came first or the head is being put.
	if h != nil {
		p.mu.Lock()
		if isClosed(abandon) {
			p.mu.Unlock()
			return nil, errAbandoned
		}
		h.phase = podv1.HibernationPhase_HIBERNATION_PHASE_HEAD
		p.mu.Unlock()
	}
	if err := p.n.script.point(ctx, HibernateHead, req); err != nil {
		return nil, err
	}
	key, err := store.put(&checkpoint{podID: id, epoch: epoch, memory: memory, spec: cloneSpec(info.Spec), files: files, bytes: diskBytes,
		volumes: p.n.volumes.checkpointed(id, epoch)})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "diskd: upload the checkpoint of pod %s: %v", id, err)
	}
	p.n.volumes.checkpoint(id, epoch)
	p.hibernating(h, func(h *hibernation) { h.phase = podv1.HibernationPhase_HIBERNATION_PHASE_LEAVING })
	cp := &podv1.Checkpoint{HeadKey: key, Epoch: epoch, Memory: memory}
	p.change(id, func(pd *pod) {
		pd.info.Checkpoint = proto.Clone(cp).(*podv1.Checkpoint)
		pd.info.Ipv6Address, pd.info.Ipv4Address = "", ""
		pd.snapshot, pd.guestDown = false, false
	})
	p.setState(id, podv1.PodState_POD_STATE_HIBERNATED, "hibernated")
	if err := os.RemoveAll(dir); err != nil {
		p.n.log.Warn("test node: the directory of a hibernated pod stays", "pod", id, "err", err)
	}
	var memoryBytes uint64
	if memory {
		memoryBytes = info.GetSpec().GetResources().GetMemoryMib() << 20
	}
	return &podv1.HibernatePodResponse{Checkpoint: cp, MemoryBytes: memoryBytes, DiskBytes: diskBytes}, nil
}

func (p *podService) RestorePod(ctx context.Context, req *podv1.RestorePodRequest) (*podv1.RestorePodResponse, error) {
	return mutate(p, ctx, func(ctx context.Context) (*podv1.RestorePodResponse, error) {
		return p.restore(ctx, req.GetPodId(), req.GetHeadKey(), req.GetCold())
	})
}

func (p *podService) restore(ctx context.Context, id, headKey string, cold bool) (*podv1.RestorePodResponse, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	if _, err := p.get(id); err != nil {
		if err := p.adopt(id, headKey); err != nil {
			return nil, err
		}
	}
	unlock, err := p.endBringUpAndLock(ctx, id)
	if err != nil {
		return nil, err
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	info, err := p.get(id)
	if err != nil {
		return nil, err
	}
	own := info.GetCheckpoint().GetHeadKey()
	key := headKey
	if key == "" {
		key = own
	}
	switch info.State {
	case podv1.PodState_POD_STATE_HIBERNATED:
	case podv1.PodState_POD_STATE_PENDING, podv1.PodState_POD_STATE_RUNNING:
		// Restored already, by a call whose caller lost the answer.
		if info.Checkpoint != nil && own == key {
			return &podv1.RestorePodResponse{Pod: info}, nil
		}
		fallthrough
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "pod %s is %s, not hibernated", id, info.State)
	}
	if key == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "pod %s has no checkpoint", id)
	}
	c, err := p.head(key)
	if err != nil {
		return nil, err
	}
	if c.podID != id {
		return nil, status.Errorf(codes.InvalidArgument, "%s is pod %s's, not %s's", key, c.podID, id)
	}
	p.mu.Lock()
	pd := p.pods[id]
	if pd == nil {
		p.mu.Unlock()
		return nil, notFound(id)
	}
	// The volumes first, as feosd's restore takes them over before it
	// places the pod: a refused restore leaves a hibernated pod without
	// addresses.
	if err := p.n.volumes.takeOver(id, c.volumes); err != nil {
		p.mu.Unlock()
		return nil, err
	}
	if hasNetwork(pd.info.Spec) {
		if err := p.allocateLocked(pd.info); err != nil {
			p.mu.Unlock()
			return nil, err
		}
	}
	pd.info.Checkpoint = &podv1.Checkpoint{HeadKey: key, Epoch: c.epoch, Memory: c.memory}
	p.mu.Unlock()
	if err := restoreDir(p.n.podDir(pd.uid), c.files); err != nil {
		return nil, status.Errorf(codes.Internal, "the disk of pod %s: %v", id, err)
	}
	p.setState(id, podv1.PodState_POD_STATE_PENDING, "restoring from "+key)
	// Where the memory loads (or the disk is made ready to boot on): a
	// call cut here leaves the pod PENDING without a bring-up.
	if err := p.n.script.wait(ctx, podv1.PodService_RestorePod_FullMethodName); err != nil {
		return nil, err
	}
	reason := whyCold(cold, c.memory, p.n.script.unrestorableReason())
	if reason == "" {
		err := p.n.groups.attachError(info.GetSpec().GetNetwork().GetSecurityGroups())
		if err == nil {
			err = p.n.names.attachError(info.GetSpec().GetNetwork().GetNameTables())
		}
		if err != nil {
			p.setState(id, podv1.PodState_POD_STATE_FAILED, err.Error())
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		var scheduled func()
		p.change(id, func(pd *pod) {
			pd.guestDown, pd.snapshot = false, false
			// A restore runs in a new microVM process.
			p.nextPID++
			pd.pid = p.nextPID
			scheduled = p.bootedLocked(pd, false)
		})
		p.n.volumes.booted(id)
		p.setState(id, podv1.PodState_POD_STATE_RUNNING, "")
		if scheduled != nil {
			scheduled()
		}
		out, err := p.get(id)
		if err != nil {
			return nil, err
		}
		return &podv1.RestorePodResponse{Pod: out, MemoryRestored: true}, nil
	}
	p.setState(id, podv1.PodState_POD_STATE_PENDING, "booting anew on its disk: "+reason)
	out, err := p.get(id)
	if err != nil {
		return nil, err
	}
	unlock()
	locked = false
	p.spawnBringUp(id)
	return &podv1.RestorePodResponse{Pod: out, MemoryRestored: false, Reason: reason}, nil
}

// adopt records a pod the node does not have, hibernated, from its
// checkpoint, which holds its spec, once its volumes are restorable here
// (feosd's adopt_checkpoint checks them first: a restore that cannot take
// its volumes leaves nothing on the node).
func (p *podService) adopt(id, headKey string) error {
	if headKey == "" {
		return status.Errorf(codes.NotFound, "no pod %s: name its checkpoint's head to restore it here", id)
	}
	c, err := p.head(headKey)
	if err != nil {
		return err
	}
	if c.podID != id {
		return status.Errorf(codes.InvalidArgument, "%s is pod %s's, not %s's", headKey, c.podID, id)
	}
	spec, err := normalize(c.spec)
	if err != nil {
		return err
	}
	uid, _ := podid.Parse(id)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pods[id] != nil {
		return nil // another restore was faster
	}
	if err := p.n.volumes.restorable(id, c.volumes); err != nil {
		return err
	}
	info := &podv1.PodInfo{
		PodId:      id,
		Spec:       spec,
		State:      podv1.PodState_POD_STATE_HIBERNATED,
		Reason:     "hibernated",
		Checkpoint: &podv1.Checkpoint{HeadKey: headKey, Epoch: c.epoch, Memory: c.memory},
	}
	p.nextPID++
	p.pods[id] = &pod{uid: uid, info: info, lock: make(chan struct{}, 1), pid: p.nextPID}
	p.announceLocked(info, false)
	return nil
}

func (p *podService) GetPod(_ context.Context, req *podv1.GetPodRequest) (*podv1.PodInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pd := p.pods[req.GetPodId()]
	if pd == nil {
		return nil, notFound(req.GetPodId())
	}
	info := clonePod(pd.info)
	if pd.hib != nil {
		info.Hibernation = pd.hib.info()
	}
	if info.State == podv1.PodState_POD_STATE_RUNNING {
		info.Microvm = &podv1.MicroVm{
			Pid:           pd.pid,
			GuestVersion:  guestVersion,
			GuestAttached: !pd.guestDown && !p.n.script.isUnreachable(pd.uid),
		}
		info.Containers = containerStatuses(info.Spec, pd.exited)
	}
	return info, nil
}

func (p *podService) ListPods(context.Context, *podv1.ListPodsRequest) (*podv1.ListPodsResponse, error) {
	return &podv1.ListPodsResponse{Pods: p.list()}, nil
}

// list returns the pods' records, sorted by ID.
func (p *podService) list() []*podv1.PodInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*podv1.PodInfo, 0, len(p.pods))
	for _, pd := range p.pods {
		info := clonePod(pd.info)
		if pd.hib != nil {
			info.Hibernation = pd.hib.info()
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b *podv1.PodInfo) int { return strings.Compare(a.PodId, b.PodId) })
	return out
}

func (p *podService) StreamPodEvents(req *podv1.StreamPodEventsRequest, stream grpc.ServerStreamingServer[podv1.PodEvent]) error {
	sub := &podSub{ch: make(chan *podv1.PodEvent, podEventsBuffer), lagged: make(chan struct{})}
	p.mu.Lock()
	p.subs[sub] = struct{}{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.subs, sub)
		p.mu.Unlock()
	}()
	only := req.GetPodId()
	ctx := stream.Context()
	for {
		select {
		case <-sub.lagged:
			p.mu.Lock()
			missed := sub.missed
			p.mu.Unlock()
			return status.Errorf(codes.DataLoss, "%d events were missed", missed)
		default:
		}
		select {
		case ev := <-sub.ch:
			if only != "" && ev.PodId != only {
				continue
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
		case <-sub.lagged:
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
}

func (p *podService) StreamPodConsole(stream grpc.BidiStreamingServer[podv1.PodConsoleInput, podv1.PodConsoleOutput]) error {
	first, err := stream.Recv()
	if err == io.EOF {
		return status.Error(codes.InvalidArgument, "the console's first message names no pod")
	}
	if err != nil {
		return err
	}
	if _, err := p.reachable(first.GetPodId()); err != nil {
		return err
	}
	// The fake console echoes its input.
	in := first
	for {
		if len(in.GetInput()) > 0 {
			if err := stream.Send(&podv1.PodConsoleOutput{Output: in.GetInput()}); err != nil {
				return err
			}
		}
		if in, err = stream.Recv(); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// reachable is check_reachable: the pod is PENDING or RUNNING.
func (p *podService) reachable(id string) (*pod, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pd := p.pods[id]
	if pd == nil {
		return nil, notFound(id)
	}
	switch pd.info.State {
	case podv1.PodState_POD_STATE_PENDING, podv1.PodState_POD_STATE_RUNNING:
		return pd, nil
	}
	return nil, status.Errorf(codes.FailedPrecondition, "pod %s is %s", id, pd.info.State)
}

// guest is a pod's guest, as a forwarded call reaches it.
type guest struct {
	id         string
	uid        podid.ID
	dir        string
	containers map[string]bool
	env        map[string]map[string]string // the containers' environments by name, from the spec
	// envPod: the pod is an environment's (its spec's label
	// ferun.environment).
	envPod bool
}

// reach is the routing of a forwarded call (forward.rs): the pod must be
// reachable, and its guest must answer, which it does not while the pod
// is PENDING, after a call that was cut left its microVM gone or paused,
// or while the script says so.
func (p *podService) reach(id string) (*guest, error) {
	pd, err := p.reachable(id)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	pending := pd.info.State == podv1.PodState_POD_STATE_PENDING
	down := pd.guestDown
	spec := pd.info.Spec
	p.mu.Unlock()
	switch {
	case pending:
		return nil, status.Errorf(codes.Unavailable, "the guest of pod %s: connection refused (the pod is still starting)", id)
	case down || p.n.script.isUnreachable(pd.uid):
		return nil, status.Errorf(codes.Unavailable, "the guest of pod %s: connection reset by peer", id)
	}
	g := &guest{id: id, uid: pd.uid, dir: p.n.podDir(pd.uid), containers: map[string]bool{"main": true}, env: map[string]map[string]string{},
		envPod: spec.GetLabels()[envLabel] != ""}
	for _, c := range spec.GetContainers() {
		g.containers[c.Name] = true
		g.env[c.Name] = c.GetEnv()
	}
	return g, nil
}

// fail fails a pod as a crashed microVM does.
func (p *podService) fail(uid podid.ID, reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pd := range p.pods {
		if pd.uid == uid {
			if pd.info.State == podv1.PodState_POD_STATE_FAILED && pd.info.Reason == reason {
				return nil
			}
			pd.info.State, pd.info.Reason = podv1.PodState_POD_STATE_FAILED, reason
			p.announceLocked(pd.info, false)
			return nil
		}
	}
	return notFound(uid.String())
}

// InventoryHash is the inventory hash of fleet.proto over pods: the
// lowercase hex SHA-256 of one line "<pod id> <state>\n" per pod that is
// POD_STATE_RUNNING or POD_STATE_SUSPENDED, sorted by pod ID, with the ID
// in its canonical form (a UUID in lowercase hyphenated form) and the
// state as its enum name.
func InventoryHash(pods []*podv1.PodInfo) string {
	var lines []string
	for _, pd := range pods {
		switch pd.GetState() {
		case podv1.PodState_POD_STATE_RUNNING, podv1.PodState_POD_STATE_SUSPENDED:
		default:
			continue
		}
		id := strings.ToLower(pd.GetPodId())
		if u, err := podid.Parse(pd.GetPodId()); err == nil {
			id = u.String()
		}
		lines = append(lines, id+" "+pd.GetState().String()+"\n")
	}
	slices.Sort(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// forget drops a pod's record without an event (Script.ForgetPod).
func (p *podService) forget(uid podid.ID) error {
	p.mu.Lock()
	pd := p.byUIDLocked(uid)
	if pd == nil {
		p.mu.Unlock()
		return notFound(uid.String())
	}
	if pd.bringUp != nil {
		pd.bringUp()
	}
	delete(p.pods, pd.info.PodId)
	p.mu.Unlock()
	p.n.volumes.release(pd.info.PodId)
	p.n.procs.deleted(pd.info.PodId)
	return nil
}

// namesTable reports whether a pod on the node, in any state, names the
// name table id.
func (p *podService) namesTable(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pd := range p.pods {
		if slices.Contains(pd.info.GetSpec().GetNetwork().GetNameTables(), id) {
			return true
		}
	}
	return false
}

// namesGroup reports whether a pod on the node, in any state, names the
// security group id.
func (p *podService) namesGroup(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pd := range p.pods {
		if slices.Contains(pd.info.GetSpec().GetNetwork().GetSecurityGroups(), id) {
			return true
		}
	}
	return false
}

// byUID returns the pod whose ID is uid; p.mu held.
func (p *podService) byUIDLocked(uid podid.ID) *pod {
	for _, pd := range p.pods {
		if pd.uid == uid {
			return pd
		}
	}
	return nil
}

// exitContainer makes the process of a running pod's container exit
// (Script.ExitContainer).
func (p *podService) exitContainer(uid podid.ID, container string, e containerExit) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	pd := p.byUIDLocked(uid)
	if pd == nil {
		return notFound(uid.String())
	}
	if pd.info.State != podv1.PodState_POD_STATE_RUNNING {
		return status.Errorf(codes.FailedPrecondition, "pod %s is %s, not running", pd.info.PodId, pd.info.State)
	}
	if !slices.ContainsFunc(pd.info.GetSpec().GetContainers(), func(c *podv1.ContainerSpec) bool { return c.Name == container }) {
		return status.Errorf(codes.NotFound, "pod %s has no container %s", pd.info.PodId, container)
	}
	if pd.exited == nil {
		pd.exited = map[string]containerExit{}
	}
	pd.exited[container] = e
	return nil
}

// appendLog adds a line to a container's log.
func (p *podService) appendLog(uid podid.ID, container, line string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	pd := p.byUIDLocked(uid)
	if pd == nil {
		return notFound(uid.String())
	}
	if pd.logs == nil {
		pd.logs = map[string][]string{}
	}
	pd.logs[container] = append(pd.logs[container], line)
	return nil
}

// containerLog returns a copy of a container's log lines.
func (p *podService) containerLog(id, container string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	pd := p.pods[id]
	if pd == nil {
		return nil
	}
	return slices.Clone(pd.logs[container])
}
