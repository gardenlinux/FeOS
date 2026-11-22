# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
# SPDX-License-Identifier: Apache-2.0

# The image of feos-k8s, the controller of FeOS's nodes on Kubernetes
# (ADR 0020, docs/architecture/kubernetes.md): static, on a distroless
# base, as a non-root user. The build context is the repository's root:
#
#   docker build -f deploy/gardener/feos-k8s/Dockerfile -t feos-k8s .

# The Go of go/go.mod's toolchain line.
FROM golang:1.26.8 AS build
WORKDIR /src/go
COPY go/go.mod go/go.sum ./
# The published module (ADR 0057), which go.mod replaces with ./ferun.
COPY go/ferun/go.mod go/ferun/go.sum ./ferun/
RUN go mod download
COPY go/ ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/feos-k8s ./cmd/feos-k8s

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/feos-k8s /feos-k8s
# /healthz
EXPOSE 8080
ENTRYPOINT ["/feos-k8s"]
