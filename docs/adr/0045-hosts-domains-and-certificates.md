# ADR 0045: Hosts, domains and certificates of public environments

**Status:** Accepted · **Date:** 2026-10-27

Every public environment has a default host under the platform's apps
zone, with a label registered to its tenant for good: a natural label
`<environment>-<tenant>` where that names one pair, a label with `--` a
tenant chooses, or a plain label for the tenants that own the zone. A
tenant claims a domain of its own by one call and binds it to one
environment by another; the claim is proven by a TXT record of the
claim's token at a label of its own, never by the record that routes the
name, and both records are checked for as long as the domain lives, with
a grace before a wrong one deactivates it. `ferund` is the ACME client
for every domain, over HTTP-01 answered by every ingress instance, in
Let's Encrypt's short-lived profile renewed at half life; the zones'
wildcards are the operator's. While a domain of an environment is
served, its default host is not.

## Context

The ingress (ADR 0021) serves hosts and needs to know which names are
whose. A public environment wants an address from the moment it is
public, without a tenant setting up DNS; a production wants its owner's
own name, with DNS at a provider that can carry an A record or a CNAME.
The first such production is a dashboard at `dashboard.fecore.dev`.

Every tenant's domains point at the same edge. A CNAME to the edge's
name, or A records with its addresses, says "the platform serves this
name", not "this tenant does": a claim proven by routing would give a
name to whoever asked first, and give it again to anyone once its owner
removed the domain and left the CNAME in DNS, or kept a wildcard there.
An apex behind an ALIAS or a flattened CNAME answers with A records, so
not even a routing target per claim would show whose it is. A caller
that is one tenant for many owners, as FeCode is for its projects, needs
a proof between them.

A proof must hold for as long as it is used. A claim that stayed
verified without its record would go on holding a name its owner gave up
or lost, and the platform would answer on that name for the old tenant,
and renew its certificate, whenever its DNS pointed here again. But a
record deleted by mistake, a DNS provider's hour of trouble or a zone
moved to another provider must not take a production off the internet
at once.

Default hosts under one zone share a cookie scope. A natural label
`<environment>-<tenant>` with the tenant's dots as hyphens reads the
same for different pairs: `shop` of `acme.io`, `shop` of `acme-io` and
`shop-acme` of `io`; whoever published first would keep the label, so
another tenant could hold the host that reads as yours. A tenant that is
a product of its own, with one tenant per project, wants to name its
hosts: a production at `<project>.<zone>` and every other environment at
`<environment>--<project>`.

The certificates of the platform's own hosts come from the cluster's
issuer. The domains' cannot: a tenant's DNS is the tenant's, and an ACME
client of a few hundred lines is cheaper than tying the platform to
cert-manager on the one path every customer uses.

## Decision

### Zones and default hosts

- The platform has an **apps zone**, whose wildcard DNS points at the
  ingress. Every public environment has one default host, `<label>.<apps
  zone>`, given at its first publish, and the label is registered to its
  tenant for good: a label an environment gives up stays its tenant's,
  and one another tenant or environment holds is refused. Without an apps
  zone no environment can be made public. The tenants people make have a
  second zone with the same rules (ADR 0031).
- The **natural label** is `<environment>-<tenant>`, given only when it
  names one pair: the environment's name has no hyphen, the tenant's
  name no dot and no `--`, and the label does not end as a fallback
  does. Every other environment gets the **fallback**, the name's start
  and a hash. A host that reads as `<environment>-<tenant>` is therefore
  that tenant's, or the zone owner's.
- A tenant may **choose** its environment's label when it publishes, and
  change it later: a DNS label with `--` in it, which a natural label
  never has, so that the forms stay apart; what stands left and right of
  the `--` is the tenant's own. The tenants the operator names as the
  apps zone's **owners** may also choose a **plain** label without `--`,
  as a platform holds the names under its own domain; a plain label that
  is the natural label of an existing tenant's environment is refused,
  so that no existing tenant's natural host is ever taken, and a tenant
  made later whose natural label the owner holds gets its fallback. A
  changed label moves the host at once; the old one answers 404.
- A route's `host` is realised: a route with a host answers on that host,
  which must be a host of its environment, else the write fails with a
  finding; a route without a host answers on every host of its
  environment.
- The default host is the address of an environment without a name of
  its own. While one of its domains is served, the environment's hosts
  are its served domains, in the order they were bound, and the default
  host answers 404; when none is served any more, the default host is its
  host again, at once, under the same label. A domain is served only
  once its certificate is there, so a binding moves the environment from
  its default host to its domain without a gap.

### Domains: a claim and a binding are two facts

- A **domain** is a tenant's claim on one name, made by one call, and
  bound to at most one environment of its tenant by another. Whose a
  name is lasts as long as its owner keeps it; which environment serves
  it changes with a move, and an environment is deleted and made again.
  A domain may be claimed before anything serves it and bound before it
  is verified; it is served when it is verified, bound to a public
  environment, routed and has a certificate. A move to another
  environment is one call and keeps the verification; no deployment
  moves a domain. An environment's deletion ends its bindings and keeps
  the domains.
- A name is stored lowercased, as its A-label. A wildcard, an address, a
  name in the apps zone or the tenant zone, the edge's name and a name
  under the platform's reserved zones cannot be claimed.

### The proof is the claim's token in DNS, never the routing record

- What proves a claim is a TXT record at a label of the platform's
  beside the name (`_ferun-challenge.<name>` by default) whose value is
  the claim's token, random and made by `ferund` per claim. It is a name
  of its own because nothing else may stand beside a CNAME at the name
  itself. The record stays as long as the claim.
- **Claims are not exclusive while unverified.** Any number of claims,
  of one tenant or of several, may name the same name, each with its own
  token, so that nobody blocks a name by claiming it first; a tenant
  holds a bounded number of unverified claims. At most one claim of a
  name is **verified**: the one whose token DNS shows. If DNS shows
  several, the one verified before stays so; of new claims, the oldest
  wins.
- **A verified claim is lost to another proof at once.** When DNS shows
  another claim's token and not its own, that claim becomes verified and
  this one **superseded**: its binding ends, its host leaves the table,
  its certificate's key is deleted, and its environment gets an
  incident. Whoever controls a name's DNS decides who holds it, also
  against a holder. A record that is merely missing takes the name away
  only after the grace.
- A deleted claim ends its binding and its key. What its owner leaves in
  DNS gives the name to no other claim, since no other token is in it.
- `ferund` asks the servers of the zone that holds a record, not a
  cache, so that a record just set counts at once and no negative answer
  a resolver keeps holds a claim back; only the way to those servers
  comes through the recursive resolver. An unverified claim is checked
  with backoff from a minute to an hour, and at once on request.

### Routing gates the certificate and nothing else

- A domain is **routed** when every address its name resolves to, and
  at least one, is an address of the edge, which the operator publishes
  under one name with the balancer's addresses. A name below its zone's
  apex takes a CNAME to the edge's name; an apex takes an ALIAS or a
  flattened CNAME where its provider has one, else A and AAAA records
  with the edge's addresses. The answer to a claim names both records.
- Routing proves nothing. `ferund` orders a certificate only for a domain
  that is verified, bound to a public environment and routed, so that no
  challenge is spent on a name that cannot pass it: the CA limits failed
  validations per name.
- A domain's verification is `unverified`, `verified`, `lapsed` or
  `superseded`; a verified domain is `unbound`, `unpublished`,
  `waiting-for-dns`, `issuing`, `served`, `dns-moved`, `inactive` or
  `failed` with a readable reason (a CAA record that names another CA, a
  refused challenge, a limit of the CA), and shows since when which
  record is wrong, when it will be deactivated, and whether its checks
  fail.

### A domain is checked for as long as it lives

- `ferund` checks a verified domain's records every hour for as long as
  the claim exists, the TXT and, once the domain is bound, the routing
  record, and every quarter of an hour while one of them is wrong. Only
  an answer counts as wrong: a name or record that does not exist, a TXT
  without the token, addresses that lead elsewhere. A lookup that fails
  changes nothing and shows as failing, so that an outage of DNS or of
  the platform's own network deactivates no domain.
- The first wrong answer for a record that was right starts the domain's
  **grace**, three days by default, which covers a weekend, on the
  database's clock; the first right answer ends it. A bound domain that
  was never routed waits and has no grace to run out. During the grace
  the domain is served as before but gets no new certificate while its
  TXT is missing, and its environment gets an incident with the deadline.
- When the grace runs out the domain is **deactivated**: its host leaves
  the table, its certificate's key is deleted, nothing is renewed, and
  its environment gets an incident. Its claim and binding stay, and it is
  checked on, hourly. With its TXT missing its verification **lapses**: a
  lapsed claim counts as unverified, so another claim whose token DNS
  shows is verified from then on, and its own token, put back, verifies
  it again unless another claim was verified meanwhile. With its routing
  wrong it is **inactive** and stays verified; once its name resolves to
  the edge again, `ferund` obtains a certificate and serves it again.

### Certificates: `ferund` is the ACME client

- `ferund` obtains and renews every domain's certificate with one ACME
  account per platform. A domain's certificate comes over HTTP-01:
  `ferund` keeps the challenge in the ingress's table and every instance
  answers it for that host on port 80, so that a tenant's DNS needs the
  claim's TXT and a record to the edge, and `ferund` no credential of it.
  A challenge is accepted only once every connected instance holds it,
  since the CA validates through the balancer against any one of them.
- Certificates are ordered in Let's Encrypt's short-lived profile, six
  days, and renewed at half their life: three days of failures are what
  a six-day certificate can afford. A failed order is retried with
  backoff; a domain whose renewals fail for a day gets an incident on its
  environment. One order per domain every three days stays far below the
  CA's limits, so its renewal information is not asked. A domain whose
  name resolves elsewhere is not renewed, as HTTP-01 could not succeed.
- Private keys are made by `ferund`, sealed in the database under the key
  that seals secrets (ADR 0044), and sent to the instances over their
  connections, which keep them in memory and in their cache.
- The zones' wildcard certificates are the operator's: issued by the
  cluster's issuer or by hand, given to the instances as files, and
  renewed as the operator renews them. `ferund` holds no DNS credential.

## Alternatives considered

- **Routing as the proof**: a CNAME to the edge or A records with its
  addresses, and a TXT record only where another tenant held the name or
  had claimed it. The routing record is every tenant's, so it proves no
  tenant's claim; a CNAME left behind or a wildcard gave a name nobody
  had claimed to whoever asked, and a caller that is one tenant for many
  owners got no proof between them at all.
- **A routing target per claim**, `<id>.edge.<domain>`, in place of the
  TXT record, as some platforms do: it shows whose a name is for a
  CNAME, but not for an apex, whose ALIAS or A records show addresses
  alone.
- **Exclusive claims**, the first claimant's until it lets go: a name
  blocked by whoever asked first, and its owner asking the operator to
  take it back.
- **A verified claim kept without its record, and a moved domain served
  until its certificate expires**: a proof checked once and trusted for
  good, a name that changed owners still answered for the old tenant,
  and keys of names that no longer point here.
- **Deactivating at the first wrong answer**: a record deleted by
  mistake, a DNS provider's hour of trouble or a zone moved to another
  provider would take a production off the internet at once. The grace
  gives the incident time to reach a person.
- **Certificates in cert-manager**, read by `ferund` from the cluster:
  one more thing that works only on Kubernetes, for an ACME client of a
  few hundred lines. The zones' wildcards, which are the platform's, may
  come from it.
- **Renewal at two thirds of a certificate's life**, the usual rule for
  90-day certificates: a six-day certificate renewed at two thirds leaves
  two days for failures; half leaves three.
- **Refusing tenant names that collide after the mapping** of dots to
  hyphens, or **a discriminator on every label**: the first does not
  cover the hyphen between name and tenant, the second makes simple
  names unreadable. The natural label only where it names one pair, and
  a fallback otherwise, keeps simple names simple and every host its
  tenant's.
- **The default host served beside an environment's domains**: a second
  address of the same production, which search engines, links and
  cookies split between, and one its tenant did not ask for. Replaced on
  2026-11-01 by the default host answering 404 while a domain is served.
- **A chosen label without `--` for every tenant**: it would read as
  another tenant's natural label; only the zone's owners, whose names
  the zone is, may hold plain labels.

## Consequences

- A tenant's DNS holds two records per domain, the TXT of its claim and
  the one that routes it, and keeps both for as long as it keeps the
  domain: without either for longer than the grace, the domain is
  deactivated. A domain's state and its incidents tell the tenant which
  record is wrong and until when.
- Operators publish the edge's name and the apps zone's wildcard, open
  port 80 to the ingress for HTTP-01, and provide the zones' wildcard
  certificates. They keep the balancer's addresses: an apex names them in
  A records that are its tenant's, so a new address is a change every
  such tenant makes in its own DNS; an operator who has to change them
  names the old addresses beside the new ones until the tenants have
  moved.
- A stolen domain key is worth six days, and revocation does not matter.
  An outage of `ferund` or the CA longer than about three days leaves the
  ingress serving an expired certificate from its cache.
- A production answers on its domain alone; its default host is the
  address of an environment without one. A host under the apps zone that
  reads as `<environment>-<tenant>` is that tenant's or the zone owner's.
- The ingress makes the cookies of answers host-only (ADR 0021), not
  those a page's script sets; a Public Suffix List entry for the apps
  zone, an operator's action, closes that for every environment.

## History

Decided on 2026-10-27 within ADR 0021 and split out on 2026-11-08. Domains
with the TXT proof, the checks and the grace were built on 2026-10-29
(ace4fa5, 90503f0), their certificates the same day (f8a5f9a). Chosen
labels, the default host answering 404 while a domain is served, and
plain labels for the zone's owners came on 2026-11-01 (b122057, 7c4c105,
794a2ec); the tenant zone of ADR 0031 on 2026-11-06 (0cda975, 20f8894).
The draft's zone wildcards over DNS-01 by `ferund` were never built; the
operator's issuer provides them.
