# ADR 0012: Reachability from declarations, enforced by security groups on the node

**Status:** Accepted · **Date:** 2026-10-15

What a deployment declares, its pods may do, and nothing else. Every
reference of a deployment, a variable from a service's port or an
addon's output, a URL that names one, a route, an entry of `uses`,
opens the port it names from the object that holds it to the object it
names; a workload's replicas reach each other; a service, a job, a
sandbox and a tenant's job choose their egress; a route names its
clients; a sandbox and a tenant's job say what of their environment
they reach. From this the control plane derives every pod's
memberships and stateful rules that only allow, which no tenant
writes, and sends them to the nodes as security groups. `feosd`
enforces them per pod with nftables, denied prefixes first, and its
resolver enforces egress by name: it answers an allowed name into a
timed set and refuses every other.

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
their rules, and allow and deny. All of them make somebody write the
call graph down twice, once in the application and once in the rules,
because the platform does not know it.

We know it. A deployment's spec names every call it makes, because the
caller needs the value: the variable that carries a service's port or
an addon's output, the URL that names a service as its host, the route
that sends the ingress to a port. In a project of eight services, two
addons and a job, the services call each other over 25 edges, every one
a reference; a second grammar for the same 25 facts is kept by hand,
and an agent that reads a warning asking for it pastes the rule. What a
tenant wants beyond the references is short: "only the payment
provider" or "no egress"; "only the company's network" for a public
route.

Within a cluster the address is the workload: a pod sends from its own
address only (ADR 0010), a node's prefix arrives only from the node
that holds the key, and the traffic between nodes is the mesh's (ADR
0010). The node can decide by address; a certificate per workload (ADR
0014) is no part of how a tenant says who reaches what.

`feosd` keeps a pod's labels and does not read them (ADR 0004). A pod's
address changes when it resumes on another node (ADR 0010), and a rule
that names a group as its peer has to learn the addresses of that
group's pods on every other node; those pods come and go with scaling,
with every roll, with an idle service resumed elsewhere, with a node
drained.

## Decision

### What a deployment declares, it may do

- **Every reference opens the port it names**, from the object that
  holds it to the object it names:
  - a variable from a service's port: that service, that port;
  - a variable from an addon's output: the addon, the port of its type;
  - a plain string whose URL names a service or an addon as its host,
    on the URL's port, else the scheme's, else, for an addon, its type's;
  - a route: the ingress to the service's port (ADR 0021);
  - a job's references as a service's, for its runs.
- **`uses`** is a reference without a variable, for a program that
  knows its peer by name: `uses: [{service: legacy, port: http}]` or
  `[{addon: db}]` on a service or a job. It means what a variable means
  and nothing more.
- **The reference is the rule.** The plan derives it; nobody writes it,
  and nobody can forget it. It lives as long as the reference: a
  deployment that drops the variable drops the rule.
- **By port name.** `{service: catalog, port: http}` admits whatever
  number `http` is in the same deployment; a changed number changes the
  rule with it.
- **Closed otherwise.** An object nothing references is reached by
  nothing, not by a pod of its own environment. A pod may send to every
  pod of its environment, and the receiver's rules decide: a rule
  between two workloads exists once, read from the caller's
  declaration.
- **A workload's replicas reach each other** on its declared ports (a
  service's ports, an addon's port), without a reference.
- **Nothing else derives.** A secret's value is sealed and names nothing
  the plan sees. A binding is reached at the node's address (ADR 0028).
  Readiness goes over the port forward. An attached service runs in its
  sandbox's pod and reaches it on loopback; its references are its
  sandbox's pod's (ADR 0035).
- **One deployment, one authority.** A reference is the same document,
  planned and applied by one grant on the environment (ADR 0031);
  nothing in an environment is owned apart from the rest. A call is
  written where it is made, and the deployment admits what it declares.
  There is nothing left to write on the receiver.
- **What the platform carries needs no declaration**: DNS from `feosd`,
  the metadata service, the node's address (traces, logs, HTTP
  bindings), readiness checks over the port forward, and the ingress on
  routed ports.

```yaml
services:
  api:
    ports: [{name: http, port: 8080}]
    env:
      CATALOG_URL:  {service: catalog, port: http, key: url}   # api → catalog:http
      DATABASE_URL: {addon: db, role: api, key: url}           # api → db, its type's port
  catalog:
    ports: [{name: http, port: 8080}]
addons:
  db: {type: postgres}
routes:
  api: {path: /api, service: api, port: http}                  # ingress → api:http
```

This environment admits three calls and no other. `catalog` reaches
nothing of it, since it names nothing; `db` admits `api` and nobody
else; the ingress reaches `api` on `http`.

### Egress is the workload's choice

- A service and a job have **`egress`**: `internet` (left out), `none`,
  or what they may reach outside their environment:

  ```yaml
  egress:
    domains: [api.stripe.com, "*.githubusercontent.com"]
    blocks: [{cidr: 203.0.113.0/24, except: [203.0.113.8/29]}]
  ```

- **Domains are enforced by the resolver.** `feosd`'s resolver is the
  one DNS a pod reaches (ADR 0010), and it sees every answer. For a
  name the pod's egress allows, an exact name or one under a `*.`
  suffix, it answers and puts the answer's addresses, of both families,
  into the pod's egress set with a timeout: the answer's TTL, and no
  less than a few minutes, since applications cache addresses longer
  than they may. Generosity costs nothing: nothing enters the set that
  the name was not. For a name the egress does not allow it answers
  REFUSED, so that the application fails at once and not in a timeout,
  and records the refusal (ADR 0013). The names of the pod's own
  environment and of its bindings are not upstream names: egress does
  not touch them.
- **Bypasses close themselves.** Another resolver, DNS over HTTPS, an
  address the program knows: all end at addresses that never entered the
  set, and the chain drops them. No rule for port 53 is needed.
- **Blocks** are an address block of either family with exceptions
  inside it. The denied prefixes come first: no declaration opens them,
  and a block inside them is refused by the plan, not accepted silently.
  `internet` is the egress table: the global unicast blocks
  of both families without the special-purpose ranges, never `::/0` or
  a bare `0.0.0.0/0`.
- **Egress is about what is not the environment.** `none` keeps the
  send to the environment's own pods; it takes away the internet.
- **An addon's** egress is its type's, the internet; the tenant does
  not set it.
- **An image's registry is egress the image declares.** The guest pulls
  a pod's image over the pod's network until the node pulls (ADR 0022,
  ADR 0050), so a pod whose egress restricts names also resolves and
  reaches its image's registry: the registry's host, the hosts a
  registry's tokens and blobs come from, and the names the cluster's
  operator gives for a registry.
- **One limit, named.** The same address may serve several names: a
  pod allowed `a.cdn.example` reaches the address and may name
  `b.cdn.example` in its handshake. Every address-based FQDN firewall
  has it, the clouds' included.

### A route names its clients

- A route has **`from`**: blocks of either family. A pod sees the
  ingress's addresses, not the client's, so the blocks go into the
  ingress's table (ADR 0021), per route. The ingress admits a client of
  the route only when its address is in one of them, and answers
  anything else with 403; its access log says that the route refused
  the client. IPv4 blocks count here: clients come over both families.
- Left out, the route is open to every client while its environment is
  public. An empty `from` is an error: no client could reach the route.
- The client's address is the one the ingress sees on the connection.
  The balancer keeps it, and headers that claim another are never
  trusted.
- A route to an attached service is served by the route door, not by
  the ingress (ADR 0043), and takes no `from`.

### A sandbox and a tenant's job say what they reach

- Their create names **`reaches`**: what of their environment the pod
  may reach, a list of the deployed deployment's objects with a port
  (`{service: api, port: http}`, `{addon: db}`), or `all`, every port
  of every pod of the environment. Left out, nothing of the
  environment: the pod is usable by its token and alone on the
  network. They have `egress` as a service does. ADR 0035 has the rest.
- **A change is no move.** What a sandbox reaches is a rule of its
  targets, in from the sandbox's pod; the sandbox's memberships never
  change, so a changed `reaches` changes rules in place. A tenant's
  job's is fixed: it runs once.
- An entry that names an object the deployed deployment no longer has
  admits nothing and is shown on the sandbox; the plan does not refuse
  a deployment for it.
- Nothing references a sandbox or a tenant's job, so no deployment's
  pod reaches them. A reference to an attached service opens the
  sandbox's pod it runs in (ADR 0035), and a pod whose `reaches` is
  `all` reaches them as it reaches everything of the environment.

### Memberships are the control plane's, never a tenant's

- Every pod is a member of its **environment's** membership, the set
  of all the environment's pods, which admits nothing and has one rule
  out, to itself: a pod may send to every pod of its environment, and
  the receiver's rules decide.
- Every pod is a member of its **workload's** membership: the object's
  pods, or the one pod of a sandbox or a tenant's job. An attached
  service has none of its own: its process runs in its sandbox's pod. A
  workload's rules in are its references' and `reaches`' sources, by
  their memberships, on the ports they name; its rules out are its
  egress: the internet's blocks, its own blocks, and the timed set its
  resolver fills.
- An image service's pod that routes target has a membership of its
  own that admits the **ingress** on its routed ports while its
  environment is public, from the membership of the ingress instances
  (ADR 0021).
- Labels and selectors are the control plane's. `feosd` sees groups,
  members and rules; a tenant sees references, `uses`, `egress`, `from`
  and `reaches`, and never a group.
- A pod's memberships are in its template and never change: its
  environment and its workload. A member leaves a membership only when
  its pod ends or moves. Addresses are never given twice (ADR 0010), so
  a removal that arrives late opens nothing. Only additions are urgent,
  and they are urgent for availability, not for security. What must
  reach every node fast for security is rules, and rules change with
  deployments and with a sandbox's `reaches`.
- The control plane owns the groups and knows their members, at the
  addresses that `feosd` reports. It sends a node the groups of the
  node's pods, and every group that their rules name as a peer, over
  the node's connection (ADR 0015), before the pod that names them,
  whole or as a delta of members from one version to the next; a pass
  of the controller compares what each connected node holds with what
  it should hold and repairs it (faster is ADR 0052). A pod's new
  address reaches the other nodes after a delay, and until then its
  peers there drop its packets; a late delta never lets a pod in under
  another pod's address.

### The node enforces security groups

- A security group has an ID, a version and rules. A rule has a
  direction, in or out; a protocol, TCP, UDP, ICMP, ICMPv6 or any;
  ports, where the protocol has them; and a peer: an address block with
  exceptions, as Kubernetes's `ipBlock` has them, or a security group.
- Rules only allow, and have no order. A pod has one or more groups,
  which the network in its spec names; what one of them allows, the pod
  may do. Everything else is dropped.
- Security groups are stateful: the replies to a connection that a rule
  allowed pass, and so do the ICMP and ICMPv6 errors that belong to it.
- A block is of either family. A pod's IPv4 address is for leaving only
  (ADR 0010), so an IPv4 block matters in rules out; a group as a peer
  stands for its members' IPv6 addresses.
- The cluster's denied prefixes (ADR 0010) come first: no rule opens
  them.
- A pod whose spec names no group gets `feosd`'s default group: out to
  the internet, which is the global addresses of both families; nothing
  in.
- `feosd` refuses a delta that does not start at the version it has;
  the control plane then sends the group whole. `feosd` keeps its groups
  on disk, so that a restart loses none, and a group that a pod names
  cannot be deleted.
- `feosd` does not start a pod whose groups it does not have. A peer
  group it does not have yet allows nothing.
- **nftables enforces it.** Each pod has a chain for what it sends and
  one for what it receives, reached through verdict maps by its TAP.
  Each group that a rule names as a peer is a named set of addresses; a
  block with exceptions is a set of intervals, one per family, the
  difference computed by `feosd`; a pod whose egress restricts names
  has, per family, a set with timeouts that its resolver fills, which
  lives on the node alone and starts empty at every boot or resume of
  the pod. In each chain, what belongs to an allowed connection passes,
  denied prefixes are dropped, the rules of the pod's groups and its
  timed sets accept, and the rest is dropped. A delta changes set
  elements, and a changed rule replaces chains, each in one netlink
  batch: atomically. Packets are dropped in `forward`, before
  connection tracking confirms an entry: traffic that is dropped leaves
  no state.
- `feosd` records every drop with the rule's reason, and every name
  its resolver refused (ADR 0013).

### The plan answers what it derived

- The plan's answer carries the deployment's **reachability**: every
  edge with its port and where it comes from (the variable, `uses`, the
  route), each service's and job's egress as resolved, each route's
  clients. A deployment's record carries the same.
- A plain URL naming a service without a port, or with one the service
  does not declare, is a warning: the call would fail. `uses` naming an
  object or a port the deployment lacks is an error. A domain that is
  not a name, a block in the denied prefixes, a wildcard not at the
  front, `uses` or `egress` on an attached service, `from` on a route to
  one: errors.
- The console's picture of an environment's reachability is drawn from
  the plan's answer, never from traffic.

## Alternatives considered

- **Named security groups of the tenant's own**, as clouds have them:
  named sets that pods are put in, rules that name other groups, ports
  and blocks, in and out, nothing open by default. This was the
  decision for one day. It made two declarations for one fact, kept by
  hand: the plan knew every edge already and warned where no rule
  admitted it, and the warning was pasted. Clouds that know a caller's
  identity offer it as a rule's source and no set beneath; our source
  is the workload, by its address.
- **Groups kept beside the derived rules**, for named sets: two kinds of
  rule for one question, and the second is needed only by a platform
  that cannot derive the first.
- **Kubernetes's NetworkPolicy**, in `feosd` or in the control plane:
  `feosd` would read labels, everything would be allowed until a policy
  selects a pod, which untrusted tenants should not get, and a selector
  is checked only at run time, while a reference is checked by the plan.
- **A list of callers on the receiver** (`services.db.allowFrom:
  [api]`): shorter for three services, but the caller's declaration
  exists already, for the value it resolves to, and a list on the
  receiver would repeat it.
- **A `relations:` section**, edges written by hand: a second grammar for
  what the references and `uses` say.
- **Consent on the receiver's side**, a reference admitting nothing until
  the receiver names the caller: it would protect a service from its own
  deployment, which owns both sides.
- **Egress open for good**: "only the payment provider" and "no egress"
  are what tenants ask a firewall first, and the resolver has every
  answer in hand.
- **Egress as `0.0.0.0/0` and `::/0`**: it would open the pods' mesh and
  every internal network that the denied prefixes leave out on a
  cluster, which is why the egress table never is either.
- **Egress by server name in the data path** (SNI), closing the CDN
  limit: a packet queued to `feosd` per connection, which ADR 0013
  refused for records. `feosd` reads server names for its records
  already and could refuse one it did not allow; that comes when a
  tenant asks for it, not before.
- **Relations learned from traffic**: never. Observed flows are shown
  and proposed; a deployment declares.
- **Egress `none` for addons**: it failed every addon's image pull, since
  the guest pulls over the pod's network; an addon's egress is its
  type's, and an image's registry is egress the image declares.
- **`from` on a route to an attached service**: the ingress checks
  `from`, and it does not serve attached services; the route door does.
- **Ordered rules that allow and deny**, as firewalls have: the rules of
  several groups would need one order between them, a deny in one group
  would silently override another, and deltas would carry positions. An
  agent that writes policies gets unordered allows right more easily.
- **Layers**, as Kubernetes puts AdminNetworkPolicy before NetworkPolicy
  and BaselineAdminNetworkPolicy after it: the operator's ordered rules
  before the tenants', defaults after them. The denied prefixes and the
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
  the network's ID in its addresses, or **an identity in the packet**,
  as Cilium's eBPF identities: rules without sets to send around, but
  one identity per address and no room in a prefix smaller than /64,
  and nothing here needs it.
- **A service mesh**, a sidecar or an ambient proxy with mutual TLS at
  every hop and policies by method and path: a proxy in the data path
  (ADR 0013, ADR 0014). What it gives at L4, the node gives by address;
  what a sidecar gives the application, the guest can give (ADR 0014);
  rules by method and path stay the application's. Where the address
  stops carrying the workload, a call that crosses a cluster, a caller
  from outside the platform, the certificate of ADR 0014 takes over.
- **Membership changed in place, without a roll**, as clouds change a
  network interface's groups: a removal would then be urgent for
  security, and every removal would need an acknowledgement from every
  node before access counts as gone. A roll is cheap for us, and makes
  removals harmless; a pod's memberships never change.
- **A move for a changed `reaches`** of a sandbox: a move is the price
  of a changed membership, and a change of the targets' rules is a
  change of rules, put in place (ADR 0035).

## Consequences

- A tenant writes no network rule. What a deployment references is
  what it reaches; a timeout between two services is a missing
  reference, and the plan names it. An agent is told one sentence:
  reference what you call, and the platform admits exactly that.
- The spec has `uses` and `egress` on services and jobs and `from` on
  routes; a sandbox's and a tenant's job's create has `reaches` and
  `egress`. A deployment that drops a variable drops the rule; a changed
  port number changes it.
- A workload's rules change with deployments and with a sandbox's
  `reaches`, and reach every node that holds them within a pass of the
  controller; the pods stay. A new pod is reached once its address is on
  its callers' nodes, which a pass or a hint brings.
- A pod restricted by name waits for its own DNS answer before its first
  connection to a name, and reaches nothing outside its environment
  that it did not resolve through `feosd`. Addresses answered for one
  allowed name serve every name behind them.
- An addon's pods reach the internet, and a pod with restricted egress
  still pulls its image.
- The ingress's access log says when a route refused a client, and the
  node records every drop and every refused name, in the pod's terms.
- A pod without a group, in labs and tests, reaches the internet and not
  another pod.

## History

Decided on 2026-10-15 (commit 42717c5) as security groups on the node,
with egress by name left open. ADR 0034 of 2026-11-06 gave tenants
security groups of their own on top of them; it was live for one day,
2026-11-07 (5c76142), and was replaced on 2026-11-08 by reachability
derived from a deployment's references (a061348, built in a529381),
which decided egress by name; it is merged here. On the same day a
workload's replicas gained each other's ports and `from` on a route to
an attached service was refused (8ac3322), and addons kept the internet
and an image's registry became egress the image declares (a96cd08,
d2efa44), after egress `none` on addons had failed every pull. The
pushed deltas of ADR 0034's revision are ADR 0052.
