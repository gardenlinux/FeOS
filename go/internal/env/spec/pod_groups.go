// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"fmt"
	"slices"
	"strings"
)

// The groups of a pod outside the spec (ADR 0035): a sandbox's and a
// tenant's job's, named at their create (and a sandbox's PUT
// …/security-groups) among the groups of their environment's deployed
// deployment.

// PodGroups checks the groups a create names: sent false (the field left
// out) is [DefaultSecurityGroup]; else 1 to MaxObjectGroups names of
// groups, each once. It returns them sorted, or the text of a 400.
func PodGroups(names []string, sent bool) ([]string, error) {
	if !sent {
		return []string{DefaultSecurityGroup}, nil
	}
	switch {
	case len(names) == 0:
		return nil, fmt.Errorf("securityGroups is empty: name at least one group (leave it out for %s)",
			DefaultSecurityGroup)
	case len(names) > MaxObjectGroups:
		return nil, fmt.Errorf("securityGroups names %d groups; at most %d are allowed", len(names), MaxObjectGroups)
	}
	out := slices.Clone(names)
	slices.Sort(out)
	for i, n := range out {
		if !IsGroupName(n) {
			return nil, fmt.Errorf("securityGroups: %q is no group's name (1 to 32 of a-z, 0-9 and -, "+
				"first a letter or digit)", n)
		}
		if i > 0 && out[i-1] == n {
			return nil, fmt.Errorf("securityGroups names %s twice", n)
		}
	}
	return out, nil
}

// UnknownPodGroups is the text of a 422 for names that the groups have
// (the environment's deployed deployment's, sorted) do not all hold; ""
// when they do.
func UnknownPodGroups(names, have []string) string {
	var unknown []string
	for _, n := range names {
		if !slices.Contains(have, n) {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) == 0 {
		return ""
	}
	return fmt.Sprintf("securityGroups: the environment's deployed deployment has no group %s; it has %s",
		strings.Join(unknown, ", "), strings.Join(have, ", "))
}
