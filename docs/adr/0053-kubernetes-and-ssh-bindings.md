# ADR 0053: Kubernetes and SSH bindings: the host adds the credential

**Status:** Proposed · **Date:** 2026-10-15

An agent in a pod deploys to a Kubernetes cluster or pushes over SSH
without ever holding the token, the client certificate or the SSH key.
A Kubernetes binding is a loopback port in the pod with a plaintext
Kubernetes API, whose requests the pod's `feos-authn` carries to the
bound cluster with the credential added; an SSH binding is an agent
socket whose host side signs only a `publickey` login to a server that
proves it holds a bound host key, as the bound user, with the bound key.
Both are sets sealed to the pod's process (ADR 0014), stated whole by
the control plane, which checks each with a preflight before any pod
has it. Every request and every signature is recorded before it is
answered.

## Context

- Agents deploy and push. A Kubernetes credential or an SSH key in the
  pod is compromised the moment the pod is (ADR 0001, ADR 0014): it
  stays valid from anywhere for as long as it lives.
- ADR 0014's dividing line: a binding that adds a credential the pod
  must never hold runs on the host, in `feos-authn`, which therefore
  sees the plaintext of those connections; the Kubernetes API's HTTP and
  the SSH agent protocol are such bindings. A binding that adds only the
  identity is the guest's.
- The guest pipes a Kubernetes binding's connections and the agent
  socket to the host (ADR 0007), and the control plane stores both kinds
  sealed at rest and seals them into a pod's credentials (ADR 0014).
  The host side that answers them is what this ADR decides.

## Decision

### Kubernetes bindings

- A binding names an API server, the name to check its certificate for,
  its CA or the system's roots, and a credential: a bearer token or a
  client certificate. The pod sees a plaintext Kubernetes API on a
  loopback port of the guest; the guest pipes each connection to the
  host's one vsock port with a preamble that names the port (ADR 0014),
  and the port is not forwardable.
- `feos-authn` speaks HTTP/1.1, one upstream connection per guest
  connection. After an upgrade both sides are a pipe, so `exec`,
  `attach`, `port-forward` and watches pass through.
- It resolves the binding's server, checks every address it would dial
  against the node's denied prefixes, and dials the address it checked.
  It drops `Authorization`, `Impersonate-*` and `Proxy-*` from the
  request and adds the credential. A denied destination answers 403, an
  unreachable one 502. A request never leaves without the credential.
  What the credential may do is RBAC in the target cluster.
- Not supported: exec plugins, cloud auth providers, OIDC with refresh.

### SSH bindings

- A binding names a server with its host keys, a user and a private key
  (Ed25519, ECDSA, or RSA of 2048 bits or more); a binding without a
  server is to any server. The pod sees an agent socket.
- The agent lists its identities, binds a session
  (`session-bind@openssh.com`, without forwarding), and signs only a
  `publickey` user authentication for a bound session whose host key,
  user and key match one binding. Everything else is refused; a client
  without session binding gets nothing. RSA signs with SHA-2 only.

### Both

- Sets, sealed, versioned, stated whole (ADR 0014). The control plane
  stores them with every credential sealed at rest, validates them,
  checks each with a preflight that dials from an instance, and shows
  them with their use from the pods' audit records. A change reaches a
  running pod at its next start, resume or restore.
- Audit before answering: every Kubernetes request (binding, host,
  method, path, status, bytes), every SSH signature (binding, host key,
  user), every refusal with its reason; the records are the pod's events
  (ADR 0014).

## Alternatives considered

- **The credential in the pod**, narrowed and short-lived: it stays
  valid from anywhere for as long as it lives, and a hijacked agent
  sends it out in one command.
- **A TLS-intercepting proxy with a CA per pod** for the cluster's API:
  the CA would be baked into the pod, and the platform would sit in the
  plaintext of every connection, not only the bound cluster's (ADR
  0014).
- **An agent that signs for any session**: without session binding the
  host cannot tell which server a signature is for, and a signature made
  for a bound server could be presented to another.
- **Proxies dialing from the node's address**: refused in ADR 0014.

## Consequences

- Kubernetes and SSH need nothing in the pod: `kubectl`, client-go,
  `ssh` and `git` work as they are, with a kubeconfig that names the
  loopback port and `SSH_AUTH_SOCK` pointing at the agent socket.
- Offloading protects the bytes, not the authority: a Kubernetes
  credential that may read secrets gives the agent a real token in one
  command. Narrow roles, short lifetimes, rate limits and the audit are
  the boundary.
- `feos-authn` parses hostile HTTP and SSH on the host, in its jail (ADR
  0014).

## History

Decided 2026-10-15 as part of ADR 0014 (4c7f609, 33e5905), after the
sandbox system's Kubernetes proxy and SSH agent; the control plane's
side was built 2026-10-16 (5a46e9b), and the guest's pipes come from
ADR 0007. Split out of ADR 0014 on 2026-11-08 as a decision of its own,
still to be built on the node.
