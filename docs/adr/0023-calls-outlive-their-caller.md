# ADR 0023: A keyed tool call outlives its caller

**Status:** Accepted · **Date:** 2026-10-27

A tool call with a side effect may carry an idempotency key, unique per
sandbox. Such a call is a record before it runs and runs on a context of
its own, bounded by its preparation and its timeout and not by the
request, so that a caller that hangs up cancels nothing. The record
commits that the call was sent before the request leaves for the guest,
so its outcome is exact: done, interrupted (it may have run) or not run
(it did not, and the same call may be sent again). A repeat of the key
with the same arguments attaches to the running call or gets its stored
result on any instance, other arguments are refused, the record is
readable without running anything, and a cancel is explicit. A dead
instance's call ends by its lease and is never run again.

## Context

An agent's tool call reaches `ferund` as an MCP call or a REST exec and
would run on the request's context: the exec in the sandbox ends when
that context does. A caller that goes away mid-call, because its process
crashed, was deployed or lost the connection, would cancel a `bash` that
may have run half-way, with its side effects half done and its result
nowhere. FeCode's own setup commands run for up to 600 s, and the model's
`bash` names whatever timeout it likes. ADR 0015 says a broken stream is
the client's to reopen, which is right for a console or a port forward
and wrong for a call with side effects: reopening does not tell the
caller what the first call did, and repeating it does the work twice.

FeCode takes its turns over between instances on a deploy or a crash
(FeCode's ADR 0010). A turn taken over in the middle of a tool call can
only write "interrupted; the result is unknown" into the transcript
unless `ferund` remembers the call apart from the request. The process
tools show the shape: `process_start` runs a process detached from any
request, records it, and lets a later call list it, read its output and
stop it. The MCP door is stateless, with signed session IDs for the
clients that open with `initialize`, and a cancel reaches whichever
instance runs a call; FeCode's MCP client opens with `server/discover`,
which has no session, so its cancels can reach nothing by session.

## Decision

### A call with a key is a record

- A tool call may carry an **idempotency key**: in MCP the call's
  `_meta` field `ferun.callId`, in REST the header `Idempotency-Key`. The
  key is the caller's: FeCode derives its keys from its session, its
  reply and the model's tool call ID. A call without a key runs on its
  request's context.
- **The key is unique per sandbox**, not per token: tokens are revocable
  and deleted with their sandbox, and every token of a sandbox sees all
  of its files. The argument hash refuses a key reused for another call.
- Keys are 1 to 255 characters of `A-Z a-z 0-9 . _ ~ : @ -`: never
  percent-encoded in a URL path, and safe as an RFC 8941 sf-string,
  which therefore holds no escape.
- **Only tools with side effects are recorded**: `bash`, `write`, `edit`,
  `process_start` and `process_stop`. A read-only call's key is accepted
  and not recorded; the call runs on its request.
- A call with a key is written to a table before it runs: the sandbox,
  the key, the tool, a hash of the arguments, the instance that runs it,
  its state, whether it was sent, its lease, and, once it ended, its
  result. The row is the call; the request is one way of waiting for it.
- **The same arguments** means the same SHA-256 over the tool and its
  normalised parameters, decoded into the tool's parameter struct and
  encoded again: the door, whitespace, key order and absent versus zero
  make no difference.
- **Four states, and `sent`**: `running`, `done`, `interrupted` and
  `not_run`. The row commits `sent` before the call's request leaves for
  the guest, and a call whose record cannot be marked is not sent.
  `not_run` is a stored state: the guest never got the call, and a
  repeat of the same call under the same key replaces the row with a new
  run. `interrupted` means the call may have run and stopped without a
  recorded result. A cancel by the caller is final, `done` with the
  cancel's error, even before the send.
- The record is bounded as the tool's output is: the output as the tool
  cuts it, the error made valid text and cut.

### The call runs detached from the request

- The call runs on a context of its own, bounded by the sandbox's
  preparation and the tool's timeout and not by the request's: the
  request's end, by a client that hung up or an instance that drained
  the request, leaves the call running. The instance renews the row's
  lease while the call runs and writes the result when it ends, whether
  or not anyone waits.
- **A keyed `bash` names at most 600 s.** A larger timeout is refused,
  and the limit is advertised. The keyed calls that run, and the
  requests that wait for calls they did not start, are bounded per
  sandbox and instance; one more is refused at once and retryably, so
  that one token cannot hold unbounded goroutines on an instance.
- A request waits for its call's result. A second request with the same
  key and the same arguments **attaches** to the running call and gets
  its result when it ends, or gets the stored result at once when it
  ended; on another instance it waits on the row, woken by a
  notification, since the call runs where it started and the node
  connection is that instance's (ADR 0015). Concurrent requests with one
  key attach, where the IETF Idempotency-Key draft answers 409. **The
  same key with other arguments is refused**, so a key never silently
  runs a second command.
- Progress after a reattach is heartbeats only: no door streams a call's
  output, so nothing is replayed to a request that attaches.
- **The record is readable without running anything**: a read by key,
  with a bounded wait for the end, returns the result as MCP renders it.
  A client that lost an answer reads it there instead of sending the
  call again.
- **Cancellation is explicit**; a hung-up caller cancels nothing. A
  cancel by key works for every client, on whichever instance runs the
  call. MCP's `notifications/cancelled` reaches a keyed call only for
  clients with an initialize session, whose door keeps the mapping for a
  while after the request ended and remembers cancels that arrive first,
  bounded per token; sessionless clients, FeCode among them, cancel by
  key. A request that has already ended when its call would start starts
  nothing.
- Results are kept for **an hour** after the call ended, then deleted;
  a key may be used again after that, and the caller that needs a result
  longer has read it by then.

### A dead instance's call ends by its lease and is never run again

- The instance renews the leases of its running calls in one statement;
  a key is recorded only while that renewal runs, and refused as
  `not_run` otherwise.
- **Three writers end a dead instance's call**, `interrupted` when it
  was sent and `not_run` when not: the leader, once it has led for a
  lease and its database has not failed within one; any instance waiting
  on the row, once its own database has not failed within a lease and
  the lease ran out more than a lease ago, so that calls also end while
  an older leader runs during a rollout, or after the leader died with
  the instance; and a start under the same instance ID.
- **`ferund` never repeats an interrupted call**: the caller reads that
  the call was interrupted and decides. What the exec did in the sandbox
  is in the sandbox. A read-only call is not recorded, and its caller
  repeats it cheaply.
- A call on an instance that lost its database stops when the nodes drop
  the instance once its row is stale, which ends its execs, or, on
  reconnect, when the instance cancels a call whose renewal misses it.
  Its final write is refused.

### A stopping instance drains its calls

- Once a stopping instance stops accepting requests, a keyed call reaches
  the guest only if its timeout plus a margin fits before the shutdown
  deadline; otherwise it ends `not_run`, and the client sends it
  elsewhere. Requests waiting on another instance's call are told to ask
  again.
- After the HTTP shutdown, the instance waits for its remaining keyed
  calls until the deadline, then cuts and records them. The shutdown
  grace, 11 minutes by default, covers a keyed `bash` of the full cap,
  and whatever stops the process waits that long and a minute more.

### Clients can tell

- The door advertises the capability, the recorded tools, the retention
  and the longest timeout, at `initialize` and `server/discover`. Every
  answer of a keyed call carries the call's state and whether it is
  another request's result replayed.
- Every tool call with a side effect is covered. The port door, watches
  and the console are streams and stay ADR 0015's: the client's to
  reopen.

## Alternatives considered

- **The caller retries** without a key: a `bash` runs twice; a file is
  written twice, which is harmless, or appended twice, which is not.
- **Every tool idempotent by construction**: possible for the file
  tools, impossible for `bash`.
- **Every exec a process tool**, started detached and polled: the same
  machinery with an asynchronous face for every call, and an agent's
  loop that polls; a key keeps the synchronous call and adds the record
  underneath.
- **Keeping the call on the request's context**: the caller's
  availability becomes the sandbox's correctness.
- **Forwarding a repeated call to the instance that runs it**, instead
  of waiting on the row: the routing layer ADR 0015 set aside; waiting
  on a row and a notification needs none.
- **Results kept until acknowledged**: a caller that never acknowledges
  keeps every result; an hour is longer than any takeover. **A flag for
  the retention**: nothing asks for another value.
- **Keys per token**: a token is revocable and goes with its sandbox,
  while every token of a sandbox sees all of its files.
- **Recording the read-only calls**: a `read` would store pictures of up
  to 3.75 MB, for nothing.
- **Deleting the row of a call that did not run**: a caller whose answer
  was lost could not tell "did not run" from "no record".
- **Recovering a lost answer by sending the call again with its key**:
  in a mixed-version window or after a rollback, the repeat can reach an
  instance that ignores keys and runs the call twice; a read of the
  record runs nothing.
- **Clamping a longer timeout silently**, or no cap: the first changes
  the call, the second gives detached calls and drains without bound.
- **Ending a dead instance's calls at once, or by the leader alone**:
  after a failover every lease looks expired; during a rollout a leader
  of the older version ends none, and attached requests would wait
  without end.
- **A self-fence by time**, an instance cancelling its calls once its
  renewals failed for a while: in a failover of the whole database the
  nodes keep every connection and the leader's guard prevents false
  verdicts already, so the fence would only kill every keyed call on a
  failover longer than its threshold; a threshold long enough to avoid
  that is longer than the verdict and guarantees nothing.
- **Mapping sessionless MCP cancels** by token hash and request ID:
  request IDs repeat across FeCode processes that share a token.
- **Handing a call to another instance at a drain**: the exec stream
  lives on this instance's node connection.
- **A shutdown grace of 2 minutes**: a FeCode `bash` would be cut on
  every deploy. A grace that covers preparation and 600 s, 18 minutes,
  is unnecessary with the send gate.
- **A prefixed `_meta` name** such as `ferun.dev/callId`: clients depend
  on `ferun.callId`, and nothing collides with it.

## Consequences

- A call runs on while its caller is away, up to its timeout: a `bash`
  of 600 s started and abandoned holds the sandbox awake for 600 s, as it
  would have for a caller that waited; the hold is the call's, not the
  request's.
- Ending the call of an instance that lost its database relies on the
  nodes' handling of the instance list: a node keeps its list only while
  telemetry fails everywhere.
- An old instance lingers for up to its grace and a minute while new
  ones serve: a rolling deploy takes that long when a call runs.
- FeCode reads the call's record after a takeover or a lost answer, and
  sends a call again only after a `not_run` answer (FeCode's ADR 0010).
- An agent that repeats a call with the same ID by mistake gets the
  first call's result, which is the right answer to a repeated call.
- The table is additive: an older instance ignores the key, the header
  and the cancel's kind, so both versions run side by side during a
  rollout.

## History

Proposed on 2026-10-27 and built on 2026-10-28 (33ea94e), together with
the replication of `ferund` to three instances; the record's shape and
the three writers were settled while it was built. FeCode's turn
takeover (its ADR 0010) reads the record since the same day.
