// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// Defaults of what a test node reports of itself.
const (
	defaultCPUCores    = 16
	defaultMemoryBytes = 64 << 30
	defaultDiskBytes   = 1 << 40
	defaultMaxStreams  = 1024
	// podDiskBytes is what a pod's disk takes of the node's disk: feosd's
	// blank data volume is 8 GiB.
	podDiskBytes = 8 << 30
)

// Config is what a test node needs.
type Config struct {
	// Name is the name the node proposes at its join, its hostname: a DNS
	// label. A token pinned to a name joins that name instead.
	Name string
	// JoinToken is the operator's token, "feos-join-<secret>.<pin>". With an
	// identity in StateDir from a join with the same token, the node takes
	// that identity instead of joining.
	JoinToken string
	// ServerName is the DNS name the node checks the control plane's
	// certificate for, whatever address it dials.
	ServerName  string
	TrustDomain string
	// StateDir keeps the node's identity and its pods' directories. Empty:
	// the identity lives in memory and the pods' directories in a temporary
	// directory that Run removes when it returns.
	StateDir string
	// Addrs are the bootstrap addresses, "host:port" of the nodes' port:
	// the node joins through them, and keeps its node connections to them
	// while the control plane lists no instances.
	Addrs []string
	// ObjectStore is the shared fake object store; nil: no object store, and
	// pods do not hibernate here.
	ObjectStore *ObjectStore
	// PendingDelay is how long a pod's bring-up takes: PENDING → RUNNING.
	PendingDelay time.Duration
	// AbortOnCancel makes mutating PodService calls stop where today's
	// feosd does when their request is cancelled (a cancelled RestorePod
	// leaves the pod PENDING without a bring-up, a cancelled SuspendPod
	// leaves the record RUNNING); without it they finish whatever happens
	// to the request, as feosd will (§19.2 of the design). A HibernatePod
	// runs on its own either way, unless ResumeWaitsForHibernation.
	AbortOnCancel bool
	// ResumeWaitsForHibernation plays feosd before a resume could abandon a
	// hibernation: a HibernatePod runs as AbortOnCancel says, a ResumePod
	// waits for the pod's lock behind it, PodInfo.Hibernation is never set,
	// and HibernatePodRequest's no_abandon and request_id are ignored.
	// Without it the node does what feosd does now: a hibernation runs on
	// its own, a HibernatePod that comes while it runs joins it, and a
	// ResumePod abandons it until its head is being put (ADR 0008).
	ResumeWaitsForHibernation bool
	// MaxStreams is the HTTP/2 stream limit the node advertises on its node
	// connections; 1024 when zero.
	MaxStreams uint32
	Log        *slog.Logger

	// CPUCores, MemoryBytes and DiskBytes are what the node reports of its
	// machine; 16 cores, 64 GiB and 1 TiB when zero.
	CPUCores    uint32
	MemoryBytes uint64
	DiskBytes   uint64
	// PodPrefix is the node's IPv6 prefix for pods; a /64 derived from Name
	// when empty. On a mesh the node reports the /64 JoinMesh gave it
	// instead, once it has one.
	PodPrefix string
	// Mesh puts the node on the pods' mesh (ADR 0017), as feosd without
	// --pod-prefix: it asks JoinMesh on its uplink with a key of its own and
	// MeshEndpoint, and serves MeshService. Without, MeshService answers
	// FAILED_PRECONDITION.
	Mesh bool
	// MeshEndpoint is the node's endpoint on the mesh; 127.0.0.1 and a port
	// derived from Name when empty.
	MeshEndpoint string
	// CheckSecurityGroups makes a pod that names a security group the node
	// does not have fail at its bring-up (POD_STATE_FAILED "the node does
	// not have the security groups …"), and its memory restore, as feosd
	// does. Without it the groups a pod names are not checked, as the test
	// node did before it served SecurityGroupService.
	CheckSecurityGroups bool
	// CheckNameTables makes a pod that names a name table the node does not
	// have fail at its bring-up and its memory restore, as feosd does.
	CheckNameTables bool
	// NoNameService makes the node serve no NameService (UNIMPLEMENTED),
	// as a feosd from before it, and ignore the name tables pods name.
	NoNameService bool
	// FakeExec makes every Exec that the script does not answer exit 0
	// without running anything (ferun-testnode --fake-exec): for a control
	// plane whose addons' probes and provisioning run commands the host
	// does not have.
	FakeExec bool
	// ContainerExits make the main container of an environment's pods
	// exit a while after each boot, by their ferun.object label: deploy
	// jobs that finish, a service that crash-loops (ferun-testnode
	// --exit). The first matching rule counts.
	ContainerExits []ExitRule
	// StartLog appends a line to each container's log at every boot of its
	// pod, and one at an exit of ContainerExits (ferun-testnode
	// --start-log), so that a control plane's log tails have something to
	// show.
	StartLog bool
	// FakeProcesses answers the control plane's background-process
	// scripts (process_start, the environment's attached services) in
	// memory without running anything (ferun-testnode --fake-processes):
	// a process runs until it is stopped, its pod boots anew or an
	// ExitRule "process/<name>" ends it, and its log has a start line.
	FakeProcesses bool
	// PortAddrs are where a port forward to a pod's port dials, by port,
	// in place of 127.0.0.1:<port> (the script's SetPortAddr;
	// ferun-testnode --port-addr): a route reaches a local server that
	// cannot listen on the pod's port.
	PortAddrs map[uint32]string
}

// Node is a test node.
type Node struct {
	cfg       Config
	log       *slog.Logger
	rl        *rateLog
	script    *Script
	events    *eventBuffer
	pods      *podService
	watch     *watchService
	tools     *toolService
	container *containerService
	forward   *forwardService
	mesh      *meshService
	volumes   *volumeService
	groups    *groupService
	names     *nameService
	procs     *processService
	podPrefix netip.Prefix
	podsDir   string
	tmpDir    string // removed when Run returns; "" with a StateDir

	// The timings of the node, fields so that tests shorten them.
	backoffMin, backoffMax time.Duration // redials and retries
	reportTimeout          time.Duration // a ReportTelemetry's deadline
	defaultInterval        time.Duration // between reports until the control plane says
	renewMinGap            time.Duration // at least this between two renewals
	renewRetry             time.Duration // after a renewal failed
	gracefulTimeout        time.Duration // a graceful restart's wait for its acknowledgement and calls

	// podCtx owns the pods' goroutines: bring-ups and the mutating calls
	// that run detached from their requests. It ends when Run returns.
	podCtx    context.Context
	podCancel context.CancelFunc
	podMu     sync.Mutex
	podWG     sync.WaitGroup
	podDone   bool

	mu             sync.Mutex
	id             *identity
	idChanged      chan struct{} // closed and replaced when the identity changes
	instances      []*fleetv1.ControlPlaneInstance
	targetsChanged chan struct{} // closed and replaced when the instances change
	startID        string
	uplink         *uplink // the uplink in use
	uplinkAddr     string
	lastRenew      time.Time
	renewMu        sync.Mutex

	// lifeMu serialises Run's start and end, Restart, Crash and Partition.
	lifeMu      sync.Mutex
	ran         bool
	running     bool
	runCtx      context.Context
	sess        *session
	halted      bool
	partitioned bool
	refused     chan error
}

// New makes a test node; Run runs it.
func New(cfg Config) (*Node, error) {
	if err := pki.ValidateTrustDomain(cfg.TrustDomain); err != nil {
		return nil, err
	}
	if cfg.ServerName == "" {
		return nil, errors.New("testnode: no server name")
	}
	if len(cfg.Addrs) == 0 {
		return nil, errors.New("testnode: no address of the control plane")
	}
	if cfg.JoinToken != "" {
		if _, _, err := pki.SplitJoinToken(cfg.JoinToken); err != nil {
			return nil, fmt.Errorf("testnode: %w", err)
		}
		if !pki.IsNodeName(cfg.Name) {
			return nil, fmt.Errorf("testnode: name %q is not a DNS label", cfg.Name)
		}
	} else if cfg.StateDir == "" {
		return nil, errors.New("testnode: neither a join token nor a state directory")
	}
	cfg.ContainerExits = slices.Clone(cfg.ContainerExits)
	for i := range cfg.ContainerExits {
		if err := cfg.ContainerExits[i].compile(); err != nil {
			return nil, fmt.Errorf("testnode: %w", err)
		}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.MaxStreams == 0 {
		cfg.MaxStreams = defaultMaxStreams
	}
	if cfg.CPUCores == 0 {
		cfg.CPUCores = defaultCPUCores
	}
	if cfg.MemoryBytes == 0 {
		cfg.MemoryBytes = defaultMemoryBytes
	}
	if cfg.DiskBytes == 0 {
		cfg.DiskBytes = defaultDiskBytes
	}
	cfg.Addrs = append([]string(nil), cfg.Addrs...)
	prefix, err := podPrefix(cfg.PodPrefix, cfg.Name)
	if err != nil {
		return nil, err
	}
	n := &Node{
		cfg:             cfg,
		log:             cfg.Log,
		rl:              newRateLog(cfg.Log),
		events:          newEventBuffer(defaultEventCapacity),
		podPrefix:       prefix,
		backoffMin:      500 * time.Millisecond,
		backoffMax:      10 * time.Second,
		reportTimeout:   3 * time.Second,
		defaultInterval: 5 * time.Second,
		renewMinGap:     10 * time.Second,
		renewRetry:      30 * time.Second,
		gracefulTimeout: 5 * time.Second,
		idChanged:       make(chan struct{}),
		targetsChanged:  make(chan struct{}),
		refused:         make(chan error, 1),
	}
	if cfg.StateDir != "" {
		n.podsDir = filepath.Join(cfg.StateDir, "pods")
	} else {
		if n.tmpDir, err = os.MkdirTemp("", "ferun-testnode-"); err != nil {
			return nil, err
		}
		n.podsDir = filepath.Join(n.tmpDir, "pods")
	}
	if err := os.MkdirAll(n.podsDir, 0o755); err != nil {
		return nil, err
	}
	n.script = newScript(n)
	for port, addr := range cfg.PortAddrs {
		n.script.SetPortAddr(port, addr)
	}
	n.pods = newPodService(n)
	n.watch = newWatchService(n)
	n.tools = &toolService{n: n}
	n.container = &containerService{n: n}
	n.forward = &forwardService{n: n}
	n.volumes = newVolumeService(n)
	n.groups = newGroupService(n)
	n.names = newNameService(n)
	n.procs = newProcessService(n)
	if n.mesh, err = newMeshService(n); err != nil {
		return nil, err
	}
	n.podCtx, n.podCancel = context.WithCancel(context.Background())
	n.newStart()
	return n, nil
}

// podPrefix parses the configured prefix, or derives a unique local /64
// from the node's name.
func podPrefix(s, name string) (netip.Prefix, error) {
	if s != "" {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("testnode: pod prefix: %w", err)
		}
		return p.Masked(), nil
	}
	h := sha256.Sum256([]byte(name))
	var a [16]byte
	a[0] = 0xfd
	copy(a[1:8], h[:7])
	return netip.PrefixFrom(netip.AddrFrom16(a), 64), nil
}

// newStart begins a new start of the node: a new start ID, whose first
// event says the node started.
func (n *Node) newStart() {
	id := uuid.NewString()
	n.mu.Lock()
	n.startID = id
	n.mu.Unlock()
	n.events.newStart(id)
	n.events.add(nodeEvent("started", "the test node started"))
}

func nodeEvent(kind, message string) *fleetv1.Event {
	return &fleetv1.Event{Body: &fleetv1.Event_Node{Node: &fleetv1.NodeEvent{Kind: kind, Message: message}}}
}

// Run joins the node, or takes its identity from the state directory, and
// keeps it connected until ctx ends (nil). It returns an error when the
// join fails for good (ErrUntrusted, ErrJoinRefused) and when the control
// plane refuses the node's certificate (ErrRefused); the node's pods stay
// as they are.
func (n *Node) Run(ctx context.Context) error {
	n.lifeMu.Lock()
	if n.ran {
		n.lifeMu.Unlock()
		return errors.New("testnode: Run called twice")
	}
	n.ran, n.running, n.runCtx = true, true, ctx
	n.lifeMu.Unlock()
	defer n.shutdown()

	id, err := n.loadOrJoin(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	n.setIdentity(id)
	n.lifeMu.Lock()
	n.startSessionLocked()
	n.lifeMu.Unlock()
	select {
	case <-ctx.Done():
		return nil
	case err := <-n.refused:
		return err
	}
}

// shutdown ends Run: the connections go at once, then the pods'
// goroutines.
func (n *Node) shutdown() {
	n.lifeMu.Lock()
	n.running = false
	n.stopSessionLocked(false)
	n.lifeMu.Unlock()
	n.podMu.Lock()
	n.podDone = true
	n.podMu.Unlock()
	n.podCancel()
	n.podWG.Wait()
	if n.tmpDir != "" {
		if err := os.RemoveAll(n.tmpDir); err != nil {
			n.log.Warn("test node: its temporary directory stays", "dir", n.tmpDir, "err", err)
		}
	}
}

// goPods runs f in a goroutine the node owns, with the pods' context;
// false once Run has ended.
func (n *Node) goPods(f func(ctx context.Context)) bool {
	n.podMu.Lock()
	defer n.podMu.Unlock()
	if n.podDone {
		return false
	}
	n.podWG.Add(1)
	go func() {
		defer n.podWG.Done()
		f(n.podCtx)
	}()
	return true
}

// refuse ends Run with ErrRefused.
func (n *Node) refuse(err error) {
	select {
	case n.refused <- err:
	default:
	}
}

// startSessionLocked starts dialling, unless the node is halted,
// partitioned, not running or not joined yet.
func (n *Node) startSessionLocked() {
	if n.sess != nil || !n.running || n.halted || n.partitioned || n.identity() == nil {
		return
	}
	n.sess = n.newSession(n.runCtx)
}

// stopSessionLocked ends the connections: gracefully (GOAWAY, the calls in
// flight finish within the graceful timeout) or at once, without a
// goodbye.
func (n *Node) stopSessionLocked(graceful bool) {
	s := n.sess
	n.sess = nil
	if s != nil {
		s.stop(graceful, n.gracefulTimeout)
	}
}

// Restart restarts the node as feosd restarts: its connections end and
// come back, under a new start ID; the pods stay. A graceful restart first
// publishes a "stopping" NodeEvent and waits (a few seconds at most) for
// its acknowledgement, and lets the calls in flight finish. Restart also
// brings a crashed node back.
func (n *Node) Restart(graceful bool) {
	n.lifeMu.Lock()
	defer n.lifeMu.Unlock()
	if graceful {
		n.mu.Lock()
		start := n.startID
		n.mu.Unlock()
		seq := n.events.add(nodeEvent("stopping", "the test node restarts"))
		if n.sess != nil {
			ctx, cancel := context.WithTimeout(n.sess.ctx, n.gracefulTimeout)
			n.events.waitAcked(ctx, start, seq)
			cancel()
		}
	}
	n.stopSessionLocked(graceful)
	n.newStart()
	n.halted = false
	n.startSessionLocked()
}

// Crash drops the node's connections without a goodbye, as a crashed
// feosd does, and keeps the node away until Restart; the pods stay.
func (n *Node) Crash() {
	n.lifeMu.Lock()
	defer n.lifeMu.Unlock()
	n.halted = true
	n.stopSessionLocked(false)
}

// Partition cuts the node off (on) or heals the cut (off): while cut, its
// connections are gone and it neither dials nor reports, and its pods keep
// running; healed, it dials again under the same start ID.
func (n *Node) Partition(on bool) {
	n.lifeMu.Lock()
	defer n.lifeMu.Unlock()
	n.partitioned = on
	if on {
		n.stopSessionLocked(false)
		return
	}
	n.startSessionLocked()
}

// Pods returns the node's pods' records, sorted by ID.
func (n *Node) Pods() []*podv1.PodInfo { return n.pods.list() }

// LogLabels are the log labels the create of a pod the node holds sent
// (CreatePodRequest.log_labels); nil when it holds none.
func (n *Node) LogLabels(pod podid.ID) map[string]string {
	l, _ := n.pods.logLabelsOf(pod.String())
	return l
}

// LogRedact are the values the create of a pod the node holds named to
// redact of its shipped lines (CreatePodRequest.log_redact).
func (n *Node) LogRedact(pod podid.ID) []string {
	_, r := n.pods.logLabelsOf(pod.String())
	return r
}

// Script returns the node's script of faults and scripted answers.
func (n *Node) Script() *Script { return n.script }

// PodDir is the directory of the pod's fake guest: where its commands run
// and what its tools see as every container's root.
func (n *Node) PodDir(pod podid.ID) string { return n.podDir(pod) }

func (n *Node) podDir(pod podid.ID) string { return filepath.Join(n.podsDir, pod.String()) }

// Name is the node's name once it joined, or took its identity; "" before.
func (n *Node) Name() string {
	if id := n.identity(); id != nil {
		return id.name
	}
	return ""
}

// StartID is the ID of the node's current start.
func (n *Node) StartID() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.startID
}

// Certificate is the node's current certificate with its key, the one it
// dials with; false before the join.
func (n *Node) Certificate() (tls.Certificate, bool) {
	if id := n.identity(); id != nil {
		return id.cert, true
	}
	return tls.Certificate{}, false
}

// Serial is the serial of the node's current certificate, as the control
// plane keeps it; "" before the join.
func (n *Node) Serial() string {
	if id := n.identity(); id != nil {
		return id.serial()
	}
	return ""
}

// Publish queues an event (an audit or traffic record, or one without a
// body) as the node's next: it gets the start's next sequence number and,
// unless it has one, the time.
func (n *Node) Publish(ev *fleetv1.Event) uint64 { return n.events.add(ev) }

// EventsPending is the number of events not yet acknowledged, over all
// starts.
func (n *Node) EventsPending() int { return n.events.pending() }

// EventsDropped is the number of events the current start dropped because
// its buffer was full.
func (n *Node) EventsDropped() uint64 { return n.events.droppedNow() }

// Connections returns the address of every node connection the node holds
// open, once per connection: a renewal holds an old one beside the new
// until the control plane closes it.
func (n *Node) Connections() []string {
	n.lifeMu.Lock()
	s := n.sess
	n.lifeMu.Unlock()
	if s == nil {
		return nil
	}
	return s.linkAddrs()
}

// UplinkAddr is the address of the uplink the node uses; "" when it has
// none.
func (n *Node) UplinkAddr() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.uplinkAddr
}

func (n *Node) identity() *identity {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.id
}

// identityNow returns the identity and the channel that is closed when it
// changes.
func (n *Node) identityNow() (*identity, <-chan struct{}) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.id, n.idChanged
}

func (n *Node) setIdentity(id *identity) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.id = id
	close(n.idChanged)
	n.idChanged = make(chan struct{})
}

// setInstances takes the instances the control plane listed.
func (n *Node) setInstances(list []*fleetv1.ControlPlaneInstance) {
	n.mu.Lock()
	defer n.mu.Unlock()
	same := len(list) == len(n.instances)
	for i := 0; same && i < len(list); i++ {
		same = list[i].GetId() == n.instances[i].GetId() && list[i].GetNodeAddress() == n.instances[i].GetNodeAddress()
	}
	if same {
		return
	}
	n.instances = list
	close(n.targetsChanged)
	n.targetsChanged = make(chan struct{})
}

// targets are the addresses the node keeps node connections to: those of
// the listed instances, or the bootstrap addresses while the list is
// empty.
func (n *Node) targets() ([]string, <-chan struct{}) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var addrs []string
	seen := map[string]bool{}
	for _, in := range n.instances {
		if a := in.GetNodeAddress(); a != "" && !seen[a] {
			seen[a] = true
			addrs = append(addrs, a)
		}
	}
	if len(addrs) == 0 {
		addrs = append(addrs, n.cfg.Addrs...)
	}
	return addrs, n.targetsChanged
}
