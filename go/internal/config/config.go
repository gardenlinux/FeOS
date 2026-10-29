// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package config is ferund's configuration: the flags of its subcommands,
// each with an environment variable FERUN_<NAME> (the flag's name in upper
// snake case), and the fixed timings that are not flags. A flag given on
// the command line wins over its variable; a variable wins over the
// default. Durations are written in Go's syntax (90s, 10m, 12h), byte sizes
// as integers with an optional KiB, MiB or GiB suffix.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/mesh"
	"github.com/gardenlinux/feos/go/internal/pki"
)

// The fixed timings of ferund (§3 of the design), named here in one place.
// The packages that act on them hold their own copies; these are what the
// configuration, the API and the documentation refer to.
const (
	// InstanceHeartbeat is how often an instance writes its row;
	// InstanceTTL how long a row counts as live without a write.
	InstanceHeartbeat = 5 * time.Second
	InstanceTTL       = 15 * time.Second
	// LeaderLeaseTTL is the leader lease's lifetime; LeaderRenew how often
	// the leader renews it.
	LeaderLeaseTTL = 15 * time.Second
	LeaderRenew    = 5 * time.Second
	// PreparationBudget bounds the preparation of a sandbox before a tool
	// call (placement, create, resume, restore): 7 minutes and 5 seconds.
	// REST advertises it as preparationWaitMs.
	PreparationBudget = 7*time.Minute + 5*time.Second
	// CallLease is how long the row of a running tool call with an id
	// (ADR 0023) lives without a renewal by the instance that runs it; the
	// instance renews it every third of it.
	CallLease = 30 * time.Second
	// CallRetention is how long the record of a tool call with an id is
	// kept after the call ended.
	CallRetention = time.Hour
	// TelemetryInterval is the interval nodes report at.
	TelemetryInterval = 5 * time.Second
	// ReconcileClaimInterval is how often one node's inventory reconcile
	// may be claimed.
	ReconcileClaimInterval = 30 * time.Second
	// HoldLease is the lease of an exec or forward hold; HoldRenew how
	// often its instance renews it.
	HoldLease = 30 * time.Second
	HoldRenew = 10 * time.Second
	// JoinTokenTTL is a join token's lifetime when none is given.
	JoinTokenTTL = time.Hour
	// ServerCertTTL is the lifetime of an instance's certificate on the
	// nodes' port; it is issued again at three quarters of it.
	ServerCertTTL = 24 * time.Hour
	// ReadinessDelay is how long a stopping instance keeps serving with
	// /readyz failing before it stops accepting requests (§7 step 2).
	ReadinessDelay = 5 * time.Second
	// ReadyWindow is how long after its start /readyz waits at most for
	// the instance's node connections (§11).
	ReadyWindow = 30 * time.Second
)

// EnvPrefix starts every environment variable of a flag.
const EnvPrefix = "FERUN_"

// The pod networks of a cluster (--pod-network), besides a ULA /64 itself.
const (
	PodNetworkRouted = ""
	PodNetworkMesh   = "mesh"
)

// The placement policies.
const (
	PlacementBinpack = "binpack"
	PlacementSpread  = "spread"
)

// The bounds of a sandbox's size (vcpus, memory_mib).
const (
	MinVCPUs     = 1
	MaxVCPUs     = 32
	MinMemoryMiB = 256
	MaxMemoryMiB = 65536
)

// Config is the configuration of `ferund serve`: every flag of §3, typed,
// and the few settings that are not flags (set by main or by tests that run
// instances in process).
type Config struct {
	// DatabaseURL is the PostgreSQL DSN (--database-url, required).
	DatabaseURL string
	// Listen is the front doors' address: REST, MCP, the UI and the ports
	// (--listen). ":0" picks a free port (tests).
	Listen string
	// TLSCert and TLSKey serve Listen with TLS; empty: plain HTTP, TLS
	// terminated by the balancer.
	TLSCert, TLSKey string
	// NodeListen is the nodes' port (--node-listen), TLS 1.3 always.
	NodeListen string
	// NodeAdvertiseAddress is the host:port nodes dial to reach this
	// instance's nodes' port; empty: the instance is not listed to nodes.
	NodeAdvertiseAddress string
	// InstanceID names the instance; empty: <hostname>-<8 random hex>,
	// new at every start.
	InstanceID string
	// TrustDomain is the SPIFFE trust domain.
	TrustDomain string
	// CADir holds the issuer: issuer.crt, issuer.key, issuer.json,
	// bundle.crt.
	CADir string
	// NodeServerNames are the DNS names of the instance's certificate on
	// the nodes' port; empty: the control-plane domain of issuer.json.
	NodeServerNames []string
	// SecretKeyFile holds the 32-byte key (raw, hex or base64) that seals
	// bindings at rest.
	SecretKeyFile string
	// Bindings serves the Kubernetes and SSH binding surfaces; it takes
	// effect only with SecretKeyFile.
	Bindings bool
	// DenyPrefixes are the destinations preflights, bindings and the image
	// resolver may not reach; nil: the built-in set (DefaultDenyPrefixes),
	// which "default" in --deny-prefix adds to the prefixes given.
	DenyPrefixes []netip.Prefix
	// ImageBakes has a pod's node bake its image once into a layer in the
	// object store, which later creates of the image start on once its
	// registry said yes to them (ADR 0025).
	ImageBakes bool
	// AdminUsers are the operators' Basic credentials (user → password),
	// the fallback beside OIDC sign-in.
	AdminUsers map[string]string
	// PublicURL is the front door's external URL, without a trailing
	// slash; required with OIDCIssuer.
	PublicURL string
	// OIDCIssuer is the OIDC issuer of human sign-in, exactly as its
	// discovery document states it; empty: no OIDC.
	OIDCIssuer           string
	OIDCClientID         string
	OIDCClientSecretFile string
	OIDCScopes           []string
	// OIDCAdminGroup makes a signed-in user an admin when the groups claim
	// holds it.
	OIDCAdminGroup string
	// SessionTTL is the absolute lifetime of a UI session.
	SessionTTL time.Duration
	// BootstrapToken becomes a token of the tenant "operator" while no
	// tenant exists.
	BootstrapToken string
	// MCPAllowedOrigins are the Origins /mcp accepts; empty: every request
	// with an Origin is refused.
	MCPAllowedOrigins []string
	// PodNetwork is the cluster's pod network (ADR 0017): PodNetworkRouted,
	// every node with a prefix of its network's; PodNetworkMesh, a ULA /64
	// drawn at random once and kept in the database; or such a /64 itself,
	// "fdXX:XXXX:XXXX::/64" (a /48 stands for its /64 of subnet 0).
	PodNetwork string
	// AppsZone is the DNS zone of public environments' default hosts,
	// which ferun-ingress serves (ADR 0021); empty: no hosts, and no
	// environment can be public.
	AppsZone string
	// EdgeName is the name tenants' domains point at with a CNAME (ADR
	// 0021), such as edge.fecode.dev, which the operator publishes with the
	// balancer's addresses; empty: domains are off.
	EdgeName string
	// EdgeAddresses are edge addresses beside what EdgeName resolves to:
	// what a domain's A and AAAA records may name.
	EdgeAddresses []netip.Addr
	// DomainChallengeLabel is the label of a claim's TXT record,
	// <label>.<name>.
	DomainChallengeLabel string
	// DomainGrace is how long a record of a verified domain may answer
	// wrong before the domain is deactivated.
	DomainGrace time.Duration
	// DomainUnverifiedMax is the most claims a tenant may hold that are
	// not verified.
	DomainUnverifiedMax int
	// DomainReserved are zones no domain may be or be under, beside the
	// apps zone and the edge's name: the platform's own domain.
	DomainReserved []string
	// DNSResolver is the resolver (host:port) the domains' checks ask;
	// empty: the system's resolvers (/etc/resolv.conf).
	DNSResolver string

	DefaultImage     string
	DefaultVCPUs     int
	DefaultMemoryMiB int
	// Placement is PlacementBinpack or PlacementSpread.
	Placement string
	// DiskBudget is placement's per-pod disk gate, in bytes.
	DiskBudget int64

	IdlePause              time.Duration // 0 disables
	HibernateAfter         time.Duration // 0 disables
	NodeTTL                time.Duration
	LostGrace              time.Duration
	NodeWait               time.Duration
	RolloutGrace           time.Duration
	DrainTimeout           time.Duration
	PendingRepairAfter     time.Duration
	UnreachableRepairAfter time.Duration
	NodeCertTTL            time.Duration
	PodCertTTL             time.Duration
	// NodeMaxStreams is the HTTP/2 stream limit nodes advertise on a node
	// connection; the stream budgets derive from it.
	NodeMaxStreams   int
	ConnDrainTimeout time.Duration
	// HibernateTimeout is how long a hibernate's node call may run; the
	// hibernate op's deadline is this plus a pause's deadline (feosd
	// suspends a running pod first) and 16 seconds per GiB of memory.
	// It is how long ferund waits: a feosd that reports its hibernations
	// goes on with one after the call ends, the sandbox is paused until its
	// end arrives as the pod's event, and a resume meanwhile abandons it
	// unless it is a drain's or past its point of no return. An older
	// feosd runs the hibernation in the call, which the deadline cuts.
	HibernateTimeout time.Duration
	// ShutdownGrace is how long a stopping instance waits for its
	// requests, its tool calls (those whose callers left too: ADR 0023)
	// and its ops (§7). A container deployment sets its termination grace
	// period to at least this plus 60 s.
	ShutdownGrace     time.Duration
	ActivityRetention time.Duration
	TrafficRetention  time.Duration
	LogLevel          slog.Level

	// Version is the build's version, shown to nodes, MCP clients and the
	// admin API. Not a flag.
	Version string
	// ReadinessDelay is §7 step 2's wait between failing /readyz and
	// stopping to accept requests; Default sets ReadinessDelay. Not a
	// flag: tests that stop instances in process shorten it.
	ReadinessDelay time.Duration
	// ReadyWindow bounds how long /readyz waits for the node connections
	// after a start; Default sets ReadyWindow. Not a flag.
	ReadyWindow time.Duration
	// CallLease and CallRetention are the lease and the retention of the
	// tool calls with an id (ADR 0023); Default sets CallLease and
	// CallRetention. Not flags: tests shorten them.
	CallLease, CallRetention time.Duration
}

// Default returns the configuration of serve with every default of §3 and
// no database URL.
func Default() Config {
	return Config{
		Listen:                 ":8100",
		NodeListen:             ":7443",
		TrustDomain:            "feos.local",
		CADir:                  "/etc/ferun/ca",
		OIDCClientID:           "ferun",
		OIDCScopes:             []string{"openid", "profile", "email"},
		OIDCAdminGroup:         "admins",
		SessionTTL:             12 * time.Hour,
		DefaultImage:           "ghcr.io/maltej/debian-ai:latest",
		ImageBakes:             true,
		DefaultVCPUs:           2,
		DefaultMemoryMiB:       512,
		Placement:              PlacementBinpack,
		DiskBudget:             4 << 30,
		IdlePause:              10 * time.Second,
		HibernateAfter:         0,
		NodeTTL:                15 * time.Second,
		LostGrace:              10 * time.Minute,
		NodeWait:               5 * time.Second,
		RolloutGrace:           30 * time.Second,
		DrainTimeout:           2 * time.Minute,
		PendingRepairAfter:     10 * time.Minute,
		UnreachableRepairAfter: 2 * time.Minute,
		NodeCertTTL:            168 * time.Hour,
		PodCertTTL:             time.Hour,
		NodeMaxStreams:         1024,
		ConnDrainTimeout:       20 * time.Minute,
		HibernateTimeout:       time.Hour,
		ShutdownGrace:          11 * time.Minute,
		ActivityRetention:      720 * time.Hour,
		TrafficRetention:       168 * time.Hour,
		DomainChallengeLabel:   "_ferun-challenge",
		DomainGrace:            72 * time.Hour,
		DomainUnverifiedMax:    50,
		LogLevel:               slog.LevelInfo,
		ReadinessDelay:         ReadinessDelay,
		ReadyWindow:            ReadyWindow,
		CallLease:              CallLease,
		CallRetention:          CallRetention,
	}
}

// EnvName is the environment variable of a flag: FERUN_ and the flag's
// name in upper snake case.
func EnvName(flagName string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// Parse reads the flags of serve from args and, for the flags not given,
// their environment variables through getenv, over Default, and validates
// the result. --help yields flag.ErrHelp (Usage prints the help).
func Parse(args []string, getenv func(string) string) (Config, error) {
	c := Default()
	fs := newFlagSet("serve")
	t := registerServe(fs, &c)
	if err := parseFlags(fs, args, getenv); err != nil {
		return Config{}, err
	}
	if err := c.finish(t); err != nil {
		return Config{}, err
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// serveTexts are the values of serve's flags that need parsing after the
// flag set (finish).
type serveTexts struct {
	adminUsers, scopes, logLevel string
}

// registerServe defines serve's flags on fs, writing into c and into the
// texts it returns.
func registerServe(fs *flag.FlagSet, c *Config) *serveTexts {
	t := &serveTexts{scopes: strings.Join(c.OIDCScopes, " "), logLevel: strings.ToLower(c.LogLevel.String())}

	fs.StringVar(&c.DatabaseURL, "database-url", c.DatabaseURL, "PostgreSQL DSN (required)")
	fs.StringVar(&c.Listen, "listen", c.Listen, "front doors: REST, MCP, the UI and the ports (HTTP; TLS by the balancer, or --tls-cert/--tls-key)")
	fs.StringVar(&c.TLSCert, "tls-cert", c.TLSCert, "certificate file (PEM) to serve --listen with TLS")
	fs.StringVar(&c.TLSKey, "tls-key", c.TLSKey, "key file (PEM) of --tls-cert")
	fs.StringVar(&c.NodeListen, "node-listen", c.NodeListen, "the nodes' port (TLS 1.3 always)")
	fs.StringVar(&c.NodeAdvertiseAddress, "node-advertise-address", c.NodeAdvertiseAddress,
		"host:port nodes dial to reach this instance's nodes' port; empty: not listed to nodes (single machine)")
	fs.StringVar(&c.InstanceID, "instance-id", c.InstanceID, "the instance's id (default <hostname>-<8 random hex>, new at every start)")
	fs.StringVar(&c.TrustDomain, "trust-domain", c.TrustDomain, "SPIFFE trust domain")
	fs.StringVar(&c.CADir, "ca-dir", c.CADir, "directory of issuer.crt, issuer.key, issuer.json and bundle.crt (never root.key)")
	fs.Var((*listValue)(&c.NodeServerNames), "node-server-names",
		"DNS names of the instance's certificate on the nodes' port, comma-separated or repeated; each under the control-plane domain (default: the control-plane domain)")
	fs.StringVar(&c.SecretKeyFile, "secret-key-file", c.SecretKeyFile, "file of the 32-byte key (raw, hex or base64) sealing bindings at rest")
	fs.BoolVar(&c.Bindings, "bindings", c.Bindings, "serve the Kubernetes and SSH binding surfaces (only with --secret-key-file)")
	fs.Var((*prefixList)(&c.DenyPrefixes), "deny-prefix",
		"a destination prefix preflights, bindings and the image resolver may not reach, comma-separated or repeated; \"default\" is the built-in set (default: the built-in set)")
	fs.BoolVar(&c.ImageBakes, "image-bakes", c.ImageBakes,
		"have a pod's node bake its image once into a layer in the object store, which later creates of the image start on once its registry said yes")
	fs.StringVar(&t.adminUsers, "admin-users", "", "user:password,user:password for the admin API: the Basic fallback beside OIDC")
	fs.StringVar(&c.PublicURL, "public-url", c.PublicURL, "the front door's external URL, e.g. https://run.fecode.dev; required with --oidc-issuer")
	fs.StringVar(&c.OIDCIssuer, "oidc-issuer", c.OIDCIssuer, "OIDC issuer of human sign-in, exactly as its discovery document states it; empty: no OIDC")
	fs.StringVar(&c.OIDCClientID, "oidc-client-id", c.OIDCClientID, "OIDC client id")
	fs.StringVar(&c.OIDCClientSecretFile, "oidc-client-secret-file", c.OIDCClientSecretFile,
		"file of the OIDC client secret (client_secret_basic); required with --oidc-issuer")
	fs.StringVar(&t.scopes, "oidc-scopes", t.scopes, "OIDC scopes, space-separated; must contain openid")
	fs.StringVar(&c.OIDCAdminGroup, "oidc-admin-group", c.OIDCAdminGroup, "a signed-in user is an admin when the groups claim holds this group")
	fs.DurationVar(&c.SessionTTL, "session-ttl", c.SessionTTL, "lifetime of a UI session (absolute)")
	fs.StringVar(&c.BootstrapToken, "bootstrap-token", c.BootstrapToken, "becomes a token of tenant operator when no tenant exists")
	fs.Var((*listValue)(&c.MCPAllowedOrigins), "mcp-allowed-origins",
		"comma-separated Origin allowlist of /mcp; empty: every request with an Origin is refused")
	fs.StringVar(&c.PodNetwork, "pod-network", c.PodNetwork,
		"the pods' network: empty, routed (every node a prefix of its network's); mesh, a ULA /64 drawn once and kept in the database; or such a /64 (fdXX:XXXX:XXXX::/64)")
	fs.StringVar(&c.AppsZone, "apps-zone", c.AppsZone,
		"the DNS zone of public environments' default hosts, served by ferun-ingress (ADR 0021); empty: no hosts, and no environment can be public")
	fs.StringVar(&c.EdgeName, "edge-name", c.EdgeName,
		"the name tenants' domains point at with a CNAME (ADR 0021), e.g. edge.fecode.dev; empty: domains are off")
	fs.Var((*addrList)(&c.EdgeAddresses), "edge-address",
		"an edge address beside what --edge-name resolves to, which a domain's A and AAAA records may name; comma-separated or repeated")
	fs.StringVar(&c.DomainChallengeLabel, "domain-challenge-label", c.DomainChallengeLabel,
		"the label of a domain claim's TXT record, <label>.<name>")
	fs.DurationVar(&c.DomainGrace, "domain-grace", c.DomainGrace,
		"how long a record of a verified domain may answer wrong before the domain is deactivated")
	fs.IntVar(&c.DomainUnverifiedMax, "domain-unverified-max", c.DomainUnverifiedMax,
		"the most domain claims a tenant may hold that are not verified")
	fs.Var((*listValue)(&c.DomainReserved), "domain-reserved",
		"a zone no domain may be or be under (the platform's own domain), beside the apps zone and --edge-name; comma-separated or repeated")
	fs.StringVar(&c.DNSResolver, "dns-resolver", c.DNSResolver,
		"the DNS resolver (host:port) the domains' checks ask; empty: the system's resolvers (/etc/resolv.conf)")
	fs.StringVar(&c.DefaultImage, "default-image", c.DefaultImage, "image of a sandbox created without one")
	fs.IntVar(&c.DefaultVCPUs, "default-vcpus", c.DefaultVCPUs, "vCPUs of a sandbox created without a size")
	fs.IntVar(&c.DefaultMemoryMiB, "default-memory-mib", c.DefaultMemoryMiB, "memory (MiB) of a sandbox created without a size")
	fs.StringVar(&c.Placement, "placement", c.Placement, "placement policy: binpack or spread")
	fs.Var((*byteSize)(&c.DiskBudget), "disk-budget", "per-pod disk gate of placement (bytes, or with a KiB, MiB or GiB suffix)")
	fs.DurationVar(&c.IdlePause, "idle-pause", c.IdlePause, "a running sandbox idle this long is paused; 0 disables")
	fs.DurationVar(&c.HibernateAfter, "hibernate-after", c.HibernateAfter, "a sandbox paused this long is hibernated; 0 disables")
	fs.DurationVar(&c.NodeTTL, "node-ttl", c.NodeTTL, "telemetry age before a node is down")
	fs.DurationVar(&c.LostGrace, "lost-grace", c.LostGrace, "how long a node is down before its sandboxes count as lost")
	fs.DurationVar(&c.NodeWait, "node-wait", c.NodeWait, "how long a call waits for a node's connection")
	fs.DurationVar(&c.RolloutGrace, "rollout-grace", c.RolloutGrace, "how long a call waits for a node that announced it is stopping")
	fs.DurationVar(&c.DrainTimeout, "drain-timeout", c.DrainTimeout, "how long a draining node waits for running execs; then they are cancelled and their sandboxes move")
	fs.DurationVar(&c.PendingRepairAfter, "pending-repair-after", c.PendingRepairAfter, "a pod pending without an op this long is repaired")
	fs.DurationVar(&c.UnreachableRepairAfter, "unreachable-repair-after", c.UnreachableRepairAfter,
		"a running pod whose guest stays unreachable this long is repaired")
	fs.DurationVar(&c.NodeCertTTL, "node-cert-ttl", c.NodeCertTTL, "lifetime of node certificates; renewed at half of it, so that a node switched off for up to half of it needs nothing")
	fs.DurationVar(&c.PodCertTTL, "pod-cert-ttl", c.PodCertTTL, "lifetime of pod certificates (renewed at three quarters)")
	fs.IntVar(&c.NodeMaxStreams, "node-max-streams", c.NodeMaxStreams, "HTTP/2 stream limit of a node connection; the stream budgets derive from it")
	fs.DurationVar(&c.ConnDrainTimeout, "conn-drain-timeout", c.ConnDrainTimeout, "a draining node connection is closed after this at the latest")
	fs.DurationVar(&c.HibernateTimeout, "hibernate-timeout", c.HibernateTimeout,
		"how long ferund waits for a hibernate (plus a pause's deadline and 16s per GiB of memory); its upload of the pod's disk and memory to the object store takes minutes per GiB, and the node's hibernation goes on after the wait")
	fs.DurationVar(&c.ShutdownGrace, "shutdown-grace", c.ShutdownGrace,
		"how long a stopping instance waits for its requests, its tool calls and its ops; set a container's termination grace to at least this plus 60s")
	fs.DurationVar(&c.ActivityRetention, "activity-retention", c.ActivityRetention, "how long activity is kept")
	fs.DurationVar(&c.TrafficRetention, "traffic-retention", c.TrafficRetention, "how long traffic records are kept")
	fs.StringVar(&t.logLevel, "log-level", t.logLevel, "debug, info, warn or error")
	return t
}

// finish parses the flag values that need more than the flag package.
func (c *Config) finish(t *serveTexts) error {
	users, err := ParseAdminUsers(t.adminUsers)
	if err != nil {
		return fmt.Errorf("--admin-users: %w", err)
	}
	c.AdminUsers = users
	c.OIDCScopes = strings.Fields(t.scopes)
	if err := c.LogLevel.UnmarshalText([]byte(t.logLevel)); err != nil {
		return fmt.Errorf("--log-level: %w", err)
	}
	c.PublicURL = strings.TrimSuffix(c.PublicURL, "/")
	return nil
}

// Validate checks a serve configuration: what Parse checks after reading
// the flags, and what an instance built in process must satisfy.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if c.DatabaseURL == "" {
		bad("--database-url is required")
	}
	if c.Listen == "" {
		bad("--listen must not be empty")
	}
	if c.NodeListen == "" {
		bad("--node-listen must not be empty")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		bad("--tls-cert and --tls-key go together")
	}
	if c.NodeAdvertiseAddress != "" {
		if _, port, err := splitHostPort(c.NodeAdvertiseAddress); err != nil || port == "" {
			bad("--node-advertise-address %q is not host:port", c.NodeAdvertiseAddress)
		}
	}
	if err := pki.ValidateTrustDomain(c.TrustDomain); err != nil {
		bad("--trust-domain: %w", err)
	}
	if c.CADir == "" {
		bad("--ca-dir must not be empty")
	}
	for _, n := range c.NodeServerNames {
		if _, err := netip.ParseAddr(n); err == nil {
			bad("--node-server-names: %q is an IP address; the certificate on the nodes' port carries DNS names only", n)
		}
	}
	for u, p := range c.AdminUsers {
		if u == "" || p == "" {
			bad("--admin-users: a user and a password must not be empty")
		}
	}
	if c.PublicURL != "" {
		if err := checkPublicURL(c.PublicURL); err != nil {
			bad("--public-url: %w", err)
		}
	}
	if c.OIDCIssuer != "" {
		if u, err := url.Parse(c.OIDCIssuer); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			bad("--oidc-issuer %q is not an http(s) URL", c.OIDCIssuer)
		}
		if c.PublicURL == "" {
			bad("--public-url is required with --oidc-issuer")
		}
		if c.OIDCClientID == "" {
			bad("--oidc-client-id must not be empty with --oidc-issuer")
		}
		if c.OIDCClientSecretFile == "" {
			bad("--oidc-client-secret-file is required with --oidc-issuer")
		}
		if !slices.Contains(c.OIDCScopes, "openid") {
			bad("--oidc-scopes must contain openid")
		}
		if c.OIDCAdminGroup == "" {
			bad("--oidc-admin-group must not be empty with --oidc-issuer")
		}
	}
	if c.SessionTTL <= 0 {
		bad("--session-ttl must be positive")
	}
	if c.PodNetwork != PodNetworkRouted && c.PodNetwork != PodNetworkMesh {
		if _, err := ParsePodNetwork(c.PodNetwork); err != nil {
			bad("--pod-network: %w", err)
		}
	}
	if c.AppsZone != "" {
		if err := pki.ValidateDomain(c.AppsZone); err != nil {
			bad("--apps-zone: %w", err)
		}
	}
	if c.EdgeName != "" {
		if err := pki.ValidateDomain(c.EdgeName); err != nil {
			bad("--edge-name: %w", err)
		}
	}
	for _, z := range c.DomainReserved {
		if err := pki.ValidateDomain(z); err != nil {
			bad("--domain-reserved: %w", err)
		}
	}
	if !isChallengeLabel(c.DomainChallengeLabel) {
		bad("--domain-challenge-label %q is not a DNS label (lowercase letters, digits, '-' and '_', at most 63, "+
			"not beginning or ending with '-')", c.DomainChallengeLabel)
	}
	if c.DomainUnverifiedMax < 1 {
		bad("--domain-unverified-max must be at least 1")
	}
	if c.DNSResolver != "" {
		if _, _, err := splitHostPort(c.DNSResolver); err != nil {
			bad("--dns-resolver %q is not host:port: %v", c.DNSResolver, err)
		}
	}
	if c.DefaultImage == "" {
		bad("--default-image must not be empty")
	}
	if c.DefaultVCPUs < MinVCPUs || c.DefaultVCPUs > MaxVCPUs {
		bad("--default-vcpus must be between %d and %d", MinVCPUs, MaxVCPUs)
	}
	if c.DefaultMemoryMiB < MinMemoryMiB || c.DefaultMemoryMiB > MaxMemoryMiB {
		bad("--default-memory-mib must be between %d and %d", MinMemoryMiB, MaxMemoryMiB)
	}
	if c.Placement != PlacementBinpack && c.Placement != PlacementSpread {
		bad("--placement must be %s or %s, not %q", PlacementBinpack, PlacementSpread, c.Placement)
	}
	if c.DiskBudget < 0 {
		bad("--disk-budget must not be negative")
	}
	if c.NodeMaxStreams < 4 {
		bad("--node-max-streams must be at least 4")
	}
	nonNegative := []struct {
		name string
		v    time.Duration
	}{{"idle-pause", c.IdlePause}, {"hibernate-after", c.HibernateAfter}, {"shutdown-grace", c.ShutdownGrace}}
	for _, d := range nonNegative {
		if d.v < 0 {
			bad("--%s must not be negative", d.name)
		}
	}
	positive := []struct {
		name string
		v    time.Duration
	}{
		{"node-ttl", c.NodeTTL}, {"lost-grace", c.LostGrace}, {"node-wait", c.NodeWait},
		{"rollout-grace", c.RolloutGrace}, {"drain-timeout", c.DrainTimeout},
		{"pending-repair-after", c.PendingRepairAfter}, {"unreachable-repair-after", c.UnreachableRepairAfter},
		{"node-cert-ttl", c.NodeCertTTL}, {"pod-cert-ttl", c.PodCertTTL}, {"conn-drain-timeout", c.ConnDrainTimeout},
		{"hibernate-timeout", c.HibernateTimeout},
		{"activity-retention", c.ActivityRetention}, {"traffic-retention", c.TrafficRetention},
		{"domain-grace", c.DomainGrace},
	}
	for _, d := range positive {
		if d.v <= 0 {
			bad("--%s must be positive", d.name)
		}
	}
	if c.ReadinessDelay < 0 || c.ReadyWindow < 0 {
		bad("the readiness delay and window must not be negative")
	}
	return errors.Join(errs...)
}

// checkPublicURL accepts an absolute http or https URL of a host, without
// a path, query or fragment: the routes of /auth/ live at the root.
func checkPublicURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("%q is not an http(s) URL of a host", s)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("%q must name only the scheme and the host", s)
	}
	return nil
}

// ParseAdminUsers parses "user:password,user:password" (a password may
// hold ":" but not ","; blanks around the pairs are ignored).
func ParseAdminUsers(s string) (map[string]string, error) {
	users := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		u, p, ok := strings.Cut(pair, ":")
		if !ok || u == "" || p == "" {
			return nil, fmt.Errorf("%q is not a user:password pair (the value is a comma-separated list of them)", redactPair(pair))
		}
		users[u] = p
	}
	return users, nil
}

// redactPair shows the user of a malformed pair but not what may be a
// password.
func redactPair(pair string) string {
	if u, _, ok := strings.Cut(pair, ":"); ok {
		return u + ":…"
	}
	return "…"
}

// splitHostPort is net.SplitHostPort without importing net for one call.
func splitHostPort(s string) (host, port string, err error) {
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return "", "", errors.New("missing port")
	}
	host, port = s[:i], s[i+1:]
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	} else if strings.Contains(host, ":") {
		return "", "", errors.New("an IPv6 address must be in brackets")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", "", errors.New("bad port")
	}
	return host, port, nil
}

// newFlagSet makes a flag set that reports errors instead of exiting and
// prints nothing itself.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseFlags parses args into fs, then sets every flag not given from its
// environment variable.
func parseFlags(fs *flag.FlagSet, args []string, getenv func(string) string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	var errs []error
	fs.VisitAll(func(f *flag.Flag) {
		if given[f.Name] || getenv == nil {
			return
		}
		v, ok := lookupEnv(getenv, EnvName(f.Name))
		if !ok {
			return
		}
		if err := fs.Set(f.Name, v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvName(f.Name), err))
		}
	})
	return errors.Join(errs...)
}

// lookupEnv reads a variable; an empty value counts as unset.
func lookupEnv(getenv func(string) string, name string) (string, bool) {
	v := getenv(name)
	return v, v != ""
}

// listValue is a flag of strings, comma-separated, repeatable.
type listValue []string

func (l *listValue) String() string { return strings.Join(*l, ",") }

func (l *listValue) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}

// isChallengeLabel reports whether s is a label of a TXT record's name:
// lowercase letters, digits, hyphens and underscores (_ferun-challenge),
// 1 to 63 of them, not beginning or ending with a hyphen.
func isChallengeLabel(s string) bool {
	if len(s) < 1 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// addrList is a flag of IP addresses, comma-separated, repeatable.
type addrList []netip.Addr

func (l *addrList) String() string {
	out := make([]string, len(*l))
	for i, a := range *l {
		out[i] = a.String()
	}
	return strings.Join(out, ",")
}

func (l *addrList) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return err
		}
		if a.Zone() != "" {
			return fmt.Errorf("%q has a zone", s)
		}
		*l = append(*l, a.Unmap())
	}
	return nil
}

// prefixList is a flag of IP prefixes, comma-separated, repeatable.
type prefixList []netip.Prefix

// DefaultDenyPrefixes is --deny-prefix's built-in set, which a set given
// replaces: the private, shared, link-local, loopback, "this network",
// multicast and reserved IPv4 ranges. "default" in --deny-prefix stands
// for it, so that a deployment adds its own networks to it (a shoot's pod
// and service networks outside these ranges).
func DefaultDenyPrefixes() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("240.0.0.0/4"),
	}
}

func (l *prefixList) String() string {
	out := make([]string, len(*l))
	for i, p := range *l {
		out[i] = p.String()
	}
	return strings.Join(out, ",")
}

func (l *prefixList) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if s == "default" {
			*l = append(*l, DefaultDenyPrefixes()...)
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return err
		}
		*l = append(*l, p.Masked())
	}
	return nil
}

// byteSize is a flag of bytes: an integer with an optional KiB, MiB or GiB
// suffix.
type byteSize int64

func (b *byteSize) String() string { return strconv.FormatInt(int64(*b), 10) }

func (b *byteSize) Set(v string) error {
	n, err := ParseByteSize(v)
	if err != nil {
		return err
	}
	*b = byteSize(n)
	return nil
}

// ParseByteSize parses an integer number of bytes with an optional KiB,
// MiB or GiB suffix.
func ParseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	shift := 0
	for _, u := range []struct {
		suffix string
		shift  int
	}{{"KiB", 10}, {"MiB", 20}, {"GiB", 30}} {
		if v, ok := strings.CutSuffix(s, u.suffix); ok {
			s, shift = strings.TrimSpace(v), u.shift
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a byte size (an integer with an optional KiB, MiB or GiB suffix)", s)
	}
	if n < 0 || n > (1<<62)>>shift {
		return 0, fmt.Errorf("byte size %q out of range", s)
	}
	return n << shift, nil
}

// ParsePodNetwork parses a cluster's ULA as --pod-network names it: a /64
// of fd00::/8 without host bits (RFC 4193), such as "fd5e:1b2c:93a0::/64".
// A /48, as the flag named one before nodes got /96s (ADR 0017), is its
// /64 of subnet 0.
func ParsePodNetwork(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is neither empty, mesh nor a ULA /64: %w", s, err)
	}
	switch {
	case !p.Addr().Is6() || p.Addr().Is4In6() || p.Addr().Zone() != "":
		return netip.Prefix{}, fmt.Errorf("%q is not an IPv6 prefix", s)
	case p.Bits() != mesh.NetworkBits && p.Bits() != 48:
		return netip.Prefix{}, fmt.Errorf("%q is not a /64", s)
	case p.Masked() != p:
		return netip.Prefix{}, fmt.Errorf("%q has host bits set", s)
	case p.Addr().As16()[0] != 0xfd:
		return netip.Prefix{}, fmt.Errorf("%q is not a ULA of fd00::/8", s)
	}
	if p.Bits() == 48 {
		// A /48 as given before nodes got /96s: its subnet 0.
		p = mesh.FromLegacy(p)
	}
	return p, nil
}
