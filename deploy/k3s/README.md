FeOS and FeRun on k3s
=====================

An example of the whole platform on a k3s cluster: FeOS on the nodes,
FeRun's control plane `ferund`, and FeChat. The nodes are Debian 13
machines with `/dev/kvm`, bare metal or VMs with nested virtualization.

| Part | Where | What |
|---|---|---|
| [`feos/`](feos/) | namespace `feos`, a DaemonSet on every node labelled `feos.fecode.dev/node=true` | installs FeOS's packages (feosd, diskd, feosctl, Firecracker, the guest) on the node and has systemd run them |
| [`ferun/`](ferun/) | namespace `ferun` | ferund, FeRun's control plane (ADR 0015), and its PostgreSQL; `https://run.<DOMAIN>`, sign-in at the OpenID Connect provider |
| [`fechat/`](fechat/) | namespace `fechat` | FeChat, the chat client whose tools run in FeRun's sandboxes; `https://chat.<DOMAIN>`, sign-in at the OpenID Connect provider |

FeOS runs on the nodes as it would on any Debian machine: the DaemonSet is
only how the packages get there. feosd and diskd are the host's services, so
their pods and disks outlive the DaemonSet's pods, a rollout, and the
DaemonSet itself. feosd connects out to ferund; no node opens a port.

```
 run.<DOMAIN> ── traefik ── ferund :8100 ──── postgres
                               │ :7443 (mTLS, TCP through kube-proxy)
     hosts: feosd ─────────────┘   dials FERUN_NODES_IP:7443 to join,
                                   then every instance's pod address
```

## What it needs

- Kubernetes 1.27 or newer (k3s v1.27+): the manifests use topology
  spread constraints with matchLabelKeys and nodeTaintsPolicy, and a
  PodDisruptionBudget with unhealthyPodEvictionPolicy.
- k3s with its traefik, and cert-manager with a ClusterIssuer
  `letsencrypt-prod`, which answers HTTP-01 through traefik or DNS-01 as
  [`../gardener/cluster.yaml`](../gardener/cluster.yaml) does.
- DNS: `run.<DOMAIN>` and `chat.<DOMAIN>` at the cluster's public
  address, where traefik listens.
- The images (`REGISTRY`): by default the public ones on GHCR,
  `ghcr.io/gardenlinux/feos`, which the nodes pull without a secret.
- An S3-compatible object store with a bucket for diskd, which the pods
  hibernate into.
- An OpenID Connect provider, at which people sign in to ferund and
  FeChat, and an OpenAI-compatible LLM gateway for FeChat.
- On the machine that deploys: docker, kubectl, `envsubst` (gettext), and
  what `make packages` needs ([`docs/development.md`](../../docs/development.md)).

## Parameters

What differs from one cluster to another is a parameter, set in
`config.env` in this directory: a copy of
[`config.env.example`](config.env.example), which git ignores. A variable
on make's command line wins over the file.

| Parameter | What |
|---|---|
| `DOMAIN` | the DNS zone: ferund at `run.<DOMAIN>`, FeChat at `chat.<DOMAIN>`; `run.<DOMAIN>` is FeRun's trust domain too |
| `REGISTRY` | where the images are pushed and pulled; default `ghcr.io/gardenlinux/feos`, public, pulled without a secret |
| `OIDC_ISSUER` | the OpenID Connect provider, exactly as its discovery document states it |
| `FERUN_NODES_IP` | the ClusterIP of the service `ferund-nodes`, which feosd on the hosts dials: a free address of the service network; default `10.43.70.70`, in k3s's default `10.43.0.0/16` |
| `FECHAT_MODEL` | FeChat's model at the LLM gateway |

The manifests say `${NAME}` where a parameter goes. The Makefile's
`apply-*` targets fill them in with `envsubst`, write the result to
`target/k3s/manifests/` and apply it; `make -C deploy/k3s manifests` only
writes them. The commands below run from the repository's root, with
`KUBECONFIG` at the cluster. Keep the secrets' files outside the
repository.

## The control plane

### Once: the CA

The platform CA's root stays offline, on the operator's machine; ferund
holds only the issuer. With ferund built (`make go-build`), `<root>` and
`<ca>` two new directories outside the repository:

    target/go/ferund init-ca --root-dir <root> --dir <ca> \
      --trust-domain run.<DOMAIN> --dns-domain run.<DOMAIN>
    kubectl apply -f deploy/k3s/ferun/namespace.yaml
    kubectl -n ferun create secret generic ferun-ca --from-file=<ca>

That makes the control-plane domain `ferun.run.<DOMAIN>`, the name the
instances' certificates on the nodes' port carry and the nodes check
(`CONTROL_PLANE_NAME` in configmap `feos-node`).

### Once: the secrets

    kubectl -n ferun create secret generic ferund-postgres --from-literal=password=$(openssl rand -hex 24)
    kubectl -n ferun create secret generic ferund \
      --from-literal=FERUN_DATABASE_URL="postgres://ferun:<password>@postgres.ferun.svc.cluster.local:5432/ferun?sslmode=disable" \
      --from-literal=FERUN_ADMIN_USERS="<user>:<password>" \
      --from-literal=FERUN_BOOTSTRAP_TOKEN=$(openssl rand -hex 32)
    kubectl -n ferun create secret generic ferund-secret-key --from-literal=key=$(openssl rand -hex 32)
    kubectl -n ferun create secret generic oidc-client-ferun \
      --from-literal=client_id=<client ID> --from-literal=client_secret=<client secret>

The secret key seals environments' secrets and addons' credentials at
rest (ADR 0018); without it ferund refuses both. Losing it loses every
sealed value, so keep a copy beside the CA's root.

The bootstrap token becomes a token of the tenant `operator` while there is
no tenant: FeChat's `FERUN_TOKEN`. The admin users are a fallback: people
sign in to ferund's UI and admin API at the OpenID Connect provider, and
its group `admins` (the `groups` claim) gets the admin surface.
`oidc-client-ferun` is ferund's client there, with the redirect URI
`https://run.<DOMAIN>/auth/callback`.

### Deploy

    make -C deploy/k3s push-ferund apply-ferun    # build and push the image, apply

Afterwards, a new image: `make -C deploy/k3s deploy-ferund`.

ferund rolls one instance at a time: the new one starts first and takes
requests once /readyz passes (normally when the nodes have dialled it, at
the latest 30 s after its start); a PodDisruptionBudget lets a drain stop
one instance at a time. On more machines, raise replicas over a
replicated database, as [`../gardener/ferun`](../gardener/ferun) does.

## Nodes

Built from `make packages` (FeOS's .deb packages, ADR 0009), with a version
per build so that apt upgrades the nodes:

    make packages FEOS_VERSION=0.6.0~dev.$(date -u +%Y%m%d%H%M).$(git rev-parse --short=7 HEAD)
    make -C deploy/k3s push-feos-node apply-feos

Then the secrets (the images are public on GHCR, `REGISTRY`, and need
none): diskd's object store, the
object store's variables of
[`hack/packages/diskd/default`](../../hack/packages/diskd/default), with
a key prefix of the cluster's own; and a join token for every node, under
its name. Labelling a node installs FeOS there:

    kubectl -n feos create secret generic diskd-s3 \
      --from-env-file=<s3.env> --from-literal=DISKD_S3_PREFIX=<prefix>/
    args=""
    for n in <node> ...; do
      args="$args --from-literal=$n=$(kubectl -n ferun exec deploy/ferund -- /ferund join-token --ttl 2h)"
    done
    kubectl -n feos create secret generic feosd-join $args
    kubectl label node <node> ... feos.fecode.dev/node=true

Every node joins once, with the token of its name; afterwards its key and
certificate are in `/var/lib/feos/node` on the node. A node added later
needs its key in `feosd-join` before its label. A node that lost its key
joins again with a token pinned to its name (`join-token --node <name>`).
Afterwards, new packages: `make -C deploy/k3s deploy-feos`, which builds,
pushes and rolls the image, a node at a time.

The manifests assume a network that routes no IPv6 to the nodes. With
the control plane, ferund runs with `FERUN_POD_NETWORK=mesh`: each node
gets a /96 of the cluster's ULA from it and reaches the other nodes' pods
through the WireGuard device `feos-mesh`, UDP port 51820 between the
nodes (ADR 0017). Without, each node takes `fd00:fe05:<last octet of its
address, hex>::/64`, which no other node reaches. Either way pods leave
over IPv4 alone. Pods are denied the nodes' and the cluster's networks
(configmap `feos-node`).

On a node, `feosctl` talks to feosd as usual, and the services' logs are
in its journal (`journalctl -u feosd -u diskd`), and in `kubectl -n feos
logs <pod> -c journal`.

FeOS keeps to its own state (`/var/lib/feos`), devices (`pod-…` TAPs),
nftables table (`feos`) and addresses (169.254.0.0/16 on the TAPs), so
the nodes can run other workloads beside it.

## FeChat

The secrets of [`fechat/README.md`](../../fechat/README.md), with
`FERUN_TOKEN` the bootstrap token, then the manifests and the images:

    make -C deploy/k3s apply-fechat
    REGISTRY=<REGISTRY> fechat/deploy.sh <tag>
