// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
)

// The subcommands of ferund.
const (
	CmdServe        = "serve"
	CmdInitCA       = "init-ca"
	CmdNewIssuer    = "new-issuer"
	CmdJoinToken    = "join-token"
	CmdServiceToken = "service-token"
	CmdMigrate      = "migrate"
)

// InitCA is the configuration of `ferund init-ca`: the platform CA's root
// (key in RootDir) and its issuer (in Dir).
type InitCA struct {
	RootDir, Dir, TrustDomain, ControlPlaneDomain string
	DNSDomains                                    []string
}

// ParseInitCA reads the flags of init-ca (and their variables).
func ParseInitCA(args []string, getenv func(string) string) (InitCA, error) {
	return parseCA(CmdInitCA, args, getenv)
}

// ParseNewIssuer reads the flags of new-issuer, init-ca's: the root's
// directory is read instead of written.
func ParseNewIssuer(args []string, getenv func(string) string) (InitCA, error) {
	return parseCA(CmdNewIssuer, args, getenv)
}

func parseCA(cmd string, args []string, getenv func(string) string) (InitCA, error) {
	c := InitCA{TrustDomain: Default().TrustDomain}
	fs := newFlagSet(cmd)
	registerCA(fs, cmd, &c)
	if err := parseFlags(fs, args, getenv); err != nil {
		return InitCA{}, err
	}
	var errs []error
	if c.RootDir == "" {
		errs = append(errs, errors.New("--root-dir is required"))
	}
	if c.Dir == "" {
		errs = append(errs, errors.New("--dir is required"))
	}
	if len(c.DNSDomains) == 0 {
		errs = append(errs, errors.New("--dns-domain is required"))
	}
	if err := pki.ValidateTrustDomain(c.TrustDomain); err != nil {
		errs = append(errs, fmt.Errorf("--trust-domain: %w", err))
	}
	return c, errors.Join(errs...)
}

func registerCA(fs *flag.FlagSet, cmd string, c *InitCA) {
	root := "directory for the root's certificate and key (keep it offline; required)"
	if cmd == CmdNewIssuer {
		root = "directory holding the root's certificate and key, root.crt and root.key (keep it offline; required)"
	}
	fs.StringVar(&c.RootDir, "root-dir", c.RootDir, root)
	fs.StringVar(&c.Dir, "dir", c.Dir, "directory for the issuer, issuer.json and bundle.crt: an instance's --ca-dir (required)")
	fs.StringVar(&c.TrustDomain, "trust-domain", c.TrustDomain, "SPIFFE trust domain")
	fs.Var((*listValue)(&c.DNSDomains), "dns-domain", "the platform's DNS domain the CA may issue under, repeatable (required)")
	fs.StringVar(&c.ControlPlaneDomain, "control-plane-domain", c.ControlPlaneDomain,
		"the instances' DNS domain on the nodes' port (default ferun.<first --dns-domain>)")
}

// JoinToken is the configuration of `ferund join-token`.
type JoinToken struct {
	DatabaseURL, CADir, TrustDomain string
	// TTL is the token's lifetime; 0 means JoinTokenTTL.
	TTL time.Duration
	// Kind is what the token joins: model.JoinKindNode (the default) or
	// model.JoinKindIngress, an instance of ferun-ingress.
	Kind string
	// Node pins the token to a name, a node's or, of kind ingress, an
	// ingress instance's; "" lets it create a new name only.
	Node     string
	LogLevel slog.Level
}

// ParseJoinToken reads the flags of join-token, with serve's variable
// names and defaults for the flags they share.
func ParseJoinToken(args []string, getenv func(string) string) (JoinToken, error) {
	d := Default()
	c := JoinToken{CADir: d.CADir, TrustDomain: d.TrustDomain, TTL: JoinTokenTTL, Kind: model.JoinKindNode, LogLevel: slog.LevelWarn}
	fs := newFlagSet(CmdJoinToken)
	level := registerJoinToken(fs, &c)
	if err := parseFlags(fs, args, getenv); err != nil {
		return JoinToken{}, err
	}
	var errs []error
	if err := c.LogLevel.UnmarshalText([]byte(*level)); err != nil {
		errs = append(errs, fmt.Errorf("--log-level: %w", err))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("--database-url is required"))
	}
	if c.CADir == "" {
		errs = append(errs, errors.New("--ca-dir must not be empty"))
	}
	if err := pki.ValidateTrustDomain(c.TrustDomain); err != nil {
		errs = append(errs, fmt.Errorf("--trust-domain: %w", err))
	}
	if c.TTL <= 0 {
		errs = append(errs, errors.New("--ttl must be positive"))
	}
	if c.Kind != model.JoinKindNode && c.Kind != model.JoinKindIngress {
		errs = append(errs, fmt.Errorf("--kind %q: %s or %s", c.Kind, model.JoinKindNode, model.JoinKindIngress))
	}
	if c.Node != "" && !pki.IsNodeName(c.Node) {
		errs = append(errs, fmt.Errorf("--node %q is not a name (a DNS label)", c.Node))
	}
	return c, errors.Join(errs...)
}

func registerJoinToken(fs *flag.FlagSet, c *JoinToken) *string {
	level := strings.ToLower(c.LogLevel.String())
	fs.StringVar(&c.DatabaseURL, "database-url", c.DatabaseURL, "PostgreSQL DSN (required)")
	fs.StringVar(&c.CADir, "ca-dir", c.CADir, "directory of the issuer (for the token's pin)")
	fs.StringVar(&c.TrustDomain, "trust-domain", c.TrustDomain, "SPIFFE trust domain")
	fs.DurationVar(&c.TTL, "ttl", c.TTL, "the token's lifetime")
	fs.StringVar(&c.Kind, "kind", c.Kind, "what the token joins: node, or ingress (an instance of ferun-ingress)")
	fs.StringVar(&c.Node, "node", c.Node, "pin the token to this name, a node's or, with --kind ingress, an ingress instance's (a re-join of that name); empty: the joiner proposes a new name")
	fs.StringVar(&level, "log-level", level, "debug, info, warn or error")
	return &level
}

// ServiceToken is the configuration of `ferund service-token` (ADR 0026).
type ServiceToken struct {
	DatabaseURL string
	// Kind is the service: model.ServiceSink, an instance of ferun-sink,
	// or model.ServiceAuth, ferun-auth (ADR 0054), which holds no
	// certificate.
	Kind string
	// Name is the holder's name, a sink's certificate's
	// spiffe://<td>/<kind>/<name>.
	Name     string
	LogLevel slog.Level
}

// ParseServiceToken reads the flags of service-token.
func ParseServiceToken(args []string, getenv func(string) string) (ServiceToken, error) {
	c := ServiceToken{Kind: model.ServiceSink, LogLevel: slog.LevelWarn}
	fs := newFlagSet(CmdServiceToken)
	level := registerServiceToken(fs, &c)
	if err := parseFlags(fs, args, getenv); err != nil {
		return ServiceToken{}, err
	}
	var errs []error
	if err := c.LogLevel.UnmarshalText([]byte(*level)); err != nil {
		errs = append(errs, fmt.Errorf("--log-level: %w", err))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("--database-url is required"))
	}
	if c.Kind != model.ServiceSink && c.Kind != model.ServiceAuth {
		errs = append(errs, fmt.Errorf("--kind %q: %s or %s", c.Kind, model.ServiceSink, model.ServiceAuth))
	}
	if !pki.IsNodeName(c.Name) {
		errs = append(errs, fmt.Errorf("--name %q is not a name (a DNS label; required)", c.Name))
	}
	return c, errors.Join(errs...)
}

func registerServiceToken(fs *flag.FlagSet, c *ServiceToken) *string {
	level := strings.ToLower(c.LogLevel.String())
	fs.StringVar(&c.DatabaseURL, "database-url", c.DatabaseURL, "PostgreSQL DSN (required)")
	fs.StringVar(&c.Kind, "kind", c.Kind, "the service: sink (an instance of ferun-sink), or auth (ferun-auth, ADR 0054)")
	fs.StringVar(&c.Name, "name", c.Name, "the holder's name, a DNS label: a sink's certificate names spiffe://<trust domain>/<kind>/<name> (required)")
	fs.StringVar(&level, "log-level", level, "debug, info, warn or error")
	return &level
}

// Migrate is the configuration of `ferund migrate`.
type Migrate struct {
	DatabaseURL string
	LogLevel    slog.Level
}

// ParseMigrate reads the flags of migrate.
func ParseMigrate(args []string, getenv func(string) string) (Migrate, error) {
	c := Migrate{LogLevel: slog.LevelInfo}
	fs := newFlagSet(CmdMigrate)
	level := registerMigrate(fs, &c)
	if err := parseFlags(fs, args, getenv); err != nil {
		return Migrate{}, err
	}
	var errs []error
	if err := c.LogLevel.UnmarshalText([]byte(*level)); err != nil {
		errs = append(errs, fmt.Errorf("--log-level: %w", err))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("--database-url is required"))
	}
	return c, errors.Join(errs...)
}

func registerMigrate(fs *flag.FlagSet, c *Migrate) *string {
	level := strings.ToLower(c.LogLevel.String())
	fs.StringVar(&c.DatabaseURL, "database-url", c.DatabaseURL, "PostgreSQL DSN (required)")
	fs.StringVar(&level, "log-level", level, "debug, info, warn or error")
	return &level
}

// Usage writes the help of a subcommand ("" or "ferund": the overview).
func Usage(w io.Writer, cmd string) {
	var fs *flag.FlagSet
	switch cmd {
	case CmdServe:
		fs = newFlagSet(cmd)
		c := Default()
		registerServe(fs, &c)
		fmt.Fprintln(w, "Usage: ferund [serve] [flags]\n\nRuns an instance of the control plane: the front doors (REST, MCP, the UI, the\nports) and the nodes' port, on the shared PostgreSQL database.")
	case CmdInitCA:
		fs = newFlagSet(cmd)
		registerCA(fs, cmd, &InitCA{TrustDomain: Default().TrustDomain})
		fmt.Fprintln(w, "Usage: ferund init-ca --root-dir R --dir D --dns-domain X [--dns-domain Y ...] [flags]\n\nMakes the platform CA: the root (its key stays in R, offline) and the issuer\nevery instance holds in its --ca-dir (D). Existing files are never overwritten.")
	case CmdNewIssuer:
		fs = newFlagSet(cmd)
		registerCA(fs, cmd, &InitCA{TrustDomain: Default().TrustDomain})
		fmt.Fprintln(w, "Usage: ferund new-issuer --root-dir R --dir D --dns-domain X [--dns-domain Y ...] [flags]\n\nMakes a new issuer under the root in R (root.crt, root.key) into D, an instance's\n--ca-dir: issuer.crt, issuer.key, issuer.json and bundle.crt, without a URI\nconstraint (ADR 0024). The root and the join tokens' pin stay. Existing files are\nnever overwritten.")
	case CmdJoinToken:
		fs = newFlagSet(cmd)
		registerJoinToken(fs, &JoinToken{CADir: Default().CADir, TrustDomain: Default().TrustDomain, TTL: JoinTokenTTL,
			Kind: model.JoinKindNode, LogLevel: slog.LevelWarn})
		fmt.Fprintln(w, "Usage: ferund join-token --database-url URL [--kind node|ingress] [flags]\n\nIssues a one-time join token for a node or an instance of ferun-ingress and prints it:\nfeos-join-<secret>.<pin>, or feos-ingress-join-<secret>.<pin> for an instance of ferun-ingress.")
	case CmdServiceToken:
		fs = newFlagSet(cmd)
		registerServiceToken(fs, &ServiceToken{Kind: model.ServiceSink, LogLevel: slog.LevelWarn})
		fmt.Fprintln(w, "Usage: ferund service-token --database-url URL --kind sink|auth --name NAME\n\nIssues a long-lived service token and prints it once: feos-sink-<secret>, which an\ninstance of ferun-sink trades for its certificates (ADR 0026), or feos-auth-<secret>,\nwith which ferun-auth answers the challenges of visitors (ADR 0054). Revoke it with\nDELETE /api/v1/admin/service-tokens/{id}.")
	case CmdMigrate:
		fs = newFlagSet(cmd)
		registerMigrate(fs, &Migrate{LogLevel: slog.LevelInfo})
		fmt.Fprintln(w, "Usage: ferund migrate --database-url URL\n\nApplies the database migrations and exits.")
	default:
		fmt.Fprintln(w, `Usage: ferund <command> [flags]

FeRun, the control plane of FeOS (ADR 0015).

Commands:
  serve          run an instance (the default when no command is given)
  init-ca        make the platform CA's root and issuer
  new-issuer     make a new issuer under the platform CA's root
  join-token     issue a join token for a node or an ingress instance
  service-token  issue a service token for an instance of ferun-sink or ferun-auth
  migrate        apply the database migrations and exit

Every flag has an environment variable FERUN_<NAME>, the flag's name in upper
snake case (--database-url: FERUN_DATABASE_URL); a flag wins over its variable.
Run "ferund <command> --help" for a command's flags.`)
		return
	}
	fmt.Fprintln(w, "\nFlags:")
	fs.VisitAll(func(f *flag.Flag) {
		def := f.DefValue
		if def != "" && def != "false" && def != "0" && def != "0s" {
			fmt.Fprintf(w, "  --%s (%s, default %q)\n", f.Name, EnvName(f.Name), def)
		} else {
			fmt.Fprintf(w, "  --%s (%s)\n", f.Name, EnvName(f.Name))
		}
		fmt.Fprintf(w, "        %s\n", f.Usage)
	})
}
