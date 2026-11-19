# ADR 0033: Credentials of an environment: a binding stated once, versioned, used by name

**Status:** Accepted · **Date:** 2026-11-06, corrected 2026-11-10

A credential is an object of an environment: an HTTP binding's whole
definition (ADR 0028), stated once under a name, with every write a new
version and no answer ever carrying its secrets. A sandbox of the
environment names the credentials its pod gets and gets each at its
latest version, so that one write rotates a credential for every running
sandbox of the environment at once. A binding of the environment's spec
names a credential and an apply pins the version it got, as it pins a
secret, so that a deployment says what its pods get and a revert
restores it. Environments share no credential: each states its own. A
credential in use is not deleted, and the console shows each with its
uses.

## Context

ADR 0028 offloads a credential: a pod calls an API by a name, and the
platform adds the credential on the node, outside the pod. A binding is
stated in one of two places. A sandbox has its own set, which its own
token writes, with the credentials as literals sealed per sandbox. An
environment's spec has bindings whose secret fields name the
environment's secrets.

For a person both places miss what a credential is to them: one thing
with a name. One GitHub token that twenty agents' sandboxes and the
services beside them use stands in twenty-one places. Rotating it, which
is what one does the day it leaks, is twenty-one writes, each with its
own way. A person's session cannot write a sandbox's set at all: only
the sandbox's token can, so the console could not offer offloading, the
product's promise (ADR 0031). A secret of an environment is not enough
either: it is a value, and the binding around it, the upstream, its
paths and its rate, is what a person states once.

Where that one thing lives decides who may use it. Everything runs in an
environment (ADR 0035), and grants follow the environment: a person with
`write` on one environment makes its sandboxes and applies its
deployments. A credential that every environment could name would let a
person with `write` on `dev` give a sandbox there the payment key that
`prod` uses. They would never see it, but with offloading, using a key
is all one needs of it. Sharing a key between environments is what one
avoids anyway: the upstream's quota, rate limit and audit log cannot
tell them apart, and a key leaked from `dev`, where agents' code runs,
cannot be revoked without taking `prod` with it. Kubernetes keeps a
Secret in the namespace of the workload that uses it for the same
reason.

## Decision

### A credential is its environment's

A credential is an object of an environment, named by a DNS label: an
HTTP binding's definition as ADR 0028 has it (upstream, mode, CA, path
prefixes, forwarded host, rate limit and a credential of one of its four
kinds), checked by the same rules and the same destination rule. Its
secret fields are literals, sealed at rest and bound to the
environment's ID, the name and the version; no answer carries them. Two
environments that call the same API hold two credentials, best with a
key each; a vendor that issues only one key gets it stated twice, where
anyone can see it.

Every write that changes the definition makes a new version; a version
never changes, and its number is never used again, not even by a
credential of the name made after a delete, so that a deployment that
pins a version gets that definition or none; the same definition again
makes none. A read answers the
latest, every secret field shown as set. A write may keep a secret field
of the latest version instead of sending it, so that a definition read,
edited and written back keeps the secrets it did not touch. It keeps one
only where the latest version sends it: with the same upstream, mode,
CA and token URL. A write that changes any of them sends its secrets
again, since `write` reads no secret and must not send a stored one to
a host of its choice, or trust a CA of its own for it; a check's draft
is held to the same rule.

Reading an environment's credentials needs `read` on it; writing,
checking and deleting them need `write` (ADR 0031), as its secrets do.
A credential goes with its environment's deletion and stays through a
rename (ADR 0037), being bound to the environment's ID.

### A sandbox names its environment's credentials

A sandbox names the credentials of its environment (ADR 0035) that its
pod gets, at its create or later with `write`, by stating the whole
list. The pod's HTTP bindings are then its own set, unchanged and its
token's, and the credentials it names, each as a binding with the
credential's name, at the credential's **latest** version. A credential
of another environment is no name it can state. A name in both, two
credentials the pod would resolve as one name, or a name an environment
gives the sandbox, is refused.

A new version is sealed to every running pod that names the credential
at once: the pod's bindings' version changes, its `feos-authn` fetches
them again, and nothing restarts. Rotation is one write.

### An environment's binding names a credential, pinned

A binding in an environment's spec may name a credential of the same
environment (`credentialRef`) instead of stating an upstream and a
credential, with nothing else of its own. An apply pins it to the latest
version, as it pins a secret (ADR 0044), and the deployment carries the
version and the version's upstream and mode, which decide whether a pod
is another (ADR 0028). The pods get the pinned version. A plan without
an environment has no credentials to name.

A new version reaches the environment's services with its next
deployment, which moves the pin; a deployment stays what it was, and a
revert gets the version it had. When only the credential changed, the
pods are sealed again in place; when the upstream or the mode changed,
they are replaced, as for any binding.

The environment's own secrets stay what they are: configuration of the
application, versioned with its deployments.

A clone (ADR 0018) copies a credential where it is named, as it copies a
secret: the source's latest version becomes the clone's first, and the
clone's apply pins it. A cloned spec that names a credential the clone
lacks is refused with its name.

### A credential in use is not deleted

A delete is refused, naming the users, while a sandbox of the
environment names the credential or the environment's latest or
deployed deployment pins it. Once none does, every version goes.

### The console shows a credential with its uses

The console shows an environment's credentials in its settings, beside
its secrets, with the bindings of its latest deployment and where each
binding's credential comes from. Each credential has a page: its
definition with every secret field shown as set, a check (ADR 0028's
preflight, with the stored credential or with a draft before it is
saved, whose kept secrets are the stored ones), and its uses: the
environment's sandboxes that name it, and its bindings with the version
the latest and the deployed deployment pin, with a way to move them to
the latest, a deployment. A new credential's form runs the preflight
before it saves.

## Alternatives considered

- **A credential of the tenant**, which every environment's sandboxes
  and bindings name: decided on 2026-11-06 for the one write that
  rotates a key everywhere. Corrected on 2026-11-10: with ADR 0035 a
  grant on one environment reached every environment's keys through it;
  a key shared by `dev` and `prod` is what one avoids; and since
  environments pin, a rotation took a deployment per environment
  anyway. The one write reached only the sandboxes, which belong to an
  environment now as well.
- **A credential of the tenant with the environments that may use it.**
  Closes the reach, but keeps the sharing and adds a list to keep right
  beside every credential.
- **The environment's spec bindings alone, which a sandbox names.** No
  new object: a sandbox takes a binding of its environment's deployment.
  Rotating a sandbox's key would be a deployment, and a sandbox would
  get only what the services get, at their pin.
- **A page that indexes the bindings where they are.** No new object:
  the console lists the sandboxes' sets and the specs' bindings and
  writes a credential into each place. It needs only the session on a
  sandbox's set. Rejected: the console would hold together what the API
  keeps apart, and rotation would stay one write per place.
- **The latest version for the environment's bindings too**, without a
  pin. Rotation would reach the services at once, but a deployment would
  no longer say what its pods get, and a revert would not restore it.
  An environment moves its pin with a deployment, as it does for
  secrets; the console offers it.

## Consequences

- A person with `write` on an environment states, rotates and uses its
  credentials, and those of no other environment.
- One write rotates a credential for every sandbox of its environment at
  once; the services follow with a deployment, which the console offers
  in one place.
- An API that several environments call is stated in each, with a key
  of each where the vendor allows it; a leak in one is a rotation there.
- A sandbox's token keeps its own set, which FeCode uses, unchanged.
- A pod's bindings come from three sources (ADR 0028); its bindings'
  version names all of them, so that any change reaches the pod.
- A credential's definition is readable by `read` on its environment,
  its secrets by no one; an upstream that echoes one gets it masked (ADR
  0028).
- A credential is an HTTP binding's; the object is shaped so that
  Kubernetes, SSH and registry credentials can become an environment's
  the same way once their node side exists.

## History

Decided and built on 2026-11-06 (b6a9fb6, the console ff5ae68) as
credentials of the tenant, the day after ADR 0028, as its third source
of bindings. An environment's page shows where each binding's credential
comes from since 2026-11-07 (5453149). Corrected on 2026-11-10 and
merged on 2026-11-19: a credential became its environment's (migration
0035), since ADR 0035 had put sandboxes and grants into environments
the day after and a tenant's credential reached across them. None had
been made by 2026-11-10; the migration refuses while
`tenant_credentials` holds a row, whose secret no migration can move.
Until then a kept secret went with whatever upstream, CA or token URL
the write named; a review before the merge found it, and since then it
is kept only for the latest version's.
The page's view of a credential's requests, decided at first, is not
built; the sink's egress records are read per environment instead.
