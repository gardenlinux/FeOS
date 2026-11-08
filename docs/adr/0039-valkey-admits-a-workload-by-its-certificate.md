# ADR 0039: Valkey admits a workload by its certificate and by nothing else — TLS 1.3 alone with a certificate of the addon's ID, an ACL user per referencing workload named by its SPIFFE ID, the default user off and no password anywhere; the addon is administered as itself; 7.2 and 8 retire into 9

**Status:** Accepted · **Date:** 2026-11-08

**Touches:** ADR 0038 (its machinery for a second type: the server
certificate, the key credential, the grants and a provisioning step
whose hash covers them; PostgreSQL is unchanged), ADR 0036 (the second
addon that maps a workload's ID, on the URI this time; a TLS binding to
an upstream without an ALPN protocol offers none), ADR 0018 (the valkey
blueprint: one version, no password, no `user` or `password` key; a
reference is its grant), ADR 0034 (unchanged: the same reference opens
the port), ADR 0035 (an attached service's grant is its sandbox's).

## Context

- **Until now a Valkey addon was a password.** The blueprint ran
  Valkey 7.2 or 8 from the official image with `--requirepass` for the
  default user, plaintext on 6379 from every address. A reference
  resolved to `redis://default:<password>@[<address>]:6379/0`, or to
  `host`, `port`, `user` and `password`. Every referencing workload was
  the default user and could do everything: `CONFIG`, `ACL`,
  `SHUTDOWN`, `MONITOR`, `MIGRATE`. The password sat in every
  referencing pod's environment for as long as it ran. `kino`'s three
  environments are the only ones on the platform with a Valkey addon,
  all on 8; its services `catalog`, `seats`, `orders` and `notify`
  reference it by `url`.
- **ADR 0038** made PostgreSQL admit a workload by its certificate: a
  server certificate of the addon's ID, a map from the referencing
  workloads to their roles, both written by a provisioning step whose
  hash moves with them, and no password. The guest's outbound TLS
  bindings (ADR 0036) carry a client's plaintext from a loopback
  address of its pod to the addon in TLS 1.3 from the first byte, with
  the pod's certificate and the server pinned to the addon's ID.
- **What Valkey offers.**
  - TLS on a port of its own (`tls-port`), `port 0` for no plaintext,
    client certificates required (`tls-auth-clients yes`), the
    protocols (`tls-protocols`); `CONFIG SET` reloads the files at once.
  - From 9.1, `tls-auth-clients-user URI` authenticates a TLS client
    during the handshake as the first enabled ACL user whose name equals
    a URI name of its certificate. Without a match the client stays the
    default user, unauthenticated unless that user has no password. 9.0
    has only `CN`, which names one pod (ADR 0036).
  - Users come from an ACL file (`aclfile`), read at the start and by
    `ACL LOAD`, whole or not at all, which closes the connections of
    every user the file no longer has; or from `ACL SETUSER`.
  - No ALPN: the server selects none and ignores a client's offer.
  - No peer authentication: every connection, the socket's too, starts
    as the default user. Protected mode refuses every connection but
    loopback's and the socket's while the default user has no password.
  - The image's `9-alpine` is 9.1.3 today (9.1 since 2026-07-15; 9.2 is
    a release candidate); `9.0-alpine` is 9.0.7.

## Decision

### 1. Version 9 alone; 7.2 and 8 retire into it

- The blueprint offers 9, `valkey/valkey:9-alpine`, the first line
  that maps a certificate's URI to a user. An instance pins the image's
  digest at its insert. There is no 9.0.
- 7.2 and 8 are **retired**: an addon that names one runs 9 on its
  volume, which keeps its data (9 reads what 8 wrote: checked with
  8.1 → 9.1.3), and every plan says so in a warning that asks for
  version 9. Nothing is refused. A running addon of 8 is recreated as 9
  by the release that ships this, once, on its volume. A definition that
  still says 8 deploys, and writing 9 later changes nothing that runs:
  the template is the same.
- Going back to an older version than the data's stays refused; there
  is no older one now.

### 2. No password, anywhere

- Valkey listens on TLS 1.3 alone: `port 0`, `tls-port 6379`,
  `tls-auth-clients yes`, the platform's roots and issuer as its CA
  file. A client without a certificate under the root, or with TLS 1.2,
  fails the handshake.
- The default user is off, with no password, keys, channels or
  commands. No user has a password: every one is `resetpass`, never
  `nopass`. So `AUTH` never succeeds, not with the password of before,
  not as a granted user, and a certificate that names no user gets
  `NOAUTH` for every command.
- The credential `password` goes. Its system secret is deleted once no
  instance's template names it, as ADR 0038's former role passwords.
- The socket in the container admits nobody either once TLS is on.

### 3. The server certificate, and the addon as its own administrator

- As ADR 0038 §2: issued by the control plane's issuer for the addon's
  workload ID, `spiffe://<td>/tenant/<t>/env/<environment ID>/addon/<name>`,
  as its one URI name, the subject a pod certificate's, 24 hours,
  renewed at three quarters; the key the credential `tls.key`, Ed25519,
  on the volume under `/data/ferun`, the server's user's, 0600.
- **ferund needs a way in, and Valkey's only way in is a certificate.**
  PostgreSQL's steps come over its socket as root; Valkey has no peer
  authentication. So the addon is administered as itself: its own ID is
  a user with every right, and its server certificate has ClientAuth
  besides ServerAuth, so that ferund's step presents it to the server
  over loopback TLS in the container.
- Only the addon's pod holds a certificate of its ID: this key on its
  volume, and its pod certificate, whose key is `feos-authn`'s. The
  network lets no pod reach the port but those of the references (ADR
  0034), and none of them has the addon's ID.
- This is one identity at both ends of the addon's own connections, not
  the two purposes ADR 0036 keeps apart (the network's and the
  application's). PostgreSQL's certificate stays ServerAuth alone.

### 4. A user per workload, named by its ID

- Valkey maps a certificate's URI to the user of that name, so every
  workload that a reference of the target names gets a user named by
  its SPIFFE ID: a service's or a job's object ID, an attached
  service's sandbox's (ADR 0036). The controller derives them with
  ADR 0038's grants, by the function that derives the pods' sets.
- **Never a name.** The ID carries the tenant, the environment's ID and
  the object at once, and the issuer writes it: a service of the same
  name in another environment or tenant has another ID and is nobody
  here. The CN, which names one pod, is never read.
- **The rights**: `on resetpass ~* &* +@all -@admin -migrate`. Every key
  and channel and every data command: strings to streams, scripts and
  functions, pub/sub, transactions, `FLUSHALL` of the data it shares.
  None of `@admin`: users, configuration, persistence, replication,
  `SHUTDOWN`, other clients (`CLIENT LIST`, `KILL`, `PAUSE`), `MONITOR`,
  `SLOWLOG`, `LATENCY`, `FAILOVER`. Not `MIGRATE`, which would have the
  addon dial a server of the caller's choosing. `DEBUG` and `MODULE`
  are off for everyone, as the image has them.
- The referencing workloads share one keyspace, as they shared the
  default user. A reference grants the role `data`, the only one,
  whatever its key (`url`, `host`, `port`).
- **A user goes with its reference**, in the deployment that drops it,
  and so do its sessions: `ACL LOAD` closes them.

### 5. The users' file, the start and the step

- The users are a file on the volume, `/data/ferun/users.acl`, which
  the step writes whole and loads with `ACL LOAD`. A load takes the
  whole file or nothing: on an error the users of before stay, and the
  step puts their file back. Valkey reads the file at every start, so a
  restart needs no step.
- **The start** is the container's command, `sh -c` between the image's
  `tini` and its entrypoint, which hands `/data` to the server's user
  and drops to it. With key, certificate, trust and users on the volume
  it serves TLS on 6379 from its first second. Without them (a fresh
  volume, or one of 8) it starts in the bootstrap: no TCP at all, the
  socket alone, the default user of before (on, no password), protected
  mode on.
- **The step** `tls`, once the controller has the certificate:
  - key, chain, trust and users written beside their places and moved
    over them;
  - in the bootstrap (the socket answers `PONG`), over the socket: TLS's
    files and settings, then the port, then the users, which turn the
    default user off;
  - else over loopback TLS as the addon's ID: TLS's files and settings
    again, so that a renewed certificate or a new root takes the next
    handshake without a restart, and the users;
  - then it checks that the server takes its own certificate as its ID.
- Between the port and the users in the bootstrap the default user still
  has no password. Protected mode refuses every connection that is not
  the pod's own loopback, and pods reach the addon from their own
  addresses; the port takes none without a certificate under the root
  anyway. Bad TLS files fail the port's opening, which changes nothing.
- The step's own TLS connection verifies no server: it is its own
  container's, on loopback, and its certificate may be the expired one
  the step replaces.
- Every input travels in the step's environment, the key as a
  credential, the rest as literals; a grant, a renewed certificate, a
  new root or a new pod changes the hash, and ADR 0018's reconciler runs
  the step again. The pod's template has no credential left: the key
  reaches the volume by the step alone.
- **The probe** asks the socket for `PING`: `PONG` in the bootstrap,
  `NOAUTH` after. It presents no certificate: one that expired while
  the addon was stopped would keep it unready, an unready addon is never
  provisioned, and its certificate would never be renewed. It does not
  see the append-only file loading, since `NOAUTH` comes first; clients
  retry for those seconds.

### 6. What a reference renders

- The keys are `host`, `port` and `url`; `user` and `password` are gone,
  and a reference that names one is an error that says why. No fields.
- Every reference reaches the addon through its pod's TLS binding, as
  ADR 0038's step 2: `host` the addon's loopback address in the pod,
  numbered with the pod's other bound addons by name (`kino`: postgres
  127.64.0.1, valkey 127.64.0.2), `port` 6379, `url`
  `redis://<loopback>:6379/0`.
- The binding dials the addon by its name from the pod's address, TLS
  1.3 from the first byte with no ALPN, the pod's certificate and the
  server pinned to the addon's ID. **No ALPN**: Valkey has no protocol
  ID, selects none and ignores an offer; an ID of FeOS's own would
  assert nothing and could clash with one Valkey registers. A binding
  with an empty ALPN offers none, and the guest and `feosd` take one.
- The addon's address is in no value: a move of the addon rolls no
  dependent; the binding resolves the name for every connection.
- **What a client must not do**, and what it gets:
  - `rediss://`, or TLS of its own, to the loopback address: TLS inside
    the binding's TLS, which the server takes for a command and drops;
  - a user or a password: `AUTH` fails with `WRONGPASS`, and most
    clients then give up;
  - the addon's name or address directly: plaintext to a TLS port; only
    a client with the workload's certificate (FeID) gets in that way;
  - Sentinel or Cluster: there is one server.
- Common clients (go-redis, redis-py, node-redis, ioredis, Jedis,
  Lettuce) take the `url` as it is: no userinfo, so no `AUTH`, and RESP3's
  `HELLO` needs none.

## Alternatives considered

- **9.0 and `tls-auth-clients-user CN`**: the CN names one pod (ADR
  0036), so a user would hold one replica until its next roll.
- **Refusing 7.2 and 8** with an error: `kino`'s addons would stop at
  the release, and every definition that says 8 (FeCode's agent writes
  it today) would fail its next deployment. An addon of 8 has to become
  9 for a certificate anyway, and 9 takes its data.
- **8 with its password until clients move**, per addon: refused, as in
  ADR 0038. Passwords go at once.
- **`ACL SETUSER` from the step**: a difference to compute against the
  running users, removals to remember, and users that a restart loses
  unless an `ACL SAVE` follows, which needs the file anyway.
- **A password for ferund, over the socket**: `AUTH` is open to every
  unauthenticated connection, TLS's too, and no ACL rule binds a user to
  the socket. It would be a password that opens the addon.
- **The default user as ferund's, on the socket alone, by protected
  mode**: protected mode refuses every connection from elsewhere before
  the certificate's user is looked up, so no workload would get in.
- **The probe over TLS as the addon's ID**: it would see the loading,
  but an expired certificate would lock provisioning out, and every
  probe would be a handshake.
- **A user per role of the options**, as PostgreSQL's: Valkey's
  options have none, and no reference names one; key patterns per
  workload come with such an option (open question).
- **A TLS proxy in front of Valkey**: a hop and a process for what
  Valkey does natively, which then has to pass Valkey an identity.
- **An ALPN of FeOS's own** (`redis`, `resp3`): Valkey ignores it today.

## Consequences

- `addons`: valkey is a `CertificateServer` and a `TLSBinder`;
  `CertificateServer.ProvisionsAsItself` and `TLS.ID`; credentials
  `tls.key` alone.
- `pki`: `IssueAddonServer` adds ClientAuth for an addon that is
  provisioned as itself.
- `ctl`: unchanged but for the issuance; the grants, the step's hash,
  the bindings and the deletion of the former password are ADR 0038's.
- The guest's TLS bindings and `feosd`'s check take an empty ALPN and
  offer none. Nodes first, then `ferund`: a node of before refuses a
  pod whose binding has none.
- `kino`'s three addons are recreated as 9 on their volumes at the
  release, once each, and its four services roll to their bindings.
- FeCode: its agent's docs say version 8 and the keys `user` and
  `password`. A definition with 8 deploys with a warning; one whose
  reference names `user` or `password` fails its plan with a finding
  that says why. They move to 9 and `url`.
- A preview's attached service is granted, as for PostgreSQL, but a
  sandbox has no TLS bindings yet (ADR 0036), so it does not reach
  Valkey until it has.
- `uses: [{addon: cache}]` opens the port (ADR 0034) but grants no user
  and makes no binding: a call whose address is in the code presents no
  certificate.

## Open questions

- **Keys per workload**: ACL key and channel patterns per reference,
  with an option that names them, as PostgreSQL's roles.
- **`tls.key` on the volume**, as ADR 0038's, until an OpenSSL provider
  lets Valkey ask `feos-authn` for the signature.
- **A new root that the old trust does not hold**: the step's own
  connection would be refused until a restart reads the new files.
- **The loading** of a large append-only file, which the probe cannot
  see without a certificate.
