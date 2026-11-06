// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/mesh"
	"github.com/gardenlinux/feos/go/internal/pki"
)

// FakeControlPlane is a stand-in for ferund's side of the ingress
// protocol, FOR TESTS ONLY (of this package, of the program and of the
// data path): it is not part of ferun-ingress's behaviour and nothing
// outside tests may use it.
//
// It listens on a loopback port with a certificate from its own CA (made
// with pki.InitCA), negotiates ALPN ferun-ingress/1, and hands a
// connection without a client certificate to its join server and one
// with a certificate of the CA to its ingress server, both gRPC with the
// JSON codec, as the nodes' port of ferund does. Its IngressService is
// scriptable: Join issues certificates with IssueIngress for tokens made
// with Token; Hello fixes the mesh key per ingress as ferund does and
// hands out places on the mesh; Sync streams a whole table, then a delta
// for every change made with PutEnv, RemoveEnv, SetMesh and
// SetAdmittedVersion, and a heartbeat every 10 s, each table in parts
// past wire.PartBytes (SetPartBytes); Wake and Hold are recorded.
// Refusals, stream ends, broken connections and sequence gaps can be
// injected.
type FakeControlPlane struct {
	iss        *pki.Issuer
	td         string
	appsZone   string
	tenantZone string
	podNetwork netip.Prefix
	lis        net.Listener
	addr       string
	tlsCfg     *tls.Config
	join       *grpc.Server
	ingress    *grpc.Server
	joinLis    *fakeListener
	ingressLis *fakeListener
	wg         sync.WaitGroup

	mu         sync.Mutex
	closed     bool
	conns      map[*fakeConn]struct{}
	tokens     map[string]*fakeToken // by hash
	certTTL    time.Duration
	renewAfter time.Duration
	joinBundle [][]byte          // the trust bundle of Join answers; nil: the CA's
	refused    map[string]string // ingress name → refusal text
	helloErr   error
	wakeDelay  time.Duration
	partBytes  int               // a table larger goes in parts (wire.Split)
	holdRefuse map[string]string // instance → reason
	keys       map[string][]byte // ingress name → its fixed mesh key
	retired    map[string][]byte // ingress name → the key its last join cleared
	subnets    map[string]int
	helloed    map[string]bool // connections (remote address) that said Hello
	admitted   map[string]uint64
	envs       map[string]wire.Env
	mesh       wire.Mesh
	streams    map[*fakeStream]struct{}
	joins      []FakeJoin
	hellos     []FakeHello
	renewals   []FakeRenewal
	wakes      []wire.WakeRequest
	holds      []FakeHold
}

// FakeConfig configures a FakeControlPlane; every field has a default.
type FakeConfig struct {
	// TrustDomain is the CA's trust domain and DNS domain; the control
	// plane's name is ferun.<TrustDomain>. Default "ingress.test".
	TrustDomain string
	// AppsZone is the zone Hello and Self answer. Default "apps.test".
	AppsZone string
	// TenantZone is the tenant zone Hello and Self answer; "": none.
	TenantZone string
	// PodNetwork is the cluster's ULA. Default fd00:1:2::/48.
	PodNetwork netip.Prefix
	// CertTTL is the lifetime of issued certificates. Default 1 h.
	CertTTL time.Duration
}

// FakeJoin is a Join the fake control plane received: the name proposed,
// the hash of the token sent, and the refusal it answered, if any.
type FakeJoin struct {
	Name      string
	TokenHash string
	Refusal   string
}

// FakeHello is a Hello: the ingress, the serial of the certificate its
// connection presented, and the request.
type FakeHello struct {
	Name, Serial string
	Request      wire.HelloRequest
}

// FakeRenewal is a renewal: the serials of the presented and the issued
// certificate.
type FakeRenewal struct {
	Name, Old, New string
}

// FakeHold is a Hold of an ingress.
type FakeHold struct {
	Name    string
	Request wire.HoldRequest
}

type fakeToken struct {
	name    string
	used    bool
	keyHash string
	usedAt  time.Time
	resp    *wire.JoinResponse
}

// NewFakeControlPlane makes the CA in dir (which must be empty or
// missing) and starts serving on a loopback port. Close stops it.
func NewFakeControlPlane(dir string, cfg FakeConfig) (*FakeControlPlane, error) {
	if cfg.TrustDomain == "" {
		cfg.TrustDomain = "ingress.test"
	}
	if cfg.AppsZone == "" {
		cfg.AppsZone = "apps.test"
	}
	if !cfg.PodNetwork.IsValid() {
		cfg.PodNetwork = netip.MustParsePrefix("fd00:1:2::/48")
	}
	if cfg.CertTTL == 0 {
		cfg.CertTTL = time.Hour
	}
	caDir := filepath.Join(dir, "ca")
	if err := pki.InitCA(pki.InitCAOptions{RootDir: filepath.Join(dir, "root"), Dir: caDir, TrustDomain: cfg.TrustDomain, DNSDomains: []string{cfg.TrustDomain}}); err != nil {
		return nil, err
	}
	iss, err := pki.LoadIssuer(caDir, cfg.TrustDomain)
	if err != nil {
		return nil, err
	}
	srvCert, err := iss.IssueServer([]string{iss.ControlPlaneDomain()}, 24*time.Hour)
	if err != nil {
		return nil, err
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &FakeControlPlane{
		iss:        iss,
		td:         cfg.TrustDomain,
		appsZone:   cfg.AppsZone,
		tenantZone: cfg.TenantZone,
		podNetwork: cfg.PodNetwork.Masked(),
		lis:        lis,
		addr:       lis.Addr().String(),
		joinLis:    newFakeListener(lis.Addr()),
		ingressLis: newFakeListener(lis.Addr()),
		conns:      map[*fakeConn]struct{}{},
		tokens:     map[string]*fakeToken{},
		certTTL:    cfg.CertTTL,
		partBytes:  wire.PartBytes,
		refused:    map[string]string{},
		holdRefuse: map[string]string{},
		keys:       map[string][]byte{},
		retired:    map[string][]byte{},
		subnets:    map[string]int{},
		helloed:    map[string]bool{},
		admitted:   map[string]uint64{},
		envs:       map[string]wire.Env{},
		streams:    map[*fakeStream]struct{}{},
	}
	f.tlsCfg = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{*srvCert},
		NextProtos:   []string{wire.ALPN},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    iss.Bundle(),
	}
	svc := &fakeService{f: f}
	f.join = grpc.NewServer(
		grpc.Creds(fakeCreds{}),
		grpc.ForceServerCodec(wire.Codec{}),
		grpc.ChainUnaryInterceptor(f.joinUnary),
		grpc.ChainStreamInterceptor(func(any, grpc.ServerStream, *grpc.StreamServerInfo, grpc.StreamHandler) error {
			return status.Error(codes.Unimplemented, "the join server serves Join only")
		}),
	)
	f.ingress = grpc.NewServer(
		grpc.Creds(fakeCreds{}),
		grpc.ForceServerCodec(wire.Codec{}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
		grpc.ChainUnaryInterceptor(f.ingressUnary),
		grpc.ChainStreamInterceptor(f.ingressStream),
	)
	wire.RegisterIngressServer(f.join, svc)
	wire.RegisterIngressServer(f.ingress, svc)
	f.wg.Add(3)
	go func() {
		defer f.wg.Done()
		_ = f.join.Serve(f.joinLis)
	}()
	go func() {
		defer f.wg.Done()
		_ = f.ingress.Serve(f.ingressLis)
	}()
	go func() {
		defer f.wg.Done()
		f.accept()
	}()
	return f, nil
}

// Addr is the address the fake control plane listens on.
func (f *FakeControlPlane) Addr() string { return f.addr }

// ServerName is the name its certificate carries: an agent's
// ControlPlaneName.
func (f *FakeControlPlane) ServerName() string { return f.iss.ControlPlaneDomain() }

// TrustDomain is its trust domain: an agent's TrustDomain.
func (f *FakeControlPlane) TrustDomain() string { return f.td }

// Issuer is its CA.
func (f *FakeControlPlane) Issuer() *pki.Issuer { return f.iss }

// Close stops serving and cuts every connection.
func (f *FakeControlPlane) Close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	f.mu.Unlock()
	_ = f.lis.Close()
	f.CloseConns()
	f.join.Stop()
	f.ingress.Stop()
	f.wg.Wait()
}

// Token makes a one-time join token for ingress name ("" for a token
// that is not pinned and takes the name the Join proposes) and returns
// it as an operator gets it, "feos-ingress-join-<secret>.<pin>".
func (f *FakeControlPlane) Token(name string) (string, error) {
	tok, _, hash, err := pki.NewIngressJoinToken(f.iss.JoinPin())
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	f.tokens[hash] = &fakeToken{name: name}
	f.mu.Unlock()
	return tok, nil
}

// SetCertTTL sets the lifetime of the certificates issued from now on.
func (f *FakeControlPlane) SetCertTTL(d time.Duration) {
	f.mu.Lock()
	f.certTTL = d
	f.mu.Unlock()
}

// SetRenewAfter makes the certificates issued from now on due for
// renewal d after they are issued; 0 keeps the issuer's renew time (half
// the lifetime).
func (f *FakeControlPlane) SetRenewAfter(d time.Duration) {
	f.mu.Lock()
	f.renewAfter = d
	f.mu.Unlock()
}

// SetJoinTrustBundle makes the Join answers from now on carry bundle (DER
// certificates) as their trust bundle; nil answers the CA's again. A
// bundle without the CA's root fails the agent's pin check of the answer,
// after the token was sent.
func (f *FakeControlPlane) SetJoinTrustBundle(bundle [][]byte) {
	f.mu.Lock()
	f.joinBundle = slices.Clone(bundle)
	f.mu.Unlock()
}

// Refuse makes every call of ingress name's certificates fail with
// PERMISSION_DENIED and text, and ends its Sync streams with it. A join
// of the ingress lifts the refusal.
func (f *FakeControlPlane) Refuse(name, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refused[name] = text
	for st := range f.streams {
		if st.name == name {
			st.end(status.Error(codes.PermissionDenied, text))
		}
	}
}

// Unrefuse lifts Refuse.
func (f *FakeControlPlane) Unrefuse(name string) {
	f.mu.Lock()
	delete(f.refused, name)
	f.mu.Unlock()
}

// RetireKey does to ingress name's mesh key what a join again does at
// the control plane: the key is no longer fixed, and the next Hello may
// not bring it back ("…; make a new one").
func (f *FakeControlPlane) RetireKey(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if k, ok := f.keys[name]; ok {
		f.retired[name] = k
		delete(f.keys, name)
	}
}

// SetHelloError makes every Hello fail with err (a gRPC status error);
// nil answers Hello again.
func (f *FakeControlPlane) SetHelloError(err error) {
	f.mu.Lock()
	f.helloErr = err
	f.mu.Unlock()
}

// SetPartBytes makes every table that takes more than n bytes go in
// parts (wire.Split); the default is wire.PartBytes.
func (f *FakeControlPlane) SetPartBytes(n int) {
	f.mu.Lock()
	f.partBytes = n
	f.mu.Unlock()
}

// SetWakeDelay makes every Wake take d before it answers.
func (f *FakeControlPlane) SetWakeDelay(d time.Duration) {
	f.mu.Lock()
	f.wakeDelay = d
	f.mu.Unlock()
}

// RefuseHold makes Hold refuse instance with reason; "" holds it again.
func (f *FakeControlPlane) RefuseHold(instance, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if reason == "" {
		delete(f.holdRefuse, instance)
		return
	}
	f.holdRefuse[instance] = reason
}

// PutEnv puts environments into the table and sends them to every
// stream.
func (f *FakeControlPlane) PutEnv(envs ...wire.Env) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range envs {
		f.envs[e.ID] = e
	}
	f.broadcastLocked(func(*fakeStream) *wire.Table { return &wire.Table{Envs: slices.Clone(envs)} })
}

// RemoveEnv removes environments from the table and from every stream's.
func (f *FakeControlPlane) RemoveEnv(ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		delete(f.envs, id)
	}
	f.broadcastLocked(func(*fakeStream) *wire.Table { return &wire.Table{Removed: slices.Clone(ids)} })
}

// SetMesh sets the mesh's nodes and version and sends them to every
// stream.
func (f *FakeControlPlane) SetMesh(m wire.Mesh) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mesh = m
	f.broadcastLocked(func(*fakeStream) *wire.Table { mc := m; return &wire.Table{Mesh: &mc} })
}

// SetAdmittedVersion sets ingress name's admitted version (Self) and
// sends it to the ingress's streams.
func (f *FakeControlPlane) SetAdmittedVersion(name string, v uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.admitted[name] = v
	f.broadcastLocked(func(st *fakeStream) *wire.Table {
		if st.name != name {
			return nil
		}
		self := f.selfLocked(name)
		return &wire.Table{Self: &self}
	})
}

// Heartbeat sends every stream an empty message.
func (f *FakeControlPlane) Heartbeat() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcastLocked(func(*fakeStream) *wire.Table { return &wire.Table{} })
}

// SkipSeq makes the next message of every stream skip a sequence number,
// as if one was lost.
func (f *FakeControlPlane) SkipSeq() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for st := range f.streams {
		st.seq++
	}
}

// CloseStreams ends every Sync stream with err; nil ends them as a server
// does that finished (status OK).
func (f *FakeControlPlane) CloseStreams(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for st := range f.streams {
		st.end(err)
	}
}

// CloseConns cuts every connection, as a crashed instance does.
func (f *FakeControlPlane) CloseConns() {
	f.mu.Lock()
	conns := make([]*fakeConn, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// Streams is the number of open Sync streams.
func (f *FakeControlPlane) Streams() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.streams)
}

// Joins returns the Joins received, in order.
func (f *FakeControlPlane) Joins() []FakeJoin {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.joins)
}

// Hellos returns the Hellos that reached the service, in order:
// answered, or failed by SetHelloError, a fixed key or a retired one (not
// those Refuse refused).
func (f *FakeControlPlane) Hellos() []FakeHello {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.hellos)
}

// Renewals returns the renewals, in order.
func (f *FakeControlPlane) Renewals() []FakeRenewal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.renewals)
}

// Wakes returns the Wakes received, in order.
func (f *FakeControlPlane) Wakes() []wire.WakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.wakes)
}

// Holds returns the Holds received, in order.
func (f *FakeControlPlane) Holds() []FakeHold {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.holds)
}

// Place returns the place on the mesh Hello gave ingress name: its /64
// and address; false before its first Hello.
func (f *FakeControlPlane) Place(name string) (prefix netip.Prefix, addr netip.Addr, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub, ok := f.subnets[name]
	if !ok {
		return netip.Prefix{}, netip.Addr{}, false
	}
	p := mesh.NodePrefix(f.podNetwork, sub)
	return p, mesh.HostAddress(p), true
}

// accept handshakes every connection and hands it to the join server
// (no client certificate) or the ingress server.
func (f *FakeControlPlane) accept() {
	for {
		c, err := f.lis.Accept()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.handshake(c)
		}()
	}
}

func (f *FakeControlPlane) handshake(raw net.Conn) {
	tc := tls.Server(raw, f.tlsCfg)
	fc := &fakeConn{Conn: tc, f: f}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		raw.Close()
		return
	}
	f.conns[fc] = struct{}{}
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tc.HandshakeContext(ctx); err != nil {
		fc.Close()
		return
	}
	if tc.ConnectionState().NegotiatedProtocol != wire.ALPN {
		fc.Close()
		return
	}
	if len(tc.ConnectionState().PeerCertificates) == 0 {
		f.joinLis.hand(fc)
		return
	}
	f.ingressLis.hand(fc)
}

// fakePeer is the ingress of a call on the ingress server.
type fakePeer struct {
	name, serial, conn string
}

type fakePeerKey struct{}

// peerOf identifies the ingress of a call from its certificate.
func (f *FakeControlPlane) peerOf(ctx context.Context) (fakePeer, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return fakePeer{}, status.Error(codes.Unauthenticated, "no peer")
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.PeerCertificates) == 0 {
		return fakePeer{}, status.Error(codes.Unauthenticated, "no client certificate")
	}
	leaf := ti.State.PeerCertificates[0]
	name, err := pki.ParseIngressID(f.td, leaf)
	if err != nil {
		return fakePeer{}, status.Error(codes.Unauthenticated, err.Error())
	}
	return fakePeer{name: name, serial: pki.Serial(leaf), conn: p.Addr.String()}, nil
}

func (f *FakeControlPlane) checkPeer(ctx context.Context, method string) (context.Context, error) {
	if method == wire.MethodJoin {
		return nil, status.Error(codes.Unimplemented, "Join is served on connections without a client certificate")
	}
	p, err := f.peerOf(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	text, refused := f.refused[p.name]
	f.mu.Unlock()
	if refused {
		return nil, status.Error(codes.PermissionDenied, text)
	}
	return context.WithValue(ctx, fakePeerKey{}, p), nil
}

func (f *FakeControlPlane) joinUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if info.FullMethod != wire.MethodJoin {
		return nil, status.Error(codes.Unimplemented, "the join server serves Join only")
	}
	return handler(ctx, req)
}

func (f *FakeControlPlane) ingressUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	ctx, err := f.checkPeer(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (f *FakeControlPlane) ingressStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx, err := f.checkPeer(ss.Context(), info.FullMethod)
	if err != nil {
		return err
	}
	return handler(srv, &ctxStream{ServerStream: ss, ctx: ctx})
}

type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *ctxStream) Context() context.Context { return s.ctx }

func fakePeerFrom(ctx context.Context) fakePeer {
	p, _ := ctx.Value(fakePeerKey{}).(fakePeer)
	return p
}

// issueLocked issues a certificate of ingress name for csr.
func (f *FakeControlPlane) issueLocked(csr []byte, name string) (wire.Certificate, error) {
	c, _, err := f.iss.IssueIngress(csr, name, f.certTTL)
	if err != nil {
		return wire.Certificate{}, err
	}
	wc := wire.CertificateFromProto(c)
	if f.renewAfter > 0 {
		wc.RenewAfter = time.Now().Add(f.renewAfter)
	}
	return wc, nil
}

func (f *FakeControlPlane) selfLocked(name string) wire.Self {
	s := wire.Self{Name: name, AppsZone: f.appsZone, TenantZone: f.tenantZone, AdmittedVersion: f.admitted[name]}
	if sub, ok := f.subnets[name]; ok {
		s.Address = mesh.HostAddress(mesh.NodePrefix(f.podNetwork, sub)).String()
	}
	return s
}

// wholeLocked is the first message of a stream of ingress name.
func (f *FakeControlPlane) wholeLocked(name string) *wire.Table {
	envs := make([]wire.Env, 0, len(f.envs))
	for _, e := range f.envs {
		envs = append(envs, e)
	}
	slices.SortFunc(envs, func(x, y wire.Env) int { return strings.Compare(x.ID, y.ID) })
	m := f.mesh
	self := f.selfLocked(name)
	return &wire.Table{Seq: 1, Whole: true, Envs: envs, Mesh: &m, Self: &self, SentAt: time.Now()}
}

// broadcastLocked sends every stream the message build makes for it (nil:
// nothing), numbered in the stream's sequence.
func (f *FakeControlPlane) broadcastLocked(build func(*fakeStream) *wire.Table) {
	for st := range f.streams {
		t := build(st)
		if t == nil {
			continue
		}
		t.Seq, t.SentAt = st.seq+1, time.Now()
		f.pushLocked(st, t)
	}
}

// pushLocked queues t, numbered already, on stream st, in parts when it
// is larger than partBytes, and moves the stream's sequence to its last
// part.
func (f *FakeControlPlane) pushLocked(st *fakeStream, t *wire.Table) {
	parts, err := wire.Split(t, f.partBytes)
	if err != nil {
		st.end(status.Errorf(codes.Internal, "code the table: %v", err))
		return
	}
	for _, p := range parts {
		st.push(p)
	}
	st.seq = parts[len(parts)-1].Seq
}

// fakeService is the fake's IngressService.
type fakeService struct {
	wire.UnimplementedIngressServer
	f *FakeControlPlane
}

func (s *fakeService) Join(_ context.Context, req *wire.JoinRequest) (*wire.JoinResponse, error) {
	f := s.f
	_, keyHash, err := pki.CheckCSR(req.CSR, true)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if !pki.IsIngressJoinSecret(req.Token) {
		return nil, status.Error(codes.InvalidArgument, "malformed join token")
	}
	hash := pki.HashJoinSecret(req.Token)
	f.mu.Lock()
	defer f.mu.Unlock()
	refuse := func(code codes.Code, text string) error {
		f.joins = append(f.joins, FakeJoin{Name: req.Name, TokenHash: hash, Refusal: text})
		return status.Error(code, text)
	}
	tok := f.tokens[hash]
	switch {
	case tok == nil:
		return nil, refuse(codes.PermissionDenied, "unknown join token")
	case tok.used && tok.keyHash == keyHash && time.Since(tok.usedAt) < 10*time.Minute:
		f.joins = append(f.joins, FakeJoin{Name: req.Name, TokenHash: hash})
		resp := *tok.resp
		if f.joinBundle != nil {
			resp.Certificate.TrustBundle = slices.Clone(f.joinBundle)
		}
		return &resp, nil
	case tok.used:
		return nil, refuse(codes.PermissionDenied, "join token was used")
	}
	name := tok.name
	if name == "" {
		name = req.Name
	}
	if !pki.IsNodeName(name) {
		return nil, refuse(codes.InvalidArgument, fmt.Sprintf("ingress name %q is not a DNS label", name))
	}
	cert, err := f.issueLocked(req.CSR, name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &wire.JoinResponse{Name: name, Certificate: cert}
	tok.used, tok.keyHash, tok.usedAt, tok.resp = true, keyHash, time.Now(), resp
	// A join is a re-join: the mesh key must change, a refusal ends.
	if k, ok := f.keys[name]; ok {
		f.retired[name] = k
	}
	delete(f.keys, name)
	delete(f.refused, name)
	f.joins = append(f.joins, FakeJoin{Name: req.Name, TokenHash: hash})
	out := *resp
	if f.joinBundle != nil {
		out.Certificate.TrustBundle = slices.Clone(f.joinBundle)
	}
	return &out, nil
}

func (s *fakeService) RenewCertificate(ctx context.Context, req *wire.RenewRequest) (*wire.RenewResponse, error) {
	f := s.f
	p := fakePeerFrom(ctx)
	if _, _, err := pki.CheckCSR(req.CSR, true); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cert, err := f.issueLocked(req.CSR, p.name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	f.renewals = append(f.renewals, FakeRenewal{Name: p.name, Old: p.serial, New: certSerial(cert)})
	return &wire.RenewResponse{Certificate: cert}, nil
}

func (s *fakeService) Hello(ctx context.Context, req *wire.HelloRequest) (*wire.HelloResponse, error) {
	f := s.f
	p := fakePeerFrom(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hellos = append(f.hellos, FakeHello{Name: p.name, Serial: p.serial, Request: cloneHello(*req)})
	if f.helloErr != nil {
		return nil, f.helloErr
	}
	if len(req.PublicKey) != 32 {
		return nil, status.Errorf(codes.InvalidArgument, "a mesh key of %d bytes", len(req.PublicKey))
	}
	if _, err := mesh.CheckEndpoint(req.Endpoint); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	k, fixed := f.keys[p.name]
	if fixed && !bytes.Equal(k, req.PublicKey) {
		return nil, status.Errorf(codes.FailedPrecondition, "the mesh key of ingress %s is fixed; join it again to change it", p.name)
	}
	if r, ok := f.retired[p.name]; !fixed && ok && bytes.Equal(r, req.PublicKey) {
		return nil, status.Errorf(codes.FailedPrecondition, "the mesh key of ingress %s was retired by its join again; make a new one", p.name)
	}
	f.keys[p.name] = slices.Clone(req.PublicKey)
	sub, ok := f.subnets[p.name]
	if !ok {
		sub = len(f.subnets) + 1
		f.subnets[p.name] = sub
	}
	f.helloed[p.conn] = true
	prefix := mesh.NodePrefix(f.podNetwork, sub)
	return &wire.HelloResponse{
		Name:       p.name,
		PodNetwork: f.podNetwork.String(),
		Prefix:     prefix.String(),
		Address:    mesh.HostAddress(prefix).String(),
		AppsZone:   f.appsZone,
		TenantZone: f.tenantZone,
		Instance:   "fake",
	}, nil
}

func (s *fakeService) Sync(_ *wire.SyncRequest, stream wire.SyncServer) error {
	f := s.f
	ctx := stream.Context()
	p := fakePeerFrom(ctx)
	st := &fakeStream{name: p.name, q: make(chan fakeMsg, 1024)}
	f.mu.Lock()
	if !f.helloed[p.conn] {
		f.mu.Unlock()
		return status.Error(codes.FailedPrecondition, "call Hello first")
	}
	f.pushLocked(st, f.wholeLocked(p.name))
	f.streams[st] = struct{}{}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.streams, st)
		f.mu.Unlock()
	}()
	// A heartbeat every 10 s, as ferund sends.
	beat := time.NewTicker(fakeHeartbeat)
	defer beat.Stop()
	for {
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-beat.C:
			f.mu.Lock()
			st.seq++
			st.push(&wire.Table{Seq: st.seq, SentAt: time.Now()})
			f.mu.Unlock()
		case m := <-st.q:
			if m.done {
				return m.err
			}
			if err := stream.Send(m.t); err != nil {
				return err
			}
		}
	}
}

func (s *fakeService) Wake(ctx context.Context, req *wire.WakeRequest) (*wire.WakeResponse, error) {
	f := s.f
	f.mu.Lock()
	f.wakes = append(f.wakes, *req)
	d := f.wakeDelay
	f.mu.Unlock()
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	return &wire.WakeResponse{Waking: 1, RetryAfterSeconds: 5}, nil
}

func (s *fakeService) Hold(ctx context.Context, req *wire.HoldRequest) (*wire.HoldResponse, error) {
	f := s.f
	p := fakePeerFrom(ctx)
	if len(req.Hold) > wire.MaxHoldIDs || len(req.Release) > wire.MaxHoldIDs {
		return nil, status.Errorf(codes.InvalidArgument, "more than %d instances", wire.MaxHoldIDs)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holds = append(f.holds, FakeHold{Name: p.name, Request: wire.HoldRequest{
		Hold: slices.Clone(req.Hold), Release: slices.Clone(req.Release), Leaving: req.Leaving,
	}})
	resp := &wire.HoldResponse{Held: []string{}, Refused: []wire.HoldRefusal{}, LeaseSeconds: 30}
	for _, id := range req.Hold {
		if reason, ok := f.holdRefuse[id]; ok {
			resp.Refused = append(resp.Refused, wire.HoldRefusal{Instance: id, Reason: reason})
			continue
		}
		resp.Held = append(resp.Held, id)
	}
	return resp, nil
}

func cloneHello(r wire.HelloRequest) wire.HelloRequest {
	r.PublicKey = slices.Clone(r.PublicKey)
	return r
}

// certSerial is the serial of an issued certificate; "" when it has
// none.
func certSerial(c wire.Certificate) string {
	if len(c.Chain) == 0 {
		return ""
	}
	leaf, err := x509.ParseCertificate(c.Chain[0])
	if err != nil {
		return ""
	}
	return pki.Serial(leaf)
}

// fakeHeartbeat is how often the fake sends a Sync stream an empty
// message.
const fakeHeartbeat = 10 * time.Second

// fakeStream is an open Sync stream.
type fakeStream struct {
	name string
	q    chan fakeMsg
	seq  uint64
	done bool // an end was queued
}

type fakeMsg struct {
	t    *wire.Table
	done bool
	err  error
}

// push queues t; a stream whose queue is full ends with UNAVAILABLE.
// The caller holds the fake's lock.
func (st *fakeStream) push(t *wire.Table) {
	if st.done {
		return
	}
	select {
	case st.q <- fakeMsg{t: t}:
	default:
		st.end(status.Error(codes.Unavailable, "the stream fell behind"))
	}
}

// end queues the end of the stream with err. The caller holds the fake's
// lock.
func (st *fakeStream) end(err error) {
	if st.done {
		return
	}
	st.done = true
	select {
	case st.q <- fakeMsg{done: true, err: err}:
	default:
		// The queue is full: make room for the end, which matters more
		// than the oldest message.
		select {
		case <-st.q:
		default:
		}
		st.q <- fakeMsg{done: true, err: err}
	}
}

// fakeConn is a handshaken connection of the fake.
type fakeConn struct {
	*tls.Conn
	f    *FakeControlPlane
	once sync.Once
}

func (c *fakeConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.f.mu.Lock()
		delete(c.f.conns, c)
		c.f.mu.Unlock()
	})
	return err
}

// fakeCreds are the transport credentials of the fake's servers: the
// connection arrives handshaken.
type fakeCreds struct{}

func (fakeCreds) ServerHandshake(c net.Conn) (net.Conn, credentials.AuthInfo, error) {
	fc, ok := c.(*fakeConn)
	if !ok {
		return nil, nil, errors.New("fake control plane: the connection was not handshaken")
	}
	return fc, credentials.TLSInfo{
		State:          fc.ConnectionState(),
		CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.PrivacyAndIntegrity},
	}, nil
}

func (fakeCreds) ClientHandshake(context.Context, string, net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("fake control plane: server credentials")
}

func (fakeCreds) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "tls"}
}
func (c fakeCreds) Clone() credentials.TransportCredentials { return c }
func (fakeCreds) OverrideServerName(string) error           { return nil }

// fakeListener hands handshaken connections to a grpc.Server.
type fakeListener struct {
	addr   net.Addr
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newFakeListener(addr net.Addr) *fakeListener {
	return &fakeListener{addr: addr, ch: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *fakeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *fakeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeListener) Addr() net.Addr { return l.addr }

func (l *fakeListener) hand(c net.Conn) {
	select {
	case l.ch <- c:
	case <-l.closed:
		c.Close()
	}
}
