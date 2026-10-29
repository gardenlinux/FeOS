The Control Plane
=================

What FeRun's control plane is and how it works: `ferund`, in `go/`, and
its management UI, in `web/`. The decisions behind it:
[ADR 0015](../adr/0015-control-plane.md) (the control plane, its
replication, and how nodes connect),
[ADR 0014](../adr/0014-identity-and-credentials.md) (identity and
credentials), [ADR 0013](../adr/0013-traffic-records.md) (traffic records),
[ADR 0017](../adr/0017-pod-mesh.md) (the pods' mesh),
[ADR 0018](../adr/0018-environments-and-releases.md) (environments and
releases), [ADR 0021](../adr/0021-ingress.md) (the ingress),
[ADR 0023](../adr/0023-calls-outlive-their-caller.md) (calls that outlive
their caller), [ADR 0009](../adr/0009-node-os-and-packages.md) (packages).
The node's side of the wire is in [node.md](node.md#the-control-plane);
`ferun-ingress`, the program that serves public environments' hosts, and
ferund's side of it are in [ingress.md](ingress.md).

ferund is the successor of the sandbox system's control plane. Its REST
API, MCP and port front door keep that system's paths, bodies, status codes
and texts, which FeCode's backend and the UI were built against; what is
new is underneath: PostgreSQL instead of SQLite, replicas instead of one
process, and nodes that connect over mTLS instead of a broker.

Parts that are decided but not built are marked as such.

## Instances

`ferund` runs as identical instances over one PostgreSQL database: three
in production, one on a single machine. An instance holds no state of its
own beyond caches, its connections to nodes and the leases it holds; any
instance serves any request. Each has two listeners:

| Listener | Default | Serves | In front of it |
|---|---|---|---|
| the front doors, `--listen` | `:8100` | the REST API, MCP at `/mcp`, the ports at `/fwd/`, sign-in at `/auth/`, the management UI, `/healthz`, `/readyz` | an HTTPS balancer, without sticky sessions; or TLS of its own with `--tls-cert` and `--tls-key` |
| the nodes' port, `--node-listen` | `:7443` | node connections, uplinks and joins, and the connections of `ferun-ingress`; TLS 1.3 always | a TCP passthrough, or the instances' own addresses |

`ferund` has four subcommands:

    ferund [serve] [flags]          run an instance (the default)
    ferund init-ca --root-dir R --dir D --dns-domain X [--dns-domain Y ...]
                   [--trust-domain T] [--control-plane-domain C]
    ferund join-token --database-url URL [--ca-dir D] [--trust-domain T]
                      [--kind node|ingress] [--ttl 1h] [--node NAME]
    ferund migrate --database-url URL

Every flag has an environment variable `FERUN_<NAME>`, the flag's name in
upper snake case (`--database-url`: `FERUN_DATABASE_URL`); a flag on the
command line wins over its variable, and an empty variable counts as
unset. Durations are written in Go's syntax (`90s`, `10m`, `12h`), byte
sizes as integers with an optional `KiB`, `MiB` or `GiB` suffix.
`ferund <command> --help` lists the flags with their variables and
defaults. `serve` migrates the database at its start; `migrate` does only
that. `join-token` issues a node's token, or with `--kind ingress` one
for an instance of `ferun-ingress`; `--node` pins it to a name of that
kind ([The ingress](#the-ingress)).

### serve's flags

| Flag | Default | What it is |
|---|---|---|
| `--database-url` | required | the PostgreSQL DSN |
| `--listen` | `:8100` | the front doors |
| `--tls-cert`, `--tls-key` | none | TLS on `--listen`; without, plain HTTP behind the balancer. Loaded at the start, which fails on a missing or bad file, and read again every minute: a pair renewed on disk is served from the next handshake on, and one that does not load keeps the pair before |
| `--node-listen` | `:7443` | the nodes' port |
| `--node-advertise-address` | none | `host:port` at which nodes reach this instance's nodes' port; listed to nodes. Empty: the instance is not listed, and nodes reach it only by the name they were configured with (one machine) |
| `--instance-id` | `<hostname>-<8 random hex>` | the instance's ID; the default is new at every start. A fixed one (a StatefulSet's pod name) must be unique among the running instances: a start under an ID whose row was written within 15 s waits until the row is older, and is refused if it is written meanwhile ([Leader and instances](#leader-and-instances)) |
| `--trust-domain` | `feos.local` | the SPIFFE trust domain |
| `--ca-dir` | `/etc/ferun/ca` | the issuer: `issuer.crt`, `issuer.key`, `issuer.json`, `bundle.crt`; never `root.key` |
| `--node-server-names` | the control-plane domain | the DNS names of the instance's certificate on the nodes' port, each under the control-plane domain |
| `--secret-key-file` | none | 32 bytes (raw, hex or base64) that seal the bindings and the environments' secrets at rest |
| `--bindings` | off | serve the Kubernetes and SSH bindings; only with `--secret-key-file` |
| `--deny-prefix` | a built-in set | a destination that preflights, bindings and the image resolver may not reach, repeatable; a set given replaces the built-in one, and `default` stands for it (`default,100.144.0.0/13` adds a network) |
| `--admin-users` | none | `user:password,...` for the admin API: Basic, beside or instead of OIDC |
| `--public-url` | none | the front door's external URL, e.g. `https://run.fecode.dev`; required with `--oidc-issuer` |
| `--oidc-issuer` | none | the OIDC issuer of human sign-in, exactly as its discovery document states it |
| `--oidc-client-id` | `ferun` | |
| `--oidc-client-secret-file` | none | the client's secret; required with `--oidc-issuer` |
| `--oidc-scopes` | `openid profile email` | space-separated, with `openid` |
| `--oidc-admin-group` | `admins` | a person is an admin when the `groups` claim holds it |
| `--session-ttl` | `12h` | the absolute lifetime of a UI session |
| `--bootstrap-token` | none | becomes a token of the tenant `operator` while no tenant exists |
| `--mcp-allowed-origins` | none | the Origins `/mcp` accepts; empty refuses every request with an Origin |
| `--pod-network` | empty | the pods' network ([The pods' mesh](#the-pods-mesh)): empty, routed, every node with a prefix of its network's; `mesh`, a ULA /64 drawn at the first start and kept in the database; or such a /64, `fdXX:XXXX:XXXX::/64` (a /48 stands for its /64 of subnet 0) |
| `--apps-zone` | empty | the DNS zone of public environments' default hosts, `<label>.<zone>`, which `ferun-ingress` serves ([The ingress](#the-ingress)); empty: no hosts, and no environment can be made public |
| `--edge-name` | empty | the name tenants' domains point at with a CNAME, such as `edge.fecode.dev`, which the operator publishes with the balancer's addresses ([Domains](#domains)); empty: domains are off |
| `--edge-address` | none | an edge address beside what `--edge-name` resolves to, which a domain's A and AAAA records may name; repeatable |
| `--domain-challenge-label` | `_ferun-challenge` | the label of a claim's TXT record, `<label>.<name>`; a lowercase DNS label that may hold `_` |
| `--domain-grace` | `72h` | how long a record of a verified domain may answer wrong before the domain is deactivated |
| `--domain-unverified-max` | 50 | the most claims a tenant may hold that are not verified |
| `--domain-reserved` | none | a zone no domain may be or be under (the platform's own domain, where `run`, `id` and `edge` are), beside the apps zone and `--edge-name`; repeatable |
| `--dns-resolver` | the system's resolvers (`/etc/resolv.conf`) | the DNS resolver, `host:port`, the domains' checks ask |
| `--default-image` | `ghcr.io/maltej/debian-ai:latest` | the image of a sandbox created without one |
| `--image-bakes` | true | a create names the baked layers of its image and lets its node bake it ([Images](#images), ADR 0025); false: neither |
| `--default-vcpus`, `--default-memory-mib` | 2, 512 | the size of a sandbox created without one |
| `--placement` | `binpack` | or `spread` |
| `--disk-budget` | `4GiB` | placement's disk gate per pod |
| `--idle-pause` | `10s` | a running sandbox idle this long is paused; 0 disables |
| `--hibernate-after` | 0 | a sandbox paused this long is hibernated; 0 disables |
| `--node-ttl` | `15s` | the age of a node's telemetry before it is down |
| `--lost-grace` | `10m` | how long a node is down before its sandboxes count as lost |
| `--node-wait` | `5s` | how long a call waits for a node's connection |
| `--rollout-grace` | `30s` | how long a call waits for a node that announced its restart |
| `--drain-timeout` | `2m` | how long a draining node waits for the execs still running in its sandboxes; then they are cancelled and the sandboxes move |
| `--pending-repair-after` | `10m` | a pod pending this long without an op is repaired |
| `--unreachable-repair-after` | `2m` | a running pod whose guest stays unreachable this long is repaired |
| `--node-cert-ttl`, `--pod-cert-ttl` | `168h`, `1h` | the certificates' lifetimes; a node's is renewed at half of it, so that a node switched off for up to half of it needs nothing, a pod's at three quarters |
| `--node-max-streams` | 1024 | the HTTP/2 stream limit nodes advertise on a node connection; the stream budgets derive from it |
| `--conn-drain-timeout` | `20m` | a draining node connection is closed after this at the latest |
| `--hibernate-timeout` | `1h` | how long ferund waits for a hibernation, plus a pause's deadline and 16 s per GiB of memory; on a feosd that reports its hibernations, the node's goes on after it |
| `--shutdown-grace` | `11m` | how long a stopping instance waits for its requests, its tool calls and its ops |
| `--activity-retention`, `--traffic-retention` | `720h`, `168h` | how long activity and traffic records are kept |
| `--log-level` | `info` | |

Fixed timings, not flags (`internal/config`): an instance's heartbeat every
5 s, live for 15 s; the leader lease 15 s, renewed every 5 s; a sandbox's
preparation before a tool call at most 7 min 5 s (REST advertises
`preparationWaitMs: 425000`); telemetry every 5 s; one node's inventory
reconcile at most every 30 s; holds leased for 30 s and renewed every
10 s; a keyed tool call's lease 30 s, renewed every 10 s; its record kept
1 h; a keyed `bash` at most 600 s ([Calls that outlive their
caller](#calls-that-outlive-their-caller)); join tokens valid 1 h when no
lifetime is given; the instance's certificate on the nodes' port 24 h;
an ingress's older certificates valid 10 minutes after it presented a
newer one, its holds leased for 30 s, its Sync stream's heartbeat every
10 s ([The ingress](#the-ingress)).

## Packages

`go/` is the module `github.com/gardenlinux/feos/go`. The generated code
of `proto/` is in `go/gen/feos/<api>/v1` (`make go-gen`), the rest in
`go/internal/`:

| Package | What it is |
|---|---|
| `config` | the flags and their variables, the fixed timings |
| `db` | the connection pool, transactions with retries, the embedded schema and its migration, notifications |
| `store` | typed queries over the schema, one file per domain |
| `model` | value types and errors that several packages share |
| `activity` | ferund's rows of the activity log, written in batches; the partitions of the log |
| `pki` | the CA: issuer, certificates, signing requests, SPIFFE IDs, join tokens and pins, HPKE |
| `mesh` | the pods' mesh: the ULA, a node's /96, endpoints, the peers' deltas |
| `nodelink` | the nodes' port: TLS, ALPN, the node connections and their budgets, the uplink, join and refusal servers |
| `fleet` | `feos.fleet.v1`: joins, renewals, telemetry, events, pods' credentials, the mesh, the nodes' liveness; the mesh's peers to the nodes |
| `leader` | the leader lease and the leader's loops, the instance's heartbeat |
| `sandbox` | the sandbox engine: states, ops, placement, sync, reconcile, tombstones, policies, images |
| `tools` | the agent's tools in a sandbox, their REST routes, and the calls that outlive their caller |
| `mcpsrv` | the MCP door |
| `ports` | the port door |
| `watch` | file watches across instances |
| `bindings` | Kubernetes and SSH bindings |
| `seal` | AES-256-GCM sealing under `--secret-key-file`'s key: the bindings' credentials, the environments' secrets |
| `env`, `env/spec`, `env/addons`, `env/ctl`, `env/envdeps` | the environments ([Environments](#environments)) |
| `ingress/service`, `ingress/table` | ferund's side of the ingress: the API `ferun-ingress` calls, its identity, the table it serves from ([The ingress](#the-ingress)) |
| `domains` | tenants' domains: claims, bindings, their checks, their states and incidents ([Domains](#domains)) |
| `ingress/wire`, `routing` | shared with `ferun-ingress`: the ingress's protocol; path matching, which the route door uses too |
| `api`, `auth`, `apierr` | the REST router, its auth classes and errors, human sign-in |
| `ui` | the embedded build of `web/` |
| `server` | one instance, built from its configuration, and its stop |
| `testnode` | a Go implementation of feosd's side of the wire, for tests and development |
| `e2e` | end-to-end tests: several instances and test nodes on one database |

`cmd/ferund` is the daemon; `cmd/ferun-testnode` runs a test node
([below](#the-test-node)); `cmd/ferun-ingress` is the ingress, with its
packages under `internal/ingress` ([ingress.md](ingress.md)).

## The database

The state is in PostgreSQL, 17 or newer: the schema declares identity
columns on partitioned tables, and `migrate` refuses an older server with
"ferund needs PostgreSQL 17 or newer". A database with replicas must
replicate synchronously ([Operations](#operations)).

- **Sessions.** Every connection of the pool runs with `lock_timeout`
  5 s, `statement_timeout` 30 s and the time zone UTC. Connections dial
  with TCP keepalive (after 5 s idle, every 5 s, 3 probes) and
  `TCP_USER_TIMEOUT` 15 s, and ask the server for the same
  (`tcp_keepalives_*`, `tcp_user_timeout`), and for
  `idle_in_transaction_session_timeout` 60 s: a connection to a server
  that vanished without a reset fails within about 20 s and the pool and
  the listener connect again, to the new primary behind the same name;
  the server ends the session of an instance that vanished,
  mid-transaction too, and frees its locks (a stranded share lock on the
  lease row would keep every instance from leading). The server's
  WARNING notices are logged, at most one per 10 s: among them a commit
  whose wait for its synchronous standby was cancelled
  (`statement_timeout`), done on the primary but perhaps not replicated.
- **Transactions** run at READ COMMITTED with explicit row locks.
  Each statement sees its own snapshot, so a check that must see what
  another transaction committed while this one waited for a lock is a
  separate, later statement. A transaction that fails with a
  serialization failure, a deadlock or a lock timeout runs again, up to
  five times with jittered backoff. Nothing slow, no call to a node, runs
  inside a transaction.
- **Time.** Every lease, TTL, grace and deadline is decided on the
  database's clock (`now()`), never on a node's or an instance's.
- **Migrations** are embedded (`internal/db/migrations/`: `0001_init.sql`,
  the schema; `0002_auth.sql`, sign-in;
  `0003_policy_backoff_and_placements.sql`, the policies' backoff
  ([Policies](#policies)) and an empty `placements`;
  `0004_traffic_node_foreign.sql`, the partial index of a node's foreign
  traffic records ([Nodes](#nodes)); `0005_mesh.sql`, the pods' mesh;
  `0006_environments.sql`, the environments' tables and five columns of
  `sandboxes` ([Environments](#environments)); `0007_names.sql`, the
  incident `names_unserved` ([Names](#names)); `0008_pod_ids.sql`, the
  sandbox IDs' columns from `uuid` to `text` and the drop of `placements`
  ([Sandbox IDs](#sandbox-ids)); `0009_tool_calls.sql`, the table
  `tool_calls` of the keyed tool calls ([Calls that outlive their
  caller](#calls-that-outlive-their-caller)); `0010_ingress.sql`, the
  ingress's tables and its columns of `pod_network`, `join_tokens`,
  `nodes`, `env_instances` and `environments` ([The
  ingress](#the-ingress)); `0012_domains.sql`, the domains and their
  incident conditions ([Domains](#domains))) and
  applied under an advisory lock, recorded in `schema_migrations`. The
  same locked section creates the cluster's MCP session key, reconciles
  `--pod-network` with the database's ([The pods'
  mesh](#the-pods-mesh)) and, with `--bootstrap-token`, creates the tenant
  `operator` and its token, when no tenant exists.
- **Notifications.** One connection per instance listens on the channels
  below and fans them out in the process. A writer sends its notification
  in the transaction that made the change, so it arrives at commit. After
  the listener reconnected, every subscriber gets an empty payload: "you
  may have missed something, read again". Waiting for another instance's
  write (`db.WaitFor`) always subscribes first, then reads, then waits for
  a notification or 500 ms, and reads again.

| Channel | Payload | Sent when |
|---|---|---|
| `ferun_sandbox` | a sandbox ID, or `*` for more than 256 | a sandbox's state, op, node, incarnation or checkpoint changed, or it was created or deleted |
| `ferun_node` | a node name | its certificates were revoked or superseded, it was removed, its identity forked, its restart was announced, its draining changed |
| `ferun_reconcile` | a node name | an instance not connected to the node asks one that is to reconcile it |
| `ferun_leader` | a hint | the leader's loops have work: the hint wakes those whose name contains it (`mesh-peers`, `env-reconcile`, `env-groups`, `env-names`, `env-delete`), every loop when none does or the hint is empty or `*` |
| `ferun_watch` | a watch ID | a watch's paths, owner or result changed |
| `ferun_cancel` | JSON | an exec, an MCP call or a keyed call (ADR 0023) is to be cancelled on whichever instance runs it |
| `ferun_call` | a tool call's ID (`tool_calls.id`), or `*` for more than 256 | a keyed call left running: done, interrupted, not run |
| `ferun_ingress` | `env:<uuid>`, `ingress:<name>`, `mesh`, or `*` | what the ingress's table shows changed: an environment's publicity, label, releases, deletion (its tenant's too), service instances or their admitted ports, a domain's binding or state (the environment it is or was bound to); an ingress instance's row or certificates; the mesh's peers; a node's admitted version of `ferun-ingress`, or more than 256 environments (`*`) |

The tables: `instances`, `leases`, `cluster_secrets`; `tenants`,
`tenant_tokens`; `nodes`, `node_telemetry` (the hot row, about a hundred
updates a second at 512 nodes, never locked by placement), `node_certs`
(every node certificate issued), `join_tokens` (of kind `node` or
`ingress`), `node_starts`, `event_gaps`; `pod_network` (the mesh's ULA,
its peers' version and the version of the group `ferun-ingress`, one
row while the cluster is meshed); `ingresses` (the instances of
`ferun-ingress`, never deleted), `ingress_certs` (every ingress
certificate issued), `host_labels` (every default host label ever given,
and its tenant); `sandboxes`, `sandbox_tokens`, `sandbox_holds`,
`pod_tombstones`, `bookings`; `tool_calls` (the keyed tool calls, [Calls
that outlive their caller](#calls-that-outlive-their-caller));
`image_bakes` (the baked layers of images, [Images](#images)),
`sandbox_image_credentials` (a sandbox's sealed image credentials);
`activity` and `traffic_records`, partitioned by day; `issued_certs`
(pods' certificates); `watches`;
`kube_bindings`, `ssh_bindings`; `auth_logins`, `ui_sessions`;
`environments`, `env_releases`, `env_tags`, `env_secrets`,
`env_instances`, `env_job_runs`, `env_attached`, `env_volumes`,
`env_incidents`, `env_route_tokens` ([Environments](#environments));
`domains` ([Domains](#domains)).
Tokens are stored as SHA-256 hashes only, and shown once, at their
creation.
`placements`, which held the placement decisions and was written and read
by nothing since `0003` emptied it, was dropped by `0008`. A placement's
audit is its activity row.

## Leader and instances

- **Instances.** Every instance writes its row in `instances` every 5 s.
  An instance is live while its row is younger than 15 s and not marked
  stopping. The leader deletes rows not seen for an hour.
- **The start** claims the instance's ID before anything serves, in one
  transaction: its row is written anew (started now, not stopping), and
  the ops the ID still owns are released as in a stop's step 4, since no
  process runs them: they are an earlier run's that ended without its
  stop, crashed or killed. Their sandboxes say "op <op> of instance <id>
  was released: the instance started again". The keyed tool calls the ID
  still runs end in the same transaction, interrupted when they reached
  the guest and not run otherwise ([Calls that outlive their
  caller](#calls-that-outlive-their-caller)). A row younger than 15 s,
  stopping or not, is another process's under the same ID, or a run's
  that ended less than 15 s ago: the start tries again every second until
  the row is older, and is refused when the row is written meanwhile.
- **The lease.** One instance at a time leads, by the row `leader` of
  `leases`: a TTL of 15 s and an epoch that grows by one on every
  acquisition, also by the holder of an expired lease. The epoch is the
  fencing token. The leader renews every 5 s and steps down at once when a
  renew fails or finds the lease gone, or 13 s after it sent the last
  renew that succeeded, so that it never acts on a lease another instance
  may hold already. The leader's bulk transactions take a share lock on
  the lease row in their epoch first and touch at most 256 rows.
- **Loops.** While it leads, an instance runs the loops of every package,
  each on its interval and when `ferun_leader` wakes it, one run at a
  time; an error is logged and the loop runs again.

| Loop | Interval | Does |
|---|---|---|
| node expiry | 5 s | marks a node down whose telemetry is older than `--node-ttl`; only while the leader has led for `--node-ttl` and the database has not failed for as long |
| reconcile sweep | 60 s | reconciles every ready node the leader is connected to |
| join-token expiry, fleet retention | 60 s, 1 h | expired join tokens; old feosd starts, event gaps, node and pod certificates |
| mesh peers | 15 s | sends every node on the pods' mesh that the leader is connected to its peers, the ingress's instances among them ([The pods' mesh](#the-pods-mesh)) |
| down sweep | 5 s | sandboxes of down and removed nodes become `unknown` |
| lost grace | 30 s | `unknown` sandboxes of nodes down past `--lost-grace` become `hibernated`, `lost`, or `pending` when their pod was never seen |
| idle pause | 1 s | pauses running sandboxes idle for `--idle-pause` (a row's own `idle_after_s` when set; 0 never), unless a failed pause backs off; only while the leader has led for 15 s and the database has not failed for as long (every instance renews its holds first); never a public environment's service instance while an ingress instance may be cut off (the [idle fence](ingress.md#the-idle-fence)) |
| hibernate-after | 5 s | hibernates sandboxes paused for `--hibernate-after`, unless a failed hibernation backs off |
| drain | 5 s | moves sandboxes off draining nodes |
| tombstones | 10 s | deletes pods that belong to no sandbox |
| stale ops | 10 s | takes over or releases the ops of dead or stopping instances; only under the same condition |
| unknown sweep | 15 s | syncs `unknown` sandboxes on connected nodes |
| repair | 30 s | repairs pods stuck pending or with an unreachable guest |
| pending re-description | 30 s | tells a sandbox waiting for capacity that there is some |
| process holds, expired holds | 60 s | releases holds of processes that ended, and expired leases; expired holds only under the same condition |
| tenant deletion | 60 s | finishes deleting tenants |
| watch expiry | 60 s | deletes watches past their lease |
| `tool-calls-expiry` | 5 s | marks running keyed calls whose lease passed: interrupted if they reached the guest, not run otherwise; only while the leader has led for a call lease (30 s) and the database has not failed within it. Waiters mark the call they wait on themselves after two leases ([Calls that outlive their caller](#calls-that-outlive-their-caller)) |
| `tool-calls-retention` | 60 s | deletes keyed calls that ended more than an hour ago |
| stale instances | 60 s | deletes instance rows older than an hour |
| activity partitions | 1 h | keeps partitions for today and the next two days; drops those past retention |
| sign-in clean-up | 10 min | deletes stale sign-ins and expired UI sessions |
| `env-reconcile`, `env-probe`, `env-groups`, `env-volumes`, `env-delete` | 2 s, 1 s, 10 s, 30 s, 5 s | the environments' controller ([Environments](#the-controller)) |
| `domains-check`, `domains-settle` | 5 s, 30 s | checks the domains that are due; settles the states that changed without a write of the domain's own; only while domains are on ([Domains](#domains)) |

An outage or a stall of the database lets the first holds expire 20 s
in (a hold's lease is 30 s, renewed every 10 s) and every hold after
30 s. A stall ends the leader's term 8 to 13 s in: 13 s (the lease's
15 s less the 2 s margin) after the send of the last renew that
succeeded, which went out 0 to 5 s before the stall. No term outlives a
stall that let a hold expire, so the next term begins after it, and its
15 s are a hold's renewal interval (10 s) and 5 s, by which every live
instance has renewed its holds and its heartbeat. An instance's row can
turn stale 10 s into a stall (written every 5 s, live for 15 s), and a
term, or its acquisition, can span a stall of up to 13 s: after a stall
of 10 to 13 s, stale ops may take over an op of an instance that still
runs, as it does for one cut off from the database; such a takeover is
safe, only redundant.

### Stopping

An instance stops on SIGTERM or SIGINT so that a rolling deploy cuts no
request, tool call or op:

1. Its row says stopping, `/readyz` fails, the leader resigns its lease.
   It begins no work of its own initiative any more, but still begins the
   ops that requests need: a request in the readiness window that needs a
   resume does not fail. From now on other instances may take its ops
   over. The ingress's side stops too: the nodes' port closes every new
   `ferun-ingress/1` connection before any check, the Sync streams end
   with `UNAVAILABLE` "instance stopping", and Wake and Hold answer
   `UNAVAILABLE`; the ingress's instances move to another instance
   ([The ingress](#the-ingress)).
2. It serves on for 5 s, so that balancers see `/readyz`, then stops
   accepting requests and waits for those in flight, until
   `--shutdown-grace`. From now on a keyed tool call reaches a guest only
   if its timeout and 30 s fit before that deadline (otherwise it ends
   not run, and its client sends it to another instance), a new keyed
   call is refused as not run unless its key names a call that ran or
   runs, and requests waiting on another instance's call are told to ask
   again ([Calls that outlive their
   caller](#calls-that-outlive-their-caller)). The watches' event streams
   end at once, and their clients reconnect elsewhere; upgraded port
   connections (WebSockets) are closed once no request is left.

   **2b.** It waits for the keyed calls whose callers left, until the
   same deadline; those still running then are cancelled and recorded:
   interrupted, with their output so far, when they reached the guest,
   not run otherwise.
3. The engine refuses new ops ("instance <id> is stopping") and waits for
   its own, until the same deadline.
4. The ops still in flight are released: their sandboxes become `unknown`
   and another instance resolves them.
5. Only then the node connections close, the nodes' port stops, the row is
   deleted and the pool closed.

The process therefore takes up to `--shutdown-grace` plus a little to
end; whatever stops it must wait at least that long plus 60 s, which
cover step 1 (10 s), the readiness delay (5 s), the cut calls' final
writes (15 s) and step 5 (2 × 10 s), with room for the ops' release
([Operations](#operations)).

## The nodes' port

One TLS 1.3 listener, routed strictly by ALPN:

| ALPN | Client certificate | Goes to |
|---|---|---|
| `feos-node/1` | the node's | a node connection: the node is the HTTP/2 server of its `feos.*` APIs, the instance the gRPC client |
| `h2` | the node's | the uplink server: `feos.fleet.v1` without Join |
| `h2` | the node's, definitively refused | the refusal server: every call `PERMISSION_DENIED` |
| `h2` | none | the join server: Join alone |
| `ferun-ingress/1` | an ingress instance's | the ingress server: `ferun.ingress.v1.IngressService` without Join ([ingress.md](ingress.md#the-connection)) |
| `ferun-ingress/1` | an ingress instance's, definitively refused | the ingress server as a refusal: every call `PERMISSION_DENIED` with the refusal's text |
| `ferun-ingress/1` | none | the ingress join server: Join alone |
| anything else, or none | | closed |

- **Accept.** Every TCP connection gets keepalive (15 s) and a
  `TCP_USER_TIMEOUT` of 20 s. At most 512 handshakes run at once, each with
  a 10 s deadline. An EOF before the ClientHello is a balancer's probe and
  is not logged; failures a remote party can cause are logged at most once
  a second per kind, with a count of the suppressed.
- **The server certificate.** Each instance issues its own from the
  issuer at its start: ECDSA P-256, 24 h, for `--node-server-names` and
  the URI `spiffe://<trust domain>/ferun`, issued again at three quarters
  of its life. The chain it shows ends with the root, so that a joining
  node can check it against its token's pin. Session tickets are off:
  every connection presents its current certificate.
- **The node's identity** is its certificate's one URI,
  `spiffe://<trust domain>/node/<name>`, never what it sends. The
  certificate is checked against the database at accept and on every
  re-check, never from a cache (at most 32 checks at once; a check waits
  at most 5 s for a slot): its serial must exist and not be revoked, the
  node neither removed nor forked. A certificate that passes at accept
  has its presentation recorded ([certificate lineage](#nodes)) in the
  same slot, so that a node that redials as fast as it can never has more
  database work under way at accept than the 32 slots. A definitive refusal
  (unknown serial, revoked, removed, forked) refuses the connection; any
  other error, such as a database failover, refuses a new connection but
  never closes one that is open: its re-check is retried with backoff. A
  connection is closed when its certificate expires.
- **Node connections.** The instance dials gRPC over the TLS connection
  it accepted, with keepalive pings every 10 s (5 s timeout) also without
  streams, no idle timeout and no service config, and registers it only
  once it is READY (within 10 s). A node may have several connections to
  one instance, after a renewal or a reconnect: the **current** one, the
  READY connection with the newest certificate, takes every new call;
  the others **drain**: their calls finish, and they are closed when idle
  or after `--conn-drain-timeout`, but never while an op's lifecycle call
  runs on them. A current connection that leaves READY (a GOAWAY, a dead
  transport) drains like the others.
- **Calls to a node.** `Conn` returns the current connection; without one
  it waits up to `--node-wait`, or up to the node's rollout deadline when
  it announced a restart, then fails with `UNAVAILABLE` "node <n> is not
  connected to instance <i>". The rollout deadline is the grace the
  database measures as left (`stopping_until - now()`) when the instance
  reloads it, counted from then on the instance's monotonic clock: an
  instance whose clock is off the database's waits as long as one whose
  clock is not. A lost connection is what tells an instance to fail calls
  for the node fast; it changes nothing in the database. A call into a
  pod's guest carries `feos-pod-id`.
- **A node that stops** sends GOAWAY on its node connections, lets the
  calls in flight end up to its bound, and ends the rest with
  `UNAVAILABLE` "the node stops: …" ([node.md](node.md#stopping)). A
  call, unary or a stream (an exec, a port forward, a watch), that had
  not reached the node (no stream opened on the connection,
  which a stats handler on the channel tells) and failed `UNAVAILABLE` on
  a connection that left READY goes to the node's next current
  connection, waiting as `Conn` does, up to the rollout deadline; before,
  a call that took the connection just before its GOAWAY failed with
  "the node's connection is gone". A call that had reached the node and
  whose connection ended without the node's status (an EOF, a reset)
  reads "node <n> stopped during the call: <error>" when the node had
  announced its stop, and "node <n>: the connection ended during the
  call: <error>" otherwise; the node's own status, as "the node stops",
  is as it sent it.
- **Stream budgets**, per connection, of `--node-max-streams` N: long
  streams (exec, watch, port forward, console, traffic records, host
  streams) at most N/2, unary tool calls at most N/4; beyond them a call
  fails at once with `RESOURCE_EXHAUSTED`. Lifecycle calls, `GetPod`,
  `ListPods`, `DeletePod` and the ops' pod event streams are not counted,
  and always find the last quarter free. Per sandbox and instance: 32
  forward streams (a request waits up to 30 s for one), 16 watch streams,
  64 exec streams.
- **The uplink server** takes at most 4 uplinks per node; it allows
  pings every 5 s without streams, 16 streams and 2 MiB messages per
  connection. An uplink whose certificate was superseded takes no new
  calls and is closed once idle.
- **The join and refusal servers** share one limit of 64 connections, each
  closed after 60 s. The refusal server tells a removed, revoked or forked
  node that it has to join again: "node <n> was removed; join it again",
  "certificate of node <n> was revoked (<reason>); join it again",
  "identity of node <n> forked; join it again with a token pinned to its
  name".
- **Re-checks.** `ferun_node` makes every instance re-check the node's
  connections and reload its rollout deadline; after the listener
  reconnected, every node's, spread over 5 s.
- **Ingress connections** (ALPN `ferun-ingress/1`, [The
  ingress](#the-ingress)) take the instance's identity from the
  certificate's one URI, `spiffe://<trust domain>/ingress/<name>`, and are
  checked against the database at accept and on every re-check, in the
  same 32 slots as the nodes' (`CheckIngress`): an unknown certificate, a
  removed ingress, a revoked or an expired certificate are definitive
  refusals, a superseded one is refused once its revocation time has
  passed and closed at that time before; any other error refuses a new
  connection only. At most 4 connections per ingress name; an ingress's
  join and refusal connections count against the join and refusal
  servers' limit of 64, and close after 60 s as theirs do. `ferun_ingress`
  `ingress:<name>` re-checks the name's connections on every instance.
  A stopping instance closes new ingress connections at once.

## The CA

The platform CA ([ADR 0014](../adr/0014-identity-and-credentials.md)) has
one root, kept offline, and one intermediate, the **issuer**, which every
instance holds in `--ca-dir`. The issuer is the only CA key on an
instance: it signs node, ingress and pod certificates and the instances'
own certificates on the nodes' port. With the database's credential and the
at-rest key of the bindings, it is one of the control plane's three
secrets.

- **`ferund init-ca`** makes both. The root: ECDSA P-256, 10 years, path
  length 1, written to `--root-dir` (`root.crt`, `root.key`). The issuer:
  ECDSA P-256, 5 years but not past the root, path length 0, written to
  `--dir` with `bundle.crt` (the root) and `issuer.json` (the
  control-plane domain, `ferun.<first --dns-domain>` by default, and the
  trust domain). Both carry the same critical name constraints: DNS names
  only under `--dns-domain`, no IP address. No URI constraint (ADR 0024):
  rustls-webpki, which `feosd` checks the control plane with, refuses
  every URI name beneath one. The two directories must differ, and no
  file is ever overwritten.
- **`ferund new-issuer`** makes a new issuer under the root in
  `--root-dir` (`root.crt`, `root.key`), with the same flags as
  `init-ca`, into a new `--dir`, and loads it before it returns. The
  root, the bundle and the join tokens' pin stay. It replaces an issuer
  that constrains URIs, as `init-ca` made them before ADR 0024, and an
  issuer near its end ([The CA ceremony](#the-ca-ceremony)).
- **At start**, `serve` verifies the issuer's chain to a root of the
  bundle, requires the trust domain of `issuer.json`, when it names one,
  to be `--trust-domain` (an issuer of before is bound by its URI
  constraint instead), the control-plane domain to lie within the
  issuer's DNS domains and every `--node-server-names` under it, and
  refuses to start when `root.key` is in `--ca-dir`. It warns when the
  issuer constrains URIs: nodes of this version refuse the nodes' port's
  certificate under such an issuer.
- **Separating pods from the control plane.** The name constraints keep
  every certificate inside the platform's domains and away from IP
  addresses, but the control-plane domain lies beneath the same platform
  domain, and no name constraint tells a pod's URI from the control
  plane's. The separation is therefore in software, on two lines: issuance
  (a pod never gets a wildcard, a name in the control-plane domain, the
  control plane's URI or an ID under `/ingress`; a node and an ingress
  instance get only their URIs; only the instances'
  server certificates carry `spiffe://<trust domain>/ferun`), and the node,
  which requires that URI on the control plane's certificate beside the
  DNS name it was configured with. A pod's certificate of the same issuer
  fails that check. A second, server-only intermediate would make the
  split a name constraint, at the price of a second CA key on every
  instance.
- **Issuance.** Serials of 128 random bits; `NotBefore` ten minutes back,
  since nodes may boot before their clock is set; never an IP address,
  never a URI outside the trust domain; a signing request's own names are
  ignored. Nodes: client auth, `spiffe://<td>/node/<name>`, Ed25519 or
  ECDSA P-256, `--node-cert-ttl`. Instances of `ferun-ingress`: client
  auth, `spiffe://<td>/ingress/<name>`, the name as CN, Ed25519 or ECDSA
  P-256, `--node-cert-ttl`. Pods: client and server auth, the sandbox's
  SPIFFE ID `spiffe://<td>/tenant/<tenant>/sandbox/<id>`, Ed25519,
  `--pod-cert-ttl`. Each says when its holder renews it
  (`renew_after`): a node's and an ingress instance's at half its
  lifetime, so that one switched off at any time keeps at least half of
  it; a pod's at three quarters.
- **Join tokens.** A token is 32 random bytes. The node sends
  `feos-join-<secret>`; the operator gets `feos-join-<secret>.<pin>`, where
  the pin is the SHA-256 of the root that anchors the issuer (its
  SubjectPublicKeyInfo). The database keeps only the hash of what the node
  sends. A token has a kind, `node` or `ingress`, and joins only its kind;
  it may be pinned to a name of its kind. A token of kind `ingress` has a
  form of its own, `feos-ingress-join-<secret>.<pin>`, so that a node's
  Join refuses it by its form before any lookup, also in a `ferund` that
  knows no kinds (during a rollout, or after a rollback). The node checks, before it
  sends the token, that the server's chain holds the pinned root, names
  the configured DNS name and carries the control plane's URI, and takes
  the join's answer only if its trust bundle holds that root.
- **Sealed bindings.** A pod's bindings travel sealed to its run's HPKE
  key: DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, ChaCha20-Poly1305, info
  `feos bindings v1`, the pod's ID and the bindings' version as associated
  data.

## Nodes

`fleet` serves `feos.fleet.v1`, the control plane's API for nodes. A node's
row is never deleted: removal is a status. Its statuses: `joined`, `ready`
(it reports), `down` (its telemetry aged past `--node-ttl`), `removed`.

- **Join.** The token's form, the name (a DNS label) and the signing
  request (at most 16 KiB) are checked first; at most 10 joins a second,
  20 in a burst, and 4 at once in the database, per instance; a token in
  an ingress's form is `INVALID_ARGUMENT` "this join token is for an
  ingress". Then, in one transaction: an unknown token is refused, an
  expired one too, and one of kind `ingress` ("this join token is for an
  ingress"). An
  unpinned token creates a new name only: a name that exists, whatever
  its status, gets `ALREADY_EXISTS` "node <name> exists; join it with a
  token pinned to its name". A pinned token is the operator's intent to
  join that name again, for a node that lost its key: it revokes every
  earlier certificate of the name (`rejoined`), clears a removal and a
  fork, and the old connections close on every instance. A used token is
  answered again only when the same key repeats it within 10 minutes and
  the certificate of its first answer is still the node's and not revoked,
  the node neither removed nor forked: the first answer was lost.
  Otherwise "join token already used".
- **Renewal.** `RenewCertificate` checks the presenting certificate in its
  transaction as at accept, issues for the request's key, which may be
  new, and records the predecessor. `GET
  /api/v1/admin/nodes/{node}/certs` shows the history.
- **Certificate lineage.** When a newer certificate of a node is first
  presented, every older one is revoked one superseded grace ahead,
  `--conn-drain-timeout` plus 10 minutes: the older connections take no
  new calls and drain, and are closed at that time at the latest. A node
  that renewed dials new connections with its new certificate at once. A
  superseded certificate presented in a new handshake after its grace
  means two holders use the node's identity: the node is marked forked,
  all its certificates are revoked, and it must join again with a pinned
  token. An old connection of the node's own, re-checked after the grace,
  is only closed. Almost every accept presents a certificate seen before
  and not newer than the current one: one read without a lock tells so,
  and nothing is written. A presentation that changes the lineage is
  recorded under the node's row lock, at most 4 at once per instance.
- **Instances for nodes.** Join and every telemetry answer list the
  instances with a `--node-advertise-address` whose row is younger than
  15 s, stopping ones included: a stopping instance stays listed until its
  last step, so nodes keep their connections while it finishes. A node
  keeps a node connection to each, and dials the name it was configured
  with only to join and while the list is empty.
- **Telemetry**, every 5 s, is the heartbeat: the receiving instance
  writes the database's time into `node_telemetry`, the figures beside it,
  and the node's static facts (CPU, memory and disk totals, versions, the
  object store and its ID, the pods' prefix) into its row when they
  change. Strings are sanitised, too-large numbers clamped. A `joined` or
  `down` node becomes `ready`. A new feosd start clears an announced
  restart and requests a reconcile.
- **Inventory hash.** The node reports a hash of its running and suspended
  pods; the registry's hash of the node uses the same formula over the
  sandboxes it holds there. A mismatch that persists for 10 s requests a
  reconcile, unless the last reconcile saw the same node hash already (a
  pod ferund does not own, made with `feosctl`, keeps the hashes apart
  for good).
- **Reconcile requests** (a new node connection, a persistent mismatch, a
  new start, a pod event for a sandbox the registry has on another node, a
  sync that cannot reach the node, the listener's reconnect) are claimed
  in the node's row at most every 30 s, by an instance connected to the
  node. An instance that is not sends `ferun_reconcile`.
- **Events.** `PublishEvents` takes batches of at most 512 events and
  1 MiB, numbered densely per feosd start. A malformed batch ends the
  stream with `INVALID_ARGUMENT`. Content is sanitised, never refused: an
  audit record's JSON that does not parse is kept as text, and an event
  this ferund does not know (from a newer feosd) is stored as an `unknown
  event` and acknowledged. In one transaction, under the lock of the
  start's row, events already stored are dropped, the rest stored; a
  number neither stored nor counted as dropped by the node is recorded as
  a gap. Only then the acknowledgement goes out; a storage failure ends
  the stream with `UNAVAILABLE` and acknowledges nothing. Pod events go to
  `activity`, audit records too (source `authn`), traffic records to
  `traffic_records`, node events (`started`, `stopping`) to `activity`; a
  `stopping` sets the node's rollout deadline, `--rollout-grace` ahead
  on the database clock, during which calls wait for it ([calls to a
  node](#the-nodes-port)). A record about a pod is attributed to its
  sandbox only when the pod is or was on that node; otherwise it is kept
  for the node's audit as foreign, and a partial index of the foreign
  records serves the node's activity a page at a time.
- **Pods' credentials.** `IssuePodCredentials` issues a pod's certificate
  when the sandbox is placed on the asking node and has a run there
  (creating, running, resuming), at most 5 per pod per minute
  (`RESOURCE_EXHAUSTED` beyond), and seals the sandbox's bindings to the
  run's HPKE key. A request repeated with the same key is answered again.
- **Liveness.** A node is marked down by the leader's node expiry, never
  on one missed report, and only while the leader is healthy: after an
  outage of the control plane or a database failover, nodes have a full
  TTL to report again. Keep `--node-ttl` at least three telemetry
  intervals plus a failover.

### The pods' mesh

Where the network routes no prefix to the nodes, their pods' IPv6 goes
over a WireGuard mesh between them
([ADR 0017](../adr/0017-pod-mesh.md)); the node's side is in
[node.md](node.md). The control plane gives every node its /96 and sends
every node the others as its peers.

- **The ULA.** `--pod-network mesh` draws a /64, `fd`, a Global ID of
  40 random bits and subnet ID 0, when the database has none, and keeps it
  in `pod_network`; `--pod-network fdXX:XXXX:XXXX::/64` keeps that one,
  and a /48 stands for its /64 of subnet 0. Migration `0011` moved a
  database meshed on a /48 to that /64, every node and ingress keeping
  its number, and counted the peers' and `ferun-ingress`'s versions up.
  Every later start keeps the database's: an instance whose
  `--pod-network` is empty while the database is meshed, or names another
  /64, does not
  start ("the database's cluster is meshed on …"). A cluster does not
  move between a routed network and a mesh, nor between meshes.
- **`JoinMesh`**, on the uplink, takes the node's name from its
  certificate, which must pass the checks of every uplink call; an unknown
  or removed node gets `PERMISSION_DENIED`. In a routed cluster the answer
  is empty, and a request without a key, from a node with a prefix of its
  own, gets the ULA alone. Otherwise the key must be 32 bytes and the
  endpoint an IP address and a port, an IPv6 address in brackets, neither
  unspecified, multicast, IPv4-mapped nor with a zone, and the key not a
  point of small order, all zeros among them (`INVALID_ARGUMENT`). A key
  is one peer's: a new key that another node or an ingress instance holds,
  removed ones aside, is `FAILED_PRECONDITION`, checked under
  `pod_network`'s row lock, which every writer of a new key takes after
  its own row's and before it checks, since feosd keeps one device peer
  per key and two peers with one key would take each other's prefix.
  Under the node's row lock, a node without a
  subnet ID gets the next of the sequence `mesh_subnets`, 1 to 2^32 - 1,
  which never hands one out twice (`RESOURCE_EXHAUSTED` once it ends), and
  its /96 is the ULA with the ID in bits 64 to 95; the same node gets the
  same one on every call, computed anew from the ULA, so that a row of a
  /48 gets its /96. The key and the endpoint are stored beside it,
  in `nodes`.
- **The peers' version.** Every change of who the peers are counts up
  `pod_network.peers_version` in its transaction and sends the hint
  `mesh-peers` on `ferun_leader`: a node's first subnet, a new key or
  endpoint, a peer's removal, and a pinned re-join, which takes the node
  off the mesh until it asks again, for a new /96. A removed node keeps
  its subnet ID, which is never given to another. Down nodes stay peers.
- **Distribution.** The leader's loop `mesh-peers` reads the peers and
  their version in one statement, every 15 s and when the hint or a
  node's first connection to the leader wakes it, and calls
  `feos.network.v1.MeshService` on the node connection of every connected
  node that is a peer and has not taken this version: `PutMeshPeers` with
  the others whole, after the node's connection came up and in a new term,
  or `UpdateMeshPeers`, the peers added, changed and removed since the
  version it took in this term; a delta refused with
  `FAILED_PRECONDITION` is followed by the peers whole. At most 16 calls
  run at once, each for at most 10 s, never in a transaction; a failure
  is logged and the node is tried again on the next run. A feosd that
  answers `UNIMPLEMENTED` is logged once and not asked again on its
  connection. What the leader sent lives in its memory only: a node's last
  connection to it gone, it is forgotten.
- **Ingress peers.** The instances of `ferun-ingress` are the first peers
  that are not nodes ([ADR 0021](../adr/0021-ingress.md)): each takes a
  /96 of the same sequence at its first Hello after its first join, keeps
  it across joins, and uses `<prefix>::1`. The peers are the nodes and
  the ingress instances that are not removed and have a subnet, a key and
  an endpoint, an instance under the name `ingress.<name>`, so that
  `feosd`, which takes peers of a node's length, needs nothing new. An instance's
  first subnet, a new key or endpoint, a re-join and a removal count the
  peers' version up. A change of the peers also sends `ferun_ingress`
  `mesh`, and the ingress gets the nodes in its table
  ([ingress.md](ingress.md#the-mesh)). An instance's WireGuard key is
  fixed from its first Hello after a join; a join again clears it and
  refuses it at the next Hello, so that every join brings a new key, and
  no other peer may hold it.
- **The admin API** shows a node's /96 and endpoint as `meshPrefix` and
  `meshEndpoint`, and an ingress instance's in `GET
  /api/v1/admin/ingresses`.

## Sandboxes

A sandbox is a row in `sandboxes`; its pod on a node has the sandbox's ID
as its pod ID. The row is the truth every instance acts on; nothing about
a sandbox lives in an instance.

### Sandbox IDs

A sandbox's ID, which is its pod's ([ADR 0019](../adr/0019-pod-ids.md)),
is 11 characters of lowercase Crockford base32, the first a letter
(`^[a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{10}$`, `internal/podid`). ferund
mints every one from the cryptographic random source: a tenant's sandbox
at its create, an environment's instance at its insert. They are unique
over all tenants, the primary key of `sandboxes`; an insert whose drawn
ID is taken draws another (`CreateMintedSandbox`). A client names none:
`POST /api/v1/sandboxes` refuses a `sandboxId` ([REST](#rest)) and
answers the new one. feosd names the pod's TAP device `pod-<ID>`, so two
pods with short IDs never share one.

A sandbox made before keeps its UUID until it is deleted: it may be
running, paused or hibernated, and its snapshot names its TAP device,
`pod-` and its first 8 hex digits. ferund stores both formats side by
side and serves both everywhere an ID goes: paths take either (a UUID in
either case, shown in lowercase), and only a UUID's pod can share a TAP
device's name with another UUID's, which the placement's TAP gate keeps
apart ([Placement](#placement)). Every column that holds a sandbox's or
a pod's ID (`sandboxes.id` and the `sandbox_id`, `pod_id` and `holder`
columns that name one) is `text COLLATE "C"`, with a check of the two
formats in their canonical form: they compare and sort byte by byte, as
feosd sorts its inventory. Other IDs (tokens, environments, volumes,
incarnations, ops, watches) stay UUIDs.

`0008_pod_ids.sql` changed those columns in one transaction, the logs
first: `activity` and `traffic_records` are rewritten with their indexes
(about a minute for 2 million rows of activity), while the sandboxes'
tables are locked only for the short rest. The change is not additive:
an instance of the previous version, which a rolling deploy runs beside
the new one, fails its writes of the logs during the rewrite, and
afterwards its queries that cast a sandbox ID to `uuid` and its reads of
a short ID, until the deploy has replaced it. feosd must accept short IDs
before ferund mints them (the rollout of ADR 0019).

### The pod and its incarnation

Every row has an **incarnation**, a UUID set when the row is inserted: a
row inserted again under an ID that was deleted would be a new
incarnation (ferund mints new IDs, so only a drawn ID of a deleted
sandbox could be).
The pod's spec is `{resources: {vcpus, memory_mib}, containers: [{name:
"main", image_ref, command: ["sleep", "infinity"]}], network: {}, labels:
{ferun.sandbox, ferun.tenant, ferun.incarnation}}`, the same on every
retry: feosd compares specs, labels included, so a create against a
leftover pod of an earlier incarnation fails instead of returning it. A
pod is **ours** when its incarnation label is the row's; ferund never
touches a pod without `ferun.sandbox`, and never adopts the state of a pod
that is not ours. A row with a pod template, an environment's instance or
an attachable sandbox, builds its pod from the template instead
([Environments](#instances-are-sandbox-rows)).

### States

`clientState`, as FeCode reads it:

| State | Booked | Means |
|---|---|---|
| `pending` | no | no pod yet, a create failed before the pod existed, or its node went down before its pod was ever seen |
| `creating` | yes | a create, start, restore or repair runs, or the pod was seen pending |
| `running` | yes | the pod runs |
| `pausing` | yes | a suspend runs |
| `paused` | no | the pod is suspended on its node |
| `resuming` | yes | a resume runs |
| `hibernating` | as before | a hibernation runs |
| `hibernated` | no | in the object store; brought back with a restore, on any node of the same store |
| `stopped`, `failed` | no | the pod stopped or failed; the description says why |
| `unknown` | as before | the truth is not known: the node is down, or an op was released; a sync or the lost grace resolves it |
| `lost` | no | the pod that ran is gone and there is no checkpoint, or it went down with its node; until deleted, unless its node returns with the pod |

**Bookings follow resource use.** Every write of a state books the pod's
size on its node or frees it, as the table says. An op's admission books
through the gates; a pod that is found running where the gates would
refuse it is booked anyway, as overbooked, with an activity row: reality
wins over the books. Lock order everywhere: tenant, sandbox, at most one
node.

### Ops

Every lifecycle change is an op: begun in one transaction under the row's
lock, which writes the op, its owner, target and deadline and the
transitional state; run against the node outside any transaction; and
finished in another transaction that applies what the node did.

| Op | From | During | Calls | Deadline |
|---|---|---|---|---|
| create | pending | creating | CreatePod | 7 min 5 s |
| start | stopped, failed | creating | StartPod | 7 min 5 s |
| resume | paused; hibernating, in the place of a hibernate that a resume abandons | resuming | ResumePod | 7 min 5 s + 8 s/GiB |
| restore | hibernated | creating | RestorePod of the checkpoint | 7 min 5 s + 8 s/GiB |
| pause | running | pausing | SuspendPod | 3 min 40 s + 5 s/GiB |
| hibernate | paused; stopped, failed and running when draining | hibernating | HibernatePod | a pause's + `--hibernate-timeout` + 16 s/GiB |
| repair | creating without an op, running with an unreachable guest; running, for an instance whose container exited (`Restart`, [Environments](#probes-exits-and-restarts)) | creating | StopPod and StartPod; for a pod stuck in its restore, DeletePod and RestorePod, never for a pod with volumes | 7 min 5 s + 8 s/GiB |

GiB is the sandbox's memory, rounded up. **A caller's deadline covers
its callee's budget**, so that feosd answers, the change or "the VM is
busy", before ferund gives up, and a pause never ends at its deadline
while the node still writes the snapshot. feosd's suspend takes 190 s +
5 s/GiB at most, of which its wait for the snapshot is 120 s + 4 s/GiB
([node.md](node.md#the-suspends-budget)); a pause waits that and 30 s
more, for the call's way there and back and the pod's read after it
(`feosdSuspendBudget`, `pauseMargin`). A hibernation of a running pod
begins with that suspend, so a hibernate waits a pause's deadline first.
A hibernation uploads the pod's
disk and memory to the object store and takes minutes per GiB, hence its
own timeout. A feosd that reports its hibernations ([Follow-ups for
feosd](#follow-ups-for-feosd), item 2) runs one in a task of its own,
which a `HibernatePod` that comes meanwhile joins and which a cancelled
call or a dropped connection never stops, so the timeout is how long
ferund waits: past it the pod is still suspended, the sandbox `paused`
("hibernation failed: …"), and the hibernation's end arrives as the
pod's event, unless a resume abandons it first. A hibernate released by
a stopping instance goes on the same way, and the next Ensure resumes
the sandbox. An older feosd runs the hibernation in its call, which the
deadline, or a stopping instance that drops it, cuts at an arbitrary
point: there the timeout is how long a hibernation may run. A resume
has a restore's deadline: a `ResumePod` that meets a hibernation it
cannot abandon waits for it, then restores the pod from the new
checkpoint inside the call.

- **Detached.** An op's node calls never run on its caller's context:
  they keep the caller's values but not its cancellation, and end at the
  op's deadline. A client hanging up, an MCP cancellation or the end of a
  leader term never cuts them, because feosd stops a mutating pod call
  (other than `HibernatePod` on a feosd that reports its hibernations) at
  an arbitrary point when its request is cancelled. Callers wait on the
  op and may stop waiting; the op goes on. An instance runs one op per
  sandbox at a time, except that a hibernate a resume preempted runs on,
  fenced out, until its call answers.
- **Before begin.** An op on the node the sandbox is on (start, resume,
  pause, hibernate, repair, a takeover) first waits for this instance's
  connection to that node, up to `--node-wait` or the node's rollout
  deadline, and sends nothing until then. A sandbox paused on a node that
  restarts within `--rollout-grace` thus resumes once the node is back.
  The leader's loops never wait: they consider connected nodes only.
- **Begin.** The row is locked by its ID. An op in flight of another
  instance is taken over only when that instance is not live (dead or
  stopping): the takeover runs the same op again with a new ID and
  deadline, since every call is idempotent or answered from the pod's
  state. An idle pause and a hibernate-after recheck, in later statements,
  that the sandbox is still idle, holds nothing and is not backed off
  ([Policies](#policies)); a pause for a drain only that nothing executes;
  an explicit pause nothing. An admission locks the target node's row,
  rechecks the gates and books. The begin's transaction runs on a context
  of its own, bounded by 30 s, never its caller's: a client that hangs up,
  or a leader term that ends, while the COMMIT is on its way would leave
  the row in an op that nobody runs. A COMMIT whose answer is lost all the
  same (the connection broke) is judged by the row: an op that committed
  runs.
- **Preempt.** A hibernate of a paused sandbox that a resume abandons now
  is replaced by a resume, in one transaction under the row's lock: the
  resume gets a new op ID, this instance as its owner and its own
  deadline, and is admitted and booked as any resume. Its `ResumePod`
  makes feosd abandon the hibernation, which stops at its next step and
  answers `ABORTED`, and the pod resumes on its node from its snapshot;
  what was uploaded stays in the object store, named by no head. Whoever
  owned the hibernate, its op is fenced out by its ID: its finish writes
  nothing but a newer checkpoint, and its activity ends "abandoned".
  Every `HibernatePod` carries its op's ID as `request_id`, and feosd
  reports a hibernation in flight in `PodInfo.hibernation`: its phase,
  whether a resume abandons it now, and the request IDs of the calls in
  it. Only Ensure preempts, and only a hibernation that its op's own call
  has reached; feosd would otherwise start that call's hibernation anew
  behind the resume, and answers `ABORTED` to a late call of a
  hibernation it abandoned. A drain's hibernate, Ensure's off a draining
  node and a takeover's on a draining node send `no_abandon`, which feosd
  keeps for the hibernation's whole run: a resume waits for these, and
  the admission refuses a resume on a draining node anyway. A preempting
  resume that reaches no connection leaves `unknown`, "node <n> is not
  connected to instance <i>; the hibernate the resume was to abandon goes
  on", for a sync to resolve.
- **Call and settle.** After the call, also after its error, the op reads
  the pod and waits while it is pending, on its event stream with a read
  every 5 s, until the deadline. `UNAVAILABLE` and `RESOURCE_EXHAUSTED`
  while waiting are transient. Call errors are never mapped to a state
  directly: the pod read after them decides.
- **A busy VM.** feosd answers `UNAVAILABLE` "the VM is busy: <why>",
  marked with the status detail `type.googleapis.com/feos.microvm.v1.VmBusy`
  (an `Any` without a value; ferund goes by the marker, never by the
  message), to a
  `SuspendPod`, `StopPod` or `HibernatePod` whose microVM takes no change
  now and does later, as while Firecracker still writes a snapshot that a
  suspend gave up on, and when the snapshot of the suspend itself is
  late: nothing changed (a `DeletePod` ends such a microVM). The pod read
  decides the state as for any error, and the description says "<op>
  postponed: the VM is busy: <why>" (`pause`, `hibernation`, `repair`)
  where the op did not reach its goal, and a repair's always. A repair
  whose `StopPod` meets the busy VM calls no `StartPod`: the pod runs on
  as it was, and its processes keep their holds, since nothing ended
  them. The op is
  tried again later: an idle pause or a hibernate-after after its
  policy's backoff ([Policies](#policies)), a drain or a repair on its
  next pass; Ensure and
  the REST pause answer `ErrBusy` "<op> on node <n>: <description>; retry
  in 30s", with a retry time of 30 s. Another `UNAVAILABLE` is a failure,
  as before, whatever its message says. A tool call whose guest feosd
  could not reach because its VM is busy gets the same marker on "the
  guest of pod <id> did not answer within 5s; the VM is busy: <why>":
  the runner does not report the guest unreachable (which would have the
  repair loop restart a pod that is only busy), the call did not run,
  and it fails as a `NodeError` that asks to come back in 30 s
  (`tools.BusyError`; REST answers 502 with `Retry-After: 30`). An
  unmarked "the guest of pod …" of a running pod is reported
  unreachable, as before.
- **Finish** is retried until it commits. An op that is no longer the
  row's (taken over, preempted, or the row deleted or re-created
  meanwhile) writes only a tombstone for a pod it may have made, or a
  newer checkpoint into the row on its node, in any state. A
  rollback, when this run reached no connection and sent nothing, puts
  the row back as the begin found it; after a takeover, which cannot know
  what the first owner sent, it leaves `unknown`.

| Observed on the target | Becomes |
|---|---|
| ours, running | `running`; a run opens, restored from the checkpoint when a resume brought the pod back from a newer one; a failed pause lands here too, "pause failed: …", or "pause postponed: the VM is busy: …" |
| ours, pending at the deadline | `creating`, "still starting on node <n> after <d>" |
| ours, suspended | `paused`; a failed hibernation lands here too, and one that a resume from elsewhere abandoned ("hibernation abandoned for a resume") |
| ours, hibernated | `hibernated`, with the checkpoint (never an older one) |
| ours, stopped or failed | `stopped` or `failed`, with the pod's reason |
| not ours | a tombstone for that pod, then as not found |
| not found | `hibernated` without a node when there is a checkpoint; `lost` when the pod existed; else `pending`, and a node where the create failed is avoided next time |
| no answer until the deadline | `unknown`, "outcome unknown"; a sync resolves it |

A newer checkpoint that our pod reports is taken in every state, here and
by Sync, never an older one. A repair that restarts a pod, and a start,
resume or restore that came up with a cold boot, end the sandbox's process
holds; a repair that found the pod healthy and made no call ends nothing.

**Stale ops.** The leader takes over the op of an instance that died or
stops, when it is connected to the op's node; otherwise it releases the
op and the sandbox becomes `unknown`. An op of a live instance is never
taken over by another; one more than 5 minutes past its deadline is
released and logged as an error. An op that the row says is an
instance's own and that the instance does not run is orphaned (its begin
committed, but nothing ran it): Ensure on that instance takes it over at
once, as a dead owner's, and every instance looks for its orphans every
10 s, taking over those on nodes it is connected to and releasing the
others. That look reads all of the instance's ops but those it runs, 64
at a time in deadline order, so an orphaned hibernate, whose deadline
sorts last, is found however many ops the instance runs.

### Ensure

Before every tool call, port forward and explicit resume, `Ensure` brings
the sandbox to running, within the preparation budget of 7 min 5 s, on
any instance: it reads the row and starts or joins what its state needs.
A pending sandbox gets its image resolved and is placed and created; a
paused one resumed on its node, or hibernated and restored elsewhere when
its node has no room or drains; a hibernated one placed and restored; a
stopped or failed one started; an `unknown` one synced. A running sandbox
on a draining node is moved first. A `lost` one is synced once, in case
its node came back with the pod, then "sandbox lost". A hibernate in
flight that a resume abandons now is preempted ([Ops](#ops)), and the
sandbox resumes on its node; while the node's answer may still change (no
hibernation reported yet, the op's call not in it yet, no answer), Ensure
asks again every second. Any other hibernation in flight (a drain's, one
past its point of no return, every one on a feosd that does not report
its hibernations) is waited for at most 30 s; after that the caller gets
`ErrBusy`, "sandbox <id> is hibernating; retry in a minute", which
carries a retry time of 60 s (`apierr.RetryAfterOf`) that the tool routes
and the port door send as `Retry-After`, and a later call restores the
sandbox. Ensure starts at most one op of each kind per node per call,
and returns the error of an op that failed instead of trying again:
"<op> on node <n> failed: <description>". Every error of Ensure reaches
the client as `ensure sandbox: <error>`.

`Acquire` is Ensure and a **hold**: a row in `sandbox_holds` that keeps
the sandbox from being paused while a call or a forward runs, leased for
30 s and renewed every 10 s by its instance, taken under a lock that
excludes a pause's begin. Holds of processes started with `process_start`
last until the process ends or its timeout passes; a timeout of 0 holds
the sandbox until the process is stopped or found ended. An instance of
`ferun-ingress` holds the instances of public environments' idle
services it sends requests to with holds of kind `forward` and the
holder `ingress:<name>`, which it renews itself every 10 s through
`ferund` ([ingress.md](ingress.md#holds-and-the-idle-fence)).

### Placement

Gates, in order, each with the reason that the description and the
placement audit (the placement's activity row, with every node's
verdict) show: the node is `ready`; for a create of a row pinned to a
node (`pin_node`, [Environments](#instances-are-sandbox-rows)), that node
("pinned to node <n>: its volume is there"); not draining; not
forked; connected to this instance; no tombstone of this pod ID on it; not
avoided after a failed create (a pinned row never is); for a sandbox with
a UUID, no other sandbox with a UUID on it whose ID starts with the same 8
hex digits and may hold a TAP device (feosd names a UUID's TAP device
after them; a short ID names its own, and passes: [Sandbox
IDs](#sandbox-ids)); for a restore, the
checkpoint's object store; slots; CPU, `floor(cores × cpu_overcommit)`,
not gated when 0; memory, the total less the headroom; free memory, the
reported available less the headroom; disk, the available against
`--disk-budget`. Among the nodes that pass,
`binpack` takes the one whose scarcest resource has the least free share,
`spread` the most, ties by name; a hibernated sandbox prefers the node
that holds its record. The op's begin rechecks the gates under the
node's lock, so two instances placing at once never overbook a node.
Nothing fits: the sandbox stays, "no capacity (retryable): no feasible
node; n1: <reason>; ...". Slots (50), headroom (1 GiB) and overcommit (16)
are per node, set with `PATCH /api/v1/admin/nodes/{node}`.

### Images

A sandbox's image is resolved once, before its first create, to
`<registry>/<repository>@sha256:<digest>` (Docker Hub names normalised,
`linux/amd64` from an index, the registry's tokens for the create's
credentials or anonymous ones, a tag cache of 60 s per asker), and pinned
in the row: every retry and every later create sends the same digest, so
a moved tag never changes a sandbox. The tenant names the image, and the image names the
registry, so the resolver reaches registries as bindings reach servers:
over https only, without a proxy, and at IPv4 addresses outside the deny
set (`--deny-prefix`, by default the private, shared, link-local,
loopback, "this network", multicast and reserved ranges), judged on the
address it dials, for the registry, every redirect and the token realm
alike. Its errors
name the address refused, or that the registry is unreachable, never
what a dial found there.

**The question before a create** (ADR 0025). Every create of a new pod
(a pending sandbox's, whatever made it pending) asks the pinned image's
registry first, before placement: `HEAD /v2/<repository>/manifests/<digest>`
(a `GET` whose body must have the digest where `HEAD` is answered with
405), the Bearer challenge answered at its realm. A 200, whose
`Docker-Content-Digest` is the digest when it names one, is a yes and
the only answer that creates the pod. 401 and 403, a realm that refuses
a token, or a challenge for a login, fail the Ensure with
`model.ErrImageRefused` ("image …: the registry refused it"), 404 the
same ("… does not have it for this create"); no answer (unreachable, a
timeout of 30 s, 429, another status, another digest) fails it with
`model.ErrRegistryUnavailable`, retryable after 15 s. Either is the
sandbox's status description, and the sandbox stays pending. A resume,
a restore, a start and a repair ask nothing: they bring up a pod that
was created. The registry's token is kept per asker, registry and scope
until 10 s before its `expires_in` (60 s when it names none), and
nothing else is: questions of the same asker and image at once share
one request, on a context of their own. Tests and test clusters replace
the registries (`Engine.SetImageAccess`): their question is answered by
the replacement, and no pull token is asked for.

**Credentials** (ADR 0025). A sandbox's create may bring
`imageCredentials: {username, password}`, kept sealed under
`--secret-key-file`'s key in `sandbox_image_credentials` (AAD
`sandbox_image_credentials|<id>`), never in the sandbox's row and never
returned; `PUT …/image-credentials` replaces them and `DELETE` removes
them (204). Without the key, a create or `PUT` with credentials is 503;
half given, 400. An environment's image service or job names them in its
spec (`imageCredentials`, each field a string or `{secret}`); its
template keeps a secret by its name at version 0, which
`ResolveSecret` reads at its latest version that is not deleted when a
pod is created, so a rotated token reaches the next replica of an old
release; the template's hash takes them, so a change of which secret or
username replaces the pods. The engine resolves a tag, asks the
question and gets the guest's pull token with them: the realm gets them
as Basic authentication, and its token, asked for anew for every create
so that it lives the registry's whole life for the pull, goes beside the
pod's spec in `CreatePodRequest.pull_token` (and in a start's or
repair's `StartPodRequest`, asked for best-effort, since the disk has the
image unless its first pull did not end). A registry that asks a create
with credentials for Basic authentication is refused: its password would
enter the pod. The tag and token caches and shared questions know an
asker by an HMAC of its credentials under a key of the process.

**Baked images** (ADR 0025, `--image-bakes`). A create names, beside its
pod spec, the baked layers of its image that `image_bakes` holds, the
newest first, at most 16, and lets the node bake the image
(`CreatePodRequest.image_layers`); the key is the pinned image, registry,
repository and manifest digest, never a digest alone. feosd starts the
new pod's disk on the first layer made for its guest and blank data
volume that the object store has, or bakes the image at the pod's bake
point ([node.md](node.md#a-pod)) and names the layer in the pod's record
(`PodInfo.baked`) once the store has it. Every observation of a pod
(`getPod`) records a bake it names in `image_bakes`, once per instance
and layer; a layer the store lost is passed over by the nodes, and the
next bake of the image is a row of its own. Nothing collects layers or
rows yet.

### Sync and reconcile

- **Sync** applies one pod's state to its row, after a pod event or when
  a caller found the pod not where or not as expected. A pod found
  suspended under a row that was not `paused`, as one that feosd
  suspended after a pause gave up on it (feosd's suspend outlives its
  call, and a pod follows its microVM into suspended:
  [node.md](node.md#a-pod)), becomes `paused` from now (`paused_at`),
  not since an earlier pause, so that hibernate-after counts from here. While an op runs,
  the sync is deferred until it ends, never dropped. It reads the pod on
  the row's node and applies it under the row's lock, if nothing changed
  in between. An instance not connected to the node hands the sandbox to
  one that is, through a reconcile request.
- **Reconcile** applies a node's whole inventory: it takes a snapshot of
  the node's rows first, then lists the pods, and acts only on rows that
  did not change since the snapshot. A pod of ferund's without a row, or
  whose row is elsewhere or of another incarnation, gets a tombstone. A
  row whose pod is listed takes its state, including a `hibernated` or
  `lost` row whose node came back with the pod: the pod is adopted and
  booked again. A row whose pod is missing becomes what a sync makes of
  not found.

### Tombstones

Every decision that a pod is to go writes a tombstone (pod, node,
incarnation) in the deciding transaction: a delete, a restore elsewhere,
a stale record, an orphan, a fenced-out op. The tombstone is processed by
an instance connected to the node: it reads the pod and deletes it only
when its incarnation is the tombstone's (or it has none), and drops the
tombstone only after the node answered. Processing always runs detached,
like an op's calls. Its `DeletePod` has feosd's suspend budget, 30 s,
and 2 min and 4 s/GiB as its deadline: feosd's delete waits for the
pod's lock, which a suspend (an idle pause of a sandbox deleted
meanwhile) holds up to that budget, and feosd runs the delete to its end
once it has the lock, whatever its caller does. While a tombstone exists, placement puts no pod of
that ID on that node. Tombstones of removed nodes are dropped.

### Policies

The down sweep marks the sandboxes of down and removed nodes `unknown`,
leaving those with an op in flight to the op. After `--lost-grace` they
become `hibernated` (with a checkpoint, restored elsewhere on the next
use), `lost` (their pod was seen there), or `pending` without a node when
their pod was never seen there, as after a first create whose outcome
the node took down with it: nothing ran, so nothing is lost, and the
next use creates the sandbox. No tombstone is written, since the pod may
still run: a returning node's pod is adopted, or tombstoned by reconcile
when its sandbox is pending or placed elsewhere by then. Idle pause,
hibernate-after, drain and repair begin ops as their triggers. The idle
pause, and its begin's recheck, leave out a public environment's service
instance while an ingress instance that is joined, has an address and
has not left was last seen between 45 s and 15 minutes ago: it may be
cut off from `ferund`, its holds lapsed, and still serve the service
(the [idle fence](ingress.md#the-idle-fence)). A drain
hibernates what is paused, and pauses and hibernates what runs without
execs; a forward does not keep a sandbox, since the port door closes its
upgraded connections, a preview's WebSocket among them, once the sandbox
leaves running, and their clients reconnect to where it runs next. A call
for a sandbox on a draining node starts nothing there: it waits until the
sandbox has moved. After `--drain-timeout`, minutes after the drain's
start, the execs that still run are cancelled and the rest moves
regardless. A sandbox that ran is restored on another node as soon as its
hibernation is done, where the calls that waited go on; without room
elsewhere it stays hibernated until its next use. A resume never abandons
a drain's hibernation. A node without an object store cannot be drained.

An idle pause that leaves the pod running, and a hibernate-after that
leaves it suspended, back their own policy off, so that a node that
cannot write a snapshot, or an object store that refuses uploads, is not
asked again on every pass: the row names the policy (`policy_failed`),
and that policy's loop, and the recheck of its begin, begin no op for the
sandbox before `policy_not_before`, 30 s after the first failure and
doubling per consecutive one of the same policy up to 30 min. The other
policy is not held back: a sandbox whose hibernation failed, resumed
since, is paused when it idles, and its idle pause's failures count from
one. A pause that ends paused and a hibernation that ends hibernated,
whatever began them, and a move to another node clear the backoff. Drain
and repair do not back off.

### Delete

`DELETE /api/v1/sandboxes/{id}` removes the row in one transaction, with
a tombstone for every node a pod of this incarnation may be on (one that a
live op may still create waits for the op's deadline), and its tokens,
holds, watches, bindings and booking with it. An op in flight is fenced
out when it finishes. A row under the same ID later (a drawn ID of a
deleted sandbox) is a new incarnation, which placement keeps off a node
until that node's tombstone is done.

## Front doors

### REST

The routes, bodies, status codes and texts are the sandbox system's;
`go/internal/api` holds the route table, with each route's auth class:

| Class | Routes | Credential |
|---|---|---|
| admin | `/api/v1/admin/…` | an admin's UI session (cookie), or Basic with `--admin-users` |
| tenant | `/api/v1/sandboxes`, `/api/v1/sandboxes/{id}` (create, list, get, delete, pause, resume, tokens, access), `/api/v1/tools`, `/api/v1/environments[/…]` but the route door, `/api/v1/domains[/…]` | a tenant's token |
| sandbox | `/api/v1/sandboxes/{id}/…`: exec, files, dirs, search, fetch, processes, calls, watches, bindings | the sandbox's own token |
| mcp | `/mcp` | the sandbox's token, with the door's own answers |
| forward | `/fwd/…`, `/api/v1/sandboxes/{id}/ports/…` | the sandbox's token in `Authorization` alone ([ports](#ports)) |
| route | `/api/v1/environments/{env}/http/…` | a route token of the environment ([the route door](#the-route-door)) |
| public | `/healthz`, `/readyz`, `/auth/…`, the UI | none |

A tenant's token is not accepted on a sandbox route, not even for its own
sandbox (the forward class answers it 401 "forwarding needs a sandbox
token", [ports](#ports)); whoever names what they may not see gets the
answer of an absent thing, 404. Every unknown route under `/api/` is 404
`{"error": "no such route: METHOD PATH"}`, never 405. A sandbox ID in a
path is a short ID or a UUID, either case ([Sandbox IDs](#sandbox-ids));
anything else is 400 "sandboxId must be a sandbox ID: 11 characters of
lowercase Crockford base32, the first a letter, or a UUID". `POST
/api/v1/sandboxes` takes `image`, `vcpus`, `memoryMib`, `attachable` and
`imageCredentials` ([Images](#images))
and no `sandboxId`: one that is neither absent, null nor `""` is 400
"sandboxId is not accepted: ferund mints a sandbox's ID; leave it out
and keep status.sandboxId of the answer". Every create makes a new
sandbox, 201 `{status, token, created: true}`, the ID ferund minted in
`status.sandboxId`; it is not idempotent, so a client keeps the ID before
it relies on the sandbox. The admin API has tenants, each with `sandboxCount` (its
sandboxes, without its environments' instances), `environmentCount` (its
environments not being deleted) and its tokens (hashed, shown once),
nodes (with their connections to the serving instance, certificates,
drain, removal and capacity settings), join tokens, instances and the leader, and the
activity of a sandbox or a node, with gaps and traffic records. `GET
/api/v1/admin/nodes` answers the nodes and `defaults`, the serving
instance's settings that the UI explains the fleet with:
`telemetryIntervalSeconds`, `nodeTtlSeconds`, `lostGraceSeconds` and
`drainTimeoutSeconds` (its timings, in seconds), `defaultVcpus`,
`defaultMemoryMib` and `defaultImage` (what a sandbox created without
them gets), and `minVcpus`, `maxVcpus`, `minMemoryMib` and
`maxMemoryMib` (the bounds a create accepts). An undrain clears the text
the drain loop left in the node's status description ("draining: …",
"drained", "cannot drain: …"); another writer's text stays. Deleting
a tenant deletes its tokens at once, then its sandboxes in batches as a
sandbox delete does, and its environments with their addons' data
(`env-delete`); the leader finishes what a request did not.
`/readyz` fails while the instance stops, and after its start until it is
connected to every ready node that reported since, at most 30 s, so that a
rolling deploy sends no requests to an instance no node has dialled yet.

### Human sign-in

People sign in to the UI and the admin API with OIDC (`--oidc-issuer`):
FeCode, FeRun and FeChat share one provider. ferund is a confidential
client (`client_secret_basic`), with the authorization code flow, PKCE,
`state` and `nonce`; the ID token's signature (RS256, ES256 or EdDSA, from
the provider's keys) and claims are checked with the standard library.
The keys are fetched again on an unknown key ID, at most once a minute,
counting the first fetch. A person whose `groups` claim holds
`--oidc-admin-group` gets a session in the database, so that a sign-in
begun on one instance completes on another and a sign-out ends it
everywhere; anyone else gets "no access" and no session. A request with
the session cookie that changes something must carry `X-Requested-With:
ferun` and, with an `Origin`, the public URL's; ferund sends no CORS
headers. Basic credentials, when present, are checked first, and a
request with them that changes something is refused, 403 "cross-origin
request refused", when the browser marks it as another origin's
(net/http's `CrossOriginProtection`): a `Sec-Fetch-Site` other than
`same-origin` or `none`, or without one an `Origin` that is neither the
`Host`'s nor the public URL's. A browser keeps the credentials typed
into its own dialog and would send them with another site's form; `curl
-u` sends neither header, and the UI's requests are same-origin. Without
OIDC and `--admin-users`, the admin routes answer 503. An admin route's
401 carries `WWW-Authenticate: Basic`, which makes a browser show its
login dialog, only with `--admin-users` and only to a request without
`X-Requested-With: ferun`: the UI sends that header on every request and
shows its own sign-in screen on a 401, so a session that expired does
not bring up the browser's dialog over it.

### Tools and MCP

The agent's tools are the sandbox system's twelve, with their
descriptions and schemas: `bash`, `read`, `write`, `edit`, `ls`, `rg`,
`fd`, `process_start`, `process_list`, `process_logs`, `process_stop`,
`webfetch`. A call acquires the sandbox, then runs on its node over this
instance's node connection: the file tools and `webfetch` as
`feos.tool.v1` calls in the container `main`, `bash` as an exec of `bash
-c` in `/`, the process tools as execs of small scripts that keep their
records in the container. Outputs are capped at 50,000 bytes with the old
markers; a call has 120 s by default and then reports `timeout after Ns`.
A call that finds the pod not running or not there syncs, acquires again
and retries once. REST and MCP share the one entry point:

- **REST** answers 200 whenever the tool ran, its exit code being its
  verdict, and 502 `{"error": "<tool>: <error>"}` otherwise, with a
  `Retry-After` when a retry is worth it (no capacity, a node or instance
  unavailable or stopping, a hibernation in flight, a budget): 60 for a
  hibernation in flight, 5 for the others.
  `PUT …/files/{path}` writes its body byte for byte, whatever the bytes;
  the activity log records the write with its content when that is UTF-8
  text, as MCP's, and by its path and size otherwise.
  Every tool route takes a key in `Idempotency-Key`, bare or as one
  quoted sf-string, and answers a malformed key, or more than one, 400
  `bad_idempotency_key`; exec, `PUT` and `PATCH …/files/{path}`, `POST
  …/processes` and `DELETE …/processes/{pid}` honour it, the other
  routes ignore it. A keyed
  call's answer carries `Ferun-Call-State` (`done`, `interrupted`,
  `not_run`, `running`) and, when it is another request's call, attached
  to or read from its record, `Ferun-Call-Replayed: true`. Beside the
  answers above, a keyed call is 422 `call_id_conflict` when its key
  names another call in the sandbox, 400 `call_timeout_too_long` for a
  `bash` timeout over 600 s, 503 `call_running` with `Retry-After: 1`
  when it runs on another instance and this one stops, and 502
  `call_interrupted` without `Retry-After` when it may have run and
  stopped without a result; one that ran and failed is 502 without
  `Retry-After`, one that did not run keeps the rule above. The codes
  are the error body's `code`. `GET …/calls/{key}?wait=N` reads a keyed
  call's record and `DELETE …/calls/{key}` cancels the call ([Calls that
  outlive their caller](#calls-that-outlive-their-caller)).
- **MCP** (`/mcp`) runs the go-sdk's Streamable HTTP handler stateless,
  so that any instance serves any request: the bearer is resolved on
  every request. Its answers are the door's own, which the SDK's clients
  act on: 403 for a foreign Origin, 405 for a GET, 401 for a missing
  token or an unknown one on `initialize`, a JSON-RPC "session not found"
  (404) for an unknown token on any other request, so that a client
  starts a new session, and 503 when the token cannot be looked up. A
  session ID exists only for cancellation, and only `initialize` issues
  one: it is signed with the cluster's key and verified by any instance,
  and a `notifications/cancelled` with it is broadcast (`ferun_cancel`)
  to whichever instance runs the call. Such a cancel reaches a keyed call
  only for these session clients: the door keeps a request's keyed call
  cancellable for 30 s after the request ended, since the go-sdk hangs up
  before it sends the cancel, and remembers for 30 s a cancel that came
  before its request, which then starts nothing. It keeps both per
  token, at most 256 of each a token on the instance: a token that would
  pass that loses its own oldest ended entry, never another token's, so
  no client's churn flushes the entries another client's cancels need,
  and a token with 256 keyed requests running on the instance gets an
  error result without `_meta` for the next, before its call is looked
  up. The broadcast names the token by a digest of its hash, so that
  every instance counts the cancel alike. Nothing in this memory is
  scanned: its entries expire and are evicted in the order they ended.
  A known limit: the `notifications/cancelled` of a verified session are
  not rate-limited before their broadcast; each is one `pg_notify` on
  `ferun_cancel`, which every instance receives, as fast as the client
  sends them.
  The go-sdk's default protocol opens with `server/discover` and has no
  session; its clients, FeCode among them, cancel a keyed call with REST
  `DELETE …/calls/{key}`. A tool call's key is its `_meta["ferun.callId"]`, and a
  keyed result carries `_meta` `ferun.callId`, `ferun.callState` and
  `ferun.callReplayed`. A JSON-RPC error `-32602` with `data.reason`
  refuses a key that is not a string or not a key (`invalid_call_id`), a
  key that names another call (`call_id_conflict`, with `data.tool`) and
  a `bash` timeout over 600 s (`timeout_too_long`, with
  `data.maxTimeoutSeconds`). The answers to `initialize` and
  `server/discover` advertise `experimental["ferun.callId"]`, beside the
  SDK's `logging` and `tools`:
  `{"tools": […], "retentionSeconds": 3600, "maxTimeoutSeconds": 600}`.
  While a call runs, progress goes out every 10 s and per output chunk,
  and an SSE comment every 25 s keeps balancers from closing the
  response.

Every deployed guest decodes at most 4 MiB a message (tonic's default),
restored ones included, and the test node's services take as much. A
write, an edit or a command whose request is larger than 4032 KiB (4 MiB
less room for framing) is therefore refused before it runs: "the content
is larger than 4032 KiB, the most one write carries into the sandbox"
(the edits, the command), on REST a 413. A JSON body is read up to 32
MiB, a write's body up to that limit.

### Calls that outlive their caller

A call of a tool with a side effect, `bash`, `write`, `edit`,
`process_start` or `process_stop`, that carries a key (MCP's
`_meta["ferun.callId"]`, REST's `Idempotency-Key`) is a **keyed call**
([ADR 0023](../adr/0023-calls-outlive-their-caller.md)). It is a row of
`tool_calls` and runs on a context of its own, in the instance that
received it; its request only waits for it. A request that ends, because
its client hung up, crashed or was deployed, therefore cancels nothing:
the call runs to its end, and its result is kept. A repeat of the key
with the same call attaches to it or gets its result, on any instance; a
repeat with another tool or other arguments is a conflict, and nothing
runs. The read-only tools check a key and ignore it: their calls run on
their request, as calls without a key do.

- **Keys** are 1 to 255 characters of `A-Z a-z 0-9 . _ ~ : @ -`, which a
  URL path never escapes and an RFC 8941 sf-string holds as they are. A
  key is unique per sandbox, whichever of the sandbox's tokens sends it.
- **The arguments' hash** is a SHA-256 over the tool and its normalised
  arguments: decoded into the tool's parameters and encoded again, so
  that spacing, key order, unknown keys and zero values spelled out make
  no other call. `write` hashes its path as given and its content's
  bytes, so that REST's raw body and MCP's string hash alike.

The row holds an ID of its own for every run of the call, the sandbox,
the key, the tool, the hash, the instance that runs the call, its state,
`sent`, its lease, its start and end, and its result: the exit code, the
output, `truncated`, `is_error` and the error. `sent` is committed before
the call's request leaves for the guest, and a call whose row cannot be
marked is not sent, so that every state is exact, whoever writes it:

| State | Means |
|---|---|
| `running` | the call runs, or its instance stopped renewing it and nobody has ended the row yet |
| `done` | the call ended with its result, or with an error after it reached the guest (a timeout, a node's error, a forced drain), or its caller cancelled it |
| `interrupted` | the call reached the guest, may have run, and stopped without a recorded result: its instance was cut at the shutdown grace, died, or lost its database; ferund never runs it again |
| `not_run` | the call never reached the guest; the same call under the same key runs anew |

```
(none) ─begin─> running ─finish──────────────────────> done ─────────1 h─> (deleted)
  ▲               │ ─finish: cut at the grace, sent──> interrupted ──1 h─> (deleted)
  │               │ ─expiry or claim, sent───────────> interrupted
  │               │ ─expiry or claim, not sent───────> not_run ──────1 h─> (deleted)
  │               └─finish or abandon, not sent──────> not_run
  │                                                       │
  └────── a begin of the same call: a new run, a new ID ──┘
```

- **The run.** The call's context keeps the request's values and drops
  its cancellation. A timer ends it at its budget, the sandbox's
  preparation (7 min 5 s), the tool's timeout and 1 min, and never a
  deadline, which gRPC would send to the node as `grpc-timeout`; the
  instance's end ends it too (the drain's cut, the runner's stop).
  `Acquire` runs inside it, so the exec hold that keeps the sandbox awake
  is the call's, not the request's: a `bash` of 600 s whose client left
  holds the sandbox for 600 s. A forced node drain (`ferun_cancel` kind
  `exec`) cancels keyed calls as it cancels the others.
- **Joining.** A request takes the key's lock on its instance. A call of
  the key that runs on the same instance is attached to in memory.
  Otherwise the request reserves one of the sandbox's 64 slots on the
  instance and inserts the row: a new row, or a new run in place of a
  `not_run` row of the same call, starts the call; an existing row of the
  same call is attached to while it runs and answered once it ended.
  Attaching across instances waits on the row, for a `ferun_call`
  notification or a read every 1 s, and tolerates database errors for
  1 min. Requests that wait for a call they did not start, attaches in
  memory or on a row and record waits (`?wait` over 0), count per
  sandbox and instance: past 256, four times the slots, one is refused
  at once with the cap's retryable error and state `running` (REST 502
  with `Retry-After`, MCP an error result whose `ferun.callState` is
  `running`, the record 502 with `Retry-After`), and the call runs on.
  This bounds a sandbox's goroutines on an instance, and its row reads
  every 1 s. A stopping instance, one whose renewal does not run and a
  full cap refuse the slot; the refusal is answered `not_run` only when
  the key has no row or a `not_run` row of the same call, and a running
  or ended row is attached to or answered instead. A request that has
  already ended when its call would start starts nothing. An answer that
  is another request's call says it is replayed.
- **Not run.** A call ends `not_run` when it failed before the guest
  could have run anything. Once its row is inserted, `not_run` is a
  stored state, and the row keeps the error for the retention: its
  preparation failed; its row could not be marked sent; the exec cap
  refused it, or the nodes' port did before the call reached a node
  connection (the node not connected to this instance, its connection
  closing, a budget of the connection exhausted); feosd's
  own refusals ("pod <id> is <STATE>", "no pod <id>") came before any
  output; the drain's send gate held it back, or the drain's cut came
  before the send; its row was ended before the send. A refusal before
  the insert stores nothing: the cap of 64, an instance that stops or
  does not record keys, and an insert that failed are answered
  `not_run`, and a later read of the key finds no record (404
  `no_such_call`) or the key's earlier `not_run` row. Only an insert
  that committed without its answer leaves a row, which the instance
  ends `not_run` at once, or its expiry later. A guest that feosd could
  not reach because its VM is busy (its marker, [A busy VM](#ops)) is a
  refusal: feosd gave up on the connect before the call reached the
  guest. An unreachable guest is otherwise
  no refusal: feosd says so in the same words before and after it hands
  the call to the guest, so the call may have run; it keeps `sent` and
  ends `done` with the error. The same call under the same key replaces
  a `not_run` row with a new run, another call under it is a conflict,
  and REST answers `not_run` 502 with a `Retry-After` when its cause is
  worth a retry. A cancel by the caller is final even before the send:
  `done`, "the call was cancelled by its caller".
- **The lease.** The instance renews the leases of its running calls
  every 10 s, for 30 s, in one statement, and cancels a call whose row
  the renewal no longer returns, because another writer ended it; the
  call's final write is then refused. It records keys only while that
  renewal runs, and refuses keyed calls `not_run` otherwise. A running
  row whose lease passed is ended by three writers, each with a
  conditional update that sends `ferun_call`, `interrupted` when the call
  was sent and `not_run` when not:
  - the leader's `tool-calls-expiry`, every 5 s, fenced, 256 rows a
    transaction, only while the leader has led for a lease and its
    database has not failed within one, so that after a failover or an
    outage every instance has had a full lease to renew its rows;
  - any instance that waits on the row, for that row, once its own
    database has not failed within a lease and the lease passed more than
    a lease ago: waiters end a dead instance's calls also while the
    leader runs an older version, or died with that instance;
  - the start of an instance under the same ID, for its earlier run's
    rows ([Leader and instances](#leader-and-instances)).

  The leader ends a dead instance's call 30 to 35 s after its last
  renewal, a waiter about 60 s after it.
- **A call on an instance that lost its database** runs on until one of
  two things stops it. The nodes drop the instance once its row is
  stale, at their next telemetry answer, about 15 to 25 s after the loss
  ([Nodes](#nodes)): its exec streams end, and the guest kills the
  command's process group. Or the database comes back first, the renewal
  misses the call's ID, and the instance cancels the call. Either way its
  final write is refused, and the row holds the expiry's verdict. This
  relies on the nodes' handling of the instance list: a node keeps its
  list while its telemetry fails, so in an outage of the whole database
  every call runs on, and the guards of the leader and the waiters keep
  their rows running. A self-fence by time was set aside: it would cut
  every keyed call on a database failover.
- **Cancel.** `DELETE /api/v1/sandboxes/{id}/calls/{key}` cancels a
  running call for every client, on whichever instance runs it
  (`ferun_cancel` kind `call`): 202 `{"cancelled": true}`, or 404
  `no_such_call` when no call of the key runs. MCP's
  `notifications/cancelled` reaches a keyed call only for session
  clients ([Tools and MCP](#tools-and-mcp)). A cancelled call ends `done`
  with "the call was cancelled by its caller", and whoever waits for it
  gets that error.
- **The record.** `GET /api/v1/sandboxes/{id}/calls/{key}?wait=N` reads
  a keyed call's record and never runs anything. It answers at once when
  the call ended; otherwise when the call ends, or after N s (0 to 60)
  with state `running`, and at once with `running` on a stopping
  instance. The body is `{key, tool, state, instance, startedAt}`, and
  once the call ended also `finishedAt`, `exitCode`, `output`,
  `truncated`, `isError`, `error` and `text`, where `text` and `isError`
  are the result as MCP renders it. A key without a record, never used or
  past its retention, is 404 `no_such_call`; a malformed key or a wait
  outside 0 to 60 is 400; a wait past the sandbox's 256 waiting requests
  on the instance (see Joining) is 502 with `Retry-After`. A client that
  lost an answer reads it here rather than sending the call again:
  FeCode recovers its calls only this way, and sends one again only
  after `not_run`.
- **The drain** ([Stopping](#stopping)) begins when a stopping instance
  stops accepting requests. New keyed calls are refused `not_run` unless
  their key names a call that ran or runs. A call reaches the guest only
  if its timeout and 30 s end before the shutdown grace; otherwise it
  ends `not_run`, and its client sends it to another instance. Requests
  that wait on another instance's call, and record reads, are answered
  at once that the call still runs (REST 503 `call_running` with
  `Retry-After: 1`, MCP an error result whose `ferun.callState` is
  `running`, the record with state `running`). After the
  HTTP shutdown the instance waits for its keyed calls until the grace's
  end, then cuts them: a call that reached the guest ends `interrupted`,
  with its output so far, one that did not ends `not_run`, and their
  final writes get 15 s more. A call sent before the drain ends within
  its timeout, at most 600 s after the drain began, and one sent during
  it ends 30 s before the deadline. So a grace below 600 s can cut a call
  that was sent before the stop, and one below 630 s refuses a keyed
  `bash` of the full 600 s `not_run` from the drain's start. The default
  of 11 minutes does neither.
- **Bounds.** The output is the tool's text as cut, at most 50,000 bytes,
  stored as bytes (`bytea`), since it may hold NUL and invalid UTF-8. The
  error is made valid UTF-8 without NUL and cut at 4 KiB on a character
  boundary. A keyed `bash` names at most 600 s, so a call lives at most
  its preparation, 600 s and 1 min. The final write is retried for up to
  5 minutes; when another writer ended the row first, the call's waiters
  get the stored row.
- **Retention.** The leader's `tool-calls-retention` deletes, every
  minute, the rows that ended more than an hour ago, up to 10 × 1000 a
  run; their keys are free again.
- **Progress** is heartbeats only: MCP's progress every 10 s, and per
  output chunk for a request on the instance that runs the call. No
  output is relayed to a request on another instance, or replayed to one
  that attaches late.
- **Older instances**, in a rolling deploy from a version without keyed
  calls, ignore the key, the header and `ferun_cancel` kind `call`, run
  such a call on its request, and never touch `tool_calls`.

### Ports

`/fwd/{id}/{port}/…` and `/api/v1/sandboxes/{id}/ports/{port}/…` forward
HTTP to a port inside the sandbox, with the sandbox's token in
`Authorization: Bearer` and nowhere else. Without it the door answers 401
"forwarding needs a sandbox token", and so it answers a tenant's token,
on both paths: the answer names no sandbox. An unknown token and another
sandbox's get 404 "no such sandbox". A sleeping sandbox is brought up
detached from the request, so that a browser that gives up on a cold
start does not cancel it. The request goes through a reverse proxy whose
transport dials `PortForwardService/Forward` on the sandbox's node
connection; the token stops at the door, and the guest sees the Host the
client sent, which for FeCode's previews is the preview host. Upgrades
such as WebSockets ride through. Connections are pooled per pod and port;
a sandbox has at most 32 on the instance, pooled ones included, and a
port's pool no more. A request waits up to 30 s for its turn at a
connection, a pooled one or a slot for a new one, then gets 503 "too many
connections into this sandbox on this instance, try again" with
`Retry-After: 1`; a dial that waits for a slot closes the sandbox's idle
pooled connections. The 30 s bounds that wait only: a dial that has its
slot waits for the node (up to its rollout deadline when it announced a
restart) and for the guest's ready as long as they take, and the request
gets their answer. A forward holds the sandbox awake. A sandbox that
cannot be brought up is 503 "the sandbox could not be brought up:
<error>" with `Retry-After: 5`, 60 for a hibernation in flight; one whose
node is unavailable is 503 "the sandbox's node is unavailable", also with
`Retry-After: 5`. A failed dial is read by its code and feosd's text,
since both failures that matter come as `UNAVAILABLE`: "127.0.0.1:<port>:
…", the guest's, means nothing listens on the port (502), "the guest of
pod …" that the guest does not answer (503), as when it does not answer
feosd's connect within 5 s while its microVM is paused or Firecracker
writes a snapshot ("…; the VM is busy: <why>"), anything else that the node
connection failed (503). A refused Kubernetes listener is 403, a pod that
does not run yet 503 with a retry. A feosd that does not forward this
service for pods answers `UNIMPLEMENTED`, and the door 502 "node <n> does
not forward ports yet" ([follow-ups](#follow-ups-for-feosd)). The
environments use the door too: their readiness checks with probe slots of
their own beside the 32, and their route door through its forward
([Environments](#probes-exits-and-restarts)).

### Watches

`POST`, `PUT`, `GET …/events` (SSE) and `DELETE` on
`/api/v1/sandboxes/{id}/watches` keep the sandbox system's contract: a
PUT carries the whole path set and doubles as the heartbeat, and `stale`
means "read these paths again". A watch is a row, so any instance answers
it, with one **owner**: the instance that holds the guest's watch stream
and the replay ring of the last 1024 events. An event stream opened on
another instance takes the watch over, and event IDs carry the owner's
epoch, so they keep increasing across owners. A PUT on another instance
writes the set and waits for the owner's answer. When the guest's stream
ends (a pause, a lost node connection, a feosd restart) the owner sends
`stale` and places the set again once the sandbox runs and its node is
connected. A watch never wakes a sandbox and never holds one awake.

### Kubernetes and SSH bindings

A sandbox's bindings ([ADR 0014](../adr/0014-identity-and-credentials.md))
are served only with `--bindings` and `--secret-key-file`. Off, the
default, their routes are not registered, so they answer 404 "no such
route", which FeCode reads as "no such surface on this platform", and
`GET …/access` answers `{"kube":null,"ssh":null,"denies":[]}`. On, they
are stored with every credential sealed at rest (AES-256-GCM under the
key, bound to the table, sandbox, name and kind), validated with the old
codes, checked with preflights that dial from the instance, and reach a
pod sealed to its run's key when its `feos-authn` asks for its
credentials; nothing is pushed to a node, so a change reaches a running
pod at its next start, resume or restore. `…/access` shows them with
their use from the pods' audit records. They stay off by default until
feosd delivers them ([follow-ups](#follow-ups-for-feosd)).

### The management UI

`web/` is a React app built with Vite into `go/internal/ui/dist`, which
ferund embeds; only `dist/.keep` is committed, so the module builds
without npm, and such a build answers the UI's routes with 404. It shows
tenants and their tokens, sandboxes with their activity, network and
access, nodes with their certificates and capacity, join tokens, and
instances with the leader, and signs people in through `/auth/`. A
tenant's delete asks with the number of its sandboxes and environments
("delete with 2 sandboxes and 3 environments and their data?") and says
that its environments' addon data goes with it. A table
too wide for the window scrolls sideways in its own box; the nodes'
table fits a 1280 px wide window, and its memory bar fills by, and is
labelled with, the host's memory in use against its total. Its node
connections are those to the instance that answered, which with several
replicas need not be the one a node dialled; so it calls a node not
connected only when the node is down or removed. An instance is shown
live, stopping while it stops but still writes its row (it serves on),
or gone once its row is older than 15 s; the API's `live` and `fresh`
say which, on the database clock.

Behind the tenant gate, with a tenant's token that the UI minted and
keeps in the browser, it shows the tenant's sandboxes and, read only, its
environments (`/tenants/<t>/environments[/<env>[/<tab>]]`): the
list, with label filters; an environment's overview (its rollout, its
objects by kind with their instances, its routes, its open incidents);
its releases with their tags, annotations, diff and spec; its incidents
with their evidence, also what a release past its deadline waited for;
and an object's logs, where the 501 of a node that serves none and the
409 of a paused instance are messages, not errors.
It polls (the list every 4 s, an environment every 3 s, the rest every
5 s), writes nothing, and shows no secret value and no route token. The
instance tables fit a 1280 px wide window. Its tests read the fixtures
of ferund's API tests (`go/internal/api/testdata/ferun`).

## Environments

An environment ([ADR 0018](../adr/0018-environments-and-releases.md)) is
a tenant's set of typed objects that run together: services, jobs,
addons, routes and secrets, each with a DNS label for its name. It
changes only by releases, numbered from 1 per environment and never
changed: a release holds the whole desired state as a spec, the release
it was made on, who made it, why, and annotations of the caller's. The
controller brings each environment to its latest release and keeps it
there. FeCode drives environments for its projects' applications
(FeCode's ADR 0008); the management UI reads them.

### The environments' packages

| Package | What it is |
|---|---|
| `env` | the service: plan, apply, revert, object changes, clone, releases, tags, secrets, status, incidents, logs, the route door and its tokens; the wire bodies and error answers; `New`, `Run`, `Loops` |
| `env/spec` | the release spec: types, strict parse, validation, normalisation, diff, merge patch, hashing |
| `env/addons` | the addon blueprints and their registry |
| `env/ctl` | the controller: its leader loops, the Ensure pool, probes and restarts, attached services, security groups, name tables, the engine's prepare hook, secret resolution |
| `env/envdeps` | the interfaces both reach the instance through (`Engine`, `Runner`, `Ports`, `Sealer`) and `SecretAAD` |

`store/env_*.go` hold the queries, `api/environments.go` the routes.
`server.New` builds the service after the streams, with the engine, the
tools' runner and the port door (`Streams.Forwarder`), each picked up by
its type, and the sealer when `--secret-key-file` is set, then sets the
engine's secret resolver and prepare hook to the controller's. What is
missing is named in one log line at the start, "environments run without
some parts": without the runner, attached services and exec checks fail;
without the port door, http and tcp checks and the route door; without
the sealer, secrets and addons.

### Instances are sandbox rows

Every pod of an environment, a service's replica, an addon, a job's run,
is a row of `sandboxes` with `environment_id` set: the engine places,
creates, syncs, moves, pauses, hibernates and deletes it as any sandbox,
with its ops, bookings, tombstones and holds. `0006` adds five columns:

| Column | What it is |
|---|---|
| `environment_id` | the instance's environment; fixed |
| `pod_template` | `model.PodTemplate`: the pod's command, env (literals, secrets by name and version, or parts of both with a URL's userinfo escaped), volumes, mounts, security groups, name tables and labels; fixed. NULL: the sandbox's own pod |
| `pin_node` | a create goes to this node alone; a start or resume goes to the row's own node anyway, where its pod and volume are, and a restore anywhere, its checkpoint carrying the volume; a pinned row avoids no node |
| `idle_after_s` | NULL: `--idle-pause`; 0: never; n: seconds |
| `address` | the pod's IPv6 address as last observed in CreatePod, RestorePod, GetPod and ListPods, cleared when the pod is gone or hibernated; a change on an instance row, or on a row whose pod has groups of its own, sends `env-groups` in its transaction |

A row with a template builds its pod from it: the container `main` with
the row's pinned image and the template's command (none: the image's
entrypoint and command run), its env resolved by the engine's secret
resolver (one that does not resolve fails the op before any pod call),
its volumes, mounts, groups and name tables, and the sandbox's labels
beside the template's `ferun.environment`, `ferun.object`
(`<kind>/<name>`) and `ferun.template` (the template hash). There is no
release label: an object a release does not change keeps its pods. Rows
are inserted with `idle_after_s` always written, IDs ferund mints ([Sandbox
IDs](#sandbox-ids)), and at least 256 MiB, the rows' floor,
which valkey's and nats's `dev` plans are raised to.

Instance rows are no tenant's sandboxes: the tenant's listing and the
admin listing's `sandboxCount` leave them out, the tenant routes answer
them 404 `unknown sandbox <id>`, the sandbox-token doors `no such
sandbox`, and no sandbox token is minted for one.
A tenant with environments is not deleted: `DeleteTenantIfEmpty` waits
for them, `env-delete` marks the environments of a deleting tenant
deleting, and an environment's create, apply, clone and secret, and the
controller's instance insert, take the tenant's row FOR SHARE and refuse
a deleting one.

| Table | Holds |
|---|---|
| `environments` | owner, name (a DNS label, never a UUID, unique per owner), labels, `release` (the latest desired), `realised`, `deleting`, `gen` (the env group's version); `public` (its tenant's decision that the ingress serves it), `ingress_group` (set at its first publish and never cleared: its image services' pods name `ing-<sandbox id>`) and `host_label` (its default host's label, given at its first publish, unique), public only with both ([Public environments](#public-environments)) |
| `env_releases` | per number: the normalised spec, `base`, author, message, annotations (a string map, at most 4 KiB), `origin` (`apply`, `revert`, `clone`, `object`) with `origin_ref` (the number reverted to, `<env id>/<number>` of a clone's source, `<kind>/<name>` of an object change), `state` (`pending`, `rolling`, `realised`, `failed`, `superseded`), `reason`, start and end |
| `env_tags` | a tag's release, who moved it and when |
| `env_secrets` | one version of a secret, sealed; `system` for an addon's credential, `addon.<addon>.<key>`, never listed; soft-deleted |
| `env_instances` | the controller's view of an instance row: kind, object, replica, template hash, release, phase (`starting`, `ready`, `unready`, `exited`, `succeeded`, `failed`, `retiring`), restarts, the last 5 exits, probe failures, the next restart, an addon's provisioning hash, detail; `ingress_ports`, the ports its `ing-` group opens to the ingress as its node holds them ([Security groups](#security-groups)) |
| `env_job_runs` | one attempt of a job: template hash, release, attempt, phase, exit code, detail, the last 50 lines of its log, redacted and bounded ([Logs](#probes-exits-and-restarts)) |
| `env_attached` | an attached service: its sandbox and the incarnation of it the service last used, template hash, process ID, phase, restarts, the next restart, probe failures, the last exit, detail |
| `env_volumes` | an addon's volume (`addon/<name>`): ID, size, node, `provisioned`, holder, `addon_type` (fixed) and `data_version`, `deleting` |
| `env_incidents` | a condition of an object in a release: `rollout_failed`, `crash_loop`, `job_failed`, `no_capacity`, `addon_unhealthy`, `volume_unavailable` or `names_unserved` (`attach_failed` is reserved), opened, closed, summary, evidence; one open per object, release and condition |
| `env_route_tokens` | the route door's tokens, as hashes |
| `host_labels` | every default host label ever given: its owner, the tenant that first claimed it and alone may claim it again, and the environment that has it, set free when that is deleted |

`hosts_written` of `env_instances` and `env_attached`, the hash of the
hosts block that ferund wrote into a pod before its names moved into
`feosd`'s resolver, is read and written by nothing since `0007` and goes
in a later migration: the previous version, which a rolling deploy runs
beside this one, still reads and writes it.

The database holds no secret value: the log tails it keeps, a run's and
an incident's, are redacted before they are stored, as far as
redaction reaches ([Logs](#probes-exits-and-restarts)). A writer that
wants the controller to act sends `env-reconcile` (or `env-groups`,
`env-names`) on `ferun_leader` in its transaction. The writers of what
the ingress's table shows (publishing, an environment's release,
realised release and deletion, a tenant's deletion, a service
instance's insert, delete, phase, release and admitted ports) send
`ferun_ingress` `env:<id>` themselves.

### The release spec

JSON, its objects in maps keyed by name (`services`, `jobs`, `addons`,
`routes`, `secrets`), parsed strictly: an unknown field, a wrong type, a
duplicate key or trailing data is a finding at its path, and null is
absent for lists, maps and objects. Every problem is a finding
`{severity, path, problem, remedy}`; an error refuses the spec. Beyond the
shapes:

- **Services** run `image`, pods of their own: replicas 0 to 20 (1),
  vCPUs 1 to 32 (1), memory 256 to 65536 MiB (512), readiness by http,
  tcp or exec with an initial delay (0s), a period of at least 1 s (2s)
  and a failure threshold (3), `idle` of at least 1m only on a route's
  target ("nothing would wake it"), `rollout.timeout` (10m). Or they run
  `attached`: a sandbox of the tenant, a command, an absolute `cwd`, and
  none of image, replicas, resources, idle and rollout. Port names are
  required DNS labels; two attached services in one sandbox, in any
  environment, do not share a port number. A service and an addon do not
  share a name: both are names of the env table. Two environments with
  attached services in one sandbox do not give it the same name either
  (their addons, their image services, their attached services in that
  sandbox): the sandbox's table includes both environments' tables, the
  first that holds a name answers it, and a process there could reach
  the other environment's.
  Against other environments both are errors, warnings in a plan without
  an environment.
- **Env values**: a string, `{value}`, `{secret}`, `{addon, key, …}` with
  the blueprint's fields, `{service, port, key}` with key `url`, `host` or
  `port`. Every reference resolves; none from an image service or a job
  names an attached service, since image pods cannot reach a sandbox, and
  an attached service names only the attached services of its own
  sandbox, the only ones its sandbox's table names.
- **Jobs**: an image, `on` `release` (the default) or `manual`, retries 0
  to 5, a timeout of 10m by default and 2h at most.
- **Image credentials** (`imageCredentials`, image services and jobs):
  a username and a password, each a string or `{secret}` of the
  environment, the secret existing as an env value's does; not on an
  attached service. A plan resolves an image with the credentials of the
  first service or job, by name, that names it with some, a secret at its
  latest version ([Images](#images)).
- **Addons**: a registered type and a version, both required; the plan
  (the blueprint's default when absent) and the options are the
  blueprint's to check, against the addon that the volume holds when it
  has one (its `addon_type` and `data_version`), else the latest
  release's: a postgres major version change and a valkey downgrade are
  errors, a volume of another type is the warning that its data is
  deleted.
- **Routes**: a path of unreserved characters without empty, `.` or
  `..` segments, unique, of at most 1 KiB (every ingress instance holds
  it); a service and one of its ports; `strip`; `host` is stored and not
  realised (info).
- **Secrets**: every `{"secret": n}` names a tenant secret of the
  environment that exists and is not deleted.
- `volumes` and a service's `mounts` are errors: not available yet.
- At most 1 MiB, 64 services, 32 jobs, 16 addons, 128 routes.

Normalisation fills the defaults, writes durations as Go does, pins
every image to `<registry>/<repository>@sha256:…` (keeping
`imageAsWritten`; a digest reference stays) and every referenced secret
to its latest version, and drops the secrets nothing references. The
normalised spec is what a release stores and what plans diff. A diff
entry is `{kind, name, change, fields: [{path, from, to}], effect}`, the
effect `in place`, `restarts` (a job's template), `replaces N pods` (an
image service's), `recreates (downtime)` (an addon's version, plan or
rendered option), `reprovisions` (an addon's databases or roles), or
`deletes data` (an addon removed, or of another type); a secret shows by
its version, an image as written with its digest.

### Plan, apply, revert, clone

- **Plan** parses, validates, resolves the images (one that does not
  resolve is a warning), normalises and diffs against `base` or the
  current release; it writes nothing. `POST /api/v1/environments/plan`
  validates without an environment.
- **Apply** plans and refuses any error (422 with every finding), pins
  the images outside the transaction (one that does not resolve is now
  an error), then in one transaction locks the tenant FOR SHARE and the
  environment FOR UPDATE (each refused while it is deleted), checks the
  pinned secret versions again, answers a spec equal to the current
  release's with that release (200 `unchanged`, checked before the base,
  so that a no-op on a stale base is no conflict) unless that release
  failed, where the same spec is the retry, checks `base` (409
  `base_mismatch` with `current`), inserts release `current+1` pending,
  supersedes older pending and rolling ones, and sends `env-reconcile`.
- **Revert** to n applies n's stored spec, origin `revert`, with n's
  annotations and `ferun.revertOf: n`; the call takes no annotations of
  its own. A secret version n pins that was deleted is an error.
- **Object changes**, `PUT` and `DELETE …/objects/{kind}/{name}`, apply
  the current spec with the object set or removed (origin `object`), or
  plan it with `dryRun`.
- **Clone** creates an environment (a name or ID taken is 409: a clone is
  not repeated; an ID taken, whoever's environment holds it, is refused
  with `id_taken` before anything is planned under it), sets its secrets
  (`{"copy": true}` the source's latest version that is not deleted,
  opened and sealed again as version 1, `{"value": …}` version 1), and
  applies as its release 1 (origin `clone`, the source release's
  annotations without `ferun.revertOf`, whose number names a release of
  the source) a whole spec of the caller's, or the source release's
  without its attached services and their routes (info findings) and
  with its secret pins reset. It changes no single value of the source's
  spec: a caller that wants other sizes or replicas passes its own spec.
  `data: true` is an error: no data is copied.

### The controller

The leader runs six loops of `env/ctl`, fenced, none with a node call
inside a transaction:

| Loop | Interval | Does |
|---|---|---|
| `env-reconcile` | 2 s, hint `env-reconcile` | a step ([below](#a-reconcile-step)) for each environment that has a release and is not deleted, four at once |
| `env-probe` | 1 s | exits, restarts and readiness of the instances whose rows run without an op on a node this instance is connected to; attached services; at most 16 checks at once |
| `env-groups` | 10 s, hint `env-groups` | per connected node, the groups its rows' templates name and the groups their rules name (`ferun-ingress` among them), put again where the node's differ, and what the node holds of the ingress's groups recorded ([Security groups](#security-groups)); the groups of environments and sandboxes that are gone deleted, once no pod names them, and an `ing-` group of a sandbox that runs elsewhere too |
| `env-names` | 2 s, hint `env-names` | per connected node, the name tables its rows' templates name and the tables they include, updated by a delta or put again where the node's differ; the tables of environments and sandboxes that are gone deleted, once no pod names them ([Names](#names)) |
| `env-volumes` | 30 s | volumes marked deleting, 64 a pass, deleted on their node once no pod holds them (`FAILED_PRECONDITION`: later, at the end of the queue; `NOT_FOUND`: done), then their rows; a removed node's lose their rows only; records of environments' volumes on a connected node that no pod holds and no row places there are deleted |
| `env-delete` | 5 s | the environments of deleting tenants marked deleting; per deleting environment, 16 a pass, the oldest-touched first: its instances deleted, its attached services cleaned up ([below](#attached-services)) before their rows go, its volumes marked deleting, its env group and env table deleted from every connected node; its row goes once no instance, volume, group, table or attached service still to clean is left, and its releases, secrets, runs, incidents and route tokens with it; an environment whose pass did not finish (a volume on a node that is down, a sandbox not woken yet) is touched, so that it goes to the end of the queue and stuck environments never hold the others back |

**The Ensure pool.** The controller runs `Engine.Ensure` for the rows
that should run, and its other slow work (an addon's provisioning, an
attached service's start in a sandbox it wakes, the cleanup of an
attached service that goes), in a pool of 8 workers per instance, one
task per key at a time, each ending with the loop's context. A step
never calls Ensure itself; it enqueues. A paused or hibernated row of a
service that idles is not enqueued: traffic wakes it. Nor is an unknown
one: it may be paused in truth, and the engine's unknown sweep or lost
grace finds out without waking it.

### A reconcile step

The target T is the latest release R, unless R failed: then the realised
release (nothing when none was), so that a failed release is not tried
again until a new one comes. R pending turns rolling when T is R. An
instance's template hash is the sha256 of its rendered template without
labels, with the image, vCPUs and memory; an addon's takes the image as
the blueprint writes it, its tag, so that a new digest under the tag
never recreates it. Instance rows are inserted in a fenced transaction
together with the controller's row (and a job's run).

An instance is **live**, and counts as ready, when its phase is `ready`
and its row runs without an op; for a service that idles, a paused or
hibernated row without an op counts too, so that an idle service still
realises. A `ready` phase left from a pod that is unknown, stopped,
failed or lost does not count: the probes look only at rows that run, so
nothing else resets it. Liveness decides the services' ready counts (the
status shows the same) and their rollouts, how long a restart of a
service waits for each replica before the next, an addon's readiness,
which the release jobs and the services wait for, the names, and when a
crash loop closes. Then:

1. **Addons**, each by its blueprint ([Addons](#addons)):
   - Credentials: system secrets `addon.<addon>.<key>`, 32 hex
     characters (valkey's flags are split at words), version 1,
     generated once and never changed. Without a sealer the release
     stays rolling with the reason "secrets need --secret-key-file", and
     no incident.
   - The volume row, `addon/<name>`, with the plan's size at its first
     creation, never resized, its type and its version.
   - No instance: one is inserted, never idle, pinned to the volume's
     node once the volume was created there, but only when the pod that
     held the volume last has no row and no tombstone left, and never
     onto a removed node (`volume_unavailable`).
   - Another template (a version, a plan, a rendered option): recreated,
     only while the instance runs on a ready node (else it is brought up
     first, a restore), or while it is lost on a node that has its volume
     free (below): the old row retires and goes, the volume records its
     node and the old row as holder, and the new row comes on that node
     once the old pod is gone.
   - Before any create or recreate onto a volume, its data decides:
     another `addon_type` is a removal and an add (the old instance and
     credentials go, the volume is deleted, a new one comes); the same
     type is checked by the blueprint with the volume's `data_version` as
     the previous version, and an error keeps the current row, fails a
     rolling R (`rollout_failed`, the finding its reason) or else opens
     `addon_unhealthy`. `data_version` becomes the version an instance
     ran ready with.
   - While R failed, an addon whose row runs R's template stays as R has
     it, never recreated back.
   - Live, with a provisioning hash that is not the steps': the steps
     run in the container, hold-free, the credentials in their env, at
     most 2 minutes each; their output is redacted of every credential
     before it is stored anywhere; a failure is the instance's detail and
     runs again after 10 s. Databases and roles change the steps, never
     the pod. Not live, the release waits: "addon <a> is <phase>", or,
     with a ready phase, "addon <a> is <state> (<description>)".
   - Whenever the row's pod is materialised on a node (running, paused,
     stopped or failed there, without an op), the volume row's node and
     the row's `pin_node` follow it there: a restore elsewhere took the
     volume along, also when the pod stopped or failed there since.
   - A lost row is replaced only on the node it was lost on, where its
     volume is, and only when that node is ready (not draining) and
     connected to this instance, GetPod there answers `NOT_FOUND`, and
     the volume is free there: never created yet, or GetVolume shows no
     pod or the lost one and AVAILABLE or CHECKPOINTED. Then, in one
     fenced transaction that checks the row is still lost there, the row
     retires, the volume records the node and the lost row as holder,
     the row goes, and a new one comes as for no instance above ("addon
     <a> was lost; it is replaced on node <n>", or "… recreated …" when
     its template changed too). Otherwise it is never replaced:
     `volume_unavailable` opens, "addon <a> is lost with node <n>, which
     has its volume" when the node was not asked or still has the pod,
     else "addon <a> is lost on node <n> and not replaced there: <why>",
     the why "the node does not have volume <v>", "volume <v> is
     attached to pod <p> there" or "volume <v> is <STATE> there"; Ensure
     stays queued to sync the row if the node returns with it. An addon
     without a volume (nats without JetStream) is never replaced when it
     is lost.
2. **Jobs** `on: release`, while R rolls: one whose template hash has no
   succeeded run runs once every addon is provisioned. Exit 0 succeeds;
   another code, none ("the node did not report an exit status"), its
   timeout or a lost row fail the attempt, which runs again while
   retries are left; the run keeps the last 50 lines of the log before
   its row goes. A failed last attempt fails R (`job_failed`). A job runs
   at most once at a time per environment, across releases: while a run
   of it that is not R's runs (a superseded release's, a manual one,
   another template's), R's waits ("job <j> waits for its run of release
   <n> (attempt <k>)") and never cancels it; that run's success under
   the same template hash serves R, and otherwise R's attempt 1 starts
   once it ended. A run's insert checks under the environment's row lock
   that no run of its job runs. Any job of T runs as a new attempt 1 on
   `POST …/jobs/{name}/run`, refused while a run of it runs.
3. **Image services**, while R rolls only after the addons are
   provisioned and the release jobs succeeded: rows of T's template are
   kept (their release and idle updated in place) and healed; the others
   are replaced one at a time, surge 1: a new one, ready, then an old
   one retires (`retire_at` 10 s ahead: routes drop it at once, the env
   table within a pass of `env-names`, and the answers that named it
   expire after their 5 s) and goes; surplus replicas retire from the
   highest; a lost row is replaced. The service's `rollout.timeout`
   counts from R's start, moved on by the time R's release jobs waited
   for runs that were not R's (see the release's deadline below): past
   it, or once one of R's new instances exited 3 times, R fails
   (`rollout_failed`, with the exits, the log tail and the diff since
   the realised release), and R's new instances retire while the old
   serve on. Healing, always: a desired row stopped, failed or pending
   gets Ensure, and one unknown, paused or hibernated too when its
   service does not idle ([the pool](#the-controller)).
4. **Attached services** ([below](#attached-services)),
   ready within 10 minutes of R's start, moved on in the same way, or R
   fails (`rollout_failed`).
5. **Removed objects**: their instances retire and go; an addon neither T
   nor R has loses its volume (marked deleting) and its credentials,
   while an addon of a failed R that T lacks stays, so that a failed
   first release deletes no data; an attached service is cleaned up, its
   process stopped, a sleeping sandbox woken for it, before its row, and
   its name with it, goes. The open `crash_loop`,
   `volume_unavailable` and `addon_unhealthy` incidents of objects that
   neither T nor R has close.
6. **Realised**, when T is R: every object as R has it and ready, addons
   provisioned, release jobs succeeded, nothing retiring. R is
   `realised`, the environment's `realised` with it, older realised and
   pending releases are superseded, and the open `rollout_failed`,
   `job_failed` and `crash_loop` incidents of older releases close.

**The release's deadline.** A rolling R fails (`rollout_failed`) once it
rolled longer than the longest of T's image services' `rollout.timeout`,
T's release jobs' timeout × (retries + 1), and 10 minutes, naming the
first object it still waits for; the evidence lists what it waits for
and those instances' details, credentials redacted. R's budgets, this
deadline as well as its services' `rollout.timeout` and its attached
services' 10 minutes, count from R's start moved on by the time its
release jobs waited for runs that were not R's: to the latest end of a
run of such a job by another release that ended after R started (the
present while one runs), unless a run of R's template of the job had
succeeded before R started. The shift is worked out on every step, so
that R keeps it once its jobs are done; the reason names the time R had
from its own start. A release without a sealer is spared. An instance
pending without capacity for 2 minutes opens `no_capacity`, with the
placement's text, which closes once it places. An object of T with a pod
on a node that serves no names opens `names_unserved` ([Names](#names)).

### Probes, exits and restarts

`env-probe` reads each running instance's pod with the lease its row
gives, without a hold:

- **An exit** of the container `main`, with its code from GetPod: a job's
  run ends; a service's or an addon's instance counts a restart, keeps
  the exit (the last 5), is `exited` ("exited with code 1; restarting in
  4s"), and is restarted after 2^(n-1) s, at most 5 minutes, by
  `Engine.Restart`, a repair op of trigger `restart`: the precheck allows
  it on any running row, it skips the repair's recheck, and it stops and
  starts the pod. An exit counts only for a new run: the same microVM pid
  with no restart due is a restart that did not happen ("repair not done:
  …"), scheduled again without counting. 3 exits within 10 minutes open
  `crash_loop` (the exits, the log tail), which closes once every
  instance of the object has been ready for 10 minutes and is live, or
  at a later realisation. Restarts count from zero again after 10
  minutes ready.
- **Readiness**: http, a GET of the path over a port-forward with `Host:
  localhost:<port>`, 200 to 399 within 2 s; tcp, a port-forward that is
  answered; exec, exit 0 in `main` within 10 s; an addon's, its
  blueprint's probe, its output redacted. `starting` becomes `ready` on
  the first success after the initial delay, `ready` becomes `unready`
  after the threshold's failures in a row, and `ready` again on a
  success. Without a check, an instance is ready once its container has
  run 2 s since it was first seen running. Routes use only `ready`
  instances.

The checks use exports of the port door, `Probe` and `DialPod`, which
take no hold and touch nothing, each within 2 s and with one of
`ports.ProbeSlots`, 4 probe slots per sandbox and instance apart from its
32 streams, so that forwarded traffic never fails a check; with every
probe slot taken a check fails at once with `ErrProbeBusy`, which counts
neither way. The runner's detached calls (`tools/detached.go`: exec,
process start, list, logs and stop) take no hold, record no activity and
touch nothing; a process's env travels in its exec request and is never
written down. `VolumeService`, `SecurityGroupService` and `NameService`
calls count as lifecycle calls in the node connections' budgets.

**Logs**: `StreamContainerLogs` with `tail_lines` (200 by default, 10000
at most), never followed; the guest also stops a tail 4 MiB from the
end ([guest.md](guest.md#containers)), a node without that cap does
not. ferund cuts each line it reads to 8 KiB, the "… (cut)" marker
included, and keeps at most 4 MiB of a logs answer, dropping the oldest
lines while it reads. A node that answers Unimplemented is 501 "this
node does not serve container logs yet", a paused or hibernated instance
409 "paused" (never woken for it), an instance that does not run 409
`not_running`. A replica the object has no row for is 409 `not_running`
"<kind> <name> has no instance of replica <r> yet: no log to read"
below the replicas the target release asks for, "… runs no replica
(replicas: 0): no log to read" for none, and 404 "<kind> <name> has no
replica <r> (it has <n>)" beyond them. A job's log is its latest run's,
the stored tail once its row is gone; an attached service's is its
process log, bounded by the tools' output cap (50 000 bytes) rather
than by the line budget.

**Stored tails.** A job's run, and the evidence of `crash_loop` and of a
service's `rollout_failed`, keep an instance's last 50 lines, read with
the 8 KiB line budget, redacted, then cut to 2 KiB a line and 64 KiB in
all; `job_failed` copies the run's. Redaction comes before the cut, so
that no part of a value the cut splits is kept, and replaces with
`[redacted]` every value of four bytes or more, also as it reads escaped
in a URL's userinfo, of: the credentials of every addon of T and R, the
environment's tenant secrets at their latest versions and at those T and
R pin, and the secrets the instance's own template names. It is best
effort: a value in another encoding is not caught. A logs answer read
from a running instance is not redacted.

### Security groups

The controller's groups are `env-<env id>` and `sbx-<sandbox id>`, and
the ingress's `ing-<sandbox id>` and `ferun-ingress` (below). The first
two carry the egress table: any protocol to `0.0.0.0/0` except `0.0.0.0/8`,
`10.0.0.0/8`, `100.64.0.0/10`, `127.0.0.0/8`, `169.254.0.0/16`,
`172.16.0.0/12`, `192.0.0.0/24`, `192.0.2.0/24`, `192.88.99.0/24`,
`192.168.0.0/16`, `198.18.0.0/15`, `198.51.100.0/24`, `203.0.113.0/24`,
`224.0.0.0/4` and `240.0.0.0/4`, and to `2000::/3` except `2001::/23`,
`2001:db8::/32`, `2002::/16` and `3fff::/20`, feosd's internet sets; never
`::/0` or a bare `0.0.0.0/0`. DNS and the metadata service need no rule.

- **The env group** adds ingress and egress of any protocol from and to
  itself. Its members are the IPv6 addresses of the environment's
  instance rows, in any phase until the row goes, and of the sandboxes
  its attached services run in, while the service may use them.
- **The sbx group** of an attachable sandbox adds egress to each env
  group the sandbox's attached services belong to, and no ingress: image
  pods never reach a sandbox.
- **The ing group** of an image service's pod of an environment whose
  `ingress_group` is set ([Public environments](#public-environments))
  is the pod's own: the controller adds `ing-<sandbox id>` to the row's
  groups at its insert. While the environment is public it allows TCP
  in on the pod's routed ports from the group `ferun-ingress`: the
  numbers, in the pod's own release, of the ports that the routes of the
  realised and the latest release give its service. Otherwise, or
  without routed ports, it has no rules; it never has members. Addons,
  jobs and attachable sandboxes name none.
- **`ferun-ingress`** has no rules; its members are the mesh addresses of
  the ingress instances that are not removed. It is never deleted.
- **Versions.** An env group's is `environments.gen`, counted up when a
  node holds the group at that version with other content; an sbx or ing
  group's is a hash of its content; `ferun-ingress`'s is
  `pod_network.ingress_group_version`, counted up under that row's lock
  in the transaction that changes the members, so that its versions
  follow commit order. Put ignores versions, so a group is put again
  whenever version or content differ from the node's.
- **What a node holds of the ingress's groups** is recorded after every
  put, and on every pass from the node's list when it has the wanted
  content already: an `ing-` group's ports in
  `env_instances.ingress_ports`, the version of `ferun-ingress` in
  `nodes.ingress_group_version`. The
  ingress sends to an instance only on those ports and only when that
  version admits it ([ingress.md](ingress.md#groups-and-admission)). Two
  puts that race (the prepare hook's and a pass's) can leave a record
  wrong for one pass; the next one records again. An `ing-` group a node
  holds that none of its rows wants is deleted when its sandbox is gone
  or placed on another node, once no pod there names it.

**Attachable sandboxes.** `POST /api/v1/sandboxes` with `"attachable":
true` gives the sandbox a template: `sleep infinity`, no env, the group
and the name table `sbx-<id>`; its status says `attachable`. Only such a
sandbox reaches an environment's addons and image services, and
resolves their names.

**The prepare hook** (`Engine.SetPrepareHook`) runs for a row with a
template once its op has the node's connection and its pod spec (a
secret that does not resolve fails first), before CreatePod, StartPod,
ResumePod and RestorePod, and inside a repair before its pod calls. The
controller's hook puts the row's groups, and those their rules name, on
the node, whole, recording what it put of the ingress's groups, and its
name tables, and those they include, as
`env-names` does ([Names](#names)). Before a create it also sees to the
volumes: one never
created is created on the node with its size and labels, then recorded
provisioned, with its node, its holder and the row's `pin_node`, in a
transaction of its own; one created is read on the node and passes when
it is AVAILABLE, CHECKPOINTED without a pod, or held by this pod, else
`volume_unavailable` opens. A hook error before the pod call means
nothing was sent: the op rolls back with the error as its cause and
description ("…; nothing was sent"). Only an error wrapping
`sandbox.ErrNodeUnfit`, and not `model.ErrNodeUnavailable`, puts the
node of an unpinned create on `avoid_nodes`: a create of the volume
that the node refuses (`ALREADY_EXISTS`, `FAILED_PRECONDITION`,
`RESOURCE_EXHAUSTED`, `INVALID_ARGUMENT`), a volume the node does not
have, has attached to another pod, or has in another state. A node that is
not connected never reaches the hook. Inside a repair a hook error is a
repair not done: the pod's next observation decides, process holds stay,
and a running pod's description reads "repair not done: <cause>".

### Names

`feosd`'s resolver answers an environment's names (ADR 0010, ADR 0018)
from the name tables the controller puts on the nodes
(`feos.network.v1.NameService`), as it puts the groups:

- **The env table**, `env-<env id>`, which the templates of the
  environment's service and job instances name (an addon's names none:
  it reaches nothing by name), names T's addons and image services with
  the IPv6 addresses of their live replicas; while none is live, those
  of the replicas whose row's state is known; only then those of unknown
  ones, whose node is out of touch and whose address may be stale; never
  a retiring one's. An environment without a release, or being deleted,
  has an empty table.
- **The sbx table**, `sbx-<sandbox id>`, which an attachable sandbox's
  template names from its creation, names the attached services whose
  rows name the sandbox and may use it, each as `127.0.0.1` and `::1`,
  and includes the env tables of their environments, which the node
  looks in after the sandbox's own names. A sandbox that gains or loses
  attached services keeps its pod: only its table changes.
- **Versions** are a hash of a table's ID, names and includes, never 0,
  so that equal tables have equal versions on every node and a delta
  means the same wherever it applies.
- **`env-names`** compares, per connected node, the tables its rows'
  templates name, and those they include, with what the node lists: a
  table the node lacks, or has with other includes, is put whole; one
  whose names differ is updated by a delta from the node's version, the
  names to put and those to remove, and put whole when the node refuses
  it (`FAILED_PRECONDITION`); the tables of environments and sandboxes
  that are gone are deleted once no pod names them. A replica whose
  phase moves, one that retires or goes, and an attached service
  recorded, moved, failed or dropped send `env-names`. The prepare hook
  puts a row's tables before its pod call.
- **A node without names**: a `feosd` from before the `NameService`
  answers `UNIMPLEMENTED`. `env-names` and the prepare hook leave the
  node without tables and remember it until it answers; its pods start
  without names (it ignores the field), and the reconcile step opens
  `names_unserved` on each object of T with a pod there, an instance or
  an attached service's sandbox ("node <n> does not serve names (its
  feosd predates the NameService): service/web does not resolve the
  environment's names there"), which closes once the object has none
  there. A new leader finds such a node again within a pass.
- **On the node**, a name of one label that a pod's tables hold is
  answered with all its addresses and a TTL of 5 s, nothing to another
  type; every other name goes upstream ([node.md](node.md#dns)). Nothing
  of it is in the guest: a paused, hibernated or moved pod resolves its
  names as soon as it runs.

A pod's tables are in its template, and so in its template hash: the
first ferund with names rolls every image service and gives every job a
new template once. A sandbox made attachable before has no table, and
resolves no names until it is created again.

### Attached services

An attached service is a supervised process in the main container of a
sandbox of the tenant's, named `ferun-env-<env>-<service>`. The step
records it with the sandbox's incarnation, moves it when the spec names
another sandbox, and wakes a sandbox that does not run (Acquire with an
exec hold) to start its process, holding it up to a minute for the first
readiness, and to clean up after it (below). The supervisor in
`env-probe` looks at it while the sandbox runs, and never wakes a paused
one for that: it starts the process with argv, cwd and the resolved env,
after stopping any other running process of that name that no other
attached service of the sandbox records; a changed template stops the
old process and starts the new; a process that is gone counts as an exit
and is restarted with the backoff above; a cwd that does not exist yet
keeps it `starting` ("cwd <dir> does not exist yet"); readiness is
checked against the sandbox's port, or by exec. It lists only the
processes of the service's name, as compact JSON; a listing cut at the
tools' output cap is an error, never a shorter list. A detached start
first removes the records of the processes of its name that are over
(exited, stopped, or lost with a cold restore) but the newest 3 by start
time; a running process keeps its record, and so does one whose start is
not fully recorded yet. Attached services have no `crash_loop`. A
restart stops and starts the process, never the sandbox's pod; in a
paused sandbox it is 409 "paused". It never counts as an exit: the row
forgets the process before the stop, and a look of the supervisor
writes only onto a row that still records the process it looked at.

The service may use its sandbox while that exists, is the environment
owner's and is no instance row. Otherwise the service is `failed`,
"sandbox <id> is gone or not this tenant's", and is never started,
checked, routed to or made a group member there; another tenant's
sandbox under its ID stays refused. A new incarnation of the owner's
sandbox under the same ID is one in which the service starts anew: the
row records the new incarnation and forgets the process, which went with
the old one, its phase is `starting`, and its restarts, next restart,
probe failures and detail are reset; a new sandbox that sleeps is woken
for the start. (A client cannot make a sandbox under an ID of its choice
any more, [ADR 0019](../adr/0019-pod-ids.md): a replaced sandbox has a
new ID, which the environment's spec names in a new release.)

**Cleanup.** A service that goes, removed from T, moved to another
sandbox or with its environment deleted, has its process stopped before
its row goes or moves, and its name in the sandbox's table with it. A
running, paused or hibernated sandbox is cleaned in the pool: Acquire
with an exec hold, which wakes a sleeping sandbox and keeps a running
one from pausing meanwhile, then, under the service's supervision lock
and with the row as it is then, the stop (a process that is not there
any more counts as stopped). Only then does the row go, while it still
names the sandbox and process that were cleaned, or, for a move, forget
its process, so that the next round moves it, its counters reset.
Nothing is left to clean when the service never started a process, or
when its sandbox is gone, another tenant's, an instance row, a new
incarnation (the old process went with the old one), or stopped, failed
or lost (no process outlives a boot). A sandbox in an op, between states
or on a node out of reach is looked at again in a later round. A
cleanup that fails keeps the row and is tried again after the restart
backoff (1 s, doubling up to 5 minutes). Until a move has cleaned the
old sandbox, the row, and the route door and the old sandbox's table
with it, names the old one.

**References** resolve thus: an addon's output (`{addon, key, …}`) to the
blueprint's parts against the addon instance's address (in brackets in a
URL), so that a dependent waits while the addon has none, and gets a new
template when it changes, rolled as a release would be; a service's
(`{service, port, key}`) to its name, its port number or
`http://<name>:<port>`, which the env table names; a secret to its
pinned version.

### The route door

`ANY /api/v1/environments/{env}/http/{path...}` has an auth class of its
own, `route`: a route token of the environment and plain-text answers.
The door reads the token from `X-FeRun-Route-Token`, and from
`Authorization: Bearer` only when that header is absent or blank, as
older clients send it. A token that is no route token, a tenant's
included, is 401 "route token required" in either place, and a wrong
one in `X-FeRun-Route-Token` is never made good by `Authorization`; a
route token of another environment is 404 "no such environment".
`POST /api/v1/environments/{env}/route-tokens` mints one (32 random
bytes, base64url, shown once, only its SHA-256 kept); `DELETE` revokes
them all. The door matches the longest prefix, segment by segment, over
the routes of the realised release (the latest while none is realised;
no match is 404 "no route for <path>") and forwards with the port door's
`ForwardTo`, naming the headers that carried the token: a forward hold
and a touch, the Host passed on, upgrades and streams kept, a
Kubernetes binding's port refused. `X-FeRun-Route-Token` and
`Proxy-Authorization` stop at the door; with the token from
`X-FeRun-Route-Token` the client's `Authorization` reaches the upstream
unchanged, unless a bearer token in it (in any of its values, the scheme
in any case) is a FeRun credential, a tenant's or a sandbox's token,
which is 401 "route token required" with nothing forwarded; with the
token from `Authorization` that stops too. A failure
inside ferund (the database, a stored spec that does not parse) is 500
"the route door failed", its cause only in the log, since the door
serves anonymous clients; a refusal below 500 keeps its text.

- An image service's request goes to a ready instance whose sandbox
  runs, round-robin; with none, to a paused or hibernated one, which the
  forward wakes (scale from zero); with none at all, 503 "no ready
  instance of <service>" with `Retry-After: 5`. The port is the one the
  chosen instance's own release gives the route's port name.
- An attached service's goes to the sandbox of its `env_attached` row
  while that exists, is the owner's and is no instance row, a new
  incarnation of it too, where the controller starts the service anew.
  The door does not look at the service's phase: a port nothing listens
  on yet is the port door's 502. Else 503 "sandbox <id> of service
  <service> is gone or not this tenant's", and without a row yet 503
  "no ready instance of <service>", both with `Retry-After: 5`.
- The path loses the route's prefix only with `strip`; the query stays.
  A segment that is, or decodes once or twice to, `.` or `..` is 400
  ("the path has a .. segment"); `%25` passes. Go's mux redirects (307) a
  plain `..` and `/http` without its slash before the door.

FeCode's environment hosts forward to the door with the environment's
route token in `X-FeRun-Route-Token`, and pass the browser's
`Authorization` on to the application.

The door serves every environment, with its route tokens. Beside it,
`ferun-ingress` serves the routes to image services of the environments
whose tenant made them public, at their default hosts and without a
token, straight to the pods ([Public environments](#public-environments));
routes to attached services are the door's alone. A plan's info finding
at each route says which doors serve it, and the status names each
route's `door`.

### Secrets

A tenant secret is named `^[a-z0-9][a-z0-9._-]{0,62}$`, never beginning
with `addon.`, the system secrets' prefix, and holds at most 64 KiB; a
new value is a new version, the highest ever plus one, also after a
delete. Versions are sealed with `internal/seal` under
`--secret-key-file`'s key, the associated data `env_secrets|<env
id>|<name>|<version>`; without the key the secrets routes answer 503
"secrets need --secret-key-file" and nothing that names a secret or has
an addon is realised. The controller resolves them (`ResolveSecret`,
also the engine's resolver) into a pod's env at its create, a process's
and a provisioning step's, and writes them nowhere. A delete is refused
(409) while the latest release pins the secret, and soft-deletes every
version, so that an older release still resolves.

**Known exposure.** A resolved env reaches feosd's record of the pod,
`GetPod` and `ListPods`, the node's disk and hibernation checkpoints in
the clear ([follow-ups](#follow-ups-for-feosd), item 10), and the nats
addon's password is in its server's argv inside the pod. A value an
application writes to its log reaches the logs answer as it is; the
tails ferund keeps are redacted, but only of the values as they are and
as a URL's userinfo escapes them ([Logs](#probes-exits-and-restarts)).

### Addons

`env/addons` registers a blueprint per type: its versions, its plans and
default plan (`small` for all three), its validation of an addon against
the previous one, the keys of its credentials, its pod (image, command,
env with credential parts, port, mount, probe), its outputs, and its
provisioning steps, idempotent. A version is required. A plan change
keeps the volume's size (a warning). An output before the addon has an
address names the addon instead (validation only); a URL's userinfo is
percent-encoded.

| Type | Versions | Plans: vCPUs, memory, volume | Pod | Outputs |
|---|---|---|---|---|
| `postgres` | 16, 17 | `dev` 1, 256 MiB, 1 GiB; `small` 1, 1 GiB, 10 GiB; `medium` 2, 4 GiB, 50 GiB; `large` 4, 16 GiB, 200 GiB | `postgres:<v>-alpine`, `docker-entrypoint.sh postgres -c shared_buffers=<memory/4>MB`, `PGDATA=/var/lib/postgresql/data/pgdata`, `POSTGRES_INITDB_ARGS=--auth-host=scram-sha-256` (loopback asks for a password too), the volume at `/var/lib/postgresql/data`; probe `pg_isready -U postgres -h 127.0.0.1` | `host`, `port` (5432), `user`, `password`, `database`, `url` `postgres://<user>:<password>@[<address>]:5432/<database>?sslmode=disable`; fields `role` and `database` |
| `valkey` | 7.2, 8 | `dev` 1, 128 MiB, 256 MiB; `small` 1, 512 MiB, 2 GiB; `medium` 1, 2 GiB, 8 GiB; `large` 2, 8 GiB, 32 GiB | `valkey/valkey:<v>-alpine` with its own command and `VALKEY_EXTRA_FLAGS` `--appendonly yes --requirepass <password> --maxmemory <70 % of memory>mb --maxmemory-policy <policy>`, the volume at `/data`; probe `valkey-cli ping` with `REDISCLI_AUTH` | `host`, `port` (6379), `user` (`default`), `password`, `url` `redis://default:<password>@[<address>]:6379/0` |
| `nats` | 2 | `dev` 1, 128 MiB, 1 GiB; `small` 1, 512 MiB, 10 GiB | `nats:2-alpine`, `sh -c` writing a config that binds the monitor to loopback, then `nats-server -a :: -p 4222 --user nats --pass "$NATS_PASSWORD"`, with `-js -sd /data` and the volume at `/data` while option `jetstream` (default true) holds; probe `wget -qO- http://127.0.0.1:8222/healthz` | `host`, `port` (4222), `user` (`nats`), `password`, `url` `nats://nats:<password>@[<address>]:4222` |

- **postgres** options: `databases` and `roles`, names
  `^[a-z][a-z0-9_]{0,62}$`; no role `postgres`, `public`, `none`,
  `current_user`, `current_role`, `session_user`, `information_schema` or
  `pg_*`, no database `template0`, `template1` or `postgres`; roles need a
  listed database. A major version change either way is an error.
  Provisioning is one step, `sh -c` with a heredoc into `psql` (`\gexec`,
  `ON_ERROR_STOP`), the superuser's password in `PGPASSWORD` and each
  role's in `FERUN_ROLE_PASSWORD_<role>`: the databases and roles that are
  missing are created, every role's password is set, and each role gets
  CONNECT and CREATE on every listed database and a schema of its name,
  its own, in each; such a schema that another role owns fails the step.
  Nothing is dropped: a database or role no longer listed stays (a
  warning). A role's output needs one of the listed databases;
  `database: postgres` is the superuser's alone.
- **valkey**: option `maxmemoryPolicy`, `noeviction` by default, one of
  the eight policies. A downgrade is an error, an upgrade an info.
- **nats**: JetStream turned off is a warning; its volume stays, unused.

`FERUN_ADDON_DOCKER=1 go test -run Docker ./internal/env/addons/` runs
each blueprint against its image in docker, as FeOS would start it.

### Status and incidents

`GET /api/v1/environments/{env}` answers the environment, its rollout
(the latest release's state, start and reason), its objects as the
target release has them (an image service with desired and ready counts,
the ready ones its live instances, and its instances, an addon with
type, version, plan, phase and `provisioned`, a job with its last run,
an attached service with its sandbox, phase, restarts and last exit;
`starting` before an object has an instance), its routes (of the
realised release, else the latest), each with its `door`, `ingress` with
its `url` (`https://<host><prefix>`) for a route to an image service of
a public environment and `route` for every other, `public`, its `hosts`
(its default host while it is public, the platform has an apps zone and
it is not being deleted; otherwise empty), and the number of open
incidents. The listing shows `public` and `hosts` too. An
incident has its object, release, condition, summary, opening and
closing, and evidence: `exits`, `logTail` (redacted and bounded), `diff`
(since the realised release), `placement`, and `pending`, what a release
past its deadline still waited for with the details of those objects'
instances, credentials redacted. A failed release's reason is its
incident's summary.

### The environments' REST API

Tenant class, JSON in camelCase. `{env}` is the environment's UUID or its
name; one the tenant does not have is 404 "no such environment".

| Method and path | Answers |
|---|---|
| `POST /api/v1/environments` `{name, id?, labels?}` | 201 its status; the same ID and name again 200; 409 `name_taken`, `id_taken` |
| `GET /api/v1/environments?label=k=v` | `{environments: [{id, name, labels, release, realised, state, public, hosts, openIncidents, createdAt}]}`, the labels ANDed; `state` is the latest release's, `none` or `deleting` |
| `GET`, `DELETE /api/v1/environments/{env}` | the status; 202 |
| `PUT /api/v1/environments/{env}/public` `{public}` | `{public, hosts, rolling}`; 400 without a boolean `public`; 409 while the platform has no apps zone (publishing) or the environment or its tenant is being deleted ([Public environments](#public-environments)) |
| `POST /api/v1/environments/plan` `{spec}` | `{valid, findings}` |
| `POST …/{env}/plan` `{spec, base?}` | `{valid, findings, diff, base, current}` |
| `POST …/{env}/releases` `{spec, base, author?, message?, annotations?}` | 201 the release; 200 with `unchanged: true`; 422 `invalid_spec` with `findings`; 409 `base_mismatch` with `current` |
| `GET …/{env}/releases?limit=50`, `…/releases/{n}` | newest first, with their tags; one with its spec and its diff against n-1 |
| `POST …/releases/{n}/revert` `{base, author?, message?}` | as a release |
| `PUT`, `DELETE …/{env}/objects/{kind}/{name}` | as a release; with `dryRun` a plan |
| `GET`, `PUT`, `DELETE …/{env}/tags[/{tag}]` | the tags, a tag (`PUT {release, author?}`), 204 |
| `POST …/{env}/clone` `{name, id?, release?, spec?, labels?, secrets?, author?, data?}` | 201 the new environment's status; 409 `id_taken` before the spec is planned, `name_taken`; 422 findings |
| `GET …/{env}/secrets`, `PUT`, `DELETE …/secrets/{name}` | the names with their latest versions; `{name, version}`; 204, 409 while pinned |
| `GET …/{env}/incidents[?open=1]`, `…/incidents/{id}` | newest first; one |
| `GET …/{env}/objects/{kind}/{name}/logs?lines=200&replica=0` | `{lines, source}`, the source `container` or `process`; 501; 409 "paused", `not_running` (also for a replica without an instance yet, or none at all); 404 for a replica beyond the object's ([Logs](#probes-exits-and-restarts)) |
| `POST …/{env}/services/{name}/restart` | 202: an image service's replicas one at a time, each waited for until it is live again, up to 5 minutes, or an attached service's process |
| `POST …/{env}/jobs/{name}/run` | 202; 409 `busy` "job X runs already (release N, attempt M)" with `Retry-After: 30` while a run of the job runs, of any release; 409 `not_ready` |
| `POST`, `DELETE …/{env}/route-tokens` | 201 `{token}`; 204 |
| `ANY …/{env}/http/{path...}` | the route door |

Other answers: plain 409s "environment X is being deleted", "tenant X is
being deleted", "release N pins secret X: apply a release without it
first", "the platform serves no apps zone (--apps-zone); an environment
cannot be public" and "paused"; 409s with a code for states the
controller expects: `busy` (with `Retry-After`) while the object has an
op in flight or, for
a job, a run, `not_running` for an instance that does not run,
`not_ready` for a job whose template cannot render yet ("job X cannot
run yet: …"); 404 "no such release", "no such tag", "no such secret",
"no such incident", "no such object: <kind>/<name>" for an object the
target release does not have; 503 "secrets need --secret-key-file". An
author has at most 1 KiB, a message 64 KiB, annotations 4 KiB, labels 64
of at most 253 bytes. Not built: MCP tools and grants for environments,
an admin listing, volumes for services, a copy of data on a clone, addon
backups and minor upgrades, a CLI.

## The ingress

`ferun-ingress` ([ADR 0021](../adr/0021-ingress.md),
[ingress.md](ingress.md)) serves public environments' hosts,
`https://<label>.<apps zone>/…`, straight to their image services' pods:
a program of its own and a peer on the pods' mesh, which connects out to
`ferund` on the nodes' port as a node does and serves from the table
`ferund` streams to it. `ferund` never calls it, carries none of its
traffic, and any instance serves any of its instances. This section is
`ferund`'s part; ingress.md describes the whole.

### Public environments

The route door serves every environment, with its route tokens. The
ingress serves an environment only once its tenant made it public, `PUT
/api/v1/environments/{env}/public {"public": true}` (tenant class):

- **The first publish**, in one transaction under the environment's row
  lock, gives the environment its host label, sets `ingress_group` and
  `public`, sends `ferun_ingress` `env:<id>` and wakes `env-reconcile`
  and `env-groups`. `ingress_group` puts `IngressGroup` into its image
  services' templates, part of the template hash, so the services roll
  once and their pods name `ing-<sandbox id>` ([Security
  groups](#security-groups)); the answer says `rolling: true`.
- **Unpublishing** takes the environment out of the ingress's table
  within about 100 ms; its `ing-` groups lose their rules in the next
  `env-groups` pass. Label and `ingress_group` stay, so publishing again
  rolls nothing.
- **Refusals**: 409 for publishing while `--apps-zone` is empty, the
  platform's switch, and for either while the environment or its tenant
  is being deleted. A clone never copies `public`; deploying the ingress
  makes nothing public.
- **The label** is `<environment>-<tenant>` when it names that pair
  alone: the name has no hyphen, the tenant no dot, it does not end in a
  hyphen and 16 hex characters, and it is a DNS label of at most 63
  characters without `--` as its third and fourth characters. Else it is
  at most 46 characters of the name, a hyphen and 16 hex characters of
  the SHA-256 of the environment's UUID ([Host
  labels](ingress.md#host-labels)). `host_labels` registers it to the
  tenant that first claimed it, in one atomic upsert: another tenant
  never gets it, and its owner may claim it again once its environment
  is gone. It never changes while the environment lives.

### The ingress service

Every instance serves `ferun.ingress.v1.IngressService`, JSON over gRPC
on the nodes' port under ALPN `ferun-ingress/1` ([The nodes'
port](#the-nodes-port), [api.md](api.md#the-ingresss-api)), from
`internal/ingress/service`:

- **Join** turns a one-time join token of kind `ingress`,
  `feos-ingress-join-<secret>`, into the instance's name and first
  certificate, `spiffe://<td>/ingress/<name>`; a token pinned to a name
  joins it again (its certificates revoked, its WireGuard key cleared and
  retired, its /96 kept). **RenewCertificate** renews only the
  instance's newest certificate (the current one, or, while none is, the
  newest not revoked), never one revoked, also from a time ahead, for its
  own key alone, and not before its renew time less 10 minutes, so that a
  copy of a certificate does not outlive the instance's own. **Hello**
  makes the presenting certificate current, superseding the older ones
  10 minutes ahead, and puts the instance on the mesh with its fixed key
  ([The pods' mesh](#the-pods-mesh)); its answer is the ULA, the
  instance's /96 and address, the apps zone and the answering instance.
  `ingress_certs` records every certificate.
- **Sync** streams the instance's table: whole, then the environments
  put and removed, the mesh's nodes when their version changes, `Self`
  (its address, the apps zone, its admitted version of `ferun-ingress`)
  when its row changes, and an empty table every 10 s that records the
  instance as seen. A stream ends `UNAVAILABLE` when its instance's
  table builder has not read the database for 30 s.
- **The table builder** (`internal/ingress/table`) runs on every
  instance that has a stream: it builds each public environment's entry
  from the database (routes of the realised release, else the latest;
  endpoints with their admitted ports; asleep, waking and pending
  counts), rebuilds an environment on `ferun_ingress` `env:<id>` or
  `ferun_sandbox` of one of its instances after 50 ms, the mesh on
  `mesh`, and everything on `*`, a reconnect and every 5 s. A replica's
  change reaches the ingress about 100 ms after its commit.
- **Wake** wakes exactly the asleep instances of a routed image service
  of a public environment, each once at a time, in the background
  (`Engine.Sync`, then `Engine.Ensure`, within the preparation budget),
  never on the caller's context.
- **Hold** admits, under the sandbox's row lock, a hold of kind
  `forward` with the holder `ingress:<name>` and a lease of 30 s on a
  running service instance of a public environment without an op;
  releases holds; and with `leaving` releases all of the instance's and
  sets `ingresses.left_at`. The holds keep idle services from pausing
  under a request; the [idle fence](ingress.md#the-idle-fence) covers an
  instance cut off from `ferund`.
- **Following the others' writes.** `ferun_ingress` `ingress:<name>`
  re-checks that instance's connections and has its streams read its
  row (a removal ends them with `PERMISSION_DENIED`); `*` and a
  reconnect re-check every connected instance.
- **Stopping** ([Stopping](#stopping), step 1): new ingress connections
  close, the streams end with `UNAVAILABLE` "instance stopping", Wake and
  Hold answer `UNAVAILABLE`.

### The ingress's admin API

- `POST /api/v1/admin/join-tokens` takes `kind`, `node` (the default) or
  `ingress`, and `name` (`node` is its older alias; both given and
  different is 400), and answers `{id, token, expiresAt, kind, name,
  node}`, so that an operator sees a node's token issued by an instance
  that knows no kinds; an ingress's token is
  `feos-ingress-join-<secret>.<pin>`. `GET` lists each token's `kind`.
  `ferund join-token --kind ingress [--node <name>]` issues one without
  REST.
- `GET /api/v1/admin/ingresses` lists the instances, removed ones
  included: name, status, joined and removed, mesh prefix, address,
  endpoint and public key, version, last seen and left, `connected` (seen
  within 30 s, on the database clock), `admittedVersion`, the
  certificate's expiry.
- `DELETE /api/v1/admin/ingresses/{name}` removes an instance (204, or
  404): a status, never a deleted row. Its certificates are revoked, its
  streams and connections closed on every instance, and it leaves the
  mesh's peers and `ferun-ingress`; a token pinned to its name joins it
  again.

### Domains

A domain ([ADR 0021](../adr/0021-ingress.md)) is a tenant's claim on one
name, proven by a TXT record of the claim's token at
`<--domain-challenge-label>.<name>`, bound to at most one environment of
the tenant, and routed to the ingress by a CNAME to `--edge-name`, an
ALIAS, or A and AAAA records of the edge's addresses. `internal/domains`
holds the claims, their bindings, their checks, the states derived from
them and the incidents of their environments. With `--edge-name` empty,
domains are off: no claim is made, no check runs.

**Not built yet: certificates**, and with them serving a domain. They
come in a later step with the ingress. Until then a verified, bound and
routed domain waits in `issuing`, the answer's `certificate` is always
null, and the states `served`, `dns-moved` and `failed`, which the schema
allows, do not occur.

**The routes**, tenant class, JSON in camelCase. `{id}` is the domain's
UUID; a domain of another tenant, or anything that is no UUID, is 404 "no
such domain". `{env}` resolves as the environments' routes resolve it,
by UUID or name.

| Method and path | Answers |
|---|---|
| `POST /api/v1/domains` `{name, labels?}` | 201 the domain; 400 `invalid_name`; 422 `reserved`; 409 `too_many_unverified`, `domains_off` |
| `GET /api/v1/domains?environment=<env>&label=k=v` | `{domains: [...]}`, oldest first, the labels ANDed; 404 "no such environment" |
| `GET /api/v1/domains/{id}` | the domain |
| `PUT /api/v1/domains/{id}/environment` `{environment}` | the domain, bound or moved; 400 without `environment`; 404 "no such environment"; 409 "environment X is being deleted"; 409 `superseded` |
| `DELETE /api/v1/domains/{id}/environment` | the domain, unbound |
| `POST /api/v1/domains/{id}/check` | the domain after one check now (its lookups bounded by 15 s, not cut when the caller leaves; a timeout is a failed lookup); 409 `domains_off` |
| `DELETE /api/v1/domains/{id}` | 204 |

- **A claim.** The name is trimmed, lowercased, loses one trailing dot,
  and must be a host name of ASCII letters, digits and hyphens of at
  least two labels whose last begins with a letter: no wildcard, no
  address; an internationalized name is given as its A-label, and one in
  Unicode is refused with that hint. The apps zone, `--edge-name` and
  every `--domain-reserved` zone, and every name under them, are
  reserved. Any number of claims may name one name, of one tenant or of
  several; a tenant holds at most `--domain-unverified-max` claims that
  are not verified (unverified, lapsed or superseded), counted under its
  row's lock. The token is 32 characters of lowercase base32 from 160
  random bits.
- **A binding** is allowed before verification. A bind or a move sets
  `bound_at`, clears `was_routed` and `dns_moved_since` (a new binding
  starts no grace), resets the backoff and has the domain checked at
  once; binding where it is bound changes nothing. An environment's
  deletion unbinds its domains (`ON DELETE SET NULL`).
- **The answer**: `id`, `name`, `labels`, `environment` (its UUID or
  null), `verification`, `state`, `reason`, `records` (the TXT record's
  `name` and `value`, the `cname` to set, the edge's `addresses`: what
  `--edge-name` resolves to plus every `--edge-address`; `apex`, always
  null, since ferund does not look up a name's zone and the owner picks
  the CNAME or the addresses), `found` (what the last answer showed:
  `txt`, `cname`, the canonical name when it differs from the name, for
  information only, and `addresses`), `warnings` (`recordMissingSince`,
  `dnsMovedSince`, `deactivatesAt`, the earlier of the two plus the grace,
  and `checkFailing`, the last lookup's error), `certificate` (always
  null for now), `createdAt`, `verifiedAt`, `boundAt`, `checkedAt`,
  `nextCheckAt`. Times are RFC 3339 in seconds; one that is not there is
  null. The answer's state is derived when it is read; the stored one may
  lag behind an environment's deletion by a settle.

**The state** is derived in one place (`domains.Derive`), the first rule
that matches: not verified, its verification (`unverified`, `lapsed`,
`superseded`); `unbound`; its environment not public or being deleted,
`unpublished`; its routing grace run out, `inactive`; not routed,
`waiting-for-dns` (also while a routing grace runs); else `issuing`,
"certificates are not available yet: the ingress serves custom domains
in a later step".

**The checks.**

- **The lookups** use Go's own resolver (`net.Resolver` with
  `PreferGo`) over `--dns-resolver`, or over the system's resolvers
  without it, with absolute names, so that no search domain is tried:
  `LookupTXT` of the TXT record's name, `LookupIPAddr` of the name, and
  `LookupCNAME` for `found.cname`. A recursive resolver answers from its
  cache for a record's TTL: a record just set or removed, or a negative
  answer cached before, may take minutes to count, also for `POST
  …/check`. Nothing asks a zone's authoritative servers.
- **When.** The loop `domains-check` takes up to 64 due domains
  (`next_check_at <= now()`, never a superseded one) `FOR UPDATE SKIP
  LOCKED`, moves their next check 5 minutes ahead as a lease, and checks 8
  at once; the lookups run outside any transaction, and each result is
  written in a short transaction that locks every claim of the name,
  oldest first. An unverified or lapsed claim is checked after a backoff
  from 60 s, doubling up to 3600 s, which `POST …/check` and a bind reset;
  a verified one every hour, and every 15 minutes while a grace runs, the
  last check at the grace's end.
- **What counts.** Only an answer counts as wrong: NXDOMAIN or no TXT
  record without the token, addresses that are not all the edge's. A
  lookup that fails (a timeout, SERVFAIL or another error that is not
  NotFound, no edge address known for comparing addresses) sets
  `check_error` and `check_failing_since` and changes nothing else; what
  it found before stays. A grace that ran out deactivates the domain at
  its end all the same, since the state follows the time.
- **The proof.** A claim whose token the TXT record shows is proven.
  While no claim of the name is verified, or the verified one's token is
  gone, the oldest unverified or lapsed claim shown becomes verified, and
  the verified one, if any, `superseded` at once: its binding ends, its
  environment gets `domain_superseded`. If the record shows the verified
  claim's token beside others, it stays verified. A verified claim whose
  token is gone and no other's shown starts its TXT grace
  (`record_missing_since`); the token back ends it; at the grace's end
  the claim lapses. A lapsed claim keeps its binding and is verified
  again by its token unless another claim was verified meanwhile. A
  superseded claim is not checked and cannot be bound; it is deleted and
  claimed anew.
- **The routing** is looked up for every claim but a superseded one: it
  is routed when its name resolves to at least one address and every one
  is an edge address, which covers a CNAME, an ALIAS and A records alike.
  A bound domain routed once since its bind (`was_routed`) that is not
  routed now starts its routing grace (`dns_moved_since`) when it is
  verified; routed again ends it; at the grace's end it is `inactive`,
  and stays verified and bound.
- **Incidents** on the bound environment, object `domain/<name>`, on its
  latest release: `domain_record_wrong` when a grace starts (which record
  since when, and `deactivatesAt`; one incident for both records, its
  summary rewritten), closed when no grace runs; `domain_deactivated`
  when a grace ran out, closed when the domain is active again;
  `domain_superseded`. A domain that leaves an environment closes its
  open `domain_record_wrong` and `domain_deactivated` there.

**Every write** of a domain goes through one step (settle): a TXT grace
that ran out lapses the claim, the state is derived, a checked domain is
rescheduled, the incidents follow, and the row is written. A change of
the binding or the state sends `ferun_ingress` `env:<id>` for the
environment before and after. The environments service runs the domains'
hook in the transaction that changes an environment's publicity or marks
it deleting, so that its domains' states follow at once. The loop
`domains-settle` (30 s) settles what changed without a write of the
domain's own, an environment's deletion or a publicity changed by
another writer; it leaves a grace's end to the check due then, which
first looks whether the record is right again.

## The activity log

`activity` holds ferund's own rows (tool calls, ops, runs, state changes,
placements, credentials, sign-ins) and the nodes' (pod events, `feosd`
starts and stops, `feos-authn`'s audit records), as events and spans:
runs (`run-<uuid>`) are roots, an op is a child of its run. ferund's rows
are written in batches from a bounded queue that never blocks a request
and counts what it drops; rows that must commit with a lifecycle write go
in that transaction. `activity` and `traffic_records` are partitioned by
day and dropped after `--activity-retention` and `--traffic-retention`.

## The test node

`internal/testnode` speaks feosd's side of the wire: it joins with a
token after checking the pin, keeps a node connection and an uplink to
every listed instance, reports telemetry with the inventory hash,
publishes events with acknowledgements, renews its certificate, and asks
for pods' credentials. Its pods live in memory under feosd's rules (short
IDs and UUIDs, their TAP devices named as feosd names them, so that two
pods whose UUIDs begin alike collide), hibernate into an object store in
memory or in a directory that several test nodes share, and have a fake
guest: commands run in a directory per pod, the tools work on it, watches
answer from a script, ports dial local addresses. It can restart, crash and be partitioned, and its mutating pod
calls can stop where today's feosd stops when their request is cancelled,
which proves that ferund never cancels them. Its hibernations run as
feosd's do: in a task of their own, which further calls join and a resume
abandons until the head is put; the script points `HibernateUpload` and
`HibernateHead` hold one in its upload or right after its point of no
return. With `ResumeWaitsForHibernation` it plays a feosd that does not
report its hibernations, whose resume waits for one. With `Mesh` it is on
the pods' mesh: it asks `JoinMesh` with a random key and its endpoint on
every new uplink and whenever its endpoint changes, and keeps the peers
that `MeshService` gets in memory; without, `MeshService` answers
`FAILED_PRECONDITION`. For the environments it serves `VolumeService`
(a create idempotent by ID with the same size and labels,
`ALREADY_EXISTS` otherwise; a pod claims its volumes at its create, all
or none, `FAILED_PRECONDITION` at once for one missing or held;
AVAILABLE, ATTACHED, CHECKPOINTED; a restore takes them over under
feosd's rules and leaves nothing behind when they refuse it; no delete of
a held one), `SecurityGroupService` (members and rules checked as
feosd does; no delete of a group a pod names; with
`CheckSecurityGroups`, a pod naming a group the node lacks goes FAILED
"the node does not have the security groups …") and `NameService`
(names, addresses and includes checked as feosd does, deltas from the
version it has, no delete of a table a pod names; with
`CheckNameTables`, a pod naming a table the node lacks goes FAILED "the
node does not have the name tables …"; `Resolve` answers a pod's name
as feosd's resolver does; with `NoNameService` it serves none, as a
feosd from before), reports a pod's IPv6
address, gives a pod a new microVM pid on every boot and restore, and
its script exits a container with a code (`ExitContainer`), appends to
its log (`AppendLog`, served by `StreamContainerLogs` with `tail_lines`,
capped as the guest's at 10000 lines and 4 MiB from the log's end, a
line that starts further back cut to its last whole 64 KiB parts within
them; it sends each line as one entry, where the guest splits a line
over 64 KiB into parts), sets a pod's exec
and port addresses (`SetPodExec`, `SetPodPortAddr`), and drops a pod's
record without an event (`ForgetPod`), as a node that comes back
without the pod does, its volumes let go; an exec into a pod labelled
`ferun.environment` exits 0 by default, so that addon probes and
provisioning pass. Its `Config` drives an environment end to end:
`ContainerExits` make the main container of an environment's pods exit
a while after each boot, by a glob over their `ferun.object` label (the
first rule that matches counts), so that release jobs finish and a
service crash-loops, and a rule `process/<name>` ends a fake process;
`FakeProcesses` answers the control plane's process scripts
(`process_start` and the attached services' start, listing, logs and
stop, the pruning of a detached start, a working directory that is not
in the pod's directory) in memory, without running anything: a process
runs until it is stopped, its pod boots anew or a rule ends it;
`StartLog` appends a line to a container's log at every boot and at an
exit of a rule; `PortAddrs` are where a port forward to a pod's port
dials in place of `127.0.0.1:<port>`. `ferun-testnode` runs one for
development:

    ferun-testnode --join-token feos-join-<secret>.<pin> \
        --server-name ferun.example.com --addr 127.0.0.1:7443 \
        [--name NAME] [--trust-domain feos.local] [--state-dir DIR] \
        [--object-store NAME | --object-store-dir DIR] [--pending-delay 2s] \
        [--mesh [--mesh-endpoint ADDRESS:PORT]] \
        [--fake-exec] [--check-security-groups=false] \
        [--check-name-tables=false] [--no-name-service] \
        [--exit '<object glob>=<code>[@<delay>]' ...] [--fake-processes] \
        [--start-log] [--port-addr <port>=<host:port> ...]

`--fake-exec` makes every exec into a pod exit 0 without running
anything; `--check-security-groups` and `--check-name-tables` (both on
by default) fail a pod whose groups or name tables the node does not
have, as feosd does; `--no-name-service` plays a feosd from before the
`NameService`. `--exit`, repeatable, is a
rule of `ContainerExits`: `job/*=0@3s`, `service/crasher=1`; the delay
is 2 s when it names none, and a negative code exits without a code, as
on a node that reports no exit status. `--fake-processes`, `--start-log`
and `--port-addr` (repeatable) set the fields of those names. Every flag
has a variable `FERUN_TESTNODE_<NAME>`, which the flag overrides. SIGHUP
restarts the node gracefully, as an upgrade of feosd does, and SIGUSR1
logs its pods, volumes, security groups and name tables as one JSON
line. An
environment with addons, release jobs, attached services and routes runs
end to end against a ferund with `--secret-key-file` and test nodes with
`--fake-exec --fake-processes --exit 'job/*=0@3s' --start-log
--port-addr 8080=127.0.0.1:18080`, a local server on `127.0.0.1:18080`
answering what the pods would on 8080
([development.md](../development.md#ferun)).

## Operations

### Deployment

One shape at every size: instances of `ferund`, PostgreSQL, and nodes
that dial the instances.

- **One machine.** The package `ferund` ([below](#the-package)), and
  PostgreSQL beside it without replicas; feosd on the same machine dials
  the control plane on the loopback with the same mTLS. Without
  `--node-advertise-address`, nodes reach the instance by the name they
  were configured with.
- **Several instances**, from the container image (`go/Dockerfile`,
  distroless, non-root, ports 8100 and 7443) or the package: an HTTPS
  balancer in front of the front doors, a TCP passthrough or DNS in front
  of the nodes' port, and every instance with a `--node-advertise-address`
  at which nodes reach that very instance. `deploy/k3s/ferun/` is an
  example deployment on k3s; `deploy/gardener/ferun/` runs three
  instances over a CloudNativePG cluster of three, on three workers.
- **The ingress**, beside either: instances of `ferun-ingress` that reach
  the nodes' port and the nodes' mesh endpoints, behind a balancer of
  their own; `deploy/gardener/ferun-ingress/` runs two
  ([ingress.md](ingress.md#kubernetes)).
- **Rollout order.** Roll ferund before the nodes' feosd. A new ferund
  covers an old feosd: its deadlines are longer than the old feosd's
  steps, and the old feosd sends no busy marker, so everything reads as
  before. An old ferund does not cover a new feosd: its pause gives up
  after 2 min and 4 s/GiB while the new feosd's suspend may take 190 s
  and 5 s/GiB ([The suspend's budget](node.md#the-suspends-budget)), it
  records "pause failed" and syncs the late suspend without resetting
  `paused_at` (so hibernate-after fires at once), and it takes the busy
  marker for an ordinary failure, so that a guest marked busy is
  reported unreachable and a repair's busy `StopPod` is followed by a
  `StartPod`.
  The one exception is migration `0011`, the mesh's /96s
  ([The pods' mesh](#the-pods-mesh)): a feosd of before refuses a /96 and
  every list of peers that holds one, so roll feos-node first, where its
  ferund is already of 0011's base, then ferund, then restart feos-node's
  pods, at which each node takes its /96. Until that restart, pods on
  different nodes do not reach each other over the mesh.

### Requirements

- **PostgreSQL 17 or newer.** With replicas it must replicate
  synchronously (`synchronous_commit = on` with `synchronous_standby_names`
  naming at least one standby, or `remote_apply`), so that a failover
  never loses a commit that ferund acknowledged: a booking, an op's
  begin, an event's acknowledgement. ferund sees a failover as a
  reconnect, after which it reconciles every node it is connected to.
  Three PostgreSQL instances on three machines keep this through the
  loss of any one: a commit waits for one of the two standbys (`ANY 1`,
  CloudNativePG's `dataDurability: required`), so a lost machine leaves
  a primary and a synchronous standby, writes go on, and a failover
  loses no acknowledged commit (RPO 0); only with both standbys gone
  does every commit wait. A failover must promote only a standby that
  has every acknowledged commit (CloudNativePG's `failoverQuorum`: with
  two standbys and `ANY 1`, while both are ready), and a standby must
  count as ready only while it streams (readiness probe of type
  `streaming`), and for longer than a lost machine takes to be noticed,
  so that both are still ready when the primary's machine is lost.
  Two instances cannot have both: with required durability every write
  waits while the standby is down, and with preferred durability, which
  goes on accepting writes, those are on the primary's volume alone,
  and only `failoverQuorum` keeps a failover from losing them. The
  guarantee holds for replication stalls shorter than
  `statement_timeout` (30 s): PostgreSQL ends a commit's wait for a
  standby when the statement is cancelled and reports the commit done,
  so a primary cut off from its standbys must stop well before that
  (CloudNativePG: liveness every 5 s, 3 failures). The server must
  notice a client that vanished; ferund's sessions ask for it
  ([The database](#the-database), Sessions).
- **Connections.** An instance holds up to 32 pooled connections
  (`pool_max_conns` in the URL changes it) and one for LISTEN. A
  stopping instance keeps them until it ends, up to its termination
  grace, so `max_connections` must cover the revisions alive within
  that grace × instances × 33, plus what else connects: 400 for three
  instances and two deploys within twelve minutes (3 × 3 × 33 = 297).
  Point the URL at the name that follows the primary (CloudNativePG's
  `-rw` service) with `target_session_attrs=read-write`, so that a
  connection that lands on a standby during a switchover is refused at
  once rather than at its first write; the password may come from
  `PGPASSWORD`.
- **Kubernetes.** Roll one instance at a time with a surge
  (`maxSurge: 1`, `maxUnavailable: 0`, `minReadySeconds`), spread the
  instances over the machines (topology spread, not required
  anti-affinity: on three machines a rolling deploy's fourth instance
  would find none), and give them a PodDisruptionBudget of
  `maxUnavailable: 1`. /readyz passes once the nodes have dialled a new
  instance, at the latest 30 s after its start
  (`deploy/k3s/ferun/ferund.yaml`).
- **Termination grace.** A stopping instance finishes its requests, its
  tool calls and its ops for up to `--shutdown-grace` (11 minutes) before
  it closes its node connections. Whatever stops it waits at least that
  plus 60 s ([Stopping](#stopping)): the package's unit has
  `TimeoutStopSec=720`, and a container deployment sets its termination
  grace period (Kubernetes: `terminationGracePeriodSeconds`) to at least
  `--shutdown-grace` plus 60 s, 720 s. The default of 30 s kills ferund
  long before the tool calls in flight end. A longer or shorter
  `--shutdown-grace` moves both; one below 600 s, a keyed `bash`'s
  longest timeout, can cut keyed calls that were sent before the stop,
  and one below 630 s refuses a keyed `bash` of 600 s from the drain's
  start ([Calls that outlive their
  caller](#calls-that-outlive-their-caller)). In a
  rolling deploy, a stopping instance whose calls still run lingers
  beside the new ones for up to the grace and 60 s, 12 minutes by
  default.
- **The front-door balancer:** an idle timeout of at least 60 s (MCP's
  keepalive comes every 25 s), no response buffering (MCP's answers, the
  watches' events and forwarded responses stream; the watches send
  `X-Accel-Buffering: no`, the rest needs the balancer configured so),
  long-lived responses (a tool call may run minutes, an event stream
  days), and no routing by Host for `/fwd/`: FeCode sets the Host of a
  forward to its preview host.
- **The nodes' balancer** passes TCP through; a proxy that terminates TLS
  or reads HTTP/2 breaks the node connection and the client
  certificates. Nodes connect to every instance listed to them, which is
  why every instance sets `--node-advertise-address`. The ingress's
  instances connect through it too, to one instance at a time.
- **For the ingress**: a meshed cluster (`--pod-network mesh`), since the
  ingress reaches pods only over the mesh; `--apps-zone` on every
  instance, its wildcard DNS record at the ingress's balancer and its
  wildcard certificate in the ingress's files; UDP between the ingress's
  endpoints and the nodes' mesh endpoints. In a rolling deploy, issue
  ingress tokens and publish environments only once every instance runs
  a version with the ingress: an older instance issues a node's token
  whatever kind is asked, and its writes send no `ferun_ingress`.
- **For domains**: `--edge-name` on every instance, its A and AAAA
  records at the ingress's balancer (and `--edge-address` for addresses
  it does not resolve to), the platform's own domain in
  `--domain-reserved`; DNS from the instances to `--dns-resolver` (or the
  system's resolvers), a resolver that resolves names on the internet.

### The CA ceremony

1. On a machine kept offline, `ferund init-ca --root-dir ROOT --dir CA
   --dns-domain example.com --trust-domain run.example.com`: the root's key
   stays in `ROOT` and never reaches an instance.
2. `CA` (the issuer, `issuer.json` and `bundle.crt`) goes to every
   instance's `--ca-dir`. `init-ca` writes `issuer.key` with mode 0600 for
   whoever ran it; ferund must read it, and no one else. With the
   package: `chgrp ferun /etc/ferun/ca/issuer.key && chmod 0640
   /etc/ferun/ca/issuer.key`; in the container image, its user 65532 must
   read it, as a Kubernetes secret mounted with `fsGroup: 65532` gives.
3. A join token, from the UI, `POST /api/v1/admin/join-tokens`, or
   `ferund join-token`, goes to the node: feosd's `--join-token-file`, with
   `--control-plane`, `--control-plane-name` (a name of
   `--node-server-names`) and `--trust-domain`.

A node's certificate lives 7 days and is renewed at half of that, so a
node switched off at any time still has at least 3.5 days of it: an
outage of hours, or a node switched off for a weekend, needs nothing. A
node switched off for longer than half of `--node-cert-ttl` may come
back with its certificate expired, which the nodes' port refuses in the
handshake. A node whose certificate expired, that lost its key, or that
was removed joins again with a token pinned to its name. `bundle.crt`
may hold several roots, for a rotation; a join token's pin is always the
root that anchors the issuer.

**Replacing the issuer.** On the offline machine, `ferund new-issuer
--root-dir ROOT --dir NEW --dns-domain example.com --trust-domain
run.example.com` (the domains of `init-ca`); `NEW` replaces every
instance's `--ca-dir`, and a rolling restart of the instances takes it.
Node and pod certificates of the old issuer stay valid until they are
renewed; the instances' certificates on the nodes' port come from the new
issuer from their start. An issuer that constrains URIs must be replaced
this way before nodes of ADR 0024's version roll: they refuse the nodes'
port's certificate under it, and nodes of before accept both.

**Replacing the root**, when its key is lost or it must go: every client
has to trust the new root before an instance serves under it. A join and
a renewal answer carry the whole of `bundle.crt`, and `feosd` and
`ferun-ingress` replace their stored bundle with it; a node reads its
bundle only when it starts.

1. `ferund init-ca` into new directories. The new root and issuer bear
   the same names as the old ones; every verifier tries each root of a
   name, so a bundle may hold both.
2. `bundle.crt` = the old root and the new one, the issuer still the
   old; restart the instances. Nothing else changes yet.
3. The clients take both roots: at their next renewal, with no break,
   or now. A node now: write the bundle of step 2 into its
   `bundle.pem` (write a file beside it and rename it over) and restart
   it gracefully, one node at a time. A node renews as soon as it
   starts when `renew_after` in its `node.json` lies in the past, and
   the control plane renews a node at any time, so the same restart
   with an earlier `renew_after` renews it on demand. An ingress instance now: a token
   pinned to its name in its join file, a graceful restart, then
   `DELETE /api/v1/admin/ingresses/{name}`; it joins again and gets the
   bundle.
4. The new issuer with the bundle of step 2; delete one instance's pod
   and check that the nodes connect to its replacement, then restart
   the rest. Join tokens now pin the new root; spend or revoke any
   token issued before.
5. Once every client presents a certificate of the new issuer (their
   renewals, or on demand as in step 3 and a join again of each ingress
   instance; the old ones are revoked when a newer one is first shown),
   `bundle.crt` = the new root alone, and restart the instances. The
   clients drop the old root at their next renewal; renewing them on
   demand once more finishes the rotation at once.

Until step 5, rolling the instances back to the old issuer is safe only
while every node still runs a version from before ADR 0024; roll such
nodes back first.

### Moving from the sandbox system

- The front doors stay on port 8100, with the same paths and texts;
  FeCode's backend and its MCP client work unchanged.
- Tenant and sandbox tokens are stored as hashes: a token is shown once,
  when it is made, and tenants' tokens are listed by hash.
- The bindings are off by default.
- The SQLite registry is not imported: sandboxes are created again.

## The package

On Debian 13, `ferund` comes as a `.deb`, built by `make ferund-package`
into `target/packages/ferund/`, apart from the node's packages of `make
packages` ([node.md](node.md#packages)), so that a node image never holds
it, and published with them to https://apt.fecode.dev by `make
publish-apt`, which refuses to build while the sources (`crates`,
`proto`, `hack`, `go`, `web`, the Cargo files) have uncommitted changes
or untracked files: a build takes untracked files too (every `.go` file
of a package, every migration), and the version names `HEAD`. `make
test-ferund-package` installs it in a container and checks what it
leaves:

| Path | What it is |
|---|---|
| `/usr/bin/ferund` | static, with the UI embedded |
| `/usr/lib/systemd/system/ferund.service` | the unit |
| `/usr/lib/sysusers.d/ferund.conf` | the user `ferun` |
| `/etc/ferun/ferund.env` | the configuration, `FERUN_*` variables; mode 0640, group `ferun` |
| `/etc/ferun/ca/` | the issuer's directory, mode 0750, group `ferun`; empty |

`ferund.service` runs `ferund serve` as `ferun`, with
`EnvironmentFile=/etc/ferun/ferund.env` and `TimeoutStopSec=720`, the
default `--shutdown-grace` plus 60 s; a longer grace needs a drop-in with
a longer timeout. It starts ferund again 5 s after it exits, without a
start limit (`StartLimitIntervalSec=0`): an instance that cannot reach
its database at its start exits, and keeps being started until the
database is back. The unit has a read-only view of the system: ferund
writes nothing locally. Installing the package creates the user and
neither enables nor starts the unit: an instance needs its database and
the issuer first (`FERUN_DATABASE_URL`, the files from `init-ca` in
`/etc/ferun/ca`, `issuer.key` readable by the group `ferun`), then
`systemctl enable --now ferund`. The group `ferun` on `/etc/ferun/ca` and
`ferund.env` is a `dpkg-statoverride`, which the first install sets once
the user exists, so that dpkg keeps it on a reinstall's directory and on
a new `ferund.env` an upgrade brings. An upgrade restarts a running unit;
removing the package stops it; purging it disables it and drops the
statoverrides, and `/etc/ferun/ca` stays when it holds the issuer.

## Choices ADR 0015 leaves open

- **ALPN names.** `feos-node/1` for the node connection, `h2` for the
  uplink and the join; `ferun-ingress/1` for the ingress's connections
  (ADR 0021).
- **MCP** runs stateless, with signed, self-describing session IDs used
  only for cancellation across instances.
- **Watches** have one owner instance each, which a stream opened
  elsewhere takes over in a new epoch.
- **The at-rest key** of the bindings is a third secret beside the issuer
  and the database's credential.
- **One issuer** signs nodes, pods and the instances' certificates on the
  nodes' port, keeping one CA key per instance; the control plane's
  identity is separated by issuance and by the node's check of its URI
  ([The CA](#the-ca)).
- **Join tokens carry a pin** of the root, so that a fresh node
  authenticates the control plane before it sends its secret.
- **Certificate lineage** supersedes a node's older certificates after a
  grace and treats an older one presented later as a forked identity.

## Follow-ups for feosd

What ferund does against today's feosd is described above; these are the
changes to feosd it relies on or improves with:

1. **The node side of ADR 0015** is built (`crates/fleet-client`,
   [node.md](node.md#the-control-plane)), except the event buffer on disk:
   today it is in memory, and a restart of feosd loses what was not
   acknowledged.
2. **Mutating pod calls in a task of their own**, which the request awaits,
   so that a cancelled call or a dropped connection never cuts them: the
   prerequisite of ADR 0015's "retry on `UNAVAILABLE`". Built for
   `HibernatePod` ([ADR 0008](../adr/0008-suspend-and-restore.md)),
   together with `PodInfo.hibernation` and the abandon by `ResumePod` that
   the preempt relies on; a feosd without it reports no hibernation, and
   Ensure waits for its hibernates as before. The other calls still run in
   their request: ferund never cancels them and repairs stuck pods.
3. **Port forwarding for pods** is built: feosd forwards
   `feos.portforward.v1.PortForwardService` to a pod's guest. The codes
   stay as they are, both "nothing listens" and "the guest is unreachable"
   `UNAVAILABLE`; the texts tell them apart, and `forward.rs` documents
   them as the contract ([Ports](#ports)). Against a feosd from before,
   which answers `UNIMPLEMENTED`, ports answer 502.
4. **Idempotent `SuspendPod` and `ResumePod`**; ferund resolves their
   `FAILED_PRECONDITION` by reading the pod meanwhile.
5. **Bindings for a running pod**: an RPC that sets a running pod's sealed
   bindings, and `feos-authn` itself; then `--bindings` can default to on.
6. **The pod's ID and HPKE key in its signing request**, checked by ferund
   against the request, and by `feos-authn` against its own pod; today a
   signing request's names are ignored.
7. **An exec's timeout and output limit** in `ExecRequest`.
8. **The node's key across a reboot under `feos-host-init`**, whose
   `/var/lib/feos` is a tmpfs; until then such a node needs a pinned
   re-join after every boot.
9. **fleet.proto's comment of `IssuePodCredentials`** gains "A request
   repeated with the same key, whose answer was lost, is answered again.
   Too many requests for one pod fail with RESOURCE_EXHAUSTED.", and
   `feos-authn` retries `RESOURCE_EXHAUSTED` with backoff.
10. **A sealed pod record.** An environment's pod gets its env with the
    secrets resolved, which feosd keeps in the pod's record and answers
    in `GetPod` and `ListPods`, on the node's disk and in hibernation
    checkpoints in the clear ([Secrets](#secrets)).
11. **Restarts and probes in the guest** ([ADR 0004](../adr/0004-pods.md)):
    until then ferund restarts an environment's exited container by
    restarting its pod, and runs the readiness checks over the node
    connection ([Probes, exits and restarts](#probes-exits-and-restarts)).
    The guest reports a container's exit code and serves its log
    ([guest.md](guest.md#containers)).

## Development

[development.md](../development.md#ferun) has the commands. The tests use
the standard library's `testing`; those that need PostgreSQL make a
database of their own per test in the server `FERUN_TEST_DATABASE_URL`
names, and skip without it. `make go-test` runs them against a throwaway
PostgreSQL 17 in docker. `internal/e2e` runs several instances in one
process against one database, with test nodes, and `ferun-ingress` in
the same process for the ingress's tests ([ingress.md](ingress.md#tests)).
