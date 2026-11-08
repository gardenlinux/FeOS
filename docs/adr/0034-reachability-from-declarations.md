# ADR 0034: Reachability from declarations — a deployment's references open the ports they name, a service chooses its egress by domains and blocks, a route names its clients' blocks, a sandbox says what it reaches, and every change reaches every node before the pod's name

**Status:** Proposed · **Date:** 2026-11-06 · **Revised 2026-11-08:**
the first version gave tenants security groups of their own (an object
`securityGroup`, memberships per object, rules written by hand, live
since 2026-11-07); the revision replaces them by what a deployment
declares anyway, and keeps the first version's machinery for the
memberships the control plane derives.

**Touches:** ADR 0012 (unchanged at the node, which gains a timed
egress set per pod; its open question "egress by name" is decided; the
control plane's groups are memberships it derives, never a tenant's,
and they get a batch of deltas), ADR 0018 (a reference gains a meaning
beyond the value it resolves to; the group `env-<id>` becomes the set
of all the environment's pods, with its implied send to them), ADR
0021 (the ingress checks a client's address against a route's `from`;
`ing-<id>` and `ferun-ingress` stay), ADR 0035 (a sandbox's and a
tenant's job's `reaches`, changed in place), ADR 0028 (a binding's
upstream is egress the binding declares), ADR 0032 (the builder's
forms), ADR 0013 and ADR 0026 (drops and refused names reach the sink),
ADR 0036 (unchanged: a workload's certificate serves an addon's
credentials and, one day, a call that crosses a cluster; the network
decides by address), FeCode's ADR 0021.

## Context

ADR 0012 built security groups into the node: stateful rules that only
allow, peers as address blocks with exceptions or as other groups,
enforced by `feosd` with nftables, kept on disk, with deltas of members
from one version to the next. ADR 0018 and ADR 0021 use them for the
platform: every environment has one group, `env-<id>`, and the ingress
reaches an image service's routed ports through `ing-<id>`. Until
2026-11-07 every service of an environment reached every other one,
every pod the whole internet, and a public environment every client;
a tenant had no say in it.

The first version of this ADR gave tenants groups of their own, as
clouds do: named sets that pods are put in, rules that name other
groups, ports and blocks, in and out, nothing open by default. A day of
use showed what they cost:

- **The plan already knows who calls whom.** It reads every reference,
  a variable from a service's port or an addon's output, a URL naming
  one, a route, and where no rule admitted the call it warned with the
  rule to write. In kino, eight services, two addons and a job call
  each other over 25 edges, every one a reference; the groups would
  restate all 25. Nobody wrote them: the projects carry the migrated
  rule, every service reaching every other. An agent that reads the
  warning pastes the rule, so the rules become a copy of the
  references, kept by hand.
- **A tenant's wish is short.** The application server reaches its
  database, A reaches B because B serves A, nothing else gets in. That
  is what a reference says. Egress is open today, and a tenant wants
  "only the payment provider" or "none". A public route wants "only
  the company's network". Everything else that groups have, named
  sets, memberships, limits per object, exists on other platforms
  because they do not know the call graph. We know it.
- The console grew a view, a form and a picker for a thing the
  deployment already says, and FeCode's `fecode.yaml` a section that
  repeats its own environment variables.

Where the identity of a workload stands was settled beside this. Within
a cluster the address is the workload: a pod sends from its own address
only (ADR 0010), a node's /96 arrives only from the node that holds the
key and the traffic between nodes is WireGuard's (ADR 0017), and
`feosd` admits by address (ADR 0012). A certificate per workload (ADR
0014, ADR 0036) serves an addon's credentials without a password in the
pod and, one day, a call that crosses a cluster; it is no part of how a
tenant says who reaches what.

What has not changed since the first version is that **membership is
volatile**. An environment's pods come and go with scaling, with every
roll (surge 1), with an idle service resumed on another node, which
gives it a new address (ADR 0010), and with a node drained. A rule that
admits a workload needs that workload's addresses on every node where a
target runs, and a new pod is useless until they are there: its
callers' packets are dropped. `env-groups` puts groups whole, every
10 s or on a hint, comparing each connected node's list with what it
should have: a member's change costs every node the whole group, and is
as slow as the loop.

## Decision

### What a deployment declares, it may do

- **Every reference opens the port it names**, from the object that
  holds it to the object it names:
  - a variable from a service's port (`{service, port}`, whichever key
    it takes): that service, that port;
  - a variable from an addon's output (`{addon, …}`): the addon, the
    port of its type (5432, 6379, 4222);
  - a plain string whose URL names a service or an addon as its host,
    with the URL's port;
  - a route: the ingress to the service's port, as `ing-<id>` carries
    it (ADR 0021);
  - a job's references as a service's, for its runs.
- **`uses`** is a reference without a variable, for a program that
  knows its peer by name: `uses: [{service: legacy, port: http}]` on a
  service or a job. It means what a variable means and nothing more.
- **The reference is the rule.** The plan derives it; nobody writes it,
  and nobody can forget it. It lives as long as the reference: a
  deployment that drops the variable drops the rule.
- **By port name.** `{service: catalog, port: http}` admits whatever
  number `http` is in the same deployment; a changed number changes the
  rule with it.
- **Closed otherwise.** An object nothing references is reached by
  nothing, not by a pod of its own environment. A pod may send to every
  pod of its environment, and the receiver's declared references decide:
  a rule between two workloads exists once, read from the caller's
  declaration, and `env-<id>` carries the implied send.
- **Nothing else derives.** A secret's value is sealed and names nothing
  the plan sees. A binding is reached at the node's address (ADR 0028).
  Readiness goes over the forward. An attached service runs in its
  sandbox's pod and reaches it on loopback; its references are its
  sandbox's pod's (ADR 0035).
- **One deployment, one authority.** The first version put the rule on
  the receiver's side because a tenant wrote it there. A reference is
  the same document, planned and applied by one grant on the
  environment (ADR 0031); nothing in an environment is owned apart from
  the rest. A call is written where it is made, and the deployment
  admits what it declares. There is nothing left for the tenant to
  write on the receiver.
- **What the platform carries needs no declaration**, as before: DNS
  from `feosd`, the metadata service, the node address `169.254.169.1`
  (traces, logs, HTTP bindings), readiness checks over the port
  forward, and the ingress on routed ports.

```yaml
services:
  api:
    ports: [{name: http, port: 8080}]
    env:
      CATALOG_URL:  {service: catalog, port: http, key: url}   # api → catalog:http
      DATABASE_URL: {addon: db, role: api, key: url}           # api → db:5432
  catalog:
    ports: [{name: http, port: 8080}]
addons:
  db: {type: postgres}
routes:
  api: {path: /api, service: api, port: http}                  # ingress → api:http
```

This environment admits three calls and no other. `catalog` reaches
nothing of it, since it names nothing; `db` admits `api` on 5432 and
nobody else; the ingress reaches `api` on `http`.

### Egress is the service's choice

- A service and a job have **`egress`**: `internet` (left out, as
  today), `none`, or what they may reach outside their environment:

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
  than they may (Java). Generosity costs nothing: nothing enters the set
  that the name was not. For a name the egress does not allow it
  answers REFUSED, so that the application fails at once and not in a
  timeout, and records the refusal (ADR 0013): "`api` asked for
  `evil.example`, not in its egress".
- **Bypasses close themselves.** Another resolver, DNS over HTTPS, an
  address the program knows: all end at addresses that never entered the
  set, and the chain drops them. No rule for port 53 is needed.
- **Blocks** are an address block of either family with exceptions, as
  ADR 0012 has them. `--deny-prefix` comes first, as before: no
  declaration opens it. A block in the private ranges is refused at the
  plan for as long as the cluster denies them, and is not accepted
  silently. `internet` is the egress table of ADR 0018, the global
  unicast blocks of both families without the special-purpose ranges,
  never `::/0` or a bare `0.0.0.0/0`.
- **Egress is about what is not the environment.** `none` keeps the
  implied send to the environment's own pods; it takes away the
  internet.
- An **addon's** egress is its type's, none today; the tenant does not
  set it.
- **A binding's upstream is egress the binding declares.** Once a
  binding dials from the pod's address (ADR 0014's second address, not
  built), its upstream's host is in the pod's egress by the binding
  itself: what is declared is allowed, there too.
- **One limit, named.** The same address may serve several names
  (CDNs): a pod allowed `a.cdn.example` reaches the address and may name
  `b.cdn.example` in its handshake. Every address-based FQDN firewall
  has it, the clouds' included. `feosd` reads server names for its
  records already (ADR 0013) and could refuse one it did not allow; that
  comes when a tenant asks for it, not before.

### A route names its clients

- A route has **`from`**: blocks of either family. A pod sees the
  ingress's addresses, not the client's, so the blocks go into the
  ingress's table (ADR 0021), per route. The ingress admits a client of
  the route only when its address is in one of them, and answers
  anything else with 403; its access log says that the route refused
  the client. IPv4 blocks count here: clients come over both families.
- Left out, the route is open to every client while its environment is
  public. The plan shows which is the case.
- The client's address is the one the ingress sees. The balancer keeps
  it (`externalTrafficPolicy: Local`), and headers that claim another
  are never trusted.
- The ingress still reaches its targets through `ing-<id>`, as now.

### A sandbox and a tenant's job say what they reach

- Their create names **`reaches`**: what of their environment the pod
  may reach, a list of the deployed deployment's objects with a port
  (`{service: api, port: http}`, `{addon: db}`), or `all`, every port
  of every pod of the environment, which is what FeCode's workspace had
  through its group. Left out, nothing of the environment: a sandbox
  made without it is usable by its token and alone on the network. They
  have `egress` as a service does. ADR 0035 has the rest.
- **A change is no move.** `PUT /api/v1/sandboxes/{id}/reaches` replaces
  `…/security-groups`. What a sandbox reaches is a rule of its targets,
  in from the sandbox's pod, and rules change in place and are
  acknowledged per node (below); the sandbox's membership, its
  environment and itself, never changes. ADR 0035's move was the price
  of a change of membership, and there is none left. A hibernated
  sandbox takes a new `reaches` at its restore, as it takes everything.
  A tenant's job's is fixed: it runs once.
- An entry that names an object the deployed deployment no longer has
  admits nothing and is shown on the sandbox; the plan does not refuse
  a deployment for it.
- Sandboxes and jobs are never referenced: nothing of the environment
  reaches them, except through an attached service's port (ADR 0035),
  which is the sandbox's pod's.

### Memberships on the nodes, and they last as long as the pod

- Every pod is a member of `env-<id>`, the set of all the environment's
  pods, with no `in` rules and the one `out` rule to itself, and of a
  membership of its workload, `wl-<id>`: the object's pods, or the one
  pod of a sandbox or a tenant's job. The ingress keeps `ing-<id>` and
  `ferun-ingress` (ADR 0021). The first version's `sg-<id>` and its rows
  go.
- A workload's `in` rules are its references' and `reaches`' sources, by
  their memberships, on the ports they name; its out is its egress: the
  internet sets, its blocks, and the timed set its resolver fills.
  `feosd` sees groups, members and rules as ADR 0012 has them, and gains
  the timed set and the resolver's part.
- A pod's memberships are in its template and never change: its
  environment and its workload. A member therefore leaves a membership
  only when its pod ends or moves. Addresses are never given twice (ADR
  0010), so a removal that arrives late opens nothing. Only additions
  are urgent, and they are urgent for availability, not for security.
  What must reach every node fast for security is rules, and rules
  change with deployments and with a sandbox's `reaches`.

### Changes reach the nodes as deltas, pushed, in order

- **Every member change is a delta in a log.** In the transaction that
  records a pod's address (its create, its move, its end), `ferund`
  increments the version of each of the pod's memberships under the
  row lock, appends the change (`version`, added, removed) to the log,
  and notifies. A move is one delta: the old address out, the new one
  in.
- **The replica that holds a node's connection pushes to it.** It knows
  which memberships the node holds: those its pods are members of and
  those their rules name (`node_security_groups`, with the version the
  node acknowledged). On a hint it takes every pending change of those,
  folds each one's changes into one delta from the node's version to
  the newest, and sends them in one call. `feosd` applies them in one
  netlink batch and one write to disk, and the call's success is the
  acknowledgement. A node that is out of the hint's reach wakes on the
  next anti-entropy pass.
- **`feosd` gains a batch**, `UpdateSecurityGroups`: whole groups and
  deltas of several groups, all or none, as one nftables transaction.
  A delta that does not start at the version `feosd` has fails the
  batch with the groups it concerns, and the sender puts those whole
  and resends the rest.
- **A member before its name, on every node.** Name tables (ADR 0018)
  travel over the same connection, through the same sender, after the
  memberships. The delta that adds an address is sent and acknowledged
  before the name-table delta that names it. On every node, a caller
  resolves a new replica only once the node admits it. No barrier
  across nodes is needed, and a slow node delays only its own callers.
  Removals go the other way: the name first, then the member.
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
- **`env-groups` stays as anti-entropy.** Every 10 s the leader
  compares versions, no longer content, per connected node, and repairs
  what a missed hint or a failed push left behind.
- **Target:** for 200 member changes a second in a cluster, a change is
  acknowledged by every connected node that holds the membership within
  1 s at the 99th percentile. `ferund` measures this from the commit to
  each acknowledgement (`ferun_security_group_propagation_seconds`,
  and a span per push).

### A drop says which declaration is missing

- `feosd` records drops and refused names (ADR 0013). `ferund` maps
  the two ends to workloads and puts them into the sink as records of
  `source: network`, in the deployment's words: "`orders` →
  `payment`:8080 refused: `orders` names no `payment`"; "`api` asked
  for `evil.example`: not in its egress", per pair and port, rate
  limited.
- A first refusal after a deployment is an incident on the object. A
  rule that drops without saying so is a rule nobody can debug, least
  of all an agent.

### The plan, the status and the console

- The plan's answer gains **`reachability`**: every edge with its port
  and where it comes from (the variable, `uses`, the route, a sandbox's
  `reaches`), each service's egress as resolved, each route's clients.
  A plain URL naming a service without a port, or with one the service
  does not declare, is a warning: the call would fail. `uses` naming an
  object or a port the deployment lacks is an error. A domain that is
  not a name, a block in the private ranges, a wildcard not at the
  front: errors. The first version's warning goes.
- A deployment's status says on how many of the nodes that hold them
  its rules are acknowledged.
- The console's picture of an environment's reachability is drawn from
  the plan's answer: an edge per reference with the variable it comes
  from, the routes' clients, each service's egress. The group forms of
  the builder go; the service's and the job's forms gain `egress` and
  `uses`, the route's `from`, the sandbox's create `reaches` and
  `egress`. Once the nodes' records reach the control plane (ADR 0013,
  not shipped today), refused flows and refused names lie over the
  picture, and the console proposes the reference; it never opens
  anything from what it saw.

### FeCode

- `fecode.yaml` loses `securityGroups` and the objects' `securityGroups`
  and gains nothing it did not have: references are FeRun's, passed
  through; `egress`, `uses` and a route's `from` pass through as well.
  The dev render appends nothing. The workspace's sandbox is made with
  `reaches: all` in its dev environment, project sessions' sandboxes
  and build jobs with nothing (they need the internet, which `egress`
  gives). The agent is told one sentence: reference what you call, and
  the platform admits exactly that; a timeout between two services is a
  missing reference, and the plan names it. FeCode's ADR 0021 is
  amended.

### One release, no mode

- The plan refuses `securityGroups` as it refuses any field it does not
  know. The migration strips it from every stored spec, latest and
  deployed, and drops the rows of `security_groups`: the stored specs
  hold nothing but the first version's migrated rule (`default` admitting
  `default`) and FeCode's `workspace` group, and both are replaced by
  what this revision derives. The derived rules admit every call those
  environments make, since their calls are references; an environment
  made closed since 2026-11-07 gains the rules its references always
  meant.
- Every pod's template loses `sg-<id>` and gains `wl-<id>`, which rolls
  every image service once, as the first version's release did.
- A sandbox's `securityGroups` become `reaches`: one in FeCode's
  `workspace` gets `all`, one in `default` gets nothing. `attachable`
  stays gone, the tenant's environment `sandboxes` stays (ADR 0035).
- FeCode removes the migrated rule from the projects it wrote it into,
  and the `securityGroups` of its own renders, in the same release.

## Alternatives considered

- **Named groups of the tenant's own**, the first version of this ADR:
  two declarations for one fact, kept by hand; the warning that asked
  for the second was pasted, and the migrated rule stayed. Clouds that
  know a caller's identity offer it as a rule's source (a firewall rule
  from a service account) and no set beneath; our source is the
  workload, by its address.
- **Groups kept beside the derived rules**, for named sets: two kinds of
  rule for one question, and the second is needed only by a platform
  that cannot derive the first.
- **Kubernetes's NetworkPolicy in the control plane**, selectors over
  labels: everything is allowed until a policy selects a pod, which ADR
  0012 rejected for untrusted tenants, and a selector is checked only at
  run time, while a reference is checked by the plan.
- **A list of callers on the receiver** (`services.db.allowFrom:
  [api]`): shorter for three services, but the caller's declaration
  exists already, for the value it resolves to, and a list on the
  receiver would repeat it.
- **A `relations:` section**, edges written by hand: a second grammar for
  what the references and `uses` say.
- **Consent on the receiver's side**, a reference admitting nothing until
  the receiver names the caller: it would protect a service from its own
  deployment, which owns both sides.
- **Egress open for good**, as until now: "only the payment provider"
  and "no egress" are what tenants ask a firewall first, and the
  resolver has every answer in hand.
- **Egress as `0.0.0.0/0` and `::/0`**: it would open the pods' mesh and
  every internal network that `--deny-prefix` leaves out on a cluster
  (ADR 0018 says why the egress table never is either).
- **Egress by server name in the data path** (SNI), closing the CDN
  limit: a packet queued to `feosd` per connection (ADR 0013 refused it
  for records). A refusal of a name the egress did not allow, from the
  copies `feosd` takes already, can come later without changing
  anything here.
- **Relations learned from traffic**: never. Observed flows are shown
  and proposed; a deployment declares.
- **A move for a changed `reaches`** (ADR 0035's first version): a move
  was needed because membership changed under the pod; a change of the
  targets' rules is a change of rules, which is put in place.
- **Membership changed in place, without a roll**, as clouds change a
  network interface's groups: a removal would then be urgent for
  security, and every removal would need an acknowledgement from every
  node before access counts as gone. A roll is cheap for us, and makes
  removals harmless; and a pod's memberships never change now.
- **A barrier across nodes before a pod counts as ready**: strict, but
  one slow node would stall every roll. Ordering members before names
  on each node gives the same thing where it matters, and the ingress
  already waits for its own admission (ADR 0021).
- **A faster loop of whole groups**: the loop's interval is the floor
  of the latency, and every change costs every node the whole group.
- **The leader pushing to every node**: one hop more through the
  replica that holds the connection, and one process for all of the
  cluster's changes.
- **A service mesh**, a sidecar or an ambient proxy with mutual TLS at
  every hop and policies by method and path: a proxy in the data path
  (ADR 0013, ADR 0036). What it gives at L4, the node gives by address;
  what a sidecar gives the application, the guest can give (ADR 0036);
  rules by method and path stay the application's. Where the address
  stops carrying the workload, a call that crosses a cluster, a caller
  from outside the platform, a server that wants its callers' names in
  band, the certificate of ADR 0036 takes over; none of it is in a
  cluster today.
- **An identity in the packet** (eBPF and identities, as Cilium has
  them): ADR 0012 rejected it, and nothing here needs it.
- **An environment-wide egress default** (`egress` on the environment,
  the services narrowing it): wanted, and an open question; a service's
  own field is the first step.

## Consequences

- **The spec** loses `securityGroups` everywhere and gains `uses` and
  `egress` on services and jobs, `from` on routes. The sandbox's and the
  tenant's job's create gain `reaches` and `egress`; `PUT
  …/security-groups` becomes `PUT …/reaches`.
- **The plan** derives reachability from references, routes, `uses`
  and `reaches`, answers it, checks egress and `from`, and refuses the
  old field.
- **`ferund`** keeps memberships per environment and per workload,
  writes each workload's rules from the derived edges and its egress,
  puts a pod's egress (domains, blocks) into its network spec, and gains
  the log of member changes, a sender per node connection for
  memberships and name tables, the propagation metric and status per
  deployment. The rows and loops of `security_groups` and the move of
  ADR 0035 go.
- **`feosd`** gains `UpdateSecurityGroups`, a version per group in its
  list that anti-entropy can compare without content, and, per pod, the
  list of allowed names and a timed egress set: the resolver answers
  allowed names into the set and REFUSED to the rest, with a record.
  `PodNetwork` gains the names and blocks. `feos.network.v1` is
  otherwise unchanged.
- **`ferun-ingress`'s** table carries each route's blocks and its access
  log the refusal.
- **The console** draws the picture from the plan's answer and loses
  the group forms and picker; the sandbox's create gains `reaches` and
  `egress`.
- **FeCode** drops `securityGroups` from `fecode.yaml` and its renders,
  makes the workspace's sandbox with `reaches: all`, passes `egress`,
  `uses` and `from` through, and rewrites the projects' files once.
- **The release** strips the field from stored specs, drops the rows,
  rolls every image service once, and ships `feosd` first (the timed
  set), then `ferund` and FeCode together, as ADR 0035's did.
- Drops and refused names of the tenants' pods reach the sink, which
  ADR 0013 has not sent so far. FeCode's Ops view can then show them.
- The architecture pages (control plane, node, ingress, API) follow the
  build.

## Open questions

- **An environment-wide egress default**, above.
- **UDP and other protocols**: a reference names a TCP port; a service's
  port has no protocol in the spec yet.
- **References across environments** (stage reaching a shared
  service): naming another environment's workload, and whose deployment
  admits it. It is a question of tenancy and of naming environments in
  another's spec, and nothing here prevents it.
- **A tenant's private networks**: `--deny-prefix` denies the private
  ranges. Reaching a customer's network over a VPN or peering needs a
  route as well as a block, and the deny list would then have to allow
  that tenant's block.
- **An addon's egress** once addons back up to the object store or
  replicate to another region.
- **Refusing server names** the egress did not allow, to close the CDN
  limit.
- **A URL in a secret**: a service whose peer's address is sealed
  derives nothing; whether a secret may name the workload it points at.
