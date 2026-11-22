// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// A run claims its instance ID: a new row, or the row of an earlier run
// that ended without its stop (a crash, a kill during the stop) written
// anew, not stopping, with the ops that run left released. A row written
// within the TTL, stopping or not, refuses the claim: a process still runs
// under the ID.
func TestClaimInstance(t *testing.T) {
	s, _ := newStore(t)
	info := store.InstanceInfo{ID: "ferund-0", Address: ":8100", NodeAddress: "10.0.0.1:7443", Version: "v1"}
	claimed, err := s.ClaimInstance(bg, info)
	if err != nil || len(claimed.Released) != 0 || claimed.ToolCalls != 0 {
		t.Fatalf("first claim: %+v %v", claimed, err)
	}
	in, err := s.GetInstance(bg, "ferund-0")
	if err != nil || !in.Live || !in.Fresh || in.NodeAddress != "10.0.0.1:7443" || in.Version != "v1" {
		t.Fatalf("claimed row %+v %v", in, err)
	}

	// A second process under the same ID, while the first runs.
	var inUse *store.InstanceInUseError
	if _, err := s.ClaimInstance(bg, info); !errors.As(err, &inUse) || inUse.Stopping || inUse.Age >= store.InstanceTTL ||
		!inUse.LastSeen.Equal(in.LastSeen) {
		t.Fatalf("claim of a running ID: %v", err)
	}
	// While the first stops: it still serves, and its last steps release
	// the ID's ops and delete the row.
	must(t, s.SetInstanceStopping(bg, "ferund-0"))
	if _, err := s.ClaimInstance(bg, info); !errors.As(err, &inUse) || !inUse.Stopping {
		t.Fatalf("claim of a stopping ID: %v", err)
	}

	// The first was killed during its stop: its row stays stopping, and
	// an op it ran stays its own.
	mkProject(t, s, "acme")
	mkNode(t, s, "n1")
	sb := mkSandbox(t, s, "acme")
	place(t, s, sb.ID, "n1", model.StateRunning)
	startOp(t, s, sb.ID, "pause", "ferund-0", "paused", time.Hour)
	other := mkSandbox(t, s, "acme")
	place(t, s, other.ID, "n1", model.StateRunning)
	startOp(t, s, other.ID, "pause", "ferund-1", "paused", time.Hour)
	exec(t, s, "UPDATE instances SET last_seen = now() - interval '16 seconds', started_at = now() - interval '1 hour'")
	before := get(t, s, sb.ID)
	claimed, err = s.ClaimInstance(bg, store.InstanceInfo{ID: "ferund-0", Address: ":8101", Version: "v2"})
	if err != nil || len(claimed.Released) != 1 || claimed.Released[0] != sb.ID || claimed.ToolCalls != 0 {
		t.Fatalf("claim after a kill: %+v %v", claimed, err)
	}
	in, err = s.GetInstance(bg, "ferund-0")
	if err != nil || !in.Live || !in.Stopping.IsZero() || time.Since(in.StartedAt) > time.Minute || in.Address != ":8101" ||
		in.NodeAddress != "" || in.Version != "v2" {
		t.Fatalf("row after the claim %+v %v", in, err)
	}
	if live, _ := s.InstanceLive(bg, "ferund-0"); !live {
		t.Fatal("the new run is not live")
	}
	r := get(t, s, sb.ID)
	if r.Op != "" || r.OpOwner != "" || r.ClientState != model.StateUnknown || r.Gen != before.Gen+1 {
		t.Fatalf("the earlier run's op was not released: %+v", r)
	}
	if r.StatusDescription != "op pause of instance ferund-0 was released: the instance started again" {
		t.Fatalf("description %q", r.StatusDescription)
	}
	if r := get(t, s, other.ID); r.OpOwner != "ferund-1" {
		t.Fatalf("another instance's op was released: %+v", r)
	}
}

// A claim releases an earlier run's op as a stop releases its own
// (ReleaseOpsOf): the same columns change to the same values, all but the
// description, which gives the claim's reason.
func TestClaimReleasesAsAStop(t *testing.T) {
	s, d := newStore(t)
	mkProject(t, s, "acme")
	mkNode(t, s, "n1")
	ids := map[string]podid.ID{}
	for _, owner := range []string{"stopping", "ended"} {
		sb := mkSandbox(t, s, "acme")
		place(t, s, sb.ID, "n1", model.StatePausing)
		startOp(t, s, sb.ID, "pause", owner, "n1", time.Hour)
		exec(t, s, "UPDATE sandboxes SET op_from_state = 'running' WHERE id = $1", sb.ID)
		ids[owner] = sb.ID
	}
	row := func(id podid.ID) map[string]any {
		t.Helper()
		var m map[string]any
		if err := s.Q().QueryRow(bg, "SELECT to_jsonb(s) FROM sandboxes s WHERE s.id = $1", id).Scan(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	before := map[string]map[string]any{}
	for owner, id := range ids {
		before[owner] = row(id)
	}
	ch, unsubscribe := d.Subscribe(db.ChanSandbox)
	defer unsubscribe()

	if released, err := s.ReleaseOpsOf(bg, "stopping"); err != nil || len(released) != 1 || released[0] != ids["stopping"] {
		t.Fatalf("stop's release %v %v", released, err)
	}
	if claimed, err := s.ClaimInstance(bg, store.InstanceInfo{ID: "ended"}); err != nil || len(claimed.Released) != 1 ||
		claimed.Released[0] != ids["ended"] {
		t.Fatalf("claim's release %+v %v", claimed, err)
	}
	expectPayload(t, ch, ids["ended"].String())

	// The columns each release changed, with their new values; updated_at
	// is now() of each.
	changed := map[string]map[string]any{}
	for owner, id := range ids {
		after := row(id)
		c := map[string]any{}
		for k, v := range after {
			if !reflect.DeepEqual(before[owner][k], v) {
				c[k] = v
			}
		}
		if _, ok := c["status_description"]; !ok {
			t.Fatalf("%s: the description did not change", owner)
		}
		delete(c, "status_description")
		delete(c, "updated_at")
		changed[owner] = c
	}
	if len(changed["ended"]) == 0 || !reflect.DeepEqual(changed["ended"], changed["stopping"]) {
		t.Fatalf("the claim released\n%v\na stop released\n%v", changed["ended"], changed["stopping"])
	}
}

// A claim after a crash ends the tool calls the earlier run had, and
// counts them: a sent one interrupted, an unsent one not_run, each
// notified; another instance's call keeps running.
func TestClaimInstanceEndsToolCalls(t *testing.T) {
	s, d := newStore(t)
	if _, err := s.ClaimInstance(bg, store.InstanceInfo{ID: "ferund-0"}); err != nil {
		t.Fatal(err)
	}
	sb := callSandbox(t, s)
	sent := beginCall(t, s, sb, "sent", "ferund-0")
	markSent(t, s, sent)
	unsent := beginCall(t, s, sb, "unsent", "ferund-0")
	other := beginCall(t, s, sb, "other", "ferund-1")
	// The run crashed: its row aged past the TTL, its calls stay running.
	exec(t, s, "UPDATE instances SET last_seen = now() - interval '16 seconds' WHERE id = 'ferund-0'")
	ch, unsubscribe := d.Subscribe(db.ChanCall)
	defer unsubscribe()

	if claimed, err := s.ClaimInstance(bg, store.InstanceInfo{ID: "ferund-0"}); err != nil || claimed.ToolCalls != 2 ||
		len(claimed.Released) != 0 {
		t.Fatalf("claim after a crash: %+v %v", claimed, err)
	}
	expectPayloads(t, ch, sent.ID.String(), unsent.ID.String())
	expectEnded(t, s, sent, store.ToolCallInterrupted, claimedSent("ferund-0"))
	expectEnded(t, s, unsent, store.ToolCallNotRun, claimedUnsent("ferund-0"))
	expectRunning(t, s, other)
}

// Two processes that start together under one ID: one claims it, the other
// is refused.
func TestClaimInstanceRace(t *testing.T) {
	s, _ := newStore(t)
	for range 5 {
		exec(t, s, "DELETE FROM instances")
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for n := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[n] = s.ClaimInstance(bg, store.InstanceInfo{ID: "ferund-0", Version: uuid.NewString()})
			}()
		}
		wg.Wait()
		var inUse *store.InstanceInUseError
		if (errs[0] == nil) == (errs[1] == nil) || !(errors.As(errs[0], &inUse) || errors.As(errs[1], &inUse)) {
			t.Fatalf("claims %v", errs)
		}
	}
}

// A stopping instance that still writes its row is fresh but not live;
// one whose row aged past the TTL is neither.
func TestInstanceFresh(t *testing.T) {
	s, _ := newStore(t)
	for _, id := range []string{"running", "stopping", "gone"} {
		must(t, s.UpsertInstance(bg, store.InstanceInfo{ID: id}))
	}
	must(t, s.SetInstanceStopping(bg, "stopping"))
	exec(t, s, "UPDATE instances SET last_seen = now() - interval '16 seconds' WHERE id = 'gone'")
	list, err := s.ListInstances(bg)
	if err != nil || len(list) != 3 {
		t.Fatalf("%+v %v", list, err)
	}
	want := map[string][2]bool{"running": {true, true}, "stopping": {true, false}, "gone": {false, false}}
	for _, in := range list {
		if w := want[in.ID]; in.Fresh != w[0] || in.Live != w[1] {
			t.Errorf("%s: fresh %v live %v, want %v", in.ID, in.Fresh, in.Live, w)
		}
	}
}
