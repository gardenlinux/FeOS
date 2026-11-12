# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
# SPDX-License-Identifier: Apache-2.0

# The image of ferund, FeRun (ADR 0015): the management UI (web/) built
# and embedded, ferund built static, on a distroless base, as a non-root
# user. It carries ferun-ingress as well (ADR 0021), built static into
# /ferun-ingress, whose pods run the same image with that entrypoint.
# ferun-sink (ADR 0026), a service of its own, is an image of its own, the
# stage ferun-sink. The build context is the repository's root, which
# holds both:
#
#   docker build -f go/Dockerfile -t ferund .
#   docker build -f go/Dockerfile --target ferun-sink -t ferun-sink .
#
# Termination grace: on SIGTERM an instance keeps serving its requests and
# finishing its ops for up to --shutdown-grace (10 minutes by default),
# then closes its node connections. A container deployment sets its
# termination grace period (Kubernetes: terminationGracePeriodSeconds) to
# at least --shutdown-grace plus 30 s; the default of 30 s would kill
# ferund long before the tool calls in flight end.
#
# ferun-ingress runs as root with NET_ADMIN (its WireGuard device), which
# its pod's securityContext sets over the image's user; its init
# container runs `/ferun-ingress prepare`, privileged. On SIGTERM it serves
# on for --shutdown-delay (10 s), then waits for its requests for up to
# --shutdown-grace (40 s) and releases its holds within 2 s: its pods set a
# termination grace above the sum, 60 s.
#
# It carries ferun-auth too (ADR 0054), the access provider of the kinds
# ferun and oidc, built static into /ferun-auth, whose pods run the image
# with that entrypoint as its nonroot user. On SIGTERM its /readyz fails
# for --shutdown-delay (5 s), then its requests have 10 s: Kubernetes'
# default termination grace of 30 s covers both.

# The management UI. The stage keeps the checkout's layout (/src/web beside
# /src/go), so that the build writes go/internal/ui/dist as
# web/vite.config.js names it.
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# The Go of go/go.mod's toolchain line.
FROM golang:1.26.8 AS build
WORKDIR /src/go
COPY go/go.mod go/go.sum ./
RUN go mod download
COPY go/ ./
# The UI of the web stage, embedded by internal/ui; a build the checkout
# holds in go/internal/ui/dist is not.
RUN rm -rf internal/ui/dist
COPY --from=web /src/go/internal/ui/dist/ ./internal/ui/dist/
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/ferund ./cmd/ferund
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/ferun-ingress ./cmd/ferun-ingress
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/ferun-sink ./cmd/ferun-sink
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/ferun-auth ./cmd/ferun-auth

# ferun-sink's image: the program alone, its API and its ingest.
FROM gcr.io/distroless/static-debian12:nonroot AS ferun-sink
COPY --from=build /out/ferun-sink /ferun-sink
EXPOSE 8080 4317
ENTRYPOINT ["/ferun-sink"]
CMD ["serve"]

# ferund's image, the default target.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ferund /ferund
COPY --from=build /out/ferun-ingress /ferun-ingress
COPY --from=build /out/ferun-auth /ferun-auth
# The front doors (REST, MCP, the UI, the ports) and the nodes' port.
EXPOSE 8100 7443
ENTRYPOINT ["/ferund"]
CMD ["serve"]
