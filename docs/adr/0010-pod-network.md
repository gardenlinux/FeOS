# ADR 0010: The pod network: IPv6 per pod, routed or meshed, IPv4 for leaving

**Status:** Accepted · **Date:** 2026-10-15

Every pod has an IPv6 address, its address on the network, unique across
nodes and never handed out twice: an address of a prefix that the
network routes to the node, or of the node's /96 of its cluster's ULA
/64, which a WireGuard mesh between the nodes carries. Every pod also
has a link-local IPv4 address, unique on its node alone, for leaving
through NAT on the node; a pod with a ULA address has no default route
over IPv6 and leaves over IPv4 at once. Each pod's TAP device is a
routed link of its own, pinned to the pod's addresses; `feosd` serves
DNS to its pods on every node, Firecracker's MMDS serves their metadata,
and a list of prefixes, the node's own addresses and the link-local,
loopback and multicast ranges are reached by no pod, whatever its
reachability allows.

## Context

Pods are untrusted workloads (ADR 0001). Agent sandboxes reach the
internet, and their users reach the ports of their sandboxes. Services
take traffic from outside and talk to each other, a service to its
database, across nodes.

Reaching pods across nodes takes addresses that are unique across nodes
and a prefix per node that the network routes to it. IPv4 makes both
scarce: a prefix per node bounds how many pods a node holds, suspended
ones included, and an address that is handed out again inherits what
other pods allowed its old owner until every node has heard of the
change (ADR 0012). IPv6 has room never to hand out an address twice.

In an agent's workspace runs whatever tool the agent picks, and much of
it breaks without IPv4: IPv4 literals, programs that speak only IPv4,
resolvers of their own or over HTTPS, and DNSSEC validation, which
synthesized records fail.

Pods resume on other nodes (ADR 0001). An address that followed a pod
from node to node would need a route per pod, changed with every move.

Many networks route no prefix to a node: a cloud that gives a VM one
IPv6 address or none, a lab whose VMs have IPv4 alone. Whatever carries
pods' packets between such nodes carries their source addresses, and
reachability is decided by them (ADR 0012): it must let nothing but a
pod's own node put the pod's address on a packet. The prefixes such
nodes would take by hand are not random, as RFC 4193 wants a ULA to be,
so every lab that copies a scheme has the same addresses.

A pod with a ULA address and a default route over IPv6 tries IPv6 first
for every name with addresses of both families (RFC 6724), and the node
answers destination unreachable, which Linux sends at most once a
second. Measured in the lab: curl and Node.js, which try both families
at once (RFC 8305), lost 200 to 250 ms per connection; Python, which
tries one address after the other, lost up to a second per IPv6 address,
and `pip download requests` took 6.8 seconds.

## Decision

### Two families, two roles

- A pod has an IPv6 address and an IPv4 address. IPv6 is its address on
  the network: unique across nodes, routed, and reachable where its
  reachability allows (ADR 0012). IPv4 is for leaving only; nothing
  allows by a pod's IPv4 address.
- Where a name has addresses of both families, programs take IPv6, since
  the pod's IPv4 address is link-local (RFC 6724); IPv4 carries what has
  no IPv6. A pod whose IPv6 address is a ULA has no default route over
  IPv6 and leaves for the internet over IPv4 (below).
- A pod that resumes on another node gets both addresses there. Nothing
  stores a pod's addresses: the control plane and the ingress follow the
  pod's record.

### IPv6: a prefix routed to the node, or a /96 of the cluster's mesh

- A cluster's pod network is routed or meshed, for all its nodes.
  Routed: every node has a prefix that its network routes to it, by a
  static route or BGP, a /64 or a smaller one such as the /80 that AWS
  delegates to an interface or the /96 that GCP routes to every VM. A
  /96 holds 2^32 addresses, enough never to hand one out twice. A prefix
  that is only on-link, for which the node would answer neighbor
  discovery on behalf of every pod, is not supported.
- Meshed: the cluster has a ULA /64, `fd`, a Global ID of 40 random bits
  (RFC 4193, section 3.2) and subnet ID 0, which the control plane draws
  when the cluster is set up as meshed and keeps, or which the operator
  gives, any /64 of `fd00::/8`, such as to keep apart two clusters that
  will be joined. Every node gets a /96 of it, the next ID in bits 64 to
  95, when it first connects after its join, and keeps it for as long as
  it is in the fleet; a node that joins anew gets a new one. No ID is
  given out twice, so a stale member set that still names a removed
  node's pods never lets in the pods of another; ID 0 is given to no
  node. A cluster has room for 2^32 - 1 joins, and a node for as many
  pods' addresses. The shape is a cloud's: GCP gives every VM a /96 of
  its subnet's /64; a cluster is one /64, which a network routes,
  filters or peers as one subnet.
- `feosd` gives a pod an address of the node's prefix when the pod comes
  to the node, keeps it in the pod's record while the pod is there, and
  never hands an address out twice. A node holding both a prefix of its
  network's and a /96 of a mesh is refused: the two exclude each other.
- The node routes the rest of its prefix as unreachable, so that a
  packet for a pod that is not running does not loop back into the
  network.

### The mesh

- Every node of a meshed cluster has one WireGuard device with a key
  pair of its own, made when the node first comes onto the mesh; the
  private key stays in the node's state directory, readable by root
  alone, beside its certificate's (ADR 0015). The device listens on one
  UDP port at the node's endpoint: the address from which the node
  reaches the control plane, or the one it is configured with where the
  nodes reach each other at another.
- Every other node of the cluster is a peer: its public key, its
  endpoint and its /96 as the one prefix allowed for it. WireGuard
  routes by these prefixes: a packet leaves for the peer whose /96 holds
  its destination, and a packet that comes in counts only when that
  peer's key authenticated it and its source lies in that peer's /96. A
  node speaks for its own pods alone, and nothing without a node's key
  speaks at all, a pod that sends to another node's port through its
  own node's masquerade included. The ingress's instances are peers of
  the same kind, with prefixes from the same counter (ADR 0021).
- The node routes the cluster's /64 to the device; its own /96 stays
  unreachable but for its pods' addresses. The reverse path rule
  (below) holds for the mesh: a packet with a source in the cluster's
  /64 comes in through the mesh or through its pod's TAP, or it is
  dropped. The device has WireGuard's MTU, which leaves room for its
  headers over either family on a network of 1500.
- Pods are denied the peers' endpoints, as they are denied the node's
  own addresses: what a pod sends to another node goes through the mesh,
  to that node's pods. The node of the pod that a packet is for filters
  it, by the source address the mesh vouches for (ADR 0012).
- The mesh's port carries pods' packets and nothing else, and WireGuard
  answers nothing that no peer's key authenticated: it is no port for
  control (ADR 0001).
- A node reports its public key and its endpoint to the control plane,
  which knows which node a key is from by the connection's certificate
  (ADR 0015) and refuses a key another peer holds. The control plane
  sends every node the others as its peers, whole with a version or as a
  delta from one version to the next; a node refuses a delta that does
  not start at the version it has, and keeps its peers on disk, so that
  a restart loses none. A node that is down stays a peer: it may come
  back with its pods. A node removed from the fleet stops being one.
- The mesh does not need the control plane: when no instance is
  reachable, the nodes keep their peers, and their pods go on reaching
  each other. A new node, or a new endpoint, reaches the others after a
  delay; until then their pods do not reach its pods.
- `feosd` sets the device up over WireGuard's generic netlink family; it
  runs no `wg`.

### IPv4: a link-local address per pod

- `feosd` gives every pod an address of a link-local range, as a /32,
  unique on the node and not across nodes. No network routes link-local
  addresses (RFC 3927), and no tenant's network collides with them.
- Nothing allows by a pod's IPv4 address, and link-local destinations
  are denied (below), so an IPv4 address may come round again: `feosd`
  hands out the one that has been free longest, and drops the
  connection tracking entries of a deleted pod's addresses.
- The range bounds how many pods a node holds, suspended ones included.
- nftables masquerades the pods' IPv4 to the node's address, with
  Linux's connection tracking. The node filters a pod's packets before
  NAT, in the family the pod sent them in (ADR 0012).

### Each pod's TAP is a link of its own

- The pod's TAP device, named by its ID (ADR 0019), is a point-to-point
  link between the node and the pod; there is no bridge. The pod's
  addresses have a /128 and a /32 route on its TAP.
- The pod's gateways are `fe80::1` and `169.254.0.1`. The node is
  `fe80::1` on every TAP, and answers ARP for `169.254.0.1` on it by
  proxy, without holding the address: some clouds' gateways are
  `169.254.0.1`, and a node there would lose its own.
- The node holds the resolver's address, and the address at which it
  takes its pods' spans and serves their HTTP bindings (ADR 0026, ADR
  0028), on every TAP as /32s, and its ARP for a pod asks from the
  resolver's. Without an address on the TAP, the kernel takes any of the
  node's, and `169.254.169.254`, which some hosts hold for their own
  metadata service, would send the pod's answers to the microVM's MMDS
  instead of the node.
- A packet from a TAP whose source address is not routed back out that
  TAP is dropped: one reverse path rule (`fib`) pins every pod to its
  addresses, in both families.
- From a pod, the node takes neighbor discovery and ARP on the TAP, DNS
  to the resolver, and spans and HTTP bindings at its address for them
  (ADR 0026, ADR 0028), nothing else. Router advertisements and
  redirects from a pod are dropped.

### A pod with a ULA address has no default route over IPv6

- The guest's network configuration carries routes, each a prefix, a
  gateway and an MTU (ADR 0007). A pod whose IPv6 address is global gets
  a default route over `fe80::1`. A pod whose address is a ULA, on a
  mesh or not, gets a route for the unique local addresses, `fc00::/7`,
  over `fe80::1` instead: no network routes a ULA to the internet (RFC
  4193), so the pod has no IPv6 there to try. On a mesh, the route
  carries the mesh's MTU, so that TCP agrees on segments that fit from
  its first packet; the link and IPv4 keep theirs.
- With no route to a global IPv6 address, the resolver's sort (RFC 6724,
  rule 1) puts a name's IPv6 addresses after its IPv4 ones, and programs
  connect over IPv4 at once.

### DNS from feosd

- `feosd` serves DNS to its pods at a link-local IPv4 address that the
  node holds, the same on every node, so that a pod's resolver
  configuration holds after a resume elsewhere. The guest is pushed this
  resolver and no other, and so are its containers.
- It tells pods apart by the source address of a query, which the TAP
  pins, and refuses everyone else.
- It answers the names of a pod's name tables itself (ADR 0018), with
  every address a table has for a name and a short TTL, and the names a
  pod's HTTP bindings give it (ADR 0028). Every other name it asks the
  upstream resolvers that the pod's spec names, or the node's, as far
  as the pod's reachability allows (ADR 0012); an upstream that a pod
  may not reach is refused.
- It passes answers on as the upstream gave them. Pods have IPv4, so
  there is no DNS64. What it takes out of answers and records of
  queries is ADR 0013's.
- Its sockets outlive restarts of `feosd`: a query waits in them, it
  does not fail. They are handed over as systemd hands sockets to a
  service: under systemd a socket unit holds them, and a query while
  `feosd` is stopped starts it; under `feos-host-init`, PID 1 binds them
  and hands them to every `feosd` it starts; a self-upgrade hands them
  to the new binary. `feosd` answers on them once it knows its pods
  again, so that a query that waited is not refused.

### Metadata from MMDS

- `169.254.169.254` is the pod's metadata service. Firecracker answers
  it inside the microVM's network device (MMDS), in the version that
  wants a session token first, as AWS's IMDSv2 does, so that a request
  that a pod's code is tricked into sending gets nothing.
- Its packets never leave the VMM. Link-local destinations are denied on
  the node, so no pod reaches the node's own cloud metadata service.
- It holds the pod's ID and its addresses on the node, which `feosd`
  writes, and what the pod's creator puts into the pod's spec, which
  FeOS does not read. A pod learns where it runs without asking `feosd`
  or the control plane, and a sandbox or a service gets its
  configuration without credentials.
- A pod's credentials are not in it (ADR 0014): whatever runs in the pod
  can read it; the session token keeps out requests that the pod's code
  is tricked into, not the code.
- A snapshot keeps where the metadata is served, not what: `feosd` puts
  it in again after every load, with the addresses the pod has where it
  is restored.

### What no pod reaches

- A pod's reachability decides what it sends and what reaches it (ADR
  0012). There is no fixed list of private destinations: what allows the
  internet says which addresses that is.
- The operator lists prefixes of either family that no pod reaches,
  whatever its reachability says: a cloud's metadata service, the
  control plane, its database, management networks. The node's own
  addresses, the mesh peers' endpoints, and the link-local, loopback,
  multicast and unspecified addresses of both families are denied
  always. Other pods' IPv4 addresses are among them: pods reach each
  other over IPv6 alone.
- `feosd` writes its nftables tables over netlink itself; it runs no
  `nft` or `iptables`.

## Alternatives considered

- **IPv4 behind NAT on a bridge of isolated ports**, the sandbox
  system's network and this ADR's first decision, reversed the next day:
  no pod can be reached from outside or from another pod, and every node
  hands out the same addresses.
- **IPv6 only, IPv4 destinations through NAT64 and DNS64**, this ADR's
  second decision, reversed the same day: one family in the guest, but
  IPv4 literals, programs that speak only IPv4, resolvers of their own
  and DNSSEC validation break.
- **Routed IPv4, a prefix per node.** Every program works as it is, and
  IPv4 clients reach pods. But a /24 holds 253 pods, suspended ones
  included; addresses come round again, so a change of who may reach a
  pod would have to reach every node before its address is handed out
  again; and private IPv4 ranges collide with tenants' own networks once
  those are joined.
- **The same IPv4 address in every pod**, such as `169.254.0.2`: the
  guest's IPv4 would be the same on every node. But connection tracking
  and routing cannot tell such pods apart; the node would have to
  rewrite every pod's address to one of its own at the TAP, in eBPF, the
  inner headers of ICMP errors included. And a resume elsewhere changes
  the pod's IPv6 address anyway.
- **A private range for the pods' IPv4**, such as `10.200.0.0/16`: it
  collides with the node's and tenants' networks; link-local addresses
  do not.
- **A CLAT (464XLAT, RFC 6877)**, in the guest or on the node, so that
  pods have IPv4 on nodes without IPv4 of their own, through a NAT64 in
  the network: a translator in eBPF to build and keep, for networks we
  do not run in.
- **NAT66 to the internet from the ULA**, as Fly.io does with
  an overlay between its nodes: the nodes the mesh is for have no IPv6
  of their own to translate to. The mesh takes the overlay without the
  NAT.
- **Addresses that move with a pod**, a route per pod over BGP: stable
  addresses, and a routing table that changes with every resume
  elsewhere.
- **An on-link prefix, with the node as neighbor discovery proxy**:
  providers that resolve every address by neighbor discovery made IPv6
  unreliable for Fly.io, which dropped public addresses per machine for
  it.
- **A bridge with isolated ports**: rules for MAC addresses, ARP and
  neighbor discovery that a route per TAP does not need.
- **The node holding `169.254.0.1` on every TAP**: no proxy ARP, but a
  node whose network's gateway is `169.254.0.1` would deliver its own
  traffic for the gateway to itself.
- **Waking a suspended pod on a packet to its address**: the node would
  have to hold connections for a pod that is not there; the ingress
  holds the request and knows what it waits for (ADR 0021).
- **VXLAN**, as flannel carries pods between nodes, with offloads in
  many network cards. It authenticates nothing: a packet to a node's
  VXLAN port delivers an inner packet of any source, a pod's own too,
  sent through its node's masquerade with the node's address as its
  source. The mesh would need rules of its own: VXLAN from peers alone,
  pods kept off the peers' addresses, and each peer's inner sources
  checked against its /96, with nftables' matching of inner headers
  (Linux 6.2). Its packets are not encrypted, and an Ethernet overlay
  needs a forwarding entry, a neighbor entry and a route per peer, where
  WireGuard needs a prefix.
- **IPv6 in IPv4 (SIT, protocol 41), GRE, Geneve or FOU**: they
  authenticate nothing either. Protocol 41 has no ports for a network to
  spread flows by, and many clouds' networks do not pass it.
- **IPsec in the kernel**: authenticated and encrypted, but keys for
  every pair of nodes, from an IKE daemon or by hand, and policies per
  pair.
- **A mesh in userspace** (Tailscale, Nebula, ZeroTier): a program to
  package, its control plane beside ours, and every packet through
  userspace.
- **BGP with EVPN**, FRR on every node: a routing daemon per node and
  route reflectors, where the control plane knows every node and its
  prefix already. A network that takes routes over BGP routes a prefix
  to the node, and the routed case applies.
- **Routes in Kubernetes's node objects**, as flannel keeps them: FeOS
  runs without Kubernetes (ADR 0001); the k3s lab is one deployment.
- **Gossip between nodes** (memberlist, as in Serf): membership without
  the control plane, but the control plane is the fleet's truth (ADR
  0015), and a node's key needs something other than the node to vouch
  for it.
- **Prefixes from the node's address**, such as `fd00:fe05:<last
  octet>::/64`: not random, so every cluster that copies the scheme has
  the same addresses, and an address the network gives a node again
  brings back the prefix of the node that had it.
- **A /48 per cluster and a /64 per node**, the mesh's first shape,
  reversed on 2026-10-29: a /64 per node is the most comfortable
  prefix, but a cluster takes 65,536 /64s that no network routes as one
  subnet, and 65,535 joins are few for nodes that come and go with a
  cloud's autoscaler.
- **A /80 per node**, as AWS delegates: 32 bits of joins from a /48, or
  16 bits from a /64. A /96 of a /64 has the same 32 bits of joins, the
  cluster in one /64, and the length that GCP gives a VM.
- **No IPv6 in pods**: reachability and pods reaching each other would
  lose the address they rest on (ADR 0012).
- **IPv4 first in `/etc/gai.conf`**: glibc alone reads it, not musl or
  Go, and every container has an `/etc` of its own.
- **ICMPv6 errors unthrottled on the node** (`net.ipv6.icmp.ratemask`
  without destination unreachable): every connection still tries IPv6
  first and loses a round trip to the node.
- **Public resolvers for pods** (Google's, Cloudflare's): no code, but
  no upstream per pod and no record of queries (ADR 0013).
- **A resolver next to `feosd`**, such as Unbound: one more program to
  package, and the upstreams per pod to configure in it.
- **Sockets that `feosd` binds at every start**: a query while it
  restarts fails, and a pod's resolver waits seconds before it asks
  again.
- **systemd's file descriptor store** (`FDSTORE=1`), into which `feosd`
  puts sockets it bound itself: the address would stay `feosd`'s alone,
  but the store keeps them across a stop only from systemd 254 on, and
  `feos-host-init` would have to keep a store of its own.
- **A ULA address for the resolver**: pods have IPv4, and a link-local
  address next to the gateway needs no prefix of its own.
- **A metadata service on the node**: it could answer from `feosd`'s
  state, but it would be one more listener that every pod reaches, where
  MMDS is in the VMM already.
- **AWS's layout of the metadata** (`latest/meta-data/...`), so that
  tools for EC2 find it: a pod is no EC2 instance, and those tools would
  find names they expect with values that mean something else.
- **Metadata in the guest's configuration**, pushed over vsock: the
  guest would have to serve it to its containers, where MMDS serves
  every process in the pod already.

## Consequences

- Pods reach each other, and are reached from outside, over IPv6 where
  their reachability allows (ADR 0001, isolation): on a routed network
  from the network, on a mesh from the mesh's peers, the ingress among
  them (ADR 0021). A pod of a meshed cluster has no IPv6 to the internet
  and leaves over IPv4 alone, at once.
- Pods reach IPv4 destinations as they are: IPv4 literals and programs
  that speak only IPv4 work. A program that listens only on `0.0.0.0`
  cannot be reached from outside; listeners bind `[::]`, and a service's
  readiness check says so (ADR 0021).
- A pod's connections do not survive a resume on another node, and its
  addresses change.
- Every node needs a prefix routed to it or a control plane that meshes
  it, and IPv4 of its own, through which its pods leave. A node whose
  operating system is FeOS takes its IPv4 by DHCPv4, as a distribution
  does, and its prefix by DHCPv6 delegation or, where it is routed
  statically, from the kernel's command line.
- A meshed cluster's nodes pass UDP on the mesh's port between them,
  every packet between nodes costs encryption, and the node's kernel
  needs WireGuard. A node without a control plane has no mesh: its pods
  reach each other on the node alone, and get the route for the ULAs
  too.
- The guest's network configuration has both families and routes (ADR
  0007). A pod without a network has no metadata service: MMDS is part
  of the network device.
- The control plane's front door reaches pods' ports over IPv6, without
  a port forward over vsock (ADR 0004, ADR 0021).
- The pods' network is routing, WireGuard and netfilter: no eBPF, no
  translator, nothing to build on nightly.
- Other tables still apply: where Docker or a firewall drops forwarded
  packets by default, pods reach nothing until it lets them through.

## History

Decided on 2026-10-14 as IPv4 behind NAT on a bridge (55229ff), anew on
2026-10-15 as IPv6 only with NAT64 (42717c5) and, the same day, as the
two families of this text (532bd25); the metadata service and the
resolver's sockets that outlive `feosd` followed on 2026-10-16 (88fa3f1,
4fb7542), the name tables on 2026-10-19 (1aafa55). ADR 0017, the
WireGuard mesh for nodes that their network routes no prefix to, was
decided on 2026-10-17 (571dfcd) with a /48 per cluster and a /64 per
node, changed to a /64 per cluster and a /96 per node on 2026-10-29
(757c260), and merged into this ADR on 2026-11-08; `feosd` and `ferund`
still take a cluster's earlier /48 as its /64 of subnet 0. The ingress
became a peer of the mesh on 2026-10-28 (33ea94e, ADR 0021). The node
takes its pods' spans (2026-10-31, e3b0dac, ADR 0026) and HTTP bindings
(2026-11-05, 6a37caa, ADR 0028) at a second address of its own on every
TAP, where it took DNS alone before.
