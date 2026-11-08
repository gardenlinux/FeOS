// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package nodelink

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// syncFirst opens a Sync stream and reads its first table.
func syncFirst(t *testing.T, is *ingressSide) wire.SyncClient {
	t.Helper()
	s, err := is.api.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tbl, err := s.Recv(); err != nil || tbl.Seq != 1 || !tbl.Whole {
		t.Fatalf("first table %+v %v", tbl, err)
	}
	return s
}

// recvEnds receives on s in a goroutine; the channel gets the error that
// ended the stream.
func recvEnds(s wire.SyncClient) <-chan error {
	ch := make(chan error, 1)
	go func() {
		for {
			if _, err := s.Recv(); err != nil {
				ch <- err
				return
			}
		}
	}()
	return ch
}

func TestIngressJoinServer(t *testing.T) {
	e := newEnv(t)
	is := e.dialIngress(nil)
	resp, err := is.api.Join(context.Background(), &wire.JoinRequest{Token: "t", Name: "in-0"})
	if err != nil || resp.Name != "in-0" {
		t.Fatalf("Join: %+v %v", resp, err)
	}
	// A join connection counts among the 64 shared with the nodes' joins.
	if len(e.srv.joinSlots) != 1 {
		t.Errorf("join slots %d", len(e.srv.joinSlots))
	}
	for name, call := range map[string]func() error{
		"Hello": func() error { _, err := is.hello(); return err },
		"RenewCertificate": func() error {
			_, err := is.api.RenewCertificate(context.Background(), &wire.RenewRequest{})
			return err
		},
		"Hold": func() error {
			_, err := is.api.Hold(context.Background(), &wire.HoldRequest{})
			return err
		},
		"Sync": func() error {
			s, err := is.api.Sync(context.Background())
			if err != nil {
				return err
			}
			_, err = s.Recv()
			return err
		},
		"FleetService/Join": func() error {
			_, err := fleetv1.NewFleetServiceClient(is.cc).Join(context.Background(), &fleetv1.JoinRequest{Name: "n1"})
			return err
		},
	} {
		if err := call(); status.Code(err) != codes.Unimplemented {
			t.Errorf("%s on the ingress join server: %v", name, err)
		}
	}
	// The join server's limit of 64 KiB, in JSON (base64 grows a CSR by
	// a third).
	if _, err := is.api.Join(context.Background(), &wire.JoinRequest{Name: "in-1", CSR: make([]byte, 40<<10)}); err != nil {
		t.Errorf("a Join of 40 KiB: %v", err)
	}
	if _, err := is.api.Join(context.Background(), &wire.JoinRequest{Name: "in-2", CSR: make([]byte, 64<<10)}); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("a Join over 64 KiB: %v", err)
	}
	if e.ichk.total() != 0 || e.chk.total() != 0 {
		t.Error("a join connection was checked")
	}
	if len(e.srv.IngressNames()) != 0 {
		t.Errorf("registered %v", e.srv.IngressNames())
	}
	if is.dials() != 1 {
		t.Errorf("dials %d", is.dials())
	}
}

func TestIngressConnection(t *testing.T) {
	e := newEnv(t)
	in := e.ingress("in-0", time.Hour)
	is := e.dialIngress(in)
	resp, err := is.hello()
	if err != nil {
		t.Fatal(err)
	}
	// The handler got the ingress from the certificate.
	if resp.Name != "in-0" || resp.Instance != in.serial {
		t.Errorf("Hello %+v", resp)
	}
	peers := e.isvc.seen()
	if len(peers) != 1 {
		t.Fatalf("peers %v", peers)
	}
	p := peers[0]
	if p.Name != "in-0" || p.Serial != in.serial || !p.NotAfter.Equal(in.leaf.NotAfter) ||
		p.Addr != is.conn(0).LocalAddr().String() || p.Conn == 0 || p.Done == nil {
		t.Errorf("peer %+v", p)
	}
	select {
	case <-p.Done:
		t.Error("Done closed on an open connection")
	default:
	}
	syncFirst(t, is)
	// Join belongs to the join server.
	if _, err := is.api.Join(context.Background(), &wire.JoinRequest{Name: "x"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("Join on the ingress server: %v", err)
	}
	if names := e.srv.IngressNames(); !slices.Equal(names, []string{"in-0"}) {
		t.Errorf("IngressNames %v", names)
	}
	if n := e.srv.IngressConnections("in-0"); n != 1 {
		t.Errorf("connections %d", n)
	}
	if e.ichk.count(in.serial, true) != 1 || e.chk.total() != 0 {
		t.Errorf("checks: ingress %d, node %d", e.ichk.count(in.serial, true), e.chk.total())
	}
	// An ingress is no node.
	if e.srv.Connected("in-0") || len(e.presented()) != 0 {
		t.Error("an ingress connection counts as a node's")
	}
	if l, u := e.linkCount("in-0"); l != 0 || u != 0 {
		t.Errorf("links %d, uplinks %d", l, u)
	}
	// A second connection is another Conn; its close closes its Done.
	other := e.dialIngress(in)
	if _, err := other.hello(); err != nil {
		t.Fatal(err)
	}
	q := e.isvc.seen()[1]
	if q.Conn == p.Conn {
		t.Errorf("two connections share Conn %d", q.Conn)
	}
	other.cc.Close()
	select {
	case <-q.Done:
	case <-time.After(3 * time.Second):
		t.Error("Done not closed when the connection closed")
	}
	eventually(t, 3*time.Second, "unregistered", func() bool { return e.srv.IngressConnections("in-0") == 1 })
	if is.dials() != 1 {
		t.Errorf("dials %d", is.dials())
	}
}

func TestIngressFromContext(t *testing.T) {
	if _, ok := IngressFromContext(context.Background()); ok {
		t.Error("an ingress in an empty context")
	}
	p := IngressPeer{Name: "in-0", Serial: "ab", Addr: "1.2.3.4:5", NotAfter: time.Unix(1, 0), Conn: 7}
	if got, ok := IngressFromContext(ContextWithIngress(context.Background(), p)); !ok || got != p {
		t.Errorf("got %v %v", got, ok)
	}
	if _, ok := NodeFromContext(ContextWithIngress(context.Background(), p)); ok {
		t.Error("an ingress is a node peer")
	}
}

func TestIngressWrongPairings(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", time.Hour)
	in := e.ingress("in-0", time.Hour)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	csr, _ := pki.CreateCSR(key, "pod")
	pc, leaf, err := e.iss.IssuePod(csr, pki.SandboxID(testTD, "acme", podid.Mint()), pki.PodSubject(testTD, "pod", ""), nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pod := &tnode{name: "pod", cert: tls.Certificate{Certificate: pc.Chain, PrivateKey: key, Leaf: leaf}, leaf: leaf}
	for _, c := range []struct {
		name string
		id   *tnode
		alpn string
	}{
		{"a node certificate on ferun-ingress/1", n, ALPNIngress},
		{"a pod certificate on ferun-ingress/1", pod, ALPNIngress},
		{"an ingress certificate on h2", in, ALPNUplink},
		{"an ingress certificate on feos-node/1", in, ALPNNode},
	} {
		conn, err := tls.Dial("tcp", e.addr(), e.tlsConfig(c.id, c.alpn))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if p := conn.ConnectionState().NegotiatedProtocol; p != c.alpn {
			t.Errorf("%s: negotiated %q", c.name, p)
		}
		if !readClosed(conn, 3*time.Second) {
			t.Errorf("%s was not closed", c.name)
		}
		conn.Close()
	}
	if e.ichk.total() != 0 || e.chk.total() != 0 {
		t.Errorf("checked: ingress %d, node %d", e.ichk.total(), e.chk.total())
	}
	if len(e.srv.IngressNames()) != 0 || e.srv.Connected("n1") {
		t.Error("registered")
	}
	// The node's own pairings still work.
	e.dialNode(n, "a")
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
}

func TestIngressRefusals(t *testing.T) {
	e := newEnv(t)
	t.Run("definitive", func(t *testing.T) {
		in := e.ingress("in-0", time.Hour)
		e.ichk.set(in.serial, time.Time{}, IngressRefusal("ingress %s was removed; join it again", "in-0"))
		is := e.dialIngress(in)
		for name, call := range map[string]func() error{
			"Hello": func() error { _, err := is.hello(); return err },
			"RenewCertificate": func() error {
				_, err := is.api.RenewCertificate(context.Background(), &wire.RenewRequest{})
				return err
			},
			"Wake": func() error {
				_, err := is.api.Wake(context.Background(), &wire.WakeRequest{})
				return err
			},
			"Hold": func() error {
				_, err := is.api.Hold(context.Background(), &wire.HoldRequest{})
				return err
			},
			"Join": func() error {
				_, err := is.api.Join(context.Background(), &wire.JoinRequest{})
				return err
			},
			"Sync": func() error {
				s, err := is.api.Sync(context.Background())
				if err != nil {
					return err
				}
				_, err = s.Recv()
				return err
			},
		} {
			if err := call(); status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "ingress in-0 was removed; join it again" {
				t.Errorf("%s: %v", name, err)
			}
		}
		if is.dials() != 1 {
			t.Errorf("dials %d", is.dials())
		}
		// A refusal connection takes one of the join slots, and is not
		// registered.
		if len(e.srv.joinSlots) != 1 {
			t.Errorf("join slots %d", len(e.srv.joinSlots))
		}
		if len(e.srv.IngressNames()) != 0 || len(e.isvc.seen()) != 0 {
			t.Error("a refused connection was registered or served")
		}
		is.cc.Close()
		eventually(t, 3*time.Second, "slot free", func() bool { return len(e.srv.joinSlots) == 0 })
	})
	t.Run("wrapped the usual way", func(t *testing.T) {
		in := e.ingress("in-1", time.Hour)
		e.ichk.set(in.serial, time.Time{}, fmt.Errorf("%w: certificate %s of ingress in-1 was revoked (removed); join it again", ErrIngressRefused, in.serial))
		_, err := e.dialIngress(in).hello()
		want := fmt.Sprintf("certificate %s of ingress in-1 was revoked (removed); join it again", in.serial)
		if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != want {
			t.Errorf("Hello: %v, want %q", err, want)
		}
	})
	t.Run("transient", func(t *testing.T) {
		in := e.ingress("in-2", time.Hour)
		e.ichk.set(in.serial, time.Time{}, errors.New("database down"))
		c, err := tls.Dial("tcp", e.addr(), e.tlsConfig(in, ALPNIngress))
		if err != nil {
			t.Fatal(err)
		}
		if !readClosed(c, 3*time.Second) {
			t.Error("a connection whose check failed transiently was not closed")
		}
		c.Close()
		if _, err := e.dialIngress(in).hello(); status.Code(err) != codes.Unavailable {
			t.Errorf("Hello: %v", err)
		}
		if len(e.srv.IngressNames()) != 0 {
			t.Errorf("registered %v", e.srv.IngressNames())
		}
	})
	t.Run("no CheckIngress", func(t *testing.T) {
		e := newEnv(t, envOpt{cfg: func(c *Config) { c.CheckIngress = nil }})
		in := e.ingress("in-0", time.Hour)
		c, err := tls.Dial("tcp", e.addr(), e.tlsConfig(in, ALPNIngress))
		if err != nil {
			t.Fatal(err)
		}
		if !readClosed(c, 3*time.Second) {
			t.Error("an ingress connection was not closed without CheckIngress")
		}
		c.Close()
		if len(e.srv.IngressNames()) != 0 {
			t.Errorf("registered %v", e.srv.IngressNames())
		}
		eventually(t, 2*time.Second, "logged", func() bool {
			return e.logs.count("nodes' port: ingress connection not checked; closed") == 1
		})
	})
}

func TestIngressRefusalText(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{IngressRefusal("ingress %s was removed; join it again", "in-0"), "ingress in-0 was removed; join it again"},
		{fmt.Errorf("check: %w", IngressRefusal("ingress in-0 was removed; join it again")), "ingress in-0 was removed; join it again"},
		{fmt.Errorf("%w: certificate ab of ingress in-0 was superseded; reconnect with the newest certificate", ErrIngressRefused),
			"certificate ab of ingress in-0 was superseded; reconnect with the newest certificate"},
		{fmt.Errorf("certificate ab is not ingress in-0's: %w", ErrIngressRefused), "certificate ab is not ingress in-0's"},
		{ErrIngressRefused, "ingress refused"},
	} {
		if got := ingressRefusalText(c.err); got != c.want {
			t.Errorf("ingressRefusalText(%q) = %q, want %q", c.err, got, c.want)
		}
	}
	if !errors.Is(IngressRefusal("x"), ErrIngressRefused) {
		t.Error("IngressRefusal does not wrap ErrIngressRefused")
	}
}

func TestIngressLimit(t *testing.T) {
	e := newEnv(t)
	in := e.ingress("in-0", time.Hour)
	var sides []*ingressSide
	for i := range 4 {
		is := e.dialIngress(in)
		if _, err := is.hello(); err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		sides = append(sides, is)
	}
	// The fifth, even with another certificate of the name, is closed.
	renewed := e.ingress("in-0", time.Hour)
	fifth := e.dialIngress(renewed)
	if _, err := fifth.hello(); err == nil {
		t.Fatal("a fifth connection of an ingress was served")
	}
	if e.srv.droppedIngress.Load() == 0 {
		t.Error("not counted")
	}
	if n := e.srv.IngressConnections("in-0"); n != 4 {
		t.Errorf("connections %d", n)
	}
	// The limit is per name.
	if _, err := e.dialIngress(e.ingress("in-1", time.Hour)).hello(); err != nil {
		t.Errorf("another ingress: %v", err)
	}
	// A closed connection frees its place.
	sides[0].cc.Close()
	eventually(t, 3*time.Second, "unregistered", func() bool { return e.srv.IngressConnections("in-0") == 3 })
	if _, err := e.dialIngress(renewed).hello(); err != nil {
		t.Errorf("a connection once one closed: %v", err)
	}
}

func TestRecheckIngress(t *testing.T) {
	e := newEnv(t, envOpt{tune: func(s *Server) {
		s.recheckMinBackoff = 50 * time.Millisecond
		s.recheckMaxBackoff = 200 * time.Millisecond
	}})
	// Two certificates of one ingress, each with a connection, and another
	// ingress.
	a := e.ingress("in-0", time.Hour)
	b := e.ingress("in-0", time.Hour)
	o := e.ingress("in-1", time.Hour)
	ia, ib, io := e.dialIngress(a), e.dialIngress(b), e.dialIngress(o)
	for _, is := range []*ingressSide{ia, ib, io} {
		if _, err := is.hello(); err != nil {
			t.Fatal(err)
		}
	}
	e.ichk.set(a.serial, time.Time{}, errors.New("database down"))
	e.ichk.set(b.serial, time.Time{}, IngressRefusal("certificate %s of ingress in-0 was revoked (removed); join it again", b.serial))
	e.ichk.set(o.serial, time.Time{}, IngressRefusal("ingress in-1 was removed; join it again"))
	e.srv.RecheckIngress(context.Background(), "in-0")
	// Only the refused connection is closed; the transient answer keeps
	// its connection, and another ingress is not re-checked.
	ib.waitClosed(t, 0, 3*time.Second)
	if ia.isClosed(0) || io.isClosed(0) {
		t.Fatal("a connection without a refusal was closed")
	}
	if n := e.srv.IngressConnections("in-0"); n != 1 {
		t.Errorf("connections %d", n)
	}
	if e.ichk.count(o.serial, true) != 1 {
		t.Error("another ingress was re-checked")
	}
	// The transient one is re-checked with backoff until it gets an answer.
	eventually(t, 3*time.Second, "retried", func() bool { return e.ichk.count(a.serial, true) >= 4 })
	e.ichk.set(a.serial, time.Time{}, nil)
	eventually(t, 3*time.Second, "retry cleared", func() bool {
		e.srv.mu.Lock()
		defer e.srv.mu.Unlock()
		st := e.srv.ingresses["in-0"]
		return st != nil && st.retry == nil
	})
	n := e.ichk.count(a.serial, true)
	never(t, 500*time.Millisecond, "re-checked after an answer", func() bool { return e.ichk.count(a.serial, true) != n })
	if _, err := ia.hello(); err != nil || ia.dials() != 1 {
		t.Errorf("Hello on the kept connection: %v, dials %d", err, ia.dials())
	}
	if io.isClosed(0) {
		t.Error("another ingress's connection was closed")
	}
	checks := e.ichk.total()
	e.srv.RecheckIngress(context.Background(), "unknown") // nothing to re-check
	if e.ichk.total() != checks {
		t.Error("checked without a connection")
	}
}

// A re-check asked for while a connection's accept check runs is repeated
// for it once it is registered, and the name counts as connected meanwhile.
func TestRecheckIngressDuringAccept(t *testing.T) {
	e := newEnv(t)
	in := e.ingress("in-0", time.Hour)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	e.ichk.setHook(func(checkCall) {
		once.Do(func() {
			close(entered)
			<-release
		})
	})
	is := e.dialIngress(in)
	done := make(chan struct{})
	go func() {
		defer close(done)
		is.hello()
	}()
	<-entered
	if names := e.srv.IngressNames(); !slices.Equal(names, []string{"in-0"}) {
		t.Errorf("IngressNames during the accept %v", names)
	}
	e.ichk.set(in.serial, time.Time{}, IngressRefusal("ingress in-0 was removed; join it again"))
	e.srv.RecheckIngress(context.Background(), "in-0")
	close(release)
	is.waitClosed(t, 0, 5*time.Second)
	eventually(t, 3*time.Second, "unregistered", func() bool { return len(e.srv.IngressNames()) == 0 })
	if e.ichk.count(in.serial, true) < 2 {
		t.Errorf("checks %d, want the accept's and a re-check", e.ichk.count(in.serial, true))
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Hello did not return")
	}
}

func TestCloseIngress(t *testing.T) {
	e := newEnv(t)
	a, b := e.ingress("in-0", time.Hour), e.ingress("in-1", time.Hour)
	sides := []*ingressSide{e.dialIngress(a), e.dialIngress(a), e.dialIngress(b)}
	for _, is := range sides {
		if _, err := is.hello(); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.srv.CloseIngress("in-0", "ingress removed"); n != 2 {
		t.Errorf("closed %d, want 2", n)
	}
	sides[0].waitClosed(t, 0, 3*time.Second)
	sides[1].waitClosed(t, 0, 3*time.Second)
	if sides[2].isClosed(0) {
		t.Error("another ingress's connection was closed")
	}
	if names := e.srv.IngressNames(); !slices.Equal(names, []string{"in-1"}) {
		t.Errorf("IngressNames %v", names)
	}
	if n := e.srv.CloseIngress("in-0", "again"); n != 0 {
		t.Errorf("closed %d again", n)
	}
	eventually(t, time.Second, "logged", func() bool { return e.logs.count("ingress connection closed") >= 2 })
}

func TestIngressRevocationCloses(t *testing.T) {
	e := newEnv(t)
	t.Run("at accept", func(t *testing.T) {
		in := e.ingress("in-0", time.Hour)
		revokeAt := time.Now().Add(600 * time.Millisecond)
		e.ichk.set(in.serial, revokeAt, nil)
		is := e.dialIngress(in)
		if _, err := is.hello(); err != nil {
			t.Fatal(err)
		}
		ends := recvEnds(syncFirst(t, is))
		is.waitClosed(t, 0, 3*time.Second)
		if time.Now().Before(revokeAt) {
			t.Error("closed before the revocation time")
		}
		select {
		case <-ends:
		case <-time.After(3 * time.Second):
			t.Error("the stream outlived the revocation")
		}
		eventually(t, 2*time.Second, "unregistered", func() bool { return e.srv.IngressConnections("in-0") == 0 })
	})
	t.Run("on re-check", func(t *testing.T) {
		in := e.ingress("in-1", time.Hour)
		is := e.dialIngress(in)
		if _, err := is.hello(); err != nil {
			t.Fatal(err)
		}
		revokeAt := time.Now().Add(700 * time.Millisecond)
		e.ichk.set(in.serial, revokeAt, nil)
		e.srv.RecheckIngress(context.Background(), "in-1")
		// An older answer without the revocation, landing late, takes
		// nothing back; neither does a later revocation time.
		e.ichk.set(in.serial, time.Time{}, nil)
		e.srv.RecheckIngress(context.Background(), "in-1")
		e.ichk.set(in.serial, time.Now().Add(time.Hour), nil)
		e.srv.RecheckIngress(context.Background(), "in-1")
		if is.isClosed(0) {
			t.Fatal("closed before the revocation time")
		}
		is.waitClosed(t, 0, 3*time.Second)
		if time.Now().Before(revokeAt) {
			t.Error("closed before the revocation time")
		}
		eventually(t, 2*time.Second, "unregistered", func() bool { return e.srv.IngressConnections("in-1") == 0 })
	})
}

func TestIngressNotAfterCloses(t *testing.T) {
	e := newEnv(t)
	in := e.ingress("in-0", 2*time.Second)
	is := e.dialIngress(in)
	if _, err := is.hello(); err != nil {
		t.Fatal(err)
	}
	is.waitClosed(t, 0, 5*time.Second)
	if time.Now().Before(in.leaf.NotAfter) {
		t.Error("closed before NotAfter")
	}
	eventually(t, 2*time.Second, "unregistered", func() bool { return len(e.srv.IngressNames()) == 0 })
	eventually(t, time.Second, "logged", func() bool { return e.logs.count("ingress connection closed") == 1 })
}

func TestIngressServerOptions(t *testing.T) {
	e := newEnv(t)
	is := e.dialIngress(e.ingress("in-0", time.Hour))
	// MaxRecvMsgSize: 4 MiB.
	resp, err := is.api.Hold(context.Background(), &wire.HoldRequest{Hold: []string{strings.Repeat("x", 3<<20)}})
	if err != nil || resp.LeaseSeconds != 1 {
		t.Fatalf("a Hold of 3 MiB: %+v %v", resp, err)
	}
	if _, err := is.api.Hold(context.Background(), &wire.HoldRequest{Hold: []string{strings.Repeat("x", 4<<20)}}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("a Hold over 4 MiB: %v", err)
	}
	// MaxConcurrentStreams: 16. A seventeenth stream waits for a free one.
	for range 16 {
		syncFirst(t, is)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	s, err := is.api.Sync(ctx)
	if err == nil {
		_, err = s.Recv()
	}
	cancel()
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("a seventeenth stream: %v", err)
	}
	if is.dials() != 1 {
		t.Errorf("dials %d", is.dials())
	}
}

func TestStopIngress(t *testing.T) {
	e := newEnv(t)
	in := e.ingress("in-0", time.Hour)
	is := e.dialIngress(in)
	ends := recvEnds(syncFirst(t, is))
	e.srv.StopIngress()
	e.srv.StopIngress() // twice is harmless
	checks := e.ichk.total()
	// New ferun-ingress/1 connections are closed at once, with a
	// certificate or without, before any check.
	for _, id := range []*tnode{in, nil} {
		c, err := tls.Dial("tcp", e.addr(), e.tlsConfig(id, ALPNIngress))
		if err != nil {
			continue // closed during the handshake
		}
		start := time.Now()
		if !readClosed(c, 3*time.Second) {
			t.Error("a new ingress connection was not closed")
		}
		if time.Since(start) > time.Second {
			t.Error("not closed at once")
		}
		c.Close()
	}
	if e.ichk.total() != checks || len(e.srv.joinSlots) != 0 {
		t.Errorf("checks %d → %d, join slots %d", checks, e.ichk.total(), len(e.srv.joinSlots))
	}
	// The established connection stays: its stream goes on, and it takes
	// new calls.
	if _, err := is.hello(); err != nil {
		t.Fatalf("Hello on an established connection: %v", err)
	}
	select {
	case err := <-ends:
		t.Fatalf("the stream ended: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if is.dials() != 1 || e.srv.IngressConnections("in-0") != 1 {
		t.Errorf("dials %d, connections %d", is.dials(), e.srv.IngressConnections("in-0"))
	}
	// Nodes are not affected.
	e.dialNode(e.node("n1", time.Hour), "a")
	eventually(t, 3*time.Second, "node connected", func() bool { return e.srv.Connected("n1") })
	if _, err := e.dialUplink(nil).fleet.Join(context.Background(), &fleetv1.JoinRequest{Name: "n2"}); err != nil {
		t.Errorf("a node's Join: %v", err)
	}

	// Shutdown closes the established connection.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e.srv.Shutdown(ctx)
	select {
	case <-ends:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream survived the shutdown")
	}
	is.waitClosed(t, 0, 3*time.Second)
	eventually(t, 2*time.Second, "unregistered", func() bool { return len(e.srv.IngressNames()) == 0 })
}

func TestShutdownClosesIngress(t *testing.T) {
	e := newEnv(t)
	is := e.dialIngress(e.ingress("in-0", time.Hour))
	ends := recvEnds(syncFirst(t, is))
	join := e.dialIngress(nil)
	if _, err := join.api.Join(context.Background(), &wire.JoinRequest{Name: "in-1"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	e.srv.Shutdown(ctx)
	if time.Since(start) > 3*time.Second {
		t.Error("Shutdown overran its deadline")
	}
	select {
	case <-ends:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream survived the shutdown")
	}
	is.waitClosed(t, 0, 3*time.Second)
	join.waitClosed(t, 0, 3*time.Second)
	if len(e.srv.IngressNames()) != 0 {
		t.Errorf("registered after the shutdown: %v", e.srv.IngressNames())
	}
	// Re-checks after the shutdown do nothing.
	e.srv.RecheckIngress(context.Background(), "in-0")
}
