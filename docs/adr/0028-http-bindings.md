# ADR 0028: HTTP bindings at the node's address — a pod calls an API by a name, the host adds the credential, and the API keeps its own name where it must

**Status:** Proposed · **Date:** 2026-11-05

**Touches:** ADR 0014 (amended: HTTP APIs are bindings of `feos-authn`,
which answers its open question on tokens for HTTP APIs; an HTTP binding
is reached at the node's address and by a name, not on a loopback port
of the guest), ADR 0010 (the resolver answers the names of a pod's
HTTP bindings), ADR 0012 and 0013 (a binding's connections leave from
the pod's address and are the pod's), ADR 0018 (an environment gains
the object `binding`, a sandbox its HTTP bindings), ADR 0026 (the node
address `169.254.169.1`, and the pods' requests through their bindings
as records in the sink), ADR 0007 and ADR 0004 (for the intercept
mode, the guest puts the pod's CA into every container: a file, a
bundle mounted over the image's own or, in a container older than the
CA, one in the pod's shared directory, and the environment of every
process it starts there).

## Context

Code in pods calls HTTP APIs with credentials: an ERP system's OData
API with an API key, a payment provider with a bearer token, a model
gateway, a knowledge graph service behind OAuth 2.0 client credentials,
a partner's API that wants a client certificate. Today the only way is
a secret of the environment (ADR 0018) in an environment variable: the
credential is in the pod, and every credential in a pod counts as
compromised (ADR 0014). An agent's sandbox gets none at all, so the
agent either cannot try what it builds against the API, or a person
pastes the key into the conversation, a file or the repository.

ADR 0014 decided the pattern: the material stays on the host, the pod
gets its use, the host knows the pod by the socket a connection arrives
on. It built that for identities, TLS, Kubernetes and SSH, and left
"JWTs for HTTP APIs, a pod as an OAuth client" to a later ADR. This is
that ADR.

ADR 0014's surface for a binding is a loopback port in the guest, piped
to the host over vsock. For HTTP APIs that surface costs:

- a port per binding in every pod, beside the ports the application
  binds itself (a dev server on 8080, a debugger on 9229), and a port
  allocation that has to stay the same in a sandbox and in an
  environment, or the code differs between them;
- a port number in every URL the code holds (`http://127.0.0.1:6447`),
  which means nothing to a reader and nothing to an agent;
- a piper per port in the guest.

The node already has addresses that every pod reaches and that mean
something: the resolver at `169.254.0.53` (ADR 0010), which answers
names per pod from its name tables, and `169.254.169.1`, where pods
send OTLP (ADR 0026). A binding can be a **name** that the resolver
answers with a node address. Malte's direction of 2026-11-05: the proxy
is addressed at the node's link-local address, not on ports of the
microVM.

Some clients cannot use another name than the API's own:

- the API puts absolute URLs into its answers — paging links (an OData
  service's `@odata.nextLink`, `__metadata.uri`), `Location`, links in
  HAL or JSON:API — and the client follows them to the API's real host,
  without the credential;
- an SDK has the base URL built in, or refuses plain HTTP;
- a request signature covers the host.

For those the platform must sit inside TLS, under the API's own name:
a man in the middle with a certificate authority the pod trusts. ADR
0014 set aside a TLS-intercepting proxy "for any destination the pod
chooses", because the platform would sit in the plaintext of every
connection. Malte asked on 2026-11-05 for interception as a mode of its
own, so that the original domain names work where the name mode runs
into trouble. It is decided here for the names a pod's bindings name,
and only for those.

## Decision

### 1. An HTTP binding

A pod's HTTP binding has:

- a **name**: one DNS label, as a service's name (ADR 0018);
- an **upstream**: an origin, `https://host[:port]`. Plain `http` is
  refused: the credential would cross the internet in the clear;
- the upstream's **CA**, or none for the WebPKI;
- a **credential** (§4);
- a **mode**: `name` (the default, §3) or `intercept` (§5);
- **options**: the path prefixes it may reach (none: all), a rate limit
  (§7), and `forwardedHost` (§3).

HTTP bindings are a set beside ADR 0014's Kubernetes, TLS and SSH
bindings (`PodBindings` gains `http`): stated whole, sealed to the run's
`feos-authn`, opened only there, held in its memory only. Nothing of
ADR 0014's sealing, jail, audit file or start order changes.

### 2. Reached at the node's address, never on a port of the guest

- A pod reaches its HTTP bindings at `169.254.169.1`, the node address
  of ADR 0026: port 80 in the name mode, 443 in the intercept mode.
  Nothing listens in the guest, and the guest has no piper for them.
- `feosd` holds the two listening sockets, which outlive its restarts
  as the resolver's do (ADR 0010). It accepts a connection, knows the
  pod by its source address, which the pod's TAP pins, and passes the
  accepted socket to that pod's `feos-authn` over its control socket
  (`SCM_RIGHTS`), without reading a byte. A connection from a pod
  without HTTP bindings, or whose process is not running, is reset.
  Hostile HTTP is parsed only in the jailed process of one pod, as ADR
  0014 wants.
- `feosd`'s resolver answers a binding's name to the pods that hold the
  binding: in the name mode the binding's label (`erp`), in the
  intercept mode the upstream's host name (`api.erp.example`). It
  answers with `169.254.169.1` (A) and an empty answer for AAAA, for
  every other type nothing, with a short TTL. Every other pod resolves
  the name as before. A binding's label shares the space of the name
  tables' labels: the control plane refuses a binding named as a
  service or addon the pod's tables hold.
- It works the same for every pod: sandboxes, attached services, image
  services and jobs (ADR 0018).

### 3. The name mode, the default

- The pod speaks plain HTTP/1.1 to `http://<name>/<path>`. The link is
  the pod's own TAP to its node; nothing else is on it.
- `feos-authn` resolves the upstream, checks every address as for
  Kubernetes (`--deny-prefix`, the node's networks), dials the address
  it checked from the pod's address (ADR 0014: the pod's security
  groups and traffic records cover it), and speaks TLS 1.2 or 1.3 with
  ALPN `h2` and `http/1.1`. Upstream connections are pooled per binding.
- On the request it drops `Authorization`, `Proxy-*` and every header
  the credential sets, sets `Host` to the upstream, adds the credential
  and passes the W3C trace context on. A request never leaves without
  the credential.
- A path outside the binding's prefixes is refused locally. Upgrades
  (WebSocket) and streamed bodies pass through as pipes; nothing is
  buffered whole.
- In the answer, a `Location` or `Content-Location` on the upstream's
  origin is rewritten to `http://<name>`. **Bodies are never
  rewritten** to the name: that would mean parsing every content type,
  compressed and streamed. With `forwardedHost`, the request carries
  `X-Forwarded-Host: <name>` and `X-Forwarded-Proto: http`, so an
  upstream that builds its absolute URLs from them builds the
  binding's. An API that does not is a case for the intercept mode.
- **Nothing of the credential goes back to the pod.** Upstreams repeat
  what they were sent: an API key in a quota error's body, a token in
  `WWW-Authenticate`, a key in a `Location`'s query, a debug echo.
  `feos-authn` replaces every value of the binding's credential of at
  least 8 bytes — a header's value, Basic's pair, its base64 and the
  password, the OAuth client secret in the forms it is sent in, the
  current and the previous token, the lines of a client key — wherever
  it appears in the answer: its reason, header values, body and
  trailers, and the upstream's WebSocket frames. The mask has the
  value's length (`redacted-by-FeOS-binding`, cut or filled with `*`),
  never the value or a hash of it, so that `Content-Length` and frame
  lengths stay true. Bodies are searched as they stream; only the
  bytes that could begin a value wait for the next chunk, so a value
  split across chunks is found and nothing is buffered whole. So that
  every body can be searched, such a request asks for its answer
  unencoded and whole (`Accept-Encoding: identity`, no `Range`, no
  WebSocket extensions); an answer that comes encoded or partial anyway
  is refused (502). Shorter values are left alone: they would mask
  ordinary text. A value the upstream transforms (escapes, encodes
  otherwise, splits itself) is not recognised.
- `feos-authn`'s own refusals carry `FeOS-Binding-Error` and a small
  JSON body with the reason: 403 for a denied destination or path, 502
  for an unreachable upstream or a failed TLS check, 503 when the
  credential cannot be had (a token endpoint that fails), 429 for the
  binding's limit. A client and an agent tell them from the upstream's
  own answers by that header.

### 4. Credentials

- **Headers**: fixed header names and values — an API key, a bearer
  token.
- **Basic**: user and password.
- **OAuth 2.0 client credentials**: token URL, client ID and secret,
  scope or audience, and the client's authentication (Basic or in the
  body). `feos-authn` fetches a token at first use, keeps it in memory,
  renews it at three quarters of its lifetime, and on a 401 from the
  upstream renews once and repeats an idempotent request once. The
  token endpoint is dialed under the same address checks.
- **Client certificate**: a chain and key for mTLS to the upstream. Not
  the pod's identity, which is a TLS binding with FeID (ADR 0014).

Not now: a person's identity on the request (token exchange,
on-behalf-of) and request signing schemes. Both are later ADRs.

### 5. The intercept mode: the API's own name, a CA per pod

- The binding's name is the upstream's host name. The resolver answers
  it with `169.254.169.1` for the pods that hold the binding; the
  client connects with TLS to port 443 and that name as its SNI.
  `feos-authn` answers with a certificate for exactly that name, issued
  by the pod's interception CA, and goes on as in the name mode: the
  same checks, the same credential, the same records.
- **Only the names of the pod's intercept bindings** reach the proxy.
  Every other name resolves as ever, and the platform sits in no other
  connection's plaintext. A connection whose SNI names no intercept
  binding of the pod is refused before a certificate is sent. This is
  the difference from the alternative ADR 0014 set aside, which would
  have intercepted every destination.
- Upstreams on port 443 only, for now.
- **The CA** is one per pod, ECDSA P-256, so that clients without
  Ed25519 certificates (Java, .NET, older OpenSSL) accept it. The
  control plane makes it at the pod's create, keeps it encrypted with
  the pod's bindings, and seals it to every run's `feos-authn` with
  them. A resumed or restored pod keeps the CA its processes trust;
  ADR 0014's rule that a key never leaves its process cannot hold for a
  key that must outlive the run, and this key is trusted by one pod
  only.
- The CA carries **name constraints**: permitted DNS names, exactly the
  intercepted names at its issue. A pod that comes to intercept a name
  its newest CA does not permit gets a new CA beside the old one in its
  trust files, and `feos-authn` issues from the newest; one whose names
  shrink keeps its CA. A process that read its trust
  store before the change reaches the new name after its restart.
- Leaf certificates live 24 hours and are made in memory at first use.
- **The CA in every container, not only in the microVM.** Only pods
  with an intercept binding get it. A container has a root filesystem
  and an environment of its own, so a CA in the guest's own store
  reaches none of them. Before a container starts, the guest:
  - writes the pod's CA certificates to a file of the pod's and mounts
    it read-only into the container at a fixed path;
  - builds a bundle for that container from that container's image: the
    image's own system bundle (the first of Debian's and Alpine's
    `/etc/ssl/certs/ca-certificates.crt`, Red Hat's
    `/etc/pki/tls/certs/ca-bundle.crt` and `/etc/ssl/cert.pem` that it
    has) and the pod's CAs, and mounts it read-only over every one of
    those paths the image has. An image with none of them (distroless,
    `scratch`) gets the bundle at Debian's path, which Go and OpenSSL
    read;
  - adds to the container's environment, where its spec does not set
    them: `SSL_CERT_FILE` (OpenSSL, Go, Ruby, curl) and
    `REQUESTS_CA_BUNDLE` (Python's requests) naming the bundle,
    `NODE_EXTRA_CA_CERTS` (Node, Bun) naming the CA file.

  This holds for every container of the pod — init, sidecar and main —
  and the same environment goes to every process the guest starts in a
  container later: `Exec`, a sandbox's tool calls and the supervised
  processes of attached services (ADR 0018). The agent's `curl` and
  `npm` trust what the application trusts.
- **A container older than the pod's first CA** — a sandbox's, which
  runs before FeCode gives it bindings — has nothing mounted, and its
  spec names no bundle. Every process the guest starts in it later gets
  a bundle of the container's own in the pod's shared directory
  instead, made at that start of the system bundle the container has
  then (one its package manager installed since included) and the
  pod's CAs, with `SSL_CERT_FILE` and `REQUESTS_CA_BUNDLE` naming it
  and `NODE_EXTRA_CA_CERTS` the CA file, where the process's
  environment names none. Nothing covers the container's own bundle, so
  `update-ca-certificates` still replaces it. A process that was running
  before gets the CAs at its restart; the container needs none.
- When the pod's CAs change (above), the guest rewrites each container's
  CA file and mounted bundle in place, never by a rename, so that the
  mounts see the new content, and replaces the bundles in the shared
  directory by a rename, so that a reader sees a whole one; a process
  that read its store at start sees the new CA after its restart.
  Java's keystore (`cacerts`) is not covered (open question).
- A client that pins its server's certificate fails here and uses the
  name mode. A client that resolves elsewhere (DNS over HTTPS, a fixed
  address) reaches the real upstream without the credential and fails
  closed.

### 6. Every request is recorded, and the control plane watches them

- ADR 0014's audit, written before the answer, gains a record per
  request: binding, method, host, path (no query string), status,
  bytes, duration, the upstream's address, the credential's kind (never
  its value), how many places of the credential were replaced in the
  answer (§3), and refusals with their reason. Bodies are never
  recorded.
- `feosd` publishes the records as the pod's events (ADR 0014) and ships
  them to the sink (ADR 0026) as the pod's egress records, under its
  labels, each a client span with the request's trace context: the
  ingress's access log (ADR 0021) in the other direction.
- **The records live in the sink, and only there.** `ferund` keeps none
  of a pod's requests in its activity log: a busy pod would fill
  Postgres with a row per request for the activity's retention. Of the
  events it keeps what the incidents below need, bounded: the requests
  counted per pod, binding and minute of the database's clock, and the
  newest few records of each pod's binding as evidence, both deleted
  after minutes.
- `ferund` opens an incident (ADR 0018) for a binding of an environment
  whose answers are mostly 401 or 403 for five minutes ("credential
  refused"), mostly 429 or 5xx ("upstream failing"), or whose token
  endpoint fails, with the records as its evidence. The thresholds are
  the architecture's.

### 7. Limits

- A binding may carry a rate limit per pod (requests per second and a
  burst). Over it, `feos-authn` answers 429 itself. It protects an
  upstream from an agent in a loop and a customer's system from a test.
- Connections per binding and pod are bounded; the bound is the
  architecture's.

### 8. Where bindings come from

- **Sandboxes**: the sandbox API takes a sandbox's HTTP bindings as a
  set, as it takes its other bindings, sealed by the control plane.
- **Environments**: a new object, `binding` (name, upstream, CA, mode,
  options), whose credential's fields reference the environment's
  secrets by name, so that values keep their versions and are never in
  the spec. Services and jobs name the bindings they get. A deployment's
  plan shows binding changes. A new version of a referenced secret is
  sealed to the running pods again without a restart; a change of name,
  upstream or mode is a rollout.
- **Preflight** (`POST /api/v1/http/preflight`), before anything is
  stored: resolve and check the upstream, connect, check TLS, fetch a
  token where the credential is OAuth, and, if asked, send one `GET` or
  `HEAD` to a given path with the credential. It answers the addresses
  (none for a denied destination: the preflight is no resolver of the
  platform's own networks), the server certificate's subject and issuer,
  the token's lifetime, the status and the latency, never a body.
- The pod's security groups apply as to its own connections. An
  environment's default egress table reaches the internet (ADR 0018),
  so a binding needs no rule of its own there.

### 9. Kubernetes and TLS bindings stay where they are

Kubernetes and TLS bindings keep ADR 0014's loopback ports for now.
Moving them to the node address and to names is an open question.

## Alternatives considered

- **Loopback ports in the guest**, ADR 0014's surface. A port per
  binding in every pod, collisions with the application's own ports,
  numbers in URLs, a piper in the guest.
- **A port per binding at the node address** (`169.254.169.1:<port>`).
  No ports in the guest, but numbers in URLs and configuration, and a
  registry of ports per pod. Names are what the resolver already gives.
- **Paths at one address** (`http://169.254.169.1/erp/…`). The API's
  absolute paths, and every client that joins a base URL and a path its
  own way, break.
- **A forward proxy through `HTTP_PROXY`.** Runtimes honour it
  unevenly (Node's `fetch` does not), and HTTPS through `CONNECT` is
  opaque without interception.
- **Intercepting every destination**, a transparent egress proxy: ADR
  0014's rejected alternative, still rejected.
- **The intercept mode alone.** Every pod would need a CA in its trust
  files and every runtime its variables, and pinned clients would
  break. The name mode needs nothing in the pod.
- **Rewriting bodies in the name mode.** Every content type,
  compression and stream would need a parser in the proxy. The
  intercept mode solves the problem at its root.
- **Decompressing answers to redact them**, instead of asking for them
  unencoded: a decompressor for every coding, and its bombs, in the
  jailed process, and the answer's length lost. Servers send identity
  when asked for it.
- **A fixed marker in place of a value**, of its own length: every
  `Content-Length` and WebSocket frame around it would have to be
  rewritten too.
- **A CA made by each run's `feos-authn`**, whose key never leaves it.
  A resumed pod's processes would trust a CA whose key is gone.
- **One CA per tenant or environment.** One key trusted by many pods;
  one per pod keeps a leak to that pod.
- **`feosd` as the proxy.** Hostile HTTP in the privileged process that
  owns every microVM (ADR 0014).
- **DNAT per pod to its `feos-authn`.** Works without passing sockets,
  but needs a port per process in their shared namespace and NAT state
  per connection; passing the accepted socket changes no table.
- **Credentials in environment variables**, as today: every credential
  in the pod.

## Consequences

- An application calls `http://erp/…` and configures nothing else; it
  holds no key and has no auth code of its own, in any language. The
  same code runs in a sandbox, a preview and production; only the
  binding's values differ.
- An agent in a sandbox calls the API as the application does, with
  the project's credential and without seeing it, even where the API
  repeats it: the answer reaches the pod masked. Answers of a binding
  with a credential come unencoded from the upstream, more bytes on
  its leg, and without ranges; an upstream that encodes them regardless
  is refused.
- Plaintext passes the pod's link and `feos-authn`, as with ADR 0014's
  TLS bindings. In the intercept mode the pod trusts a CA of its own,
  constrained to the names it intercepts.
- Offloading protects the bytes, not the authority: a pod that has been
  taken over can send requests with the credential while it runs. The
  path prefixes, the limits, the records and the incidents are the
  boundary.
- `feosd` gains two listening sockets, the passing of accepted sockets
  and per-pod answers for bindings' names; the guest gains, for the
  intercept mode, a CA file and a bundle per container mounted over the
  image's own or, for a container older than the CA, kept in the shared
  directory, and the variables in every container and every process it
  starts in one; `feos-authn` gains an HTTP proxy,
  an OAuth client and the issuing of leaf certificates; `ferund` gains
  the object `binding`, sealing of HTTP bindings and of the CA, the
  preflight and the incidents; the sink gains egress records.
- `feos-authn` is not built yet (ADR 0014). Its HTTP side becomes its
  first part; the identity, FeID and TLS bindings follow.

## Open questions

- Upstreams in a private network, a company's systems behind its
  firewall: a connector the company runs that dials out, and a binding
  that names it.
- A person's identity on a request (token exchange, on-behalf-of).
- The intercept mode on ports other than 443; HTTP/2 without TLS (h2c)
  in the name mode, for gRPC clients.
- Java's keystore, and other runtimes with stores of their own.
- Upstreams that encode answers whatever they are asked (objects stored
  with `Content-Encoding: gzip`): decompressing gzip in `feos-authn`
  after all, or a binding option that trusts such an upstream.
- A bundle mounted over a system bundle's path cannot be replaced: a
  rename onto a mount fails (`EBUSY`), so `update-ca-certificates`, and
  a package manager that installs or updates `ca-certificates`, fails
  in a container created with CAs. Images build their store before
  they run; whether such a container should get the bundle in the
  shared directory alone, as a container older than the CA does.
- Moving Kubernetes and TLS bindings to the node address and to names.
- Quotas per tenant across pods.
