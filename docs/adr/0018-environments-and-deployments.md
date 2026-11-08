# ADR 0018: An environment changes only by deployments: planned, numbered and immutable

**Status:** Accepted · **Date:** 2026-10-18

An environment is a tenant's set of typed objects, services, jobs,
addons, routes, secrets and bindings, that reference each other by name;
its ID is its identity and its name a handle that can change. It changes
only by deployments: numbered, immutable records of the whole desired
state, each planned before anything is written, with findings a model
can act on, its images pinned to digests and its secrets to versions, a
base for compare-and-swap, a revert and tags. The controller works
toward the latest deployment and keeps the last deployed one serving
when a rollout fails; what it cannot resolve is an incident with
evidence. The environment's names are answered by `feosd`'s resolver
from tables the control plane puts on the nodes, and an environment is
cloned from a deployment. FeCode, an agent, a person and a CI drive it
through one API.

## Context

ADR 0001 makes CaaS a product with an API of its own, modelled on what
Kubernetes does for a pod and not on Kubernetes, and puts "services with
replicas and routes" in the control plane. ADR 0004 gives a pod what a
service needs, ADR 0016 persistent volumes, ADR 0015 the control plane
in replicas over PostgreSQL, whose sandboxes settled the shape of
everything in it: a row is the truth, every change an op under the
row's lock, a spec pinned by digest and the same on every retry.

Two callers operate CaaS. The first is an agent in a FeCode sandbox,
writing an application and needing to say how it runs: which services,
which databases, which routes, and to learn at once whether what it said
holds. The second is a person: deploying a version, reading what changed
in production last month, copying an environment for a new region or a
customer's demo. A third, a customer without FeCode, drives the same
thing from a CLI or a CI.

What happens without the platform is on record. An agent built a demo
application in FeCode, Expensa: five services, Postgres, NATS. It wrote
an ADR about running it, a compose file, a Caddyfile, an init SQL for
roles and schemas, and, because the sandbox has no Docker, a shell
supervisor of three hundred lines that installs Postgres from apt,
builds `nats-server`, starts the services with PID files, waits for
their health, keeps logs and proxies ports in place of Caddy. The
compose file never ran. Every line of that work is the platform's work,
done again per project, never the same twice and never what production
runs. An agent routes around a missing platform by building one; the
platform has to be where the agent is, with the service graph as objects
it can state and get an answer for at once.

## Decision

### An environment holds typed objects

- An **environment** is the unit of CaaS in `ferund`: a tenant's, with a
  name that is a DNS label unique in the tenant and an ID that is its
  identity, and a name space in which its objects address each other by
  name. Either names it in a path.
- It holds objects, each with a name unique in the environment: a
  **service**, a pod template per ADR 0004 with its image pinned to a
  digest, replicas, the ports it serves, a readiness check and an idle
  policy after which its pods are suspended and resumed by the next
  request, or a process in a sandbox when its runtime is `attached`
  (ADR 0042); a **job** (ADR 0041); an **addon**, a managed backing
  service from a blueprint (ADR 0040); a **route**, a path to a port of
  a service (ADR 0043, ADR 0021); a **secret** (ADR 0044); a **binding**
  (ADR 0028). Services have no volumes of their own: data lives in
  addons, and a spec that mounts a volume into a service is refused
  with a finding that says so.
- Objects reference each other by name, and the references are resolved
  when a write is validated: a service's environment variable from an
  addon's output, from a secret or from another service's port, a
  route's service and port. A dangling reference is an error of the
  write, not a pod that fails at start. A service's reference resolves
  to its name; an addon's to the addon's outputs, so that a dependent is
  rolled when they change. What an object references, it may reach
  (ADR 0012).
- A service and an addon do not share a name: both are names of the
  environment.

### The name is a handle, the ID the identity

- Everything `ferund` keeps or makes for an environment is keyed by its
  ID. The name is for people and a second way to address the
  environment in a path.
- A rename gives the environment a new name in one call, checked as a
  create's name is; a name another environment of the tenant has is a
  conflict, the name it has already changes nothing, and the old name is
  free for another environment at once. It is the environment admin's
  or the tenant writer's, as a deletion is.
- A rename leaves the ID, labels, deployments, tags, secrets, grants,
  tokens and domains, the default host whose label was claimed at the
  first publish, every pod, template, group and name table, the
  attached services' processes, and the logs, traces and traffic, which
  the sink labels with the ID; nothing rolls. It changes the name in
  every answer and message, the path that names it and the console's
  URLs. There are no aliases.

### Every write is a deployment

- The objects of an environment are not changed in place. A write, an
  `apply` of the whole desired state or an operation on one object that
  `ferund` completes to the whole, produces a **deployment**: a number,
  monotonic per environment; the complete desired state, normalised;
  who, when, and a message; the deployment it was based on; and
  annotations, a small string map the caller keeps with it. Deployments
  are immutable and kept.
- **Plan** takes the same input as a write and answers what the write
  would do: its validation findings, each with the path of the object
  and field it concerns, the problem and what would make it pass, and
  the diff against the current deployment, object by object, with what
  each change does to the running pods. Plan writes nothing. Apply runs
  the same validation and, when it passes, records the deployment. The
  findings are worded for whoever writes the objects, a model included.
- Apply pins every image to its digest and every referenced secret to
  its latest version, and stores the deployment so: a deployment names
  what it runs, and a revert runs the same bytes. Plan only warns about
  an image that does not resolve; apply refuses it.
- A write names the deployment it was planned against, its **base**.
  When the environment has moved since, the write fails with a conflict
  that names the current deployment, and the caller plans again. A
  write that changes nothing answers with the current deployment,
  whatever its base, unless that deployment failed: then the same spec
  makes a new deployment, which is how a failed one is tried again.
- **Revert** to deployment K is a new deployment with K's content and
  K's annotations, marked as K's revert; it takes no annotations of its
  own. A **tag** names a deployment and moves only by an explicit call,
  which is recorded. The history of an environment is the list of its
  deployments with their diffs: what changed in production in October is
  a query.
- A deployment is **waiting** once applied, **deploying** while the
  controller brings the environment to it, **deployed** once every
  object matched it and was ready, **failed**, or **replaced** by a
  later one; an environment's deployed deployment is its latest
  deployed one, and a pod that no longer serves and goes after its grace
  is **stopping**. The word is deployment, not release: in FeCode a
  release is a version its people tag.

### The controller works toward the latest deployment

- An environment's pods are sandboxes: each instance, a service's
  replica, an addon, a job's run, is a row of the sandboxes' registry
  that names its environment and carries a pod template, so that the
  sandboxes' engine places, starts, moves, suspends, hibernates and
  deletes it with its ops, locks, tombstones and holds, unchanged. The
  controller inserts and deletes those rows and keeps its own view of
  them. The pods' labels carry environment, object and template, not
  the deployment: an object a deployment does not change keeps its pods.
- A deployment's rollout provisions its addons, runs its deploy jobs,
  then moves each service replica by replica, a new pod ready before an
  old one goes, routes following readiness, and brings its attached
  services up. It is deployed when every object matches it; the status
  names the deployed deployment and the one in progress.
- A rollout that does not reach readiness within its budget has
  **failed**: the service keeps the pods of the last deployed deployment
  serving, the new ones are removed, and the environment stays on its
  failed deployment until the next one, a revert or a fix. Each service
  has a budget (`rollout.timeout`), and the deployment one beyond them,
  the longest of its services' budgets, its deploy jobs' time with their
  retries and a floor, after which it fails naming what it still waits
  for, so that a deployment that waits for an addon or for capacity
  never rolls forever. The budgets count from the deployment's start,
  moved on by the time it waited for another deployment's run of a job
  (ADR 0041). `ferund` writes no deployment of its own: whether a failed
  rollout is reverted at once is the caller's policy, not a mechanism
  that invents history.
- The controller heals what it runs: a lost replica is replaced, and
  until the guest supervises containers (ADR 0048) `ferund` runs the
  readiness checks over the node's connection and restarts an exited
  container by restarting its pod, with a backoff, counting repeated
  exits as a crash loop.
- What the reconciler cannot resolve ends in an **incident** on the
  environment: the object, the condition (a failed rollout, a crash
  loop, a failed job, no capacity, an addon that cannot run on its data,
  a volume that cannot attach) and the evidence `ferund` has: the pod's
  last exits and restarts, its log tail redacted of the environment's
  secrets and bounded, the placement's verdict, what the deployment
  waited for, the diff since the last deployed deployment. An incident
  is opened once per object and deployment and closed when the
  condition clears. Incidents are the hook for whoever operates: a
  person, or FeCode's agent.

### The environment's names

- An environment's names are its addons, image services and attached
  services. `ferund` keeps a name table per environment that names each
  with the addresses of all its live replicas, never a retiring one's,
  and while a service has no live replica the best it has, so that a
  name does not go during a roll. The table reaches the nodes whole or
  as a delta, versioned by a hash of its content, before the pod that
  names it, and `feosd`'s resolver answers the names of a pod's tables
  itself with a short TTL (ADR 0010). The guest takes no part: a pod
  that was paused, suspended, hibernated or moved resolves its names as
  soon as it runs again. A node whose `feosd` does not serve tables runs
  the pods without names, and an incident says so.

### An environment is copied from a deployment

- `clone` makes a new environment from a deployment of another: the
  source deployment's objects, or a whole spec of the caller's, which is
  how a copy gets other plan sizes or replicas. Secrets are copied only
  where each is named. Attached services are not copied: they run in a
  sandbox of the source. A clone's addons start empty. A copy for a new
  region and a demo for a customer are a call and a plan; idle, the copy
  costs what suspended pods cost.

### One API for every caller

- FeCode's backend for its projects' environments, an agent through its
  tools (ADR 0031), a person through the console or a command line, a
  customer's CI: all use the same API, and it accepts objects. A bundle,
  the whole desired state as one JSON document, is one input of `apply`
  and the export of a deployment. GitOps is then a way a caller chooses
  to drive the API, not the architecture.

## Alternatives considered

- **GitOps: the declaration as a file in the repository, a controller
  that pulls it** (Flux). What Git gives, history, rollback, a draft per
  branch, the declaration travelling with the code, the deployment
  model gives too, except the last, which FeCode handles at its landing.
  What Git lacks is what the operator asks for: history per
  environment, a copy of an environment, tags on states, and above all
  an answer at the moment of writing. The file would need a format, a
  validator in the agent's loop, and a second store for the secrets and
  each environment's pointer regardless; its history would be the
  history of one file. A caller who wants Git in front runs `apply`
  from its CI.
- **Compose as the declaration.** The most natural thing a model writes,
  and it says nothing of managed addons, routes, scale to zero,
  environments or secrets by environment; what we added would live in
  `x-` extensions, which is where the platform's semantics would be. As
  an import it is useful; as the API it would make the platform's
  objects second-class.
- **Mutable objects without deployments**, Kubernetes's model: every
  change in place, `resourceVersion` for concurrency, history nobody
  keeps. Nomad's job versions with `plan`, `history` and `revert`,
  Heroku's releases and CloudFormation's change sets are the idea here.
- **Drafts in `ferund`**: a stored desired state the reconciler ignores
  until it is applied. The draft is the caller's, FeCode's workspace or
  a CI's branch. Plan with a `base` gives what a stored plan is for
  without an owner, a lifetime and a sweep for drafts.
- **An automatic revert in `ferund`.** A deployment the operator did not
  make, in a history that is supposed to be theirs. The mechanism,
  keeping the last deployed deployment serving, is `ferund`'s; the
  policy is the caller's.
- **A generic store of kinds** with a schema per kind, as custom
  resources are: the generality of Kubernetes without its ecosystem. A
  few kinds, typed and validated against each other, are what the
  callers need.
- **Instances in tables of their own**, with a reconciler beside the
  sandboxes' engine: a second engine for placement, bookings, ops,
  hibernation, drain and tombstones, which the sandboxes have settled.
- **The deployment's number as a label on every pod**: an object a
  deployment does not change would be rolled for nothing.
- **A block of `/etc/hosts` that `ferund` writes into every pod**, the
  first form of the names: it needs nothing of the node, but holds one
  address per name, so that one replica takes every connection; a file
  has no TTL, and a name follows a roll only once the block is written
  again; every change is an exec into every pod of the environment and
  every sandbox of its attached services; a sandbox that sleeps keeps a
  stale block and is woken to take a name out. A table on the node
  changes once per node, holds every replica, expires in seconds and
  needs nothing of the guest.
- **A file the guest writes**, from what the node pushes over vsock: the
  same file without the exec, and a guest that has to hear of every
  change while it runs and cannot while it sleeps.
- **A name table per security group**, without a field in the spec: a
  group says what a pod reaches and a table what it calls by name, and
  every group would become a table on every node that has it.
- **A clone that takes values to change** (plan sizes, replicas) instead
  of a whole spec: a caller that wants other values renders its own
  spec and passes it, as FeCode does.
- **"Release", "realised", "superseded", "pending", "rolling"** as the
  words, taken from this decision's prose: people who run deployments
  know the words of their tools, FeCode shows FeRun's words as they are,
  and a FeCode release is a version its people tag; the words were
  changed everywhere on 2026-11-02.
- **Clone and delete instead of a rename**: a new ID, new hosts, addons
  empty, grants, tokens and logs left with the old environment.
  **Aliases**, the old name still resolving: a name would mean two
  environments over time and keep names taken nobody uses. **A display
  name beside a fixed name**: the console, the messages and FeCode's
  operators read the name.

## Consequences

- The sandboxes' patterns carry over: rows, ops, pinned specs,
  tombstones. The node knows nothing of environments or deployments; it
  sees pods, volumes, security groups and name tables, and labels carry
  the rest.
- An environment's pods are no tenant's sandboxes and are absent from
  its sandbox routes; a sandbox is not an object of an environment, an
  attached service names one.
- A caller that stores an environment's name or builds URLs with it
  must store the new one after a rename; by ID nothing changes, and the
  rename is idempotent, so a client retries it.
- A caller that wants a text declaration edits one through the API,
  validated per operation, and exports the deployment. FeCode's side, a
  definition per project, environments per class, promotion and what
  its agent may apply, is FeCode's.
- The REST API grows with the objects; the console shows environments,
  their deployments, incidents and logs, and edits a draft of the next
  deployment (ADR 0032).

## History

Proposed and accepted as built on 2026-10-18 (add3bb0, 3175001,
abd2c87); names moved from a block of `/etc/hosts` into the nodes' name
tables on 2026-10-19 (49a1ba2). The states' words and the word
deployment replaced release, realised and superseded on 2026-11-02
(e3743d9, fa41479, 53c9df2). ADR 0035 (2026-11-07) put sandboxes and
jobs into environments, and ADR 0034's revision (2026-11-08) replaced
the environment's own security group and egress table by reachability
derived from references. ADR 0037, the rename of 2026-11-08 (8951c44),
is merged here; addons, jobs, attached services, the route door and
secrets were split into ADR 0040 to 0044 on 2026-11-08.
