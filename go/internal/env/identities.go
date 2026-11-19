// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"cmp"
	"context"
	"crypto/x509"
	"maps"
	"slices"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Identities is GET /api/v1/environments/{env}/identities: who the
// environment's workloads are to the platform (ADR 0036), the SPIFFE ID
// their pods' certificates carry under the trust domain, and what the
// certificate each pod holds names. No key and no certificate is in it.
// The trust domain is "" on an instance without an issuer, and with it
// the issuer and every SPIFFE ID, OU and subject. Issuer is the issuing
// CA's subject (RFC 2253), which signs every pod's certificate.
type Identities struct {
	TrustDomain string             `json:"trustDomain"`
	Issuer      string             `json:"issuer"`
	Workloads   []WorkloadIdentity `json:"workloads"`
}

// WorkloadIdentity is a workload: an object of the latest deployment
// (kind service, job or addon, by its name), a sandbox of the environment
// (kind sandbox, by its ID), or an object only an earlier deployment has
// whose pods are still there; its SPIFFE ID (the certificates' one URI
// name), the OU of its pods' subjects (the ID its set carries on the
// nodes, on which a postgres addon maps a role), and its pods ([] for
// none). An attached service runs in its sandbox's pod: its ID, its OU
// and its pod are the sandbox's.
type WorkloadIdentity struct {
	Kind     string        `json:"kind"`
	Name     string        `json:"name"`
	SPIFFEID string        `json:"spiffeId"`
	OU       string        `json:"ou"`
	Pods     []PodIdentity `json:"pods"`
}

// PodIdentity is a pod of a workload: its ID (an instance's, as the
// status shows it, or the sandbox's), the subject its certificates carry
// (pki.PodSubject, RFC 2253: CN=<trust domain>/<pod>,OU=<workload>), and
// of the newest certificate issued to it its serial, when it was issued
// and its end ("" before its first), and the node it was issued on (the
// pod's node before its first).
type PodIdentity struct {
	Pod      string `json:"pod"`
	Subject  string `json:"subject"`
	Serial   string `json:"serial"`
	IssuedAt string `json:"issuedAt"`
	NotAfter string `json:"notAfter"`
	Node     string `json:"node"`
}

// issuerName is the subject of the CA an issuer signs with, "" for one
// that does not say (pki.Issuer does).
func issuerName(iss any) string {
	if c, ok := iss.(interface{ Certificate() *x509.Certificate }); ok && c.Certificate() != nil {
		return c.Certificate().Subject.String()
	}
	return ""
}

// Identities is GET /api/v1/environments/{env}/identities: the objects of
// the latest deployment in the order services, jobs, addons, each by
// name, then the environment's sandboxes by ID, then the workloads of
// pods that are of no object of it; each pod with the newest certificate
// issued_certs keeps of it.
func (s *Service) Identities(ctx context.Context, tenant, env string) (Identities, error) {
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return Identities{}, err
	}
	td, issuer := "", ""
	if s.d.Issuer != nil {
		td, issuer = s.d.Issuer.TrustDomain(), issuerName(s.d.Issuer)
	}
	id := func(kind, name string) string {
		if td == "" {
			return ""
		}
		return pki.WorkloadID(td, e.Owner, e.ID, kind, name)
	}
	// ou is the OU of a workload's pods, as store.Sandbox.Workload names
	// their set.
	ou := func(kind, name string) string {
		switch {
		case td == "":
			return ""
		case kind == pki.WorkloadSandbox:
			return model.SandboxWorkloadGroupID(name)
		}
		return model.WorkloadGroupID(e.ID, kind, name)
	}
	instances, err := s.st.EnvCtlSandboxes(ctx, e.ID)
	if err != nil {
		return Identities{}, err
	}
	sandboxes, err := s.st.ListSandboxes(ctx, e.Owner, []uuid.UUID{e.ID})
	if err != nil {
		return Identities{}, err
	}
	rows := slices.Concat(instances, sandboxes)
	ids := make([]podid.ID, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	certs, err := s.st.LatestIssuedCerts(ctx, ids)
	if err != nil {
		return Identities{}, err
	}
	pods := map[[2]string][]PodIdentity{}
	for _, r := range rows {
		kind, name, set := r.Workload()
		if kind == "" {
			continue
		}
		p := PodIdentity{Pod: r.ID.String(), Node: r.Node}
		if td != "" {
			p.Subject = pki.PodSubject(td, r.ID.String(), set).String()
		}
		if c, ok := certs[r.ID]; ok {
			p.Serial, p.IssuedAt, p.NotAfter, p.Node = c.Serial, rfc3339(c.IssuedAt), rfc3339(c.NotAfter), c.Node
		}
		pods[[2]string{kind, name}] = append(pods[[2]string{kind, name}], p)
	}
	for k := range pods {
		slices.SortFunc(pods[k], func(a, b PodIdentity) int { return cmp.Compare(a.Pod, b.Pod) })
	}

	out := Identities{TrustDomain: td, Issuer: issuer, Workloads: []WorkloadIdentity{}}
	listed := map[[2]string]bool{}
	add := func(kind, name string, of [2]string) {
		listed[of] = true
		w := WorkloadIdentity{Kind: kind, Name: name, SPIFFEID: id(of[0], of[1]), OU: ou(of[0], of[1]), Pods: pods[of]}
		if w.Pods == nil {
			w.Pods = []PodIdentity{}
		}
		out.Workloads = append(out.Workloads, w)
	}
	if e.Deployment > 0 {
		sp, err := s.deploymentSpec(ctx, e, e.Deployment)
		if err != nil {
			return Identities{}, err
		}
		for _, name := range slices.Sorted(maps.Keys(sp.Services)) {
			of := [2]string{spec.KindService, name}
			if svc := sp.Services[name]; spec.IsAttached(svc) {
				if sbx, err := podid.Parse(svc.Sandbox); err == nil {
					of = [2]string{pki.WorkloadSandbox, sbx.String()}
				}
			}
			add(spec.KindService, name, of)
		}
		for _, name := range slices.Sorted(maps.Keys(sp.Jobs)) {
			add(spec.KindJob, name, [2]string{spec.KindJob, name})
		}
		for _, name := range slices.Sorted(maps.Keys(sp.Addons)) {
			add(spec.KindAddon, name, [2]string{spec.KindAddon, name})
		}
	}
	for _, r := range sandboxes {
		of := [2]string{pki.WorkloadSandbox, r.ID.String()}
		add(of[0], of[1], of)
	}
	var rest [][2]string
	for k := range pods {
		if !listed[k] {
			rest = append(rest, k)
		}
	}
	slices.SortFunc(rest, func(a, b [2]string) int { return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1])) })
	for _, k := range rest {
		add(k[0], k[1], k)
	}
	return out, nil
}
