// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ingress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/agent"
	"github.com/gardenlinux/feos/go/internal/ingress/proxy"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// The program's fixed timings (§7.6).
const (
	// handshakeFresh is how old a node's last WireGuard handshake may be
	// for the node to count as fresh: the data path prefers its
	// endpoints, and readiness waits for it.
	handshakeFresh = 180 * time.Second
	// readyWindow is how long after the first table of the process
	// readiness waits for every node with endpoints to be fresh and
	// admitted; afterwards one such node is enough.
	readyWindow = 60 * time.Second
	// leaveTimeout bounds the final Hold{Leaving} of a shutdown.
	leaveTimeout = 2 * time.Second
	// flushTimeout bounds the last sends of the access log.
	flushTimeout = 3 * time.Second
	// healthCloseTimeout bounds the health server's shutdown, the last
	// step.
	healthCloseTimeout = time.Second
	// healthReadHeaderTimeout is the health server's header timeout.
	healthReadHeaderTimeout = 10 * time.Second
	// memoryLimitPercent is the share of --memory-limit that the Go
	// runtime's soft limit takes: garbage collection works harder from
	// there on, before the container's limit kills the process.
	memoryLimitPercent = 90
)

// IPv6ConfDir is where the kernel keeps the IPv6 settings per interface;
// Prepare writes under it.
const IPv6ConfDir = "/proc/sys/net/ipv6/conf"

// Deps are the program's collaborators.
type Deps struct {
	// Mesh is the WireGuard device; required: agent.NewKernelMesh in
	// production, a fake in tests.
	Mesh agent.MeshDevice
	// Dial, when set, opens the connections to the instances in place of
	// a net.Dialer (tests map pod addresses with it).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// DialControlPlane, when set, opens the TCP connection to a control
	// plane address in place of the agent's dialer.
	DialControlPlane func(ctx context.Context, addr string) (net.Conn, error)
	// Log is the logger; none when nil.
	Log *slog.Logger
}

// Program is one ferun-ingress instance: the agent, which keeps the
// identity, the connection to the control plane, the table and the mesh,
// and the proxy, which serves the HTTPS and HTTP listeners from that
// table, plus the health listener. New binds the listeners; Run serves
// them until its context ends, then shuts down in the order of §7.6.
type Program struct {
	cfg   Config
	log   *slog.Logger
	now   func() time.Time
	agent *agent.Agent
	proxy *proxy.Proxy
	certs *proxy.Certs
	// tenantCerts is the tenant zone's wildcard; nil: none.
	tenantCerts *proxy.Certs

	lisHTTP, lisHTTPS, lisHealth net.Listener
	httpSrv, httpsSrv, healthSrv *http.Server

	// ran is set by the first Run or Close.
	ran      atomic.Bool
	draining atomic.Bool

	mu           sync.Mutex
	firstStateAt time.Time
	// readyKnown and lastReady are the last readiness verdict, logged
	// when it changes.
	readyKnown, lastReady bool
}

// Addrs are the addresses a program's listeners are bound to.
type Addrs struct {
	HTTP, HTTPS, Health string
}

// New checks cfg, loads the TLS certificate, makes the agent and the
// proxy, and binds the listeners. Run serves them; Close releases them
// when Run is not called.
func New(cfg Config, deps Deps) (*Program, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if deps.Mesh == nil {
		return nil, errors.New("ferun-ingress: no mesh device")
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	certs, err := proxy.LoadPair(cfg.TLSCert, cfg.TLSKey, log)
	if err != nil {
		return nil, fmt.Errorf("ferun-ingress: %w", err)
	}
	p := &Program{cfg: cfg, log: log, now: time.Now, certs: certs}
	if cfg.TenantZoneTLSCert != "" {
		if p.tenantCerts, err = proxy.LoadPair(cfg.TenantZoneTLSCert, cfg.TenantZoneTLSKey, log); err != nil {
			return nil, fmt.Errorf("ferun-ingress: the tenant zone's certificate: %w", err)
		}
	}
	a, err := agent.New(agent.Config{
		Name:             cfg.Name,
		StateDir:         cfg.StateDir,
		JoinTokenFile:    cfg.JoinTokenFile,
		ControlPlane:     cfg.ControlPlane,
		ControlPlaneName: cfg.ControlPlaneName,
		TrustDomain:      cfg.TrustDomain,
		MeshDevice:       cfg.MeshDevice,
		MeshPort:         cfg.MeshPort,
		MeshEndpoint:     cfg.MeshEndpoint,
		MeshMTU:          cfg.MeshMTU,
		MeshRepairEvery:  cfg.meshRepairEvery,
		Version:          cfg.Version,
		SinkEndpoint:     cfg.SinkEndpoint,
	}, agent.Deps{
		Mesh:             deps.Mesh,
		OnState:          p.onState,
		OnMesh:           p.onMesh,
		DialControlPlane: deps.DialControlPlane,
		Log:              log,
	})
	if err != nil {
		return nil, err
	}
	p.agent = a
	p.proxy = proxy.New(proxy.Config{
		WakeHold:             cfg.WakeHold,
		PendingHold:          cfg.PendingHold,
		MaxHeld:              cfg.MaxHeld,
		MaxHeldPerTenant:     cfg.MaxHeldPerTenant,
		MaxHeldPerEnv:        cfg.MaxHeldPerEnv,
		MaxInflight:          cfg.MaxInflight,
		MaxInflightPerTenant: cfg.MaxInflightPerTenant,
		MaxInflightPerEnv:    cfg.MaxInflightPerEnv,
		DialTimeout:          cfg.UpstreamDialTimeout,
		IdleTimeout:          cfg.UpstreamIdleTimeout,
		ConnWaitTimeout:      cfg.UpstreamWaitTimeout,
		MaxBody:              cfg.MaxRequestBody,
		MaxConnsPerEndpoint:  cfg.MaxConnsPerEndpoint,
	}, proxy.Deps{Waker: a, Holder: a, AccessLog: a, Dial: deps.Dial, Log: log})

	if err := p.listen(); err != nil {
		return nil, err
	}
	p.httpsSrv = p.proxy.HTTPSServer(certs, p.tenantCerts)
	p.httpSrv = p.proxy.HTTPServer()
	p.healthSrv = &http.Server{
		Handler:           p.healthHandler(),
		ReadHeaderTimeout: healthReadHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	return p, nil
}

// listen binds the three listeners, or none.
func (p *Program) listen() error {
	var bound []net.Listener
	bind := func(name, addr string) (net.Listener, error) {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			for _, b := range bound {
				_ = b.Close()
			}
			return nil, fmt.Errorf("ferun-ingress: the %s listener: %w", name, err)
		}
		bound = append(bound, l)
		return l, nil
	}
	var err error
	if p.lisHTTPS, err = bind("HTTPS", p.cfg.ListenHTTPS); err != nil {
		return err
	}
	if p.lisHTTP, err = bind("HTTP", p.cfg.ListenHTTP); err != nil {
		return err
	}
	if p.lisHealth, err = bind("health", p.cfg.HealthListen); err != nil {
		return err
	}
	return nil
}

// Addrs returns the addresses the listeners are bound to.
func (p *Program) Addrs() Addrs {
	return Addrs{HTTP: p.lisHTTP.Addr().String(), HTTPS: p.lisHTTPS.Addr().String(), Health: p.lisHealth.Addr().String()}
}

// Close releases the listeners of a program whose Run was not called;
// Run fails afterwards. It does nothing once Run was called.
func (p *Program) Close() error {
	if !p.ran.CompareAndSwap(false, true) {
		return nil
	}
	return errors.Join(p.lisHTTPS.Close(), p.lisHTTP.Close(), p.lisHealth.Close())
}

// onState is the agent's OnState: the proxy routes by the new table, and
// the first table of the process starts readiness's window.
func (p *Program) onState(s *wire.State) {
	p.proxy.SetState(s)
	p.mu.Lock()
	if p.firstStateAt.IsZero() {
		p.firstStateAt = p.now()
	}
	p.mu.Unlock()
}

// onMesh is the agent's OnMesh: a node whose last handshake is younger
// than 180 s is fresh, and the proxy prefers its endpoints.
func (p *Program) onMesh(handshakes map[string]time.Time) {
	p.proxy.SetMeshHealth(freshNodes(handshakes, p.now()))
}

// freshNodes tells for each node whether its last handshake (the zero
// time for none) is fresh at now.
func freshNodes(handshakes map[string]time.Time, now time.Time) map[string]bool {
	fresh := make(map[string]bool, len(handshakes))
	for node, at := range handshakes {
		fresh[node] = isFresh(at, now)
	}
	return fresh
}

func isFresh(handshake, now time.Time) bool {
	return !handshake.IsZero() && now.Sub(handshake) < handshakeFresh
}

// Run serves the listeners and runs the agent until ctx ends, then stops
// in the order of §7.6:
//
//  1. draining: /readyz fails, held requests end with 503, and no idle
//     hold is admitted any more;
//  2. the listeners serve on for --shutdown-delay, while the balancers
//     take the instance out;
//  3. the HTTPS and HTTP servers shut down within --shutdown-grace, and
//     the connections still open are cut after it;
//  4. the connections to the instances are closed, which ends upgraded
//     connections and long polls;
//  5. Hold{Leaving} releases every hold of the instance (within 2 s);
//  6. the connection to the control plane is closed;
//  7. the health listener closes.
//
// It returns nil after a stop by ctx, and an error when the agent or a
// listener failed, after the same sequence without the delay. Every
// goroutine it starts ends before it returns.
func (p *Program) Run(ctx context.Context) error {
	if !p.ran.CompareAndSwap(false, true) {
		return errors.New("ferun-ingress: Run called twice, or after Close")
	}
	base := context.WithoutCancel(ctx)
	agentCtx, stopAgent := context.WithCancel(base)
	defer stopAgent()
	loopCtx, stopLoops := context.WithCancel(base)
	defer stopLoops()

	softLimit := setMemoryLimit(p.cfg.MemoryLimit)
	agentDone := make(chan error, 1)
	go func() { agentDone <- p.agent.Run(agentCtx) }()
	var loops sync.WaitGroup
	loops.Go(func() { p.proxy.Run(loopCtx) })
	loops.Go(func() { p.certs.Run(loopCtx) })
	if p.tenantCerts != nil {
		loops.Go(func() { p.tenantCerts.Run(loopCtx) })
	}

	failed := make(chan error, 3)
	var servers sync.WaitGroup
	serve := func(name string, run func() error) {
		servers.Go(func() {
			if err := run(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				failed <- fmt.Errorf("ferun-ingress: the %s listener: %w", name, err)
			}
		})
	}
	serve("health", func() error { return p.healthSrv.Serve(p.lisHealth) })
	serve("HTTPS", func() error { return p.httpsSrv.ServeTLS(p.lisHTTPS, "", "") })
	serve("HTTP", func() error { return p.httpSrv.Serve(p.lisHTTP) })
	addrs := p.Addrs()
	p.log.Info("ferun-ingress: serving", "name", p.cfg.Name, "version", p.cfg.Version,
		"https", addrs.HTTPS, "http", addrs.HTTP, "health", addrs.Health, "controlPlane", p.cfg.ControlPlane,
		"memoryLimit", softLimit)

	var runErr error
	agentEnded := false
	select {
	case <-ctx.Done():
		p.log.Info("ferun-ingress: stopping", "delay", p.cfg.ShutdownDelay, "grace", p.cfg.ShutdownGrace)
	case err := <-agentDone:
		agentEnded = true
		if err == nil {
			err = errors.New("ferun-ingress: the agent stopped")
		}
		runErr = err
	case err := <-failed:
		runErr = err
	}
	if runErr != nil {
		p.log.Error("ferun-ingress: stopping after a failure", "err", runErr)
	}

	// 1. Draining.
	p.draining.Store(true)
	p.proxy.Drain()
	// 2. Serve on while the balancers notice /readyz.
	if runErr == nil {
		t := time.NewTimer(p.cfg.ShutdownDelay)
		select {
		case <-t.C:
		case err := <-failed:
			t.Stop()
			runErr = err
			p.log.Error("ferun-ingress: a listener failed while draining", "err", err)
		}
	}
	// 3. Stop accepting, and wait for the requests within the grace.
	p.shutdownServers(base)
	// 4. Cut what is left towards the instances.
	p.proxy.CloseUpstreams()
	// 5. Send the rest of the access log, then release every hold of
	// the instance at once.
	if !agentEnded {
		p.flushAccessLog(base)
		p.leave(base)
	}
	// 6. Close the control connection.
	stopAgent()
	if !agentEnded {
		if err := <-agentDone; err != nil && runErr == nil {
			runErr = err
		}
	}
	stopLoops()
	loops.Wait()
	// 7. The health listener last: the probes see the drain to the end.
	hctx, cancel := context.WithTimeout(base, healthCloseTimeout)
	if err := p.healthSrv.Shutdown(hctx); err != nil {
		_ = p.healthSrv.Close()
	}
	cancel()
	servers.Wait()
	if runErr == nil {
		p.log.Info("ferun-ingress: stopped")
	}
	return runErr
}

// setMemoryLimit sets the Go runtime's soft memory limit to
// memoryLimitPercent of limit, unless limit is 0 or a soft limit is set
// already (GOMEMLIMIT). It returns the soft limit in effect, 0 for none.
func setMemoryLimit(limit int64) int64 {
	if cur := debug.SetMemoryLimit(-1); cur != math.MaxInt64 {
		return cur
	}
	if limit <= 0 {
		return 0
	}
	soft := limit / 100 * memoryLimitPercent
	debug.SetMemoryLimit(soft)
	return soft
}

// shutdownServers shuts the HTTPS and HTTP servers down within the
// grace, and closes the connections still open after it.
func (p *Program) shutdownServers(base context.Context) {
	ctx, cancel := context.WithTimeout(base, p.cfg.ShutdownGrace)
	defer cancel()
	var wg sync.WaitGroup
	for _, s := range []struct {
		name string
		srv  *http.Server
	}{{"HTTPS", p.httpsSrv}, {"HTTP", p.httpSrv}} {
		wg.Go(func() {
			if err := s.srv.Shutdown(ctx); err != nil {
				p.log.Warn("ferun-ingress: requests outlasted the shutdown grace; cutting their connections",
					"listener", s.name, "grace", p.cfg.ShutdownGrace)
				_ = s.srv.Close()
			}
		})
	}
	wg.Wait()
}

// flushAccessLog sends what the access log holds still; what it cannot
// send within flushTimeout is lost.
func (p *Program) flushAccessLog(base context.Context) {
	ctx, cancel := context.WithTimeout(base, flushTimeout)
	defer cancel()
	if err := p.agent.FlushAccessLog(ctx); err != nil {
		_, _, pending := p.agent.AccessLogCounts()
		p.log.Warn("ferun-ingress: could not send the rest of the access log", "err", err, "records", pending)
	}
}

// leave sends the final Hold{Leaving}: the control plane releases every
// hold of the instance, so the idle timers start now, and stops counting
// it for the idle fence.
func (p *Program) leave(base context.Context) {
	ctx, cancel := context.WithTimeout(base, leaveTimeout)
	defer cancel()
	held := p.proxy.Held()
	if err := p.agent.Leave(ctx); err != nil {
		p.log.Warn("ferun-ingress: could not tell the control plane that the instance leaves; its holds lapse within 30 s",
			"err", err, "held", len(held))
		return
	}
	p.log.Info("ferun-ingress: told the control plane that the instance leaves", "released", len(held))
}

// maxJoinTokenFile bounds the join token file CopyJoinToken copies: a
// token takes about 130 bytes.
const maxJoinTokenFile = 4096

// CopyJoinToken copies the instance's own join token from src, its key in
// the Secret of every instance's tokens, to dst, the file serve reads
// (--join-token-file), with mode 0400; prepare runs it in the init
// container, the one container that mounts the Secret, so that serve,
// which faces the internet, never sees another instance's token. Without
// src (the Secret is optional and holds a token only while an instance
// needs one), dst is removed. A file above 4 KiB is an error.
func CopyJoinToken(src, dst string, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	f, err := os.Open(src)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove the join token file: %w", err)
		}
		log.Info("ferun-ingress prepare: no join token for this instance", "source", src)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the join token: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxJoinTokenFile+1))
	if err != nil {
		return fmt.Errorf("read the join token: %w", err)
	}
	if len(data) > maxJoinTokenFile {
		return fmt.Errorf("the join token in %s takes more than %d bytes", src, maxJoinTokenFile)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".join-token-*")
	if err != nil {
		return fmt.Errorf("write the join token file: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(0o400)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		return fmt.Errorf("write the join token file: %w", err)
	}
	log.Info("ferun-ingress prepare: copied the instance's join token", "source", src, "file", dst)
	return nil
}

// Prepare readies the network namespace for the mesh, as root before
// serve (the init container): it writes 0 to the disable_ipv6 setting of
// all and default under confDir (IPv6ConfDir) where present and not 0,
// and logs what it changed.
func Prepare(confDir string, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	for _, conf := range []string{"all", "default"} {
		path := filepath.Join(confDir, conf, "disable_ipv6")
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			log.Warn("ferun-ingress prepare: no IPv6 setting; the mesh needs IPv6", "path", path)
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		was := strings.TrimSpace(string(data))
		if was == "0" {
			log.Info("ferun-ingress prepare: IPv6 is enabled", "path", path)
			continue
		}
		if err := os.WriteFile(path, []byte("0\n"), 0o644); err != nil {
			return fmt.Errorf("enable IPv6: write 0 to %s: %w", path, err)
		}
		log.Info("ferun-ingress prepare: enabled IPv6", "path", path, "was", was)
	}
	return nil
}
