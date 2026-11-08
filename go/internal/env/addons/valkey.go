// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package addons

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
)

// valkey is Valkey (the Redis fork) from the official alpine image, with
// the append-only file on the volume.
//
// It takes no password from anyone (ADR 0039). It serves TLS 1.3 alone, on
// 6379, with a server certificate the controller issues for the addon's
// ID, and admits a client only by a certificate under the platform's
// root: Valkey from 9.1 authenticates it as the ACL user named by its URI
// (tls-auth-clients-user URI), and there is one user per workload that a
// reference of the deployment names, its SPIFFE ID, with every data
// command on every key and channel and none of the administrative ones.
// The default user is off and nobody has a password, so a certificate
// that names no user gets nothing but NOAUTH, and AUTH never succeeds.
//
// The addon administers itself: ferund's tls step reaches it over
// loopback TLS as the addon's own ID, with the server certificate, which
// is a client's too (ProvisionsAsItself), and that ID's user may do
// everything. Before the first step there is no certificate: the server
// then listens on its socket in the container alone, with the default
// user of before (bootstrap), until the step turns TLS on and loads the
// users, which turn that user off.
//
// Option "maxmemoryPolicy": what happens at the memory limit (70% of the
// plan's memory), noeviction by default.
type valkey struct{}

func init() { _ = Register(valkey{}) }

const (
	valkeyPort  = 6379
	valkeyMount = "/data"
	// valkeyVersion is the version valkey runs: the first line that maps
	// a certificate's URI to a user is 9.1, and the image's 9-alpine has
	// been 9.1 or later since 9.1.0.
	valkeyVersion       = "9"
	valkeyDefaultPolicy = "noeviction"
	// valkeyDir holds the server's key, its certificate's chain, what it
	// trusts of its clients and the users, on the volume beside the data.
	valkeyDir = valkeyMount + "/ferun"
	// valkeySocket is the server's socket in the container, in a
	// directory of the server's user: the probe's, and the steps' before
	// TLS is on.
	valkeySocketDir = "/run/valkey"
	valkeySocket    = valkeySocketDir + "/valkey.sock"
	// valkeyRole is the one role a reference grants (GrantedRole): every
	// data command on every key and channel (valkeyClientRules).
	valkeyRole = "data"
	// valkeyClientRules are a workload's rights, besides on and no
	// password: every key and channel, every command but the
	// administrative ones (users, configuration, persistence, replication,
	// shutdown, other clients, MONITOR, DEBUG, modules) and MIGRATE, which
	// would have the addon dial another server.
	valkeyClientRules = "~* &* +@all -@admin -migrate"
	// valkeyAdminRules are the addon's own ID's: everything.
	valkeyAdminRules = "~* &* +@all"
	// valkeyBootstrapACL is the users before the first step: the default
	// user of before, on the socket alone (the server has no TCP then, and
	// protected mode refuses any other connection while it has no
	// password).
	valkeyBootstrapACL = "user default on nopass ~* &* +@all"
)

// valkeyRetired are the versions an addon may still name that run as
// valkeyVersion (ADR 0039): before 9.1 Valkey cannot admit a workload by
// its certificate, and 9 takes their data on the same volume.
var valkeyRetired = []string{"7.2", "8"}

// valkeyTLS is the server's TLS, as flags at its start and as CONFIG SET
// when the step turns it on: TLS 1.3, a client certificate under the
// platform's root (the CA file), authenticated as the user its URI names.
var valkeyTLS = [][2]string{
	{"tls-cert-file", valkeyDir + "/server.crt"},
	{"tls-key-file", valkeyDir + "/server.key"},
	{"tls-ca-cert-file", valkeyDir + "/ca.crt"},
	{"tls-auth-clients", "yes"},
	{"tls-auth-clients-user", "URI"},
	{"tls-protocols", "TLSv1.3"},
}

// valkeyUserName is a workload's SPIFFE ID as an ACL user's name: an ACL
// file splits a line at spaces, and none of these characters is one.
var valkeyUserName = regexp.MustCompile(`^spiffe://[a-z0-9.-]+(/[A-Za-z0-9._~-]+)+$`)

// valkeyPolicies are the maxmemory-policy values of valkey 9; any other
// stops the server at start.
var valkeyPolicies = []string{
	"volatile-lru", "volatile-lfu", "volatile-random", "volatile-ttl",
	"allkeys-lru", "allkeys-lfu", "allkeys-random", "noeviction",
}

func (valkey) Type() string        { return "valkey" }
func (valkey) Versions() []string  { return []string{valkeyVersion} }
func (valkey) DefaultPlan() string { return "small" }

// runs is the version an addon of version runs: valkeyVersion for a
// retired one.
func (valkey) runs(version string) string {
	if slices.Contains(valkeyRetired, version) {
		return valkeyVersion
	}
	return version
}

func (valkey) Plans() map[string]Plan {
	return map[string]Plan{
		"dev":    {VCPUs: 1, MemoryMib: 128, VolumeMib: 256},
		"small":  {VCPUs: 1, MemoryMib: 512, VolumeMib: 2048},
		"medium": {VCPUs: 1, MemoryMib: 2048, VolumeMib: 8192},
		"large":  {VCPUs: 2, MemoryMib: 8192, VolumeMib: 32768},
	}
}

// valkeyMaxmemoryPolicy is valkey's option, which policy reads.
var valkeyMaxmemoryPolicy = Option{Name: "maxmemoryPolicy", Kind: OptionOneOf, Default: valkeyDefaultPolicy,
	Values: valkeyPolicies,
	Description: "What valkey does at its memory limit (70% of the plan's memory): noeviction refuses writes, " +
		"the others evict keys (allkeys-*: any key, volatile-*: keys with a TTL)."}

func (valkey) OutputKeys() []string { return []string{"host", "port", "url"} }

func (valkey) OutputFields() []string { return nil }

func (valkey) Options() []Option { return []Option{valkeyMaxmemoryPolicy} }

func (v valkey) policy(a Addon) (string, []spec.Finding) {
	opt := valkeyMaxmemoryPolicy
	fs := unknownOptions(v, a.Options)
	policy, pfs := stringOption(a.Options, opt.Name, valkeyDefaultPolicy)
	fs = append(fs, pfs...)
	if len(pfs) == 0 && !slices.Contains(opt.Values, policy) {
		fs = append(fs, errorf("options."+opt.Name, fmt.Sprintf("valkey has no maxmemory policy %q", policy),
			"use one of "+strings.Join(opt.Values, ", ")))
	}
	return policy, fs
}

// Validate: a retired version is a warning, and the addon is checked as
// the version it runs; going back to an older version than the data's is
// refused.
func (v valkey) Validate(prev *Addon, next Addon) []spec.Finding {
	prev = sameType(prev, v.Type())
	var fs []spec.Finding
	if slices.Contains(valkeyRetired, next.Version) {
		fs = append(fs, warning("version",
			fmt.Sprintf("valkey %s is retired: before 9.1 Valkey cannot admit a workload by its certificate, "+
				"which is the only way in (ADR 0039); the addon runs %s, which takes %s's data on its volume",
				next.Version, valkeyVersion, next.Version),
			"write version "+valkeyVersion+", which changes nothing that runs"))
	}
	runs := next
	runs.Version = v.runs(next.Version)
	var ran *Addon
	if prev != nil {
		p := *prev
		p.Version = v.runs(prev.Version)
		ran = &p
	}
	fs = append(fs, checkVersionPlan(v, ran, runs, true)...)
	if ran != nil {
		from, to := slices.Index(v.Versions(), ran.Version), slices.Index(v.Versions(), runs.Version)
		switch {
		case from < 0 || to < 0 || from == to:
		case to < from:
			fs = append(fs, errorf("version",
				fmt.Sprintf("valkey cannot go back from %s to %s: a newer version's data files are not "+
					"promised to load in an older one", ran.Version, runs.Version),
				"keep version "+ran.Version))
		default:
			fs = append(fs, info("version",
				fmt.Sprintf("valkey goes from %s to %s on the same volume, which keeps the data; going back "+
					"later is refused", ran.Version, runs.Version), ""))
		}
	}
	_, pfs := v.policy(next)
	return append(fs, pfs...)
}

// Credentials: the server's key (ADR 0039). There is no password.
func (valkey) Credentials(Addon) []string { return []string{TLSKey} }

// GrantedRole: every reference grants its workload the role "data" (ADR
// 0039): a user of its ID with valkeyClientRules.
func (valkey) GrantedRole(Ref) string { return valkeyRole }

// ProvisionsAsItself: yes, over loopback TLS as the addon's own ID, since
// nothing but a certificate gets in.
func (valkey) ProvisionsAsItself() bool { return true }

func (v valkey) Render(a Addon, _ Context) (Instance, error) {
	version := v.runs(a.Version)
	if !slices.Contains(v.Versions(), version) {
		return Instance{}, fmt.Errorf("valkey: no version %q", a.Version)
	}
	name, plan, ok := planOf(v, a)
	if !ok {
		return Instance{}, fmt.Errorf("valkey: no plan %q", name)
	}
	policy, fs := v.policy(a)
	if err := firstError(v.Type(), fs); err != nil {
		return Instance{}, err
	}
	// The rest of the plan's memory is the kernel's, the guest's and an
	// AOF rewrite's fork.
	maxmemory := plan.MemoryMib * 7 / 10
	return Instance{
		Image: "valkey/valkey:" + version + "-alpine",
		// The image's own entrypoint (tini, then docker-entrypoint.sh,
		// which chowns /data and drops to the valkey user) with a start of
		// our own in between (valkeyStart).
		Command: []string{"tini", "--", "sh", "-c", valkeyStart(maxmemory, policy)},
		Port:    valkeyPort,
		Mount:   valkeyMount,
		Probe:   []string{"sh", "-c", valkeyProbe},
	}, nil
}

// valkeyStart is the container's command: the server with the append-only
// file, the plan's memory and the policy, no plaintext TCP (port 0),
// protected mode, its socket in the container and the users' file on the
// volume. With a key, a certificate, a trust and users on the volume (a
// step wrote them) it serves TLS on 6379 (valkeyTLS); without, the
// bootstrap's users and no TLS, until the step turns it on.
func valkeyStart(maxmemory int, policy string) string {
	var tls strings.Builder
	for _, kv := range valkeyTLS {
		fmt.Fprintf(&tls, " --%s %s", kv[0], kv[1])
	}
	return `set -eu
umask 077
f=` + valkeyDir + `
mkdir -p "$f" ` + valkeySocketDir + `
chown valkey:valkey ` + valkeySocketDir + `
if [ -s "$f/server.key" ] && [ -s "$f/server.crt" ] && [ -s "$f/ca.crt" ] && [ -s "$f/users.acl" ]; then
  set -- --tls-port ` + strconv.Itoa(valkeyPort) + tls.String() + `
else
  printf '%s\n' '` + valkeyBootstrapACL + `' >"$f/users.acl"
  set --
fi
exec docker-entrypoint.sh valkey-server --port 0 --protected-mode yes --unixsocket ` + valkeySocket +
		` --unixsocketperm 700 --aclfile "$f/users.acl" --appendonly yes --maxmemory ` + strconv.Itoa(maxmemory) +
		`mb --maxmemory-policy ` + policy + ` "$@"
`
}

// valkeyProbe is the readiness check: the server answers on its socket,
// PONG from the bootstrap's default user, NOAUTH once only a certificate
// gets in. It presents no certificate: one that expired while the addon
// was stopped would keep it unready, and an unready addon is never
// provisioned, so its certificate would never be renewed. valkey-cli
// exits 0 on an error reply, hence the grep.
const valkeyProbe = "valkey-cli -s " + valkeySocket + ` ping 2>&1 | grep -qE '^(PONG|NOAUTH )'`

// Outputs: host and port of the referencing pod's TLS binding to the
// addon (TLSBinding), and url redis://<loopback>:6379/0, without a user
// or a password: the workload gets in by its certificate, which the
// guest presents (ADR 0039). The client speaks plaintext to the
// loopback address and must neither authenticate nor ask for TLS of its
// own. No fields.
func (v valkey) Outputs(_ Addon, c Context, ref Ref) ([]model.TemplatePart, error) {
	if ref.Key == "user" || ref.Key == "password" {
		return nil, fmt.Errorf("valkey addon %s has no %s: the workload that holds a reference gets in by its "+
			"certificate, through its pod's TLS binding, and sends none (ADR 0039); the keys are %s", c.Name,
			ref.Key, strings.Join(v.OutputKeys(), ", "))
	}
	if err := checkRef(v.Type(), ref, v.OutputKeys(), v.OutputFields()...); err != nil {
		return nil, err
	}
	loopback := c.Loopback
	if loopback == "" {
		loopback = Loopback(1)
	}
	switch ref.Key {
	case "host":
		return literal(loopback), nil
	case "port":
		return literal(strconv.Itoa(valkeyPort)), nil
	}
	return literal("redis://" + loopback + ":" + strconv.Itoa(valkeyPort) + "/0"), nil
}

// TLSBinding: every reference reaches the addon through the pod's binding,
// TLS from the first byte with no ALPN protocol, since Valkey has none
// (ADR 0039).
func (valkey) TLSBinding(Ref) (int, string, bool) { return valkeyPort, "", true }

// Provision is one step, "tls", with c.TLS: the server's key, chain and
// trust and the users (valkeyACL) on the volume, then TLS and the users
// in effect. Before TLS is on (the socket answers PONG: the bootstrap's
// default user) the step sets TLS over the socket, opens its port and
// loads the users, which turn the default user off; after, the step
// reaches the server over loopback TLS as the addon's own ID, reloads the
// TLS files (a renewed certificate, a new root) and loads the users, which
// ends the sessions of users no longer there. Either way it checks that
// the server takes the addon's own certificate as the addon's ID, and
// puts the users of before back if not. Every input is in the step's env,
// so its hash changes with a grant, a renewed certificate or a new root,
// and it runs again. Without c.TLS: none, and the server stays on its
// socket alone.
func (v valkey) Provision(_ Addon, c Context) []Step {
	if c.TLS == nil {
		return nil
	}
	if !valkeyUserName.MatchString(c.TLS.ID) {
		// The users would lock the steps out: fail, and change nothing.
		return []Step{{Name: "tls", Argv: []string{"sh", "-c", "echo " + shellWord(fmt.Sprintf(
			"valkey: the addon's ID %q is no user name", c.TLS.ID)) + "; exit 1"}}}
	}
	return []Step{{Name: "tls", Argv: []string{"sh", "-c", valkeyTLSScript}, Env: map[string]model.TemplateValue{
		"FERUN_TLS_KEY":    model.CredentialValue(TLSKey),
		"FERUN_TLS_CERT":   model.Literal(c.TLS.CertPEM),
		"FERUN_TLS_CA":     model.Literal(c.TLS.CAPEM),
		"FERUN_VALKEY_ID":  model.Literal(c.TLS.ID),
		"FERUN_VALKEY_ACL": model.Literal(valkeyACL(c.TLS.ID, c.TLS.Grants)),
	}}}
}

// valkeyACL is the users' file as the tls step writes it: the default user
// off, without a password, keys, channels or commands; the addon's own ID
// with everything; and one user per grant, named by the workload's SPIFFE
// ID, which its certificate carries as its URI, with valkeyClientRules.
// Nobody has a password (resetpass, never nopass), so no AUTH succeeds.
// An ACL file has no comments.
func valkeyACL(admin string, grants []Grant) string {
	var b strings.Builder
	b.WriteString("user default off resetpass resetkeys resetchannels -@all\n")
	seen := map[string]bool{}
	if valkeyUserName.MatchString(admin) {
		b.WriteString("user " + admin + " on resetpass " + valkeyAdminRules + "\n")
		seen[admin] = true
	}
	for _, g := range grants {
		if g.Role != valkeyRole || !valkeyUserName.MatchString(g.ID) || seen[g.ID] {
			continue
		}
		seen[g.ID] = true
		b.WriteString("user " + g.ID + " on resetpass " + valkeyClientRules + "\n")
	}
	return b.String()
}

// valkeyTLSScript is the tls step (Provision). valkey-cli exits 0 on an
// error reply, so every command's answer is compared with what it must
// be; the step prints nothing unless it fails. Before TLS is on, the TLS
// files are read only when the port opens, which fails without changing
// anything if they are bad; between the port and the users, protected
// mode refuses every connection from outside the pod while the default
// user has no password, and the port takes none without a certificate
// under the root anyway. The step's own TLS connection verifies no
// server: the server is its own container's, on loopback, and its
// certificate may be the expired one the step replaces.
var valkeyTLSScript = func() string {
	var set strings.Builder
	for _, kv := range valkeyTLS {
		fmt.Fprintf(&set, " %s %s", kv[0], kv[1])
	}
	port := strconv.Itoa(valkeyPort)
	return `set -eu
umask 077
f=` + valkeyDir + `
s=` + valkeySocket + `
mkdir -p "$f"
chown valkey:valkey "$f"
put() { printf '%s\n' "$2" >"$1.ferun" && chown valkey:valkey "$1.ferun" && mv -f "$1.ferun" "$1"; }
put "$f/server.key" "$FERUN_TLS_KEY"
put "$f/server.crt" "$FERUN_TLS_CERT"
put "$f/ca.crt" "$FERUN_TLS_CA"
rm -f "$f/users.acl.ferun-before"
[ ! -f "$f/users.acl" ] || cp -p "$f/users.acl" "$f/users.acl.ferun-before"
put "$f/users.acl" "$FERUN_VALKEY_ACL"
back() { [ ! -f "$f/users.acl.ferun-before" ] || mv -f "$f/users.acl.ferun-before" "$f/users.acl"; }
sock() { valkey-cli -s "$s" "$@" 2>&1; }
tls() { valkey-cli --tls --insecure --cert "$f/server.crt" --key "$f/server.key" -h 127.0.0.1 -p ` + port + ` "$@" 2>&1; }
on() {
  c=$1
  shift
  out=$("$c" "$@") || true
  if [ "$out" != OK ]; then printf '%s %s: %s\n' "$1" "$2" "$out"; back; exit 1; fi
}
if [ "$(sock ping)" = PONG ]; then
  on sock CONFIG SET` + set.String() + `
  on sock CONFIG SET tls-port ` + port + `
  on sock ACL LOAD
else
  on tls CONFIG SET` + set.String() + `
  on tls ACL LOAD
fi
if [ "$(tls ACL WHOAMI)" != "$FERUN_VALKEY_ID" ]; then
  printf 'valkey does not take its own certificate as %s\n' "$FERUN_VALKEY_ID"
  exit 1
fi
rm -f "$f/users.acl.ferun-before"
`
}()
