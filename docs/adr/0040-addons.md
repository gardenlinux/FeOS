# ADR 0040: Addons: managed backing services from blueprints, on volumes of their own

**Status:** Accepted · **Date:** 2026-10-18

An addon is a managed backing service of an environment, declared by
type, version, plan and options and realised by `ferund` from a
blueprint per type: `postgres`, `valkey` and `nats`. Each is one
instance on a persistent volume of its own, with credentials generated
once, provisioning steps that run in its container whenever their inputs
change, and outputs that references resolve to. The volume's data
decides what may run on it: a change of type deletes the data, as the
plan warns, and a version the blueprint cannot run on the data is
refused. A failed deployment never recreates an addon back onto older
software, and a lost addon is replaced only where its volume is. How a
workload proves itself to postgres and valkey is ADR 0038.

## Context

An application needs a database, a cache or a message bus beside its
services, and an agent without the platform builds them by hand: a
Postgres from apt, a `nats-server` built from source, an init SQL for
roles and schemas, a shell supervisor that waits for their health
(ADR 0018). None of it is what production runs, and none of it is the
same twice. A service's own volume would tie a stateful program to one
replica and leave its roles, schemas and credentials to the tenant; a
backing service with a volume of its own, provisioned by the platform
and referenced by name, is what the agent and the person both ask for.
Data outlives software: a volume holds what one type at one version
wrote, and no deployment may put software in front of it that cannot
read it.

## Decision

### A blueprint per type

- An addon is declared by its **type** and **version**, both required; a
  **plan**, which is its size (vCPUs, memory, volume; the blueprint's
  default when absent); and the **options** of its type, which the
  blueprint declares and the catalog answers: databases and roles for
  postgres, the eviction policy for valkey, JetStream for nats. An option
  the blueprint does not declare is an error of the plan.
- `ferund` realises an addon from its blueprint: its pod, its volume
  (ADR 0016), the credentials it generates, its health check and its
  provisioning. An addon is one instance and never idles; replication
  is a decision per type, and no type has it.
- Its **credentials** are system secrets of the environment (ADR 0044),
  generated once at the addon's first deployment and never changed: a
  server key for the types that admit a workload by its certificate
  (ADR 0038), a password for nats.
- Its **outputs**, an address, a port, a user or role, a database and a
  URL, are what a reference `{addon, key, …}` resolves to; a reference
  names what the type needs, a postgres reference its role. A dependent
  gets a new template when an output changes and is rolled as a
  deployment would roll it. A reference opens the addon's port to the
  object that holds it (ADR 0012); the addon itself reaches the
  internet, as its guest pulls its image.

### Provisioning

- A blueprint's provisioning is a list of **steps**, each an argv run in
  the addon's container with its inputs in the step's environment. A
  hash over the steps decides whether they run: a changed database list,
  role or certificate runs them again, and a deployment waits while they
  run. Steps create what is missing and never drop: a database or role
  no longer listed stays, and the plan warns.
- The steps run once the addon is live, and their output is redacted of
  every credential before anything of it is stored. A step that fails is
  the instance's detail and runs again; an addon that is not live keeps
  the deployment waiting with the reason.

### What runs on the data

- An addon's volume holds data of one type, written by a version, and
  the volume decides what may run on it. An addon whose type changes is
  removed and added, its data deleted, as the plan's warning said. A
  version the blueprint cannot run on the data, an older major version
  of postgres, a downgrade of valkey, is refused, the running instance
  kept. A version the blueprint has retired runs the current one on its
  volume, with a warning.
- While a deployment that recreated an addon has failed, the addon stays
  as that deployment has it, never recreated back onto older software.
  An addon that only a failed first deployment has keeps its data; an
  addon that neither the target nor the latest deployment has loses its
  volume and credentials.
- A change of plan keeps the volume's size, and the plan warns.
- An addon whose pod a node that is up has lost is replaced on that
  node, on its volume, when the node reports the volume free. One lost
  with a node that is down or removed, or whose volume is missing there
  or attached to another pod, is not replaced, its data being where it
  cannot run, and its incident says what the node answered.

### Health

- Until the guest supervises containers (ADR 0048), `ferund` runs the
  addon's readiness check over the node's connection and restarts its
  exited container as it restarts a service's (ADR 0018). The deploy
  jobs and the services of a deployment wait for its addons to be ready
  and provisioned.

## Alternatives considered

- **The tenant's own database as a service with a volume.** A volume
  attaches to one pod, so the program could not scale; roles, schemas,
  credentials and health would be the tenant's to script, which is the
  shell supervisor of ADR 0018's context, done again per project.
- **Recreating an addon back onto older software when a deployment
  fails.** The data may have been written by the newer version; the
  addon stays as the failed deployment has it, and a revert says so in
  its plan.
- **Replacing a lost addon wherever it fits**, as a service's lost
  replica is. Its data is on the node it was lost on; a new instance
  elsewhere would start empty and the data would be gone when the node
  returns.
- **Resizing the volume on a change of plan.** A plan is the pod's size;
  the volume keeps the size it was created with, and the plan warns.

## Consequences

- `ferund` creates volumes on nodes (ADR 0016), generates credentials
  and runs provisioning inside tenants' pods.
- A clone's addons start empty: an addon has no backup and no copy, and a
  copy of an environment is a copy of its declaration, not of its data.
- A plan's warnings about data are the tenant's last word before it is
  deleted: a type change and a dropped database are both warned, and
  only the first deletes anything.
- The nats addon's password is in its server's arguments inside its pod,
  and a pod's resolved environment reaches the node's record of the pod
  in the clear (ADR 0044).

## History

Decided with ADR 0018 on 2026-10-18 and built the same day (abd2c87);
split out of it on 2026-11-08. The volume's record of which type and
version wrote its data came the same day (a2e195c); the rule that a
failed deployment never recreates an addon back followed while ADR 0018
was finished, by 2026-10-19. PostgreSQL and Valkey stopped taking
passwords on 2026-11-08 (ADR 0038, e188260; ADR 0039, 2f2a0ae).
