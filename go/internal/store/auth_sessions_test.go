// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/store"
)

// A sign-in under way is taken once, and only within its 10 minutes.
func TestAuthLogins(t *testing.T) {
	s, _ := newStore(t)
	must(t, s.InsertAuthLogin(bg, store.AuthLogin{StateHash: "a", Verifier: "v", Nonce: "n", Next: "/x"}))
	must(t, s.InsertAuthLogin(bg, store.AuthLogin{StateHash: "b", Verifier: "v", Nonce: "n", Next: "/"}))
	l, err := s.TakeAuthLogin(bg, "a")
	if err != nil || l.Verifier != "v" || l.Nonce != "n" || l.Next != "/x" || l.CreatedAt.IsZero() {
		t.Fatalf("take %+v %v", l, err)
	}
	if _, err := s.TakeAuthLogin(bg, "a"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("second take: %v", err)
	}
	exec(t, s, "UPDATE auth_logins SET created_at = now() - interval '10 minutes 1 second' WHERE state_hash = 'b'")
	if _, err := s.TakeAuthLogin(bg, "b"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("expired take: %v", err)
	}
	must(t, s.InsertAuthLogin(bg, store.AuthLogin{StateHash: "c", Verifier: "v", Nonce: "n", Next: "/"}))
	exec(t, s, "UPDATE auth_logins SET created_at = now() - interval '11 minutes' WHERE state_hash = 'c'")
	must(t, s.InsertAuthLogin(bg, store.AuthLogin{StateHash: "d", Verifier: "v", Nonce: "n", Next: "/"}))
	if n, err := s.DeleteStaleAuthLogins(bg); err != nil || n != 2 {
		t.Fatalf("stale %d %v", n, err)
	}
	if _, err := s.TakeAuthLogin(bg, "d"); err != nil {
		t.Fatal(err)
	}
}

// Sessions: everyone's that has not expired resolves, marked admin or
// not; the touch is throttled; a sign-out returns what it deleted.
func TestUISessions(t *testing.T) {
	s, _ := newStore(t)
	u, err := s.InsertUISession(bg, store.UISession{IDHash: "h1", Sub: "sub", Email: "e", Name: "n", Username: "u",
		Picture: "p", Groups: []string{"admins"}, Admin: true, IDToken: "tok"}, time.Hour)
	if err != nil || u.ExpiresAt.Sub(u.CreatedAt) != time.Hour || u.Groups[0] != "admins" {
		t.Fatalf("insert %+v %v", u, err)
	}
	if _, err := s.InsertUISession(bg, store.UISession{IDHash: "h2", Admin: false}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertUISession(bg, store.UISession{IDHash: "h3", Admin: true}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	got, err := s.UISessionOf(bg, "h1")
	if err != nil || got.Sub != "sub" || got.IDToken != "tok" || !got.Admin {
		t.Fatalf("h1 %+v %v", got, err)
	}
	if got, err := s.UISessionOf(bg, "h2"); err != nil || got.Admin {
		t.Fatalf("h2 %+v %v", got, err)
	}
	for _, h := range []string{"h3", "nope"} {
		if _, err := s.UISessionOf(bg, h); !errors.Is(err, store.ErrNoRow) {
			t.Fatalf("%s: %v", h, err)
		}
	}
	exec(t, s, "UPDATE ui_sessions SET last_seen_at = now() - interval '2 minutes' WHERE id_hash = 'h1'")
	must(t, s.TouchUISession(bg, "h1"))
	var recent bool
	if err := s.Q().QueryRow(bg, "SELECT last_seen_at > now() - interval '1 second' FROM ui_sessions WHERE id_hash = 'h1'").
		Scan(&recent); err != nil || !recent {
		t.Fatalf("touch %v %v", recent, err)
	}
	if n, err := s.DeleteExpiredUISessions(bg); err != nil || n != 1 {
		t.Fatalf("expired %d %v", n, err)
	}
	gone, err := s.DeleteUISession(bg, "h1")
	if err != nil || gone.IDToken != "tok" {
		t.Fatalf("delete %+v %v", gone, err)
	}
	if _, err := s.DeleteUISession(bg, "h1"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("second delete: %v", err)
	}
}
