# ADR 0043: The route door: ferund serves an environment's HTTP routes with route tokens

**Status:** Accepted · **Date:** 2026-10-18

`ferund` serves an environment's HTTP routes in a door of its own front
door: the longest route prefix of the deployed deployment picks the
service, a ready instance takes the request, and a suspended one is
woken. The door admits only a route token of that environment, minted by
its tenant and shown once, so that public traffic never travels with a
tenant's credential. The token travels in a header of its own,
`X-FeRun-Route-Token`, which stops at the door, so that the application
receives the client's `Authorization` as it was sent, unless that holds
a FeRun credential, which the door refuses rather than pass on. Routes
to attached services and to environments that are not public are the
door's alone; a public environment's image services are served by the
ingress as well (ADR 0021).

## Context

A deployment declares routes, and something has to serve them. The port
door of `ferund`'s front door already carries previews into sandboxes
over the serving instance's node connection (ADR 0015), waking a paused
sandbox for the first request. An attached service runs in a sandbox,
which no ingress reaches: sandboxes stay behind the doors (ADR 0021). An
environment that is not public has no host of its own. FeCode serves
each environment on a host of its own and forwards to `ferund`; what it
forwards with must not be a tenant's token, which would travel with
every public request, and must not displace the `Authorization` a
browser sends the application.

## Decision

- `ferund` serves an environment's HTTP routes in a **route door**, a
  path of its front door per environment, through the port door's
  machinery: the longest route prefix of the deployed deployment, the
  latest while none is deployed, picks the service and the port the
  chosen instance's own deployment gives the route's port name; a ready
  instance of an image service takes the request, round-robin, and with
  none a paused or hibernated one is woken; an attached service's
  request goes to its sandbox (ADR 0042). The route's prefix is
  stripped only when the route says so; the query stays. Hosts, streams
  and upgrades pass as the port door passes them.
- The door takes only a **route token** of that environment: minted by
  the environment's admin, shown once, kept as a hash, revoked all at
  once. A token of another environment finds nothing. A tenant's or a
  sandbox's token is refused, in whichever header it comes.
- The token travels in a header of its own, **`X-FeRun-Route-Token`**,
  which stops at the door. The client's `Authorization` reaches the
  application as it was sent, unless it carries a FeRun credential, a
  tenant's or a sandbox's token, which the door refuses rather than pass
  on. A client that sends the route token as its bearer gets no
  `Authorization` through.
- The door serves anonymous clients: a failure inside `ferund` says that
  the door failed and nothing of its cause, which is in the log.
- A route has no host of its own: the door serves every route of an
  environment under the environment's path, and the ingress serves a
  public environment's routes to image services on every host the
  environment has (ADR 0021). A plan says which doors serve each route.

## Alternatives considered

- **An ingress as the only way in.** Public traffic to image services
  needs one (ADR 0021), but it reaches no sandbox, so attached services
  would have no route, and an environment that is not public would have
  none either. The door stays beside it.
- **The route door as the ingress**, `ferund`'s instances the data path
  for every host: considered and rejected in ADR 0021.
- **The route token as the bearer token**, the door's first form. The
  door would have to strip
  `Authorization`, and the application would never see the credential
  its own client sent; a header of the platform's own leaves the
  client's alone.
- **Forwarding with the tenant's token.** One credential for every
  public request, revocable only by revoking the tenant's access; a
  route token is minted for the one purpose and revoked without
  touching anything else.
- **Passing every `Authorization` on unread.** A client that sends a
  tenant's or a sandbox's token there would hand it to the application;
  the door refuses a FeRun credential and forwards the rest.

## Consequences

- `ferund` carries application traffic in its front door: a request to a
  route goes through an instance and its node connection, with the port
  door's holds and touches, so a routed request counts as activity of
  the sandbox or instance it reaches.
- FeCode's environment hosts forward to the door with the environment's
  route token in `X-FeRun-Route-Token` and pass the browser's
  `Authorization` on to the application.
- A rename of the environment changes the door's path by name; the ID's
  path stays (ADR 0018).
- A route to an attached service is reached only through the door, so
  such a service is never public without FeCode or another forwarder in
  front.

## History

Built on 2026-10-18 with ADR 0018 (abd2c87), whose proposal had left
routes to an ingress, with the route token as the bearer; the token moved
into a header of its own on 2026-10-19 (3b047c4), and the door refuses a
FeRun credential in an `Authorization` it would pass on since the same
day (3c2851f, e46bd4f). Since ADR 0021 on 2026-10-28 (33ea94e) the
ingress serves public environments' image services beside the door.
Split out of ADR 0018 on 2026-11-08.
