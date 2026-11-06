# ADR 0033: Credentials of a tenant: an HTTP binding stated once, versioned, and used by name by sandboxes and environments

**Status:** Accepted · **Date:** 2026-11-06

**Touches:** ADR 0028 (an HTTP binding gets a third source beside a
sandbox's own set and an environment's spec), ADR 0018 (a spec's
binding may name a credential, which an apply pins like a secret), ADR
0031 (who may read and write them), ADR 0032 (the builder shows them).

## Context

ADR 0028 offloads a credential: a pod calls an API by a name, and the
platform adds the credential on the node, outside the pod. A binding is
stated in one of two places. A sandbox has its own set, which its own
token writes (`PUT /api/v1/sandboxes/{id}/http`), with the credentials as
literals sealed per sandbox. An environment's spec has bindings whose
secret fields name the environment's secrets.

For a person both places miss what a credential is to them: one thing
with a name. One GitHub token that twenty agents' sandboxes and two
environments use stands in 22 places. Rotating it, which is what one does
the day it leaks, is 22 writes, each with its own way. A tenant's session
cannot write a sandbox's set at all: only the sandbox's token can, so the
management UI could not offer offloading, the product's promise, and the
console's concept (decision A) named this as the gap. A secret of an
environment is not enough either: it is a value, and the binding around
it (the upstream, its paths and rate) is what a person states once.

## Decision

### 1. A credential is the tenant's

A credential is an object of the tenant, named by a DNS label: an HTTP
binding's definition as ADR 0028 has it (upstream, mode, CA, path
prefixes, forwarded host, rate limit, and the credential of kind
`headers`, `basic`, `oauth2` or `clientCertificate`), checked by the same
rules (`httpcheck`, the destination rule). Its secret fields are
literals, sealed at rest under `--secret-key-file`, bound to the table,
the tenant, the name and the version; no answer carries them.

Every write makes a new version; a version never changes. A read answers
the latest. A write may send a secret field as `{"set": true}`: the
previous version's value is kept, so that a definition read, edited and
written back keeps the secrets it did not touch.

Reading credentials needs `read` on the tenant, writing and deleting
them `write` (ADR 0031), as a sandbox's sets and an environment's
creation do.

### 2. A sandbox names the credentials it gets

A sandbox names the credentials its pod gets: at its create
(`credentials`) or later, with `write` on the tenant, by stating the
whole list. The pod's HTTP bindings are then its own set (unchanged, its
token's) and the credentials it names, each as a binding with the
credential's name, at the credential's **latest** version. A name in
both is refused (`name_taken`).

A new version is sealed to every running pod that names the credential
at once: the pod's bindings' version changes, its feos-authn fetches them
again, and nothing restarts. Rotation is one write.

### 3. An environment's binding names a credential, pinned

A binding in an environment's spec may name a credential instead of
stating an upstream and a credential: `{"tenantCredential": {"name":
"github"}}`, with nothing else. An apply pins it to the latest version,
as it pins a secret, and the deployment carries the version, and the
version's upstream and mode, which decide whether a pod is another (ADR
0028). The pods get the pinned version.

A new version reaches an environment with its next deployment, which
moves the pin; a deployment stays what it was, and a revert gets the
version it had. When only the credential changed, the pods are sealed
again in place; when the upstream or the mode changed, they are replaced,
as for any binding.

The environment's own secrets stay what they are: configuration of the
application, versioned with its deployments.

### 4. A credential in use is not deleted

A delete is refused (`credential_in_use`, naming the users) while a
sandbox names the credential or the latest or the deployed deployment of
an environment pins it. Once none does, every version goes.

### 5. The console shows a credential with its uses

The console gets a page per credential: its definition with every secret
field shown as set, a check (the preflight of ADR 0028 with the stored
credential), its uses (the sandboxes that name it; the environments and
the version each pins, with a way to move them to the latest), and its
requests, which the sink records as before by the binding's name.

## Alternatives considered

- **A page that indexes the bindings where they are.** No new object:
  the console lists the sandboxes' sets and the specs' bindings and writes
  a credential into each place. It needs only the session on a sandbox's
  set. Rejected: the console would hold together what the API keeps apart,
  and rotation would stay one write per place.
- **Secrets of the tenant**, which an environment's binding and a
  sandbox's set name by `{secret}`. Shares the value, not the binding:
  the upstream, its paths and its rate would still be stated in every
  place.
- **The latest version for environments too**, without a pin. Rotation
  would reach them at once, but a deployment would no longer say what its
  pods get, and a revert would not restore it. An environment moves its
  pin with a deployment, as it does for secrets; the console offers it.

## Consequences

- A tenant's session can give a sandbox a credential, which the UI needs;
  a sandbox's token keeps its own set, which FeCode uses, unchanged.
- One write rotates a credential for every sandbox at once; environments
  follow with a deployment each, which the console offers in one place.
- The pod's bindings come from three sources; its bindings' version
  names all of them, so that any change reaches the pod.
- A credential's definition is readable by `read` on the tenant, its
  secrets by no one.

## Open questions

- Kubernetes, SSH and registry credentials as the tenant's too, once
  their node side is built (ADR 0014, ADR 0025).
- A narrower use: a sandbox or a binding that takes a credential with
  fewer paths or a lower rate than its definition.
- The tools of the general MCP door (ADR 0031) for credentials, without
  their secrets.
