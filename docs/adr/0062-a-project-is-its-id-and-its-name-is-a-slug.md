# ADR 0062: A project is its ID, and its name is a slug

**Status:** Proposed · **Date:** 2026-11-22

A project is its **ID**: a UUID that FeRun gives it when it is made and
that no other project ever has. Its **name** is a slug. It is unique
among the projects that exist and free once its project is gone, and it
is what people type and read. Inside FeRun every record refers to a
project by its ID. So does everything that keeps something of a project
or is matched against it later: a pod's labels, the sink's streams,
spans and quotas, and FeCode's binding. A call names a project by its
name or by its ID, as it names an environment. A caller that keeps a
project names it by its ID, and a call for an ID that is gone finds no
project, whatever exists under its name.

## Context

- People make projects themselves (ADR 0031), and a project's admin
  deletes it. A name freed by a deletion is free to anyone, and a
  project made under it again is another (ADR 0056).
- FeRun's tables do not say so. A project's name is its key. Every
  table of a project refers to it by name: its tokens, grants,
  environments, sandboxes, jobs, domains, invitations, access providers
  and visitors, its account and the integrations' grants. The activity
  and the traffic records (ADR 0013) keep it by name too.
- So what outlives a project, or is matched against one after it is
  gone, meets its successor. The sink keeps streams, spans and the
  day's usage by the name (ADR 0026). FeCode keeps each of its projects
  bound to a FeRun project (FeCode's ADR 0037).
- The answer so far is the project's making, `createdAt`: FeCode binds
  to it, and the sink reads a project from it on. A time stands in for
  an identity and fails wherever the old project acts after the new
  one's making:
  - A pod on a node out of the leader's reach when its environment
    went runs on until its tombstone deletes it there. Its lines and
    spans pass the bound.
  - The day's usage has no time to compare. A project made under the
    name on the same day starts with the deleted one's.
  - Every reader of a project must compare times, and one that forgets
    reads its predecessor.
- An environment has long been what this asks of a project: an ID that
  its routes take as well as its name, and a name that may not look
  like an ID (ADR 0018, ADR 0035).

## Decision

### An ID that never comes back

- Every project has an ID, a random UUID that ferund mints when the
  project is made. It never changes, and no project made later has it.
- Its name is a slug. It is unique among the projects that exist,
  including one being deleted, and free once its project is gone. A
  name may not have the shape of a UUID, so no string names both a
  project's ID and another project's name.
- The ID is the project. Two projects of one name, at different times,
  are two projects, and nothing of one is the other's.

### Inside FeRun the ID is the key

- Every record of a project refers to it by its ID. The project's own
  row is keyed by it, and the name is a column of that row alone.
- Records kept after the project is gone, the activity and the traffic
  records, keep its ID too, and its name beside it for reading.
- A project's pods carry its ID in their labels, beside its name. So
  does everything the sink is sent of them: a stream's labels and a
  span's resource.

### Outward, the name or the ID

- A call names a project as it names an environment, by its name or by
  its ID. This holds in the routes that take a project and in the
  header that names a call's project.
- Every answer that names a project gives both.
- A caller that keeps a project, and acts on it later, names it by its
  ID. A call that names an ID that is gone is answered as one for an
  unknown project, even if a project of that name exists. FeCode calls
  this way (FeCode's ADR 0041).
- People keep the name. The console, the hosts, a domain's claim, the
  spec and a workload's identity name the project by its name. Each
  exists only while its project does, or carries something unique of
  its own: a workload's identity names its environment by ID
  (ADR 0014).

### The sink reads a project by its ID

- The sink keeps streams, spans and the day's usage by the project's
  ID. A reader reads the project whose ID ferund answers for it, and
  nothing kept under another ID, whatever its name or its time.
- A pod of a deleted project that sends on sends under an ID no project
  has. Nobody reads what it sends, it counts against no quota, and it
  goes with its retention.
- Streams and spans that carry no ID come from pods placed before this
  release. They are read by name and from the project's making on, as
  before. That bound goes once their retention has passed.

### A binding is to the ID

- A caller that keeps something of a project binds it to the project's
  ID, not to its name or its making.
- `createdAt` stays a fact of a project, shown and answered. It binds
  nothing.

## Alternatives considered

- **`createdAt` as the identity**, as since ADR 0056's acceptance. A
  time is no identity:
  - what acts after a successor's making passes the bound;
  - the day's usage has no time to compare;
  - every reader must compare, and one that forgets reads the
    predecessor.
- **An ID beside the name, with the name still the key inside ferund.**
  A smaller migration, but then two keys stand for one thing. Every
  record that outlives the project needs the ID anyway, and a project
  could never be renamed.
- **A deleted project's name kept until its tombstones have run.**
  It closes the pod that runs on, but not the day's usage or the other
  records, and it blocks the name for as long as a node is out of
  reach.
- **A deleted project's streams and spans purged on deletion.** Purging
  only frees storage (backlog). It misses what a pod sends after the
  deletion, and Quickwit deletes asynchronously.
- **The sink's writers authorized per project**, by mTLS on topics or
  on streams. The sink already takes only nodes and ingresses, by their
  certificates. A pod that runs on sends through a node that is still a
  node. What goes wrong is which project the lines are of, not who
  writes them.
- **The ID in hosts and identities too.** Hosts are what people type
  and see, and a workload's identity already carries its environment's
  ID. Both exist only while the project does.

## Consequences

- ferund's migration gives every project an ID and points every table
  at it. It is not additive: as with ADR 0061's, the release stops
  ferund for its migration.
- The activity and traffic records of projects already gone keep their
  name and get no ID.
- The module (ADR 0057) answers a project's ID wherever it names a
  project, and its client names a project by ID where the caller gives
  one.
- The sink's tables and Quickwit's mappings carry the project's ID. The
  time bound stays only for what has no ID, until its retention has
  passed.
- FeCode binds by the ID (FeCode's ADR 0041). FeChat names its project
  by name with its token, and nothing changes for it.
- A project can be renamed without its records following the name.
  Whether it may be, and what follows the name, is backlog: its hosts,
  its domains' claims, its identities and a FeCode project's name.
- The backlog's lines on the sink's time bound and on the day's usage
  by name are taken up here.

## History

Proposed on 2026-11-22. A project's admin had just been given the right
to delete it, and the sink had been bound to a project's making (FeOS
master 15e05073). A review then left two gaps: a pod on a node out of
reach sending into a name made again, and the day's usage kept by name.
Asked whether mTLS on the sink's topics would close them, Malte
proposed instead to work with UUIDs as project IDs inside, each with a
unique slug, the pattern environments already follow.
