// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"context"
	"errors"
	"io"

	"google.golang.org/grpc"
)

// Client is the client side of IngressService on one connection. Every
// call forces Codec, so the connection needs no codec of its own.
type Client struct {
	cc grpc.ClientConnInterface
}

// NewClient returns a client on cc.
func NewClient(cc grpc.ClientConnInterface) *Client {
	return &Client{cc: cc}
}

// SyncClient is the client's end of a Sync stream: Recv returns the
// tables in order, then io.EOF or the stream's status.
type SyncClient interface {
	Recv() (*Table, error)
	grpc.ClientStream
}

func callOptions(opts []grpc.CallOption) []grpc.CallOption {
	return append([]grpc.CallOption{grpc.StaticMethod(), grpc.ForceCodec(Codec{})}, opts...)
}

func invoke[Req, Res any](ctx context.Context, cc grpc.ClientConnInterface, method string, in *Req, opts []grpc.CallOption) (*Res, error) {
	out := new(Res)
	if err := cc.Invoke(ctx, method, in, out, callOptions(opts)...); err != nil {
		return nil, err
	}
	return out, nil
}

// Join joins with a one-time token, on a connection without a client
// certificate.
func (c *Client) Join(ctx context.Context, in *JoinRequest, opts ...grpc.CallOption) (*JoinResponse, error) {
	return invoke[JoinRequest, JoinResponse](ctx, c.cc, MethodJoin, in, opts)
}

// RenewCertificate renews the certificate the connection presents.
func (c *Client) RenewCertificate(ctx context.Context, in *RenewRequest, opts ...grpc.CallOption) (*RenewResponse, error) {
	return invoke[RenewRequest, RenewResponse](ctx, c.cc, MethodRenew, in, opts)
}

// Hello takes the ingress's place on the pods' mesh.
func (c *Client) Hello(ctx context.Context, in *HelloRequest, opts ...grpc.CallOption) (*HelloResponse, error) {
	return invoke[HelloRequest, HelloResponse](ctx, c.cc, MethodHello, in, opts)
}

// Wake asks to wake a service's asleep instances.
func (c *Client) Wake(ctx context.Context, in *WakeRequest, opts ...grpc.CallOption) (*WakeResponse, error) {
	return invoke[WakeRequest, WakeResponse](ctx, c.cc, MethodWake, in, opts)
}

// Hold holds and releases instances.
func (c *Client) Hold(ctx context.Context, in *HoldRequest, opts ...grpc.CallOption) (*HoldResponse, error) {
	return invoke[HoldRequest, HoldResponse](ctx, c.cc, MethodHold, in, opts)
}

// RedeemVisit redeems an accepted code for a visit's first token.
func (c *Client) RedeemVisit(ctx context.Context, in *RedeemVisitRequest, opts ...grpc.CallOption) (*RedeemVisitResponse, error) {
	return invoke[RedeemVisitRequest, RedeemVisitResponse](ctx, c.cc, MethodRedeemVisit, in, opts)
}

// RenewVisit asks for a visit's next token.
func (c *Client) RenewVisit(ctx context.Context, in *RenewVisitRequest, opts ...grpc.CallOption) (*RenewVisitResponse, error) {
	return invoke[RenewVisitRequest, RenewVisitResponse](ctx, c.cc, MethodRenewVisit, in, opts)
}

// EndVisit ends a visit whose visitor signed out.
func (c *Client) EndVisit(ctx context.Context, in *EndVisitRequest, opts ...grpc.CallOption) (*EndVisitResponse, error) {
	return invoke[EndVisitRequest, EndVisitResponse](ctx, c.cc, MethodEndVisit, in, opts)
}

// Sync opens the Sync stream: it sends the SyncRequest and closes the
// sending side. The stream ends with ctx.
func (c *Client) Sync(ctx context.Context, opts ...grpc.CallOption) (SyncClient, error) {
	stream, err := c.cc.NewStream(ctx, &ServiceDesc.Streams[0], MethodSync, callOptions(opts)...)
	if err != nil {
		return nil, err
	}
	x := &grpc.GenericClientStream[SyncRequest, Table]{ClientStream: stream}
	// io.EOF means the server ended the stream already: Recv returns its
	// status.
	if err := x.SendMsg(&SyncRequest{}); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err := x.CloseSend(); err != nil {
		return nil, err
	}
	return x, nil
}
