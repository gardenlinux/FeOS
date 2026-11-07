# ADR 0034: Security groups of a tenant's own — an environment's pods in groups that name each other, ports and address blocks, and a new member on every node before its name

**Status:** Proposed · **Date:** 2026-11-06

**Touches:** ADR 0012 (groups get a batch of deltas, and the control
plane sends deltas at last), ADR 0018 (an environment gains the object
`securityGroup`, its services, jobs and addons name theirs; the group
`env-<id>` becomes the set of all the environment's pods), ADR 0021 (the
ingress checks a client's address against the blocks of the target's
groups), ADR 0013 and ADR 0026 (drops of a tenant's groups reach the
sink).

## Context

ADR 0012 built security groups into the node: stateful rules that only
allow, peers as address blocks with exceptions or as other groups,
enforced by `feosd` with nftables, kept on disk, with deltas of members
from one version to the next. ADR 0018 and ADR 0021 use them for the
platform alone. Every environment has one group, `env-<id>`, whose pods
reach each other on every port and the internet, and the ingress reaches
an image service's routed ports through `ing-<id>` while the environment
is public. A tenant has no say in it:

- every service of an environment reaches every other one, the web
  front end the Postgres addon as much as the API that owns it;
- every pod reaches the whole internet, and nothing narrower can be
  said: not "only the payment provider's network", not "no egress";
- a public environment is public to every client; "only the company's
  network" cannot be said.

What a tenant needs is what clouds give: groups that pods are put in,
rules that name other groups, ports and address blocks, in and out.

Membership is volatile. An environment's pods come and go with scaling,
with every roll (surge 1), with an idle service resumed on another node,
which gives it a new address (ADR 0010), and with a node drained. A
group that names another as its peer needs that group's addresses on
every node where one of its members runs, and a new pod is useless
until they are there: its callers' packets are dropped. Today
`env-groups` puts groups whole, every 10 s or on a hint, comparing each
connected node's list with what it should have: a member's change costs
every node the whole group, and is as slow as the loop.

## Decision

### Groups are objects of an environment

- An environment gains the object `securityGroup`, under the key
  `securityGroups`: a name unique in the environment (1 to 32 of
  lowercase letters, digits and `-`), rules `in` and rules `out`.
- Services, jobs and addons name their groups in `securityGroups`, at
  most five each. All pods of an object are in the same groups: a
  group is part of the pod's template.
- Every environment has the group `default`. An object that names no
  group is in `default`; an empty list is an error of the plan, so
  that every pod of an environment has at least one group.
- **Secure by default.** `default`, like every group the tenant
  declares without rules, lets nothing in and reaches the internet.
  Nothing reaches a pod that no rule lets in, not even a pod of the
  same environment. The spec may write `default`'s rules like any
  other group's.
- Groups belong to their environment: their members are its pods, and
  their rules name only its groups. A copy of an environment gets
  groups of its own.
- In `ferund` a group is a row with a UUID and the ID `sg-<uuid>` on the
  nodes, so that the name may be long and an environment copied for a
  new region gets new groups.

```yaml
securityGroups:
  web:
    in:
      - ports: [8080]                     # routed: only these clients
        from: {cidr: 203.0.113.0/24}
  api:
    in:
      - ports: [9000]
        from: {group: web}
    out:
      - to: internet
  db:
    in:
      - ports: [5432]
        from: {group: api}
      - ports: [5432]
        from: {group: migrate}
  migrate: {}
services:
  web: {securityGroups: [web]}
  api: {securityGroups: [api]}
jobs:
  migrate: {on: deployment, securityGroups: [migrate]}
addons:
  db: {type: postgres, securityGroups: [db]}
```

### Rules

- A rule has a protocol (`tcp` when it is left out, `udp`, `icmp`, or
  `any` without ports), ports or port ranges, and exactly one peer:
  - `{group: <name>}`: the members of a group of the same environment;
  - `{cidr: <prefix>, except: [<prefix>…]}`: an address block of either
    family, as ADR 0012 has it;
  - `internet`: the egress table of ADR 0018, the global unicast blocks
    of both families without the special-purpose ranges.
- Rules only allow, have no order and are stateful (ADR 0012): what one
  of a pod's groups allows, the pod may do, and the replies pass.
- **Out to a pod of the same environment is implied.** A pod may send
  to every pod of its environment, and the receiver's `in` rules decide.
  A rule between two groups is then written once, on the side that is
  reached, as clouds' groups do with their default egress. `out` rules
  matter for what is not the environment: blocks and the internet.
- **A group without `out` gets `out: [internet]`.** `out: []` means
  none. `ferund` writes the default into the normalised spec, so that
  the plan shows it.
- `--deny-prefix` comes first, as before: no rule opens it. A block in
  the private ranges is refused at the plan for as long as the cluster
  denies them, and is not accepted silently.
- What the platform carries needs no rule, as now: DNS from `feosd`, the
  metadata service, the node address `169.254.169.1` (traces, logs,
  HTTP bindings), readiness checks (over the port forward, not the
  network), and the ingress on routed ports (below).
- Limits: 50 groups per environment, 5 per object, 60 rules per group,
  each with one peer. Members are as many as the environment has pods.

The plan checks the groups as it checks the rest. A group named that
does not exist, a group deleted while an object names it and a block
outside its family are errors. Since nothing is open by default, the
plan also says what is missing. An object that uses an addon's
credentials, or names another object as the host of a variable's URL,
while no rule lets it reach that object's port, is a warning that names
the rule to write. An agent learns what it forgot before the
deployment, not from a timeout.

### The ingress checks the client

- A pod sees the ingress's addresses, not the client's: a block in an
  `in` rule on a routed port means nothing on the node. So the blocks
  of such rules go into the ingress's table (ADR 0021), per service
  and port. The ingress admits a client of a route to that port only
  when its address is in one of them, and answers anything else with
  403. Its access log says that a security group refused the client.
  IPv4 blocks count here: clients come over both families.
- A routed port of a public environment stays open to every client
  unless one of the service's groups has an `in` rule with a block on
  that port. Then it is open only to those blocks. The plan shows which
  is the case.
- The client's address is the one the ingress sees. The balancer keeps
  it (`externalTrafficPolicy: Local`), and headers that claim another
  are never trusted.
- The ingress still reaches its targets through `ing-<id>`, as now.

### Membership lasts as long as the pod

- A pod's groups are in its template. A changed list is a new template,
  and the object rolls, as with any other change of the template.
- A member therefore leaves a group only when its pod ends or moves.
  Addresses are never given twice (ADR 0010), so a removal that arrives
  late opens nothing. Only additions are urgent, and they are urgent
  for availability, not for security. What must reach every node fast
  for security is rules, and rules change rarely.
- The exception is an attachable sandbox: it becomes a member of the
  groups of the services attached to it while it runs. Its own chains
  keep its `sbx-<id>` rules (no ingress), so that pods of the
  environment admit the sandbox and the sandbox admits none of them.
  Leaving a group when an attached service is removed is a removal that
  matters, and is acknowledged like a change of rules. This is good
  enough for development, and no image pod depends on it.
- `env-<id>` stays as the set of all the environment's pods: no `in`
  rules, the one `out` rule to itself, the implied rule above.

### Changes reach the nodes as deltas, pushed, in order

- **Every member change is a delta in a log.** In the transaction that
  records a pod's address (its create, its move, its end), `ferund`
  increments the version of each of the pod's groups under the group's
  row lock, appends the change (`version`, added, removed) to the
  group's log, and notifies. A move is one delta: the old address out,
  the new one in.
- **The replica that holds a node's connection pushes to it.** It knows
  which groups the node holds: those its pods are members of and those
  their rules name (`node_security_groups`, with the version the node
  acknowledged). On a hint it takes every pending change of those
  groups, folds each group's changes into one delta from the node's
  version to the newest, and sends them in one call. `feosd` applies
  them in one netlink batch and one write to disk, and the call's
  success is the acknowledgement. A node that is out of the hint's
  reach wakes on the next anti-entropy pass.
- **`feosd` gains a batch**, `UpdateSecurityGroups`: whole groups and
  deltas of several groups, all or none, as one nftables transaction.
  A delta that does not start at the version `feosd` has fails the
  batch with the groups it concerns, and the sender puts those whole
  and resends the rest.
- **A group before its name, on every node.** Name tables (ADR 0018)
  travel over the same connection, through the same sender, after the
  groups. The delta that adds an address to a group is sent and
  acknowledged before the name-table delta that names it. On every
  node, a caller resolves a new replica only once the node admits it.
  No barrier across nodes is needed, and a slow node delays only its
  own callers. Removals go the other way: the name first, then the
  member.
- **The address is known before the guest boots.** `feosd` assigns it
  at the pod's create (ADR 0010), so the delta races the boot and
  usually wins.
- **Rules are put whole.** A group whose rules change is put whole on
  every node that holds it, and its chains are replaced atomically
  there. A deployment puts the changed groups first, as it does now,
  and its status says on how many of the nodes that hold them the new
  rules are acknowledged. A node out of reach keeps its old rules from
  disk, never none, and gets the new ones before anything else when it
  connects again.
- **The log is short.** A group's changes are kept for 15 minutes. A
  node further behind gets the group whole.
- **`env-groups` stays as anti-entropy.** Every 10 s the leader
  compares versions, no longer content, per connected node, and repairs
  what a missed hint or a failed push left behind.
- **Target:** for 200 member changes a second in a cluster, a change is
  acknowledged by every connected node that holds the group within
  1 s at the 99th percentile. `ferund` measures this from the commit to
  each acknowledgement (`ferun_security_group_propagation_seconds`,
  and a span per push).

### A drop says which rule is missing

- `feosd` records drops (ADR 0013). For the tenant's groups, `ferund`
  maps the two ends to objects and puts them into the sink as records
  of `source: network`: "`web` → `db`:5432, refused, no rule of `db`
  admits `web`", per object pair and port, rate limited.
- A first refusal after a deployment is an incident on the object. A
  rule that drops without saying so is a rule nobody can debug, least
  of all an agent.

## Alternatives considered

- **Kubernetes's NetworkPolicy in the control plane**, selectors over
  labels: everything is allowed until a policy selects a pod, which ADR
  0012 rejected for untrusted tenants. A selector is also checked only
  at run time, while a named group is checked by the plan.
- **`default` letting its own members in**, as clouds' default groups
  do and as `env-<id>` does today: no environment would notice the
  change, but every service of an environment would reach every other
  one until someone writes rules, and most never would.
- **Egress as `0.0.0.0/0` and `::/0`**: it would open the pods' mesh and
  every internal network that `--deny-prefix` leaves out on a cluster
  (ADR 0018 says why the egress table never is either). Pods of the
  environment are reached by the implied rule, so `internet` lacks
  nothing a tenant can reach today.
- **Rules on the objects, without groups** (`services.db.allowFrom:
  [api]`): shorter for three services, but there is nothing to share.
  Every service that should reach the same things repeats them, and
  "the services the API team owns" has no name.
- **Groups across environments** (stage reaching a shared service):
  this is wanted, but it is a question of tenancy and of naming
  environments in another's spec. It is left to an ADR of its own, and
  nothing here prevents it.
- **Membership changed in place, without a roll**, as clouds change a
  network interface's groups: a removal would then be urgent for
  security, and every removal would need an acknowledgement from every
  node before access counts as gone. A roll is cheap for us, and makes
  removals harmless.
- **A barrier across nodes before a pod counts as ready**: strict, but
  one slow node would stall every roll. Ordering groups before names on
  each node gives the same thing where it matters, and the ingress
  already waits for its own admission (ADR 0021).
- **A faster loop of whole groups**: the loop's interval is the floor
  of the latency, and every change costs every node the whole group.
- **The leader pushing to every node**: one hop more through the
  replica that holds the connection, and one process for all of the
  cluster's changes.
- **`out` rules required between groups**, as written both ways: twice
  the rules for the same intent, and a frequent mistake (one side
  forgotten) that the plan could only guess at.
- **An identity in the packet** (eBPF and identities, as Cilium has
  them): ADR 0012 rejected it, and nothing here needs it.

## Consequences

- The environment spec gains `securityGroups`, and services, jobs and
  addons gain `securityGroups`. Normalisation writes the defaults, and
  the plan checks names, families, limits, the private ranges and the
  addon warning.
- Every environment gets a group `default`. Its pods name `env-<id>`
  and `sg-<default>` from the first `ferund` with this ADR, which rolls
  every image service and gives every job a new template once.
- **Existing environments are migrated once, open as they were.** The
  migration writes `default`'s rule of today (in from `default`, any
  protocol) into their latest and deployed specs, so that nothing
  breaks on the roll. FeCode writes the same rule into the
  `fecode.yaml` of each existing project in one commit, or the next
  deployment from the file would close it. Environments created after
  that start closed. The migrated rule shows in the plan as what it
  is, every service reaching every other, and FeCode's agent is asked
  to replace it with rules of its own.
- `ferund` gains the tables of groups, members, the change log and
  what each node holds, a sender per node connection for groups and
  name tables, the propagation metric, and status per deployment.
- `feosd` gains `UpdateSecurityGroups`, and a version per group in its
  list that anti-entropy can compare without content.
- `ferun-ingress`'s table carries blocks per service and port, and its
  access log the refusal.
- Drops of the tenant's groups reach the sink, which ADR 0013 has not
  sent so far. FeCode's Ops view can then show them.
- FeCode passes `securityGroups` through `fecode.yaml` (a FeCode ADR of
  its own), and its agent is told that a timeout between services is
  usually a missing rule, and where to read it.

## Open questions

- **Egress by name** (`*.stripe.com`), as ADR 0012 already asks:
  blocks of providers change, and tenants think in names.
- **A tenant's private networks**: `--deny-prefix` denies the private
  ranges. Reaching a customer's network over a VPN or peering needs a
  route as well as a rule, and the deny list would then have to allow
  that tenant's block.
- **Ports by name** (`ports: [postgres]`) from the target's ports, so
  that a changed port changes the rule with it.
- **Groups across environments**, above.
