// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"slices"
	"testing"
)

// The zones of default hosts (ADR 0031): a project's hosts are in the apps
// zone or the project zone, a zone the platform does not serve has none;
// people's projects get the project zone while there is one; only the apps
// zone's owners choose plain labels, and only there.
func TestZones(t *testing.T) {
	z := Zones{Apps: "apps.example.com", AppsOwners: []string{"fecode"}, Project: "apps.run.example.com"}
	for zone, want := range map[string]string{
		"": "apps.example.com", "apps.example.com": "apps.example.com",
		"apps.run.example.com": "apps.run.example.com", "old.example.com": "",
	} {
		if got := z.Of(zone); got != want {
			t.Errorf("Of(%q) = %q, want %q", zone, got, want)
		}
	}
	if z.ForNewProject(true) != "apps.run.example.com" || z.ForNewProject(false) != "" {
		t.Errorf("new projects %q %q", z.ForNewProject(true), z.ForNewProject(false))
	}
	noProjectZone := Zones{Apps: "apps.example.com"}
	if noProjectZone.ForNewProject(true) != "" || noProjectZone.Of("apps.run.example.com") != "" {
		t.Error("without a project zone")
	}
	for name, want := range map[string]struct {
		zone string
		ok   bool
	}{
		"apps.example.com": {"", true}, "Apps.Example.Com.": {"", true},
		"apps.run.example.com": {"apps.run.example.com", true}, "": {"", false}, "other.example.com": {"", false},
	} {
		if zone, ok := z.Column(name); zone != want.zone || ok != want.ok {
			t.Errorf("Column(%q) = %q %v, want %+v", name, zone, ok, want)
		}
	}
	if _, ok := (Zones{}).Column(""); ok {
		t.Error("no zones: the empty name is a zone")
	}
	for _, c := range []struct {
		project, zone string
		want          bool
	}{
		{"fecode", "", true}, {"fecode", "apps.run.example.com", false}, {"acme", "", false},
		{"fecode", "old.example.com", false},
	} {
		if got := z.PlainLabels(c.project, c.zone); got != c.want {
			t.Errorf("PlainLabels(%q, %q) = %v", c.project, c.zone, got)
		}
	}
	if got := z.Names(); !slices.Equal(got, []string{"apps.example.com", "apps.run.example.com"}) {
		t.Errorf("names %v", got)
	}
	if got := noProjectZone.Names(); !slices.Equal(got, []string{"apps.example.com"}) {
		t.Errorf("names %v", got)
	}
}
