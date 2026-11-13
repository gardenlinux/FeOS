# ADR 0048: The guest supervises a pod's containers

**Status:** Proposed · **Date:** 2026-10-14

A pod's containers are supervised inside the pod, by `feos-guest`, not by
`feosd` or the control plane. Every container has a role, init, sidecar or
main, and the pod a restart policy, `Always`, `OnFailure` or `Never`, with
Kubernetes's backoff; containers have startup, readiness and liveness
probes that the guest runs inside the pod's network, and a container's
start, exit and change of readiness are events the guest reports on its
uplink and `feosd` forwards to the control plane. An idle container's
PID 1 is FeOS's reaper, a process runs as its image's user, a stop is
`SIGTERM`, the pod's grace period and `SIGKILL` in role order, and a
container's log is bounded, with a tail kept in the pod's record once the
pod ends. A pod ends when its main containers are done, and its microVM
goes with it.

## Context

A pod has no kind (ADR 0004), yet sandboxes, services and jobs differ in
what a process's exit means: nothing for a sandbox, a restart for a
service, the end for a job. What differs must be in the spec, as fields
every pod has: a restart policy, and a role per container, so that a
sidecar restarts beside a job that does not.

Restarts and probes belong where the containers are. Decided in `feosd` or
the control plane, every exit and every probe crosses vsock and the node's
connection, nothing restarts while `feosd` restarts or the control plane
cannot be reached, and a stop cannot be graceful from outside.

Every container has a PID namespace of its own, and its process is PID 1
in it. A process orphaned in the container is reparented to that PID 1:
under a process that does not reap, a background process that exits stays
a zombie. Docker has the same problem, and `docker run --init` as its
remedy. A sandbox's container runs nothing of its own, so its PID 1 must
be something that reaps and never exits on a signal from inside.

A job's output matters most when it has ended, and a service's log must
not fill the pod's disk.

While the guest does not supervise, the control plane does (ADR 0018): it
restarts an environment's exited container by stopping and starting its
pod, runs the readiness checks over the node's connection, counts restarts
and exits itself, and runs a sandbox's container as `sleep infinity`.
`feosd` sends a stopped pod's containers `SIGTERM` and gives them a fixed
grace before the microVM ends them (ADR 0004).

## Decision

### A container's role

Every container has one of three roles, and the guest starts them in this
order:

1. **Init** containers run one at a time, in their order, each to its end.
   The next starts only when the one before exited with 0.
2. **Sidecar** containers start next, in their order. Each has started,
   and passed its startup probe if it has one, before the next starts.
3. **Main** containers, the default, start last, together.

Main containers are the pod's work. Sidecars serve them: they keep no pod
alive, and they are stopped after the main containers, in reverse order.

### What a container runs

- `command` replaces the image's entrypoint and `args` its command, as in
  Kubernetes; each is the image's when empty.
- The process runs as the image's user, as Docker and Kubernetes do; as
  root only when the image says so or the spec asks for it. The image's
  stop signal is honoured.
- An **idle** container runs no process of its own. Its PID 1 is FeOS's
  reaper, a small static init mounted read-only like the toolbox, which
  reaps whatever is reparented to it and waits until the guest stops the
  container; no signal from inside the container ends it. Its work comes
  through exec and the tools.
- A container with a process gets the reaper in front of it on request,
  like `docker run --init`. The reaper forwards signals to the process,
  reaps, and exits with the process's exit code.

### When a process exits

- The pod's restart policy, `Always`, `OnFailure` or `Never`, says what
  happens when a main container's process exits, as in Kubernetes. It is
  `Always` when unset.
- An init container that fails is run again, unless the policy is `Never`:
  then the pod fails, and nothing after it starts.
- A sidecar is started again whenever it exits, until the pod ends.
- The guest starts a container again with Kubernetes's backoff: 10 s,
  doubling up to 5 min, reset after 10 min of running. The guest sees an
  exit first, and restarts go on while `feosd` restarts or the control
  plane cannot be reached. A container that cannot be started (its
  executable not found, its image unusable) counts as an exit with the
  same backoff, its reason in its last exit, as the control plane counts
  it until then (ADR 0018).
- A container started again keeps its writable layer, as with Docker's
  restart policies: only its process is new. What an agent wrote in its
  sandbox survives a restart.
- A container's status carries its restart count, its last exit and its
  readiness.

### Events, not polls

The guest reports a container's start, exit and change of readiness on
its uplink, and `feosd` forwards them to the control plane as the pod's
events (ADR 0015). Nothing above the node polls a pod for its containers.

### Health

- Containers have startup, liveness and readiness probes, as in
  Kubernetes: exec, HTTP GET and TCP. The guest runs them, inside the
  pod's network.
- A failed liveness probe ends the container's process, and the pod's
  restart policy decides what follows. Readiness goes into the container's
  status and the pod's events, and from there into the routes
  (ADR 0018, ADR 0021).

### When a pod ends

- A pod ends when it has main containers and none of them runs or will
  run again. Its sidecars are stopped, and `feosd` shuts its microVM down,
  which gives its memory back. The pod has succeeded when every main
  container exited with 0, and failed otherwise. Its record keeps each
  container's last exit until the pod is deleted.
- A pod whose main containers are idle, or are always started again, or
  that has none, never ends on its own: it runs until it is stopped,
  suspended or deleted.
- Stopping a pod sends its main containers `SIGTERM`, and `SIGKILL` after
  the pod's grace period, 30 s when unset, as in Kubernetes; then the
  sidecars the same way, in reverse order. Then the microVM shuts down.
- A start brings an ended pod up again: a new boot, from its init
  containers on.

The products differ in fields, not in kinds:

| | Main container | Restart policy |
|---|---|---|
| Sandbox | idle | `Always` |
| Service | the image's process | `Always` |
| Job | the image's process | `OnFailure` or `Never` |

### Logs

A container's log is bounded: a ring of a fixed size on the pod's disk,
rotated, each line stamped with its time and stream (ADR 0026). When a pod
ends, a tail of each container's log goes into the pod's record and stays
until the pod is deleted: a job's output is there when the job is not.

## Alternatives considered

- **`sleep infinity` for a sandbox's container**, which the control plane
  runs while the idle container does not exist. It needs `sleep` in the
  image or the toolbox, and it reaps nothing.
- **FeOS's reaper in every container**, since exec is offered on every
  container. Images expect what Docker and Kubernetes do: those built on
  s6-overlay (linuxserver.io's, Home Assistant's) refuse to start unless
  they are PID 1, as they do under `docker run --init`.
- **Restarts and probes in `feosd` or the control plane**, which is where
  they run while the guest does not supervise (ADR 0018). Every exit and
  every probe crosses vsock and the node's connection, nothing restarts
  while `feosd` restarts, and a stop cannot be graceful from outside.
- **Sidecars as init containers that restart always**, as Kubernetes
  writes them. One field would mean two things, for a compatibility we do
  not need (ADR 0001). A role says what a container is for.
- **A restart policy per container.** The roles cover what one is wanted
  for, a sidecar that restarts beside a job that does not; Kubernetes, too,
  has one per pod.
- **Keeping an ended pod's microVM**, to exec into what is left and read
  its logs. It would hold its memory until the pod is deleted.
- **An ended pod's logs gone with its microVM.** A job's output matters
  most when it has ended; a bounded tail in the pod's record costs little.
- **Every container as root**, whatever its image says, as the guest runs
  them while it does not supervise. Docker and Kubernetes run the image's
  user, and images expect it.
- **Background processes as a capability of the guest**: start, list, logs
  and stop. The control plane runs and supervises them through exec
  (ADR 0018, ADR 0023), and the reaper keeps the container clean of what
  they leave.

## Consequences

- `feos-guest` supervises its containers: it starts them in order,
  restarts them, runs their probes, stops the sidecars when the main
  containers are done, and tells `feosd` over its uplink when the pod has
  ended. A suspended pod runs no probes; after a resume they run again at
  once.
- The control plane sheds its restarts by reboot, its polls and its
  readiness checks over the node's connection, and routes by the readiness
  the guest reports.
- A service that scales to zero is a suspended pod. It is not ready while
  it resumes, so the control plane holds its requests until it is.
- How often a job may fail and how long it may run is the control plane's:
  it sees every restart, and stops the pod.
- Exec into a container whose process does not reap leaves zombies there,
  as with Docker and Kubernetes; the reaper on request is the remedy.
- The control plane's API can take Kubernetes's `command`, `args`,
  `restartPolicy`, init containers, sidecars and probes as they are.
- The pod and container specs and the container's status grow additively
  (ADR 0007).

## History

Decided on 2026-10-14 as part of ADR 0004 (f539c16), when sandboxes,
services and jobs were put on one kind of pod. ADR 0022 (2026-10-27,
33ea94e) gave it its order of building and decided the three questions
ADR 0004 had left open: the stop's signals and grace, the tail of an ended
pod's log in its record, and the image's user. Split out of ADR 0004 on
2026-11-08 as the part not built; ADR 0004 keeps what is: a container's
exit in the pod's record, and `feosd`'s `SIGTERM` before the microVM shuts
down (6681651, 2026-10-31).
