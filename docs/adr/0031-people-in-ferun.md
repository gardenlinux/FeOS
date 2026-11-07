# ADR 0031: People in FeRun — tenants with members, a grant on a tenant or on one of its environments, the UI as the tenants' console, a general MCP door for a person's agents beside the sandbox's door that stays FeCode's, quotas, and a zone for the tenants people make

**Status:** Proposed · **Date:** 2026-11-06

**Touches:** ADR 0001 (CaaS gets a door for its customers themselves),
ADR 0015 (the front doors: sign-in for everyone, a general MCP door),
ADR 0018 (one API for every caller, now with a person's grants; the MCP
tools it deferred), ADR 0021 (a second zone for default hosts; the
tenant's zone it foresaw), ADR 0026 (the sink asks which environments a
caller may read), ADR 0032 (the builder that writes an environment's
deployments). FeCode's ADR 0020 (the shared provider signs everyone in
and serves MCP clients).

## Context

ADR 0001 offers two products, agent sandboxes and a container as a
service, whose customers "run their own container images as services
without operating a cluster". ADR 0018 names three callers of CaaS: an
agent in a FeCode sandbox, a person, and "a CaaS customer without
FeCode, from a CLI or a CI". Only the first exists. FeRun has no person
in it:

- A **tenant** is a name and bearer tokens (`tenants`, `tenant_tokens`).
  Only an operator makes one (`POST /api/v1/admin/tenants`). A token can
  do everything its tenant can; there are no roles and no quotas.
- **Sign-in** is for operators. A person whose `groups` claim lacks
  `--oidc-admin-group` gets "no access" and no session, although
  `ui_sessions` already has an `admin` column. The UI's tenant pages work
  on a tenant token that an operator mints or pastes, which the browser
  keeps in its storage. Environments are read-only in the UI.
- **MCP** (`/mcp`) takes only a sandbox's token: one sandbox per token,
  the twelve tools, nothing about sandboxes or environments. ADR 0018
  deferred "the MCP tools and their grants".
- The products on FeRun keep their people apart themselves. FeCode is
  one tenant, with its projects as labels and its own members and roles;
  FeChat holds one tenant's token and signs its references to
  conversations' sandboxes. FeCode's ADR 0012 rejected an authorization
  model in FeRun for the registry, because FeRun "does not otherwise have"
  one.

FeRun is now to be a product of its own: people sign in, make tenants or
are added to them, and work there with the roles admin, write or read,
on the whole tenant or on some of its environments. The UI changes
things, domains included; deployments are assembled in a builder (ADR
0032). A person's agents reach the tenant's sandboxes and environments
over MCP once the person has signed in. Git and a registry stay out:
images come from any registry with the create's credentials (ADR 0022,
ADR 0025).

Two things outside FeRun shape this:

- **The MCP authorization spec** (2025-11-25) makes an MCP server an
  OAuth 2.1 resource server: it publishes protected resource metadata
  (RFC 9728) that names its authorization server, answers 401 with
  `WWW-Authenticate: Bearer resource_metadata=…`, and accepts only access
  tokens issued for it, by audience. Clients send the RFC 8707 `resource`
  parameter and register by a client ID metadata document or by dynamic
  registration (RFC 7591). The provider FeCode, FeRun and FeChat share is
  Ory Hydra behind a login app of the platform's own. Hydra has dynamic
  registration but neither resource indicators nor client ID metadata
  documents, so the token's audience stays empty unless the login app
  grants it at consent. FeCode's ADR 0020 moves company sign-in and
  password accounts into that provider and gives its login app that
  part.
- **Default hosts** (ADR 0021) live under one apps zone beside the
  platform's own hosts. A label with `--` is a tenant's choice, first
  come; plain labels are kept for the tenants that own the zone. Once
  anyone can make a tenant, anyone can take a label that reads as an
  operator tenant's product. ADR 0021 foresaw that "a tenant may be given
  a zone of its own instead".

## Decision

### A person is a principal of their own

- A **person** is an issuer and a subject of the shared provider
  (`--oidc-issuer`), a row of `people` with an ID of ferund's, the e-mail
  address and the name of the last sign-in, when they were first and last
  seen, and `disabled_at`. E-mail and name are for display: nothing in
  FeRun matches a person by an address. An invitation is a link (below).
- Everyone the provider lets in may sign in, and gets a session. Whom the
  provider lets in, and to which products, is the provider's: its
  consent refuses FeRun's clients to a person who does not have FeRun
  (FeCode's ADR 0020). The operators are those of
  `--oidc-admin-group` as before; their session is marked `admin`.
- A session keeps the provider's `sid` (the ID token's claim), and
  `ferund`'s client has a back-channel logout URI,
  `/auth/backchannel-logout`. A logout token, signed with the provider's
  keys and for `ferund`'s client ID, ends every session with its `sid`
  on every instance. The provider sends one when the person signs out of
  any of its apps or at their company (FeCode's ADR 0020, §12). A
  sign-out ends sessions only: tokens of MCP clients live on.
- An **operator** keeps the operator's pages and, in every tenant, the
  rights of its admin, as an operator has them today through the tokens
  it can mint. What an operator changes in a tenant is recorded as the
  operator's, so a tenant can see it.
- A person disabled in FeRun (`disabled_at`, by an operator) loses every
  session and every grant's effect at once; the grants stay, so an
  undo restores them.

### A tenant has members: a grant names a principal, a scope and a role

- A **grant** gives a principal a role in a scope:
  - the principal is a person or a tenant token;
  - the scope is the tenant, or one environment of it;
  - the role is `read`, `write` or `admin`, each including the one
    before.

  A principal holds at most one grant per scope. Its role on an
  environment is the higher of its tenant grant and its grant on that
  environment; its role on the tenant is its tenant grant alone. A grant
  on an environment ends with the environment.
- What a role may do on an **environment**:
  - `read`: its objects, deployments and plans, tags, incidents,
    traffic, logs and traces, the names and versions of its secrets
    (never a value, as today), its hosts and bound domains;
  - `write`: also plan and apply, revert, tags, secrets' values,
    restarts, runs of its jobs;
  - `admin`: also its route tokens, `public` and its host label, binding
    and unbinding domains, its deletion, and the grants on it.
- What only a **tenant grant** gives, because it reaches beyond one
  environment:
  - `read`: the list of environments, sandboxes, jobs and domains;
  - `write`: making, cloning and deleting environments; sandboxes, their
    tokens and their tools; tenants' jobs; services with the runtime
    `attached` (ADR 0035 moves these three to a grant on the sandbox's or
    job's environment, since each then belongs to one). A sandbox is the tenant's: an attachable one reaches the
    addons and image services of the environments whose services it
    runs (ADR 0018), and an attached service runs in a sandbox, so a
    grant on one environment must not give either. A write is judged by
    what it changes against the current deployment: a deployment whose
    attached services are unchanged passes for a principal that writes
    the environment alone (ADR 0032);
  - `admin`: members and invitations, tokens, claiming and deleting
    domains (a claim is the tenant's, ADR 0021), the tenant's deletion.
- A **tenant token** is a principal like a person: it is minted with a
  role and a scope, never above its minter's. The tokens there are
  become `admin` on their tenant, so FeCode and FeChat go on unchanged.
  Sandbox tokens stay what they are, a sandbox's own.
- **Answers**: a principal that may not see an object gets the answer of
  an absent one, 404, as today; one that sees it but may not do what it
  asks gets 403 with the role it lacks (`{"code": "role_required",
  "role": "write"}`).
- A tenant always has an admin: the last admin grant cannot be removed
  or lowered.
- Where a route touches both scopes, the implementation reads the rules
  above so:
  - deleting an environment needs `admin` on it **or** `write` on the
    tenant: either suffices;
  - the list of environments answers every member, filtered for a
    principal without a tenant grant to the environments it holds a
    grant on;
  - a plan without an environment (`POST /api/v1/environments/plan`,
    which reads nothing of the tenant's) needs `read` on the tenant;
  - the preflights (`POST /api/v1/{kube,ssh,http}/preflight`), which
    check what a sandbox would get, need `write` on the tenant, as the
    sandboxes do.

### People make tenants and invite each other

- A signed-in person makes a tenant, named by today's rules, and holds
  `admin` on it. Names the platform needs stay reserved (`operator`, the
  zone owners', `admin`, `api`, `www` and the platform's host labels):
  422 `reserved_name` for `operator`, `admin`, `api`, `www`, the apps
  zone's owners (`--apps-zone-owners`) and the first label of the public
  URL's host, of `--edge-name`, of `--apps-zone` and of `--tenant-zone`.
  An operator who makes a tenant with `POST /api/v1/tenants` (with a
  session) gets a person's tenant: its maker, the `admin` grant, the
  default limits and the tenant zone, but no bound of
  `--tenants-per-person`; `POST /api/v1/admin/tenants` stays the
  operators' tenant of before, in the apps zone, with its token.
  `--tenant-creation` says who may make tenants, `people` or
  `operators`; `--tenants-per-person` bounds how many tenants one person
  may hold as their maker (3).
- An **invitation** is a single-use link with a role and a scope, made by
  an admin of that scope, valid for 7 days, its secret stored as a hash.
  The person who opens it while signed in gets the grant; nobody else can
  use it after. FeRun sends no mail: the admin passes the link on.
- Members of a tenant see each other's name and e-mail address in that
  tenant, and nowhere else.

### The API takes a person as it takes a token

- The tenant class accepts a person's session beside a token. The
  session's cookie is `__Host-ferun_session` (Secure, `Path=/`, no
  `Domain`): the tenant zone lies below `ferund`'s host
  (`apps.run.<domain>` beside `run.<domain>`), and the prefix keeps an
  app there from setting a cookie the browser sends to `ferund`. The
  name before, `ferun_session`, is read and passed on for one release,
  so that sessions survive the rollout and a sink or `ferund` of before
  finds the session; the release after drops it. A session
  names its tenant in the header `FeRun-Tenant`; a token's tenant is its
  own, and a header that names another is answered as an unknown tenant.
  A session's changes need `X-Requested-With: ferun` and, with an
  `Origin`, the public URL's, as an operator's do today.
- Every route states the role and scope it needs at the route, as the
  auth classes are stated today; a list answers with what the principal
  may read.
- `ferund` records who made every change itself: deployments, secrets,
  tags, domains, grants, invitations, tokens, sandboxes and jobs carry
  the principal (person or token) beside the free `author` a client
  sends, which stays the client's (FeCode's `fecode:<user>`).
- `GET /api/v1/tenant`, which the sink asks (ADR 0026), answers for a
  session too: the tenant, the principal's role on it and the
  environments it may read. The sink keeps a stream whose `environment`
  label names one of those, and a stream without an environment
  (sandboxes, tenants' jobs) for a tenant `read`. Where the sink is
  served under `ferund`'s host, the session's cookie reaches it, and it
  passes the cookie and the header on. `ferund`'s own reads of the sink
  for a person (access log, traffic, an object's log) pass the session's
  cookie and header on the same way: no service credential reads the
  sink for a tenant, and the sink trusts `ferund`'s answer alone. A
  principal of environments only is answered only a sink that says it
  filters (`?filter=environments`), since a sink of before reads any
  answer as the whole tenant's. Deleting streams
  (`DELETE /api/v1/log-streams`) needs `write` on the tenant, also for a
  stream of one environment: the sink's deletion is the tenant's, not an
  environment's, and a principal of environments only deletes nothing
  there.

### The UI is the tenants' console

- After sign-in a person sees their tenants, and in a tenant what their
  roles allow:
  - environments, with the builder of ADR 0032: deployments, plan,
    apply, revert, tags, secrets, public and the host label, domains
    bound;
  - domains: claim, the records to set (TXT and the route to the edge),
    their state and the last check, binding and moving, the
    certificate;
  - sandboxes: create, pause, resume, delete, their tokens, and
    "connect an agent", which mints a sandbox token and shows the
    address of `/mcp`;
  - tenants' jobs: create, cancel, their log's tail;
  - logs and traces, as today;
  - members, invitations, tokens, and the quota's use.
- The operators' pages (nodes, join tokens, instances, every tenant,
  activity) are for operators only.
- The browser keeps no tenant token: the session acts. What a role cannot
  do is not offered; the API decides.

### A sandbox's MCP door stays FeCode's, and a general door comes for people

- **A sandbox's door is `/mcp` as it is**: the twelve tools of the
  baseline on a sandbox's token, one sandbox per token, with keyed calls
  (ADR 0023). It is the door of the programs that run agents in
  sandboxes, FeCode first, FeChat beside it. Its path, its token, its
  tools' names and schemas, `_meta["ferun.callId"]` and its answers stay
  as they are, so FeCode changes nothing. What it gains later is added
  beside what is there, never in its place.
- A person reaches a sandbox's door with a sandbox's token, which a
  principal with `write` on the tenant mints in the UI or over REST
  ("connect an agent").
- **The general door, `/mcp/tenants`**, comes later (Steps; Malte,
  2026-11-06). It lets an agent do what its person may do in FeRun:
  what the UI offers, as tools. Its tools take the tenant (and the
  environment) as arguments, so one server reaches every tenant the
  person is a member of.
- The two are apart because they answer different questions. The
  general door is the console: it makes and changes tenants,
  environments and sandboxes. A sandbox's door runs commands in one
  sandbox. Keeping the twelve tools out of the general door keeps the
  sandbox door's contract FeCode's alone, and keeps a person's console
  from being a shell into every sandbox.

#### The general door is an OAuth 2.1 resource server

- It publishes its protected resource metadata (RFC 9728) at
  `/.well-known/oauth-protected-resource/mcp/tenants`, with the shared
  provider as its one authorization server. It answers a request without
  a valid token 401 with `WWW-Authenticate: Bearer resource_metadata="…"`,
  and a call the principal's role does not allow 403 with
  `error="insufficient_scope"`. It uses the MCP go-sdk's `auth` package
  for both, which the fixed dependencies include.
- It accepts an access token of the provider that meets these checks:
  - it is a JWT signed with the provider's keys, verified with the
    standard library as the ID token is today;
  - it is unexpired and issued by `--oidc-issuer`;
  - its `aud` holds the door's canonical URL, or FeRun's MCP base
    (`<public URL>/mcp/`). The provider grants the base to a client that
    names no resource (FeCode's ADR 0020).

  The `sub` names the person.
- It accepts no other token of the provider. Hydra binds no audience by
  itself; the provider's login app grants the `resource` a client asks
  for only under FeRun's MCP prefix. Taking any of the provider's tokens
  instead would let a token issued to another client or for another
  service act in a tenant, which the spec forbids.
- It also takes a tenant token, whose tools reach its own tenant, for
  agents in a CI.
- The principal's grants decide every call and are read on every
  request, so a removed grant or a disabled person ends access at once,
  whatever the token's lifetime.
- A session ID is signed over the principal, not over the token, so a refreshed token keeps its session. Origins are refused as
  at `/mcp` unless `--mcp-allowed-origins` names them.

#### The general door's tools are the API's operations

- There is one tool per operation of the tenant class that a person can
  do in the UI:
  - tenants: list, create, delete;
  - members: list, grants, invitations, whose link the tool returns;
  - environments: list, get with its current deployment, create, clone,
    delete, plan, apply with a `base`, revert, tags, public and the host
    label, restart a service, run a job;
  - their state: status, incidents, traffic, logs, traces;
  - domains: claim (the answer names the records to set), check, bind,
    move, delete;
  - sandboxes: list, create, pause, resume, delete;
  - tenants' jobs: create, get, the log's tail, cancel;
  - the quota's use.
- A tool calls the same code as its REST route, with the same role
  check. A test keeps the tools and the routes together: every route of
  the tenant class has its tool or is named as left out, with the
  reason.
- Three kinds of operation are left out, because their answer or their
  input is a secret that would pass through the agent's model:
  - tokens: minting a tenant's, a sandbox's or a route token;
  - secrets' values;
  - image credentials.

  Secrets appear by name, version and whether they are missing. A value
  is set in the UI or over REST, or generated by `ferund` with
  `secret_generate` (random bytes in a given form, the value never
  answered), which is how a database password needs no person at all.
  A sandbox of a private image is made in the UI or over REST.
- Tools carry MCP's annotations: read-only ones `readOnlyHint`, deletes,
  reverts and apply `destructiveHint`, so that clients ask their person
  first.

### Quotas per tenant

- A tenant has **limits** on what it asks for, checked when it asks: the
  number of sandboxes, of environments and of tenants' jobs running, and
  the sums of vCPUs and of memory over its sandboxes, its services'
  replicas, its addons' plans and its running jobs. A plan over a limit
  has a finding; a create is 422 `quota_exceeded` with the limit and the
  use.
- Tenants that people make get the defaults of flags; tenants that
  operators make have none unless an operator sets them; an operator
  changes any tenant's. The sink's daily quotas for logs and traces
  stay as they are.
- There is no billing. The UI shows the use against the limits.

### The tenants people make get their hosts under a zone of their own

- `--tenant-zone` is the zone of the default hosts of the tenants that
  people make; the tenants that operators make keep `--apps-zone`. For
  now the tenant zone is a subdomain of the platform's domain; later a
  registrable domain of its own.
- The zone is the tenant's, a column set when the tenant is made. Moving
  a tenant to another zone moves its environments' hosts as a changed
  label does (ADR 0021).
- The tenant zone has the apps zone's rules: the operator's wildcard
  certificate, cookies made host-only by the ingress, no claims of names
  under it, labels by ADR 0021, and no plain labels, since it has no
  owners.
- A label is a name in its zone: `host_labels` registers it per zone,
  so a label taken in the tenant zone is free in the apps zone. The
  migration that keys the labels by zone breaks the label claims of a
  `ferund` of before during the rolling deploy (its `ON CONFLICT (label)`
  matches no key any more): its first publishes and chosen labels fail
  until it is replaced. No index keeps both, so the window is accepted
  and documented (control-plane.md, "Labels per zone"). A move
  keeps each environment's label where it may (free or the tenant's in
  the new zone, and not a plain one where the tenant owns none), else
  gives it its natural label or its fallback there, and leaves the old
  ones the tenant's. The operator moves a tenant with `PUT
  /api/v1/admin/tenants/{tenant}/zone`, and gives ferun-ingress the
  zone's wildcard as files of its own (`--tenant-zone-tls-cert`,
  `--tenant-zone-tls-key`), served for every name it covers.
- A zone of its own keeps the labels of the platform's products out of
  reach of anyone who makes a tenant. A registrable domain of its own
  also keeps the platform's cookies, reputation and blocklist entries
  apart from what tenants publish; that is why the subdomain is a step.

## Alternatives considered

- **FeRun stays the operators'**, and a product in front of it carries
  people, as FeCode does. A CaaS customer without FeCode would need
  another product to sign in, and every such product would build tenants,
  roles and quotas again over one token.
- **FeCode as FeRun's front door**: CaaS customers as FeCode users with a
  project each. The container service would carry the coding product's
  model, git and registry included, which are out of scope here.
- **Grants per object** (a service, a secret) **or roles of the tenant's
  own**: more than is asked for. The environment is CaaS's unit and the
  tenant the unit of ownership; three roles on two scopes say what is
  needed and fit a table.
- **A tenant per person**, no members: sharing would mean sharing a
  token, and nobody would know who changed what.
- **An authorization engine** (OpenFGA, SpiceDB): another system to run
  and keep consistent with `ferund`'s rows, for three roles on two
  scopes.
- **`ferund` as an OAuth authorization server of its own** for MCP,
  issuing tokens after a sign-in at the provider: it could bind
  audiences and read client ID metadata documents itself, but it would be
  a second issuer of people's tokens, with its keys, refresh tokens and
  consent screens in the control plane, beside the login app that already
  asks for consent.
- **One door with everything**, the twelve tools with a `sandbox`
  argument beside the console's tools: an agent could work in a sandbox
  it has just made without its person adding a server, but the tools
  would lose the schemas FeCode's agents know, and a person's console
  would be a shell into every sandbox of every tenant.
- **A sandbox's door for people with the provider's tokens**
  (`/mcp/sandboxes/{id}`): a second contract for the same twelve tools
  beside FeCode's, for clients that a sandbox's token serves already. It
  can come beside `/mcp` if a client needs OAuth there.
- **A door per tenant** (`/mcp/tenants/{tenant}`): a token bound to one
  tenant by its audience, but a person in several tenants adds a server
  for each, and a tenant made over MCP is out of reach until they do.
  The grants bound what a token can do; the audience binds it to
  FeRun.
- **Labels of their own in the apps zone** (a prefix per tenant made by a
  person): still beside the platform's hosts and cookies, and the move
  to a registrable domain would change every label twice.

## Consequences

- `ferund` gets `people`, `grants` and `invitations`; tenant tokens get
  a role and a scope; tenants get their zone, their maker and their
  limits; `ui_sessions` holds people, not only operators. A migration
  makes every token there an admin of its tenant.
- Every route of the tenant class states its role, and its tests cover
  each role, a grant on another environment and a principal of another
  tenant.
- FeCode and FeChat change nothing: their tokens hold admin grants.
  FeCode's ADR 0012 still holds for the registry, and FeRun's grants
  reach none of FeCode's projects, which stay labels in FeCode's tenant.
- The UI drops the tenant tokens in browser storage and becomes the
  tenants' console; the builder is ADR 0032's.
- The sink resolves sessions and environment grants through
  `/api/v1/tenant`.
- `/mcp` stays FeCode's contract, unchanged. The general door, when it
  comes, has tools that follow the tenant class's routes, kept together
  by a test.
- An operator publishes the tenant zone's wildcard DNS and certificate,
  and chooses `--tenant-creation`, `--tenants-per-person` and the
  default limits.
- `docs/architecture/control-plane.md` changes with the code: "Human
  sign-in", the tenants, the routes' roles, "Tools and MCP".

## Steps

1. **People and grants**: everyone's sessions, `people`, tenants made by
   people with the tenant zone, grants and invitations, roles at the
   routes, tokens with a role and a scope, quotas, `/api/v1/tenant` for
   sessions and the sink's filter; in the UI the tenants, members,
   invitations and tokens.
2. **The console**: sandboxes, tenants' jobs, domains and environments
   that change, with the first step of ADR 0032.
3. **Later**: the general door, with the operations of steps 1 and 2
   and the provider's tokens, once its login app grants audiences
   (FeCode's ADR 0020); a CLI with a person's tokens; a terminal (it
   needs interactive exec, which the container API does not have, ADR
   0004); billing.

## Open questions

- How fast a person removed at the provider loses FeRun without a
  sign-out: a session lives its `--session-ttl` (FeCode's requirement
  A9 asks the same).
- Whether a tenant's admin may invite a person who has no account yet:
  FeRun would ask the provider for a registration ticket (FeCode's ADR
  0020), which is a decision about cost. Until then an invitation is for
  people who can sign in already.
- Egress of the tenants people make: the internet as today, or none
  until an operator allows it, against abuse.
- The defaults of the limits.
- A tenant's display name beside its name, which is part of its hosts.
- FeCode's projects as tenants of their own (FeCode's ADR 0008, question
  4), which would give FeRun one authorization model for both products.
