# ADR 0059: A tenant is on an account, which says whom its use is charged to

**Status:** Proposed · **Date:** 2026-11-21

Every tenant is on one **account**. An account is whom the operator
charges: a name, a reference into the operator's own accounting, and
the people who may put tenants on it. A person makes a tenant on an
account they are a member of, and a tenant moves to another account as
it is: who is charged changes from that moment, and nothing else of it.
FeRun measures and prices nothing by this decision. It settles whose a
tenant's use is, so that whatever is counted later has someone to go
to.

## Context

A tenant is made by a person, has limits, and counts toward the number
of tenants its maker may hold (ADR 0031). Nothing says who pays for it.
The number per person keeps one person from taking too much; it is no
statement about cost.

Whoever pays is seldom the person who made the tenant. An installation
inside a company charges what its teams use to their cost centres or to
an order; one that serves customers charges a customer's account. Both
are something several people share, that holds several tenants, and
that stays when a person leaves or changes teams.

Who pays also changes while a tenant runs. A team is reorganised, a
budget ends, a trial becomes a product with a cost centre of its own,
an application is handed to another team. The application does not
stop for that, and its addresses do not change.

Software in front of FeRun needs the same answer and has none of its
own. FeCode works in tenants that exist, by a grant (ADR 0056), and
what it runs for a project, its builds and its workspaces' sandboxes,
runs in the project's tenant. If the tenant says whom its use is
charged to, a platform in front needs no accounts, no prices and no
billing; if it does not, every platform invents them.

## Decision

### An account is whom the operator charges

- An **account** has a name, formed as a tenant's name is and unique; a
  title; and a **reference**, free text that the operator's accounting
  understands: a cost centre, an order, a customer's number. FeRun
  keeps and shows the reference and reads nothing into it.
- Operators make, change and close accounts, through the admin API and
  on the operators' pages. A closed account takes no new tenant and
  keeps the ones it has. An account that has tenants is not deleted.
- Every installation has the account `default`. An installation that
  charges nobody has it alone and behaves as it did without accounts.

### An account's people

- An account has **members**, who may put tenants on it, and among them
  **admins**, who also invite and remove its people. They are people: a
  tenant token and an integration (ADR 0056) are on no account.
- An operator adds an account's people from those FeRun knows. An admin
  of the account invites with ADR 0031's invitation, a single-use link
  whose scope is the account and whose role is member or admin.
- Being on an account gives no role in its tenants. An account's people
  see the tenants on it: each one's name, maker and admins and its use
  against its limits, and nothing inside it.
- Everyone who may make tenants at all is a member of `default`. Its
  admins are the operators.

### A tenant is on one account

- A request that makes a tenant names its account, and its maker has to
  be a member of it. A request that names none means `default`.
- The rules for making a tenant stay ADR 0031's: its name, its zone,
  and the number of tenants a person may hold as their maker. The
  operator's choice whether people make tenants at all is the choice
  whether `default` is open to everyone; a member of another account
  makes tenants on it either way.
- A tenant an operator makes through the admin API is on the account
  the operator names, else on `default`.
- Every member of a tenant reads which account it is on, by name and
  title. The reference is for the account's people and the operators.

### A tenant moves to another account

- A tenant moves to another account at any time. An admin of the
  tenant who is a member of the account that receives it moves it, or
  an operator. The account it leaves is not asked: a cost leaves it.
- The account that receives it is open. A closed account takes no
  tenant, moved or new, and a tenant leaves a closed account as it
  leaves any other.
- A move changes whom the tenant's use is charged to, from its moment
  on, and nothing else. The tenant keeps its name, its zone and hosts,
  its domains, its people and their grants, its tokens, its limits and
  its workloads' identities. Nothing restarts and no request fails.
- So that this holds, the account is in nothing that names a tenant or
  what is in it: no name, no host, no route of the API, no identity of
  a workload, no key in the sink and no path in storage. A tenant's
  name is unique in the installation and not within its account.
- ferund keeps a tenant's accounts over time: each with the moment the
  tenant came to it and who moved it. Use belongs to the account the
  tenant was on when it happened. A move is not dated back.
- The tenant's members, the people of both accounts and the operators
  read the move: from which account to which, when, and by whom.
- An integration moves no tenant, whomever it names (ADR 0056).

### Nothing is measured or priced here

- FeRun counts no use for an account, has no prices and writes no
  invoice by this decision. What it counts already, a tenant's use
  against its limits, is shown per account and no more.
- What this decision fixes is the address: at every moment every tenant
  is on exactly one account, and the account names what the operator's
  accounting charges.

## Alternatives considered

- **The account in a tenant's address**: a name that is unique within
  the account, routes and identities that begin with the account, as
  three levels suggest. A move would rename the tenant: new hosts, new
  certificates and identities for every workload, and callers that
  break. Who pays changes more often than what an application is
  called.
- **A tenant fixed on its account**, and a new tenant for a new payer.
  A change of cost centre would be the migration of a running
  application, with its data and its domains.
- **A move that both accounts agree to.** The account that is left has
  nothing to refuse: it stops paying. The one that receives agrees by
  who moves, a member of it.
- **A move offered by the tenant's admin and taken by one of the
  receiving account's people**, for an admin who may not spend there.
  Until it exists that account's admin makes the tenant's admin a
  member, or an operator moves the tenant. Backlog.
- **The maker pays.** A person is no cost centre: they leave, change
  teams, and share a budget with others. A tenant would have to change
  hands whenever its maker does.
- **A reference on the tenant alone**, free text its admin sets. Anyone
  could charge any cost centre, and nobody would have agreed to it. The
  account is where someone who answers for the cost says who may spend
  on it.
- **An organisation above tenants**, whose admins are admins of its
  tenants. It mixes who pays with who may act. A tenant's grants stay
  the tenant's (ADR 0031); who pays for it needs no way into it.
- **Accounts from the provider's claims**, a cost centre read from the
  company's directory at sign-in. It binds FeRun to one provider's
  claims, and a person has one department and may spend on several
  budgets. It may fill an account's members one day; it does not
  replace the account.
- **Accounts in the platform in front**, FeCode's. A tenant used
  without the platform would have none, and one used with it two.
- **People make accounts themselves.** An installation that serves the
  public needs it, with a means of payment behind each account. Inside
  a company the operator knows the cost centres; that comes first, and
  the other is backlog.
- **Limits of an account** in place of the person's number: how many
  tenants, how much use. Backlog, with the meter it needs.

## Consequences

- ferund gains accounts and their people, a tenant's account as a
  history of moves and not as one field, the routes for both in the
  admin API and for an account's people, and a scope more for
  invitations. Every existing tenant is on `default`, and a move brings
  each to the account that pays for it.
- The console offers a person's accounts where a tenant is made, names
  the account on a tenant's page and moves it there, and has a page per
  account for its people: members, tenants, those that came and left,
  their use against their limits.
- A meter, prices and a report per account's reference can follow
  without a change to who is charged (backlog). The meter reads a
  tenant's accounts over time: a tenant that moved within a month is in
  that month's report of both accounts, each for its part. Until then
  an operator reads the tenants of an account and their use and charges
  by hand.
- What is built for a tenant later, a store, a queue, a report, names
  the tenant and looks its account up. Whatever carried the account in
  a name or a key would break the move.
- A platform on FeRun keeps no accounts. FeCode's projects are tenants
  on the accounts of those who made them, and what FeCode runs for a
  project is that tenant's use (FeCode's ADR 0037). FeCode itself is
  not what is charged.
- Whoever pays cannot yet stop a tenant: an account's admin asks the
  tenant's admins or an operator (backlog).
- An installation that charges nobody changes in nothing.
- `docs/architecture/control-plane.md` describes accounts, and the
  operator's reference documents the setting that opens `default`.

## History

Proposed on 2026-11-21, together with ADR 0056; nothing of it is built.
It takes up ADR 0031's backlog line on billing as far as the account.
Measuring, prices and reports were left out at Malte's word: they are
straightforward once every tenant has an account, and the change this
ADR belongs to does not need them. It amends ADR 0031 in two places:
making a tenant names an account, and the operator's choice whether
people make tenants is whether `default` is open to them.

The move was one line in the first writing. It was written out the
same day, when Malte asked that a project can be moved to another
account: with the rule that keeps the account out of every name, and
the history that a meter will read.
