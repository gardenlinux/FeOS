# ADR 0013: feosd records the pods' traffic

**Status:** Accepted · **Date:** 2026-10-15

`feosd` records what its pods do on the network, as the host sees it:
every flow from connection tracking, every drop with its reason from the
drop rules, every DNS query from its resolver, and the server name of
every TLS or HTTP connection a pod opens, read from copies of its first
packets. Nothing in the data path waits for `feosd`, and there is no
eBPF: what the kernel cannot hand over in time is dropped and counted.
The records name the pod by its own addresses and stream from the node;
the resolver strips ECH from HTTPS records so that server names stay
readable.

## Context

Pods run untrusted code (ADR 0001) at addresses the internet can reach
(ADR 0010). The operator answers for what they send: an abuse report
names an address and a time, an incident asks what a pod talked to. The
users of an agent's work want to see what its code contacted, and a
rule that drops a packet without saying so is a rule nobody can debug
(ADR 0012).

From the host, a pod is a TAP: the host sees its packets, not its
containers or processes. A ClientHello often spans two segments now that
key shares are post-quantum and browsers shuffle the order of
extensions, so a server name wants reassembling in user space. A pod's
IPv4 address is translated on the node (ADR 0010), so a record must
carry the pod's own addresses, not what the internet saw.

## Decision

### What is recorded

- **Flows**: every connection of a pod, in either direction: the pod,
  the protocol, addresses and ports (the pod's own, before NAT), when it
  began and ended, and bytes and packets each way.
- **Drops**: packets that a rule of the node dropped (ADR 0012), limited
  in rate per pod: the pod, the protocol, addresses and ports, and why.
- **DNS**: every query of a pod that is answered: the name, the type, the
  answer's addresses and its code; a name the resolver refuses is
  recorded so (ADR 0012).
- **Server names**: for a TLS connection that a pod opens, the name in
  its ClientHello; for plain HTTP, the request's host. Both are joined to
  the connection's flow.
- Nothing else of a payload is kept.

### Where it comes from

- Flows come from connection tracking: `feosd` reads its events, new and
  destroyed, over netlink, with accounting and timestamps on, and finds
  the pod by either of its addresses.
- Drops come from the drop rules, which log through nflog with a limit
  per pod and a prefix that says why.
- DNS comes from `feosd`'s resolver (ADR 0010), which every query passes;
  a pod's queries to the resolver are left out of the flows.
- Server names come from copies: nftables copies the first packets of
  every TCP connection that a pod opens to `feosd` through nflog. `feosd`
  reassembles the ClientHello and reads the name.
- There is no eBPF, and nothing in the data path waits for `feosd`. A
  `feosd` that restarts or falls behind misses records; traffic does not
  stall. `feosd` counts what it missed in the next record.
- A pod that left is known by its addresses for a while more, unless
  another pod holds one of them by then, so that the ends of the
  connections its delete or hibernation ends are still attributed to it.

### Server names stay readable

`feosd`'s resolver removes the `ech` parameter from HTTPS records, so
that clients send the server name in the clear. A client that brings an
ECH configuration of its own is not seen.

### Where records go

- Records name the pod. `feosd` streams them to any subscriber on the
  node (`TrafficService`, `feosctl traffic watch`), from a bounded buffer
  behind which a subscriber that falls behind misses records and learns
  how many.
- Whatever store takes them off the node joins them to tenants and
  decides how long they are kept and who sees them.

## Alternatives considered

- **eBPF on every TAP**, as the sandbox system before FeOS had it:
  counting in the kernel pays off once one event per flow is too many.
  But connection tracking sees every flow already, a ClientHello over
  two segments wants reassembling in user space anyway, and the host
  sees no processes either way.
- **Queueing packets to `feosd`** (NFQUEUE): it could block by server
  name before a connection goes on, but traffic would wait for `feosd`,
  and stall while it restarts.
- **Recording in `feos-guest`**: the guest knows containers and
  processes, but it runs inside the tenant's VM, where the workload can
  forge or suppress its records.
- **Intercepting TLS** with a CA of ours in every container: full URLs,
  but it breaks pinning, needs the CA in every image, and reads tenants'
  content.
- **Capturing whole packets**: the volume, and tenants' content.
- **Blocking by name** before a connection goes on, should rules by name
  not be enough: not pursued; the resolver's egress by name (ADR 0012)
  refuses a name a pod may not reach before any connection is made.

## Consequences

- Connection tracking keeps accounting and timestamps; `feosd` turns
  them on once the table has loaded it.
- `feosd`'s work grows with the pods' connections. Under load the kernel
  drops events that `feosd` does not read in time, and the counts say
  so.
- Tenants' DNS names and server names are recorded on the node and kept
  there only until a subscriber has read them; whoever stores them
  decides their retention.
- A record is attributed to a pod by its addresses, so an address handed
  out again soon after a pod left could mix two pods' records; `feosd`
  forgets an address's connections when the pod goes.

## Open questions

- **Where the records leave the node to.** `ferund` takes traffic
  records as events on the node's uplink into `traffic_records`, with a
  retention of its own and an admin view per sandbox and node (ADR 0015),
  but `feosd` sends none; ADR 0012 names the sink (ADR 0026) for drops
  and refused names, mapped to the deployment's words. Until one of the
  two is built, records are read on the node alone, and tenants see
  nothing of their pods' traffic.

## History

Decided on 2026-10-15 with the pods' network (42717c5) and built in
`feosd` the same day (3739081, b3cab67); records carry both of a pod's
addresses since 2e50456. `ferund`'s side of the uplink, the `traffic`
event and `traffic_records`, came on 2026-10-16 and 2026-10-17 (6e9ddce,
0fe6895) and has nothing to receive. ADR 0034's revision of 2026-11-08
(a061348) answered the question of blocking by name and named the sink
as the home of drops and refused names.
