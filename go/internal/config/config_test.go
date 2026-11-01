// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"flag"
	"log/slog"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

const testURL = "postgres://u:p@db:5432/ferun"

// Without flags or variables, every default of §3.
func TestDefaults(t *testing.T) {
	t.Parallel()
	c, err := Parse([]string{"--database-url", testURL}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Default()
	want.DatabaseURL = testURL
	want.AdminUsers = map[string]string{}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got\n%+v\nwant\n%+v", c, want)
	}
	for name, got := range map[string]any{
		"listen": c.Listen, "node-listen": c.NodeListen, "trust-domain": c.TrustDomain, "ca-dir": c.CADir,
		"oidc-client-id": c.OIDCClientID, "oidc-admin-group": c.OIDCAdminGroup, "session-ttl": c.SessionTTL,
		"default-image": c.DefaultImage, "default-vcpus": c.DefaultVCPUs, "default-memory-mib": c.DefaultMemoryMiB,
		"placement": c.Placement, "disk-budget": c.DiskBudget, "idle-pause": c.IdlePause, "hibernate-after": c.HibernateAfter,
		"node-ttl": c.NodeTTL, "lost-grace": c.LostGrace, "node-wait": c.NodeWait, "rollout-grace": c.RolloutGrace,
		"drain-timeout": c.DrainTimeout, "pending-repair-after": c.PendingRepairAfter,
		"unreachable-repair-after": c.UnreachableRepairAfter, "node-cert-ttl": c.NodeCertTTL, "pod-cert-ttl": c.PodCertTTL,
		"node-max-streams": c.NodeMaxStreams, "conn-drain-timeout": c.ConnDrainTimeout, "shutdown-grace": c.ShutdownGrace,
		"activity-retention": c.ActivityRetention, "traffic-retention": c.TrafficRetention, "log-level": c.LogLevel,
		"oidc-scopes": strings.Join(c.OIDCScopes, " "), "hibernate-timeout": c.HibernateTimeout,
	} {
		wantV := map[string]any{
			"listen": ":8100", "node-listen": ":7443", "trust-domain": "feos.local", "ca-dir": "/etc/ferun/ca",
			"oidc-client-id": "ferun", "oidc-admin-group": "admins", "session-ttl": 12 * time.Hour,
			"default-image": "ghcr.io/maltej/debian-ai:latest", "default-vcpus": 2, "default-memory-mib": 512,
			"placement": "binpack", "disk-budget": int64(4 << 30), "idle-pause": 10 * time.Second,
			"hibernate-after": time.Duration(0), "node-ttl": 15 * time.Second, "lost-grace": 10 * time.Minute,
			"node-wait": 5 * time.Second, "rollout-grace": 30 * time.Second, "drain-timeout": 2 * time.Minute,
			"pending-repair-after": 10 * time.Minute, "unreachable-repair-after": 2 * time.Minute,
			"node-cert-ttl": 168 * time.Hour, "pod-cert-ttl": time.Hour, "node-max-streams": 1024,
			"conn-drain-timeout": 20 * time.Minute, "shutdown-grace": 11 * time.Minute,
			"activity-retention": 720 * time.Hour, "traffic-retention": 168 * time.Hour, "log-level": slog.LevelInfo,
			"oidc-scopes": "openid profile email", "hibernate-timeout": time.Hour,
		}[name]
		if !reflect.DeepEqual(got, wantV) {
			t.Errorf("--%s default %v, want %v", name, got, wantV)
		}
	}
	if c.ReadinessDelay != 5*time.Second || c.ReadyWindow != 30*time.Second {
		t.Errorf("readiness %v %v", c.ReadinessDelay, c.ReadyWindow)
	}
	if c.CallLease != 30*time.Second || c.CallRetention != time.Hour {
		t.Errorf("calls with an id: lease %v, retention %v", c.CallLease, c.CallRetention)
	}
}

// The lab deployment's variables (deploy/k3s/ferun/ferund.yaml), each of
// them read.
func TestDeploymentEnv(t *testing.T) {
	t.Parallel()
	got, err := Parse(nil, env(map[string]string{
		"FERUN_DATABASE_URL":            testURL,
		"FERUN_LISTEN":                  ":8100",
		"FERUN_NODE_LISTEN":             ":7443",
		"FERUN_NODE_ADVERTISE_ADDRESS":  "10.42.0.7:7443",
		"FERUN_TRUST_DOMAIN":            "run.fecode.dev",
		"FERUN_CA_DIR":                  "/etc/ferun/ca",
		"FERUN_HIBERNATE_AFTER":         "2m",
		"FERUN_SHUTDOWN_GRACE":          "11m",
		"FERUN_PUBLIC_URL":              "https://run.fecode.dev",
		"FERUN_OIDC_ISSUER":             "https://id.fecode.dev/",
		"FERUN_OIDC_CLIENT_ID":          "client-123",
		"FERUN_OIDC_CLIENT_SECRET_FILE": "/etc/ferun/oidc/secret",
		"FERUN_OIDC_ADMIN_GROUP":        "admins",
		"FERUN_SESSION_TTL":             "12h",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got.DatabaseURL != testURL || got.Listen != ":8100" || got.NodeListen != ":7443" ||
		got.NodeAdvertiseAddress != "10.42.0.7:7443" || got.TrustDomain != "run.fecode.dev" || got.CADir != "/etc/ferun/ca" ||
		got.HibernateAfter != 2*time.Minute || got.ShutdownGrace != 11*time.Minute || got.PublicURL != "https://run.fecode.dev" ||
		got.OIDCIssuer != "https://id.fecode.dev/" || got.OIDCClientID != "client-123" ||
		got.OIDCClientSecretFile != "/etc/ferun/oidc/secret" || got.OIDCAdminGroup != "admins" || got.SessionTTL != 12*time.Hour {
		t.Fatalf("parsed %+v", got)
	}
}

// "default" in --deny-prefix is the built-in set, so that a deployment
// adds its own networks to it; repeated prefixes stay.
func TestDenyPrefixDefault(t *testing.T) {
	t.Parallel()
	c, err := Parse([]string{"--database-url", testURL}, env(map[string]string{
		"FERUN_DENY_PREFIX": "default, 100.144.0.0/13,100.152.0.0/13",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := append(DefaultDenyPrefixes(), netip.MustParsePrefix("100.144.0.0/13"), netip.MustParsePrefix("100.152.0.0/13"))
	if !reflect.DeepEqual(c.DenyPrefixes, want) {
		t.Fatalf("deny %v, want %v", c.DenyPrefixes, want)
	}
	if _, err := Parse([]string{"--database-url", testURL, "--deny-prefix", "defaults"}, env(nil)); err == nil {
		t.Fatal("a misspelt default parsed")
	}
}

// A flag wins over its variable, a variable over the default; lists,
// sizes, admin users, scopes and the log level parse.
func TestFlagsAndEnv(t *testing.T) {
	t.Parallel()
	c, err := Parse([]string{
		"--database-url", testURL, "--listen", "127.0.0.1:0", "--node-server-names", "a.ferun.x,b.ferun.x",
		"--node-server-names", "c.ferun.x", "--deny-prefix", "10.0.0.0/8", "--admin-users", "a:p:with:colons, b:q ",
		"--oidc-scopes", "openid  groups", "--bindings", "--disk-budget", "2GiB",
	}, env(map[string]string{
		"FERUN_LISTEN":              ":9999", // the flag wins
		"FERUN_PLACEMENT":           "spread",
		"FERUN_DENY_PREFIX":         "192.168.0.0/16", // the flag wins
		"FERUN_MCP_ALLOWED_ORIGINS": "https://a.example, https://b.example",
		"FERUN_IDLE_PAUSE":          "0",
		"FERUN_LOG_LEVEL":           "debug",
		"FERUN_DEFAULT_VCPUS":       "4",
		"FERUN_BOOTSTRAP_TOKEN":     "tok",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:0" || c.Placement != "spread" || c.IdlePause != 0 || c.LogLevel != slog.LevelDebug ||
		c.DefaultVCPUs != 4 || c.BootstrapToken != "tok" || !c.Bindings || c.DiskBudget != 2<<30 {
		t.Fatalf("parsed %+v", c)
	}
	if strings.Join(c.NodeServerNames, ",") != "a.ferun.x,b.ferun.x,c.ferun.x" {
		t.Fatalf("names %v", c.NodeServerNames)
	}
	if !reflect.DeepEqual(c.DenyPrefixes, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}) {
		t.Fatalf("deny %v", c.DenyPrefixes)
	}
	if !reflect.DeepEqual(c.AdminUsers, map[string]string{"a": "p:with:colons", "b": "q"}) {
		t.Fatalf("admins %v", c.AdminUsers)
	}
	if strings.Join(c.MCPAllowedOrigins, "|") != "https://a.example|https://b.example" ||
		strings.Join(c.OIDCScopes, ",") != "openid,groups" {
		t.Fatalf("origins %v scopes %v", c.MCPAllowedOrigins, c.OIDCScopes)
	}
}

// The refusals: missing, malformed or contradictory settings.
func TestValidation(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		args []string
		env  map[string]string
		msg  string
	}{
		{nil, nil, "--database-url is required"},
		{[]string{"--trust-domain", "Bad_Domain"}, nil, "--trust-domain"},
		{[]string{"--placement", "random"}, nil, `--placement must be binpack or spread, not "random"`},
		{[]string{"--default-vcpus", "64"}, nil, "--default-vcpus must be between 1 and 32"},
		{[]string{"--default-memory-mib", "64"}, nil, "--default-memory-mib must be between 256 and 65536"},
		{[]string{"--tls-cert", "c.pem"}, nil, "--tls-cert and --tls-key go together"},
		{[]string{"--admin-users", "nopassword"}, nil, `--admin-users: "…" is not a user:password pair`},
		{[]string{"--oidc-issuer", "https://id.example/"}, nil, "--public-url is required with --oidc-issuer"},
		{[]string{"--oidc-issuer", "https://id.example/", "--public-url", "https://run.example"}, nil,
			"--oidc-client-secret-file is required with --oidc-issuer"},
		{[]string{"--oidc-issuer", "https://id.example/", "--public-url", "https://run.example", "--oidc-client-secret-file",
			"s", "--oidc-scopes", "profile"}, nil, "--oidc-scopes must contain openid"},
		{[]string{"--public-url", "https://run.example/app"}, nil, "--public-url"},
		{[]string{"--node-advertise-address", "10.0.0.1"}, nil, `--node-advertise-address "10.0.0.1" is not host:port`},
		{[]string{"--node-server-names", "10.0.0.1"}, nil, "is an IP address"},
		{[]string{"--node-ttl", "0s"}, nil, "--node-ttl must be positive"},
		{[]string{"--hibernate-timeout", "0s"}, nil, "--hibernate-timeout must be positive"},
		{nil, map[string]string{"FERUN_HIBERNATE_TIMEOUT": "-5m"}, "--hibernate-timeout must be positive"},
		{nil, map[string]string{"FERUN_HIBERNATE_TIMEOUT": "an hour"}, "FERUN_HIBERNATE_TIMEOUT"},
		{[]string{"--disk-budget", "4GB"}, nil, "is not a byte size"},
		{nil, map[string]string{"FERUN_NODE_TTL": "soon"}, "FERUN_NODE_TTL"},
		{[]string{"extra"}, nil, `unexpected arguments ["extra"]`},
	} {
		args := c.args
		if !(len(args) > 0 && args[0] == "extra") && c.msg != "--database-url is required" {
			args = append([]string{"--database-url", testURL}, args...)
		}
		_, err := Parse(args, env(c.env))
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%v %v: %v, want %q", c.args, c.env, err, c.msg)
		}
	}
	if _, err := Parse([]string{"--help"}, env(nil)); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("--help: %v", err)
	}
	// A public URL loses its trailing slash.
	c, err := Parse([]string{"--database-url", testURL, "--public-url", "https://run.example/"}, env(nil))
	if err != nil || c.PublicURL != "https://run.example" {
		t.Fatalf("%q %v", c.PublicURL, err)
	}
}

// Validate wraps the causes it names (%w), so that errors.Is and errors.As
// find them: the *url.Error of a public URL that does not parse.
func TestValidationWrapsCauses(t *testing.T) {
	t.Parallel()
	c := Default()
	c.DatabaseURL = testURL
	c.PublicURL = "https://run.example/%zz"
	err := c.Validate()
	var ue *url.Error
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "--public-url: ") {
		t.Fatalf("%v: the cause is not wrapped", err)
	}
}

func TestByteSize(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int64{"0": 0, "4096": 4096, "1KiB": 1 << 10, "3 MiB": 3 << 20, "4GiB": 4 << 30} {
		got, err := ParseByteSize(in)
		if err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	for _, in := range []string{"", "-1", "1TB", "x", "1.5GiB"} {
		if _, err := ParseByteSize(in); err == nil {
			t.Errorf("%q parsed", in)
		}
	}
}

// The subcommands' flags, with serve's variable names and defaults where
// they share them.
func TestSubcommands(t *testing.T) {
	t.Parallel()
	ic, err := ParseInitCA([]string{"--root-dir", "/r", "--dir", "/d", "--dns-domain", "a.example", "--dns-domain", "b.example"},
		env(map[string]string{"FERUN_TRUST_DOMAIN": "feos.test", "FERUN_CONTROL_PLANE_DOMAIN": "cp.a.example"}))
	if err != nil {
		t.Fatal(err)
	}
	if ic.RootDir != "/r" || ic.Dir != "/d" || ic.TrustDomain != "feos.test" || ic.ControlPlaneDomain != "cp.a.example" ||
		strings.Join(ic.DNSDomains, ",") != "a.example,b.example" {
		t.Fatalf("init-ca %+v", ic)
	}
	if _, err := ParseInitCA([]string{"--root-dir", "/r"}, env(nil)); err == nil ||
		!strings.Contains(err.Error(), "--dir is required") || !strings.Contains(err.Error(), "--dns-domain is required") {
		t.Fatalf("init-ca refusal: %v", err)
	}
	ni, err := ParseNewIssuer([]string{"--root-dir", "/r", "--dir", "/n", "--dns-domain", "a.example"},
		env(map[string]string{"FERUN_TRUST_DOMAIN": "feos.test"}))
	if err != nil || ni.RootDir != "/r" || ni.Dir != "/n" || ni.TrustDomain != "feos.test" || strings.Join(ni.DNSDomains, ",") != "a.example" {
		t.Fatalf("new-issuer %+v %v", ni, err)
	}
	if _, err := ParseNewIssuer([]string{"--dir", "/n"}, env(nil)); err == nil ||
		!strings.Contains(err.Error(), "--root-dir is required") || !strings.Contains(err.Error(), "--dns-domain is required") {
		t.Fatalf("new-issuer refusal: %v", err)
	}

	jt, err := ParseJoinToken([]string{"--node", "n7", "--ttl", "30m"},
		env(map[string]string{"FERUN_DATABASE_URL": testURL, "FERUN_TRUST_DOMAIN": "run.fecode.dev"}))
	if err != nil {
		t.Fatal(err)
	}
	if jt.DatabaseURL != testURL || jt.CADir != "/etc/ferun/ca" || jt.TrustDomain != "run.fecode.dev" || jt.Node != "n7" ||
		jt.TTL != 30*time.Minute {
		t.Fatalf("join-token %+v", jt)
	}
	jt, err = ParseJoinToken(nil, env(map[string]string{"FERUN_DATABASE_URL": testURL}))
	if err != nil || jt.TTL != time.Hour || jt.TrustDomain != "feos.local" {
		t.Fatalf("join-token defaults %+v %v", jt, err)
	}
	for _, args := range [][]string{{"--node", "Bad_Name"}, {"--ttl", "0s"}, {"--trust-domain", "x_y"}} {
		if _, err := ParseJoinToken(args, env(map[string]string{"FERUN_DATABASE_URL": testURL})); err == nil {
			t.Errorf("join-token %v parsed", args)
		}
	}
	if _, err := ParseJoinToken(nil, env(nil)); err == nil || !strings.Contains(err.Error(), "--database-url is required") {
		t.Fatalf("join-token without a database: %v", err)
	}

	m, err := ParseMigrate(nil, env(map[string]string{"FERUN_DATABASE_URL": testURL}))
	if err != nil || m.DatabaseURL != testURL {
		t.Fatalf("migrate %+v %v", m, err)
	}
	if _, err := ParseMigrate(nil, env(nil)); err == nil {
		t.Fatal("migrate without a database")
	}
}

// The help names every flag with its variable.
func TestUsage(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	Usage(&b, CmdServe)
	for _, s := range []string{"--database-url (FERUN_DATABASE_URL)", "--oidc-client-secret-file (FERUN_OIDC_CLIENT_SECRET_FILE)",
		`--shutdown-grace (FERUN_SHUTDOWN_GRACE, default "11m0s")`, "--node-advertise-address (FERUN_NODE_ADVERTISE_ADDRESS)",
		"how long a stopping instance waits for its requests, its tool calls and its ops; set a container's termination grace to at least this plus 60s",
		`--hibernate-timeout (FERUN_HIBERNATE_TIMEOUT, default "1h0m0s")`,
		"how long ferund waits for a hibernate (plus a pause's deadline and 16s per GiB of memory); its upload of the pod's disk and memory to the object store takes minutes per GiB, and the node's hibernation goes on after the wait",
		"lifetime of node certificates; renewed at half of it, so that a node switched off for up to half of it needs nothing"} {
		if !strings.Contains(b.String(), s) {
			t.Errorf("serve help lacks %q", s)
		}
	}
	for _, cmd := range []string{"", CmdInitCA, CmdNewIssuer, CmdJoinToken, CmdMigrate} {
		b.Reset()
		Usage(&b, cmd)
		if !strings.Contains(b.String(), "Usage: ferund") {
			t.Errorf("help of %q: %s", cmd, b.String())
		}
	}
	if EnvName("oidc-client-secret-file") != "FERUN_OIDC_CLIENT_SECRET_FILE" {
		t.Fatal(EnvName("oidc-client-secret-file"))
	}
}

// --hibernate-timeout bounds how long ferund waits for a hibernate's node
// call: its variable is FERUN_HIBERNATE_TIMEOUT, its default an hour, and
// the flag wins over the variable.
func TestHibernateTimeout(t *testing.T) {
	t.Parallel()
	if EnvName("hibernate-timeout") != "FERUN_HIBERNATE_TIMEOUT" {
		t.Fatal(EnvName("hibernate-timeout"))
	}
	c, err := Parse([]string{"--database-url", testURL}, env(nil))
	if err != nil || c.HibernateTimeout != time.Hour || Default().HibernateTimeout != time.Hour {
		t.Fatalf("default %v %v", c.HibernateTimeout, err)
	}
	c, err = Parse([]string{"--database-url", testURL}, env(map[string]string{"FERUN_HIBERNATE_TIMEOUT": "90m"}))
	if err != nil || c.HibernateTimeout != 90*time.Minute {
		t.Fatalf("from the variable %v %v", c.HibernateTimeout, err)
	}
	c, err = Parse([]string{"--database-url", testURL, "--hibernate-timeout", "2h"},
		env(map[string]string{"FERUN_HIBERNATE_TIMEOUT": "90m"}))
	if err != nil || c.HibernateTimeout != 2*time.Hour {
		t.Fatalf("from the flag %v %v", c.HibernateTimeout, err)
	}
	c = Default()
	c.DatabaseURL = testURL
	c.HibernateTimeout = 0
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "--hibernate-timeout must be positive") {
		t.Fatalf("an instance built in process with no hibernate timeout: %v", err)
	}
}

// --pod-network: routed by default, mesh, or a ULA /64 of fd00::/8 without
// host bits (a /48 of before for its /64 of subnet 0).
func TestPodNetwork(t *testing.T) {
	t.Parallel()
	c, err := Parse([]string{"--database-url", testURL}, env(nil))
	if err != nil || c.PodNetwork != PodNetworkRouted {
		t.Fatalf("default %q, %v", c.PodNetwork, err)
	}
	for _, ok := range []string{"mesh", "fd5e:1b2c:93a0::/64", "fdff:ffff:ffff:ffff::/64", "fd5e:1b2c:93a0::/48"} {
		c, err := Parse([]string{"--database-url", testURL}, env(map[string]string{"FERUN_POD_NETWORK": ok}))
		if err != nil || c.PodNetwork != ok {
			t.Errorf("%q: %q, %v", ok, c.PodNetwork, err)
		}
	}
	for _, bad := range []string{
		"Mesh", "routed", "fd5e:1b2c:93a0::/56", "fd5e:1b2c:93a0::/96", "fd5e:1b2c:93a0::1/64", "fd5e:1b2c:93a0::1/48",
		"fc00:1:2::/64", "2001:db8:1::/64",
		"10.0.0.0/8", "fd5e:1b2c:93a0::", "::ffff:10.0.0.0/120",
	} {
		if _, err := Parse([]string{"--database-url", testURL, "--pod-network", bad}, env(nil)); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	for in, want := range map[string]string{
		"fd5e:1b2c:93a0:7::/64": "fd5e:1b2c:93a0:7::/64",
		"fd5e:1b2c:93a0::/48":   "fd5e:1b2c:93a0::/64",
	} {
		p, err := ParsePodNetwork(in)
		if err != nil || p != netip.MustParsePrefix(want) {
			t.Errorf("%s: %v %v", in, p, err)
		}
	}
}

// --acme-directory: empty by default, which obtains no certificate;
// otherwise an https URL, with --secret-key-file. --acme-email: empty, or
// an email address. Both from the flag or their variables.
func TestACME(t *testing.T) {
	t.Parallel()
	if EnvName("acme-directory") != "FERUN_ACME_DIRECTORY" || EnvName("acme-email") != "FERUN_ACME_EMAIL" {
		t.Fatal(EnvName("acme-directory"), EnvName("acme-email"))
	}
	const dir = "https://acme.example/directory"
	c, err := Parse([]string{"--database-url", testURL}, env(nil))
	if err != nil || c.ACMEDirectory != "" || c.ACMEEmail != "" || Default().ACMEDirectory != "" ||
		c.ACMEProfile != "shortlived" {
		t.Fatalf("default %q %q, %v", c.ACMEDirectory, c.ACMEEmail, err)
	}
	c, err = Parse([]string{"--database-url", testURL, "--secret-key-file", "/k"},
		env(map[string]string{"FERUN_ACME_DIRECTORY": dir, "FERUN_ACME_EMAIL": "ops@fecore.dev"}))
	if err != nil || c.ACMEDirectory != dir || c.ACMEEmail != "ops@fecore.dev" {
		t.Fatalf("from the variables %q %q, %v", c.ACMEDirectory, c.ACMEEmail, err)
	}
	c, err = Parse([]string{"--database-url", testURL, "--secret-key-file", "/k", "--acme-directory", "https://other.example/dir",
		"--acme-email", ""}, env(map[string]string{"FERUN_ACME_DIRECTORY": dir, "FERUN_ACME_EMAIL": "ops@fecore.dev"}))
	if err != nil || c.ACMEDirectory != "https://other.example/dir" || c.ACMEEmail != "" {
		t.Fatalf("from the flags %q %q, %v", c.ACMEDirectory, c.ACMEEmail, err)
	}
	if _, err := Parse([]string{"--database-url", testURL, "--acme-directory", dir}, env(nil)); err == nil ||
		!strings.Contains(err.Error(), "--secret-key-file") {
		t.Errorf("a directory without --secret-key-file: %v", err)
	}
	if c, err := Parse([]string{"--database-url", testURL, "--acme-profile", ""}, env(nil)); err != nil || c.ACMEProfile != "" {
		t.Fatalf("an empty profile %q, %v", c.ACMEProfile, err)
	}
	if c, err := Parse([]string{"--database-url", testURL}, env(map[string]string{"FERUN_ACME_PROFILE": "classic"})); err != nil ||
		c.ACMEProfile != "classic" {
		t.Fatalf("a profile from the variable %q, %v", c.ACMEProfile, err)
	}
	for _, bad := range []string{"short lived", "a/b", strings.Repeat("x", 65)} {
		if _, err := Parse([]string{"--database-url", testURL, "--acme-profile", bad}, env(nil)); err == nil ||
			!strings.Contains(err.Error(), "--acme-profile") {
			t.Errorf("profile %q: %v", bad, err)
		}
	}
	for _, bad := range []string{"http://acme.example/directory", "acme.example/directory", "https://", "https://u:p@acme.example/d",
		"ftp://acme.example", " https://acme.example"} {
		if _, err := Parse([]string{"--database-url", testURL, "--secret-key-file", "/k", "--acme-directory", bad},
			env(nil)); err == nil || !strings.Contains(err.Error(), "--acme-directory") {
			t.Errorf("directory %q: %v", bad, err)
		}
	}
	for _, bad := range []string{"ops", "ops@", "Ops <ops@fecore.dev>", "ops@fecore.dev, b@fecore.dev", " ops@fecore.dev",
		"mailto:ops@fecore.dev"} {
		if _, err := Parse([]string{"--database-url", testURL, "--acme-email", bad}, env(nil)); err == nil ||
			!strings.Contains(err.Error(), "--acme-email") {
			t.Errorf("email %q: %v", bad, err)
		}
	}
	c = Default()
	c.DatabaseURL, c.ACMEDirectory = testURL, dir
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "--secret-key-file") {
		t.Fatalf("an instance built in process without a key: %v", err)
	}
	var b strings.Builder
	Usage(&b, CmdServe)
	if !strings.Contains(b.String(), "--acme-directory (FERUN_ACME_DIRECTORY)") ||
		!strings.Contains(b.String(), "--acme-email (FERUN_ACME_EMAIL)") {
		t.Fatalf("serve help lacks the ACME flags:\n%s", b.String())
	}
}

// --apps-zone: empty by default, which serves no hosts; otherwise a
// lowercase DNS name, from the flag or FERUN_APPS_ZONE.
func TestAppsZone(t *testing.T) {
	t.Parallel()
	if EnvName("apps-zone") != "FERUN_APPS_ZONE" {
		t.Fatal(EnvName("apps-zone"))
	}
	c, err := Parse([]string{"--database-url", testURL}, env(nil))
	if err != nil || c.AppsZone != "" || Default().AppsZone != "" {
		t.Fatalf("default %q, %v", c.AppsZone, err)
	}
	c, err = Parse([]string{"--database-url", testURL, "--apps-zone", ""}, env(map[string]string{"FERUN_APPS_ZONE": "apps.example"}))
	if err != nil || c.AppsZone != "" {
		t.Fatalf("an empty zone: %q, %v", c.AppsZone, err)
	}
	c, err = Parse([]string{"--database-url", testURL}, env(map[string]string{"FERUN_APPS_ZONE": "apps.example"}))
	if err != nil || c.AppsZone != "apps.example" {
		t.Fatalf("from the variable %q, %v", c.AppsZone, err)
	}
	c, err = Parse([]string{"--database-url", testURL, "--apps-zone", "fecode-apps.dev"},
		env(map[string]string{"FERUN_APPS_ZONE": "apps.example"}))
	if err != nil || c.AppsZone != "fecode-apps.dev" {
		t.Fatalf("from the flag %q, %v", c.AppsZone, err)
	}
	for _, bad := range []string{
		"Apps.Example", "apps.example.", ".apps.example", "apps..example", "-apps.example", "apps_zone.example",
		"apps.example:443", "https://apps.example", "*.apps.example", "10.0.0.1", "2001:db8::1", " apps.example",
		strings.Repeat("a", 64) + ".example",
	} {
		if _, err := Parse([]string{"--database-url", testURL, "--apps-zone", bad}, env(nil)); err == nil ||
			!strings.Contains(err.Error(), "--apps-zone") {
			t.Errorf("%q: %v", bad, err)
		}
		if _, err := Parse([]string{"--database-url", testURL}, env(map[string]string{"FERUN_APPS_ZONE": bad})); err == nil {
			t.Errorf("FERUN_APPS_ZONE %q was taken", bad)
		}
	}
	c = Default()
	c.DatabaseURL = testURL
	c.AppsZone = "Bad_Zone"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "--apps-zone") {
		t.Fatalf("an instance built in process with a bad zone: %v", err)
	}
	var b strings.Builder
	Usage(&b, CmdServe)
	if !strings.Contains(b.String(), "--apps-zone (FERUN_APPS_ZONE)") {
		t.Fatalf("serve help lacks --apps-zone:\n%s", b.String())
	}
}

// --apps-zone-owners: none by default; tenant names from the flag or
// FERUN_APPS_ZONE_OWNERS, comma-separated, the flag winning; a name that
// is no tenant's is refused.
func TestAppsZoneOwners(t *testing.T) {
	t.Parallel()
	if EnvName("apps-zone-owners") != "FERUN_APPS_ZONE_OWNERS" {
		t.Fatal(EnvName("apps-zone-owners"))
	}
	c, err := Parse([]string{"--database-url", testURL}, env(nil))
	if err != nil || c.AppsZoneOwners != nil || Default().AppsZoneOwners != nil {
		t.Fatalf("default %q, %v", c.AppsZoneOwners, err)
	}
	c, err = Parse([]string{"--database-url", testURL}, env(map[string]string{"FERUN_APPS_ZONE_OWNERS": "fecode, acme.io"}))
	if err != nil || !reflect.DeepEqual(c.AppsZoneOwners, []string{"fecode", "acme.io"}) {
		t.Fatalf("from the variable %q, %v", c.AppsZoneOwners, err)
	}
	c, err = Parse([]string{"--database-url", testURL, "--apps-zone-owners", "fecode"},
		env(map[string]string{"FERUN_APPS_ZONE_OWNERS": "other"}))
	if err != nil || !reflect.DeepEqual(c.AppsZoneOwners, []string{"fecode"}) {
		t.Fatalf("from the flag %q, %v", c.AppsZoneOwners, err)
	}
	for _, bad := range []string{"FeCode", "f", "-fecode", "fe_code", "fecode/x"} {
		if _, err := Parse([]string{"--database-url", testURL, "--apps-zone-owners", bad}, env(nil)); err == nil ||
			!strings.Contains(err.Error(), "--apps-zone-owners") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	var b strings.Builder
	Usage(&b, CmdServe)
	if !strings.Contains(b.String(), "--apps-zone-owners (FERUN_APPS_ZONE_OWNERS)") {
		t.Fatalf("serve help lacks --apps-zone-owners:\n%s", b.String())
	}
}

// The domains' flags (ADR 0021): their defaults (domains off, the label
// _ferun-challenge, a grace of 72 h, 50 unverified claims), their
// variables, and what Validate refuses.
func TestDomainFlags(t *testing.T) {
	t.Parallel()
	c, err := Parse([]string{"--database-url", testURL}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.EdgeName != "" || c.EdgeAddresses != nil || c.DomainChallengeLabel != "_ferun-challenge" ||
		c.DomainGrace != 72*time.Hour || c.DomainUnverifiedMax != 50 || c.DomainReserved != nil || c.DNSResolver != "" {
		t.Fatalf("defaults %+v", c)
	}
	c, err = Parse([]string{"--database-url", testURL, "--edge-address", "194.11.242.65", "--edge-address",
		"2001:db8::1,::ffff:10.0.0.1", "--domain-reserved", "fecode.dev,fecore.dev"}, env(map[string]string{
		"FERUN_EDGE_NAME": "edge.fecode.dev", "FERUN_DOMAIN_CHALLENGE_LABEL": "_fecode-challenge",
		"FERUN_DOMAIN_GRACE": "48h", "FERUN_DOMAIN_UNVERIFIED_MAX": "7", "FERUN_DNS_RESOLVER": "10.0.0.10:53",
		"FERUN_DOMAIN_RESERVED": "ignored.dev",
	}))
	if err != nil {
		t.Fatal(err)
	}
	wantAddrs := []netip.Addr{netip.MustParseAddr("194.11.242.65"), netip.MustParseAddr("2001:db8::1"),
		netip.MustParseAddr("10.0.0.1")}
	if c.EdgeName != "edge.fecode.dev" || !reflect.DeepEqual(c.EdgeAddresses, wantAddrs) ||
		c.DomainChallengeLabel != "_fecode-challenge" || c.DomainGrace != 48*time.Hour || c.DomainUnverifiedMax != 7 ||
		!reflect.DeepEqual(c.DomainReserved, []string{"fecode.dev", "fecore.dev"}) || c.DNSResolver != "10.0.0.10:53" {
		t.Fatalf("parsed %+v", c)
	}
	for _, bad := range [][]string{
		{"--edge-name", "Edge.Fecode.Dev"}, {"--edge-name", "10.0.0.1"}, {"--edge-address", "edge.fecode.dev"},
		{"--edge-address", "fe80::1%eth0"}, {"--domain-challenge-label", ""}, {"--domain-challenge-label", "-x"},
		{"--domain-challenge-label", "_a.b"}, {"--domain-challenge-label", "_Ferun"}, {"--domain-grace", "0s"},
		{"--domain-unverified-max", "0"}, {"--domain-reserved", "*.fecode.dev"}, {"--dns-resolver", "10.0.0.10"},
		{"--dns-resolver", "::1:53"},
	} {
		if _, err := Parse(append([]string{"--database-url", testURL}, bad...), env(nil)); err == nil ||
			!strings.Contains(err.Error(), strings.TrimPrefix(bad[0], "-")) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	var b strings.Builder
	Usage(&b, CmdServe)
	for _, f := range []string{"--edge-name (FERUN_EDGE_NAME)", "--edge-address (FERUN_EDGE_ADDRESS)",
		`--domain-challenge-label (FERUN_DOMAIN_CHALLENGE_LABEL, default "_ferun-challenge")`,
		`--domain-grace (FERUN_DOMAIN_GRACE, default "72h0m0s")`,
		`--domain-unverified-max (FERUN_DOMAIN_UNVERIFIED_MAX, default "50")`, "--domain-reserved (FERUN_DOMAIN_RESERVED)",
		"--dns-resolver (FERUN_DNS_RESOLVER)"} {
		if !strings.Contains(b.String(), f) {
			t.Errorf("serve help lacks %s", f)
		}
	}
}

// The sink's flags (ADR 0026): empty by default, from their variables,
// and what Validate refuses.
func TestSinkFlags(t *testing.T) {
	t.Parallel()
	c, err := Parse([]string{"--database-url", testURL}, env(nil))
	if err != nil || c.SinkEndpoint != "" || c.SinkURL != "" {
		t.Fatalf("defaults %q %q %v", c.SinkEndpoint, c.SinkURL, err)
	}
	c, err = Parse([]string{"--database-url", testURL, "--sink-url", "http://ferun-sink:8080"},
		env(map[string]string{"FERUN_SINK_ENDPOINT": "sink.fecode.dev:4317"}))
	if err != nil || c.SinkEndpoint != "sink.fecode.dev:4317" || c.SinkURL != "http://ferun-sink:8080" {
		t.Fatalf("parsed %q %q %v", c.SinkEndpoint, c.SinkURL, err)
	}
	for _, bad := range [][]string{
		{"--sink-endpoint", "sink.fecode.dev"}, {"--sink-endpoint", ":4317"}, {"--sink-endpoint", "::1:4317"},
		{"--sink-url", "ferun-sink:8080"}, {"--sink-url", "ftp://ferun-sink"}, {"--sink-url", "http://ferun-sink?x=1"},
	} {
		if _, err := Parse(append([]string{"--database-url", testURL}, bad...), env(nil)); err == nil ||
			!strings.Contains(err.Error(), strings.TrimPrefix(bad[0], "-")) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	var b strings.Builder
	Usage(&b, CmdServe)
	for _, f := range []string{"--sink-endpoint (FERUN_SINK_ENDPOINT)", "--sink-url (FERUN_SINK_URL)"} {
		if !strings.Contains(b.String(), f) {
			t.Errorf("serve help lacks %s", f)
		}
	}
}
