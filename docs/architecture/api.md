The API
=======

The `feos.*` gRPC APIs are the contract between the node and the control
plane, and between `feosd` and `feos-guest`. They live in
`proto/feos/<api>/v1/`, in the package `feos.<api>.v1`. One API is not in
`proto/`: the ingress's, between `ferund` and `ferun-ingress`
([below](#the-ingresss-api)).

## Packages

| Package | What it is for | Served by today |
|---|---|---|
| `feos.pod.v1` | pods: create, start, stop, delete, suspend, resume, hibernate, restore, console, events; the clear part of a pod's HTTP bindings (`PodSpec.http_bindings`, `SetPodHttpBindings`, [ADR 0028](../adr/0028-http-bindings.md)) | `feosd` |
| `feos.volume.v1` | persistent volumes: create, delete, get, list; a pod's spec names them ([node.md](node.md#volumes)) | `feosd` |
| `feos.disk.v1` | the pods' disks and memory over a chunk store and the object store, internal to the node ([disk.md](disk.md)) | `diskd` |
| `feos.network.v1` | the pods' security groups (`SecurityGroupService`), their name tables (`NameService`), the records of their traffic (`TrafficService`) and the node's mesh (`MeshService`) | `feosd` |
| `feos.fleet.v1` | the control plane's API for nodes, on the uplink: join, the node's certificate, telemetry, events, pods' credentials and their sealed bindings (Kubernetes, SSH, HTTP and the interception CAs, [control-plane.md](control-plane.md#http-bindings)), the node's place on the mesh ([control-plane.md](control-plane.md#nodes)) | `ferund` |
| `feos.microvm.v1` | the pods' microVMs, internal to `feosd`, served on no listener | – |
| `feos.container.v1` | containers: create, start, stop, delete, exec, logs, events | `feosd`, `feos-guest` |
| `feos.image.v1` | OCI image pulls and the image store | `feosd`, `feos-guest` |
| `feos.host.v1` | the machine: hostname, memory, CPU, network, version, logs, power, upgrade; and, for now, the guest's pushed time and network, freezing and `PreparePause` | `feosd`, `feos-guest` |
| `feos.task.v1` | the container runtime, internal between the main process and the task service | both, internally |
| `feos.lifecycle.v1` | the guest's uplink: `Attach` is held open while the guest runs | `feosd` (server), `feos-guest` (client) |
| `feos.guest.v1` | what only a guest does for its host: `MountVolumes` ([guest.md](guest.md#volumes)) | `feos-guest` |
| `feos.tool.v1` | an agent's file and web tools on a container's files: read, write, edit, ls, rg, fd, webfetch | `feos-guest` |
| `feos.watch.v1` | inotify subscriptions on a container's files | `feos-guest` |
| `feos.portforward.v1` | streams to ports inside the guest | `feos-guest` |
| `feos.kube.v1`, `feos.ssh.v1` | the listeners for Kubernetes and SSH credential offloading | `feos-guest` |

Decided, not built:

- `feos.guest.v1.GuestService`, which has `MountVolumes`, takes
  `SetTime`, `GetTime`, `AdjustTime`, `SetNetworkConfig`, `GetNetworkConfig`,
  `FreezeFilesystem`, `ThawFilesystem` and `PreparePause` out of
  `HostService`; only `feos-guest` serves it
  ([ADR 0007](../adr/0007-guest.md)).

## Listeners

On a node (`feosd`):

| Listener | Services |
|---|---|
| `/run/feos/api.sock`, and TCP with `--tcp` | Pod, Volume, SecurityGroup, Traffic, Container, Host, Image; and a pod's Container, Image, Tool and Watch |
| `/run/feos/image.sock` | Image, internal |
| `/run/feos/task.sock` | Task, internal, in the task service process |
| `<vm dir>/v.sock_10001`, per VM | Lifecycle, for the guest's uplink |

In a pod (`feos-guest`):

| Listener | Services |
|---|---|
| vsock port 10000, `/run/feos/api.sock` | Container, Host, Image, Tool, Watch, PortForward, Kube, SSH, Guest |
| `/run/feos/image.sock`, `/run/feos/task.sock` | Image and Task, internal |

The guest dials the host on vsock port 10001 (the uplink) and pipes
Kubernetes and SSH connections to vsock ports 10002 and 10003.

A call to `feosd` that carries the metadata `feos-pod-id` goes unchanged
to that pod's guest, on vsock port 10000, if it is for the guest's
Container, Image, Tool, Watch or PortForward service and the pod is pending or running
([ADR 0004](../adr/0004-pods.md)); for any other service it fails with
`UNIMPLEMENTED`. `feosctl` sends it with `--pod <id>`.

Every public listener serves the same services. TCP has no
authentication; `feos-host-init` starts `feosd` with `--tcp [::]:1337`.
With `--control-plane`, `feosd` serves them on the node connections it
opens to the control plane too ([ADR 0015](../adr/0015-control-plane.md),
[node.md](node.md#the-control-plane)).

## The pods' network

A pod's network ([ADR 0010](../adr/0010-pod-network.md),
[node.md](node.md#the-pods-network)) is in its spec and its record:

- `PodSpec.network` (`PodNetwork`): `none` for no network at all;
  `security_groups`, the IDs of the pod's security groups, the node's
  default group when empty; `dns_upstreams`, the IP addresses of the
  resolvers that `feosd`'s resolver asks for the pod, the node's when
  empty; `name_tables`, the IDs of the name tables whose names the
  resolver answers the pod itself, in order; `restrict_egress_names`
  and `egress_domains` (ADR 0034, revised), the pod's egress by name:
  with the flag the resolver answers upstream names only when they are
  exact names of `egress_domains` or lie strictly below a `*.` suffix of
  it, others REFUSED (and recorded with that rcode), and puts every
  A/AAAA address of an allowed answer into the pod's egress set for
  max(TTL, 300 s), which its out chain admits; the flag without domains
  is no upstream name at all (egress `none`). Names of its tables and its
  HTTP bindings are answered either way; without the flag nothing
  changes. A create with an invalid
  group or table ID, a table named twice, or an upstream the pod may not
  use fails with `INVALID_ARGUMENT`; one with a network on a node without
  `--pod-prefix`, or whose network is not set up, fails with
  `FAILED_PRECONDITION`.
- `PodSpec.metadata`: keys and values that the pod reads from its
  metadata service at `169.254.169.254`, beside `feosd`'s own, the pod's
  ID and addresses ([node.md](node.md#metadata)); no credentials, which
  anything in the pod could read. A create with a key other than 1 to
  128 letters, digits, `-`, `_` and `.`, with more than 32 KiB as JSON,
  or with metadata and no network fails with `INVALID_ARGUMENT`.
  `feosctl pod create --metadata KEY=value`.
- `PodInfo.ipv6_address`: the pod's address on the network, of the
  node's prefix, while it is on the node. `PodInfo.ipv4_address`: its
  IPv4 address on the node, of `--pod-ipv4-range`, for leaving through
  masquerade; another pod may have it once the pod is deleted.

`feos.network.v1` is the node's side of ADR 0012 and ADR 0013:

- `SecurityGroupService`: `PutSecurityGroup` replaces a group whole;
  `UpdateSecurityGroup` adds and removes members from `from_version` to
  `version`, and fails with `FAILED_PRECONDITION` unless the node has the
  group at `from_version`; `DeleteSecurityGroup` fails with
  `FAILED_PRECONDITION` while a pod names the group; `GetSecurityGroup`
  and `ListSecurityGroups`. A rule is a direction, a protocol (TCP, UDP,
  ICMP, ICMPv6 or any), ports of TCP and UDP, and a peer: an address block
  with exceptions, of either family, or a group's members, of IPv6. An
  IPv4 block counts in rules out alone: nothing new comes in over IPv4. A
  change applies to the pods' rules within moments of the call.
- `NameService` (ADR 0018): `PutNameTable` replaces a table whole: its
  names, each one label of a DNS name with one or more addresses of
  either family, and the tables it includes; `UpdateNameTable` puts and
  removes names from `from_version` to another `version`, and fails with
  `FAILED_PRECONDITION` unless the node has the table at `from_version`;
  `DeleteNameTable` fails with `FAILED_PRECONDITION` while a pod names
  the table; `GetNameTable` and `ListNameTables`. A change answers the
  pods' next queries ([node.md](node.md#name-tables)). `feosctl
  name-table`.
- `MeshService` (ADR 0017): `PutMeshPeers` replaces the node's peers on
  the mesh whole, at a version; `UpdateMeshPeers` puts and removes peers
  from `from_version` to `version`, and fails with `FAILED_PRECONDITION`
  unless the node has them at `from_version`; both fail with
  `FAILED_PRECONDITION` on a node that is not on a mesh or has no /96 of
  it yet. A peer is a node's name, its WireGuard public key, its endpoint
  and its /96. `GetMesh` shows the node's ULA, /96, key, port, version and
  peers ([node.md](node.md#the-mesh)).
- `TrafficService.StreamTrafficRecords` streams flows, drops, DNS queries
  and server names as `feosd` makes them, of one pod or of all; what a
  slow call misses is counted in the next record's `missed`.

The guest's network configuration (`SetNetworkConfigRequest`,
`GetNetworkConfigResponse`, `feos.microvm.v1.GuestNetworkConfig`) gained
`ipv6_cidr` and `ipv6_gateway` by addition, and `routes` after them;
`feosd` pushes both families: the IPv6 /128 via `fe80::1`, the IPv4 /32
via `169.254.0.1`, and its resolver, and for a pod whose IPv6 address is a
ULA a route for `fc00::/7` in place of the default route over IPv6
([guest.md](guest.md)). An older guest ignores the routes and keeps its
default route.

## The ingress's API

`ferun.ingress.v1.IngressService` is what `ferun-ingress` calls on
`ferund` ([ADR 0021](../adr/0021-ingress.md),
[ingress.md](ingress.md#identity-and-connection)): gRPC on the nodes'
port under its own ALPN, `ferun-ingress/1`, with mTLS. Its messages are
Go structs coded as JSON, and its service descriptor is written by hand,
in `go/internal/ingress/wire`, which both programs import: the build has
no `protoc`, and a protobuf service would need a generator for every
change. Both ends force the codec `json`, so every call carries the
content type `application/grpc+json`. Method names are
`/ferun.ingress.v1.IngressService/<Method>`; `ferund` serves it,
`ferund` never calls the ingress.

| Method | Server | Request → answer |
|---|---|---|
| `Join` | the ingress join server, without a client certificate | `{token, name, csr}` → `{name, certificate}` |
| `RenewCertificate` | the ingress server, with the instance's certificate | `{csr}` → `{certificate}` |
| `Hello` | the ingress server | `{publicKey, endpoint, version, capabilities?}` → `{name, podNetwork, prefix, address, appsZone, tenantZone?, instance}` |
| `Sync` | the ingress server, streaming | `{}` → a stream of `Table` |
| `Wake` | the ingress server | `{env, service}` → `{waking, retryAfterSeconds}` |
| `Hold` | the ingress server | `{hold, release, leaving}` → `{held, refused: [{instance, reason}], leaseSeconds}` |
| `RedeemVisit` | the ingress server | `{code, binding, visitBinding, host}` → `{outcome: "admitted", token}` or `{outcome: "refused", reason: "used" \| "expired" \| "ended" \| "unknown"}` |
| `RenewVisit` | the ingress server | `{token, secret}` → `{outcome: "renewed", token}` or `{outcome: "ended", reason}` |
| `EndVisit` | the ingress server | `{token, secret}` → `{}` |

- **Encoding.** JSON names are camelCase, and nothing is HTML-escaped;
  `[]byte` is base64, a time RFC 3339 with nanoseconds. A certificate is
  `{spiffeId, chain, trustBundle, notBefore, notAfter, renewAfter}`, as
  `feos.fleet.v1`'s: the chain without the root, the issued certificate
  first, and the bundle's roots, DER each.
- **The table.** `Table{seq, whole, more, envs, removed, mesh, self,
  challenges, sentAt}`: `seq` counts from 1 per stream, one per message;
  `whole` replaces the state; otherwise `envs` are put by ID, `removed`
  are deleted, and a `mesh` or `self` present replaces the one before
  (absent: unchanged). `challenges`, `{items: [{token, host,
  keyAuth}]}`, present, are every pending ACME HTTP-01 answer and
  replace the ones before; absent, a delta keeps them and a whole table
  leaves none. An empty table is the heartbeat. A table that takes more
  than 4 MiB comes in parts: consecutive messages, every one but the
  last with `more`, the first with `whole`, `mesh`, `self` and
  `challenges`, the `envs` and then the `removed` spread over them; the
  receiver applies them together once the last has arrived, and nothing
  comes between them. A message takes at most 32 MiB, an environment at
  most 2 MiB (a larger one is left out of the table; domains that would
  make it larger are left out of it). An `Env` is `{id, tenant, name,
  hosts, deployment, hash, routes: [{name, path, service, port, strip,
  door}], services: [{name, idle, endpoints: [{instance, address, node,
  ports, groupVersion}], asleep, waking, pending}], domains: [{name,
  chain, key}]}`: `domains`, absent without any, are the hosts with a
  certificate of their own, each its chain (DER, leaf first) and private
  key (PKCS #8 DER), and each name is in `hosts` too; `Mesh` is
  `{version, peers: [{name, publicKey, endpoint, prefix}]}`, the nodes
  alone; `Self` is `{name, address, appsZone, tenantZone?, admittedVersion}` (`tenantZone`, ADR 0031, left out without one). The
  receiver keeps the state (`wire.State`, `Apply`): a delta or a part
  whose `seq` is not the next ends the stream, and the ingress connects
  again.
- **Capabilities.** Hello's `capabilities` are what an ingress does
  beyond what the fields' zero values say; an older one names none. With
  `visits` it checks visits ([ingress.md](ingress.md#visitors), ADR
  0054): `ferund` gives an environment whose `access` is set only to an
  ingress whose Hello named it, and leaves it out of every other stream.
- **Visits** ([control-plane.md](control-plane.md#visitors)). An `Env`
  gains `access` (`""` anyone, `visitors`; any other value admits
  nobody), `provider` (the name a token's `prv` must equal) and `signIn`
  (where a navigation without a visit is sent, with `challenge=<sealed>`
  added to its query; `""` with `visitors`: nobody can sign in); without
  them it codes as before, its hash unchanged. `Table` and the state gain
  `visitKeys`, `{tokenKid, tokenKey, challengeKid, challengeKey}`: the
  Ed25519 public key of tokens and codes and the AES-256 key of
  challenges with their IDs, the same for every instance; present, it
  replaces the keys before, absent, a delta keeps them and a whole table
  has none; it travels in a table's first part. `RedeemVisit` redeems an
  accepted code a browser brought to `host` under `/.ferun/auth`, with the
  secret of that browser's challenge cookie (`binding`) and the hash of
  the visit secret the ingress made for its cookie (`visitBinding`, the
  token's `vbnd`). `RenewVisit` asks for a visit's next token with the
  visit cookie's secret; `ended` carries an end reason, or `unknown`: the
  cookie holds no visit, though the visit may live on for another.
  `EndVisit` ends a visit at a sign-out, with the cookie's secret; it
  answers `{}` also when nothing ended. Every error is no answer:
  `INVALID_ARGUMENT` for a malformed request, `UNAVAILABLE` for any
  failure, and an older `ferund` answers `UNIMPLEMENTED`, all of which the
  ingress takes for `ferund` being away, never for a visit's end.
- **The access log** is not on this service: the ingress writes it into
  the sink ([sink.md](sink.md#the-ingresss-access-log)). The method
  `PublishAccessLogs`, which carried it to `ferund` before, was removed
  on 2026-10-31; an ingress of before that calls it gets
  `UNIMPLEMENTED`.
- **Errors.** A refused connection answers every call
  `PERMISSION_DENIED` with the refusal's text, by which the ingress tells
  a removal from other refusals ([ingress.md](ingress.md#refusals)).
  `UNAUTHENTICATED` without an ingress certificate; `FAILED_PRECONDITION`
  for a Sync before Hello on its connection, a Hello on a cluster that is
  not meshed, and a Hello with another WireGuard key than the one fixed;
  `UNAVAILABLE` from a stopping instance, an instance whose table is not
  ready or cannot read the database, and on database errors;
  `NOT_FOUND` for a Wake of an environment that is not public or a
  service that is not routed; `INVALID_ARGUMENT` for a Hold list of more
  than 10 000 IDs. A Hold refuses an ID by itself, with the reason
  `gone`, `not public`, `not running`, `busy` or `invalid`.
- **Compatibility** follows protobuf's rules, kept by hand: a field is
  added, never renamed, retyped or reused, and its JSON name never
  changes; a receiver ignores fields it does not know and reads a
  missing one as its zero value, so a new field means "as before" at
  that value. A method is added, never changed; an end without it
  answers `UNIMPLEMENTED`.

## Compatibility

- **The guest's API only grows.** A suspended pod keeps the `feos-guest`
  it was suspended with, so a newer `feosd` talks to older guests. Within
  v1, fields and RPCs are added, never renumbered, removed or given another
  meaning. This holds for `feos.lifecycle.v1` and every package
  `feos-guest` serves.
- **The node's API** is what the control plane calls. It follows the same
  rule within v1; a break is a new version of the package.
- **Internal APIs** (`feos.task.v1`) change freely: both ends are one
  binary.
