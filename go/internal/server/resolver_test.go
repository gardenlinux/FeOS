// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// --deny-prefix is the image resolver's deny set too: with a set that
// leaves out the loopback range, which the built-in set holds, the
// resolver dials a registry on 127.0.0.1.
func TestDenyPrefixesReachTheResolver(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	dialed := make(chan struct{}, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case dialed <- struct{}{}:
			default:
			}
			c.Close()
		}
	}()

	cfg := testConfig(dbtest.NewDatabase(t), testCA(t))
	cfg.DenyPrefixes = []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}
	i, err := New(context.Background(), cfg, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	started(t, i)
	waitServing(t, i)
	ctx := context.Background()
	if _, _, err := i.Store.CreateTenant(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	sb, _, err := i.Store.CreateSandbox(ctx, store.NewSandbox{ID: podid.Mint(), Owner: "acme",
		Image: ln.Addr().String() + "/x/y:z", VCPUs: 2, MemoryMiB: 512, EnvironmentID: dbtest.Environment(t, i.Store.Q(), "acme", "box")})
	if err != nil {
		t.Fatal(err)
	}
	ectx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// The listener closes the connection, so the resolve fails, but after
	// the dial.
	_, err = i.Engine.Ensure(ectx, sb.ID)
	if err == nil || strings.Contains(err.Error(), "internal address") {
		t.Fatalf("ensure: %v, want a registry that was dialled and did not answer", err)
	}
	select {
	case <-dialed:
	default:
		t.Fatalf("the resolver did not dial the registry: %v", err)
	}
}
