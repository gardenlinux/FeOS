# ADR 0022: Images: the registry asked before every pod, baked once behind the question

**Status:** Accepted · **Date:** 2026-10-27

Before `ferund` creates a pod, it asks the image's registry whether the
credentials the create brings may pull that image, and only a yes
creates the pod; a sandbox's create, and an environment's service's or
job's, brings its own registry credentials, never the tenant's. An image
is baked once into a sealed layer of diskd's (ADR 0011), keyed by its
source (registry, repository and manifest digest, with the guest and the
L0 it was made for) and recorded in a catalog of `ferund`'s, and every
later pod of that image, on any node, starts on the layer and pulls
nothing, once the registry has said yes. FeRun is a client of every
registry alike and issues no registry tokens; the platform's registry is
FeCode's. The bake is made in an image's first pod, after the guest's
pull and before its first container, and the guest pulls with the
registry's short-lived pull token, the one credential that enters a pod
until the node pulls itself (ADR 0050). A build is a tenant's job
(ADR 0018).

## Context

A production application needs a private image pulled once, and a
rollout that starts a replica in the time a disk layer takes to attach.
A guest that pulls its own image pulls from scratch in every new pod,
"once per pod and node even when the same image ran next door a minute
ago" (ADR 0011), and every replica of a rollout pulls again. ADR 0001
lists private images as the first thing CaaS needs, and the principle
stands that no credential enters a pod. FeCode's projects run images from
the platform's registry, which wants a login and is FeCode's, its tokens
FeCode's too (FeCode's ADR 0009 and ADR 0012); FeRun is a client of
every registry alike, GHCR, Docker Hub and FeCode's.

FeOS's proof of concept shared images between pods: the first sandbox of
an image pulled it in its guest, froze the guest's filesystem and sealed
the disk's top as a layer, a catalog keyed by the image's config digest
named the layer, and every later sandbox of that image, on any node,
started on it with no pull, no registry round trip and no unpack. It
could, because it ran public images only: a hit asked no one. With
private images, a cache that answers without the registry hands an image
to whoever names it:

- **A digest is no secret.** Deployments, plans, logs and manifests show
  manifest digests; `docker images` shows the config digest as the
  image's ID. A create that names a private image by digest, with
  credentials that do not reach it or with none, would start on its
  layer.
- **A digest alone names no source.** A create that names
  `evil.example/x@sha256:<the digest of another's private image>` asks a
  registry its author runs, which says yes to any digest. A cache keyed
  by the digest alone answers that create with the other image, and the
  config digest can be read from the manifest without a byte of the
  image.
- **Access ends.** A token is revoked, a person leaves an organisation, a
  repository turns private, an image is withdrawn for its licence or a
  vulnerability. A resolve at plan time or at a sandbox's first create
  says nothing about next month's replica, revert, or re-create after a
  lost node.

The boundary is not FeRun's tenant: FeRun's one tenant is FeCode, which
serves every project and person. It is the credentials a create brings:
whoever may pull an image with them may run it, and no one else.

## Decision

### The registry is asked before every new pod

- **The question.** Before `ferund` creates a pod whose container names
  an image, it asks the image's registry whether the credentials the
  create brings may pull that manifest: a `HEAD` of the manifest by
  digest, the registry's Bearer challenge answered with the credentials
  at its token realm, or the credentials sent as Basic authentication
  where the registry asks for that, and anonymously when the create
  brings none. A yes is a 200 whose content digest, when the registry
  names one, is the digest asked for; a registry that does not answer
  `HEAD` is asked with a `GET`, and the body's digest must match. The
  question is whether these credentials reach this image, not whether
  they are valid: a login that works says nothing about a repository it
  does not reach.
- **The answers.** A refusal, and "not there for these credentials",
  which is how registries answer a private repository, fail the create
  with the registry's answer, as an image that does not resolve fails
  it. No answer is no: a registry or token realm that is unreachable, a
  timeout, a rate limit, a server error, or a digest other than the one
  asked for, and the create waits and is retried as a create without
  capacity is, the reason in the sandbox's status. Only a yes creates a
  pod.
- **When.** Every create of a new pod: a sandbox's first and every later
  create of its pod (after a failure, a lost node, a repair by
  re-create), an image service's replicas, a rollout's and a revert's
  pods, a clone's, a job's runs. Not a resume, a restore of a hibernated
  pod on any node, a start of a stopped pod on its disk, or a
  container's restart in its pod: they continue a pod that was admitted,
  on its own disk, and take nothing from a cache. Access that ends stops
  new pods, not running ones.
- **Where.** In the sandbox engine, before a create claims a node,
  outside any transaction, on whichever instance runs the create;
  sandboxes and environments' pods (ADR 0018) pass through that one
  step. The node does not ask: it holds no create's credentials, and
  `ferund` is its only caller that names images.
- **Nothing is remembered but the registry's token.** Every create asks
  again. What `ferund` keeps is the Bearer token the registry gave for
  these credentials and the repository's pull, in memory, until shortly
  before the life the registry gave it: the registry's own yes, for as
  long as the registry gives it. Questions with the same credentials,
  repository and digest at the same time share one request, so twenty
  replicas of a rollout ask once. The caches and the shared questions
  know credentials by a keyed hash under a key of the process, never by
  the credentials themselves.
- **Tags resolve with the credentials.** `ferund` resolves a tag to a
  digest with the create's credentials, before a sandbox's first create
  and in an environment's plan, and a tag's digest is cached briefly for
  those credentials alone: what one asker resolved says nothing about
  another's access. The plan's finding for an image that does not
  resolve says whether the registry refused or does not know it. A
  reference with a digest is pinned as written; the question before the
  create asks the registry for it, so that a private image named by
  digest fails at its create and never in a pod.
- The resolver reaches registries as bindings reach servers (ADR 0014):
  over https only, without a proxy, never at a denied address, for the
  registry, a redirect and the token realm alike; the credentials go
  only to the token realm the registry names.

### Credentials come with the create

- **A sandbox** takes image credentials, a username and a password, at
  its create. `ferund` seals them beside the sandbox's row under the
  same key as the bindings' credentials (ADR 0014), uses them for this
  sandbox's questions, resolves and pulls, and never returns them. They
  can be replaced when they rotate, and removed.
- **An environment's** services and jobs take image credentials in their
  spec, each value a string or a secret (ADR 0044). Unlike every other
  secret of a deployment, a secret named there is read at its latest
  version at every create, not pinned to the deployment: it is access,
  not configuration, and a token rotated or revoked must reach next
  month's replica of last month's deployment. A tenant's job (ADR 0041)
  brings its image credentials in the call.
- **No credentials of the tenant, and no matching by host.** A tenant
  serves many: FeCode serves every project. A credential of the tenant
  chosen by the image's registry would ask for one project's image with
  another's credentials, the crossing this ADR prevents.

### An image is baked once, behind the question

- **The bake is made in an image's first pod.** `feosd` has the guest
  pull the image before the pod's first container, freezes the guest's
  filesystem, has diskd seal the frozen top as a layer on L0
  (ADR 0011), thaws it, and only then creates the containers. It pushes
  the layer to the object store in the background and reports the bake
  once the store has it. The bake point comes before anything of the
  pod's own reaches its disk: no container has run, so no tenant's code
  either, and the spec's command, environment and volumes are not there
  yet. A bake that fails costs only the next pod's pull.
- **The key is the source**: the registry, the repository and the
  manifest digest, with the guest's version and the disk's L0 the layer
  is made for. Never a digest alone, never the config digest. The same
  image in two repositories is two bakes, stored once, since diskd's
  chunks are content-addressed. A registry's yes speaks for its own
  repositories only, and a registry that serves a manifest has not shown
  that it holds the layers: only serving them shows it, and that is a
  pull.
- **A catalog in `ferund`**: bakes by that key, the newest first, a row
  only for a layer the object store has; a second bake of the same (two
  nodes at once, or after the store lost a layer) is a row of its own.
  After the yes, a create names its image's layers to the node, beside
  the pod's spec and not in it. The node starts the pod's disk on the
  first one made for the guest it boots that the store has (the chain
  L0, layer), and the guest finds the image in its store and pulls
  nothing; with none, the pod pulls and bakes. The pod's record says
  what its disk was made on.
- **Only what was checked is baked**: a pull whose manifest and layers
  matched their digests, of a pod whose containers name one image. A
  node without the object store bakes nothing. A sandbox's image is
  baked like any other.
- **The disk, not the memory.** A pod on a baked layer boots its guest.

### The pull token is the one credential in a pod

- No credential enters a pod (ADR 0001), but one while the guest pulls:
  the guest pulls with the Bearer token of the yes, scoped to the
  repository's pull and expiring as the registry says, asked for anew
  for every create so that it lives the registry's whole life for the
  pull, and held in memory only. Code in the sandbox that reads it may
  pull every tag of the repository while it lives, the exposure recorded
  in [security.md](../security.md).
- The token rides beside the pod's spec, not in it, so that a create
  retried with a new token is the same create; `feosd` keeps it off the
  pod's record and its disk, and hands it to no guest whose disk starts
  on a baked layer.
- A registry that takes only Basic authentication issues no token, and a
  password does not enter a pod: a create that brings credentials for
  such a registry fails until the node pulls (ADR 0050).

### FeRun issues no registry tokens

- The platform's registry is FeCode's, and so are its tokens: who may
  read and write a project's image is FeCode's knowledge (FeCode's
  ADR 0012). FeRun is a client of every registry alike: it pulls with
  the credentials a create brings, issues no registry tokens and keeps
  no namespaces. The platform's own images are pulled as any other.

### A build is a tenant's job

- A build is a tenant's job (ADR 0041) of a builder image, placed,
  admitted, logged and bounded in time like any tenant's job: the
  builder clones the source and pushes the image with short-lived tokens
  the caller passes as the job's secrets. `ferund` keeps no build
  record, fetches no source and has no builds API.

## Alternatives considered

- **A yes remembered** for a while, by source, digest and a fingerprint
  of the credentials: one `HEAD` saved per create, against access that
  ends lasting the reuse beyond the token's life, a fingerprint of
  credentials kept, and an outage longer than the reuse stopping new
  pods anyway. The token kept is already the registry's own remembered
  yes, for the time the registry chose.
- **Asking at resolve only**, at plan or a sandbox's first create: the
  proof of concept's hit with one step more. Access that ends is never
  seen, and next month's replica runs on this month's answer.
- **Asking whether the credentials are valid** (a login): says nothing
  about the repository.
- **The question, with a cache keyed by the digest**: a registry of one's
  own says yes to any digest.
- **A bake shared across sources** once a `GET` of the manifest matches
  its digest: a registry may hold a manifest without its layers; the
  question shows access to a source, not that it holds another source's
  layers.
- **Resolving a digest reference at the registry**, with the credentials,
  so that a private image named by digest fails at plan: a reference
  with a digest names its image already, and the question before the
  create asks the registry for it anyway; a plan that asked too would
  ask twice for one answer. This was the first text of this ADR; the
  code pins a digest reference as written.
- **The tenant as the boundary**, its credentials chosen by the image's
  host as Docker's `config.json` does (this ADR's first draft): FeRun's
  one tenant serves every project, so one project's image would be asked
  for with another's credentials.
- **The node asks** before it attaches a layer, or checks a grant that
  `ferund` signed: the node would need every create's credentials, even
  for a layer it has, and `ferund`, its only caller, may create any pod
  anyway; the node's check would show nothing that `ferund`'s does not.
- **Every pod pulls**, as Kubernetes's `AlwaysPullImages` makes it: the
  pull is the question, but every replica pulls and unpacks.
- **Open when the registry does not answer**, for an image the same
  credentials ran lately: with a bake the question is all that needs the
  registry, and a rule that holds only while the registry answers is not
  one.
- **The credentials in the pod** for a registry that takes only Basic
  authentication: against ADR 0001; such a registry waits for the node's
  pull (ADR 0050).
- **A layer per image layer**, mounted as overlay lowers in the guest, as
  Kata and firecracker-containerd do: exact sharing of a base layer
  between images, but a drive per layer against the pod's drive limit
  and an overlay of many lowers in the guest. One layer per image, with
  diskd's chunk deduplication across layers, gives most of the sharing
  for none of that.
- **Lazy image formats** (eStargz, Nydus) and a converter beside the
  registry: a second image format to keep; a baked layer in the object
  store is fetched lazily by chunk already (ADR 0011).
- **A memory snapshot** of an image's first pod, restored for the next:
  the boot saved, but every pod starts with the same random state and
  the same guest until each is reseeded, tenants' pods share the guest's
  pages (a side channel, as with KSM), and the snapshot is pinned to the
  node's versions (ADR 0008). A decision of its own, if the boot is ever
  what a start waits for.
- **`ferund` as the registry's token issuer**, with a namespace per
  tenant (this ADR's first draft): FeRun would own an authorization
  model of FeCode's projects and members that it has nowhere else;
  withdrawn by FeCode's ADR 0012.
- **The platform's own images under a prefix in FeCode's registry**, so
  that their question stays in the cluster (an earlier text of this ADR): the
  platform's images are pulled from GHCR like any other image, so that the
  platform does not depend on a registry it runs.
- **Builds elsewhere than in a tenant's job**: a builds API in `ferund`
  that fetches the source itself (this ADR's first draft, deferred by
  FeCode's ADR 0009), a build sandbox driven by exec, a build in the
  agent's own sandbox, or a CI outside pushing to the registry. ADR 0018
  records why a tenant's job: FeCode's projects have no CI, FeCode is
  their workbench, and "stage follows main" is true only when the
  platform builds what main is.
- **kaniko or buildah** as the builder: BuildKit is what `docker build`
  is, with the cache and the frontends people's Dockerfiles expect, and
  in a microVM it may run with the privileges it wants.

## Consequences

- A registry that does not answer stops new pods of its images, baked or
  not; running pods, resumes and restores go on.
- A create costs one `HEAD`, and a token request when none is held:
  milliseconds in the cluster, tens of milliseconds to a public registry.
  Docker Hub does not count a `HEAD` against its pull limit.
- A create of a known image pulls nothing: the second pod of an image on
  any node starts on its layer, which diskd fetches lazily by chunk. An
  image's first pod pays the pull and the bake; a pod of several images,
  or on a node without an object store, pulls as before and bakes
  nothing.
- A pod's disk holds no image where it starts on a layer: its size is
  the writable layer's and the logs'.
- The pull token is the one credential that enters a pod, in the guest's
  memory for the pull, scoped to one repository's pull and expiring in
  minutes; a registry that takes only Basic authentication refuses a
  create with credentials until the node pulls (ADR 0050).
- The registry's authentication is FeCode's, and nothing of it is
  FeRun's. FeCode's stage and production run images the platform built
  as tenants' jobs (ADR 0041), and promotion carries their digests.
- Layers baked of images that no deployment names stay in the object
  store and the catalog until a collection removes them.

## History

Written on 2026-10-27 and committed on 2026-10-28 (33ea94e) with four
decisions: the node's pull with a bake pod, the registry as FeRun's with
`ferund` its token issuer, builds on the platform, and a service's pod
supervised by its guest. On 2026-10-29 ADR 0025 (01b6de1) moved the
credentials from the tenant to the create, put the question before every
pod, keyed the bake by its source and brought it back into an image's
first pod; it was built the same day (11537e4, fc10bf7) and merged into
this ADR on 2026-11-08. The token issuer was withdrawn on 2026-10-29 by
FeCode's ADR 0012, the builds API was deferred the same day by FeCode's
ADR 0009, and builds became tenants' jobs on 2026-10-30 (ADR 0018,
86eff10); the platform's images moved to GHCR on 2026-10-31 (518608d).
The node's pull and the bake pod are ADR 0050, and the pod's lifecycle in
the guest is ADR 0004's and ADR 0048's.
