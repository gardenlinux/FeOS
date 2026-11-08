# ADR 0028: HTTP bindings: APIs by a name, the credential added on the node

**Status:** Accepted · **Date:** 2026-11-05

A pod calls an HTTP API by a name; its node adds the credential and dials
the API, and the pod never holds the secret. The node's resolver answers
the name with the node's address, where `feosd` accepts the connection
and hands it to the pod's own jailed `feos-authn` (ADR 0014), which
checks the request, adds the credential, speaks TLS to the upstream and
strips every trace of the credential from the answer. The name mode gives
the API a short name over plain HTTP on the pod's link; the intercept
mode keeps the API's own host name under TLS, with a certificate
authority of the pod's put into every container. Every request is
recorded and shipped to the sink, and the control plane opens incidents
for a credential that is refused or an upstream that fails. A binding
comes from a sandbox's own set, from a credential of the tenant (ADR
0033) or from an environment's deployment.

## Context

Code in pods calls HTTP APIs with credentials: an ERP system's OData API
with an API key, a payment provider with a bearer token, a model gateway,
a knowledge graph behind OAuth 2.0 client credentials, a partner's API
that wants a client certificate. A secret of the environment (ADR 0044)
in an environment variable puts the credential into the pod, and every
credential in a pod counts as compromised (ADR 0014). An agent's sandbox
gets none at all, so the agent either cannot try what it builds against
the API, or a person pastes the key into the conversation, a file or the
repository.

ADR 0014 sets the pattern: the material stays on the host, the pod gets
its use, the host knows the pod by the socket a connection arrives on. It
builds that for identities, TLS, Kubernetes and SSH, and leaves HTTP
APIs, tokens and a pod as an OAuth client, to this ADR.

ADR 0014's surface for a binding is a loopback port in the guest, piped to
the host over vsock. For HTTP APIs that costs a port per binding in every
pod, beside the application's own ports and the same in a sandbox and in
an environment or the code differs between them; a port number in every
URL the code holds, which means nothing to a reader or an agent; and a
piper per port in the guest. The node already has addresses that every
pod reaches and that mean something: the resolver (ADR 0010), which
answers names per pod from its name tables, and the node address where
pods send OTLP (ADR 0026). A binding can be a name that the resolver
answers with the node's address.

Some clients cannot use another name than the API's own: the API puts
absolute URLs into its answers (an OData service's paging links,
`Location`, links in HAL or JSON:API) and the client follows them to the
real host without the credential; an SDK has the base URL built in or
refuses plain HTTP; a request signature covers the host. For those the
platform must sit inside TLS under the API's own name: a man in the
middle with a certificate authority the pod trusts. ADR 0014 sets aside a
TLS-intercepting proxy for any destination the pod chooses, because the
platform would sit in the plaintext of every connection. Here interception
is a mode of its own, for the names a pod's bindings name and for those
only.

## Decision

### An HTTP binding

A pod's HTTP binding has a **name**, one DNS label as a service's (ADR
0018); an **upstream**, an origin over TLS, never plain HTTP, since the
credential would cross the network in the clear; the upstream's **CA**,
or none for the WebPKI; a **credential**; a **mode**, `name` (the
default) or `intercept`; and **options**: the path prefixes it may
reach (none: all), a rate limit, and whether the request names the
binding as its forwarded host.

HTTP bindings are a set beside the TLS bindings of ADR 0014 and the
Kubernetes and SSH bindings of ADR 0053: stated whole, sealed to the run's `feos-authn`, opened only
there, held in its memory only. Nothing of ADR 0014's sealing, jail,
audit file or start order changes for them.

### Reached at the node's address, never on a port of the guest

- A pod reaches its HTTP bindings at the node address of ADR 0026, on
  the standard ports: plain HTTP in the name mode, TLS in the intercept
  mode. Nothing listens in the guest, and the guest has no piper.
- `feosd` holds the two listening sockets, which outlive its restarts as
  the resolver's do (ADR 0010). It accepts a connection, knows the pod by
  its source address, which the pod's TAP pins, and passes the accepted
  socket to that pod's `feos-authn` over its control socket, without
  reading a byte. A connection from no pod's address, from a pod without
  HTTP bindings or from one whose process is not running is reset.
  Hostile HTTP is parsed only in the jailed process of one pod, as ADR
  0014 wants.
- `feosd`'s resolver answers a binding's name to the pod that holds the
  binding, and to no other: in the name mode the binding's label, in the
  intercept mode the upstream's host name, with the node address and
  nothing else, at a short TTL. It answers them whatever the pod's
  egress restricts (ADR 0012). A
  binding's name shares the space of the pod's name tables: the control
  plane refuses a binding that would resolve as a name an environment
  gives the pod, and a spec that names a binding and a service alike.
- It works the same for every pod: sandboxes, attached services, image
  services and jobs (ADR 0018, ADR 0035).

### The name mode, the default

- The pod speaks plain HTTP/1.1 to `http://<name>/<path>`. The link is
  the pod's own TAP to its node; nothing else is on it.
- `feos-authn` resolves the upstream, checks every address it resolves
  to against the node's denied prefixes and the node's own networks (one
  denied address refuses the name), dials the address it checked and
  speaks TLS 1.2 or 1.3 to it, HTTP/2 or HTTP/1.1, pooled per binding.
  The connection is the node's: it leaves from the node's own address
  and namespace, so the destination rule, not the pod's egress (ADR
  0012), bounds it, and the pod's traffic records (ADR 0013) do not see
  it; the binding's records do.
- The request loses `Authorization`, the proxy and hop-by-hop headers
  and every header the credential sets; it gains the credential, the
  upstream as its `Host` and the W3C trace context under a span of the
  proxy's. A request never leaves without the credential. A path outside
  the binding's prefixes is refused locally; a prefix holds whole
  segments, and dot segments and encoded separators are refused.
  Upgrades (WebSocket) and streamed bodies pass through as pipes;
  nothing is buffered whole.
- In the answer, a `Location` or `Content-Location` on the upstream's
  origin is rewritten to the binding's name. **Bodies are never
  rewritten** to the name: that would mean parsing every content type,
  compressed and streamed. With the forwarded-host option the request
  names the binding as its forwarded host and plain HTTP as its
  forwarded protocol, so that an upstream that builds its absolute URLs
  from them builds the binding's. An API that does not is a case for the
  intercept mode.
- **Nothing of the credential goes back to the pod.** Upstreams repeat
  what they were sent: an API key in a quota error's body, a token in
  `WWW-Authenticate`, a key in a `Location`'s query, a debug echo.
  `feos-authn` replaces every value of the binding's credential, in
  every form it is sent in (a password alone, a base64 pair, the current
  and the previous token, the lines of a key), wherever it appears: the
  answer's reason, header values, body, trailers and the upstream's
  WebSocket frames. The mask has the value's length, never the value or
  a hash of it, so that `Content-Length`, frame lengths and every offset
  stay true. Bodies are searched as they stream; only the bytes that
  could begin a value wait for the next chunk. So that every body can be
  searched, such a request asks for its answer unencoded and whole: no
  content coding, no range, no WebSocket extensions. An answer that
  comes gzip anyway is unpacked as it streams, a bounded step at a time,
  and passes unpacked; one in another coding, or partial, is refused.
  Values shorter than a minimum are left alone: they would mask ordinary
  text. A value the upstream transforms (escapes, encodes otherwise,
  splits itself) is not recognised. The proxy's own refusals and its
  records are redacted the same way.
- The proxy's own refusals carry a header of their own and a small JSON
  body with a code: a denied destination or path, an unreachable
  upstream or a failed TLS check, a credential that cannot be had, the
  binding's limit, an answer that cannot be searched. A client and an
  agent tell them from the upstream's own answers by that header.

### Credentials

- **Headers**: fixed header names and values, an API key, a bearer
  token.
- **Basic**: user and password.
- **OAuth 2.0 client credentials**: token URL, client ID and secret,
  scope or audience, and the client's authentication (Basic or in the
  body). `feos-authn` fetches a token at first use, keeps it in memory,
  renews it in the background at three quarters of its lifetime while
  the old one serves on, and on a 401 from the upstream renews once and
  repeats an idempotent request without a body once. The token endpoint
  is dialed under the same address checks. Bearer tokens only.
- **Client certificate**: a chain and key for mTLS to the upstream. Not
  the pod's identity, which is FeID's (ADR 0014).

A binding the set holds but that cannot be served (no credential, a
value that is no header, a CA that does not parse) keeps its name and
refuses its requests, so that the pod's names stay what the control
plane stated; the reason is in the records, never with a value.

### The intercept mode: the API's own name, a CA per pod

- The binding's name is the upstream's host name. The resolver answers
  it with the node address for the pods that hold the binding; the
  client connects with TLS to the HTTPS port and that name as its SNI.
  `feos-authn` answers with a certificate for exactly that name, issued
  by the pod's interception CA, and goes on as in the name mode: the
  same checks, credential, redaction and records. An intercepted
  upstream is on the HTTPS port, since the client connects to the API's
  own name and port.
- **Only the names of the pod's intercept bindings** reach the proxy.
  Every other name resolves as ever, and the platform sits in no other
  connection's plaintext. A connection whose SNI names no intercept
  binding of the pod is closed before a certificate is sent, and a
  request on it for another host is refused. This is the difference from
  the alternative ADR 0014 set aside, which would have intercepted every
  destination.
- **The CA** is one per pod, ECDSA and not Ed25519, so that clients
  without Ed25519 support (Java, .NET, older OpenSSL) accept it. The
  control plane makes it when the pod first intercepts a name, keeps its
  key sealed with the pod's bindings, and seals it to every run's
  `feos-authn` with them. A resumed or restored pod keeps the CA its
  processes trust: ADR 0014's rule that a key never leaves its process
  cannot hold for a key that must outlive the run, and this key is
  trusted by one pod only.
- The CA carries **name constraints**: permitted DNS names exactly the
  intercepted names at its issue, every IP address excluded, a CA for
  leaves only. A pod that comes to intercept a name its newest CA does
  not permit gets a new CA beside the old ones in its trust files, and
  `feos-authn` issues from the newest; one whose names shrink keeps its
  CA, so that a set and a part of it never make CAs by turns. Leaf
  certificates are short-lived and made in memory at first use.
- **The CA in every container, not only in the microVM**, and only in
  pods with an intercept binding. A container has a root filesystem and
  an environment of its own, so a CA in the guest's own store reaches
  none of them. Before a container starts, the guest mounts the pod's CA
  certificates read-only into it, mounts a bundle of the image's own
  system bundle and the pod's CAs read-only over every path the image
  keeps a system bundle at (an image with none gets it where Go and
  OpenSSL look), and sets, where the container's spec and its image do
  not, the variables by which OpenSSL, Go, Ruby, curl, Python's
  requests, Node and Bun take a trust store. This holds for init,
  sidecar and main containers alike, and the same environment goes to
  every process the guest starts in a container later: an exec, a
  sandbox's tool calls, the supervised processes of attached services
  (ADR 0018). The agent's `curl` and `npm` trust what the application
  trusts.
- **A container older than the pod's first CA**, a sandbox's, which runs
  before it gets bindings, has nothing mounted. Every process the guest
  starts in it later gets a bundle of its own instead, made at that
  start of the system bundle the container has then and the pod's CAs,
  and named by the same variables where the process's environment names
  none. Nothing covers the container's own bundle, so its package
  manager still replaces it. A process that was running before gets the
  CAs at its restart; the container needs none.
- When the pod's CAs change, the guest replaces every file and bundle so
  that a mount sees the new content and a reader a whole file; a process
  that read its store at start sees the new CA after its restart. Java's
  keystore is not covered.
- A client that pins its server's certificate fails here and uses the
  name mode. A client that resolves elsewhere (DNS over HTTPS, a fixed
  address) reaches the real upstream without the credential and fails
  closed.

### Every request is recorded, and the control plane watches them

- ADR 0014's audit, written before the answer, gains a record per
  request: binding, mode, method, host, path (no query string), status,
  bytes, duration, the upstream's address, the credential's kind (never
  its value), how many places of the credential were replaced in the
  answer, and refusals with their code; a token fetch and a set
  opened are records too. Bodies are never recorded.
- `feosd` publishes the records as the pod's events (ADR 0014) and ships
  them to the sink (ADR 0026) as the pod's egress records, under its
  labels, each a client span with the request's trace context: the
  ingress's access log (ADR 0021) in the other direction. The console
  reads them per environment as its outbound requests.
- **The records live in the sink, and only there.** `ferund` keeps none
  of a pod's requests in its activity log: a busy pod would fill
  Postgres with a row per request for the activity's retention. Of the
  events it keeps what the incidents need, bounded: the requests counted
  per pod, binding and minute of the database's clock, and the newest
  few records of each pod's binding as evidence, both deleted after
  minutes.
- `ferund` opens an incident (ADR 0018) on a binding of an environment's
  latest deployment whose requests over a window of minutes, given a
  handful of them, were mostly answered 401 or 403 by the upstream
  ("credential refused"), mostly 429 or 5xx or not answered ("upstream
  failing"), or mostly without a token ("token failing"), one condition
  at a time, with the counts and the newest records as evidence.
  Refusals for the pod's own mistakes and the platform's count for none.
  It closes once the condition has not held for a whole window, so that
  a share around a half does not open and close it by turns, and at once
  when the binding is gone. Window and thresholds are the architecture's.

### Limits

- A binding may carry a rate limit per pod, a token bucket of a burst
  that fills at a rate. Over it, `feos-authn` answers 429 itself, with
  `Retry-After`. It protects an upstream from an agent in a loop and a
  customer's system from a test.
- Requests in flight per binding and pod, and a pod's connections at
  once, are bounded; the bounds are the architecture's.

### Where bindings come from

A pod's HTTP bindings come from three places and from no other. Its
bindings' version covers all three, so that any change reaches its
`feos-authn`, which fetches the set again, and nothing restarts.

- **A sandbox's own set**, stated whole by the sandbox's own token, as
  its other bindings are (ADR 0014), the credentials as literals sealed
  per sandbox. FeCode gives its sandboxes bindings this way.
- **The tenant's credentials** (ADR 0033): a binding's definition stated
  once as the tenant's object, versioned, which a sandbox names at its
  latest version and an environment's binding names at a pinned one.
- **An environment's binding**, an object of the spec (ADR 0018, ADR
  0032): name, upstream, CA, mode, options, and a credential whose secret
  fields reference the environment's secrets by name, so that values
  keep their versions and are never in the spec. Services and jobs name
  the bindings they get, and a pod gets exactly those; an attached
  service has its sandbox's. A deployment's plan shows binding changes
  and warns of a secret that is no valid value. A new version of a
  referenced secret is sealed to the running pods again without a
  restart; a change of name, upstream or mode is a rollout.
- **Preflight**, before anything is stored, from the control plane:
  resolve and check the upstream, connect, check TLS, fetch a token
  where the credential is OAuth, and, if asked, send one `GET` or `HEAD`
  to a path within the binding's prefixes with the credential. It
  answers the addresses (none for a denied destination: the preflight is
  no resolver of the platform's own networks), the server certificate's
  subject and issuer, the token's lifetime, the status and the latency,
  never a body or a token. A stored binding is checked the same way.
- **A binding's upstream is egress the binding declares** (ADR 0012). A
  pod reaches it through the binding whatever its own egress allows: the
  resolver answers the binding's names and the node takes the connection
  for every pod that holds the binding, and the connection to the
  upstream is the node's.

## Alternatives considered

- **Loopback ports in the guest**, ADR 0014's surface. A port per
  binding in every pod, collisions with the application's own ports,
  numbers in URLs, a piper in the guest.
- **A port per binding at the node address.** No ports in the guest, but
  numbers in URLs and configuration, and a registry of ports per pod.
  Names are what the resolver already gives.
- **Paths at one address** (`http://<node>/erp/…`). The API's absolute
  paths, and every client that joins a base URL and a path its own way,
  break.
- **A forward proxy through `HTTP_PROXY`.** Runtimes honour it unevenly
  (Node's `fetch` does not), and HTTPS through `CONNECT` is opaque
  without interception.
- **Intercepting every destination**, a transparent egress proxy: ADR
  0014's rejected alternative, still rejected.
- **The intercept mode alone.** Every pod would need a CA in its trust
  files and every runtime its variables, and pinned clients would break.
  The name mode needs nothing in the pod.
- **Rewriting bodies in the name mode.** Every content type, compression
  and stream would need a parser in the proxy. The intercept mode solves
  the problem at its root.
- **Decompressing every answer to redact it**, instead of asking for it
  unencoded: a decompressor for every coding, and its bombs, in the
  jailed process, and the answer's length lost. Asking for identity
  stays the rule; most servers honour it. **Refusing every answer that
  comes encoded anyway** was the rule at first and was reversed for gzip
  alone: an ERP vendor's API sandbox sends gzip whatever it is asked,
  which made every answer of its binding a refusal, so gzip is unpacked
  in bounded steps, and other codings are still refused.
- **A fixed marker in place of a value**, of its own length: every
  `Content-Length` and WebSocket frame around it would have to be
  rewritten too.
- **A CA made by each run's `feos-authn`**, whose key never leaves it. A
  resumed pod's processes would trust a CA whose key is gone.
- **One CA per tenant or environment.** One key trusted by many pods;
  one per pod keeps a leak to that pod.
- **`feosd` as the proxy.** Hostile HTTP in the privileged process that
  owns every microVM (ADR 0014).
- **DNAT per pod to its `feos-authn`.** Works without passing sockets,
  but needs a port per process in their shared namespace and NAT state
  per connection; passing the accepted socket changes no table.
- **The pod's security groups as the bound of a binding's connections**,
  the rule at first: tenant security groups went when ADR 0012 derived reachability from references, and the
  connection is the node's, so the destination rule bounds it until
  bindings dial from the pod's address.
- **Credentials in environment variables**: every credential in the
  pod.

## Consequences

- An application calls `http://erp/…` and configures nothing else; it
  holds no key and has no auth code of its own, in any language. The
  same code runs in a sandbox, a preview and production; only the
  binding's values differ.
- An agent in a sandbox calls the API as the application does, with the
  project's credential and without seeing it, even where the API repeats
  it: the answer reaches the pod masked. Answers of a binding with a
  credential come unencoded from the upstream, more bytes on its leg,
  and without ranges; an upstream that encodes them otherwise than gzip
  is refused.
- Plaintext passes the pod's link and `feos-authn`, as with ADR 0014's
  TLS bindings. In the intercept mode the pod trusts a CA of its own,
  constrained to the names it intercepts.
- Offloading protects the bytes, not the authority: a pod that has been
  taken over can send requests with the credential while it runs. The
  path prefixes, the limits, the records and the incidents are the
  boundary.
- A binding's connections are the node's, not the pod's: the pod's
  egress rules and traffic records do not cover them; the destination
  rule and the binding's records do. The jail's seccomp allowlist,
  dropped capabilities and address checks guard the node's network from
  a compromised `feos-authn` until a network namespace of its own does.
- A sandbox's container exists before its bindings, so a process in it
  trusts the pod's CA only from its next start; an environment's pod
  trusts it from its first.

## History

Decided and built on 2026-11-05 (836aca6): the control plane's part,
`feosd`'s listeners and resolver answers, `feos-authn`'s HTTP side and
the CAs in every container; the redaction of the credential in answers
came the same day (f158459). ADR 0033 added the tenant's credentials as
the third source on 2026-11-06. An answer that came gzip although
identity was asked for was refused until 2026-11-08, when it came to be
unpacked and redacted instead (caa883d). The text at first leaned on the
pod's security groups, which ADR 0034 replaced with reachability from
declarations on 2026-11-08 (a061348), declaring a binding's upstream
egress the binding declares; the pod's second address, from which ADR
0014 wants a binding's connections to leave, is not built, and the proxy
dials from the node.
