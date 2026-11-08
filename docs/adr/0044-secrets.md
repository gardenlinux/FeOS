# ADR 0044: Secrets of an environment: versioned values that a deployment pins

**Status:** Accepted · **Date:** 2026-10-18

A secret of an environment is a name with versioned values, sealed under
the platform's key at rest; a new value is a new version, and a
deployment pins the version it was applied with, so that a rotation is
in the history as a rotation and never as a value. The API never returns
a value: it reaches a pod as an environment variable at its start, a
process's start or a provisioning step's, and nothing else, and is
written to no disk. A secret cannot be deleted while the latest
deployment pins it, and a deleted one still resolves for the deployments
that pin it. The addons' credentials are system secrets generated once.
The log tails `ferund` keeps are redacted of the environment's secrets.

## Context

A deployment is the whole desired state, immutable and kept, and a
service needs values that must appear in no spec, plan or diff: a token
for an API, a password an external system issued. A deployment that
named values would put them into every stored spec and every plan a
model reads; one that named a secret without a version could not say
what it ran, and a revert would run something else. Pods take their
environment at start, and `ferund` resolves it there (ADR 0015); an
attached service's process takes its environment from a start request
in a sandbox whose disk the agent reads (ADR 0042). The log tails
`ferund` keeps as evidence of incidents would otherwise hold whatever an
application printed.

## Decision

- A **secret** belongs to an environment and has a name, a lowercase DNS
  label, and **versions**: a new value is a new version, the highest
  ever plus one, also after a delete. Versions are sealed under the
  platform's key at rest, bound to the environment, the name and the
  version; without the key nothing that names a secret, and no addon,
  is deployed.
- A deployment **pins** every secret it references to the latest version
  at its apply, and stores the pin (ADR 0018): a deployment names what
  it runs, and a revert runs the same values. A rotation is a new
  version and a deployment that pins it.
- The API **never returns a value**. A secret's value reaches a pod as an
  environment variable at its create, an attached service's process in
  its start request, and an addon's provisioning step in its
  environment; `ferund` writes it nowhere else. Image credentials of a
  service or a job are secrets at their latest version (ADR 0022), and a
  binding's credential fields name a secret, never a literal (ADR 0028).
- A secret is **not deleted while the latest deployment pins it**. A
  delete soft-deletes every version, so that the pods of older
  deployments still resolve it; a new deployment, a revert among them,
  cannot pin a deleted secret.
- **System secrets** carry the prefix `addon.`, which a tenant's secret
  cannot: an addon's credentials are generated once at its first
  deployment, version one, and never changed (ADR 0040).
- The log tails `ferund` keeps, of incidents and of job runs, are
  **redacted** of the environment's secrets and addon credentials, as
  the values are and as a URL escapes them, before they are stored.
- A clone copies a secret only where each is named, as a new version one
  of the clone's (ADR 0018).

## Alternatives considered

- **Values in the spec.** Every stored deployment, every plan and every
  diff would hold them, and a model that plans would read them; the
  bindings' rule that a credential field is a `{secret}` and never a
  literal follows from this.
- **A reference without a version**, the latest at every start: a
  deployment could not say what it ran, a revert would run the current
  value, and a rotation would roll nothing and show nowhere.
- **A value written onto the sandbox's disk** for an attached service,
  as an env file: the agent reads the disk, and a stopped service would
  leave the value behind. The start request carries it instead.
- **Deleting a secret's versions outright**: the pods of an older
  deployment that pins it would fail at their next start; soft deletion
  keeps them running and refuses new pins.
- **Returning a value to the caller that set it**: the caller has it;
  nobody else should get it from the API.

## Consequences

- A rotation is two calls, a new version and a deployment, and reads in
  the history as a rotation: the version changed, the value unseen.
- **Known exposure.** A pod's environment, secrets resolved, reaches
  `feosd`'s record of the pod, the node's disk and hibernation
  checkpoints in the clear, and the nats addon's password is in its
  server's arguments inside its pod. The database holds no value in the
  clear. A value an application writes to its log reaches the logs
  answer read from the running pod as it is; the tails `ferund` keeps
  are redacted, as far as matching the values and their URL-escaped form
  reaches.
- A tenant's job's secrets are sealed in its own record and dropped when
  it ends (ADR 0041).

## History

Decided with ADR 0018 on 2026-10-18 and built the same day (abd2c87);
the tails' redaction of values and their URL-escaped form came with the
controller (cf5457a), and the lab's `ferund` got a key to seal with on
2026-10-19 (2c8cea8). Split out of ADR 0018 on 2026-11-08.
