feos-authn
==========

A pod's credentials on its node, in a process of its own per running pod
([ADR 0014](../adr/0014-identity-and-credentials.md)). Its first part is
the HTTP side ([ADR 0028](../adr/0028-http-bindings.md)): the pod's HTTP
bindings, reached at the node address `169.254.169.1`, by a name on port
80 or under the upstream's own name with TLS on port 443, to which
feos-authn adds the credential. The pod's identity, FeID and the TLS,
Kubernetes and SSH bindings are not built yet: their sets are opened and
kept with the HTTP bindings, and nothing serves them.

The code is `crates/feos-authn`; the binary ships in the `feosd` package
as `/usr/sbin/feos-authn`, beside `feosd`.

```
pod ──TCP 169.254.169.1:80 / :443──▶ feosd (accepts; knows the pod by the source address)
                                       │ SCM_RIGHTS over <pod dir>/authn.sock
                                       ▼
                                 feos-authn (one per pod, jailed) ──TLS, from the pod's second address──▶ upstream
                                       │                          └─(OAuth)──▶ token endpoint
                                       └──▶ <pod dir>/authn-audit.ndjson ──▶ feosd ──▶ events, sink
```

## The process

`feosd` starts it with the pod's microVM, on create, resume and restore,
and stops it when the run ends:

    feos-authn --pod-id <pod ID> --audit-fd 3 --control-fd 4

- `--pod-id`: the pod's ID as the control plane writes it, a short ID or
  a UUID in lowercase hyphenated form. It is part of the sealed box's
  additional data.
- fd 3: the audit file, `<pod dir>/authn-audit.ndjson`, opened by
  `feosd` for appending.
- fd 4: a listening `SOCK_SEQPACKET` Unix socket that `feosd` bound at
  `<pod dir>/authn.sock`.
- stderr: its log (`RUST_LOG`, `info` by default). Never a credential.

It survives a restart of `feosd`: a new `feosd` connects to the control
socket again, which replaces the old connection, and adopts the process
by its pid and start time.

At every start it makes its keys in memory, never anywhere else: the
pod's Ed25519 identity key and its signing request (whose subject the
control plane ignores), and an X25519 key to which the control plane
seals the bindings. Both serve the run; a `refetch` asks again with the
same keys.

### The jail

What `feosd` sets up at spawn (the node agent's side):

- a user ID of the pod's own, and a cgroup (`pids.max`, memory);
- a mount namespace with an empty root. The libc resolver reads
  `/etc/resolv.conf` (and `/etc/hosts`) there: the root needs a
  `resolv.conf` naming a resolver that the process's network namespace
  reaches, or no upstream name resolves;
- the network namespace of the node's proxies: a veth to the node,
  blackhole routes for every `--deny-prefix` and the node's own
  networks, and the pods' second addresses, from which a binding's
  connections leave.

What feos-authn does to itself before it serves (`src/jail.rs`):

- closes every descriptor but 0, 1, 2, the audit file and the control
  socket;
- `PR_SET_DUMPABLE` 0; rlimits: no core file, no locked memory, no
  message queues, at most 4096 descriptors;
- drops every capability: ambient, bounding (where it may), permitted,
  effective, inheritable;
- `no_new_privs`, and a seccomp allowlist on all its threads (TSYNC),
  installed once its runtime's threads exist: what a tokio proxy with
  rustls and the libc resolver calls. `socket` is IPv4 and IPv6 only,
  `clone` threads only and into no new namespace, `open`/`openat`
  reading only, `mmap`/`mprotect` never executable, `ioctl` FIONBIO and
  FIOCLEX, `prctl` thread names, `prlimit64` reading. `clone3` fails
  with ENOSYS, so that the libc falls back to `clone`. Another
  architecture is killed; every other call fails with EPERM, which the
  code that made it returns as an error. `execve`, `ptrace`, `unshare`,
  `listen`, `socketpair` and the rest are among them.

The process also checks every address it dials itself (below), so that a
gap in the namespace's routes is no way into the node network.

## The control protocol

One JSON object per seqpacket message, `"v": 1` in each. File
descriptors pass only with `conn`, by `SCM_RIGHTS`. A message of another
version, or of an unknown `op`, is ignored; within a version everything
is additive. A message may be up to 1 MiB; one larger than `feosd`'s
send buffer (about 208 KiB by default) needs a larger `SO_SNDBUF` there.

`feosd` → feos-authn:

| `op` | Fields | Meaning |
|---|---|---|
| `hello` | `podId`, `denyPrefixes` (`["10.0.0.0/8", …]`), `sourceV4`, `sourceV6` (an address or null) | first on every connection. A `podId` that is not the process's closes the connection. Invalid prefixes or addresses: nothing is dialed until a valid `hello`. |
| `credentials` | `certificate` (an object or null), `bindings` (`{"enc": b64, "ciphertext": b64, "version": …}` or null) | the answer to a credentials request; null bindings are an empty set of version `""`. The certificate is kept and not used yet. |
| `error` | `message` | the request failed (e.g. `RESOURCE_EXHAUSTED`): asked again after 2 s, doubling up to 60 s. |
| `refetch` | | the pod's bindings changed: ask again with the same keys; the new set replaces the old whole. |
| `conn` | `port` (80 or 443), `peer` (`"ip:port"`, for the log) and exactly one fd | an accepted TCP connection of the pod's to `169.254.169.1:<port>`. Without exactly one fd it is closed. |

Before `hello`, everything but `hello` is ignored.

feos-authn → `feosd`:

| `op` | Fields | Meaning |
|---|---|---|
| `credentials` | `hpkePublicKey` (b64, 32 bytes), `csr` (b64 DER PKCS#10, Ed25519) | ask the control plane (`IssuePodCredentials`); relay the answer as `credentials` or `error`. At start, after `refetch`, after an error, and again on a new connection when an answer was pending. |
| `ready` | `version` | a set is opened and serves. Sent after every set, and after every `hello` once there is one, so that a new `feosd` knows. |
| `error` | `message` | an answer that cannot be used (a box that does not open); it asks again later, and the set in place keeps serving. |

The sealed box is `feos.fleet.v1.PodBindings`, HPKE (RFC 9180) base mode,
DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, ChaCha20-Poly1305, info `feos
bindings v1`, additional data the pod's ID, a zero byte and the version
(`go/internal/pki/hpke.go`). `src/hpke.rs` is tested against RFC 9180's
vector and a box `pki.Seal` sealed (`testdata/sealed-bindings.json`,
which `go/internal/pki`'s `TestBindingsVector` opens too and
`FEOS_WRITE_BINDINGS_VECTOR=1` writes anew).

Until the first set arrives, a request on port 80 waits 5 seconds, then
gets 503 `not_ready`; a connection to port 443 waits as long and is then
closed.

## The audit file

One JSON object per line, written with one `write(2)` under a lock; a
request is recorded **before its answer's head leaves**, when its status
is known, and a request whose record cannot be written is refused (503
`audit_unavailable`). Bytes and the whole duration are only known at the
end, so each request has two records, joined by `spanId`:

```json
{"ts":"2026-11-05T12:00:00.123456789Z","type":"http","binding":"erp","mode":"name",
 "method":"GET","host":"api.erp.example","path":"/odata/x","status":200,"durationMs":12,
 "upstreamAddr":"192.0.2.10:443","credential":"headers","traceId":"0af7…319c",
 "spanId":"b7ad6b7169203331","parentSpanId":"00f067aa0ba902b7","error":null}
{"ts":"…","type":"http_end","binding":"erp","traceId":"0af7…319c","spanId":"b7ad6b7169203331",
 "status":200,"bytesIn":0,"bytesOut":5120,"durationMs":40,"error":null}
```

- `http`: `binding` and `mode` (`name`, `intercept`) are null when no
  binding matched, and `host` is then what the pod asked for; otherwise
  `host` is the upstream's. `path` has no query string. `durationMs` is
  until the answer's head. `upstreamAddr` is the address dialed, null
  for a refusal before any. `credential` is `headers`, `basic`,
  `oauth2` or `clientCertificate`, never a value. `error` is a refusal's
  `{"code","message"}` (the codes below), null for the upstream's
  answers, whatever their status.
- `http_end`: once the answer's body is done, or for an upgrade the
  tunnel. `bytesIn` is the request body's bytes (an upgrade's: what the
  pod sent through the tunnel), `bytesOut` the answer body's (what it
  received). `durationMs` is from the request's arrival to its end.
  `error` is set when it did not end as it should: `aborted` (the pod
  went away first), `upstream_body`, `upgrade`, `tunnel`. A process that
  is killed leaves an `http` without its `http_end`.
- `token`: a fetch of an OAuth token, before the token is used:
  `{"ts","type":"token","binding","host":<token endpoint>,"reason":
  "first_use"|"renewal"|"unauthorized","status":200|null,"expiresIn":
  3600|null,"durationMs","upstreamAddr","error"}`.
- `bindings`: a set opened and in use: `{"ts","type":"bindings",
  "version","http":[names],"invalid":[{"binding","message"}]}`.

Each `http`/`http_end` pair is a client span with the request's trace
context: `traceId`, `spanId` (the proxy's) and `parentSpanId` (the
pod's, null when the request started the trace).

## The HTTP side

### Name mode, port 80

The pod speaks HTTP/1.1 to `http://<name>/…`. `Host` (or an
absolute-form request's authority), lowercase without port or final dot,
selects a name-mode binding; none is 404 `unknown_binding`.

### Intercept mode, port 443

The ClientHello's SNI must be an intercept binding's upstream host;
anything else, a ClientHello without SNI included, is closed before a
certificate is sent. The certificate is a leaf for exactly that name,
ECDSA P-256, valid 24 hours from five minutes ago, issued in memory from
the newest (last) of the set's interception CAs and kept per name until
an hour before its end; a new set starts with none. ALPN offers `h2` and
`http/1.1`. A request whose authority is not the SNI is 421
`misdirected`. Intercepted upstreams are on port 443 only.

### A request

1. The binding's path prefixes, when it has any: a prefix holds whole
   segments (`/api` holds `/api` and `/api/x`, not `/apix`); a path with
   dot segments, `\`, or encoded `/`, `\` or `.` is refused. Outside:
   403 `denied_path`.
2. The rate limit, a token bucket per binding (`perSecond`, `burst`; a
   burst of 0 is one second's worth): 429 `rate_limited` with
   `Retry-After`. At most 256 requests per binding in flight, bodies and
   upgrades included: 429 `rate_limited`.
3. The credential (below); a token that cannot be had: 503
   `credential_unavailable`.
4. The request loses `Host`, `Authorization`, `Expect`, every `Proxy-*`,
   the hop-by-hop headers and those `Connection` names, and every header
   the credential sets; `TE: trailers` stays. It gains the credential,
   `Host` (the upstream's, with its port unless 443) and its trace
   context: `traceparent` with the proxy's span as parent (a new
   sampled trace when the request had no valid one), `tracestate` as it
   came. With `forwardedHost` (name mode): `X-Forwarded-Host: <name>`,
   `X-Forwarded-Proto: http`.
5. The upstream: resolved with the system's resolver; every address
   checked against `denyPrefixes`, and never one that is no unicast
   address (unspecified, `0.0.0.0/8`, broadcast, multicast). One denied
   address refuses the name: 403 `denied_destination`. The checked
   address is dialed, bound to `sourceV4`/`sourceV6` when given
   (`IP_BIND_ADDRESS_NO_PORT`); nothing answers: 502 `unreachable`.
   TLS 1.3 or 1.2 with ALPN `h2` and `http/1.1`, SNI the upstream's
   host, the server checked against the binding's CA, or the WebPKI's
   roots (built in) without one; a failure: 502 `tls`. Connections are
   pooled per binding (16 idle, 90 s).
6. An upgrade (`Connection: upgrade` with `Upgrade`, HTTP/1.1) goes over
   a pool of the binding's that offers `http/1.1` only; a 101 makes both
   sides a pipe.
7. The answer loses its hop-by-hop headers and `Alt-Svc`. In the name
   mode a `Location` or `Content-Location` on the upstream's origin
   (absolute or scheme-relative) becomes `http://<name>…`. Bodies pass
   as they stream, never rewritten, never buffered whole.

CONNECT is 405 `method_not_allowed`. A binding the set holds but that
cannot be served (no credential, a header value that is no header, a CA
that does not parse, an intercept upstream not on 443, an intercept
binding without a CA) keeps its name and answers 503 `binding_invalid`;
the `bindings` record and the log say why, never with a value.

### Credentials

- `headers`: the binding's header names and values.
- `basic`: `Authorization: Basic`.
- `oauth2`: client credentials (RFC 6749 §4.4). The token is fetched at
  first use (one fetch at a time per binding), kept in memory, renewed
  at three quarters of its lifetime (`expires_in`, 15 minutes when the
  endpoint says none); the old token serves while a renewal fails. A
  failed fetch is not tried again for 5 seconds. On a 401 from the
  upstream the token is renewed (at most once per 10 seconds per
  binding) and an idempotent request without a body is repeated once;
  any other request gets the 401, and the next one the new token. The
  client authenticates with Basic of the form-encoded ID and secret, or
  in the body (`clientAuthInBody`). The token endpoint is dialed under
  the same address checks, its server checked against the WebPKI and
  the binding's CA. Only `Bearer` tokens.
- `clientCertificate`: the chain and key for mTLS to the upstream.

### Refusals

The proxy's own answers carry `FeOS-Binding-Error: <code>` and
`{"error":{"code":…,"message":…}}`:

| Status | Code | |
|---|---|---|
| 403 | `denied_destination` | the upstream (or token endpoint, as 503) resolves to a denied address |
| 403 | `denied_path` | the path is outside the binding's prefixes |
| 404 | `unknown_binding` | no binding of the name, or none any more |
| 405 | `method_not_allowed` | CONNECT |
| 421 | `misdirected` | an intercepted connection's request for another host |
| 429 | `rate_limited` | the binding's rate limit, or its requests in flight |
| 502 | `unreachable` | no address, none answered, or an upgrade that cannot be carried |
| 502 | `tls` | the TLS handshake or the server's certificate failed |
| 503 | `credential_unavailable` | no OAuth token can be had |
| 503 | `not_ready` | the pod's bindings have not arrived |
| 503 | `binding_invalid` | the binding cannot be served |
| 503 | `audit_unavailable` | the request's record cannot be written |

### Limits

1024 of the pod's connections at once (more are closed), 256 requests in
flight per binding, 30 s to read a request's head, 5 s to resolve, 10 s
to connect, 10 s for TLS, 15 s for a token.
