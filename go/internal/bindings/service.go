// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/seal"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Config is the bindings' share of ferund's configuration.
type Config struct {
	// Enabled is --bindings. The surfaces are on only with it and a Key.
	Enabled bool
	// Key is the 32-byte AES-256 key of --secret-key-file that seals the
	// credentials at rest; nil: none configured.
	Key []byte
	// DenyPrefixes are the destinations no binding and no preflight may
	// reach (--deny-prefix); nil: DefaultDenyPrefixes. Every address that
	// is not IPv4 is refused as well.
	DenyPrefixes []netip.Prefix
	Log          *slog.Logger
}

// The budgets of a preflight and a check, and of the name lookup of the
// destination rule at validation.
const (
	preflightBudget = 10 * time.Second
	lookupTimeout   = 5 * time.Second
)

// Service is the Kubernetes, SSH and HTTP bindings of ferund: the REST
// handlers of the sets, the preflights and the check, the access route,
// the source of a pod's bindings that fleet seals into
// IssuePodCredentials, and the clear part of its HTTP bindings that the
// engine sends its node. It implements fleet.BindingsSource and
// ports.KubePorts.
type Service struct {
	enabled bool
	sealer  *seal.Sealer // nil when off
	st      *store.Store
	d       *db.DB
	log     *slog.Logger

	// The destination rule; tests replace its policies to reach loopback
	// servers.
	dest destRule
	// budget bounds one preflight or check, dial to last byte.
	budget time.Duration
	// betweenReads, when set (by tests only), runs inside snapshot between
	// the read of the Kubernetes set and that of the SSH set.
	betweenReads func()
	// afterApply, when set (by tests only), runs in PutKube, PutSSH and
	// PutHTTP after the write has committed and before the answer is
	// written.
	afterApply func()

	// The hooks set after New (SetEnvSource, SetPusher).
	hooks  sync.Mutex
	envSrc EnvSource
	pusher Pusher
}

// New returns the Service. It is on when cfg.Enabled is set and cfg.Key is
// configured; a key that is not 32 bytes is an error.
func New(cfg Config, st *store.Store, d *db.DB) (*Service, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	s := &Service{st: st, d: d, log: log, budget: preflightBudget}
	deny := cfg.DenyPrefixes
	if deny == nil {
		deny = DefaultDenyPrefixes()
	}
	s.dest = newDestRule(deny)
	if cfg.Key != nil {
		sealer, err := seal.New(cfg.Key)
		if err != nil {
			return nil, err
		}
		s.sealer = sealer
	}
	s.enabled = cfg.Enabled && s.sealer != nil
	if cfg.Enabled && s.sealer == nil {
		log.Warn("bindings: --bindings is set but no --secret-key-file is configured; the Kubernetes and SSH surfaces stay off")
	}
	return s, nil
}

// Enabled reports whether the binding surfaces are on: --bindings and a
// key. Off, api does not register their routes, and Access answers the
// empty object.
func (s *Service) Enabled() bool { return s.enabled }

// PodBindings returns a pod's bindings, opened from their sealing at rest,
// and their version, for IssuePodCredentials to seal to the run's key: a
// sandbox's Kubernetes, SSH and HTTP sets and its interception CAs, or an
// environment's pod's HTTP bindings (those its object names) and its CAs.
// A pod without bindings, a sandbox that does not exist, or the surfaces
// off: nil and "". A sandbox's sets and their versions are read in one
// REPEATABLE READ snapshot, so the version names exactly the bindings
// returned.
func (s *Service) PodBindings(ctx context.Context, podID podid.ID) (*fleetv1.PodBindings, string, error) {
	if !s.enabled {
		return nil, "", nil
	}
	row, err := s.rowOf(ctx, podID)
	if err != nil || row == nil {
		return nil, "", err
	}
	if isEnvPod(*row) {
		return s.envPodBindings(ctx, *row)
	}
	snap, err := s.snapshot(ctx, podID)
	if errors.Is(err, errNoSandbox) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	kube, err := s.openKube(podID, snap.kubeVersion, snap.kube)
	if err != nil {
		return nil, "", err
	}
	ssh, err := s.openSSH(podID, snap.sshVersion, snap.ssh)
	if err != nil {
		return nil, "", err
	}
	http, err := s.openHTTP(podID, snap.http)
	if err != nil {
		return nil, "", err
	}
	if len(kube) == 0 && len(ssh) == 0 && len(http) == 0 {
		return nil, "", nil
	}
	out := &fleetv1.PodBindings{}
	for _, b := range kube {
		out.Kube = append(out.Kube, b.proto())
	}
	for _, b := range ssh {
		out.Ssh = append(out.Ssh, b.proto())
	}
	for _, b := range http {
		out.Http = append(out.Http, b.proto())
	}
	cas := givenCAs(http, snap.cas)
	if out.InterceptCas, err = s.openCAs(podID, cas); err != nil {
		return nil, "", err
	}
	return out, podVersionHTTP(podVersion(snap.kubeVersion, snap.sshVersion, ssh), snap.httpVersion, cas), nil
}

// KubePorts returns the loopback ports of a sandbox's Kubernetes bindings,
// which the port front door never forwards. It answers the stored ports
// whether the surfaces are on or off: bindings are delivered per run, so a
// pod started while they were on keeps its Kubernetes listeners after an
// instance restarts with them off, and reading the ports needs no key.
func (s *Service) KubePorts(ctx context.Context, id podid.ID) ([]int, error) {
	ports, err := s.st.KubePorts(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("kube ports of %s: %w", id, err)
	}
	return ports, nil
}

// off answers 404 "no such route" and reports true when the surfaces are
// off. api registers the set, preflight and check routes only when they
// are on; this keeps "off" meaning the same when a handler is reached
// anyway, and no handler ever seals or opens without a key.
func (s *Service) off(w http.ResponseWriter, r *http.Request) bool {
	if s.enabled {
		return false
	}
	apierr.Write(w, apierr.New(http.StatusNotFound, "no such route: %s %s", r.Method, r.URL.Path))
	return true
}

// fail answers err; an error that is not the API's own is logged, since
// its 500 carries only its text.
func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		s.log.Error("bindings: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	apierr.Write(w, err)
}

// podSnapshot is a sandbox's stored sets, their versions and its
// interception CAs, read in one snapshot.
type podSnapshot struct {
	kubeVersion, sshVersion, httpVersion string
	kube                                 []store.KubeBinding
	ssh                                  []store.SSHBinding
	http                                 []store.HTTPBinding
	cas                                  []store.InterceptCA
}

// snapshot reads the sets and CAs of a sandbox in one REPEATABLE READ,
// read-only transaction; errNoSandbox when the sandbox does not exist.
func (s *Service) snapshot(ctx context.Context, id podid.ID) (podSnapshot, error) {
	var snap podSnapshot
	tx, err := s.d.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return snap, fmt.Errorf("bindings of %s: %w", id, err)
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = tx.Rollback(rctx)
		cancel()
	}()
	st := store.On(tx)
	snap.kubeVersion, snap.kube, err = st.KubeBindings(ctx, id)
	if err == nil && s.betweenReads != nil {
		s.betweenReads()
	}
	if err == nil {
		snap.sshVersion, snap.ssh, err = st.SSHBindings(ctx, id)
	}
	if err == nil {
		snap.httpVersion, snap.http, err = st.HTTPBindings(ctx, id)
	}
	if err == nil {
		snap.cas, err = st.InterceptCAs(ctx, id)
	}
	if errors.Is(err, model.ErrNotFound) {
		return snap, errNoSandbox
	}
	if err != nil {
		return snap, fmt.Errorf("bindings of %s: %w", id, err)
	}
	return snap, nil
}
