# ADR 0019: Pods have short IDs that the control plane mints

**Status:** Accepted · **Date:** 2026-10-26

A pod's ID is 11 characters of lowercase Crockford base32, a letter first,
minted from a cryptographic random source by the control plane, or by
`feosctl` on a node without one; a client never chooses one. The ID names
the pod's microVM, its paths on the node and its TAP device whole, so two
pods on a node cannot collide, and it is a DNS label, a file name and a
cgroup's name as it is. Pods made before short IDs keep their UUIDs until
they are deleted; nothing makes new ones with UUIDs.

## Context

A pod's ID names its microVM, its paths on the node, and its TAP device
(ADR 0010). A TAP device's name has 15 characters at most (`IFNAMSIZ`),
and `pod-` takes four of them. An ID chosen by a client is a UUID at best,
whose first 8 hex digits do not fit the TAP name alone: the high bits of a
UUIDv7 are its time stamp, so every UUIDv7 made within the same 65 seconds
starts alike, and a placement that keeps pods with alike prefixes off one
node finds no node for the third sandbox within a minute on a cluster of
two.

Any rule that lets a client choose a pod's ID and squeezes it into 15
characters can collide. The ID itself has to fit.

## Decision

### The format

A pod ID is 11 characters of lowercase Crockford base32
(`0123456789abcdefghjkmnpqrstvwxyz`), the first of them a letter:

    ^[a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{10}$

That is about 2^54 IDs. `pod-` and the ID are 15 characters, a TAP
device's name. The ID is also a DNS label, a file name, a cgroup's name,
and a jailer's ID as it is; no character needs escaping anywhere. The
leading letter keeps an ID from being read as a number, and the alphabet
leaves out `i`, `l`, `o` and `u`, which are misread for digits.

### Who mints them

- **The control plane mints every pod's ID**, a sandbox's and an
  environment's replica's, from a cryptographic random source. Its
  sandboxes' IDs are unique over all tenants; a create whose new ID is
  taken draws another.
- **A client does not choose one.** A create that names an ID is refused;
  the answer carries the new sandbox's ID, which the client keeps. A
  create is not idempotent: the client keeps the ID before it relies on
  the sandbox.
- **`feosctl`** mints one the same way for a pod it creates on a node
  without a control plane.

### What the node makes of them

- The TAP device is `pod-<ID>`.
- The guest's MAC address is locally administered and derived from a hash
  of the ID: stable for the pod, and not the ID's characters, which are
  not hex.
- `feosd` checks a pod ID on every call that takes one, and refuses
  anything else.

### Pods with UUIDs stay

A pod made before short IDs keeps its UUID until it is deleted: its
microVM's snapshot names its TAP device, and a hibernated pod comes back
on any node under the same name. `feosd` accepts a UUID as a pod ID and
names its TAP device and its MAC address from the UUID's first 8 hex
digits; the control plane stores UUIDs and short IDs side by side and
keeps a placement gate for TAP names, which only UUIDs can share. Neither
makes new pods with UUIDs. The UUID path goes once no pod with one is
left.

## Alternatives considered

- **Clients keep choosing the ID**, with a rule that fits it into the TAP
  name. Any such rule can collide; UUIDv7 collided within a minute.
- **A TAP name of the node's own**, kept beside the pod's ID. The
  snapshot of a suspended pod names its TAP device, so the name has to be
  the pod's wherever it comes back.
- **Idempotent creates by a client-chosen ID**, which is what let clients
  choose. The client keeps the ID the answer carries instead, before it
  relies on the sandbox.

## Consequences

- A pod's ID is its TAP device's name: no two pods on a node can collide,
  and the placement gate only matters for pods with UUIDs.
- Clients keep a mapping from their objects to sandbox IDs: a client with
  a database in its database, one without by handing its user the sandbox
  ID bound to them.
- The control plane's sandbox IDs are text with a check of the two
  formats; its other IDs (tokens, environments, volumes, incarnations)
  stay UUIDs.
- 2^54 IDs are not secrets and not meant to be: a sandbox is reached with
  its token, never by knowing its ID.

## History

Decided on 2026-10-26 (559813f), after a cluster of two nodes refused a
third sandbox within a minute because its UUIDv7's prefix was taken. Rolled
out the same day in three steps that each worked with the one before:
clients stopped sending IDs, `feosd` took short IDs beside UUIDs (c66a2b1),
and the control plane minted short IDs and stored IDs as text (738495f,
af4f950).
