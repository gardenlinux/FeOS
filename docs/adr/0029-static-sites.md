# ADR 0029: Static sites: immutable bundles, served from the object store, switched blue-green

**Status:** Proposed · **Date:** 2026-11-06

A static site is an object of an environment with no pods: its files
live in the object store as a bundle, every file a blob named by its
hash and a manifest whose digest names the version for good, and the
ingress serves them from there on the routes that name the site. A
deployment pins a site to a bundle as it pins a service to a digest, and
switches a site blue-green: the next bundle is loaded by every ingress
instance beside the live one, then made live in one version of the
table, so that no page ever mixes two versions, and the one before stays
loaded for a revert. The rules a site needs, how a missing path is
answered, redirects and rewrites, headers and CORS, are typed and live
in two places: the application's in `_ferun.json` at the bundle's root,
versioned with its files, and the environment's in the spec, laid over
them.

## Context

A static site runs on FeRun as a service: nginx in an image, in microVMs
beside the application's API on one host, with an `nginx.conf` that
holds what every single-page application needs, a fallback to
`index.html` and a long cache lifetime for the hashed assets. Every byte
of such a site is a file that exists before the first request, and yet
each replica is a pod, rolled replica by replica, paused when idle and
woken by the next visitor, a cold start for a file; each version is an
image, built, pushed, pinned and baked (ADR 0022), whose only content is
a directory; and the rules live in a web server's configuration, written
again by every project and never the same twice, the pattern ADR 0018
set out to end: an agent builds the platform it lacks, per project.

What is wanted is what static hosts do: a person or a build uploads the
built output, a zip, a tar archive or an OCI image; the platform keeps
the files in object storage; the ingress answers from there, with the
CORS headers, rewrites, redirects and cache headers a site needs.
Cloudflare Pages is the model: how a missing file is answered, how
`.html` and trailing slashes are treated, the `_redirects` and
`_headers` files, and deployments that switch at once. Versions must be
versions: each one complete and immutable in the store before it serves,
the switch one step for all ingress instances, the version before ready
to switch back to, and the history in the environment's deployments
(ADR 0018), where a revert runs the same bytes. And the rules are the
application's: with nginx in the image they were a file of the project's
repository, reviewed with the code that needs them and promoted and
reverted with it. Once the image is gone they must not turn into
settings of the platform, kept apart from the code they belong to.

The platform has deployments that pin what they run (ADR 0018); an
ingress that serves from a table `ferund` pushes and keeps through an
outage of `ferund`, as identical instances with a volume each
(ADR 0021), on hosts and domains with certificates (ADR 0045); and an
S3-compatible object store, used by the disk daemon (ADR 0011) and by
Quickwit behind the sink (ADR 0026). Neither `ferund` nor
`ferun-ingress` speaks to the store, and their dependencies are fixed:
the standard library reads zip, tar and gzip; zstd is in the module
graph through the sink's Kafka client; for xz there is nothing.

## Decision

### A site is an object of an environment

- An environment has a seventh kind of object beside ADR 0018's six, the
  **site**: a name, a **source**, and the environment's rules for its
  files; the application's rules come with the files, in the bundle. A
  site has no pods, no replicas, no readiness and no idle policy; it
  costs what its files cost in the store, and it never sleeps.
- Its source is a **bundle** the tenant uploaded before, named by its
  digest, or a directory of an **OCI image** or artifact, with image
  credentials as a service has them (ADR 0022).
- A **route** names a site or a service (`{"path": "/", "site": "web"}`
  beside `{"path": "/api", "service": "api", "port": "http"}`). The
  longest prefix picks, as for services, so an application and its front
  end share a host without a web server of their own; `strip` works as
  for a service, and a redirect to a path of the site gets the route's
  prefix back.
- Apply pins a site as it pins a service: an image to its digest, an
  uploaded bundle to itself, after checking that the bundle exists and
  is the tenant's. A deployment names the bytes each site serves, and a
  revert serves the same.
- Sites are served by the ingress, and so only in a public environment
  (ADR 0021); the route door (ADR 0043) serves none, and plan says so
  for a site route in an environment that is not public. A route to a
  site opens no port (ADR 0012); its `from` holds as on any route.

### A version is a bundle: its files by their hash, its manifest last

- A bundle is the content of one version of a site: its files by path,
  each stored as a blob named by the SHA-256 of its bytes, and a
  **manifest** that lists every path with its blob, size, content type
  and compressed variants, and the rules the bundle brings. The manifest
  is canonical JSON, sorted and without times, and the SHA-256 of its
  bytes is the bundle's **digest**: the same files give the same digest,
  so an upload is idempotent by construction, and a digest names the
  same bytes for good.
- In the store a tenant's blobs and bundles live under a prefix of the
  tenant's own. The blobs are written first, the manifest last, so that
  a manifest that exists names only blobs that exist, as a hibernation
  writes its head last (ADR 0008). A blob the tenant already has is not
  written again: a new version stores only the files it changed.
- Blobs are shared within a tenant, never across tenants. Promotion from
  one environment to the next is a deployment that names the same digest
  and costs nothing in the store. Across tenants the question "is this
  blob there already?" would tell one tenant what another holds, and a
  tenant's deletion would not be its prefix.
- `ferund` keeps every bundle and the blobs per tenant in its database,
  for the quota and the sweep; the store holds the bytes. Files, not the
  archive: the ingress reads one file per request, two versions share
  what they share, and nothing is unpacked on the request's path.

### Where a bundle comes from: an upload, or an image

- **An upload** takes an archive as its body: zip, tar, and tar
  compressed with gzip, xz or zstd, told apart by their first bytes. It
  needs `write` on the tenant, or on the environment it names, which the
  bundle records (ADR 0031); bundles are the tenant's, so that
  environments share their blobs. The call answers when the bundle is
  complete, with its digest, its files and bytes and its findings, or
  with why it failed. A tenant lists its bundles and deletes one that no
  kept deployment names.
- **`ferund` unpacks in its own process**, streaming, hashing each file
  as it is read and storing it if the tenant lacks it. The work is
  bounded: the body, the bytes unpacked, the number of files, a file's
  size and a path's length have limits, and an instance runs a bounded
  number of ingests at a time and answers 429 with `Retry-After` beyond.
  The standard library's archive readers are memory-safe, and an archive
  that unpacks to more than its limit stops at the limit, which is what
  a bomb is. An archive holds regular files and directories; a link
  inside the root becomes its target's blob, anything else is skipped
  with a finding; a path is UTF-8 without NUL, control characters or
  backslashes, has no `.` or `..` segment, and appears once.
  `_ferun.json`, `_redirects` and `_headers` at the root are read as
  rules and are not served.
- **An image** is ingested by the reconciler, in the site's rollout:
  `ferund` asks the registry with the image's credentials as before
  every create (ADR 0022), reads the manifest and the layers in order,
  applies their whiteouts, and makes a bundle of the tree below the
  path, under the same rules as an upload. One image and path become one
  bundle once, keyed by registry, repository, manifest digest, path and
  the ingest's version, so that stage and production ingest in no time
  what dev ingested already. The pull is `ferund`'s and not a node's,
  because nothing of the image runs: no pod and no bake.

### Blue-green: the next bundle is loaded everywhere, then switched at once

- The ingress's table names, for every site of a public environment, the
  bundle it serves, **live**, and while a version comes, the bundle it
  switches to, **next**, each together with the spec's rules of its
  deployment, so that files, rules and switch are one step. An instance
  reports on its connection which bundles it **holds**: their manifests
  read from the store and checked against the digest, and the documents
  the rules name fetched.
- A site's rollout: the bundle is complete, uploaded already or ingested
  from the image now; `ferund` puts it into the table as `next`, and
  every instance loads it beside `live`, which it goes on serving; once
  every connected instance holds it, `ferund` makes it `live` in one
  version of the table, the **switch**. An instance swaps the site's
  bundle at that version as one pointer; a request is served by the
  bundle it found when it began, never by two.
- The barrier is what keeps versions from mixing. An instance that
  switched alone would serve the new document while its neighbours serve
  the old assets, and behind a balancer that spreads one page's requests
  over instances a page would ask for files its version has and the
  other does not. With the barrier the window is one delta of the table.
- An instance that connects later, or restarts, loads `live` before it
  serves the site: a request waits for a bound, then gets 503 with
  `Retry-After`, as for a pending service. An instance that has lost
  `ferund` serves what it has and does not hold the barrier up: only
  connected instances count.
- When `next` is not held by every connected instance within the site's
  rollout budget (`rollout.timeout`, as a service has it), the rollout
  fails: blue stays live, the deployment fails as ADR 0018 says, and the
  incident names the instances and what they answered.
- **Blue stays loaded** until the next switch of that site or for a day,
  so that a revert, a deployment that names blue's digest, finds the
  barrier passed and switches back in one delta of the table.
- **Sites switch after services**, so that a new front end never meets
  the old API; a failed rollout of a service switches no site. The other
  order is the one APIs are written to bear.
- **A manual switch**, for a version someone looks at before the public
  does: a site with `"switch": "manual"` stops after the load. Its next
  bundle is reachable on every host of the site for a request that
  carries a header of FeRun's own with a token per deployment, shown to
  whoever may read the environment, and a call with `write` on the
  environment (ADR 0031) makes it live; until then the deployment is
  `deploying` and says that it waits.

### The ingress serves a site

- The table names a site's bundles by digest and its tenant's prefix;
  manifests are not in the table, whose entry per environment is bounded
  (ADR 0021). An instance keeps manifests and blobs on its volume under
  bounds, least recently used first out, each blob checked against its
  hash. It answers `GET` and `HEAD`, `OPTIONS` where CORS answers a
  preflight, and 405 with `Allow` to anything else; a file is served
  with the standard library's file serving, which answers `Range` and
  the conditional requests; the path is matched as the ingress already
  checks it, decoded, case-sensitive, and the query plays no part in the
  lookup and is kept on a redirect.
- Without `ferund`, nothing changes: the table is cached, and so are the
  manifests. Without the store, files on an instance's volume are served
  and others answer 502 naming the store.
- A site has no wake, no hold, no activity, no endpoints and no
  admission. Its route's `from` is checked before the site's rules, with
  403 (ADR 0012). Its requests count towards the shares per tenant and
  environment the ingress keeps for every request (ADR 0021), so that
  one tenant's site takes no more of an instance than its service would.
- **Cache headers.** `ETag` is strong, from the blob's hash: exact, the
  same on every instance, changed by the content alone; no
  `Last-Modified`, since an archive's times say nothing. `Cache-Control`
  is by default `public, max-age=0, must-revalidate`, as Cloudflare Pages
  sends it: a browser asks again each time, gets a 304 while nothing
  changed, and sees a switch at its next request; what may be cached for
  longer, hashed assets above all, a headers rule says. The ingress
  keeps no shared cache of answers: a CDN in front of a domain is a
  tenant's choice (ADR 0030).
- **Compression and type are decided at the ingest**, once, not per
  request: `ferund` stores a gzip variant of every compressible file
  that is worth it, and a zstd variant once its dependency is approved;
  precompressed siblings a build writes become variants of their file.
  The ingress picks by `Accept-Encoding`, sends `Vary: Accept-Encoding`
  for every file that has a variant, and serves a `Range` request
  uncompressed. The content type comes from the extension by a table in
  the code, not the machine's `mime.types`, is part of the manifest, so
  that a version's types do not change with the ingress's version, and
  may be overridden by a headers rule. Every answer carries
  `X-Content-Type-Options: nosniff`.

### Rules: what they say, and where they live

- **A request is answered in this order:** the redirects, first match
  wins; the file, with the site's `html` handling; the rewrites, first
  match wins; the site's `notFound` handling. Headers rules and CORS
  then apply to whatever is answered. A redirect wins over a file, as on
  Cloudflare, so a version that moves a page redirects its old path even
  where the old file is still in the bundle; a file wins over a rewrite,
  as on Netlify, so `/* /index.html 200` serves the document for the
  application's paths and the files for theirs, never a script's path
  with the HTML document.
- **What the rules can say**, each typed and checked as the spec is: how
  `.html` and trailing slashes are treated (`html`, with 307 redirects
  so that no browser keeps one for good); how a missing path is answered
  (`notFound`: the nearest `404.html`, the single-page application's
  document for a request that wants a document and a 404 for a missing
  script or image, or a plain 404); redirects, and rewrites to a path of
  the same site and never to another host, since what is proxied to a
  program is a route's to say; headers to set or unset per path pattern,
  where the framing headers, `ETag` and `Set-Cookie` are the ingress's
  and refused at plan, since a cookie from a site under the apps zone
  would undercut the host-only cookies of ADR 0021; and CORS, typed and
  not a headers rule, because a preflight is an answer the ingress gives
  itself and a listed origin has to be echoed with `Vary: Origin`.
  Without `cors` a site sends no CORS header at all.
- **The application's rules travel in its bundle**: how a missing path
  is answered, since its router owns the paths; the cache lifetimes its
  hashed names allow; the headers its code needs, such as a
  `Content-Security-Policy` that names what this version loads; the
  redirects from paths an earlier version had. That is what the
  `nginx.conf` in the image was, and it stays where it was: in the
  project's repository beside the code, built into the version, promoted
  and reverted with it under one digest. **`_ferun.json`** at the root
  of a bundle holds them in the spec's own schema, a site's object
  without `source` and `switch`; the upload or the ingest checks it with
  the code that checks the spec at plan, and a file that does not
  validate fails the upload with the path of each error, since no
  version should go live with rules that are wrong. Its rules are stored
  in the manifest, so the digest covers them. **`_redirects` and
  `_headers`**, in the syntax Cloudflare and Netlify read, are read
  beside it, because static site generators write them; a line that does
  not parse is a finding and is skipped, as those hosts skip it, and
  their rules come after `_ferun.json`'s.
- **The environment's rules live in the spec**: what differs by place,
  the origins CORS allows, a redirect to a host that is another one on
  the stage, a header only one environment sends (`X-Robots-Tag:
  noindex`), the switch. A bundle is the same in every environment, so
  it cannot carry these; a spec is not what a build writes, so it should
  not have to carry the application's. The spec is the overlay: its
  redirects and rewrites are tried before the bundle's, its headers
  rules applied after them, so that the environment has the last word;
  `html`, `notFound` and `cors` are one value each, the spec's where it
  sets one, else the bundle's, else the default; `"bundleRules": false`
  sets the bundle's aside.
- **Both are versioned, and both are seen.** A deployment pins the
  bundle, whose digest covers its rules, and holds the spec; a revert
  brings both back, and the two switch together. Plan shows every site's
  effective rules with where each comes from (`_ferun.json`,
  `_redirects:12`, `spec`) and names what conflicts; a bundle whose
  rules fail fails its rollout before the switch, and blue stays live.

A site's `_ferun.json`, kept in the project's repository where its build
copies files unchanged:

```json
{
  "notFound": "spa",
  "headers": [
    {"path": "/assets/*",
     "set": {"Cache-Control": "public, max-age=31536000, immutable"}}
  ],
  "redirects": [
    {"from": "/settings/*", "to": "/account/:splat", "status": 301}
  ]
}
```

### Records, limits, retention, the sweep, the store

- A site's requests are lines and spans of a stream of the site's own,
  shipped by the ingress as its access log is (ADR 0021, ADR 0026),
  labelled with the environment, the kind `site`, the site's name and
  the deployment, each with the bundle's digest. The traffic per route
  target includes sites as it includes services.
- **Limits** are the operator's, with defaults near Cloudflare Pages': a
  bundle's files and bytes, a file's size, an upload's body, a path's
  length, a site's rules, spec and bundle together. A tenant's sites
  take at most a quota of the store, the sum of its distinct blobs; an
  upload or ingest beyond is refused with what the quota is and what is
  in use.
- **Retention.** A bundle is kept while a kept deployment names it, and
  a deployment's bundle is kept while the deployment is the latest of
  its environment, the deployed one, tagged, or one of a bounded number
  of the most recent; and any bundle while it is young, so that an
  upload has time to be deployed. Deployments themselves are kept for
  good (ADR 0018); what goes is their content.
- **The sweep**, a leader loop, once a day: a bundle that is no longer
  kept loses its manifest first, then its rows; a blob that no kept
  bundle names is deleted a day after the last one stopped naming it, so
  that an ingest in flight that reuses it never loses it. A revert to a
  deployment whose bundle was swept is refused at plan with a finding:
  for bundles, FeRun is the registry. A tenant's deletion removes its
  prefix.
- Sites have a **bucket of their own**, with a key for `ferund`, which
  writes and deletes there, and a read-only key for `ferun-ingress`,
  which faces the internet and must read nothing but what is published;
  the disk daemon's bucket holds every pod's disk and memory (ADR 0008,
  ADR 0011) and the sink's the logs (ADR 0026). A prefix in a shared
  bucket would not do: object stores' tokens are per bucket, not per
  prefix. The client is the standard library's, SigV4 and the five calls
  a site needs, in a package both programs share; no SDK, since the
  dependencies are fixed and five calls are not an SDK's worth. The
  table carries no credential; the ingress's key is the operator's, and
  the instances' egress policy (ADR 0021) gains the store's endpoint.

## Alternatives considered

- **A site as a service**, nginx or Caddy in an image: no code in FeRun,
  and every cost of the Context. It stays possible for whoever wants a
  web server of their own.
- **A public bucket** behind a CDN: a provider's rules engine for every
  tenant, a bucket readable by anyone who knows a key, and a provider in
  every request's path, which ADR 0021 declined for the ingress.
- **Cloudflare Pages for the tenants**: the model, not the platform; the
  same provider dependency, and FeRun's deployments, hosts and records
  apart from the site's.
- **The archive in the store, read by ranges**: no unpacking, but an
  index per archive, no reading in the middle of a tar compressed with
  xz, and two versions that share nothing.
- **The ingest in a tenant's job**, a pod that unpacks: a microVM
  between a hostile archive and the control plane, and every format a
  tool exists for, at the cost of a pod per upload and a way for the job
  to write blobs. The archive readers are memory-safe and the limits
  bound the work; the same code can move into a job's image if the load
  or a format asks.
- **Rules in the spec alone**, as a bucket's rules sit in a CDN's
  console: part of the application apart from its code, a version that
  changes its routes needing a change of the spec in the same
  deployment, which no build writes, and what generators write
  translated by hand.
- **Rules in the bundle alone**, as Pages and Netlify have them: what
  differs by environment would need a bundle per environment, which ends
  promotion by digest.
- **A configuration file of the repository, read at deploy**, as
  `vercel.json` or `netlify.toml`: those hosts build from the repository;
  FeRun receives what was built, so the file has to be part of what it
  receives, which is `_ferun.json`.
- **A file per directory, as Apache's `.htaccess`**: read on every
  request, the rules for a path spread over every directory above it,
  and `mod_rewrite` a language of its own where a few typed rules
  suffice. One file at the root, read once at the upload, keeps the idea
  and loses those; its name starts with `_` and not with `.` because
  some CI tools leave hidden files out of what they upload.
- **Cloudflare's files alone**: `_redirects` and `_headers` cannot say
  `notFound` or `html` and have no CORS, and their hosts skip a wrong
  line without a word. A typed file of FeRun's own is checked as the
  spec is.
- **Manifests in the table**: a site of many thousand files has a
  manifest of megabytes, beyond the table's bound per environment and
  sent to every instance at every connect; it is immutable, which is
  what the store is for.
- **A switch per instance, without the barrier**: simpler, and a page
  whose document comes from green and whose assets from blue for as long
  as the slowest instance takes.
- **Canary splits by percentage**: a site has no pods to roll, and a
  share of requests per version mixes versions within one page's load
  unless the split is sticky.
- **Blobs shared across tenants**: less storage, and an oracle of other
  tenants' files.
- **Compression per request**: CPU on every answer for bytes that never
  change.

## Consequences

- A tenant uploads a built directory or names an image, and the site is
  live on its routes in one deployment, with no pod, no cold start and
  no web server configuration of its own; a revert serves the bytes it
  served before, and promotion costs nothing in the store.
- `ferund` holds the store's write key and does the ingest's work,
  bounded by limits and a quota; `ferun-ingress` holds a read key and a
  cache on its volume, and reaches the store beside `ferund`, the sink
  and the nodes. Operators make the sites' bucket and two keys, and
  size the ingress's volumes.
- The ingress's table carries two bundles per site and a barrier; a
  site's switch is one delta of the table, after the deployment's
  services. The deployment builder (ADR 0032) shows a site's source and
  the environment's rules, with the bundle's rules beside them,
  read-only, so that the overlay is seen where it is written.
- A site's rules are checked at the upload and at plan, in one schema,
  and a version with wrong rules never goes live.
- FeCode decides its side in an ADR of its own: sites in its definition,
  a build whose last stage holds the built site, previews that keep
  serving a dev server through an attached service, and where a person
  uploads an archive. The dashboard's `web` is the first site, its
  `nginx.conf` a `_ferun.json` in its `public/`.

## Open questions

- **Two dependencies.** xz needs `github.com/ulikunitz/xz`, and zstd,
  for tar archives and for the zstd layers BuildKit writes, needs
  `klauspost/compress` to become a direct dependency; the repository's
  rule asks for both to be approved. Without them the upload takes zip,
  tar and tar with gzip, and an image with zstd layers fails its ingest
  with a finding.

## History

Decided on 2026-11-06 and committed on branch `static-sites` on
2026-11-08 (40fc62b), with ADR 0030 (53d9121) beside it. Brought to the
main line on 2026-11-08 as Proposed; nothing of it is built, and static
sites come after the work in hand. The original cited the ADRs of its
day; here they stand by their consolidated numbers: hosts, domains and
certificates are ADR 0045, split from ADR 0021; the registry client is
ADR 0022's, into which ADR 0025 merged; a route's `from` is ADR 0012's;
the route door is ADR 0043. ADR 0026 knows a stream as one container's
output or the ingress's access log; a site's stream is the ingress's, of
a kind of its own.
