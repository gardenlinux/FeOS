# Backlog

Questions and intended changes that the ADRs decided to leave open and
that block nothing. One line each, under the ADR they belong to. A line
that is taken up becomes an ADR of its own or a section of the one it
names; a line that is given up is deleted. Nothing here is a decision.

## ADR 0004

- ADR 0004: exec takes no stdin and no terminal; a user's shell in a sandbox and debugging a service need both, as one stream.

## ADR 0007

- ADR 0007: `SetTime`, `GetTime`, `AdjustTime`, `SetNetworkConfig`, `GetNetworkConfig`, `FreezeFilesystem`, `ThawFilesystem` and `PreparePause` still live in `feos.host.v1.HostService`, which `feosd` answers on the node too; move them into `feos.guest.v1`, which only the guest serves.

## ADR 0008

- ADR 0008: detach a restored guest's memory from the disk daemon by installing all of it right after the restore, so that the guest no longer depends on the daemon.
- ADR 0008: whether a hibernation should skip the guest's compaction before its suspend, since every moved page is a chunk to upload again.

## ADR 0010

- ADR 0010: Changing a pod's metadata while it runs: the spec is fixed at the pod's creation, and Firecracker's store takes changes at any time.
- ADR 0010: Fixed IPv4 addresses for leaving, per tenant: an address of the node's per tenant to masquerade to.
- ADR 0010: Rotating the mesh's keys, with the node's certificate or on their own; a node that changes its key is taken as a new peer, nothing rotates one.
- ADR 0010: Clusters with both kinds of node: a node with a routed prefix as a peer of the mesh, its prefix allowed for it, and a route to it in the meshed nodes' pods; the two exclude each other.
- ADR 0010: Nodes that cannot reach each other's endpoints, both behind NAT: a relay.

## ADR 0011

- ADR 0011: a disk size per pod, by a filesystem the guest grows online or one it formats with `mke2fs` as it does volumes.
- ADR 0011: where diskd keeps its state on a node whose operating system is FeOS, whose `/var/lib/feos` is a tmpfs without an object store.
- ADR 0011: how long a pod's I/O may wait for a recovering diskd before the pod is given up.
- ADR 0011: whether the host's page cache stays in front of the ublk devices, or what was served is dropped.
- ADR 0011: whether the chunk pool stays global, which is deduplication across tenants.
- ADR 0011: the control plane runs diskd's sweep with every live head and baked layer; nothing sweeps yet.

## ADR 0012

- ADR 0012: An environment-wide egress default that its services narrow; a service's own field is the first step.
- ADR 0012: References for UDP and other protocols; a reference names a TCP port, and a service's port has no protocol in the spec.
- ADR 0012: References across environments (stage reaching a shared service), how another environment's workload is named and whose deployment admits it.
- ADR 0012: A tenant's private networks over a VPN or peering, which need a route as well as a block and an exception to the denied prefixes for that tenant.
- ADR 0012: An addon's egress once addons back up to the object store or replicate to another region; it is the internet now.
- ADR 0012: Refusing server names the egress did not allow, from the copies feosd takes for its records, to close the CDN limit of address-based FQDN egress.
- ADR 0012: Whether a secret may name the workload its URL points at, so that a service whose peer's address is sealed derives an edge.
- ADR 0012: A binding's upstream host as egress the binding declares, once a binding dials from the pod's own address instead of the node's.
- ADR 0012: Limits on the node for groups per pod, rules per group and members per group; the plan bounds `uses`, egress domains and blocks and a route's `from`, feosd bounds nothing.
- ADR 0012: Drops and refused names of a pod as records in the sink, mapped by ferund to workloads in the deployment's words, per pair and port, rate limited; a first refusal after a deployment as an incident on the object.
- ADR 0012: The console laying refused flows and refused names over the reachability picture and proposing the missing reference, once those records reach the control plane.

## ADR 0013

- ADR 0013: QUIC: read the server name from Initial packets, whose keys derive from the connection ID, or keep UDP 443 closed so that clients fall back to TCP.
- ADR 0013: a hint from `feos-guest` which container and process opened a connection, beside the host's record.

## ADR 0014

- ADR 0014: the Go library of FeID, a port of the Rust crate, released as a Go module.
- ADR 0014: a development signer in `feosctl`, feid/1 on a Unix socket with a CA made for the run, so that an application runs the same on a laptop and on FeOS.
- ADR 0014: the issuer writes a service's DNS names into its pods' certificates (none are issued); without them FeID's check by name reaches only pinned peers.
- ADR 0014: `feos-authn`'s network namespace, the pod's second address from which host-side bindings dial (ADR 0028, 0053), and its cgroup.
- ADR 0014: the control plane joins TLS signatures, handshake reports and the flows of ADR 0013 into one record per connection, both ends named by their IDs from the addresses it gave out, and a graph of an environment's workloads from them.
- ADR 0014: an OpenSSL provider that delegates the signature to `feos-authn`, for Python's `ssl`, libpq, curl, Node and the addons' own server keys.
- ADR 0014: session resumption in FeID's clients and the guest's bindings, which is neither signed nor audited; rustls resumes by default.
- ADR 0014: a limit on signatures per pod per second.
- ADR 0014: integrity of the audit, a hash chain per pod so that the control plane sees a gap.
- ADR 0014: tokens, JWTs for HTTP APIs and a pod as an OAuth client, and how a workload's call carries the user it acts for.
- ADR 0014: libraries for Java and .NET.
- ADR 0014: ECDSA P-256 as a key of its own, for peers without Ed25519.
- ADR 0014: a default scope in FeID's servers and inbound bindings that terminate mutual TLS for a server without FeID and hand it its caller's ID, for a call whose address names no workload.
- ADR 0014: a second certificate of the pod, with the same ID and a key of its own, should the network key traffic per pair of workloads.
- ADR 0014: issuers per tenant, for a tenant's own verifiers that check only the chain.
- ADR 0014: TLS bindings for a sandbox, whose spec is fixed at its create, so that an attached service reaches an addon without FeID.
- ADR 0014: `feosctl` sealing a pod's bindings to its process on a node without a control plane.

## ADR 0015

- ADR 0015: attestation of a node at its join, by its TPM.
- ADR 0015: traffic records at 512 nodes: how much a node aggregates or samples before it sends, and whether PostgreSQL is where they rest or only where they pass through.
- ADR 0015: a routing layer among the instances, when they are many.

## ADR 0016

- ADR 0016: growing a volume: diskd's device, Firecracker's drive and the filesystem online, which writes zeroed inode tables into the top.
- ADR 0016: ephemeral volumes as Kubernetes's `emptyDir`, a directory on the pod's disk shared by its containers and gone with the pod; waits for sidecars (ADR 0004).
- ADR 0016: moving a free volume between nodes without a pod, as a checkpoint of the volume alone.
- ADR 0016: snapshots and clones of a volume, which diskd's layers make cheap.
- ADR 0016: quotas: how much of a node's disk its volumes may take.

## ADR 0018

- ADR 0018: whether revert takes annotations of the caller's; it copies the reverted deployment's and adds `ferun.revertOf`.
- ADR 0018: whether a service's rollout budget starts when the service may move rather than at the deployment's start.
- ADR 0018: volumes for services and their mounts; data lives in addons until then.
- ADR 0018: an admin's listing of every tenant's environments.
- ADR 0018: a CLI for the environments' API.
- ADR 0018: a FeRun tenant per FeCode project, for quotas and billing; FeCode keeps its projects' environments under one tenant, labelled by project.

## ADR 0019

- ADR 0019: remove the UUID path of pod IDs (`feos_utils::pod_id`, `go/internal/podid`, the placement gate for TAP names) once no pod with a UUID is left.

## ADR 0021

- ADR 0021: L4 routes, TCP and UDP services, and TLS passed through by SNI to a service that terminates it itself.
- ADR 0021: h2c to upstreams for gRPC services; the ingress speaks HTTP/1.1 to every service.
- ADR 0021: a workbench's previews at the ingress, admitted as an environment's visitors are (ADR 0054), so that FeCode's previews could leave its forwarder.
- ADR 0021: rate limits, body limits and timeouts per domain or tenant, and a cap per client address, which the client's address behind `externalTrafficPolicy: Local` makes possible.
- ADR 0021: the proxy apart from the mesh's privileges: the agent root with `NET_ADMIN` and the proxy nonroot as two containers, or the device's calls alone behind a socket in a small privileged container.
- ADR 0021: health checks at the balancer (`healthCheckNodePort`, ironcore#1584) and its node set honoured (cloud-provider-ironcore#618), so that fewer instances than workers suffice and no share waits while one restarts.
- ADR 0021: where the ingress runs at scale: a pool of its own, anycast addresses.

## ADR 0022

- ADR 0022: garbage collection of baked layers, their catalog rows and blobs on the nodes and in the object store, per source: which deployments' images a cluster keeps, and for how long.
- ADR 0022: pods of several images (sidecars): one bake per set of images, or a layer per image.
- ADR 0022: whether a registry's refusal ever stops a pod that runs.
- ADR 0022: scanning images for known vulnerabilities before a deployment runs them.
- ADR 0022: signing a build's result and checking the signature when a deployment runs it.

## ADR 0023

- ADR 0023: whether 600 s is the right cap for a keyed `bash`; models ask for 1800 s for big builds, and a larger cap raises the shutdown grace one for one.
- ADR 0023: an operator's view of a sandbox's keyed calls, running and recent, in the console or the admin API.

## ADR 0026

- ADR 0026: sampling of traces, and whether the ingress's spans of every request are kept once the volume asks.
- ADR 0026: audit records in the sink: never dropped, kept a year, not deletable by a tenant; which of `ferund`'s calls write one, and whether they are signed or chained.
- ADR 0026: whether an attached service's process log becomes a stream; a workspace's logs are not shipped.

## ADR 0027

- ADR 0027: follow `/dev/ptp0` in the guest where it exists, with `feosd`'s loop as the fallback, should the nodes get an invariant TSC.

## ADR 0028

- ADR 0028: upstreams in a private network, a company's systems behind its firewall: a connector the company runs that dials out, and a binding that names it.
- ADR 0028: a person's identity on a request (token exchange, on-behalf-of), and request signing schemes.
- ADR 0028: the intercept mode on ports other than 443, and HTTP/2 without TLS (h2c) in the name mode, for gRPC clients.
- ADR 0028: Java's keystore and other runtimes with trust stores of their own, which the bundle per container does not reach.
- ADR 0028: upstreams that answer br or zstd whatever they are asked: unpacked as gzip is, or a binding option that trusts such an upstream.
- ADR 0028: a bundle mounted over a system bundle's path cannot be replaced (a rename onto a mount fails with `EBUSY`), so `update-ca-certificates` and a package manager that updates `ca-certificates` fail in a container created with CAs; whether such a container should get its bundle in the shared directory alone, as a container older than the CA does.
- ADR 0028: Kubernetes and TLS bindings at the node address and by names, as HTTP bindings are, instead of loopback ports in the guest (ADR 0014).
- ADR 0028: quotas per tenant across pods, beyond the rate limit per binding and pod.

## ADR 0029

- ADR 0029: skew after a switch: serving blue's files that green lacks for a while (Vercel's skew protection), opt-in per site and for how long; it would also serve a file a version removed on purpose.
- ADR 0029: the rules on a laptop: a small command around the ingress's site package (`ferun-ingress serve-site ./dist`) to try a `_ferun.json` before the upload; the manual switch tries it on the real ingress, but only after.
- ADR 0029: an upload by hashes, as Cloudflare's protocol: the manifest first, then only the blobs the tenant lacks, so that a large site from a CI sends what changed.
- ADR 0029: brotli variants at the ingest, which need a dependency; precompressed siblings alone until then.
- ADR 0029: canary and sticky splits between blue and green; a share of requests per version mixes versions within one page's load unless the split is sticky.
- ADR 0029: redirects by host (`www` to the apex), a domain's matter (ADR 0045) rather than a site's.
- ADR 0029: sites in environments that are not public, served by the route door (ADR 0043), for FeCode's previews of a built site.
- ADR 0029: Early Hints (103) from a document's `<link rel="preload">`.
- ADR 0029: files larger than a bundle's file limit (videos), read by ranges from the store without the cache.
- ADR 0029: traffic as a cost: the store's reads and the ingress's bytes per tenant, and whether a tenant gets a quota on them.

## ADR 0030

- ADR 0030: a CDN of the operator's in front of the whole edge: the same secret per installation, and the default hosts behind it.
- ADR 0030: a rate limit per client address behind a CDN, which the CDN's header makes possible there (the limit itself is ADR 0021's line).
- ADR 0030: more providers (Fastly, Akamai, Bunny): presets, recipes and purges.
- ADR 0030: a second proof beside the secret: authenticated origin pulls with the tenant's own certificate, where its CDN offers them.

## ADR 0031

- ADR 0031: a general MCP door for a person's agents (`/mcp/tenants`: an OAuth 2.1 resource server taking the shared provider's audience-bound access tokens and tenant tokens, one tool per operation of the tenant class, tokens, secrets' values and image credentials left out, a `secret_generate` tool), once the provider's login app grants audiences (FeCode's ADR 0020).
- ADR 0031: a tenant's admin deletes the tenant themselves (an operator does, through the admin API).
- ADR 0031: a CLI with a person's tokens, and a terminal into a sandbox (needs interactive exec, ADR 0004).
- ADR 0031: billing beside the limits.
- ADR 0031: a registrable domain of its own as the tenant zone, keeping cookies, reputation and blocklists apart from the platform's (a subdomain of the platform's domain serves it).
- ADR 0031: a person removed at the provider without a sign-out keeps their session until its TTL (back-channel logout covers a sign-out; FeCode's requirement A9 asks the same).
- ADR 0031: inviting a person who has no account yet, by a registration ticket from the provider (FeCode's ADR 0020), a decision about cost.
- ADR 0031: whether internet egress of the tenants people make waits for an operator's allowance, against abuse (a service's egress is declared per ADR 0034; a sandbox's default is out to the internet).
- ADR 0031: a tenant's display name beside its name, which is part of its hosts.
- ADR 0031: FeCode's projects as tenants of their own, one authorization model for both products (FeCode's ADR 0008, question 4).

## ADR 0032

- ADR 0032: templates ("web and Postgres", "web, API and Valkey") as fragments of a spec that add objects to the draft and are nothing afterwards, not stored and without versions.
- ADR 0032: whether a plan also answers an image's exposed ports, from its config, so that the first port is proposed rather than typed.
- ADR 0032: whether proposed variable names (`DATABASE_URL`) belong to the catalog, as each blueprint's suggestion, or to the UI.
- ADR 0032: whether the list carries an environment at the spec's limits (64 services, 32 jobs, 128 routes), or needs a search and grouping by label.
- ADR 0032: a review of a draft before it is applied, if deployments get approvals; that is a policy of the caller.

## ADR 0033

- ADR 0033: Kubernetes, SSH and registry credentials as the tenant's objects too, once their node side exists (ADR 0014, ADR 0025).
- ADR 0033: a narrower use: a sandbox or a binding that takes a credential with fewer paths or a lower rate than its definition.
- ADR 0033: the tools of the general MCP door (ADR 0031) for credentials, without their secrets.
- ADR 0033: a credential's page shows its requests, which the sink records by the binding's name; they are read per environment as its outbound requests meanwhile.

## ADR 0035

- ADR 0035: An environment's secrets by name in a sandbox's or a tenant's job's create, pinned at the create as an apply pins; both carry literals.
- ADR 0035: Routes to a sandbox's port other than through an attached service.
- ADR 0035: Quotas per environment, now that everything an environment runs is counted in it; they are per tenant.

## ADR 0038

- ADR 0038: a PostgreSQL session that outlives its grant; `pg_stat_ssl` truncates the client's DN, so the step cannot match a session to the line it lost.
- ADR 0038: `tls.key` on the addon's volume until the OpenSSL provider, and whether a checkpoint or a clone with data (ADR 0016, 0018) leaves it behind.
- ADR 0038: a developer's `psql` from a sandbox without an attached service, by `reaches` entries that name a role as lines of the map.
- ADR 0038: PostgreSQL before 17 through a binding, whether the binding learns the one `SSLRequest` message or 16 is left to FeID and the OpenSSL provider.
- ADR 0038: Valkey keys and channels per workload, with an option that names them, as PostgreSQL's roles.
- ADR 0038: a new root that an addon's old trust file does not hold; the step's own TLS connection would be refused until a restart reads the new files.
- ADR 0038: the loading of a large append-only file, which Valkey's probe cannot see without a certificate.

## ADR 0040

- ADR 0040: backups of an addon to the object store, and a clone that brings an addon's data along.
- ADR 0040: minor upgrades of an addon, and more types (`clickhouse`).
- ADR 0040: rotating an addon's generated credentials (nats's password); they are never changed.
- ADR 0040: whether a lost addon whose data ties it to no node is recreated wherever it fits, as a service's lost replica is.

## ADR 0041

- ADR 0041: `follow` on a tenant's job's log.
- ADR 0041: whether an environment job's run becomes a record like a tenant's job, so that a manual run answers an ID whose status and log can be read, and takes an environment per run.
- ADR 0041: whether the 7 days a tenant's job's record stays are a flag, and whether a list shows jobs of every age or only the kept ones.

## ADR 0042

- ADR 0042: a crash loop for attached services; they are restarted with a backoff without one.

## ADR 0043

- ADR 0043: a host per route; `host` is stored and not served, and every route of an environment is served on every host the environment has.

## ADR 0044

- ADR 0044: `feosd` sealing the pod's record, so that a pod's resolved secrets do not lie in the clear on the node.
- ADR 0044: whether the logs answer read from a running pod is redacted as the kept tails are, at the cost of resolving the secrets for every read, and how far redaction reaches.

## ADR 0045

- ADR 0045: zones of a tenant's own, and a zone a tenant verified with one record that lets it claim every name below it and wildcard domains, which need DNS-01.
- ADR 0045: `ferund` as a DNS-01 client for the zones' wildcards, with a DNS credential per provider; the operator's issuer provides them.
- ADR 0045: a certificate before the cutover or behind a proxy that answers HTTP itself: ADR 0030 proposes DNS-01 through a CNAME of `_acme-challenge.<name>` into a zone of the platform's.
- ADR 0045: certificates a tenant brings, for a domain whose CA is not Let's Encrypt.
- ADR 0045: whether the cap on unverified claims and certificates only for routed domains shield the ACME account enough, or a tenant wants a cap on its certificates.
- ADR 0045: a Public Suffix List entry for the apps zone, an operator's action, so that a page's script cannot set a cookie for the zone.

## ADR 0048

- ADR 0048: ending a pod when one of its main containers fails, without waiting for the others.
- ADR 0048: the reaper's binary: one of its own, or `feos-guest` mounted into the container and run again, as for the task service.
- ADR 0048: per-container resources and limits in the guest, and the memory a pod's sidecars may take.

## ADR 0050

- ADR 0050: multi-platform images and a second architecture of nodes.

## ADR 0053

- ADR 0053: the guest pipes the SSH agent socket to a vsock port of its own without a preamble; ADR 0014 has one port with a preamble.

## ADR 0054

- ADR 0054: machine clients of a guarded environment: admission by a route token at the ingress, or a route that stays open beside guarded ones, such as a webhook's.
- ADR 0054: OAuth 2.0 issuers without OpenID Connect, where no ID token says who signed in.
- ADR 0054: the issuer's access token for the application, where it is to act for the visitor elsewhere.
- ADR 0054: `ferun-auth` as a workload on FeRun, once FeRun deploys parts of itself.
- ADR 0054: a CDN in front of a guarded host (ADR 0030): what it may cache, and who checks the visit at the edge.
- ADR 0054: the visitor's subject in the access log (ADR 0021).
- ADR 0054: a visit's longest time as an installation's setting; the first value is 12 hours.
