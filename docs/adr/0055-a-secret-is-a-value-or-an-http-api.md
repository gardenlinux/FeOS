# ADR 0055: A secret is a value or an HTTP API: one object per key, its kind says where it lives

**Status:** Proposed · **Date:** 2026-11-20

Every key of an environment is a secret, and its kind says where the key
lives. A `value` secret is handed to the containers that reference it. An
`http` secret is an HTTP API: its URL, how the code calls it, its
authentication and its options, versioned as one; its key is bound to
its URL and only the node uses it, adding it to the workload's requests.
A service or job gets an HTTP secret by a variable that references it,
which gives it the URL to call, and a deployment pins the version; a
sandbox names the HTTP secrets of its environment and gets each at its
latest version. Bindings stated in a spec, a sandbox's own set of
bindings and the environment's credentials (ADR 0033) give way to it.
The console shows each key once, under where it lives. Once built, this
supersedes ADR 0033 and amends ADR 0044 and ADR 0028.

## Context

ADR 0028 offloads a key: the node adds it to a workload's HTTP requests,
and the container never holds it. Where the key and the binding around
it are stated grew in three places, each with its own rules:

- **A binding in the spec** names an upstream and a credential whose
  secret fields reference the environment's secrets (ADR 0028, ADR
  0044). The secret is a plain value: nothing ties it to the binding.
  A deployment that changes the binding's upstream sends the key there,
  and a variable that references the same secret hands it to a
  container. That the key stays at the node holds for one deployment,
  not for the key.
- **A credential of the environment** (ADR 0033) is a whole binding
  under a name, versioned, its key bound to its upstream, which a
  sandbox names at its latest version and a spec's binding names by
  `credentialRef` at a pinned one.
- **A sandbox's own set**, written with the sandbox's token, its keys
  sealed per sandbox. No person's session writes or reads it, and the
  console does not show it.

What a person sees follows the objects, not the keys. An environment's
Secrets view lists one API under Credentials and again under HTTP
bindings, and one key as a secret and again as a binding's credential,
each marked "offloaded". Asked what tells a secret from a credential, a
reader of that view could not say, and took the binding's mode, the
name it is called by or its own name intercepted, for the difference;
the mode is independent of both. "Credential" names four things: a
binding's authentication, the environment's object, a pod's image
login and a workload's identity (ADR 0014). And the first question a
new credential asks is the mode, a question about the client's code,
whose change asks for the key again although the key goes to the same
upstream.

FeCode already has one object for this. Its `fecode.yaml` declares an
HTTP API as a secret of type `http` with its mode, a service gets it by
a variable `{secret: <name>, key: url}`, and each place attaches the
URL, the authentication and the options (FeCode ADR 0018). Rendered for
FeRun, each such secret becomes a spec binding and secrets named
`http.<name>.<field>`, which the Secrets view shows as such.

## Decision

### A secret has a kind

A secret of an environment has a kind, fixed when it is made:

- **`value`**: a value, as ADR 0044 has it. It reaches a container as a
  variable (a process's start and an addon's provisioning step alike),
  and the node's image pull as a login (ADR 0022). Nothing binds it to
  a destination.
- **`http`**: an HTTP API of the environment. Its definition is the
  API's origin and the CA of a private PKI where it has one; the
  **mode**, how the code calls it: by the secret's name,
  `http://<name>`, the default, or by the API's own name, intercepted at
  the node with a CA per pod (ADR 0028); whether the node forwards the
  host (the name mode only); the path prefixes and the rate limit; and
  its **authentication**, one of ADR 0028's kinds (headers, Basic,
  OAuth 2.0 client credentials, a client certificate) with its fields.
  The fields that are secret (a header's value, a password, a client
  secret, a private key) are sealed with the version; the others (a
  user, a client ID, a token URL, the headers' names) are readable.
  Its name is a DNS label, since the name mode makes it a host name.

Both kinds share ADR 0044's rules: a write is a new version, the
highest the name ever had plus one; a version never changes; no answer
carries a secret field, which answers as set; a deployment pins the
version it was applied with; a secret the latest deployment pins is not
deleted. The kind does not change: a value that was handed to
containers cannot become a key that never was.

**An HTTP secret's key is bound to its API.** A write may keep a stored
secret field instead of sending it again only while the origin, the CA
and the token URL stay the latest version's; a write that changes any of
them sends the secret fields again, since `write` on the environment
reads no key and must not send a stored one to a host of its choice or
trust a CA of its own for it. The mode, the forwarded host, the prefixes
and the rate limit change without it: the key goes to the same API.
Nothing but the node uses an HTTP secret's key. A variable that
references it gets its URL, never a field.

### A workload gets an HTTP API by a variable

A service or job gets an HTTP secret exactly when one of its variables
references it, and its pods get that binding and no other:

- `{secret: <name>, key: url}` is the base URL the code calls:
  `http://<name>` in the name mode, `https://<the API's host>` in the
  intercept mode. `key: host` is the host alone.
- `{secret: <name>}` without a key on an HTTP secret is an error, as is
  a key on a value secret.
- A client that needs no variable, a library with its host built in
  under the intercept mode, still names one: the variable is the
  dependency, written down, and the same code runs everywhere.

A deployment pins the version, and its normalised spec carries the
pinned version's origin and mode, which decide whether a pod is another
(ADR 0028); the pods get the pinned version. A new version reaches the
services with the next deployment; a change of the authentication or the
options alone seals the running pods again in place, a change of the
origin or the mode replaces them. A revert gets the version it had. An
attached service has its sandbox's. A clone copies an HTTP secret where
it is named, as it copies a value (ADR 0018).

### A sandbox names its environment's HTTP secrets

A sandbox names the HTTP secrets of its environment that its pod gets,
at its create or later by stating the whole list, and gets each at its
**latest** version: a new version is sealed to every running pod that
names it at once, and nothing restarts. A sandbox names no value secret
and no secret of another environment. These are a pod's HTTP bindings,
with a deployment's: there is no other source, and a person with `read`
on the environment sees every one.

### Where each key lives, said once

A key's place follows its kind, and the console says it with the marks
it has: **by identity** for an addon that admits a workload by its
certificate (ADR 0038), which needs no key; **at the node** for an HTTP
secret and a value secret that only image pulls name; **in the
container** for a value secret a variable references and an addon's
password. An environment's Secrets view groups its keys by these marks,
each key in one row with its uses: an HTTP secret's row has its URL, how
the code calls it, who uses it (services and jobs with the version they
pin, sandboxes at the latest), its requests of the last day and its
check. A new secret asks first which kind it is: a value the code reads,
or an API the platform calls with the key. The word "credential" is
left to the authentication inside an HTTP secret.

### What gives way

A plan refuses a binding stated in the spec and a `credentialRef`;
deployments stored with them keep them, so that a revert to one runs
what it ran. The environment's credentials become HTTP secrets of the
same names and versions. A sandbox's own set of bindings goes once
FeCode gives its workspaces their environments' HTTP secrets.

## Alternatives considered

- **A "connection", the credential renamed, beside secrets that are
  values alone.** Equal in substance, and the word says the object is
  more than a key. It keeps a second object for what people call a
  secret, with its own pins, routes, clone and view, which FeCode ADR
  0018 rejected for the same reason; and FeCode's one secret would
  still become two objects in FeRun.
- **Bindings in the spec alone.** No new object, but a sandbox gets
  nothing without a deployment, and the key stays a value that any
  deployment may send anywhere.
- **Better words and grouping, the objects as they are.** Cheap, and it
  leaves two ways to state one API and a key that is bound to its API
  in one of them only.
- **The mode on the reference.** The code depends on it, so the
  reference would carry it; two references of one pod could disagree,
  and every sandbox would state it again. The mode is a version of the
  secret, which a deployment pins.
- **A list of the HTTP secrets a service uses, beside its variables.**
  A second way to say the same; the variable is the dependency.
- **A kind that changes.** A value handed to containers would be marked
  at the node from then on, which it is not.

## Consequences

- "Where does this key live" has one answer per secret, which does not
  change with a deployment: the kind says it.
- An HTTP secret's key reaches its API and nothing else, whatever a
  deployment, a sandbox or a person with `write` states.
- An environment has one list of its keys; the bindings, the
  credentials and a sandbox's own set leave the API and the console.
- FeCode's `fecode.yaml` stays as it is. Its rendering passes
  `{secret, key}` through and writes an environment's HTTP secrets
  instead of spec bindings and `http.<name>.<field>` secrets; its env
  mode, which hands the key to the containers, renders value secrets,
  as now. A workspace gets the project's development values as HTTP
  secrets of its own environment, which its sandbox names.
- A registry login stays a value secret that image pulls name; a
  `registry` kind of the same shape, bound to its registry, follows
  (BACKLOG).
- **The way there**, each step working on its own:
  1. ferund: the kinds in the environment's secrets, `{secret, key}` in
     the spec, a sandbox's names; the environment's credentials become
     HTTP secrets in a migration that refuses, naming them, while a
     name is both a credential and a secret.
  2. A command of ferund turns each environment's spec bindings of its
     latest deployment into HTTP secrets, their keys from the versions
     that deployment pins, so that nobody enters a key again; the next
     deployment of each environment references them.
  3. The console: the Secrets view by marks, the new secret's question,
     the builder's variable "URL of an HTTP secret" instead of its
     binding form.
  4. FeCode renders HTTP secrets and writes them per environment; once
     each environment's latest deployment pins them, the
     `http.<name>.<field>` secrets go.
  5. FeCode's workspaces name their environments' HTTP secrets; then a
     sandbox's own set goes.
  6. Plans refuse spec bindings and `credentialRef`.
- On acceptance ADR 0033 is superseded by this one, ADR 0044 is amended
  by its kinds and ADR 0028's "Where bindings come from" by its single
  source; `docs/architecture/control-plane.md` changes with the code.

## History

Proposed on 2026-11-20. A reader of an environment's Secrets view could
not tell a secret from a credential, and took a binding's mode for the
difference; a review of the model found that a key in a spec binding is
bound to no upstream, and that FeCode already declares an HTTP API as a
secret of type `http` (FeCode ADR 0018), which this follows.
