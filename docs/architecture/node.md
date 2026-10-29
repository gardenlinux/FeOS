The Node
========

What runs on a node and how. The decisions behind it:
[ADR 0003](../adr/0003-programs.md) (programs),
[ADR 0005](../adr/0005-firecracker.md) (Firecracker),
[ADR 0008](../adr/0008-suspend-and-restore.md) (suspend, hibernation and
restore), [ADR 0009](../adr/0009-node-os-and-packages.md) (operating system
and packages), [ADR 0010](../adr/0010-pod-network.md) (the pods' network),
[ADR 0011](../adr/0011-disk-daemon.md) (the disk daemon),
[ADR 0012](../adr/0012-security-groups.md) (security groups),
[ADR 0013](../adr/0013-traffic-records.md) (traffic records),
[ADR 0016](../adr/0016-volumes.md) (volumes).

Parts that are decided but not built are marked as such.

## Processes

- **`feos-host-init`**: PID 1 when FeOS is the node's operating system. It
  sets the machine up, starts `diskd` and `feosd` and starts each again
  when it ends.
- **`feosd`**: the node agent: the API, the pod and microVM services, the container and
  image services. It runs under systemd, `feos-host-init` or in a
  Kubernetes pod ([On Kubernetes](#on-kubernetes)), never as PID 1.
- **`feosd --task-service`**: the container runtime (libcontainer) in a
  process of its own, started and restarted by `feosd`. Its pid is in
  `/run/feos/task-service.pid`.
- **`diskd`**: the pods' disks, and the node's object store
  ([disk.md](disk.md)).
- **The jailer and Firecracker**, one process per running pod.

## Machine setup: feos-host-init

`feos-host-init` moves its root off the initramfs, mounts `/proc`, `/sys`,
`/dev`, `/dev/pts`, `/run`, a tmpfs at `/var/lib/feos` and the cgroup
filesystem, configures SR-IOV VFs, and configures the uplink `eth0`:

- **IPv6** from router advertisements, which the uplink takes although
  the node forwards (`accept_ra` 2): the router's default route, and an
  address by SLAAC where the router offers one; and from DHCPv6, which
  gets 30 seconds: an address, a delegated prefix, resolvers and NTP
  servers.
- **IPv4** from DHCPv4: the lease's address and prefix, a default route by
  its router (on the link if outside the prefix), resolvers and NTP
  servers. The client renews the lease at T1 and rebinds at T2 for as
  long as the node runs, and asks again, without end, where no server
  answers or a lease ends.

The resolvers DHCPv4 and DHCPv6 name, of both families in turn, replace
`/etc/resolv.conf`; until one names any, the initramfs's stay (Google's
and Cloudflare's). It keeps the clock with NTP from the servers DHCPv6
names, then those DHCPv4 names, then `pool.ntp.org` by IPv6, else by
IPv4, each given five seconds to answer.
It takes Ctrl-Alt-Del over (a SIGINT makes it
sync and reboot), reboots on `SIGRTMIN+5` and powers off on `SIGRTMIN+4`,
reaps orphans, runs `/usr/sbin/diskd --state-dir /var/lib/feos/disk`
without an object store, and `/usr/sbin/feosd --self-upgrade --tcp [::]:1337`,
with `--pod-prefix` and the pods' prefix if it has one: the one the
kernel's command line names (`feos.pod-prefix=PREFIX/LEN`, for a prefix
the network routes to the node statically), else the one DHCPv6
delegated, which it also routes as unreachable. It starts each again a
second after it ends. It binds the pods' resolver's UDP and TCP sockets
to `169.254.0.53:53` before the node holds the address (`IP_FREEBIND`),
keeps them, and hands them to every `feosd` it starts, as systemd would
([DNS](#dns)). It serves no API and logs to `/dev/kmsg`, whose limit of
ten records in five seconds it lifts (`kernel.printk_devkmsg=on`).

`make test-host` boots the host kernel in QEMU: the tests of `feosd`'s
network against the kernel, then the node on QEMU's user network, with
a pod prefix from the kernel's command line, and with `PODS=1` a pod on
it that reaches the internet.

On a distribution all of this is the operating system's
([ADR 0009](../adr/0009-node-os-and-packages.md)).

## feosd

`feosd [--tcp ADDRESS] [--self-upgrade] [--pod-prefix PREFIX] ...` sets
nothing of the machine up but the pods' network ([below](#the-pods-network)).

- It serves the API on `/run/feos/api.sock` and, with `--tcp`, on TCP,
  without authentication ([api.md](api.md)). If a group `feos` exists, it
  owns the socket, mode 0660: its members can do on the node what root
  can.
- Once its listeners are bound, it sends `READY=1` to `$NOTIFY_SOCKET` if
  systemd set one.
- `HostService.Reboot` and `Shutdown` send PID 1 `SIGRTMIN+5` and
  `SIGRTMIN+4`; in a PID namespace of its own, as in a container, they
  fail with `FAILED_PRECONDITION`. `UpgradeFeosBinary` replaces the binary and runs it with
  the same pid, and the resolver's sockets, only with `--self-upgrade`;
  without, the call fails with `FAILED_PRECONDITION`.
- It is the subreaper of its children. Pods and the task service outlive
  it: a `feosd` that starts again takes the running pods over and keeps the
  running task service. In a container they end with it
  ([On Kubernetes](#on-kubernetes)).
- One `feosd` per node: before it touches anything, it takes an exclusive
  `flock(2)` on `/var/lib/feos/feosd.lock`, writes its pid into it and
  holds it until it ends. While another process holds it, `feosd` says so
  and tries again every second, as long as it takes, rather than exit: a
  `feosd` that stops, such as a rollout's old pod that suspends its pods,
  ends in its own time, and an exit would only meet systemd's start limit
  or Kubernetes's back-off. The file is open close-on-exec: a
  self-upgrade's exec takes the lock again, and nothing `feosd` runs holds
  it. `diskd` holds `/var/lib/feos/disk/diskd.lock` alike.
- With `--suspend-on-stop`, SIGTERM suspends the pods for the next `feosd`
  ([On Kubernetes](#on-kubernetes)); without, with a control plane, it
  tells the control plane that `feosd` stops, closes the node connections
  once their calls are done ([Stopping](#stopping)), and ends.
  `--stop-grace SECONDS` (300 by default, feos-node's
  `terminationGracePeriodSeconds`) is how long a stop may take before
  `feosd` is killed; it bounds what the stop waits for.
- Nothing that blocks runs on its runtime's threads, nor under a lock
  that a reader takes. The records it keeps are written with fsync(2),
  which can take seconds while the disk writes back much else, as a
  snapshot's memory. A pod's record is written on a thread of its own,
  `pod-records`, in the order of the pod's changes: a change is seen at
  once, and its call answered and its event sent once it is kept. The
  volumes, the security groups, the name tables and the mesh are written
  on the blocking pool, one change at a time, and a change is seen once it
  is kept: `ListPods`, `GetPod`, a call to a guest and the resolver's
  lookups never wait for a write. The pods' addresses are handed out and
  kept on the blocking pool, one pod at a time, before the pod is
  recorded; so is the record of a stop. Removing a snapshot or a
  container's directory, clearing a layer a pull unpacked before, and the
  image store's syncfs(2) run there too.

## Directories

| Path | Holds |
|---|---|
| `/var/lib/feos/pods/<id>.pb` | a pod: its spec, its state and why, its addresses (a `feos.pod.v1.PodInfo`) |
| `/var/lib/feos/pods/next-ipv6-address` | the counter of the pods' IPv6 addresses: the next one handed out |
| `/var/lib/feos/pods/ipv4-addresses` | the pods' IPv4 addresses: the lowest never handed out, and those freed since, the longest free first, with when |
| `/var/lib/feos/security-groups/<id>.pb` | a security group (a `feos.network.v1.SecurityGroup`) |
| `/var/lib/feos/name-tables/<id>.pb` | a name table (a `feos.network.v1.NameTable`) |
| `/var/lib/feos/volumes/<id>.pb` | a volume (a `feos.volume.v1.Volume`); `empty-layer.img`, the image of the volumes' empty L0 |
| `/var/lib/feos/feosd.lock` | the lock of the one `feosd` on the node, with its pid |
| `/var/lib/feos/suspended-for-stop.json` | the pods a stop of `feosd` suspended, for the next `feosd` to resume ([On Kubernetes](#on-kubernetes)) |
| `/var/lib/feos/vms.db` | the microVM records (SQLite), by the pod's ID: a UUID as its 16 bytes, a short ID as text |
| `/var/lib/feos/vms/firecracker/<id>/` | a microVM's jail: `jail.json` and the chroot `root/` |
| `/var/lib/feos/disk/` | diskd's state: the pods' disks, the chunk cache ([disk.md](disk.md)), the lock `diskd.lock` |
| `/var/lib/feos/images/` | pulled images, a `metadata.json` each |
| `/var/lib/feos/containers/` | the host's containers, a `container.json` each |
| `/var/lib/feos/upgrade/` | a downloaded binary before an upgrade |
| `/var/lib/feos/node/` | the node's identity for the control plane ([below](#the-control-plane)) |
| `/var/lib/feos/node/mesh/` | the mesh's private key `key` and `mesh.pb`, what the control plane gave and sent the node (a `feos.network.v1.Mesh`) ([the mesh](#the-mesh)) |
| `/run/feos/` | the API sockets, the task service's pid, the runtime state of containers |
| `/usr/lib/feos/firecracker/<version>/` | Firecracker and its jailer, a version each, side by side |
| `/usr/lib/feos/guest/<version>/` | the guest's `vmlinuz`, `initramfs` and blank data volume `data.img.zst`, a version each, side by side |

`/var/lib/feos` is a tmpfs when FeOS is the node's operating system and a
directory on disk on a distribution.

## The control plane

With `--control-plane`, `feosd` joins the control plane and connects to
every instance of it ([ADR 0015](../adr/0015-control-plane.md),
`crates/fleet-client`); without, the node is on its own. Nothing of it
stops `feosd`: what fails is logged and tried again, and the socket
serves on.

| Option | Default | What it is |
|---|---|---|
| `--control-plane HOST:PORT` | none | the control plane's port for nodes: the join dials it, and so do the node connections and the uplink while the control plane lists no instances |
| `--control-plane-name NAME` | HOST | the DNS name the control plane's certificate is checked for, wherever `feosd` dials; needed when HOST is an address |
| `--trust-domain NAME` | `feos.local` | the control plane's SPIFFE trust domain |
| `--join-token-file PATH` | `/etc/feos/join-token` | the operator's join token, `feos-join-<secret>.<pin>`, read while the node has not joined |
| `--node-dir PATH` | `/var/lib/feos/node` | the node's identity, and the mesh's key and peers in `mesh/` |
| `--mesh-port PORT` | 51820 | the UDP port of the mesh's WireGuard device, on a node without `--pod-prefix` |
| `--mesh-address ADDRESS` | the address from which the node reaches the control plane | where the other nodes reach the node's mesh |

- **The join.** A node without an identity makes an ECDSA P-256 key and
  keeps it, then sends the token's secret, its hostname as its name and a
  signing request. Before the token is sent, the server's chain must hold
  the root the token pins, and its certificate must name
  `--control-plane-name` and carry the one URI `spiffe://<trust
  domain>/ferun`; the answer's trust bundle must hold that root. A join
  that fails is tried again with the same key; a refused token is not sent
  again until the file holds another.
- **The node's directory**, mode 0700: `key.pem` (PKCS#8, 0600),
  `chain.pem` (the node's certificate, then the issuer), `bundle.pem` (the
  platform CA's roots) and `node.json` (the name, the SPIFFE ID, the
  certificate's end and renewal time, the instances listed last), each
  written whole. The key stays across renewals.
- **Node connections**, ALPN `feos-node/1`, one to every instance the
  control plane lists: on each, `feosd` is the HTTP/2 server of the API it
  serves on its socket, with up to 1024 streams and pings of its own. A
  lost one is dialed again after 1 s, doubling up to 30 s, until `feosd`
  stops ([Stopping](#stopping)).
- **The uplink**, ALPN `h2`, to one instance at a time, moving to the next
  at once when a call fails: telemetry, three seconds per report, as often
  as the control plane asks; the certificate's renewal at its renewal
  time, after which new node connections and a new uplink use the new
  certificate while the old ones drain; the node's events, pods' and
  `feosd`'s own (`started`, and `stopping` on SIGTERM and before an
  upgrade), numbered from 1 per start and kept until the control plane
  acknowledges them. The buffer is in memory, at most 16384 events, the
  oldest dropped and counted; a buffer on disk is to follow. After the
  first report that is answered, `JoinMesh` ([the mesh](#the-mesh)), and
  again whenever the node's key or endpoint changes or an answer was not
  taken; a node with `--pod-prefix` sends no key and asks only whether
  the cluster is meshed.
- **A refused node.** `PERMISSION_DENIED` on the uplink means the node was
  removed or its certificate revoked: `feosd` closes its connections,
  moves its identity into `removed-<time>/` in the node's directory, and
  joins again once the token file holds a new token.

All connections are TLS 1.3, without session resumption, and the server is
always checked for `--control-plane-name` and the URI, later against the
trust bundle. A root, of the bundle or pinned, is trusted for its key,
without its name constraints (ADR 0024): rustls-webpki refuses every URI
name beneath a URI constraint, which the roots of `ferund init-ca` carried
before. The issuer's constraints still hold, and an issuer that
constrains URIs is refused. Their sockets have keepalive and a `TCP_USER_TIMEOUT` of
20 s.

The control plane's side runs on two threads of its own, on which nothing
else of `feosd` runs: the join, the reports, the events, the dialers and
the HTTP/2 of the node connections, their pings included. A call on a node
connection runs on `feosd`'s runtime, as one on the socket does, and so
does its answer's body; a call that the control plane resets is dropped
there. A report waits a second at most for the pods and the installed
versions, which may wait for the pods' records' lock, and reports them as
they were when they do not answer; the mesh's answer is taken on `feosd`'s
runtime, one at a time, and the report after learns whether it was taken
or has to be asked for again. The reports keep their rate whatever their
collection takes. A tokio runtime can stall as a whole while one of its
threads blocks, such as on an unlink of a file that Firecracker still
writes; the reports and the node connections go on all the same, and the
node stays up. Not for ever: a task on `feosd`'s runtime ticks every
second, and while it has not ticked for 90 s, or the pods have not
answered for as long, the node sends no reports, so that the control
plane counts it down and its sandboxes move rather than wait on calls
that hang. It reports again once `feosd` moves.

### Stopping

On SIGTERM, with a control plane, `feosd` tells it that it stops
(`stopping`, waiting up to 3 s for the acknowledgement) and then closes
the node connections gracefully (`Fleet::stop`):

1. The server sends HTTP/2 GOAWAY on every node connection and takes no
   new stream; the dialers dial no more and hold their connections until
   the server lets go of them. The control plane starts no new call on
   them: a call that had not reached the node waits for its next
   connection, up to the node's rollout deadline
   ([control-plane.md](control-plane.md#the-nodes-port)).
2. The calls and streams in flight end as they do, up to a tenth of
   `--stop-grace`, 30 s at most; a connection closes once its last call
   ended.
3. Those still running then end with `UNAVAILABLE` "the node stops: the
   call did not end within <N>s of feosd's stop": a call not answered yet
   gets it as its answer, and is dropped where it runs; a stream gets it
   as its trailers, after what it sent. That is done on the control
   plane's threads, whatever `feosd`'s runtime does. The connections close
   within 5 s of it.
4. The reports end, and `feosd` lets go of the control plane for the rest
   of its life.

So a call in flight when the node restarts either answers, or ends with a
clear status, never with the connection's EOF.

## A pod

A pod ([ADR 0004](../adr/0004-pods.md)) is a microVM
with the pod's ID, booted from the newest installed guest, on a disk of
its own from diskd ([disk.md](disk.md)), and the containers its spec
names, which the guest runs with the names as their IDs. Its spec gives
vCPUs and memory, the containers (image, command, environment, the
volumes they mount), whether it has a network, and its volumes. The
microVM has a balloon with free page reporting.

A pod's ID is 11 characters of lowercase Crockford base32, a letter
first, which ferund or `feosctl` mints ([ADR 0019](../adr/0019-pod-ids.md));
it names the microVM, the pod's paths and its TAP device. A pod made
before keeps its UUID, and `feosd` takes both (`feos_utils::pod_id`);
every call with any other ID fails with `INVALID_ARGUMENT`.

- **Create** claims the pod's volumes, records the pod as `PENDING` and
  brings it up in the background: open its disk and its volumes' disks,
  create and start the microVM, wait for the guest to attach (60 s at
  most), have it mount the volumes, then create (which pulls) and start
  the containers, in order. The pod is then `RUNNING`, or `FAILED` with
  the reason. Each step looks at what is there first, so a bring-up that
  a restart of `feosd` cut off is taken up again where it ended. A create with the ID of a pod
  that exists returns that pod if the spec is the same, and fails with
  `ALREADY_EXISTS` if not.
- **Baked images** (ADR 0025). A create may name, beside its spec, the
  image of the pod's one container, layers baked of it, and whether the
  pod may bake it (`image_layers`, kept in the pod's record and never
  compared). The pod's new disk starts on the first of those layers that
  is made for the newest guest installed (its directory's name) and that
  guest's blank data volume, and whose manifest diskd has or fetches
  (`Pull`); one it cannot fetch is passed over, and with none the disk is
  on the blank data volume. The record says what the disk was made on
  (`disk`). A pod on the blank data volume whose create lets it bakes the
  image at its bake point, after its volumes are mounted and before its
  first container, unless the guest has that container already or diskd
  has no object store: the guest pulls the image into its image store
  (`PullImage`), its data volume is frozen (`FreezeFilesystem`, after a
  thaw of a freeze a restart cut off), diskd seals the disk's top as a
  layer on L0 (`Bake`), and the volume is thawed. A pull, freeze or seal
  that fails, and a guest that cannot freeze, leave the bring-up as it
  would have been; a volume that stays frozen fails it. The layer goes to
  the object store in the background (`Push`, three tries), and once it
  is there the record names it (`baked`) and the pod's event says so.
- **Stop** ends a bring-up, shuts the microVM down, or removes it if it
  does not go or is not running, and stops the disk; a microVM that is
  busy (its late snapshot) is left alone, and the stop answers
  `UNAVAILABLE` "the VM is busy: <why>"; **Start** brings a
  stopped or failed pod up anew on its disk: the containers the disk holds
  are adopted, with what they wrote.
- **Suspend** and **Resume** suspend and resume the microVM, with
  `purge_guest`; the containers come back as they were. The disk stops
  with the microVM and starts again before the resume. A suspend runs in
  a task of its own, which holds the pod's lock: a call that goes away,
  as one past its deadline, leaves it running to its end, so that the
  pod's record follows its microVM. It answers within the suspend's
  budget ([The suspend's budget](#the-suspends-budget)), the suspend or
  `UNAVAILABLE` "the VM is busy: <why>", which changed nothing and may be
  asked again later.
- A running pod whose microVM is suspended without the pod, as when the
  microVM's suspend went through and the suspend failed after it (the
  microVM layer recorded the change later than 10 s), or a `feosd` before
  this one did not see a suspend to its end, is `SUSPENDED`, its disk
  stopped: once the pod's lock is free, its microVM's record decides,
  after the microVM's `Suspended` event and when `feosd` starts. A microVM
  that runs again after a failed suspend, its late snapshot discarded,
  finds its pod `RUNNING`, as the suspend left it.
- **Hibernate** and **Restore** take a pod into the object store and
  bring it back, on this node or another, with its memory or on its disk
  alone ([disk.md](disk.md#hibernation)). A hibernated pod is `HIBERNATED`,
  a record with its checkpoint; **Resume** restores it. A hibernation
  runs in a task of its own, which holds the pod's lock: a call that goes
  away leaves it running, one that comes while it runs joins it and gets
  its answer, and `GetPod` shows it in `PodInfo.hibernation` until it
  ends. A **Resume** that comes while the pod hibernates abandons the
  hibernation until its heads are being put, unless a call said
  `no_abandon` (a drain) or the pod has no memory: the hibernation stops,
  in the middle of a seal or an upload of diskd's too, its calls fail
  with `ABORTED`, and the pod resumes from its snapshot on the node.
  Otherwise the resume waits for the hibernation and restores the pod.
- **Delete** ends a bring-up and removes the microVM, the disk and the
  record. Its volumes stay, their disks stopped, attached to no pod. The
  pod leaves the pods and its record's removal is asked for in one step,
  under the pods' lock, so that no save of the pod (as of the addresses
  the network gives it when it comes up) lands behind the removal and
  writes the record back; a removal that fails puts the pod back, to be
  deleted again.
- A running pod whose microVM crashes is `FAILED`; one whose microVM stops
  is `STOPPED`. Either keeps its disk, and its disk stops. Volumes go as
  the disk does.

What changes a pod runs under a lock of the pod's; a call that waited for
it acts only if the lock is still its pod's, not on a pod created again
with the ID after a delete. Every call that changes a pod (create, start,
stop, delete, suspend, resume, hibernate, restore) runs in a task of its
own: a caller that goes away, as one whose deadline passed, whose stream
ferund reset, or that the node's stop cut, leaves the change running to
its end. Its steps await the pod's record, the microVM service and
diskd, and a change cut off between two of them would leave the pod
half changed, as a pod `PENDING` that nothing brings up, or one deleted
whose record stays. `GetPod` asks the guest for the containers'
states while the pod runs.

A call to the guest, as an exec, a port forward, `GetPod`'s states or a
bring-up's, connects to it on vsock within 5 s, its handshake included.
A guest that does not answer, as while its microVM is paused for a
snapshot or Firecracker still writes one, fails the call with
`UNAVAILABLE` "the guest of pod <id> did not answer within 5s", and "; the
VM is busy: <why>" when that is known: the pod is being suspended, it
hibernates, or the microVM's snapshot is late (its `check_free`). Such a
busy answer carries the busy marker (`vmm::marked_busy`, the status
detail `VmBusy`), so that ferund takes the guest for busy, not
unreachable, and does not repair the pod. A
connected call has no bound but its caller's: a stream lasts as long as
it lasts. The microVM service's own calls to a guest bound their connect
with their own deadline: the ping of a resumed guest, before the time
and the network are pushed, waits 30 s, its connect included, since a VM
restored from a snapshot written elsewhere has its memory served page by
page from object storage and may answer later than 5 s; a push and the
request to empty the memory before a snapshot wait 10 s.

## Volumes

A volume (`feos.volume.v1.VolumeService`,
[ADR 0016](../adr/0016-volumes.md)) is a disk of diskd's that outlives
the pods it is attached to ([disk.md](disk.md#the-volumes-disks)). The
code is `crates/services/pod-service/src/volumes.rs`.

- `CreateVolume(id, size, labels)` records a volume, `AVAILABLE`: a
  lowercase UUID, from 64 MiB to 16 TiB in whole MiB. A create with the
  same ID, size and labels finds it; another fails with
  `ALREADY_EXISTS`. Its disk is made at its first boot.
- `CreatePod` claims the volumes its spec names (`PodSpec.volumes`: a
  name, 1 to 63 lowercase letters, digits and `-`, and the volume's ID;
  at most eight), all or none: each must be on the node and attached to
  no other pod, else `FAILED_PRECONDITION`. They are `ATTACHED` to it
  then, until `DeletePod`, which leaves them `AVAILABLE`.
- A hibernation makes them `CHECKPOINTED`, their data in object storage
  alone; the next boot of a pod with one opens it from its checkpoint,
  and it is `ATTACHED` again. A checkpointed volume whose pod was
  deleted stays checkpointed.
- `DeleteVolume` removes a volume that no pod has, with its disk, and
  fails with `FAILED_PRECONDITION` while a pod has it.
- `Volume.formatted` is false until the guest reported the volume
  mounted: until then every boot asks the guest to format it
  ([guest.md](guest.md#volumes)).
- At its start, `feosd` lets go of volumes whose pod is gone and claims
  the volumes of kept pods that name no pod, after a crash between the
  two records.

After a boot, once the guest attached, `feosd` calls
`feos.guest.v1.GuestService.MountVolumes` with each volume's name, ID,
drive (1 for the first volume; 0 is the pod's disk), size and whether to
format it, records the volumes the guest mounted as formatted, and fails
the bring-up with the reasons of those it did not. Then it creates the
containers, whose `ContainerSpec.volume_mounts` (a volume's name, a path,
`read_only`) go to the guest's `ContainerConfig.volume_mounts`.

## The pods' network

Every pod has a network unless its spec says `none`. A pod has an IPv6
address, its address on the network, and an IPv4 address for leaving the
node ([ADR 0010](../adr/0010-pod-network.md)); its security groups decide
what it reaches ([ADR 0012](../adr/0012-security-groups.md)), and its
traffic is recorded ([ADR 0013](../adr/0013-traffic-records.md)). The code
is `crates/services/pod-service/src/network*`, on top of
`network-service` (the groups and their compiler), `dns-service` (the
resolver) and `traffic-service` (the records).

`feosd` takes these options:

| Option | Default | What it is |
|---|---|---|
| `--pod-prefix PREFIX` | none | the IPv6 prefix the network routes to the node, /1 to /120, such as a /64, AWS's /80 or GCP's /96. Without, a node with `--control-plane` takes a /96 of its cluster's [mesh](#the-mesh) |
| `--pod-ipv4-range PREFIX` | `169.254.16.0/20` | the pods' IPv4 addresses, a /32 each; /1 to /30, with neither the resolver's address nor `169.254.0.1` in it |
| `--dns-address ADDRESS` | `169.254.0.53` | the pods' resolver, an IPv4 address that the node holds, the same on every node; never `169.254.0.1` |
| `--dns-upstream ADDRESS` | Google's and Cloudflare's, IPv6 first | the upstreams of pods that name none (repeatable) |
| `--deny-prefix PREFIX` | none | what no pod reaches, of either family (repeatable) |

`--pod-ipv4-range` and `--dns-address` move the pods' IPv4 addresses and
the resolver where the node's own network uses link-local addresses.
Without `--pod-prefix` or a mesh, or when the network cannot be set up,
`feosd` logs the error and runs on: creating a pod with a network fails
with `FAILED_PRECONDITION` and the reason, and a kept pod with a network
fails when it boots or resumes. Pods without a network and the rest of
the API work as before. On a mesh whose /96 the node does not have yet,
the same holds until the control plane gives it one; the network comes up
then, as at a start: kept pods get the addresses they lack, the resolver
answers, the rules are written, and running pods get their TAP devices.

### Devices and routes

At its start `feosd`:

1. removes the first version's network: the bridge `feos-pods` and the
   table `bridge feos` (a kernel without the bridge family, as
   `feos-host-init`'s, has none to remove); and what the NAT64 version
   left: the device `feos-nat64` with its routes and its program, the
   maps pinned in `/sys/fs/bpf/feos/nat64`, and the counter of its IPv4
   pool, `/var/lib/feos/pods/next-address`;
2. turns IPv4 and IPv6 forwarding on; interfaces that take router
   advertisements (`accept_ra` 1) go on taking them (2);
3. routes the pod prefix as `unreachable`: a packet for a pod that is not
   here goes back as unreachable rather than out again;
4. gives the dummy device `feos-dns` the resolver's address as a /32, and
   no other global address, and takes the resolver's UDP and TCP sockets
   at it, port 53, that were handed to it ([DNS](#dns)), or binds them.

Of the pods' addresses on their links, the node holds the resolver's
alone. It never holds `169.254.0.1`: where that is the gateway of the
node's own network, as in some clouds, the node would lose its own.

Before every boot and resume of a pod, `feosd` makes its TAP device,
`pod-<ID>` ([ADR 0019](../adr/0019-pod-ids.md); `pod-<8 hex digits of
the ID>` for a pod with a UUID), a link of its own between the node and
the pod:

- no router advertisements, no address autoconfiguration and no
  link-local address of the kernel's, set before it goes up;
- the resolver's address, `169.254.0.53/32`, its one IPv4 address; ARP
  answered only for addresses on the device (`arp_ignore` 1), and asked
  from that address (`arp_announce` 2); forwarding on. Without an address
  of its own, the TAP's ARP for the pod would ask from any address of the
  node, such as `169.254.169.254`, which some hosts hold on `lo` for a
  metadata service of their own; the microVM's MMDS takes every ARP frame
  for that address, the pod's answers too, and the node would lose the
  pod's link whenever its neighbor entry for the pod lapsed;
- `fe80::1/64` without duplicate address detection: the pod's IPv6
  gateway;
- a proxy neighbor entry (`NTF_PROXY`) for `169.254.0.1`, the pod's IPv4
  gateway, and `proxy_delay` 0: the kernel answers ARP for it on the TAP
  at once, with the TAP's MAC address, as long as its own route to
  `169.254.0.1` leaves through another device, as any default route does,
  and forwarding is on;
- routes for the pod's addresses, a /128 and a /32, to it.

The guest is pushed its IPv6 address as a /128 with `fe80::1` as its
gateway, its IPv4 address as a /32 with `169.254.0.1` as its gateway, on
the link although outside the /32, and the resolver
([guest.md](guest.md)); its MAC address is `06:00` and the first four
bytes of the SHA-256 of its ID, for a pod with a UUID the 8 hex digits
of its TAP device. A pod whose IPv6 address is a ULA has no default
route over IPv6 ([ADR 0017](../adr/0017-pod-mesh.md)): it is pushed a route for
`fc00::/7` via `fe80::1` instead, with the mesh's MTU of 1420 on a mesh,
so that a name with addresses of both families is reached over IPv4 at
once (RFC 6724, rule 1) rather than after an unreachable that the node
sends at most once a second. A guest from before routes keeps its default
route until the pod boots anew. Deleting the pod deletes the TAP device, which takes the
routes and the proxy entry with it.

### Addresses

A pod gets an IPv6 address of the prefix and an IPv4 address of the
range when it is created, and keeps them in its record
(`PodInfo.ipv6_address`, `PodInfo.ipv4_address`) while it is on the node.

- **IPv6**: the next address comes from a counter in
  `/var/lib/feos/pods/next-ipv6-address`, from `::2` on, written before
  an address is handed out: no address is handed out twice, not after a
  restart either. The counter goes past any address a kept pod holds.
- **IPv4**: a /32 of the range, unique on the node; the range's first and
  last address are left out. An address never handed out comes first,
  the lowest; once there is none, the one that has been free the longest.
  `/var/lib/feos/pods/ipv4-addresses` keeps the lowest address never
  handed out and the addresses freed since, in the order they were freed
  and with when, and is written before an address is handed out. At its
  start `feosd` counts an address that was handed out, that no kept pod
  holds and that is not recorded as free, such as one whose pod was
  deleted while `feosd` could not record it, as free from then on.

Deleting a pod takes it off the network (the resolver, the TAP device),
then deletes the connection tracking entries whose original source is
one of its addresses, so that the next pod with its IPv4 address finds
none of them, and whose ends the records name the pod for ([traffic
records](#traffic-records)); then the pod leaves the table, and its IPv4
address is freed. Hibernating one does the same.

A pod kept from an earlier network gets what it lacks at the start of
`feosd`: an IPv6 address if it has none of the prefix, such as one of the
first version, and an IPv4 address if it has none of the range, such as
one of IPv6 alone or with an address of the first version's
`10.200.0.0/16`. Its microVM was made for that network: one that is
stopped is made anew at the next boot. One that runs, or is suspended,
when it resumes, keeps its devices; where only its guest's configuration
differs, as for a pod of IPv6 alone, its record takes this one and the
guest is pushed it, and applies it at once, its containers' resolvers
too (a guest that is not attached yet gets it when it attaches). One
whose devices differ, such as the first version's on a bridge, keeps the
old network until the pod is stopped and started, and `feosd` warns
about it.

### The mesh

A node whose network routes it no prefix, one with `--control-plane` and
without `--pod-prefix`, is on its cluster's mesh
([ADR 0017](../adr/0017-pod-mesh.md), `network/mesh.rs`):

- **The device** `feos-mesh`, a WireGuard link with an MTU of 1420, no
  addresses and no router advertisements, made at `feosd`'s start. Its
  private key is made once, 32 random bytes from `getrandom`, and kept in
  `<node dir>/mesh/key` (0600); the kernel derives the public key from it.
  It listens on `--mesh-port`. `feosd` sets it up over WireGuard's generic
  netlink family (`feos_utils::wireguard`), without `wg`.
- **The node's /96.** The fleet client calls `JoinMesh` with the public
  key and the endpoint, `--mesh-address` or the address from which the
  node reaches the control plane, at `--mesh-port`. The answer, the
  cluster's ULA /64 and the node's /96 of it, is kept in `mesh.pb`; the
  /96 is the node's pod prefix from then on, and the /64 is routed to
  `feos-mesh`. The node's own /96 stays routed as unreachable but for its
  pods' /128s, which are longer. Another /96 than the network was set up
  with takes effect at the next start. `feosd` still takes a /48 and a
  /64 of it, as the control plane gave them before, and peers of both
  lengths. At its start it removes, for the prefix kept, the routes an earlier
  prefix left: those of the main table
  to `feos-mesh` but the ULA's, and the unreachable ones in the /48 of
  the ULA's Global ID but its own prefix. A node with `--pod-prefix` in a
  meshed cluster fails its pods with a network: the two exclude each
  other.
- **The peers** come from the control plane over the node connection,
  `feos.network.v1.MeshService`: `PutMeshPeers` whole with a version,
  `UpdateMeshPeers` from the version the node has to the next, refused
  with `FAILED_PRECONDITION` from any other. They are kept in `mesh.pb`
  and given to the device as they change: a peer that stays keeps its
  session, one that goes is removed, one that is new or changed is set
  with its endpoint and its /96 as its one allowed prefix. WireGuard sends
  a packet to the peer whose /96 holds its destination and takes one from
  a peer only with that peer's key and a source in its /96. `GetMesh`
  shows the node's prefixes, key, port, version and peers.
- **The rules.** The peers' endpoints are denied to pods, as the node's
  own addresses are, and `prerouting` drops what comes from an address of
  the cluster's ULA through another device than the one it is routed to.

Without a control plane the mesh, its peers and its rules stay untouched;
pods reach each other across nodes only where their security groups allow
it, as anywhere.

### DNS

`feosd`'s resolver (`dns-service`) answers the pods it knows, by the
source address of a query, the pod's IPv4 address, which the TAP pins,
and refuses everyone else. It asks the upstreams of the pod's spec
(`PodNetwork.dns_upstreams`), or `--dns-upstream`, in order. A create
refuses an upstream that is not an IP address, is the resolver itself, a
loopback, link-local or multicast address, in a denied prefix, a pod's
(the pod prefix or the IPv4 range) or one of the node's addresses.

A name of one label, in any case, that the pod's name tables hold
(`PodNetwork.name_tables`, [Name tables](#name-tables)) the resolver
answers itself, with authority and never asking upstream: AAAA with the
name's IPv6 addresses, A with its IPv4 addresses (the loopback of a
sandbox's own attached services), each with a TTL of 5 s, and no record
of another type. Every other name goes upstream.

Answers pass as the upstream gave them: pods have IPv4, so nothing is
synthesized. The resolver takes `ech` out of HTTPS records, and records
every query it answers, its own answers too.

Its sockets outlive `feosd`, so that a query waits in them while `feosd`
restarts rather than fails. They are handed to `feosd` by systemd's
protocol (`sd_listen_fds`: from fd 3 on, `LISTEN_FDS`, `LISTEN_PID`,
`LISTEN_FDNAMES`, here `dns`), by `feosd.socket` under systemd
([Packages](#packages)), by `feos-host-init` under FeOS, and by `feosd`
itself to the binary of a self-upgrade. `feosd` takes, before it starts
threads, the UDP socket and the listening TCP socket bound to
`--dns-address`, port 53, closes the others, and removes the variables
from its environment; without, it binds its own. It answers on them once
it has gone through its pods, so that a query that waited is not
refused.

### Metadata

A pod with a network has a metadata service at `169.254.169.254`, which
Firecracker answers in the microVM's network device, `eth0` (MMDS,
ADR 0010); its packets never reach the TAP. It is version 2: a `GET`
needs a session token, which a `PUT /latest/api/token` with the header
`X-metadata-token-ttl-seconds` (1 to 21600) hands out, and which the
`GET` carries in `X-metadata-token`; without one it gets `401`. With
`Accept: application/json` an answer is JSON, without it a string, or
the keys of an object one per line.

It holds a JSON object:

```json
{
  "feos": {
    "pod-id": "22222222-3333-4444-5555-666666666666",
    "ipv6": "2001:db8:0:1::5",
    "ipv4": "169.254.16.5"
  },
  "metadata": {"role": "web"}
}
```

`feos` is the pod's ID and its addresses on this node; `metadata` is the
spec's (`PodSpec.metadata`), keys of 1 to 128 letters, digits, `-`, `_`
and `.`, at most 32 KiB as JSON. `GET /feos/ipv6` gives the address,
`GET /metadata/role` the value.

The VM service writes the object into the microVM's directory,
`metadata.json`, when the microVM is made, and Firecracker loads it
before the guest boots (`--metadata`). A snapshot keeps where the
metadata is served, not what: after every load, a resume on the node as
a restore from a checkpoint, the VM service puts it in again
(`PUT /mmds`) before the guest runs, with the addresses the pod has on
this node; a restored pod has new ones. A restart of `feosd` leaves
Firecracker's store alone. A microVM made before pods had a metadata
service has none: one that is stopped is made anew at the next boot,
one that runs or is suspended goes on without it until the pod is
stopped and started, and `feosd` warns about it: MMDS cannot be turned
on in a running Firecracker.

Every other link-local destination is denied on the node, so no pod
reaches the node's own cloud metadata service.

### The table

`feosd` writes one nftables table over netlink, `inet feos`. Its base
chains accept what they do not decide on, so that the node's other
traffic is not theirs:

| Chain | What it does |
|---|---|
| `prerouting` (filter, -150) | from a pod's TAP: drops packets whose source is not routed back out the TAP (`fib saddr . iif oif missing`), of either family, logged as `spoofed`, and router advertisements and redirects of both families; on a mesh, the same for a source in the cluster's ULA through any device |
| `input` | from pods takes neighbor solicitations and advertisements and DNS to the resolver's address over UDP and TCP, and drops the rest |
| `forward` | copies the first 4 packets of the TCP connections pods open to nflog group 3 (`ct direction original`, `ct original packets < 5`), of both families, before anything else; accepts what belongs to a connection; drops invalid packets from and to pods; then jumps to `from-pods` what comes from a TAP and to `to-pods` what goes to one |
| `from-pods`, `to-pods` | go through the verdict maps `pod-out` (by input interface) and `pod-in` (by output interface) to the pod's chains, and drop and log (`no-rule`) what a TAP the maps do not have sends or receives, such as one whose rules failed to be written |
| `out-<TAP>`, `in-<TAP>` | a pod's chains: drop and log (`deny`) what goes to or comes from a denied address, `return` what a rule of the pod's groups allows, drop and log (`no-rule`) the rest; `out-` of both families, `in-` of IPv6 alone, since nothing new comes in over IPv4 |
| `postrouting` (nat, 100) | masquerades the pods' IPv4 range where it leaves the node (`ip saddr 169.254.16.0/20 oifname != "pod-*" masquerade`) |

ARP is not IP: the table does not see it, and the kernel answers a pod
only for `169.254.0.1`, by proxy. A pod's IPv4 leaves the node
masqueraded to the node's address, through Linux's connection tracking;
its IPv6 is routed as it is. Every node needs IPv4 of its own for its
pods to reach IPv4 destinations; under `feos-host-init`, DHCPv4 gives it.

A pod's chains end allowed traffic with `return`, not `accept`, which
would end the base chain: a packet between two pods passes the sender's
`out-` chain and the receiver's `in-` chain. The maps go to them
(`goto`), so their `return` leads back to `forward`. The sets are what
`network-service` compiles, each of one family: `deny4` and `deny6` (the
denied prefixes, the node's own addresses, the mesh's peers' endpoints,
the pods' IPv4 range,
link-local, loopback, multicast and unspecified addresses: pods reach
each other over IPv6 alone), `internet4` and `internet6` (the default
group's), `g-<group>` (a group's members, of IPv6) and `b-<hash>` (an
address block without its exceptions, of its family). An IPv4 block
counts in rules out; rules in ignore it. ICMP rules are of IPv4, ICMPv6
rules of IPv6. Drops are logged to nflog group 2, at most 10 a second per
pod and reason.

`feosd` replaces the table at its start and whenever pods come or go or
rules change; when only members of groups or the node's addresses change,
it adds and removes set elements in one batch. It writes the rules again
when a group changes and every minute, which picks up new addresses of
the node.

Other tables still apply: where Docker or a firewall sets the `FORWARD`
policy to drop, pods reach nothing until it lets them through.

### Security groups

`feosd` keeps the groups it is sent (`feos.network.v1.SecurityGroupService`)
in `/var/lib/feos/security-groups/<id>.pb`. A pod's spec names its groups
(`PodNetwork.security_groups`); one that names none has the default
group: out to the internet, nothing in. A pod whose groups the node does
not have does not boot or resume: its bring-up fails, and the pod is
`FAILED` with a reason that names them; `StartPod` brings it up once they
are there, and `ResumePod` fails with `FAILED_PRECONDITION` until then. A
group that a pod names cannot be deleted.

### Name tables

`feosd` keeps the name tables it is sent (`feos.network.v1.NameService`)
in `/var/lib/feos/name-tables/<id>.pb`, each with an ID, a version, its
names, each one label of a DNS name in lower case with one or more IPv6
or IPv4 addresses, and the tables it includes. A pod's spec names its
tables (`PodNetwork.name_tables`), and the resolver answers it their
names ([DNS](#dns)): the first of the pod's tables that holds a name
answers it, a table's own names before those of the tables it includes,
in their order; the includes of an included table are not followed, and
one the node does not have holds nothing. A pod whose tables the node
does not have does not boot or resume, as with its groups; a table that
a pod names cannot be deleted. A delta (`UpdateNameTable`) puts and
removes names from the version the node has to another, which the
control plane chooses and need not grow. The tables live on the node, so
that a pod's names hold while it is paused, suspended or restored, with
nothing in its guest.

### Traffic records

`feosd` records what its pods do on the network
(`feos.network.v1.TrafficService`, `feosctl traffic watch`):

- **flows** from connection tracking's events over ctnetlink, with
  `nf_conntrack_acct` and `nf_conntrack_timestamp` on, which `feosd` turns
  on once the table has loaded connection tracking;
- **drops** from nflog group 2, the reason from the rule's prefix;
- **server names** from the copies on nflog group 3: a TLS ClientHello's
  server name, or an HTTP request's Host;
- **DNS** queries from the resolver.

The collector's threads and the resolver feed one channel, from which
every call streams; a pod's queries to the resolver are left out of the
flows, which the DNS records cover. What is lost on the way is counted in
the next record's `missed`.

A record names the pod by the addresses of the event, or by its TAP
device. A pod that left, deleted or hibernated, is known by its
addresses for five minutes more, unless another pod holds one of them by
then: the ends of the connections that its delete or hibernation ends
are read after it left.

## A pod's microVM

The microVM layer under the pods (`crates/services/microvm-service`,
internal `feos.microvm.v1`) runs the microVMs; FeOS runs no VMs of their
own. A microVM's directory, `/var/lib/feos/vms/firecracker/<id>/root/`, is
the chroot its Firecracker runs in:

| File | What it is |
|---|---|
| `config.json` | the Firecracker configuration, written at creation |
| `fc.sock` | Firecracker's API socket |
| `v.sock` | the host end of the guest's vsock device; `v.sock_10001` is where the guest's uplink arrives |
| `console.in` | a FIFO, the serial console's input (Firecracker's stdin) |
| `serial.log` | the serial console's output since the last boot (Firecracker's stdout) |
| `vmm.log` | Firecracker's own log |
| `firecracker.pid`, `firecracker`, `dev/` | the jailer's |
| `vmlinuz`, `initramfs` | the installed guest's files, linked in |
| `data` | the data disk: a device node of the microVM's own, of the device the `data_disk` link names, made at every start and restore |
| `volume0`, `volume1`, ... | the pod's volumes, as `data` |
| `snapshot/` | the snapshot of a suspended microVM: `vmstate` and `memory` |
| `memory.sock` | where diskd serves the memory of a microVM restored from object storage |

`jail.json`, beside the chroot, records what a start links in and what a
deletion undoes.

### Lifecycle

- **Create** takes the newest installed guest and keeps that version: its
  jail links that version's files. It writes `config.json`; no process
  starts.
- **Start** runs the jailer, which starts Firecracker, which boots the
  microVM at once. The process ends when the guest reboots or shuts down:
  exit code 0 makes the microVM stopped, anything else crashed.
- **Shutdown** sends Ctrl-Alt-Del and waits for the process to end.
- **Suspend** writes a snapshot, ends the process and punches holes over
  the pages of zeroes. A snapshot that is not written within its timeout,
  120 s and 4 s per GiB ([The suspend's budget](#the-suspends-budget)),
  fails the suspend at once (`UNAVAILABLE` "the VM is busy: <why>").
  Firecracker cannot stop in the
  middle of one and writes on; its files stay alone, and the microVM
  refuses suspends and shutdowns as busy. Once Firecracker is done, the
  microVM resumes if it ran, and the snapshot is discarded. Until then
  every suspend and shutdown of the microVM fails with `UNAVAILABLE` "the
  VM is busy: Firecracker still writes the snapshot …"; a delete ends it.
  A busy answer carries a marker, which tells it apart from any other
  `UNAVAILABLE` whatever the message says (`vmm::busy_status`,
  `vmm::is_busy`): a status detail, a `google.protobuf.Any` of type
  `type.googleapis.com/feos.microvm.v1.VmBusy` without a value, in the
  status's `grpc-status-details-bin`. feosd's own stop and FeRun go by
  it.
  A marker in
  the jail, `late-snapshot`, keeps this across a restart of `feosd`: the
  next one finds it when it takes the microVM on, and waits for
  Firecracker to answer again before it does the same. One whose
  memory file does not grow for five minutes, as when its memory's
  userfaultfd handler is gone, ends Firecracker: the microVM crashed. The
  first suspend after a restore from another node reads all of the memory
  through diskd from the object store (ADR 0008), and can take that long.
  **Resume** loads the snapshot with the vCPUs
  stopped, then resumes them and removes the snapshot. A created microVM
  that never booted resumes from a `RemoteSnapshot`: its vmstate is moved
  into `snapshot/`, and its memory comes from a userfaultfd handler on
  `memory.sock`; it runs the installed Firecracker of the snapshot's
  version, which it keeps.
- **Delete** stops the microVM, removes what `jail.json` records, its TAP
  devices and its directory.

The layer records a change of state after the call that made it returns;
the pod service waits for it. A microVM has one data disk at most and
its volumes after it, in their order, which Firecracker opens with a
writeback cache, so that the guest's flushes reach diskd, and no huge
pages, which Firecracker backs neither an initramfs nor a balloon with.

### The suspend's budget

A `SuspendPod` answers within its budget, which its caller's deadline
covers, so that the caller hears the suspend's end, or that the VM is
busy, before it gives up: FeRun's pause waits the budget and 30 s more
([control-plane.md](control-plane.md#ops)). Both follow from one formula,
in `microvm_service::vmm::budget` and in ferund's `feosdSuspendBudget`,
whose tests hold the same table. GiB is the microVM's memory, rounded up.

| Part | Takes at most |
|---|---|
| before the snapshot: the guest empties its memory (10 s), free page reporting settles (3 s), Firecracker's API reads and pauses the microVM (10 s each), the snapshot's directory | 40 s |
| the snapshot | 120 s + 4 s/GiB |
| after it: Firecracker's version (10 s), its end (5 s), the microVM's record (10 s), the host record, the pod's record and its disk; the zeroes punched out of the memory file | 30 s + 1 s/GiB |
| **the budget** | **190 s + 5 s/GiB** |

4 s per GiB is a write of 256 MiB/s, which a node's disk sustains while
it writes back other memory, and 120 s leaves room for a sync behind
it. A snapshot that is slower is late: its suspend fails as busy at the
snapshot's timeout, still within the budget, and Firecracker writes on
(above). The parts before and after the snapshot are bounded by their
own timeouts but for the file steps, which their slack covers.

### Jail, user and cgroup

Firecracker runs in the jailer: chrooted into the microVM's directory, in
a mount namespace of its own, as a user of the microVM's own, which owns
its TAP devices. The layer creates TAP devices that do not exist and
deletes them with the microVM.

Every microVM has a cgroup of its own, `vms/<id>`, with `cpu.weight` 100
per vCPU and no caps: `/sys/fs/cgroup/vms/<id>` under `feos-host-init`,
below the cgroup `feosd` is given;
`/sys/fs/cgroup/system.slice/feos-vms.service/vms/<id>` under systemd,
below the cgroup `--vms-cgroup` names; in a container with a cgroup
namespace of its own, `/sys/fs/cgroup/vms/<id>` as the container sees it,
below the container's cgroup. `feosd` creates the microVM's
cgroup and enables `cpu` and `memory` in the cgroup they go below and in
`vms/`, never above; the jailer only moves Firecracker into it. A cgroup
whose children use controllers may hold no processes, so `feosd` moves
into the leaf `feosd` of its cgroup, where systemd starts it already with
`DelegateSubgroup=feosd`. Only the real root of the hierarchy may hold
processes beside such children, and there `feosd` stays; the root of a
cgroup namespace, which `/proc/self/cgroup` shows as `/` too, is a
container's cgroup and may not. The kernel tells them apart:
`cgroup.type` is in every cgroup but the real root.

Under systemd the microVMs are not below `feosd.service`: from systemd 258
on, systemd starts a service's main process in the service's cgroup and
moves it into its subgroup only then, which the kernel refuses while the
cgroup has controllers on for its children, and `feosd.service` would not
start again until its microVMs were gone. `feos-vms.service` holds them
instead: a unit that runs no process, which systemd never has to start one
in. A `feosd` from before `--vms-cgroup` put its microVMs below its own
cgroup: `feosd.service`'s `ExecStopPost=` (`feosd --hand-over-vms`), and
`feosd` when it starts, move their processes to `feos-vms.service`, each
with its `cpu.weight`, and turn `cpu` and `memory` off in `feosd.service`'s
cgroup. The memory a microVM was charged before stays with its old cgroup.

### The guest

The layer listens on `v.sock_10001` from the microVM's creation to its
deletion and again after a restart of `feosd`. When the guest attaches
(after a boot, or when its call was lost) and after every resume, it
pushes the wall time and, if the pod has an address, its network
configuration through the guest's API. After a resume it pings the guest
once instead of waiting for the uplink.

### Balloon

A pod's microVM has a virtio balloon with free page reporting: the guest
hands the memory it frees back. A suspend first asks the guest to drop
its caches and compact its memory, and waits until the VMM stops
shrinking.

## Firecracker

Firecracker versions are installed side by side in
`/usr/lib/feos/firecracker/<version>/`, as the packages lay them out
([ADR 0009](../adr/0009-node-os-and-packages.md)). A boot runs the newest
version that has `firecracker` and `jailer`, or, where none is installed,
the ones in `PATH`; `jail.json` records which. A restore runs what the
snapshot's boot ran, and fails with `FAILED_PRECONDITION` if that is not
installed any more. The tests download Firecracker v1.17.0, pinned by
sha256.

## On Kubernetes

On Kubernetes ([ADR 0020](../adr/0020-feos-on-kubernetes.md)) `feosd`
and `diskd` run in a privileged pod per node, from a DaemonSet, the same
binaries as the packages', and the microVMs live and end in `feosd`'s
container. What the pod needs:

- **Process namespace**: `shareProcessNamespace: true`, so that the
  pod's pause process is PID 1, which `feosd` refuses to be. Its
  container's command execs `feosd` (a wrapper that stays would hold the
  container's cgroup, which `feosd` leaves). `feosd` is the subreaper of
  its children either way.
- **Cgroups**: the container's own cgroup namespace (containerd's default
  on cgroup v2), mounted read-write, as a privileged container has it.
  `feosd` moves into `feosd/` of the container's cgroup and puts the
  microVMs into `vms/<id>` beside it ([Jail, user and
  cgroup](#jail-user-and-cgroup)); `kubectl exec` and exec probes land in
  `feosd/` too. When the container ends, the microVMs end with it.
- **Host**: `hostNetwork` (TAP devices, nftables, the mesh's WireGuard
  device, the resolver's address, which `feosd` binds itself, without
  `feosd.socket`), the host's `/dev` (KVM, TUN, ublk), `hostPath`
  `/var/lib/feos` (a directory on disk, never shared with a `feosd` or
  `diskd` of the host's: their locks keep a second one waiting) and
  `/usr/lib/feos` with every Firecracker and guest version the pods'
  snapshots pin; `/run/feos` may be the pod's own. `$NOTIFY_SOCKET` is not
  set, and nothing waits for it.
- **diskd stops after `feosd`**: the pods' disks and the memory of pods
  restored from a checkpoint are `diskd`'s, and a suspend writes through
  them. `diskd --stop-after-lock /var/lib/feos/feosd.lock` serves on after
  SIGTERM until no process holds `feosd`'s lock, then exits; better still,
  `diskd` is a sidecar (an init container with `restartPolicy: Always`),
  which Kubernetes starts before `feosd` and stops after it.

### Suspend on stop

`feosd --suspend-on-stop` is for a `feosd` whose microVMs end with it. On
SIGTERM it

1. refuses every call that changes a pod (`CreatePod`, `StartPod`,
   `StopPod`, `DeletePod`, `SuspendPod`, `ResumePod`, `HibernatePod`,
   `RestorePod`) with `UNAVAILABLE`, "feosd stops";
2. with a control plane, tells it that `feosd` stops and closes the node
   connections as [Stopping](#stopping) says: GOAWAY, the calls in flight
   up to a tenth of `--stop-grace` (30 s at most), the rest ended with
   `UNAVAILABLE` "the node stops"; then the reports end, and `feosd` lets
   go of the control plane for good;
3. suspends every running pod into its directory as `SuspendPod` does
   ([ADR 0008](../adr/0008-suspend-and-restore.md)), four at a time, and
   records each in `/var/lib/feos/suspended-for-stop.json` (the pods and
   the time of the stop, written whole and synced) as soon as it is
   suspended, so that a stop that is killed keeps what it did. It waits for
   the hibernations in flight, whose pods are not running on the node
   then. A pod whose microVM is busy (`UNAVAILABLE` marked busy, as while
   Firecracker still writes a late snapshot: [The suspend's
   budget](#the-suspends-budget)) is tried again every 5 s after the first
   pass over all pods, so that it holds none of the four places meanwhile,
   until it is suspended, or found suspended as its microVM's suspend went
   through after all (both recorded), or until 10 s before
   `--stop-grace` ends. A pod whose suspend fails, or that is still busy
   then, ends with `feosd`, as on a crash;
4. exits 0.

The next `feosd` on the node resumes the recorded pods that are still
suspended, four at a time, once `diskd` answers (it waits up to two
minutes), and removes the record. A pod whose resume fails stays suspended
and is logged; `feosd` serves on. A stop during the resumes keeps the pods
not resumed yet in the record. Without `--suspend-on-stop` a record is
honoured all the same, so a rollback leaves no pod behind.

**With the control plane** `feosd` resumes before it connects: it starts
the join, the node connections and the reports only once the resumes are
done, or after two minutes, while the rest go on. FeRun so never sees the
pods suspended. Its rows stay `running` while the node is away: calls to
the node wait for its rollout deadline ([control-plane.md](control-plane.md),
`--rollout-grace`), the node is marked down when its reports stay away
past its TTL, and its sandboxes become `unknown`, which no repair and no
tombstone acts on. The new start requests a reconcile, which finds the
pods running as the rows say. A pod event or an inventory hash with the
pods suspended, a call into a suspended guest that marks it unreachable
for the repair, or a `ResumePod` racing `feosd`'s own resume cannot
happen, since the node connections close, their calls done or ended,
before the first suspend and open after the resumes. An op of FeRun's
that the stop cut off reads the pod again once the node is back, as
after any lost answer.

A stop longer ago than five minutes is not resumed when a control plane
is configured: FeRun counts the sandboxes of a node that is down past its
`--lost-grace` (10 minutes) as lost or hibernated and may restore them
elsewhere. The pods stay suspended, FeRun's reconcile finds them so, and
it resumes them on their next use, or deletes them where their sandbox
moved on.

**The budget.** A suspend writes the guest's memory at the disk's speed:
the snapshot is as large as the guest's memory, less what free page
reporting gave back, which is written as zeroes and punched out. The pod's
`terminationGracePeriodSeconds` covers the 3 s of the `stopping`
acknowledgement, the calls in flight (a tenth of the grace, 30 s at
most), and about 60 s plus 4 s per GiB of the memory the node gives
microVMs, as the snapshot's timeout counts it
([The suspend's budget](#the-suspends-budget)); `feosd --stop-grace`
says the same grace (300 s by default, feos-node's). FeRun's
`--rollout-grace` covers that grace plus
the start of the new pod and its resumes, so that calls wait rather than
fail. A crash, a kill past the grace or the OOM killer ends the microVMs
that are not suspended yet: FeRun treats them as on a lost node.

## Packages

On Debian 13 and Garden Linux, `feosd` and what it runs come as `.deb`
packages from https://apt.fecode.dev
([ADR 0009](../adr/0009-node-os-and-packages.md)):

| Package | Contents |
|---|---|
| `feosd` | `/usr/sbin/feosd`, `feosd.service`, `feos-vms.service`, `feosd.socket`, the group `feos` (sysusers), `/etc/default/feosd`, `/etc/apt/apt.conf.d/50feosd` |
| `feosctl` | `/usr/bin/feosctl` |
| `diskd` | `/usr/sbin/diskd`, `/usr/sbin/diskctl`, `diskd.service`, `/etc/default/diskd`, `/usr/lib/modules-load.d/diskd.conf` |
| `feos-firecracker-<version>` | `/usr/lib/feos/firecracker/<version>/{firecracker,jailer}` |
| `feos-guest-<version>` | `/usr/lib/feos/guest/<version>/{vmlinuz,initramfs,data.img.zst}` |
| `feos-firecracker`, `feos-guest` | nothing; they depend on the newest versioned package |

`feosd` depends on `diskd`, `feos-firecracker` and `feos-guest`; the last two depend on the
newest versioned packages, as `linux-image-amd64` does on the newest
kernel: an upgrade installs a new Firecracker or guest beside the old
ones, which the pods booted with them keep using. `50feosd` keeps every
version from `apt autoremove`; a version nothing uses is removed with
`apt remove`.

`feosd.service` is `Type=notify`, with `Delegate=yes`,
`DelegateSubgroup=feosd` and `KillMode=process`: a restart or an upgrade
stops `feosd` alone, and the next one takes the pods over. It wants
`feos-vms.service`, whose cgroup holds the microVMs' (`--vms-cgroup`):
a oneshot unit with `RemainAfterExit=yes` and no process, `Delegate=yes`
and `KillMode=process`; stopping it stops no microVM.
`StateDirectory=feos` makes `/var/lib/feos` a directory on disk.
`FEOSD_ARGS` in `/etc/default/feosd` passes options, such as `--tcp`
and the pods' network's, of which the file has a description; it gives
no `--pod-prefix`, so pods with a network fail until one is set.
Installing the package enables the unit the first time and starts it, or
restarts it after an upgrade, whenever it is enabled; removing the package
stops the unit; purging it disables the unit. `/var/lib/feos` stays.

`feosd.socket` holds the pods' resolver's sockets, UDP and TCP at
`169.254.0.53:53`, `FreeBind=yes` since `feosd` gives the node the
address later, named `dns`; `feosd.service` wants it, and it starts
before. A restart or an upgrade of `feosd` leaves it, and a query while
`feosd` is stopped starts `feosd`. A node whose `--dns-address` is
another moves the socket with a drop-in, as the unit says. Installing the
package stops a running `feosd` first when the socket is not active yet:
a `feosd` from before the socket holds the address itself. Removing the
package stops the socket too.

`diskd.service` is `Type=notify`: active once diskd serves, its devices
recovered; `feosd.service` wants it and starts after it. `KillMode=process`
and the devices' user recovery keep the pods' disks across a restart or
an upgrade of diskd. `StateDirectory=feos/disk`, `LimitMEMLOCK=infinity`:
every started device locks its buffers. `/etc/default/diskd`, mode 0600,
holds `DISKD_ARGS` and the object store's `S3_BUCKET` and `AWS_*`; without
them pods do not hibernate. The package loads `ublk_drv` when it is
installed and at every boot, and behaves like `feosd`'s on install,
removal and purge; `/var/lib/feos/disk` stays.

The repository has one suite, `stable`, with the component `main`, and is
signed by its own key:

    curl -fsSL https://apt.fecode.dev/feos.gpg -o /usr/share/keyrings/feos.gpg
    echo 'deb [signed-by=/usr/share/keyrings/feos.gpg] https://apt.fecode.dev stable main' \
      > /etc/apt/sources.list.d/feos.list
    apt-get update && apt-get install feosd feosctl

`make publish-apt` builds the packages of `HEAD` as
`<version>~dev.<UTC time>.<commit>`, below the release they lead to, and
`hack/packages/publish-apt` uploads them to the bucket behind the
repository, keeping what was published before.
