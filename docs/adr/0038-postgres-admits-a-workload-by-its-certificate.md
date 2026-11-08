# ADR 0038: PostgreSQL admits a workload by its certificate and by nothing else — no password on the network or in a role; the addon terminates TLS itself with a certificate of its own ID, maps a client's certificate by its workload to the role a reference grants, and `ferund` writes key, chain, rules and map as provisioning steps

**Status:** Accepted · **Date:** 2026-11-08

**Touches:** ADR 0036 (the first addon that maps a workload's ID: on
the subject's `OU` with `clientname=DN`; its open question on the
addon's server key is answered for now by a key of the addon's own),
ADR 0018 (the postgres blueprint takes no password and provisions TLS;
a reference to a role is its grant and must name one), ADR 0014 (the
server certificate follows the pattern of ADR 0036's network credential:
the same ID, a key of its own, a purpose of its own), ADR 0034
(unchanged: the same reference opens the port; the map names a workload
by the ID its sets carry), ADR 0035 (an attached service's grant is its
sandbox's), ADR 0037 (an environment's ID, never its name, is in the
IDs).

## Context

- **Until now a role was a password.** The postgres blueprint runs the
  official image. `options.roles` were login roles with a generated
  password each. A reference `{addon: db, role: app, key: url}` resolved
  to `postgres://app:<password>@[<address>]:5432/app?sslmode=disable`:
  the password sat in the pod's environment for as long as it ran, the
  connection was plaintext, and the server had no certificate.
  `ferund` provisioned as the superuser over the pod's loopback with the
  superuser's password. `kino`'s three environments are the only ones
  on the platform with a PostgreSQL addon, all on 17.
- **ADR 0036** decided that a reference to an addon's role grants that
  role to the referencing workload, that an addon maps the workload's
  certificate to the role and never admits on the chain alone, and that
  a client presents the ID through FeID or through an outbound TLS
  binding in the guest: TLS from the first byte with the upstream's
  ALPN, `feos-authn` signing. A workload's certificate carries its
  SPIFFE ID as its one URI name, with the environment's ID; its subject
  is `CN=<td>/<pod ID>` and one `OU`, the workload's ID as its sets on
  the nodes carry it: `wl-<32 hex>` for a service, a job or an addon,
  the first half of SHA-256 over the environment's ID, the object's
  kind and its name; `wl-s-<sandbox ID>` for a sandbox. The issuer
  writes the subject; nothing of it is readable as a name.
- **What PostgreSQL offers.** The method `cert`: the client's
  certificate is verified against `ssl_ca_file`, and its CN, or with
  `clientname=DN` its whole subject in RFC 2253 form, must map to the
  requested role through a `pg_ident.conf` map, exact or by a regular
  expression; no password is consulted. TLS shares the port with
  plaintext: 5432 takes `SSLRequest` and, from 17, TLS from the first
  byte with ALPN `postgresql`. PostgreSQL has no port of its own for
  TLS; `hostssl` and `hostnossl` rules decide. TLS files, rules and the
  map are read again on a reload, which drops no session. No released
  version reads a URI name.

## Decision

### 1. No password, anywhere

- The network reaches the addon over TLS with a client certificate
  under the platform's root, or not at all. There is no rule that asks
  for a password: not for the network, not for loopback TCP, which a
  port-forward dials in the pod. Everyone who connects over TCP presents
  a certificate.
- Roles have no password: provisioning sets `PASSWORD NULL` on every
  listed role and on the superuser. The image's entrypoint needs a
  superuser password for `initdb` on a fresh volume, so the credential
  `password` stays in the pod's environment for that alone; provisioning
  removes it from the role seconds later, and it opens nothing. Dropping
  it from the template would recreate every addon's pod and is left for
  a roll that recreates them anyway.
- `ferund` provisions over the Unix socket in the container, as root,
  which a `peer` rule maps to the superuser; nothing else may use the
  socket. An operator who must be the superuser execs into the addon's
  pod and does the same.
- The roles' former passwords, system secrets `addon.<a>.role.<r>.password`,
  are deleted by the controller once no addon of T or R has them.

### 2. PostgreSQL terminates TLS itself, with a certificate of the addon's ID

- TLS 1.3 only, a server certificate and key, and the platform's roots
  and issuer as `ssl_ca_file`. Nothing stands in front of the server.
- **The server certificate** is issued by the control plane's issuer
  (ADR 0024) for the addon's workload ID,
  `spiffe://<td>/tenant/<t>/env/<environment ID>/addon/<name>`, as its
  one URI name and no other name: no DNS name (the addon has a bare
  label the issuer may not sign) and no IP name (the address moves with
  the pod). Its subject is a pod certificate's, by the same function:
  `CN=<td>/<addon pod ID>`, `OU=` the addon's workload ID. ServerAuth
  only, 24 hours, renewed at three quarters by the database's clock and
  issued anew for a new pod or a new key.
- The controller keeps the certificate in memory: it is state it can
  always make again, and a new leader that issues anew costs one reload.
- **The key** is Ed25519, generated once by `ferund` as the addon's
  credential `tls.key`, sealed like every credential, and written to the
  volume beside `PGDATA`, owned by the server's user, 0600, since
  PostgreSQL reads its key from a file. The key protects connections to
  data that lives on the same volume. The pod certificate replaces it
  when an OpenSSL provider lets PostgreSQL ask `feos-authn` for the
  signature (ADR 0014's open question); then the key goes and nothing of
  the grants changes.
- **A client verifies the server** by the chain to the platform's root
  and by the addon's ID, which the reference knows: the `feid` crate's
  pinned verifier does exactly that, and the guest's binding is that
  crate.

### 3. The map is the grant, by tenant, environment and object at once

- `pg_hba.conf` reads the whole subject (`clientname=DN`) and the map
  `feid` in `pg_ident.conf` has one line per pair of a referencing
  workload and a role: a regular expression anchored at both ends on the
  workload's `OU`, the full SPIFFE ID in a comment above it:

  ```text
  # spiffe://run.fecode.dev/tenant/fecode/env/01a196e9-…/service/catalog
  feid  "/^CN=[^,]+,OU=wl-3f9c0a7e2b514d86c1e9f03a7b2d4e58$"  catalog
  ```

- **Never a name.** The `OU` is not the service's name, which any tenant
  may give any object in any environment. It is a hash over the
  environment's ID, the object's kind and its name. An environment's ID
  is unique across the platform and belongs to one tenant, so the `OU`
  names the tenant, the environment and the object at once. The issuer
  writes it; a tenant cannot choose it. A service `api` of another
  tenant or environment has another `OU` and matches no line, and the
  network would not let it reach the port in the first place (ADR 0034).
- The CN, which names one pod, is matched for its shape alone, so that
  a grant holds for every replica and every roll. The issuer refuses a
  subject with anything but letters, digits, `.`, `/` and `-`, so no
  value can carry a comma or a second attribute into the string the
  expression reads. The expression is quoted, since the map's tokenizer
  splits an unquoted field at a comma.
- **Which lines**: every reference of the target's services and jobs to
  the addon that names a role. A service's or job's line is its
  `wl-<32 hex>`; an attached service's is its sandbox's `wl-s-<id>`,
  since it has the sandbox's identity (ADR 0036). The controller derives
  them with the function that derives the pods' sets, so the map and the
  network name a workload by the same string.
- **The role is the same role.** The map decides who may become `app`,
  not what `app` may do: the role keeps its login, its CONNECT and
  CREATE on every listed database and its own schema in each.
- **The superuser is never in the map**, and a reference must name a
  role: one without is an error of the write.
- **A line goes with its reference**, in the deployment that drops it.

### 4. The rules

```text
local     all  postgres        peer  map=ferun
hostssl   all  all       all   cert  clientname=DN map=feid
```

with the map `ferun` taking `root` (and the image's own `postgres`) in
the container to the superuser. Nothing else: no `host`, no `hostnossl`,
no password method.

### 5. `ferund` does it as provisioning

- **`provision`**, over the socket: the superuser's password removed,
  the databases and roles created as before, every role `PASSWORD NULL`.
- **`tls`**, when the controller has the certificate: first the key,
  the chain and the trust on the volume, each written beside its place
  and moved over it; `ssl`, the files, TLS 1.3 and `shared_buffers` as
  the command line has it (so that a reload finds nothing to complain
  about) with `ALTER SYSTEM`; a reload. Then `pg_hba.conf` and
  `pg_ident.conf` whole, checked by the server's own parser through
  `pg_hba_file_rules` and `pg_ident_file_mappings` before a reload takes
  them, the previous files put back on any error; a reload; and the end
  of every session that came over the network without TLS, which the
  rules of before had admitted.
- Every input travels in the steps' environment: the key as a
  credential, the rest as literals. A grant, a renewed certificate, a
  new root or a new pod changes the steps' hash, and ADR 0018's
  reconciler runs them again. No timer in the container, no sidecar.
- **The probe** asks over the socket (`pg_isready`, peer-authenticated)
  and checks the TCP listener with `nc -z`, which sends nothing: over
  TCP no rule would take the probe, and every probe would be a line in
  the log. The entrypoint's temporary server has no TCP, so the probe
  still reports the real server alone. The probe is no part of the
  template, so the change recreates no pod.
- PostgreSQL logs every refused connection by itself
  (`connection requires a valid client certificate`, `certificate
  authentication failed for user …`, `no pg_hba.conf entry … no
  encryption`). `log_connections` stays off; the successful handshakes
  are audited by `feos-authn`'s signature records (ADR 0014).
- Versions 16 and 17 get the same server side. The guest's binding
  speaks TLS from the first byte, which only 17 takes.

### 6. What a reference renders

- The keys are `host`, `port`, `user`, `database` and `url`; `password`
  is gone, and every reference names a role.
- **Step 1, this ADR's first release:** `url` is
  `postgres://<role>@[<address>]:5432/<database>?sslmode=require`,
  without a password. A client without the workload's certificate
  reaches the server, negotiates TLS and is refused with `connection
  requires a valid client certificate`. This is deliberate: every
  application on a PostgreSQL addon stops authenticating until step 2,
  `kino` in its three environments included, and the addon's log shows
  why.
- **Step 2, the guest's binding** (ADR 0036, built by the FeID work):
  every reference to a role resolves to `host` 127.0.0.1, a loopback
  port per addon in the pod, the role, no password, and `url`
  `postgres://<role>@127.0.0.1:<port>/<database>`. The controller puts
  one outbound TLS binding per referenced addon into the pod: it dials
  the addon by its name from the pod's own address, TLS from the first
  byte with ALPN `postgresql`, verifies the server by the addon's ID,
  and presents the pod's certificate, its CertificateVerify signed by
  `feos-authn`. No spelling in the spec asks for it: with certificates
  only, there is no other way in.

## Alternatives considered

- **A password rule while a reference asks for one**, the first draft
  of this ADR, to retire passwords per addon as clients moved: refused.
  Passwords go at once, and the applications are made to work by step
  2, not by a way around it.
- **A port of its own for TLS**: PostgreSQL has none, and the rules
  separate TLS from plaintext on the one port.
- **The CN alone**, PostgreSQL's default `clientname=CN`: the CN names
  one pod, so a line would hold one replica until its next roll. It fails
  closed, and it is not a grant.
- **An exact DN line**: the subject holds the pod's CN, so an exact line
  is one pod again.
- **The URI name**: no released PostgreSQL reads it. The `OU` carries
  the workload for exactly this reason.
- **The service's name in the map**: not unique; any tenant names any
  object anything in any environment.
- **A role per workload**, the role equal to the workload's ID: roles
  stay the addon's option and the owners of their schemas, grants the
  deployment's references, and two workloads may share a role.
- **A proxy in front of PostgreSQL** that terminates mutual TLS: a hop
  and a process for what PostgreSQL does natively, which then needs to
  pass PostgreSQL an identity it cannot read.
- **The password offloaded** into the stream by `feos-authn`: a password
  would remain, stored, rotated and leakable, and the connection's
  plaintext would pass through the host.
- **Key and certificate at the pod's start**, by a wrapper before the
  server: a renewed certificate would roll the single instance every
  day. A step and a reload cost nothing.
- **An IP name in the server certificate** for `libpq`'s `verify-full`:
  a reissue per move for a check no client of the platform makes.
- **`log_connections`** for an audit of successes in the addon's log:
  every probe and every step would be lines in it, and `feos-authn`
  audits the handshakes already.
- **Admission on the chain**, the CN equal to the role and no map:
  every workload of every tenant is under the one root. ADR 0036
  refused it.

## Consequences

- `addons`: `Context.TLS` with the server's chain, the trust and the
  grants; `CertificateServer` for blueprints that map certificates;
  postgres without role passwords and without `password`, the steps
  `provision` and `tls`, the probe over the socket. Valkey and NATS are
  untouched: Valkey maps no user from a certificate, NATS's
  `verify_and_map` may follow.
- `pki`: `IssueAddonServer` and `TrustPEM`; the subject by
  `PodSubject`, the ID by `WorkloadID`.
- `ctl`: the issuer in its dependencies, the key credential, the
  certificate in memory, the grants from the references, the former
  role passwords deleted.
- Every connection a pod opens is a handshake with one signature by
  `feos-authn` once step 2 is there; pools keep that small.
- FeCode needs no change for step 1: its definitions pass references
  through, and every reference of `kino` names a role. Its agent is told
  with step 2 that the URL has no password.

## Open questions

- **A session that outlives its grant**: a TLS session of a removed
  workload stays until it ends; `pg_stat_ssl` truncates the client's DN,
  so the step cannot match the session to the line it lost.
- **Revocation** of a pod certificate before its hour ends: none; the
  map, the network and the lifetime are the boundary (ADR 0014).
- **`tls.key` on the volume** until the OpenSSL provider, and whether a
  checkpoint or a clone with data (ADR 0016, ADR 0018) leaves it behind.
- **A sandbox without an attached service**, a developer's `psql`:
  `reaches` entries that name a role, as lines of the map.
