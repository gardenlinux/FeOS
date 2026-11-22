// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package oldnames

import "testing"

func TestRefuse(t *testing.T) {
	t.Parallel()
	if err := Refuse([]string{"FERUN_PROJECT_ZONE=z", "HOME=/root", "OTHER_TENANT=x", "FERUN_LOG_LEVEL"}); err != nil { // KEEP-TENANT: a variable outside FERUN_ is not ours
		t.Fatalf("new names refused: %v", err)
	}
	err := Refuse([]string{"FERUN_TENANTS_PER_PERSON=3", "FERUN_PROJECT_CREATION=open", // KEEP-TENANT: the old names refused
		"FERUN_INGRESS_MAX_HELD_PER_TENANT=8"}) // KEEP-TENANT: the old names refused
	want := "environment variables of an earlier name (ADR 0061): " +
		"FERUN_INGRESS_MAX_HELD_PER_TENANT is FERUN_INGRESS_MAX_HELD_PER_PROJECT now; " + // KEEP-TENANT: the old names refused
		"FERUN_TENANTS_PER_PERSON is FERUN_PROJECTS_PER_PERSON now" // KEEP-TENANT: the old names refused
	if err == nil || err.Error() != want {
		t.Fatalf("Refuse = %v\nwant %s", err, want)
	}
}
