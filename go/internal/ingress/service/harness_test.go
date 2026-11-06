// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/ingress/table"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/store"
)

var bg = context.Background()

const (
	testTD     = "feos.test"
	testDomain = "example.test"
	serverName = "ferun." + testDomain
	testZone   = "apps.example.test"
)

// testULA is the pods' mesh of a meshed harness.
var testULA = netip.MustParsePrefix("fd12:3456:789a::/64")

// issuerDir holds one platform CA for every test of the package, made by
// TestMain.
var issuerDir string

func TestMain(m *testing.M) {
	base, err := os.MkdirTemp("", "ingress-service-ca-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	issuerDir = filepath.Join(base, "ca")
	err = pki.InitCA(pki.InitCAOptions{
		RootDir: filepath.Join(base, "root"), Dir: issuerDir, TrustDomain: testTD, DNSDomains: []string{testDomain},
	})
	code := 1
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	} else {
		code = m.Run()
	}
	os.RemoveAll(base)
	os.Exit(code)
}

// harness is an instance's ingress side: a database with its listener,
// a real nodes' port on 127.0.0.1:0 whose CheckIngress is the service's,
// and the service registered on it and running, with its table's
// builder (b).
type harness struct {
	t     *testing.T
	d     *db.DB
	st    *store.Store
	iss   *pki.Issuer
	links *nodelink.Server
	s     *Service
	b     *table.Builder
}

// harnessOpts shape a harness: noMesh leaves the cluster routed; grace is
// the superseded grace (10 minutes when zero); engine is the service's
// Engine (none when nil); heartbeat is the Sync streams' (10 s when
// zero); dbTimeout bounds the streams' row reads and writes and each
// statement of a Hold (5 s when zero); partBytes is the most a Sync
// message takes before the table goes in parts (wire.PartBytes when
// zero); the table's builder reads with tableStore (the harness's when
// nil), collects changes for 10 ms, and tableMod changes it before it
// runs. renewAnytime lets RenewCertificate take a certificate long
// before its renew time.
type harnessOpts struct {
	noMesh       bool
	grace        time.Duration
	engine       Engine
	heartbeat    time.Duration
	dbTimeout    time.Duration
	partBytes    int
	tableStore   *store.Store
	tableMod     func(b *table.Builder)
	renewAnytime bool
}

func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	t.Parallel()
	d := dbtest.New(t)
	dbtest.StartListener(t, d)
	log := dbtest.Logger(t)
	h := &harness{t: t, d: d, st: store.New(d)}
	var ula netip.Prefix
	if !o.noMesh {
		ula = testULA
		if err := h.st.InsertPodNetwork(bg, ula); err != nil {
			t.Fatal(err)
		}
	}
	iss, err := pki.LoadIssuer(issuerDir, testTD)
	if err != nil {
		t.Fatal(err)
	}
	h.iss = iss
	src, err := pki.NewServerCertSource(iss, []string{serverName}, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	var svc *Service
	links, err := nodelink.New(nodelink.Config{
		Addr: "127.0.0.1:0", TrustDomain: testTD, InstanceID: "i1", Bundle: iss.Bundle(), ServerCert: src.GetCertificate,
		CheckNode: func(context.Context, string, string, string, bool) (time.Time, error) {
			return time.Time{}, errors.New("no nodes in this test")
		},
		CheckIngress: func(ctx context.Context, name, serial, peer string) (time.Time, error) {
			return svc.CheckIngress(ctx, name, serial, peer)
		},
		Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	b := table.NewBuilder(d, o.tableStore, model.Zones{Apps: testZone}, log)
	b.Debounce = 10 * time.Millisecond
	if o.tableMod != nil {
		o.tableMod(b)
	}
	svc = New(Config{InstanceID: "i1", TrustDomain: testTD, AppsZone: testZone, SupersedeGrace: o.grace,
		PodNetwork: ula, Log: log}, Deps{DB: d, Issuer: iss, Links: links, Engine: o.engine, Builder: b})
	if o.heartbeat > 0 {
		svc.heartbeat = o.heartbeat
	}
	if o.dbTimeout > 0 {
		svc.rowTimeout, svc.holdTimeout = o.dbTimeout, o.dbTimeout
	}
	if o.partBytes > 0 {
		svc.partBytes = o.partBytes
	}
	if o.renewAnytime {
		svc.renewEarly = 1000 * time.Hour
	}
	svc.Register()
	h.links, h.s, h.b = links, svc, b
	ctx, cancel := context.WithCancel(bg)
	served, ran := make(chan error, 1), make(chan error, 1)
	go func() { served <- links.Serve(ctx) }()
	go func() { ran <- svc.Run(ctx) }()
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(bg, 10*time.Second)
		defer scancel()
		links.Shutdown(sctx)
		cancel()
		for _, ch := range []chan error{served, ran} {
			select {
			case <-ch:
			case <-time.After(10 * time.Second):
				t.Error("the port or the service did not stop")
			}
		}
	})
	eventually(t, 5*time.Second, "the service runs", svc.running)
	return h
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.d.Pool.Exec(bg, sql, args...); err != nil {
		h.t.Fatalf("%s: %v", sql, err)
	}
}

func (h *harness) scalar(dst any, sql string, args ...any) {
	h.t.Helper()
	if err := h.d.Pool.QueryRow(bg, sql, args...).Scan(dst); err != nil {
		h.t.Fatalf("%s: %v", sql, err)
	}
}

func (h *harness) ingress(name string) store.Ingress {
	h.t.Helper()
	g, err := h.st.GetIngress(bg, name)
	if err != nil {
		h.t.Fatalf("ingress %s: %v", name, err)
	}
	return g
}

func (h *harness) cert(serial string) store.IngressCert {
	h.t.Helper()
	c, err := h.st.GetIngressCert(bg, serial)
	if err != nil {
		h.t.Fatalf("certificate %s: %v", serial, err)
	}
	return c
}

func (h *harness) peersVersion() int64 {
	h.t.Helper()
	var v int64
	h.scalar(&v, "SELECT peers_version FROM pod_network")
	return v
}

// token issues a join token of kind ingress, pinned to name unless "",
// and returns what the ingress sends.
func (h *harness) token(name string) string {
	h.t.Helper()
	_, token, _, err := h.s.IssueJoinToken(bg, "test", name, time.Hour)
	if err != nil {
		h.t.Fatal(err)
	}
	sent, pin, err := pki.SplitIngressJoinToken(token)
	if err != nil {
		h.t.Fatal(err)
	}
	if pin != h.iss.JoinPin() {
		h.t.Fatalf("token pin %s, issuer pin %s", pin, h.iss.JoinPin())
	}
	return sent
}

// newKey returns a new Ed25519 key.
func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// csrOf returns a signing request for key.
func csrOf(t *testing.T, key ed25519.PrivateKey) []byte {
	t.Helper()
	csr, err := pki.CreateCSR(key, "ignored.example.com")
	if err != nil {
		t.Fatal(err)
	}
	return csr
}

// wgKey is a WireGuard public key for a Hello: 32 bytes of b.
func wgKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// identity is an ingress's certificate and key.
type identity struct {
	name   string
	key    ed25519.PrivateKey
	cert   tls.Certificate
	leaf   *x509.Certificate
	serial string
}

// identityOf makes the identity of a Join's or a renewal's certificate.
func identityOf(t *testing.T, name string, c wire.Certificate, key ed25519.PrivateKey) *identity {
	t.Helper()
	if len(c.Chain) == 0 {
		t.Fatal("a certificate without a chain")
	}
	leaf, err := x509.ParseCertificate(c.Chain[0])
	if err != nil {
		t.Fatal(err)
	}
	return &identity{name: name, key: key, cert: tls.Certificate{Certificate: c.Chain, PrivateKey: key, Leaf: leaf}, leaf: leaf,
		serial: pki.Serial(leaf)}
}

// watchedConn records when it is closed: the ingress's view of a
// connection that ferund (or the ingress) ended.
type watchedConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (w *watchedConn) Close() error {
	err := w.Conn.Close()
	w.once.Do(func() { close(w.closed) })
	return err
}

// client is an ingress's channel to the port, with a certificate or
// without (nil identity); every dial is a new TLS connection with ALPN
// ferun-ingress/1, recorded in conns.
type client struct {
	cc    *grpc.ClientConn
	api   *wire.Client
	mu    sync.Mutex
	conns []*watchedConn
}

func (h *harness) dial(id *identity) *client {
	h.t.Helper()
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: serverName,
		RootCAs:    h.iss.Bundle(),
		NextProtos: []string{wire.ALPN},
		VerifyConnection: func(cs tls.ConnectionState) error {
			raw := make([][]byte, len(cs.PeerCertificates))
			for i, c := range cs.PeerCertificates {
				raw[i] = c.Raw
			}
			return pki.VerifyControlPlane(h.iss.Bundle(), serverName, testTD, raw)
		},
	}
	if id != nil {
		cfg.Certificates = []tls.Certificate{id.cert}
	}
	c := &client{}
	addr := h.links.Addr().String()
	cc, err := grpc.NewClient("passthrough:///"+serverName,
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			tc, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			w := &watchedConn{Conn: tc, closed: make(chan struct{})}
			c.mu.Lock()
			c.conns = append(c.conns, w)
			c.mu.Unlock()
			return w, nil
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 15 * time.Second, Timeout: 5 * time.Second, PermitWithoutStream: true}),
	)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { cc.Close() })
	c.cc, c.api = cc, wire.NewClient(cc)
	return c
}

// conn returns the client's i-th connection.
func (c *client) conn(t *testing.T, i int) *watchedConn {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if i >= len(c.conns) {
		t.Fatalf("connection %d of %d", i, len(c.conns))
	}
	return c.conns[i]
}

func (c *client) hello(key []byte, endpoint string) (*wire.HelloResponse, error) {
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	return c.api.Hello(ctx, &wire.HelloRequest{PublicKey: key, Endpoint: endpoint, Version: "test"})
}

func (c *client) renew(t *testing.T, key ed25519.PrivateKey) (*wire.RenewResponse, error) {
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	return c.api.RenewCertificate(ctx, &wire.RenewRequest{CSR: csrOf(t, key)})
}

// join joins with token under the proposed name with key, on a
// connection without a certificate.
func (h *harness) join(c *client, token, name string, key ed25519.PrivateKey) (*wire.JoinResponse, error) {
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	return c.api.Join(ctx, &wire.JoinRequest{Token: token, Name: name, CSR: csrOf(h.t, key)})
}

// mustJoin joins a new key with a new token pinned to pin ("" for none),
// proposing name, and returns the identity.
func (h *harness) mustJoin(pin, name string) (*identity, ed25519.PrivateKey) {
	h.t.Helper()
	key := newKey(h.t)
	resp, err := h.join(h.dial(nil), h.token(pin), name, key)
	if err != nil {
		h.t.Fatalf("join %s: %v", name, err)
	}
	return identityOf(h.t, resp.Name, resp.Certificate, key), key
}

// connect joins a new ingress name (an unpinned token), connects with its
// certificate and says Hello with key wg from endpoint.
func (h *harness) connect(name string, wg byte, endpoint string) (*identity, *client, *wire.HelloResponse) {
	h.t.Helper()
	id, _ := h.mustJoin("", name)
	c := h.dial(id)
	resp, err := c.hello(wgKey(wg), endpoint)
	if err != nil {
		h.t.Fatalf("hello %s: %v", name, err)
	}
	return id, c, resp
}

// wantCode fails unless err is a status with code whose message contains
// text.
func wantCode(t *testing.T, what string, err error, code codes.Code, text string) {
	t.Helper()
	st, _ := status.FromError(err)
	if st.Code() != code || !strings.Contains(st.Message(), text) {
		t.Errorf("%s: %v, want %s with %q", what, err, code, text)
	}
}

// eventually polls cond until it holds or within has passed.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", within, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// closedWithin waits until the ingress side sees w closed.
func closedWithin(t *testing.T, w *watchedConn, within time.Duration, what string) {
	t.Helper()
	select {
	case <-w.closed:
	case <-time.After(within):
		t.Fatalf("%s: connection not closed within %v", what, within)
	}
}
