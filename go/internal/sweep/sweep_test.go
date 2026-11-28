// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sweep

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/store"
)

// fakeConns answers SweepObjectStore per node: an error, or a report.
type fakeConns struct {
	errs  map[string]error
	calls []*podv1.SweepObjectStoreRequest
	nodes []string
}

func (f *fakeConns) Connected(string) bool { return true }

func (f *fakeConns) Conn(_ context.Context, node string) (grpc.ClientConnInterface, error) {
	return fakeConn{f: f, node: node}, nil
}

type fakeConn struct {
	f    *fakeConns
	node string
}

func (c fakeConn) Invoke(_ context.Context, method string, args, reply any, _ ...grpc.CallOption) error {
	if method != podv1.PodService_SweepObjectStore_FullMethodName {
		return status.Error(codes.Unimplemented, method)
	}
	if err := c.f.errs[c.node]; err != nil {
		return err
	}
	c.f.calls = append(c.f.calls, args.(*podv1.SweepObjectStoreRequest))
	c.f.nodes = append(c.f.nodes, c.node)
	reply.(*podv1.SweepObjectStoreResponse).ChunksDeleted = 7
	return nil
}

func (fakeConn) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

type extraLive struct{}

func (extraLive) LiveSet(context.Context) ([]string, []string, error) {
	return []string{"snapshots/volume-b/1/head.json"}, nil, nil
}

func newSweeper(mode string, conns *fakeConns, live store.SweepLive, extra LiveSource) *Sweeper {
	s := New(Config{Mode: mode, Every: 0, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, nil, conns, extra)
	s.liveSet = func(context.Context, string) (store.SweepLive, error) { return live, nil }
	return s
}

// A sweep names the rows' heads, the catalog's layers and the backups'
// heads; it is a dry run unless it is on; a node whose feosd does not
// know the call is passed over; an empty live set sweeps nothing.
func TestSweepStore(t *testing.T) {
	ctx := context.Background()
	live := store.SweepLive{Heads: []string{"snapshots/a/1/head.json"}, Layers: []string{"l1"}}
	conns := &fakeConns{errs: map[string]error{"old": status.Error(codes.Unimplemented, "unknown method")}}
	s := newSweeper(config.SweepDryRun, conns, live, extraLive{})
	if err := s.sweepStore(ctx, "store-1", []string{"old", "new"}); err != nil {
		t.Fatal(err)
	}
	if len(conns.calls) != 1 || conns.nodes[0] != "new" {
		t.Fatalf("calls %v on %v", conns.calls, conns.nodes)
	}
	c := conns.calls[0]
	if !c.DryRun || len(c.LiveHeads) != 2 || c.LiveHeads[1] != "snapshots/volume-b/1/head.json" || len(c.LiveLayers) != 1 {
		t.Fatalf("request %+v", c)
	}

	on := &fakeConns{}
	if err := newSweeper(config.SweepOn, on, live, nil).sweepStore(ctx, "store-1", []string{"n"}); err != nil {
		t.Fatal(err)
	}
	if len(on.calls) != 1 || on.calls[0].DryRun {
		t.Fatalf("on: %+v", on.calls)
	}

	empty := &fakeConns{}
	if err := newSweeper(config.SweepOn, empty, store.SweepLive{Layers: []string{"l1"}}, nil).sweepStore(ctx, "store-1",
		[]string{"n"}); err != nil {
		t.Fatal(err)
	}
	if len(empty.calls) != 0 {
		t.Fatalf("an empty live set swept: %+v", empty.calls)
	}

	failing := &fakeConns{errs: map[string]error{"n": status.Error(codes.Internal, "S3 not configured")}}
	if err := newSweeper(config.SweepOn, failing, live, nil).sweepStore(ctx, "store-1", []string{"n"}); err == nil {
		t.Fatal("a failed sweep reported no error")
	}
}

// The sweep's loop exists only when it is not off.
func TestLoops(t *testing.T) {
	if n := len(newSweeper(config.SweepOff, &fakeConns{}, store.SweepLive{}, nil).Loops()); n != 0 {
		t.Fatalf("%d loops when off", n)
	}
	if n := len(newSweeper(config.SweepDryRun, &fakeConns{}, store.SweepLive{}, nil).Loops()); n != 1 {
		t.Fatalf("%d loops in dry run", n)
	}
}
