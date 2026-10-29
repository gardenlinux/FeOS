// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/store"
)

// A bake is recorded once, the newest first; the image is the key, so the
// same digest at another registry has none (ADR 0025).
func TestImageBakes(t *testing.T) {
	s, _ := newStore(t)
	digest := "@sha256:" + strings.Repeat("a", 64)
	image := "ghcr.io/acme/app" + digest
	b := store.ImageBake{Image: image, GuestVersion: "g1", L0: "l0", Layer: "first", Node: "n1"}
	for i, want := range []bool{true, false} {
		if added, err := s.RecordImageBake(bg, b); err != nil || added != want {
			t.Fatalf("record %d: %v %v", i, added, err)
		}
	}
	if _, err := s.Q().Exec(bg, "UPDATE image_bakes SET baked_at = now() - interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	b.Layer, b.Node = "second", "n2"
	if _, err := s.RecordImageBake(bg, b); err != nil {
		t.Fatal(err)
	}
	got, err := s.ImageBakes(bg, image)
	if err != nil || len(got) != 2 || got[0].Layer != "second" || got[1].Layer != "first" || got[0].Node != "n2" {
		t.Fatalf("bakes %+v %v", got, err)
	}
	if got, err := s.ImageBakes(bg, "evil.example/x"+digest); err != nil || len(got) != 0 {
		t.Fatalf("another registry's bakes %+v %v", got, err)
	}
	// Only an image pinned to a digest is a key.
	b.Image = "ghcr.io/acme/app:latest"
	if _, err := s.RecordImageBake(bg, b); err == nil {
		t.Fatal("a tag recorded")
	}
}
