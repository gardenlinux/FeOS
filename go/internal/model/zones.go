// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"slices"
	"strings"
)

// Zones are the platform's zones of default hosts (ADR 0021, ADR 0031):
// the apps zone (--apps-zone) with the tenants that own its plain labels
// (--apps-zone-owners), and the tenant zone (--tenant-zone), where the
// tenants that people make get their hosts. "" switches a zone off. A
// tenant's zone (tenants.zone) is "" for the apps zone, else the name of
// its zone.
type Zones struct {
	Apps       string
	AppsOwners []string
	Tenant     string
}

// Of is the zone a tenant whose tenants.zone is zone has its default hosts
// under: the apps zone for "" (or the apps zone's name), the tenant zone
// for its name, and "" for any other zone, one the platform does not serve
// (any more), whose hosts are off until an operator moves the tenant.
func (z Zones) Of(zone string) string {
	switch {
	case zone == "" || zone == z.Apps:
		return z.Apps
	case zone == z.Tenant:
		return z.Tenant
	}
	return ""
}

// ForNewTenant is tenants.zone of a new tenant: the tenant zone for one a
// person makes (byPerson), while the platform has one; "", the apps zone,
// for one an operator makes, and for every tenant without a tenant zone.
func (z Zones) ForNewTenant(byPerson bool) string {
	if byPerson {
		return z.Tenant
	}
	return ""
}

// Column is what tenants.zone holds for a zone an operator names, given
// by its name: "" for the apps zone, the tenant zone's name for it; ok is
// false for any other name, a zone the platform does not serve.
func (z Zones) Column(name string) (zone string, ok bool) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	switch {
	case z.Apps != "" && name == z.Apps:
		return "", true
	case z.Tenant != "" && name == z.Tenant:
		return z.Tenant, true
	}
	return "", false
}

// PlainLabels reports whether tenant, whose tenants.zone is zone, may
// choose a plain label (PlainHostLabel): it owns the apps zone and its
// hosts are there. The tenant zone has no owners, so nobody chooses a
// plain label in it.
func (z Zones) PlainLabels(tenant, zone string) bool {
	return z.Apps != "" && z.Of(zone) == z.Apps && slices.Contains(z.AppsOwners, tenant)
}

// Names are the zones the platform serves, the apps zone first.
func (z Zones) Names() []string {
	var out []string
	for _, n := range []string{z.Apps, z.Tenant} {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}
