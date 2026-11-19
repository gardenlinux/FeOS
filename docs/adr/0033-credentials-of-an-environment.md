# ADR 0033: Credentials of a tenant: a binding stated once, versioned, used by name

**Status:** Accepted · **Date:** 2026-11-06

A credential is an object of the tenant: an HTTP binding's whole
definition (ADR 0028), stated once under a name, with every write a new
version and no answer ever carrying its secrets. A sandbox names the
credentials its pod gets and gets each at its latest version, so that one
write rotates a credential for every running sandbox at once. An
environment's binding names a credential and an apply pins the version it
got, as it pins a secret, so that a deployment says what its pods get and
a revert restores it. A credential in use is not deleted, and the console
shows each with its uses.

## Context

ADR 0028 offloads a credential: a pod calls an API by a name, and the
platform adds the credential on the node, outside the pod. A binding is
stated in one of two places. A sandbox has its own set, which its own
token writes, with the credentials as literals sealed per sandbox. An
environment's spec has bindings whose secret fields name the
environment's secrets.

For a person both places miss what a credential is to them: one thing
with a name. One GitHub token that twenty agents' sandboxes and two
environments use stands in twenty-two places. Rotating it, which is what
one does the day it leaks, is twenty-two writes, each with its own way. A
tenant's session cannot write a sandbox's set at all: only the sandbox's
token can, so the console could not offer offloading, the product's
promise (ADR 0031). A secret of an environment is not enough either: it is
a value, and the binding around it, the upstream, its paths and its rate,
is what a person states once.

## Decision

### A credential is the tenant's

A credential is an object of the tenant, named by a DNS label: an HTTP
binding's definition as ADR 0028 has it (upstream, mode, CA, path
prefixes, forwarded host, rate limit and a credential of one of its four
kinds), checked by the same rules and the same destination rule. Its
secret fields are literals, sealed at rest and bound to the tenant, the
name and the version; no answer carries them.

Every write that changes the definition makes a new version; a version
never changes; the same definition again makes none. A read answers the
latest, every secret field shown as set. A write may keep a secret field
of the latest version instead of sending it, so that a definition read,
edited and written back keeps the secrets it did not touch.

Reading credentials needs `read` on the tenant, writing and deleting them
`write` (ADR 0031), as a sandbox's sets and an environment's creation do.

### A sandbox names the credentials it gets

A sandbox names the credentials its pod gets, at its create or later with
`write`, by stating the whole list. The pod's HTTP bindings are then its
own set, unchanged and its token's, and the credentials it names, each as
a binding with the credential's name, at the credential's **latest**
version. A name in both, two credentials the pod would resolve as one
name, or a name an environment gives the sandbox, is refused.

A new version is sealed to every running pod that names the credential at
once: the pod's bindings' version changes, its `feos-authn` fetches them
again, and nothing restarts. Rotation is one write.

### An environment's binding names a credential, pinned

A binding in an environment's spec may name a credential instead of
stating an upstream and a credential, with nothing else of its own. An
apply pins it to the latest version, as it pins a secret (ADR 0044), and
the deployment carries the version and the version's upstream and mode,
which decide whether a pod is another (ADR 0028). The pods get the pinned
version.

A new version reaches an environment with its next deployment, which
moves the pin; a deployment stays what it was, and a revert gets the
version it had. When only the credential changed, the pods are sealed
again in place; when the upstream or the mode changed, they are replaced,
as for any binding.

The environment's own secrets stay what they are: configuration of the
application, versioned with its deployments.

### A credential in use is not deleted

A delete is refused, naming the users, while a sandbox names the
credential or the latest or the deployed deployment of an environment
pins it. Once none does, every version goes.

### The console shows a credential with its uses

The console has a page per credential: its definition with every secret
field shown as set, a check (ADR 0028's preflight with the stored
credential), and its uses: the sandboxes that name it, and the
environments with the version each pins and a way to move them to the
latest, a deployment each. A new credential's form runs the preflight
before it saves.

## Alternatives considered

- **A page that indexes the bindings where they are.** No new object:
  the console lists the sandboxes' sets and the specs' bindings and
  writes a credential into each place. It needs only the session on a
  sandbox's set. Rejected: the console would hold together what the API
  keeps apart, and rotation would stay one write per place.
- **Secrets of the tenant**, which an environment's binding and a
  sandbox's set name by `{secret}`. Shares the value, not the binding:
  the upstream, its paths and its rate would still be stated in every
  place.
- **The latest version for environments too**, without a pin. Rotation
  would reach them at once, but a deployment would no longer say what
  its pods get, and a revert would not restore it. An environment moves
  its pin with a deployment, as it does for secrets; the console offers
  it.

## Consequences

- A tenant's session can give a sandbox a credential, which the console
  needs; a sandbox's token keeps its own set, which FeCode uses,
  unchanged.
- One write rotates a credential for every sandbox at once; environments
  follow with a deployment each, which the console offers in one place.
- A pod's bindings come from three sources (ADR 0028); its bindings'
  version names all of them, so that any change reaches the pod.
- A credential's definition is readable by `read` on the tenant, its
  secrets by no one; an upstream that echoes one gets it masked (ADR
  0028).
- A credential is an HTTP binding's; the object is shaped so that
  Kubernetes, SSH and registry credentials can become the tenant's the
  same way once their node side exists.

## History

Decided and built on 2026-11-06 (b6a9fb6, the console ff5ae68), the day
after ADR 0028, as its third source of bindings. An environment's page
shows where each binding's credential comes from since 2026-11-07
(5453149). The page's view of a credential's requests, decided at first,
is not built; the sink's egress records are read per environment
instead.
