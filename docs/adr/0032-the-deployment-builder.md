# ADR 0032: The deployment builder: one draft, forms and JSON, planned on every change

**Status:** Accepted · **Date:** 2026-11-06

The console's deployment builder edits an environment's deployment spec
and nothing else: one draft, the spec in the API's JSON and the number
of the deployment it starts from, kept in the browser and never in
ferund, shown as forms and as JSON text, two views of the one draft.
Every change is planned by ferund, whose findings appear at the object
and field their path names; apply writes the next deployment with the
draft's base and the image digests the plan showed, and a refused base
rebases the draft object by object. Secrets' values go through the
secrets API at once and never into the draft, a registry's password
always as a secret. ferund answers a catalog of the addons' blueprints,
so the forms copy none of its rules. ADR 0031's grants decide what the
builder offers.

## Context

FeRun is a container service that people use without FeCode, through
its own console (ADR 0031). ADR 0018 gives every caller one API, a
person through a UI among them, and the API is complete: plan, apply
with a `base`, revert, tags, clone, object changes, findings worded for
whoever writes the spec (the path, the problem, the remedy). Without a
builder the only way to state an environment is to write its spec as
JSON and send it, which an agent does and most people will not.

An environment holds services, jobs, addons, routes, secrets and
bindings, each with a name, and they reference each other by name: a
service's variable takes an addon's output, another service's address
or a secret; a route names a service and its port; a service names its
bindings, a binding its secrets or a credential of the tenant. A
dangling reference is a finding of the write, not a pod that fails.
Every one of these references is a field of one object.

What constrains a builder:

- **ferund is the only judge.** Validation, defaults, the addons'
  blueprints and the effects of a change are ferund's and change with
  its versions. A UI that judges on its own drifts from them.
- **The UI's dependencies are fixed**: React, Vite and Vitest; a package
  needs the maintainer's approval.
- **No git and no registry.** Images come from any registry, with the
  create's credentials (ADR 0022); FeRun stores no source.
- **People have roles** (ADR 0031): some only read, some write one
  environment, some a tenant.
- **Narrow screens** have room for one column.

What a builder must not become: a second truth (a format of its own
beside the deployments, templates that live on, positions that need a
store), or a tool that changes what its user did not touch (a tag
resolved again, a secret's new version rolled in unasked).

## Decision

### The builder edits the spec and nothing else

- A **draft** is a deployment spec in the API's JSON and the number of
  the deployment it starts from, its base. It starts from the latest
  deployment's stored spec (whatever its state: the desired one), from
  an empty spec, or from an imported file. A new environment's draft
  has no base; a clone is an action of the environment's settings (ADR
  0018).
- The forms and the text are two views of the one draft. An edit in
  either is an edit of the JSON; the builder holds nothing beyond it but
  the base, an undo history and which object is open.
- **Plan on every change**: after a short pause in editing, one plan in
  flight, the answer to an older draft dropped. Each finding is shown at
  the object and field its path names; one without a path above the
  forms. The review shows the diff against the base with each change's
  effect; `deletes data` and `recreates (downtime)` are asked again at
  apply.
- **Apply** sends the draft, its base and a message, which the builder
  proposes from the diff and the person may change. A new deployment
  shows the rollout and ends the draft; an unchanged spec says so; a
  refused spec shows the findings as a plan's; a refused base rebases
  (below).
- The builder writes deployments and secrets' values, nothing else.
  Restarts, job runs, revert, tags, publishing, route tokens and
  domains are actions of their own views.
- Its own checks only make a field enterable (a DNS label, a number in
  range). The verdict is the plan's: a spec ferund accepts, the builder
  accepts.

### Only what the person changed changes

- **Images.** A stored spec has every image pinned to a digest, with
  the image as written beside it, and a digest reference stays as it is
  when normalised. So a draft from a deployment keeps its digests, and a
  change of a variable never pulls a tag that moved. The form shows the
  image as written with its digest; "Check for a newer image" puts the
  image as written back, which plan resolves, and the diff shows the
  move.
- Apply resolves images again, outside its transaction, so a tag that
  moves between plan and apply would deploy a digest no plan showed. A
  plan's answer therefore carries the pins it resolved, and the builder
  writes them into the draft before it applies: what was shown is what
  runs. An image that does not resolve is a plan's warning and an
  apply's error, and has no pin; a digest reference needs none. The
  builder does not apply while an image that is not a digest reference
  has no pin. A plan without an environment, a new environment's,
  answers the pins as well.
- **Secrets.** A stored spec pins every referenced secret to a version,
  and the draft keeps the pins. A secret whose latest version is newer
  than its pin says so in the list and at every variable that names it,
  with the move offered.
- **Defaults** that normalisation filled in stay in the draft: they
  change nothing, and they show what runs.

### A secret's value goes through the secrets API

- No value is in the draft, the browser's storage, the undo history or
  an export. A secret's form shows its name, its pinned and latest
  versions and what references it.
- A value entered, for a new secret or a new version, is written to the
  secrets API at once, and the draft pins the version it made. A draft
  discarded afterwards leaves a version nothing pins, which is history,
  not exposure. For an environment that does not exist yet the builder
  keeps the values in the page until its first apply, and then creates
  the environment, writes the secrets and applies deployment 1, in that
  order; the environment's ID is the draft's, so a repeat is the same
  create, and a step that fails leaves what the earlier ones made, which
  the builder shows and continues from.
- A reference to a secret that does not exist is the plan's error, and
  the secret is marked missing, with "Set a value" in its form and at
  the variable.
- **Registry logins**: the form picks a secret and a user, or asks for a
  user and a password and always stores the password as a secret (it
  proposes a name from the registry's host), never as a literal, which
  would stand in the clear in the spec, its plans and its deployments,
  as ADR 0028 rules out for bindings. Such a secret is read at its
  latest version whenever a pod is created, not at the pin (ADR 0022),
  and its form says so.

### Forms: a list of the objects and the selected one's form

People know their architecture and come to change a field: an image, a
variable, a replica count. Cloud consoles edit a service in forms, and a
reference is a typed field of the object that holds it. The builder
draws no graph.

- **What is edited is said first.** A band above names the draft: the
  deployment it will become and the one it starts from, the number of
  changes against the base, the plan's verdict, and that nothing changes
  in the environment until it is deployed; its main action is the
  review. The environment's page names the builder "New deployment".
- **The list**: the spec's objects by kind, services, jobs, addons,
  routes, bindings and secrets, each a row with its name, one line (a
  service's image tag, an addon's type and plan, a route's path and
  service, a binding's credential, a secret's pin), its change against
  the base (added, changed, removed; the removed ones kept as rows with
  a restore) and its findings' count; an add per kind, an addon from
  the catalog. Nothing selected, the other side shows the draft's
  overview: what the environment runs, its entry (the hosts as ferund
  reports them, or the route door) and the routes.
- **The form** of the selected object, titled with its kind and name,
  in sections: a service's container (the image with its pin, the
  command one argument per line, the ports, the registry login), its
  variables (a table of names and values, each value a typed choice:
  text, a secret with its pin and "set a value", an addon's output from
  the catalog's keys, a service's address with its port and `url`,
  `host` or `port`), its reachability (what it `uses` beyond its
  references and its `egress`, ADR 0012; a route's `from`), scaling and
  resources within the spec's bounds and the idle, the health check,
  credentials and bindings (the spec's bindings to tick; a binding of a
  tenant's credential, ADR 0033, added and ticked at once) and the
  rollout; its remove at the bottom, which offers to take the
  references to it along. A binding is a tenant's credential, or, folded
  as advanced, ADR 0028's fields with the environment's secrets. An
  attached service's form names its sandbox and says that its
  reachability is its sandbox's pod's (ADR 0035).
- **The address names the object**: an object's form has an address of
  its own, so an object's page links its Edit there, and a binding's
  page its Add and Edit. Kinds to come slot in as groups of the list
  and forms.
- **Narrow screens**: the list is the first screen; a tap opens the form
  at full width with a way back. The band, the review and the text are
  the same.
- The draft's operations (references of a spec, the list's lines and
  marks, the typed values to the spec's forms and back, findings by
  path, pins, rebase) are pure functions beside the environments'
  module, tested with Vitest against the fixtures of ferund's API tests;
  the components render.

### The catalog and a first deployment

- One form per kind, with the API's fields and its defaults as
  placeholders.
- **A catalog in ferund**: the tenant class answers the registered
  blueprints, each with its type, versions, plans, default plan, the
  output keys and fields a reference may name, and the option fields the
  blueprint declares (a name, a kind, a default, whether it is required,
  a description and the constraints the validation applies). The
  validation reads the same declarations. A version has no default (one
  would change under a running addon); the newest is what a new addon
  is offered with. The UI copies none of it: a new blueprint appears
  without a UI release, and options a blueprint does not declare get a
  JSON field.
- **A first deployment is a minute's work**: an empty environment offers
  "Add a service", an image and a port, which the builder names `http`
  and gives the route `/`; plan, apply. Making it public is the
  environment admin's switch beside the entry (ADR 0021, ADR 0031).

### The text is JSON

- The text view is the draft as JSON, indented, its keys in the order
  the API answers them. An edit is parsed on each change; text that does
  not parse is marked, and the forms and the plan keep the last draft
  that did.
- JSON, because it is the API's format, parsed strictly by ferund (an
  unknown field or a duplicate key is a finding at its path), and what
  deployments store.
- **Export** writes the draft or any deployment's spec as a file;
  **import** replaces the draft, and the plan shows what it changes
  against the base. A stored spec is valid input.

### The draft lives in the browser

- In local storage, per tenant and environment and one slot for a new
  environment, written on every change and read when the builder opens.
  Where storage fails, the draft lives in memory and the builder says
  so. Undo and redo live in memory.
- **Rebase, object by object.** A draft whose base is no longer the
  latest deployment, opened or refused with a base mismatch, is rebased:
  its changes since the base, object by object (kind and name), are
  applied to the latest deployment's spec. An object changed in the
  draft and since the base is a conflict, shown with both versions; the
  person keeps one. Then plan runs again. A draft is a whole spec:
  applied onto a newer deployment without a rebase it would undo the
  other change, which `base` exists to prevent.
- **No drafts in ferund.** Two people at once have the base and the
  rebase; a draft passes to someone else as an export.

### Who may do what

- ADR 0031's grants decide: `read` sees the forms, the deployments'
  specs and the text, and exports; `write` on the environment edits,
  plans, sets secrets' values, applies and edits attached services (ADR
  0035); `write` on the tenant makes a new environment from the builder;
  `admin` on the environment publishes it. Below `write` the builder
  shows the latest deployment's spec read-only, plans nothing (a plan
  needs `write`), writes no secret and stores no draft. The builder
  offers what the principal may, as ferund reports it, and ferund
  refuses the rest.
- The author of a deployment is the principal ferund authenticated (ADR
  0031); the builder sends the message only.

## Alternatives considered

- **A graph of the references**, built and dropped: services, jobs,
  addons and bindings as nodes, the references and routes as edges,
  laid out by dependency, a reference drawn by dragging a handle onto
  its target. People did not understand it and saw no value over forms:
  they know their architecture and come to change a field, and a graph
  is harder to read and to change than a typed field (which edge is
  which variable, where its key is chosen), needs a layout and a canvas
  that narrow screens lack, and no cloud console edits a service that
  way. The references stay visible as the typed values and each
  object's "used by".
- **Forms for a tenant's security groups** in the builder, for one day:
  they went when ADR 0012 came to derive reachability from
  the spec's references, so there are no groups to edit; the service and
  job forms carry `egress` and `uses`, the route form `from`, and the
  environment's Reachability view draws the picture.
- **A wizard**: one path through the kinds. Fine for a first service,
  which the empty draft's first step is; a hindrance for the change of
  one field, which is most of the work.
- **A format of the builder's own** (a compose-like file, a graph
  document with positions) translated into the spec: a second truth with
  versions of its own and a translation that drifts from ferund's
  validation. ADR 0018 set a file aside as the architecture for the same
  reasons.
- **Validation in the UI.** Immediate, and a copy of ferund's rules that
  changes with each version. A plan answers in the time a person takes
  to look up, and its findings are worded for whoever writes.
- **Object changes per form** (a `PUT` of one object): every save a
  deployment and a rollout, and a change across objects (an addon and
  the variable that uses it, a renamed service and its references) rolls
  twice, the first time with a dangling reference.
- **The draft as written, without pins**, so that every apply resolves
  tags: it changes what the person did not touch.
- **Drafts in ferund**: a table, grants, expiry and a merge between
  editors, for a tool one person uses at a time on one environment.
- **YAML** as the text: a parser is a package the UI does not have, and
  YAML 1.1's implicit types change meaning without a word: a job's key
  `on` reads as `true`, `no` as false, `1.20` as a number.
- **Judging a write by what it changes** (a draft whose attached
  services are unchanged passing for someone who writes the environment
  alone): needed while attached services were the tenant grant's; ADR
  0035 makes them the environment's, so the role on the environment
  decides.

## Consequences

- ferund answers two small things for the builder: a plan's image pins
  and the addons' catalog, for which blueprints declare their option
  fields. No table.
- The console writes deployments and secrets' values through the one
  API: a spec the builder writes is one an agent, a CLI or a CI writes,
  and a deployment an agent made opens in the builder. People who never
  wrote JSON make environments; agents keep writing JSON.
- A draft is one browser's. Another device does not see it; an export
  moves it.
- An apply from the builder runs the digests its plan showed; a tag that
  moves afterwards waits for the next "Check for a newer image".
- `docs/architecture/control-plane.md` describes the builder's storage,
  modules and role rules as the code has them.

## History

Proposed on 2026-11-06 (83ec240) with a graph of the references as the
main view; the plan's image pins and the catalog came the same day
(cfc051f), and the graph was built (4250b9a, ce2bc85) and replaced by
forms on 2026-11-07 (5df7f5d) after people did not take to it. Forms
for a tenant's security groups were added on 2026-11-07 (fd72896) and
went with ADR 0034's revision on 2026-11-08 (a061348, 551904f), which
gave the forms `egress`, `uses` and `from`. ADR 0035 (fea6fcb,
2026-11-07) made an attached service a matter of `write` on the
environment. Templates, planned as a second step, were not built and
are in the backlog.
