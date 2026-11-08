# ADR 0014: A workload's identity and credentials stay on the host

**Status:** Accepted · **Date:** 2026-10-15

Every pod runs with the identity of its workload: an X.509 certificate
with one SPIFFE ID, whose private key is made and held by `feos-authn`,
a jailed process of its own on the host, one per running pod, which
signs only TLS 1.3 CertificateVerify inputs over one vsock port and
records every signature before it gives it. A service's, a job's or an
addon's pods share their object's ID through every replica and every
roll, a sandbox and a tenant's job are named by their IDs, and the
subject names the pod in its CN and the workload's set in an OU, nothing
readable. The certificate says who, never what: within a cluster the
address is the workload (ADR 0012), and the certificate serves an
addon's credentials without a password in the pod (ADR 0038) and, one
day, calls whose address names no workload. An application presents the
ID with FeID, a library on its own sockets, or, without FeID, through an
outbound TLS binding that the guest terminates; a binding that adds a
secret the pod must never hold runs on the host instead (ADR 0028, ADR
0053). Secrets reach the host process sealed to it, and the CA that
issues all of this is the platform's one (ADR 0024).

## Context

- Code in a pod needs access: a service talks to its database or to
  another service, an agent deploys to a cluster or pushes over SSH. A
  pod runs untrusted code (ADR 0001), and a hijacked agent sends out
  whatever it finds; a credential inside a pod stays valid from
  anywhere, for as long as it lives. So the material stays on the host,
  and the pod gets the use of it, never the bytes. The host knows which
  pod is asking from the socket a connection arrives on, never from what
  the pod sends.
- A server that authenticates by certificate (PostgreSQL, Valkey, NATS)
  can see the workload itself, with no password and no proxy in the data
  path, if the application's TLS stack delegates the one private-key
  operation of a handshake, the CertificateVerify signature, to a signer
  outside the pod. Go's `crypto.Signer` and rustls's `SigningKey` allow
  that; OpenSSL, Python and Node do not.
- Within a cluster the address is the workload: a pod sends from its own
  address only (ADR 0010), the traffic between nodes is WireGuard's (ADR
  0010), and `feosd` admits by address what the deployment's references
  declare (ADR 0012). Where the address stops naming the workload, a
  call across clusters or from outside the platform, only a certificate
  can; no such call exists yet.
- An ID per pod would defeat every map from an ID to a role: two
  replicas of one service would have two IDs, and every roll new ones.
- Every form of credential needs bindings per pod as sets, a jail, audit
  records, and a way for secrets to reach the node that is neither the
  bus nor `feosd`'s memory.

## Decision

### What a pod gets

- **An identity**: a certificate with its workload's SPIFFE ID, whose
  private key stays on the host. Every pod has one.
- **FeID**: libraries with which an application uses that identity for
  mutual TLS end to end, from its own sockets.
- **Outbound TLS bindings**, for applications without FeID: a loopback
  port in the pod whose connections the guest carries to a named
  upstream in TLS 1.3 with the pod's identity.
- **Bindings that add a secret**: HTTP bindings (ADR 0028), Kubernetes
  and SSH bindings (ADR 0053). The pod sees a plain API or an agent,
  and the host adds the credential.

### feos-authn: one process per running pod

- `feos-authn` is a program of its own (ADR 0003), shipped with
  `feosd`. `feosd` starts one per pod with the microVM, on create,
  resume and restore, and stops it when the run ends. A suspended pod
  has no process, and no key or secret of it is on any node.
- The process holds one pod's material and nothing else. There is no
  lookup from a connection to a tenant, and a bug in a parser costs one
  pod's credentials, not the node's.
- It survives a restart of `feosd` with the microVM: `feosd` binds a
  control socket in the pod's directory and hands it to the process; a
  `feosd` that starts again dials it and adopts the process by its pid
  and start time, as it adopts Firecracker. The control protocol is
  versioned, so that an upgraded `feosd` adopts the processes the
  version before it started.
- At every start the process makes the pod's key pair in its own
  memory. Its signing request goes to the control plane, the
  registration authority, which chooses the ID and issues a short-lived
  certificate; `feosd` chooses no identity and the application never
  names its own. The private key never exists outside the process that
  signs with it: not on a disk, not in `feosd`, not in the control
  plane.
- The certificate is renewed at three quarters of its lifetime with the
  same key. An issuer that cannot be reached is retried; until the
  certificate expires, the old one serves. After that the signer
  refuses, loudly.

### One vsock port, the socket names the pod

- The guest pipes every connection for the host process to one vsock
  port (ADR 0007). A one-line preamble selects what the stream is for:
  FeID, or a binding by its kind and port. The preamble carries no
  authority: identity comes from the host-side socket the stream
  arrives on, which belongs to the pod's microVM. A process in the pod
  that dials the vsock port itself gains nothing the guest's sockets do
  not already give it.
- In the guest, FeID is a socket at a fixed path in every container,
  present while the pod has an identity; bindings are loopback ports or
  an agent socket, each present only while the pod has what it serves.
  `feosd` pushes them with the pod's configuration.

### The signer

- The pod's TLS stack fetches its certificate chain and the trust bundle
  from the signer and asks for signatures. The signer signs only input
  of the exact shape of a TLS 1.3 CertificateVerify, client or server;
  keys are Ed25519, so the input arrives unhashed and can be checked. It
  is not a signing oracle. A request without a valid certificate waits a
  few seconds, then is refused retriably: a pod just resumed, an issuer
  late.

### One ID per workload

- A pod's ID names the workload the pod is part of, never the pod:

  ```text
  spiffe://<td>/tenant/<tenant>/env/<environment ID>/service/<name>
  spiffe://<td>/tenant/<tenant>/env/<environment ID>/job/<name>
  spiffe://<td>/tenant/<tenant>/env/<environment ID>/addon/<name>
  spiffe://<td>/tenant/<tenant>/env/<environment ID>/sandbox/<id>
  spiffe://<td>/tenant/<tenant>/env/<environment ID>/tenant-job/<id>
  ```

- A service's, a job's and an addon's pods share their object's ID:
  every replica, every run, every pod after a roll. A deployment that
  removes an object and adds one of the same name gives the new one the
  old ID, as it gives it the old name.
- A sandbox and a tenant's job, made by a call and not by the spec (ADR
  0035), are one pod each and are named by their IDs (ADR 0019); their
  segments differ from the objects', so a pod's ID never reads as a
  job's name. An attached service runs in its sandbox's pod and has the
  sandbox's ID: nothing in the pod separates its processes from the
  sandbox's other code, so no ID could.
- The environment is in the ID by its ID, which is its identity (ADR
  0018): `api` of one environment is never `api` of another, and a
  rename changes no certificate.
- The trust domain is the platform's one (ADR 0024). The platform's own
  IDs, `node`, `ingress`, `sink` and `ferun`, are namespaces a pod never
  gets. A certificate has exactly one URI name, as an X.509-SVID must.

### The subject names the pod and its workload

- The subject's CN is the trust domain and the pod's ID. A pod ID is
  never given out twice, so every connection an addon logs names one
  microVM, and a verifier that reads only the CN, as PostgreSQL does by
  default, matches one pod and fails closed instead of reaching another
  workload.
- One `OU` names the workload by the ID its sets on the nodes carry (ADR
  0012): a hash over the environment's ID, the object's kind and its
  name for a service, a job or an addon; the sandbox's or the job's ID
  for the others. An addon that maps a subject maps this attribute (ADR
  0038).
- The subject carries no readable name: the tenant's, the environment's
  and the object's are in the URI. A readable attribute would be the
  next one a verifier maps alone. The issuer refuses a subject with
  anything but letters, digits, `.`, `/` and `-`, so that a map anchored
  on the printed subject holds.

### A certificate says who, never what

- The certificate carries the ID and the pod. It carries no role and no
  right, and none of what the network decides on: no reference, no
  `reaches`. Rights change with a deployment, a certificate lasts its
  lifetime: a right in it would outlive its removal by up to that
  lifetime, and a verifier that read the network's rules from it would
  authorize on rules written for reachability.
- **May A reach B's port?** The network answers, by address, from the
  deployment's references (ADR 0012). Inside a cluster no application
  checks its callers' certificates for it, and FeOS asks none to.
- **What a caller may do** is the application's or the addon's: methods,
  rows, roles. The platform has no policy language for it and reads no
  request.
- **A reference to an addon's role is its grant**: it grants the role to
  the referencing object's ID, and the addon maps the certificate to the
  role, never admitting on the chain alone, since every workload of
  every tenant is under one root (ADR 0038). The same reference opens
  the addon's port (ADR 0012); the network decides whether the pod
  reaches the addon, the map which role it gets there, and each holds
  without the other.

### Two ways to present the ID; the dividing line is the secret

- **FeID**: the library dials the peer from the application's own socket
  and has its CertificateVerify signed by `feos-authn`.
- **An outbound TLS binding**, for a client without FeID: it names an
  upstream, a host and a port, the ALPN protocol the upstream speaks or
  none, and the SPIFFE ID the upstream must carry. The guest listens on
  a loopback address of the pod, one per upstream, dials the upstream by
  its name from the pod's own address, runs TLS 1.3 from the first byte
  with the pod's certificate, signed by `feos-authn` over FeID's socket,
  verifies the upstream by its ID under the platform's root alone, and
  pipes both directions. The client's plaintext never leaves the pod,
  and the connection leaves from the pod's address, so the network's
  rules and the records see the pod. The guest speaks no upstream's
  protocol: an upstream that negotiates TLS inside its protocol is not
  reached through a binding.
- The bindings run in a process of their own in the guest, so that a
  fault in TLS never takes PID 1 down (ADR 0007). PID 1 binds the
  listeners before it answers the host's set and the process inherits
  them, so a container never finds a binding's port closed: a
  connection waits in the backlog while the process starts, restarts or
  waits for the pod's signer. The process is jailed as `feos-authn` is
  once it holds its listeners: a user of its own, no capabilities, a
  root that holds the share directory alone, no new privileges, the
  same seccomp allowlist. A fault in it gets what a container of the
  pod has.
- **The dividing line is the secret.** A binding that adds a credential
  the pod must never hold runs on the host, in `feos-authn`, which
  therefore holds the plaintext of those connections (ADR 0028, ADR
  0053). A binding that adds only the identity runs in the guest: the
  key is not in the pod either way, only signatures cross the vsock, and
  the plaintext stays where it is produced. One rule, not one per type.
- The guest is FeID's first client: its bindings are the Rust library.

### Secrets reach the process sealed to it

- Bindings are sets: the spec names all of a pod's bindings, and a
  change states the whole set again. What it does not name is gone.
- At start, `feos-authn` makes a second key pair, for HPKE (RFC 9180),
  and its public half goes out with the signing request. The control
  plane seals the bindings to it; `feosd` relays ciphertext. Only the
  pod's own process can open them, and it keeps them in memory only. A
  new run has a new key, so the control plane seals again after every
  start, resume and restore.
- The sealing keeps the bindings off the bus and out of `feosd`'s logs
  and dumps. It does not protect against a `feosd` that has been taken
  over: that owns the microVM's memory anyway.

### What a run waits for

- The certificate and the sealed bindings come in one round trip, and
  the microVM boots meanwhile. On a start, the containers run only when
  both have arrived and `feosd` has pushed the configuration: nothing in
  the pod runs before the pod has what it was promised. A pod whose
  credentials do not come in time fails with why.
- After a resume or restore, the containers are already running. Until
  the new certificate and bindings arrive, a request waits a few
  seconds, then gets a refusal that FeID's libraries retry.

### Audit

- Every signature and every request is recorded before it is answered,
  refusals with their reason, and FeID's libraries report every
  handshake they complete.
- The record goes to an append-only file in the pod's directory, which
  `feosd` opens and the process inherits. `feosd` reads the file and
  publishes the records as the pod's events; a `feosd` that restarts
  stalls no handshake. A signature without a handshake report is an
  anomaly: a library bypassed, or a report lost.

### The jail

- Each process runs as a user ID of its own, one per running pod, so
  that two pods' processes cannot signal or trace each other.
- It runs in a mount namespace with an empty root, without capabilities,
  with `no_new_privs`, a seccomp allowlist and rlimits. Its descriptors
  are its listeners, its control socket, its audit file and stderr.
- It checks every address it dials against the node's denied prefixes
  and the node's own networks itself: a proxy that a parser bug has
  taken over has no way into the node network or the platform's own
  cluster.

### FeID, the application side

- FeID is the SDK with which an application on FeOS uses its pod's
  identity, for languages whose TLS stacks let a library sign (rustls's
  `SigningKey`, Go's `crypto.Signer`). The Rust library is the
  reference; another language's is its port. The libraries are released
  from this repository (ADR 0002) and versioned on their own, not with
  FeOS.
- **Nothing to configure.** A library finds the signer at its fixed
  path, or where an environment variable points, for development.
  Creating a client without configuration works in every pod that has
  an identity; a pod without one has no socket, and the library fails at
  start and says why.
- **Peers by name, or pinned.** A client dials a service by its name and
  checks the server by that name, as any TLS client does, against the
  platform CA and the WebPKI together, or pins the server to its SPIFFE
  ID. A server requires a client certificate under the platform's root
  and hands the application the peer's parsed ID; it applies no scope of
  its own, since inside a cluster the network has decided who reaches
  the port.
- **The protocol**, feid/1, is versioned NDJSON over the socket: a hello
  negotiates the version, and within a version everything is additive,
  so a library in an image built months before a node's upgrade keeps
  working, and a new library with an old node. TLS 1.3 only, Ed25519
  only. Both sides, the CertificateVerify check, the error codes and the
  golden frames are one crate, `feid-proto`, which `feos-authn` and the
  library use: the check exists once. The chain is fetched in every
  handshake, since a resumed pod has a new key.

## Alternatives considered

- **One process per kind and pod**, as the sandbox system ran them, the
  SSH agent in an empty network namespace: it halves the isolation for
  little, since a process that has been taken over can always write
  back to the guest over its vsock stream. The tenant boundary, one pod
  per process, stays.
- **One daemon per node**, under the rule "no sidecars": every tenant's
  keys in one address space and a lookup on every connection. A per-pod
  process on the host is no sidecar: nothing runs in the pod.
- **`feosd` running itself again**, as for the task service: the process
  would carry all of `feosd`'s code, and its seccomp allowlist would be
  larger.
- **Inside `feosd`**: hostile HTTP and SSH parsed in the privileged
  process that owns every microVM on the node.
- **The control plane makes the keys and sends them with the pod**: one
  message fewer at start, but the key would exist outside the process
  that uses it.
- **Bindings over Valkey in plaintext, or pushed from the control plane
  to the node**: the first shows them to everything that reads the bus,
  the second needs a listener on the node (ADR 0015 has none), and the
  plaintext would still pass through `feosd`.
- **Audit records to `feosd` only, without a file**: every restart of
  `feosd` would stall every handshake on the node.
- **Host-side proxies dialing from the node's address**: the pod's
  security groups would not apply to them, and the pod's records would
  not show what it reached. Refused as the design; the process checks
  every address it dials itself, whichever address it dials from.
- **A TLS-intercepting proxy with a CA per pod**, for any destination
  the pod chooses: no library and no binding, but the platform in the
  plaintext of every connection and a CA baked into the pod. A binding
  sits in the plaintext only of the upstreams it names.
- **Outbound TLS bindings terminated in `feos-authn`**, the first
  version of this decision: the host held the plaintext of a connection
  to which it adds nothing but a signature it would make for the guest
  anyway. Reversed; the guest terminates them.
- **An identity proxy at the node's address**, trusting a pod by its
  TAP: refused for the reasons of the two above.
- **A proxy in the application's container**, as a hyperscaler's
  database proxy: the image would have to carry it and bootstrap its
  identity. The guest is in the pod already and has the identity.
- **A binding that speaks the upstream's protocol** to start TLS inside
  it: a parser per type in the guest, for upstreams that take TLS from
  the first byte anyway.
- **Peers only by SPIFFE ID**, as the prototype had it: every client
  would have to know its peers' IDs, clients without FeID could not
  check a server, and the libraries had to switch hostname verification
  off and verify by hand, the one place where a copied configuration
  loses all verification.
- **protobuf or gRPC for feid/1**: a protobuf or HTTP/2 runtime in every
  application for a handful of messages.
- **An ID per pod**, as the issuer wrote at first: an addon's map would
  change with every replica and every roll. Reversed.
- **The workload in the CN**, as a readable name or a UUID derived from
  the ID: a name is ambiguous across environments and tenants, so a
  verifier that checks only the CN admits every workload of that name;
  a UUID tells no replica from another, and every log line names a
  group instead of a microVM. An earlier CN layout, the ID without
  its scheme cut with a hash, was longer than a CN may be for many
  workloads. Reversed.
- **Rights in the certificate**, roles or the network's rules as claims
  beside the ID: a right would outlive its removal by the certificate's
  lifetime, and verifiers would authorize on rules written for the
  network.
- **Every call inside a cluster checked by certificate**, as a mesh's
  mutual TLS at every hop: the address names the workload already, and
  the network decides on it. A second check of the same fact costs a
  signature per connection and every application a change, and the two
  answers could disagree. Parked, not refused, for calls whose address
  names no workload; so are a default scope in FeID's servers and
  inbound bindings that hand a server its caller's ID, which the first
  version of the identity decision had.
- **An issuer per tenant**, and a tenant's pods trusting only their
  tenant's: a verifier that checks only the chain would refuse other
  tenants too, but it puts a key per tenant on every instance and a
  bundle per tenant on every pod. The addons' maps never admit on the
  chain, and other tenants cannot reach a pod past the network.
- **Authorization in the platform** by method and path, as a mesh's
  proxy does: a proxy in the data path that reads HTTP, on every
  connection. FeOS adds none.
- **One certificate for the network and the application**, should the
  network ever key traffic per pair of workloads: one key for two
  purposes. The network's would be a second certificate of the pod with
  the same ID and a key of its own.
- **ECDSA P-256 for peers without Ed25519**: Go's TLS hands an ECDSA
  signer a digest, not the message, so the signer could not check its
  shape and would be an oracle for that key. If P-256 comes, it is a
  key of its own, for TLS only.

## Consequences

- An application in Rust adds FeID and creates a client. It configures
  nothing, holds no key, and its database sees its workload's ID.
  Applications in Python, Node and everything built on OpenSSL get the
  same identity through a TLS binding, with no change but the address
  they connect to.
- Starting a pod costs a round trip to the control plane, during which
  the microVM boots. A resumed pod serves its old connections and signs
  new ones once its certificate arrives.
- The control plane is an issuer and the registration authority: it
  knows every pod's workload, and a pod starts only while it answers.
- Offloading protects the bytes, not the authority. A pod that has been
  taken over can have signatures made for new connections while it
  runs. Narrow roles in the addon, short lifetimes and the audit are the
  boundary; nothing revokes a certificate before its end.
- A peer that cannot do Ed25519 cannot check a FeID server: old Java,
  browsers. Browsers reach services through an ingress with public
  certificates (ADR 0021, ADR 0045).
- The addons' maps and the network name a workload by the same string,
  the set's ID, so a grant and a reachability rule cannot name
  different things.

## History

Decided 2026-10-15 as two ADRs, `feos-authn` on the host and FeID in the
application (commits 4c7f609, 7e35131), merged into this one the same
day (33e5905); the design came from FeTLS, an unpublished prototype of
the delegated signer, and from the sandbox system's proxies. The signer,
feid/1 and the Rust library were built 2026-11-08 (649707e, a707f07).
ADR 0036 of 2026-11-07, revised and committed 2026-11-08 (e939702),
decided the layout of the IDs and the subject, that a certificate says
who and never what, that within a cluster the address is the workload,
and that outbound TLS bindings move from `feos-authn` to the guest; it
was merged here on 2026-11-08, the day the guest's bindings were built
(2e1bfce, 7c7b84c, a0a31b6). The Kubernetes and SSH bindings this ADR
first held are ADR 0053 since the same day.
