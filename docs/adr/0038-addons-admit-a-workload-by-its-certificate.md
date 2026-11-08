# ADR 0038: Addons admit a workload by its certificate and by nothing else

**Status:** Accepted · **Date:** 2026-11-08

An addon whose server can map a client certificate to a user admits a
workload by the certificate of its workload ID (ADR 0014) and by nothing
else: there is no password anywhere, not on the network, not in a role,
not in a referencing pod's environment. The addon terminates TLS 1.3
itself with a server certificate the control plane issues for the
addon's own ID, and a map from the referencing workloads' identities to
the roles their references grant decides who may become what; the chain
alone admits nobody. `ferund` writes the key, the chain, the trust and
the map into the addon as provisioning steps (ADR 0040), whose hash
moves with every grant, renewal and new root. A referencing pod reaches
the addon through its guest's outbound TLS binding, which presents the
pod's identity and pins the addon's (ADR 0014). PostgreSQL maps the
subject's `OU`, Valkey names an ACL user by the SPIFFE ID; NATS is not
yet decided.

## Context

- A reference to an addon's role (ADR 0040) is otherwise a password: held
  by the pod as long as it runs, leaking with its environment, changing
  only when the addon rotates it, every referencing workload the same
  all-powerful user, and the connection plaintext.
- ADR 0014: a reference to an addon's role grants that role to the
  referencing workload's ID; an addon maps the certificate to the role
  and never admits on the chain alone, since every workload of every
  tenant is under one root; a client presents the ID through FeID or
  through the guest's outbound TLS binding, TLS from the first byte with
  the upstream's ALPN or none. A workload's certificate carries its
  SPIFFE ID as its one URI name; its subject is `CN=<td>/<pod ID>` and
  one `OU`, the workload's set ID, which the issuer writes and no tenant
  chooses.
- What the servers offer. PostgreSQL: the method `cert`, the client's
  certificate verified against its CA file and its CN, or with
  `clientname=DN` its whole subject in RFC 2253 form, mapped to the
  requested role through `pg_ident.conf`, exact or by a regular
  expression; TLS on its one port, from 17 also from the first byte with
  ALPN `postgresql`; files, rules and map read again on a reload, which
  drops no session; no released version reads a URI name. Valkey: TLS
  on a port of its own and no plaintext port, client certificates
  required; from 9.1 `tls-auth-clients-user URI` authenticates a client
  in the handshake as the ACL user whose name equals a URI name of its
  certificate (9.0 has only `CN`, which names one pod); users from a
  file loaded whole or not at all, which closes the sessions of users it
  drops; no ALPN; no peer authentication, every connection starting as
  the default user. NATS: `verify_and_map` on a certificate's names and
  `handshake_first` for TLS from the first byte.

## Decision

### No password, anywhere

- The network reaches an addon of these types over TLS 1.3 with a client
  certificate under the platform's root, or not at all; loopback TCP in
  the addon's pod, which a port-forward dials, included. No rule asks for
  a password, no role or user has one, and a password login never
  succeeds. A reference renders no password, and one that asks for a
  user or a password is an error that says why.
- **The server certificate** is issued by the control plane's issuer
  (ADR 0024) for the addon's workload ID as its one URI name and no
  other name: no DNS name (the addon has a bare label the issuer may not
  sign) and no IP name (the address moves with the pod). Its subject is
  a pod certificate's: the addon's pod in the CN, the addon's set ID in
  the `OU`. ServerAuth, a day, renewed at three quarters by the
  database's clock and issued anew for a new pod or a new key; the
  controller keeps it in memory, state it can always make again.
- **The key** is Ed25519, generated once by `ferund` as the addon's
  credential `tls.key`, sealed like every credential and written to the
  addon's volume for the server's user alone, since these servers read
  their key from a file. It protects connections to data that lives on
  the same volume. A provider that let the server ask `feos-authn` for
  the signature would replace it, and nothing of the grants would
  change.
- **A client verifies the server** by the chain to the platform's root
  and by the addon's ID, which the reference knows: FeID's pinned
  verifier, which the guest's binding is.

### The map is the grant

- One entry per pair of a referencing workload and a role: every
  reference of the target's services and jobs to the addon that names a
  role, an attached service's as its sandbox's (ADR 0035). The
  controller derives them with the function that derives the pods' sets,
  so that the map and the network name a workload by the same string
  (ADR 0012).
- **Never a name.** An entry names the workload by what the issuer wrote
  and no tenant chooses: the `OU`, a hash over the environment's ID, the
  object's kind and its name, or the SPIFFE ID, which carries the
  tenant, the environment's ID and the object at once. A service `api`
  of another tenant or environment has another entry and matches
  nothing, and the network would not let it reach the port in the first
  place.
- The map decides who may become a role, not what the role may do. Two
  workloads may share a role, and an administrator's role is never in
  the map.
- An entry goes with its reference, in the deployment that drops it.

### `ferund` does it as provisioning

- The key, the chain, the trust and the map reach the addon in
  provisioning steps (ADR 0040), each file written beside its place and
  moved over it, the rules checked by the server's own parser where it
  has one before they take effect, the files of before put back on any
  error. Every input travels in the step's environment, the key as a
  credential and the rest as literals, so a grant, a renewed
  certificate, a new root or a new pod changes the step's hash and the
  reconciler runs it again. No timer in the container, no sidecar.
- The readiness probe asks over the server's socket in the container and
  presents no certificate: one that expired while the addon was stopped
  would keep it unready, an unready addon is never provisioned, and its
  certificate would never be renewed.
- A former password credential is deleted once no instance's template
  names it, so that a pod of before still restarts with what it was
  made with, and then fails at the server.

### What a reference renders

- A reference's keys name the addon's loopback address in the
  referencing pod, one address per addon however many references name
  it, numbered by the addons' names; the addon's port; the role; and a
  URL without a password. The controller puts one outbound TLS binding
  per referenced addon into the pod, which dials the addon by its name
  from the pod's address, presents the pod's certificate and pins the
  addon's ID (ADR 0014). No spelling in the spec asks for it: with
  certificates only, there is no other way in.
- The URL asks for no TLS of the client's own: the driver speaks
  plaintext to loopback, since TLS of its own inside the guest's would
  reach the server as garbage.
- The addon's address is in no value: a move of the addon rolls no
  dependent, and the binding resolves the name for every connection.

### PostgreSQL

- `pg_hba.conf` has two rules and nothing else: the Unix socket in the
  container for the superuser by `peer`, through which `ferund`
  provisions as root; and `hostssl` for everyone, `cert` with
  `clientname=DN` and the map. No `host`, no `hostnossl`, no password
  method; TLS 1.3 only.

  ```text
  local     all  postgres        peer  map=ferun
  hostssl   all  all       all   cert  clientname=DN map=feid
  ```

- The map `feid` has one line per grant: a regular expression anchored
  at both ends on the workload's `OU`, the CN matched for its shape
  alone, so that a grant holds for every replica and every roll; the
  SPIFFE ID stands in a comment above it. The issuer refuses a subject
  with anything but letters, digits, `.`, `/` and `-`, so no value
  carries a comma or a second attribute into the string the expression
  reads.

  ```text
  # spiffe://run.fecode.dev/tenant/fecode/env/01a196e9-…/service/catalog
  feid  "/^CN=[^,]+,OU=wl-3f9c0a7e2b514d86c1e9f03a7b2d4e58$"  catalog
  ```

- Roles have no password: provisioning sets `PASSWORD NULL` on every
  listed role and on the superuser. The superuser is never in the map,
  and a reference must name a role. The image's entrypoint needs a
  superuser password for `initdb` on a fresh volume, so that credential
  stays in the pod's environment for that alone; provisioning removes it
  from the role seconds later, and it opens nothing.
- The server certificate is ServerAuth alone. The `tls` step turns TLS
  on under the rules of before, then writes the rules and the map,
  checks them through the server's own parser, reloads, and ends every
  session that came over the network without TLS. PostgreSQL logs every
  refused connection itself; successful handshakes are audited by
  `feos-authn`'s signature records (ADR 0014).
- Versions 16 and 17 get the same server side. A referencing pod's
  binding offers ALPN `postgresql`, TLS from the first byte, which
  PostgreSQL takes from 17; a version that starts TLS inside its
  protocol is not reached through a binding (ADR 0014).

### Valkey

- Version 9 alone, the first line that maps a certificate's URI to a
  user. 7.2 and 8 are retired into it: an addon that names one runs 9 on
  its volume, which keeps its data, and every plan says so in a warning
  that asks for version 9; nothing is refused. Going back to an older
  version than the data's stays refused.
- Valkey listens on TLS 1.3 alone, no plaintext port, client
  certificates required, authenticated as the user named by the
  certificate's URI. The default user is off, with no password, keys,
  channels or commands; every user is `resetpass`, never `nopass`, so
  `AUTH` never succeeds and a certificate that names no user gets
  `NOAUTH` for every command. The socket in the container admits nobody
  either once TLS is on.
- One ACL user per granted workload, named by its SPIFFE ID, with `on
  resetpass ~* &* +@all -@admin -migrate`: every key and channel and
  every data command; none of `@admin` (users, configuration,
  persistence, replication, shutdown, other clients, `MONITOR`) and not
  `MIGRATE`, which would have the addon dial a server of the caller's
  choosing. The referencing workloads share one keyspace; a reference
  grants the one role `data`, whatever its key.
- **The addon is administered as itself.** Valkey has no peer
  authentication, and its only way in is a certificate, so the addon's
  own ID is a user with every right, and its server certificate has
  ClientAuth besides ServerAuth: `ferund`'s step presents it over
  loopback TLS in the container, verifying no server, since it is its
  own container's and its certificate may be the expired one the step
  replaces. Only the addon's pod holds a certificate of its ID. This is
  one identity at both ends of the addon's own connections, not two
  purposes for one key.
- The users are a file on the volume, written whole and loaded with `ACL
  LOAD`, which takes the file whole or not at all and closes the
  sessions of users no longer there; the server reads it at every start,
  so a restart needs no step. Without key, chain, trust and users on the
  volume (a fresh one, or one of 8) the server starts in a bootstrap: no
  TCP, the socket alone, the default user on, protected mode on. The
  `tls` step then sets TLS over the socket, opens the port and loads the
  users, which turn the default user off; between the port and the
  users, protected mode refuses every connection that is not the pod's
  own loopback, and the port takes none without a certificate under the
  root anyway. The step ends by checking that the server takes the
  addon's own certificate as the addon's ID.
- The binding offers no ALPN: Valkey has no protocol ID, selects none
  and ignores an offer, and an ID of FeOS's own would assert nothing and
  could clash with one Valkey registers. A binding with an empty ALPN
  offers none.
- A client takes the URL as it is: no userinfo, so no `AUTH`, and RESP3's
  `HELLO` needs none. TLS of its own to the loopback address, a user or
  a password, the addon's name directly, Sentinel or Cluster all fail.

### NATS

- NATS keeps its password credential until its certificate map is
  decided (open question).

## Alternatives considered

- **A password rule while a reference asks for one**, the first draft
  for both types: refused. Passwords go at once, and the applications
  are made to work by the binding, not by a way around it.
- **A port of its own for TLS** in PostgreSQL: it has none; the rules
  separate TLS from plaintext on the one port.
- **The CN alone** (`clientname=CN`, `tls-auth-clients-user CN`): the CN
  names one pod, so an entry would hold one replica until its next roll.
  It fails closed, and it is not a grant.
- **An exact DN line**: the subject holds the pod's CN, so it is one pod
  again.
- **The URI name in PostgreSQL**: no released version reads it; the `OU`
  carries the workload for exactly this reason.
- **The service's name in the map**: not unique; any tenant names any
  object anything in any environment.
- **A role per workload**, the role equal to the ID: roles stay the
  addon's option and the owners of their schemas, grants the
  deployment's references, and two workloads may share a role.
- **A user per role of the options in Valkey**, as PostgreSQL's: its
  options have none, and no reference names one.
- **A proxy in front of the addon** that terminates mutual TLS: a hop
  and a process for what the server does natively, which then has to be
  passed an identity it cannot read.
- **The password offloaded into the stream by `feos-authn`**: a password
  would remain, stored, rotated and leakable, and the plaintext would
  pass through the host.
- **Key and certificate at the pod's start**, by a wrapper before the
  server: a renewed certificate would roll the single instance every
  day. A step and a reload cost nothing.
- **An IP name in the server certificate** for `libpq`'s `verify-full`:
  a reissue per move for a check no client of the platform makes.
- **`log_connections`** for an audit of successes: every probe and every
  step a line in the log, and `feos-authn` audits the handshakes
  already.
- **Admission on the chain**, the CN equal to the role and no map: every
  workload of every tenant is under one root (ADR 0014).
- **Refusing Valkey 7.2 and 8** with an error: every running addon would
  stop at the release, and every definition that says 8 would fail its
  next deployment. An addon of 8 has to become 9 for a certificate
  anyway, and 9 takes its data.
- **`ACL SETUSER` from the step**: a difference to compute against the
  running users, removals to remember, and users a restart loses unless
  an `ACL SAVE` follows, which needs the file anyway.
- **A password for `ferund` over Valkey's socket**: `AUTH` is open to
  every unauthenticated connection, TLS's too, and no ACL rule binds a
  user to the socket. It would be a password that opens the addon.
- **The default user as `ferund`'s, on the socket alone, by protected
  mode**: protected mode refuses every connection from elsewhere before
  the certificate's user is looked up, so no workload would get in.
- **The probe over TLS as the addon's ID**: it would see the append-only
  file loading, but an expired certificate would lock provisioning out,
  and every probe would be a handshake.
- **An ALPN of FeOS's own for Valkey** (`redis`, `resp3`): Valkey
  ignores it.

## Consequences

- Every connection a pod opens to an addon is a handshake with one
  signature by `feos-authn`; pools keep that small.
- A removed reference removes the grant with the deployment. In Valkey
  `ACL LOAD` ends the workload's sessions; in PostgreSQL a session that
  is open stays until it ends, since the map is read at login and
  nothing revokes a pod's certificate before its end (ADR 0014).
- A `uses` without a variable opens the port (ADR 0012) but grants
  nothing and makes no binding: a call whose address is in the code
  presents no certificate.
- A sandbox's spec is fixed at its create and names no bindings, so an
  attached service, granted as its sandbox, reaches an addon with FeID
  alone.
- The addon's TLS key lives on its volume, beside the data it protects.
- Definitions name no `user` or `password` and Valkey 9; one with 8
  deploys with a warning, one whose reference asks for a password fails
  its plan with a finding that says why.

## Open questions

- **NATS**: `verify_and_map` on the certificate's URI and
  `handshake_first`, how its server key stays out of its pod, and what
  a reference renders; until then NATS is the one addon with a password.

## History

PostgreSQL was decided and built 2026-11-08 (e188260), at first with a
`url` to the addon's address asking for `sslmode=require`, so that every
client without the workload's certificate was refused by the server,
until the guest's outbound TLS bindings came the same day (2e1bfce) and
every reference moved onto one. Valkey followed the same day as ADR 0039
(2f2a0ae), with the bindings offering no ALPN (a0a31b6), and was merged
here on 2026-11-08. ADR 0036's open question on an addon's own server
key is answered here by a key of the addon's own on its volume.
