// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

var bg = context.Background()

// newStore gives the test a database of its own; the tests that use it run
// in parallel.
func newStore(t *testing.T) (*store.Store, *db.DB) {
	t.Helper()
	t.Parallel()
	d := dbtest.New(t)
	return store.New(d), d
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func exec(t *testing.T, s *store.Store, sql string, args ...any) {
	t.Helper()
	if _, err := s.Q().Exec(bg, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func mkTenant(t *testing.T, s *store.Store, name string) {
	t.Helper()
	if _, _, err := s.CreateTenant(bg, name); err != nil {
		t.Fatal(err)
	}
}

// mkNode makes a ready node with 8 cores and 32 GiB.
func mkNode(t *testing.T, s *store.Store, name string) {
	t.Helper()
	if _, err := s.InsertNode(bg, name); err != nil {
		t.Fatal(err)
	}
	exec(t, s, `UPDATE nodes SET status = 'ready', cpu_cores = 8, memory_total_bytes = 34359738368 WHERE name = $1`, name)
}

func mkSandbox(t *testing.T, s *store.Store, owner string) store.Sandbox {
	t.Helper()
	return mkSandboxID(t, s, owner, podid.Mint())
}

// legacyID is a new sandbox ID in the format of a sandbox made before ADR
// 0019: a UUID in lowercase.
func legacyID() podid.ID { return podid.ID(uuid.NewString()) }

// boxEnv is the environment "box" of owner, made when it has none: where
// the tests' sandboxes are (ADR 0035).
func boxEnv(t *testing.T, s *store.Store, owner string) uuid.UUID {
	t.Helper()
	e, err := s.LockEnvironmentByName(bg, owner, "box", store.NoLock)
	if errors.Is(err, store.ErrNoRow) {
		e, err = s.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: owner, Name: "box"})
	}
	if err != nil {
		t.Fatal(err)
	}
	return e.ID
}

func mkSandboxID(t *testing.T, s *store.Store, owner string, id podid.ID) store.Sandbox {
	t.Helper()
	return mkSandboxIn(t, s, owner, boxEnv(t, s, owner), id)
}

// mkSandboxIn makes a sandbox of owner in environment env.
func mkSandboxIn(t *testing.T, s *store.Store, owner string, env uuid.UUID, id podid.ID) store.Sandbox {
	t.Helper()
	sb, created, err := s.CreateSandbox(bg, store.NewSandbox{ID: id, Owner: owner, Image: "alpine", VCPUs: 2, MemoryMiB: 512,
		EnvironmentID: env})
	if err != nil || !created {
		t.Fatalf("create sandbox: %v (created %v)", err, created)
	}
	return sb
}

// place puts a sandbox on node in state, without an op.
func place(t *testing.T, s *store.Store, id podid.ID, node, state string) {
	t.Helper()
	exec(t, s, "UPDATE sandboxes SET node = $2, client_state = $3 WHERE id = $1", id, node, state)
}

// startOp puts an op in flight on a sandbox, owned by owner.
func startOp(t *testing.T, s *store.Store, id podid.ID, op, owner, target string, deadline time.Duration) {
	t.Helper()
	exec(t, s, `UPDATE sandboxes SET op = $2, op_id = gen_random_uuid(), op_owner = $3, op_target = $4,
		op_deadline = now() + $5::interval WHERE id = $1`, id, op, owner, target, fmt.Sprintf("%d microseconds", deadline.Microseconds()))
}

func get(t *testing.T, s *store.Store, id podid.ID) store.Sandbox {
	t.Helper()
	sb, err := s.GetSandbox(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

// expectPayload waits for want on ch, skipping other payloads.
func expectPayload(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case p := <-ch:
			if p == want {
				return
			}
		case <-timer.C:
			t.Fatalf("no notification %q", want)
		}
	}
}

// expectNoPayload checks that want does not arrive on ch for a while.
func expectNoPayload(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case p := <-ch:
			if p == want {
				t.Fatalf("unexpected notification %q", want)
			}
		case <-timer.C:
			return
		}
	}
}

func tx(t *testing.T, s *store.Store, fn func(tx *store.Store) error) {
	t.Helper()
	if err := s.InTx(bg, fn); err != nil {
		t.Fatal(err)
	}
}
