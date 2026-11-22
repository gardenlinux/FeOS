# ADR 0059: A project is on an account, which says whom its use is charged to

**Status:** Proposed · **Date:** 2026-11-21

Every project is on one **account**. An account is whom the operator
charges: a name, a reference into the operator's own accounting, and
the people who may put projects on it. A person makes a project on an
account they are a member of, and a project moves to another account as
it is: who is charged changes from that moment, and nothing else of it.
FeRun measures and prices nothing by this decision. It settles whose a
project's use is, so that whatever is counted later has someone to go
to.

## Context

A project is made by a person, has limits, and counts toward the number
of projects its maker may hold (ADR 0031). Nothing says who pays for it.
The number per person keeps one person from taking too much; it is no
statement about cost.

Whoever pays is seldom the person who made the project. An installation
inside a company charges what its teams use to their cost centres or to
an order; one that serves customers charges a customer's account. Both
are something several people share, that holds several projects, and
that stays when a person leaves or changes teams.

Who pays also changes while a project runs. A team is reorganised, a
budget ends, a trial becomes a product with a cost centre of its own,
an application is handed to another team. The application does not
stop for that, and its addresses do not change.

Software in front of FeRun needs the same answer and has none of its
own. FeCode works in projects that exist, by a grant (ADR 0056), and
what it runs for one of its projects, its builds and its workspaces'
sandboxes, runs in that project at FeRun. If the project says whom its use is
charged to, a platform in front needs no accounts, no prices and no
billing; if it does not, every platform invents them.

## Decision

### An account is whom the operator charges

- An **account** has a name, formed as a project's name is and unique; a
  title; and a **reference**, free text that the operator's accounting
  understands: a cost centre, an order, a customer's number. FeRun
  keeps and shows the reference and reads nothing into it.
- Operators make, change and close accounts, through the admin API and
  on the operators' pages. A closed account takes no new project and
  keeps the ones it has. An account that has projects is not deleted.
- Every installation has the account `default`. An installation that
  charges nobody has it alone and behaves as it did without accounts.

### An account's people

- An account has **members**, who may put projects on it, and among them
  **admins**, who also invite and remove its people. They are people: a
  project token and an integration (ADR 0056) are on no account.
- An operator adds an account's people from those FeRun knows. An admin
  of the account invites with ADR 0031's invitation, a single-use link
  whose scope is the account and whose role is member or admin.
- Being on an account gives no role in its projects. An account's people
  see the projects on it: each one's name, maker and admins and its use
  against its limits, and nothing inside it.
- Everyone who may make projects at all is a member of `default`. Its
  admins are the operators.

### A project is on one account

- A request that makes a project names its account, and its maker has to
  be a member of it. A request that names none means `default`.
- The rules for making a project stay ADR 0031's: its name, its zone,
  and the number of projects a person may hold as their maker. The
  operator's choice whether people make projects at all is the choice
  whether `default` is open to everyone; a member of another account
  makes projects on it either way.
- A project an operator makes through the admin API is on the account
  the operator names, else on `default`.
- Every member of a project reads which account it is on, by name and
  title. The reference is for the account's people and the operators.

### A project moves to another account

- A project moves to another account at any time. An admin of the
  project who is a member of the account that receives it moves it, or
  an operator. The account it leaves is not asked: a cost leaves it.
- The account that receives it is open. A closed account takes no
  project, moved or new, and a project leaves a closed account as it
  leaves any other.
- A move changes whom the project's use is charged to, from its moment
  on, and nothing else. The project keeps its name, its zone and hosts,
  its domains, its people and their grants, its tokens, its limits and
  its workloads' identities. Nothing restarts and no request fails.
- So that this holds, the account is in nothing that names a project or
  what is in it: no name, no host, no route of the API, no identity of
  a workload, no key in the sink and no path in storage. A project's
  name is unique in the installation and not within its account.
- ferund keeps a project's accounts over time: each with the moment the
  project came to it and who moved it. Use belongs to the account the
  project was on when it happened. A move is not dated back.
- The project's members, the people of both accounts and the operators
  read the move: from which account to which, when, and by whom.
- An integration moves no project, whomever it names (ADR 0056).

### Nothing is measured or priced here

- FeRun counts no use for an account, has no prices and writes no
  invoice by this decision. What it counts already, a project's use
  against its limits, is shown per account and no more.
- What this decision fixes is the address: at every moment every project
  is on exactly one account, and the account names what the operator's
  accounting charges.

## Alternatives considered

- **The account in a project's address**: a name that is unique within
  the account, routes and identities that begin with the account, as
  three levels suggest. A move would rename the project: new hosts, new
  certificates and identities for every workload, and callers that
  break. Who pays changes more often than what an application is
  called.
- **A project fixed on its account**, and a new project for a new payer.
  A change of cost centre would be the migration of a running
  application, with its data and its domains.
- **A move that both accounts agree to.** The account that is left has
  nothing to refuse: it stops paying. The one that receives agrees by
  who moves, a member of it.
- **A move offered by the project's admin and taken by one of the
  receiving account's people**, for an admin who may not spend there.
  Until it exists that account's admin makes the project's admin a
  member, or an operator moves the project. Backlog.
- **The maker pays.** A person is no cost centre: they leave, change
  teams, and share a budget with others. A project would have to change
  hands whenever its maker does.
- **A reference on the project alone**, free text its admin sets. Anyone
  could charge any cost centre, and nobody would have agreed to it. The
  account is where someone who answers for the cost says who may spend
  on it.
- **An organisation above projects**, whose admins are admins of its
  projects. It mixes who pays with who may act. A project's grants stay
  the project's (ADR 0031); who pays for it needs no way into it.
- **Accounts from the provider's claims**, a cost centre read from the
  company's directory at sign-in. It binds FeRun to one provider's
  claims, and a person has one department and may spend on several
  budgets. It may fill an account's members one day; it does not
  replace the account.
- **Accounts in the platform in front**, FeCode's. A project used
  without the platform would have none, and one used with it two.
- **People make accounts themselves.** An installation that serves the
  public needs it, with a means of payment behind each account. Inside
  a company the operator knows the cost centres; that comes first, and
  the other is backlog.
- **Limits of an account** in place of the person's number: how many
  projects, how much use. Backlog, with the meter it needs.

## Consequences

- ferund gains accounts and their people, a project's account as a
  history of moves and not as one field, the routes for both in the
  admin API and for an account's people, and a scope more for
  invitations. Every existing project is on `default`, and a move brings
  each to the account that pays for it.
- The console offers a person's accounts where a project is made, names
  the account on a project's page and moves it there, and has a page per
  account for its people: members, projects, those that came and left,
  their use against their limits.
- A meter, prices and a report per account's reference can follow
  without a change to who is charged (backlog). The meter reads a
  project's accounts over time: a project that moved within a month is in
  that month's report of both accounts, each for its part. Until then
  an operator reads the projects of an account and their use and charges
  by hand.
- What is built for a project later, a store, a queue, a report, names
  the project and looks its account up. Whatever carried the account in
  a name or a key would break the move.
- A platform on FeRun keeps no accounts. FeCode's projects are FeRun projects
  on the accounts of those who made them, and what FeCode runs for a
  project is that project's use (FeCode's ADR 0037). FeCode itself is
  not what is charged.
- Whoever pays cannot yet stop a project: an account's admin asks the
  project's admins or an operator (backlog).
- An installation that charges nobody changes in nothing.
- `docs/architecture/control-plane.md` describes accounts, and the
  operator's reference documents the setting that opens `default`.

## History

Proposed on 2026-11-21, together with ADR 0056; nothing of it is built.
It takes up ADR 0031's backlog line on billing as far as the account.
Measuring, prices and reports were left out at Malte's word: they are
straightforward once every project has an account, and the change this
ADR belongs to does not need them. It amends ADR 0031 in two places:
making a project names an account, and the operator's choice whether
people make projects is whether `default` is open to them.

The move was one line in the first writing. It was written out the
same day, when Malte asked that a project can be moved to another
account: with the rule that keeps the account out of every name, and
the history that a meter will read.
