// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/store"
)

// An owner renews only the watches it holds in the epochs it names; a
// release ends its leases at once.
func TestOwnedWatches(t *testing.T) {
	s, _ := newStore(t)
	mkProject(t, s, "t1")
	sb := mkSandbox(t, s, "t1")
	mk := func() store.Watch {
		t.Helper()
		w, err := s.CreateWatch(bg, store.NewWatch{ID: uuid.New(), SandboxID: sb.ID, Paths: []string{"/"}}, "i1",
			time.Minute, time.Second)
		must(t, err)
		return w
	}
	mine, moved, gone := mk(), mk(), mk()
	// moved is taken over by i2; gone is deleted.
	_, err := s.TakeWatchOwnership(bg, moved.ID, "i2", time.Minute, time.Minute)
	must(t, err)
	_, err = s.DeleteWatch(bg, gone.ID)
	must(t, err)

	renewed, err := s.RenewOwnedWatches(bg, "i1", []store.OwnedWatch{{ID: mine.ID, Epoch: 1}, {ID: moved.ID, Epoch: 1},
		{ID: gone.ID, Epoch: 1}}, time.Minute)
	must(t, err)
	if len(renewed) != 1 || !renewed[mine.ID] {
		t.Fatalf("renewed %v", renewed)
	}
	w, err := s.GetWatch(bg, mine.ID)
	must(t, err)
	if !w.OwnerLive || w.OwnerUntil.Sub(w.CreatedAt) < 50*time.Second {
		t.Errorf("renewed watch %+v", w)
	}
	// A stale epoch is not renewed, even for the right owner.
	_, err = s.TakeWatchOwnership(bg, mine.ID, "i1", time.Minute, time.Minute)
	must(t, err)
	renewed, err = s.RenewOwnedWatches(bg, "i1", []store.OwnedWatch{{ID: mine.ID, Epoch: 1}}, time.Minute)
	must(t, err)
	if len(renewed) != 0 {
		t.Errorf("renewed in a stale epoch: %v", renewed)
	}
	if renewed, err := s.RenewOwnedWatches(bg, "i1", nil, time.Minute); err != nil || len(renewed) != 0 {
		t.Errorf("nothing held: %v %v", renewed, err)
	}

	n, err := s.ReleaseWatchOwnership(bg, "i1")
	must(t, err)
	if n != 1 {
		t.Errorf("released %d", n)
	}
	w, err = s.GetWatch(bg, mine.ID)
	must(t, err)
	if w.OwnerLive || w.Owner != "i1" {
		t.Errorf("released watch %+v", w)
	}
	w, err = s.GetWatch(bg, moved.ID)
	must(t, err)
	if !w.OwnerLive || w.Owner != "i2" {
		t.Errorf("another owner's watch %+v", w)
	}
}
