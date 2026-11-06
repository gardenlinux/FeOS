// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package ingress is ferun-ingress, the ingress of FeRun (ADR 0021): its
// configuration, the Program that wires the agent (package agent) to the
// data path (package proxy) and serves the HTTP, HTTPS and health
// listeners, readiness, metrics and status, and the shutdown sequence.
//
// Every flag of serve has an environment variable FERUN_INGRESS_<NAME>
// (the flag's name in upper snake case). A flag given on the command line
// wins over its variable; a variable wins over the default. Durations are
// written in Go's syntax (10s, 2m), byte sizes as integers with an
// optional KiB, MiB or GiB suffix.
package ingress

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/agent"
	"github.com/gardenlinux/feos/go/internal/ingress/proxy"
	"github.com/gardenlinux/feos/go/internal/pki"
)

// EnvPrefix starts every environment variable of a flag.
const EnvPrefix = "FERUN_INGRESS_"

// The subcommands of ferun-ingress.
const (
	CmdServe   = "serve"
	CmdPrepare = "prepare"
	CmdStatus  = "status"
	CmdVersion = "version"
)

// The log formats (--log-format).
const (
	LogFormatJSON = "json"
	LogFormatText = "text"
)

// maxDeviceName is the longest name of a network device (IFNAMSIZ less
// the terminating NUL).
const maxDeviceName = 15

// Config is the configuration of `ferun-ingress serve`: every flag of
// §7.1 of the design, typed, and the version, which is not a flag.
type Config struct {
	// Name is the instance's name, a DNS label ($(POD_NAME) on
	// Kubernetes); its identity is spiffe://<TrustDomain>/ingress/<Name>.
	Name string
	// StateDir holds the identity, the keys and the caches.
	StateDir string
	// JoinTokenFile holds a join token; it is read only while no valid
	// certificate is kept.
	JoinTokenFile string
	// ControlPlane are the control plane's addresses, host:port of its
	// nodes' port, tried in random order.
	ControlPlane []string
	// ControlPlaneName is the DNS name the control plane's certificates
	// are verified for: ferun.<domain>.
	ControlPlaneName string
	TrustDomain      string
	// SinkEndpoint is host:port of the sink's ingest (ADR 0026 §8), where
	// the access log goes; "": the access log is not kept.
	SinkEndpoint string
	// ListenHTTP, ListenHTTPS and HealthListen are the listeners: an IP
	// address and a port each, never an unspecified address (D20). Port 0
	// picks a free port (tests).
	ListenHTTP, ListenHTTPS, HealthListen string
	// TLSCert and TLSKey are the HTTPS listener's certificate (the apps
	// zone's wildcard) and key, PEM, read again every 30 s.
	TLSCert, TLSKey string
	// TenantZoneTLSCert and TenantZoneTLSKey are the tenant zone's wildcard
	// (ADR 0031) and its key, PEM, read again every 30 s: served for every
	// SNI it covers; "" both: none, every default host gets TLSCert.
	TenantZoneTLSCert, TenantZoneTLSKey string
	// MeshDevice is the WireGuard device's name.
	MeshDevice string
	// MeshPort is its UDP port.
	MeshPort int
	// MeshEndpoint is where the nodes reach the device: the pod's IP
	// address and MeshPort.
	MeshEndpoint string
	// MeshMTU is the device's MTU; 0 computes it from the interface that
	// holds MeshEndpoint's address.
	MeshMTU int
	// WakeHold bounds a request held while instances of its service are
	// asleep or waking; PendingHold one held while they are only pending.
	WakeHold, PendingHold time.Duration
	// MaxHeld is how many requests may be held at once, MaxHeldPerTenant
	// how many of them one tenant's, MaxHeldPerEnv how many one
	// environment's.
	MaxHeld, MaxHeldPerTenant, MaxHeldPerEnv int
	// MaxInflight is how many requests may be forwarded at once, waiting
	// for a connection or an answer, upgraded ones included;
	// MaxInflightPerTenant how many of them one tenant's,
	// MaxInflightPerEnv how many one environment's.
	MaxInflight, MaxInflightPerTenant, MaxInflightPerEnv int
	// UpstreamDialTimeout bounds a dial to an instance;
	// UpstreamIdleTimeout closes a pooled connection idle that long.
	UpstreamDialTimeout, UpstreamIdleTimeout time.Duration
	// UpstreamWaitTimeout bounds each wait for a connection to an
	// instance, the dial included.
	UpstreamWaitTimeout time.Duration
	// MaxRequestBody is the largest request body in bytes; 0 means no
	// limit.
	MaxRequestBody int64
	// MaxConnsPerEndpoint bounds the connections to one instance's port.
	MaxConnsPerEndpoint int
	// MemoryLimit is the container's memory limit in bytes; the Go
	// runtime's soft limit is set to 90% of it unless GOMEMLIMIT is set.
	// 0: none.
	MemoryLimit int64
	// ShutdownDelay is how long a stopping instance serves on with
	// /readyz failing, so that the balancers take it out first;
	// ShutdownGrace how long it then waits for its requests.
	ShutdownDelay, ShutdownGrace time.Duration
	LogLevel                     slog.Level
	// LogFormat is LogFormatJSON or LogFormatText.
	LogFormat string

	// Version is the build's version, sent in Hello and shown in the
	// status. Not a flag.
	Version string
	// meshRepairEvery is the agent's Config.MeshRepairEvery: 0, its
	// default, but in tests. Not a flag.
	meshRepairEvery time.Duration
}

// Default returns the configuration of serve with every default of §7.1
// and none of the required settings.
func Default() Config {
	return Config{
		StateDir:             "/var/lib/ferun-ingress",
		MeshDevice:           agent.DefaultMeshDevice,
		MeshPort:             agent.DefaultMeshPort,
		WakeHold:             proxy.DefaultWakeHold,
		PendingHold:          proxy.DefaultPendingHold,
		MaxHeld:              proxy.DefaultMaxHeld,
		MaxHeldPerTenant:     proxy.DefaultMaxHeldPerTenant,
		MaxHeldPerEnv:        proxy.DefaultMaxHeldPerEnv,
		MaxInflight:          proxy.DefaultMaxInflight,
		MaxInflightPerTenant: proxy.DefaultMaxInflightPerTenant,
		MaxInflightPerEnv:    proxy.DefaultMaxInflightPerEnv,
		UpstreamDialTimeout:  proxy.DefaultDialTimeout,
		UpstreamIdleTimeout:  proxy.DefaultIdleTimeout,
		UpstreamWaitTimeout:  proxy.DefaultConnWaitTimeout,
		MaxRequestBody:       proxy.DefaultMaxBody,
		MaxConnsPerEndpoint:  proxy.DefaultMaxConnsPerEndpoint,
		ShutdownDelay:        10 * time.Second,
		ShutdownGrace:        40 * time.Second,
		LogLevel:             slog.LevelInfo,
		LogFormat:            LogFormatJSON,
	}
}

// EnvName is the environment variable of a flag: FERUN_INGRESS_ and the
// flag's name in upper snake case.
func EnvName(flagName string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// Parse reads the flags of serve from args and, for the flags not given,
// their environment variables through getenv, over Default, and validates
// the result. --help yields flag.ErrHelp (Usage prints the help).
func Parse(args []string, getenv func(string) string) (Config, error) {
	c := Default()
	fs := newFlagSet(CmdServe)
	logLevel := registerServe(fs, &c)
	if err := parseFlags(fs, args, getenv); err != nil {
		return Config{}, err
	}
	if err := c.LogLevel.UnmarshalText([]byte(*logLevel)); err != nil {
		return Config{}, fmt.Errorf("--log-level: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// registerServe defines serve's flags on fs, writing into c; it returns
// the text of --log-level.
func registerServe(fs *flag.FlagSet, c *Config) *string {
	fs.StringVar(&c.Name, "name", c.Name, "the instance's name, a DNS label: $(POD_NAME) on Kubernetes (required)")
	fs.StringVar(&c.StateDir, "state-dir", c.StateDir, "the directory of the identity, the keys and the caches (kept across restarts)")
	fs.StringVar(&c.JoinTokenFile, "join-token-file", c.JoinTokenFile,
		"a file holding a join token, read only while no valid certificate is kept")
	fs.Var((*addrList)(&c.ControlPlane), "control-plane",
		"the control plane's addresses (host:port of its nodes' port), comma-separated, tried in random order (required)")
	fs.StringVar(&c.ControlPlaneName, "control-plane-name", c.ControlPlaneName,
		"the DNS name the control plane's certificates are verified for: ferun.<domain> (required)")
	fs.StringVar(&c.TrustDomain, "trust-domain", c.TrustDomain, "the SPIFFE trust domain (required)")
	fs.StringVar(&c.SinkEndpoint, "sink-endpoint", c.SinkEndpoint,
		"host:port of the sink's ingest, where the access log goes (a name is resolved, any of its addresses serves); empty: the access log is not kept")
	fs.StringVar(&c.ListenHTTP, "listen-http", c.ListenHTTP,
		"the HTTP listener (redirects to https), an IP address and a port: the pod's IP, never a wildcard (required)")
	fs.StringVar(&c.ListenHTTPS, "listen-https", c.ListenHTTPS,
		"the HTTPS listener, an IP address and a port: the pod's IP, never a wildcard (required)")
	fs.StringVar(&c.HealthListen, "health-listen", c.HealthListen,
		"the listener of /healthz, /readyz, /metrics and /statusz, an IP address and a port, never a wildcard (required)")
	fs.StringVar(&c.TLSCert, "tls-cert", c.TLSCert, "the HTTPS certificate file (PEM), the apps zone's wildcard, read again every 30 s (required)")
	fs.StringVar(&c.TLSKey, "tls-key", c.TLSKey, "the key file (PEM) of --tls-cert (required)")
	fs.StringVar(&c.TenantZoneTLSCert, "tenant-zone-tls-cert", c.TenantZoneTLSCert,
		"the tenant zone's wildcard certificate file (PEM), served for every name it covers, read again every 30 s; empty: none")
	fs.StringVar(&c.TenantZoneTLSKey, "tenant-zone-tls-key", c.TenantZoneTLSKey, "the key file (PEM) of --tenant-zone-tls-cert")
	fs.StringVar(&c.MeshDevice, "mesh-device", c.MeshDevice, "the WireGuard device on the pods' mesh")
	fs.IntVar(&c.MeshPort, "mesh-port", c.MeshPort, "the WireGuard device's UDP port")
	fs.StringVar(&c.MeshEndpoint, "mesh-endpoint", c.MeshEndpoint,
		"where the nodes reach the WireGuard device: $(POD_IP):51820 on Kubernetes (required)")
	fs.IntVar(&c.MeshMTU, "mesh-mtu", c.MeshMTU, "the WireGuard device's MTU; 0 computes it from the interface of --mesh-endpoint")
	fs.DurationVar(&c.WakeHold, "wake-hold", c.WakeHold, "how long a request waits while instances of its service are asleep or waking")
	fs.DurationVar(&c.PendingHold, "pending-hold", c.PendingHold, "how long a request waits while instances of its service are only pending")
	fs.IntVar(&c.MaxHeld, "max-held", c.MaxHeld, "how many requests may wait at once")
	fs.IntVar(&c.MaxHeldPerTenant, "max-held-per-tenant", c.MaxHeldPerTenant, "how many requests of one tenant may wait at once")
	fs.IntVar(&c.MaxHeldPerEnv, "max-held-per-env", c.MaxHeldPerEnv, "how many requests of one environment may wait at once")
	fs.IntVar(&c.MaxInflight, "max-inflight", c.MaxInflight,
		"how many requests may be forwarded at once, waiting for a connection or an answer, upgraded ones included")
	fs.IntVar(&c.MaxInflightPerTenant, "max-inflight-per-tenant", c.MaxInflightPerTenant,
		"how many requests of one tenant may be forwarded at once")
	fs.IntVar(&c.MaxInflightPerEnv, "max-inflight-per-env", c.MaxInflightPerEnv,
		"how many requests of one environment may be forwarded at once")
	fs.DurationVar(&c.UpstreamDialTimeout, "upstream-dial-timeout", c.UpstreamDialTimeout, "the bound of a dial to an instance")
	fs.DurationVar(&c.UpstreamIdleTimeout, "upstream-idle-timeout", c.UpstreamIdleTimeout,
		"a pooled connection to an instance idle this long is closed; keep it below the services' keep-alive timeouts")
	fs.DurationVar(&c.UpstreamWaitTimeout, "upstream-wait-timeout", c.UpstreamWaitTimeout,
		"the bound of the wait for a connection to an instance, the dial included; at least --upstream-dial-timeout")
	fs.Var((*byteSize)(&c.MaxRequestBody), "max-request-body", "the largest request body (bytes, or with a KiB, MiB or GiB suffix); 0: no limit")
	fs.IntVar(&c.MaxConnsPerEndpoint, "max-conns-per-endpoint", c.MaxConnsPerEndpoint, "the most connections to one instance's port")
	fs.Var((*byteSize)(&c.MemoryLimit), "memory-limit",
		"the container's memory limit (bytes, or with a KiB, MiB or GiB suffix); the Go runtime's soft limit is 90% of it unless GOMEMLIMIT is set; 0: none")
	fs.DurationVar(&c.ShutdownDelay, "shutdown-delay", c.ShutdownDelay,
		"how long a stopping instance serves on with /readyz failing before it stops accepting connections")
	fs.DurationVar(&c.ShutdownGrace, "shutdown-grace", c.ShutdownGrace,
		"how long a stopping instance then waits for its requests; set the pod's termination grace above --shutdown-delay plus this plus 5s")
	level := strings.ToLower(c.LogLevel.String())
	fs.StringVar(&level, "log-level", level, "debug, info, warn or error")
	fs.StringVar(&c.LogFormat, "log-format", c.LogFormat, "json or text")
	return &level
}

// Validate checks a serve configuration: what Parse checks after reading
// the flags, and what a program built in process must satisfy.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if c.Name == "" {
		bad("--name is required")
	} else if !pki.IsNodeName(c.Name) {
		bad("--name %q is not a DNS label", c.Name)
	}
	if c.StateDir == "" {
		bad("--state-dir must not be empty")
	}
	if len(c.ControlPlane) == 0 {
		bad("--control-plane is required")
	}
	for _, a := range c.ControlPlane {
		if err := checkHostPort(a); err != nil {
			bad("--control-plane: %w", err)
		}
	}
	if c.ControlPlaneName == "" {
		bad("--control-plane-name is required")
	} else if err := pki.ValidateDomain(c.ControlPlaneName); err != nil {
		bad("--control-plane-name: %w", err)
	}
	if c.TrustDomain == "" {
		bad("--trust-domain is required")
	} else if err := pki.ValidateTrustDomain(c.TrustDomain); err != nil {
		bad("--trust-domain: %w", err)
	}
	if c.SinkEndpoint != "" {
		if err := checkHostPort(c.SinkEndpoint); err != nil {
			bad("--sink-endpoint: %w", err)
		}
	}
	for _, l := range []struct{ name, v string }{
		{"listen-http", c.ListenHTTP}, {"listen-https", c.ListenHTTPS}, {"health-listen", c.HealthListen},
	} {
		if l.v == "" {
			bad("--%s is required", l.name)
		} else if err := checkListen(l.v); err != nil {
			bad("--%s: %w", l.name, err)
		}
	}
	if c.TLSCert == "" {
		bad("--tls-cert is required")
	}
	if c.TLSKey == "" {
		bad("--tls-key is required")
	}
	if (c.TenantZoneTLSCert == "") != (c.TenantZoneTLSKey == "") {
		bad("--tenant-zone-tls-cert and --tenant-zone-tls-key go together")
	}
	if c.MeshDevice == "" || len(c.MeshDevice) > maxDeviceName || strings.ContainsAny(c.MeshDevice, "/: \t\n") {
		bad("--mesh-device %q is not a device name (1 to %d characters, no slash, colon or blank)", c.MeshDevice, maxDeviceName)
	}
	if c.MeshPort < 1 || c.MeshPort > 65535 {
		bad("--mesh-port %d is out of range", c.MeshPort)
	}
	if c.MeshEndpoint == "" {
		bad("--mesh-endpoint is required")
	} else if ap, err := netip.ParseAddrPort(c.MeshEndpoint); err != nil || ap.Port() == 0 || ap.Addr().IsUnspecified() {
		bad("--mesh-endpoint %q is not an IP address and a port", c.MeshEndpoint)
	}
	if c.MeshMTU != 0 && (c.MeshMTU < 1280 || c.MeshMTU > 65535) {
		bad("--mesh-mtu must be 0 (computed) or between 1280 and 65535")
	}
	for _, n := range []struct {
		name string
		v    int
	}{
		{"max-held", c.MaxHeld}, {"max-held-per-tenant", c.MaxHeldPerTenant}, {"max-held-per-env", c.MaxHeldPerEnv},
		{"max-inflight", c.MaxInflight}, {"max-inflight-per-tenant", c.MaxInflightPerTenant},
		{"max-inflight-per-env", c.MaxInflightPerEnv},
		{"max-conns-per-endpoint", c.MaxConnsPerEndpoint},
	} {
		if n.v < 1 {
			bad("--%s must be at least 1", n.name)
		}
	}
	if c.MaxRequestBody < 0 {
		bad("--max-request-body must not be negative")
	}
	if c.MemoryLimit < 0 {
		bad("--memory-limit must not be negative")
	}
	if c.UpstreamWaitTimeout > 0 && c.UpstreamWaitTimeout < c.UpstreamDialTimeout {
		bad("--upstream-wait-timeout must not be below --upstream-dial-timeout")
	}
	for _, d := range []struct {
		name string
		v    time.Duration
	}{
		{"wake-hold", c.WakeHold}, {"pending-hold", c.PendingHold},
		{"upstream-dial-timeout", c.UpstreamDialTimeout}, {"upstream-idle-timeout", c.UpstreamIdleTimeout},
		{"upstream-wait-timeout", c.UpstreamWaitTimeout},
		{"shutdown-delay", c.ShutdownDelay}, {"shutdown-grace", c.ShutdownGrace},
	} {
		if d.v <= 0 {
			bad("--%s must be positive", d.name)
		}
	}
	if c.LogFormat != LogFormatJSON && c.LogFormat != LogFormatText {
		bad("--log-format must be %s or %s, not %q", LogFormatJSON, LogFormatText, c.LogFormat)
	}
	return errors.Join(errs...)
}

// checkListen accepts an IP address and a port, the address not
// unspecified: a wildcard would bind the mesh device's address as well,
// and the mesh must not reach a listener (D20). Loopback is allowed.
func checkListen(s string) error {
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return fmt.Errorf("%q is not an IP address and a port (an IPv6 address in brackets)", s)
	}
	if ap.Addr().IsUnspecified() {
		return fmt.Errorf("%q binds every address; bind the pod's IP, never a wildcard: the mesh device must not reach a listener", s)
	}
	return nil
}

// checkHostPort accepts host:port with a host and a port from 1 to 65535.
func checkHostPort(s string) error {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return fmt.Errorf("%q is not host:port: %w", s, err)
	}
	if host == "" {
		return fmt.Errorf("%q has no host", s)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q has no port from 1 to 65535", s)
	}
	return nil
}

// StatusConfig is the configuration of `ferun-ingress status`.
type StatusConfig struct {
	// HealthListen is the health listener of the running instance (serve's
	// --health-listen, so FERUN_INGRESS_HEALTH_LISTEN serves both).
	HealthListen string
	// Timeout bounds the request.
	Timeout time.Duration
}

// ParseStatus reads the flags of status and their variables.
func ParseStatus(args []string, getenv func(string) string) (StatusConfig, error) {
	c := StatusConfig{Timeout: 5 * time.Second}
	fs := newFlagSet(CmdStatus)
	registerStatus(fs, &c)
	if err := parseFlags(fs, args, getenv); err != nil {
		return StatusConfig{}, err
	}
	var errs []error
	if c.HealthListen == "" {
		errs = append(errs, errors.New("--health-listen is required (or "+EnvName("health-listen")+")"))
	} else if err := checkHostPort(c.HealthListen); err != nil {
		errs = append(errs, fmt.Errorf("--health-listen: %w", err))
	}
	if c.Timeout <= 0 {
		errs = append(errs, errors.New("--timeout must be positive"))
	}
	return c, errors.Join(errs...)
}

func registerStatus(fs *flag.FlagSet, c *StatusConfig) {
	fs.StringVar(&c.HealthListen, "health-listen", c.HealthListen, "the running instance's health listener (serve's --health-listen)")
	fs.DurationVar(&c.Timeout, "timeout", c.Timeout, "the bound of the request")
}

// PrepareConfig is the configuration of `ferun-ingress prepare`.
type PrepareConfig struct {
	LogLevel  slog.Level
	LogFormat string
	// JoinTokenSource, when set, is the instance's own join token in the
	// Secret of every instance's tokens (/join-secret/$(POD_NAME)); it is
	// copied to JoinTokenFile, the file serve reads, in a volume of the pod
	// that serve mounts in place of the Secret, so that no instance can
	// read another's token.
	JoinTokenSource string
	JoinTokenFile   string
}

// ParsePrepare reads the flags of prepare and their variables: serve's
// log flags, --join-token-source and serve's --join-token-file.
func ParsePrepare(args []string, getenv func(string) string) (PrepareConfig, error) {
	d := Default()
	c := PrepareConfig{LogLevel: d.LogLevel, LogFormat: d.LogFormat}
	fs := newFlagSet(CmdPrepare)
	level := registerPrepare(fs, &c)
	if err := parseFlags(fs, args, getenv); err != nil {
		return PrepareConfig{}, err
	}
	if err := c.LogLevel.UnmarshalText([]byte(*level)); err != nil {
		return PrepareConfig{}, fmt.Errorf("--log-level: %w", err)
	}
	if c.LogFormat != LogFormatJSON && c.LogFormat != LogFormatText {
		return PrepareConfig{}, fmt.Errorf("--log-format must be %s or %s, not %q", LogFormatJSON, LogFormatText, c.LogFormat)
	}
	if c.JoinTokenSource != "" && c.JoinTokenFile == "" {
		return PrepareConfig{}, errors.New("--join-token-source needs --join-token-file, where it is copied to")
	}
	return c, nil
}

func registerPrepare(fs *flag.FlagSet, c *PrepareConfig) *string {
	level := strings.ToLower(c.LogLevel.String())
	fs.StringVar(&level, "log-level", level, "debug, info, warn or error")
	fs.StringVar(&c.LogFormat, "log-format", c.LogFormat, "json or text")
	fs.StringVar(&c.JoinTokenSource, "join-token-source", c.JoinTokenSource,
		"the instance's own join token in the Secret of every instance's; copied to --join-token-file (empty: nothing copied)")
	fs.StringVar(&c.JoinTokenFile, "join-token-file", c.JoinTokenFile, "where --join-token-source is copied to: serve's --join-token-file")
	return &level
}

// NewLogger returns the logger of a log format and level, writing to w.
func NewLogger(w io.Writer, format string, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == LogFormatText {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// Usage prints the help of a subcommand, or the overview for "".
func Usage(w io.Writer, cmd string) {
	var fs *flag.FlagSet
	switch cmd {
	case CmdServe:
		fs = newFlagSet(CmdServe)
		c := Default()
		registerServe(fs, &c)
		fmt.Fprintln(w, "Usage: ferun-ingress [serve] [flags]\n\nRuns the ingress until SIGTERM or SIGINT. Every flag has an environment variable "+
			EnvPrefix+"<NAME>.\n\nFlags:")
	case CmdPrepare:
		fs = newFlagSet(CmdPrepare)
		var c PrepareConfig
		registerPrepare(fs, &c)
		fmt.Fprintln(w, "Usage: ferun-ingress prepare [flags]\n\nEnables IPv6 in the network namespace and copies the instance's own join token (an init\ncontainer, as root).\n\nFlags:")
	case CmdStatus:
		fs = newFlagSet(CmdStatus)
		c := StatusConfig{Timeout: 5 * time.Second}
		registerStatus(fs, &c)
		fmt.Fprintln(w, "Usage: ferun-ingress status [flags]\n\nPrints the running instance's /statusz; exits 1 when it cannot be read.\n\nFlags:")
	default:
		fmt.Fprint(w, `Usage: ferun-ingress <command> [flags]

Commands:
  serve     run the ingress (the default)
  prepare   enable IPv6 and copy the instance's join token (an init container, as root)
  status    print the running instance's /statusz
  version   print the version

Run ferun-ingress help <command> for a command's flags.
`)
		return
	}
	fs.SetOutput(w)
	fs.PrintDefaults()
}

// newFlagSet makes a flag set that reports errors instead of exiting and
// prints nothing itself.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseFlags parses args into fs, then sets every flag not given from its
// environment variable; an empty variable counts as unset.
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
		v := getenv(EnvName(f.Name))
		if v == "" {
			return
		}
		if err := fs.Set(f.Name, v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvName(f.Name), err))
		}
	})
	return errors.Join(errs...)
}

// addrList is a flag of addresses, comma-separated; a later value
// replaces an earlier one, so the variable never adds to the flag.
// Blanks around the entries and empty entries are ignored.
type addrList []string

func (l *addrList) String() string { return strings.Join(*l, ",") }

func (l *addrList) Set(v string) error {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if err := checkHostPort(s); err != nil {
			return err
		}
		out = append(out, s)
	}
	*l = out
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
// MiB or GiB suffix, such as 64MiB.
func ParseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	num, shift := s, 0
	for _, u := range []struct {
		suffix string
		shift  int
	}{{"KiB", 10}, {"MiB", 20}, {"GiB", 30}} {
		if v, ok := strings.CutSuffix(s, u.suffix); ok {
			num, shift = strings.TrimSpace(v), u.shift
			break
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a byte size (an integer with an optional KiB, MiB or GiB suffix)", s)
	}
	if n < 0 || n > (1<<62)>>shift {
		return 0, fmt.Errorf("byte size %q out of range", s)
	}
	return n << shift, nil
}
