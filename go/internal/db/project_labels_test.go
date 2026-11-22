// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/model"
)

// 0044 (ADR 0063): an environment that holds the natural label of ADR
// 0045, <environment>-<project>, gets <environment>--<project>, or its
// fallback where that is no label (a name of two characters) or another
// project holds it, and its old label stays its project's, free. Chosen
// labels, fallbacks and the plain labels of the zone owners do not move.
// A gone project's labels that name a project in ADR 0063's forms are that
// project's first, so dev of hb-baxxter gets dev--hb-baxxter where the gone
// project fecode held it.
func TestProjectLabelsMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 44)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("INSERT INTO projects (id) VALUES ('kino'), ('hb-baxxter'), ('other'), ('shop.io')")
	env := func(owner, name, label string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO environments (id, owner, name, public, ingress_group, host_label)
			VALUES ($1, $2, $3, true, true, $4)`, id, owner, name, label)
		exec("INSERT INTO host_labels (zone, label, owner, env_id) VALUES ('', $1, $2, $3)", label, owner, id)
		return id
	}
	prod := env("kino", "prod", "prod-kino")
	qa := env("kino", "qa", "qa-kino")
	dev := env("hb-baxxter", "dev", "dev-hb-baxxter")
	stage := env("kino", "stage", "stage-kino")
	blue := env("kino", "blue", "blue--kino")
	plain := env("other", "prod", "other")
	feature := uuid.New()
	_, _, featureFallback := model.HostLabels("feature-x", "kino", feature)
	exec(`INSERT INTO environments (id, owner, name, public, ingress_group, host_label)
		VALUES ($1, 'kino', 'feature-x', true, true, $2)`, feature, featureFallback)
	exec("INSERT INTO host_labels (zone, label, owner, env_id) VALUES ('', $1, 'kino', $2)", featureFallback, feature)
	// The gone project fecode holds dev--hb-baxxter, kino and x--y--kino:
	// the first two go to the projects they name, the third to none.
	exec("INSERT INTO host_labels (zone, label, owner) VALUES ('', 'dev--hb-baxxter', 'fecode'), ('', 'kino', 'fecode'), ('', 'x--y--kino', 'fecode')")
	// Another project holds stage--kino: stage gets its fallback.
	exec("INSERT INTO host_labels (zone, label, owner) VALUES ('', 'stage--kino', 'other')")

	if err := d.Migrate(ctx, nil); err != nil {
		t.Fatal(err)
	}
	_, _, qaFallback := model.HostLabels("qa", "kino", qa)
	_, _, stageFallback := model.HostLabels("stage", "kino", stage)
	for id, want := range map[uuid.UUID]string{
		prod: "prod--kino", qa: qaFallback, dev: "dev--hb-baxxter", stage: stageFallback,
		blue: "blue--kino", plain: "other", feature: featureFallback,
	} {
		var label, held string
		if err := d.Pool.QueryRow(ctx, `SELECT e.host_label, coalesce(h.label, '') FROM environments e
			LEFT JOIN host_labels h ON h.env_id = e.id WHERE e.id = $1`, id).Scan(&label, &held); err != nil {
			t.Fatal(err)
		}
		if label != want || held != want {
			t.Errorf("environment %s: label %q, holds %q, want %q", id, label, held, want)
		}
	}
	for _, old := range []string{"prod-kino", "qa-kino", "dev-hb-baxxter"} {
		var owner string
		var envID *uuid.UUID
		if err := d.Pool.QueryRow(ctx, "SELECT owner, env_id FROM host_labels WHERE zone = '' AND label = $1",
			old).Scan(&owner, &envID); err != nil {
			t.Fatalf("%s: %v", old, err)
		}
		if envID != nil || (owner != "kino" && owner != "hb-baxxter") {
			t.Errorf("old label %s: owner %s, environment %v; want its project's, free", old, owner, envID)
		}
	}
	for label, want := range map[string]string{"kino": "kino", "dev--hb-baxxter": "hb-baxxter", "x--y--kino": "fecode"} {
		var owner string
		if err := d.Pool.QueryRow(ctx, "SELECT owner FROM host_labels WHERE zone = '' AND label = $1", label).Scan(&owner); err != nil ||
			owner != want {
			t.Errorf("%s: owner %q %v, want %s", label, owner, err, want)
		}
	}
	if natural, ok, _ := model.HostLabels("prod", "kino", prod); !ok || natural != "prod--kino" {
		t.Errorf("model.HostLabels(prod, kino) = %q, %v: the migration and the model differ", natural, ok)
	}
}
