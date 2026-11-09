# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
# SPDX-License-Identifier: Apache-2.0

FROM golang:1.26.10-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o fechat-backend .

FROM alpine:3.22.6
RUN apk add --no-cache ca-certificates
COPY --from=build /app/fechat-backend /fechat-backend
EXPOSE 8080
ENTRYPOINT ["/fechat-backend"]
