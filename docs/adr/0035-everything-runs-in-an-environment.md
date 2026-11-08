# ADR 0035: Everything runs in an environment — a sandbox and a tenant's job are made in one, reaching what they are told, outside its deployments

**Status:** Proposed · **Date:** 2026-11-07 · **Revised 2026-11-08** with
[ADR 0034](0034-reachability-from-declarations.md)'s revision: a sandbox
and a tenant's job say what of their environment they reach (`reaches`)
instead of naming security groups, and a change of it is a change of
rules, in place, no move.

**Touches:** ADR 0018 (a sandbox and a tenant's job belong to an
environment; `attachable` and the group and name table `sbx-<id>` go; an
attached service runs in a sandbox of its own environment), ADR 0031
(sandboxes, their tokens and tools, tenants' jobs and attached services
become an environment grant's), ADR 0034 (a sandbox and a job say what
of their environment they reach; the attachable sandbox's exception
goes; a change of it is a change of the targets' rules, in place), ADR 0033 (unchanged: a sandbox
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

### They say what of the environment they reach

- The create takes **`reaches`**: what of the environment the pod may
  reach, a list of the deployed deployment's objects with a port
  (`{service: api, port: http}`, `{addon: db}`) or `all`, every port of
  every pod of the environment (ADR 0034). **Left out, nothing of the
  environment**: the pod is usable by its token and alone on the
  network. An entry naming an object the deployed deployment does not
  have is 422 with the names it has; one that a later deployment removes
  admits nothing and is shown on the sandbox. The create takes `egress`
  as a service's does.
- Exec, the tools, the port door and the ingress's routes to an
  attached service do not go over the pod network, or come from the
  ingress (ADR 0021, ADR 0034): they need no declaration.
- Like every pod of the environment, a sandbox and a job's pod are
  members of `env-<id>` and of a membership of their own, and resolve
  the environment's names from `env-<id>`'s table. Nothing references a
  sandbox, so nothing of the environment reaches it, except through an
  attached service's port (below).

### A change of what a sandbox reaches is a change of rules

- A sandbox lives long and holds state; it cannot roll as a service
  does. What it reaches changes by `PUT /api/v1/sandboxes/{id}/reaches`.
- The change is a change of its targets' rules, in from the sandbox's
  pod, which `ferund` puts whole on the nodes that hold them and
  acknowledges per node (ADR 0034). The sandbox's memberships, its
  environment and itself, never change, so nothing moves: no new
  address, no broken connection. A removal counts as gone when the
  nodes that hold the rule have acknowledged it, as any rule's does.
- A hibernated sandbox takes a new `reaches` at its restore, as it
  takes everything.
- A tenant's job's `reaches` is fixed: it runs once.

### An attached service runs in a sandbox of its environment

- An attached service names a sandbox of **its own environment**. A
  sandbox of another environment is an error of the plan.
- **Every sandbox can run attached services.** `attachable` goes: what
  it gave (a group, a name table, from the creation on) every sandbox
  now has from its environment. A create that sends `attachable` is
  400 with this ADR's name, so that a caller notices.
- An attached service has **nothing of its own on the network**: its
  process runs in the sandbox's pod. Its references open what they name
  for the sandbox's pod (ADR 0034); `reaches` and `egress` on an
  attached service are errors of the plan that name the sandbox.
- Its name is in `env-<id>`'s table with the sandbox's address, so that
  pods of the environment reach it where a rule lets them. Inside the
  sandbox it is reached over the guest's loopback, as now: `feosd`'s
  resolver answers a pod a name whose addresses include the pod's own
  with `127.0.0.1` and `::1`, so that the attached services of one
  sandbox reach each other by name without the table `sbx-<id>`.
- A route to an attached service's port names its clients' blocks as
  any route does (ADR 0034, `from`).
- The group and the name table `sbx-<id>` go, and with them the case of
  two environments' names and ports in one sandbox.

### Grants follow the environment

- On an environment (ADR 0031):
  - `read`: also its sandboxes and tenants' jobs, their status, their
    logs and traces;
  - `write`: also making, pausing, resuming, deleting its sandboxes,
    their tokens, their tools and what they reach; making and cancelling
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
- **`reaches` required on the create**: nothing of the environment is
  the safe default, and a sandbox that needs only its token needs no
  list.
- **A sandbox reaching all of its environment by default**: a sandbox
  runs agents' code, and what it reaches is the tenant's decision, made
  per sandbox; `all` is one word away.
- **A move for a changed `reaches`**, a hibernation and a restore under
  a new address, as the first version of this ADR had it for a change of
  groups: a change of membership needed it; a change of rules is put in
  place and acknowledged per node (ADR 0034).
- **A sandbox serving several environments**, as `attachable` allows:
  nobody uses it (FeCode's workspace sandbox serves one dev
  environment), and it is what made names, ports and groups
  exceptions.

## Consequences

- `POST /api/v1/sandboxes` and `POST /api/v1/jobs` gain `environment`
  (required), `reaches` and `egress`; the sandbox gains `PUT
  …/reaches`; lists take `?environment=`; `attachable` is refused. The
  rows of sandboxes and jobs get `environment_id` not null, a sandbox's
  `reaches` its row.
- `ferund` drops the group and name table `sbx-<id>`; the controller
  puts every sandbox and job pod into `env-<id>` and a membership of its
  own, writes its `reaches` as its targets' rules, gives it `env-<id>`'s
  table, and names attached services in that table.
- A restore carries the pod's memberships and name tables
  (`RestorePodRequest`'s `security_groups` and `name_tables`, which
  replace the record's when set, as `log_labels` do); since ADR 0034's
  revision they no longer change between a pod's runs.
- The plan shows a `reaches` entry whose object a deployment removes,
  and checks an attached service's sandbox.
- ADR 0031's grants move as above; the console's sandbox and job forms
  get an environment, `reaches` (`all` or a list) and `egress`, and an
  environment's page lists its sandboxes and jobs.
- **Existing sandboxes and jobs moved once, at the release of
  2026-11-07, with no second mode.** A migration puts each sandbox that an environment's
  attached service names into that environment, and every other
  sandbox and every job of a tenant into an environment `sandboxes`
  that it makes for the tenant, empty. All are in `default`. Running
  and paused sandboxes are moved once by `ferund` after the release,
  hibernated ones take their groups at their restore; running jobs
  finish under their old groups. ADR 0034's revision turns a sandbox's
  groups into `reaches`: `all` for FeCode's `workspace`, nothing for
  `default`.
- **FeCode changes in the same release** (a FeCode ADR of its own):
  - a workspace's dev environment is made with its sandbox, before it,
    and the sandbox in it, reaching all of it;
  - a project's own sandbox and its build jobs get environments of the
    project (builds need only the internet, which `egress` gives);
  - a lost workspace sandbox is replaced in the same environment, and
    the dev environment's deployment names the new one, as now;
  - it stops sending `attachable`.

## Open questions

- **An environment's secrets in a sandbox or a job** by name, pinned
  at the create as an apply pins: a job's `secrets` and a sandbox's
  exec carry literals today, and ADR 0033's credentials cover HTTP.
- **Routes to a sandbox's port** other than through an attached
  service.
- **Quotas per environment**, now that everything an environment runs
  is counted in it.
