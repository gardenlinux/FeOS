// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package addons

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
)

// postgres is PostgreSQL from the official alpine image. The image's
// entrypoint runs initdb on a fresh volume, chowns PGDATA and drops to
// the postgres user (FeOS starts every container as uid 0); it listens
// on every address, IPv6 included, by default. PGDATA is a directory
// below the mount, since a fresh volume's root holds lost+found, which
// initdb refuses.
//
// Options: "databases", the databases to create, and "roles", login
// roles with a password each (credential "role.<r>.password"), CONNECT
// and CREATE on every listed database and a schema of their own name in
// each. Provisioning creates what is missing and sets the roles'
// passwords on every run; it never drops anything.
type postgres struct{}

func init() { _ = Register(postgres{}) }

const (
	postgresPort  = 5432
	postgresMount = "/var/lib/postgresql/data"
	// postgresData is PGDATA, beside the volume's lost+found.
	postgresData      = postgresMount + "/pgdata"
	postgresSuperuser = "postgres"
	postgresDefaultDB = "postgres"
	// postgresInitdbArgs makes TCP from loopback ask for the password
	// too: initdb's default trusts 127.0.0.1 and ::1, so a port-forward
	// (which dials 127.0.0.1 in the pod) would be a superuser session
	// without one. Only a fresh initdb reads it.
	postgresInitdbArgs = "--auth-host=scram-sha-256"
	// postgresPGOptions keeps the provisioning's re-runs quiet ("already
	// exists, skipping") and a failed statement's text, which may hold a
	// role's password, out of the server log.
	postgresPGOptions = "-c client_min_messages=warning -c log_min_error_statement=panic"
	// postgresRolePasswordEnv is the prefix of the variable that carries
	// a role's password into the provisioning step.
	postgresRolePasswordEnv = "FERUN_ROLE_PASSWORD_"
)

// postgresName is a database or role name: lowercase, so that it is the
// same quoted and unquoted, and short enough for an identifier.
var postgresName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// The names a role cannot have: the superuser (provisioning would set
// its password to the role's), the names PostgreSQL reserves, and the
// ones that read as a role specification.
var postgresReservedRoles = []string{
	"postgres", "public", "none", "current_user", "current_role", "session_user", "information_schema",
}

// The names a database cannot have: the ones that exist already, which
// provisioning would grant on.
var postgresReservedDatabases = []string{"template0", "template1", "postgres"}

func (postgres) Type() string       { return "postgres" }
func (postgres) Versions() []string { return []string{"16", "17"} }
func (postgres) DefaultPlan() string {
	return "small"
}

func (postgres) Plans() map[string]Plan {
	return map[string]Plan{
		"dev":    {VCPUs: 1, MemoryMib: 256, VolumeMib: 1024},
		"small":  {VCPUs: 1, MemoryMib: 1024, VolumeMib: 10240},
		"medium": {VCPUs: 2, MemoryMib: 4096, VolumeMib: 51200},
		"large":  {VCPUs: 4, MemoryMib: 16384, VolumeMib: 204800},
	}
}

func (postgres) OutputKeys() []string {
	return []string{"host", "port", "user", "password", "database", "url"}
}

// postgresOptions are a postgres addon's options as read.
type postgresOptions struct {
	databases, roles []string
}

// options reads a's options, with every finding about them (prev: the
// current deployment's addon, for what provisioning leaves behind).
func (p postgres) options(prev *Addon, a Addon) (postgresOptions, []spec.Finding) {
	fs := unknownOptions(p.Type(), a.Options, "databases", "roles")
	dbItems, dfs := stringList(a.Options, "databases")
	roleItems, rfs := stringList(a.Options, "roles")
	fs = append(fs, dfs...)
	fs = append(fs, checkNames("databases", dbItems, postgresReservedDatabases, false)...)
	fs = append(fs, rfs...)
	fs = append(fs, checkNames("roles", roleItems, postgresReservedRoles, true)...)
	dbs, roles := names(dbItems), names(roleItems)
	if len(roles) > 0 && len(dbs) == 0 {
		fs = append(fs, errorf("options.roles", "roles need a database: a role gets CONNECT, CREATE and a schema in "+
			"each listed database, and there is none", "list a database in options.databases"))
	}
	for _, it := range roleItems {
		// The role's credential is the system secret
		// addon.<addon>.role.<r>.password, at most 127 characters.
		secret := CredentialSecret(a.Name, roleKey(it.name))
		if it.ok && postgresName.MatchString(it.name) && len(secret) > 127 {
			fs = append(fs, errorf(it.path("roles"),
				fmt.Sprintf("role %s's credential %s is %d characters long, more than 127", it.name, secret,
					len(secret)),
				"shorten the role's name or the addon's"))
		}
	}
	if prev := sameType(prev, p.Type()); prev != nil {
		old, _ := p.options(nil, *prev)
		for _, d := range old.databases {
			if !slices.Contains(dbs, d) {
				fs = append(fs, warning("options.databases", "database "+d+" is no longer listed; it is not dropped "+
					"and keeps its data", "drop it yourself if it should go"))
			}
		}
		for _, r := range old.roles {
			if !slices.Contains(roles, r) {
				fs = append(fs, warning("options.roles", "role "+r+" is no longer listed; it is not dropped and "+
					"keeps its login, its last password and its schemas", "drop it yourself if it should go"))
			}
		}
	}
	return postgresOptions{databases: dbs, roles: roles}, fs
}

// checkNames checks the items of list key, in their order: a string,
// the name rule, the reserved names (and, for roles, the pg_ prefix),
// duplicates.
func checkNames(key string, items []listItem, reserved []string, role bool) []spec.Finding {
	var fs []spec.Finding
	what := strings.TrimSuffix(key, "s")
	seen := map[string]bool{}
	for _, it := range items {
		path, n := it.path(key), it.name
		switch {
		case !it.ok:
			fs = append(fs, errorf(path, fmt.Sprintf("want a %s name, not %s", what, it.not),
				"write the name as a JSON string"))
		case !postgresName.MatchString(n):
			fs = append(fs, errorf(path, fmt.Sprintf("%s name %q is not a lowercase identifier", what, n),
				"use a-z, 0-9 and _, starting with a letter, at most 63 characters"))
		case slices.Contains(reserved, n) || role && strings.HasPrefix(n, "pg_"):
			fs = append(fs, errorf(path, fmt.Sprintf("%s name %s is reserved", what, n), "choose another name"))
		case seen[n]:
			fs = append(fs, errorf(path, fmt.Sprintf("%s %s is listed twice", what, n), "list it once"))
		}
		if it.ok {
			seen[n] = true
		}
	}
	return fs
}

func (p postgres) Validate(prev *Addon, next Addon) []spec.Finding {
	prev = sameType(prev, p.Type())
	fs := checkVersionPlan(p, prev, next, true)
	if prev != nil && slices.Contains(p.Versions(), prev.Version) && slices.Contains(p.Versions(), next.Version) &&
		prev.Version != next.Version {
		fs = append(fs, errorf("version",
			fmt.Sprintf("changing postgres from %s to %s needs a data migration (the data directory of one "+
				"major version does not start with another), which is not available", prev.Version, next.Version),
			fmt.Sprintf("keep version %s; for %s, add a second postgres addon and move the data with pg_dump",
				prev.Version, next.Version)))
	}
	_, ofs := p.options(prev, next)
	return append(fs, ofs...)
}

func roleKey(role string) string { return "role." + role + ".password" }

func (p postgres) Credentials(a Addon) []string {
	opts, _ := p.options(nil, a)
	keys := []string{passwordKey}
	for _, r := range opts.roles {
		if k := roleKey(r); !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

func (p postgres) Render(a Addon, _ Context) (Instance, error) {
	if !slices.Contains(p.Versions(), a.Version) {
		return Instance{}, fmt.Errorf("postgres: no version %q", a.Version)
	}
	name, plan, ok := planOf(p, a)
	if !ok {
		return Instance{}, fmt.Errorf("postgres: no plan %q", name)
	}
	return Instance{
		Image: "postgres:" + a.Version + "-alpine",
		// The entrypoint's own command with shared_buffers sized to the
		// plan (a quarter of its memory; the default 128MB would crowd
		// the smallest plans): $1 is still postgres, so initdb, the chown
		// and the privilege drop happen as with no command.
		Command: []string{"docker-entrypoint.sh", "postgres", "-c",
			"shared_buffers=" + strconv.Itoa(plan.MemoryMib/4) + "MB"},
		Env: map[string]model.TemplateValue{
			"PGDATA":               model.Literal(postgresData),
			"POSTGRES_PASSWORD":    model.CredentialValue(passwordKey),
			"POSTGRES_INITDB_ARGS": model.Literal(postgresInitdbArgs),
		},
		Port:  postgresPort,
		Mount: postgresMount,
		// Over TCP: the entrypoint's temporary server for initdb listens
		// on the socket only, so this reports the real server alone.
		Probe: []string{"pg_isready", "-U", postgresSuperuser, "-h", "127.0.0.1"},
	}, nil
}

// Outputs: host, port, user, password, database and url of the
// superuser, or with field "role" of that role; field "database" picks
// the database (default: the first listed, else postgres; a role's must
// be listed).
func (p postgres) Outputs(a Addon, c Context, ref Ref) ([]model.TemplatePart, error) {
	if err := checkRef(p.Type(), ref, p.OutputKeys(), "role", "database"); err != nil {
		return nil, err
	}
	// The options' own problems are Validate's findings; a reference
	// only needs what it names to be listed.
	opts, _ := p.options(nil, a)
	user, pwKey := postgresSuperuser, passwordKey
	if r, ok := ref.Fields["role"]; ok {
		if !slices.Contains(opts.roles, r) {
			return nil, fmt.Errorf("postgres addon %s has no role %q in options.roles", c.Name, r)
		}
		user, pwKey = r, roleKey(r)
	}
	db := postgresDefaultDB
	if len(opts.databases) > 0 {
		db = opts.databases[0]
	}
	if d, ok := ref.Fields["database"]; ok {
		switch {
		case slices.Contains(opts.databases, d):
		case d == postgresDefaultDB && user == postgresSuperuser:
		case user != postgresSuperuser:
			return nil, fmt.Errorf("postgres addon %s gives role %s no database %q; it has %s", c.Name, user, d,
				strings.Join(opts.databases, ", "))
		default:
			return nil, fmt.Errorf("postgres addon %s has no database %q in options.databases", c.Name, d)
		}
		db = d
	}
	switch ref.Key {
	case "host":
		return literal(host(c)), nil
	case "port":
		return literal(strconv.Itoa(postgresPort)), nil
	case "user":
		return literal(user), nil
	case "password":
		return credential(pwKey), nil
	case "database":
		return literal(db), nil
	}
	return serverURL("postgres", user, pwKey, c, postgresPort, "/"+url.PathEscape(db)+"?sslmode=disable"), nil
}

// Provision is one step, "provision": psql in the container, fed a
// quoted heredoc by sh (an exec has no stdin, and psql -c cannot run
// \gexec). The names travel as psql variables and are quoted by psql
// (:'d0' a literal, :"d0" an identifier); the passwords come from the
// step's environment (\getenv), so no secret is in its argv or script.
// It is idempotent: what exists is skipped, the roles' passwords are set
// again (the only write of a re-run). None without databases and roles.
func (p postgres) Provision(a Addon, _ Context) []Step {
	opts, _ := p.options(nil, a)
	if len(opts.databases) == 0 && len(opts.roles) == 0 {
		return nil
	}
	env := map[string]model.TemplateValue{
		"PGPASSWORD": model.CredentialValue(passwordKey),
		"PGOPTIONS":  model.Literal(postgresPGOptions),
	}
	for _, r := range opts.roles {
		env[postgresRolePasswordEnv+r] = model.CredentialValue(roleKey(r))
	}
	return []Step{{
		Name: "provision",
		Argv: []string{"sh", "-c", postgresScript(opts.databases, opts.roles)},
		Env:  env,
	}}
}

// postgresScript is the provisioning's shell command: databases in
// their order (d<i>), then roles (r<j>), each granted on every database
// and given a schema in each (postgresSchemaOwnerCheck first).
func postgresScript(dbs, roles []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exec psql -X -q -v ON_ERROR_STOP=1 -h 127.0.0.1 -p %d -U %s -d %s", postgresPort,
		postgresSuperuser, postgresDefaultDB)
	for i, d := range dbs {
		fmt.Fprintf(&b, " -v d%d=%s", i, shellWord(d))
	}
	for j, r := range roles {
		fmt.Fprintf(&b, " -v r%d=%s", j, shellWord(r))
	}
	b.WriteString(" <<'SQL'\n")
	for j, r := range roles {
		fmt.Fprintf(&b, "\\getenv p%d %s%s\n", j, postgresRolePasswordEnv, r)
	}
	for i := range dbs {
		fmt.Fprintf(&b, "SELECT format('CREATE DATABASE %%I', :'d%d') WHERE NOT EXISTS "+
			"(SELECT FROM pg_database WHERE datname = :'d%d') \\gexec\n", i, i)
	}
	for j := range roles {
		fmt.Fprintf(&b, "SELECT format('CREATE ROLE %%I', :'r%d') WHERE NOT EXISTS "+
			"(SELECT FROM pg_roles WHERE rolname = :'r%d') \\gexec\n", j, j)
		fmt.Fprintf(&b, "ALTER ROLE :\"r%d\" WITH LOGIN PASSWORD :'p%d';\n", j, j)
	}
	if len(roles) > 0 {
		grantees := make([]string, len(roles))
		for j := range roles {
			grantees[j] = fmt.Sprintf(":\"r%d\"", j)
		}
		for i := range dbs {
			fmt.Fprintf(&b, "GRANT CONNECT, CREATE ON DATABASE :\"d%d\" TO %s;\n", i, strings.Join(grantees, ", "))
		}
		for i := range dbs {
			fmt.Fprintf(&b, "\\connect :\"d%d\"\n", i)
			for j := range roles {
				fmt.Fprintf(&b, "%s\n", postgresSchemaOwnerCheck(j))
				fmt.Fprintf(&b, "CREATE SCHEMA IF NOT EXISTS :\"r%d\" AUTHORIZATION :\"r%d\";\n", j, j)
			}
		}
	}
	b.WriteString("SQL\n")
	return b.String()
}

// postgresSchemaOwnerCheck is the psql line before role r<j>'s CREATE
// SCHEMA IF NOT EXISTS in a database: it fails the step (a DO block that
// raises, run by \gexec; ON_ERROR_STOP makes psql exit non-zero) when a
// schema of the role's name exists there and another role owns it. Every
// role may create schemas in every listed database, so a role added later
// can find its schema made by another one, which would own what the new
// role's unqualified names resolve to; IF NOT EXISTS would adopt it
// silently, and handing it over (ALTER SCHEMA OWNER) would keep the
// objects and grants the other role put in it. The message names the
// schema, the database and the owner, and quotes them (%I, %L) as
// PostgreSQL would.
func postgresSchemaOwnerCheck(j int) string {
	return fmt.Sprintf("SELECT format('DO %%L', format('BEGIN RAISE EXCEPTION USING MESSAGE = %%L; END', "+
		"format('schema %%I in database %%I belongs to role %%I, not to role %%I', nspname, current_database(), "+
		"pg_get_userbyid(nspowner), nspname))) FROM pg_namespace WHERE nspname = :'r%d' "+
		"AND nspowner <> (SELECT oid FROM pg_roles WHERE rolname = :'r%d') \\gexec", j, j)
}
