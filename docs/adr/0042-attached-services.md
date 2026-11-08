# ADR 0042: Attached services: a service as a supervised process in a sandbox

**Status:** Accepted · **Date:** 2026-10-18

A service whose runtime is `attached` runs as a supervised process in
the main container of a sandbox of its environment, from the sandbox's
working tree, with a command, a working directory, readiness checks,
restarts and logs, and the environment's addons and services reachable
by name. It is how FeCode's agent runs the application it writes, from
source, while its databases are real; stage and production run images.
`ferund` starts, watches and stops the process through the sandbox's
process tools and resolves its environment, secrets included, into the
start and never onto the sandbox's disk. While it starts, the service
keeps its sandbox awake for its readiness budget; once ready it takes
`idle` as an image service does, so that a preview sleeps and wakes with
its traffic. That the sandbox is of the service's own environment is
ADR 0035.

## Context

An agent in a FeCode sandbox writes an application and needs to run it
as it will run: with the environment's Postgres and NATS, under the
routes a deployment declares, with the readiness and restarts a service
has, but from the working tree it is editing, not from an image it would
have to build on every change. A sandbox is a pod with a main container,
process tools that start a process detached from any request and read
its output, and an idle policy of its own: it pauses when nothing holds
it. A preview's process may build for ten minutes before it listens; a
workspace's idle rule, ten seconds after the agent's last call, must not
decide when the application sleeps, and a ready preview nobody uses
should sleep as a scaled-to-zero service does.

## Decision

### A process, not a pod

- A service's runtime is `image`, pods of its own (ADR 0018), or
  `attached`: a sandbox of its environment (ADR 0035), a command, an
  absolute working directory in the sandbox's tree, the ports it serves,
  a readiness check and `idle`. It has no image, replicas, resources,
  rollout or bindings of its own: it is its sandbox's pod on the
  network, its references are the sandbox's pod's (ADR 0012), and it
  uses the sandbox's bindings.
- `ferund` starts the process detached with its argv, working directory
  and resolved environment, watches it while the sandbox runs, and never
  wakes a paused sandbox for a look. A changed template stops the old
  process and starts the new; a process that is gone counts as an exit
  and is restarted with a backoff; readiness is checked against the
  sandbox's port or by exec. A restart restarts the process, never the
  sandbox's pod. A working directory that does not exist yet keeps the
  service starting.
- The process is named by the environment's ID and the service's name,
  so that a rename of the environment leaves it (ADR 0018); the
  supervisor finds a running process by the ID its row records, whatever
  its name.

### Which sandbox

- An attached service uses its sandbox while that sandbox exists, is the
  environment owner's and is no environment's pod. A sandbox deleted and
  created again under the same ID is a new incarnation, in which the
  service starts anew, its restarts and checks counted from zero; a
  sandbox that another tenant holds is never the service's, and the
  service fails rather than start, route or reach there.
- A service that goes, removed, moved to another sandbox or with its
  environment deleted, has its process stopped first, a sleeping sandbox
  woken for it; its record, and its name in the environment's table
  with it, goes or moves only then, or once the process is gone with its
  sandbox for certain, so that no process runs on with the environment's
  secrets.
- A clone does not copy attached services: they run in a sandbox of the
  source (ADR 0018).

### Awake while starting, idle once ready

- **A service that starts is work the platform asked for.** From the
  start of its process, by a deployment, a restart or a replacement,
  until it is ready, an attached service keeps its sandbox awake, for at
  most its readiness budget. A service that is not ready in time fails
  its deployment, and the sandbox may sleep.
- **A service that is ready and unused may sleep.** An attached service
  takes `idle` as an image service does: the sandbox it runs in stays
  awake while the sandbox was active within the longest `idle` of its
  ready attached services, and `idle: never` keeps it awake while the
  service runs. Activity is what it is for any sandbox: an exec, a
  forwarded request, a process. A request to a sleeping preview wakes
  it. Without `idle`, a ready service holds nothing, and the sandbox's
  own idle interval applies.
- A sandbox's idle interval is therefore the longest of its own claim
  and its attached services', read in one statement, so that a pause
  never races the controller. A paused process is frozen, not stopped:
  it goes on where it was when the sandbox resumes. Hibernation of a
  paused sandbox is unchanged.

### Reached by name and through the door

- An attached service's name is in its environment's table with its
  sandbox's address; inside its own sandbox it is reached over loopback
  (ADR 0035). Its routes are served by the route door alone (ADR 0043),
  never by the ingress (ADR 0021): a sandbox stays behind the doors.

## Alternatives considered

- **A container added to the sandbox's pod** for an attached service: a
  node mechanism (ADR 0004, ADR 0007) for what a process in the main
  container, which already sees the working tree, does.
- **The sandbox held only while `ferund` wakes it to start the
  service**, for at most a minute, and nothing once started: a preview
  that builds before it listens was paused ten seconds after the
  agent's last call and went on only while the agent called again, until
  its budget ran out. Replaced on 2026-10-30 by the budget and `idle`.
- **The process named by the environment's name**: a rename would
  orphan it. Replaced by the ID on 2026-11-08.
- **Attachable sandboxes**, created so, with a security group and a name
  table of their own that included the tables of the environments whose
  services they ran: replaced by ADR 0035, which makes every sandbox a
  sandbox of one environment with that environment's names and rules.
- **Every attached service holding its sandbox awake for good**: the
  workspace would never sleep; scale to zero is a feature, and the
  caller chooses `idle: never` where it wants that.

## Consequences

- Nothing is needed of the node, of ADR 0004 or of ADR 0007: the
  process tools do the work.
- A preview whose process builds for minutes stays awake until it is
  ready or its budget runs out, whatever the agent does; FeCode gives a
  preview's attached services the `idle` its class gives image services.
- An attached service has no crash loop of its own: it is restarted with
  a backoff for as long as it fails. Its routes use the port the
  deployed deployment gives the route's name.
- A process's resolved environment is in the start request and the
  guest's process records, never on the sandbox's disk; a service
  removed has its process stopped before its secrets are forgotten.
- A replaced sandbox has a new ID (ADR 0019), which a new deployment
  names; the service starts there anew.

## History

Decided with ADR 0018 on 2026-10-18 and built the same day. Holding the
sandbox awake while a service starts, and `idle` for a ready one, came on
2026-10-30 (e2b6fcf) after FeCode's previews froze while they built.
`attachable` and the sandbox's own group and table went with ADR 0035 on
2026-11-07 (8e4854a); the process was named by the environment's ID on
2026-11-08 (8951c44, ADR 0037). Split out of ADR 0018 on 2026-11-08.
