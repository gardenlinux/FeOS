FeOS on Kubernetes
==================

How FeOS nodes that are Kubernetes nodes come and go with the cluster:
`feos-k8s`, in `go/`. The decision behind it:
[ADR 0020](../adr/0020-feos-on-kubernetes.md) (FeOS in a pod per node,
and a controller that joins, drains and removes nodes). The pod per node,
`feos-node`, is described there; this page is about the controller.

## What feos-k8s does

`feos-k8s` runs as a Deployment of one replica (strategy `Recreate`)
beside ferund. It speaks to the Kubernetes API with its service account's
token and to FeRun's admin API (`/api/v1/admin/…`) with an admin user's
Basic credentials. It does not watch: every `--interval` (5 s) a pass reads
the cluster's nodes, the nodes of `--node-selector`, FeRun's nodes, the
guard pods and the join secret, and acts on the difference. Every pass is
idempotent; what has to outlive a restart of the controller lives in
Kubernetes (a node annotation, the guards, the secret's keys) or in FeRun
(its nodes and join tokens). A FeOS node's name is its Kubernetes node's
name, which must be a DNS label (FeRun's node names are); a selected
node with another name is skipped with a warning.

### Join

For every selected node that FeRun does not know, or knows as `removed`,
the controller makes a join token pinned to the node's name (`POST
/api/v1/admin/join-tokens {node, ttlSeconds}`, `--join-token-ttl`, 1 h)
and puts it into the secret `--join-secret` (`feosd-join`) under the
node's name, creating the secret if there is none. A pinned token joins a
new name as well as a removed one. While the key is there and FeRun lists
an unused, unexpired token pinned to the node, nothing more happens; a
token that expired unused is replaced by a new one. Once FeRun has the node
`ready`, the key is removed: the token is used up. Tokens are never
logged; the log names the token's ID and expiry.

A node whose identity forked (`identityForked`: two holders used its
certificate) gets no token: that is an operator's call. A node an
operator removes in FeRun while its Kubernetes node is still selected
joins again; to take a node out of FeOS, take the label off it.

The pod on the node reads `/<mount>/<node name>` from the secret, mounted
as a volume (not `subPath`, `optional: true`): a new node's pod starts
before its key exists, and the kubelet brings the key into the volume
within its sync period (about a minute), so the pod waits for the file
rather than failing.

### Guard and drain

For every selected node that is schedulable, the controller keeps a guard
pod, `feos-guard-<node>` in `--namespace` (`feos`):

| Field | Value |
|---|---|
| labels | `app.kubernetes.io/name: feos-guard`, `feos.fecode.dev/node: <node>` |
| `spec.nodeName` | the node: no scheduler, no cordon in the way |
| image | `--guard-image`, `registry.k8s.io/pause:3.10`, as user 65535, read-only, no capabilities |
| requests | `cpu: 1m`, `memory: 4Mi`; no limits |
| tolerations | one, `operator: Exists`: every taint, `NoExecute` too |
| `priorityClassName` | `--guard-priority-class`, `system-node-critical` |
| `terminationGracePeriodSeconds` | 0 |
| service account token, service links | off |

A PodDisruptionBudget ([below](#the-poddisruptionbudget)) refuses every
eviction of a guard. A drain, by the machine controller of Gardener,
`kubectl drain --force` or the cluster autoscaler, cordons the node and
evicts its pods; the guard's eviction is refused and retried, so the
drain waits. The controller sees the cordon (`spec.unschedulable`) and:

1. annotates the node `feos.fecode.dev/drain: requested`, unless FeRun
   drains the node already;
2. drains it in FeRun (`POST /api/v1/admin/nodes/{node}/drain`), unless
   it drains already: FeRun keeps placement off the node and hibernates
   its sandboxes into the object store, restoring those that ran on
   other nodes;
3. deletes the guard (a `DELETE` of the pod, not an eviction) once
   FeRun has the node drained, or does not know it, or has it `down` or
   `removed`, or answers the drain 409 (down or removed meanwhile): then
   nothing on the node waits for FeRun. The drain goes on and the machine
   goes.

**Drained** is what FeRun's drain loop writes (`internal/sandbox`,
`drainNode`): the node has `draining: true` and the status description
`drained`, which the loop writes on a pass that finds nothing on the node
but hibernated, lost and pending sandboxes and no op in flight
(`CountUnevacuated` is 0). Until then it writes `draining: <n> remain`.
`cannot drain: …` (a node without an object store) keeps the guard; the
controller logs it once per change and the drain waits, until an operator
acts or the machine controller's drain timeout (about 2 h on Gardener)
deletes the machine anyway. The loop runs only for nodes the leader is
connected to; a node it cannot reach keeps its last description and its
guard until FeRun has it down.

No guard is made for a cordoned node. When a node with the annotation is
schedulable again, the controller undrains it in FeRun (`POST
…/undrain`, which clears the drain's description), removes the
annotation, and makes the guard on the next pass. A drain an operator
began in FeRun has no annotation: a cordon waits for it, and an uncordon
leaves it.

### Removal and cleanup

A FeRun node whose name begins with `--node-name-prefix`, that has no
Kubernetes node, and that FeRun has `down` (or `joined` without ever
reporting) for `--remove-after` (1 min, counted by the controller, which
starts over after a restart) is removed: `DELETE
/api/v1/admin/nodes/{node}?force=true`. By force, because the sandboxes
a drain hibernated keep the node's name, and whatever else FeRun has on
it was on a machine that is gone; they go through the down sweep and the
lost grace at once. Nodes without the prefix, other clusters' or bare
machines', are never removed. The prefix is required.

Guards of nodes that are gone or no longer selected are deleted, and the
join secret's keys of nodes that are gone are removed.

## Flags

Every flag has an environment variable `FEOS_K8S_<NAME>`, the flag's name
in upper snake case; a flag wins over its variable.

| Flag | Default | What it is |
|---|---|---|
| `--ferun-url` | required | FeRun's front door, e.g. `http://ferund.ferun.svc.cluster.local:8100` |
| `--ferun-credentials-file` | required | a file with `user:password` of one of ferund's `--admin-users`, read at every request |
| `--node-name-prefix` | required | only FeRun nodes with this prefix are removed; on Gardener the pool's, `shoot--<project>--<shoot>-<pool>-`: the shoot's alone also matches the nodes of a shoot whose name begins with it |
| `--node-selector` | `feos.fecode.dev/node=true` | the label selector of the nodes that run FeOS |
| `--namespace` | `feos` | the guards' and the join secret's namespace |
| `--join-secret` | `feosd-join` | the secret with a join token per node name |
| `--join-token-ttl` | `1h` | a join token's lifetime; one that expires unused is replaced |
| `--guard-image` | `registry.k8s.io/pause:3.10` | |
| `--guard-priority-class` | `system-node-critical` | |
| `--interval` | `5s` | the time between passes |
| `--remove-after` | `1m` | how long a FeRun node is down without a Kubernetes node before it is removed |
| `--health-listen` | `:8080` | `GET /healthz`: 503 when no pass ended within 6 intervals (at least a minute), which a pass that hangs would show; empty: none |
| `--log-level`, `--log-json` | `info`, text | |
| `--kube-server` | in-cluster | the API's URL instead of the in-cluster one, without a token (development, `kubectl proxy`) |

`make feos-k8s` builds it static (`CGO_ENABLED=0`) into
`target/go/feos-k8s`.

## The FeRun credential

FeRun's admin routes take an admin's UI session (OIDC, for people) or
Basic credentials of `--admin-users`; the bootstrap token and project
tokens are refused there. So feos-k8s is an admin user of its own, which
FeRun's records then name as who acted (join tokens' `createdBy`, the
removal of a node, the drain's log line):

    pw=$(openssl rand -hex 32)
    # add feos-k8s:$pw to ferund's FERUN_ADMIN_USERS (comma-separated
    # user:password pairs; in the secret ferun/ferund), and roll ferund
    kubectl -n feos create secret generic feos-k8s-ferun --from-literal=credentials="feos-k8s:$pw"

and the file of the secret's key `credentials` is
`--ferun-credentials-file`. The password must not hold a comma. Inside
the cluster the front door is plain HTTP; the credentials cross the pod
network in the clear unless `--ferun-url` names the HTTPS front door.

## RBAC

A ClusterRole for the nodes, a Role in `--namespace` for the guards and
the secret, both bound to the controller's service account:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: feos-k8s
rules:
  - apiGroups: [""]
    resources: [nodes]
    verbs: [list, patch]            # the cordon; the drain annotation
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: feos-k8s
  namespace: feos
rules:
  - apiGroups: [""]
    resources: [pods]
    verbs: [list, create, delete]   # the guards
  - apiGroups: [""]
    resources: [secrets]
    verbs: [create]                 # the join secret, when there is none
  - apiGroups: [""]
    resources: [secrets]
    resourceNames: [feosd-join]
    verbs: [get, patch]             # its keys
```

## The PodDisruptionBudget

The guards have no controller, and Kubernetes' disruption controller
counts only pods with one for `maxUnavailable` and for a percentage: a
budget of `maxUnavailable: 0` over pods without a controller expects none
of them and allows every eviction. With pods without a controller only
an integer `minAvailable` works, as the minimum number of healthy pods
among those selected. One budget whose `minAvailable` is more than there
will ever be guards refuses every eviction, also of a guard that is not
ready:

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: feos-guard
  namespace: feos
spec:
  # More than there are guards: no eviction is ever allowed. Not
  # maxUnavailable: 0, which allows every eviction of pods without a
  # controller.
  minAvailable: 100000
  selector:
    matchLabels:
      app.kubernetes.io/name: feos-guard
```

`kubectl drain` refuses pods without a controller unless `--force`, and
with it reports the budget's refusal and retries until its `--timeout`.

## Failures

- **FeRun unreachable, or the credentials refused:** the pass ends early
  with the error logged; guards stay, nothing is drained, removed or
  joined. A drain waits for FeRun, at most until the machine
  controller's drain timeout.
- **The Kubernetes API unreachable:** the pass ends early; FeRun is left
  as it is.
- **A failure for one node** (a refused patch, a failed drain) is logged
  at the end of the pass; the other nodes are still handled, and the next
  pass tries again.
- **A restart** in any step: the annotation says which drains the
  controller began, the guards and keys say the rest; tokens in FeRun
  answer whether a key is still good. Only `--remove-after` starts over.
- **The controller down** while a node is replaced: the guard stays and
  the drain waits until the controller is back, or until the machine
  controller's drain timeout.
- **Two replicas** would do every step twice (two tokens for a node,
  two drains, which FeRun takes); the Deployment runs one,
  with `Recreate`.
