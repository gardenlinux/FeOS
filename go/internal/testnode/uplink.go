// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync"
	"time"

	"google.golang.org/grpc"
	grpcbackoff "google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// Version is what the test node reports as its feosd version.
const Version = "testnode"

// uplinkDrain bounds how long an uplink the node moved from stays open for
// the calls begun on it: the longest deadline of the node's own uplink
// calls, the renewal's.
const uplinkDrain = 30 * time.Second

// refusal reports whether an uplink call was answered by the refusal
// server: the node's certificate is refused for good.
func refusal(err error) bool { return status.Code(err) == codes.PermissionDenied }

// uplink is a gRPC client of one instance's uplink server, made with one
// identity of the node.
type uplink struct {
	addr   string
	id     *identity
	cc     *grpc.ClientConn
	client fleetv1.FleetServiceClient

	mu    sync.Mutex
	calls int           // the calls begun on it and not yet ended
	idle  chan struct{} // set by drained, closed once calls is 0
}

// begin counts a call on the uplink; end ends it.
func (u *uplink) begin() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
}

func (u *uplink) end() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls--
	if u.calls == 0 && u.idle != nil {
		close(u.idle)
		u.idle = nil
	}
}

// drained returns a channel that is closed once no call is in flight on
// the uplink.
func (u *uplink) drained() <-chan struct{} {
	u.mu.Lock()
	defer u.mu.Unlock()
	ch := make(chan struct{})
	if u.calls == 0 {
		close(ch)
	} else {
		u.idle = ch
	}
	return ch
}

// eventsRun is the events stream of the uplink in use.
type eventsRun struct {
	cancel context.CancelFunc
	done   chan error
	ended  bool
}

// stop ends the stream and waits until it has.
func (r *eventsRun) stop() {
	r.cancel()
	if !r.ended {
		<-r.done
		r.ended = true
	}
}

// uplinks are the uplinks of a session: one to every target address with
// the node's current identity, one of them in use (active), on which the
// events stream runs. Only the session's uplink loop touches them.
type uplinks struct {
	n      *Node
	s      *session
	addrs  []string // the target addresses, in the order listed
	pool   map[string]*uplink
	active *uplink
	events *eventsRun
}

// sync makes the uplinks those of id to addrs: it dials an uplink to every
// address that has none with id, moves off an uplink in use that is not
// among them, and closes the others that are not, each once the calls
// begun on it have ended.
func (p *uplinks) sync(id *identity, addrs []string) {
	p.addrs = addrs
	want := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		want[a] = true
	}
	var gone []*uplink
	for a, u := range p.pool {
		if !want[a] || u.id != id {
			delete(p.pool, a)
			gone = append(gone, u)
		}
	}
	for _, a := range addrs {
		if p.pool[a] != nil {
			continue
		}
		u, err := p.dial(a, id)
		if err != nil {
			p.n.rl.warn("uplink "+a, "test node: making an uplink failed", "addr", a, "err", err)
			continue
		}
		p.pool[a] = u
	}
	if p.active == nil || p.pool[p.active.addr] != p.active {
		var next *uplink
		if p.active != nil {
			next = p.pool[p.active.addr]
		}
		for _, a := range addrs {
			if next != nil {
				break
			}
			next = p.pool[a]
		}
		p.use(next)
	}
	for _, u := range gone {
		p.retire(u)
	}
}

// dial makes the uplink to addr with id, and starts connecting it.
func (p *uplinks) dial(addr string, id *identity) (*uplink, error) {
	cc, err := p.n.newUplink(p.s, addr, id)
	if err != nil {
		return nil, err
	}
	cc.Connect()
	return &uplink{addr: addr, id: id, cc: cc, client: fleetv1.NewFleetServiceClient(cc)}, nil
}

// use moves the node to the uplink u (nil: none): the events stream ends
// on the one before it starts on u, and calls begun from now on go to u.
func (p *uplinks) use(u *uplink) {
	if p.events != nil {
		p.events.stop()
		p.events = nil
	}
	p.active = u
	p.n.mu.Lock()
	p.n.uplink, p.n.uplinkAddr = u, ""
	if u != nil {
		p.n.uplinkAddr = u.addr
	}
	p.n.mu.Unlock()
	if u != nil {
		p.startEvents()
	}
}

// startEvents runs the events stream on the uplink in use.
func (p *uplinks) startEvents() {
	u := p.active
	ctx, cancel := context.WithCancel(p.s.ctx)
	r := &eventsRun{cancel: cancel, done: make(chan error, 1)}
	u.begin()
	go func() {
		defer u.end()
		r.done <- p.n.publishLoop(ctx, u.client)
	}()
	p.events = r
}

// eventsEnded is the channel on which the events stream ends; nil when
// none runs.
func (p *uplinks) eventsEnded() <-chan error {
	if p.events == nil || p.events.ended {
		return nil
	}
	return p.events.done
}

// next moves the node to the uplink of the next address after the one in
// use, after a report failed.
func (p *uplinks) next() {
	if len(p.addrs) == 0 {
		return
	}
	i := -1
	if p.active != nil {
		for j, a := range p.addrs {
			if a == p.active.addr {
				i = j
			}
		}
	}
	for k := 1; k <= len(p.addrs); k++ {
		u := p.pool[p.addrs[(i+k)%len(p.addrs)]]
		if u == nil {
			continue
		}
		if u != p.active {
			p.use(u)
		}
		return
	}
}

// retire closes an uplink the node no longer uses, once the calls begun on
// it have ended (at most uplinkDrain), or at once when the session ends.
func (p *uplinks) retire(u *uplink) {
	idle := u.drained()
	p.s.goOwned(func() {
		t := time.NewTimer(uplinkDrain)
		defer t.Stop()
		select {
		case <-idle:
		case <-t.C:
		case <-p.s.ctx.Done():
		}
		u.cc.Close()
	})
}

// close ends the events stream and closes every uplink at once: the
// session ends, or the node was refused.
func (p *uplinks) close() {
	if p.events != nil {
		p.events.stop()
		p.events = nil
	}
	p.n.mu.Lock()
	if p.active != nil && p.n.uplink == p.active {
		p.n.uplink, p.n.uplinkAddr = nil, ""
	}
	p.n.mu.Unlock()
	for _, u := range p.pool {
		u.cc.Close()
	}
	p.pool, p.active = nil, nil
}

// uplinkLoop runs the uplinks of a session (§17 and §19.1 of the design):
// one to every target address, telemetry every interval on the one in use,
// the events stream on it, and the renewal of the certificate when it is
// due. A report that fails moves the node to the next address at once;
// once a report failed on every address in turn, the next waits with
// backoff. After a renewal, and when the targets change, the node dials
// the uplinks it lacks first, moves the events stream, and closes the
// uplinks it left once their calls have ended. A PERMISSION_DENIED (the
// refusal server) ends Run with ErrRefused.
func (n *Node) uplinkLoop(s *session) {
	ctx := s.ctx
	p := &uplinks{n: n, s: s, pool: map[string]*uplink{}}
	defer p.close()

	id, renewed := n.identityNow()
	addrs, changed := n.targets()
	p.sync(id, addrs)
	n.mesh.newUplink()

	b := newBackoff(n.backoffMin, n.backoffMax)
	failed := 0
	interval := n.defaultInterval
	report := time.NewTimer(0)
	defer report.Stop()
	renewal := time.NewTimer(n.renewDelay(id))
	defer renewal.Stop()
	renewing := false
	renewDone := make(chan error, 1)
	for {
		select {
		case <-ctx.Done():
			return
		case <-changed:
			addrs, changed = n.targets()
			p.sync(id, addrs)
		case <-renewed:
			id, renewed = n.identityNow()
			p.sync(id, addrs)
			if !renewing {
				renewal.Reset(n.renewDelay(id))
			}
		case err := <-p.eventsEnded():
			p.events.ended = true
			if refusal(err) {
				n.refusedBy(p.active.addr, err)
				return
			}
			p.startEvents()
		case <-renewal.C:
			// The renewal runs beside the reports, so that a slow one never
			// holds back the heartbeat.
			u := p.active
			if u == nil {
				renewal.Reset(n.renewRetry)
				continue
			}
			renewing = true
			u.begin()
			s.goOwned(func() {
				defer u.end()
				renewDone <- n.renew(ctx, u.client)
			})
		case err := <-renewDone:
			renewing = false
			switch {
			case err == nil:
				renewal.Reset(n.renewDelay(n.identity()))
			case ctx.Err() != nil:
				return
			case refusal(err):
				n.refusedBy("", err)
				return
			default:
				n.rl.warn("renew", "test node: renewing the certificate failed; retrying", "err", err)
				renewal.Reset(n.renewRetry)
			}
		case <-report.C:
			u := p.active
			if u == nil {
				p.sync(id, addrs)
				report.Reset(b.delay())
				continue
			}
			resp, err := n.report(ctx, u.client)
			switch {
			case ctx.Err() != nil:
				return
			case refusal(err):
				n.refusedBy(u.addr, err)
				return
			case err != nil:
				n.rl.warn("uplink", "test node: report failed; moving to the next instance", "addr", u.addr, "err", err)
				p.next()
				failed++
				if failed >= len(p.addrs) {
					failed = 0
					report.Reset(b.delay())
				} else {
					report.Reset(0)
				}
				continue
			}
			failed = 0
			b.reset()
			n.setInstances(resp.GetInstances())
			if err := n.mesh.join(ctx, u.client); err != nil && ctx.Err() == nil {
				if refusal(err) {
					n.refusedBy(u.addr, err)
					return
				}
				n.rl.warn("mesh", "test node: JoinMesh failed; asking again after the next report", "err", err)
			}
			if s := resp.GetIntervalSeconds(); s > 0 {
				interval = time.Duration(s) * time.Second
			}
			report.Reset(interval)
		}
	}
}

// refusedBy ends Run with ErrRefused: the control plane refused the node's
// certificate, and the node must join again.
func (n *Node) refusedBy(addr string, err error) {
	msg := status.Convert(err).Message()
	n.log.Error("test node: the control plane refused the node; it must join again", "addr", addr, "err", msg)
	n.refuse(fmt.Errorf("%w: %s", ErrRefused, msg))
}

// newUplink makes the gRPC client of an uplink to addr with the identity
// id: every (re)dial is a TLS connection with ALPN "h2", pinged every 10 s.
func (n *Node) newUplink(s *session, addr string, id *identity) (*grpc.ClientConn, error) {
	cfg := n.clientTLS(id, alpnUplink)
	return grpc.NewClient("passthrough:///"+n.cfg.ServerName,
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return n.dialTLS(ctx, s, addr, cfg)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 10 * time.Second, Timeout: 5 * time.Second, PermitWithoutStream: true}),
		grpc.WithDisableServiceConfig(),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           grpcbackoff.Config{BaseDelay: 200 * time.Millisecond, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 5 * time.Second},
			MinConnectTimeout: dialTimeout,
		}),
	)
}

// renewDelay is how long until the node renews: at its certificate's
// renew_after, but never sooner than renewMinGap after the last renewal.
func (n *Node) renewDelay(id *identity) time.Duration {
	n.mu.Lock()
	last := n.lastRenew
	n.mu.Unlock()
	at := id.renewAfter
	if gap := last.Add(n.renewMinGap); gap.After(at) {
		at = gap
	}
	return max(time.Until(at), 0)
}

// publishLoop runs the events stream on the uplink, again after every
// failure, until ctx ends (nil) or the stream is refused (its error).
func (n *Node) publishLoop(ctx context.Context, client fleetv1.FleetServiceClient) error {
	b := newBackoff(200*time.Millisecond, 5*time.Second)
	for {
		err := n.events.publish(ctx, client)
		if ctx.Err() != nil {
			return nil
		}
		if refusal(err) {
			return err
		}
		n.rl.warn("events", "test node: events stream ended; reopening", "err", err)
		if !b.wait(ctx) {
			return nil
		}
	}
}

// report sends one telemetry report, with a 3 s deadline.
func (n *Node) report(ctx context.Context, client fleetv1.FleetServiceClient) (*fleetv1.TelemetryResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, n.reportTimeout)
	defer cancel()
	return client.ReportTelemetry(ctx, n.telemetry())
}

// telemetry is the node's report: its machine, its pods by state and
// their inventory hash, its versions and its object store.
func (n *Node) telemetry() *fleetv1.Telemetry {
	pods := n.pods.list()
	t := &fleetv1.Telemetry{
		StartId:          n.StartID(),
		Hostname:         n.Name(),
		Arch:             arch(),
		CpuModel:         "test node CPU",
		CpuCores:         n.cfg.CPUCores,
		MemoryTotalBytes: n.cfg.MemoryBytes,
		DiskTotalBytes:   n.cfg.DiskBytes,
		InventoryHash:    InventoryHash(pods),
		Versions: &fleetv1.Versions{
			Feosd:       Version,
			Kernel:      "6.12.0-testnode",
			Firecracker: []string{"1.12.0"},
			Guest:       []string{guestVersion},
			Diskd:       Version,
		},
		PodPrefix: n.podPrefix.String(),
	}
	if p := n.mesh.prefix(); p != "" {
		t.PodPrefix = p
	}
	if store := n.cfg.ObjectStore; store != nil {
		t.ObjectStore, t.ObjectStoreId = true, store.ID()
	}
	var memUsed, diskUsed uint64
	for _, p := range pods {
		switch p.GetState() {
		case podv1.PodState_POD_STATE_PENDING:
			t.PodsPending++
		case podv1.PodState_POD_STATE_RUNNING:
			t.PodsRunning++
		case podv1.PodState_POD_STATE_SUSPENDED:
			t.PodsSuspended++
		default:
			t.PodsOther++
		}
		switch p.GetState() {
		case podv1.PodState_POD_STATE_PENDING, podv1.PodState_POD_STATE_RUNNING:
			memUsed += p.GetSpec().GetResources().GetMemoryMib() << 20
		}
		if p.GetState() != podv1.PodState_POD_STATE_HIBERNATED {
			diskUsed += max(podDiskBytes, uint64(p.GetSpec().GetResources().GetDiskGib())<<30)
		}
	}
	t.MemoryAvailableBytes = n.cfg.MemoryBytes - min(memUsed, n.cfg.MemoryBytes)
	t.DiskAvailableBytes = n.cfg.DiskBytes - min(diskUsed, n.cfg.DiskBytes)
	return t
}

func arch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return runtime.GOARCH
}

// useUplink returns the client of the uplink the node uses, and the
// function that ends the call: the node closes an uplink it moved from
// only once the calls begun on it have ended.
func (n *Node) useUplink() (fleetv1.FleetServiceClient, func(), error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	u := n.uplink
	if u == nil {
		return nil, nil, status.Error(codes.Unavailable, "the test node has no uplink")
	}
	u.begin()
	return u.client, u.end, nil
}

// Renew renews the node's certificate now, as it does at renew_after: a
// new key, RenewCertificate on the current uplink, the new identity kept
// before it is used. Then the node dials new node connections and a new
// uplink with it, and leaves the old ones to the control plane.
func (n *Node) Renew(ctx context.Context) error {
	client, done, err := n.useUplink()
	if err != nil {
		return err
	}
	defer done()
	return n.renew(ctx, client)
}

func (n *Node) renew(ctx context.Context, client fleetv1.FleetServiceClient) error {
	n.renewMu.Lock()
	defer n.renewMu.Unlock()
	old := n.identity()
	if old == nil {
		return errors.New("the node has not joined")
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	csr, err := pki.CreateCSR(key, old.name)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := client.RenewCertificate(cctx, &fleetv1.RenewCertificateRequest{Csr: csr})
	if err != nil {
		return err
	}
	id, err := newIdentity(n.cfg.TrustDomain, key, resp.GetCertificate(), old.joinHash)
	if err != nil {
		return fmt.Errorf("the renewal's answer: %w", err)
	}
	if id.name != old.name {
		return fmt.Errorf("the renewal's answer names node %q, not %q", id.name, old.name)
	}
	if n.cfg.StateDir != "" {
		if err := id.save(n.cfg.StateDir); err != nil {
			return fmt.Errorf("keep the renewed identity: %w", err)
		}
	}
	n.mu.Lock()
	n.lastRenew = time.Now()
	n.mu.Unlock()
	n.setIdentity(id)
	n.log.Info("test node: renewed its certificate", "name", id.name, "serial", id.serial(), "previous", old.serial())
	return nil
}

// Credentials are what a pod's feos-authn holds after IssuePodCredentials:
// its key and certificate, the run's HPKE key, and the bindings opened
// from the sealed box.
type Credentials struct {
	Key         ed25519.PrivateKey
	HPKEKey     hpke.PrivateKey
	Certificate *fleetv1.Certificate
	Leaf        *x509.Certificate
	// Bindings is nil when the answer carried none; Version is theirs.
	Bindings *fleetv1.PodBindings
	Version  string
}

// IssuePodCredentials asks for a pod's credentials on the current uplink,
// as the pod's feos-authn does at every start: a new Ed25519 key and HPKE
// key, the signing request, and the sealed bindings opened with
// pki.Open.
func (n *Node) IssuePodCredentials(ctx context.Context, pod podid.ID) (*Credentials, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	hk, err := pki.HPKEKEM().GenerateKey()
	if err != nil {
		return nil, err
	}
	return n.IssuePodCredentialsWith(ctx, pod, key, hk)
}

// IssuePodCredentialsWith asks for a pod's credentials with given keys, as
// feos-authn repeats a request whose answer it lost.
func (n *Node) IssuePodCredentialsWith(ctx context.Context, pod podid.ID, key ed25519.PrivateKey, hk hpke.PrivateKey) (*Credentials, error) {
	client, done, err := n.useUplink()
	if err != nil {
		return nil, err
	}
	defer done()
	csr, err := pki.CreateCSR(key, pod.String())
	if err != nil {
		return nil, err
	}
	resp, err := client.IssuePodCredentials(ctx, &fleetv1.IssuePodCredentialsRequest{
		PodId:         pod.String(),
		Csr:           csr,
		HpkePublicKey: hk.PublicKey().Bytes(),
	})
	if err != nil {
		return nil, err
	}
	c := &Credentials{Key: key, HPKEKey: hk, Certificate: resp.GetCertificate()}
	if len(c.Certificate.GetChain()) == 0 {
		return nil, errors.New("the answer holds no certificate")
	}
	if c.Leaf, err = x509.ParseCertificate(c.Certificate.Chain[0]); err != nil {
		return nil, fmt.Errorf("the pod's certificate: %w", err)
	}
	if pub, ok := c.Leaf.PublicKey.(ed25519.PublicKey); !ok || !pub.Equal(key.Public()) {
		return nil, errors.New("the pod's certificate is not for its key")
	}
	if sealed := resp.GetBindings(); sealed != nil {
		if c.Bindings, err = pki.Open(hk, pod, sealed); err != nil {
			return nil, err
		}
		c.Version = sealed.GetVersion()
	}
	return c, nil
}
