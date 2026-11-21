# ADR 0056: An integration is a principal that works in the tenants whose admins grant it a role

**Status:** Proposed · **Date:** 2026-11-21

Software in front of FeRun that works in many tenants, as FeCode
develops and operates one application per tenant, is an
**integration**: a principal of its own, registered once by an
operator, that signs in as itself with the shared provider's client
credentials. It reaches a tenant only by a grant that the tenant's
admin gives it, with the roles people have, and it is out when the
grant goes. It makes and deletes no tenant. Whatever its role, it never
decides who else has access: members, grants and tenant tokens are not
its to change. `write` then works inside a tenant, and `admin` decides
what the outside sees of it.

## Context

ADR 0031 gave people tenants of their own and left software two ways
in. A tenant token is a principal of one tenant, which fits a program
that serves one tenant, a team's pipeline or a script. An operator's
token reads and changes every tenant of the installation.

A platform that serves many customers has neither. FeCode took the
first and put every project into its one tenant, and drew the projects'
borders there again, one feature at a time: it names every environment
by its project, maps domain claims to projects because the rule between
tenants does not separate them, filters the sink's streams by
environment, answers the ingress as one access provider for every
project, and shares one set of limits and one daily quota of the sink
among all of them. What keeps one project from another's environment is
a comparison in FeCode's handlers; the token behind them is `admin` on
all of it.

The tenant per customer exists: people make tenants, as many as the
operator allows, and each is on an account that is charged for it
(ADR 0059). So a tenant begins at FeRun, with a person who answers for
it, whether or not a platform is ever put in front. What is missing is
a way for software to work in a tenant that is not its own, on the word
of the people the tenant belongs to: they let it in, see that it is in,
say how far, and put it out again.

Two things shape that way. Some of a platform's users work in the
platform alone and are to have no access at FeRun, so the platform
cannot act with its users' rights. And a platform acts without a person
in front of it, a poller, a build, an agent that answers an incident,
so there is not always a person to act for.

The shared provider knows such software already. FeCode, FeRun and
FeChat are its clients (FeCode's ADR 0020), and a client gets a token
for itself there, without a person.

## Decision

### An integration is a principal of its own

- An **integration** is software that works in tenants on their admins'
  word: a platform in front of FeRun, a pipeline that serves many
  teams, a tool that reads what tenants run. It is the third kind of
  principal, beside a person and a tenant token (ADR 0031).
- An operator registers it, through the admin API and on the operators'
  pages: a name, which is unique and shown wherever it acts; a title
  and a URL that tell the people who grant it what it is; the issuer
  and subject its tokens carry; and, where it admits visitors, the
  address at which it answers for them (below).
- A registration reaches no tenant. An integration has no session and
  no console, is no member of a tenant and on no account (ADR 0059),
  takes no invitation, and makes and deletes no tenant.
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
- An integration never acts in a person's name. It holds no token of a
  person's, and what it does for one of its users it does itself, by
  its own grant. Its users need no grant at FeRun, and no person's
  access at FeRun is in software's hands.
- A call names its tenant in the request header, as a session does. A
  tenant that gave the integration no grant is an unknown tenant to it.
- It reads the tenants that granted it, in the answer a session gets
  about itself: each tenant with its role there and the environments it
  holds a role on.
- Its one credential is its client's secret or key at the provider,
  which the provider rotates. FeRun keeps no secret of an integration.

### A tenant's admin grants it

- An admin gives an integration a grant as a person has one (ADR 0031):
  `read`, `write` or `admin`, on the tenant or on one environment, and
  its role on an environment is the higher of the two. There is no
  invitation: the admin picks the integration from the registered ones.
  An admin of the scope changes and removes the grant, with effect at
  once.
- Every view of who has access to a tenant shows its integrations
  beside its people and tokens, with their roles and who gave them.
- ferund records the integration as the principal of what it changes,
  beside the free `author` it sends, in which a platform names its own
  user or the agent that acted.
- Who of an integration's own users may do what within the grant is the
  integration's to decide. FeRun knows the integration, never its
  users.

### An integration acts and never decides who acts

- Whatever its role, an integration changes no principal of a tenant:
  no member, no invitation, no tenant token, and no grant on the tenant
  or an environment. Of its own grants it may give one up and nothing
  more. It reads what every member reads: the tenant with its limits
  and its use, and its members with their roles, each person with
  issuer and subject, so that software on the same provider finds its
  own users among them.
- So it cannot raise its role, let anyone else in, or mint a key that
  outlasts it. `admin` means for an integration what it means for a
  person without that part.
- A sandbox token and a route token it mints are its own keys and go
  with its access: each is refused once the integration holds no role
  that could mint it, by a grant removed or lowered, by an operator's
  disabling, or with its registration.
- What it made stays the tenant's: environments, deployments, secrets,
  sandboxes and running jobs go on and are the tenant's people's to
  keep or stop.
- An integration's `admin` is not the admin a tenant always keeps
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
- A tenant's admin therefore says how far an integration goes with one
  word:
  - `read`: it reads state, deployments, logs, traces and traffic;
  - `write` on the tenant: it also makes, clones and deletes
    environments, and in each one plans and deploys, reverts, sets
    secrets, runs jobs and sandboxes, restarts, and mints route tokens;
  - `admin` on an environment: it also publishes that one, `public`
    with its access and host label, and binds domains to it;
  - `admin` on the tenant: it also claims and deletes domains and sets
    the tenant's access providers;
  - whatever its role, never: members, invitations, grants, tenant
    tokens, the tenant's account, making or deleting a tenant.

### An integration answers for its own visitors

- An integration registered with a visits address is an access provider
  (ADR 0054) in every tenant that grants it, under its own name and
  without being made there. An environment of such a tenant names it
  as it names a provider of kind `tenant`.
- The integration reads, accepts and rejects the challenges of those
  environments as itself, whatever its role in the tenant, and ferund
  asks its address at a renewal as it asks a tenant's service.
- Its name is reserved as `ferun` is: no tenant makes a provider of a
  registered integration's name, and no integration is registered under
  a name a tenant's provider holds.
- The provider goes with the grant. An environment that still names it
  admits no visitor until its admin names another.

## Alternatives considered

- **A tenant token per tenant, minted by an admin and pasted into the
  software.** It works as things are and stays right for a program with
  one tenant. For a platform it is a secret on a clipboard for every
  tenant, kept there for good and rotated one by one; FeRun cannot say
  which software holds it; and a token that is `admin` mints tokens and
  manages members.
- **The software makes the tenant in a person's name** and is answered
  an admin token of it: the people's route taking an access token of
  the provider, bound to that route by its audience. The second draft
  of this ADR. The tenant would begin at the software, when it begins
  at FeRun, on an account; the software would hold a tenant's whole
  `admin`, principals included; and every tenant needed a browser's
  round trip to the provider.
- **The software as the maker and holder of tenants**, a product
  registered by an operator. The first draft. Its tenants would be
  nobody's: no person to count them for, to answer for their cost or to
  take them out of the software's hands, and one credential would be
  `admin` on all of them.
- **Acting in the user's name**, each call with a token of the person
  in front of the software. Every user would need a grant at FeRun,
  which is what users of the platform alone are not to have; the
  platform's own rules, an approval or a protected environment, would
  have to be said in FeRun's roles; and its work without a person would
  have no name to act in.
- **An operator's token in the software.** It reaches every tenant of
  the installation. A flaw in the software would be a flaw of the
  platform.
- **Roles or a list of permissions of an integration's own.** A second
  vocabulary beside the roles, to be decided again for every route. One
  rule, no principals, and one role moved, the route tokens', leave the
  roles as they are for everyone.
- **`admin` with principals for an integration.** It could mint a
  tenant token and stay after its grant is gone, and let in whom it
  likes.
- **A credential that FeRun mints for an integration**, as it mints
  tenant tokens. A long-lived secret that spans tenants, in two places.
  The provider keeps clients already, rotates their secrets, takes a
  key in place of one, and is where an installation's owner sees and
  stops its software.
- **Route tokens left with `admin`, and whoever makes an environment
  its admin.** Every person with `write` on a tenant could then publish
  what they make. Or the integration always `admin`: a tenant's admins
  could not keep publishing in their own hands.
- **One tenant per platform**, the rule ADR 0031 left in place.
  Reversed, because the platform then rebuilds what a tenant is, and
  the border between two of its customers is its own check alone.

## Consequences

- ferund gains the integrations with their routes in the admin API, a
  kind of principal in a grant, and a bearer that is the provider's
  signed token, verified with the keys it already reads and without a
  new dependency. The routes that change principals refuse an
  integration by its kind, and the routes' role list and its test say
  so. Route tokens move to `write` on the environment; route tokens and
  sandbox tokens record who minted them. The members' list gains each
  person's issuer and subject.
- The tenant answer the sink asks for (ADR 0026) is given for an
  integration as for a session: the tenant its header names, its role
  there and the environments it reads.
- The provider has to issue a client an access token for itself, signed
  and with FeRun's API as its audience. FeCode ID does for a client set
  up for it, which is the token form of FeCode's ADR 0034 without its
  registration of clients and its page of consent; a company's provider
  is set up to.
- A tenant's page of access lists integrations beside members and
  tokens. Adding one names its role and says in a line what `write`
  and `admin` let it do.
- An integration with `write` on a tenant reaches every environment of
  it, production among them, since a tenant's grant reaches them all.
  What stands between its users and production are the integration's
  own rules. An environment that a tenant's grant does not reach is
  backlog.
- `author` is the integration's word. FeRun vouches for the
  integration, not for the person it names.
- A flaw in an integration reaches the tenants that grant it, as far as
  their admins said, and nothing else: no other tenant, no principal,
  and nothing that outlasts the grant.
- FeCode becomes an integration, and every project a tenant (FeCode's
  ADR 0037): names, domain claims, streams, quotas and limits are then
  the tenant's, and FeCode maps none of them. FeChat keeps its one
  tenant and its token, and tenant tokens change in nothing.
- `docs/architecture/control-plane.md` describes the third principal,
  its sign-in and what it never does. Tests cover a token for another
  audience, an expired one, a subject that is no integration, a tenant
  without a grant, each route of principals, and a minted token after
  the grant is gone.

## History

Proposed on 2026-11-21, together with ADR 0059 and FeCode's ADR 0037;
nothing of it is built. It amends ADR 0031 (the kinds of principal, and
the role that mints route tokens), ADR 0043 (who mints a route token)
and ADR 0054 (a provider that is an integration's).

Two drafts of the same day were set aside. The first made such
software a principal that made and held its tenants; the second let it
make a tenant in a person's name and gave it an admin token of that
tenant. Both began the tenant at the software. They gave way once it
was said that FeRun is the base people come to first, with tenants on
accounts, and a platform like FeCode an addition, some of whose users
are never to act at FeRun themselves. That FeCode reaches no tenant
but those it is let into was Malte's condition from the start.
