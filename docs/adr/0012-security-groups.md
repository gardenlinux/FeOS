# ADR 0012: Security groups decide what pods reach

**Status:** Proposed · **Date:** 2026-10-15 · Amended by [ADR 0034](0034-reachability-from-declarations.md) (revised 2026-11-08): egress by name, the open question, is decided — the resolver puts the addresses of a pod's allowed names into a timed set and refuses the rest; the control plane's groups are the memberships it derives (`env-`, `wl-`, `ing-`), never a tenant's

## Context

With ADR 0010, every pod has an IPv6 address that other pods and the
internet can reach. Pods run untrusted code of many tenants (ADR 0001),
so what may reach a pod, and what a pod may reach, has to be said for
every pod and enforced on its node.

Kubernetes says it with NetworkPolicy: label selectors, rules that only
allow, peers as selectors or as address blocks with exceptions, and
everything allowed until a policy selects a pod. Clouds say it with
security groups: stateful rules that only allow, peers as address blocks
or other groups, and nothing in that no rule allows. Firewalls order
their rules, and allow and deny.

`feosd` keeps a pod's labels and does not read them (ADR 0004). A pod's
address changes when it resumes on another node (ADR 0010), and a rule
that names a group as its peer has to learn the addresses of that group's
pods on every other node.

## Decision

### Security groups

- A security group has an ID, a version and rules. A rule has a
  direction, in or out; a protocol, TCP, UDP, ICMP, ICMPv6 or any; ports,
  where the protocol has them; and a peer: an address block with
  exceptions, as Kubernetes's `ipBlock` has them, or a security group.
- Rules only allow, and have no order. A pod has one or more groups,
  which the network in its spec names; what one of them allows, the pod
  may do. Everything else is dropped.
- Security groups are stateful: the replies to a connection that a rule
  allowed pass, and so do the ICMP and ICMPv6 errors that belong to it.
- A block is of either family. A pod's IPv4 address is for leaving only
  (ADR 0010), so an IPv4 block matters in rules out; a group as a peer
  stands for its members' IPv6 addresses.
- The prefixes of `--deny-prefix` (ADR 0010) come first: no rule opens
  them.
- A pod whose spec names no group gets `feosd`'s default group: out to
  the internet, which is the global addresses of both families; nothing
  in. That is what the first version of
  ADR 0010 gave every pod.
- Labels and selectors are the control plane's. It may offer Kubernetes's
  NetworkPolicy to CaaS and turn it into groups; `feosd` sees groups and
  addresses only.

### The control plane sends each node its groups

- The control plane owns the groups and knows their members: the pods in
  each group, at the addresses that `feosd` reports.
- It sends a node the groups of the node's pods, and every group that
  their rules name as a peer, through `feos.network.v1` on the node's
  connection (ADR 0015). A group comes whole, with its rules, members and
  version, or as a delta from one version to the next: the members added
  and removed.
- `feosd` refuses a delta that does not start at the version it has; the
  control plane then sends the group whole. `feosd` keeps its groups on
  disk, so that a restart loses none.
- `feosd` does not start a pod whose groups it does not have. A peer
  group it does not have yet allows nothing.
- A pod's new address reaches the other nodes after a delay, and until
  then its peers there drop its packets. No address is handed out twice
  (ADR 0010), so a late delta never lets a pod in under another pod's
  address; it only keeps it out for a moment.

### nftables enforces them

- Each pod has a chain for what it sends and one for what it receives,
  reached through verdict maps by its TAP. Each group that a rule names
  as a peer is a named set of addresses; a block with exceptions is a set
  of intervals, one per family, the difference computed by `feosd`.
- In each chain, what belongs to an allowed connection passes, denied
  prefixes are dropped, the rules of the pod's groups accept, and the
  rest is dropped.
- A delta changes set elements, and a changed rule replaces chains, each
  in one netlink batch: atomically.
- Packets are dropped in `forward`, before connection tracking confirms
  an entry: traffic that is dropped leaves no state.

## Alternatives considered

- **Kubernetes's NetworkPolicy in `feosd`**: `feosd` would read labels,
  and everything would be allowed until a policy selects a pod, which
  untrusted tenants should not get.
- **Ordered rules that allow and deny**, as firewalls have: the rules of
  several groups would need one order between them, a deny in one group
  would silently override another, and deltas would carry positions. An
  agent that writes policies gets unordered allows right more easily.
- **Layers**, as Kubernetes puts AdminNetworkPolicy before NetworkPolicy
  and BaselineAdminNetworkPolicy after it: the operator's ordered rules
  before the tenants', defaults after them. `--deny-prefix` and the
  default group cover what we need of them. A layer of ordered rules
  before the groups can still come without changing them.
- **eBPF on every TAP**, with maps of prefixes: a delta is a map update,
  and the longest prefix gives exceptions for free. But security groups
  are stateful. The program would need connection tracking of its own,
  or read netfilter's after netfilter has confirmed an entry for every
  packet the program then drops, so that a flood of SYNs at a pod's
  address fills the node's table. Several map updates are not one
  transaction.
- **One version for all groups**: every change would reach every node,
  and a gap in a group a node does not use would make it fetch them all.
- **`feosd` reading a stream per group from Valkey**: `feosd` would have
  to work out which groups it needs; the control plane places the pods
  and knows it already.
- **An identity in the address**, as Fly.io's private network carries
  the network's ID in its addresses: rules without sets to send around,
  but one identity per address, and no room in a prefix smaller than
  /64.

## Consequences

- The network in `PodSpec` names the pod's security groups. `feosd` serves
  `feos.network.v1`; `feosctl` gets commands for groups.
- The control plane keeps pods' addresses and sends deltas when a pod is
  created, moves or is deleted.
- `feos_utils::nft` learns sets of intervals, verdict maps and changes of
  set elements.
- With the default group, labs and tests work as they do: pods reach the
  internet and not each other.

## Open questions

- **Egress by name** (`*.github.com`): `feosd`'s resolver (ADR 0010)
  could put the addresses of allowed names into a set for their TTL, as
  Cilium's `toFQDNs` does.
- **Limits**: groups per pod, rules per group, members per group.
