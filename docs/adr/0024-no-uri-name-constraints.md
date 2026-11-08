# ADR 0024: The platform CA carries no URI name constraint, and nodes trust a root for its key

**Status:** Accepted · **Date:** 2026-10-29

The platform CA has one root, kept offline, and one issuer that every
control-plane instance holds; both are constrained to the platform's DNS
domains and to no IP address, and neither constrains URIs, because
rustls-webpki, with which `feosd` verifies the control plane, refuses
every URI name beneath a URI constraint. The trust domain is bound to
the issuer by a record beside it, which `serve` checks, not by the
certificate. A node trusts a root of its bundle, or of its join token's
pin, for its key and not for its name constraints; the issuer's
constraints still hold, and the control plane's leaf is held to its
configured DNS name, its one URI and no IP address. A new issuer under
the same root replaces an old one without a new root, bundle or join.

## Context

- The platform CA (ADR 0014) issues for nodes, ingress and sink
  instances, pods and the instances' own port. Its name constraints keep
  a leaked CA key from naming anything on the internet: DNS names only
  under the platform's domains, no IP address.
- A URI constraint, restricting URIs to the trust domain, adds nothing:
  every verifier of this platform reads an identity for the exact trust
  domain, and issuance never writes a URI outside it. The constraint
  would only keep a compromised issuer from naming another trust domain,
  which no verifier accepts.
- rustls-webpki, from 0.103.16, does not evaluate URI name constraints
  and fails closed: it refuses every URI name beneath one. The control
  plane's certificate on the nodes' port carries the URI
  `spiffe://<trust domain>/ferun`, which is how a node tells it from a
  pod's certificate of the same issuer. Under a URI-constrained CA a
  node refuses every chain: on the join, on every node connection and on
  the uplink.
- The trust domain's binding to the CA is a configuration check, which
  does not need the certificate.

## Decision

- **No URI constraint.** The root and the issuer carry DNS and IP
  constraints only. The issuer's metadata records the trust domain
  beside the control-plane domain, and `serve` refuses a trust domain
  other than the recorded one. An issuer of before, without the record,
  is bound by its URI constraint as before.
- **A node trusts a root for its key.** `feosd` makes its trust anchors
  from the trust bundle, and on a join from the pinned root, without the
  root's name constraints. The constraints of the certificates between
  the root and the leaf, the issuer's, still hold, and the control
  plane's certificate is held to the configured DNS name, the one URI
  and no IP address, which is more than the root's constraints asked.
- **A new issuer under the same root.** `ferund new-issuer` reads the
  root's certificate and key and makes a new issuer into a new CA
  directory, without a URI constraint, and loads it before it returns.
  The root, the bundle and the join tokens' pin stay, so nodes keep
  their identities and need no new join. It replaces an issuer that
  constrains URIs and an issuer near its end.
- **A warning, not a refusal.** `serve` warns when its issuer constrains
  URIs and names `new-issuer`; it serves nodes of before meanwhile.

## Alternatives considered

- **`feosd` checks the chain itself**: the issuer against the root by
  hand, the leaf with rustls-webpki against the issuer, and the URI
  constraint in its own code. No ceremony, but path validation of our
  own on the node's most sensitive connection, kept only until
  rustls-webpki checks URI constraints.
- **A new root without the constraint**: the cleanest result, but every
  node and ingress instance must trust the new root before the instances
  serve under it, which takes a renewal of every client or a restart and
  a join again, and every pod and ingress certificate is issued again.
  Where the old root's key is lost, it is the only way
  ([Replacing the root](../architecture/control-plane.md#the-ca-ceremony)).
- **Pinning rustls-webpki below 0.103.16**: it would take back a
  security update.

## Consequences

- The root's constraints are a ceremony's promise, no longer a node's
  check: a node relies on the issuer's constraints, which `new-issuer`
  and `init-ca` make the same, and on its own checks of the leaf.
- Go's chain verification admits a foreign trust domain's URI; nothing
  reads one, since every identity is parsed for the exact trust domain,
  and a test says so.
- Replacing the issuer on a running platform is a ceremony with the
  root's key and a rolling restart of the instances before the nodes
  roll; the order is in
  [The CA ceremony](../architecture/control-plane.md#the-ca-ceremony).

## History

Decided 2026-10-29 (0065d53), after a rollout of `feosd` with
rustls-webpki 0.103.16 took the nodes of shoot `fecode` down within a
minute, since the CA of `init-ca` constrained URIs; the rollout was
taken back, the constraint dropped, and the ceremonies for replacing an
issuer and a root described (38d21b5). Consolidated 2026-11-08 and kept
as a decision of its own beside ADR 0014, which cites it for the trust
domain and the CA.
