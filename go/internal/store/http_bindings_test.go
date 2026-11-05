// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/store"
)

// SetHTTPPushed records a push against what its pusher read before it: a
// push that another one overtook leaves the record unknown, so that the
// loop pushes again; a create resets it.
func TestSetHTTPPushed(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	sb := mkSandbox(t, s, "acme")
	pushed := func() string {
		t.Helper()
		row, err := s.GetSandbox(bg, sb.ID)
		must(t, err)
		return row.HTTPPushed
	}
	set := func(from, v string, want bool) {
		t.Helper()
		ok, err := s.SetHTTPPushed(bg, sb.ID, sb.Incarnation, from, v)
		must(t, err)
		if ok != want {
			t.Fatalf("%q→%q: %v, want %v", from, v, ok, want)
		}
	}
	set("", "v1", true)
	set("", "v1", true) // the same again
	if got := pushed(); got != "v1" {
		t.Fatalf("http_pushed %q", got)
	}
	// Two pushes read v1; v3's is recorded first, v2's then is not known to
	// be the node's last.
	set("v1", "v3", true)
	set("v1", "v2", false)
	if got := pushed(); got != store.HTTPPushedUnknown {
		t.Fatalf("after the race: %q", got)
	}
	// Two pushes of the same part: both hold.
	set(store.HTTPPushedUnknown, "v4", true)
	set(store.HTTPPushedUnknown, "v4", true)
	// Another incarnation's push is left.
	if ok, err := s.SetHTTPPushed(bg, sb.ID, uuid.New(), "v4", "v5"); err != nil || ok {
		t.Fatalf("another incarnation: %v %v", ok, err)
	}
	must(t, s.ResetHTTPPushed(bg, sb.ID, sb.Incarnation))
	if got := pushed(); got != "" {
		t.Fatalf("after the reset: %q", got)
	}
}
