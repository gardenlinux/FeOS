FeID
====

FeID gives an application mutual TLS with its workload's identity
without the application ever holding the key. The application runs an
ordinary TLS 1.3 handshake from its own socket; its certificate comes
from the platform, and its CertificateVerify is signed by a signer
outside it, over [feid/1](docs/protocol.md). A server that reads the
peer's certificate sees the calling workload's SPIFFE ID; a database can
map it to a role.

On FeOS the signer is the pod's `feos-authn` on the host, which makes the
key in its own memory and never gives it out
([ADR 0014](../docs/adr/0014-identity-and-credentials.md)). A pod has
its workload's ID,
`spiffe://<trust domain>/tenant/<tenant>/env/<environment ID>/service/<name>`
for a service's every replica, and its subject names the pod
(`CN=<trust domain>/<pod ID>`) and the workload (`OU=wl-…`), on which an
addon maps a role ([ADR 0036](../docs/adr/0036-a-workloads-identity.md)).

```
feid/
  docs/protocol.md   feid/1: framing, versions, operations, the check of
                     sign, error codes, golden frames
  feid-proto/        the protocol in Rust: the client the libraries use,
                     the server feos-authn uses, the CertificateVerify check
  feid/              the Rust library on rustls: configurations for clients
                     (servers checked by name, or pinned to a SPIFFE ID) and
                     servers (clients required), the peer's ID, handshakes
                     over tokio that report themselves; examples/loopback.rs
                     is a workload's handshake with itself
```

The Go library follows, and a development signer that runs feid/1 on a
Unix socket with a CA made for the run, so that an application runs the
same on a laptop and on FeOS.

FeID started from FeTLS, a prototype of the same design; feid/1 is its
fetls/1 on the wire. It lives in this directory without dependencies on
the rest of FeOS, versioned on its own, so that it can move into a
repository of its own once others use it.

Apache-2.0, as FeOS.
