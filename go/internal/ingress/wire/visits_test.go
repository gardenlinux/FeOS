// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func testVisitKeys() *VisitKeys {
	return &VisitKeys{TokenKID: "tkid-0123456789a", TokenKey: bytes.Repeat([]byte{1}, 32),
		ChallengeKID: "ckid-0123456789a", ChallengeKey: bytes.Repeat([]byte{2}, 32)}
}

// The visit keys replace the state's when a message carries them; a
// delta without them keeps them, a whole table without them leaves none.
func TestStateVisitKeys(t *testing.T) {
	s := NewState()
	one := testVisitKeys()
	mustApply(t, s, &Table{Seq: 1, Whole: true, VisitKeys: one})
	mustApply(t, s, &Table{Seq: 2, Envs: []Env{env("a", "1")}})
	if !reflect.DeepEqual(s.VisitKeys, one) {
		t.Fatalf("a delta without keys changed them: %+v", s.VisitKeys)
	}
	two := &VisitKeys{TokenKID: "t2", TokenKey: []byte{3}, ChallengeKID: "c2", ChallengeKey: []byte{4}}
	mustApply(t, s, &Table{Seq: 3, VisitKeys: two})
	if !reflect.DeepEqual(s.VisitKeys, two) {
		t.Fatalf("keys %+v", s.VisitKeys)
	}
	mustApply(t, s, &Table{Seq: 1, Whole: true})
	if s.VisitKeys != nil {
		t.Fatalf("a whole table without keys kept %+v", s.VisitKeys)
	}

	// Clone copies them down to their bytes, and the cache round trip
	// keeps them.
	mustApply(t, s, &Table{Seq: 1, Whole: true, VisitKeys: testVisitKeys()})
	c := s.Clone()
	if !reflect.DeepEqual(c.VisitKeys, s.VisitKeys) || c.VisitKeys == s.VisitKeys {
		t.Fatalf("clone %+v", c.VisitKeys)
	}
	c.VisitKeys.TokenKey[0], c.VisitKeys.ChallengeKey[0], c.VisitKeys.TokenKID = 9, 9, "changed"
	if !reflect.DeepEqual(s.VisitKeys, testVisitKeys()) {
		t.Errorf("changing the clone changed the state: %+v", s.VisitKeys)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back State
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back.VisitKeys, s.VisitKeys) {
		t.Errorf("cache round trip %+v %v", back.VisitKeys, err)
	}
	if (&State{}).Clone().VisitKeys != nil {
		t.Error("a state without keys clones some")
	}
}

// An environment without the fields of ADR 0054 codes as before them, so
// that an unchanged environment keeps its hash and an older ingress reads
// it as it did; with them, an older reader ignores them.
func TestEnvAccessCompatible(t *testing.T) {
	c := Codec{}
	e := Env{ID: "e1", Tenant: "acme", Name: "stage", Hosts: []string{"stage--acme.apps.test"}, Deployment: 2, Hash: "h"}
	b, err := c.Marshal(&e)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"e1","tenant":"acme","name":"stage","hosts":["stage--acme.apps.test"],"deployment":2,"hash":"h",` +
		`"routes":null,"services":null}`
	if string(b) != want {
		t.Errorf("without access:\n got %s\nwant %s", b, want)
	}
	e.Access, e.Provider, e.SignIn = AccessVisitors, "fecode", "https://fecode.example/api/v1/visit"
	b, err = c.Marshal(&e)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"access":"visitors"`, `"provider":"fecode"`, `"signIn":"https://fecode.example/api/v1/visit"`} {
		if !bytes.Contains(b, []byte(field)) {
			t.Errorf("%s lacks %s", b, field)
		}
	}
	var back Env
	if err := c.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, e) {
		t.Errorf("round trip %+v %v", back, err)
	}
	// A table without keys codes as before them; a Hello without
	// capabilities too.
	if b, _ := c.Marshal(&Table{Seq: 1}); bytes.Contains(b, []byte("visitKeys")) {
		t.Errorf("a table without keys: %s", b)
	}
	if b, _ := c.Marshal(&HelloRequest{Endpoint: "e:1"}); bytes.Contains(b, []byte("capabilities")) {
		t.Errorf("a Hello without capabilities: %s", b)
	}
}

// visitServer answers the three calls of visits.
type visitServer struct {
	UnimplementedIngressServer
	hello *HelloRequest
}

func (v *visitServer) Hello(_ context.Context, in *HelloRequest) (*HelloResponse, error) {
	v.hello = in
	return &HelloResponse{Name: "ingress-0"}, nil
}

func (v *visitServer) RedeemVisit(_ context.Context, in *RedeemVisitRequest) (*RedeemVisitResponse, error) {
	switch in.Code {
	case "used":
		return &RedeemVisitResponse{Outcome: OutcomeRefused, Reason: "used"}, nil
	case "bad":
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	return &RedeemVisitResponse{Outcome: OutcomeAdmitted, Token: in.Code + "/" + in.Binding + "/" + in.VisitBinding + "/" + in.Host}, nil
}

func (v *visitServer) RenewVisit(_ context.Context, in *RenewVisitRequest) (*RenewVisitResponse, error) {
	if in.Secret == "" {
		return &RenewVisitResponse{Outcome: OutcomeEnded, Reason: "unknown"}, nil
	}
	return &RenewVisitResponse{Outcome: OutcomeRenewed, Token: in.Token + "+"}, nil
}

func (v *visitServer) EndVisit(_ context.Context, in *EndVisitRequest) (*EndVisitResponse, error) {
	if in.Token == "" {
		return nil, status.Error(codes.Unavailable, "ferund is busy")
	}
	return &EndVisitResponse{}, nil
}

func TestVisitCalls(t *testing.T) {
	rec := &recorder{calls: map[string]string{}}
	srv := &visitServer{}
	c := NewClient(serve(t, srv, rec.options()...))
	ctx := testContext(t)

	if _, err := c.Hello(ctx, &HelloRequest{Endpoint: "e:1", Capabilities: []string{CapabilityVisits}}); err != nil ||
		!reflect.DeepEqual(srv.hello.Capabilities, []string{"visits"}) {
		t.Fatalf("Hello %+v %v", srv.hello, err)
	}
	rr, err := c.RedeemVisit(ctx, &RedeemVisitRequest{Code: "c", Binding: "b", VisitBinding: "v", Host: "h"})
	if err != nil || *rr != (RedeemVisitResponse{Outcome: OutcomeAdmitted, Token: "c/b/v/h"}) {
		t.Errorf("RedeemVisit %+v %v", rr, err)
	}
	rr, err = c.RedeemVisit(ctx, &RedeemVisitRequest{Code: "used"})
	if err != nil || *rr != (RedeemVisitResponse{Outcome: OutcomeRefused, Reason: "used"}) {
		t.Errorf("RedeemVisit used %+v %v", rr, err)
	}
	_, err = c.RedeemVisit(ctx, &RedeemVisitRequest{Code: "bad"})
	wantCode(t, "RedeemVisit", err, codes.InvalidArgument, "invalid request")
	nr, err := c.RenewVisit(ctx, &RenewVisitRequest{Token: "t", Secret: "s"})
	if err != nil || *nr != (RenewVisitResponse{Outcome: OutcomeRenewed, Token: "t+"}) {
		t.Errorf("RenewVisit %+v %v", nr, err)
	}
	nr, err = c.RenewVisit(ctx, &RenewVisitRequest{Token: "t"})
	if err != nil || *nr != (RenewVisitResponse{Outcome: OutcomeEnded, Reason: "unknown"}) {
		t.Errorf("RenewVisit unknown %+v %v", nr, err)
	}
	if _, err := c.EndVisit(ctx, &EndVisitRequest{Token: "t", Secret: "s"}); err != nil {
		t.Errorf("EndVisit %v", err)
	}
	_, err = c.EndVisit(ctx, &EndVisitRequest{})
	wantCode(t, "EndVisit", err, codes.Unavailable, "ferund is busy")
	for _, m := range []string{MethodRedeemVisit, MethodRenewVisit, MethodEndVisit} {
		rec.mu.Lock()
		ct, ok := rec.calls[m]
		rec.mu.Unlock()
		if !ok || ct != ContentType {
			t.Errorf("%s: seen %v, content-type %q", m, ok, ct)
		}
	}
	if MethodRedeemVisit != "/ferun.ingress.v1.IngressService/RedeemVisit" || MethodEndVisit != "/ferun.ingress.v1.IngressService/EndVisit" {
		t.Errorf("method names %s %s", MethodRedeemVisit, MethodEndVisit)
	}
}
