# ADR 0021: The ingress: a program of its own and a peer of the pod mesh

**Status:** Accepted · **Date:** 2026-10-27

Inbound HTTP traffic to public environments is served by `ferun-ingress`,
FeRun's second program: not a front door of `ferund`, not a role of the
node, but identical instances where the public addresses are, each a
peer of the pod mesh with a prefix of the nodes' counter. An instance
connects out to `ferund` as a node does, serves from a versioned table
`ferund` pushes and keeps serving it through an outage of the control
plane. It reaches the routed ports of image services over IPv6 through
the groups `ing-<id>` and `ferun-ingress` while the environment is public
(ADR 0012); sandboxes, attached services, addons and jobs stay behind
`ferund`'s doors (ADR 0018). It wakes idle services and holds their
requests, makes apps' cookies host-only, sets the forwarding headers and
drops a client's own, and writes every request as a line and a server
span of the service into the sink (ADR 0026). An L4 balancer with
`externalTrafficPolicy: Local` stands in front, one instance on every
worker it sends to. Hosts, domains and certificates are ADR 0045.

## Context

ADR 0001 names load balancers for HTTPS with ACME and the way in for IPv4
clients as the first thing CaaS needs. ADR 0015 keeps them out of
`ferund`: inbound traffic to services is not a front door of the control
plane. ADR 0010 asks about peers of the mesh that are not nodes. ADR 0018
has a route door and a port door of `ferund` for routes.

The doors are the right path for a sandbox, whose dev server binds the
loopback and whose pod no group opens, and the wrong path for a service
in production. A request through a door goes from `ferund` over the
serving instance's node connection as a `PortForward` stream, through
`feosd`, over vsock, to the guest's loopback: every byte crosses the
control plane and its node connections, which budget forward streams per
sandbox and long streams per node connection; the upstream speaks
HTTP/1.1 only, sees `127.0.0.1` as its peer and gets no `X-Forwarded-*`;
a rollout of `ferund` is a rollout of the data path. A product's
workbench in front of the door, one replica replaced with `Recreate`,
takes every environment host down while it deploys, and stands in the
data path of the applications it built.

The first application that goes to production this way is a dashboard a
FeCode project built, at a domain of its owner's, with DNS that can carry
an A record or a CNAME. It has to stay up while FeCode and `ferund`
roll, run in two replicas, and be reached over IPv4, because that is
what its users have.

The network gives the ingress what it needs. Every pod has an IPv6
address of its node's /96 of the cluster's ULA (ADR 0010); a
packet reaches it through its node's `forward` chain, where the node
admits or drops it by the source address the mesh vouches for
(ADR 0012); the node's own processes cannot reach pods over IP. `ferund`
knows every service's instances, their addresses and readiness, and runs
the idle policy that pauses a service nobody calls; a suspended service
is woken by whoever holds the request until the pod runs.

## Decision

### A program of its own, `ferun-ingress`

- The ingress is FeRun's second program, built from the Go module beside
  `ferund` and shipped in the same image. It serves the HTTP traffic of
  public environments' hosts and nothing else: no REST API, no MCP, no
  UI, no access to the database and no tenant's token. It runs as
  identical instances, as many as the deployment wants; an instance holds
  no state of its own, only a cache of what `ferund` sent it.
- It is not a front door of `ferund`: `ferund`'s instances carry no
  application traffic, a rollout of `ferund` cuts no request, and an
  outage of `ferund` stops no traffic, because the ingress serves from
  what it has. It is not a role of the node: nodes open no port but the
  mesh's (ADR 0001), and the ingress runs where public addresses are.
- An instance connects out to `ferund`, as a node does (ADR 0015), on the
  nodes' port under an ALPN of its own. It joins with a one-time token an
  operator issues, of a form of its own that a node's join refuses before
  any lookup; holds a certificate of the platform CA with the ID
  `spiffe://<trust domain>/ingress/<name>`; renews it over the same
  connection, for its own key, near its renew time, and only its newest
  certificate; and takes everything it serves from `ferund` on that
  connection. `ferund` opens nothing to it. One connection to one
  instance of `ferund` at a time: a failover is a reconnect and a whole
  table. An instance gives its identity up only when `ferund` says it was
  removed or refuses it for minutes, and joins again only with a token it
  has not tried. Each instance sees only its own join token.

### A peer of the mesh with a prefix of the nodes' counter

- Every instance has a WireGuard key pair of its own and an endpoint at
  which the nodes reach it. `ferund` gives it the next prefix of the
  nodes' counter, a /96 of the cluster's ULA that is never given out
  twice (ADR 0010), and the instance uses the prefix's first address.
  Nodes take it as an ordinary peer of a node's length, so `feosd`
  changes nothing; ID 0 stays reserved. A re-join keeps the prefix.
- `ferund` sends every node the instances as peers in the same table as
  the nodes, with the same versions and deltas, and every instance the
  nodes as its peers with their /96s. A packet from the ingress reaches a
  pod through the pod's node's `forward` chain like a packet from another
  node's pod: the node's rules decide (ADR 0012), the reverse path rule
  holds, and the flow is recorded (ADR 0013). An instance listens on
  nothing at its mesh address.
- The WireGuard key is fixed from the instance's first Hello after a
  join, so a stolen certificate cannot take its place on the mesh. Every
  join brings a new key, and a join again retires the key before it, so
  that a copy of the state directory does not stay on the mesh. A key is
  one peer's across nodes and ingress instances, and keys of small order
  are refused.
- On Kubernetes an instance is a pod with `NET_ADMIN` and a network
  namespace of its own, so that its WireGuard device and its route for
  the cluster's ULA never meet a node's on one host; its endpoint is its
  pod address, which `ferund` sends anew when the pod is replaced. On one
  machine, beside `feosd`, it runs in a network namespace of its own for
  the same reason.
- A WireGuard flow keeps its addresses and ports for good, so a flow an
  outage left broken in the network would last until the pod is
  replaced. An instance repairs the path to a node that runs endpoints
  and has had no handshake for a while: its device moves to another port,
  that node's peer is set anew, and a Hello tells `ferund` the new
  endpoint. Readiness needs one fresh node with endpoints, not all of
  them: the balancer checks no worker's health, so an instance that is
  not ready drops its worker's share of every service, and a node whose
  own network is down must not take every instance out. An instance that
  cannot reach a node answers that node's services 502 or 503 and closes
  the client's connection, whose next one the balancer likely sends to
  another worker.

### What the ingress reaches

- The ingress reaches the routed ports of an environment's image
  services, and nothing else. A pod of such a service names its group
  `ing-<sandbox id>` once its environment has been public; its first
  publish rolls the services once. While the environment is public, the
  group's one rule allows TCP on the ports the service's routes use (in
  the deployed and the latest deployment) from `ferun-ingress`, whose
  members are the instances' addresses (ADR 0012). Route
  changes and publishing change rules, not pods. The groups are the
  boundary, the table the routing.
- An instance sends to an endpoint only when its node holds the `ing-`
  group with that port, holds `ferun-ingress` at a version that contains
  the instance, and has a WireGuard handshake with the instance younger
  than the bound. The last condition catches a node that never took the
  instance as a peer.
- An image service binds `[::]` (ADR 0010); a program that binds
  `127.0.0.1` or `0.0.0.0` alone cannot be reached over IPv6, and the
  ingress answers 502 with that hint. An attached service runs in a
  sandbox, on its loopback, and no address of the mesh reaches it: routes
  to attached services, and every route of an environment that is not
  public, are the route door's (ADR 0043), which takes the environment's
  route tokens. FeCode's previews stay behind the doors and behind
  FeCode's forwarder, where their authorization belongs.
- An environment is public by its tenant's decision, made by one call.
  The ingress authorizes nothing: what a route of a public environment
  names is public, except that a route's `from` blocks refuse every other
  client with 403 (ADR 0012). Without an apps zone (ADR 0045) nothing
  can be made public.

### The table: what an instance serves from

- `ferund` sends every instance one table, whole at connect and then as
  deltas with versions, as it sends groups and name tables to nodes: for
  every host its environment and certificate; for every environment its
  routes (path, service, port, `strip`, the clients' blocks, which door
  serves it); for every routed service its instances with their
  addresses, their admitted ports and whether they are ready, asleep,
  waking or pending; the ACME challenges. The zones' wildcards are not in
  the table: they are the operator's, given to the instances as files
  (ADR 0045). Every instance of `ferund` builds the whole table from the
  database, so any instance serves any ingress.
- An instance serves from the table and from nothing else: a host that
  is not in it gets 404 under the zone's wildcard. It keeps the last
  table and its certificates in its state directory and serves them
  through an outage of `ferund` and a restart of its own, as the nodes
  keep their peers and groups: what was routable stays routable while the
  control plane is away, and nothing new becomes so. Nothing expires it.
- The table is bounded per environment, not in total, so that no tenant
  can make it too large for every instance: an environment whose entry
  would exceed the bound is left out; a large table goes in parts, which
  an instance applies together.
- A change of the table cuts the connections to what is no longer
  routed, upgraded ones included: at once when the environment, its
  service or the instance's port is no longer routed, so that
  unpublishing ends every way in; after a short delay when only the
  instance left a service that stays routed, so that a retiring replica's
  requests end with its pod. The nodes keep established flows after their
  rules shrink, so the ingress closes them itself.

### Requests

- An instance terminates TLS with the host's certificate by SNI (a
  domain's from the table, else the zone's wildcard), takes HTTP/1.1 and
  HTTP/2 from clients, and speaks HTTP/1.1 to the service over pooled
  connections. Upgrades and WebSockets pass; answers stream unbuffered.
  It sets `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Host` and
  `X-Real-IP` from the connection and drops a client's own, `Forwarded`
  and `True-Client-IP` too, and keeps the `Host`: an application behind
  the ingress sees who called it. Plain HTTP on port 80 answers ACME
  challenges and redirects everything else to HTTPS.
- Every request is a server span of the ingress in W3C Trace Context: a
  valid `traceparent` the client sent is continued (its trace, its span as
  the parent, its `tracestate`), else the ingress starts a sampled trace;
  the service gets `traceparent` with the ingress's span as its parent.
  No tracing SDK: the span is the request's access log record.
- A request goes to a usable instance of the route's service, round robin
  per ingress instance. A connection that cannot be dialled is tried once
  on another instance for every method; one that fails after the request
  was written only for GET, HEAD and OPTIONS. An environment whose latest
  deployment has failed is served by the instances of the deployed one
  (ADR 0018); the table says which are ready.
- **Waking.** When a routed service has no usable instance because its
  pods are asleep or waking, the ingress asks `ferund` on its connection
  to wake the service and holds the request until the table shows one,
  for a bound, then answers 503 with `Retry-After`; a service whose
  instances are only pending is held for a shorter bound. The wake is
  `ferund`'s, as the door's is, and the policy that suspended the pod
  stays where it is.
- **Activity is a hold, not a touch.** For a service with an idle policy
  the instance holds the pod at `ferund` as the port door does, admitted
  before the first request and renewed while requests or connections are
  open, so that an idle pause never freezes a request. While an instance
  has lost `ferund`, idle pauses of public environments' services are
  fenced off for a while. Pod-to-pod traffic counts as before: not at
  all.
- **One tenant cannot take an instance from the others.** Forwarded
  requests, from the pick of an endpoint to the end of the answer or of
  the upgraded connection, have a share per tenant, a share per
  environment within it, and a cap per instance that fits its memory
  limit; held requests have the same shares beside their cap; beyond, the
  answer is 503. Each wait for a connection to an instance is bounded,
  the answer's time is not. An HTTP/2 connection carries a bounded number
  of streams, and what a waiting request keeps (headers, the body's
  window) is bounded too, so that the caps bound memory.
- **Cookies are host-only under the zones.** An app under the apps zone
  or the tenant zone could set cookies for the zone's parent, which reach
  the platform's hosts (cookie tossing, login CSRF), or for the other
  apps. On a host under a zone the ingress removes from every
  `Set-Cookie` of an answer each `Domain` attribute that names anything
  but the request's host. Cookies a page's script sets are not stopped;
  the platform's hosts guard against those with `__Host-` cookies.

### The access log and the traffic

- For every request routed to an environment the instance keeps a
  record: host, method, path without its query (which often carries
  tokens), status, outcome, bytes, duration, time held, the client's
  address, the service and instance it went to, and the span's IDs. It
  writes the record into the sink as a line of the service's streams and
  as the request's server span, with its own certificate (ADR 0026);
  `ferund` keeps the API that reads an environment's access log and reads
  the sink alone. A request that reaches no environment is counted, not
  recorded. The buffer is bounded; what overflows is dropped and counted.
- An environment's traffic per service, requests, errors and latency
  over a window, is aggregated in the sink from spans: the service's own
  server spans where it sends them, else the ingress's. The numbers are
  best effort: the ingress sends a batch of spans once.

### What stands in front

- On Kubernetes a Service of type `LoadBalancer` for ports 80 and 443
  with `externalTrafficPolicy: Local` gives the platform a public address
  beside the cluster's own gateway, which goes on serving the platform's
  hosts. `Local` keeps the client's address, so applications see it;
  with `Cluster` they would see a worker's. The cluster's gateway is not
  in the path: it would own the certificates, or pass TLS through by SNI
  and add a hop and a second place where routes are configured. On bare
  metal the instances' own addresses stand in DNS, or an L4 balancer
  passes TCP to them.
- Behind a balancer that checks no worker's health and sends every
  worker its share, an instance runs on every worker the balancer sends
  to and never two on one; a worker excluded from the balancer carries
  none. While one restarts, its worker's share waits, which clients' SYN
  retries bridge.
- The ingress is a StatefulSet: a volume per instance; root with
  `NET_ADMIN` alone under the runtime's default seccomp profile; a
  privileged init container that enables IPv6 and copies the pod's own
  join token; network policies that keep the health port and WireGuard to
  the nodes and let the instances dial only DNS, `ferund`'s nodes' port,
  the sink and the nodes' WireGuard, so that an instance taken over
  through its proxy reaches nothing else in the cluster.
- Clients reach the ingress over IPv4, and over IPv6 where the balancer
  has it; pods' addresses are never public. The ingress is the way in
  for IPv4 clients that ADR 0001 asked for.

## Alternatives considered

- **The route door as the ingress**, `ferund`'s instances the data
  plane, with hosts and certificates added to them. Every byte through
  the control plane's node connections and their budgets, HTTP/1.1
  alone, no client address, and a rollout of the control plane in every
  request's path. ADR 0015 kept the ingress out of `ferund` for this.
- **The ingress on every node**, a program beside `feosd` that uses the
  node's mesh device and reaches its own pods over their TAPs. No new
  kind of peer, but every node a public listener, an `output` chain and
  `input` rules for the node's own packets, every domain's key on every
  node, and an edge tied to the node's drains and rollouts. A fleet of
  many nodes wants an edge pool of its own, which may run on nodes where
  that is all there is.
- **The node's vsock door from a sidecar**: the same-node pods alone, no
  groups and no records, and vsock's limits.
- **Kubernetes's ingress**, the cluster's gateway with cert-manager and
  an `HTTPRoute` and a listener per domain that `ferund` writes, as the
  platform's own hosts have. The routes and certificates of the platform
  as Kubernetes objects, a second owner of the edge, and a platform that
  runs without Kubernetes (ADR 0001) tied to it on the one path every
  customer uses.
- **A provider's edge** (a tunnel to a CDN) in front of the doors: a
  dependency on one provider for every tenant's traffic; a tenant may
  put one in front of its domain by itself.
- **WireGuard in userspace** in the ingress process, without
  `NET_ADMIN`: every packet through userspace and a dependency of its
  own; the kernel's device under a capability is what the nodes do.
- **Inbound IPv4 to pods**, a public IPv4 per service: ADR 0010 made
  IPv4 the family for leaving; the ingress is the way in.
- **Waking a pod by a packet** (ADR 0010's question): the ingress holds
  the request and knows what it waits for; a wake on a packet would need
  the node to hold connections for a pod that is not there.
- **Authorization at the ingress for previews**, so that FeCode leaves
  the data path altogether: FeCode keeps it in its forwarder, which knows
  the person; previews are a workbench's, not a customer's.
- **Addresses of subnet 0 as /128s**, the ingress block one prefix in
  every routed service's group, as the draft had it: a new kind of peer
  for `feosd`, and a group ID longer than a pod's groups allow. A /96 of
  the nodes' counter makes an instance an ordinary peer.
- **A group `in-<environment>-<service>`** per routed service: a pod
  names its groups at its creation, and the ID exceeds `feosd`'s bound;
  a group per pod named at the first publish needs no second roll.
- **Activity as a touch every ten seconds**, in place of the door's hold:
  an idle pause could freeze a request between touches; a hold admitted
  before the first request cannot.
- **`http` traffic records and an access log in `ferund`'s database**,
  sent over the instance's connection and kept with the node's traffic
  records: the decision until 2026-10-31, replaced by lines and spans in
  the sink (ADR 0026), because records of an application belong beside
  its logs and the control plane should carry no write path of that
  load.
- **`unrecognized_name` for a host not in the table**: the zone's
  wildcard answers every name under it anyway, and a 404 tells the
  caller more than a TLS alert.

## Consequences

- FeRun has a second program and a second kind of peer: a node's mesh
  peers may be an ingress instance with a /96 of its own, and nothing
  else of the node changes. `ferund` joins, identifies and feeds the
  instances as it does nodes.
- The route door stays, for sandboxes, attached services and
  environments that are not public, with its route tokens (ADR 0018):
  routes to image services of public environments are realised by the
  ingress, every other route by the door. The doors keep vsock.
- An application behind the ingress sees the client's address and
  protocol, may speak HTTP/2 to its callers, and is reached while FeCode
  deploys and `ferund` rolls. FeCode's forwarder stops serving stage,
  production and demo; its previews stay as they are.
- The reference deployment runs three instances of `ferund` with
  anti-affinity over a replicated PostgreSQL, an instance of the ingress
  on every worker its balancer sends to, and a second `LoadBalancer`.
  Operators publish the balancer's address and open port 80 to the
  ingress for HTTP-01 (ADR 0045).
- One process parses the internet's requests and holds `NET_ADMIN` as
  root, confined by the seccomp profile and the egress policy. An
  ingress machine is as trusted as a node, which holds every pod's
  memory and, here, every domain's key.
- The ingress serves HTTP alone, speaks HTTP/1.1 to services, and has no
  authorization and no rate limits of its own beyond the shares per
  tenant and environment.

## History

Proposed on 2026-10-27 as the answer to the open questions of ADR 0001,
0010, 0015, 0017 and 0018 on inbound traffic, and built from 2026-10-28
(commit 33ea94e) with a /96 of the nodes' counter and the groups `ing-`
and `ferun-ingress` in place of the draft's subnet 0 and
`in-<environment>-<service>`; live on the Gardener shoot behind a
balancer with `externalTrafficPolicy: Local` since 2026-10-29 (b928852).
The access log went into `ferund`'s database on 2026-10-29 (cc1c6be) and
into the sink as lines and spans on 2026-10-31 (8000c13, 371279d,
07cc707); the traffic per service followed on 2026-11-02 (3970bd2) and
reads the services' own spans since 2026-11-07 (6dfd8ff). Lost mesh paths
are repaired since 2026-11-05 (8653b98); a route's `from` is checked
since 2026-11-08 (d35d5cb, ADR 0034). Hosts, domains and certificates
were split out into ADR 0045 on 2026-11-08.
