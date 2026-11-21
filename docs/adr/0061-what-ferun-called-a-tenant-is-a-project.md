# ADR 0061: What FeRun called a tenant is a project

**Status:** Proposed · **Date:** 2026-11-21

FeRun has three levels: an **account**, which is charged; a
**project**, which is the border; and an **environment**, which runs.
The unit that a person makes, that has its people and their roles, its
zone, its limits and its environments, and that was called a tenant, is
called a project, everywhere FeRun names it. The word tenant goes and
is given to nothing else.

## Context

Tenant was the right word while a tenant was all that a customer had:
one name, one token, one border. Since ADR 0031 a person makes several,
one for each thing they build, and since ADR 0059 each of them is on an
account that is charged for it.

To most readers a tenant is the customer: who signs up and who pays.
That is the account now. What FeRun calls a tenant is one piece of that
customer's work, and a reader meets the word one level too low.

The account is no tenant in FeRun's sense either. It has no border, no
names and no limits, and it gives no role in what is on it (ADR 0059).
So the word does not move up. It fits neither level.

The unit has a name where people come from: a project, on an account
that is billed, is a shape they know. It is also what FeCode calls the
same thing, since a project of FeCode's is a tenant (FeCode's
ADR 0037), and FeCode shows FeRun's objects in FeRun's words and with
FeRun's views (FeCode's ADR 0039). With two words for one thing, one
screen says both.

## Decision

### Account, project, environment

- An **account** is whom the operator charges (ADR 0059). A **project**
  is on one account and is what a tenant was: people make it and hold
  roles in it (ADR 0031), and its names, its zone, its domains, its
  tokens, its limits and its workloads' identities are its own. An
  **environment** is in one project.
- The three are levels of belonging and not of naming. A project is
  named by itself and never under its account, in the API, in its
  hosts and in its workloads' identities, so that it moves to another
  account as it is (ADR 0059).
- The border is where it was. One project is kept from another as one
  tenant was, and an account neither joins nor divides the projects on
  it.
- Tenant names nothing in FeRun, the account included. That an
  installation serves many projects of many accounts on shared nodes is
  said in those words.

### One word, everywhere, at once

- The word changes in what people read: the console, the docs, messages
  and errors.
- It changes in what programs call: the API's routes, the request
  header that names a project, the fields of answers, the name of a
  project's token, and the words of the spec's module (ADR 0057) and of
  the sink.
- It changes in a workload's identity, whose path names its project
  (ADR 0014).
- It changes in the code and in the tables.
- No old word is answered beside the new one. FeRun changes with the
  two programs that call it, FeCode and FeChat, in one release.

## Alternatives considered

- **Tenant stays**, and project stays FeCode's word for it. Two words
  for one thing in one screen and in one conversation, and a word that
  points at the wrong level once accounts exist.
- **The account becomes the tenant**: the border moves up to who pays.
  The projects of one account would share names, limits and people,
  which is what FeCode's one tenant for every project was (ADR 0056),
  and who pays would decide who may act (ADR 0059).
- **The word changes on screen alone**, and routes, header and
  identities keep tenant. Every page of the docs that shows a call
  would say both, for good, and every caller would map one to the
  other.
- **Old routes and the old header answered beside the new for a
  time.** A second surface to test and to remove, for callers that are
  two programs in the same hands. Backlog, for an installation with
  callers of its own.
- **Another word.** Workspace is a person's working copy in FeCode.
  Space and organisation come as a pair, and FeRun has no organisation.
  Application is what a project runs, and it may run several.

## Consequences

- The rename goes through ferund, the sink, `ferun-auth`, the console,
  the docs, FeCode and FeChat, with a migration that renames tables and
  columns. It is one release, made in the window in which FeCode's
  projects move (FeCode's ADR 0037), so that nothing moves twice.
- A workload's identity changes its path. Certificates are short-lived
  and issued anew, and what admits a workload by its identity, an
  addon's roles among them (ADR 0038), is set anew by the deployment
  that follows.
- The ADRs say project from the commit that renames the code. A word
  changes in them and no decision. Until then they say tenant, the ADRs
  proposed with this one included.
- FeCode's project and FeRun's are one word for one thing, and no
  sentence has to translate between them.
- An installation of someone else's meets the new routes and header
  with the release that brings them. Tokens stay valid.

## History

Proposed on 2026-11-21, together with ADR 0056, ADR 0059 and ADR 0060;
nothing of it is built. Malte asked whether accounts, once they exist,
are not the tenants. They are in the sense of who is served and
charged, and they are not in the sense of what is kept apart, which is
why the word goes and does not move. That the rename is whole and at
once is his decision, and follows how release became deployment
(ADR 0018).
