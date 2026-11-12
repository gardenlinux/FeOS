// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/store"
)

// A service token is found by its hash without counting a certificate,
// and its touch writes last_used_at at most once a minute; a revoked one
// is no row.
func TestServiceTokenOf(t *testing.T) {
	s, _ := newStore(t)
	made, err := s.CreateServiceToken(bg, "h-auth", "auth", "ferun-auth", "test")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ServiceTokenOf(bg, "h-auth")
	if err != nil || got.ID != made.ID || got.Kind != "auth" || got.Name != "ferun-auth" || !got.LastUsedAt.IsZero() {
		t.Fatalf("token %+v %v", got, err)
	}
	var count int
	if err := s.Q().QueryRow(bg, "SELECT cert_count FROM service_tokens WHERE hash = 'h-auth'").Scan(&count); err != nil ||
		count != 0 {
		t.Fatalf("cert_count %d %v", count, err)
	}
	if _, err := s.ServiceTokenOf(bg, "h-none"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown: %v", err)
	}

	must(t, s.TouchServiceToken(bg, "h-auth"))
	first, err := s.ServiceTokenOf(bg, "h-auth")
	if err != nil || first.LastUsedAt.IsZero() {
		t.Fatalf("touched %+v %v", first, err)
	}
	// Within the minute a touch writes nothing.
	exec(t, s, "UPDATE service_tokens SET last_used_at = now() - interval '30 seconds' WHERE hash = 'h-auth'")
	held, _ := s.ServiceTokenOf(bg, "h-auth")
	must(t, s.TouchServiceToken(bg, "h-auth"))
	if again, _ := s.ServiceTokenOf(bg, "h-auth"); !again.LastUsedAt.Equal(held.LastUsedAt) {
		t.Fatalf("touched again within the minute: %v, was %v", again.LastUsedAt, held.LastUsedAt)
	}
	// After it, the touch writes.
	exec(t, s, "UPDATE service_tokens SET last_used_at = now() - interval '61 seconds' WHERE hash = 'h-auth'")
	old, _ := s.ServiceTokenOf(bg, "h-auth")
	must(t, s.TouchServiceToken(bg, "h-auth"))
	if later, _ := s.ServiceTokenOf(bg, "h-auth"); later.LastUsedAt.Sub(old.LastUsedAt) < time.Minute {
		t.Fatalf("not touched after the minute: %v, was %v", later.LastUsedAt, old.LastUsedAt)
	}
	// A touch of an unknown token is no error.
	must(t, s.TouchServiceToken(bg, "h-none"))

	if _, err := s.DeleteServiceToken(bg, made.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ServiceTokenOf(bg, "h-auth"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("revoked: %v", err)
	}
}
