# ADR 0063: A project has its own label, its environments `<environment>--<project>`

**Status:** Accepted · **Date:** 2026-11-22

A project's name is a host label of its own in the zone it is in:
`<project>.<zone>` is the **project's address**, and one environment of
the project at a time holds it. Every other public environment's
default host is `<environment>--<project>.<zone>`. A label a project
chooses ends in `--<project>`. So every host under a zone names its
project, the project's address is short, and the forms never meet. This
amends ADR 0045's labels: the natural label `<environment>-<project>`
and the plain labels of the zone's owners go.

## Context

ADR 0045 gave every public environment the natural label
`<environment>-<project>`, kept `--` for labels a project chooses, and
let only the zone's owners take plain labels, names without `--`. A
plain label for every project would have read as another project's
natural label: `shop-acme` is environment `shop` of project `acme`, and
nothing would have kept project `shop-acme` from holding it.

A project wants an address of its own: `kino.apps.fecore.dev`, not
`prod-kino.apps.fecore.dev`. FeCode had that while it was the apps
zone's owner and named its projects' hosts itself (FeCode ADR 0009); since
FeCode is a layer on FeRun (FeCode ADR 0037) it chooses no label, and its
"project's address" moves no host (FeCode ADR 0037 waits for this ADR).
A domain gives a production an address, but a project that has no name
of its own should not need one for its first.

The cause is the single hyphen. With `--` between the environment and
the project, the natural label has a form no plain project name has,
and the project's own name can be its label.

## Decision

### Labels in a zone

A label under a zone has one of four forms, and each names its project:

- The **project's label** is the project's name, `<project>`: the
  project's address. It is the project's for as long as the project
  exists, and at most one of its environments holds it. A project whose
  name is no label of this form — it has a dot or a `--`, is no DNS
  label, or ends as a fallback does — has no project's label.
- The **natural label** is `<environment>--<project>`, given when it
  names one pair: the environment's name has no `--`, the project has a
  project's label, and the whole is a DNS label of at most 63 characters
  that is not R-LDH (its third and fourth characters are not `--`, so an
  environment of two characters gets the fallback). It is cut at its one
  `--`: left the environment, right the project.
- A **chosen label** is `<choice>--<project>`, the project's own name
  after the `--` and a label part without `--` before it. A project
  chooses one for an environment when it publishes, and may change it.
- The **fallback** stays as ADR 0045 has it: the start of the
  environment's name and a hash of its ID.

No two forms meet: a project's label has no `--`, a natural or chosen
label exactly one, with its own project after it, and the fallback ends
in its hash. A chosen label can be a natural label of the same project
only, for an environment the project may still make; that environment
then gets its fallback, as one whose label is held does now. Labels stay
registered to their project for good, as ADR 0045 decided.

### The project's address

- An environment takes the project's address with `PUT
  /api/v1/environments/{env}/public` and `hostLabel` the project's name.
  The environment that held it gets its natural label (or its fallback)
  in the same transaction: there is one project's address, and taking it
  moves it. Giving it up is a `hostLabel` of the environment's natural
  label, of its fallback (its own, where its name gives no natural
  label) or of a chosen one.
- An environment's view says whether it holds the project's address
  (`projectAddress`), beside its `hostLabel` and hosts.
- While a domain of that environment is served, the project's address
  answers 404, as every default host does (ADR 0045). A domain stays the
  address that does not move with a decision inside the project.
- The console shows the project's address in an environment's settings,
  with the environment that holds it now.

### The labels given before

- At the release, every environment whose label is the natural label of
  ADR 0045 gets the natural label of this ADR, and its old host answers
  404 (ADR 0045: a changed label moves the host at once). The old label
  stays its project's.
- A label of a project that is gone, held by no environment, that
  names a project in this ADR's forms (`<project>` or
  `<choice>--<project>`) becomes that project's first. FeCode's
  environments ran in its project `fecode` until FeCode became a layer
  (ADR 0056), and the labels FeCode chose there are the hosts of the
  projects they moved to: `dev` of `hb-baxxter` gets `dev--hb-baxxter`,
  and `kino` its address.
- A chosen label from before stays where it is; a new choice follows
  this ADR.
- The zone's owners and their plain labels go
  (`--apps-zone-owners`): no zone has one, and a plain label of the
  platform's would take a name a project may want. A plain label held
  now is kept and moves nowhere.

## Alternatives

- **The project's name as a label for every project, the natural label
  as it is**: `shop-acme` would be both project `shop-acme`'s address and
  environment `shop` of project `acme`; one of them would get a
  fallback, by who came first.
- **`<project>--<environment>`**, the order FeCode's names had in its
  one project (FeCode ADR 0022): the same uniqueness, but a host is read
  from the left, and what differs between a project's hosts is the
  environment.
- **A discriminator on the project's label** (`p--<project>`,
  `<project>-app`): unique, and no shorter than the host it replaces.
- **Old natural labels kept as a second host**: a redirect or a second
  default host per environment for links from before, for projects that
  are still demos; an environment has one default host, and a domain is
  the address that stays.

## Consequences

- A host under a zone tells its project: `<project>` or
  `…--<project>`. The cookie scope is still the zone's (ADR 0045), so
  the project's address shares cookies with every other project's hosts
  as before.
- Every public environment from before moves its host once, at the
  release, from `<environment>-<project>` to
  `<environment>--<project>`; links to the old hosts end. Domains do not
  move.
- FeCode's project's address (FeCode ADR 0037) is this label: FeCode
  takes and gives it up at FeRun for the person, and reads which
  environment holds it from FeRun. FeCode keeps no copy of it; its
  `project_host` column goes a release later.
- A project whose name has a dot or a `--` has no project's address and
  no natural or chosen labels; its environments get fallbacks, and a
  domain gives one an address.
- The labels name a project by its name, as hosts do (ADR 0062).

## History

Proposed on 2026-11-22, after a project's address at
`<project>.apps.fecore.dev` answered `no such host` since FeCode's move
onto FeRun (FeCode ADR 0037).

Accepted on 2026-11-22 and live in eu-central-1 with ferund of FeOS
98e9e59f and FeCode be5bb7d: migration 0044 moved every environment's
host, gave the labels FeCode chose in its gone project `fecode` to the
projects they name, and `hb-baxxter.apps.fecore.dev` is the project's
address of `hb-baxxter`.
