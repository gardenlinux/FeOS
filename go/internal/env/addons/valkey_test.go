// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package addons

import (
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/model"
)

func TestValkeyVersionsPlans(t *testing.T) {
	var v valkey
	if b, ok := Get("valkey"); !ok || b != (valkey{}) {
		t.Fatalf("Get(valkey) = %v, %v", b, ok)
	}
	if got := v.Versions(); !slices.Equal(got, []string{"9"}) {
		t.Fatal(got)
	}
	want := map[string]Plan{
		"dev":    {VCPUs: 1, MemoryMib: 128, VolumeMib: 256},
		"small":  {VCPUs: 1, MemoryMib: 512, VolumeMib: 2048},
		"medium": {VCPUs: 1, MemoryMib: 2048, VolumeMib: 8192},
		"large":  {VCPUs: 2, MemoryMib: 8192, VolumeMib: 32768},
	}
	if got := v.Plans(); !reflect.DeepEqual(got, want) {
		t.Fatalf("plans %v, want %v", got, want)
	}
	if v.DefaultPlan() != "small" {
		t.Fatal(v.DefaultPlan())
	}
	// No user and no password: the workload gets in by its certificate
	// (ADR 0039).
	if k := v.OutputKeys(); !slices.Equal(k, []string{"host", "port", "url"}) {
		t.Fatal(k)
	}
	if c := v.Credentials(addon(t, "cache", `{"type":"valkey","version":"9"}`)); !slices.Equal(c, []string{TLSKey}) {
		t.Fatal(c)
	}
	// Without its certificate it is provisioned with nothing.
	if s := v.Provision(addon(t, "cache", `{"type":"valkey","version":"9"}`), Context{}); s != nil {
		t.Fatal(s)
	}
	var b Blueprint = v
	cs, ok := b.(CertificateServer)
	if !ok || !cs.ProvisionsAsItself() || cs.GrantedRole(Ref{Key: "url"}) != "data" {
		t.Fatalf("valkey is a CertificateServer that provisions as itself and grants data: %v", ok)
	}
	if _, ok := b.(TLSBinder); !ok {
		t.Fatal("valkey is reached through a TLS binding")
	}
	if (postgres{}).ProvisionsAsItself() {
		t.Fatal("postgres provisions over its socket")
	}
}

func TestValkeyValidate(t *testing.T) {
	cases := []struct {
		name     string
		prev     *Addon
		next     string
		want     []string
		problems []string
	}{
		{name: "9", next: `{"type":"valkey","version":"9"}`},
		{name: "9 dev", next: `{"type":"valkey","version":"9","plan":"dev"}`},
		{name: "policy", next: `{"type":"valkey","version":"9","options":{"maxmemoryPolicy":"allkeys-lru"}}`},
		{name: "no version", next: `{"type":"valkey"}`, want: []string{"error version"}},
		{name: "unknown version", next: `{"type":"valkey","version":"7"}`,
			want: []string{"error version"}, problems: []string{`no version "7"`}},
		{name: "9.0 is no version", next: `{"type":"valkey","version":"9.0"}`,
			want: []string{"error version"}, problems: []string{`no version "9.0"`}},
		// The retired versions run 9 and say so; nothing is refused.
		{name: "8 retired", next: `{"type":"valkey","version":"8"}`,
			want: []string{"warning version"}, problems: []string{"valkey 8 is retired", "runs 9", "ADR 0039"}},
		{name: "7.2 retired", next: `{"type":"valkey","version":"7.2","plan":"dev"}`,
			want: []string{"warning version"}, problems: []string{"valkey 7.2 is retired"}},
		{name: "8 unchanged", prev: ptr(addon(t, "cache", `{"type":"valkey","version":"8"}`)),
			next: `{"type":"valkey","version":"8"}`, want: []string{"warning version"}},
		{name: "8 to 9", prev: ptr(addon(t, "cache", `{"type":"valkey","version":"8"}`)),
			next: `{"type":"valkey","version":"9"}`},
		{name: "7.2 to 8", prev: ptr(addon(t, "cache", `{"type":"valkey","version":"7.2"}`)),
			next: `{"type":"valkey","version":"8"}`, want: []string{"warning version"}},
		// 9's data under a retired version's name: the same 9 runs.
		{name: "9 to 8", prev: ptr(addon(t, "cache", `{"type":"valkey","version":"9"}`)),
			next: `{"type":"valkey","version":"8"}`, want: []string{"warning version"}},
		{name: "unknown plan", next: `{"type":"valkey","version":"9","plan":"tiny"}`, want: []string{"error plan"}},
		{name: "unknown policy", next: `{"type":"valkey","version":"9","options":{"maxmemoryPolicy":"lru"}}`,
			want: []string{"error options.maxmemoryPolicy"}, problems: []string{`no maxmemory policy "lru"`}},
		{name: "policy not a string", next: `{"type":"valkey","version":"9","options":{"maxmemoryPolicy":1}}`,
			want: []string{"error options.maxmemoryPolicy"}, problems: []string{"want a string, not a number"}},
		{name: "unknown option", next: `{"type":"valkey","version":"9","options":{"maxmemory":"1gb"}}`,
			want: []string{"error options.maxmemory"}},
		{name: "unchanged", prev: ptr(addon(t, "cache", `{"type":"valkey","version":"9"}`)),
			next: `{"type":"valkey","version":"9","plan":"small"}`},
		{name: "plan change", prev: ptr(addon(t, "cache", `{"type":"valkey","version":"9","plan":"dev"}`)),
			next: `{"type":"valkey","version":"9","plan":"large"}`,
			want: []string{"warning plan"}, problems: []string{"from dev to large", "large's 32768 MiB is not applied"}},
		{name: "plan change of a retired version", prev: ptr(addon(t, "cache", `{"type":"valkey","version":"8","plan":"dev"}`)),
			next: `{"type":"valkey","version":"8","plan":"small"}`, want: []string{"warning version", "warning plan"}},
		{name: "from another type", prev: ptr(addon(t, "cache", `{"type":"postgres","version":"17","plan":"dev"}`)),
			next: `{"type":"valkey","version":"9"}`},
	}
	for _, p := range valkeyPolicies {
		cases = append(cases, struct {
			name     string
			prev     *Addon
			next     string
			want     []string
			problems []string
		}{name: p, next: `{"type":"valkey","version":"9","options":{"maxmemoryPolicy":"` + p + `"}}`})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkFindings(t, valkey{}.Validate(c.prev, addon(t, "cache", c.next)), c.want, c.problems...)
		})
	}
	if len(valkeyPolicies) != 8 {
		t.Fatalf("%d policies", len(valkeyPolicies))
	}
}

func TestValkeyRender(t *testing.T) {
	probe := []string{"sh", "-c", `valkey-cli -s /run/valkey/valkey.sock ping 2>&1 | grep -qE '^(PONG|NOAUTH )'`}
	cases := []struct {
		addon, tail string
	}{
		{`{"type":"valkey","version":"9","plan":"dev"}`, " --maxmemory 89mb --maxmemory-policy noeviction"},
		{`{"type":"valkey","version":"9"}`, " --maxmemory 358mb --maxmemory-policy noeviction"},
		// A retired version runs 9, exactly as 9 does.
		{`{"type":"valkey","version":"8"}`, " --maxmemory 358mb --maxmemory-policy noeviction"},
		{`{"type":"valkey","version":"7.2","plan":"medium","options":{"maxmemoryPolicy":"volatile-ttl"}}`,
			" --maxmemory 1433mb --maxmemory-policy volatile-ttl"},
		{`{"type":"valkey","version":"9","plan":"large","options":{"maxmemoryPolicy":"allkeys-lru"}}`,
			" --maxmemory 5734mb --maxmemory-policy allkeys-lru"},
	}
	for _, c := range cases {
		t.Run(c.addon, func(t *testing.T) {
			got, err := valkey{}.Render(addon(t, "cache", c.addon), Context{Env: "stage", Name: "cache", Address: "fd6b::7"})
			if err != nil {
				t.Fatal(err)
			}
			if got.Image != "valkey/valkey:9-alpine" || got.Port != 6379 || got.Mount != "/data" ||
				!slices.Equal(got.Probe, probe) || len(got.Env) != 0 {
				t.Fatalf("render %#v", got)
			}
			// The image's entrypoint, tini and docker-entrypoint.sh, which
			// drops privileges, around a start of the blueprint's.
			if len(got.Command) != 5 || !slices.Equal(got.Command[:4], []string{"tini", "--", "sh", "-c"}) {
				t.Fatalf("command %q", got.Command)
			}
			start := got.Command[4]
			for _, want := range []string{
				"exec docker-entrypoint.sh valkey-server --port 0 --protected-mode yes " +
					"--unixsocket /run/valkey/valkey.sock --unixsocketperm 700 --aclfile \"$f/users.acl\" --appendonly yes" +
					c.tail + ` "$@"`,
				"set -- --tls-port 6379 --tls-cert-file /data/ferun/server.crt --tls-key-file /data/ferun/server.key " +
					"--tls-ca-cert-file /data/ferun/ca.crt --tls-auth-clients yes --tls-auth-clients-user URI --tls-protocols TLSv1.3\n",
				`printf '%s\n' 'user default on nopass ~* &* +@all' >"$f/users.acl"`,
			} {
				if !strings.Contains(start, want) {
					t.Errorf("the start has not %q:\n%s", want, start)
				}
			}
			// No password, and no plaintext TCP.
			for _, never := range []string{"requirepass", "--port 6379", "VALKEY_PASSWORD"} {
				if strings.Contains(start, never) {
					t.Errorf("the start has %q:\n%s", never, start)
				}
			}
		})
	}
	for _, bad := range []string{`{"type":"valkey"}`, `{"type":"valkey","version":"10"}`, `{"type":"valkey","version":"9.0"}`,
		`{"type":"valkey","version":"9","plan":"huge"}`,
		`{"type":"valkey","version":"9","options":{"maxmemoryPolicy":"lru"}}`,
		`{"type":"valkey","version":"9","options":{"x":1}}`} {
		if _, err := (valkey{}).Render(addon(t, "cache", bad), Context{}); err == nil {
			t.Errorf("Render(%s) did not fail", bad)
		}
	}
}

func TestValkeyOutputs(t *testing.T) {
	a := addon(t, "cache", `{"type":"valkey","version":"9"}`)
	bound := Context{Env: "stage", Name: "cache", Address: "fd6b:59f2:b4a2:3::7", Loopback: "127.64.0.2"}
	// A reference's validation has no binding: the first loopback.
	none := Context{Env: "stage", Name: "cache"}
	cases := []struct {
		name string
		c    Context
		key  string
		want string
	}{
		{"host", bound, "host", "127.64.0.2"},
		{"host without a binding", none, "host", "127.64.0.1"},
		{"port", bound, "port", "6379"},
		{"url", bound, "url", "redis://127.64.0.2:6379/0"},
		{"url without a binding", none, "url", "redis://127.64.0.1:6379/0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := valkey{}.Outputs(a, c.c, Ref{Key: c.key})
			if err != nil {
				t.Fatal(err)
			}
			if want := []model.TemplatePart{{Value: c.want}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("outputs %s, want %s", jsonOf(t, got), jsonOf(t, want))
			}
		})
	}
	for _, key := range []string{"user", "password"} {
		_, err := valkey{}.Outputs(a, bound, Ref{Key: key})
		if err == nil || !strings.Contains(err.Error(), "by its certificate") || !strings.Contains(err.Error(), "ADR 0039") {
			t.Fatalf("%s: %v", key, err)
		}
	}
	for _, bad := range []Ref{{}, {Key: "database"}, {Key: "url", Fields: map[string]string{"role": "x"}}} {
		if _, err := (valkey{}).Outputs(a, bound, bad); err == nil {
			t.Errorf("Outputs(%v) did not fail", bad)
		}
	}
	_, err := valkey{}.Outputs(a, bound, Ref{Key: "url", Fields: map[string]string{"db": "1"}})
	if err == nil || !strings.Contains(err.Error(), "takes only addon and key") {
		t.Fatalf("error %v", err)
	}
	// What common clients parse: no user, no password, plaintext to the
	// loopback address, database 0.
	parts, _ := valkey{}.Outputs(a, bound, Ref{Key: "url"})
	u, err := url.Parse(resolveParts(t, parts, nil))
	if err != nil || u.Scheme != "redis" || u.User != nil || u.Hostname() != "127.64.0.2" || u.Port() != "6379" ||
		u.Path != "/0" {
		t.Fatalf("%v parses to %#v", err, u)
	}
	// Every reference goes through the binding, with no ALPN.
	for _, key := range []string{"host", "port", "url"} {
		if port, alpn, ok := (valkey{}).TLSBinding(Ref{Key: key}); !ok || port != 6379 || alpn != "" {
			t.Fatalf("TLSBinding(%s) = %d, %q, %v", key, port, alpn, ok)
		}
	}
}

func TestValkeyProvision(t *testing.T) {
	a := addon(t, "cache", `{"type":"valkey","version":"9"}`)
	const (
		admin   = "spiffe://run.test/tenant/t/env/01a196e9-4ac8-71a8-94d7-f6aa5688b1fc/addon/cache"
		catalog = "spiffe://run.test/tenant/t/env/01a196e9-4ac8-71a8-94d7-f6aa5688b1fc/service/catalog"
		sandbox = "spiffe://run.test/tenant/t/env/01a196e9-4ac8-71a8-94d7-f6aa5688b1fc/sandbox/xmxfgckmkt6"
	)
	tls := &TLS{CertPEM: "CHAIN", CAPEM: "ROOTS", ID: admin, Grants: []Grant{
		{Workload: "wl-0123456789abcdef0123456789abcdef", ID: catalog, Role: "data"},
		{Workload: "wl-s-xmxfgckmkt6", ID: sandbox, Role: "data"},
		// What valkey never grants: another role, a name with a space or
		// a line break, the same ID twice, the addon's own ID.
		{Workload: "wl-11111111111111111111111111111111", ID: catalog + "x", Role: "admin"},
		{Workload: "wl-22222222222222222222222222222222", ID: catalog + " +@all", Role: "data"},
		{Workload: "wl-33333333333333333333333333333333", ID: catalog + "\nuser default on", Role: "data"},
		{Workload: "wl-0123456789abcdef0123456789abcdef", ID: catalog, Role: "data"},
		{Workload: "wl-44444444444444444444444444444444", ID: admin, Role: "data"},
	}}
	steps := valkey{}.Provision(a, Context{Env: "stage", Name: "cache", TLS: tls})
	if len(steps) != 1 || steps[0].Name != "tls" || !slices.Equal(steps[0].Argv[:2], []string{"sh", "-c"}) {
		t.Fatalf("steps %+v", steps)
	}
	env := steps[0].Env
	for k, want := range map[string]model.TemplateValue{"FERUN_TLS_KEY": model.CredentialValue(TLSKey),
		"FERUN_TLS_CERT": model.Literal("CHAIN"), "FERUN_TLS_CA": model.Literal("ROOTS"), "FERUN_VALKEY_ID": model.Literal(admin)} {
		if !reflect.DeepEqual(env[k], want) {
			t.Errorf("%s = %+v, want %+v", k, env[k], want)
		}
	}
	want := "user default off resetpass resetkeys resetchannels -@all\n" +
		"user " + admin + " on resetpass ~* &* +@all\n" +
		"user " + catalog + " on resetpass ~* &* +@all -@admin -migrate\n" +
		"user " + sandbox + " on resetpass ~* &* +@all -@admin -migrate\n"
	if got := env["FERUN_VALKEY_ACL"].Value; got != want {
		t.Fatalf("users\n%s\nwant\n%s", got, want)
	}
	if acl := valkeyACL(admin, nil); strings.Contains(acl, "nopass") || strings.Count(acl, "\n") != 2 {
		t.Fatalf("users without grants:\n%s", acl)
	}
	script := steps[0].Argv[2]
	for _, want := range []string{
		"on sock CONFIG SET tls-cert-file /data/ferun/server.crt tls-key-file /data/ferun/server.key " +
			"tls-ca-cert-file /data/ferun/ca.crt tls-auth-clients yes tls-auth-clients-user URI tls-protocols TLSv1.3\n" +
			"  on sock CONFIG SET tls-port 6379\n  on sock ACL LOAD\n",
		"on tls CONFIG SET tls-cert-file", "on tls ACL LOAD\n",
		`if [ "$(tls ACL WHOAMI)" != "$FERUN_VALKEY_ID" ]`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the step has not %q:\n%s", want, script)
		}
	}
	// The step's TLS settings are the start's.
	inst, _ := valkey{}.Render(a, Context{})
	for _, kv := range valkeyTLS {
		if !strings.Contains(inst.Command[4], " --"+kv[0]+" "+kv[1]) || !strings.Contains(script, " "+kv[0]+" "+kv[1]) {
			t.Errorf("%s %s is not in both", kv[0], kv[1])
		}
	}
	// An ID that cannot be a user locks nothing: the step fails, and
	// writes nothing.
	bad := *tls
	bad.ID = "spiffe://run.test/x y"
	steps = valkey{}.Provision(a, Context{TLS: &bad})
	if len(steps) != 1 || strings.Contains(steps[0].Argv[2], "users.acl") || !strings.Contains(steps[0].Argv[2], "exit 1") {
		t.Fatalf("steps for a bad ID %+v", steps)
	}
}
