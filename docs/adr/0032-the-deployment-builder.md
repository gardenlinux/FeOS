# ADR 0032: The deployment builder: the UI edits an environment's spec as a graph, forms and JSON of one draft, kept in the browser, planned on every change and applied as the next deployment

**Status:** Proposed · **Date:** 2026-11-06

**Touches:** ADR 0018 (a plan answers the images it pinned; a catalog
of the addons' blueprints; the management UI writes deployments), ADR
0021 (an environment's hosts are the graph's entry), ADR 0025 (the
builder writes a registry's password only as a secret), ADR 0028
(bindings are objects of the graph), ADR 0031 (its grants decide what
the builder offers), and the draft of security groups of a tenant's own
(an object the graph shows once it exists).

## Context

FeRun becomes a container service that people use without FeCode,
through its own UI (ADR 0031). ADR 0018 gives every caller one API, "a
person through a CLI or FeCode's UI" among them, and the API is
complete: plan, apply with a `base`, revert, tags, clone, object
changes, findings worded for whoever writes the spec (the path, the
problem, the remedy). The management UI reads environments and "writes
nothing". The only way to state an environment is to write its spec as
JSON and send it, which an agent does and most people will not.

The spec is a graph already. An environment holds services, jobs,
addons, routes, secrets and bindings, each with a name, and they
reference each other by name: a service's variable takes an addon's
output (`{addon: db, key: url}`), another service's address
(`{service: api, port: http, key: url}`) or a secret; a route names a
service and its port; a service names its bindings, a binding its
secrets. A dangling reference is a finding of the write, not a pod that
fails. A person draws the same thing: the internet reaches the web at
`/`, the web calls the API, the API has its Postgres.

What constrains a builder:

- **`ferund` is the only judge.** Validation, defaults, the addons'
  blueprints and the effects of a change are `ferund`'s and change with
  its versions. A UI that judges on its own drifts from them.
- **The UI's dependencies are fixed**: React, Vite and Vitest; a
  package needs the maintainer's approval.
- **No git and no registry.** Images come from any registry, with the
  create's credentials (ADR 0025); FeRun stores no source.
- **People have roles** (ADR 0031): some only read, some write one
  environment, some a tenant.
- **Narrow screens** have no room for a canvas.

What a builder must not become: a second truth (a format of its own
beside the deployments, templates that live on, positions that need a
store), or a tool that changes what its user did not touch (a tag
resolved again, a secret's new version rolled in unasked).

## Decision

### The builder edits the spec and nothing else

- A **draft** is a deployment spec in the API's JSON and the number of
  the deployment it starts from, its base. It starts from the latest
  deployment's stored spec (whatever its state: the desired one), from
  an empty spec, from a deployment of another environment (a clone, a
  new environment) or from an imported file.
- The graph, the forms and the text are three views of the one draft.
  An edit in any of them is an edit of the JSON; the builder holds
  nothing beyond it but the base, an undo history and which object is
  open.
- **Plan on every change**: 500 ms after the last edit, one plan in
  flight, the answer to an older draft dropped. Each finding is shown at
  the object and field its path names (`services.web.ports[1].port` is
  the node `web`, its field `ports`); one without a path above the
  graph. The diff against the base is the deployments tab's, with each
  change's effect; `deletes data` and `recreates (downtime)` are asked
  again at apply.
- **Apply** sends the draft, its base and a message, which the builder
  proposes from the diff ("web: image nginx:1.27 → 1.28; route /api
  added") and the person may change. `201` shows the deployment and its
  rollout and ends the draft; `unchanged` says so; `422` shows the
  findings as a plan's; `409 base_mismatch` rebases (below).
- The builder writes deployments and secrets' values, nothing else.
  Restarts, job runs, revert, tags, publishing, route tokens and domains
  are actions of their own views.
- Its own checks only make a field enterable (a DNS label, a number in
  range). The verdict is the plan's: a spec `ferund` accepts, the
  builder accepts.

### Only what the person changed changes

- **Images.** A stored spec has every image pinned,
  `<registry>/<repository>@sha256:…`, with `imageAsWritten`, and a
  digest reference stays as it is when normalised. So a draft from a
  deployment keeps its digests, and a change of a variable never pulls a
  tag that moved. The form shows the image as written with its digest;
  "Check for a newer image" puts the image as written back, which plan
  resolves, and the diff shows `<as written> (<digest>)` when it moved.
- Apply resolves images again, outside its transaction, so a tag that
  moves between plan and apply would deploy a digest no plan showed. A
  plan's answer therefore gains the pins it resolved, `images:
  {"<as written>": "<registry>/<repository>@sha256:…"}`, and the
  builder writes them into the draft, with `imageAsWritten`, before it
  applies: what was shown is what runs. An image that does not resolve
  is a plan's warning and an apply's error, and no key of `images`; a
  digest reference is never one (it stays, with its `imageAsWritten`).
  The builder does not apply while an image that is not a digest
  reference has no pin. A plan without an environment (`POST
  /api/v1/environments/plan`, a new environment's) answers `images` as
  well.
- **Secrets.** A stored spec pins every referenced secret to a version
  (`secrets: {name: {version}}`), and the draft keeps the pins. A
  secret whose latest version is newer than its pin says so on its node
  ("version 4, pinned 3", "Use version 4").
- **Defaults** that normalisation filled in stay in the draft: they
  change nothing, and they show what runs.

### A secret's value goes through the secrets API

- No value is in the draft, the browser's storage, the undo history or
  an export. A secret's node shows its name, its pinned and latest
  versions and what references it.
- A value entered, for a new secret or a new version, is a `PUT
  …/secrets/{name}` at once, and the draft pins the version it made. A
  draft discarded afterwards leaves a version nothing pins, which is
  history, not exposure. For an environment that does not exist yet the
  builder keeps the values in memory until its first apply, and then
  creates the environment, writes the secrets and applies deployment 1,
  in that order; a step that fails leaves what the earlier ones made,
  which the builder shows and continues from.
- A reference to a secret that does not exist is the plan's error, and
  the node is drawn as missing with "Set a value".
- **Registry logins** (`imageCredentials`): the form asks for a user and
  a password and always stores the password as a secret (it proposes
  `registry-<host>`), never as a literal, which would stand in the clear
  in the spec, its plans and its deployments, as ADR 0028 rules out for
  bindings. Such a secret is read at its latest version whenever a pod
  is created, not at the pin (ADR 0025), and its node says so.

### The graph

- **Nodes**: services, jobs, addons, bindings and secrets, and one
  **entry**: the environment's hosts as `ferund` reports them, or, for
  an environment that is not public, the route door with its route
  tokens. An attached service's sandbox is a node outside the
  environment's frame.
- **Edges** are references, drawn from the object that holds them:
  - a variable `{addon, key, …}` to the addon, labelled with the
    variable and the key (and a role);
  - a variable `{service, port, key}` to that service's port;
  - a variable `{secret}`, and `imageCredentials` (dashed), to the
    secret;
  - a service's or job's `bindings` to each binding, a binding's
    credential to its secrets;
  - an attached service to its sandbox;
  - a **route** from the entry to a service's port, labelled with its
    path. Routes are edges, not nodes.
- Kinds to come slot in without changing the rest: volumes and mounts
  (ADR 0016, when services get them) as nodes with an edge from their
  service; security groups of a tenant's own as frames around their
  members. The graph does not wait for them.
- **The layout is automatic and the same for everyone**: columns by
  kind (the entry; services, then jobs; addons and bindings; secrets),
  in each column an order that keeps edges short (by the mean position
  of a node's neighbours, ties by name). A service that another service
  or a job calls stands in a column of the services right of its
  caller, so that every reference runs left to right (a cycle is cut
  where it closes); an edge that passes a column runs through a gap
  between its nodes. No positions are stored: not in
  the spec, since a position changes nothing of the environment, not in
  a deployment's annotations, which are the caller's and 4 KiB, and not
  in a table of their own, where they would be stale for every object an
  agent or a CI adds.
- **Live state.** Drawn for a deployment, each node carries its
  object's tone from the environment's status (ready, starting, failed)
  and its open incidents.

### Drawing a reference

- A service's or job's node has a handle. Dropped on an addon, it offers
  the addon's outputs from the catalog and a variable name
  (`DATABASE_URL` for Postgres's `url`); on a service, that service's
  ports and the keys `url`, `host` and `port`; on a secret, a variable
  named after it; on a binding, the binding. The choice writes the
  reference into the object's `env` or `bindings`.
- Dragged from the entry onto a service's port, the handle makes a
  route: `/` for the first, a path asked for the others.
- Every edge is a line of its object's form too (the variables, the
  bindings, the routes): the graph is never the only way, for the
  keyboard and for narrow screens.
- Removing a node that others reference leaves dangling references,
  which the plan names; the builder offers to remove them with it.

### Forms, the catalog, a first deployment

- One form per kind, with the API's fields and its defaults as
  placeholders.
- **A catalog in `ferund`**: `GET /api/v1/addons` answers the registered
  blueprints, each with its type, versions, plans (vCPUs, memory,
  volume), default plan, the output keys and fields a reference may
  name, and the option fields the blueprint declares (a name, a kind:
  string, list, boolean or one of values, and a default, with whether
  it is required, a description and the constraints the validation
  applies: a pattern, reserved names and prefixes, uniqueness). The
  validation reads the same declarations. A version has no default (one
  would change under a running addon); `defaultVersion`, the newest, is
  what a new addon is offered with. The UI copies
  none of it: a new blueprint appears without a UI release, and options
  a blueprint does not declare get a JSON field.
- **A first deployment is a minute's work**: an empty environment offers
  "Add a service", an image and a port named `http`, and the builder
  adds the route `/` to it; plan, apply. Making it public is the
  environment admin's switch beside the entry (ADR 0021, ADR 0031).
- **Templates** ("web and Postgres", "web, API and Valkey") come in step
  2, as fragments of a spec that add objects to the draft and are
  nothing afterwards: not stored, no versions, no link to what they
  made.
- An **attached** service is offered only to those who write the tenant
  (ADR 0031), since it runs in a sandbox of the tenant; others see it
  and cannot edit it.

### The text is JSON

- The text view is the draft as JSON, indented, its keys in the order
  the API answers them. An edit is parsed on each change; text that
  does not parse is marked, and the graph and the plan keep the last
  draft that did.
- JSON, because it is the API's format, parsed strictly by `ferund`
  (an unknown field or a duplicate key is a finding at its path), and
  what deployments store. Not YAML: a parser is a package the UI does
  not have, and YAML 1.1's implicit types change meaning without a
  word: a job's key `on` reads as `true`, `no` as false, `1.20` as a
  number.
- **Export** writes the draft or any deployment's spec as a file;
  **import** replaces the draft, and the plan shows what it changes
  against the base. A stored spec is valid input.

### The draft lives in the browser

- In local storage, per tenant and environment and one slot for a new
  environment, written on every change and read when the builder opens.
  Where storage fails, the draft lives in memory and the builder says
  so. Undo and redo live in memory.
- **Rebase, object by object.** A draft whose base is no longer the
  latest deployment, opened or refused with `base_mismatch`, is rebased:
  its changes since the base, object by object (kind and name), are
  applied to the latest deployment's spec. An object changed in the
  draft and since the base is a conflict, shown with both versions; the
  person keeps one. Then plan runs again. A draft is a whole spec:
  applied onto a newer deployment without a rebase it would undo the
  other change, which `base` exists to prevent.
- **No drafts in `ferund`.** They would need a table, grants, expiry
  and a merge between editors, for a tool one person uses at a time on
  one environment. Two people at once have the base and the rebase; a
  draft passes to someone else as an export.

### Who may do what

- ADR 0031's grants decide: `read` sees the graph, the deployments'
  specs and the text, and exports; `write` on the environment edits,
  plans, sets secrets' values and applies; `write` on the tenant creates
  and clones environments from the builder and edits attached services;
  `admin` on the environment publishes it. The builder offers what the
  principal may, as `ferund` reports it, and `ferund` refuses the rest.
- `ferund` judges a write by what it changes against the current
  deployment: a draft whose attached services are unchanged passes for
  someone who writes the environment alone.
- The author of a deployment is the principal `ferund` authenticated
  (ADR 0031); the builder sends the message only.

### Narrow screens

- Below 900 px there is no canvas. The same draft is a list of objects
  by kind, each a card with its references as lines ("DATABASE_URL →
  db · url") that opens its form at full width, and a reference is made
  with "Add reference", which asks for the target and the key as the
  drop does. Plan, diff, apply and the text are the same.

### Drawn by hand

- Nodes are HTML boxes placed by the layout over an SVG layer of edges;
  a handle is dragged with pointer events; the canvas scrolls, and a
  focus mode dims what is not next to the open node. No zoom in step 1.
- By hand, because the layout is automatic: what a graph library brings
  (moving nodes, zoom, a minimap) is what this decision leaves out, and
  the UI's dependencies are fixed. React Flow (`@xyflow/react`, MIT) is
  the candidate if moving nodes or zoom are wanted after step 1, with
  the maintainer's approval as for any package.
- The draft's operations (references of a spec, edges, layout, findings
  by path, rebase) are pure functions beside `envs.js`, tested with
  Vitest against the fixtures of `ferund`'s API tests; the components
  render.

### Steps

1. In `ferund`: the plan's `images` and `GET /api/v1/addons` with the
   blueprints' option fields. In the UI: an environment's tab
   **Builder** and "New environment"; drafts from the latest
   deployment, empty or imported; the forms of image services, jobs,
   addons, routes and secrets; the graph with drawn variables and
   routes; the JSON text; plan on every change with findings and diff;
   apply with base and rebase; secrets' values and registry logins;
   export; the list for narrow screens.
2. The bindings' forms; templates; clone into a new environment; the
   overview's object cards become the graph of the deployed deployment
   with its live state.
3. Attached services for those who write the tenant; security groups of
   a tenant's own and volumes as they arrive; what step 1 taught about
   large environments (zoom, React Flow).

The overview, deployments, incidents, logs and traces tabs stay as they
are until step 2.

## Alternatives considered

- **Forms without a graph**, a wizard. Simpler, and the references, the
  part that makes an environment, stay invisible. It stays as the list
  for narrow screens.
- **A format of the builder's own** (a compose-like file, a graph
  document with positions) translated into the spec: a second truth with
  versions of its own and a translation that drifts from `ferund`'s
  validation. ADR 0018 set a file aside as the architecture for the same
  reasons.
- **Validation in the UI.** Immediate, and a copy of `ferund`'s rules
  that changes with each version. A plan answers in the time a person
  takes to look up, and its findings are worded for whoever writes.
- **Object changes per form** (`PUT …/objects/{kind}/{name}`): every
  save a deployment and a rollout, and a change across objects (an addon
  and the variable that uses it, a renamed service and its references)
  rolls twice, the first time with a dangling reference.
- **The draft as written, without pins**, so that every apply resolves
  tags: it changes what the person did not touch.
- **Drafts in `ferund`**, **stored positions**, **YAML** and **React
  Flow now**: above.

## Consequences

- `ferund` gains two small answers: a plan's `images` and the addons'
  catalog, for which blueprints declare their option fields. No table.
- The management UI writes deployments and secrets' values; its
  description in `docs/architecture/control-plane.md` changes with step
  1.
- A spec the builder writes is one an agent, a CLI or a CI writes, and
  a deployment an agent made opens in the builder: one API stays one
  API.
- People who never wrote JSON make environments; agents keep writing
  JSON.
- A draft is one browser's. Another device does not see it; an export
  moves it.
- An apply from the builder runs the digests its plan showed; a tag that
  moves afterwards waits for the next "Check for a newer image".

## Open questions

- Whether a plan also answers an image's exposed ports, from its
  config, so that the first port is proposed rather than typed.
- Whether the proposed variable names (`DATABASE_URL`) belong to the
  catalog, as each blueprint's suggestion, or to the UI.
- Whether columns carry an environment at the spec's limits (64
  services, 32 jobs, 128 routes), or zoom and grouping by label are
  needed.
- A review of a draft before it is applied, if deployments get
  approvals; today that is a policy of the caller.
