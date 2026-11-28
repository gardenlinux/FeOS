# ADR 0064: A postgres addon is backed up through its node, restored anywhere, and kept by the platform

**Status:** Proposed · **Date:** 2026-11-28

A postgres addon's data lives in the object store as well as on its
node. The disk daemon snapshots the addon's volume while it runs, every
day and before every change that could lose data, and the node archives
PostgreSQL's WAL segment by segment, at most a minute behind its
commits; both go the way a hibernation's data goes, and nothing in the
guest holds a key of the store. From them an addon is restored to any
moment of its retention, always as a new addon, on any node, its volume
read lazily from the store; a clone brings its data along; an addon
whose node is lost comes back on another node, short of at most the
last minute; and a removed addon's backups stay for its retention. On
that ground the platform keeps the addon: it moves it off a draining
node with a snapshot ahead, applies minor releases in the environment's
maintenance window, grows its volume, upgrades its major version with a
snapshot to go back to, runs an image of FeRun's own with curated
extensions and the parameters the blueprint allows, reports the addon's
figures and opens incidents on them, tests its restores every week, lets
people in through a sandbox, imports and exports databases, idles the
addon with its environment and runs a standby for an addon that asks
for one. It is built in three steps, the backups first, and amends
ADR 0040 for postgres; valkey and nats keep its rules.

## Context

An addon is one instance on a volume of its node (ADR 0040), and the
volume is a disk of that node's disk daemon (ADR 0016). Its data is
nowhere else: an addon has no backup and no copy, a clone's addons start
empty, and an addon lost with its node is not replaced, since its data
is where it cannot run; one whose node was removed waits for good, and
the only way out is to remove it and add it again, empty. A node's disk
may be a cloud machine's local storage, which goes with the machine: a
database then lives as long as one machine, and a node that is replaced,
by a failure, a new machine image or a smaller pool, takes its data
along.

The rest of a managed database is missing too. Removing an addon, or
changing its type, deletes its volume and credentials at once, in a
protected environment as anywhere. The image is the upstream one, a tag
whose digest a row pins when it is made, so a patch release arrives
only with a recreate that happens for another reason, and no extension
beyond PostgreSQL's own is there. A change of major version is refused
in both directions. A plan change keeps the volume's size. Its options
are databases and roles, nothing else. Nobody sees an addon's fill, its
connections or its age of transaction IDs before it fails, since FeRun
keeps no figures of its workloads; and no person reaches an addon,
since its map grants only the services and jobs that reference it
(ADR 0038).

What a project expects of a managed PostgreSQL is what Amazon RDS and
CloudNativePG (CNPG) do. Both keep backups that restore to any moment
of a retention, RDS's of up to 35 days, and both restore into a new
instance, never over the running one. Both take a backup in two parts,
a copy of the data from time to time and the WAL in between: RDS
snapshots its storage and ships the log to S3 every five minutes, CNPG
archives the WAL to an object store and takes base backups with Barman
or as snapshots of the volume. Both apply minor releases in a window,
upgrade major versions, set parameters from a list, grow storage, offer
extensions, report figures, run a standby with automatic failover (RDS's
Multi-AZ, CNPG's replicas with failover and switchover), stop or
hibernate an instance, and import a database from elsewhere.

FeOS has most of the parts. The disk daemon cuts every disk into
chunks named by their content, compressed in the object store, and
uploads only those the store lacks (ADR 0011); a hibernation takes a
pod with its volumes and its memory to the store and brings it back on
any node, its chunks read as they are needed (ADR 0008, ADR 0016), and
a drain moves an addon that way today. The daemon seals only a disk
that is stopped; its one live seal, a bake, needs the guest's
filesystem frozen and stacks a layer the chain has no place for. A
hibernated pod keeps its checkpoint in its record after it runs again,
so an addon that a drain once moved still names the checkpoint it came
from, and a node lost after that would bring it back as it was at the
drain, while the newer volume left on that node is swept. All of
a node's traffic with the store is the daemon's, with one key for the
installation (ADR 0011): nothing in a pod reaches the store, and an HTTP
binding (ADR 0028) adds a header to a request and signs nothing. A
container already reaches the host through a socket of the guest's, as
it reaches its identity (ADR 0014). PostgreSQL has the rest: continuous
archiving through `archive_command` and `restore_command`, a base backup
by any copy taken between `pg_backup_start` and `pg_backup_stop`,
recovery to a moment onto a new timeline, streaming replication and
`pg_upgrade`.

## Decision

### A backup is a snapshot of the volume and the WAL after it

- **A snapshot of a running volume.** The disk daemon cuts a running
  volume: the device's one thread (ADR 0011) switches its top between
  two requests, the old top taking no more writes and a fresh one taking
  them. The cut is the volume at one instant, the state after a power
  cut, which ext4's journal and PostgreSQL's WAL keep whole, as they
  keep a hibernation's disk (ADR 0008); the filesystem is not frozen.
  The cut top is sealed into the volume's own layer and
  uploaded while the pod writes on, and a head names the snapshot, last.
  Between two snapshots the chain has its shape of ADR 0011; during
  one, the cut top lies between the volume's layer and the fresh top,
  and reads go through it.
- **Inside PostgreSQL's backup.** The cut is taken between
  `pg_backup_start` and `pg_backup_stop`, in one session of a helper of
  FeRun's in the addon's image, which asks the node for the cut and
  waits for it. The backup label PostgreSQL returns goes into the
  snapshot's head, so that a restore knows from where the WAL is
  replayed and from when the data is consistent.
- **Incremental by content.** A snapshot's layer names every chunk of
  the volume, and its upload sends only the chunks that changed since
  the snapshot before, as a hibernation does. A snapshot costs what was
  written since the last one, not the volume's size.
- **When.** Every day, at a time `ferund` spreads over the addons;
  sooner once the WAL archived since the last snapshot passes a bound,
  so that a restore replays at most that much; before every change of
  the platform's or the project's that could lose data, a move, a minor
  release, a major upgrade and a removal; and on demand, under a name,
  by a principal with `write` on the environment. When a snapshot is
  taken is `ferund`'s policy; the node takes it.
- **The WAL through the node.** `archive_command` hands every finished
  WAL segment and history file to the helper, which hands it through a
  socket of the guest's in the addon's container to `feosd` over vsock,
  and `feosd` has the disk daemon put it, compressed, into the addon's
  archive in the store. The command succeeds only once the segment is
  in the store. A name is written once: a put of a name the archive has
  succeeds for the same bytes and is refused for others, as PostgreSQL
  asks of an archive. `restore_command` fetches through the same path.
- **A minute at most.** `archive_timeout` is one minute: a committed
  transaction is in the store a minute after its commit, and the
  upload's time, or the archive has failed and an incident says so.
- **The archive is the addon's.** A pod's spec names the addon's archive
  beside its volume, and the guest gives the addon's container that
  archive and no other; a restore's pod reads its origin's archive as
  well, and never writes it.
- **No key in the guest.** Nothing of the store's credentials reaches
  the pod, and the addon needs no egress for its backups: the bytes go
  the node's way, which a hibernation's go.

### What a backup is kept for

- An addon keeps its backups for its **retention**: seven days unless
  its declaration names another, at most 35. Every moment of the window
  is restorable: the oldest snapshot kept is the newest one at or before
  the window's start, and the archive is kept from that snapshot's label
  on.
- An addon may turn its backups off. The plan warns that a lost node or
  a removal then loses its data, as ADR 0040 has it.
- **`ferund` sweeps the store.** It names what is live, the pods'
  checkpoints, the bakes, the snapshots within their addons' retentions
  and the archives' segments from their oldest label on, and the disk
  daemon deletes the rest once it is older than its grace (ADR 0011).
  A snapshot on demand stays until it is deleted or its addon's backups
  go.
- **A removal keeps the backups.** An addon that a deployment removes,
  or whose type changes, or whose environment is deleted, gets a last
  snapshot after its last archived segment, and its backups stay for its
  retention; the plan's warning names the day until which it can be
  restored, by its name. A project's deletion keeps its addons' backups
  for their retentions too, restorable by the installation's operators
  alone. Only an addon without backups loses its data at once.
- Backups count toward the project's use (ADR 0059): what its
  snapshots' chunks and its archive hold in the store.

### A restore is a new addon

- **Always new.** An addon is restored from another's backups when it is
  made: its declaration names the addon it comes `from`, by name, in its
  own environment or another of the project's, and a moment within that
  addon's retention, the newest by default, or a snapshot on demand by
  its name. `from` acts in the deployment that makes the addon and on
  nothing after: a plan that changes it on an addon that exists is
  refused with a finding, and a revert that brings an addon back with
  its `from` restores it anew. Nothing is restored over a running addon.
  To put production back to a moment, a deployment adds the restored
  addon under a new name and moves the references to it; the addon it
  replaces stays as long as the spec has it, and keeps its backups when
  it goes.
- **Lazy.** The restored volume starts with the snapshot's layer as its
  own, a manifest copied and no byte of data, on whatever node the addon
  is placed. The disk daemon reads its chunks from the store as they are
  read and fetches the rest behind them. PostgreSQL starts from the
  snapshot's label, replays the origin's archive to the moment, is
  promoted onto a new timeline and archives from then on into the new
  addon's archive.
- **An addon of its own.** A restored addon has its own credentials, its
  server key and certificate, which its provisioning writes over those
  on the volume, its own workload identity and map (ADR 0038), its own
  archive and retention. Its databases and roles are the snapshot's, and
  its provisioning adds those its declaration names and drops none
  (ADR 0040). A version older than the snapshot's is refused; a newer
  major is an upgrade, below.
- **Who.** Taking an addon's data into another environment needs
  `write` on the environment it comes from, as reading that
  environment's secrets' values does (ADR 0031); for a protected
  environment that is a grant on the environment itself or the
  project's `admin` (ADR 0060).
- **A clone with data.** `clone` (ADR 0018) may ask for its postgres
  addons' data: each addon of the copy is then made `from` its source at
  the moment of the clone. Without it, a clone's addons start empty.

### A lost node costs an addon at most its last minute

- An addon with backups whose node is removed, or down for longer than
  the lost grace, is restored on another node from its newest snapshot
  and its whole archive, under its own name, with its ID, its
  credentials and its archive, on a new timeline. What it committed in
  the minute before the loss may be missing: the segment that was not
  yet archived. Its incident says so, with the moment it came back to.
- **Never from a checkpoint it ran past.** A pod's checkpoint is the
  pod at its hibernation. An addon that has run since is not restored
  from it when its node is lost; only its backups bring it back.
- **The bindings fence.** The dependents reach the addon through their
  TLS bindings, which dial it by its name (ADR 0038). The name now
  answers with the new pod, and every binding ends the connections it
  still carries to the address the name left, so that no workload
  outside the lost node writes to the old instance, a pool's open
  connection included; the old instance's own rules, on the lost node,
  would still admit them. A node that comes back stops the old pod
  before anything of it serves; its volume becomes a snapshot of its
  own, kept for the retention, so that an operator can still read what
  the archive missed, and then leaves the node.
- An addon without backups keeps ADR 0040's rule: it is not replaced
  elsewhere, and its incident says what the node answered.

### Upkeep is the platform's

- **Moves.** A drain moves an addon by hibernation, as it does today
  (ADR 0008), after a snapshot taken while it still runs: the
  hibernation then uploads only what was written since, and the addon
  is away for seconds. A restore with memory finds PostgreSQL running,
  and its clients reconnect by its name.
- **Releases are the blueprint's.** The blueprint names a release per
  major version and its image by digest, so that no recreate brings a
  release nobody chose. An addon's status says which release it runs
  and which one the blueprint has.
- **Minor releases in a window.** An environment has a maintenance
  window, an hour of the week, the installation's unless it names its
  own. In it, `ferund` brings every postgres addon whose release is
  older than the blueprint's to the blueprint's: a snapshot, a fast
  shutdown, the new release on the same volume, ready again. An addon
  that does not become ready goes back to the release it ran, which
  reads the same data. The addon's declaration does not change, so this
  is no deployment; the addon's record keeps each release it ran and
  since when. Outside the window a release comes by the project's own
  call, or by an operator's for a release that closes a vulnerability,
  announced on the environment a day ahead.
- **Major upgrades.** A higher major version in the declaration is an
  upgrade, which the plan warns stops the addon for its length: a
  snapshot, a fast shutdown, `pg_upgrade` with hard links on the volume
  in a pod of the new release that carries the old major's binaries,
  the new server started and its statistics gathered. An upgrade that
  fails brings the addon back from the snapshot taken before it, which
  nothing wrote after. An upgraded addon starts a new archive, since
  `pg_upgrade` makes a new cluster; its backups of before stay
  restorable for their retention, at their version. A lower major stays
  refused. PostgreSQL 18 joins 16 and 17; a major leaves the catalog
  with its end of life upstream, after a plan's warnings for a year.
- **The volume grows.** An addon's volume is its plan's size, or the
  larger size its declaration names, and a change to a larger plan or
  size grows it at the addon's next start, which the deployment brings
  about: the disk daemon grows the disk, and the guest grows ext4 with
  the kernel's online resize, no tool beside `mke2fs` (ADR 0016). It
  never shrinks; a smaller size is refused. An addon may name a ceiling,
  up to which `ferund` grows its volume in steps as it fills, in the
  window, or at once when the volume is nearly full; without one, a
  filling volume is an incident.
- **Fast shutdown.** For a snapshot ahead of a change, a release, an
  upgrade and a growth the platform ends PostgreSQL with its fast
  shutdown, which ends the sessions and writes a checkpoint, and not
  with `SIGTERM`'s smart shutdown, which waits for every client.

### The image, its extensions and its parameters

- **FeRun's image.** The blueprint runs an image of FeRun's own per
  major version, built from PostgreSQL's release with the helper, the
  extensions of the catalog and the previous major's binaries for
  `pg_upgrade`. It is published with FeRun's releases, so that every
  installation runs the same bytes, and an installation may mirror it.
- **Extensions from a list.** An addon names the extensions it wants
  from those the catalog lists for its version, pgvector, PostGIS,
  pg_cron, pg_stat_statements, pg_trgm, pgcrypto, citext, pgaudit and
  the like, and its provisioning creates them in each of its databases.
  One that must be preloaded the blueprint adds to the server's
  libraries, and the plan warns of the restart. A name off the list is
  an error of the plan; one dropped from the list stays, as a dropped
  database does (ADR 0040).
- **Parameters from an allowlist.** An addon names values for the
  parameters the blueprint allows, each with its type and its bounds,
  some relative to the plan's memory: `work_mem`, `max_connections`,
  `statement_timeout`, `idle_in_transaction_session_timeout`,
  `log_min_duration_statement` and the like. The plan says whether a
  change reloads the server or restarts it. What the platform sets stays
  the platform's: files, listeners, TLS, authentication, the archive,
  the preloaded libraries and `shared_buffers`, which follows the plan.

### Figures, incidents and restore tests

- **Figures.** Every minute, a step that runs the helper in the addon's
  container reads its figures: the volume's fill, its connections
  against `max_connections`, the age of its oldest transaction ID, its
  longest open transaction, the archive's last success and its failures,
  the newest snapshot's age, a standby's lag, and the statements that
  took the most time, from pg_stat_statements, without their literals.
  The latest are the addon's status; the series go to the sink as the
  addon's records (ADR 0026), from which the console draws them. FeRun
  keeps no metrics store for this; the sink is where the platform
  already says what its pods do.
- **Incidents.** An environment's incidents open on them, each closing
  once its condition has not held for a window, as a binding's do
  (ADR 0028): a volume beyond 85 % and beyond 95 %, connections beyond
  90 % of the maximum, transaction IDs approaching wraparound, an archive
  that has failed for ten minutes, no snapshot for two days, a standby
  out of reach or far behind, and a restore test that failed.
- **Restore tests.** Once a week `ferund` restores each addon's newest
  snapshot and its archive to its end into a pod that nothing reaches,
  waits until PostgreSQL is consistent, checks its catalog, records how
  long it took, which is what a lost node would cost, and deletes it. A
  failure is an incident of the addon's environment. The tests are the
  installation's cost, not the project's.
- PostgreSQL's log is the container's output and goes to the sink like
  any other's; `log_min_duration_statement` and pgaudit's parameters make
  it a log of slow statements and an audit where a project asks.

### People reach an addon through a sandbox

- A sandbox's `reaches` (ADR 0012) may name an addon with a role: one of
  the declaration's, or the addon's read-only role, which the blueprint
  makes with `pg_read_all_data`. The sandbox's workload is mapped to
  that role while the sandbox lives, as a reference maps a service's
  (ADR 0038), and its TLS binding gives `psql` an address on loopback:
  no password, and no certificate of the person's. The superuser is in
  no map.
- The console's `psql` is such a sandbox. Every session is a handshake
  that `feos-authn` signs and records (ADR 0014), with the sandbox and
  the person who made it.

### A standby, for an addon that asks

- An addon may ask for a standby: a second instance on another node,
  made as a restore is, from the newest snapshot and the archive, then
  streaming the WAL from the primary over the pod mesh, admitted by its
  workload's certificate as the replication role. Replication is
  asynchronous.
- **`ferund` decides which instance is the primary**, by a lease in its
  database, by the database's clock; the addon's name answers with the
  primary, and the primary alone archives.
- **Failover.** When the primary's node has been down for the addon's
  failover delay, a minute unless it names another, `ferund` promotes
  the standby, moves the name to it and makes a new standby on a third
  node. What the standby had not received is lost: seconds at most. A
  primary whose node comes back is stopped before anything of it serves,
  and becomes a standby anew, from a snapshot.
- **Switchover.** A drain and a minor release switch the primary to the
  standby first, so that the addon is away for the switch alone,
  seconds. A major upgrade stops both, upgrades the primary and makes
  the standby anew. A standby counts toward the project's use as the
  instance it is.

### Import and export

- **Import.** An addon may be made `from` a database outside FeRun, a
  URL and a secret of the environment with its credentials. A job of the
  platform's (ADR 0041), with egress to the source's host and the
  addon's owner role, dumps it with the addon's release of `pg_dump` and
  restores it, the objects owned by the declaration's roles. An import
  that fails fails the deployment and leaves the addon empty for another
  try. A move by logical replication, with little downtime, is later.
- **Export.** A principal with `write` on the environment exports a
  database: a sandbox that reaches the addon's read-only role writes a
  dump in PostgreSQL's custom format onto its disk, and the person
  downloads it through the sandbox's files and deletes the sandbox, or
  it goes after a day.

### An addon idles with its environment

- An addon whose dependents are all hibernated or stopped, as an
  environment of previews or one that nobody uses is, hibernates with
  its memory after the last of them, and is restored before the first
  of them starts again. A hibernated addon costs its checkpoint in the
  store and takes no snapshots, since nothing changes. An addon that a
  running sandbox reaches stays up, and one whose dependents never idle
  never idles.

### Steps

1. **The backups:** the live snapshot and the archive through the node,
   FeRun's image with the helper, retention and the sweep, restores into
   new addons and clones with data, the restore after a lost node, the
   backups a removal keeps, the archive's and the snapshots' figures and
   incidents, and the restore tests.
2. **The upkeep:** moves with a snapshot ahead, releases by the
   blueprint and the maintenance window, the volume's growth,
   extensions, parameters, the other figures and their incidents, and
   people through sandboxes.
3. **The rest:** the standby with failover and switchover, major
   upgrades and PostgreSQL 18, import and export, the statements in the
   console, snapshots on demand, and idling with the environment.

## Alternatives considered

- **pgBackRest, WAL-G or Barman in the addon**, writing to the object
  store. They archive and back up well, but the store's key would be in
  the guest, where the superuser's `COPY … TO PROGRAM` and any extension
  read it; every base backup, full or delta, reads the database through
  the addon's own vCPUs, one on most plans; their format is a second
  copy of the data beside the disk daemon's chunks, with a retention and
  a sweep of its own; and a restore downloads the backup before
  PostgreSQL starts, where the disk daemon serves a volume as it is
  read. What remains of them here is PostgreSQL's own archiving, through
  the node.
- **The same tools through a binding that signs for the store on the
  node.** It keeps the key out of the guest, and nothing else of the
  above. An HTTP binding checks a path and not a query, a copy's source
  or a list's prefix, so it cannot keep an addon to its own keys; and
  the streaming signatures of S3 clients would have to be signed anew
  chunk by chunk.
- **Snapshots alone, every few minutes, without the WAL.** A lost node
  costs the interval; each snapshot uploads the pages a write touched,
  more bytes than the WAL that changed them; and no moment between two
  snapshots can be restored.
- **Freezing the filesystem for the seal**, as a bake does (ADR 0011).
  Every commit would wait for the seal, whose length grows with what was
  written since the last one; the cut takes an instant, and the journal
  keeps what it cut whole.
- **Logical dumps as backups.** Slow to take and slower to restore, the
  restore's time growing with the data, and no moment between two dumps.
  They stay for import and export.
- **Streaming the WAL to a receiver of the control plane**
  (`pg_receivewal`) for a loss of nothing. A process per addon beside
  `ferund`, and synchronous replication's wait in every commit; a minute
  is what an addon without a standby promises, and a standby promises
  seconds.
- **A standby before backups.** A standby replicates a `DROP TABLE` as
  fast as anything else: it saves an addon from a lost node, not from a
  mistake, and costs every addon a second instance. Backups save from
  both, and a standby is built on them.
- **Restoring over the running addon.** It destroys the state a person
  may still need, and gives a deployment's revert a second meaning: a
  revert would put data back as well as declarations. A restore is a new
  addon, as at RDS and CNPG.
- **A node that stops its addons when it loses the control plane**, to
  fence them. An outage of `ferund` would stop every addon of the
  installation. The bindings, which follow the name and end what they
  carried to the old address, fence the old instance for every workload
  outside its node, and a node that comes back stops it first.
- **CNPG itself.** FeRun's addons are pods of FeOS on volumes of the disk
  daemon, not Kubernetes pods on claims, and an operator there would see
  none of them; CNPG's model, a lease for the primary, failover and
  switchover, archive and base backups, bootstrap from a backup or an
  import, is taken over here in FeRun's terms.
- **Synchronous replication, read replicas, a pooler and backups in a
  second region.** None is asked for yet; each fits on what this ADR
  builds and is in the backlog.

## Consequences

- The disk daemon gains a cut of a running disk and an archive of named
  files per addon, both on the node's path to the store; `feosd` gains
  the archive's end of the guest's socket, and the guest the socket and
  the growth of a volume's filesystem by the kernel's resize. ADR 0016's
  rule holds: `mke2fs` stays the one tool the guest runs. The guest's
  TLS bindings end the connections to an address their name has left.
- `ferund` sweeps the object store, which it has not done before:
  nothing was deleted from it until now. Its sweep names the backups
  beside the checkpoints and the bakes.
- The store holds, per postgres addon, the chunks of its snapshots,
  shared between them, and its archive, as plain as the checkpoints are:
  compressed, named by their content, protected by the store's own
  encryption. A key per project would cover every object of the disk
  daemon's, checkpoints too, and would end the sharing of chunks
  between projects; it is in the backlog.
- A postgres addon's volume is on its node and in the store. A lost
  node or a replaced machine costs at most a minute of commits, a lost
  primary with a standby seconds, a drain none; a mistake is undone by a
  restore to the moment before it; and a restore's time is measured
  every week.
- The postgres image is FeRun's: built per major with the helper and the
  extensions, published with FeRun's releases. The upstream image is no
  longer what runs, and a release is chosen, not met by a recreate.
- ADR 0040 changes for postgres: a lost addon is restored elsewhere from
  its backups, a removal keeps them, a clone may bring data, the volume
  grows, releases move in windows, a major version may go up, and an
  addon idles with its environment. ADR 0018's environment gains a
  maintenance window, and its `clone` the data. ADR 0012's addon needs
  no egress for backups, and the two instances of an addon with a
  standby reach each other. ADR 0038's key on the volume is written anew
  by every restored addon, and a sandbox may be granted a role.
- Placement counts an addon's instances as peers, so that a standby is
  never on its primary's node, and avoids a node a lost addon left.
- Incidents gain conditions of their own for backups, archives, fills,
  connections, wraparound, standbys and restore tests, in the
  environment's list, with the evidence that names what was found.
- The plan's warnings name what a change costs: a restart, a stop for
  an upgrade, and the day until which a removed addon can be restored.

## Open questions

- **The cut in the disk daemon.** Whether a device's thread can switch
  its top between two requests and have reads go through the cut top
  while it is sealed, under libublk's queue as it is, or whether the
  cut pauses the queue for the moment of the switch; and how the seal of
  the cut top merges into the volume's layer while the device reads from
  both. Step 1 waits for it.

## History

Proposed on 2026-11-28, from a comparison of the postgres addon with
Amazon RDS and CloudNativePG; nothing of it is built. A comparison with
CNPG alone on 2026-11-07 had put backups first and a standby last, for
the reason the Context gives: the volume is on one node. It amends
ADR 0040, which stays as it is for valkey and nats.
