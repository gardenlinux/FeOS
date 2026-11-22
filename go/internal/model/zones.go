// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package model

import "strings"

// Zones are the platform's zones of default hosts (ADR 0021, ADR 0031):
// the apps zone (--apps-zone) and the project zone (--project-zone), where the
// projects that people make get their hosts. "" switches a zone off. A
// project's zone (projects.zone) is "" for the apps zone, else the name of
// its zone.
type Zones struct {
	Apps    string
	Project string
}

// Of is the zone a project whose projects.zone is zone has its default hosts
// under: the apps zone for "" (or the apps zone's name), the project zone
// for its name, and "" for any other zone, one the platform does not serve
// (any more), whose hosts are off until an operator moves the project.
func (z Zones) Of(zone string) string {
	switch {
	case zone == "" || zone == z.Apps:
		return z.Apps
	case zone == z.Project:
		return z.Project
	}
	return ""
}

// ForNewProject is projects.zone of a new project: the project zone for one a
// person makes (byPerson), while the platform has one; "", the apps zone,
// for one an operator makes, and for every project without a project zone.
func (z Zones) ForNewProject(byPerson bool) string {
	if byPerson {
		return z.Project
	}
	return ""
}

// Column is what projects.zone holds for a zone an operator names, given
// by its name: "" for the apps zone, the project zone's name for it; ok is
// false for any other name, a zone the platform does not serve.
func (z Zones) Column(name string) (zone string, ok bool) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	switch {
	case z.Apps != "" && name == z.Apps:
		return "", true
	case z.Project != "" && name == z.Project:
		return z.Project, true
	}
	return "", false
}

// Names are the zones the platform serves, the apps zone first.
func (z Zones) Names() []string {
	var out []string
	for _, n := range []string{z.Apps, z.Project} {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}
