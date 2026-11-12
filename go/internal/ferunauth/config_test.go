// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ferunauth

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/egress"
)

// required are the settings serve needs, as flags.
func required(t *testing.T) []string {
	return []string{"--public-url", "https://auth.example.test", "--ferun-url", "http://ferund.ferun.svc:8100",
		"--token-file", writeFile(t, "token", "feos-auth-x"), "--key-file", writeFile(t, "key", strings.Repeat("ab", 32))}
}

func TestParseDefaults(t *testing.T) {
	c, err := Parse(required(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.HealthListen != ":9090" || !slices.Equal(c.Scopes, []string{"openid", "profile", "email"}) ||
		!slices.Equal(c.Deny, egress.DefaultDeny()) || c.Allow != nil || c.LogLevel != slog.LevelInfo ||
		c.LogFormat != LogFormatJSON || c.ShutdownDelay != 5*time.Second || c.Issuer != "" {
		t.Fatalf("defaults %+v", c)
	}
}

// Every flag has its variable FERUN_AUTH_<NAME>; a flag given wins.
func TestParseEnv(t *testing.T) {
	env := map[string]string{
		"FERUN_AUTH_PUBLIC_URL":         "https://auth.example.test/",
		"FERUN_AUTH_FERUN_URL":          "http://ferund:8100",
		"FERUN_AUTH_TOKEN_FILE":         "/t",
		"FERUN_AUTH_KEY_FILE":           "/k",
		"FERUN_AUTH_ISSUER":             "https://id.example.test/",
		"FERUN_AUTH_CLIENT_ID":          "ferun-auth",
		"FERUN_AUTH_CLIENT_SECRET_FILE": "/s",
		"FERUN_AUTH_SCOPES":             "openid,email",
		"FERUN_AUTH_DENY_PREFIX":        "default,100.144.0.0/13",
		"FERUN_AUTH_ALLOW_PREFIX":       "10.1.0.0/16",
		"FERUN_AUTH_LOG_LEVEL":          "debug",
		"FERUN_AUTH_LOG_FORMAT":         "text",
		"FERUN_AUTH_SHUTDOWN_DELAY":     "1s",
		"FERUN_AUTH_LISTEN":             ":1",
		"FERUN_AUTH_HEALTH_LISTEN":      ":2",
	}
	c, err := Parse([]string{"--listen", ":3"}, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://auth.example.test" || c.FerunURL != "http://ferund:8100" || c.TokenFile != "/t" ||
		c.KeyFile != "/k" || c.Issuer != "https://id.example.test/" || c.ClientID != "ferun-auth" ||
		c.ClientSecretFile != "/s" || !slices.Equal(c.Scopes, []string{"openid", "email"}) ||
		len(c.Deny) != len(egress.DefaultDeny())+1 || !slices.Equal(c.Allow, []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16")}) ||
		c.LogLevel != slog.LevelDebug || c.LogFormat != LogFormatText || c.ShutdownDelay != time.Second ||
		c.Listen != ":3" || c.HealthListen != ":2" {
		t.Fatalf("%+v", c)
	}
	if EnvName("client-secret-file") != "FERUN_AUTH_CLIENT_SECRET_FILE" {
		t.Fatal(EnvName("client-secret-file"))
	}
}

func TestParseRefusals(t *testing.T) {
	without := func(flag string) []string {
		args := required(t)
		i := slices.Index(args, flag)
		return slices.Delete(args, i, i+2)
	}
	with := func(more ...string) []string { return append(required(t), more...) }
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no public URL":        {without("--public-url"), "--public-url is required"},
		"no ferun URL":         {without("--ferun-url"), "--ferun-url is required"},
		"no token file":        {without("--token-file"), "--token-file is required"},
		"no key file":          {without("--key-file"), "--key-file is required"},
		"an http public URL":   {with("--public-url", "http://auth.example.test"), "--public-url must be https://host"},
		"a public URL's path":  {with("--public-url", "https://auth.example.test/auth"), "--public-url must be https://host"},
		"a public URL's query": {with("--public-url", "https://auth.example.test?x"), "--public-url must be https://host"},
		"no host":              {with("--public-url", "https://"), "--public-url must be https://host"},
		"a bad ferun URL":      {with("--ferun-url", "ferund:8100"), "--ferun-url must be"},
		"issuer, no client":    {with("--issuer", "https://id.example.test/", "--client-secret-file", "/s"), "--client-id is required with --issuer"},
		"issuer, no secret":    {with("--issuer", "https://id.example.test/", "--client-id", "c"), "--client-secret-file is required with --issuer"},
		"an http issuer":       {with("--issuer", "http://id.example.test/", "--client-id", "c", "--client-secret-file", "/s"), "--issuer must be an https URL"},
		"a client, no issuer":  {with("--client-id", "c"), "go with --issuer"},
		"no openid":            {with("--scopes", "email"), "--scopes must hold openid"},
		"a bad prefix":         {with("--deny-prefix", "10.0.0.0"), "deny-prefix"},
		"a bad format":         {with("--log-format", "xml"), "--log-format"},
		"a bad level":          {with("--log-level", "loud"), "--log-level"},
		"arguments":            {with("x"), "unexpected arguments"},
	} {
		if _, err := Parse(tc.args, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// New reads the files: an empty token or secret, or a key that is none,
// is a start error naming its flag, never the content.
func TestNewFiles(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	base := func() Config {
		c, err := Parse(required(t), nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if _, err := New(base(), log); err != nil {
		t.Fatal(err)
	}
	c := base()
	c.TokenFile = writeFile(t, "token", " \n")
	if _, err := New(c, log); err == nil || !strings.Contains(err.Error(), "--token-file") {
		t.Errorf("empty token: %v", err)
	}
	c = base()
	c.KeyFile = writeFile(t, "key", "short-and-secret")
	if _, err := New(c, log); err == nil || !strings.Contains(err.Error(), "--key-file") || strings.Contains(err.Error(), "short-and-secret") {
		t.Errorf("bad key: %v", err)
	}
	c = base()
	c.TokenFile = "/nonexistent/token"
	if _, err := New(c, log); err == nil || !strings.Contains(err.Error(), "--token-file") {
		t.Errorf("missing token file: %v", err)
	}
	c = base()
	c.Issuer, c.ClientID, c.ClientSecretFile = "https://id.example.test/", "c", writeFile(t, "secret", "\n")
	if _, err := New(c, log); err == nil || !strings.Contains(err.Error(), "--client-secret-file") {
		t.Errorf("empty secret: %v", err)
	}
	for _, key := range []string{strings.Repeat("k", 32), "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"} {
		c = base()
		c.KeyFile = writeFile(t, "key", key)
		if _, err := New(c, log); err != nil {
			t.Errorf("key %q: %v", key, err)
		}
	}
}

// Run serves until its context ends, /readyz failing while it stops, and
// returns nil; a listener that cannot be had is an error.
func TestRun(t *testing.T) {
	c, err := Parse(append(required(t), "--listen", "127.0.0.1:0", "--health-listen", "127.0.0.1:0", "--shutdown-delay", "10ms"), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if !srv.draining.Load() {
		t.Fatal("not draining")
	}
	c.Listen = "256.0.0.1:1"
	srv, _ = New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := srv.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "--listen") {
		t.Fatalf("bad listener: %v", err)
	}
}
