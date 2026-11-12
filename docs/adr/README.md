Architecture Decision Records
=============================

An ADR records one decision: its context, what was decided, the
alternatives, and what follows. How the system works is described in
[docs/overview.md](../overview.md) and [docs/architecture/](../architecture/);
the ADRs say why it works that way. What the ADRs left open and still
want is in [BACKLOG.md](BACKLOG.md).

## Index

| # | Decision | Status |
|---|---|---|
| [0001](0001-what-we-build.md) | What we build: two products on one platform of isolated pods | Accepted |
| [0002](0002-one-repository.md) | One repository, laid out by language | Accepted |
| [0003](0003-programs.md) | The programs of node and control plane, and where they run | Accepted |
| [0004](0004-pods.md) | Pods are what feosd runs | Accepted |
| [0005](0005-firecracker.md) | Firecracker is the VMM, in its jailer | Accepted |
| [0006](0006-control-transport.md) | The control plane's calls travel through Valkey | Superseded by [ADR 0015](0015-control-plane.md) |
| [0007](0007-guest.md) | feos-guest, the pod's PID 1, and its API | Accepted |
| [0008](0008-suspend-and-restore.md) | Suspend to the node, hibernate to object storage, restore anywhere | Accepted |
| [0009](0009-node-os-and-packages.md) | One feosd under systemd or feos-host-init, from signed packages per version | Accepted |
| [0010](0010-pod-network.md) | The pod network: IPv6 per pod, routed or meshed, IPv4 for leaving | Accepted |
| [0011](0011-disk-daemon.md) | Disks are chains of sealed layers, served by one disk daemon per node | Accepted |
| [0012](0012-reachability-from-declarations.md) | Reachability from declarations, enforced by security groups on the node | Accepted |
| [0013](0013-traffic-records.md) | feosd records the pods' traffic | Accepted |
| [0014](0014-identity-and-credentials.md) | A workload's identity and credentials stay on the host | Accepted |
| [0015](0015-control-plane.md) | FeRun, one replicated control plane that nodes connect to over mTLS | Accepted |
| [0016](0016-volumes.md) | A volume is the node's resource of fixed size, held by one pod | Accepted |
| [0017](0017-pod-mesh.md) | A WireGuard mesh carries pods' IPv6 between nodes | Merged into [ADR 0010](0010-pod-network.md) |
| [0018](0018-environments-and-deployments.md) | An environment changes only by deployments: planned, numbered and immutable | Accepted |
| [0019](0019-pod-ids.md) | Pods have short IDs that the control plane mints | Accepted |
| [0020](0020-feos-on-kubernetes.md) | FeOS on Kubernetes: a pod per node whose microVMs end with it | Accepted |
| [0021](0021-ingress.md) | The ingress: a program of its own and a peer of the pod mesh | Accepted |
| [0022](0022-images.md) | Images: the registry asked before every pod, baked once behind the question | Accepted |
| [0023](0023-calls-outlive-their-caller.md) | A keyed tool call outlives its caller | Accepted |
| [0024](0024-no-uri-name-constraints.md) | The platform CA carries no URI name constraint, and nodes trust a root for its key | Accepted |
| [0025](0025-private-images.md) | Private images: a create brings its registry credentials | Merged into [ADR 0022](0022-images.md) |
| [0026](0026-the-sink.md) | The sink keeps what the platform and its pods say | Accepted |
| [0027](0027-the-guests-clock.md) | A guest's clock follows its node's, slewed from measurements over vsock | Accepted |
| [0028](0028-http-bindings.md) | HTTP bindings: APIs by a name, the credential added on the node | Accepted |
| [0029](0029-static-sites.md) | Static sites: immutable bundles, served from the object store, switched blue-green | Proposed |
| [0030](0030-cdns.md) | CDNs in front of the ingress, declared on a tenant's domain | Proposed |
| [0031](0031-people-in-ferun.md) | People in FeRun: tenants with members, grants, and the tenants' console | Accepted |
| [0032](0032-the-deployment-builder.md) | The deployment builder: one draft, forms and JSON, planned on every change | Accepted |
| [0033](0033-credentials-of-a-tenant.md) | Credentials of a tenant: a binding stated once, versioned, used by name | Accepted |
| [0034](0034-reachability-from-declarations.md) | Reachability from declarations | Merged into [ADR 0012](0012-reachability-from-declarations.md) |
| [0035](0035-everything-runs-in-an-environment.md) | Everything runs in an environment: sandboxes and tenants' jobs belong to one | Accepted |
| [0036](0036-a-workloads-identity.md) | A workload's identity: one ID per service, job, addon and sandbox | Merged into [ADR 0014](0014-identity-and-credentials.md) |
| [0037](0037-environments-can-be-renamed.md) | An environment can be renamed | Merged into [ADR 0018](0018-environments-and-deployments.md) |
| [0038](0038-addons-admit-a-workload-by-its-certificate.md) | Addons admit a workload by its certificate and by nothing else | Accepted |
| [0039](0039-valkey-admits-a-workload-by-its-certificate.md) | Valkey admits a workload by its certificate and by nothing else | Merged into [ADR 0038](0038-addons-admit-a-workload-by-its-certificate.md) |
| [0040](0040-addons.md) | Addons: managed backing services from blueprints, on volumes of their own | Accepted |
| [0041](0041-jobs.md) | Jobs of an environment, and a tenant's job that runs exactly once | Accepted |
| [0042](0042-attached-services.md) | Attached services: a service as a supervised process in a sandbox | Accepted |
| [0043](0043-the-route-door.md) | The route door: ferund serves an environment's HTTP routes with route tokens | Accepted |
| [0044](0044-secrets.md) | Secrets of an environment: versioned values that a deployment pins | Accepted |
| [0045](0045-hosts-domains-and-certificates.md) | Hosts, domains and certificates of public environments | Accepted |
| [0048](0048-guest-supervises-containers.md) | The guest supervises a pod's containers | Proposed |
| [0050](0050-node-pulls-images.md) | The node pulls images, and a bake pod unpacks them | Proposed |
| [0052](0052-pushed-membership-deltas.md) | Membership changes reach the nodes as pushed deltas, members before names | Proposed |
| [0053](0053-kubernetes-and-ssh-bindings.md) | Kubernetes and SSH bindings: the host adds the credential | Proposed |
| [0054](0054-visitors-of-an-environment.md) | Visitors: a guarded environment admits whom its tenant's access provider vouches for | Proposed |

## Rules

- **One decision per ADR**, with a title of one line. The first
  paragraph after the status line states the decision whole; a reader
  who stops there knows what was decided.
- **Sections:** Context, Decision, Alternatives considered,
  Consequences, Open questions (only when something is undecided and
  blocks; at most three), History. Four-digit numbers, in English.
- **Status:** `Accepted` when the decision is in force and the code
  implements it; `Proposed` when it is decided in direction and not
  built; `Merged into NNNN` or `Superseded by NNNN` for a number whose
  decision now lives elsewhere. A merged or superseded file is a stub of
  a few sentences and stays, because code and documents cite ADR
  numbers.
- **Written as if it had always been so.** The Decision carries no
  history: no earlier versions, no amendments, no dates. What changed,
  when and why stands in History, in a few sentences with the commits.
  An alternative that once was the decision stands under Alternatives
  with the reason it was reversed.
- **Decisions, not descriptions.** A concrete value (an address, a port,
  a path, a flag, an RPC, a file name) stays only when the decision is
  about that value. Everything else belongs in
  [docs/architecture/](../architecture/), which changes with the code in
  the same commit.
- **Accepted ADRs are not rewritten.** A change of mind is a new ADR
  that supersedes or amends the old one, or a new section under a new
  number. Open questions that are wanted and block nothing go to
  BACKLOG.md, not into the ADR.
- **Matching the code.** Before an ADR is set to Accepted, its rules are
  checked against the code on the commit that lands it. The code wins
  where it changed deliberately; the ADR follows and History says so.
