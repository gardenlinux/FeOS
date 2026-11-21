# ADR 0057: The spec is one format for every caller: a published module, JSON or YAML

**Status:** Proposed · **Date:** 2026-11-21

An environment's spec has one definition, FeRun's, and every caller
uses that one: FeRun publishes the spec's types, its strict parser and
its merge, the API's answer types and a client as a Go module of its
own, so that a caller compiles against the format instead of keeping a
copy of it. The API takes a spec as JSON or as YAML, read by the same
rules. What a caller may write, the sizes, the plans, the bounds, it
asks FeRun and keeps no list of. Judging a spec stays `ferund`'s alone:
a caller parses, and asks the plan.

## Context

ADR 0018 gave every caller the same API and left a caller that wants
other values to render its own spec and pass it, as FeCode does. FeCode
does, and to do it keeps its own: types of a service, a job, an addon,
a route and a secret, which began as FeRun's and have fewer fields; a
parser and its findings; a reader of YAML; a client of the API and
types for its answers, written by hand; and copies of what FeRun
allows, the plans by name and the most replicas a service may have, the
latter with another number than FeRun's. One word differs too: FeCode
writes `idle: never`, which FeRun takes for an attached service alone
(ADR 0042) and refuses for a service from an image, where the absence
says the same.

Each copy was right when it was written. A field the spec gains reaches
the copy's users only when someone adds it there as well, and until
then the caller's file cannot say what FeRun can do. Findings come from
two judges in two wordings, the caller's before the plan and FeRun's in
it. The spec's types, the parser and the answers live under the module's
`internal` path, which no other module can import, so a copy is the
only thing a caller in Go could have.

The spec is strict JSON, because JSON is what the API speaks and what a
deployment stores (ADR 0032). A file in a repository is YAML nearly
everywhere, for its comments, and a caller that keeps one reads YAML by
rules of its own.

## Decision

### FeRun publishes the format

- FeRun's repository has a **Go module of its own for callers**, apart
  from `ferund`'s and with no dependency of `ferund`'s, holding three
  things:
  - the **spec**: its types as the API takes and answers them, the
    strict parser (an unknown field, a wrong type, a duplicate key and
    trailing data are error findings with their path), the reader of
    YAML (below), and the merge patch of a spec document (RFC 7396),
    with which a caller lays its own values over a spec;
  - the **answers** of the tenant class: an environment's status, a
    plan with its findings and its diff, deployments, the catalog, the
    quota, and the finding itself, `{severity, path, problem, remedy}`;
  - a **client** of the tenant class over them: the routes, the header
    that names a tenant, and the two answers of a refusal (ADR 0031).
- `ferund` uses the same module. There is one definition and no
  published copy of it.

### Judging stays behind the plan

- Validation, normalisation, reach and the diff are not published. They
  need the catalog, the environment's secrets, the tenant's limits and
  the other environments, and `ferund` is the only judge (ADR 0032): a
  judge compiled into a caller would be an older `ferund` that
  disagrees with the running one.
- A caller parses a spec to read and to change it, and asks the plan
  whether it holds. The parser's findings are the ones `ferund` gives
  for the same document.

### A spec is JSON or YAML

- Plan, with an environment or without one, apply and clone take a
  spec as JSON or as YAML, told apart by the request's content type.
  YAML is read as the JSON it stands for and then parsed as JSON is,
  so every rule and every finding's path is the same.
- The reading is strict and plain: one document; a key is its text and
  a mapping holds it once; a scalar has the type YAML 1.2's core schema
  gives it, so `on`, `no` and `yes` are strings and only `true` and
  `false` are booleans; aliases are followed and merge keys refused.
  Each refusal is an error finding with its path.
- FeRun keeps no YAML. A deployment stores the normalised spec as JSON,
  every answer is JSON, and the builder's text stays JSON (ADR 0032).
  The YAML text, with its comments and its order, is a file of the
  caller's.

### `never` is every runtime's word

- `idle: never` is valid for every service: it stays awake as long as
  it runs. For a service from an image it says what leaving `idle` out
  says, and normalisation leaves it out, so two specs that differ only
  in it are one deployment. For an attached service it means what
  ADR 0042 gives it.

### What a caller may write, it asks

- The addons' types, versions, plans, options and outputs are the
  catalog's answer. The bounds of a spec, the most replicas of a
  service among them, and the tenant's limits are what the plan's
  findings say, with the value that is allowed in the remedy.
- A caller keeps no list of plans and no bound of its own, and gives no
  finding for what the plan judges. A form reads the catalog; a check
  is a plan.

### Fields are added, never given a new meaning

- A field of the spec or of an answer keeps its name, type and meaning.
  New fields are added; a field leaves only through a release in which
  it is still read.
- The module's exported names are part of FeRun's API. A caller
  compiled against it finds a change that breaks it when it builds, not
  when it runs.

## Alternatives considered

- **Every caller its own types**, the way FeCode took: the format
  drifts with every change on either side, and each drift shows as a
  feature one of the two cannot express or a finding the two word
  differently.
- **The spec passed through as an untyped document**, changed by path
  and never parsed by the caller. Nothing to compile against: what a
  caller reads from it, a service's ports, an image, is again knowledge
  of the format kept privately, and a caller cannot say before the plan
  why a document is no spec.
- **A JSON Schema or an OpenAPI document, and types generated from
  it.** Another description to keep true to the Go types, and it says
  nothing of the parser's strictness or of the findings' words. It
  serves callers that are not Go; it is in the backlog.
- **Validation published too**, so that a caller judges offline. It
  needs what only `ferund` has, and two judges of two builds give two
  answers; the plan is one call.
- **The published packages inside `ferund`'s module**, under a path
  outside `internal`. A caller's module would then list every
  dependency of `ferund`'s as its own.
- **YAML as the caller's business**, converted before the call, as it
  was. Every caller reads it by its own rules, and the rules matter: a
  job's key `on` is `true` to a YAML 1.1 reader. One reader is the
  format's.
- **YAML kept and answered.** A normalised spec has lost the comments
  and the order that made the text worth keeping, and a second stored
  form would need a rule for which of the two is the deployment.
- **Values per environment kept by FeRun**, a patch laid over every
  spec it is given: what FeCode's environments have. One caller wants
  it and has the merge to do it with; it is in the backlog.

## Consequences

- The spec's package is cut along its imports: types, parser and merge
  move to the published module; validation, normalisation, reach, the
  diff and the HTTP secrets' checks stay in `ferund`, which they need.
  The API's answer types move from `ferund`'s handlers to the module.
- A YAML parser becomes a dependency, of the published module alone;
  the fixed list of dependencies gains it. `ferund` gets it only
  through that module.
- FeCode drops its types, its parser, its reader of YAML, its client
  and its answer types (FeCode's ADRs 0038 and 0039) and builds against
  FeRun's module, fetched from FeRun's repository.
- A caller compiled against an older module refuses a field FeRun
  gained since as an unknown one, until it is built again. A caller
  states which FeRun it needs, and the two are released in that order.
- `idle: never` in a spec for an image service stops being an error.
- A customer's repository can hold its environment as a YAML file and
  send it with one call; the console imports JSON as before.
- `docs/architecture/control-plane.md` describes the module, the
  content types and the YAML reader's rules as the code has them.

## History

Proposed on 2026-11-21, together with FeCode's ADR 0038, which makes
`fecode.yaml` a spec of this format; nothing of it is built. It amends
ADR 0018, whose one API for every caller took one JSON document and
left the types to each caller, and ADR 0042, whose `never` was an
attached service's word alone. ADR 0032 keeps its JSON text and its
only judge.
