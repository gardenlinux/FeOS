// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sweep

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/store"
)

// fakeConns answers SweepObjectStore per node (an error, or a report)
// and ListLiveObjects (an error, or the node's name as a head and a
// layer).
type fakeConns struct {
	errs     map[string]error
	liveErrs map[string]error
	calls    []*podv1.SweepObjectStoreRequest
	nodes    []string
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
	if method == podv1.PodService_ListLiveObjects_FullMethodName {
		if err := c.f.liveErrs[c.node]; err != nil {
			return err
		}
		r := reply.(*podv1.ListLiveObjectsResponse)
		r.Heads, r.Layers = []string{"snapshots/volume-" + c.node + "/1/head.json"}, []string{"layer-" + c.node}
		return nil
	}
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
	wantHeads := []string{"snapshots/a/1/head.json", "snapshots/volume-b/1/head.json", "snapshots/volume-new/1/head.json",
		"snapshots/volume-old/1/head.json"}
	if !c.DryRun || !slices.Equal(c.LiveHeads, wantHeads) || !slices.Equal(c.LiveLayers, []string{"l1", "layer-new", "layer-old"}) {
		t.Fatalf("request %+v", c)
	}

	// A node that cannot say what it needs stops the sweep of its store.
	blind := &fakeConns{liveErrs: map[string]error{"old": status.Error(codes.Unimplemented, "unknown method")}}
	if err := newSweeper(config.SweepOn, blind, live, nil).sweepStore(ctx, "store-1", []string{"old", "new"}); err != nil {
		t.Fatal(err)
	}
	if len(blind.calls) != 0 {
		t.Fatalf("swept without a node's live set: %+v", blind.calls)
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
