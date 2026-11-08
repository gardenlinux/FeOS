feid/1
======

The protocol between an application's TLS stack and the signer that
holds its workload identity's key. The application fetches its
certificate chain and the platform's trust bundle, and has the
CertificateVerify of its TLS 1.3 handshakes signed; the key never enters
it. On FeOS the signer is the pod's `feos-authn` on the host
([ADR 0014](../../docs/adr/0014-identity-and-credentials.md)).

On the wire feid/1 is fetls/1, the protocol of the FeTLS prototype: a
library of either speaks to a signer of either. The Rust crate
[`feid-proto`](../feid-proto) implements both sides; its tests hold the
golden frames below.

## 1. Transport and attribution

- Any stream that is the caller's by construction: a Unix socket, a vsock
  stream. The protocol adds no cryptography of its own.
- **Attribution is the transport's.** No request names a workload. The
  signer knows whose stream it serves before it reads the first frame:
  on FeOS from the host-side socket the vsock stream arrives on, which
  belongs to one pod's microVM. A frame never selects an identity.
- On FeOS, the application dials `/run/feos/feid.sock` in its container,
  or where `FEID_SOCKET` points. The guest pipes each connection to the
  host's vsock port 10002 after the one-line preamble `feid\n`, which
  carries no authority; from then on the stream is feid/1.

## 2. Framing

- One frame is one compact JSON object, UTF-8, and `\n`. JSON as a
  conforming encoder writes it has no raw newline, so a line is a frame.
  Never pretty-printed. A `\r` before the `\n` is ignored.
- A frame is at most **1 MiB**, without its newline, either way. A reader
  enforces the cap while it reads, never after holding the whole line.
  After a frame too large the stream cannot be read on: the server sends
  `bad_frame` and closes.
- Requests and answers are strictly one at a time per connection, in
  order. One-way frames may come at any time and are never answered.

## 3. Versions

Every frame carries `"v"`. The first frame is the client's hello:

```json
{"v":1,"op":"hello","min":1,"max":1,"client":"feid-rs/0.1.0"}
```

`min` and `max` are the versions the client speaks, inclusive. `client`
is for the server's log only and never attributes anything. The server
takes the highest version both speak:

```json
{"v":1,"op":"hello_ack","proto":1,"server":"feos-authn/0.6.1"}
```

Without an overlap it answers `unsupported_version` and closes. After
the hello, every frame carries the version negotiated; a frame with
another is answered `unsupported_version`, and the connection closed.

A frame may carry `"id"`, an unsigned 32-bit number, which the answer
echoes, an error included. v1 clients need not send it.

## 4. Operations

### `get_certificate`, answered by `certificate`

```json
{"v":1,"op":"get_certificate"}
{"v":1,"op":"certificate","chain_pem":"-----BEGIN CERTIFICATE-----\n..."}
```

The workload's chain, PEM, its own certificate first. The request has no
fields and never will.

### `get_ca`, answered by `ca_bundle`

```json
{"v":1,"op":"get_ca"}
{"v":1,"op":"ca_bundle","ca_pem":"-----BEGIN CERTIFICATE-----\n..."}
```

The platform's trust bundle, PEM.

### `sign`, answered by `signature`

```json
{"v":1,"op":"sign","hash":"none","message":"ICAg..."}
{"v":1,"op":"signature","signature":"YWJj..."}
```

`message` and `signature` are base64, the standard alphabet, padded.

The signer checks, before the key touches anything:

- `hash` is `none`: the message is the whole input, unhashed, as TLS
  hands it to an Ed25519 key. Anything else is
  `refused.unsupported_hash`.
- The message is exactly a TLS 1.3 CertificateVerify input (RFC 8446
  §4.4.3):

  ```text
  64 × 0x20 || context || 0x00 || transcript hash
  ```

  with the context `TLS 1.3, client CertificateVerify` or `TLS 1.3,
  server CertificateVerify`, and a transcript hash of 32 or 48 bytes.
  Anything else is `refused.not_certificate_verify`.

This is what keeps the signer from being a signing oracle: it signs only
what it can read as the commitment to a TLS 1.3 transcript. The check is
part of the protocol, so that every signer makes it the same.

A signature is recorded in the signer's audit before it is given. When
the record cannot be written, the signature is not made:
`unavailable.audit_log`, retriable.

### `handshake_report`, one-way

```json
{"v":1,"op":"handshake_report","report":{"direction":"outbound",
 "tuple":"[fd00::2]:51234>[fd00::3]:5432","version":"TLS 1.3",
 "cipher_suite":"TLS_AES_128_GCM_SHA256","sni":"db","alpn":"postgresql",
 "peer_spiffe":"spiffe://example.org/db","peer_fingerprint":"9f2c..."}}
```

What the library saw of one completed handshake: metadata of the peer's
certificate (subject, issuer, serial, expiry, fingerprint), never its
bytes, never a byte of the connection. Never answered; lost under
pressure by design. A report that does not parse is logged by the
signer, not answered.

### `wire_event`, one-way

A passive wire observer's report, which fetls/1 had. A signer that does
not take it drops it without an answer.

### `error`

```json
{"v":1,"op":"error","in_reply_to":"sign","code":"refused.not_certificate_verify",
 "message":"refusing to sign: not a TLS 1.3 CertificateVerify input (…)","retriable":false}
```

| Code | Meaning | Retriable |
|---|---|---|
| `bad_frame` | no JSON, or too large; after too large, closed | no |
| `bad_request` | JSON, but no valid request: the hello not first, a payload of the wrong shape | no |
| `unsupported_version` | no overlap, or a frame of another version; closed | no |
| `unsupported_op` | an op the server does not know: how a client learns it talks to an older one | no |
| `not_attributed` | the transport names no workload; closed | no |
| `refused.not_certificate_verify` | the input is no CertificateVerify | no |
| `refused.unsupported_hash` | a `hash` other than `none` | no |
| `unavailable.audit_log` | the audit record could not be written | **yes** |
| `unavailable.identity` | no valid certificate yet or any more: a pod just resumed, a renewal late | **yes** |
| `identity.unprovisioned` | the workload has no identity at all | no |
| `internal` | anything else; the message never carries the signer's detail | no |

`retriable` missing is false: a client never asks again what it was
refused, unless the frame says so. A transport failure, a stream that
ended, is not a refusal: the library may dial again. A client handles a
code it does not know as `internal`.

## 5. Limits

| Limit | Value |
|---|---|
| frame | 1 MiB, enforced while reading |
| requests in flight | one per connection |
| the hello | within 10 seconds of the connection, on FeOS |
| reports | one-way; the socket's buffer, then dropped by the library |

## 6. Rules within a version

- New fields may be added to any frame, and are optional; a receiver
  ignores fields it does not know.
- New ops are added; an older peer answers `unsupported_op`.
- Names, types and meanings of fields never change.
- New codes are added; a client handles one it does not know as
  `internal`.
- Anything else, the check of `sign` included, is a new version,
  negotiated by the hello.

## 7. Golden frames

`feid-proto`'s tests write and read these exactly:

```text
{"v":1,"op":"hello","min":1,"max":1,"client":"fetls-go/1.0"}
{"v":1,"op":"hello_ack","id":3,"proto":1,"server":"test-signer/1"}
{"v":1,"op":"get_certificate"}
{"v":1,"op":"certificate","id":9,"chain_pem":"-----BEGIN CERTIFICATE-----\nleaf\n-----END CERTIFICATE-----\n"}
{"v":1,"op":"ca_bundle","ca_pem":"bundle"}
{"v":1,"op":"sign","hash":"none","message":"IA=="}
{"v":1,"op":"error","id":4,"in_reply_to":"delete_everything","code":"unsupported_op","message":"unknown op \"delete_everything\"","retriable":false}
```
