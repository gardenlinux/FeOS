# ADR 0052: Membership changes reach the nodes as pushed deltas, members before names

**Status:** Proposed · **Date:** 2026-11-08

A change of a membership's members, a pod created, moved or ended, is
appended as a delta to a short log in the transaction that records the
pod's address, and the replica that holds a node's connection pushes
every pending delta of the memberships the node holds to it in one
batch, before the name table that names the new address. Rules are put
whole, and a deployment's status says on how many of the nodes that
hold them they are acknowledged. The controller's loop stays as
anti-entropy, comparing versions and not content. The target: a member
change is acknowledged by every connected node that holds the
membership within 1 s at the 99th percentile, at 200 changes a second
in a cluster.

## Context

ADR 0012 derives every pod's memberships and rules and sends them to
the nodes as security groups. Membership is volatile: an environment's
pods come and go with scaling, with every roll (surge 1), with an idle
service resumed on another node, which gives it a new address (ADR
0010), and with a node drained. A rule that admits a workload needs
that workload's addresses on every node where a target runs, and a new
pod is useless until they are there: its callers' packets are dropped.

The controller puts groups whole, every few seconds or on a hint,
comparing each connected node's list with what it should have. A
member's change costs every node the whole group, and is as slow as the
loop; a hint that is lost waits for the next pass. `feosd` takes a
delta of one group from one version to the next already, and refuses
one that does not start at the version it has (ADR 0012); the control
plane does not send them.

A pod's memberships never change (ADR 0012), so a member leaves one
only when its pod ends or moves, and addresses are never given twice
(ADR 0010): a removal that arrives late opens nothing. Only additions
are urgent, for availability. Rules change with deployments and with a
sandbox's `reaches`, and those are urgent for security.

A name table (ADR 0018) names a new replica's address to its callers.
A caller that resolves the address before the node of the target admits
it fails, and retries into a timeout that looks like a missing
reference.

## Decision

- **Every member change is a delta in a log.** In the transaction that
  records a pod's address (its create, its move, its end), `ferund`
  increments the version of each of the pod's memberships under the
  row lock, appends the change (the version, the members added and
  removed) to the log, and notifies. A move is one delta: the old
  address out, the new one in.
- **The replica that holds a node's connection pushes to it.** It knows
  which memberships the node holds, those its pods are members of and
  those their rules name, with the version the node acknowledged. On a
  hint it takes every pending change of those, folds each one's changes
  into one delta from the node's version to the newest, and sends them
  in one call. `feosd` applies them in one netlink batch and one write
  to disk, and the call's success is the acknowledgement. A node that is
  out of the hint's reach wakes on the next anti-entropy pass.
- **`feosd` gains a batch**: whole groups and deltas of several groups,
  all or none, as one nftables transaction. A delta that does not start
  at the version `feosd` has fails the batch with the groups it
  concerns, and the sender puts those whole and resends the rest.
- **A member before its name, on every node.** Name tables travel over
  the same connection, through the same sender, after the memberships.
  The delta that adds an address is sent and acknowledged before the
  name-table delta that names it. On every node, a caller resolves a
  new replica only once the node admits it. No barrier across nodes is
  needed, and a slow node delays only its own callers. Removals go the
  other way: the name first, then the member.
- **The address is known before the guest boots.** `feosd` assigns it
  at the pod's create (ADR 0010), so the delta races the boot and
  usually wins.
- **Rules are put whole.** A workload whose rules change, by a
  deployment or by a sandbox's `reaches`, is put whole on every node
  that holds it, and its chains are replaced atomically there. A
  deployment puts the changed rules first, and its status says on how
  many of the nodes that hold them the new rules are acknowledged. A
  node out of reach keeps its old rules from disk, never none, and gets
  the new ones before anything else when it connects again.
- **The log is short.** Changes are kept for 15 minutes. A node further
  behind gets the membership whole.
- **The loop stays as anti-entropy.** Every pass the leader compares
  versions, not content, per connected node, and repairs what a missed
  hint or a failed push left behind.
- **Target:** for 200 member changes a second in a cluster, a change is
  acknowledged by every connected node that holds the membership within
  1 s at the 99th percentile. `ferund` measures this from the commit to
  each acknowledgement, as a metric and a span per push.

## Alternatives considered

- **A barrier across nodes before a pod counts as ready**: strict, but
  one slow node would stall every roll. Ordering members before names
  on each node gives the same thing where it matters, and the ingress
  already waits for its own admission (ADR 0021).
- **A faster loop of whole groups**: the loop's interval is the floor
  of the latency, and every change costs every node the whole group.
- **The leader pushing to every node**: one hop more through the
  replica that holds the connection, and one process for all of the
  cluster's changes.
- **One delta call per group**, the call `feosd` has: a pod's create
  changes two memberships and often a name table, and each would be
  acknowledged alone; a batch is one transaction on the node and one
  acknowledgement.
- **Membership changed in place, without a roll**, which would make
  removals urgent too: ADR 0012 keeps memberships fixed for a pod's
  life, so that only additions matter here.

## Consequences

- A new replica is reached by its callers within about a second of its
  address being known, instead of within a pass of the loop; a roll's
  surge is usable as soon as it is ready.
- A caller never resolves a replica that its node does not yet admit,
  so a timeout between two services means a missing reference and
  nothing else.
- A deployment's status tells how far its rules have reached, and an
  operator sees the propagation's latency.
- A node that does not understand a batch of deltas is served whole
  groups by the loop, so old and new nodes mix.
- The log, the per-node record of acknowledged versions and the sender
  per connection are new state in `ferund`; the loop keeps them honest.

## History

Decided on 2026-11-08 as the propagation part of ADR 0034's revision
(a061348); the derivation of reachability it carried was built the
same day (a529381) with the deltas, the batch push and the metric left
as later work. Split out of ADR 0012 at the consolidation of
2026-11-08, so that ADR 0012 states what is built and this ADR what is
decided in direction.
