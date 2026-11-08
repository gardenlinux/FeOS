# ADR 0036: A workload's identity — one ID per service, job, addon and sandbox, in a certificate that says who and never what; an addon maps it to the role a reference grants, so that no password reaches the pod; within a cluster the address is the workload

**Status:** Proposed · **Date:** 2026-11-07 · **Revised 2026-11-08:**
the first version also had servers inside a cluster read their callers'
IDs, by a default scope in FeID and by inbound bindings in the guest.
Within a cluster the address is the workload (ADR 0034), so those are
parked until a call crosses a cluster.

**Touches:** ADR 0014 (amended: the layout of the IDs and what a sandbox
is called, its open question; a certificate's URI names its workload,
its CN the pod; outbound TLS bindings terminate in the pod, and `feos-authn` only
signs for them), ADR 0018 (a reference to an addon's role grants it to
the referencing object's ID), ADR 0037 (the environment is in the ID by
its ID), ADR 0028 (unchanged: bindings that add a
credential stay on the host), ADR 0034 (unchanged: the network decides
by address what the deployment declares; none of it is in a
certificate), ADR 0035 (sandboxes and tenants' jobs get IDs in their
environment), ADR 0013 (the control plane names both ends of a flow by
their IDs), ADR 0021 (unchanged: routed ports stay plain HTTP from the
ingress).

## Context

- **Within a cluster the address is the workload.** A pod sends from
  its own address only (ADR 0010), a node's /96 arrives only from the
  node that holds the key, the traffic between nodes is WireGuard's
  (ADR 0017), and `feosd` admits by address what the deployment's
  references declare (ADR 0012, ADR 0034). The application takes no
  part.
- **A pod's certificate** (ADR 0014) carries a SPIFFE ID, and its key
  stays in `feos-authn`. Today the issuer writes
  `spiffe://<td>/tenant/<tenant>/sandbox/<pod id>` for every pod: two
  replicas of one service have two IDs, and every roll makes new ones.
  Nothing that maps an ID to a role could keep up.
- **An addon's credential is a password today.** A reference to an
  addon's role (ADR 0018) gives the pod a password, which it holds as
  long as it runs, which leaks with its environment, and which changes
  only when the addon rotates it. PostgreSQL and NATS can map a
  client's certificate to a user instead.
- **Where the address stops naming the workload**, a call that crosses
  a cluster or comes from outside the platform, only a certificate
  can. No such call exists today.
- ADR 0014 leaves open the layout of the IDs and what a sandbox is
  called. Its TLS bindings give a client without FeID the identity,
  with the client's plaintext passing through `feos-authn` on the host.

## Decision

### The address within a cluster, the certificate where it stops

- **May A reach B's port?** The network answers, by address, from the
  deployment's references (ADR 0034). Inside a cluster no application
  checks its callers' certificates for it, and FeOS asks none to.
- **The certificate** serves an addon's credentials without a password
  in the pod (below) and, one day, calls whose address names no
  workload: across clusters, from outside the platform (a later ADR).
- **What a caller may do** is the application's or the addon's:
  methods, rows, roles. The platform has no policy language for it and
  reads no request.
- **On whose behalf** a workload calls, a user's token passed along, is
  ADR 0014's open question on tokens and a later ADR's.

### One ID per workload

- A pod's ID names the workload the pod is part of, never the pod:

  ```text
  spiffe://<td>/tenant/<tenant>/env/<environment ID>/service/<name>
  spiffe://<td>/tenant/<tenant>/env/<environment ID>/job/<name>
  spiffe://<td>/tenant/<tenant>/env/<environment ID>/addon/<name>
  spiffe://<td>/tenant/<tenant>/env/<environment ID>/sandbox/<id>
  spiffe://<td>/tenant/<tenant>/env/<environment ID>/tenant-job/<id>
  ```

- A service's, a job's and an addon's pods share their object's ID: every
  replica, every run and every pod after a roll. A deployment that keeps
  the object keeps the ID; one that removes it and adds one of the same
  name gives the new one the old ID, as it gives it the old name.
- A sandbox and a tenant's job, made by a call and not by the spec
  (ADR 0035), are one pod each and are named by their IDs (ADR 0019).
  Their segments differ from the objects', so a pod ID never reads as a
  job's name.
- An attached service runs in its sandbox's pod (ADR 0035) and has the
  sandbox's ID. Nothing in the pod separates its processes from the
  sandbox's other code, so no ID could.
- The environment is part of the ID, by its ID, which is its identity
  (ADR 0037): `api` of one environment is never `api` of another, and a
  rename changes no certificate. The tenant is named as the sink's
  labels name it (ADR 0026).
- The trust domain stays the platform's one, which `issuer.json`
  records (ADR 0024). The platform's own IDs (`node`, `ingress`, `sink`,
  `ferun`) are unchanged.
- A certificate has exactly one URI name, as an X.509-SVID must.

### The subject names the pod and its workload

- The subject's CN names the pod: the trust domain and the pod's ID,
  `run.fecore.dev/xmxfgckmkt6`. A pod ID is never given out twice, so
  every connection an addon logs names one microVM. It stays within the
  64 characters that RFC 5280 gives a CN for trust domains of up to 52.
- One `OU` attribute names the workload, by the ID its sets on the
  nodes carry (ADR 0034): `wl-<32 hex digits>` for a service, a job or
  an addon, `wl-s-<id>` for a sandbox, `wl-j-<id>` for a tenant's job.
  An addon that maps a subject maps this attribute, PostgreSQL with
  `clientname=DN` and a map anchored on it, which `ferund` writes.
- A verifier that checks only the CN, as PostgreSQL does by default,
  matches one pod and no other: a grant that should have been the
  workload's fails closed instead of reaching another workload.
- The subject carries no names: the tenant's, the environment's and the
  object's are in the URI. A readable attribute would be the next one
  that a verifier maps alone.
- This replaces ADR 0014's CN, the ID without its scheme, which is longer
  than 64 characters for many workloads and then has to be cut.

### A certificate says who, not what

- An application's certificate carries the ID, the DNS names of the
  service (ADR 0014) and the pod. It carries no role and no right, and
  none of what the network decides on: no reference, no `reaches`.
- Rights change with a deployment, a certificate lasts its lifetime. A
  right in it would outlive its removal by up to that lifetime.
- A verifier that read the network's rules from a certificate would
  authorize on rules written for reachability, and a change made for
  the network would change what a caller may do, unseen.

### A reference to an addon's role is its grant

- A service's or job's reference to an addon's role (`{addon: db, role:
  expense_svc}`, ADR 0018) grants that role to the referencing object's
  ID. An addon whose type can map a certificate to a user (PostgreSQL's
  `cert` method on the subject's workload `OU`, Valkey's
  `tls-auth-clients-user URI` from 9.1, NATS's `verify_and_map` on the
  URI) lets the workload in as the role, by the map that `ferund` writes
  from the spec;
  a removed reference leaves the map with the deployment.
- The same reference opens the addon's port to the object (ADR 0034).
  The network decides whether the pod reaches the addon, the map which
  role it gets there; each holds without the other.
- An addon never admits on the chain alone: every workload of every
  tenant has a certificate under the same root.
- The password stays beside the certificate until the object's clients
  present the ID. Which types and in which order, and how the addon's
  own server key stays out of its pod, is the addons' to settle (open
  questions).

### A client presents the ID in one of two ways

- **FeID** (Go, Rust): the library dials the peer from the application's
  own socket, verifies it by its DNS name under the platform's root
  (ADR 0014), and has its CertificateVerify signed by `feos-authn`.
- **An outbound TLS binding**, for a client without FeID (`psql`,
  `libpq`, Python, Node): it names an upstream, a host and a port, and
  its CA, or the platform's bundle. The guest listens on a loopback port
  of the pod, dials the upstream from the pod's own address, runs TLS 1.3
  with the pod's certificate, its CertificateVerify signed by
  `feos-authn`, verifies the upstream by its name, and pipes both
  directions. The client speaks plaintext on loopback, which never leaves
  the pod. The connection leaves from the pod's address, so the network's
  rules and the records see the pod (ADR 0014 refused proxies that dial
  from the node's).
- A reference to an addon's role that asks for the binding gets its
  `url`, `host` and `port` on the binding's loopback port and no
  `password`; the guest dials the addon by its name in the environment
  and verifies it by the addon's ID, under the platform's root alone, as
  FeID pins a server to its SPIFFE ID. The addon's certificate needs no
  address, which changes when its pod moves (ADR 0010). How the spec
  spells the binding follows
  ADR 0028's `binding` object.
- This amends ADR 0014: the TLS end of an outbound binding moves from
  `feos-authn` to the guest, and the host sees none of its plaintext.
  Kubernetes, SSH and HTTP bindings (ADR 0028) stay where they are.
- **The dividing line is the secret.** A binding that adds a credential
  the pod must never hold runs on the host, in `feos-authn`, which
  therefore holds the plaintext of those connections. A binding that
  adds only the identity runs in the guest: the key is not in the pod
  either way, only signatures cross the vsock, and the plaintext stays
  where it is produced. One rule, not one per type.
- The guest speaks no upstream's protocol: the connection is TLS from
  its first byte, and the guest sends the upstream's ALPN. PostgreSQL
  from 17 takes that with ALPN `postgresql`, Valkey takes TLS directly,
  NATS with `handshake_first`. An upstream that negotiates TLS inside
  its protocol, as PostgreSQL before 17 did with `SSLRequest`, is not
  reached through a binding (open question).
- **The guest is FeID's first client.** Its bindings are the Rust
  library: the client of `feid/1`, the `SigningKey` that asks
  `feos-authn`, the certificate fetched per handshake. An application in
  Rust uses that crate as it is; the Go library is its port. Publishing
  the libraries for applications is a step after the guest, not a
  mechanism of its own.

### Parked: a server inside a cluster that reads its callers' IDs

- FeID's server side requires a client certificate under the platform's
  root and hands the server the peer's parsed ID. It applies no scope of
  its own: inside a cluster the network has decided who reaches the
  port.
- The first version of this ADR gave FeID's servers a default scope of
  their own tenant and environment, and servers without FeID an inbound
  binding that the guest terminates and hands on, with the peer's ID in
  a PROXY protocol v2 TLV (`identity: proxy`) or a `FeID-Peer` header on
  every HTTP/1.1 request (`identity: header`). Both wait for a call
  whose address names no workload, or for a server that asks for its
  callers' names in band; the ADR that brings either decides them.

### The network's credential is a certificate of its own

- Should the network key traffic per pair of workloads (a later ADR), its
  credential is a second certificate of the pod: the same ID, a key of
  its own that the node's key manager holds, and an extended key usage
  of its own.
- An application never gets it, and FeID never signs with it: one key,
  one purpose (NIST SP 800-57). The issuer issues both in the round trip
  of ADR 0014, while the microVM boots.
- It is one PKI: one root, one issuer, one trust bundle, one trust
  domain, and the same ID in both certificates, by which the audit joins
  what the network and the applications report. The network's traffic
  keys are not the certificate's key in any case: a handshake derives
  them, and the certificate authenticates the handshake alone.

### One record per connection names both ends

- The control plane names both ends of every flow it receives (ADR 0013)
  by their IDs, from the addresses it gave out: an IPv6 address names
  one pod for good, so the name holds for old records too. A flow
  between two workloads without FeID still has both IDs.
- ADR 0014's join of signatures and handshake reports to flows gains the
  same IDs. A connection that carried an identity shows it, one that did
  not shows the addresses' IDs, and the two never disagree without an
  anomaly.
- These records are what a graph of an environment's workloads is drawn
  from, together with the applications' spans (ADR 0026): edges from
  flows for every protocol, rates and errors from spans where an
  application sends them.

## Alternatives considered

- **An ID per pod**, as the issuer writes today. An addon's map would
  change with every replica and every roll.
- **The workload in the CN**, as a readable name (`api`) or as a UUID
  derived from the ID: a readable name is ambiguous across environments
  and tenants, so a verifier that checks only the CN admits every
  workload of that name; a UUID is unique but tells no replica from
  another, and every log line names a group instead of a microVM.
- **Rights in the certificate**, roles or the network's rules as claims
  beside the ID. A right would outlive its removal by the certificate's
  lifetime, and verifiers would authorize on rules written for the
  network.
- **Every call inside a cluster checked by certificate**, as a mesh's
  mutual TLS at every hop: the address names the workload already, and
  the network decides on it (ADR 0034). A second check of the same fact
  costs a signature per connection and every application a change, and
  the two answers could disagree. Parked, not refused, for calls whose
  address names no workload.
- **An issuer per tenant**, and a tenant's pods trusting only their
  tenant's: a verifier that checks only the chain would refuse other
  tenants, too. The control plane holds one issuer on every instance
  (ADR 0015, ADR 0024); an issuer per tenant puts a key per tenant on
  every instance and a bundle per tenant on every pod. The addons' maps
  never admit on the chain, and other tenants cannot reach a pod past
  the network. Kept as an open question for verifiers of a tenant's own
  that check only the chain.
- **Authorization in the platform** by method and path, as a mesh's
  proxy does: it needs a proxy in the data path that reads HTTP, on
  every connection. FeOS adds none.
- **Outbound TLS bindings kept in `feos-authn`**, as ADR 0014 has them:
  the host holds the plaintext of a connection to which it adds nothing
  but a signature it would make for the guest anyway.
- **An identity proxy at the node's address**, which trusts a pod by its
  TAP and connects on its behalf: ADR 0014 refused proxies that dial
  from the node's address, since the pod's rules would not apply and
  its records would not show the connection; and the plaintext would
  pass through the host.
- **A proxy in the application's container**, as a hyperscaler's
  database proxy runs beside the application: the image would have to
  carry it, and it would have to bootstrap its own identity. The guest
  is already in the pod and already has the identity.
- **A binding that speaks the upstream's protocol** to start TLS inside
  it: a parser per type in the guest, for upstreams that take TLS from
  the first byte anyway.
- **One certificate for the network and the application**: one key for
  two purposes.
- **The identity in the packet** (ADR 0012, ADR 0034): unchanged.

## Consequences

- The issuer writes the workload's ID for a pod of an object, and the
  environment into a sandbox's and a tenant's job's; the subject gets
  the pod in its CN and the workload's set ID in an `OU`. A running pod's
  certificate gets the new ID at its next renewal. Nothing authorizes on
  pod IDs today, so nothing moves with them.
- FeID's libraries hand a server the peer's parsed ID and apply no scope
  of their own, in Go and Rust.
- `feos-guest` gains outbound TLS bindings: a TLS 1.3 client whose
  signatures come from `feos-authn` over feid/1. They run in a process
  of their own, as libcontainer does (ADR 0007), so that a fault in it
  never takes PID 1 down. It reads the plaintext of the pod's clients
  and the TLS of upstreams, so it is jailed as `feos-authn` is (ADR
  0014), once its file is read and its listeners bound: a user of its
  own, no capabilities, a root that holds the share directory alone,
  `no_new_privs` and `feos-authn`'s seccomp allowlist with Unix sockets
  for FeID's. A fault in it gets what a container of the pod has, not the
  guest; it serves nothing outside its jail. `feos-authn` keeps its
  signer, its Kubernetes, SSH and HTTP bindings, and loses its TLS
  bindings.
- The order of the work follows from the guest being FeID's first
  client: the signer side of feid/1 in `feos-authn`, the Rust crate, the
  guest's bindings on it, the addons' maps (PostgreSQL first) and the
  references' bindings, the Go library as the crate's port. Each step is
  tested against the one before.
- The control plane names flows by IDs, which a graph of an
  environment's workloads in the UI reads.
- FeCode's agent is told to reach an addon through its reference's
  binding, without a password, once the addon's type maps certificates.
- ADR 0014's limits stay: Ed25519 only, so a peer that cannot verify it
  cannot use the ID, and a pod that has been taken over can have
  signatures made for new connections while it runs. Narrow roles in
  the addon, short lifetimes, rate limits and the audit are the
  boundary.

## Open questions

- **An addon's own server key.** PostgreSQL terminates TLS itself and
  reads its key from a file. ADR 0014's OpenSSL provider, which would
  delegate the signature to `feos-authn`, would keep it out of the
  addon's pod and give `libpq` clients the ID as well.
- **Session resumption** in FeID's clients and the guest's bindings. A
  resumed handshake is neither signed nor audited (ADR 0014); turning
  it off costs a signature and a record per connection, which pools
  keep small. rustls resumes by default, and the Rust crate does today.
- **Issuers per tenant**, for a tenant's own verifiers that check only
  the chain.
- **Users' tokens** and how a workload's call carries the user it acts
  for.
- **Upstreams that start TLS inside their protocol** (PostgreSQL before
  17): whether a binding learns the one message, or they are left to
  FeID and the OpenSSL provider.
