// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux && (amd64 || arm64)

package wg

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"
)

// nsRunner is a goroutine locked to an OS thread in a network namespace,
// which runs the closures sent to it. Every netlink call, socket, listen
// and dial for that namespace runs through its runner: a socket belongs to
// the namespace of the thread that makes it.
type nsRunner struct {
	work chan func()
	done chan struct{}
	tid  int
}

// startRunner starts a runner whose thread enters its namespace with
// enter. The thread is never unlocked: it dies with the goroutine when the
// test ends, instead of going back to the scheduler inside the namespace.
func startRunner(t *testing.T, enter func() error) *nsRunner {
	t.Helper()
	r := &nsRunner{work: make(chan func()), done: make(chan struct{})}
	started := make(chan error, 1)
	go func() {
		defer close(r.done)
		runtime.LockOSThread()
		if err := enter(); err != nil {
			started <- err
			return
		}
		r.tid = syscall.Gettid()
		started <- nil
		for f := range r.work {
			f()
		}
	}()
	if err := <-started; err != nil {
		t.Fatalf("enter a network namespace: %v", err)
	}
	t.Cleanup(func() {
		close(r.work)
		<-r.done
	})
	return r
}

// do runs f on the runner's thread and returns its error.
func (r *nsRunner) do(f func() error) error {
	errc := make(chan error, 1)
	r.work <- func() { errc <- f() }
	return <-errc
}

func unshareNetns() error { return syscall.Unshare(syscall.CLONE_NEWNET) }

// setnsInto enters the network namespace that the open file fd refers to.
func setnsInto(fd int) func() error {
	return func() error {
		if _, _, errno := syscall.Syscall(sysSetns, uintptr(fd), syscall.CLONE_NEWNET, 0); errno != 0 {
			return fmt.Errorf("setns: %w", errno)
		}
		return nil
	}
}

func must(t *testing.T, step string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", step, err)
	}
}

// TestNetnsWireGuard sets up two WireGuard devices that talk over
// 127.0.0.1 in one namespace, NS1, while one of them, wgB, sits in a
// second, NS2: it is made in NS1 and moved, so its UDP socket stays in NS1.
// An HTTP request from NS1 to a listener on wgB's address in NS2 goes
// through the tunnel, also after wgA moved to another port and after its
// peer was set anew. Needs root and a kernel with WireGuard;
// hack/go/netns-test runs it.
func TestNetnsWireGuard(t *testing.T) {
	if os.Getenv("FERUN_TEST_NETNS") != "1" || os.Geteuid() != 0 {
		t.Skip("needs root and FERUN_TEST_NETNS=1; hack/go/netns-test runs it")
	}
	privA, err := GenerateKey()
	must(t, "key", err)
	privB, err := GenerateKey()
	must(t, "key", err)
	ula := netip.MustParsePrefix("fd00::/16")
	addrA, addrB := netip.MustParsePrefix("fd00:1::1/128"), netip.MustParsePrefix("fd00:2::1/128")
	peerB := Peer{PublicKey: privB.PublicKey(), Endpoint: netip.MustParseAddrPort("127.0.0.1:51822"),
		AllowedIPs: []netip.Prefix{netip.MustParsePrefix("fd00:2::/64")}, Keepalive: 25 * time.Second}
	peerA := Peer{PublicKey: privA.PublicKey(), Endpoint: netip.MustParseAddrPort("127.0.0.1:51821"),
		AllowedIPs: []netip.Prefix{netip.MustParsePrefix("fd00:1::/64")}, Keepalive: 25 * time.Second}

	// 1. NS1: lo up, wgA and wgB.
	ns1 := startRunner(t, unshareNetns)
	var idxA, idxB int
	must(t, "NS1", ns1.do(func() error {
		lo, err := LinkByName("lo")
		if err != nil {
			return err
		}
		if err := SetUp(lo.Index); err != nil {
			return err
		}
		if idxA, err = EnsureLink("wgA"); err != nil {
			return err
		}
		if idxB, err = EnsureLink("wgB"); err != nil {
			return err
		}
		if again, err := EnsureLink("wgA"); err != nil || again != idxA {
			return fmt.Errorf("EnsureLink of an existing device: %d, %v; want %d", again, err, idxA)
		}
		if _, err := EnsureLink("lo"); err == nil {
			return errors.New("EnsureLink took lo, a link of another kind")
		}
		return nil
	}))

	// 2. NS2.
	ns2 := startRunner(t, unshareNetns)

	// 3. wgB moves to NS2, by the file of NS2 that runner 2's thread names.
	var fd2 int
	must(t, "open NS2", ns2.do(func() (err error) {
		fd2, err = syscall.Open(fmt.Sprintf("/proc/%d/ns/net", ns2.tid), syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		return err
	}))
	t.Cleanup(func() { _ = syscall.Close(fd2) })
	must(t, "move wgB", ns1.do(func() error {
		if err := MoveLinkToNetns(idxB, fd2); err != nil {
			return err
		}
		if _, err := LinkByName("wgB"); !errors.Is(err, syscall.ENODEV) {
			return fmt.Errorf("wgB in NS1 after the move: %v", err)
		}
		return nil
	}))
	// A third thread joins NS2 through the same file and finds wgB there.
	joined := startRunner(t, setnsInto(fd2))
	must(t, "wgB in NS2", joined.do(func() error {
		l, err := LinkByName("wgB")
		if err != nil {
			return err
		}
		if l.Kind != "wireguard" {
			return fmt.Errorf("wgB in NS2: %+v", l)
		}
		idxB = l.Index
		if _, err := LinkByName("wgA"); !errors.Is(err, syscall.ENODEV) {
			return fmt.Errorf("wgA in NS2: %v", err)
		}
		return nil
	}))

	// 4. Each device set up in its own namespace.
	setup := func(name string, index int, priv Key, port int, addr netip.Prefix, peer Peer) func() error {
		return func() error {
			if err := SetMTU(index, 1380); err != nil {
				return err
			}
			if l, err := LinkByName(name); err != nil || l.MTU != 1380 {
				return fmt.Errorf("after SetMTU 1380: %+v, %v", l, err)
			}
			if err := SetMTU(index, 1420); err != nil {
				return err
			}
			if err := SetUp(index); err != nil {
				return err
			}
			if l, err := LinkByName(name); err != nil || !l.Up || l.MTU != 1420 {
				return fmt.Errorf("after SetUp: %+v, %v", l, err)
			}
			// Twice: a replace is idempotent.
			for range 2 {
				if err := ReplaceAddress(index, addr); err != nil {
					return err
				}
				if err := ReplaceRoute(Route{Dst: ula, Index: index, Src: addr.Addr(), MTU: 1420}); err != nil {
					return err
				}
			}
			if err := Configure(name, priv, port); err != nil {
				return err
			}
			if err := SetPeers(name, []Peer{peer}); err != nil {
				return err
			}
			d, err := Get(name)
			if err != nil {
				return err
			}
			if d.Name != name || d.Index != index || d.PublicKey != priv.PublicKey() || d.ListenPort != port ||
				len(d.Peers) != 1 || !samePeer(d.Peers[0].Peer, peer) || d.Peers[0].Endpoint != peer.Endpoint {
				return fmt.Errorf("device %s: %+v", name, d)
			}
			return nil
		}
	}
	must(t, "set up wgA", ns1.do(setup("wgA", idxA, privA, 51821, addrA, peerB)))
	must(t, "set up wgB", ns2.do(setup("wgB", idxB, privB, 51822, addrB, peerA)))

	// Addresses lists wgA's, and DeleteAddress takes a stray one away.
	must(t, "addresses", ns1.do(func() error {
		stray := netip.MustParsePrefix("fd00:1::99/128")
		if err := ReplaceAddress(idxA, stray); err != nil {
			return err
		}
		have, err := Addresses(idxA)
		if err != nil {
			return err
		}
		if !slices.Contains(have, addrA) || !slices.Contains(have, stray) {
			return fmt.Errorf("addresses with the stray one: %v", have)
		}
		if err := DeleteAddress(idxA, stray); err != nil {
			return err
		}
		if have, err = Addresses(idxA); err != nil {
			return err
		}
		if !slices.Contains(have, addrA) || slices.Contains(have, stray) ||
			slices.Contains(have, netip.MustParsePrefix("127.0.0.1/8")) {
			return fmt.Errorf("addresses after the delete: %v", have)
		}
		return nil
	}))

	// 5. The listener in NS2, on wgB's address; it serves from anywhere.
	var ln net.Listener
	must(t, "listen", ns2.do(func() (err error) {
		ln, err = net.Listen("tcp", "[fd00:2::1]:8080")
		return err
	}))
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "through the tunnel")
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	// 6. A request from NS1. A dial of a literal address makes its socket
	// on the calling goroutine, so in NS1.
	request := func(timeout time.Duration) (string, error) {
		var body string
		err := ns1.do(func() error {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			var d net.Dialer
			conn, err := d.DialContext(ctx, "tcp", "[fd00:2::1]:8080")
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
				return err
			}
			req, err := http.NewRequest(http.MethodGet, "http://[fd00:2::1]:8080/", nil)
			if err != nil {
				return err
			}
			if err := req.Write(conn); err != nil {
				return err
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), req)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("status %s", resp.Status)
			}
			body = string(b)
			return nil
		})
		return body, err
	}
	if body, err := request(10 * time.Second); err != nil || body != "through the tunnel" {
		t.Fatalf("request through the tunnel: %q, %v", body, err)
	}

	// A handshake, and traffic both ways.
	var before PeerState
	must(t, "handshake", ns1.do(func() error {
		d, err := Get("wgA")
		if err != nil {
			return err
		}
		if len(d.Peers) != 1 || d.Peers[0].LastHandshake.IsZero() || d.Peers[0].RxBytes == 0 || d.Peers[0].TxBytes == 0 {
			return fmt.Errorf("peers after a request: %+v", d.Peers)
		}
		before = d.Peers[0]
		return nil
	}))

	// The same peers again: nothing changes, the session stays.
	must(t, "the same peers", ns1.do(func() error {
		if err := SetPeers("wgA", []Peer{peerB}); err != nil {
			return err
		}
		d, err := Get("wgA")
		if err != nil {
			return err
		}
		if len(d.Peers) != 1 || !d.Peers[0].LastHandshake.Equal(before.LastHandshake) {
			return fmt.Errorf("peers after setting the same: %+v, handshake before %v", d.Peers, before.LastHandshake)
		}
		return nil
	}))
	if body, err := request(10 * time.Second); err != nil || body != "through the tunnel" {
		t.Fatalf("request after setting the same peers: %q, %v", body, err)
	}

	// A move to another port, as the agent's repair of a lost path does:
	// wgA keeps its session, and wgB, whose peer still names 51821,
	// follows the new port with wgA's next packet.
	must(t, "move wgA's port", ns1.do(func() error {
		if err := Configure("wgA", privA, 51831); err != nil {
			return err
		}
		d, err := Get("wgA")
		if err != nil {
			return err
		}
		if d.ListenPort != 51831 || len(d.Peers) != 1 || !d.Peers[0].LastHandshake.Equal(before.LastHandshake) {
			return fmt.Errorf("after the move: port %d, peers %+v, handshake before %v", d.ListenPort, d.Peers, before.LastHandshake)
		}
		return nil
	}))
	if body, err := request(10 * time.Second); err != nil || body != "through the tunnel" {
		t.Fatalf("request after the move: %q, %v", body, err)
	}
	must(t, "wgB follows the port", ns2.do(func() error {
		d, err := Get("wgB")
		if err != nil {
			return err
		}
		if len(d.Peers) != 1 || d.Peers[0].Endpoint != netip.MustParseAddrPort("127.0.0.1:51831") {
			return fmt.Errorf("wgB's peers after the move: %+v", d.Peers)
		}
		return nil
	}))
	// Set anew, the peer has no session and handshakes at once, without
	// a packet of its own to send.
	must(t, "the peer set anew", ns1.do(func() error {
		if err := SetPeers("wgA", nil); err != nil {
			return err
		}
		if err := SetPeers("wgA", []Peer{peerB}); err != nil {
			return err
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			d, err := Get("wgA")
			if err != nil {
				return err
			}
			if len(d.Peers) == 1 && !d.Peers[0].LastHandshake.IsZero() {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("no handshake within 5 s of setting the peer anew: %+v", d.Peers)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}))
	if body, err := request(10 * time.Second); err != nil || body != "through the tunnel" {
		t.Fatalf("request after setting the peer anew: %q, %v", body, err)
	}

	// Without the peer, nothing goes through.
	must(t, "remove the peer", ns1.do(func() error {
		if err := SetPeers("wgA", nil); err != nil {
			return err
		}
		d, err := Get("wgA")
		if err != nil {
			return err
		}
		if len(d.Peers) != 0 {
			return fmt.Errorf("peers after removing them: %+v", d.Peers)
		}
		return nil
	}))
	if body, err := request(2 * time.Second); err == nil {
		t.Fatalf("a request went through without the peer: %q", body)
	} else {
		t.Logf("without the peer: %v", err)
	}

	must(t, "delete wgA", ns1.do(func() error {
		if err := DeleteLink(idxA); err != nil {
			return err
		}
		if _, err := LinkByName("wgA"); !errors.Is(err, syscall.ENODEV) {
			return fmt.Errorf("wgA after DeleteLink: %v", err)
		}
		if _, err := Get("wgA"); !errors.Is(err, syscall.ENODEV) {
			return fmt.Errorf("Get of a deleted device: %v", err)
		}
		return nil
	}))
}
