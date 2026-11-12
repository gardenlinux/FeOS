// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package wire is the protocol between ferund and ferun-ingress (ADR
// 0021): the service ferun.ingress.v1.IngressService on the nodes' port,
// behind its own ALPN, ferun-ingress/1. It holds the messages, the codec,
// the service descriptor, the client and the table state an ingress
// builds from the Sync stream. Both ends import it; it imports nothing of
// the control plane's (no nodelink, store or server), so that the ingress
// binary stays small.
//
// The service is gRPC, for what gRPC gives a long-lived control
// connection: deadlines, status codes, keepalive, one multiplexed
// connection, server streaming and the nodes' port's accept limits. Its
// messages are Go structs coded as JSON, and its descriptor is written
// by hand, because the build has no protoc: a .proto file would need a
// generator for every change, and extending FleetService would tie the
// ingress to the node protocol. Both ends force the codec (Codec, named
// "json"), so a call carries the content type application/grpc+json and
// never falls back to protobuf.
//
// The compatibility rule is protobuf's, kept by hand: a field is added,
// never renamed, retyped or reused, and its JSON name (camelCase, from
// its tag) never changes. A receiver ignores fields it does not know and
// reads a missing field as its zero value, so a new field must mean
// "as before" at its zero value. A method is added, never changed; an
// end that lacks one answers UNIMPLEMENTED, which the caller treats as
// "not supported here". Where an older receiver would act wrongly on what
// it ignores (an environment that admits visitors only, ADR 0054), the
// ingress names what it does in its Hello's Capabilities, and ferund
// sends such a thing only to an ingress that named it.
package wire
