# ADR 0025: Private images: a create brings its registry credentials, the registry is asked before every new pod, and an image is baked once behind that question

**Status:** Proposed · **Date:** 2026-10-29

## Context

FeRun runs public images only. `ferund`'s resolver speaks the anonymous
registry protocol ("private registries are not supported and look like
this"), passes a reference with a digest through without asking, and
keeps a tag's digest for 60 s under the reference as written. The guest
pulls each pod's image anonymously into that pod's disk. Nothing is
shared between pods, so every pod's pull asks the registry, and a
private image runs nowhere. FeCode needs private images now: its
projects' stage and production run images from the platform's registry,
`oci.fecode.dev`, which wants a login and is FeCode's, its tokens
FeCode's (FeCode's ADR 0009 and ADR 0012). FeRun is a client of every
registry alike: GHCR, Docker Hub and `oci.fecode.dev`.

gaios, FeOS's proof of concept, shared images between pods. The first
sandbox of an image pulled it in its guest, froze the guest's filesystem
and sealed the disk's top as a layer (`diskd`'s `Bake`, which FeOS kept
and never calls). A catalog keyed by the image's config digest, the
guest's build and L0 named the layer, the first bake winning, and every
later sandbox of that image, on any node, started on the layer: "no
pull, no registry round-trips, no unpack" (gaios ADR 0014 §4). It could,
because it ran public images only: a hit asked no one, and no one had to
be asked. ADR 0022 brings the bake back, keyed by the manifest digest,
and lets "a second replica, a replica on another node, a restart after a
crash and a revert to last week's release all attach a layer that is
there".

With private images, a cache that answers without the registry hands an
image to whoever names it:

- **A digest is no secret.** Releases, plans, logs and deployment
  manifests show manifest digests; `docker images` shows the config
  digest as the image's ID. A create that names a private image by
  digest, with credentials that do not reach it or with none, would
  start on its layer.
- **A digest alone names no source.** A create that names
  `evil.example/x@sha256:<the digest of another's private image>` asks a
  registry its author runs, which says yes to any digest. A cache keyed
  by the digest alone answers that create with the other image. gaios
  read the config digest from the manifest the registry served, so not
  even a byte of the image was needed.
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
  create brings may pull that manifest: `HEAD
  /v2/<repository>/manifests/<digest>`, the registry's Bearer challenge
  answered with the credentials at its token realm (or the credentials
  sent as Basic authentication where the registry asks for that), and
  anonymously when the create brings none. A yes is a 200 whose
  `Docker-Content-Digest`, when it has one, is the digest asked for; a
  registry that does not answer `HEAD` (405) is asked with a `GET`, and
  the body's digest must match. The question is whether these
  credentials reach this image, not whether they are valid: a login that
  works says nothing about a repository it does not reach.
- **The answers.** 401 and 403 are a refusal, 404 is "not there for
  these credentials" (registries answer a private repository with 404);
  either fails the create with the registry's answer, as an image that
  does not resolve fails it today. No answer is no: a registry or token
  realm that is unreachable, a timeout, 429, 5xx, or a digest other than
  the one asked for, and the create waits and is retried as a create
  without capacity is, the reason in the sandbox's status ("image access
  not confirmed (retryable): …"). Only a yes creates a pod.
- **When.** Every create of a new pod: a sandbox's first and every later
  create of its pod (after a failure, a lost node, a repair by
  re-create), an image service's replicas, a rollout's and a revert's
  pods, a clone's, a job's runs. Not a resume, a restore of a hibernated
  pod on any node, a start of a stopped pod on its disk, or a container's
  restart in its pod: they continue a pod that was admitted, on its own
  disk, and take nothing from a cache. Access that ends stops new pods,
  not running ones.
- **Where.** In the sandbox engine, before a create claims a node,
  outside any transaction, on whichever instance runs the create;
  sandboxes and environments' pods (ADR 0018) pass through that one
  step. The node does not ask: it holds no create's credentials, and
  `ferund` is its only caller that names images.
- **Nothing is remembered but the registry's token.** Every create asks
  again. What `ferund` keeps is the Bearer token the registry gave for
  these credentials and `repository:<name>:pull`, in memory, until
  shortly before its `expires_in` (60 s when it names none): the
  registry's own yes, for as long as the registry gives it. Questions with
  the same credentials, repository and digest at the same time share one
  request, so twenty replicas of a rollout ask once. The token cache and
  the sharing know credentials by an HMAC under a key of the process,
  never stored.
- **The plan asks too.** A plan resolves every image with its
  credentials, a reference with a digest included, so that an image that
  will be refused fails at plan and not at a pod; the tag cache is keyed
  by the credentials as well as the reference.

The resolver's network rules stay (https only, no proxy, `--deny-prefix`
on every address dialled, for the registry, a redirect and the token
realm alike), and the credentials go only to the token realm the
registry names, over https, as Docker sends them.

### Credentials come with the create

- **A sandbox** takes `imageCredentials: {username, password}` at its
  create. `ferund` seals them in the sandbox's row under the key of
  `--secret-key-file`, as it seals the bindings' credentials (ADR 0014),
  uses them for this sandbox's questions, resolves and pulls, and never
  returns them. `PUT /api/v1/sandboxes/{id}/image-credentials` replaces
  them when they rotate, `DELETE` removes them.
- **An environment's** services and jobs take `imageCredentials` with two
  env values, a string or `{secret}` each (ADR 0018). Unlike every other
  secret of a release, a secret named there is read at its latest version
  at every create, not pinned to the release: it is access, not
  configuration, and a token rotated or revoked must reach next month's
  replica of last month's release.
- **No credentials of the tenant, and no matching by host.** A tenant
  serves many: FeCode serves every project. A credential of the tenant
  chosen by the image's registry would ask for one project's image with
  another's credentials, the crossing this ADR prevents.
- **No credential enters a pod** (ADR 0001), but one while the guest
  pulls: until the node pulls (ADR 0022), the guest pulls with the
  Bearer token of the yes, scoped to the repository's pull and expiring
  as the registry says, and holds it in memory only. That token is the
  follow-up recorded in [security.md](../security.md): code in the
  sandbox that reads it may pull every tag of the repository while it
  lives. The token rides beside the pod's spec, not in it, so that
  a create retried with a new token is the same create. A registry that
  takes only Basic authentication (distribution with htpasswd, as
  `oci.fecode.dev` is today) would need the password in the pod: a
  create that brings credentials for one fails ("the registry issues no
  tokens") until the node pulls.

### An image is baked once, behind the question

- **The bake comes back as gaios had it**, in an image's first pod:
  `feosd` has the guest pull the image before the pod's first container,
  freezes the guest's filesystem (`FreezeFilesystem`), has `diskd` seal
  the frozen top as a layer on L0 (`Bake`), thaws it, and only then
  creates the containers. It pushes the layer to the object store in the
  background and reports the bake once the store has it. The bake point
  comes before anything of the pod's own reaches its disk: no container
  has run, so no tenant's code either, and the spec's command,
  environment and volumes are not there yet. What the guest writes to its
  disk before the point besides its image store, its logs among them,
  is kept off it. A bake that fails costs only the next pod's pull.
- **The key is the source**: the registry, the repository and the
  manifest digest, with the guest's version and the disk's L0, the layout
  and the size the layer is made for (gaios's `gaios_bundle` and `l0`).
  Never a digest alone, never the config digest. The same image in two
  repositories is two bakes, stored once, since `diskd`'s chunks are
  content-addressed. A registry's yes speaks for its own repositories
  only, and a registry that serves a manifest has not shown that it holds
  the layers: only serving them shows it, and that is a pull.
- **A catalog in `ferund`**: a table of bakes by that key, the newest
  first, a row only for a layer the object store has; a second bake of
  the same (two nodes at once, or after the store lost a layer) is a row
  of its own. After the yes, a create names its image's layers to the
  node, beside the pod's spec and not in it. The node starts the pod's
  disk on the first one made for the guest it boots that the store has
  (the chain `L0, layer`), and the guest finds the image in its store and
  pulls nothing; with none, the pod pulls and bakes. The pod's record
  says what its disk was made on.
- **Only what was checked is baked**: a pull whose manifest and layers
  matched their digests (the registry client checks them), of a pod whose
  containers name one image. A node without the object store bakes
  nothing.
- **The disk, not the memory.** A pod on a baked layer boots its guest.
- ADR 0022's bake pod, which bakes without a tenant's pod and pulls on
  the host, replaces the bake in the first pod when the node pulls; the
  key, the catalog and the question stay.

## Alternatives considered

- **A yes remembered** for a while, by source, digest and a fingerprint
  of the credentials: one `HEAD` saved per create, against access that
  ends lasting the reuse beyond the token's life, a fingerprint of
  credentials kept (an HMAC with a key, since a plain hash of a password
  is guessed offline), and an outage longer than the reuse stopping new
  pods anyway. The token kept is already the registry's own remembered
  yes, for the time the registry chose.
- **Asking at resolve only**, at plan or a sandbox's first create:
  gaios's hit with one step more. Access that ends is never seen, and
  next month's replica runs on this month's answer.
- **Asking whether the credentials are valid** (a login, `GET /v2/`):
  says nothing about the repository.
- **The question, with a cache keyed by the digest**: a registry of one's
  own says yes to any digest.
- **A bake shared across sources** once a `GET` of the manifest matches
  its digest: a registry may hold a manifest without its layers; the
  question shows access to a source, not that it holds another source's
  layers.
- **The tenant as the boundary**, its credentials chosen by the image's
  host (ADR 0022's draft, Docker's `config.json`): see above; FeRun's one
  tenant serves every project.
- **The node asks** before it attaches a layer, or checks a grant that
  `ferund` signed: the node would need every create's credentials, even
  for a layer it has, and `ferund`, its only caller, may create any pod
  anyway; the node's check would show nothing that `ferund`'s does not.
- **Every pod pulls**, as today and as Kubernetes's `AlwaysPullImages`
  makes it: the pull is the question, but every replica pulls and
  unpacks (ADR 0022).
- **Open when the registry does not answer**, for an image the same
  credentials ran lately: an outage stops new pods today already, since
  every pod pulls; with a bake the question is all that needs the
  registry, and a rule that holds only while the registry answers is not
  one.
- **The credentials in the pod** for a registry that takes only Basic
  authentication: against ADR 0001; such a registry waits for the node's
  pull.
- **A memory snapshot** of an image's first pod, restored for the next:
  the boot saved, but every pod starts with the same random state and
  the same guest until each is reseeded, tenants' pods share the guest's
  pages (a side channel, as with KSM), and the snapshot is pinned to the
  node's versions (ADR 0008). A decision of its own, if the boot is ever
  what a start waits for.

## Consequences

- **A registry that does not answer stops new pods of its images**, baked
  or not, as today, where every pod pulls; running pods, resumes and
  restores go on. The platform's own images are public on GHCR
  (`ghcr.io/gardenlinux/feos/…`, FeCode's ADR 0012 §2), as the
  sandboxes' default image is: their question is anonymous and goes to
  GHCR, the addons' to Docker Hub until they are mirrored there.
- **A create costs one `HEAD`**, and a token request when none is held:
  milliseconds in the cluster, tens of milliseconds to a public registry.
  Docker Hub does not count a `HEAD` against its pull limit.
- **A create of a known image pulls nothing**: the second pod of an image
  on any node starts on its layer, which `diskd` fetches lazily by chunk.
- `ferund` gains the question in the engine's create, its token cache and
  shared requests; credentials sealed in sandbox rows and their API;
  `imageCredentials` in the release spec, read at the latest version; the
  resolver with credentials and the tag cache keyed by them; the bake
  catalog and its lookup; the answers in status, plans and incidents.
- `feosd` gains the bake point in a pod's bring-up (the guest's
  `PullImage`, `FreezeFilesystem`, `diskd`'s `Bake` and `Push`,
  `ThawFilesystem`), a disk on a baked layer, the token beside the spec,
  and the bake's report. The guest's pull takes a Bearer token
  (`RegistryAuth::Bearer`). `diskd` needs nothing new.
- ADR 0022 changes: the credentials are the create's, not the tenant's;
  a baked layer is keyed by its source and used only after the question;
  the first bake is in the image's first pod until the node pulls. Its
  section on the platform's registry, with `ferund` as the issuer, is
  withdrawn by FeCode's ADR 0012: the registry is FeCode's, which sends
  a project's pull credential with the create like any other.
- **Built in this order**, each step leaving no cached image used without
  the question:
  1. The question for every create, anonymous, and the tag cache keyed
     by credentials: public images as before, plus the question.
  2. Credentials with the create, and the Bearer token for the guest's
     pull: private images from registries that issue tokens (GHCR,
     Docker Hub, GitLab, Harbor, Quay).
  3. The bake, its catalog, and the hit behind the question. It needs
     only the first step, and may be built beside the second.

  `oci.fecode.dev` needs nothing more of FeRun than the second step:
  its token issuer is FeCode's (FeCode's ADR 0012). The node's pull,
  after which no credential enters a pod, is ADR 0022's bake pod.

## Open questions

- **Garbage collection** of baked layers and their catalog rows
  (ADR 0022's open question), now per source.
- **Pods of several images** (sidecars): one bake per set of images, or
  a layer per image.
- **Running pods of a withdrawn image**: whether a refusal ever stops a
  pod that runs.
