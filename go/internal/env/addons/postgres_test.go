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

func TestPostgresVersionsPlans(t *testing.T) {
	var p postgres
	if b, ok := Get("postgres"); !ok || b != (postgres{}) {
		t.Fatalf("Get(postgres) = %v, %v", b, ok)
	}
	if v := p.Versions(); !slices.Equal(v, []string{"16", "17"}) {
		t.Fatal(v)
	}
	want := map[string]Plan{
		"dev":    {VCPUs: 1, MemoryMib: 256, VolumeMib: 1024},
		"small":  {VCPUs: 1, MemoryMib: 1024, VolumeMib: 10240},
		"medium": {VCPUs: 2, MemoryMib: 4096, VolumeMib: 51200},
		"large":  {VCPUs: 4, MemoryMib: 16384, VolumeMib: 204800},
	}
	if got := p.Plans(); !reflect.DeepEqual(got, want) {
		t.Fatalf("plans %v, want %v", got, want)
	}
	p.Plans()["dev"] = Plan{} // a caller's change stays its own
	if p.Plans()["dev"].MemoryMib != 256 {
		t.Fatal("Plans shares its map")
	}
	if p.DefaultPlan() != "small" {
		t.Fatal(p.DefaultPlan())
	}
	if k := p.OutputKeys(); !slices.Equal(k, []string{"host", "port", "user", "database", "url"}) {
		t.Fatal(k)
	}
}

func TestPostgresValidate(t *testing.T) {
	const full = `{"type":"postgres","version":"17","plan":"small",
		"options":{"databases":["expensa","reports"],"roles":["expense_svc","approval_svc"]}}`
	long := strings.Repeat("a", 63)
	cases := []struct {
		name     string
		prev     *Addon
		next     string
		addon    string // the addon's name; "" is "postgres"
		want     []string
		problems []string
	}{
		{name: "minimal", next: `{"type":"postgres","version":"17"}`},
		{name: "full", next: full},
		{name: "16 dev", next: `{"type":"postgres","version":"16","plan":"dev"}`},
		{name: "options null", next: `{"type":"postgres","version":"17","options":null}`},
		{name: "lists null", next: `{"type":"postgres","version":"17","options":{"databases":null,"roles":null}}`},
		{name: "databases only", next: `{"type":"postgres","version":"17","options":{"databases":["a","b_2"]}}`},
		{name: "no version", next: `{"type":"postgres"}`,
			want: []string{"error version"}, problems: []string{"needs a version"}},
		{name: "unknown version", next: `{"type":"postgres","version":"15"}`,
			want: []string{"error version"}, problems: []string{`no version "15"`}},
		{name: "unknown plan", next: `{"type":"postgres","version":"17","plan":"huge"}`,
			want: []string{"error plan"}, problems: []string{`no plan "huge"`}},
		{name: "unknown option", next: `{"type":"postgres","version":"17","options":{"users":["x"],"database":"y"}}`,
			want: []string{"error options.database", "error options.users"}},
		{name: "databases not a list", next: `{"type":"postgres","version":"17","options":{"databases":"expensa"}}`,
			want: []string{"error options.databases"}, problems: []string{"want a list of names"}},
		{name: "element not a string",
			next: `{"type":"postgres","version":"17","options":{"databases":["a",5,true],"roles":[{"x":1}]}}`,
			want: []string{"error options.databases[1]", "error options.databases[2]", "error options.roles[0]"}},
		{name: "bad names",
			next: `{"type":"postgres","version":"17","options":{"databases":["Expensa","1db","my-db","` + long + `x",""],
				"roles":["Svc","a.b"]}}`,
			want: []string{"error options.databases[0]", "error options.databases[1]", "error options.databases[2]",
				"error options.databases[3]", "error options.databases[4]", "error options.roles[0]",
				"error options.roles[1]"},
			problems: []string{`database name "Expensa" is not a lowercase identifier`, `role name "a.b"`}},
		{name: "longest name", next: `{"type":"postgres","version":"17","options":{"databases":["` + long + `"]}}`},
		{name: "reserved databases",
			next:     `{"type":"postgres","version":"17","options":{"databases":["template0","template1","postgres","ok"]}}`,
			want:     []string{"error options.databases[0]", "error options.databases[1]", "error options.databases[2]"},
			problems: []string{"database name template0 is reserved"}},
		{name: "reserved roles",
			next: `{"type":"postgres","version":"17","options":{"databases":["d"],"roles":["postgres","public","none",
				"current_user","current_role","session_user","information_schema","pg_monitor","pg_x","ok","pgx"]}}`,
			want: []string{"error options.roles[0]", "error options.roles[1]", "error options.roles[2]",
				"error options.roles[3]", "error options.roles[4]", "error options.roles[5]", "error options.roles[6]",
				"error options.roles[7]", "error options.roles[8]"},
			problems: []string{"role name postgres is reserved", "role name pg_monitor is reserved"}},
		{name: "duplicates",
			next:     `{"type":"postgres","version":"17","options":{"databases":["a","b","a"],"roles":["r","r"]}}`,
			want:     []string{"error options.databases[2]", "error options.roles[1]"},
			problems: []string{"database a is listed twice", "role r is listed twice"}},
		{name: "indexes count every element",
			next:     `{"type":"postgres","version":"17","options":{"databases":["a",5,"a","B"]}}`,
			want:     []string{"error options.databases[1]", "error options.databases[2]", "error options.databases[3]"},
			problems: []string{"want a database name, not a number", "database a is listed twice"}},
		{name: "roles without databases", next: `{"type":"postgres","version":"17","options":{"roles":["svc"]}}`,
			want: []string{"error options.roles"}, problems: []string{"roles need a database"}},
		{name: "roles with empty databases",
			next: `{"type":"postgres","version":"17","options":{"databases":[],"roles":["svc"]}}`,
			want: []string{"error options.roles"}},
		// Roles have no credential (ADR 0038), so a long role name of a
		// long addon is fine.
		{name: "long names", addon: long,
			next: `{"type":"postgres","version":"17","options":{"databases":["d"],"roles":["` + long[:43] + `","` + long[:44] + `"]}}`},
		{name: "major upgrade", prev: ptr(addon(t, "postgres", `{"type":"postgres","version":"16"}`)),
			next: `{"type":"postgres","version":"17"}`,
			want: []string{"error version"}, problems: []string{"from 16 to 17 needs a data migration"}},
		{name: "major downgrade", prev: ptr(addon(t, "postgres", `{"type":"postgres","version":"17"}`)),
			next: `{"type":"postgres","version":"16"}`,
			want: []string{"error version"}, problems: []string{"from 17 to 16"}},
		{name: "unchanged", prev: ptr(addon(t, "postgres", full)), next: full},
		{name: "to an unknown version", prev: ptr(addon(t, "postgres", `{"type":"postgres","version":"17"}`)),
			next: `{"type":"postgres","version":"18"}`, want: []string{"error version"}},
		{name: "from another type", prev: ptr(addon(t, "postgres", `{"type":"valkey","version":"8"}`)),
			next: `{"type":"postgres","version":"17","plan":"dev"}`},
		{name: "plan change", prev: ptr(addon(t, "postgres", `{"type":"postgres","version":"17","plan":"small"}`)),
			next: `{"type":"postgres","version":"17","plan":"medium"}`,
			want: []string{"warning plan"}, problems: []string{"from small to medium", "medium's 51200 MiB is not applied"}},
		{name: "default plan is small", prev: ptr(addon(t, "postgres", `{"type":"postgres","version":"17"}`)),
			next: `{"type":"postgres","version":"17","plan":"small"}`},
		{name: "plan change and major", prev: ptr(addon(t, "postgres", `{"type":"postgres","version":"16","plan":"dev"}`)),
			next: `{"type":"postgres","version":"17"}`, want: []string{"warning plan", "error version"}},
		{name: "options grow", prev: ptr(addon(t, "postgres", `{"type":"postgres","version":"17","options":{"databases":["expensa"]}}`)),
			next: full},
		{name: "options shrink", prev: ptr(addon(t, "postgres", full)),
			next:     `{"type":"postgres","version":"17","plan":"small","options":{"databases":["expensa"],"roles":["approval_svc"]}}`,
			want:     []string{"warning options.databases", "warning options.roles"},
			problems: []string{"database reports is no longer listed", "role expense_svc is no longer listed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name := c.addon
			if name == "" {
				name = "postgres"
			}
			checkFindings(t, postgres{}.Validate(c.prev, addon(t, name, c.next)), c.want, c.problems...)
		})
	}
}

func TestPostgresRender(t *testing.T) {
	env := `{"PGDATA":{"value":"/var/lib/postgresql/data/pgdata"},` +
		`"POSTGRES_INITDB_ARGS":{"value":"--auth-host=scram-sha-256"},` +
		`"POSTGRES_PASSWORD":{"parts":[{"credential":"password"}]}}`
	probe := []string{"sh", "-c", "pg_isready -q -h /var/run/postgresql -U postgres && nc -z -w 2 127.0.0.1 5432"}
	cases := []struct {
		addon, image, buffers string
	}{
		{`{"type":"postgres","version":"16","plan":"dev"}`, "postgres:16-alpine", "64MB"},
		{`{"type":"postgres","version":"17"}`, "postgres:17-alpine", "256MB"},
		{`{"type":"postgres","version":"17","plan":"medium"}`, "postgres:17-alpine", "1024MB"},
		{`{"type":"postgres","version":"17","plan":"large","options":{"databases":["d"],"roles":["r"]}}`,
			"postgres:17-alpine", "4096MB"},
	}
	for _, c := range cases {
		t.Run(c.addon, func(t *testing.T) {
			a := addon(t, "postgres", c.addon)
			got, err := postgres{}.Render(a, Context{Env: "stage", Name: "postgres", Address: "fd6b::5"})
			if err != nil {
				t.Fatal(err)
			}
			want := Instance{
				Image:   c.image,
				Command: []string{"docker-entrypoint.sh", "postgres", "-c", "shared_buffers=" + c.buffers},
				Env: map[string]model.TemplateValue{
					"PGDATA":               model.Literal("/var/lib/postgresql/data/pgdata"),
					"POSTGRES_PASSWORD":    model.CredentialValue("password"),
					"POSTGRES_INITDB_ARGS": model.Literal("--auth-host=scram-sha-256"),
				},
				Port:  5432,
				Mount: "/var/lib/postgresql/data",
				Probe: probe,
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("render\n%#v\nwant\n%#v", got, want)
			}
			if j := jsonOf(t, got.Env); j != env {
				t.Fatalf("env %s, want %s", j, env)
			}
			// Options are provisioned, never rendered: adding a role
			// must not recreate the pod.
			a.Options = nil
			bare, _ := postgres{}.Render(a, Context{})
			if !reflect.DeepEqual(bare, got) {
				t.Fatal("options or the context changed the rendering")
			}
		})
	}
	for _, bad := range []string{`{"type":"postgres"}`, `{"type":"postgres","version":"15"}`,
		`{"type":"postgres","version":"17","plan":"huge"}`} {
		if _, err := (postgres{}).Render(addon(t, "postgres", bad), Context{}); err == nil {
			t.Errorf("Render(%s) did not fail", bad)
		}
	}
}

func TestPostgresCredentials(t *testing.T) {
	// The superuser's password (initdb's, removed from the role) and the
	// server's key; roles have none (ADR 0038).
	cases := map[string][]string{
		`{"type":"postgres","version":"17"}`: {"password", "tls.key"},
		`{"type":"postgres","version":"17","options":{"databases":["d"],"roles":["expense_svc","approval_svc"]}}`: {
			"password", "tls.key"},
		`{"type":"postgres","version":"16","options":{"databases":["d"],"roles":["r","r"]}}`: {"password", "tls.key"},
	}
	for raw, want := range cases {
		if got := (postgres{}).Credentials(addon(t, "postgres", raw)); !slices.Equal(got, want) {
			t.Errorf("Credentials(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestPostgresOutputs(t *testing.T) {
	full := addon(t, "db", `{"type":"postgres","version":"17",
		"options":{"databases":["expensa","reports"],"roles":["expense_svc","approval_svc"]}}`)
	v6 := Context{Env: "stage", Name: "db", Address: "fd6b:59f2:b4a2:3::5"}
	none := Context{Env: "stage", Name: "db"}
	lit := func(s string) []model.TemplatePart { return []model.TemplatePart{{Value: s}} }
	role := func(r string, more ...string) map[string]string {
		f := map[string]string{"role": r}
		if len(more) > 0 {
			f["database"] = more[0]
		}
		return f
	}
	cases := []struct {
		name   string
		a      Addon
		c      Context
		key    string
		fields map[string]string
		want   []model.TemplatePart
	}{
		{"host", full, v6, "host", role("expense_svc"), lit("fd6b:59f2:b4a2:3::5")},
		{"host without address", full, none, "host", role("expense_svc"), lit("db")},
		{"port", full, v6, "port", role("expense_svc"), lit("5432")},
		{"user", full, v6, "user", role("expense_svc"), lit("expense_svc")},
		{"database is the first", full, v6, "database", role("expense_svc"), lit("expensa")},
		{"database field", full, v6, "database", role("expense_svc", "reports"), lit("reports")},
		// No password, since there is none; TLS asked for, which the
		// workload's certificate completes (ADR 0038).
		{"url", full, v6, "url", role("expense_svc", "reports"),
			lit("postgres://expense_svc@[fd6b:59f2:b4a2:3::5]:5432/reports?sslmode=require")},
		{"url without address", full, none, "url", role("approval_svc"),
			lit("postgres://approval_svc@db:5432/expensa?sslmode=require")},
		{"options with problems", addon(t, "db", `{"type":"postgres","version":"17","options":{"databases":["d"],"roles":["svc","pg_x"]}}`),
			v6, "user", role("pg_x"), lit("pg_x")},
		{"url with an IPv4 address", full, Context{Name: "db", Address: "10.0.0.5"}, "url", role("approval_svc"),
			lit("postgres://approval_svc@10.0.0.5:5432/expensa?sslmode=require")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := postgres{}.Outputs(c.a, c.c, Ref{Key: c.key, Fields: c.fields})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("outputs %s, want %s", jsonOf(t, got), jsonOf(t, c.want))
			}
			if (model.TemplateValue{Parts: got}).HasCredential() {
				t.Fatal("an output names a credential")
			}
		})
	}
	bad := []struct {
		name   string
		a      Addon
		key    string
		fields map[string]string
		says   string
	}{
		{"no key", full, "", role("expense_svc"), "needs a key"},
		{"unknown key", full, "uri", role("expense_svc"), `no key "uri"`},
		{"a password", full, "password", role("expense_svc"), `no key "password"`},
		{"unknown field", full, "url", map[string]string{"role": "expense_svc", "schema": "x"}, `no field "schema"`},
		{"no role", full, "url", nil, "the superuser is the platform's"},
		{"no role for the host", full, "host", nil, "names a role"},
		{"unknown role", full, "url", role("nobody"), `no role "nobody"`},
		{"empty role", full, "user", role(""), `no role ""`},
		{"unknown database", full, "url", role("expense_svc", "nope"), `no database "nope"`},
		{"role in postgres", full, "url", role("expense_svc", "postgres"),
			`gives role expense_svc no database "postgres"`},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, err := postgres{}.Outputs(c.a, v6, Ref{Key: c.key, Fields: c.fields})
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("error %v, want one that says %q", err, c.says)
			}
		})
	}
	if r := (postgres{}).GrantedRole(Ref{Key: "url", Fields: role("expense_svc")}); r != "expense_svc" {
		t.Errorf("GrantedRole = %q", r)
	}
	if r := (postgres{}).GrantedRole(Ref{Key: "host"}); r != "" {
		t.Errorf("GrantedRole without a role = %q", r)
	}
}

// TestPostgresURLParses reads the url back with net/url: no password,
// TLS required.
func TestPostgresURLParses(t *testing.T) {
	a := addon(t, "db", `{"type":"postgres","version":"17","options":{"databases":["expensa"],"roles":["svc"]}}`)
	parts, err := postgres{}.Outputs(a, Context{Name: "db", Address: "fd6b::5"},
		Ref{Key: "url", Fields: map[string]string{"role": "svc"}})
	if err != nil {
		t.Fatal(err)
	}
	raw := resolveParts(t, parts, nil)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	_, hasPassword := u.User.Password()
	if u.Scheme != "postgres" || u.User.Username() != "svc" || hasPassword || u.Hostname() != "fd6b::5" ||
		u.Port() != "5432" || u.Path != "/expensa" || u.Query().Get("sslmode") != "require" {
		t.Fatalf("%s parses to %#v", raw, u)
	}
}

// The script of the spike's verified run (.claude/caas/spike-addons.md
// §1): databases expensa, reports; roles expense_svc, approval_svc. It is
// kept as the report has it; provisioning runs it with the owner checks
// added (withOwnerChecks).
const postgresSpikeScript = `exec psql -X -q -v ON_ERROR_STOP=1 -h 127.0.0.1 -p 5432 -U postgres -d postgres -v d0=expensa -v d1=reports -v r0=expense_svc -v r1=approval_svc <<'SQL'
\getenv p0 FERUN_ROLE_PASSWORD_expense_svc
\getenv p1 FERUN_ROLE_PASSWORD_approval_svc
SELECT format('CREATE DATABASE %I', :'d0') WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'d0') \gexec
SELECT format('CREATE DATABASE %I', :'d1') WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'d1') \gexec
SELECT format('CREATE ROLE %I', :'r0') WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'r0') \gexec
ALTER ROLE :"r0" WITH LOGIN PASSWORD :'p0';
SELECT format('CREATE ROLE %I', :'r1') WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'r1') \gexec
ALTER ROLE :"r1" WITH LOGIN PASSWORD :'p1';
GRANT CONNECT, CREATE ON DATABASE :"d0" TO :"r0", :"r1";
GRANT CONNECT, CREATE ON DATABASE :"d1" TO :"r0", :"r1";
\connect :"d0"
CREATE SCHEMA IF NOT EXISTS :"r0" AUTHORIZATION :"r0";
CREATE SCHEMA IF NOT EXISTS :"r1" AUTHORIZATION :"r1";
\connect :"d1"
CREATE SCHEMA IF NOT EXISTS :"r0" AUTHORIZATION :"r0";
CREATE SCHEMA IF NOT EXISTS :"r1" AUTHORIZATION :"r1";
SQL
`

// postgresOwnerCheck is the line provisioning has run since the review of
// S1 (finding C4) before the CREATE SCHEMA IF NOT EXISTS of each role
// (r<J>) in each database: it fails the step when a schema of the role's
// name exists there and another role owns it.
const postgresOwnerCheck = `SELECT format('DO %L', format('BEGIN RAISE EXCEPTION USING MESSAGE = %L; END', ` +
	`format('schema %I in database %I belongs to role %I, not to role %I', nspname, current_database(), ` +
	`pg_get_userbyid(nspowner), nspname))) FROM pg_namespace WHERE nspname = :'r<J>' ` +
	`AND nspowner <> (SELECT oid FROM pg_roles WHERE rolname = :'r<J>') \gexec`

// withOwnerChecks is a script of the spike's form with the owner check
// inserted before each CREATE SCHEMA line and nothing else changed. The
// spike's report stays as it was verified and written, so the spike case
// compares against its script plus the checks; TestPostgresDocker runs
// the checks against the real images.
func withOwnerChecks(script string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(script, "\n") {
		if rest, ok := strings.CutPrefix(line, `CREATE SCHEMA IF NOT EXISTS :"r`); ok {
			j, _, _ := strings.Cut(rest, `"`)
			b.WriteString(strings.ReplaceAll(postgresOwnerCheck, "<J>", j) + "\n")
		}
		b.WriteString(line)
	}
	return b.String()
}

// withoutPasswords is a script of the spike's form as ADR 0038 has it:
// over the socket instead of loopback TCP, the superuser's password
// removed first, the roles' set to none, no \getenv; nothing else.
func withoutPasswords(script string) string {
	script = strings.Replace(script, "-h 127.0.0.1 ", "-h /var/run/postgresql ", 1)
	var b strings.Builder
	for _, line := range strings.SplitAfter(script, "\n") {
		if strings.HasPrefix(line, `\getenv `) {
			continue
		}
		if before, _, ok := strings.Cut(line, " PASSWORD :'p"); ok {
			line = before + " PASSWORD NULL;\n"
		}
		b.WriteString(line)
		if strings.HasSuffix(line, "<<'SQL'\n") {
			b.WriteString("ALTER ROLE postgres PASSWORD NULL;\n")
		}
	}
	return b.String()
}

func TestPostgresProvision(t *testing.T) {
	pgoptions := model.Literal("-c client_min_messages=warning -c log_min_error_statement=panic")
	env := map[string]model.TemplateValue{"PGOPTIONS": pgoptions}
	cases := []struct {
		name   string
		addon  string
		script string
	}{
		{
			name:   "spike",
			addon:  `{"type":"postgres","version":"17","options":{"databases":["expensa","reports"],"roles":["expense_svc","approval_svc"]}}`,
			script: withoutPasswords(withOwnerChecks(postgresSpikeScript)),
		},
		{
			name:  "databases only",
			addon: `{"type":"postgres","version":"16","options":{"databases":["expensa"]}}`,
			script: `exec psql -X -q -v ON_ERROR_STOP=1 -h /var/run/postgresql -p 5432 -U postgres -d postgres -v d0=expensa <<'SQL'
ALTER ROLE postgres PASSWORD NULL;
SELECT format('CREATE DATABASE %I', :'d0') WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'d0') \gexec
SQL
`,
		},
		{
			name:  "one of each",
			addon: `{"type":"postgres","version":"17","options":{"roles":["app"],"databases":["app"]}}`,
			script: `exec psql -X -q -v ON_ERROR_STOP=1 -h /var/run/postgresql -p 5432 -U postgres -d postgres -v d0=app -v r0=app <<'SQL'
ALTER ROLE postgres PASSWORD NULL;
SELECT format('CREATE DATABASE %I', :'d0') WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'d0') \gexec
SELECT format('CREATE ROLE %I', :'r0') WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'r0') \gexec
ALTER ROLE :"r0" WITH LOGIN PASSWORD NULL;
GRANT CONNECT, CREATE ON DATABASE :"d0" TO :"r0";
\connect :"d0"
SELECT format('DO %L', format('BEGIN RAISE EXCEPTION USING MESSAGE = %L; END', format('schema %I in database %I belongs to role %I, not to role %I', nspname, current_database(), pg_get_userbyid(nspowner), nspname))) FROM pg_namespace WHERE nspname = :'r0' AND nspowner <> (SELECT oid FROM pg_roles WHERE rolname = :'r0') \gexec
CREATE SCHEMA IF NOT EXISTS :"r0" AUTHORIZATION :"r0";
SQL
`,
		},
		{
			// Nothing to create: the superuser's password still goes.
			name:  "bare",
			addon: `{"type":"postgres","version":"17"}`,
			script: `exec psql -X -q -v ON_ERROR_STOP=1 -h /var/run/postgresql -p 5432 -U postgres -d postgres <<'SQL'
ALTER ROLE postgres PASSWORD NULL;
SQL
`,
		},
	}
	if n := strings.Count(withOwnerChecks(postgresSpikeScript), "\\gexec\n") - strings.Count(postgresSpikeScript, "\\gexec\n"); n != 4 {
		t.Fatalf("%d owner checks added to the spike's script, want 4 (2 databases, 2 roles)", n)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := addon(t, "postgres", c.addon)
			steps := postgres{}.Provision(a, Context{Env: "stage", Name: "postgres", Address: "fd6b::5"})
			want := []Step{{Name: "provision", Argv: []string{"sh", "-c", c.script}, Env: env}}
			if !reflect.DeepEqual(steps, want) {
				t.Fatalf("steps\n%#v\nwant\n%#v", steps, want)
			}
			// Deterministic, so that its hash decides re-runs: the same
			// addon gives the same steps, in the same JSON.
			again := postgres{}.Provision(a, Context{})
			if jsonOf(t, again) != jsonOf(t, steps) {
				t.Fatal("Provision is not deterministic")
			}
			if strings.Contains(c.script, "PASSWORD '") || strings.Contains(c.script, "getenv") {
				t.Fatal("a password in the script")
			}
		})
	}
}

// TestPostgresTLSStep: with the controller's TLS material the steps are
// provision and tls; tls carries the key as a credential, everything else
// as literals, so its hash moves with a grant, a certificate or a root.
func TestPostgresTLSStep(t *testing.T) {
	a := addon(t, "postgres", `{"type":"postgres","version":"17","options":{"databases":["kino"],"roles":["catalog","seats"]}}`)
	const (
		api     = "wl-0123456789abcdef0123456789abcdef"
		migrate = "wl-fedcba9876543210fedcba9876543210"
	)
	tls := &TLS{CertPEM: "-----BEGIN CERTIFICATE-----\nA\n-----END CERTIFICATE-----\n", CAPEM: "CA",
		Grants: []Grant{
			{Workload: api, ID: "spiffe://run.fecode.dev/tenant/t/env/e/service/catalog", Role: "catalog"},
			{Workload: migrate, ID: "spiffe://run.fecode.dev/tenant/t/env/e/job/migrate", Role: "catalog"},
			{Workload: migrate, ID: "spiffe://run.fecode.dev/tenant/t/env/e/job/migrate", Role: "seats"},
			{Workload: "wl-s-xmxfgckmkt6", ID: "spiffe://run.fecode.dev/tenant/t/env/e/sandbox/xmxfgckmkt6", Role: "seats"},
			// Never mapped: the superuser, a workload that is no ID, a
			// role that is no name.
			{Workload: api, ID: "x", Role: "postgres"},
			{Workload: "api", ID: "x", Role: "catalog"},
			{Workload: "wl-0123456789abcdef0123456789abcdef$|.*", ID: "x", Role: "catalog"},
			{Workload: api, ID: "x", Role: "Catalog x"},
		}}
	steps := postgres{}.Provision(a, Context{Env: "stage", Name: "postgres", TLS: tls})
	if len(steps) != 2 || steps[0].Name != "provision" || steps[1].Name != "tls" {
		t.Fatalf("steps %s", jsonOf(t, steps))
	}
	st := steps[1]
	if !slices.Equal(st.Argv, []string{"sh", "-c", postgresTLSScript}) {
		t.Fatalf("argv %q", st.Argv)
	}
	if v := st.Env["FERUN_TLS_KEY"]; !reflect.DeepEqual(v, model.CredentialValue(TLSKey)) {
		t.Fatalf("key %s", jsonOf(t, v))
	}
	for k, want := range map[string]string{"FERUN_TLS_CERT": tls.CertPEM, "FERUN_TLS_CA": "CA", "FERUN_PG_HBA": postgresHBA} {
		if v := st.Env[k]; v.Value != want || v.Parts != nil {
			t.Errorf("%s = %s", k, jsonOf(t, v))
		}
	}
	wantIdent := `# Written by ferund (ADR 0038); a change here is overwritten.
ferun  root      postgres
ferun  postgres  postgres
# spiffe://run.fecode.dev/tenant/t/env/e/service/catalog
feid  "/^CN=[^,]+,OU=wl-0123456789abcdef0123456789abcdef$"  catalog
# spiffe://run.fecode.dev/tenant/t/env/e/job/migrate
feid  "/^CN=[^,]+,OU=wl-fedcba9876543210fedcba9876543210$"  catalog
# spiffe://run.fecode.dev/tenant/t/env/e/job/migrate
feid  "/^CN=[^,]+,OU=wl-fedcba9876543210fedcba9876543210$"  seats
# spiffe://run.fecode.dev/tenant/t/env/e/sandbox/xmxfgckmkt6
feid  "/^CN=[^,]+,OU=wl-s-xmxfgckmkt6$"  seats
`
	if got := st.Env["FERUN_PG_IDENT"].Value; got != wantIdent {
		t.Fatalf("pg_ident.conf\n%s\nwant\n%s", got, wantIdent)
	}
	// No rule takes a password, and the socket is the superuser's alone.
	for _, line := range strings.Split(postgresHBA, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		switch f[0] {
		case "local":
			if strings.Join(f, " ") != "local all postgres peer map=ferun" {
				t.Errorf("hba %q", line)
			}
		case "hostssl":
			if strings.Join(f, " ") != "hostssl all all all cert clientname=DN map=feid" {
				t.Errorf("hba %q", line)
			}
		default:
			t.Errorf("hba %q: only local and hostssl", line)
		}
	}
	other := *tls
	other.Grants = tls.Grants[:1]
	if jsonOf(t, postgres{}.Provision(a, Context{TLS: &other})) == jsonOf(t, steps) {
		t.Fatal("a grant less gives the same steps")
	}
}

// TestPostgresScriptQuotes: provisioning quotes a name the shell would
// read otherwise; Validate refuses such names, so this is only a guard.
func TestPostgresScriptQuotes(t *testing.T) {
	got := postgresScript([]string{"a b", "it's"}, nil)
	want := `exec psql -X -q -v ON_ERROR_STOP=1 -h /var/run/postgresql -p 5432 -U postgres -d postgres -v d0='a b' -v d1='it'\''s' <<'SQL'` + "\n"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("script %q, want it to start with %q", got, want)
	}
}
