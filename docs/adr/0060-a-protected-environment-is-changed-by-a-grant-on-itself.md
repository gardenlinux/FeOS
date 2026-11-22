# ADR 0060: A protected environment is changed by a grant on itself, and by an integration only for a person

**Status:** Accepted · **Date:** 2026-11-21

An environment can be **protected**. A grant on the project then reads
it and no longer changes it: whoever changes it holds a grant on the
environment itself, or is `admin` of the project. An integration
(ADR 0056) changes it only for a person it names, never by itself. It
is one switch on an environment and no new role. With it a project keeps
production out of the hands of everyone who merely works in the project,
the software it let in among them, whichever way they come.

## Context

A principal's role on an environment is the higher of its grant on the
project and its grant on that environment (ADR 0031). `write` on the
project therefore changes every environment, production as it changes a
scratch environment made an hour ago. A project that wants fewer hands
on production can give nobody `write` on the project, and then nobody
but an admin makes an environment.

Integrations make it pressing (ADR 0056). A platform needs `write` on
the project for its work: it makes environments, runs builds and opens
sandboxes. With that it reaches production too, and it acts by itself,
on a push or with an agent.

FeCode answered this with a rule of its own: an environment's
protection, and approvals for what protection held back (FeCode's
ADR 0032). The rule held for what went through FeCode. A person at
FeRun's console and a project token passed by it. Who may change an
environment is FeRun's question, and one answer has to hold for the
console, a token and a platform alike.

## Decision

### Protected is a property of an environment

- An environment is **protected** or it is not, and it is not unless
  set. Whoever is `admin` on the environment sets and lifts it. Every
  answer and view of an environment says it.
- A clone is a new environment and starts unprotected, whatever its
  source is.

### A project's grant reads a protected environment

- On a protected environment a principal's role is the higher of its
  grant on that environment and what its grant on the project counts for
  there: `admin` as `admin`, `write` and `read` as `read`.
- This holds for every principal alike: a person, a project token, an
  integration.
- So a principal with `write` on the project and no grant on the
  environment reads it: its objects, deployments and plans, its logs,
  traces and traffic, the names of its secrets. It does nothing that
  `write` on an environment gives (ADR 0031): no deployment and no
  revert, no secret's value, no restart, no run of a job, no sandbox
  and no route token.
- A plan changes nothing and is not held back: whoever could plan
  against the environment were it not protected plans against it.
- Deleting a protected environment takes `admin` on it. `write` on the
  project, which deletes other environments, does not suffice.
- Cloning from it stays what it is, `write` on the project: it reads the
  source and changes nothing of it.
- Who visits a protected environment is its access's matter and not
  this one's (ADR 0054).

### An integration changes it only for a person

- A call of an integration that changes a protected environment names a
  person (ADR 0056), and the integration and the person each hold the
  role for it there, by the rule above. A call that names nobody reads
  and plans there and changes nothing, whatever the integration's
  grant.
- Naming a person for such a change says that this person asked for
  this change themselves. What a platform's agent wants changed there,
  in whoever's session, the platform holds back until a person takes it
  up.
- A project's admin thereby has three settings for a platform that
  holds `write` on the project, and production. With no grant on it,
  the platform reads it and plans against it. With `write` on it, the
  platform deploys there for the people who hold `write` on it too.
  With `admin` on it, it also publishes it and binds its domains for
  the people who are `admin` there.

## Alternatives considered

- **The platform's own rule**, FeCode's protection and approvals as
  they were. It holds for one way in: the project's console and its
  tokens pass by it, and FeRun cannot show or check it.
- **No `write` on the project for anyone**, and a grant on each
  environment. Making an environment needs `write` on the project, and
  every new environment would need its grants one by one, for people
  and for a platform that makes an environment per workspace.
- **A fourth role**, a releaser between `write` and `admin`, or a list
  of permissions. A second vocabulary, decided again for every route.
  A grant on one environment is the releaser.
- **Protected means `admin` alone changes it.** Whoever releases to
  production would also publish it, bind its domains, delete it and
  give grants on it.
- **An approval kept by FeRun**, a change that waits for a second
  person. That is a queue with a state, a notice and a page, which a
  platform in front has. Who may is FeRun's; the wait is the
  platform's. Two people for one change is backlog.
- **No integration in a protected environment at all.** A platform's
  pipeline would end before the environment it is there for, and every
  release to production would be made by hand at the console.
- **An integration that changes it by itself where it holds a grant on
  it**: an agent that reverts production at night. It is wanted, and it
  takes more than a switch: what it may do there, how often, and who
  hears of it. Backlog.
- **Protection by a name**, every environment called `prod`. A rule
  that depends on spelling.

## Consequences

- ferund gains one property of an environment and one rule where a
  role on an environment is computed. The routes' role list and its
  test gain the protected case for each kind of principal, with the
  plan as the one exception and the deletion as the stricter one.
- The console shows protection on an environment's page and wherever
  environments are listed. The dialog that protects one names who
  changes it by a project's grant today, people, tokens and
  integrations, and offers each a grant on it, since from then on they
  read it.
- A project token with `write` on the project, a pipeline's, stops
  deploying to an environment that becomes protected until it holds a
  grant on it.
- A person with `write` on the project still makes environments, works
  in them and plans against production. What they want changed there,
  someone with a role on it changes.
- An integration's reach into production is the project's admin's
  decision, per environment. The person named is the integration's word
  (ADR 0056): protection keeps an honest platform's agent out, and it
  does not stop a flawed platform that holds a grant there from naming
  a person. Who does not trust a platform that far gives it no grant on
  the environment.
- FeCode's protection becomes this property, and its approvals the
  wait in front of it (FeCode's ADR 0040).
- `docs/architecture/control-plane.md` describes protection where it
  describes roles.

## History

Proposed on 2026-11-21, together with ADR 0056 and FeCode's ADR 0040. It amends ADR 0031 (a principal's role on an
environment, and who deletes one).

It began as FeCode's protection (FeCode's ADR 0032) and as a backlog
line of ADR 0056, an environment that a project's grant does not reach.
It moved here once a platform's users were the project's people with
FeRun's roles: a rule about who may change an environment then had one
place to be.

Accepted on 2026-11-22. Built on the branch ferun-layer by parallel
builders against one contract and merged into master as 562ee9e5; live
on EKS in Frankfurt the same day, in one maintenance window: ferund's
migrations 0041 to 0043 and the sink's 0009 ran once, and FeCode
(c50a3df) moved its thirteen projects into FeRun projects of their own.
After the move the three production environments FeCode had made subject
to approval (agentwasm, ironcore-dashboard, kino) were protected.
