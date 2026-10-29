FeOS and FeRun on Gardener
==========================

An example of the whole platform on a Gardener shoot: FeOS on the
workers of a pool, in a pod per worker (ADR 0020), FeRun's control plane
`ferund` beside it, `ferun-ingress`, which serves public environments
(ADR 0021), FeChat, and an image registry. It was built on
IronCore, whose workers are VMs with nested virtualization (`/dev/kvm`,
`kvm_intel nested=Y`) running Garden Linux. Garden Linux's kernel has
what FeOS needs (ublk, tun, vhost-vsock, WireGuard, nftables).

| Part | Where | What |
|---|---|---|
| [`feos/`](feos/) | namespace `feos`, the DaemonSet `feos-node` on every worker of the pool `WORKER_POOL`, and `feos-k8s` | diskd and feosd in a pod per worker, whose microVMs live in it (ADR 0020); feos-k8s joins new workers and drains cordoned ones in FeRun |
| [`ferun/`](ferun/) | namespace `ferun` | ferund, three instances, one on each worker, and its PostgreSQL `ferun-db`, a CloudNativePG cluster of three instances, one on each worker ([`ferun/postgres.yaml`](ferun/postgres.yaml)); the k3s manifests of ferund ([`../k3s/ferun`](../k3s/ferun)) with the nodes' port at a shoot address; `https://run.<DOMAIN>` by the HTTPRoute [`ferun/route.yaml`](ferun/route.yaml) in place of k3s's Ingress |
| [`ferun-ingress/`](ferun-ingress/) | namespace `ferun-ingress` | ferun-ingress, two instances on two of the workers, behind a LoadBalancer of their own: `https://<label>.<APPS_ZONE>`, public environments' hosts, straight to their pods over the mesh ([The ingress](#the-ingress)) |
| [`../k3s/fechat`](../k3s/fechat), [`fechat/`](fechat/) | namespace `fechat` | FeChat, unchanged but for its route: `https://chat.<DOMAIN>` by the HTTPRoute [`fechat/route.yaml`](fechat/route.yaml) in place of k3s's Ingress |
| [`registry/`](registry/) | namespace `registry` | an image registry, `https://oci.<DOMAIN>`: distribution with its blobs in an object store |
| [`cluster.yaml`](cluster.yaml) | | the issuer `letsencrypt-prod` the Gateway's certificates and ferun-ingress's name, answering DNS-01 |

Every name under `DOMAIN` points at the shoot's public address, the
LoadBalancer of Traefik, which serves the cluster's Gateway (Gateway
API): run., chat. and oci.<DOMAIN> are HTTPRoutes on it, its listeners
hold their certificates. The apps zone's names point at the second
address, ferun-ingress's.

```
 *.<DOMAIN> ───── LoadBalancer ── traefik ── ferund :8100 ──── ferun-db (CNPG, 3)
                                               │ :7443 (mTLS, through kube-proxy)
   workers: feos-node pod ─────────────────────┤   dials FERUN_NODES_IP:7443,
       │ feos-mesh, UDP 51820 between workers  │   then the instances' pod addresses
       │ and to ferun-ingress                  │   (ferun-ingress as feosd)
 *.<APPS_ZONE> ── LoadBalancer ── ferun-ingress (2) ── over feos-mesh to the pods
```

## What it needs

- A shoot of three workers or more, one for each instance of
  `ferun-db` ([A lost worker](#a-lost-worker)), in a worker pool for
  FeOS whose machines have `/dev/kvm` and the memory and disk of [the
  sizing](#differences-to-k3s): workers of 64 GiB and a root of 50 GiB
  or more as the manifests stand. The network carries UDP between the
  workers (port 51820, the pods' mesh), and between the workers and the
  pods' network, where ferun-ingress joins the mesh. Kubernetes 1.30 or
  newer.
- Traefik behind a LoadBalancer, serving the Gateway API (its standard
  CRDs), and cert-manager. A Gateway, `GATEWAY_NAMESPACE/GATEWAY_NAME`
  (default `traefik/platform`), with an HTTPS listener for each of
  run., chat. and oci.<DOMAIN>: its certificate, from the issuer of
  [`cluster.yaml`](cluster.yaml), and routes admitted from the
  namespace `ferun`, `fechat` or `registry` respectively. FeCode's
  `deploy/fecode/gateway.yaml` is that Gateway on the shoot `fecode`.
  No Ingress is used here; deploy/k3s keeps its Ingresses for k3s's
  Traefik.
- CloudNativePG 1.30 or newer in the shoot, run with two operator
  replicas: chart `cloudnative-pg` with `replicaCount` 2 and a topology
  spread over `kubernetes.io/hostname`, and a PodDisruptionBudget
  (`maxUnavailable: 1`) for the operator in `cnpg-system`.
- The DNS zone `DOMAIN` at Cloudflare, for the issuer's DNS-01
  ([`cluster.yaml`](cluster.yaml) says where another provider goes), and
  the zone that holds `APPS_ZONE` there too, for its wildcard.
- A second public address, ferun-ingress's LoadBalancer, and a volume
  of 1 GiB for each of its two instances.
- An S3-compatible object store: a bucket for diskd, which the pods
  hibernate into, and one for the registry.
- An OpenID Connect provider, at which people sign in to ferund and
  FeChat, and an OpenAI-compatible LLM gateway for FeChat.
- On the machine that deploys: docker, kubectl, `envsubst` (gettext), and
  what `make packages` needs ([`docs/development.md`](../../docs/development.md)).

## Parameters

What differs from one shoot to another is a parameter, set in
`config.env` in this directory: a copy of
[`config.env.example`](config.env.example), which git ignores. A variable
on make's command line wins over the file.

| Parameter | What |
|---|---|
| `DOMAIN` | the DNS zone: ferund at `run.<DOMAIN>`, FeChat at `chat.<DOMAIN>`, the registry at `oci.<DOMAIN>`; `run.<DOMAIN>` is FeRun's trust domain too |
| `REGISTRY` | where the images are pushed and pulled; default `ghcr.io/gardenlinux/feos`, public, pulled without a secret |
| `OIDC_ISSUER` | the OpenID Connect provider, exactly as its discovery document states it |
| `WORKER_POOL` | the worker pool that runs FeOS, its label `worker.gardener.cloud/pool` |
| `NODE_NAME_PREFIX` | the beginning of the pool's node names, on IronCore `shoot--<project>--<shoot>-<pool>-`; feos-k8s removes from FeRun only nodes with it, by force, so no other shoot's nodes may match ([`config.env.example`](config.env.example)) |
| `FERUN_NODES_IP` | the ClusterIP of the service `ferund-nodes`, which feosd and ferun-ingress dial: a free address of the shoot's service network (`spec.networking.services`) |
| `FERUN_ROLLOUT_GRACE` | how long a call waits for a node that announced it stops, ferund's `--rollout-grace`: default `8m`, for the feos-node pods, which suspend their microVMs and resume them; `30s` where feosd runs on the host without `--suspend-on-stop` ([`config.env.example`](config.env.example)) |
| `EDGE_NAME`, `EDGE_ADDRESSES`, `DOMAIN_CHALLENGE_LABEL` | tenants' domains (ADR 0021): the name a domain's CNAME points at (default `edge.<DOMAIN>`; empty: domains off), the balancer's addresses for apex A records, and the TXT label that proves a claim (default `_ferun-challenge`); names under `DOMAIN` are reserved ([`config.env.example`](config.env.example)) |
| `APPS_ZONE` | the zone of public environments' default hosts, `<label>.<APPS_ZONE>`, which ferun-ingress serves: default `apps.<DOMAIN>`; empty, no environment is served or can be made public, and make refuses ferun-ingress's manifests (`manifests` stops there) ([`config.env.example`](config.env.example)) |
| `NODE_CIDR` | the workers' network (`spec.networking.nodes`), the only source let through to ferun-ingress's health port and mesh, and the only network its mesh may send to: default `10.250.0.0/16` |
| `DENY_PREFIXES` | what the pods may not reach, feosd's `--deny-prefix`, and, beside its built-in set, what ferund's preflights, bindings and image resolver may not (its `--deny-prefix`): every network of the shoot, its nodes', pods' and services' (the ConfigMap `kube-system/shoot-info` states them), and the private ranges; no default, since a network left out is open to the microVMs, and `apply-feos` stops on one ([`config.env.example`](config.env.example)) |
| `FECHAT_MODEL` | FeChat's model at the LLM gateway |

The manifests say `${NAME}` where a parameter goes. The Makefile's
`apply-*` targets fill them in with `envsubst`, write the result to
`target/gardener/manifests/` and apply it; `make -C deploy/gardener
manifests` only writes them.

## Nodes: a pod per worker

FeOS on the shoot is Kubernetes workload (ADR 0020), not host services:
the DaemonSet `feos-node` runs a pod on every worker of the pool, with
two containers of the image `feos-node` (the packages' binaries,
Firecracker and the guest, unpacked): `diskd` and `feosd`. The pod is
privileged and on the host's network, with the host's `/dev` and
`/var/lib/feos`. Nothing is installed on the worker.

- **The microVMs live in the feosd container**, in cgroups below its own,
  and end with it. Its memory request (40 GiB) is what the worker gives
  them, which the scheduler keeps free of other pods; it has no limits.
- **A rollout suspends and resumes them**: on `SIGTERM`, feosd announces
  its restart to FeRun, suspends every running pod into its directory and
  exits; the next feosd resumes them. A crash ends them; FeRun brings their
  sandboxes back from their last hibernation.
- **Versions side by side**: the init container adds the image's
  Firecracker and guest versions to `/var/lib/feos/dist`, which the feosd
  container sees as `/usr/lib/feos`, so that a pod resumes with the
  Firecracker it was suspended with.
- **Workers come and go**: Gardener replaces them on updates and in the
  shoot's maintenance window by cordoning, draining and deleting the
  machine. `feos-k8s` makes a join token for every new worker and keeps a
  guard pod on every worker, under a PodDisruptionBudget that allows no
  eviction; when a worker is cordoned, it drains the node in FeRun, whose
  sandboxes hibernate into the object store and are restored on the
  others, and only then lets the guard go.

The image comes from the packages:

    make packages FEOS_VERSION=0.6.0~dev.$(date -u +%Y%m%d%H%M).$(git rev-parse --short=7 HEAD)
    make -C deploy/gardener deploy-feos      # build, push and roll feos-node

Roll ferund (`make -C deploy/gardener deploy-ferund`) before feos-node
when both change: a new ferund covers an old feosd, but an old ferund's
pause deadline does not cover a new feosd's suspend budget, and it takes
a new feosd's busy answers for failures
([control-plane.md](../../docs/architecture/control-plane.md#deployment)).

## First setup

From the repository's root, with `KUBECONFIG` at the shoot and
`config.env` written. Keep the secrets' files outside the repository.

The issuer, and the registry, whose secrets
[`registry/registry.yaml`](registry/registry.yaml) describes:

    kubectl -n cert-manager create secret generic cloudflare-api-token --from-literal=api-token=<token>
    kubectl apply -f deploy/gardener/cluster.yaml
    make -C deploy/gardener apply-registry
    kubectl -n registry create secret generic registry-config --from-file=config.yml=<config.yml>
    kubectl -n registry create secret generic registry-auth --from-file=htpasswd=<htpasswd>
    docker login <REGISTRY>

The platform's images do not come from it: they are public on GHCR
(`REGISTRY`, FeCode's ADR 0012 §2), and no namespace needs a pull
secret. The registry here holds projects' images; FeCode runs it now
(its `deploy/registry`).

ferund, with CloudNativePG running ([What it needs](#what-it-needs)):
the CA and the secrets of [`../k3s/README.md`](../k3s/README.md),
`ferun-ca`, `ferund`, `ferund-secret-key` and
`oidc-client-ferun`. The secret `ferund` holds only `FERUN_ADMIN_USERS`
and `FERUN_BOOTSTRAP_TOKEN`: the database's URL is built in
[`ferun/kustomization.yaml`](ferun/kustomization.yaml), and its password
comes from CloudNativePG's secret `ferun-db-app`. `ferund-postgres` is
not needed. Then:

    make -C deploy/gardener push-ferund apply-ferun

ferund's pods wait (CreateContainerConfigError, then restarts) until
`ferun-db-app` exists and the primary answers, and its writes until a
standby streams. All three instances of `ferun-db` are up once

    kubectl -n ferun wait cluster/ferun-db --for=condition=Ready --timeout=15m

returns. A rollout's old instances of ferund may take up to 12 minutes,
their termination grace, to end after `kubectl rollout status` returned.

The nodes, and feos-k8s with an admin user of ferund's own
([`feos/feos-k8s.yaml`](feos/feos-k8s.yaml) says how). `<s3.env>` holds
the object store's variables of
[`hack/packages/diskd/default`](../../hack/packages/diskd/default). A key
prefix lets several FeRuns share a bucket; every node of one FeRun needs
the same, on another shoot too, so that a sandbox hibernated on one node
is restored on another. `apply-feos` first reads the shoot's networks
from `kube-system/shoot-info` and stops if `DENY_PREFIXES` leaves one out:

    make packages FEOS_VERSION=...
    make -C deploy/gardener push-feos-node apply-feos
    kubectl -n feos create secret generic diskd-s3 --from-env-file=<s3.env> \
      --from-literal=DISKD_S3_PREFIX=<prefix>/
    kubectl -n feos create secret generic feos-k8s-ferun --from-literal=credentials="feos-k8s:$pw"
    make -C deploy/gardener deploy-feos-k8s

feos-k8s puts a join token for every worker into `feosd-join`; feosd
waits for it.

FeChat: the secrets of [`../../fechat/README.md`](../../fechat/README.md),
with `FERUN_TOKEN` the bootstrap token, then the manifests and the
images:

    make -C deploy/gardener apply-fechat
    REGISTRY=<REGISTRY> fechat/deploy.sh <tag>

Every other client of FeRun gets a tenant of its own: `POST
/api/v1/admin/tenants` with `{"name":"<tenant>"}`, which answers with a
first token, and more with `POST /api/v1/admin/tenants/<tenant>/tokens`,
with an admin user's Basic credentials and `X-Requested-With: ferun`.

ferun-ingress, once ferund runs with the apps zone: [The
ingress](#the-ingress).

## The ingress

ferun-ingress (ADR 0021) serves public environments' hosts,
`<label>.<APPS_ZONE>`, straight to their pods: the StatefulSet
`ferun-ingress` of [`ferun-ingress/`](ferun-ingress/ferun-ingress.yaml),
two instances on two of the workers, behind a LoadBalancer of their own
beside Traefik's. An instance joins ferund once, as a node does, and keeps
its identity, its mesh key and a copy of the routing table on its volume;
ferund streams the table to it over the nodes' port, and it reaches the
pods as a peer on the mesh. Its program ships in ferund's image
(`/ferun-ingress`).

Nothing is served until a tenant publishes an environment, with a token
of its own:

    PUT /api/v1/environments/<env>/public    {"public":true}

The answer names the environment's hosts; its image services roll once.
`{"public":false}` takes it off again. While `APPS_ZONE` is empty,
ferund serves no host and refuses to publish.

Setting it up, with every instance of ferund on a version that knows the
apps zone (an older one issues node tokens whatever kind is asked) and
`APPS_ZONE` in `config.env` applied (`apply-ferun`). Keep the tokens out
of the repository.

1. A join token for each instance, one-time, pinned to its name, for
   24 h, through ferund's CLI:

       for n in 0 1; do
         kubectl -n ferun exec deploy/ferund -- /ferund join-token --kind ingress --node ferun-ingress-$n --ttl 24h
       done

   or through REST, `POST /api/v1/admin/join-tokens` with
   `{"kind":"ingress","name":"ferun-ingress-<n>","ttlSeconds":86400}`, an
   admin user's credentials and `X-Requested-With: ferun`. An ingress's
   token is `feos-ingress-join-<secret>.<pin>`; an answer without
   `"kind":"ingress"`, or with a `feos-join-` token, came from an older
   instance: discard it.
2. The namespace and the tokens under the instances' names (only each
   pod's init container mounts this Secret, and copies the pod's own
   token for it):

       kubectl create namespace ferun-ingress
       kubectl -n ferun-ingress create secret generic ferun-ingress-join \
         --from-literal=ferun-ingress-0=<token> --from-literal=ferun-ingress-1=<token>

3. The manifests. cert-manager issues the wildcard `*.<APPS_ZONE>` into
   `apps-wildcard-tls` through the issuer's DNS-01, whose token must edit
   the zone that holds `APPS_ZONE`; the pods wait for it:

       make -C deploy/gardener apply-ferun-ingress
       kubectl -n ferun-ingress wait certificate/apps-wildcard --for=condition=Ready --timeout=15m
       kubectl -n ferun-ingress rollout status statefulset/ferun-ingress --timeout=10m

4. DNS, DNS only (not proxied), at the balancer's address (`kubectl -n
   ferun-ingress get svc ferun-ingress`): `*.<APPS_ZONE>` A `<address>`,
   and `edge.<DOMAIN>` A `<address>`, reserved for domains later.
5. Each instance, from inside its pod:

       kubectl -n ferun-ingress exec ferun-ingress-0 -- /ferun-ingress status

   prints its `/statusz`: `connected`, its mesh `address`, an
   `admittedVersion` above 0, and a peer for every worker at
   `<worker>:51820` with `fresh` true. No handshake means that UDP 51820
   does not pass between the pods' network and the workers (IronCore's
   security groups, Calico, `NODE_CIDR`). `connected` false while ferund
   runs: the instances may dial only ferund's pods and `FERUN_NODES_IP`
   on 7443 (the NetworkPolicy `ferun-ingress-egress`), so check that
   ferund's pods are `app: ferund` in `ferun`. `GET
   /api/v1/admin/ingresses` lists both, joined and connected.

Afterwards:

- **A new image** is ferund's: roll ferund first (`make -C
  deploy/gardener deploy-ferund`), then `make -C deploy/gardener
  deploy-ferun-ingress`, which replaces one instance at a time, each once
  the new one has been ready for 10 s. A stopping instance serves on for
  10 s with `/readyz` failing, then finishes its requests.
- **The balancer** has `externalTrafficPolicy: Cluster`. IronCore's load
  balancer checks no node's health ([A lost worker](#a-lost-worker)), and
  with `Local` the worker without an instance would drop its share of
  connections, a third on three workers, all the time, and a worker
  whose instance restarts or was drained another third during every
  rollout. The cost: apps see a worker's address in `X-Forwarded-For`,
  not the client's, until the PROXY protocol (ADR 0021).
- **Never force-delete a pod** (`--force`, `--grace-period=0`): its name
  is its identity, and two processes with one identity make WireGuard
  flap between them. A lost worker's instance stays Terminating until
  Gardener deletes the Node; the other instance serves meanwhile, all
  but the lost worker's share of new connections ([A lost
  worker](#a-lost-worker)).
- **A lost identity** (`identityRemoved` in the status, after `DELETE
  /api/v1/admin/ingresses/<name>`, or a certificate that expired while
  the instance was down for more than a week) joins again with a new
  pinned token under its name in `ferun-ingress-join`, after a deletion
  of its pod (`kubectl -n ferun-ingress delete pod <name>`, never
  forced): the init container copies the token at the pod's start. An
  instance never uses a token twice. A stolen state directory takes the
  same steps after the `DELETE`: the join again revokes every certificate
  and the instance makes a new WireGuard key, which ferund requires, so
  the volume needs no wiping.
- **What an instance may do.** It faces the internet as root with
  `NET_ADMIN` alone, for its WireGuard device, under the runtime's
  default seccomp profile (`RuntimeDefault`), and dials only DNS,
  ferund's 7443 and the workers' UDP 51820 (`ferun-ingress-egress`).
  A proxy without the capability, in a container of its own, is the
  follow-up (ADR 0021, open questions).
- **Cookies.** ferun-ingress makes every cookie that an app's answer sets
  host-only, so that no app sets one for another or for `DOMAIN`. An
  app's JavaScript still can: list `APPS_ZONE` in the Public Suffix List.
- **Taking it off**: `kubectl -n ferun-ingress scale
  statefulset/ferun-ingress --replicas=0`, whose instances leave
  ferund's tables cleanly, and the DNS records removed. The route door
  and FeCode's forwarder go on working.

## Moving the database

A shoot whose ferund runs on the StatefulSet `postgres` of the k3s
manifests moves its data into `ferun-db` once, before `apply-ferun`,
which refuses until then. CloudNativePG creates `ferun-db` from a copy of
`postgres-0`'s database ([`ferun/import`](ferun/import/kustomization.yaml)).
ferund is stopped for the move, since what it wrote after the copy began
would be lost: it is down for a few minutes, plus however long its
stopping instances take to finish the tool calls in flight (up to their
termination grace). No node is marked down while no instance runs, so
`--lost-grace` does not start. Keep the dump and the counts outside the
repository.

1. Optionally, rehearse first, with ferund running: render `ferun/import`
   (`kubectl kustomize --load-restrictor LoadRestrictionsNone
   deploy/gardener/ferun/import`), rename the Cluster (for example
   `ferun-db-rehearsal`), apply it, time it until it is Ready, check it as
   in step 6, and delete it.
2. Stop ferund and wait for its pods to go:

       kubectl -n ferun scale deploy/ferund --replicas=0
       kubectl -n ferun wait --for=delete pod -l app=ferund --timeout=13m

3. A final dump to a local file, and the tables' row counts:

       Q="select table_schema||'.'||table_name, (xpath('/row/c/text()', query_to_xml(format('select count(*) as c from %I.%I', table_schema, table_name), false, true, '')))[1]::text::bigint from information_schema.tables where table_schema not in ('pg_catalog','information_schema') and table_type='BASE TABLE' order by 1"
       kubectl -n ferun exec postgres-0 -- pg_dump -U ferun -d ferun -Fc > <ferun-final.dump>
       kubectl -n ferun exec postgres-0 -- psql -U ferun -d ferun -tAc "$Q" > <counts-old.txt>

4. Create `ferun-db` from the copy:

       make -C deploy/gardener apply-ferun-db-import

5. Wait for it:

       kubectl -n ferun wait cluster/ferun-db --for=condition=Ready --timeout=15m

6. Check owners and row counts on its primary: every table is ferun's,
   and the counts are `postgres-0`'s.

       P=$(kubectl -n ferun get cluster ferun-db -o jsonpath='{.status.currentPrimary}')
       kubectl -n ferun exec $P -c postgres -- psql -d ferun -tAc "select tableowner, count(*) from pg_tables where schemaname not in ('pg_catalog','information_schema') group by 1"
       kubectl -n ferun exec $P -c postgres -- psql -d ferun -tAc "$Q" | diff <counts-old.txt> -

7. ferund, three instances over `ferun-db`. `ferun-db` keeps the data it
   was created with, though `ferun.yaml` no longer names the import:

       make -C deploy/gardener apply-ferun

8. Wait until every node is ready again (`GET /api/v1/admin/nodes`, or
   the UI): the nodes connect about 30 to 60 s after the instances start,
   and until then tool calls fail.
9. Then fence the old copy, so that an old manifest fails at start
   (`--database-url is required`) instead of running on stale data:

       kubectl -n ferun patch secret ferund --type json -p '[{"op":"remove","path":"/data/FERUN_DATABASE_URL"}]'
       kubectl -n ferun scale statefulset/postgres --replicas=0

10. After the soak, delete the StatefulSet, its Service, its volume and
    its password:

        kubectl -n ferun delete statefulset/postgres service/postgres pvc/data-postgres-0 secret/ferund-postgres

Going back during the soak copies `ferun-db` into `postgres-0` first,
with ferund stopped: what ferund wrote since the move is only in
`ferun-db`.

Onto another shoot (as from `fecode-dev` to `fecode`): steps 2 and 3 on
the old shoot, where ferund stays stopped; then, on the new one and
before anything of `ferun/` is applied there (`apply-ferun` would start
ferund on an empty `ferun-db`), a StatefulSet `postgres` that holds the
dump, and steps 4 to 10 there, whose secret `ferund` has no URL for
step 9 to remove:

    kubectl apply -f deploy/k3s/ferun/namespace.yaml
    kubectl -n ferun create secret generic ferund-postgres --from-literal=password=$(openssl rand -hex 24)
    kubectl apply -f deploy/k3s/ferun/postgres.yaml
    kubectl -n ferun wait pod/postgres-0 --for=condition=Ready --timeout=5m
    kubectl -n ferun exec -i postgres-0 -- pg_restore -U ferun -d ferun --exit-on-error < <ferun-final.dump>

The new shoot has a `config.env` of its own: `WORKER_POOL`,
`NODE_NAME_PREFIX`, `FERUN_NODES_IP` and `DENY_PREFIXES` name its pool
and its networks, which `kube-system/shoot-info` states. `fecode` has its
pods and services outside CGNAT, which the old shoot's `DENY_PREFIXES`
does not cover:

    FERUN_NODES_IP=100.152.70.70
    DENY_PREFIXES=10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 100.144.0.0/13 100.152.0.0/13

## A lost worker

IronCore's load balancer does not check the nodes' health. A worker
whose machine hangs or is off keeps its share of new connections, about
a third on three workers, until the machine controller replaces the
machine (the worker pool's
`machineControllerManager.machineHealthTimeout`, 10 minutes by default;
a shorter one replaces machines sooner on short faults too). Nothing on
the shoot's side shortens it: IronCore's cloud controller, in the mode
Gardener runs it (`--use-nic-selector`), sends every worker's network
interface the traffic and ignores both
`node.kubernetes.io/exclude-from-external-load-balancers` and a deleted
Node ([cloud-provider-ironcore#618](https://github.com/ironcore-dev/cloud-provider-ironcore/issues/618));
its balancer has no health checks of its targets
([ironcore#1584](https://github.com/ironcore-dev/ironcore/issues/1584)).

`ferun-db` keeps a primary and a synchronous standby: if the worker held
the primary, a standby takes over once the worker is noticed (40 to
55 s), and writes go on without losing a commit ferund was told of. The
third instance comes back only on the worker that replaces the lost one,
since the instances never share a worker. A second worker lost before
then leaves one instance: a primary, whose every commit waits for a
standby until ferund's `statement_timeout` (30 s) ends the wait and the
commit is on its volume alone, or a standby, which is not promoted
without a person (`kubectl cnpg promote`, after checking what it
received). ferund's lost instance starts again on another worker after
a minute.

## Differences to k3s

- ferun-ingress runs here only; on k3s the route door serves.
- The nodes' port is `FERUN_NODES_IP`, in the shoot's service network.
- ferund runs three instances, at most one more on any worker than on
  another, under the k3s manifest's PodDisruptionBudget (one instance
  at a time) and with its shutdown grace, and tolerates unreachable and
  not-ready nodes for 60 s instead of 300 s.
- `ferun-db` instead of the StatefulSet `postgres`: three instances, one
  on each worker, synchronous (`ANY 1` of the two standbys, required
  durability, `failoverQuorum`, readiness of type streaming), liveness
  every 5 s, TCP keepalives, `max_connections` 400, a NetworkPolicy,
  and a URL with `target_session_attrs=read-write`; the password from
  `ferun-db-app`.
- How long ferund waits for a node that announced it stops is a
  parameter, `FERUN_ROLLOUT_GRACE`: 8 minutes by default, for the
  feos-node pods, where k3s keeps ferund's default of 30 s.
- Pods are denied the shoot's networks, its nodes', pods' and services'
  (`spec.networking`), and the private ranges: `DENY_PREFIXES`, without
  a default. RFC 1918 and CGNAT hold Gardener's default networks, but not
  every shoot's: `fecode` has its pods in `100.144.0.0/13` and its
  services in `100.152.0.0/13`.
- IronCore routes no IPv6 to the workers: pods are on the mesh's ULA and
  leave over IPv4, through the shoot's NAT gateway.
- The microVMs' memory is the feosd container's request: 40 GiB, for
  workers of 64 GiB; the rest is the shoot's own workloads'. The chunk
  cache is 12 GiB (`DISKD_ARGS`), for a 50 GiB root, which holds the
  pods' disks too. Other machines change both in
  [`feos/feos.yaml`](feos/feos.yaml), and `terminationGracePeriodSeconds`
  with the memory: a stop suspends all of it.
