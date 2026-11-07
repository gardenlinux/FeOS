# ADR 0035: Everything runs in an environment — a sandbox and a tenant's job are made in one, in its security groups, outside its deployments

**Status:** Proposed · **Date:** 2026-11-07

**Touches:** ADR 0018 (a sandbox and a tenant's job belong to an
environment; `attachable` and the group and name table `sbx-<id>` go; an
attached service runs in a sandbox of its own environment), ADR 0031
(sandboxes, their tokens and tools, tenants' jobs and attached services
become an environment grant's), ADR 0034 (sandboxes and jobs name
groups of their environment; the attachable sandbox's exception goes; a
sandbox's groups change by a move), ADR 0033 (unchanged: a sandbox
still names the tenant's credentials).

## Context

An environment holds what a deployment declares: services, jobs,
addons, routes, volumes, secrets, bindings, and from ADR 0034 security
groups. Two kinds of pod stand outside every environment:

- a **sandbox** (`POST /api/v1/sandboxes`), the tenant's, with its own
  idle, pause and hibernation, reached by exec and its port door;
- a **tenant's job** (`POST /api/v1/jobs`, ADR 0018), one pod run once,
  a build or one-off maintenance.

In `ferund` they are rows of the same registry as an environment's
instances, placed, admitted and suspended by the same engine. Only the
API keeps them apart, and the separation costs:

- **Network.** A sandbox reaches an environment only when it was made
  `attachable` and an environment's attached service names it. It then
  gets a group of its own, `sbx-<id>`, with egress to every
  environment it serves, and a name table that includes theirs. One
  sandbox can serve several environments, so names and ports of two
  environments can meet in it, and ADR 0034 needs an exception: the
  only pod whose groups change while it runs.
- **Grants.** ADR 0031 gives sandboxes, tenants' jobs and attached
  services to a tenant grant alone, because a sandbox may reach any of
  the tenant's environments. A person with `write` on one environment
  can deploy there but cannot start a pod beside it: no shell next to
  the database, no migration run by hand, no preview.
- **Security groups.** ADR 0034 puts every pod of an environment into
  named groups, secure by default. A sandbox has no environment, so it
  can name no group: it is either in `feosd`'s default group (out to
  the internet, nothing in) or in `sbx-<id>`, which reaches everything
  of the environments it serves. "This sandbox may reach the database
  on 5432, nothing else" cannot be said.
- **Logs, traces, deletion.** What the sink keeps of a sandbox is the
  tenant's, not an environment's; a person who reads one environment
  does not see the sandbox that runs its preview. An environment that
  is deleted leaves its sandboxes behind.

What is wanted is the namespace of Kubernetes: one place where a
tenant's declared workloads and its single pods run side by side, under
the same rules for network, names and access.

## Decision

### A sandbox and a tenant's job belong to an environment

- A sandbox's and a tenant's job's create names its **environment**,
  by name or ID, in the request's `environment`. It is required and
  fixed for the pod's life. There is no sandbox or tenant's job without
  an environment; the field missing is 400.
- They are **not objects of the spec.** A deployment declares what
  should run; a sandbox and a tenant's job are made by a call and live
  until deleted, ended or expired. Making one creates no deployment,
  and a deployment never makes, changes or removes one. An environment
  without deployments is a valid place for them: a room with nothing
  declared.
- The paths stay: `/api/v1/sandboxes/{id}` and `/api/v1/jobs/{id}`, as
  the IDs are the cluster's (ADR 0019). A list takes
  `?environment=`; without it, it lists the tenant's.
- A sandbox's token, its exec, its tools, its port door, its idle,
  pause and hibernation are unchanged. `/mcp` on a sandbox's token is
  FeCode's contract (ADR 0031) and stays exactly as it is.

### Their pods are in the environment's security groups

- The create takes **`securityGroups`**: names of groups of the
  environment (ADR 0034), at most five. **Left out, the pod is in
  `default`**, as a service that names none is. An empty list is 400,
  so that every pod has a group. A name that the environment's
  deployed deployment does not have, or that a deployment on its way
  removes, is 422 with the names it has.
- `default` lets nothing in and reaches the internet (ADR 0034). A
  sandbox made without groups is therefore what a sandbox is today:
  usable by its token, invisible on the network. What it may reach in
  its environment, and what may reach it, the tenant says in rules.
- Exec, the tools, the port door and the ingress's routes to an
  attached service do not go over the pod network, or come from the
  ingress's own group (ADR 0021, ADR 0034): they need no rule.
- Like every pod of the environment, a sandbox and a job's pod are
  members of `env-<id>`, may send to every pod of the environment (the
  receiver's `in` decides), and resolve the environment's names from
  `env-<id>`'s table.
- **A plan counts them as users.** A deployment that removes a group a
  sandbox or a running job names is refused at the plan, with the
  pods that name it, as it is for an object that names it.

### Changing a sandbox's groups is a move

- A sandbox lives long and holds state; it cannot roll as a service
  does. Its groups change by `PUT /api/v1/sandboxes/{id}/security-groups`.
- The change is a **move**: `ferund` hibernates the sandbox and
  restores it (ADR 0008) with the new groups. A hibernated pod gives up
  its addresses and a restored one gets new ones, on any node, its own
  included, so the sandbox comes back under a **new address**, its disk
  and memory as they were. Processes go on where they were; open
  connections break, as they do when a sandbox resumes on another node
  (ADR 0010). It takes as long as a hibernation and a restore.
- So ADR 0034's rule holds for every pod: a member leaves a group only
  when its pod ends or moves, and since addresses are never given twice,
  a removal that arrives late opens nothing. There is no change of
  membership in place.
- A hibernated sandbox takes its new groups at its restore, which
  already is a move; a paused one is hibernated for it.
- A tenant's job's groups are fixed: it runs once.

### An attached service runs in a sandbox of its environment

- An attached service names a sandbox of **its own environment**. A
  sandbox of another environment is an error of the plan.
- **Every sandbox can run attached services.** `attachable` goes: what
  it gave (a group, a name table, from the creation on) every sandbox
  now has from its environment. A create that sends `attachable` is
  400 with this ADR's name, so that a caller notices.
- An attached service has **no groups of its own**: its process runs in
  the sandbox's pod, so it is in the sandbox's groups. `securityGroups`
  on an attached service is an error of the plan that names the
  sandbox's groups.
- Its name is in `env-<id>`'s table with the sandbox's address, so that
  pods of the environment reach it where a rule lets them. Inside the
  sandbox the name is that address too: the guest reaches its own
  address over its loopback interface, but no longer as `127.0.0.1` or
  `::1`, which `sbx-<id>` gave. **This changes what a service must
  listen on**: its sandbox's IPv6 address, or all interfaces (`::`); a
  service bound to `127.0.0.1` or to IPv4's `0.0.0.0` alone is no longer
  reached by its name, also by the attached services beside it. A
  service with a routed port listens there already, since the ingress
  and the environment's pods reach it at that address.
- The blocks of `in` rules on a routed port of an attached service
  (ADR 0034, the ingress checks the client) are those of its sandbox's
  groups.
- The group and the name table `sbx-<id>` go, and with them the case of
  two environments' names and ports in one sandbox.

### Grants follow the environment

- On an environment (ADR 0031):
  - `read`: also its sandboxes and tenants' jobs, their status, their
    logs and traces;
  - `write`: also making, pausing, resuming, deleting its sandboxes,
    their tokens, their tools and their groups; making and cancelling
    its tenants' jobs; services with the runtime `attached`.
- A tenant grant's `read` still lists them all; what only a tenant
  grant gives no longer includes sandboxes, jobs or attached services.
  A sandbox reaches only its environment, so the reason ADR 0031 gave
  for keeping them on the tenant is gone.
- A sandbox's token stays the sandbox's own, of no principal.
- Quotas stay per tenant (ADR 0031), counted as before.

### What an environment's life does to them

- **Delete.** An environment with sandboxes or with running jobs is not
  deleted: 409 with their IDs, so that nobody loses a workspace's disk
  by deleting its environment. The caller deletes them first. Ended
  jobs' records go with the environment.
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
  default group as today: two kinds of sandbox, two sets of rules for
  grants, network and deletion, and the exception in ADR 0034 kept for
  the ones that serve environments. One mode is simpler, and an
  environment with nothing declared costs nothing.
- **A default environment per tenant** for sandboxes made without one:
  the same two kinds under one name, and a place where every sandbox
  without a better home reaches every other.
- **`securityGroups` required on the create**: `default` is closed
  inbound already, and a service may leave its groups out too. A rule
  for sandboxes alone would be a second rule for the same thing.
- **A sandbox in its environment's groups automatically**, all of
  them, or a fixed `sandbox` group open to the environment: a sandbox
  runs agents' code, and what it reaches is the tenant's decision, made
  per sandbox.
- **Groups changed in place, without a move**: ADR 0034 rejected it,
  since every removal would then need every node's acknowledgement
  before access counts as gone. A move costs a sandbox a hibernation
  and a restore, and keeps removals harmless.
- **A sandbox serving several environments**, as `attachable` allows:
  nobody uses it (FeCode's workspace sandbox serves one dev
  environment), and it is what made names, ports and groups
  exceptions.

## Consequences

- `POST /api/v1/sandboxes` and `POST /api/v1/jobs` gain `environment`
  (required) and `securityGroups`; the sandbox gains `PUT
  …/security-groups`; lists take `?environment=`; `attachable` is
  refused. The rows of sandboxes and jobs get `environment_id` not
  null, a sandbox's groups its pod template.
- `ferund` drops the group and name table `sbx-<id>`; the controller
  puts every sandbox and job pod into `env-<id>`, its named groups and
  `env-<id>`'s table, and names attached services in that table.
- A group change is hibernation and restore, which already give the
  pod new addresses. The restore takes the groups and name tables from
  the pod's checkpointed spec, so `RestorePodRequest` gains
  `security_groups` and `name_tables`, which replace the record's when
  set, as `log_labels` do. A move that skips the checkpoint's upload
  when the sandbox stays on its node is an optimisation for later.
- The plan checks groups that sandboxes and running jobs name, and an
  attached service's sandbox and its lack of groups.
- ADR 0031's grants move as above; the console's sandbox and job forms
  get an environment and a choice of groups, showing what `default`
  means ("nothing in, internet out"), and an environment's page lists
  its sandboxes and jobs.
- **Existing sandboxes and jobs move once, at the release, with no
  second mode.** A migration puts each sandbox that an environment's
  attached service names into that environment, and every other
  sandbox and every job of a tenant into an environment `sandboxes`
  that it makes for the tenant, empty. All are in `default`. Running
  and paused sandboxes are moved once by `ferund` after the release,
  hibernated ones take their groups at their restore; running jobs
  finish under their old groups.
- **FeCode changes in the same release** (a FeCode ADR of its own):
  - a workspace's dev environment is made with its sandbox, before it,
    and the sandbox in it, in a group that the dev environment's
    addons let in;
  - a project's own sandbox and its build jobs get environments of the
    project (builds need only the internet, which `default` gives);
  - a lost workspace sandbox is replaced in the same environment, and
    the dev environment's deployment names the new one, as now;
  - it stops sending `attachable`;
  - its agent's instructions and its templates have an attached
    service listen on `::` (or its sandbox's address), not on
    `127.0.0.1`, since its name no longer resolves to loopback.

## Open questions

- **An environment's secrets in a sandbox or a job** by name, pinned
  at the create as an apply pins: a job's `secrets` and a sandbox's
  exec carry literals today, and ADR 0033's credentials cover HTTP.
- **Routes to a sandbox's port** other than through an attached
  service.
- **Quotas per environment**, now that everything an environment runs
  is counted in it.
