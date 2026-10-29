// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package server builds one ferund instance from its configuration and
// runs it until it is told to stop, then stops it as §7 of the design
// says: no request, tool call or op is cut by a rolling deploy. cmd/ferund
// runs one; the end-to-end tests run several in one process, on ":0"
// addresses, against one database.
//
// An instance is: the database pool and its notification listener; the
// issuer and the instance's certificate on the nodes' port; the nodes'
// port (nodelink) with fleet's uplink and join servers; the leader
// election with every package's loops; the sandbox engine; the tools'
// runner; the MCP door; the bindings; the ports and watches (through the
// streams hook); the environments (ADR 0018) with their controller; the
// REST API with the UI; and its heartbeat row.
//
// Its stop, at the end of Run's context:
//  1. the row says stopping, /readyz fails, the leader resigns, the
//     engine begins no work of its own initiative (the ops that requests
//     need still begin), and the ingress's side refuses new ingress
//     connections, ends its Sync streams and refuses Wake and Hold;
//  2. it serves on for the readiness delay (5 s), then stops accepting
//     requests and waits for those in flight until the shutdown grace;
//     the watch streams end at once (OnShutdown), upgraded port
//     connections once no request is left (AfterRequests). From the
//     delay's end on, a tool call with an id reaches a guest only if it
//     ends before the grace, and the requests that wait for another
//     instance's call are told to ask again (ADR 0023). Then (2b) it
//     waits for the tool calls with an id whose callers left too, until
//     the same deadline, and cuts and records those still running then;
//  3. the engine refuses new ops and waits for its own, until the same
//     deadline;
//  4. the ops still in flight are released (the engine's Stop);
//  5. only then the node connections close, the nodes' port stops, the
//     background work ends, the row is deleted and the pool closed.
package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/api"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/bindings"
	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/domains"
	"github.com/gardenlinux/feos/go/internal/env"
	"github.com/gardenlinux/feos/go/internal/fleet"
	ingresssvc "github.com/gardenlinux/feos/go/internal/ingress/service"
	"github.com/gardenlinux/feos/go/internal/ingress/table"
	"github.com/gardenlinux/feos/go/internal/leader"
	"github.com/gardenlinux/feos/go/internal/mcpsrv"
	"github.com/gardenlinux/feos/go/internal/mesh"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/ports"
	"github.com/gardenlinux/feos/go/internal/sandbox"
	"github.com/gardenlinux/feos/go/internal/seal"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/tools"
	"github.com/gardenlinux/feos/go/internal/ui"
)

// Streams is what the streams hook builds for an instance: the port door
// and the watch service (internal/ports, internal/watch), with their
// background work, their leader loops and their part in the stop.
type Streams struct {
	// Ports is the forward class's handler; nil: its routes answer 503.
	Ports http.Handler
	// Forwarder is the same port door, typed: the environments' probes and
	// route door forward through it (env.Ports); nil: they cannot.
	Forwarder *ports.Handler
	// Watches are the watch handlers; nil: their routes answer 503.
	Watches api.Watches
	// Run is started with the instance and ends with its last step.
	Run []func(context.Context) error
	// Loops are registered with the leader election.
	Loops []leader.Loop
	// OnShutdown runs when the HTTP server's shutdown begins (§7 step 2):
	// watch.Service.EndStreams.
	OnShutdown func()
	// AfterRequests runs once the HTTP server's shutdown has returned (§7
	// step 2): ports.Handler.CloseUpgraded.
	AfterRequests func()
}

// streams is the extension point that internal/server/streams.go sets
// (WP-I): it builds the ports handler and the watch service of an
// instance from the instance's parts, which it reads from the Instance's
// fields: Engine (ports.Sandboxes, watch.Sandboxes), Links
// (nodelink.Conns), Bindings (ports.KubePorts), DB, the ID, Log and
// ResolveSandbox. nil: those routes answer 503 "not available on this
// instance".
var streams func(i *Instance) Streams

// The instance's own timings.
const (
	// stepTimeout bounds the database writes of the stop's steps 1 and 5
	// and the node connections' shutdown.
	stepTimeout = 10 * time.Second
	// listedFor is how long an instance with a node address is listed to
	// nodes before /readyz may pass: two telemetry intervals, so that
	// every node has dialled it.
	listedFor = 2 * config.TelemetryInterval
	// readHeaderTimeout bounds a request's headers; there is no write
	// timeout (tool calls run minutes).
	readHeaderTimeout = 10 * time.Second
	// claimRetry is how often a start tries again to claim a fresh row of
	// its instance ID.
	claimRetry = time.Second
	// ingressSupersedeGrace is how long an ingress's certificate stays
	// valid after a newer one was presented in a Hello.
	ingressSupersedeGrace = 10 * time.Minute
)

// Instance is one ferund instance.
type Instance struct {
	cfg     config.Config
	log     *slog.Logger
	id      string
	version string

	// The parts, built by New. The streams hook reads them.
	DB        *db.DB
	Store     *store.Store
	Issuer    *pki.Issuer
	certs     *pki.ServerCertSource
	Links     *nodelink.Server
	Elector   *leader.Elector
	heartbeat *leader.Heartbeat
	Activity  *activity.Recorder
	Fleet     *fleet.Service
	Ingress   *ingresssvc.Service
	Engine    *sandbox.Engine
	Runner    *tools.Runner
	Env       *env.Service
	Domains   *domains.Service // ADR 0021
	mcp       *mcpsrv.Server
	Bindings  *bindings.Service
	streams   Streams
	handler   http.Handler
	// podNetwork is the cluster's ULA when it is meshed (ADR 0017), from
	// the migration's bootstrap.
	podNetwork netip.Prefix

	httpLn  net.Listener
	httpSrv *http.Server
	// front is the front doors' certificate (--tls-cert); nil: plain HTTP.
	front *frontCert

	// started is set when Run starts; Ready reads it from other
	// goroutines.
	started  atomic.Pointer[startTimes]
	stopping atomic.Bool
	// nodesReady latches /readyz's wait for the node connections.
	nodesReady atomic.Bool
	// claimed is set by the first Run or Close: whichever it is releases
	// the instance; the other does nothing.
	claimed atomic.Bool
	// endReadinessDelay, when closed, ends the stop's readiness delay
	// early (tests).
	endReadinessDelay chan struct{}
	// afterStop runs after the stop's step 4, before step 5 (tests).
	afterStop func()
}

// startTimes is when an instance's Run started: the database's time (the
// nodes' reports are compared with it) and the local monotonic one (for
// elapsed times).
type startTimes struct {
	db   time.Time
	mono time.Time
}

// New builds an instance: it loads the front doors' certificate (with
// --tls-cert) and the issuer, opens and migrates the database (and
// bootstraps the operator tenant), and binds the front doors' and the
// nodes' listeners. Nothing serves before Run.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (_ *Instance, err error) {
	if log == nil {
		log = slog.Default()
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	i := &Instance{cfg: cfg, version: cfg.Version}
	if i.version == "" {
		i.version = "dev"
	}
	i.id = cfg.InstanceID
	if i.id == "" {
		if i.id, err = defaultInstanceID(); err != nil {
			return nil, err
		}
	}
	i.log = log.With("instance", i.id)

	// What can fail without side effects first.
	if cfg.TLSCert != "" {
		if i.front, err = loadFrontCert(cfg.TLSCert, cfg.TLSKey, i.log); err != nil {
			return nil, err
		}
	}
	var key []byte
	if cfg.SecretKeyFile != "" {
		if key, err = readSecretKey(cfg.SecretKeyFile); err != nil {
			return nil, fmt.Errorf("--secret-key-file: %w", err)
		}
	}
	var oidc *api.OIDCConfig
	if cfg.OIDCIssuer != "" {
		secret, err := os.ReadFile(cfg.OIDCClientSecretFile)
		if err != nil {
			return nil, fmt.Errorf("--oidc-client-secret-file: %w", err)
		}
		oidc = &api.OIDCConfig{Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCClientID,
			ClientSecret: strings.TrimSpace(string(secret)), Scopes: cfg.OIDCScopes, AdminGroup: cfg.OIDCAdminGroup}
		if oidc.ClientSecret == "" {
			return nil, errors.New("--oidc-client-secret-file: the file is empty")
		}
	}
	if i.Issuer, err = pki.LoadIssuer(cfg.CADir, cfg.TrustDomain); err != nil {
		return nil, fmt.Errorf("load the issuer from %s: %w", cfg.CADir, err)
	}
	if i.Issuer.ConstrainsURIs() {
		i.log.Warn("the issuer constrains URIs, and nodes since ADR 0024 refuse the nodes' port's certificate under it; "+
			"make a new issuer under the same root with ferund new-issuer", "ca_dir", cfg.CADir)
	}
	names := cfg.NodeServerNames
	if len(names) == 0 {
		names = []string{i.Issuer.ControlPlaneDomain()}
	}
	if i.certs, err = pki.NewServerCertSource(i.Issuer, names, config.ServerCertTTL, i.log); err != nil {
		return nil, fmt.Errorf("the certificate of the nodes' port: %w", err)
	}

	// The database.
	if i.DB, err = db.Open(ctx, cfg.DatabaseURL, i.log); err != nil {
		return nil, fmt.Errorf("open the database: %w", err)
	}
	defer func() {
		if err != nil {
			i.close()
		}
	}()
	if err = i.DB.Migrate(ctx, i.bootstrap); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	i.Store = store.New(i.DB)

	// The parts.
	i.Elector = leader.NewElector(i.DB, i.id, i.log)
	i.Activity = activity.NewRecorder(i.Store, i.id, i.log)
	var fl *fleet.Service
	var ing *ingresssvc.Service
	if i.Links, err = nodelink.New(nodelink.Config{
		Addr: cfg.NodeListen, TrustDomain: cfg.TrustDomain, InstanceID: i.id,
		Bundle: i.Issuer.Bundle(), ServerCert: i.certs.GetCertificate,
		NodeWait: cfg.NodeWait, ConnDrainTimeout: cfg.ConnDrainTimeout, MaxStreams: cfg.NodeMaxStreams,
		CheckNode: func(ctx context.Context, node, serial, peer string, atAccept bool) (time.Time, error) {
			return fl.CheckNode(ctx, node, serial, peer, atAccept)
		},
		CheckIngress: func(ctx context.Context, name, serial, peer string) (time.Time, error) {
			return ing.CheckIngress(ctx, name, serial, peer)
		},
		OnPresented: func(node, serial, peer string) { fl.OnPresented(node, serial, peer) },
		OnNodeConn:  func(node string) { fl.OnNodeConn(node) },
		Log:         i.log,
	}); err != nil {
		return nil, fmt.Errorf("the nodes' port: %w", err)
	}
	if i.Bindings, err = bindings.New(bindings.Config{Enabled: cfg.Bindings, Key: key, DenyPrefixes: cfg.DenyPrefixes,
		Log: i.log}, i.Store, i.DB); err != nil {
		return nil, fmt.Errorf("bindings: %w", err)
	}
	if i.Engine, err = sandbox.New(sandbox.Config{
		InstanceID: i.id, TrustDomain: cfg.TrustDomain, DefaultImage: cfg.DefaultImage, Placement: cfg.Placement,
		DefaultVCPUs: cfg.DefaultVCPUs, DefaultMemoryMiB: cfg.DefaultMemoryMiB, DiskBudget: cfg.DiskBudget,
		IdlePause: cfg.IdlePause, HibernateAfter: cfg.HibernateAfter, LostGrace: cfg.LostGrace, NodeWait: cfg.NodeWait,
		DrainTimeout: cfg.DrainTimeout, PendingRepairAfter: cfg.PendingRepairAfter,
		UnreachableRepairAfter: cfg.UnreachableRepairAfter, HibernateTimeout: cfg.HibernateTimeout,
		DenyPrefixes: cfg.DenyPrefixes, ImageBakes: cfg.ImageBakes, Log: i.log,
	}, sandbox.Deps{DB: i.DB, Store: i.Store, Conns: i.Links, Elector: i.Elector, Activity: i.Activity}); err != nil {
		return nil, fmt.Errorf("the sandbox engine: %w", err)
	}
	fl = fleet.New(fleet.Config{
		InstanceID: i.id, TrustDomain: cfg.TrustDomain, NodeCertTTL: cfg.NodeCertTTL, PodCertTTL: cfg.PodCertTTL,
		RolloutGrace: cfg.RolloutGrace, NodeTTL: cfg.NodeTTL, ConnDrainTimeout: cfg.ConnDrainTimeout,
		ActivityRetention: cfg.ActivityRetention, PodNetwork: i.podNetwork, Log: i.log,
	}, fleet.Deps{DB: i.DB, Store: i.Store, Issuer: i.Issuer, Links: i.Links, Hooks: i.Engine, Bindings: i.Bindings,
		Activity: i.Activity, Elector: i.Elector})
	i.Fleet = fl
	fleetv1.RegisterFleetServiceServer(i.Links.Uplink(), fl.UplinkServer())
	fleetv1.RegisterFleetServiceServer(i.Links.Join(), fl.JoinServer())
	ing = ingresssvc.New(ingresssvc.Config{
		InstanceID: i.id, TrustDomain: cfg.TrustDomain, AppsZone: cfg.AppsZone, CertTTL: cfg.NodeCertTTL,
		SupersedeGrace: ingressSupersedeGrace, PodNetwork: i.podNetwork, Log: i.log,
	}, ingresssvc.Deps{DB: i.DB, Store: i.Store, Issuer: i.Issuer, Links: i.Links, Engine: i.Engine,
		Builder: table.NewBuilder(i.DB, i.Store, cfg.AppsZone, i.log)})
	i.Ingress = ing
	ing.Register()
	i.Runner = tools.NewRunner(i.Engine, i.Links, i.Activity, i.DB, i.id, i.log)
	i.Runner.SetCallConfig(tools.CallConfig{Lease: cfg.CallLease, Retention: cfg.CallRetention, Leader: i.Elector})
	i.Engine.SetProcessLister(i.Runner)
	i.mcp = mcpsrv.New(mcpsrv.Config{AllowedOrigins: cfg.MCPAllowedOrigins, Version: i.version, Log: i.log},
		i.Runner, i.ResolveSandbox, i.DB)
	if streams != nil {
		i.streams = streams(i)
	}
	if i.Domains, err = i.newDomains(); err != nil {
		return nil, fmt.Errorf("domains: %w", err)
	}
	if i.Env, err = i.newEnvironments(key); err != nil {
		return nil, fmt.Errorf("environments: %w", err)
	}

	deps := api.Deps{
		DB: i.DB, Store: i.Store, Engine: i.Engine, Elector: i.Elector, Conns: i.Links, Fleet: fl,
		Tools: tools.NewREST(i.Runner), Specs: tools.Specs, MCP: i.mcp, Ports: i.streams.Ports,
		Watches: i.streams.Watches, Bindings: i.Bindings, Environments: i.Env, Ingress: ing, Domains: i.Domains,
		Ready: i.ready,
	}
	if ui.Built() {
		deps.UI = ui.FS()
	}
	i.Elector.Register(fl.Loops()...)
	i.Elector.Register(i.Engine.Loops()...)
	i.Elector.Register(activity.Loops(i.DB, activity.RetentionConfig{Activity: cfg.ActivityRetention,
		Traffic: cfg.TrafficRetention}, i.log)...)
	i.Elector.Register(api.Loops(deps)...)
	i.Elector.Register(i.streams.Loops...)
	i.Elector.Register(i.Env.Loops()...)
	i.Elector.Register(i.Domains.Loops()...)
	i.Elector.Register(i.Runner.Loops()...)
	i.handler = api.New(api.Config{
		AdminUsers: cfg.AdminUsers, InstanceID: i.id, PreparationWaitMs: config.PreparationBudget.Milliseconds(),
		PublicURL: cfg.PublicURL, OIDC: oidc, SessionTTL: cfg.SessionTTL, DefaultImage: cfg.DefaultImage,
		DefaultVCPUs: cfg.DefaultVCPUs, DefaultMemoryMiB: cfg.DefaultMemoryMiB, JoinTokenTTL: config.JoinTokenTTL,
		TelemetryInterval: config.TelemetryInterval, NodeTTL: cfg.NodeTTL, LostGrace: cfg.LostGrace,
		DrainTimeout: cfg.DrainTimeout, Log: i.log,
	}, deps)

	// The front doors' listener, last.
	if i.httpLn, err = net.Listen("tcp", cfg.Listen); err != nil {
		return nil, fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	i.httpSrv = &http.Server{
		Handler:           accessLog(i.handler, i.log),
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(i.log.Handler(), slog.LevelDebug),
	}
	if i.front != nil {
		i.httpSrv.TLSConfig = &tls.Config{GetCertificate: i.front.GetCertificate}
	}
	if i.streams.OnShutdown != nil {
		i.httpSrv.RegisterOnShutdown(i.streams.OnShutdown)
	}
	i.heartbeat = leader.NewHeartbeat(i.DB, leader.InstanceInfo{ID: i.id, Address: i.httpLn.Addr().String(),
		NodeAddress: cfg.NodeAdvertiseAddress, Version: i.version}, i.log)
	return i, nil
}

// newEnvironments builds the environments service on the engine, the
// tools' runner and the port door, and gives the engine its hooks: the
// unsealing of a pod template's secrets and the preparation of a node for
// an instance's pod. The runner's detached calls and the door's exports
// are taken where the instance's runner and door have them; without, the
// service says what does not work. The secrets are sealed under key,
// --secret-key-file's (internal/seal); without it they are refused.
func (i *Instance) newEnvironments(key []byte) (*env.Service, error) {
	runner, _ := any(i.Runner).(env.Runner)
	var fwd env.Ports
	if i.streams.Forwarder != nil {
		fwd, _ = any(i.streams.Forwarder).(env.Ports)
	}
	var sealer env.Sealer
	if key != nil {
		sl, err := seal.New(key)
		if err != nil {
			return nil, fmt.Errorf("--secret-key-file: %w", err)
		}
		sealer = sl
	}
	s, err := env.New(env.Deps{
		InstanceID: i.id, DB: i.DB, Store: i.Store, Elector: i.Elector, Conns: i.Links, Engine: i.Engine,
		Runner: runner, Ports: fwd, Sealer: sealer, AppsZone: i.cfg.AppsZone, EnvChanged: i.Domains.EnvChanged,
		Log: i.log,
	})
	if err != nil {
		return nil, err
	}
	i.Engine.SetSecretResolver(s.ResolveSecret)
	i.Engine.SetPrepareHook(s.PrepareHook)
	return s, nil
}

// newDomains builds the domains' core (ADR 0021): its checks ask
// --dns-resolver, or the system's resolvers without it; the apps zone,
// the edge's name and every --domain-reserved zone cannot be claimed.
func (i *Instance) newDomains() (*domains.Service, error) {
	cfg := i.cfg
	reserved := slices.Clone(cfg.DomainReserved)
	if cfg.AppsZone != "" {
		reserved = append(reserved, cfg.AppsZone)
	}
	s, err := domains.New(domains.Config{
		EdgeName: cfg.EdgeName, EdgeAddresses: cfg.EdgeAddresses, ChallengeLabel: cfg.DomainChallengeLabel,
		Grace: cfg.DomainGrace, UnverifiedMax: cfg.DomainUnverifiedMax, Reserved: reserved, Log: i.log,
	}, domains.Deps{Store: i.Store, Elector: i.Elector, DNS: domains.NewResolver(cfg.DNSResolver, 0)})
	if err != nil {
		return nil, err
	}
	if cfg.EdgeName == "" {
		i.log.Info("domains are off: no --edge-name")
	}
	return s, nil
}

// bootstrap is the migration's bootstrap: the cluster's pod network, and
// with --bootstrap-token the tenant "operator" with that token, while no
// tenant exists.
func (i *Instance) bootstrap(ctx context.Context, tx pgx.Tx) error {
	if err := i.bootstrapPodNetwork(ctx, store.On(tx)); err != nil {
		return err
	}
	if i.cfg.BootstrapToken == "" {
		return nil
	}
	created, err := store.On(tx).BootstrapOperator(ctx, store.HashToken(i.cfg.BootstrapToken))
	if err != nil {
		return err
	}
	if created {
		i.log.Info("bootstrapped the tenant operator with the bootstrap token")
	}
	return nil
}

// bootstrapPodNetwork reconciles --pod-network with the database's
// (ADR 0017): mesh draws the ULA unless the database has one, and keeps
// it; a /64 is kept unless the database has another (a /48 of before
// names its /64 of subnet 0, as migration 0011 made of the database's). A database that is
// meshed does not become routed, nor another mesh, by a flag: the nodes'
// addresses are of its ULA.
func (i *Instance) bootstrapPodNetwork(ctx context.Context, st *store.Store) error {
	i.podNetwork = netip.Prefix{}
	have, meshed, err := st.PodNetwork(ctx)
	if err != nil {
		return fmt.Errorf("read the pod network: %w", err)
	}
	want := i.cfg.PodNetwork
	switch {
	case want == config.PodNetworkRouted && meshed:
		return fmt.Errorf("the database's cluster is meshed on %s, and --pod-network is empty (routed): set it to mesh or %s",
			have, have)
	case want == config.PodNetworkRouted:
		return nil
	case meshed && want != config.PodNetworkMesh && !samePodNetwork(want, have):
		return fmt.Errorf("the database's cluster is meshed on %s, not on --pod-network %s", have, want)
	case meshed:
		i.podNetwork = have
		return nil
	}
	var ula netip.Prefix
	if want == config.PodNetworkMesh {
		ula, err = mesh.Draw()
	} else {
		ula, err = config.ParsePodNetwork(want)
	}
	if err != nil {
		return fmt.Errorf("--pod-network: %w", err)
	}
	if err := st.InsertPodNetwork(ctx, ula); err != nil {
		return fmt.Errorf("keep the pod network: %w", err)
	}
	i.log.Info("the pods are on the mesh of a new ULA", "pod_network", ula.String())
	i.podNetwork = ula
	return nil
}

// samePodNetwork reports whether --pod-network's ULA, want, is have.
func samePodNetwork(want string, have netip.Prefix) bool {
	p, err := config.ParsePodNetwork(want)
	return err == nil && p == have
}

// close releases what New opened, when New fails or Run never runs.
func (i *Instance) close() {
	if i.httpLn != nil {
		_ = i.httpLn.Close()
	}
	if i.Links != nil {
		ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
		i.Links.Shutdown(ctx)
		cancel()
	}
	if i.DB != nil {
		i.DB.Close()
	}
}

// Close releases an instance that New built and Run did not run: the
// front doors' listener, the nodes' port and the database pool. It does
// nothing, and returns at once, when Run has begun (Run releases the
// instance at its end); a later Run returns an error.
func (i *Instance) Close() {
	if i.claimed.CompareAndSwap(false, true) {
		i.close()
	}
}

// ID is the instance's id.
func (i *Instance) ID() string { return i.id }

// HTTPAddr is the front doors' listen address (the port chosen for ":0").
func (i *Instance) HTTPAddr() string { return i.httpLn.Addr().String() }

// NodeAddr is the nodes' port's listen address.
func (i *Instance) NodeAddr() string { return i.Links.Addr().String() }

// Handler is the instance's HTTP handler, the front doors without the
// listener (tests).
func (i *Instance) Handler() http.Handler { return i.handler }

// ResolveSandbox resolves a sandbox token for the doors with their own
// classes (MCP, the ports): model.ErrUnknownToken when it names none.
func (i *Instance) ResolveSandbox(ctx context.Context, bearer string) (auth.Sandbox, error) {
	return api.ResolveSandboxToken(ctx, i.DB.Pool, bearer)
}

// Ready is /readyz: the instance is not stopping, the database answers,
// and after a start the instance is connected to every ready node that
// reported since, and, with a node address, has been listed to nodes for
// two telemetry intervals; that wait ends after the ready window at the
// latest.
func (i *Instance) Ready(ctx context.Context) error { return i.ready(ctx) }

func (i *Instance) ready(ctx context.Context) error {
	if i.stopping.Load() {
		return errors.New("the instance is stopping")
	}
	if err := i.DB.Pool.Ping(ctx); err != nil {
		return fmt.Errorf("the database does not answer: %w", err)
	}
	if i.nodesReady.Load() {
		return nil
	}
	started := i.started.Load()
	if started == nil {
		return errors.New("the instance is starting")
	}
	elapsed := time.Since(started.mono)
	if elapsed >= i.cfg.ReadyWindow {
		i.nodesReady.Store(true)
		return nil
	}
	if i.cfg.NodeAdvertiseAddress != "" && elapsed < listedFor {
		return errors.New("waiting for the nodes to dial this instance")
	}
	nodes, err := i.Store.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("read the nodes: %w", err)
	}
	tel, err := i.Store.ListTelemetry(ctx)
	if err != nil {
		return fmt.Errorf("read the nodes: %w", err)
	}
	for _, n := range nodes {
		t, reported := tel[n.Name]
		if n.Status == model.NodeReady && reported && t.LastSeen.After(started.db) && !i.Links.Connected(n.Name) {
			return fmt.Errorf("waiting for node %s to connect", n.Name)
		}
	}
	i.nodesReady.Store(true)
	return nil
}

// Run serves until ctx ends, then stops the instance (§7) and returns.
// It returns an error when a listener fails; the instance is stopped then
// as well. Before it serves, it claims the instance's row (claim): it
// refuses to run under an ID another process runs under. An instance runs
// once, and not after Close.
func (i *Instance) Run(ctx context.Context) error {
	if !i.claimed.CompareAndSwap(false, true) {
		return errors.New("an instance runs once, and not after Close")
	}
	if err := i.claim(ctx); err != nil {
		i.close()
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	return i.run(ctx)
}

// claim makes the instance's row this run's before anything serves
// (store.ClaimInstance): written anew, not stopping, and the ops an earlier
// run under the same ID left released. A row written within the TTL is
// another process's under this ID, running or stopping, or the row of a
// run that ended without its stop (a crash, a kill) less than the TTL
// ago. claim tries again every second until the row is older than the TTL
// and claims it then; a row written meanwhile refuses the start, within a
// heartbeat interval.
func (i *Instance) claim(ctx context.Context) error {
	info := store.InstanceInfo{ID: i.id, Address: i.httpLn.Addr().String(), NodeAddress: i.cfg.NodeAdvertiseAddress,
		Version: i.version}
	var seen time.Time
	for {
		cctx, cancel := context.WithTimeout(ctx, stepTimeout)
		claimed, err := i.Store.ClaimInstance(cctx, info)
		cancel()
		var inUse *store.InstanceInUseError
		switch {
		case err == nil:
			if len(claimed.Released) > 0 || claimed.ToolCalls > 0 {
				i.log.Warn("released the ops and ended the tool calls an earlier run under this instance id left",
					"sandboxes", len(claimed.Released), "calls", claimed.ToolCalls)
			}
			return nil
		case !errors.As(err, &inUse):
			return fmt.Errorf("claim the instance's row: %w", err)
		case !seen.IsZero() && !inUse.LastSeen.Equal(seen):
			return fmt.Errorf("%w; another process runs under this --instance-id, which must be unique among the running instances", err)
		}
		if seen.IsZero() {
			i.log.Warn("the row of this instance id is fresh; waiting for it to age past the TTL",
				"last_seen", inUse.LastSeen, "stopping", inUse.Stopping, "age", inUse.Age.Round(time.Millisecond))
		}
		seen = inUse.LastSeen
		t := time.NewTimer(min(claimRetry, store.InstanceTTL-inUse.Age+100*time.Millisecond))
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (i *Instance) run(ctx context.Context) error {
	st := &startTimes{mono: time.Now()}
	if now, err := i.Store.Now(ctx); err == nil {
		st.db = now
	} else {
		st.db = time.Now()
	}
	i.started.Store(st)

	// The contexts of the stop's last step: the instance's parts, then the
	// activity recorder (it flushes what they recorded), then the
	// database's listener.
	life, endLife := context.WithCancel(context.WithoutCancel(ctx))
	recCtx, endRec := context.WithCancel(context.WithoutCancel(ctx))
	dbCtx, endDB := context.WithCancel(context.WithoutCancel(ctx))
	defer endLife()
	defer endRec()
	defer endDB()

	fatal := make(chan error, 2)
	var lifeWG, recWG, dbWG sync.WaitGroup
	start := func(wg *sync.WaitGroup, name string, c context.Context, f func(context.Context) error, critical bool) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(c); err != nil && c.Err() == nil {
				i.log.Error("stopped", "part", name, "err", err)
				if critical {
					select {
					case fatal <- fmt.Errorf("%s: %w", name, err):
					default:
					}
				}
			}
		}()
	}
	start(&dbWG, "database listener", dbCtx, i.DB.RunListener, false)
	start(&recWG, "activity", recCtx, i.Activity.Run, false)
	start(&lifeWG, "heartbeat", life, i.heartbeat.Run, false)
	start(&lifeWG, "leader", life, i.Elector.Run, false)
	start(&lifeWG, "fleet", life, i.Fleet.Run, false)
	start(&lifeWG, "ingress", life, i.Ingress.Run, false)
	start(&lifeWG, "engine", life, i.Engine.Run, false)
	start(&lifeWG, "environments", life, i.Env.Run, false)
	start(&lifeWG, "tools", life, i.Runner.Run, false)
	start(&lifeWG, "mcp", life, i.mcp.Run, false)
	start(&lifeWG, "server certificate", life, i.certs.Run, false)
	if i.front != nil {
		start(&lifeWG, "front doors' certificate", life, i.front.Run, false)
	}
	start(&lifeWG, "nodes' port", life, i.Links.Serve, true)
	for n, f := range i.streams.Run {
		start(&lifeWG, fmt.Sprintf("streams %d", n), life, f, false)
	}
	httpDone := make(chan struct{})
	go func() {
		defer close(httpDone)
		var err error
		if i.front != nil {
			err = i.httpSrv.ServeTLS(i.httpLn, "", "")
		} else {
			err = i.httpSrv.Serve(i.httpLn)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case fatal <- fmt.Errorf("front doors: %w", err):
			default:
			}
		}
	}()
	i.log.Info("instance started", "listen", i.HTTPAddr(), "nodes", i.NodeAddr(),
		"advertise", i.cfg.NodeAdvertiseAddress, "version", i.version)

	var runErr error
	select {
	case <-ctx.Done():
		i.log.Info("stopping")
	case runErr = <-fatal:
		i.log.Error("stopping after a failure", "err", runErr)
	}
	i.stop(httpDone)

	// Step 5: the node connections, the nodes' port, the background work,
	// the row, the pool.
	sctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
	i.Links.Shutdown(sctx)
	cancel()
	endLife()
	lifeWG.Wait()
	sctx, cancel = context.WithTimeout(context.Background(), stepTimeout)
	if err := i.heartbeat.Remove(sctx); err != nil {
		i.log.Warn("delete the instance's row", "err", err)
	}
	cancel()
	endRec()
	recWG.Wait()
	endDB()
	dbWG.Wait()
	i.DB.Close()
	i.log.Info("stopped")
	return runErr
}

// stop is steps 1 to 4 of §7.
func (i *Instance) stop(httpDone <-chan struct{}) {
	// Step 1: stopping; /readyz fails; no leader; no work of the
	// instance's own initiative.
	i.stopping.Store(true)
	sctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
	if err := i.heartbeat.SetStopping(sctx); err != nil {
		i.log.Warn("mark the instance stopping", "err", err)
	}
	if err := i.Elector.Resign(sctx); err != nil {
		i.log.Warn("resign the leadership", "err", err)
	}
	cancel()
	i.Engine.BeginStopping()
	i.Ingress.BeginStopping()

	// Step 2: serve on while balancers notice, then no new requests and
	// wait for those in flight.
	delay := time.NewTimer(i.cfg.ReadinessDelay)
	select {
	case <-delay.C:
	case <-i.endReadinessDelay:
	}
	delay.Stop()
	deadline, cancelDeadline := context.WithTimeout(context.Background(), i.cfg.ShutdownGrace)
	defer cancelDeadline()
	// Step 2 (ADR 0023): from now on a keyed call reaches a guest only if
	// it ends before the deadline, and requests waiting on another
	// instance's call are told to ask again.
	if dl, ok := deadline.Deadline(); ok {
		i.Runner.BeginDrain(dl)
	}
	if err := i.httpSrv.Shutdown(deadline); err != nil {
		i.log.Warn("requests still in flight at the shutdown grace; closing them", "err", err)
		_ = i.httpSrv.Close()
	}
	<-httpDone
	if i.streams.AfterRequests != nil {
		i.streams.AfterRequests()
	}
	// Step 2b (ADR 0023): the tool calls whose callers left, until the
	// same deadline; those still running then are cut and recorded.
	i.Runner.Drain(deadline)

	// Steps 3 and 4: no new ops; this instance's ops are waited for until
	// the same deadline, and released then.
	if err := i.Engine.Stop(deadline); err != nil {
		i.log.Warn("stop the engine", "err", err)
	}
	if i.afterStop != nil {
		i.afterStop()
	}
}

// accessLog logs every request at debug level, never its credentials.
func accessLog(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Debug("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start).Round(time.Millisecond))
	})
}

// defaultInstanceID is <short hostname>-<8 random hex>, new at every
// start.
func defaultInstanceID() (string, error) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "ferund"
	}
	if j := strings.IndexByte(host, '.'); j > 0 {
		host = host[:j]
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return strings.ToLower(host) + "-" + hex.EncodeToString(b[:]), nil
}

// readSecretKey reads the 32-byte key of --secret-key-file: the raw bytes,
// or their hex or base64 (standard or URL alphabet, padded or not), with
// surrounding whitespace ignored.
func readSecretKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) == 32 {
		return b, nil
	}
	s := strings.TrimSpace(string(b))
	if k, err := hex.DecodeString(s); err == nil && len(k) == 32 {
		return k, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if k, err := enc.DecodeString(s); err == nil && len(k) == 32 {
			return k, nil
		}
	}
	return nil, errors.New("the file holds no 32-byte key (raw, hex or base64)")
}
