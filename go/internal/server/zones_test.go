// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/domains"
)

// No domain can be the apps zone, the tenant zone (ADR 0031) or a name
// under either; a name beside them can.
func TestZonesAreReserved(t *testing.T) {
	cfg := testConfig(dbtest.NewDatabase(t), testCA(t))
	cfg.AppsZone, cfg.TenantZone, cfg.EdgeName = "apps.example.com", "apps.run.example.com", "edge.example.com"
	i, err := New(context.Background(), cfg, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	ctx := context.Background()
	for _, name := range []string{"apps.run.example.com", "web-acme.apps.run.example.com", "a.b.apps.run.example.com",
		"apps.example.com", "kino.apps.example.com"} {
		_, err := i.Domains.CreateDomain(ctx, "acme", domains.CreateRequest{Name: name})
		var ae *apierr.Error
		if !errors.As(err, &ae) || ae.Status != http.StatusUnprocessableEntity || ae.Code != domains.CodeReserved {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := i.Store.CreateTenant(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Domains.CreateDomain(ctx, "acme", domains.CreateRequest{Name: "run.example.com"}); err != nil {
		t.Errorf("a name beside the zones: %v", err)
	}
}
