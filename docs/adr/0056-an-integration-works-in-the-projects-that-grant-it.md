# ADR 0056: An integration is a principal that works in the projects whose admins grant it a role

**Status:** Proposed · **Date:** 2026-11-21

Software in front of FeRun that works in many projects, as FeCode
develops and operates one application per project, is an
**integration**: a principal of its own, registered once by an
operator, that signs in as itself with the shared provider's client
credentials. It reaches a project only by a grant that the project's
admin gives it, with the roles people have, and it is out when the
grant goes. It makes and deletes no project. A call it makes for a
person names that person and does what both may do: the lower of the
two roles decides. Whatever its role and whomever it names, it never
decides who else has access: members, grants and project tokens are not
its to change. `write` then works inside a project, and `admin` decides
what the outside sees of it.

## Context

ADR 0031 gave people projects of their own and left software two ways
in. A project token is a principal of one project, which fits a program
that serves one project, a team's pipeline or a script. An operator's
token reads and changes every project of the installation.

A platform that serves many customers has neither. FeCode took the
first and put all of its projects into its one FeRun project, and drew
their borders there again, one feature at a time: it names every environment
by its project, maps domain claims to projects because the rule between
projects does not separate them, filters the sink's streams by
environment, answers the ingress as one access provider for every
project, and shares one set of limits and one daily quota of the sink
among all of them. What keeps one project from another's environment is
a comparison in FeCode's handlers; the token behind them is `admin` on
all of it.

The project per customer exists: people make projects, as many as the
operator allows, and each is on an account that is charged for it
(ADR 0059). So a project begins at FeRun, with a person who answers for
it, whether or not a platform is ever put in front. What is missing is
a way for software to work in a project that is not its own, on the word
of the people the project belongs to: they let it in, see that it is in,
say how far, and put it out again.

Two things shape that way. A platform's users are the people of the
projects it works in. A platform that kept a list of them, with roles of
its own, would give a project two answers to who may do what, and FeRun
could not see the platform's. So what a platform does for a person is
bounded by that person's role at FeRun, and the platform needs no roles
of its own. And a platform acts without a person in front of it, a
poller, a build, an agent that answers an incident. There is not always
a person to act for, so it needs a role that is its own.

The shared provider knows such software already. FeCode, FeRun and
FeChat are its clients (FeCode's ADR 0020), and a client gets a token
for itself there, without a person.

## Decision

### An integration is a principal of its own

- An **integration** is software that works in projects on their admins'
  word: a platform in front of FeRun, a pipeline that serves many
  teams, a tool that reads what projects run. It is the third kind of
  principal, beside a person and a project token (ADR 0031).
- An operator registers it, through the admin API and on the operators'
  pages: a name, which is unique and shown wherever it acts; a title
  and a URL that tell the people who grant it what it is; and the
  issuer and subject its tokens carry.
- A registration reaches no project. An integration has no session and
  no console, is no member of a project and on no account (ADR 0059),
  takes no invitation, and makes and deletes no project.
- Every signed-in person reads the registered integrations: name, title
  and URL.
- An operator disables an integration, which ends the effect of every
  grant at once and brings it back when enabled, as for a person, or
  removes it, and its grants go with it.

### It signs in as itself

- An integration is a client of the shared provider and gets an access
  token there for itself, by the client credentials grant: signed with
  the provider's keys, with this installation's API as its audience and
  the registered subject. ferund checks the signature against the keys
  it reads for ID tokens, the issuer, the audience and the expiry, and
  finds the integration by issuer and subject.
- A token whose subject is no registered integration opens nothing, a
  person's access token among them. This is not the door for a person's
  agents (backlog, ADR 0031).
- It holds no token of a person's. Whom a call is made for it says with
  the call (below), and the most it can do for anyone is what its own
  grants give.
- A call names its project in the request header, as a session does. A
  project that gave the integration no grant is an unknown project to it.
- It reads the projects that granted it, in the answer a session gets
  about itself: each project with its role there and the environments it
  holds a role on.
- Its one credential is its client's secret or key at the provider,
  which the provider rotates. FeRun keeps no secret of an integration.

### A project's admin grants it

- An admin gives an integration a grant as a person has one (ADR 0031):
  `read`, `write` or `admin`, on the project or on one environment, and
  its role on an environment is the higher of the two. There is no
  invitation: the admin picks the integration from the registered ones.
  An admin of the scope changes and removes the grant, with effect at
  once.
- Every view of who has access to a project shows its integrations
  beside its people and tokens, with their roles and who gave them.

### A call names the person it is made for

- A call an integration makes for a person names that person in a
  request header, as it names its project: by their subject at the
  provider that signs the integration's own tokens. It names nobody of
  another issuer. A call that names nobody is the integration's own: a
  poller, a build, an agent with no one in front of it.
- For a call that names a person, the role on every scope is the lower
  of two, the integration's and the person's. The call does what both
  may do. Nothing an integration does for a person goes beyond its own
  grant, and nothing beyond that person's.
- A named person whom FeRun does not know, one who is disabled, and one
  who holds no role where the call goes are answered as that person
  would be: the project, or the environment, is unknown.
- Asked with a person named, the answer about itself lists the projects
  in which both hold a role, and for each the two roles, on the project
  and on its environments. A platform reads there what a person sees
  and may do, and keeps no members and no roles of its own.
- ferund records the integration as the principal of what it changes,
  the person it named, and the free `author` it sends, in which a
  platform names the session or the agent that acted.
- Naming a person is the integration's word. FeRun holds the
  integration's signature and no proof that the person asked. A named
  person therefore narrows a call and never widens it.

### An integration acts and never decides who acts

- Whatever its role and whomever it names, an integration changes no
  principal of a project: no member, no invitation, no project token, and
  no grant on the project or an environment. Of its own grants it may
  give one up and nothing more. It reads what every member reads: the
  project with its limits and its use, and its members with their roles,
  each person with issuer and subject, so that software on the same
  provider finds its own users among them.
- So it cannot raise its role, let anyone else in, or mint a key that
  outlasts it. `admin` means for an integration what it means for a
  person without that part.
- A sandbox token and a route token it mints are its own keys and go
  with its access: each is refused once the integration holds no role
  that could mint it, by a grant removed or lowered, by an operator's
  disabling, or with its registration.
- What it made stays the project's: environments, deployments, secrets,
  sandboxes and running jobs go on and are the project's people's to
  keep or stop.
- An integration's `admin` is not the admin a project always keeps
  (ADR 0031).

### Route tokens are `write`'s

- Minting and revoking an environment's route tokens needs `write` on
  the environment, for every principal.
- Whoever has `write` deploys the environment's code, sets its secrets
  and opens a sandbox that reaches its services (ADR 0012). A route
  token reaches its HTTP routes through ferund's door (ADR 0043) and
  nothing else; it gives `write` no reach it lacks. `admin` keeps what
  shows an environment to others: `public` with its access and host
  label, its domains, its grants and its deletion.
- A project's admin therefore says how far an integration goes with one
  word:
  - `read`: it reads state, deployments, logs, traces and traffic;
  - `write` on the project: it also makes, clones and deletes
    environments, and in each one plans and deploys, reverts, sets
    secrets, runs jobs and sandboxes, restarts, and mints route tokens;
    a protected environment it reads (ADR 0060);
  - `admin` on an environment: it also publishes that one, `public`
    with its access and host label, and binds domains to it;
  - `admin` on the project: it also claims and deletes domains and sets
    the project's access providers;
  - whatever its role, never: members, invitations, grants, project
    tokens, the project's account, making or deleting a project.

## Alternatives considered

- **A project token per project, minted by an admin and pasted into the
  software.** It works as things are and stays right for a program with
  one project. For a platform it is a secret on a clipboard for every
  project, kept there for good and rotated one by one; FeRun cannot say
  which software holds it; and a token that is `admin` mints tokens and
  manages members.
- **The software makes the project in a person's name** and is answered
  an admin token of it: the people's route taking an access token of
  the provider, bound to that route by its audience. The second draft
  of this ADR. The project would begin at the software, when it begins
  at FeRun, on an account; the software would hold a project's whole
  `admin`, principals included; and every project needed a browser's
  round trip to the provider.
- **The software as the maker and holder of projects**, a product
  registered by an operator. The first draft. Its projects would be
  nobody's: no person to count them for, to answer for their cost or to
  take them out of the software's hands, and one credential would be
  `admin` on all of them.
- **An integration that acts by its own grant alone**, for users FeRun
  never hears of. The third draft. The platform then keeps members and
  roles of its own: a second answer to who may do what in a project,
  which holds only for who comes through the platform, and rules for
  production that the project's own console passes by.
- **The person's own token on every call**, in place of a name. FeRun
  would have proof that the person is there. The platform would hold
  its users' tokens for FeRun's whole API and keep them fresh for work
  that goes on after the person left, an agent's turn or a build.
  Naming is weaker and says so; a proof for the changes that need one
  is backlog.
- **The person's role alone**, or the higher of the two. The
  integration would act beyond what the project's admin granted it.
- **An integration as an access provider**, which admits its own users
  as an environment's visitors (ADR 0054). Its users are the project's
  people, and the provider `ferun` admits those. A platform whose
  visitors are others is backlog.
- **An operator's token in the software.** It reaches every project of
  the installation. A flaw in the software would be a flaw of the
  platform.
- **Roles or a list of permissions of an integration's own.** A second
  vocabulary beside the roles, to be decided again for every route. One
  rule, no principals, and one role moved, the route tokens', leave the
  roles as they are for everyone.
- **`admin` with principals for an integration.** It could mint a
  project token and stay after its grant is gone, and let in whom it
  likes.
- **A credential that FeRun mints for an integration**, as it mints
  project tokens. A long-lived secret that spans projects, in two places.
  The provider keeps clients already, rotates their secrets, takes a
  key in place of one, and is where an installation's owner sees and
  stops its software.
- **Route tokens left with `admin`, and whoever makes an environment
  its admin.** Every person with `write` on a project could then publish
  what they make. Or the integration always `admin`: a project's admins
  could not keep publishing in their own hands.
- **One project per platform**, the rule ADR 0031 left in place.
  Reversed, because the platform then rebuilds what a project is, and
  the border between two of its customers is its own check alone.

## Consequences

- ferund gains the integrations with their routes in the admin API, a
  kind of principal in a grant, a bearer that is the provider's signed
  token, verified with the keys it already reads and without a new
  dependency, and the header that names a person, with the lower of
  two roles taken where a role is computed. The routes that change
  principals refuse an integration by its kind, and the routes' role
  list and its test say so. Route tokens move to `write` on the
  environment; route tokens and sandbox tokens record who minted them.
  The members' list gains each person's issuer and subject, and the
  record of a change the person it was made for.
- The project answer the sink asks for (ADR 0026) is given for an
  integration as for a session: the project its header names, its role
  there and the environments it reads, bounded by the person's where
  the call names one. The sink passes that header on as it passes a
  session's.
- The provider has to issue a client an access token for itself, signed
  and with FeRun's API as its audience. FeCode ID does for a client set
  up for it, which is the token form of FeCode's ADR 0034 without its
  registration of clients and its page of consent; a company's provider
  is set up to.
- A project's page of access lists integrations beside members and
  tokens. Adding one names its role and says in a line what `write`
  and `admin` let it do.
- A platform in front of FeRun keeps no people. Who its users are in a
  project and what each may do are the project's grants, and every one of
  its users has signed in at FeRun once, to open an invitation, and may
  work there directly with the same role.
- `write` on a project changes every environment of it but a protected
  one, which it reads. An integration changes a protected environment
  only for a person, whatever its grant (ADR 0060).
- The person named and `author` are the integration's word. FeRun
  vouches for the integration and for the bound, not for the person's
  asking.
- A flaw in an integration reaches the projects that grant it, as far as
  their admins said, and nothing else: no other project, no principal,
  and nothing that outlasts the grant. Within that it can name whom it
  likes, so a grant is given to software that is trusted that far.
- FeCode becomes an integration, every FeCode project a FeRun project
  and a FeCode project's people the FeRun project's (FeCode's ADR 0037
  and ADR 0040): names, domain
  claims, streams, quotas, limits, members and roles are then the
  project's, and FeCode maps none of them. Its access provider goes,
  since the provider `ferun` admits the same people (ADR 0054). FeChat
  keeps its one project and its token, and project tokens change in
  nothing.
- `docs/architecture/control-plane.md` describes the third principal,
  its sign-in, how it names a person and what it never does. Tests
  cover a token for another audience, an expired one, a subject that is
  no integration, a project without a grant, a named person with the
  lower role, with none and unknown, each route of principals with an
  admin named, and a minted token after the grant is gone.

## History

Proposed on 2026-11-21, together with ADR 0059 and FeCode's ADR 0037;
nothing of it is built. It amends ADR 0031 (the kinds of principal, and
the role that mints route tokens) and ADR 0043 (who mints a route
token).

Three drafts of the same day were set aside. The first made such
software a principal that made and held its projects; the second let it
make a project in a person's name and gave it an admin token of that
project. Both began the project at the software, and gave way once it was
said that FeRun is the base people come to first, with projects on
accounts, and a platform like FeCode an addition. That FeCode reaches
no project but those it is let into was Malte's condition from the
start.

The third (ff307feb) had the integration act by its own grant alone,
for users FeRun never knew, and answer for them as an access provider.
Malte saw in a second layer of rights in the platform the duality this
change was to end: FeRun's three roles are the roles of a platform's
users. Naming the person, and the lower of two roles, is how they are.
