// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"crypto/x509/pkix"

	"github.com/google/uuid"
)

// The kinds of WorkloadID that a call makes, named by the pod's ID; an
// environment's objects are named by spec.KindService, KindJob and
// KindAddon and the object's name.
const (
	WorkloadSandbox   = "sandbox"
	WorkloadTenantJob = "tenant-job"
)

// WorkloadID is the SPIFFE ID of a workload (ADR 0036): every pod of an
// environment's object, every replica and every pod after a roll, and the
// one pod of a sandbox or a tenant's job, has
// spiffe://<td>/tenant/<tenant>/env/<environment ID>/<kind>/<name>. The
// environment is named by its ID, which a rename keeps (ADR 0037).
func WorkloadID(td, tenant string, envID uuid.UUID, kind, name string) string {
	return spiffeScheme + td + "/tenant/" + tenant + "/env/" + envID.String() + "/" + kind + "/" + name
}

// PodSubject is a pod certificate's subject (ADR 0036): the CN names the
// pod, as the trust domain and the pod's ID; one OU names its workload by
// the ID its set carries on the nodes (model.WorkloadGroupID and its
// kin), on which an addon maps a role; nothing else, so that RFC 2253
// prints CN=<td>/<pod>,OU=<workload>. A CN-only check thus matches one
// pod and fails closed. workload "" leaves the OU out.
func PodSubject(td, pod, workload string) pkix.Name {
	n := pkix.Name{CommonName: td + "/" + pod}
	if workload != "" {
		n.OrganizationalUnit = []string{workload}
	}
	return n
}
