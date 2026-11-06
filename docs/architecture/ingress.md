The Ingress
===========

How public environments' HTTP traffic reaches their pods:
`ferun-ingress`, FeRun's second program, in `go/`, and the parts of
`ferund` that serve it. The decisions behind it:
[ADR 0021](../adr/0021-ingress.md) (the ingress, and what changed while
it was built), [ADR 0017](../adr/0017-pod-mesh.md) (the pods' mesh, which
the ingress joins), [ADR 0018](../adr/0018-environments-and-releases.md)
(environments, routes and the route door). The rest of `ferund` is in
[control-plane.md](control-plane.md); the deployment on Gardener is in
[deploy/gardener/README.md](../../deploy/gardener/README.md#the-ingress).

`ferun-ingress` serves `https://<label>.<zone>/…`, where the zone is the
apps zone or the tenant zone of the tenants people make (ADR 0031), and the tenants'
domains of the environments whose tenant made them public, straight to their image
services' pods over IPv6 on the pods' mesh. It has no REST API, no
database and no tenant's token: it connects out to `ferund` on the
nodes' port, as a node does, and serves from the table `ferund` streams
to it. A rollout or an outage of `ferund` cuts no request.

Not built: `http` traffic records, zones of a tenant's own, certificates over
DNS-01 (a zone's wildcard), L4 routes and h2c upstreams. The domains'
claims, checks and states are `ferund`'s domains core; their
certificates are ACME's ([control-plane.md](control-plane.md#certificates-of-domains)).

## Overview

```
 clients ──IPv4──▶ LoadBalancer ferun-ingress (80→8080, 443→8443)
                     │   NetworkPolicy: 8080/8443 from anywhere; 9090, udp/51820-51835 from the workers
                     │   egress: DNS, ferund's 7443, udp/51820 to the workers, nothing else
   StatefulSet ferun-ingress-0/1 (root with NET_ADMIN, seccomp RuntimeDefault, a volume each)
     ├─ proxy:  TLS (SNI: a domain's certificate from the table, else the tenant zone's wildcard where it covers the name, else the apps zone's) → HTTP/1.1 or HTTP/2
     │          port 80: ACME HTTP-01 answers from the table, else 308 to https
     │          → host → environment → route → service → endpoint
     │          ├─ usable: port admitted ∧ group version ≥ admitted ∧ node's handshake < 180 s
     │          ├─ idle services: a hold before the first request, renewed every 10 s
     │          └─ HTTP/1.1, pooled (idle 4 s), to [<pod address>]:<port>
     ├─ agent:  state directory, join, renewal, Hello, Sync with its cache, Wake, Hold
     │          └─ gRPC + JSON, mTLS, ALPN ferun-ingress/1 ──▶ one --control-plane address at a time
     └─ mesh:   WireGuard ferun-mesh, <its /96>::1/128, route <ULA>/64, the nodes as peers
                  ├─ UDP 51820 (a port after it once repaired) ⇄ the workers' feos-mesh at 51820
                  └─ a node with endpoints and no handshake for 180 s: a new port, its peer set anew
 node:  feos-mesh → forward chain → the pod's group ing-<sandbox id>: tcp <port> from ferun-ingress → pod
 ferund (every instance): the ingress service (Join, RenewCertificate, Hello, Sync, Wake, Hold), the table builder
 ferund (the leader):     mesh-peers (ingress peers too), env-groups (ing-, ferun-ingress, admission), idle pause (fenced)
```

| Package | Binary | What it is |
|---|---|---|
| `internal/ingress/wire` | both | the protocol: messages, the JSON codec, the service descriptor, the client, the table state ([api.md](api.md#the-ingresss-api)) |
| `internal/routing` | both | path matching, shared with the route door |
| `internal/ingress/service` | `ferund` | Join, RenewCertificate, Hello, Sync, Wake, Hold, the connection check, the operator's calls |
| `internal/ingress/table` | `ferund` | the table: built per environment from the database, the builder that keeps it, the diff a stream sends |
| `internal/ingress` | `ferun-ingress` | the configuration, the program that wires agent and proxy, health, readiness, metrics, status, shutdown |
| `internal/ingress/agent` | `ferun-ingress` | identity, state directory, the connection, the table and its cache, the mesh device, Wake and Hold |
| `internal/ingress/proxy` | `ferun-ingress` | TLS, routing, gating, holds, the reverse proxy, cookies, metrics |
| `internal/ingress/wg` | `ferun-ingress` | WireGuard and rtnetlink over stdlib `syscall`, keys |
| `cmd/ferun-ingress` | `ferun-ingress` | `serve`, `prepare`, `status`, `version` |

`ferun-ingress` is static (`CGO_ENABLED=0`), built by `make go-build`
into `target/go/`, and shipped in ferund's image as `/ferun-ingress`.

## The program

    ferun-ingress [serve] [flags]     run the ingress (the default)
    ferun-ingress prepare             enable IPv6, copy the instance's join token (as root)
    ferun-ingress status              print the running instance's /statusz
    ferun-ingress version             print the version

Every flag has an environment variable `FERUN_INGRESS_<NAME>`, the flag's
name in upper snake case (`--control-plane`:
`FERUN_INGRESS_CONTROL_PLANE`); a flag on the command line wins over its
variable, and an empty variable counts as unset. Durations are written in
Go's syntax, byte sizes as integers with an optional `KiB`, `MiB` or
`GiB` suffix. `ferun-ingress help <command>` lists the flags.

### serve's flags

| Flag | Default | What it is |
|---|---|---|
| `--name` | required | the instance's name, a DNS label: `$(POD_NAME)` on Kubernetes; its identity is `spiffe://<trust domain>/ingress/<name>` |
| `--state-dir` | `/var/lib/ferun-ingress` | the identity, the keys and the caches, kept across restarts |
| `--join-token-file` | none | a join token, `feos-ingress-join-<secret>.<pin>`, read only while no valid certificate is kept |
| `--control-plane` | required | the nodes' port of `ferund`, `host:port`, comma-separated, tried in random order |
| `--control-plane-name` | required | the DNS name the control plane's certificates are verified for: `ferun.<domain>` |
| `--trust-domain` | required | the SPIFFE trust domain |
| `--sink-endpoint` | none | the sink's ingest, `host:port`, where the access log goes ([Access log](#access-log)); a name is resolved by the instance, any of its addresses serves; empty: the access log is not kept, its records counted as dropped (logged once at start) |
| `--listen-http`, `--listen-https`, `--health-listen` | required | an IP address and a port each: the pod's IP, never a wildcard, which would bind the mesh device's address too |
| `--tls-cert`, `--tls-key` | required | the apps zone's wildcard certificate and its key, PEM, read again every 30 s |
| `--tenant-zone-tls-cert`, `--tenant-zone-tls-key` | empty | the tenant zone's wildcard certificate and its key (ADR 0031), PEM, read again every 30 s, served for every name it covers; both or neither ([The tenant zone](#the-tenant-zone)) |
| `--mesh-device` | `ferun-mesh` | the WireGuard device |
| `--mesh-port` | `51820` | its UDP port; a repair moves the device to one of the 15 after it ([Lost paths](#lost-paths)) |
| `--mesh-endpoint` | required | where the nodes reach the device: `$(POD_IP):51820` |
| `--mesh-mtu` | `0` | the device's MTU; 0 computes it ([The mesh](#the-mesh)) |
| `--wake-hold` | `2m` | how long a request waits while instances of its service are asleep or waking |
| `--pending-hold` | `10s` | how long a request waits while they are only pending |
| `--max-held` | `1024` | how many requests may wait at once |
| `--max-held-per-tenant` | `256` | how many requests of one tenant may wait at once |
| `--max-held-per-env` | `128` | how many requests of one environment may wait at once |
| `--max-inflight` | `2048` | how many requests may be forwarded at once, waiting for a connection or an answer, upgraded ones included ([Limits](#limits)) |
| `--max-inflight-per-tenant` | `512` | how many requests of one tenant may be forwarded at once |
| `--max-inflight-per-env` | `256` | how many requests of one environment may be forwarded at once |
| `--upstream-dial-timeout` | `3s` | the bound of a dial to an instance |
| `--upstream-idle-timeout` | `4s` | a pooled connection to an instance idle this long is closed |
| `--upstream-wait-timeout` | `10s` | the bound of the wait for a connection to an instance, the dial included; at least `--upstream-dial-timeout` |
| `--max-request-body` | `64MiB` | the largest request body; 0: no limit |
| `--max-conns-per-endpoint` | `256` | the most connections to one instance's port |
| `--memory-limit` | `0` | the container's memory limit; the Go runtime's soft limit is 90% of it unless `GOMEMLIMIT` is set; 0: none |
| `--shutdown-delay` | `10s` | how long a stopping instance serves on with `/readyz` failing |
| `--shutdown-grace` | `40s` | how long it then waits for its requests |
| `--log-level`, `--log-format` | `info`, `json` | `text` is the other format |

`status` takes `--health-listen` (the same variable as serve's, so it
needs no flag inside the pod) and `--timeout` (5 s); `prepare` takes the
log flags, `--join-token-source` and serve's `--join-token-file`
([Kubernetes](#kubernetes)).

### The state directory

The directory is 0700 and its files 0600. Every write goes to a
temporary file that is synced, renamed into place and followed by a sync
of the directory, so that a crash leaves the old content or the new.

| File | Holds |
|---|---|
| `tls.key` | the certificate's key, ECDSA P-256, PKCS #8 PEM |
| `cert.json` | the certificate, its chain and the trust bundle |
| `cert.json.lost-<unix>` | the certificate of an identity declared lost |
| `join-tried` | the SHA-256 (hex) of every join token spent, one per line |
| `removed` | the refusal that said the ingress was removed, until the next join |
| `wg.key` | the WireGuard private key, 32 raw bytes |
| `mesh.json` | the place on the mesh: `{podNetwork, prefix, address}` |
| `table.json` | the table's cache, `{savedAt, state}`, written 1 s after a change and at least once a minute while heartbeats confirm it; it holds the domains' certificates and private keys and the ACME challenges |

The certificate is renewed for the same key; a new key is made only
when the identity is declared lost. A new `wg.key` is made after every
join, before the certificate is kept, so the first Hello after a join
always brings a new WireGuard key, which `ferund` fixes then. A lost
identity always ends in a join, and so in a new `wg.key`.

## Identity and connection

### Join

An instance without a valid certificate joins with a one-time join token
of kind `ingress` (`ferund join-token --kind ingress`, or `POST
/api/v1/admin/join-tokens` with `"kind": "ingress"`),
`feos-ingress-join-<secret>.<pin>`: a node's token begins `feos-join-`,
and a node's Join, also that of a `ferund` that knows no kinds, refuses
an ingress's token by its form, before any lookup. It reads
`--join-token-file`, every 30 s while there is no token it has not tried,
dials a control-plane address with TLS 1.3 and ALPN `ferun-ingress/1`,
checks before it sends the token that the server's chain verifies to the
root the token pins for `--control-plane-name` and carries the control
plane's URI, calls `Join` with its name and a signing request, and takes
the answer only if its trust bundle holds that root. A token is spent
once a Join with it was answered or refused: its hash goes into
`join-tried`, and it is never sent again. A Join that got no answer is
sent again with the same token and key.

`ferund` answers on the ingress join server, which takes connections
without a client certificate and serves `Join` alone. It checks the
token's form (a node's, `feos-join-`, is `INVALID_ARGUMENT` "this join
token is for a node", as a node's Join refuses an ingress's with "this
join token is for an ingress"), the name (a DNS label) and the signing
request, at most 20 joins a second and 4 at once in the database per
instance, then, in one transaction:

- an unknown token is "unknown join token", one of kind `node` "this
  join token is for a node", an expired one "join token expired"
  (`PERMISSION_DENIED`);
- a used token is answered again only when the same key repeats it
  within 10 minutes, the ingress is not removed and the first
  certificate is not revoked; otherwise "join token already used";
- a token pinned to a name creates that ingress or joins it again: every
  earlier certificate is revoked (`rejoined`), a removal is cleared, the
  WireGuard key and endpoint are cleared, the key is kept as the retired
  one (`previous_mesh_public_key`), which the next Hello may not bring
  back, and its /96 is kept;
- an unpinned token creates the proposed name only, and a name that
  exists is `ALREADY_EXISTS` "ingress <name> exists; join it with a token
  pinned to its name".

The certificate is ECDSA P-256 or Ed25519, client auth, CN the name, the
URI `spiffe://<trust domain>/ingress/<name>`, valid `--node-cert-ttl`
(168 h) and renewed at half of it; a pod's SPIFFE ID can never be under
`/ingress`. `ingress_certs` records every certificate issued.

### Certificates and supersession

- **Renewal.** At its renew time the agent calls `RenewCertificate` on
  its connection with a signing request for its key (again every 60 s
  while that fails). `ferund` checks the presenting certificate as the
  connection check does, and under the ingress's row lock also that it
  is the ingress's newest certificate (the current one, or, while none
  is current, the newest one not revoked) and not revoked, not even from
  a time ahead (`PERMISSION_DENIED`); that the request is for its key, as
  a new key comes only with a join (`INVALID_ARGUMENT`); and that its
  renew time less 10 minutes has come on the database's clock
  (`FAILED_PRECONDITION` "… is renewed from <time> on"). It issues one
  for that key and records its predecessor. A copy of a certificate thus
  never renews beside the instance: an older one, or the newest after the
  instance renewed and said Hello, is refused, and the newest one only
  in its renew window, for the instance's own key. The agent keeps it,
  connects with it, and drops the old connection once the new one said
  Hello.
- **Supersession.** `ingresses.current_serial` is the newest certificate
  presented in a Hello. When a newer one is presented, every older
  certificate not revoked is revoked 10 minutes ahead (`superseded`);
  the connection check refuses a superseded certificate once that time
  has passed, and the nodes' port closes a connection that uses one at
  that time.
- **The connection check** (`CheckIngress`) runs at accept and on every
  re-check, in one statement on the database clock, never from a cache:
  an unknown certificate, a removed ingress, a revoked certificate and
  an expired one are refused for good, with the texts below; a database
  error refuses only the new connection.

### The connection

The agent holds one connection to one `ferund` instance at a time. It
tries the `--control-plane` addresses in random order, with backoff from
200 ms to 5 s (±20 %) once every address failed: TLS 1.3 with its
certificate, the trust bundle as roots, the control plane's DNS name and
URI checked, session tickets off, ALPN checked; TCP keepalive 15 s and
`TCP_USER_TIMEOUT` 20 s; gRPC keepalive pings every 15 s (5 s timeout),
also without streams; answers up to 32 MiB (`wire.MaxMessageBytes`),
eight times what a message of the table takes at most
([The stream](#the-stream)). In production the list is
the one ClusterIP of `ferund-nodes`, where kube-proxy picks the instance;
in tests and on bare metal it lists every instance. `ferund` never calls
the ingress, and every instance computes the whole table, so a failover
is a reconnect and a whole snapshot.

On the nodes' port the ALPN `ferun-ingress/1` goes to the ingress's own
gRPC servers, beside the nodes'
([control-plane.md](control-plane.md#the-nodes-port)):
the join server without a client certificate, and with one the ingress
server: 16 streams and 4 MiB messages per connection, keepalive pings
every 30 s, pings allowed every 5 s without streams, at most 4
connections per ingress name. A connection is closed when its
certificate expires or its superseded certificate's revocation time
comes.

On a connection the agent says `Hello` first: its WireGuard public key,
its endpoint and its version. Hello takes the instance's place on the
mesh and answers the cluster's ULA, its /96 and address, the apps zone,
the tenant zone and the answering instance; the agent keeps `mesh.json` and configures
the device ([The mesh](#the-mesh)). `Sync` then streams the table. A
message out of sequence, or a stream that ends or stays silent for 45 s,
makes the agent connect again.

### Refusals

A refused connection gets `PERMISSION_DENIED` with one text on every
call:

- "ingress <n> was removed; join it again";
- "certificate <s> of ingress <n> was revoked (<reason>); join it again";
- "certificate <s> of ingress <n> was superseded; reconnect with the
  newest certificate";
- "certificate <s> of ingress <n> expired; join it again";
- "certificate <s> is not ingress <n>'s".

A refusal never throws the identity away by itself. The agent keeps
`cert.json` and keeps connecting with backoff; it declares the identity
lost when the text says the ingress was removed, or when refusals persist
for 5 minutes over at least 3 attempts without a successful Hello in
between, as a database restored from before its join would cause. Then
`cert.json` becomes `cert.json.lost-<unix>`, a new key is made, and the
agent joins again with a token it has not tried. A removal fails
readiness at once and is reported until the next join, also after a
restart; other losses leave readiness to the data path.

A Hello refused with `FAILED_PRECONDITION` "the mesh key of ingress <n>
is fixed; join it again to change it" means that `wg.key` changed
without a join: the agent logs it, serves on from what it has, and tries
again every 60 s. One refused with "the mesh key of ingress <n> was
retired by its join again; make a new one" (a crash between the join and
the new `wg.key`) or "… is another peer's; make a new one" makes a new
`wg.key` and connects again after its backoff. A key of small order is
`INVALID_ARGUMENT`. A cluster that is not meshed answers every Hello
`FAILED_PRECONDITION` "the cluster is not meshed; the ingress needs the
pods' mesh".

## The table

### What it holds

The table is what the ingress routes, one entry per public environment
(`wire.Env`, [api.md](api.md#the-ingresss-api)):

- its ID, tenant, name and hosts: its served domains, else
  `<label>.<zone>` in its tenant's zone while the platform serves that
  zone (the apps zone, or the tenant zone, ADR 0031): an
  environment on a domain of its own is not served at its default host
  (ADR 0021);
- its **domains** (`domains`): each served domain's name, its
  certificate's chain (DER, leaf first) and private key (PKCS #8 DER),
  opened by the builder from `domain_certs`. A domain is served when its
  state is `served` or `dns-moved`, it is verified and bound to the
  environment, which is its tenant's, and its certificate's `not_after`
  is ahead; ordered by when they were bound. A domain takes at most
  64 KiB (`table.MaxDomainBytes`, a chain takes a few), and the domains
  that would take the entry past 2 MiB are left out, never the
  environment;
- the routes of its routing deployment, the deployed one, else the latest
  while none is deployed: name, path prefix, service, port name,
  `strip`, and `door` for a route to an attached service, which the
  route door alone serves;
- per image service that a route sends to, its idle policy, its
  **endpoints**, the instances that are ready, run without an op and have
  an address and a node, with the ports their node admits under the
  routes' port names, numbered by each instance's own deployment (else the
  routing deployment's), the node and the node's version of
  `ferun-ingress`; and the counts of the other instances:
  - `asleep`: paused, hibernated, pausing or hibernating, or ready and
    running with an op (a pause on its way): a Wake helps;
  - `waking`: resuming, or with an op `resume` or `restore`;
  - `pending`: pending or creating, running but not ready, or ready
    without an admitted port;
  - stopped, failed, unknown and lost instances are not counted; the
    controller heals them;
- a hash of the rest, by which a stream tells a change.

Beside the environments the table holds the mesh, the nodes with their
keys, endpoints and /96s at the peers' version, `Self`: the
instance's name, mesh address, the apps zone, the tenant zone and its admitted version
([Admission](#admission)), and the **challenges**: every pending ACME
HTTP-01 answer (`acme_challenges` whose `expires_at` is ahead, at most
1024), each its token, host and key authorization, for every instance. An environment that is not public, is being
deleted, has no label or no deployment, or whose tenant is being deleted
has no entry; nor has one whose routing deployment's spec does not parse,
or whose entry would take more than 2 MiB coded (`table.MaxEnvBytes`;
logged at most once a minute per environment): every instance holds
every entry, so one tenant's environment must not grow the messages of
all. A valid spec stays far below that size, since a route's path has at
most 1 KiB ([control-plane.md](control-plane.md#the-deployment-spec)); a
deployment stored before that limit can exceed it.

### The builder

Every `ferund` instance that has an ingress stream builds the whole table
from the database (`store.IngressRouting`, one query per environment or
for all), while a stream uses it, and keeps it as an immutable snapshot:

- `ferun_ingress` `env:<uuid>` rebuilds that environment (ACME's write of
  a domain's certificate sends it, as the domains core does for a change
  of a domain's state or binding), `mesh` reads the mesh's peers again,
  `challenges` the ACME challenges, `*` and the listener's reconnect
  rebuild everything; `ingress:<name>` is the service's, not the
  builder's;
- the domains' keys are opened with `--secret-key-file`'s key; without
  it no domain is served, and a key that does not open leaves its
  domain out, logged at most once a minute;
- `ferun_sandbox` with the ID of an instance's sandbox rebuilds its
  environment;
- an environment whose rebuild finds no rows leaves the snapshot;
- rebuilds are debounced 50 ms, and everything is read again every 5 s
  as a backstop: writers of an older `ferund` send no `ferun_ingress`;
- a failed read keeps the snapshot and is tried again with backoff (100
  ms to 5 s); the builder is healthy while its last good read is younger
  than 30 s.

Every writer of what the table shows notifies in its transaction:
publishing, an environment's deployment, deployed deployment or deletion, a
tenant's deletion, an instance's insert, delete, phase or deployment, the
admission records, and the ingress and mesh rows
([control-plane.md](control-plane.md#the-database)). A replica that
becomes ready, pauses or stops reaches the ingress about 100 ms after
its write commits, at most 5 s when an older `ferund` wrote it.

### The stream

`Sync`, after a Hello on its connection (else `FAILED_PRECONDITION` "call
Hello first"):

1. waits up to 10 s for the instance's snapshot, else `UNAVAILABLE` "the
   table is not ready on this instance";
2. reads the ingress's row: a removed ingress is refused, one without a
   mesh address is told to say Hello first;
3. sends the whole table (`seq` 1, `whole`), the challenges with it;
4. then sends deltas as the snapshot changes: the environments put and
   removed since what it sent, the mesh when its version changed, `Self`
   when the row changed, the challenges, all of them, when they changed
   (absent: unchanged); a send that blocks lets changes coalesce. A
   renewed certificate changes its environment's hash, so the
   environment is sent again;
5. records the tokens of the challenges it sent as the ingress's
   (`ingresses.challenge_tokens`) when they changed, and at a heartbeat
   when that write failed: an ACME order accepts its challenge only once
   every connected instance holds it
   ([control-plane.md](control-plane.md#certificates-of-domains));
6. every 10 s sends an empty table and records the ingress as seen
   (`last_seen`), which keeps the [idle fence](#the-idle-fence) off.

A table, whole or delta, that takes more than 4 MiB coded
(`wire.PartBytes`) goes in **parts** (`wire.Split`): consecutive
messages, each with its own `seq`, every one but the last with `more`;
the first carries `whole`, the mesh, `Self` and the challenges, and the
environments, then the removals, are spread over the parts. Nothing comes between the
parts. As an entry takes at most 2 MiB, a part stays far below the
ingress's 32 MiB however large the table grows; a larger message would
fail every stream with `RESOURCE_EXHAUSTED`, at every reconnect, and
leave every instance on its cached table. Messages are JSON without
HTML escaping, so `&`, `<` and `>` take one byte, not six.

A stream ends `UNAVAILABLE` "this instance cannot read the database" once
the builder has been unhealthy for 30 s, and `UNAVAILABLE` "instance
stopping" when the instance stops ([Shutdown](#shutdown)). A removal ends
the removed ingress's streams with `PERMISSION_DENIED` before its
connections close.

The agent applies each message (`State.Apply`: a whole table replaces the
state, a delta must carry the next `seq`; the parts of a message are
applied together once the last one has arrived, never one alone, and a
part out of sequence drops them), hands a copy to the proxy,
sets the device's peers when the mesh's version changed, and writes the
cache. A table older than an hour is served with a warning in the log;
nothing expires it.

### The cache

At its start the agent loads `table.json`, `mesh.json` and `wg.key`,
configures the mesh device and hands the cached table to the proxy before
it contacts `ferund`: an instance that restarts while `ferund` is away
serves at once what was routable, with the domains' certificates, and
nothing new becomes routable until `ferund` is back. The cache holds the
domains' private keys, like the state directory's other keys readable
by the instance's user alone (0600 in a 0700 directory). A whole table
replaces the challenges; one without them (an older `ferund`) leaves
none.

## The mesh

### Address and peers

Each instance has a /96 of the cluster's ULA, from the nodes' sequence
`mesh_subnets`, which never hands an ID out twice; ID 0 stays
reserved. It is given at the first Hello after the ingress's first join
and kept across joins; the instance's address is `<prefix>::1`, its member
of the security group `ferun-ingress`. Nodes see the instance as one more
/96 peer, `ingress.<name>` (node names are DNS labels, so the dot never
collides), in the same peer list as the nodes, with the same versions and
deltas ([control-plane.md](control-plane.md#the-pods-mesh)). A
first subnet, a new key or endpoint, a re-join and a removal count the
peers' version up and wake `mesh-peers`. The instance gets the nodes in
its table and sets them as its device's peers: each node's /96 its only
allowed prefix, its endpoint, persistent keepalive 25 s.

**The key is fixed** from the first Hello after a join: a Hello with
another key is refused until the ingress joins again, which clears it.
`wg.key` and the certificate share the state directory, so a legitimate
new key always comes with a join, and a stolen certificate can neither
take the instance's place on the mesh nor make its key flap. **A join
again changes the key**: the key it cleared is refused at the next
Hello, and the agent makes a new one after every join, so the copy of a
stolen state directory does not stay on the mesh once the operator has
joined the instance again. **A key is one peer's**: a Hello whose new key
a node or another ingress holds (not removed) is refused, and so is a
node's `JoinMesh` with an ingress's key or another node's, checked under
`pod_network`'s row lock, which every writer of a new key takes before it
checks; feosd keeps one device peer per key, and two peers with one key
would take each other's prefix. Keys of small order, all zeros among
them, are refused.

### The device

The agent configures the kernel's WireGuard device over rtnetlink and
generic netlink, with stdlib `syscall` alone (`internal/ingress/wg`):

1. IPv6 is checked for `all`, `default` and the device, and turned on
   where it is off; without the right to, the error names `ferun-ingress
   prepare`;
2. the device is made (`ferun-mesh`, kind `wireguard`);
3. its MTU: `--mesh-mtu`, or `min(1420, MTU − 80)` of the interface that
   holds the endpoint's address, at least 1280 (WireGuard 32 bytes, UDP 8,
   the outer IPv6 header 40); the smaller MTU clamps the MSS of the
   connections the ingress opens;
4. up, with the address `<prefix>::1/128` (no duplicate address
   detection), and every other address on the device deleted, such as an
   old prefix;
5. a route for the ULA /64 through the device, from that address;
6. the key and the port; then the peers, as a diff that leaves unchanged
   peers and their sessions alone.

A device that cannot be configured, or read, is configured again every
2 s while the connection and the table go on. Every 2 s the agent reads
the device's peers and hands each node's last handshake to the proxy.

### Lost paths

A WireGuard peer is one UDP flow, from the device's port to the node's
51820, for as long as the peer lives, and the instance never lets it
rest: the keepalive goes every 25 s, a handshake that is not answered is
tried again every 5 s. Whatever the network keeps for that flow never
expires, a flow an outage left broken included; and an endpoint the
device learnt from a stray packet would lead nowhere for good, as the
nodes only answer the instance's traffic. On 2026-11-05 such flows stayed broken after outages
of the cloud's network, while new connections from the same pods
worked, until the pods were deleted (ADR 0021, "A lost path is
repaired").

So the mesh loop repairs a node that is **lost**: one that runs
endpoints of the table, has had no handshake for 180 s (none, or an
older one: the freshness of [Admission](#admission)), and has been a
peer for at least a minute, so that a new peer, or one a repair set
anew, gets that long. At most once a minute, a repair:

1. moves the device to the next of 16 ports, from `--mesh-port` on
   (51820 to 51835, cyclic; a port that is taken is skipped), so that
   every node is reached in a flow that is new to the network. With a
   `--mesh-endpoint` whose port is not `--mesh-port` (a port forwarded to
   the device) the port stays;
2. removes the lost nodes' peers and sets them again: their sessions,
   timers and learnt endpoints go, their configured endpoints come back,
   and the kernel handshakes at once;
3. says Hello again on the current connection with the new endpoint,
   `<pod IP>:<port>`, which `ferund` keeps, counting the mesh's peers up
   as for any new endpoint, so that `mesh-peers` gives it to the nodes.
   A Hello that fails is tried again from the mesh loop, and every new
   connection's Hello sends the endpoint anyway.

The nodes that answer keep their sessions; they follow the new port with
the instance's next packet, and the control plane's endpoint after it.
The port stays until the next repair or a restart, which starts at
`--mesh-port` again. A node without endpoints is not repaired: it carries
no traffic, and one that is down has none, so a dead node moves nothing.
`meshRepairs`, `lastMeshRepair`, `meshPort` and `meshEndpoint` in the
status, and `ferun_ingress_mesh_repairs_total` and
`ferun_ingress_mesh_port` in the metrics, show the repairs; each is logged
("gave the mesh device new paths to them") with the nodes.

`prepare`, the init container's command, writes 0 to
`net.ipv6.conf.{all,default}.disable_ipv6` where they are not 0, as root,
and logs what it changed. With `--join-token-source` it also copies that
file, the instance's own join token, to `--join-token-file` with mode
0400, and removes the copy when the source is missing. Nothing turns
forwarding on in the pod's network namespace: the ingress reaches pods,
and nothing reaches through it.

## Public environments

### Publishing

The ingress serves an environment only once its tenant made it public:

    PUT /api/v1/environments/{env}/public    {"public": true}

`{env}` is the environment's name or UUID; the body's `public` is
required (400 when it is missing, null or not a boolean). The answer is
`{public, hosts, rolling}`. The route door keeps requiring route tokens
for every environment; publishing is what lets the ingress serve one
without them.

- **The first publish** gives the environment its host label (below) and
  sets `ingress_group`, which makes its image services' pods name their
  own group `ing-<sandbox id>` ([Groups](#groups-and-admission)): their
  template changes, so the services roll once, surge 1 and gated by
  readiness. The answer says `rolling: true`. Until the new replicas are
  ready, requests wait the pending bound, then get 503.
- **Unpublishing** (`{"public": false}`) takes the environment out of the
  table within about 100 ms; the ingress then closes its open
  connections to the environment's instances, upgraded ones and
  streaming answers included, since the nodes keep established flows
  after the rules change. Its groups' rules shrink to none in the next
  `env-groups` pass. Label and `ingress_group` stay: publishing again
  rolls nothing.
- Refused with 409: publishing while the platform has no apps zone ("the
  platform serves no apps zone (--apps-zone); an environment cannot be
  public") or serves not the tenant's zone ("the platform serves no zone
  old.example.com, tenant acme's; …", a zone left behind by a changed
  `--tenant-zone`), and either while the environment or its tenant is being
  deleted. A call that changes nothing writes nothing.
- A change notifies `ferun_ingress env:<id>`, wakes `env-reconcile` and
  `env-groups`, and is logged with the tenant and the environment.

`--apps-zone` (`FERUN_APPS_ZONE`) of `ferund` names the zone; empty, the
default, is the platform's switch: no environment has a host, and none
can be published. `--tenant-zone` (`FERUN_TENANT_ZONE`) names the
second zone, the tenant zone ([The tenant zone](#the-tenant-zone)). Deploying the ingress makes no environment public, and
a clone never copies `public`.

`GET /api/v1/environments` and `GET …/{env}` show `public` and `hosts`
(while the environment is public and not being deleted: its served
domains, else the default host while the platform has a zone; otherwise
empty), and each route its
`door`: `ingress` for a route to an image service of a public
environment, with its `url` (`https://<host><prefix>`), `route` for every
other. A plan says the same as info findings at `routes.<name>`: "served
by the ingress at https://<host><prefix> and by the route door", "served
by the route door; the ingress serves it once the environment is
public", or "served by the route door alone: an attached service runs in
a sandbox, which the ingress does not reach". A route's `host` stays
stored and not used: the ingress and the route door serve a route on
every host of its environment.

### Host labels

An environment's default host is `<label>.<zone>`, in its tenant's zone
(`tenants.zone`: the apps zone for none, else the tenant zone, ADR 0031).
The label is a name in that zone, given at its first publish, or chosen by its tenant (below), and changes
only when its tenant chooses another:

- **natural**: `<environment>-<tenant>`, when it names that pair alone
  and is a valid label:
  - the environment's name has no hyphen, so the label splits at its
    first hyphen;
  - the tenant has no dot, so the label holds the tenant as it is, and
    no `--`, so the label has none;
  - it does not end in a hyphen and 16 hex characters, the fallback's
    form;
  - it is a DNS label of at most 63 characters whose third and fourth
    characters are not `--` (RFC 5891's reserved form);
- **fallback**: at most 46 characters of the name (prefixed `e-` when the
  name has that reserved form, trailing hyphens trimmed), a hyphen, and
  the first 16 hex characters of the SHA-256 of the environment's UUID;
- **chosen**: a label the tenant names with `PUT …/public {"public":
  true, "hostLabel": "stage--kino"}`: a DNS label of at most 63
  characters with `--` in it, not as its third and fourth characters,
  and not ending in a hyphen and 16 hex characters. It replaces the
  label the environment had or would get; that one stays the tenant's;
- **plain**: a chosen label without `--`, which only a tenant that owns
  the apps zone may choose (`--apps-zone-owners`, FeCode's installation
  `fecode`, for a project's production at `<project>.<apps zone>`), and
  only while its hosts are in the apps zone: the tenant zone has no
  owners, and a plain label there is a 400. A plain label is a
  DNS label of at most 63 characters, not ending in a hyphen and 16 hex
  characters. One whose part after its first hyphen names an existing
  tenant is refused (409 `host_label_taken`), since it is the natural
  label of that tenant's environment, unless the owner holds it already.

So no two pairs of environment name and tenant have the same natural
label, and no natural label is another environment's fallback: a host
that reads as `<environment>-<tenant>` belongs to that tenant. `web` of
`acme-io` is `web-acme-io`; `web` of `acme.io` and `web-acme` of `io`,
which would read the same, get the fallback. A chosen label is neither
form: no natural label has `--`, and every fallback ends in its hash.
A plain label is never a fallback, but one with a hyphen reads as a
natural label: `ironcore-dashboard` as `ironcore` of `dashboard`. That
is the zone's owner's trade, bounded so: an existing tenant's natural
label is refused, and a tenant made later whose natural label the owner
holds gets its fallback, as for a label another tenant holds. A host
that reads as `<environment>-<tenant>` is that tenant's or the zone's
owner's.

`host_labels` registers every label ever given, per zone (`zone`: `''`
for the apps zone, else the zone's name, migration `0029`), with the
tenant that first claimed it there: a label taken in the tenant zone is
not taken in the apps zone, so whoever makes a tenant cannot reach the
labels of the platform's products. A claim is one atomic upsert, the chosen label, or the
natural label first, then the fallback: a label is free only for its owner, and only once its
environment is gone (the foreign key sets it free on delete). So a host,
its cookies, service workers and HSTS never pass to another tenant; a
tenant created again under the same name inherits its labels.
`environments.host_label` mirrors the claim; `host_labels` (its key
`(zone, label)` and its unique `env_id`) keeps one host per environment
and one environment per host, so the label alone is no longer unique
there.

### The route door beside the ingress

The route door ([control-plane.md](control-plane.md#the-route-door))
stays as it is: it serves every environment's routes to a request with
the environment's route token, attached services' routes included. The
ingress serves the routes to image services of public environments only;
a route to an attached service runs in a sandbox, whose loopback no
address of the mesh reaches, and the ingress answers it 404 "route
<path> is served by the route door". FeCode keeps using the route door.

## Groups and admission

### The groups

- **`ing-<sandbox id>`**: every image service's pod of an environment
  whose `ingress_group` is set names a group of its own, from its
  creation (`PodTemplate.IngressGroup`, part of the template hash with
  `omitempty`, so that no other template's hash changes; the controller
  appends the ID to the row's groups at its insert). While the
  environment is public its one rule allows TCP in on the pod's routed
  ports from the group `ferun-ingress`; otherwise, or without routed
  ports, it has no rules. The routed ports are the numbers, in the pod's
  own deployment, of the ports that the routes of the deployed and the
  latest deployment give its service. It has no members; its version is a
  hash of its content. Addons, jobs and attachable sandboxes never name
  one, and a service no route names opens nothing.
- **`ferun-ingress`** has no rules. Its members are the mesh addresses of
  the ingress instances that are not removed. Its version is
  `pod_network.ingress_group_version`, a counter counted up under that
  row's lock in the transaction that changes the members (a first
  address, a removal, a re-join of a removed ingress), so that versions
  follow commit order. It is never deleted.

Route changes and publishing change rules, never pods. The groups are put
on the nodes by `env-groups` and the prepare hook, as the environments'
other groups ([control-plane.md](control-plane.md#security-groups)). An
`ing-` group a node holds that none of its rows wants is deleted when its
sandbox is gone or runs on another node, once no pod there names it.

### What the nodes hold

What a node holds is recorded after a successful put, and on every
`env-groups` pass from what the node lists when it has the wanted content
already:

- `env_instances.ingress_ports`: the ports of the pod's `ing-` group as
  its node holds them;
- `nodes.ingress_group_version`: the version of `ferun-ingress` the node
  holds.

Puts ignore versions, so a put of the prepare hook and one of
`env-groups` that race can leave a record wrong for one pass (10 s); the
next pass records again from what the node lists. A wrong record costs a
request one dial timeout and a retry elsewhere.

### Admission

The table carries only admitted ports, and the ingress sends to an
endpoint only when all three hold:

1. **The port is admitted**: the pod's node holds its `ing-` group with
   the route's port.
2. **The group version is admitted**: the endpoint's node holds a version
   of `ferun-ingress` at least the instance's `admittedVersion`, the
   counter's value at which its address last entered (or left) the group.
   Versions follow commit order, so `≥` is sound.
3. **The mesh is admitted**: the ingress has a WireGuard handshake with
   that node younger than 180 s, WireGuard's reject-after time; with a
   keepalive of 25 s a healthy peer handshakes about every 2 minutes. A
   handshake proves that the node holds the instance's current key and
   that UDP passes both ways, which catches a node whose `mesh-peers`
   call failed.

An endpoint on a node without a fresh handshake is the last resort: it
is used only when no other endpoint is usable, and its node's path is
repaired ([Lost paths](#lost-paths)). A new ingress instance
counts an endpoint only once that endpoint's node holds the group at its
version, within one `env-groups` pass.

## Requests

### Routing

1. **The host**, in lower case, without its port and a trailing dot,
   names the environment, a domain as the default host; an unknown host
   is 404 "no such host".
2. **The path** may not hold a segment that is, or decodes once or twice
   to, `.` or `..` (400), and must decode (400).
3. **The route** with the longest prefix, segment by segment, matches;
   ties go to the route whose name sorts first. None is 404 "no route for
   <path>"; a route to an attached service is 404 "route <path> is served
   by the route door".
4. **The body**: one larger than `--max-request-body`, by its
   `Content-Length` or as it is read, is 413.
5. **The endpoint**: round robin over the usable endpoints of the route's
   service ([Admission](#admission)), else over the last resort; an
   endpoint whose dial failed cools down for 5 s. For a service with an
   idle policy the instance is held first
   ([Holds](#holds-and-the-idle-fence)); an instance whose hold is
   refused is skipped until the table changes.
6. **None usable**: the request is held ([below](#held-requests-and-wake)).

### Port 80

The HTTP listener answers ACME's HTTP-01 challenges first, on every
host, whether the table serves it yet or not (a domain's first
certificate comes before the domain is served): `GET` (or `HEAD`)
`/.well-known/acme-challenge/<token>` is 200 with the challenge's key
authorization as `text/plain` (`Cache-Control: no-store`) when the table
holds the token and the request's host, canonical as above, is the
challenge's host. Any other request under that path, an unknown token,
another host, another method, is 404 "no such challenge". Everything
else of a host the table knows is 308 to `https://`, and of any other
host 404 "no such host".

### Forwarding

- The upstream is `http://[<pod address>]:<port>`, the path stripped of
  the route's prefix only with `strip`, the query kept. The upstream sees
  the `Host` the client sent, and `X-Forwarded-For`, `X-Forwarded-Host`,
  `X-Forwarded-Proto` and `X-Real-IP` set by the ingress from the
  connection; the client's own are dropped, and so are `Forwarded` and
  `True-Client-IP`, which frameworks also read for the client's address.
  Under `externalTrafficPolicy: Local` the connection's address is the
  client's ([Kubernetes](#kubernetes)).
- The upstream gets `traceparent` (and `tracestate`) of the request's
  span at the ingress ([Trace context](#trace-context)).
- HTTP/1.1 to the upstream, HTTP/1.1 or HTTP/2 from the client.
  Responses stream unbuffered; upgrades such as WebSockets pass through;
  the encodings pass as they are.
- Pooled connections: at most 32 idle per endpoint and port, closed after
  `--upstream-idle-timeout` (4 s), below common upstreams' keep-alive
  timeouts (Node.js 5 s, uvicorn 5 s), so that the ingress closes first;
  lower it for gunicorn (2 s). At most `--max-conns-per-endpoint` per
  endpoint and port; further requests wait for one, at most
  `--upstream-wait-timeout` (10 s) with the dial, each time they wait
  (Go's transport sends a GET once more by itself when the reused
  connection it went out on closes without an answer). A request that
  waited that long goes once to another usable endpoint, as nothing of
  it was sent, else it is 503 "the service is busy, try again",
  `Retry-After: 5`; the endpoint does not cool down. When endpoints leave the table
  the pool's idle connections are closed.
- **When the table changes**, the open connections to what it no longer
  routes to are closed, upgraded ones and streaming answers included: at
  once when the environment left it (unpublished, deleted, no apps
  zone), its service lost its routes, or the instance the route's port;
  30 to 40 s later when only the instance left a service that stays
  routed (a replica stops, is not ready or pauses), so that its
  requests end with its pod, which goes after 10 s, rather than with the
  table's change. A request that picked an instance just before the table
  dropped it does not dial it, and a dial under way when the table
  dropped it does not join the pool: the request is routed again by the
  new table.
- The servers: header timeout 10 s, idle timeout 120 s, headers at most
  32 KiB in at most 100 fields (431 beyond), no request trailers (400),
  at most 63 streams per HTTP/2 connection and 64 KiB of a request's
  body received before the request reads it ([Limits](#limits)); no
  write timeout and no timeout for an upstream's response headers, so
  that streams and long polls live.
- On a host under the apps zone or the tenant zone, every cookie of a
  response is made host-only ([Cookies](#cookies)).

### Trace context

Each request routed to an environment is a server span of the ingress
in [W3C Trace Context](https://www.w3.org/TR/trace-context/), without a
tracing SDK:

- A valid `traceparent` is continued: exactly one field, version `00`
  at 55 characters or a later version at least as long, lower case hex,
  neither ID all zeros, version `ff` invalid. Its trace ID and flags are
  kept, its span ID becomes the parent, and `tracestate` is passed on
  when it is printable ASCII of at most 512 characters.
- Otherwise the ingress starts a trace: a random trace ID, sampled
  (`01`), since every request is recorded; a client's `tracestate` is
  dropped with its invalid `traceparent`.
- The span ID is always the ingress's own, random; the upstream gets
  `traceparent: 00-<trace>-<span>-<flags>`, so the application's spans
  are its children. A retry to another instance keeps the span.
- The span's data is the request's access log record
  ([Access log](#access-log)), which the agent also sends to the sink as
  an OTLP server span (below).

### Access log

- After its response, each request routed to an environment becomes a
  record: its start and duration, the environment and its tenant, the service, the
  instance of the last attempt, host, method, the escaped path without
  the query, protocol, status (101 for an upgrade, 0 when none was
  written), outcome (the status class, `upgraded`, `aborted`), bytes in
  and out, attempts, how long it was held, client address, user agent,
  and the span's IDs. The path is cut to 1 KiB, the user agent to 256
  bytes, other text to 256 bytes. Requests that reach no environment
  (unknown host, a refusal before routing, port 80) are only counted in
  the metrics.
- **Into the sink** (ADR 0026 §8, the contract in
  [sink.md](sink.md#the-ingresss-access-log)), its only destination.
  With `--sink-endpoint` the agent sends the records to the sink
  (`LogsService/Export`, OTLP over gRPC, TLS 1.3 with its ingress
  certificate; the sink's certificate must verify to the trust bundle,
  carry no IP address and name `spiffe://<td>/sink/<name>`, whatever
  address was dialled). The name is resolved by gRPC's DNS resolver and
  dialled again after a lost connection. Each (environment, service)
  is a stream of the run, keyed `ingress/<name>`, `<env>/<service>`, the
  run's ID (drawn when the instance starts), each record a JSON line of
  it numbered from 0, with the environment's tenant as the proxy routed
  it. The agent keeps up to 10,000 records unsent (at most about 15 MiB,
  the batch on its way included) and sends every second, or as soon as
  1,000 wait. A batch holds at most 1,000 records or 1 MiB of lines and
  is numbered once: one the sink failed to take is sent again as it was
  after a backoff (1 s, doubling to 30 s), while new records wait in the
  buffer; one it refuses (`INVALID_ARGUMENT`, `RESOURCE_EXHAUSTED`) is
  dropped and counted. A full buffer drops the new record, and a record
  without a tenant is dropped; both are counted
  (`ferun_ingress_access_log_dropped_total`). The agent sends while it
  holds a certificate, with or without its connection to `ferund`, and
  the shutdown sends the rest the same way.
- **Spans** (ADR 0026 §10, the contract in
  [traces.md](traces.md#the-ingress)): each batch's records also go as
  server spans, a batch of their own to the sink's `TraceService/Export`
  on the same connection, once the sink took or refused the batch's
  lines. A span batch is sent once: one the sink failed to take or
  refused, and spans it rejected (`partial_success`), are dropped and
  counted (`ferun_ingress_spans_dropped_total`); the lines are written
  as before.
- **Without `--sink-endpoint`** the access log is not kept: the agent
  says so once at its start (Info) and counts every record as dropped;
  the proxy never waits for it.
- `ferund` reads the lines back for
  `GET /api/v1/environments/{env}/access-logs`
  ([control-plane.md](control-plane.md)); it keeps no copy of its own
  (its table `access_logs` was dropped on 2026-10-31, migration `0020`).

### Retries

A failed attempt goes once to another usable endpoint (or the last
resort):

- whatever the method, when the dial failed and nothing of the body was
  read: nothing reached the upstream;
- for GET, HEAD and OPTIONS without a body, when the request was written
  and no response came.

Otherwise it is 502 "the service did not answer"; a refused connection is
502 "the service refused the connection; it must listen on [::]:<port>".
The answer to a dial that failed otherwise (a timeout, no route), and
503 "no ready instance of <service>" while the service's instances only
cool down after one, close the client's connection (`Connection: close`,
GOAWAY on HTTP/2): its next request comes on a new connection, which the
balancer likely sends to another worker, whose instance may reach the
node.
An image service must listen on `[::]`: one that binds `127.0.0.1` or
`0.0.0.0` alone passes its readiness check, which goes through the port
door on the pod's loopback, and fails here.

### Held requests and wake

A request that finds no usable endpoint is held until the table or the
mesh's health changes, its client goes away or the instance drains:

| The service has | The request waits | Then |
|---|---|---|
| asleep or waking instances | the wake bound, `--wake-hold` (2 m), from the request's start | 503 "no instance of <service> woke in time, try again", `Retry-After: 10` |
| pending instances, or endpoints not admitted yet | the pending bound, `--pending-hold` (10 s) | 503 "no ready instance of <service>", `Retry-After: 5` |
| neither | not at all | 503 "no ready instance of <service>", `Retry-After: 5` |

A request's bound only grows: one that saw asleep or waking instances at
its start or at any later pick keeps the wake bound. While instances are
asleep the proxy asks the agent to wake the service; the agent sends
`Wake(env, service)`, one call per service at a time and at most every 5
s, and drops it while it is not connected; a held request looks again at
least every 5 s, and so asks again. More than `--max-held-per-env` held
requests of one environment, or `--max-held-per-tenant` of one tenant:
503 "too many requests waiting for this host"; more than `--max-held`
in all: 503 "too many requests waiting";
both with `Retry-After: 1` ([Limits](#limits)). Draining ends every held
request with 503 "the ingress is shutting down, try again",
`Retry-After: 1`.

`ferund`'s `Wake` answers `NOT_FOUND` for an environment that is not
public, deleting or unknown, and for a service that no route of the
routing deployment sends to or that is attached. It wakes exactly the asleep
instances, the table's predicate, never creating, stopped, failed or
other ones: each once at a time on that instance, in the background on
the service's context, never the caller's (`Engine.Sync`, then
`Engine.Ensure`, within the preparation budget). It answers how many it
wakes and when to look again: 5 s, 10 s when one is hibernated or
hibernating.

### Limits

One tenant must not take an instance from the others, nor one
environment its tenant's other environments. Without bounds, an app that
accepts connections and never answers, reached by clients that open many
HTTP/2 streams, pins requests, goroutines and memory until the instance
is killed for its memory, and every public app goes down with it; an
environment whose service stays pending would fill the held requests,
and every other environment's cold start would get 503.

The shares nest: a tenant's environments together have the tenant's
share, a quarter of the instance's cap by default, so that it takes four
tenants to fill the cap however many environments each makes public;
within it each environment has its own, an eighth of the cap. A share
larger than the one it sits in has no effect.

- **Forwarded requests.** A request counts from its first pick of an
  endpoint to its end: the idle hold, the wait for a connection and the
  answer, an upgraded connection for its whole life. One environment may
  have `--max-inflight-per-env` (256) at once, one tenant
  `--max-inflight-per-tenant` (512): beyond, 503 "too many requests to
  this host, try again", `Retry-After: 5`. The instance may have
  `--max-inflight` (2048): beyond, 503 "the ingress is busy, try again",
  `Retry-After: 1`.
- **Held requests.** One environment may have `--max-held-per-env` (128)
  held at once, one tenant `--max-held-per-tenant` (256), the instance
  `--max-held` (1024) ([above](#held-requests-and-wake)).
- **What a request keeps.** A held or forwarded request keeps its
  headers and the part of its body that came but was not read: a held
  request reads none, nor does one whose instance reads nothing. So:
  - its headers take at most 32 KiB in at most 100 fields; beyond, 431;
  - a request that announces trailers gets 400: HTTP/2 would keep up to
    900 of their fields, past the bound of 100;
  - an HTTP/2 client may send 64 KiB of a body before the request reads
    it. That is HTTP/2's own initial window and the least that works,
    since a client may send that much before it has the server's
    settings; Go's default of 1 MiB let one tenant's held requests pin
    280 MiB. The smaller window slows HTTP/2 uploads over long round
    trips: 64 KiB per round trip, about 5 Mbit/s at 100 ms. HTTP/1.1
    uploads keep TCP's window, as their bodies wait in the kernel;
  - a connection's window is every stream's, 63 × 64 KiB (net/http takes less than 4 MiB), so that the
    held requests of a connection never stall its other uploads: a
    browser may carry every app under the zone on one connection (one
    wildcard certificate, one address), and with Go's default of 1 MiB
    16 requests held there stopped every upload on it until they ended.
    It adds no memory, since each request's part stays within its own
    window.
- **Memory.** Measured with Go 1.26, heap and stacks, each request on a
  connection of its own: a held request takes at most about 250 KiB
  (HTTP/2, its body's 64 KiB window full, its path and headers at their
  bound; about 90 KiB over HTTP/1.1), a forwarded one about 350 KiB (the
  same, to an instance that reads nothing; an upgraded connection over
  TLS takes about 140 KiB, up to about 180 KiB resident after large
  messages, beside its headers). Requests that share a connection take
  less each. The default caps therefore take about 250 MiB for the 1024
  held and 700 MiB for the 2048 forwarded requests, together about 950
  MiB of the 1.5 GiB on Kubernetes, and leave room for the client
  connections and the process below the soft limit (1.35 GiB).
  The kernel's socket buffers count against the container's limit as
  well and are not in these figures. Raise the caps and the memory limit
  together. `--memory-limit` is the container's limit (on Kubernetes
  from the downward API); the Go runtime's soft limit is set to 90% of
  it, so that garbage collection works harder before the kernel kills
  the process. `GOMEMLIMIT`, when set, wins.
- **The wait for a connection** to an instance is bounded, each wait on
  its own (`--upstream-wait-timeout`, [Forwarding](#forwarding)); the
  answer's time is not, for long polls and streams.
- **HTTP/2**: at most 63 streams per connection (Go's default is 250);
  further streams are refused.
- **Not bounded yet**, the known limits:
  - client connections per client address. Behind the shoot's
    balancer, with `externalTrafficPolicy: Local`, the ingress sees the
    client's address, so a cap per client can be built;
  - the number of environments a tenant may make public. Every instance
    of the ingress holds every entry of the table, up to 2 MiB each
    coded ([What it holds](#what-it-holds)), and every `ferund` with a
    stream builds them all, so one tenant's many public environments
    grow the memory of all of them;
  - the builder's parsed deployment specs (`table.Specs`), bounded by
    count, 4096, not by bytes. A stored spec has at most 1 MiB
    ([control-plane.md](control-plane.md#the-deployment-spec)), which
    parses to about as much when it is long values and to about 20 MiB
    when it is many small environment variables, so a few dozen public
    environments with such specs fill a `ferund` of 512 MiB.

### Answers

| Status | Text | When |
|---|---|---|
| 200 | the key authorization | port 80: an ACME HTTP-01 challenge the table holds for the host ([Port 80](#port-80)) |
| 308 | | plain HTTP for a known host: to `https://` |
| 404 | "no such challenge" | port 80: anything else under `/.well-known/acme-challenge/` |
| 400 | "the path has a .. segment", "the path has a . segment", "the path is not escaped correctly" | |
| 400 | "request trailers are not supported" | a request that announces trailers ([Limits](#limits)) |
| 404 | "no such host" | a host the table does not know, on either listener |
| 404 | "no route for <path>", "route <path> is served by the route door" | |
| 405 | "CONNECT is not supported" | |
| 413 | "the request body is larger than <n> bytes" | |
| 431 | "the request has more than 100 header fields" | headers beyond 32 KiB get Go's own 431 |
| 502 | "the service did not answer", "the service refused the connection; it must listen on [::]:<port>", "the service's protocol switch failed" | |
| 503 | "the ingress has no routing table yet" (`Retry-After: 5`), and the answers of held requests above | |
| 503 | "too many requests to this host, try again", "the service is busy, try again" (`Retry-After: 5`), "the ingress is busy, try again" (`Retry-After: 1`) | [Limits](#limits) |

## Holds and the idle fence

### Holds

A service with an idle policy (`idle` in its spec) is paused by the idle
loop when nothing holds it. Touches cannot keep a pause from freezing a
request that reached the pod just before it; holds can, since a pause's
begin rechecks them under the sandbox's row lock. So the ingress holds
such a service's instances the way the port door does:

- **Admit.** Before the first request to an instance whose hold it does
  not hold validly, the proxy calls `Hold{hold: [id]}`, once at a time
  per instance, within 2 s; concurrent requests share the answer. `ferund`
  locks the sandbox's row `FOR NO KEY UPDATE` and requires a service
  instance of a public environment that is not being deleted, running
  without an op; it then writes a hold of kind `forward` with the holder
  `ingress:<name>`, the serving `ferund` instance as its `instance_id`
  and a lease of 30 s, and touches the sandbox. A refusal ("gone", "not
  public", "not running", "busy", "invalid") skips the instance; the
  request picks again or is held. When `ferund` cannot be reached the
  request goes on unheld, and the instance's requests do for 5 s; the
  fence covers that case.
- **Renew.** Every 10 s the instances with a request in flight or one in
  the last 10 s are held again. A hold counts as valid until its send
  time plus the lease less 10 s.
- **Release.** The quiet ones are released: the hold is deleted and the
  sandbox touched, so that its idle interval starts with the release.
- **Leave.** A stopping instance sends `Hold{leaving: true}`, which
  releases all of its holds and sets `ingresses.left_at`.

`ferund` refuses Hold while it stops, and a list of more than 10 000 IDs.
Expiry is by lease, so the holds of an instance that crashed lapse within
30 s. Services without an idle policy are never paused for idleness and
are not held. Every Hold records the ingress as seen.

### The idle fence

While an ingress instance is cut off from `ferund`, its holds lapse, and
the idle loop would pause services it still serves. So the idle loop,
and an idle pause's recheck in its begin, leave out a sandbox that is a
service instance of a public environment while an ingress instance
exists that is joined, has a mesh address, has not left, and was last
seen between 45 s and 15 minutes ago. An ingress is seen at every Hello,
every Sync heartbeat (10 s) and every Hold. After 15 minutes it counts as
gone; one that left cleanly never fences.

## TLS and the zones

### The certificate

The HTTPS listener picks the certificate by SNI. A domain the table
serves gets its own certificate from the table (its chain and key, which
`ferund` obtained over ACME, [control-plane.md](control-plane.md#certificates-of-domains)):
the SNI in lower case without a trailing dot is one of its
environment's domains. Parsed once per table that changes it, and
reused while it stays the same, so a renewal is served from the next
handshake on; a certificate that does not parse, whose key is not its
own, or that is not for its name is logged and its host gets the
wildcard.

An SNI that the tenant zone's wildcard covers (`--tenant-zone-tls-cert`,
one label under the zone, as a wildcard does) gets that one. Every other
SNI, or none, gets the operator's wildcard `*.<apps zone>`. Both come
from files that cert-manager renews into a Secret and the kubelet into
the pod. They are read again every 30 s, and a pair that changed and
loads replaces the one in use; one that does not load keeps it. An
unknown host is the proxy's 404. TLS 1.2 and newer, ALPN `h2` and
`http/1.1`.

### The apps zone

`--apps-zone` of `ferund` names it, and `Hello` hands it to the ingress.
The Gardener deployment derives it from its domain, `apps.<DOMAIN>`
(`apps.fecode.dev`); domains of tenants come later, beside it. DNS: a
wildcard record `*.<apps zone>` pointing at the ingress's balancer, DNS
only.

### The tenant zone

The tenants that people make get their default hosts under a zone of
their own (ADR 0031), such as `apps.run.example.com`, so that their
labels and cookies stay away from the apps zone, where the platform's
products have their hosts. `--tenant-zone` of `ferund` names it (empty:
none; it needs `--apps-zone`, and neither zone may be or lie under the
other); `Hello` and `Self` hand it to the ingress as `tenantZone`. It
has the apps zone's rules: an operator's wildcard certificate, cookies
made host-only, no claims of names under it, the labels above, and no
plain labels, since it has no owners.

- **A tenant's zone** is `tenants.zone` (migration `0027`): NULL, the
  apps zone, for the tenants operators make
  (`POST /api/v1/admin/tenants`) and for every tenant while there is no
  tenant zone; the tenant zone's name for the tenants people make, set
  at their creation (`model.Zones.ForNewTenant`, `store.MakeTenant`).
  FeCode's tenant `fecode` stays in the apps zone with its plain labels.
- **An operator moves a tenant**, `PUT /api/v1/admin/tenants/{tenant}/zone
  {"zone": "apps.run.example.com"}` (or the apps zone's name). Its
  environments' hosts move as a changed label does: each keeps its
  label where it may in the new zone (free, or the tenant's there, and
  not a plain one unless the tenant owns the new zone's plain names),
  else gets its natural label or its fallback there; the labels left
  behind stay the tenant's. The old hosts answer 404 at once. The
  answer lists every environment with its label before and after and
  its new host; an activity row `tenant_zone_set` records the move.
- A tenant whose `tenants.zone` names a zone the platform does not serve
  (after `--tenant-zone` changed) has no default hosts, and publishing is
  a 409, until an operator moves it.
- **The operator** publishes a wildcard record `*.<tenant zone>` to the
  ingress's balancer, a wildcard certificate for it into the ingress's
  pods (`--tenant-zone-tls-cert`, `--tenant-zone-tls-key`), and, as for
  the apps zone, a Public Suffix List entry is recommended. Without that
  certificate the tenant zone's hosts get the apps zone's wildcard,
  which no client accepts for them.

### Cookies

Apps under `apps.fecode.dev` share their registrable domain with the
platform's hosts (`run`, `id`, FeCode), so an app could set cookies for
`fecode.dev` that reach them (cookie tossing, login CSRF) or for other
apps under the zone. On a host under the apps zone or the tenant zone the ingress therefore
removes, from every `Set-Cookie` header of a response (any status, a
1xx's and an upgrade's included), each `Domain` attribute whose value,
without a leading dot and in lower case, is anything but the request's
host: the cookie becomes host-only. A cookie without `Domain`, or with
the host as its `Domain`, passes unchanged; the other attributes, their
spelling and the headers' order are kept. Removals are counted
(`ferun_ingress_cookie_domains_removed_total`) and logged at debug, once
per host and minute. Hosts outside the zone, the domains, are not
rewritten: a domain is its tenant's own registrable name.

A proxy cannot stop cookies an app's own JavaScript sets
(`document.cookie`). The platform's hosts guard against those with
`__Host-` session cookies, which no other host can set or overwrite
(FeCode's are), and a Public Suffix List entry for the apps zone, an
operator's action, makes browsers refuse such cookies altogether; it is
recommended.

## Health, metrics and status

The health listener (`--health-listen`, port 9090 on Kubernetes) serves:

- `GET /healthz`: 200 while the process serves;
- `GET /readyz`: 200 "ready", or 503 "not ready: <reason>";
- `GET /metrics`: Prometheus text;
- `GET /statusz`: the status as JSON.

### Readiness

An instance is ready when it is not draining, its identity was not
removed, it has a table (cached or streamed) and a configured mesh
device, and its data path either has no endpoints, or has at least one
node with endpoints whose handshake is fresh and whose group version
admits it, and either every node with endpoints is so or 60 s have passed
since the first table of the process. The last rule keeps a restarted
instance out of the balancer until the nodes have handshaken with it,
without letting one node that never took the instance as a peer keep it
out for good. A change of the verdict is logged.

Readiness does not need every node with endpoints, and does not drop an
instance that has lost one node while it reaches another. The balancer
checks no worker's health and `Local` keeps a worker's share to its own
instance ([Kubernetes](#kubernetes)): an instance that is not ready drops
its worker's share of every service, not only of the lost node's, and a
node whose own network is down would take every other instance out at
once. A node's pods have no other path to fail over to. The instance
answers the lost node's services 502 or 503 instead
([Retries](#retries)), closes the client's connection with them, and
repairs the path ([Lost paths](#lost-paths)).

### Status

`/statusz`, and `ferun-ingress status`, which reads it from inside the
pod: the name, the mesh's ULA, prefix and address, the apps zone, the
admitted version, whether and to which address and instance it is
connected, the control-plane addresses, whether it joins, the table's
sequence, age, number of environments and mesh version, the
certificate's serial, expiry and renew time, the refusals (since the last
Hello, in all, the last one and when), whether the identity was removed,
the mesh device, its public key, MTU, port and endpoint and the repairs
of its paths (how many, the last), each peer with its endpoint,
last handshake and freshness, the held instances, the data path (the
nodes with endpoints, those fresh and admitted, the number of endpoints),
readiness and its reason, draining, and the HTTPS certificate's names and
expiry.

### Metrics

| Metric | What it counts |
|---|---|
| `ferun_ingress_requests_total{class}` | requests answered, by status class, `upgraded` or `aborted` |
| `ferun_ingress_upstream_errors_total{kind}` | failed attempts: `dial`, `response`, or `wait` (no connection came free in time) |
| `ferun_ingress_rejected_total{limit}`, `ferun_ingress_inflight` | requests answered 503 because a limit was taken: `inflight_env`, `inflight_tenant`, `inflight`, `held_env`, `held_tenant` or `held`; requests forwarded now |
| `ferun_ingress_upstream_refused_total`, `…_retries_total`, `…_last_resort_total`, `…_too_large_total` | refused connections, retries, picks of a last-resort endpoint, 413s |
| `ferun_ingress_headers_rejected_total{reason}` | requests rejected for their headers: `fields` (431), `trailers` (400) |
| `ferun_ingress_held_total`, `…_hold_timeouts_total`, `…_wakes_total`, `ferun_ingress_held` | held requests, those that hit their bound, wakes asked for; held now |
| `ferun_ingress_idle_hold_calls_total`, `…_admit_failures_total`, `…_refusals_total`, `…_renew_failures_total`, `ferun_ingress_idle_holds` | Hold calls and their failures; instances held now |
| `ferun_ingress_cookie_domains_removed_total` | `Domain` attributes removed |
| `ferun_ingress_upstream_connections_cut_total` | open connections to instances closed because the table no longer routes to them |
| `ferun_ingress_upstream_connections`, `ferun_ingress_endpoints`, `ferun_ingress_endpoint_nodes{admitted}`, `ferun_ingress_hosts` | the data path now |
| `ferun_ingress_domain_certificates`, `ferun_ingress_acme_challenges`, `ferun_ingress_acme_answers_total{result}` | hosts with a certificate of their own; the challenges in the table; requests under `/.well-known/acme-challenge/`, `answered` or `unknown` |
| `ferun_ingress_access_log_sent_total`, `…_dropped_total`, `ferun_ingress_access_log_pending` | access log records the sink took, those dropped (no `--sink-endpoint`, a full buffer, a refusal), those waiting (a batch on its way to the sink included) |
| `ferun_ingress_spans_sent_total`, `…_dropped_total` | server spans of batched records the sink took, and those dropped (failed, refused or rejected by the sink, or without a tenant) |
| `ferun_ingress_table_age_seconds`, `ferun_ingress_mesh_nodes{handshake}`, `ferun_ingress_mesh_handshake_age_seconds{node}`, `ferun_ingress_mesh_peers`, `ferun_ingress_mesh_configured` | the table's age and the mesh's health |
| `ferun_ingress_mesh_repairs_total`, `ferun_ingress_mesh_port` | repairs of the paths to lost nodes ([Lost paths](#lost-paths)); the device's port |
| `ferun_ingress_info{name,version}`, `ferun_ingress_ready`, `ferun_ingress_draining`, `ferun_ingress_control_connected`, `ferun_ingress_control_refusals_total`, `ferun_ingress_identity_removed`, `ferun_ingress_identity_not_after_seconds`, `ferun_ingress_tls_not_after_seconds`, `ferun_ingress_tenant_zone_tls_not_after_seconds` (with a tenant zone's certificate) | the instance |

## Shutdown

### ferun-ingress

On SIGTERM or SIGINT:

1. it drains: `/readyz` fails, held requests end with 503, no idle hold
   is admitted any more;
2. it serves on for `--shutdown-delay` (10 s), while the balancer takes
   it out;
3. the HTTPS and HTTP servers stop accepting and wait for their requests
   up to `--shutdown-grace` (40 s), then cut what is left;
4. the connections to the instances are closed, which ends upgraded
   connections and long polls;
5. it sends what the access log holds (within 3 s), then
   `Hold{leaving: true}` releases its holds (within 2 s);
6. the connection to `ferund` closes;
7. the health listener closes last.

Its pod's termination grace is 60 s, above the sum; it needs no preStop.

### ferund

At step 1 of an instance's stop
([control-plane.md](control-plane.md#stopping)), the ingress's side
stops: the nodes' port closes every new `ferun-ingress/1` connection
before any check, the Sync streams end with `UNAVAILABLE` "instance
stopping", and Wake and Hold answer `UNAVAILABLE`. The agents reconnect
to another instance and get a whole table there. Instances of a version
without the ingress do not know the ALPN and close the connection, and
send no `ferun_ingress`, which the builders' 5 s backstop covers.

## Kubernetes

On the Gardener shoot
([ferun-ingress.yaml](../../deploy/gardener/ferun-ingress/ferun-ingress.yaml),
[README](../../deploy/gardener/README.md#the-ingress)), in the namespace
`ferun-ingress`:

- **A StatefulSet of one instance per worker**, `ferun-ingress-0` to
  `-2` on the shoot's three, with the headless
  Service `ferun-ingress-pods`: a volume of 1 GiB per instance at
  `/var/lib/ferun-ingress`, so that the identity, the /96 and the cache
  survive restarts and no subnet is spent per pod; `Parallel` pod
  management, rolling one at a time with `minReadySeconds: 10`; never
  two instances on one worker (required pod anti-affinity) and none on a
  worker labelled `node.kubernetes.io/exclude-from-external-load-balancers`
  (the balancer below); a PodDisruptionBudget of `maxUnavailable: 1`
  that lets an unhealthy pod be evicted.
- **The container** runs `/ferun-ingress serve` from ferund's image as
  root with `NET_ADMIN` alone, a read-only root filesystem, no
  privilege escalation and the runtime's default seccomp profile
  (`RuntimeDefault`, set for the pod; [Privileges](#privileges));
  listeners on the pod's IP (8080, 8443, 9090), the
  mesh endpoint `$(POD_IP):51820`, `--control-plane` the ClusterIP of
  `ferund-nodes`, the join token file `/join/token` in a memory volume
  of the pod. Readiness `/readyz` every 2 s
  (2 failures), liveness `/healthz` every 10 s (6 failures); 100m CPU and
  64 MiB requested, 1.5 GiB at most, which `FERUN_INGRESS_MEMORY_LIMIT`
  passes on from the container's limit (`resourceFieldRef`), so that the
  Go runtime's soft limit is 90% of it; the default caps take at most
  about 950 MiB ([Limits](#limits)).
- **The init container** runs `/ferun-ingress prepare`, privileged and as
  root, since the image's user is not; a privileged container runs
  without a seccomp profile. It alone mounts the optional
  Secret `ferun-ingress-join` with every instance's token, and copies the
  pod's own, the key `$(POD_NAME)`, to `/join/token`: the instance, which
  faces the internet, never sees another's token, which would let it
  take that identity over with a join again. The copy is made at the
  pod's start, so a token put into the Secret later takes a deletion of
  the pod.
- **The certificate** `apps-wildcard`, `*.${APPS_ZONE}`, from
  cert-manager's DNS-01 issuer into the Secret `apps-wildcard-tls`; with
  a tenant zone, a second one, `*.<tenant zone>`, into a Secret of its
  own, mounted beside it and named by `--tenant-zone-tls-cert` and
  `--tenant-zone-tls-key` ([deploy/gardener/README.md](../../deploy/gardener/README.md)).
- **The balancer**, the Service `ferun-ingress` of type `LoadBalancer`
  (80 → 8080, 443 → 8443), with `externalTrafficPolicy: Local`: a
  worker passes a connection only to its own instance, without
  rewriting its source, so the instance sees the client's address and
  `X-Forwarded-For` carries it; `Cluster` would rewrite it to the
  worker's. IronCore's balancer is direct server return (the packet
  reaches the worker unchanged, the answer leaves it straight to the
  client), so it can add no PROXY protocol, and it checks no node's
  health: it sends every worker its share, which is why every worker
  runs an instance. A worker whose instance restarts drops its share of
  new connections until the new one is ready (about 20 s on 2026-10-29),
  which the clients' SYN retries turn into a delay.
- **Two NetworkPolicies.** `ferun-ingress` lets 8080 and 8443 in from
  anywhere, and the health port and UDP 51820 to 51835 (`endPort`, the
  ports a repair moves the device through) only from the workers'
  network (`NODE_CIDR`). `ferun-ingress-egress` lets the instances dial
  three things and nothing else:
  - DNS: the pods `k8s-app: kube-dns` of `kube-system`, on 53 and on
    8053, where Gardener's CoreDNS answers behind the Service's 53; the
    instance resolves names only for a `--control-plane` given by name;
  - TCP 7443 to the pods `app: ferund` of `ferun` and to `FERUN_NODES_IP`,
    the address of `ferund-nodes`;
  - UDP 51820 to the workers' network, their `feos-mesh`.

  Calico, with kube-proxy in iptables mode as on the shoot, checks a
  connection to a Service at the pod and port it was translated to,
  hence 8053 for DNS; for a network that checks the Service's own
  address, the rule for `ferund` names `FERUN_NODES_IP` too. Requests
  to the apps' pods go inside the mesh device, so the network sees only
  its UDP. An instance taken over through the proxy can therefore not
  reach the cluster's other services. The workers' network is not
  SNATed by Calico, so the pods and the workers' `feos-mesh` reach each
  other's endpoints.

A pod's name is its identity: never force-delete one, or two processes
with one key and address make WireGuard flap between their endpoints.

### Privileges

The agent and the proxy are one process, root with `NET_ADMIN`: the
code that parses the internet's TLS, HTTP/1.1, HTTP/2 and WebSockets
holds the right to configure the pod's network, and with it the
kernel's netfilter and traffic control, the usual way out of a
container. It also holds the instance's identity, `wg.key` and the
apps zone's key. The confinement around it:

- **Seccomp `RuntimeDefault`.** The agent configures the device with
  `socket(AF_NETLINK, SOCK_RAW)` for rtnetlink and generic netlink,
  `bind`, `getsockname`, `setsockopt` (`SOL_NETLINK`, `SO_RCVTIMEO`),
  `sendto`, `recvmsg` and `close`, and reads `disable_ipv6` under
  `/proc/sys` as a file: the profile lets every one of them pass, and
  so does everything else the program does (TCP, files, the Go
  runtime). It refuses `unshare` and `setns` without `CAP_SYS_ADMIN`,
  `bpf`, `keyctl` and `userfaultfd`. Only the tests in network
  namespaces (`internal/ingress/wg`) call `unshare` and `setns`, as root
  outside a container. The init container's `prepare` writes to
  `/proc/sys` and copies a file; it is privileged, which runs it
  without a profile.
- **The egress policy**: DNS, `ferund`'s nodes' port and the workers'
  WireGuard alone ([Kubernetes](#kubernetes)).
- No other capability, no privilege escalation, a read-only root
  filesystem, no service account token.

**The proxy apart from the privileges is the follow-up**, not built:
two containers in the pod's one network namespace. The agent, root
with `NET_ADMIN`, keeps the state directory (the identity, `wg.key`),
the connection to `ferund` and the device. The proxy runs as nonroot
with every capability dropped, mounts the wildcard certificate alone,
and gets the table, the nodes' handshakes and the answers to its wakes
and holds from the agent over a Unix socket in a volume of the pod; its
dials to the pods go through the device without a capability. It takes
a protocol between the two, readiness and status over both, and a
shutdown that keeps the agent until the proxy's requests are done.
A narrower first step moves only the device behind the socket: the
agent's `MeshDevice` (`Configure`, `SetPeers`, `Status`) served by a
small root container with `NET_ADMIN`, the agent and the proxy nonroot
without capabilities; that needs the state directory, the token copy
and the certificate readable by the nonroot user. Both are ADR 0021's
open question.

## Operations

- **Instances**: `GET /api/v1/admin/ingresses` lists them, removed ones
  included: name, status, joined and removed, mesh prefix, address,
  endpoint and public key, version, last seen and left, `connected` (seen
  within 30 s, on the database clock), `admittedVersion` and the
  certificate's expiry. `DELETE /api/v1/admin/ingresses/{name}` removes
  one (204, or 404): its certificates are revoked, its streams and
  connections closed on every instance, and it leaves the mesh's peers
  and `ferun-ingress`; a token pinned to its name joins it again, with its
  /96.
- **Join tokens**: `POST /api/v1/admin/join-tokens` takes `kind` (`node`,
  the default, or `ingress`) and `name` (`node` is its older alias; both
  given and different is 400), and answers `{id, token, expiresAt, kind,
  name, node}`; an ingress's token is `feos-ingress-join-<secret>.<pin>`,
  and an answer without `"kind": "ingress"`, or with a `feos-join-`
  token, comes from an instance that knows no kinds and is a node's
  token. `ferund join-token --kind ingress [--node <name>]` issues one
  from the command line.
- **Order of a deploy**: every instance of `ferund` on a version with the
  ingress before tokens are issued or an environment is published; then
  the ingress. A new image is `ferund`'s, so `ferund` rolls first.
- **A lost identity** (removed, or a certificate that expired while the
  instance was down: a renewed one has at least half of
  `--node-cert-ttl` left, 3.5 days) joins again with a new token pinned to
  its name in its join token file; on the shoot the token goes into
  `ferun-ingress-join` and the pod is deleted (never forced), so that its
  init container copies it. An instance never uses a token twice.
- **A stolen state directory**: remove the ingress (`DELETE`), then join
  it again with a new pinned token. The join again revokes every
  certificate, and the instance makes a new WireGuard key, while
  `ferund` refuses the old one, so the directory needs no wiping.

## Failure modes

| Event | What happens | Bound |
|---|---|---|
| A `ferund` instance crashes | The stream breaks (a reset, or keepalive after 15 + 5 s). The agent connects to another address or instance and gets a whole table, serving from memory meanwhile. Held requests ask again for a wake every 5 s; another engine takes the op over once its owner is dead. Holds stay valid and renew through the new instance. | stale for about 20 s; no request lost |
| `ferund` rolls | Stopping instances refuse new ingress connections and end their streams; older instances close the ALPN; the 5 s backstop covers their writes. | seconds |
| An ingress instance restarts or crashes | Kubernetes takes it out of the endpoints by readiness; its worker's share of new connections waits until it is ready again, since the balancer sends every worker its share and `Local` keeps a worker to its own instance (the clients' SYN retries bridge it), and the other instances serve the other workers' shares. Its identity and /96 persist; its new pod address reaches the nodes through Hello and `mesh-peers`. It serves the cache at once and is ready once the nodes' handshakes are fresh. A crash leaves its holds to lapse (30 s), and without `left_at` the fence keeps public services awake for up to 15 minutes. | its worker's share waits for it, about 20 s |
| A force-deleted ingress pod | Two processes may use one key and address, and WireGuard flaps between their endpoints. | until one dies |
| A database failover | The builders' reads fail and the snapshots stay; streams go on. After 30 s unhealthy the streams end and the agents try other instances. Join, Hello, renewal and Hold fail: holds lapse, but the fence starts only 45 s after `last_seen`. | the data path is unaffected |
| The ingress cut off from `ferund` | The cached table is served. No wakes: held requests time out with 503. Holds lapse after 30 s, but the fence keeps public services from idling for 15 minutes; after that, a request to a paused instance waits until its client gives up. | 15 minutes |
| The ingress cut off from a node | The node's handshake goes stale within 180 s and its endpoints become the last resort; until then a failed dial is retried elsewhere and the endpoint cools down for 5 s. A request with no other endpoint gets 502, then 503 while the endpoint cools down, and its client connection closes. Once stale, the path is repaired every minute ([Lost paths](#lost-paths)) until the node answers. Readiness keeps the instance while another node with endpoints is fresh. | 180 s of extra dials; a minute after the network is back |
| An outage leaves the flow to a node broken after the network is back (2026-11-05) | WireGuard's retries keep the broken flow alive, so it never heals by itself; the repair moves the device to a new port, a new flow, and sets the node's peer anew; the nodes get the new endpoint through Hello and `mesh-peers`. An instance that had no fresh node with endpoints is ready again with the first node it reaches. | a minute after the node went stale |
| `feosd` restarts on a node | It keeps the device, its key and port, and the sessions, and sets the peers it kept, which the device has already; an endpoint the device learnt that differs from the control plane's is set back. The instances notice nothing. A node whose device was made anew (after a reboot) has no session: an instance makes a new one 15 s after a packet the node did not answer, or at its rekey within 2 minutes. | none; 15 s after a reboot |
| A node that never took the ingress as a peer | No handshake: its endpoints are the last resort, and readiness ignores the node after 60 s when another node works. With endpoints, it is repaired every minute, and every repair's new endpoint makes `mesh-peers` send the node its peers again. | |
| `ferund` cut off from the database | That instance's builder turns unhealthy after 30 s and ends its streams. | 30 s |
| A new leader | No effect on the table or the data path; `mesh-peers`, `env-groups` and the idle loop pause for the election. | the lease's TTL |
| A replica stops | It leaves the table in about 100 ms and the idle pooled connections to it are closed; its pod goes after 10 s, which cuts longer requests; the ingress closes what is still open after 30 s. | |
| An environment is unpublished | New requests get 404; the open connections to its instances, upgraded ones included, are closed. | about 100 ms |
| An app accepts connections and never answers | Its requests wait for a connection 10 s at most; at most 256 of them are forwarded at once per instance (512 for all its tenant's environments), and the others get 503 at once. Other tenants keep their shares, and the instance's memory stays bounded. | 256 requests per instance |
| A service stays pending | Its requests are held up to the pending bound, at most 128 at once per instance (256 for all its tenant's environments); other tenants' requests are still held. | 128 per instance |
| Held or stuck requests with large bodies or headers | Each keeps at most 64 KiB of its body that came (HTTP/2's window) and 32 KiB of headers in 100 fields; trailers are refused. The default caps take at most about 950 MiB of the 1.5 GiB limit. | about 250 KiB per held, 350 KiB per forwarded request |
| A new replica becomes ready | It is an endpoint once its ports are recorded (the prepare hook puts its group before the pod exists) and its node is fresh and admitted. | about 100 ms |
| A new route on an unchanged service | Apply wakes `env-groups`; the port is in the table once admitted; meanwhile requests wait the pending bound. | one `env-groups` pass |
| The first publish | The image services roll once; until their new replicas are ready, requests wait 10 s, then get 503. | one rollout |
| A new ingress instance | Its address enters `ferun-ingress`; endpoints count only on nodes at that version; readiness waits. | one `env-groups` pass |
| An ingress removed | Its certificates are revoked, its connections closed, and it leaves the peers and the group; the agent declares its identity lost and fails readiness. | seconds |
| A stolen certificate | It cannot change the mesh key, and it is renewed only while it is the instance's newest certificate, for the instance's key, in its renew window: once the instance has renewed and said Hello, the copy lives at most until its own expiry. Before that, it can connect, wake and hold: the operator removes the ingress and joins it again, which revokes every certificate and changes the WireGuard key. | the copy's lifetime, or an operator's action |
| The wildcard renewed | cert-manager writes the Secret, the kubelet the files (about a minute), the ingress reads them within 30 s. | |
| A domain's certificate renewed | `ferund` keeps it and notifies its environment; the environment's entry, with a new hash, reaches every instance and is served from the next handshake on. | about 100 ms |
| An ACME order | Its challenge reaches every instance's port 80 through the table; `ferund` accepts it once every connected instance's stream has sent it, else the order fails without the CA validating. | about 100 ms |
| The ingress cut off from `ferund` during an order | Its stream records no token; the order fails within 30 s and backs off an hour. A cached instance still in the balancer cannot answer the new challenge: a known limit. | |
| An ingress of a version without domains behind a `ferund` with them | It routes a domain (it is in the hosts) with the wildcard, which the client refuses, and answers no challenge: roll the ingress before domains are bound. | |
| A service listening on `127.0.0.1` or `0.0.0.0` alone | It passes readiness through the port door; the ingress's dials are refused: 502 with the `[::]` hint. | a known limit |

## Tests

- **Unit tests**: the protocol (codec, `State.Apply`, splitting a table
  into parts and joining them, a table larger than a message, the
  service and client over an in-memory connection); WireGuard's messages
  against golden bytes, dumps, the peers' diff and RFC 7748's vector; the
  table's build, its classes, an environment too large left out, the
  diff and the builder against a database, the domains in an entry and
  their bound, the builder's domains and challenges; the Sync stream's
  challenges and the tokens it records; the
  proxy against `httptest` upstreams (routing, gating, holds, the wake
  bound, a client that goes away, draining, the body limit, retries, the
  idle timeout, cookies, the limits against an app that never answers,
  the shares per tenant and per environment, each wait for a connection,
  HTTP/2's streams, the memory of held HTTP/2 requests with large bodies,
  the header bounds, the connections cut when the table changes, a
  dial under way included, the certificate by SNI with a renewal and
  the wildcard, the port-80 answers); the agent
  against a fake control plane
  (`agent.NewFakeControlPlane`: join and pin, reconnects over the address
  list, renewal, refusals, the fixed key, a new key after every join and
  after a refused one, the cache at the start, a table in parts, wakes,
  holds, the mesh's handshakes); the repair of lost paths on a fake
  network that keeps broken flows broken (a node whose flow stopped
  being answered, an outage that broke every flow it carried, a node
  without endpoints, a forwarded port); the program's readiness, also
  its recovery after such an outage without a restart, shutdown and
  health listener, and `prepare`'s copy of the join token; the client
  connection closed with the answer to an instance that cannot be
  reached.
- **Database tests**: the ingresses (join, re-join, removal, the counter
  in commit order with two interleaved Hellos, the fixed key, the key a
  re-join retired, one key per peer across nodes and ingresses with two
  interleaved writers, supersession, the renewal's refusals, the mesh's
  peers, the connection check), public
  environments and their labels (reuse by the same tenant only, the
  fallback, the check constraint), the admission records and the
  notifications, holds and the fence.
- **The controller**: the first publish rolls once and puts `ing-` with
  its rule and `ferun-ingress` with its version and records both;
  unpublishing and publishing again roll nothing; routes added and
  removed; addons and jobs never name `ing-`; a removed ingress counts the
  version up; groups of deleted or moved sandboxes are deleted.
- **In network namespaces** (`make go-test-netns`, `FERUN_TEST_NETNS=1`,
  root): two WireGuard devices in two namespaces handshake and carry a
  request; peers set again unchanged keep their session; a device moved
  to another port keeps it, and the other side follows the new port; a
  peer set anew handshakes at once; a removed peer
  stops traffic.
- **End to end** (`internal/e2e`, `TestIngress…`): instances of `ferund`
  and test nodes on the mesh, and an in-process `ferun-ingress` with a
  fake mesh device and a dial hook that maps a pod's address to its
  server: the join and the mesh, publishing and routing (nothing served
  before), a rollout without a failed request, a wake of a paused
  service, holds against the idle loop, an outage of `ferund`, a move to
  another instance, and a removal.
- **Not testable before the live cluster**: `feosd`'s nftables to a real
  microVM, IronCore's and Calico's UDP and MTU, Calico's enforcement of
  the egress policy, the runtime's seccomp profile, the balancer's
  health checks, cert-manager, the volumes, and real apps that bind
  `[::]`.
