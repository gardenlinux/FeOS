feos-authn
==========

A pod's credentials on its node, in a process of its own per running pod
([ADR 0014](../adr/0014-identity-and-credentials.md)). Its first part is
the HTTP side ([ADR 0028](../adr/0028-http-bindings.md)): the pod's HTTP
bindings, reached at the node address `169.254.169.1`, by a name on port
80 or under the upstream's own name with TLS on port 443, to which
feos-authn adds the credential. Its second is FeID's signer: the pod's
identity for its own TLS, whose key stays here, served over vsock port
10002 with [feid/1](../../feid/docs/protocol.md) ([below](#feids-signer)).
The Kubernetes and SSH bindings ([ADR 0053](../adr/0053-kubernetes-and-ssh-bindings.md))
are not built yet: their sets are opened and kept with the HTTP bindings,
and nothing serves them. Outbound TLS bindings are the guest's
([guest.md](guest.md#containers)), with this process only signing.

The code is `crates/feos-authn`; the binary ships in the `feosd` package
as `/usr/sbin/feos-authn`, beside `feosd`.

```
pod ──TCP 169.254.169.1:80 / :443──▶ feosd (accepts; knows the pod by the source address)
                                       │ SCM_RIGHTS over <pod dir>/authn.sock
                                       ▼
                                 feos-authn (one per pod, jailed) ──TLS, from the node's address──▶ upstream
                                       │                          └─(OAuth)──▶ token endpoint
                                       └──▶ <pod dir>/authn-audit.ndjson ──▶ feosd ──▶ events, sink
```

## The process

`feosd` starts it with the pod's microVM, on create, resume and restore,
for a pod with HTTP bindings or an identity (`PodSpec.identity`), and
stops it when the run ends:

    feos-authn --pod-id <pod ID> --audit-fd 3 --control-fd 4 [--vsock-fd 5]

- `--pod-id`: the pod's ID as the control plane writes it, a short ID or
  a UUID in lowercase hyphenated form. It is part of the sealed box's
  additional data.
- fd 3: the audit file, `<pod dir>/authn-audit.ndjson`, opened by
  `feosd` for appending.
- fd 4: a listening `SOCK_SEQPACKET` Unix socket that `feosd` bound at
  `<pod dir>/authn.sock`.
- fd 5, with `--vsock-fd`: a listening stream socket on which FeID's
  signer serves, for a run without `feosd`. `feosd` hands that socket
  over the control socket instead (`vsock`).
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
- no network namespace of its own yet: the process shares the node's,
  dials upstreams from the node's address and checks every address it
  dials against `--deny-prefix` and the node's networks itself. The
  namespace of the node's proxies (a veth to the node, blackhole routes
  for the denied prefixes, the pods' second addresses from which a
  binding's connections would leave) is decided (ADR 0014) and not
  built; the backlog has it.

What feos-authn does to itself before it serves (`crates/feos-jail`, which
the guest's TLS bindings use too):

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
| `vsock` | `inode` and exactly one fd | the listening socket `<vm dir>/v.sock_10002`, which Firecracker connects the guest's streams to vsock port 10002 to, and its path's inode. FeID's signer serves on it from now on; the one before closes, its accepted streams go on. |

Before `hello`, everything but `hello` is ignored.

feos-authn → `feosd`:

| `op` | Fields | Meaning |
|---|---|---|
| `credentials` | `hpkePublicKey` (b64, 32 bytes), `csr` (b64 DER PKCS#10, Ed25519) | ask the control plane (`IssuePodCredentials`); relay the answer as `credentials` or `error`. At start, after `refetch`, after an error, and again on a new connection when an answer was pending. |
| `ready` | `version` | a set is opened and serves. Sent after every set, and after every `hello` once there is one, so that a new `feosd` knows. |
| `error` | `message` | an answer that cannot be used (a box that does not open); it asks again later, and the set in place keeps serving. |
| `vsock` | `inode` | the inode it was given with the vsock listener it serves on; after it took one, and after every `hello` once it has one, so that a new `feosd` does not bind it again. |

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
 "spanId":"b7ad6b7169203331","parentSpanId":"00f067aa0ba902b7","redacted":0,"error":null}
{"ts":"…","type":"http_end","binding":"erp","traceId":"0af7…319c","spanId":"b7ad6b7169203331",
 "status":200,"bytesIn":0,"bytesOut":5120,"redacted":1,"durationMs":40,"error":null}
```

- `http`: `binding` and `mode` (`name`, `intercept`) are null when no
  binding matched, and `host` is then what the pod asked for; otherwise
  `host` is the upstream's. `path` has no query string. `durationMs` is
  until the answer's head. `upstreamAddr` is the address dialed, null
  for a refusal before any. `credential` is `headers`, `basic`,
  `oauth2` or `clientCertificate`, never a value. `redacted` is how many
  places of the credential were replaced in the answer's head (its reason
  and header values) or in a refusal's message (below, "Redaction").
  `error` is a refusal's `{"code","message"}` (the codes below), null for
  the upstream's answers, whatever their status.
- `http_end`: once the answer's body is done, or for an upgrade the
  tunnel. `bytesIn` is the request body's bytes (an upgrade's: what the
  pod sent through the tunnel), `bytesOut` the answer body's (what it
  received). `redacted` is how many places of the credential were
  replaced in the whole answer: head, body and trailers, or what came
  through the tunnel from the upstream. `durationMs` is from the
  request's arrival to its end. `error` is set when it did not end as it
  should: `aborted` (the pod went away first), `upstream_body`,
  `upgrade`, `tunnel`, `unredactable` (an upstream's WebSocket frame that
  cannot be searched). A process that is killed leaves an `http` without
  its `http_end`.
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

A request whose path does not begin with `/` (an asterisk, an
authority-form request, an HTTP/2 `:path` without its `/`) is 400
`bad_request`. The upstream's URL is the binding's origin and the
request's path and query, and a binding's connections dial its
upstream's host and port alone, whatever a URL names. Then:

1. The binding's path prefixes, when it has any: a prefix holds whole
   segments (`/api` holds `/api` and `/api/x`, not `/apix`); a path with
   dot segments (with parameters too: `..;x`), `\`, or encoded `/`, `\`
   or `.` is refused. Outside:
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
   `X-Forwarded-Proto: http`. When its answer is redacted (below): its
   `Accept-Encoding` is `identity`, and it loses `Range`, `If-Range` and
   `Sec-WebSocket-Extensions`.
5. The upstream: resolved with the system's resolver; every address
   checked against `denyPrefixes`, and never one that is no unicast
   address (unspecified, `0.0.0.0/8`, broadcast, multicast), nor a
   loopback or link-local one (`127.0.0.0/8`, `::1`, `169.254.0.0/16`,
   `fe80::/10`), IPv4-mapped ones too, whatever `hello` says. One denied
   address refuses the name: 403 `denied_destination`. The checked
   address is dialed, bound to `sourceV4`/`sourceV6` when given
   (`IP_BIND_ADDRESS_NO_PORT`); nothing answers: 502 `unreachable`.
   TLS 1.3 or 1.2 with ALPN `h2` and `http/1.1`, SNI the upstream's
   host, the server checked against the binding's CA, or the WebPKI's
   roots (built in) without one; a failure: 502 `tls`. Connections are
   pooled per binding (16 idle, 90 s).
6. An upgrade to WebSocket (`Connection: upgrade` with `Upgrade:
   websocket` alone, HTTP/1.1) goes over a pool of the binding's that
   offers `http/1.1` only; a 101 makes both sides a pipe, the upstream's
   frames redacted on their way. A request for another upgrade goes on
   as a request, without its `Upgrade`.
7. The answer loses its hop-by-hop headers and `Alt-Svc`. In the name
   mode a `Location` or `Content-Location` on the upstream's origin
   (absolute or scheme-relative) becomes `http://<name>…`. Then its
   reason and header values are redacted, a gzip answer is unpacked, and
   an answer with a body that cannot be searched is 502 `unredactable`
   (below). Bodies pass as they stream, never buffered whole, redacted on
   their way.

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
  in the background at three quarters of its lifetime (`expires_in`, 15
  minutes when the endpoint says none) while it serves on; only a
  request without a valid token waits for a fetch, and the old token
  serves while a renewal fails. A failed fetch is not tried again for 5
  seconds. On a 401 from the upstream the token is renewed (at most once
  per 10 seconds per binding), the requests meanwhile wait for the new
  one, and an idempotent request without a body is repeated once; any
  other request gets the 401, and the next one the new token. The
  client authenticates with Basic of the form-encoded ID and secret, or
  in the body (`clientAuthInBody`). The token endpoint is dialed under
  the same address checks, its server checked against the WebPKI and
  the binding's CA. Only `Bearer` tokens.
- `clientCertificate`: the chain and key for mTLS to the upstream.

### Redaction

The credential never goes back to the pod, whatever the upstream repeats
of it: an API key in a quota error's body, a token in `WWW-Authenticate`
or in a `Location`'s query, a request echoed for debugging
(`src/redact.rs`). The values, each at least **8 bytes** long, byte for
byte:

- `headers`: every header's value, and of one of the form `<scheme>
  <credentials>` (`Bearer …`, `Token …`) the credentials alone too;
- `basic`: `user:password`, its base64, and the password alone; not the
  user alone, which is no secret;
- `oauth2`: the client secret as it is, form-encoded, and the base64 of
  `client_secret_basic`; the token in use and the one before it;
- `clientCertificate`: each line of the key's PEM base64, so that the
  key is found as PEM, joined, or with escaped line breaks. An upstream
  never receives the key; this is for one that has it from elsewhere.

A shorter value (a tenant header of `42`) is not redacted: it would mask
ordinary text. A value the upstream changes — escapes (`\/` in JSON),
encodes otherwise, cuts itself (an API's own paging of a text) — is not
recognised.

Where a value appears, in the answer's reason (HTTP/1.1), its header
values, body and trailers, and the upstream's WebSocket frames, it is
replaced by a mask of the **same length**: `redacted-by-FeOS-binding`
when it fits, `redacted` when that fits, `*` for the rest, so that a
24-byte key becomes `redacted-by-FeOS-binding` and a 12-byte one
`redacted****`. ASCII without spaces, so that a masked header, URL or
token stays one. Never the value or a hash of it. Its length keeps
`Content-Length`, a frame's length and every offset true; nothing about
the answer's framing changes. Overlapping places are masked as one.

A body is searched as it streams. A chunk goes on at once, without a
copy when it holds no value, but for its end when that end could begin
one: those bytes, fewer than the longest value, wait for the next chunk,
so that a value split across chunks is found. A body that ends gives up
what waited. A WebSocket tunnel is searched frame by frame: each
payload of the upstream's as it streams, ended with its frame; a value
split across two frames of one message is not found.

So that every answer can be searched, a request of a binding with a
value to redact asks for it unencoded and whole: `Accept-Encoding:
identity` (the pod's own wish for gzip is answered unencoded, as a
server may), no `Range` or `If-Range` (so that a pod cannot put a value
together from parts that each hold less of it), no WebSocket extensions
(permessage-deflate). Some servers send gzip anyway (an ERP vendor's API
sandbox does, whatever it is asked). Such an answer is unpacked as it
streams and goes to the pod unpacked: without `Content-Encoding` and
`Content-Length`, its `ETag` made weak (`W/"…"`). A chunk is unpacked a
step at a time, each step at most 32 KiB of output, so that a gzip bomb
holds no more than that in the proxy and comes out at the pod's pace; a
gzip stream that is cut or broken ends the body with an error
(`upstream_body`). An answer with a body that comes with another content
or transfer coding (br, zstd, gzip after another one), or a 206, is
refused with 502 `unredactable`; one without a body (HEAD, 204, 304, an
empty one) passes as it came. An upstream's WebSocket frame with RSV
bits (compressed) or a mask ends the tunnel (`http_end` error
`unredactable`).

The proxy's refusals are redacted too: their message may hold what a
token endpoint said. So are the records and the log of a token fetch.
The records count the places replaced (`redacted` in `http` and
`http_end`), never the values; `feosd` puts the count on the request's
span (`ferun.redacted`).

### Refusals

The proxy's own answers carry `FeOS-Binding-Error: <code>` and
`{"error":{"code":…,"message":…}}`:

| Status | Code | |
|---|---|---|
| 400 | `bad_request` | the request's path does not begin with `/` |
| 403 | `denied_destination` | the upstream (or token endpoint, as 503) resolves to a denied address |
| 403 | `denied_path` | the path is outside the binding's prefixes |
| 404 | `unknown_binding` | no binding of the name, or none any more |
| 405 | `method_not_allowed` | CONNECT |
| 421 | `misdirected` | an intercepted connection's request for another host |
| 429 | `rate_limited` | the binding's rate limit, or its requests in flight |
| 502 | `unreachable` | no address, none answered, or an upgrade that cannot be carried |
| 502 | `tls` | the TLS handshake or the server's certificate failed |
| 502 | `unredactable` | the answer has a body that cannot be searched for the credential: encoded otherwise than gzip, or a range |
| 503 | `credential_unavailable` | no OAuth token can be had |
| 503 | `not_ready` | the pod's bindings have not arrived |
| 503 | `binding_invalid` | the binding cannot be served |
| 503 | `audit_unavailable` | the request's record cannot be written |

### Limits

1024 of the pod's connections at once (more are closed), 256 requests in
flight per binding, 30 s to read a request's head, 5 s to resolve, 10 s
to connect, 10 s for TLS, 15 s for a token. A body's search holds back
fewer bytes than the binding's longest value.

## FeID's signer

The pod's TLS stack fetches its certificate chain and the trust bundle
and has its TLS 1.3 CertificateVerify signed with the pod's Ed25519 key,
which this process made and never gives out
([feid/1](../../feid/docs/protocol.md), the crate `feid-proto`; the
libraries are in `feid/`).

```
container ──/run/feos/feid.sock──▶ feos-guest ──vsock 2:10002, "feid\n"──▶ Firecracker
    ──<vm dir>/v.sock_10002──▶ feos-authn (accepts on the listener feosd handed it)
```

- **The socket.** `feosd` binds `<vm dir>/v.sock_10002`, mode 0666 in
  the microVM's jail, once the jail is there, and hands it over
  (`vsock`); again when the file is gone or is not the one the process
  said it holds, as when the microVM is made anew. The process accepts
  on it itself, so that a `feosd` that restarts stalls no handshake.
- **The preamble.** A stream's first line, at most 64 bytes, within 5 s,
  says what it is for: `feid` is feid/1. Anything else is closed. It
  carries no authority: the socket is the pod's.
- **The identity** is the certificate of `credentials`, taken when its
  first certificate is for the run's key and carries its `spiffeId` as
  its URI name; one that is not is logged and the one in place serves
  on. It is renewed at `renewAfter` with the same keys.
- **Waiting.** `get_certificate`, `get_ca` and `sign` wait up to 5 s for
  a certificate valid now, then are refused with `unavailable.identity`,
  retriable: a pod just resumed, an issuer late. An expired certificate
  is logged once, loudly.
- **The check** of every input to `sign` is feid-proto's: `hash` must be
  `none` and the message a TLS 1.3 CertificateVerify input, client or
  server, with a hash of 32 or 48 bytes.
- **Limits.** 256 of the pod's streams at once (more are closed), one
  request at a time on each, a frame of 1 MiB, 10 s for the hello.

Its audit records, written before the answer:

```json
{"ts":"…","type":"tls_signature","identity":"spiffe://…/sandbox/k2v8d0h3q9x","role":"client","conn":3,"error":null}
{"ts":"…","type":"tls_signature","identity":"spiffe://…","role":null,"conn":3,
 "error":{"code":"refused.not_certificate_verify","message":"…"}}
{"ts":"…","type":"handshake_report","identity":"spiffe://…","conn":3,
 "report":{"direction":"outbound","tuple":"…","version":"TLS 1.3","cipher_suite":"…","sni":"db","peer_spiffe":"…"}}
```

- `tls_signature`: a signature given (`error` null) or refused, with the
  role whose CertificateVerify it was, null when the input was no
  CertificateVerify at all. A signature whose record cannot be written
  is not made: `unavailable.audit_log`.
- `handshake_report`: what a library reported of a handshake, its
  fields cut to 512 characters and its lists to 64 entries.
- `conn` numbers the run's feid/1 connections. FeID's libraries send
  their reports over a connection of their own, so a signature and the
  report of its handshake share the identity and the moment, not the
  number. `feosd` publishes both as the pod's audit events.
