# ADR 0035: Everything runs in an environment: sandboxes and tenants' jobs belong to one

**Status:** Accepted · **Date:** 2026-11-07

A sandbox and a tenant's job are made in an environment, which their
create names and which is fixed for the pod's life; they are not
objects of the spec, so making one is no deployment. They say what of
the environment they reach, as ADR 0012 has it, and are otherwise
reached by nothing. An attached service runs in a sandbox of its own
environment, and every sandbox can run one. Grants on the environment
cover its sandboxes and jobs, and an environment with sandboxes or
running jobs is not deleted.

## Context

An environment holds what a deployment declares: services, jobs,
addons, routes, volumes, secrets, bindings. Two kinds of pod are made by
a call instead:

- a **sandbox**, the tenant's, with its own idle, pause and
  hibernation, reached by exec and its port door;
- a **tenant's job** (ADR 0041), one pod run once, a build or one-off
  maintenance.

In `ferund` they are rows of the same registry as an environment's
instances, placed, admitted and suspended by the same engine. If only
the API kept them apart, the separation would cost:

- **Network.** A pod outside every environment can be in the node's
  default group, out to the internet and nothing in, or reach everything
  of the environments it serves. "This sandbox may reach the database,
  nothing else" cannot be said, and a sandbox that serves several
  environments mixes their names and ports. A pod whose groups change
  while it runs is the one exception to ADR 0012's rule that
  memberships never change.
- **Grants.** Sandboxes, tenants' jobs and attached services would
  belong to a tenant grant alone (ADR 0031), because a sandbox might
  reach any of the tenant's environments. A person with `write` on one
  environment could deploy there but not start a pod beside it: no
  shell next to the database, no migration run by hand, no preview.
- **Logs, traces, deletion.** What the sink keeps of a sandbox would be
  the tenant's, not an environment's; a person who reads one
  environment would not see the sandbox that runs its preview. An
  environment that is deleted would leave its sandboxes behind.

What is wanted is the namespace of Kubernetes: one place where a
tenant's declared workloads and its single pods run side by side, under
the same rules for network, names and access.

## Decision

### A sandbox and a tenant's job belong to an environment

- A sandbox's and a tenant's job's create names its **environment**,
  by name or ID. It is required and fixed for the pod's life. There is
  no sandbox or tenant's job without an environment.
- They are **not objects of the spec.** A deployment declares what
  should run; a sandbox and a tenant's job are made by a call and live
  until deleted, ended or expired. Making one creates no deployment,
  and a deployment never makes, changes or removes one. An environment
  without deployments is a valid place for them: a room with nothing
  declared.
- Their IDs are the cluster's (ADR 0019), so their paths do not carry
  the environment; a list takes the environment as a filter, and
  without it lists the tenant's.
- A sandbox's token, its exec, its tools, its port door, its idle,
  pause and hibernation do not depend on its environment. `/mcp` on a
  sandbox's token is FeCode's contract (ADR 0031) and is untouched by
  it.

### They say what of the environment they reach

- The create takes `reaches` and `egress` as ADR 0012 has them: a list
  of the deployed deployment's objects with a port, or `all`; left out,
  nothing of the environment. An entry naming an object the deployed
  deployment does not have is refused with the names it has; one that a
  later deployment removes admits nothing and is shown on the sandbox.
- What a sandbox reaches changes by a call, in place: a sandbox lives
  long and holds state, and it cannot roll as a service does. The
  change is a change of its targets' rules; the sandbox's memberships,
  its environment and itself, never change, so nothing moves: no new
  address, no broken connection. A removal counts as gone when the
  nodes that hold the rule have it, as any rule's does. A hibernated
  sandbox takes a new `reaches` at its restore, as it takes everything.
  A tenant's job's `reaches` is fixed: it runs once.
- Exec, the tools, the port door and the routes to an attached service
  do not go over the pod network: they need no declaration.
- Like every pod of the environment, a sandbox's and a job's pod are
  members of their environment and of a membership of their own, and
  resolve the environment's names from its table. Nothing references a
  sandbox, so nothing of the environment reaches it, except through an
  attached service's port (below).

### An attached service runs in a sandbox of its environment

- An attached service names a sandbox of **its own environment**. A
  sandbox of another environment is an error of the plan.
- **Every sandbox can run attached services.** There is no attachable
  kind of sandbox: what it would give (a group, a name table, from the
  creation on) every sandbox has from its environment. A create that
  asks for one is refused with this ADR's name, so that a caller
  notices.
- An attached service has **nothing of its own on the network**: its
  process runs in the sandbox's pod. Its references open what they name
  for the sandbox's pod (ADR 0012); `uses` and `egress` on an attached
  service are errors of the plan that name the sandbox.
- Its name is in the environment's table with the sandbox's address, so
  that pods of the environment reach it where a rule lets them. Inside
  the sandbox it is reached over the guest's loopback: `feosd`'s
  resolver answers a pod a name whose addresses include the pod's own
  with loopback, so that the attached services of one sandbox reach each
  other by name without a table of their own.
- A route to an attached service is the route door's (ADR 0043); the
  ingress does not serve it.

### Grants follow the environment

- On an environment (ADR 0031):
  - `read`: also its sandboxes and tenants' jobs, their status, their
    logs and traces;
  - `write`: also making, pausing, resuming, deleting its sandboxes,
    their tokens, their tools and what they reach; making and cancelling
    its tenants' jobs; services with the runtime `attached`.
- A tenant grant's `read` still lists them all; what only a tenant
  grant gives does not include sandboxes, jobs or attached services. A
  sandbox reaches only its environment, so there is no reason to keep
  them on the tenant.
- A sandbox's token stays the sandbox's own, of no principal.
- Quotas are per tenant (ADR 0031), and an environment's sandboxes and
  jobs count against the tenant's.

### What an environment's life does to them

- **Delete.** An environment with sandboxes or with queued or running
  jobs is not deleted: the refusal names them, so that nobody loses a
  workspace's disk by deleting its environment. The caller deletes them
  first. Ended jobs' records go with the environment.
- **Clone** copies neither: they are not declared.
- **The sink** labels their logs and traces with the environment, as it
  does an instance's, and filters them by the reader's grants (ADR
  0031).

## Alternatives considered

- **Sandboxes as objects of the spec** (`sandboxes:` beside
  `services:`): every sandbox made or deleted would be a deployment, a
  workspace a line in production's history, and a revert would delete
  an agent's disk. A sandbox is made by a call, as a pod is next to a
  Deployment in a Kubernetes namespace.
- **The environment optional**, a sandbox without one in `feosd`'s
  default group: two kinds of sandbox, two sets of rules for grants,
  network and deletion, and an exception in ADR 0012 for the ones that
  serve environments. One mode is simpler, and an environment with
  nothing declared costs nothing.
- **A default environment per tenant** for sandboxes made without one:
  the same two kinds under one name, and a place where every sandbox
  without a better home reaches every other.
- **`reaches` required on the create**: nothing of the environment is
  the safe default, and a sandbox that needs only its token needs no
  list.
- **A sandbox reaching all of its environment by default**: a sandbox
  runs agents' code, and what it reaches is the tenant's decision, made
  per sandbox; `all` is one word away.
- **A move for a changed `reaches`**, a hibernation and a restore under
  a new address: this was the decision while a sandbox named security
  groups of its own, because its membership changed under it; a change
  of its targets' rules is put in place and needs no move.
- **A sandbox serving several environments**, an attachable sandbox
  that an attached service of any environment may name: nobody used it
  (FeCode's workspace sandbox serves one dev environment), and it is
  what made names, ports and groups exceptions.

## Consequences

- A person with `write` on an environment opens a shell next to its
  database, runs a migration by hand and starts a preview there,
  without a tenant grant.
- A sandbox is usable by its token alone the moment it is made, and
  reaches nothing of its environment until it is told to; what it is
  told changes without a new address.
- A preview's attached services are reached by the environment's pods
  under their own names, and by each other on loopback, in one sandbox
  of the environment.
- Deleting an environment never takes a workspace's disk with it; the
  caller sees what is in the way.
- A reader of an environment sees the logs and traces of the sandboxes
  and jobs that run in it.
- FeCode makes a workspace's dev environment before its sandbox and the
  sandbox in it, reaching all of it; a project's own sandbox and its
  build jobs get environments of the project, and need only the
  internet, which `egress` gives.

## History

Decided on 2026-11-07 (fea6fcb) and live the same day: a migration
moved every sandbox and job into an environment once, into the one its
attached service named, else into an environment `sandboxes` made for
the tenant, and `ferund` moved the running ones under their new groups.
At first a sandbox named security groups on its create and moved under
a new address when they changed; on 2026-11-08 they became `reaches`,
changed in place (a061348, built in a529381), together with the
reachability from declarations that ADR 0012 holds.
