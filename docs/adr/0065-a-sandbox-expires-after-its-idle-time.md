# ADR 0065: A sandbox expires after its idle time, and the store keeps only what something live reaches

**Status:** Proposed · **Date:** 2026-11-28

A sandbox **expires** once it has been idle for its `expiresAfterIdle`.
Its creator sets this value when making it: a number of days or hours,
or `never`. When the creator names none, the installation's default
applies, 30 days. An expired sandbox keeps its row: its ID, its
environment, its spec, who made it, when it was made and when it was
last used. It loses its pod and its checkpoint, so it cannot be
resumed, and the only thing left to do with it is delete it. `ferund`
sweeps the object store every day. Live is what a row still names:
every sandbox's checkpoint, every bake, and every backup that ADR 0064
keeps. The disk daemon deletes everything else once it is older than
its grace. So the chunks that only an expired or a deleted sandbox
reached leave the store, and the chunks it shared with others stay.

## Context

A sandbox that nobody uses is paused on its node and hibernated into the
object store after a while (ADR 0008, ADR 0015). Nothing ends it
after that: a hibernated sandbox waits for its next tool call forever.
FeChat makes a sandbox for every conversation that runs a command and
never deletes one; 43 of them were in the store on 2026-11-28, the
oldest from 2026-11-09. The checkpoint of a conversation's sandbox
alone, the chunks nothing else reaches, was 7–28 MB in most cases and
160–180 MB where the memory was nearly full; its disk was mostly the
image's bake, shared with every other pod of the image.

Deleting a sandbox did not free its storage either. The disk daemon's
sweep (ADR 0011) existed, but nothing called it. The store held
56 GB of checkpoints on 2026-11-28, the deleted sandboxes' among them,
and 3.3 GB of layers that no head reached at all.

Different callers want different lifetimes. A conversation in FeChat is
seldom resumed after weeks, and a person who comes back can start over
in a new sandbox. A FeCode workspace is somebody's work in progress:
it may wait a month for review, and FeCode deletes its sandbox when
the workspace goes (FeCode ADR 0021). So the lifetime belongs to the
creator, and the platform gives a default to those who do not choose one.

The chunk store shares chunks between all pods, so a sandbox has no
files of its own to delete. What only one sandbox reached is known
only by asking what everything else still reaches, and that is the
sweep's question.

## Decision

### A sandbox's idle time

- `POST /api/v1/sandboxes` takes **`expiresAfterIdle`**: `"<n>d"`,
  `"<n>h"` (at least one hour) or `"never"`. If the creator leaves it
  out, the installation's default applies (`--sandbox-expires-after-idle`,
  30 days). `PUT /api/v1/sandboxes/{id}/expiry` changes it in place.
  The sandbox's status gives the value and, while the sandbox is not
  running, `expiresAt`.
- The time counts from the sandbox's **last activity**: the same
  `last_active_at` that the idle pause reads. A sandbox in use is never
  idle long enough, however old it is.
- It applies to the sandboxes a caller makes. An environment's instances
  and a project's jobs are not made by a caller: they live and go with
  their deployments, and they have no `expiresAfterIdle`.
- Rows that exist at the release get `never`. Nothing made before
  expiry existed loses its data to it; its creator may set a time in
  place.

### An expiry ends the pod and keeps the row

- An expiry is an op of the sandbox engine, like a hibernation. It
  checks the row again when it begins: the sandbox is not running, no op
  is in flight, nothing holds it, and its idle time has passed. Any
  state but `running` and the states between two others can expire.
- The op ends the sandbox's pod as a delete does, by tombstones
  (ADR 0015), so a paused pod's snapshot on its node goes too. It
  clears the row's checkpoint, the head the sweep would keep, and sets
  the client state to **`expired`**, with a status description that
  gives the day and the idle time.
- An expired sandbox answers its tool calls, files, processes, port
  door, resume and token mint with **410 Gone**, code `expired`. A
  route or a port that would wake it answers the same. Reading,
  listing and deleting it work as before.
- An expired sandbox counts toward no limit of its project and toward no
  use (ADR 0059). An environment whose remaining sandboxes are all
  expired can be deleted, and its expired rows go with it.
- The sink records the expiry on the sandbox's object, so the console
  and a project's logs show when and why it ended.

### `ferund` sweeps the object store

- A loop of the leader runs the disk daemon's sweep once a day, through
  one ready node per object store. It names as live:
  - every sandbox row's checkpoint head, in any state, instances and
    jobs included;
  - every layer of the image catalog (ADR 0022);
  - the backups that ADR 0064 keeps.
  The disk daemon deletes what none of these reach, layers and chunks
  down the chains included, once it is older than its grace (ADR 0011:
  24 hours, more than 12 hours plus the longest hibernation). A
  hibernation still being written is younger than the grace and is
  kept.
- The live set is read in one transaction. A sweep whose read fails,
  or whose live set is empty, deletes nothing. A run that reaches its
  time budget stops, and the next run continues.
- `--object-store-sweep` is `off`, `dry-run` or `on`. In `dry-run` the
  sweep reports what it would delete without deleting anything. Every
  run logs what it saw and what it deleted, and exports both as
  metrics.

## Alternatives considered

- **A lifetime counted from creation**: simpler to explain, but it ends
  a sandbox somebody uses every day. The idle time ends only what is
  left alone.
- **Deleting the sandbox at its expiry**: the row is gone, and with it
  what the sandbox was, who made it and when it last ran. A caller that
  finds a 404 cannot tell an expired sandbox from one that never
  existed. The row is small, and the caller decides when it goes.
- **The default for the rows from before, counted from the release**:
  it would have cleaned up FeChat's old conversations within a month,
  and it would have ended any sandbox whose creator did not know expiry
  was coming. Nothing that existed before expires by itself.
- **Deleting a sandbox's own chunks at its expiry**: no chunk is a
  sandbox's own. Whether one is shared is only known against
  everything else that is live, and an expiry that asked that question
  for itself would be a sweep without the sweep's safeguards.
- **`never` only for a project's admins**: FeCode needs it for every
  workspace it makes, and FeCode is an integration with the role its
  project granted it (ADR 0056). Whoever may create a sandbox may say
  how long it lives.
- **A default per project**: no caller needs it yet. FeCode and FeChat
  each set one value for all their sandboxes. It is in the backlog.
- **Firecracker's diff snapshots instead of full ones**: the large
  checkpoints were large because the guest's memory was full. A diff
  would have been no smaller, and expiry is about how long a checkpoint
  stays, not how large it is.

## Consequences

- The store shrinks once: the first run that is `on` deletes what
  deleted sandboxes and abandoned hibernations left behind. A
  `dry-run` comes first on every installation.
- After that, a sandbox costs storage until it is deleted or has been
  idle for its time, plus the sweep's grace.
- FeChat leaves `expiresAfterIdle` out and gets the default. A
  conversation whose sandbox expired gets a new sandbox at its next
  command, and both the person and the model are told that the files
  from before are gone.
- FeCode makes its sandboxes with `never` (FeCode ADR 0021). Its
  sandboxes from before have `never` already.
- The console shows an expired sandbox with its day, offers only
  deleting it, and shows `expiresAt` for every sandbox that is not
  running.
- The client states gain `expired`. A client that does not know the
  state reads it as one it cannot use.

## History

Proposed on 2026-11-28 after measuring the store: 83 GB across the
installation's buckets, 56 GB of it checkpoints, and no sweep ever run.
ADR 0064 had decided the sweep for the backups of postgres addons; this
ADR runs the sweep on its own, ahead of those backups. The same day the
rows from before were given `never` instead of the default counted from
the release, so that no data is lost to the change (migration 0045).
