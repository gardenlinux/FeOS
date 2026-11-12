// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ALPN is the protocol a connection of ferun-ingress negotiates on the
// nodes' port.
const ALPN = "ferun-ingress/1"

// ServiceName is the gRPC service of this package.
const ServiceName = "ferun.ingress.v1.IngressService"

// The full method names, as interceptors see them in FullMethod and as
// a call names them.
const (
	MethodJoin  = "/" + ServiceName + "/Join"
	MethodRenew = "/" + ServiceName + "/RenewCertificate"
	MethodHello = "/" + ServiceName + "/Hello"
	MethodSync  = "/" + ServiceName + "/Sync"
	MethodWake  = "/" + ServiceName + "/Wake"
	MethodHold  = "/" + ServiceName + "/Hold"

	MethodRedeemVisit = "/" + ServiceName + "/RedeemVisit"
	MethodRenewVisit  = "/" + ServiceName + "/RenewVisit"
	MethodEndVisit    = "/" + ServiceName + "/EndVisit"
)

// IngressServer is the server side of IngressService. An implementation
// embeds UnimplementedIngressServer by value, so that a method added
// here answers UNIMPLEMENTED until it implements it.
type IngressServer interface {
	// Join is served without a client certificate, on the join server.
	Join(context.Context, *JoinRequest) (*JoinResponse, error)
	RenewCertificate(context.Context, *RenewRequest) (*RenewResponse, error)
	Hello(context.Context, *HelloRequest) (*HelloResponse, error)
	// Sync streams the ingress's table until the stream ends; its
	// returned error is the stream's status.
	Sync(*SyncRequest, SyncServer) error
	Wake(context.Context, *WakeRequest) (*WakeResponse, error)
	Hold(context.Context, *HoldRequest) (*HoldResponse, error)
	// RedeemVisit, RenewVisit and EndVisit are the visits of ADR 0054.
	RedeemVisit(context.Context, *RedeemVisitRequest) (*RedeemVisitResponse, error)
	RenewVisit(context.Context, *RenewVisitRequest) (*RenewVisitResponse, error)
	EndVisit(context.Context, *EndVisitRequest) (*EndVisitResponse, error)
	mustEmbedUnimplementedIngressServer()
}

// SyncServer is the server's end of a Sync stream.
type SyncServer interface {
	Send(*Table) error
	grpc.ServerStream
}

// UnimplementedIngressServer answers UNIMPLEMENTED on every method.
type UnimplementedIngressServer struct{}

func (UnimplementedIngressServer) Join(context.Context, *JoinRequest) (*JoinResponse, error) {
	return nil, status.Error(codes.Unimplemented, "method Join not implemented")
}

func (UnimplementedIngressServer) RenewCertificate(context.Context, *RenewRequest) (*RenewResponse, error) {
	return nil, status.Error(codes.Unimplemented, "method RenewCertificate not implemented")
}

func (UnimplementedIngressServer) Hello(context.Context, *HelloRequest) (*HelloResponse, error) {
	return nil, status.Error(codes.Unimplemented, "method Hello not implemented")
}

func (UnimplementedIngressServer) Sync(*SyncRequest, SyncServer) error {
	return status.Error(codes.Unimplemented, "method Sync not implemented")
}

func (UnimplementedIngressServer) Wake(context.Context, *WakeRequest) (*WakeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "method Wake not implemented")
}

func (UnimplementedIngressServer) Hold(context.Context, *HoldRequest) (*HoldResponse, error) {
	return nil, status.Error(codes.Unimplemented, "method Hold not implemented")
}

func (UnimplementedIngressServer) RedeemVisit(context.Context, *RedeemVisitRequest) (*RedeemVisitResponse, error) {
	return nil, status.Error(codes.Unimplemented, "method RedeemVisit not implemented")
}

func (UnimplementedIngressServer) RenewVisit(context.Context, *RenewVisitRequest) (*RenewVisitResponse, error) {
	return nil, status.Error(codes.Unimplemented, "method RenewVisit not implemented")
}

func (UnimplementedIngressServer) EndVisit(context.Context, *EndVisitRequest) (*EndVisitResponse, error) {
	return nil, status.Error(codes.Unimplemented, "method EndVisit not implemented")
}

func (UnimplementedIngressServer) mustEmbedUnimplementedIngressServer() {}

// RegisterIngressServer registers srv with s. The server must force
// Codec (grpc.ForceServerCodec(Codec{})).
func RegisterIngressServer(s grpc.ServiceRegistrar, srv IngressServer) {
	s.RegisterService(&ServiceDesc, srv)
}

// ServiceDesc describes IngressService to grpc, written by hand as
// protoc-gen-go-grpc would generate it.
var ServiceDesc = grpc.ServiceDesc{
	ServiceName: ServiceName,
	HandlerType: (*IngressServer)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "Join", Handler: unaryHandler(MethodJoin, IngressServer.Join)},
		{MethodName: "RenewCertificate", Handler: unaryHandler(MethodRenew, IngressServer.RenewCertificate)},
		{MethodName: "Hello", Handler: unaryHandler(MethodHello, IngressServer.Hello)},
		{MethodName: "Wake", Handler: unaryHandler(MethodWake, IngressServer.Wake)},
		{MethodName: "Hold", Handler: unaryHandler(MethodHold, IngressServer.Hold)},
		{MethodName: "RedeemVisit", Handler: unaryHandler(MethodRedeemVisit, IngressServer.RedeemVisit)},
		{MethodName: "RenewVisit", Handler: unaryHandler(MethodRenewVisit, IngressServer.RenewVisit)},
		{MethodName: "EndVisit", Handler: unaryHandler(MethodEndVisit, IngressServer.EndVisit)},
	},
	Streams: []grpc.StreamDesc{
		{StreamName: "Sync", Handler: syncHandler, ServerStreams: true},
	},
	Metadata: "go/internal/ingress/wire",
}

// unaryHandler is a unary method's handler as generated code has it:
// decode the request, then call the method through the server's
// interceptor, or directly without one.
func unaryHandler[Req, Res any](fullMethod string, method func(IngressServer, context.Context, *Req) (*Res, error)) grpc.MethodHandler {
	return func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		in := new(Req)
		if err := dec(in); err != nil {
			return nil, err
		}
		if interceptor == nil {
			return method(srv.(IngressServer), ctx, in)
		}
		info := &grpc.UnaryServerInfo{Server: srv, FullMethod: fullMethod}
		handler := func(ctx context.Context, req any) (any, error) {
			return method(srv.(IngressServer), ctx, req.(*Req))
		}
		return interceptor(ctx, in, info, handler)
	}
}

// syncHandler receives the one SyncRequest and runs Sync on the stream.
// The server applies its stream interceptor around this handler.
func syncHandler(srv any, stream grpc.ServerStream) error {
	in := new(SyncRequest)
	if err := stream.RecvMsg(in); err != nil {
		return err
	}
	return srv.(IngressServer).Sync(in, &grpc.GenericServerStream[SyncRequest, Table]{ServerStream: stream})
}
