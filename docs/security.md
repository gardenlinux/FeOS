Security
========

Where FeOS and FeRun protect less than their ADRs intend, each with why
it matters and what closes it. An entry goes, with its fix, in the commit
that closes it; what is closed, the architecture describes.

## Open

### A registry token enters the pod while its guest pulls

**Since** ADR 0025's second step (registry credentials with the create).
**Intended:** no credential enters a pod (ADR 0001).

Until the node pulls images itself, a pod whose create brings registry
credentials pulls its image in its guest, with the Bearer token `ferund`
got from the registry's realm for the question before the create. The
credentials themselves (a password, a token of GHCR's, a project's pull
credential of FeCode's) stay in `ferund` and its database; the token
does not. It is scoped to `repository:<name>:pull` and lives as long as
the registry says (Docker Hub 300 s, FeCode's registry 5 minutes,
FeCode's ADR 0012), in `feos-guest`'s memory while it pulls. A pod gets
the token of its own create's credentials only, never another's.

**Who can take it:** code in the sandbox that reads the guest's memory
or its traffic to the registry while the token lives. `feos-guest` runs
in the same microVM as the containers, so a container that escapes into
the guest can; the likely attacker is an agent in an attachable sandbox
that a prompt turned.

**What it gives:** every tag and digest of that one repository while the
token lives, older releases and unreleased builds among them, not only
the image the pod runs. No other repository, no push, nothing after the
token's end.

**Meanwhile:** the token rides beside the pod's spec, never in it, in its
record or on its disk; `feosd` holds it in memory for one bring-up, and
the guest takes it out of the request at once into a type whose `Debug`
is redacted, so that no log shows it; a registry that takes only Basic
authentication is refused (ADR 0025), so that no password enters a pod;
a baked layer holds no token, since the guest keeps it in memory; and a
pod that starts on a baked layer pulls nothing, so `feosd` keeps the
token its create carried from its guest.

**What closes it:** the node pulls (ADR 0022's host pull and bake pod).
`feosd` holds the token in memory for the pull, and the pod gets a baked
layer: no pod sees the registry, and Basic authentication works too.

### A routed cluster trusts the sources its network delivers

**Since** ADR 0010 (routed pod prefixes). **Intended:** nothing but a
pod's own node puts the pod's address on a packet (ADR 0017), since the
nodes admit by source address (ADR 0012, ADR 0034).

In a meshed cluster a node takes a packet from another node only over
WireGuard, with that node's key and a source in its /96, and drops a
source of the cluster's ULA that comes in another way. In a routed
cluster, every node with a prefix the network routes to it
(`--pod-prefix`), the nodes' traffic goes over the network as it is:
`prerouting` checks a pod's source at its TAP, but nothing checks that a
packet from the uplink with a source in another node's prefix came from
that node.

**Who can use it:** whoever can put packets on the network between the
nodes with a forged source: another node's root, or another machine on
that network, where the network does not filter sources itself.

**What it gives:** packets into whatever the forged pod is admitted to,
one way: the answers go to the real pod, so a TCP connection does not
come up, but a single packet does, and UDP where a rule admits any
protocol (a sandbox that `reaches` all). Traffic between nodes is not
encrypted either, so the same machines can read it.

**Meanwhile:** a meshed cluster does not have it. A routed one relies
on its network's source filtering, which its operator must check.

**What closes it:** routed nodes on the mesh too (ADR 0017's open
question), or a check of a source in another node's prefix against the
network's next hop for it.
