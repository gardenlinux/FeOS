# ADR 0031: People in FeRun: projects with members, grants, and the projects' console

**Status:** Accepted · **Date:** 2026-11-06

People sign in to FeRun through the shared OpenID provider and work in
projects as members: a grant gives a person or a project token the role
`read`, `write` or `admin` on a project or on one of its environments,
a signed-in person makes projects and invites others by single-use
links, and a project always keeps an admin. The API takes a person's
session as it takes a token, every route states the role and scope it
needs, ferund records which principal made every change, and the sink
filters what a reader may see by the same grants. The management UI is
the projects' console: it acts with the session alone and offers what
the role allows. A project has limits on what it asks for, and the
projects people make get their default hosts under a zone of their own.
The sandbox's MCP door stays the sandbox token's and FeCode's contract;
a door for a person's agents, if it comes, is added beside it.

## Context

ADR 0001 offers two products, agent sandboxes and a container as a
service whose customers "run their own container images as services
without operating a cluster". ADR 0018 names three callers of CaaS: an
agent in a FeCode sandbox, a person, and a CaaS customer without FeCode
from a CLI or a CI. Without people in FeRun only the first exists:

- A project is a name and bearer tokens. Only an operator makes one, a
  token can do everything its project can, and there are no roles and no
  quotas.
- Sign-in is for operators. A person outside the operators' group gets
  "no access" and no session. The UI's project pages would have to work
  on a project token that an operator mints or pastes and the browser
  keeps in its storage.
- MCP takes only a sandbox's token: one sandbox per token, the tool
  baseline, nothing about sandboxes or environments. ADR 0018 deferred
  the MCP tools and their grants.
- The products on FeRun keep their people apart themselves. FeCode is
  one FeRun project, with its own projects as labels and its own members and
  roles; FeChat holds one project's token. FeCode's ADR 0012 rejected an
  authorization model in FeRun for the registry, because FeRun had none.

FeRun is to be a product of its own: people sign in, make projects or
are added to them, and work there with the roles admin, write or read,
on the whole project or on some of its environments. The UI changes
things, domains included; deployments are assembled in a builder (ADR
0032). Git and a registry stay out: images come from any registry with
the create's credentials (ADR 0022).

Two things outside FeRun shape the decision:

- The MCP authorization spec (2025-11-25) makes an MCP server an OAuth
  2.1 resource server that accepts only access tokens issued for it, by
  audience (RFC 8707 `resource`, RFC 9728 metadata). The provider
  FeCode, FeRun and FeChat share is Ory Hydra behind a login app of the
  platform's own; Hydra has neither resource indicators nor client ID
  metadata documents, so a token's audience stays empty unless the
  login app grants it at consent (FeCode's ADR 0020). A door for a
  person's agents waits for that.
- Default hosts (ADR 0045) live under one apps zone beside the
  platform's own hosts. A label with `--` is a project's choice, first
  come; plain labels are kept for the projects that own the zone. Once
  anyone can make a project, anyone could take a label that reads as an
  operator project's product. ADR 0021 foresees that a project may be
  given a zone of its own instead.

## Decision

### A person is a principal of their own

- A **person** is an issuer and a subject of the shared provider, kept
  as a row with an ID of ferund's, the e-mail address and the name of
  the last sign-in, when they were first and last seen, and whether an
  operator disabled them. E-mail and name are for display: nothing in
  FeRun matches a person by an address. A person is never deleted.
- Everyone the provider lets in may sign in and gets a session. Whom
  the provider lets in, and to which products, is the provider's
  (FeCode's ADR 0020). The operators are the members of the operators'
  group of the provider's `groups` claim; their session is marked as an
  operator's.
- A session keeps the provider's session ID (the ID token's `sid`), and
  ferund's client has a back-channel logout URI: a logout token signed
  with the provider's keys for ferund's client ends every session with
  its `sid` on every instance. The provider sends one when the person
  signs out of any of its apps or at their company (FeCode's ADR 0020).
  A sign-out ends sessions only: tokens live on.
- An **operator** keeps the operators' pages and holds, in every project,
  the rights of its admin. What an operator changes in a project is
  recorded as the operator's, so the project can see it.
- A person an operator **disables** loses every session and every
  grant's effect at once; the grants stay, so enabling restores them.

### A grant names a principal, a scope and a role

- A **grant** gives a principal a role in a scope: the principal is a
  person or a project token; the scope is the project, or one environment
  of it; the role is `read`, `write` or `admin`, each including the one
  before. A principal holds at most one grant per scope. Its role on an
  environment is the higher of its project grant and its grant on that
  environment; its role on the project is its project grant alone. A grant
  on an environment ends with the environment.
- What a role may do on an **environment**:
  - `read`: its objects, deployments and plans, tags, incidents,
    traffic, logs and traces, the names and versions of its secrets
    (never a value), its credentials (never a secret, ADR 0033), its
    hosts and bound domains, and its sandboxes and jobs (ADR 0035);
  - `write`: also plan and apply, revert, tags, secrets' values, its
    credentials and their checks, restarts, runs of its jobs, a
    binding's check, its sandboxes and their tokens, its projects' jobs,
    and services with the runtime `attached` (ADR 0035: each of these
    belongs to one environment);
  - `admin`: also its route tokens, `public` and its host label, binding
    and unbinding domains, its deletion, and the grants on it.
- What only a **project grant** gives, because it reaches beyond one
  environment:
  - `read`: the whole list of environments, the tools' reference, and
    a plan without an environment;
  - `write`: making, cloning and deleting environments, and the
    preflights, which check what a sandbox would get;
  - `admin`: members and invitations, the project's tokens, and claiming,
    checking and deleting domains (a claim is the project's, ADR 0045).
- A **project token** is a principal like a person: it is minted with a
  role and a scope, never above its minter's role on that scope. A token
  minted without a role is `admin` on its project, so FeCode and FeChat
  need no change. Sandbox tokens stay a sandbox's own, of no principal.
- **Answers**: a principal that may not see an object gets the answer of
  an absent one, 404; one that sees it but may not do what it asks gets
  403 naming the role and scope it lacks.
- A project that has an admin always keeps one: the last admin grant
  cannot be removed, lowered or left.
- Where a route touches both scopes, the rules above decide: deleting
  an environment needs `admin` on it or `write` on the project, either
  suffices; a list answers every member, filtered to what it reads.

### People make projects and invite each other

- A signed-in person makes a project, under the projects' name rules, and
  holds `admin` on it. The names the platform needs stay reserved: the
  operators' project, `admin`, `api`, `www`, the apps zone's owners and
  the first label of each of the platform's hosts. An operator chooses
  whether people may make projects at all and how many one person may
  hold as their maker; operators are not bound. A project an operator
  makes through the people's route is a person's project (its maker, the
  admin grant, the default limits, the project zone); the projects of the
  admin API stay the operators' projects, in the apps zone, with a token.
- An **invitation** is a single-use link with a role and a scope, made
  by an admin of that scope, valid for seven days, its secret stored as
  a hash. The signed-in person who opens it first gets the grant, or
  keeps the higher role they hold there; nobody else can use it after.
  FeRun sends no mail: the admin passes the link on. Only a member is
  given a grant directly; a person joins by an invitation.
- A member may leave a project, giving up every grant in it, unless they
  are its last admin.
- Members of a project see each other's name and e-mail address in that
  project, and nowhere else.

### A project's admin deletes it

- A project's **admin** deletes it, as an operator does: its tokens stop
  at once, and its sandboxes, its environments with their addons' data
  and the project go after them. Nothing is kept to restore. Its name is
  free then, and a project made under it again is another (ADR 0056).
- An operator deletes any project, through the admin API or in the
  project's settings as its admin. A project token deletes none: ending
  a project is a person's decision, and a token is a program's. An
  integration deletes one only for a person it names who is the
  project's admin, and as `admin` itself (ADR 0056).
- Deleting the project deletes every environment of it, so whoever
  deletes it may delete each of them, a protected one too (ADR 0060).
- The call names the project twice, in its header and in its body, so
  that a call meant for another project deletes nothing.
- Who may is one rule, which the route checks and the answer about the
  caller gives per project, so that the console and an integration offer
  the deletion where it is allowed and nowhere else.

### The API takes a person as it takes a token

- The project class accepts a person's session beside a token. A session
  names its project in a request header; a token's project is its own, and
  a header that names another is answered as an unknown project. The
  session's cookie is host-only (the `__Host-` prefix): the project zone
  lies below ferund's host, and the prefix keeps an app there from
  setting a cookie the browser sends to ferund. A session's changes
  carry the UI's request header and, with an `Origin`, the public URL's,
  as an operator's do.
- Every route states the role and scope it needs at the route, as the
  auth classes are stated, and a test keeps the list.
- ferund records who made every change itself: deployments, secrets,
  tags, domains, grants, invitations, tokens, sandboxes and jobs carry
  the principal (person or token) beside the free `author` a client
  sends, which stays the client's (FeCode's `fecode:<user>`).
- The project answer the sink asks for (ADR 0026) is given for a session
  too: the project, the principal's role on it and the environments it
  may read. A role on the project reads every stream and span, a
  principal of environments only those of its environments, a stream
  without an environment being a project `read`'s. ferund's own reads of
  the sink for a person pass the session's cookie and header on, as
  they pass a token: no service credential reads the sink for a project,
  and the sink trusts ferund's answer alone. A principal of environments
  only is answered only a sink that says it filters, since one that does
  not would read any answer as the whole project's. Deleting streams
  needs `write` on the project, also for a stream of one environment: the
  sink's deletion is the project's.

### The UI is the projects' console

- After sign-in a person sees their projects, and in a project what their
  roles allow: environments with the builder of ADR 0032 (deployments,
  plan, apply, revert, tags, secrets, public and the host label, bound
  domains); domains (claim, the records to set, their state and last
  check, binding and moving, the certificate); sandboxes (create, pause,
  resume, delete, their tokens, and Connect, which mints a sandbox token
  and shows the MCP address); projects' jobs (create, cancel, the log's
  tail); logs and traces; members, invitations, tokens, and the use
  against the limits.
- A sandbox's activity is its environment's: whoever reads the
  environment sees the sandbox's Activity and Network, the commands its
  agents ran with their output, its state changes (paused, hibernated,
  restored) and its traffic, as they read its services' logs, on the
  project's route `GET /api/v1/sandboxes/{id}/activity`. What tells of
  the fleet stays out of it: a placement names the node it chose, not
  every node's verdict with that node's room, and a node's overbooking
  is not shown. An operator reads it whole, a deleted sandbox's too, on
  the admin route.
- The operators' pages (nodes, join tokens, instances, every project,
  people) are for operators only.
- The browser keeps no project token: the session acts. What a role
  cannot do is not offered; the API decides.

### The sandbox's door stays the sandbox's

- The MCP door `/mcp` is the sandbox's: the tool baseline on a sandbox's
  token, one sandbox per token, with keyed calls (ADR 0023). It is the
  door of the programs that run agents in sandboxes, FeCode first,
  FeChat beside it. Its path, its token, its tools' names and schemas,
  its call IDs and its answers are FeCode's contract: what it gains is
  added beside what is there, never in its place.
- A person reaches a sandbox's door with a sandbox token, which a
  principal with `write` on the sandbox's environment mints in the
  console or over REST.
- A door for a person's agents, should it come, answers another
  question: it is the console as tools, making and changing projects,
  environments and sandboxes, while the sandbox's door runs commands in
  one sandbox. The two stay apart, so that the sandbox door's contract
  stays FeCode's alone and a person's console never becomes a shell
  into every sandbox.

### Limits per project

- A project has **limits** on what it asks for, checked when it asks: the
  number of its sandboxes, of its environments and of its projects' jobs
  running, and the sums of vCPUs and of memory over its sandboxes, its
  services' replicas, its addons' plans and its running jobs. A plan
  that would bring a sum past a limit, and asks for more than the
  current deployment has, carries an error finding; a create over a
  limit is refused with the limit and the use. Concurrent creates cannot
  pass a limit together.
- Projects that people make get the operator's defaults; projects that
  operators make have none unless an operator sets them; an operator
  changes any project's. The sink's daily quotas for logs and traces are
  apart. There is no billing; the console shows the use against the
  limits.

### A zone for the projects people make

- The **project zone** is the zone of the default hosts of the projects
  people make; the projects operators make keep the apps zone. The zone
  is the project's, set when the project is made; an operator may move a
  project to another zone, which moves its environments' hosts as a
  changed label does (ADR 0045).
- The project zone has the apps zone's rules: the operator's wildcard
  certificate, served by the ingress for every name it covers; cookies
  made host-only by the ingress; no claims of names under it; labels by
  ADR 0045. It has no owners, hence no plain labels.
- A label is a name in its zone, registered per zone, so a label taken
  in the project zone is free in the apps zone. A move keeps each
  environment's label where it may, else gives it its natural label or
  its fallback there.
- A zone of its own keeps the labels of the platform's products out of
  reach of anyone who makes a project. The zone may be a subdomain of the
  platform's domain or a registrable domain of its own; the latter also
  keeps the platform's cookies, reputation and blocklist entries apart
  from what projects publish.

## Alternatives considered

- **FeRun stays the operators'**, and a product in front of it carries
  people, as FeCode does. A CaaS customer without FeCode would need
  another product to sign in, and every such product would build
  projects, roles and quotas again over one token.
- **FeCode as FeRun's front door**: CaaS customers as FeCode users with
  a project each. The container service would carry the coding
  product's model, git and registry included, which are out of scope.
- **Grants per object** (a service, a secret) **or roles of the
  project's own**: more than is asked for. The environment is CaaS's
  unit and the project the unit of ownership; three roles on two scopes
  say what is needed and fit a table.
- **Sandboxes, jobs and attached services on the project grant alone**,
  because a sandbox could reach any of the project's environments: the
  rule for one day, it went with ADR 0035, which puts every sandbox and
  job into one environment, so that the environment's grant covers them
  and a person who writes one environment can run a shell or a
  migration beside it.
- **A project per person**, no members: sharing would mean sharing a
  token, and nobody would know who changed what.
- **Deletion by operators only**, the rule until 2026-11-22: a person who
  made a project asked an operator to delete it, and a platform in front
  of FeRun that deleted a project of its own left the project at FeRun
  behind (FeCode's ADR 0037).
- **A sandbox's activity for operators only**, the rule until
  2026-11-11, because it holds the tool calls' parameters and output.
  Those are the project's own, written by its agents in its sandboxes,
  and a reader of the environment reads its logs, which hold as much;
  kept from the project, nobody but an operator could see what an agent
  ran or when a sandbox paused and hibernated.
- **The activity whole for its readers**: a placement's verdicts name
  every node and its booked room, which is the fleet's, the same for
  every project, and no project's to read.
- **An authorization engine** (OpenFGA, SpiceDB): another system to run
  and keep consistent with ferund's rows, for three roles on two scopes.
- **Labels of their own in the apps zone** (a prefix per project made by
  a person): still beside the platform's hosts and cookies, and the move
  to a registrable domain would change every label twice.
- For a door for a person's agents, not built (backlog), the shape
  weighed and kept: one door for every project the person is a member
  of, an OAuth 2.1 resource server that accepts the shared provider's
  access tokens bound to it by audience, and project tokens for agents
  in a CI, with one tool per operation of the project class and nothing
  that passes a secret through the agent's model. Set aside:
  - **ferund as an OAuth authorization server of its own**: it could
    bind audiences and read client ID metadata documents itself, but it
    would be a second issuer of people's tokens, with keys, refresh
    tokens and consent screens in the control plane, beside the login
    app that already asks for consent.
  - **Accepting any token of the provider**: a token issued to another
    client or for another service could act in a project, which the
    spec forbids.
  - **One door with everything**, the sandbox tools with a `sandbox`
    argument beside the console's: an agent could work in a sandbox it
    has just made without its person adding a server, but the tools
    would lose the schemas FeCode's agents know, and a person's console
    would be a shell into every sandbox of every project.
  - **A sandbox's door for people with the provider's tokens**: a
    second contract for the same tools beside FeCode's, for clients
    that a sandbox's token serves already. It can come beside `/mcp` if
    a client needs OAuth there.
  - **A door per project**: a token bound to one project by its audience,
    but a person in several projects adds a server for each, and a
    project made over MCP is out of reach until they do. The grants
    bound what a token can do; the audience binds it to FeRun.

## Consequences

- ferund holds people, grants and invitations; project tokens carry a
  role and a scope; projects carry their zone, their maker and their
  limits; sessions belong to people, not only to operators.
- Every route of the project class states its role, and its tests cover
  each role, a grant on another environment and a principal of another
  project.
- FeCode and FeChat change nothing: their tokens are admins of their
  projects. FeCode's ADR 0012 still holds for the registry, and FeRun's
  grants reach none of FeCode's projects, which stay labels in FeCode's
  one project at FeRun.
- The console works with the session alone; a person with grants on
  environments alone opens a project at those environments. The sink
  resolves sessions and environment grants through ferund. `/mcp` stays
  FeCode's contract, unchanged.
- An operator publishes the project zone's wildcard DNS and certificate,
  chooses whether people make projects and how many, and sets the
  default limits. `docs/architecture/control-plane.md` describes the
  sign-in, the grants, the members' routes, the quotas, the zones and
  the console as the code has them.

## History

Proposed on 2026-11-06 (544bac4) and built the same day in steps:
people and everyone's sign-in (6c79801), projects made by people,
invitations and the last admin (58356c9), quotas (cab3158), the project
zone and labels per zone (0cda975, 20f8894), the sink's readers by
session (be087e0, e382591), the console for people (1f7cc0c); acfc24a
recorded what the implementation chose, and 62c12da made the session's
cookie host-only. ADR 0035 (fea6fcb, 2026-11-07) moved sandboxes,
projects' jobs and attached services from the project grant to the
environment's. The general MCP door for a person's agents, planned as a
later step, was not built and is in the backlog. On 2026-11-11 a
sandbox's Activity and Network went from the operators to the readers
of its environment, without the fleet's part. On 2026-11-22 Malte
decided that a project's admin deletes it, which the backlog had kept:
`DELETE /api/v1/project` and the console's Delete project (0bec7b45),
an integration for a person it names who is its admin.
