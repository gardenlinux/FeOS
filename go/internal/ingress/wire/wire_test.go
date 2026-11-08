// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
)

var t0 = time.Date(2026, 10, 28, 12, 30, 45, 123456789, time.UTC)

func TestCodecRoundTrip(t *testing.T) {
	c := Codec{}
	if c.Name() != "json" || ContentType != "application/grpc+json" {
		t.Errorf("Name %q, ContentType %q", c.Name(), ContentType)
	}
	in := &Table{
		Seq:   7,
		Whole: true,
		Envs: []Env{{
			ID: "e1", Tenant: "acme", Name: "prod", Hosts: []string{"a.apps.test"}, Deployment: 3, Hash: "h",
			Routes: []Route{{Name: "web", Path: "/", Service: "web", Port: "http", Strip: true},
				{Name: "admin", Path: "/admin", Service: "web", Port: "http", From: []string{"192.0.2.0/24", "2001:db8::/32"}}},
			Services: []Service{{
				Name: "web", Idle: true, Asleep: 1, Waking: 2, Pending: 3,
				Endpoints: []Endpoint{{
					Instance: "i1", Address: "fd00::1", Node: "n1",
					Ports: map[string]int{"http": 8080, "metrics": 9090}, GroupVersion: 1 << 40,
				}},
			}},
		}},
		Removed: []string{"e0"},
		Mesh:    &Mesh{Version: 4, Peers: []Peer{{Name: "n1", PublicKey: []byte{0, 1, 2, 0xff}, Endpoint: "[2001:db8::1]:51820", Prefix: "fd00:0:0:1::/64"}}},
		Self:    &Self{Name: "ingress-0", Address: "fd00::1", AppsZone: "apps.test", AdmittedVersion: 9},
		SentAt:  t0,
	}
	b, err := c.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out := new(Table)
	if err := c.Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip\n got %+v\nwant %+v", out, in)
	}

	cert := Certificate{
		SpiffeID:    "spiffe://feos.test/ingress/ingress-0",
		Chain:       [][]byte{{1, 2, 3}, {4, 5}},
		TrustBundle: [][]byte{{6}},
		NotBefore:   t0.In(time.FixedZone("CET", 3600)),
		NotAfter:    t0.Add(time.Hour),
		RenewAfter:  t0.Add(30 * time.Minute),
	}
	b, err = c.Marshal(&JoinResponse{Name: "ingress-0", Certificate: cert})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"spiffeId":`, `"trustBundle":["Bg=="]`, `"chain":["AQID","BAU="]`, `"notBefore":"2026-10-28T13:30:45.123456789+01:00"`, `"renewAfter":`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s lacks %s", b, want)
		}
	}
	var jr JoinResponse
	if err := c.Unmarshal(b, &jr); err != nil {
		t.Fatal(err)
	}
	if got := jr.Certificate; !got.NotBefore.Equal(cert.NotBefore) || !got.NotAfter.Equal(cert.NotAfter) ||
		!got.RenewAfter.Equal(cert.RenewAfter) || !reflect.DeepEqual(got.Chain, cert.Chain) ||
		!reflect.DeepEqual(got.TrustBundle, cert.TrustBundle) || got.SpiffeID != cert.SpiffeID {
		t.Errorf("certificate %+v, want %+v", got, cert)
	}

	// Unknown fields are ignored, missing fields are zero values.
	var hr HelloResponse
	if err := c.Unmarshal([]byte(`{"name":"ingress-0","future":{"x":[1,2]},"prefix":"fd00::/64","later":true}`), &hr); err != nil {
		t.Fatalf("unknown fields: %v", err)
	}
	if hr != (HelloResponse{Name: "ingress-0", Prefix: "fd00::/64"}) {
		t.Errorf("HelloResponse %+v", hr)
	}
	// Empty data is the zero message.
	var sr SyncRequest
	if err := c.Unmarshal(nil, &sr); err != nil {
		t.Errorf("empty data: %v", err)
	}
	if err := c.Unmarshal([]byte(`{"name":`), &hr); err == nil {
		t.Error("truncated JSON accepted")
	}
	if _, err := c.Marshal(func() {}); err == nil {
		t.Error("a func marshalled")
	}
}

// TestJSONNames checks the compatibility rule's fixed names: every field
// of every message has a camelCase JSON name of its own.
func TestJSONNames(t *testing.T) {
	for _, v := range []any{
		Certificate{}, JoinRequest{}, JoinResponse{}, RenewRequest{}, RenewResponse{},
		HelloRequest{}, HelloResponse{}, SyncRequest{}, Table{}, Mesh{}, Peer{}, Self{},
		Env{}, Route{}, Service{}, Endpoint{}, WakeRequest{}, WakeResponse{},
		HoldRequest{}, HoldRefusal{}, HoldResponse{}, State{}, Domain{}, Challenges{}, Challenge{},
	} {
		typ := reflect.TypeOf(v)
		seen := map[string]bool{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if !f.IsExported() {
				continue // not coded
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || !unicode.IsLower(rune(name[0])) || strings.ContainsAny(name, "_-") {
				t.Errorf("%s.%s: JSON name %q is not camelCase", typ.Name(), f.Name, name)
			}
			if !strings.EqualFold(name, f.Name) {
				t.Errorf("%s.%s: JSON name %q differs from the field's", typ.Name(), f.Name, name)
			}
			if seen[name] {
				t.Errorf("%s: JSON name %q twice", typ.Name(), name)
			}
			seen[name] = true
		}
	}
}

func TestCertificateFromProto(t *testing.T) {
	if c := CertificateFromProto(nil); !reflect.DeepEqual(c, Certificate{}) {
		t.Errorf("nil: %+v", c)
	}
	pc := &fleetv1.Certificate{
		SpiffeId:    "spiffe://feos.test/ingress/a",
		Chain:       [][]byte{{1}, {2}},
		TrustBundle: [][]byte{{3}},
		NotBefore:   timestamppb.New(t0),
		NotAfter:    timestamppb.New(t0.Add(time.Hour)),
		RenewAfter:  timestamppb.New(t0.Add(30 * time.Minute)),
	}
	c := CertificateFromProto(pc)
	if c.SpiffeID != pc.SpiffeId || !reflect.DeepEqual(c.Chain, pc.Chain) || !reflect.DeepEqual(c.TrustBundle, pc.TrustBundle) ||
		!c.NotBefore.Equal(t0) || !c.NotAfter.Equal(t0.Add(time.Hour)) || !c.RenewAfter.Equal(t0.Add(30*time.Minute)) {
		t.Errorf("%+v", c)
	}
	c.Chain[0][0] = 9
	if pc.Chain[0][0] != 1 {
		t.Error("the chain is shared with the proto")
	}
	if c := CertificateFromProto(&fleetv1.Certificate{SpiffeId: "x"}); !c.NotAfter.IsZero() || !c.RenewAfter.IsZero() {
		t.Errorf("missing times: %+v", c)
	}
}

// fakeServer answers every method with values derived from the request,
// or with an error status for the request values the tests pick.
type fakeServer struct {
	UnimplementedIngressServer
	tables []*Table
	end    error

	mu      sync.Mutex
	syncReq *SyncRequest
}

func (f *fakeServer) Join(_ context.Context, in *JoinRequest) (*JoinResponse, error) {
	if in.Token == "bad" {
		return nil, status.Error(codes.PermissionDenied, "unknown join token")
	}
	return &JoinResponse{Name: in.Name, Certificate: Certificate{
		SpiffeID: "spiffe://feos.test/ingress/" + in.Name, Chain: [][]byte{in.CSR}, NotAfter: t0,
	}}, nil
}

func (f *fakeServer) RenewCertificate(_ context.Context, in *RenewRequest) (*RenewResponse, error) {
	return &RenewResponse{Certificate: Certificate{Chain: [][]byte{in.CSR}, RenewAfter: t0}}, nil
}

func (f *fakeServer) Hello(_ context.Context, in *HelloRequest) (*HelloResponse, error) {
	if in.Endpoint == "" {
		return nil, status.Error(codes.FailedPrecondition, "the cluster is not meshed; the ingress needs the pods' mesh")
	}
	return &HelloResponse{
		Name: "ingress-0", PodNetwork: "fd00::/48", Prefix: "fd00:0:0:5::/64", Address: "fd00:0:0:5::1",
		AppsZone: "apps.test", Instance: in.Version + "@" + in.Endpoint + "/" + string(in.PublicKey),
	}, nil
}

func (f *fakeServer) Sync(in *SyncRequest, stream SyncServer) error {
	f.mu.Lock()
	f.syncReq = in
	f.mu.Unlock()
	for _, t := range f.tables {
		if err := stream.Send(t); err != nil {
			return err
		}
	}
	return f.end
}

func (f *fakeServer) Wake(_ context.Context, in *WakeRequest) (*WakeResponse, error) {
	if in.Env == "missing" {
		return nil, status.Errorf(codes.NotFound, "service %s of %s is not routed", in.Service, in.Env)
	}
	return &WakeResponse{Waking: 2, RetryAfterSeconds: 5}, nil
}

func (f *fakeServer) Hold(_ context.Context, in *HoldRequest) (*HoldResponse, error) {
	out := &HoldResponse{LeaseSeconds: 30}
	for _, id := range in.Hold {
		if id == "bad" {
			out.Refused = append(out.Refused, HoldRefusal{Instance: id, Reason: "invalid"})
			continue
		}
		out.Held = append(out.Held, id)
	}
	if in.Leaving {
		out.LeaseSeconds = 0
	}
	return out, nil
}

// recorder is a pair of interceptors that record the method and the
// content type of each call.
type recorder struct {
	mu    sync.Mutex
	calls map[string]string // full method → content-type
}

func (r *recorder) record(ctx context.Context, method string) {
	md, _ := metadata.FromIncomingContext(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[method] = strings.Join(md.Get("content-type"), ",")
}

func (r *recorder) options() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			r.record(ctx, info.FullMethod)
			return h(ctx, req)
		}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			r.record(ss.Context(), info.FullMethod)
			return h(srv, ss)
		}),
	}
}

// serve runs srv on a bufconn listener with the codec forced and returns
// a client connection to it.
func serve(t *testing.T, srv IngressServer, opts ...grpc.ServerOption) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	s := grpc.NewServer(append([]grpc.ServerOption{grpc.ForceServerCodec(Codec{})}, opts...)...)
	RegisterIngressServer(s, srv)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Serve(lis)
	}()
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cc.Close()
		s.Stop()
		<-done
	})
	return cc
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func wantCode(t *testing.T, what string, err error, code codes.Code, msg string) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok || st.Code() != code || (msg != "" && st.Message() != msg) {
		t.Errorf("%s: %v, want %v %q", what, err, code, msg)
	}
}

func TestUnary(t *testing.T) {
	for _, intercepted := range []bool{false, true} {
		rec := &recorder{calls: map[string]string{}}
		var opts []grpc.ServerOption
		if intercepted {
			opts = rec.options()
		}
		c := NewClient(serve(t, &fakeServer{}, opts...))
		ctx := testContext(t)

		jr, err := c.Join(ctx, &JoinRequest{Token: "tok", Name: "ingress-0", CSR: []byte{0, 1, 0xfe}})
		if err != nil {
			t.Fatal(err)
		}
		if jr.Name != "ingress-0" || jr.Certificate.SpiffeID != "spiffe://feos.test/ingress/ingress-0" ||
			!reflect.DeepEqual(jr.Certificate.Chain, [][]byte{{0, 1, 0xfe}}) || !jr.Certificate.NotAfter.Equal(t0) {
			t.Errorf("Join %+v", jr)
		}
		_, err = c.Join(ctx, &JoinRequest{Token: "bad"})
		wantCode(t, "Join", err, codes.PermissionDenied, "unknown join token")

		rr, err := c.RenewCertificate(ctx, &RenewRequest{CSR: []byte("csr")})
		if err != nil || string(rr.Certificate.Chain[0]) != "csr" || !rr.Certificate.RenewAfter.Equal(t0) {
			t.Errorf("RenewCertificate %+v %v", rr, err)
		}

		hr, err := c.Hello(ctx, &HelloRequest{PublicKey: []byte("k"), Endpoint: "[2001:db8::2]:51820", Version: "v1"})
		if err != nil || hr.Instance != "v1@[2001:db8::2]:51820/k" || hr.Prefix != "fd00:0:0:5::/64" || hr.AppsZone != "apps.test" {
			t.Errorf("Hello %+v %v", hr, err)
		}
		_, err = c.Hello(ctx, &HelloRequest{})
		wantCode(t, "Hello", err, codes.FailedPrecondition, "the cluster is not meshed; the ingress needs the pods' mesh")

		wr, err := c.Wake(ctx, &WakeRequest{Env: "e1", Service: "web"})
		if err != nil || *wr != (WakeResponse{Waking: 2, RetryAfterSeconds: 5}) {
			t.Errorf("Wake %+v %v", wr, err)
		}
		_, err = c.Wake(ctx, &WakeRequest{Env: "missing", Service: "web"})
		wantCode(t, "Wake", err, codes.NotFound, "service web of missing is not routed")

		hold, err := c.Hold(ctx, &HoldRequest{Hold: []string{"a", "bad", "b"}, Release: []string{"c"}})
		if err != nil || !reflect.DeepEqual(hold.Held, []string{"a", "b"}) ||
			!reflect.DeepEqual(hold.Refused, []HoldRefusal{{Instance: "bad", Reason: "invalid"}}) || hold.LeaseSeconds != 30 {
			t.Errorf("Hold %+v %v", hold, err)
		}
		if hold, err := c.Hold(ctx, &HoldRequest{Leaving: true}); err != nil || hold.LeaseSeconds != 0 || hold.Held != nil {
			t.Errorf("Hold leaving %+v %v", hold, err)
		}

		if intercepted {
			// The interceptor saw every unary method by its full name,
			// with the JSON content type.
			for _, m := range []string{MethodJoin, MethodRenew, MethodHello, MethodWake, MethodHold} {
				rec.mu.Lock()
				ct, ok := rec.calls[m]
				rec.mu.Unlock()
				if !ok || ct != ContentType {
					t.Errorf("%s: seen %v, content-type %q", m, ok, ct)
				}
			}
		}
	}
}

func TestSync(t *testing.T) {
	tables := []*Table{
		{Seq: 1, Whole: true, Envs: []Env{{ID: "e1", Hash: "a"}}, Mesh: &Mesh{Version: 1}, Self: &Self{Name: "ingress-0"}, SentAt: t0},
		{Seq: 2, Envs: []Env{{ID: "e2", Hash: "b"}}, SentAt: t0.Add(time.Second)},
		{Seq: 3, Removed: []string{"e1"}, SentAt: t0.Add(2 * time.Second)},
	}
	srv := &fakeServer{tables: tables, end: status.Error(codes.Unavailable, "instance stopping")}
	rec := &recorder{calls: map[string]string{}}
	c := NewClient(serve(t, srv, rec.options()...))
	stream, err := c.Sync(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	st := NewState()
	for i, want := range tables {
		got, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv %d: %v", i, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("table %d: %+v, want %+v", i, got, want)
		}
		if _, err := st.Apply(got); err != nil {
			t.Errorf("Apply %d: %v", i, err)
		}
	}
	_, err = stream.Recv()
	wantCode(t, "Sync", err, codes.Unavailable, "instance stopping")
	if len(st.Envs) != 1 || st.Envs["e2"].Hash != "b" || st.Seq != 3 || st.Mesh.Version != 1 || st.Self.Name != "ingress-0" {
		t.Errorf("state %+v", st)
	}
	srv.mu.Lock()
	if srv.syncReq == nil {
		t.Error("Sync got no request")
	}
	srv.mu.Unlock()
	rec.mu.Lock()
	if ct := rec.calls[MethodSync]; ct != ContentType {
		t.Errorf("Sync content-type %q", ct)
	}
	rec.mu.Unlock()

	// A stream the server ends with OK ends with io.EOF.
	c = NewClient(serve(t, &fakeServer{tables: tables[:1]}))
	stream, err = c.Sync(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("end of stream: %v, want io.EOF", err)
	}
}

func TestUnimplemented(t *testing.T) {
	type bare struct{ UnimplementedIngressServer }
	cc := serve(t, bare{})
	c := NewClient(cc)
	ctx := testContext(t)
	_, err := c.Join(ctx, &JoinRequest{})
	wantCode(t, "Join", err, codes.Unimplemented, "method Join not implemented")
	_, err = c.RenewCertificate(ctx, &RenewRequest{})
	wantCode(t, "RenewCertificate", err, codes.Unimplemented, "")
	_, err = c.Hello(ctx, &HelloRequest{})
	wantCode(t, "Hello", err, codes.Unimplemented, "")
	_, err = c.Wake(ctx, &WakeRequest{})
	wantCode(t, "Wake", err, codes.Unimplemented, "")
	_, err = c.Hold(ctx, &HoldRequest{})
	wantCode(t, "Hold", err, codes.Unimplemented, "")
	stream, err := c.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	wantCode(t, "Sync", err, codes.Unimplemented, "method Sync not implemented")

	// A method this end does not know, as an older server meets a newer
	// client.
	err = cc.Invoke(ctx, "/"+ServiceName+"/Future", &struct{}{}, &struct{}{}, grpc.ForceCodec(Codec{}))
	wantCode(t, "Future", err, codes.Unimplemented, "")
}

// TestMessageCompatibility sends a request with a field the server does
// not know, and reads an answer that lacks fields, over the codec.
func TestMessageCompatibility(t *testing.T) {
	cc := serve(t, &fakeServer{})
	ctx := testContext(t)
	type newerHello struct {
		HelloRequest
		Capabilities []string `json:"capabilities"`
	}
	var out struct {
		Name string `json:"name"`
	}
	in := newerHello{HelloRequest: HelloRequest{Endpoint: "e:1", Version: "v2"}, Capabilities: []string{"x"}}
	if err := cc.Invoke(ctx, MethodHello, &in, &out, grpc.ForceCodec(Codec{})); err != nil || out.Name != "ingress-0" {
		t.Errorf("newer request: %+v %v", out, err)
	}
	if b, _ := json.Marshal(in); !strings.Contains(string(b), `"capabilities"`) {
		t.Errorf("the newer request lacks its field: %s", b)
	}
}

// A route without clients' blocks codes as before the field (so an
// unchanged environment keeps its hash), and an older ingress, which
// does not know the field, reads a route with them.
func TestRouteFromCompatible(t *testing.T) {
	c := Codec{}
	b, err := c.Marshal(&Route{Name: "web", Path: "/", Service: "web", Port: "http"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"name":"web","path":"/","service":"web","port":"http","strip":false,"door":false}`; string(b) != want {
		t.Errorf("without from: %s, want %s", b, want)
	}
	b, err = c.Marshal(&Route{Name: "web", Path: "/", From: []string{"::/0"}})
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := c.Unmarshal(b, &old); err != nil || old.Name != "web" || old.Path != "/" {
		t.Errorf("an older reader: %+v, %v", old, err)
	}
}
