# ADR 0054: Visitors: a guarded environment admits whom its tenant's access provider vouches for

**Status:** Proposed · **Date:** 2026-11-12

A public environment admits `anyone` or `visitors`, and one that is
published without saying admits visitors. Who a visitor is, and whether
they may enter, is not FeRun's to know: an access provider of the
environment's tenant vouches for them, by answering through `ferund`'s
API a challenge the ingress handed the browser. `ferund` records the
answer as a visit, and the ingress keeps it in a cookie on the host the
visitor opened: a token of `ferund`'s for 15 minutes that the ingress
checks by itself and renews over its connection, so that after the
sign-in neither `ferund` nor the provider is in a request's path. At
each renewal `ferund` asks the provider whether it still vouches, so
nobody has to tell FeRun that a visitor may no longer enter. A provider
is the
tenant's own service, which knows its people by itself, or `ferun-auth`,
a program of its own beside `ferund`: an OpenID Connect proxy for
FeRun's people and for any issuer a tenant names. `ferund` serves no
visitor.

## Context

A public environment is open to everyone: the ingress authorizes
nothing but a route's `from` blocks (ADR 0021). A stage, a demo or a
work in progress is public from the moment it has a host, and the only
way to close it is not to publish it, which leaves it without a host.

Three callers want it closed in three ways. A platform built on FeRun
gives its projects environments, and wants a project's environments open
to the project's members: FeCode is one tenant for all its projects, and
its people, its projects and its sign-in are its own, not FeRun's
persons and grants (ADR 0031), even where both use one issuer. A tenant
people made wants its stage open to the people who work on it, who hold
grants. And an application that has no sign-in of its own wants one in
front of it, with whatever issuer its owner uses.

An environment's hosts have nothing in common. Its default host lies
under the apps zone or the tenant zone, where cookies are host-only
(ADR 0021), and its domains are its tenant's (ADR 0045). A cookie shared
between hosts is not possible and not wanted, and an issuer takes no
redirect URI per domain a tenant binds tomorrow.

The ingress serves from what it has through an outage of `ferund`
(ADR 0021). A check that asks someone on every request would end that,
and would put whoever is asked into the path of every application.

`ferund`'s front door serves tenants and operators: the API and the
console. A sign-in for visitors is a page open to anyone on the
internet, and an OpenID Connect proxy fetches from addresses that
tenants name.

## Decision

### An environment admits anyone or visitors

- A public environment has an **access**: `anyone`, or `visitors`
  together with the name of one of its tenant's providers. It is set
  where the environment is made public, by the same right. A publish
  that does not say admits visitors of the provider `ferun`; to open an
  environment to everyone is a statement.
- Being public stays what gives an environment its hosts and opens its
  routed ports to the ingress (ADR 0021, ADR 0045); the access says who
  may open them. It holds on every host of the environment alike, and
  for everything the ingress serves there.
- The ingress checks in this order: the route, the route's `from`, the
  visit, and only then the service. A client outside a route's blocks
  gets 403 before anyone is asked to sign in, and a visitor nobody
  vouched for wakes no pod.
- A request without a valid visit is sent to the provider only when it
  is a navigation. Every other request, a script's or an API client's,
  gets 401: a redirect would be followed into a sign-in page nobody
  sees.
- The path `/.ferun/` is the ingress's on every host, before any route.
  There the browser comes back from a provider, a visitor signs out, and
  the keys for the visit's token are published.

### The challenge is the ingress's, the answer the provider's

- For a navigation without a visit, the ingress makes a **challenge**
  by itself: the environment, the host, the path to return to and an end
  minutes away, sealed with a key `ferund` gives the instances. It sends
  the browser to the provider's address with the challenge and nothing
  else, and sets a short-lived cookie on the host that ties the
  challenge to this browser.
- The provider cannot open a challenge and need not. It shows it to
  `ferund`, which opens it and says which environment and host are asked
  for. Nothing a browser can change names them.
- The provider signs the visitor in by its own means, applies its own
  rule, and **accepts** the challenge at `ferund` with a subject, what it
  wants the application to know of them, an end, and the session of its
  own the visit hangs on, where it has one, which it is told again when
  it is asked about the visit; or it rejects it.
  `ferund` takes an answer only from a provider the environment names,
  and each challenge once. Its answer is where to send the browser: the
  host in the seal, under `/.ferun/`, with a code that carries `ferund`'s
  signature.
- The ingress redeems a code at `ferund` over its connection, only with
  the challenge's cookie beside it, and only once. A code that arrives
  in another browser signs nobody in. A rejected challenge is answered
  403 on the host. A challenge past its end still names its host, so a
  provider whose sign-in took longer sends the browser there to begin
  again.
- A visitor nobody vouched for costs `ferund` nothing: no call and no
  row. Its first caller in a sign-in is a provider with a credential.

### The visit: a record at ferund, a short token on the host

- A **visit** is `ferund`'s record that a provider admitted a subject
  to an environment until a time. The end is the provider's, within a
  bound of the installation's.
- The ingress sets the visit's cookie on the host the visitor opened and
  on no other, host-only and unreadable to scripts, and takes it out of
  the request before the application sees it. The cookie holds a token
  `ferund` signed for 15 minutes, which the ingress checks with keys
  from the table, without a call.
- When a token nears its end, the ingress asks `ferund` for the next
  over its connection, on any request, so that a page that stays open
  does not fail.
- **A visit is put to its provider again at every renewal.** `ferund`
  gives the next token while the visit lives and its provider still
  vouches for it, and ends the visit on the provider's no. So a token's
  15 minutes are how long a visitor stays after their provider would no
  longer admit them, and there is no call by which a tenant ends a
  visit: what the provider knows is asked, not copied.
- A provider that does not answer has not said no: the visit gets its
  next token and is put to the provider again at the following renewal,
  up to its end.
- A visit ends at its time; on its provider's no; when the visitor
  signs out on the host; when its environment is unpublished or names
  another provider. An ended visit gets no next token, and the ingress
  closes its upgraded connections with it. Signing out on a host ends
  the visit, not the session at the provider.
- At a visit's end the next navigation goes through the provider again.
  A provider that still knows the visitor answers without asking them.
- While `ferund` is away, the ingress serves the visits it holds: a
  token past its end counts until its visit's end. What was admitted
  stays admitted while the control plane is away, and nobody new is.

### A provider is the tenant's own service, or ferun-auth

- A **provider** is a tenant's object with a name and a kind. An
  environment names one provider; the rule that says who is admitted is
  the provider's, not the environment's.
- Kind **`tenant`**: the tenant's own service at an address. It reads,
  accepts and rejects challenges with a token of the tenant that
  administers the environment. At a renewal `ferund` calls the same
  address and names the visit; the service reads the visit as it reads
  a challenge, with its environment, subject and session, and answers
  yes or no. `ferund` dials that address as it dials a binding's
  upstream for a preflight (ADR 0028): never an address of the
  cluster's or the nodes' networks. FeRun signs nobody in and knows
  nothing of the service's people; nothing is kept in step between the
  two. This is the kind for a platform on FeRun: FeCode admits a
  project's members.
- Kind **`oidc`**: an OpenID Connect issuer the tenant names, with a
  client and its secret, kept sealed, and a rule over the ID token's
  claims: everyone the issuer signs in, or those of an email, an email
  domain or a group. `ferun-auth` runs the sign-in. Nothing is asked
  again at a renewal: the visit lives to its end, or until the issuer's
  back-channel logout names its session.
- Kind **`ferun`**, which every tenant has without making it: the
  installation's own issuer, and the rule is a grant of at least `read`
  on the environment (ADR 0031). `ferun-auth` runs the sign-in, and
  `ferund` looks the grant up itself, at the accept and before every
  next token.
- An installation without `ferun-auth` serves providers of kind `tenant`
  alone, and refuses a publish that asks for another.

### ferun-auth: a program of its own beside ferund

- `ferun-auth` is built from the Go module beside `ferund` and shipped
  in the same image. It is the provider for the kinds `ferun` and
  `oidc`, and nothing else: no API for tenants, no access to the
  database, no node connection. It runs as identical instances that hold
  no state: a sign-in in flight lives in a sealed cookie on its host,
  and the long session is the issuer's.
- It has a host of its own, where the platform's hosts are served, never
  a path of `ferund`'s host. That host carries the one redirect URI an
  issuer needs for the installation, whatever hosts and domains its
  environments have, and takes the issuers' back-channel logouts, which
  end the visits of a session.
- To `ferund` it is a caller with a service token of its own kind. With
  it, it reads and answers challenges of environments whose provider is
  of kind `ferun` or `oidc`, reads those providers with their secrets,
  and ends visits by an issuer's session. It reaches nothing else of a
  tenant.
- It fetches from an issuer only at addresses of the public internet:
  not the cluster's networks, not private or link-local ranges, checked
  on the address it connects to. Blocks an operator names are the
  exception, for an issuer in a private network.

### What the application receives

- On a request of a visit the ingress sets the visitor's subject, their
  email where the provider gave one, and the visit's token in headers of
  FeRun's own, and drops those headers from every client's request, on
  every host, whatever the environment's access. The route door drops
  them too.
- The token names the visit, the subject, what the provider said of
  them, and the environment as its audience. An application that is
  called only through the ingress may take the headers as they are; one
  that other workloads of its environment reach, or that passes the
  identity on, checks the token.
- The issuer's tokens stay in `ferun-auth`. FeRun tells an application
  who is there; it gives it nothing to act for them elsewhere.

## Alternatives considered

- **`ferund` signs visitors in with its own issuer**, and a tenant
  pushes the list of who may open each environment. A platform's people
  are not FeRun's, the list would have to be kept in step with every
  change of membership, and `ferund`'s sign-in would stand in front of
  every tenant's application.
- **A signed answer carried through the browser**, a ticket the
  provider signs and the ingress verifies. Every provider would hold a
  signing key and every environment its public keys, with their
  rotation; and the environment and host would be parameters of a URL,
  bound only by what the provider remembers to put under its signature.
  A challenge read and answered over the back channel needs neither.
- **A challenge stored at `ferund` before the provider is asked.** Every
  anonymous request to a guarded host would be a call to the control
  plane and a row in its database.
- **Asking on every request**, the provider or `ferund`, as a
  forward-auth call. A visit would end at once, and whoever is asked
  would be in the path of every request; the ingress would stop serving
  when they are away.
- **The visit in the cookie alone**, a token valid for hours and no
  record. Nothing could end it before its time.
- **A visit of 15 minutes and no renewal**, the browser going through
  the provider again each time. Only a navigation can be sent there: a
  page that stays open would have its requests refused every quarter of
  an hour, and a form sent after a pause would be lost.
- **The tenant ends visits through the API**, when a member is removed,
  signs out or is disabled. Every place that changes who may enter would
  have to call, and a call that fails leaves the visit to its end.
  Asking again finds out whatever changed, without the tenant's doing.
- **One cookie for a zone**, so that a sign-in holds on every host under
  it. An application could read and send another's visit, the zones'
  cookies are host-only for that reason (ADR 0021), and a tenant's
  domains share no parent anyway.
- **An OpenID Connect client per host.** An issuer wants every redirect
  URI registered and takes no wildcard; a domain bound tomorrow would
  need its tenant to go to the issuer first.
- **The OpenID Connect proxy in `ferund`'s front door.** `ferund` would
  serve a page to anyone on the internet and fetch from addresses that
  tenants name, from where the database and the nodes' port are. It
  keeps to tenants and operators.
- **`ferun-auth` as a workload on FeRun**, in an environment of the
  operator's: a microVM and an egress rule from the platform instead of
  a check of its own. FeRun has no way to deploy its own parts onto
  itself, and would need one for this alone.
- **A proxy in every tenant's deployment**, a container in front of each
  application. Every tenant would run and configure one, per
  environment, behind an ingress that already ends TLS and knows the
  host.
- **The platform's forwarder in front of its environments**, as FeCode
  has for previews. It would stand in the data path of applications
  again, which ADR 0021 took it out of.
- **Guarding every environment that is public today.** What is live and
  open would close without its tenant's doing.

## Consequences

- The ingress checks a visit where it authorized nothing (ADR 0021). It
  still decides nothing about people: it holds no list and no rule, only
  what the table gives it, the access of each environment, its
  provider's address and the keys.
- `ferund` keeps providers, visits and the keys for challenges and
  tokens, answers providers on its API and the ingress on its
  connection, and serves no browser for any of it.
- FeRun has a program more, with a host and a service token of its own.
  It parses what the internet and issuers send it, and what it can do
  when taken over is bounded by its token: admit visitors to
  environments of the kinds it serves, never to those whose rule is a
  grant without one, and read their issuers' client secrets.
- A platform on FeRun serves one address, for the browser with a
  challenge and for `ferund` with a visit, and reads and answers both
  at the API. It tells FeRun nothing when a membership ends.
- A removed member stays for up to 15 minutes. While their provider or
  `ferund` is away they stay until one is back, to the visit's end at
  most; an application's visitors are not turned away because the
  platform that admitted them is being deployed.
- `ferund` calls an address a tenant names, once per visit and quarter
  of an hour, bounded as its preflights are.
- An environment that is public when this is built stays open to
  everyone; its tenant closes it.
- A guarded environment answers machines 401. One that takes webhooks or
  serves an API beside its pages stays `anyone` and guards itself.
- An application gets the visitor's identity without a sign-in of its
  own, which is what makes the gate a proxy.

## History

Proposed on 2026-11-12. It takes up ADR 0021's question of an
authorization at the ingress, for environments; a workbench's previews
stay behind its forwarder.
