// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package nodelink

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// readClosed reports whether the peer closed c within d; what the peer
// sends meanwhile is read and dropped.
func readClosed(c net.Conn, d time.Duration) bool {
	c.SetReadDeadline(time.Now().Add(d))
	var b [4096]byte
	for {
		_, err := c.Read(b[:])
		if err == nil {
			continue
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return false
		}
		return true
	}
}

func TestALPNRouting(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", time.Hour)

	t.Run("none", func(t *testing.T) {
		c, err := tls.Dial("tcp", e.addr(), e.tlsConfig(n))
		if err != nil {
			t.Fatalf("handshake without ALPN: %v", err)
		}
		defer c.Close()
		if p := c.ConnectionState().NegotiatedProtocol; p != "" {
			t.Fatalf("negotiated %q", p)
		}
		if !readClosed(c, 3*time.Second) {
			t.Error("a connection without ALPN was not closed")
		}
	})
	t.Run("http/1.1", func(t *testing.T) {
		c, err := tls.Dial("tcp", e.addr(), e.tlsConfig(n, "http/1.1"))
		if err == nil {
			defer c.Close()
			if !readClosed(c, 3*time.Second) {
				t.Error("an http/1.1 connection was not closed")
			}
		}
	})
	t.Run("node connection without a certificate", func(t *testing.T) {
		c, err := tls.Dial("tcp", e.addr(), e.tlsConfig(nil, ALPNNode))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if !readClosed(c, 3*time.Second) {
			t.Error("a node connection without a certificate was not closed")
		}
	})
	t.Run("node connection with a pod certificate", func(t *testing.T) {
		_, key, _ := ed25519.GenerateKey(rand.Reader)
		csr, _ := pki.CreateCSR(key, "pod")
		pc, leaf, err := e.iss.IssuePod(csr, pki.SandboxID(testTD, "acme", podid.Mint()), pki.PodSubject(testTD, "pod", ""), nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		pod := &tnode{name: "pod", cert: tls.Certificate{Certificate: pc.Chain, PrivateKey: key, Leaf: leaf}, leaf: leaf}
		for _, alpn := range []string{ALPNNode, ALPNUplink} {
			c, err := tls.Dial("tcp", e.addr(), e.tlsConfig(pod, alpn))
			if err != nil {
				t.Fatal(err)
			}
			if !readClosed(c, 3*time.Second) {
				t.Errorf("%s with a pod certificate was not closed", alpn)
			}
			c.Close()
		}
	})
	if n := e.chk.total(); n != 0 {
		t.Errorf("CheckNode was asked %d times about connections without a node", n)
	}
	// A node connection with ALPN feos-node/1 and a node certificate works.
	ns := e.dialNode(n, "a")
	eventually(t, 5*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	_ = ns
}

func TestHandshakeDeadline(t *testing.T) {
	e := newEnv(t, envOpt{tune: func(s *Server) { s.handshakeTimeout = 300 * time.Millisecond }})
	c, err := net.Dial("tcp", e.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	if !readClosed(c, 5*time.Second) {
		t.Fatal("a silent connection was not closed")
	}
	if d := time.Since(start); d < 200*time.Millisecond || d > 3*time.Second {
		t.Errorf("closed after %v, want about 300ms", d)
	}
	eventually(t, time.Second, "logged", func() bool { return e.logs.count("nodes' port: TLS handshake failed") == 1 })
}

func TestProbeNotLogged(t *testing.T) {
	e := newEnv(t)
	for range 3 {
		c, err := net.Dial("tcp", e.addr())
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	// A real failure after the probes is logged; the probes before it are not.
	c, _ := net.Dial("tcp", e.addr())
	c.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	readClosed(c, 3*time.Second)
	c.Close()
	eventually(t, 2*time.Second, "the failure logged", func() bool { return e.logs.count("nodes' port: TLS handshake failed") == 1 })
	never(t, 200*time.Millisecond, "a probe logged", func() bool { return e.logs.count("nodes' port: TLS handshake failed") > 1 })
}

// halfCloser shuts its write side down after its first write: a peer that
// sends its ClientHello and nothing more.
type halfCloser struct {
	*net.TCPConn
	once sync.Once
}

func (h *halfCloser) Write(b []byte) (int, error) {
	n, err := h.TCPConn.Write(b)
	h.once.Do(func() { h.TCPConn.CloseWrite() })
	return n, err
}

func TestEOFAfterClientHelloLogged(t *testing.T) {
	e := newEnv(t)
	c, err := net.Dial("tcp", e.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tc := tls.Client(&halfCloser{TCPConn: c.(*net.TCPConn)}, e.tlsConfig(nil, ALPNUplink))
	tc.SetDeadline(time.Now().Add(3 * time.Second))
	if err := tc.Handshake(); err == nil {
		t.Fatal("a handshake without the client's Finished succeeded")
	}
	// The port read EOF where the client's Finished belongs: a failed
	// handshake, not a probe.
	eventually(t, 2*time.Second, "the failure logged", func() bool { return e.logs.count("nodes' port: TLS handshake failed") == 1 })
}

// flakyListener fails its first errs Accepts as accept4 does when the
// process has no descriptor left.
type flakyListener struct {
	net.Listener
	mu   sync.Mutex
	errs int
}

func (l *flakyListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.errs > 0 {
		l.errs--
		l.mu.Unlock()
		return nil, &net.OpError{Op: "accept", Net: "tcp", Addr: l.Addr(), Err: os.NewSyscallError("accept4", syscall.EMFILE)}
	}
	l.mu.Unlock()
	return l.Listener.Accept()
}

func TestAcceptErrorsRetried(t *testing.T) {
	var fl *flakyListener
	e := newEnv(t, envOpt{tune: func(s *Server) {
		fl = &flakyListener{Listener: s.lis, errs: 3}
		s.lis = fl
	}})
	eventually(t, 2*time.Second, "the accept errors", func() bool {
		fl.mu.Lock()
		defer fl.mu.Unlock()
		return fl.errs == 0
	})
	// The port goes on accepting.
	select {
	case err := <-e.served:
		e.served <- err
		t.Fatalf("Serve returned on an accept error: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	e.dialNode(e.node("n1", time.Hour), "a")
	eventually(t, 3*time.Second, "connected after accept errors", func() bool { return e.srv.Connected("n1") })
	if e.logs.count("nodes' port: accept failed; retrying") != 1 {
		t.Errorf("accept failures logged %d times, want once (rate-limited)", e.logs.count("nodes' port: accept failed; retrying"))
	}
}

func TestHandshakeSemaphore(t *testing.T) {
	e := newEnv(t, envOpt{tune: func(s *Server) {
		s.handshakes = make(chan struct{}, 2)
		s.handshakeTimeout = 2 * time.Second
	}})
	var held []net.Conn
	for range 2 {
		c, err := net.Dial("tcp", e.addr())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		held = append(held, c)
	}
	eventually(t, 2*time.Second, "both handshakes pending", func() bool { return len(e.srv.handshakes) == 2 })
	c, err := net.Dial("tcp", e.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	if !readClosed(c, time.Second) {
		t.Fatal("a third pending handshake was admitted")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("the excess connection was not closed at once")
	}
	if e.srv.droppedHandshakes.Load() != 1 {
		t.Errorf("dropped %d", e.srv.droppedHandshakes.Load())
	}
	// Once the pending ones time out, handshakes are admitted again.
	for _, h := range held {
		readClosed(h, 5*time.Second)
	}
	eventually(t, 3*time.Second, "slots free", func() bool { return len(e.srv.handshakes) == 0 })
	e.dialNode(e.node("n1", time.Hour), "a")
	eventually(t, 5*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
}

func TestCheckSemaphore(t *testing.T) {
	block := make(chan struct{})
	e := newEnv(t, envOpt{tune: func(s *Server) {
		s.checks = make(chan struct{}, 1)
		s.checkWait = 200 * time.Millisecond
	}})
	entered := make(chan struct{}, 4)
	e.chk.setHook(func(c checkCall) {
		if c.node == "slow" {
			entered <- struct{}{}
			<-block
		}
	})
	defer close(block)
	slow := e.node("slow", time.Hour)
	go e.tryDialNode(slow, "slow")
	<-entered
	// The only slot is taken: the next check waits checkWait, then the
	// connection is refused as on a transient error.
	n1 := e.node("n1", time.Hour)
	ns, err := e.tryDialNode(n1, "a")
	if err != nil {
		t.Fatal(err)
	}
	ns.waitClosed(t, 3*time.Second)
	if e.srv.Connected("n1") || e.chk.count(n1.serial, true) != 0 {
		t.Error("connected or checked without a check slot")
	}
}

// OnPresented runs in the accept check's slot: a node that redials as
// fast as it can never has more presentations recorded at once than
// there are check slots, and while they all wait (on the node's row lock,
// say), the next accept waits for a slot like any check.
func TestPresentedInCheckSlot(t *testing.T) {
	block := make(chan struct{})
	entered := make(chan struct{}, 4)
	e := newEnv(t, envOpt{
		cfg: func(c *Config) {
			c.OnPresented = func(node, serial, addr string) {
				if node == "slow" {
					entered <- struct{}{}
					<-block
				}
			}
		},
		tune: func(s *Server) {
			s.checks = make(chan struct{}, 1)
			s.checkWait = 200 * time.Millisecond
		},
	})
	defer close(block)
	slow := e.node("slow", time.Hour)
	go e.tryDialNode(slow, "slow")
	<-entered
	if e.chk.count(slow.serial, true) != 1 {
		t.Fatal("the presentation came before the check")
	}
	n1 := e.node("n1", time.Hour)
	ns, err := e.tryDialNode(n1, "a")
	if err != nil {
		t.Fatal(err)
	}
	ns.waitClosed(t, 3*time.Second)
	if e.srv.Connected("n1") || e.chk.count(n1.serial, true) != 0 {
		t.Error("checked or connected while a presentation held the only slot")
	}
}
