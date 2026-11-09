FeChat
======

FeChat is the reference chat client of FeRun, live at
https://chat.fecode.dev, for everybody with a FeCode ID (the users of
FeCode, FeRun and FeChat at https://id.fecode.dev, who sign in with GitHub
and get in by invitation). A React SPA and a small,
stateless Go backend: the backend runs an OpenAI-compatible LLM with tool
calling, and the tools run in a sandbox per conversation, which FeRun's
control plane `ferund` provides. The backend is an ordinary client of
ferund's public API, the same one any other agent uses:

- **Provisioning**, with a FeRun tenant token: `POST /api/v1/sandboxes`
  with `{"image"}` and no `sandboxId`: ferund mints the ID (ADR 0019) and
  answers it (`status.sandboxId`) with the sandbox's first `token`. A
  sandbox the backend has no session for (after a restart) gets a new
  token from `POST /api/v1/sandboxes/{id}/tokens` with `{"label"}`.
- **Sandbox references**: the backend keeps no database, so the browser
  keeps each conversation's sandbox ID, as a reference the backend signs
  (`backend/sandboxref.go`): `v1.<sandbox ID>.<MAC>`, the MAC an
  HMAC-SHA256 under a key derived from `SESSION_SECRET`, over the signed-in
  user's subject, the conversation ID and the sandbox ID. The chat stream
  hands it out in a `sandbox` event, and the SPA sends it back as
  `sandbox_ref` with every request of the conversation. A reference of
  another user or conversation, or a forged one, is ignored; one whose
  sandbox is gone (404) gets a new sandbox and a new reference. A
  conversation from before ADR 0019 has its UUID as its sandbox ID and no
  reference: without one, the backend looks for a sandbox of that ID and
  keeps the conversation in it.
- **Tools**: `GET /api/v1/tools` gives the tool schemas the LLM is
  offered, and `preparationWaitMs`, how long bringing a sandbox up may
  take, which sizes every tool call's timeout.
- **Execution**: MCP (Streamable HTTP) at `/mcp`, with the sandbox token
  as bearer. One MCP session per conversation; on a 404 (ferund restarted)
  the backend initializes once more.

## Structure

- `src/`: the SPA (React and Vite; Markdown with syntax highlighting, tool
  call chips, a field for the sandbox image). `src/backend.js` is the SSE
  client of `POST /api/v1/chat`.
- `backend/`: the Go backend. It streams SSE events to the SPA (`delta`,
  `tool_start`, `tool`, `history`, `sandbox`, `done`, `error`). Because it is
  stateless, the client carries the history: `history` events deliver, per
  completed round, the exact OpenAI messages (the assistant's `tool_calls`
  and the `tool` results, or the final answer), and the client sends them
  back unchanged with the next request. A client that sends back only the
  answer text teaches the model to claim tool use instead of calling tools.
  The backend has its own `go.mod`: it depends on FeRun's public API only
  and does not import the repository's `go/` module.
- `hack/history-e2e.py`: an end-to-end check against a running backend
  that the model calls its tools on every turn of a conversation.
- `Dockerfile`, `nginx.conf`: the SPA's image, served by nginx on port
  8080. `backend/Dockerfile`: the backend's image, a static binary
  (`CGO_ENABLED=0`).
- `deploy.sh [tag]`: builds both images, pushes them to `REGISTRY`
  (default oci.fecode.dev) and rolls both deployments to the tag.
- [`/deploy/k3s/fechat/`](../deploy/k3s/fechat/): the manifests for k3s,
  namespace `fechat`: the deployments and services `fechat-app` and
  `fechat-backend`, the ConfigMap `fechat-backend-config` (model and system
  prompt) and the ingress, with the deployment's parameters
  ([`deploy/k3s/README.md`](../deploy/k3s/README.md#parameters)).

Sign-in: the backend is an OpenID Connect client of id.fecode.dev (code
flow with PKCE and a nonce, `backend/auth.go`), and keeps the session in a
signed cookie, so it stays stateless. `/api/v1/auth/login`, `/callback`,
`/me` and `/logout` are its routes; the chat and the image check answer
signed-in users only, and the SPA sends the browser to sign in when they
answer 401.

The backend's routes: `POST /api/v1/chat`, `POST /api/v1/image-check` (an
anonymous manifest lookup, so public images only: a private registry
looks like "not found") and `GET /api/v1/healthz`. The SPA's default image
is `ghcr.io/maltej/debian-ai`; with the field empty, the backend sends no
image and ferund uses its default.

## Configuration

The backend reads its environment:

| variable             | from                          | meaning |
|----------------------|-------------------------------|---------|
| `OPENAI_API_KEY`     | Secret `fechat-backend`       | API key of the LLM gateway |
| `OPENAI_BASE_URL`    | Secret `fechat-backend`       | the gateway's OpenAI-compatible base URL, ending in `/v1` |
| `FERUN_TOKEN`        | Secret `fechat-backend`       | a FeRun tenant token, for provisioning |
| `FERUN_URL`          | the deployment                | ferund's base URL, default `http://ferund.ferun.svc.cluster.local:8100` |
| `FERUN_ENVIRONMENT`  | the deployment                | the tenant's environment the conversations' sandboxes are in (FeRun ADR 0035), by name or ID; required: `sandboxes` |
| `OIDC_ISSUER`        | the deployment                | the IdP, `https://id.fecode.dev/`; unset, the chat is open to everybody (development only) |
| `OIDC_CLIENT_ID`     | Secret `oidc-client-fechat`   | FeChat's client there |
| `OIDC_CLIENT_SECRET` | Secret `oidc-client-fechat`   | |
| `PUBLIC_URL`         | the deployment                | `https://chat.fecode.dev`: the callback is `PUBLIC_URL/api/v1/auth/callback` |
| `SESSION_SECRET`     | Secret `fechat-backend`       | at least 32 bytes; signs the session cookies (24 h) and the sandbox references; a new one gives open conversations new sandboxes |
| `MODEL`              | ConfigMap, key `model`        | the model, the deployment's `FECHAT_MODEL` |
| `SYSTEM_PROMPT_FILE` | ConfigMap, mounted at `/etc/fechat` | default `/etc/fechat/system-prompt.txt`; without the file, the backend's built-in prompt (`defaultSystemPrompt` in `backend/main.go`) |
| `PORT`               |                               | listen port, default 8080 |

## Secrets

None of them is in the repository. Create them once, in the namespace
`fechat`, after `kubectl apply -f deploy/k3s/fechat/namespace.yaml`. Keep
the local files below outside the repository, or at least out of git
(`fechat/.gitignore` ignores `*.env`).

**`oci-pull`**, for pulling the images from oci.fecode.dev:

```sh
kubectl -n fechat create secret docker-registry oci-pull \
  --docker-server=oci.fecode.dev \
  --docker-username='<registry user>' --docker-password='<registry password>'
```

**`fechat-backend`**, the backend's environment. Write a local file
`fechat-backend.env`:

```sh
OPENAI_API_KEY=<API key of the LLM gateway>
OPENAI_BASE_URL=<https://gateway.example/v1>
FERUN_TOKEN=<FeRun tenant token>
SESSION_SECRET=<32 random bytes or more, e.g. openssl rand -hex 32>
```

With the deployments of `deploy/`, the tenant token is ferund's bootstrap
token (`FERUN_BOOTSTRAP_TOKEN`, see [`deploy/k3s/README.md`](../deploy/k3s/README.md)).
Every sandbox is in an environment of the tenant (FeRun ADR 0035); make
FeChat's once, with that token:

```sh
curl -H "Authorization: Bearer $FERUN_TOKEN" -H 'Content-Type: application/json' -d '{"name":"sandboxes"}' \
  https://run.<DOMAIN>/api/v1/environments
```
Create the secret from the file:

```sh
kubectl -n fechat create secret generic fechat-backend \
  --from-env-file=fechat-backend.env
```

**`oidc-client-fechat`**, FeChat's client at the IdP (`OIDC_ISSUER`),
registered with the redirect URI `PUBLIC_URL/api/v1/auth/callback`
(`https://chat.fecode.dev/api/v1/auth/callback`):

```sh
kubectl -n fechat create secret generic oidc-client-fechat \
  --from-literal=client_id='<client ID>' --from-literal=client_secret='<client secret>'
```

Who may sign in is decided at id.fecode.dev: an admin invites people at
https://id.fecode.dev/admin.

To change a secret later, create it the same way with
`--dry-run=client -o yaml | kubectl apply -f -` appended, and restart the
backend for a new environment
(`kubectl -n fechat rollout restart deploy/fechat-backend`).

## Deploying

The cluster needs traefik (k3s's default ingress), cert-manager with the
ClusterIssuer `letsencrypt-prod`, DNS for chat.fecode.dev pointing at it,
and ferund at `FERUN_URL`. The manifests take the deployment's
parameters, which `make` fills in (`make -C deploy/gardener apply-fechat`
on the Gardener shoot). The first time, from the repository's root:

```sh
kubectl apply -f deploy/k3s/fechat/namespace.yaml
# create the three secrets above
make -C deploy/k3s apply-fechat
fechat/deploy.sh 0.5.0
```

From then on, `fechat/deploy.sh <tag>` builds and pushes both images and
rolls the deployments to them. It needs `docker login oci.fecode.dev`;
`REGISTRY` selects another registry (the deployment's), `DOCKER` another
client (`DOCKER="sudo docker"`, `DOCKER=podman`).
Give every deploy a new tag: the nodes keep an image they have
(`imagePullPolicy: IfNotPresent`). Applying `app.yaml` or `backend.yaml`
again (`make apply-fechat`) sets the tag written there, so raise it there
too, or run `deploy.sh` afterwards.

## Changing the system prompt

Edit `system-prompt.txt` in
[`deploy/k3s/fechat/config.yaml`](../deploy/k3s/fechat/config.yaml), then:

```sh
make -C deploy/k3s apply-fechat
```

The backend reads the prompt on every request, so it needs no restart; the
kubelet updates the mounted file within about a minute. A new `model` in
the same ConfigMap needs `kubectl -n fechat rollout restart
deploy/fechat-backend`. The prompt lists the tools ferund offers (bash,
read, write, edit, ls, rg, fd, webfetch); keep it in step with them.

## Building and testing

```sh
cd fechat/backend && go test ./...
cd fechat && npm ci && npm run build    # the SPA, into dist/
```
