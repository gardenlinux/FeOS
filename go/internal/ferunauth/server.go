// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ferunauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gardenlinux/feos/go/internal/egress"
	"github.com/gardenlinux/feos/go/internal/seal"
)

// issuerTimeout bounds every call to an issuer, redirects included.
const issuerTimeout = 10 * time.Second

// The HTTP servers' bounds.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 64 << 10
	// shutdownGrace is how long the requests in flight have once the
	// listener closes.
	shutdownGrace = 10 * time.Second
)

// Server is ferun-auth: the sign-in of visitors for providers of kind
// ferun and oidc, and the issuers' back-channel logouts. It holds no state
// but its caches; a sign-in in flight lives in a sealed cookie.
type Server struct {
	cfg     Config
	log     *slog.Logger
	sec     secrets
	sealer  *seal.Sealer
	ferund  *ferundClient
	issuers *issuers
	now     func() time.Time

	draining atomic.Bool
}

// New makes the server of cfg (Parse's), reading its files.
func New(cfg Config, log *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	sec, err := cfg.readSecrets()
	if err != nil {
		return nil, err
	}
	sl, err := seal.New(sec.key)
	if err != nil {
		return nil, fmt.Errorf("--key-file: %w", err)
	}
	deny := cfg.Deny
	if deny == nil {
		deny = egress.DefaultDeny()
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}
	ruled := egress.Client(egress.Rule{Deny: deny, Allow: cfg.Allow}.Judge, issuerTimeout, true, cfg.roots)
	return &Server{
		cfg:     cfg,
		log:     log,
		sec:     sec,
		sealer:  sl,
		ferund:  newFerundClient(cfg.FerunURL, sec.token),
		issuers: newIssuers(ruled, cfg.Scopes, now),
		now:     now,
	}, nil
}

// redirectURI is a provider's redirect URI, cb its callback path
// (callbackOf): <PublicURL>/callback/ferun, <PublicURL>/callback/<tenant>/<provider>.
func (s *Server) redirectURI(cb string) string { return s.cfg.PublicURL + callbackPrefix + cb }

// Handler is the sign-in's handler: GET /visit, GET /callback/ferun, GET
// /callback/{tenant}/{provider}, POST /backchannel/ferun and POST
// /backchannel/{tenant}/{provider}; anything else is 404.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.EscapedPath(); {
		case p == "/visit" && r.Method == http.MethodGet:
			s.visit(w, r)
		case strings.HasPrefix(p, callbackPrefix) && validCallback(p[len(callbackPrefix):]) && r.Method == http.MethodGet:
			s.callback(w, r, p[len(callbackPrefix):])
		case p == "/backchannel/ferun" && r.Method == http.MethodPost:
			s.backchannelFerun(w, r)
		case len(p) > len(backchannelPrefix) && p[:len(backchannelPrefix)] == backchannelPrefix && r.Method == http.MethodPost:
			s.backchannelTenant(w, r, p[len(backchannelPrefix):])
		default:
			notFound(w)
		}
	})
}

// healthHandler answers /healthz (the process runs) and /readyz (it takes
// sign-ins: not while it stops).
func (s *Server) healthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.draining.Load() {
			http.Error(w, "stopping", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// Run serves the listeners until ctx ends; then /readyz answers 503 for
// --shutdown-delay, while the balancer takes the instance out, and the
// listeners close, the requests in flight given 10 s. It returns nil after
// a stop by ctx, an error when a listener failed. Every goroutine it
// starts ends before it returns.
func (s *Server) Run(ctx context.Context) error {
	lis, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("--listen: %w", err)
	}
	hlis, err := net.Listen("tcp", s.cfg.HealthListen)
	if err != nil {
		_ = lis.Close()
		return fmt.Errorf("--health-listen: %w", err)
	}
	errLog := slog.NewLogLogger(s.log.Handler(), slog.LevelWarn)
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: readTimeout,
		WriteTimeout: writeTimeout, IdleTimeout: idleTimeout, MaxHeaderBytes: maxHeaderBytes, ErrorLog: errLog}
	hsrv := &http.Server{Handler: s.healthHandler(), ReadHeaderTimeout: readHeaderTimeout, ErrorLog: errLog}
	failed := make(chan error, 2)
	var wg sync.WaitGroup
	serve := func(name string, srv *http.Server, lis net.Listener) {
		wg.Go(func() {
			if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
				failed <- fmt.Errorf("ferun-auth: the %s listener: %w", name, err)
			}
		})
	}
	serve("sign-in", srv, lis)
	serve("health", hsrv, hlis)
	s.log.Info("ferun-auth: serving", "listen", lis.Addr().String(), "health", hlis.Addr().String(),
		"publicURL", s.cfg.PublicURL, "ferun", s.cfg.FerunURL, "issuer", s.cfg.Issuer)

	var runErr error
	select {
	case <-ctx.Done():
		s.log.Info("ferun-auth: stopping", "delay", s.cfg.ShutdownDelay)
	case runErr = <-failed:
		s.log.Error("ferun-auth: stopping after a failure", "err", runErr)
	}
	s.draining.Store(true)
	if runErr == nil && s.cfg.ShutdownDelay > 0 {
		t := time.NewTimer(s.cfg.ShutdownDelay)
		select {
		case <-t.C:
		case runErr = <-failed:
			t.Stop()
		}
	}
	base := context.WithoutCancel(ctx)
	sctx, cancel := context.WithTimeout(base, shutdownGrace)
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
	}
	cancel()
	hctx, cancel := context.WithTimeout(base, time.Second)
	if err := hsrv.Shutdown(hctx); err != nil {
		_ = hsrv.Close()
	}
	cancel()
	wg.Wait()
	if runErr == nil {
		select {
		case runErr = <-failed:
		default:
		}
	}
	if runErr == nil {
		s.log.Info("ferun-auth: stopped")
	}
	return runErr
}
