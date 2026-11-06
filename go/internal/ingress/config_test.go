// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ingress

import (
	"errors"
	"flag"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

// requiredEnv sets every required flag through its variable.
func requiredEnv() map[string]string {
	return map[string]string{
		"FERUN_INGRESS_NAME":               "ferun-ingress-0",
		"FERUN_INGRESS_CONTROL_PLANE":      "10.250.0.10:7443",
		"FERUN_INGRESS_CONTROL_PLANE_NAME": "ferun.run.fecode.dev",
		"FERUN_INGRESS_TRUST_DOMAIN":       "run.fecode.dev",
		"FERUN_INGRESS_LISTEN_HTTP":        "100.64.1.2:8080",
		"FERUN_INGRESS_LISTEN_HTTPS":       "100.64.1.2:8443",
		"FERUN_INGRESS_HEALTH_LISTEN":      "100.64.1.2:9090",
		"FERUN_INGRESS_TLS_CERT":           "/tls/tls.crt",
		"FERUN_INGRESS_TLS_KEY":            "/tls/tls.key",
		"FERUN_INGRESS_MESH_ENDPOINT":      "100.64.1.2:51820",
	}
}

func getenvOf(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func TestParseDefaultsAndEnv(t *testing.T) {
	t.Parallel()
	c, err := Parse(nil, getenvOf(requiredEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "ferun-ingress-0" || !slices.Equal(c.ControlPlane, []string{"10.250.0.10:7443"}) ||
		c.ControlPlaneName != "ferun.run.fecode.dev" || c.TrustDomain != "run.fecode.dev" ||
		c.ListenHTTP != "100.64.1.2:8080" || c.ListenHTTPS != "100.64.1.2:8443" || c.HealthListen != "100.64.1.2:9090" ||
		c.TLSCert != "/tls/tls.crt" || c.TLSKey != "/tls/tls.key" || c.MeshEndpoint != "100.64.1.2:51820" ||
		c.SinkEndpoint != "" {
		t.Fatalf("required settings %+v", c)
	}
	sinkEnv := requiredEnv()
	sinkEnv["FERUN_INGRESS_SINK_ENDPOINT"] = "ferun-sink-ingest.ferun.svc.cluster.local:4317"
	if c, err := Parse(nil, getenvOf(sinkEnv)); err != nil || c.SinkEndpoint != sinkEnv["FERUN_INGRESS_SINK_ENDPOINT"] {
		t.Fatalf("sink endpoint %q, %v", c.SinkEndpoint, err)
	}
	want := Default()
	if c.StateDir != "/var/lib/ferun-ingress" || c.JoinTokenFile != "" || c.MeshDevice != "ferun-mesh" ||
		c.MeshPort != 51820 || c.MeshMTU != 0 || c.WakeHold != 2*time.Minute || c.PendingHold != 10*time.Second ||
		c.MaxHeld != 1024 || c.UpstreamDialTimeout != 3*time.Second || c.UpstreamIdleTimeout != 4*time.Second ||
		c.MaxHeldPerTenant != 256 || c.MaxHeldPerEnv != 128 || c.MaxInflight != 2048 ||
		c.MaxInflightPerTenant != 512 || c.MaxInflightPerEnv != 256 ||
		c.UpstreamWaitTimeout != 10*time.Second || c.MemoryLimit != 0 ||
		c.MaxRequestBody != 64<<20 || c.MaxConnsPerEndpoint != 256 || c.ShutdownDelay != 10*time.Second ||
		c.ShutdownGrace != 40*time.Second || c.LogLevel != slog.LevelInfo || c.LogFormat != LogFormatJSON ||
		c.StateDir != want.StateDir {
		t.Fatalf("defaults %+v", c)
	}

	// A flag wins over its variable; every flag has one.
	env := requiredEnv()
	env["FERUN_INGRESS_SHUTDOWN_DELAY"] = "3s"
	env["FERUN_INGRESS_MAX_REQUEST_BODY"] = "1GiB"
	env["FERUN_INGRESS_LOG_LEVEL"] = "debug"
	env["FERUN_INGRESS_MESH_MTU"] = "1380"
	// The container's memory limit, as Kubernetes' resourceFieldRef
	// writes it: bytes.
	env["FERUN_INGRESS_MEMORY_LIMIT"] = "536870912"
	env["FERUN_INGRESS_MAX_INFLIGHT_PER_ENV"] = "64"
	env["FERUN_INGRESS_MAX_HELD_PER_TENANT"] = "32"
	c, err = Parse([]string{"--shutdown-delay", "5s", "--name", "ferun-ingress-1", "--log-format", "text",
		"--max-held-per-env", "8", "--max-inflight", "100", "--max-inflight-per-tenant", "80",
		"--upstream-wait-timeout", "5s"}, getenvOf(env))
	if err != nil {
		t.Fatal(err)
	}
	if c.ShutdownDelay != 5*time.Second || c.Name != "ferun-ingress-1" || c.MaxRequestBody != 1<<30 ||
		c.LogLevel != slog.LevelDebug || c.LogFormat != LogFormatText || c.MeshMTU != 1380 ||
		c.MemoryLimit != 512<<20 || c.MaxInflightPerEnv != 64 || c.MaxHeldPerEnv != 8 || c.MaxInflight != 100 ||
		c.MaxInflightPerTenant != 80 || c.MaxHeldPerTenant != 32 || c.UpstreamWaitTimeout != 5*time.Second {
		t.Fatalf("flags over variables %+v", c)
	}
	if EnvName("upstream-dial-timeout") != "FERUN_INGRESS_UPSTREAM_DIAL_TIMEOUT" {
		t.Fatal(EnvName("upstream-dial-timeout"))
	}

	if _, err := Parse([]string{"--help"}, getenvOf(requiredEnv())); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("--help: %v", err)
	}
	if _, err := Parse([]string{"extra"}, getenvOf(requiredEnv())); err == nil {
		t.Fatal("an argument was accepted")
	}
}

func TestParseRequired(t *testing.T) {
	t.Parallel()
	flags := map[string]string{
		"FERUN_INGRESS_NAME":               "--name",
		"FERUN_INGRESS_CONTROL_PLANE":      "--control-plane",
		"FERUN_INGRESS_CONTROL_PLANE_NAME": "--control-plane-name",
		"FERUN_INGRESS_TRUST_DOMAIN":       "--trust-domain",
		"FERUN_INGRESS_LISTEN_HTTP":        "--listen-http",
		"FERUN_INGRESS_LISTEN_HTTPS":       "--listen-https",
		"FERUN_INGRESS_HEALTH_LISTEN":      "--health-listen",
		"FERUN_INGRESS_TLS_CERT":           "--tls-cert",
		"FERUN_INGRESS_TLS_KEY":            "--tls-key",
		"FERUN_INGRESS_MESH_ENDPOINT":      "--mesh-endpoint",
	}
	for name, flagName := range flags {
		env := requiredEnv()
		delete(env, name)
		_, err := Parse(nil, getenvOf(env))
		if err == nil || !strings.Contains(err.Error(), flagName+" is required") {
			t.Errorf("without %s: %v", name, err)
		}
	}
	// Every error at once.
	_, err := Parse(nil, getenvOf(nil))
	for _, flagName := range flags {
		if err == nil || !strings.Contains(err.Error(), flagName+" is required") {
			t.Errorf("nothing set: %s missing from %v", flagName, err)
		}
	}
}

// The tenant zone's certificate (ADR 0031) is optional, its two files go
// together, from the flags or FERUN_INGRESS_TENANT_ZONE_TLS_*.
func TestParseTenantZoneTLS(t *testing.T) {
	t.Parallel()
	c, err := Parse(nil, getenvOf(requiredEnv()))
	if err != nil || c.TenantZoneTLSCert != "" || c.TenantZoneTLSKey != "" {
		t.Fatalf("default %q %q %v", c.TenantZoneTLSCert, c.TenantZoneTLSKey, err)
	}
	env := requiredEnv()
	env["FERUN_INGRESS_TENANT_ZONE_TLS_CERT"] = "/tenant-tls/tls.crt"
	env["FERUN_INGRESS_TENANT_ZONE_TLS_KEY"] = "/tenant-tls/tls.key"
	if c, err = Parse(nil, getenvOf(env)); err != nil || c.TenantZoneTLSCert != "/tenant-tls/tls.crt" ||
		c.TenantZoneTLSKey != "/tenant-tls/tls.key" {
		t.Fatalf("from the variables %q %q %v", c.TenantZoneTLSCert, c.TenantZoneTLSKey, err)
	}
	for _, args := range [][]string{{"--tenant-zone-tls-cert", "/a.crt"}, {"--tenant-zone-tls-key", "/a.key"}} {
		if _, err := Parse(args, getenvOf(requiredEnv())); err == nil || !strings.Contains(err.Error(), "go together") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestParseListen(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"0.0.0.0:8080", "[::]:8443", ":9090", "localhost:8080", "100.64.1.2", "fd00::1:8080", "*:80"} {
		_, err := Parse([]string{"--listen-https", addr}, getenvOf(requiredEnv()))
		if err == nil || !strings.Contains(err.Error(), "--listen-https") {
			t.Errorf("listen %q: %v", addr, err)
		}
	}
	if _, err := Parse([]string{"--health-listen", "[::]:9090"}, getenvOf(requiredEnv())); err == nil ||
		!strings.Contains(err.Error(), "never a wildcard") {
		t.Errorf("a wildcard health listener: %v", err)
	}
	for _, addr := range []string{"127.0.0.1:0", "[::1]:8443", "10.1.2.3:8080", "[fd00:1:2::5]:8443"} {
		if _, err := Parse([]string{"--listen-https", addr, "--listen-http", addr, "--health-listen", addr},
			getenvOf(requiredEnv())); err != nil {
			t.Errorf("listen %q: %v", addr, err)
		}
	}
}

func TestParseControlPlaneList(t *testing.T) {
	t.Parallel()
	c, err := Parse([]string{"--control-plane", " 10.250.0.10:7443, ferun-nodes.ferun:7443,,[fd00::1]:7443 "}, getenvOf(requiredEnv()))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.250.0.10:7443", "ferun-nodes.ferun:7443", "[fd00::1]:7443"}
	if !slices.Equal(c.ControlPlane, want) {
		t.Fatalf("control plane %q, want %q", c.ControlPlane, want)
	}
	// The flag replaces the variable's list; it does not add to it.
	env := requiredEnv()
	env["FERUN_INGRESS_CONTROL_PLANE"] = "a:1,b:2"
	if c, err = Parse([]string{"--control-plane", "c:3"}, getenvOf(env)); err != nil || !slices.Equal(c.ControlPlane, []string{"c:3"}) {
		t.Fatalf("flag over variable: %q %v", c.ControlPlane, err)
	}
	for _, list := range []string{"10.250.0.10", "a:1,b", ":7443", "a:0", "a:65536", "a:x", "fd00::1:7443", ","} {
		if _, err := Parse([]string{"--control-plane", list}, getenvOf(requiredEnv())); err == nil {
			t.Errorf("control plane %q accepted", list)
		}
	}
}

func TestParseSizesAndDurations(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int64{"64MiB": 64 << 20, "512": 512, "0": 0, "1 KiB": 1 << 10, "2GiB": 2 << 30} {
		c, err := Parse([]string{"--max-request-body", in}, getenvOf(requiredEnv()))
		if err != nil || c.MaxRequestBody != want {
			t.Errorf("--max-request-body %q: %d %v, want %d", in, c.MaxRequestBody, err, want)
		}
	}
	for _, in := range []string{"64MB", "12kb", "-1", "1.5MiB", "MiB", "99999999999GiB"} {
		if _, err := Parse([]string{"--max-request-body", in}, getenvOf(requiredEnv())); err == nil {
			t.Errorf("--max-request-body %q accepted", in)
		}
	}
	for _, name := range []string{"wake-hold", "pending-hold", "upstream-dial-timeout", "upstream-idle-timeout",
		"upstream-wait-timeout", "shutdown-delay", "shutdown-grace"} {
		for _, v := range []string{"0s", "-1s"} {
			_, err := Parse([]string{"--" + name, v}, getenvOf(requiredEnv()))
			if err == nil || !strings.Contains(err.Error(), "--"+name+" must be positive") {
				t.Errorf("--%s %s: %v", name, v, err)
			}
		}
	}
	for _, args := range [][]string{
		{"--max-held", "0"}, {"--max-conns-per-endpoint", "0"}, {"--mesh-port", "0"}, {"--mesh-port", "70000"},
		{"--max-held-per-env", "0"}, {"--max-inflight", "0"}, {"--max-inflight-per-env", "-1"}, {"--memory-limit", "-1"},
		{"--max-held-per-tenant", "0"}, {"--max-inflight-per-tenant", "0"},
		{"--upstream-wait-timeout", "2s"}, {"--upstream-dial-timeout", "11s"},
		{"--mesh-mtu", "1000"}, {"--mesh-device", "a-device-name-too-long"}, {"--mesh-device", "a/b"},
		{"--mesh-endpoint", "0.0.0.0:51820"}, {"--mesh-endpoint", "10.0.0.1"}, {"--name", "Ferun_Ingress"},
		{"--trust-domain", "https://x"}, {"--control-plane-name", "10.0.0.1"}, {"--log-format", "xml"},
		{"--log-level", "loud"}, {"--sink-endpoint", "ferun-sink-ingest"}, {"--sink-endpoint", ":4317"},
	} {
		if _, err := Parse(args, getenvOf(requiredEnv())); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
}

func TestParseStatusAndPrepare(t *testing.T) {
	t.Parallel()
	c, err := ParseStatus(nil, getenvOf(requiredEnv()))
	if err != nil || c.HealthListen != "100.64.1.2:9090" || c.Timeout != 5*time.Second {
		t.Fatalf("status %+v %v", c, err)
	}
	if c, err = ParseStatus([]string{"--health-listen", "127.0.0.1:9", "--timeout", "1s"}, getenvOf(requiredEnv())); err != nil ||
		c.HealthListen != "127.0.0.1:9" || c.Timeout != time.Second {
		t.Fatalf("status flags %+v %v", c, err)
	}
	if _, err := ParseStatus(nil, getenvOf(nil)); err == nil || !strings.Contains(err.Error(), "FERUN_INGRESS_HEALTH_LISTEN") {
		t.Fatalf("status without the listener: %v", err)
	}
	p, err := ParsePrepare([]string{"--log-format", "text"}, getenvOf(map[string]string{"FERUN_INGRESS_LOG_LEVEL": "warn"}))
	if err != nil || p.LogFormat != LogFormatText || p.LogLevel != slog.LevelWarn {
		t.Fatalf("prepare %+v %v", p, err)
	}
	if _, err := ParsePrepare([]string{"--log-format", "xml"}, nil); err == nil {
		t.Fatal("prepare accepted --log-format xml")
	}
	p, err = ParsePrepare([]string{"--join-token-source", "/join-secret/ferun-ingress-0"},
		getenvOf(map[string]string{"FERUN_INGRESS_JOIN_TOKEN_FILE": "/join/token"}))
	if err != nil || p.JoinTokenSource != "/join-secret/ferun-ingress-0" || p.JoinTokenFile != "/join/token" {
		t.Fatalf("prepare's join token %+v %v", p, err)
	}
	if _, err := ParsePrepare([]string{"--join-token-source", "/join-secret/x"}, nil); err == nil ||
		!strings.Contains(err.Error(), "--join-token-file") {
		t.Fatalf("a source without a file: %v", err)
	}
}

func TestUsage(t *testing.T) {
	t.Parallel()
	for cmd, want := range map[string]string{
		"": "status", CmdServe: "-max-request-body", CmdStatus: "-health-listen", CmdPrepare: "-log-format",
	} {
		var b strings.Builder
		Usage(&b, cmd)
		if !strings.Contains(b.String(), want) {
			t.Errorf("usage of %q lacks %q:\n%s", cmd, want, b.String())
		}
	}
}
