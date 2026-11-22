# ADR 0058: A spec says what its code expects of a secret, and the plan checks it

**Status:** Accepted · **Date:** 2026-11-21

A spec may state, for each secret it names, what its code is written
for: the secret's kind and, for an HTTP secret, its mode and whether
the host is forwarded. The plan compares that with the version the
deployment would pin and gives an error finding where the two differ.
The expectation travels with the code, from a workspace to production;
the secret stays the environment's, and no deployment changes it. No
mode hands an HTTP secret's key to a container: an API whose client
must hold the key is a value secret.

## Context

ADR 0055 made the mode a part of the secret: a version of an HTTP
secret says whether the code calls it by the secret's name or by the
API's own host, and a deployment pins the version. The code, though, is
written for one of the two. In the name mode it takes
`http://<name>` from its variable and speaks plain HTTP; in the
intercept mode it calls the API's host over TLS and trusts the pod's
CA; and an API that builds absolute links stays on the binding only
when the host is forwarded. A secret made in the other mode fails at
the first request, in the environment where it was made differently,
which is seldom the one the code was written in.

Nothing in a spec says which mode the code wants. A written mode is
taken for a claim about the secret and refused unless it repeats the
pinned version's, with the remedy to remove it; a clone drops it with
the pin. FeCode closes the gap in front of FeRun: its file states type,
mode and the forwarded host per secret, and FeCode compares them with
the environment's secret before it asks the plan. It turned down a mode
chosen per place for the reason above, and a warning because a warning
deploys. A caller without FeCode has no such check.

FeCode's file has a third mode, `env`, in which the credential's fields
are variables of the containers, rendered to value secrets, with two
ticks by which a project admin consents. On FeRun the kind says where a
key lives and never changes (ADR 0055).

## Decision

### The spec states an expectation

- In a spec's `secrets`, a name may carry `type`, `mode` and
  `forwardedHost`. Written by a caller they are what the code expects:
  `type` is `value` or `http`; `mode`, for an HTTP secret, is `name` or
  `intercept`; `forwardedHost`, in the name mode, is whether the node
  forwards the host.
- What is left out is not expected: a spec that names a secret without
  them takes the secret as it is, and its normalised form carries the
  pinned version's kind, origin and mode, as ADR 0055 has it.
- The expectation stands once per secret, where the pin stands, never
  on a reference. A reference's shape still says the kind, a key for an
  HTTP secret and none for a value, and the two must agree.

### The plan checks it

- The plan compares the expectation with the version it would pin. A
  secret of the other kind, in the other mode or with the host
  forwarded otherwise is an **error finding** at the secret's path,
  naming the environment's secret and what the spec expects. Nothing is
  deployed.
- The remedy names both ways: change the secret, or change the spec.
  The mode and the forwarded host of an HTTP secret change with a new
  version and without its key (ADR 0055), so the first is a click for
  someone with `write`; the kind does not change, so there the secret
  is made anew under the kind the code wants, or the code follows.
- A secret that is missing has the finding it always had, and that
  finding now says what to make: the kind and the mode the spec
  expects.
- A normalised spec keeps the expectation, so a revert is checked
  against it as it was. A clone and a plan from another environment's
  deployment carry `type`, `mode` and `forwardedHost` over and leave
  the version and the origin behind, which are the place's.
- A sandbox states no expectation. It names its environment's HTTP
  secrets and takes them as they are.

### An HTTP secret's key reaches no container, in any mode

- There are two modes, and in both the node alone uses the key. A
  client that must hold a key itself, because it signs requests with it
  or speaks a protocol the node does not offload, gets it from a value
  secret, referenced as a value, and the environment's list of secrets
  says so by its kind.

## Alternatives considered

- **A mode `env` on the HTTP kind**, FeCode's third mode moved to
  FeRun: one declaration per API whatever its client needs, and the
  same form for its credential. ADR 0055's rule that an HTTP secret's
  key reaches its API and nothing else would then hold for two modes of
  three; a person with `write`, who reads no key, could change a
  secret's mode and read it from a container; and the kind would no
  longer say where a key lives. Guarding the change of mode with the
  key, as a change of origin is guarded, would save the rule and leave
  a kind that means two things.
- **The expectation on the reference**, `{secret, key, mode}`: ADR 0055
  turned the mode on the reference down, since two references of one
  pod could disagree. Stated once in `secrets` it cannot.
- **A warning.** The request that follows fails for certain; a warning
  reports it and deploys.
- **The check in front of FeRun**, in each caller, as FeCode had it: a
  second judge, a copy of the rule, and none for a caller without one.
- **The mode decided by the spec**, the secret following: a deployment
  would write a secret, which only someone who sets it does (ADR 0055),
  and two deployments that expect different modes would turn it back
  and forth.
- **A written mode stays a claim to be removed**, as it was: a spec
  could not say what its code needs, and an exported spec carried a
  mode that its next reader had to delete.

## Consequences

- A written `mode` is read as an expectation and no longer refused; a
  written origin is still the pinned version's and still compared, as
  on a revert. A pin gains `forwardedHost`.
- The mode a deployment fails for is found at the plan, in every
  environment, for every caller. FeCode drops its own comparison and
  its `secrets` block is FeRun's (FeCode's ADR 0038).
- FeCode's `env` mode has no counterpart here. FeCode declares such an
  API's key as value secrets, in its file as on FeRun (FeCode's
  ADR 0038), and its two ticks go: the kind is the statement.
- A copy of an environment keeps what its code expects, so a secret
  made in the copy under the wrong mode is found before the first
  request.
- The console's builder shows the expectation with the secret's row and
  offers it when a missing secret is made.

## History

Proposed on 2026-11-21, together with FeCode's ADR 0038. It amends ADR 0055, under which a spec's written mode was the
secret's own and an error unless it repeated the pinned version's. A
mode `env` for FeRun was weighed first and set aside against ADR 0055's
rule for a key.

Accepted on 2026-11-22. Built on the branch ferun-layer by parallel
builders against one contract and merged into master as 562ee9e5; live
on EKS in Frankfurt the same day, in one maintenance window: ferund's
migrations 0041 to 0043 and the sink's 0009 ran once, and FeCode
(c50a3df) moved its thirteen projects into FeRun projects of their own.
