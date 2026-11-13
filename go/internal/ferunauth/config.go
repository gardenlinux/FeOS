// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ferunauth

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/egress"
)

// EnvPrefix starts every environment variable of a flag.
const EnvPrefix = "FERUN_AUTH_"

// The log formats (--log-format).
const (
	LogFormatJSON = "json"
	LogFormatText = "text"
)

// Config is the configuration of `ferun-auth serve`: its flags, and the
// fields tests set, which are not flags.
type Config struct {
	// Listen is the address of the sign-in, plain HTTP behind the
	// platform's Gateway; HealthListen that of /healthz and /readyz.
	Listen, HealthListen string
	// PublicURL is ferun-auth's own base URL, https://host without a
	// path: the redirect URIs <PublicURL>/callback/ferun and
	// <PublicURL>/callback/<tenant>/<provider> and the back-channel URIs
	// derive from it.
	PublicURL string
	// FerunURL is ferund's API, inside the cluster.
	FerunURL string
	// TokenFile holds the service token of kind auth.
	TokenFile string
	// KeyFile holds the 32 bytes that seal a sign-in in flight.
	KeyFile string
	// Issuer is the installation's issuer for kind ferun, exactly as
	// ferund's --oidc-issuer; "": kind ferun is refused. ClientID and
	// ClientSecretFile are ferun-auth's client there.
	Issuer, ClientID, ClientSecretFile string
	Scopes                             []string
	// Deny and Allow are the blocks no issuer fetch may reach, and the
	// operator's exceptions; Deny nil: egress.DefaultDeny().
	Deny, Allow []netip.Prefix
	LogLevel    slog.Level
	LogFormat   string
	// ShutdownDelay is how long /readyz answers 503 before the listener
	// closes.
	ShutdownDelay time.Duration

	// roots are the issuers' roots (nil: the system's); only tests set
	// them.
	roots *x509.CertPool
	// now is the clock of the flows; nil: time.Now (tests set it).
	now func() time.Time
}

// Default returns the defaults of serve, without the required settings.
func Default() Config {
	return Config{
		Listen:        ":8080",
		HealthListen:  ":9090",
		Scopes:        []string{"openid", "profile", "email"},
		LogLevel:      slog.LevelInfo,
		LogFormat:     LogFormatJSON,
		ShutdownDelay: 5 * time.Second,
	}
}

// EnvName is the environment variable of a flag.
func EnvName(flagName string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// Parse reads the flags of serve from args, and the variables of the flags
// not given through getenv, over Default, and validates the result. The
// files are read by New. --help yields flag.ErrHelp.
func Parse(args []string, getenv func(string) string) (Config, error) {
	c := Default()
	fs := newFlagSet("serve")
	opts := register(fs, &c)
	if err := parseFlags(fs, args, getenv); err != nil {
		return Config{}, err
	}
	if err := c.LogLevel.UnmarshalText([]byte(*opts.level)); err != nil {
		return Config{}, fmt.Errorf("--log-level: %w", err)
	}
	c.Scopes = strings.Fields(strings.ReplaceAll(*opts.scopes, ",", " "))
	c.Deny, c.Allow = *opts.deny, *opts.allow
	if len(c.Deny) == 0 {
		c.Deny = egress.DefaultDeny()
	}
	c.PublicURL = strings.TrimSuffix(c.PublicURL, "/")
	c.FerunURL = strings.TrimSuffix(c.FerunURL, "/")
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// flagValues are the flags that are parsed into something else.
type flagValues struct {
	level, scopes *string
	deny, allow   *egress.PrefixList
}

func register(fs *flag.FlagSet, c *Config) flagValues {
	fs.StringVar(&c.Listen, "listen", c.Listen, "the sign-in's address, plain HTTP behind the platform's Gateway")
	fs.StringVar(&c.HealthListen, "health-listen", c.HealthListen, "the address of /healthz and /readyz")
	fs.StringVar(&c.PublicURL, "public-url", c.PublicURL, "ferun-auth's own base URL, https://host without a path, e.g. https://auth.example.com (required)")
	fs.StringVar(&c.FerunURL, "ferun-url", c.FerunURL, "ferund's API inside the cluster, e.g. http://ferund.ferun.svc:8100 (required)")
	fs.StringVar(&c.TokenFile, "token-file", c.TokenFile, "the file of ferun-auth's service token at ferund, of kind auth (required)")
	fs.StringVar(&c.KeyFile, "key-file", c.KeyFile, "the file of the 32 bytes (raw, hex or base64) that seal a sign-in in flight, the same for every instance (required)")
	fs.StringVar(&c.Issuer, "issuer", c.Issuer, "the installation's issuer for providers of kind ferun, exactly as ferund's --oidc-issuer; empty: kind ferun is refused")
	fs.StringVar(&c.ClientID, "client-id", c.ClientID, "ferun-auth's client at --issuer (required with --issuer)")
	fs.StringVar(&c.ClientSecretFile, "client-secret-file", c.ClientSecretFile, "the file of that client's secret (required with --issuer)")
	v := flagValues{deny: new(egress.PrefixList), allow: new(egress.PrefixList)}
	scopes := strings.Join(c.Scopes, " ")
	v.scopes = &scopes
	fs.StringVar(v.scopes, "scopes", scopes, "the scopes of a sign-in at an issuer, space- or comma-separated")
	fs.Var(v.deny, "deny-prefix", "the blocks no issuer fetch may reach, comma-separated or repeated; \"default\" is the built-in set, which a value given replaces unless it names it (default \"default\")")
	fs.Var(v.allow, "allow-prefix", "blocks an issuer may be reached in despite --deny-prefix, for an issuer in a private network")
	level := strings.ToLower(c.LogLevel.String())
	v.level = &level
	fs.StringVar(v.level, "log-level", level, "debug, info, warn or error")
	fs.StringVar(&c.LogFormat, "log-format", c.LogFormat, "json or text")
	fs.DurationVar(&c.ShutdownDelay, "shutdown-delay", c.ShutdownDelay, "how long /readyz answers 503 before the listener closes, on SIGTERM")
	return v
}

// Validate checks a configuration.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if c.Listen == "" || c.HealthListen == "" {
		bad("--listen and --health-listen must not be empty")
	}
	if c.PublicURL == "" {
		bad("--public-url is required")
	} else if u, err := url.Parse(c.PublicURL); err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		bad("--public-url must be https://host without a path, query or fragment")
	}
	if c.FerunURL == "" {
		bad("--ferun-url is required")
	} else if u, err := url.Parse(c.FerunURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		bad("--ferun-url must be an http(s) URL without a path, query or fragment")
	}
	if c.TokenFile == "" {
		bad("--token-file is required")
	}
	if c.KeyFile == "" {
		bad("--key-file is required")
	}
	if c.Issuer != "" {
		if u, err := url.Parse(c.Issuer); err != nil || u.Scheme != "https" || u.Host == "" {
			bad("--issuer must be an https URL")
		}
		if c.ClientID == "" {
			bad("--client-id is required with --issuer")
		}
		if c.ClientSecretFile == "" {
			bad("--client-secret-file is required with --issuer")
		}
	} else if c.ClientID != "" || c.ClientSecretFile != "" {
		bad("--client-id and --client-secret-file go with --issuer")
	}
	if !slices.Contains(c.Scopes, "openid") {
		bad("--scopes must hold openid")
	}
	if c.LogFormat != LogFormatJSON && c.LogFormat != LogFormatText {
		bad("--log-format must be %s or %s, not %q", LogFormatJSON, LogFormatText, c.LogFormat)
	}
	if c.ShutdownDelay < 0 {
		bad("--shutdown-delay must not be negative")
	}
	return errors.Join(errs...)
}

// secrets are what the configuration's files hold.
type secrets struct {
	token        string
	key          []byte
	clientSecret string
}

// readSecrets reads the token, the key and, with an issuer, the client
// secret. Errors name the flag, never a file's content.
func (c *Config) readSecrets() (secrets, error) {
	var s secrets
	tok, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return s, fmt.Errorf("--token-file: %w", err)
	}
	if s.token = strings.TrimSpace(string(tok)); s.token == "" {
		return s, errors.New("--token-file: the file is empty")
	}
	if s.key, err = readSecretKey(c.KeyFile); err != nil {
		return s, fmt.Errorf("--key-file: %w", err)
	}
	if c.Issuer != "" {
		sec, err := os.ReadFile(c.ClientSecretFile)
		if err != nil {
			return s, fmt.Errorf("--client-secret-file: %w", err)
		}
		if s.clientSecret = strings.TrimSpace(string(sec)); s.clientSecret == "" {
			return s, errors.New("--client-secret-file: the file is empty")
		}
	}
	return s, nil
}

// readSecretKey reads a 32-byte key: raw, hex or base64. A copy of
// internal/server's readSecretKey (server.go), which ferun-auth does not
// link.
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

// NewLogger returns the logger of a log format and level, writing to w.
func NewLogger(w io.Writer, format string, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == LogFormatText {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// Usage prints the help of serve.
func Usage(w io.Writer) {
	fs := newFlagSet("serve")
	c := Default()
	register(fs, &c)
	fmt.Fprintln(w, "Usage: ferun-auth [serve] [flags]\n\nSigns visitors in for providers of kind ferun and oidc (ADR 0054) until SIGTERM or\nSIGINT. Every flag has an environment variable "+
		EnvPrefix+"<NAME>.\n\nFlags:")
	fs.SetOutput(w)
	fs.PrintDefaults()
}

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
		if v := getenv(EnvName(f.Name)); v != "" {
			if err := fs.Set(f.Name, v); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", EnvName(f.Name), err))
			}
		}
	})
	return errors.Join(errs...)
}
