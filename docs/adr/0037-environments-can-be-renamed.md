# ADR 0037: An environment can be renamed; its ID is its identity

**Status:** Accepted · **Date:** 2026-11-08 · Amends [ADR 0018](0018-environments-and-releases.md)

## Context

ADR 0018 gives an environment a UUID and a name, a DNS label unique per
tenant, and lets every route take either as its `{env}`. Nothing could
change the name: a client that wanted another one had to clone the
environment under a new ID and delete the old one, losing its data, its
hosts, its grants and its history.

FeCode, the first client, names the environments it makes. Its names
were a project, an environment and eight random hex digits
(`kino-prod-9101dfcb`); it now names them by structure, segments joined
by `--` (`kino--prod`, `kino--tools`, `kino--ws--login-fix`), readable
and free of collisions without the random part. Its environments that
exist must keep running under the new names: same environment, same ID,
new name. FeCode addresses environments by ID in nearly every call, but
FeRun itself named a few things by the environment's name.

## Decision

- **A rename.** `PATCH /api/v1/environments/{env} {"name": "<new>"}`
  gives the environment a new name and answers its status as `GET`
  does. The name is checked as a create's (a DNS label, not a UUID;
  400); a name another environment of the tenant has is 409
  `name_taken`; the name it has already is 200 and changes nothing; an
  environment being deleted is 409. It is the environment admin's or the
  tenant writer's, as a deletion is.
- **The ID is the identity.** Everything FeRun keeps or makes for an
  environment is keyed by its ID; the name is a handle for people and a
  second way to address it in a path. What still used the name moves to
  the ID: an attached service's process is `ferun-env-<env id>-<service>`
  (it was `ferun-env-<env name>-<service>`).
- **What a rename leaves:** the ID, labels, deployments, tags, secrets,
  grants, tenant tokens, route tokens and domains; the default host (its
  label was claimed at the first publish and is kept, also by a zone
  move); every pod, template, group and name table (nothing rolls);
  attached services' processes; logs, traces, access logs and traffic,
  which the sink labels with the ID, so an environment's history stays
  one history.
- **What a rename changes:** the name in every answer and message, the
  `{env}` of a path that names it (the old name is 404 and free for
  another environment of the tenant at once), the console's URLs, which
  name it. An environment that was never public gets its default host
  from the name it has when it is first published.
- **No transition of data.** No migration: the name is one column. An
  attached service's process started under the environment's name by a
  ferund before is found by the ID its row records, before and after a
  rename, and runs on until it is replaced; the stop of strays also
  looks for the environment's name of now.

## Alternatives considered

- **Clone and delete.** A new ID: new hosts, addons empty, grants,
  tokens and logs left with the old environment. It is what a rename
  must not be.
- **Aliases** (the old name still resolving, or reserved). A name is a
  handle the client chose; a client that renames knows the new one, and
  every lasting reference uses the ID. Aliases would make a name mean
  two environments over time and keep names taken nobody uses.
- **Display names in labels**, the name fixed. The console, the
  messages and FeCode's operators read the name; a label nobody shows
  would leave the random suffix where people look.

## Consequences

- A client that stores an environment's name, or builds URLs with it
  (the route door's, the console's), must store the new one; by ID
  nothing changes. Log and trace queries are by ID and need nothing.
- A rename and the client's own record are two writes; the rename is
  idempotent (the same name again is 200), so a client retries it.
- Order: ferund first; its first version names new attached processes
  by the ID and adopts the running ones. Then the client renames.
- The console stops reading anything into a name: it showed the eight
  hex digits FeCode added dimmed; it shows the name whole.
