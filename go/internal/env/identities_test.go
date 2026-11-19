// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"crypto"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// tdIssuer is an issuer of trust domain td that issues nothing.
type tdIssuer string

func (i tdIssuer) TrustDomain() string { return string(i) }
func (tdIssuer) IssueAddonServer(crypto.PublicKey, string, string, string, time.Duration, bool) ([]byte, *x509.Certificate,
	error) {
	return nil, nil, errors.New("this issuer issues nothing")
}
func (tdIssuer) TrustPEM() []byte { return nil }

// The identities: the latest deployment's services (an attached one
// with its sandbox's), jobs and addons, the environment's sandboxes, and
// an earlier object's pods still there; each pod with its newest
// certificate's end and node; another tenant's environment is a 404, and
// without an issuer every ID is "".
func TestIdentities(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.create("stage", nil)
	h.putSecret("stage", "token", "v1")
	sb := h.sandbox(tenantA, "stage")
	h.apply("stage", previewSpec(sb), 0)
	e := h.env("stage")
	h.svc.d.Issuer = tdIssuer("run.test")
	pod := func(kind, object string, replica int, node string) podid.ID {
		id := h.instance(e.ID, kind, object, replica, 1, model.PhaseReady, model.StateRunning, node, "")
		h.exec(`UPDATE sandboxes SET log_labels = jsonb_build_object('ferun.kind', $2::text, 'ferun.object', $3::text)
			WHERE id = $1`, id, kind, object)
		return id
	}
	api0, api1 := pod("service", "api", 0, "n1"), pod("service", "api", 1, "n2")
	pg := pod("addon", "pg", 0, "n2")
	old := pod("service", "old", 0, "n1")
	// A tenant's job's pod in the environment is none of its workloads.
	h.exec(`INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib, environment_id, tenant_job, log_labels)
		SELECT $1, owner, image, vcpus, memory_mib, environment_id, true, '{}' FROM sandboxes WHERE id = $2`,
		podid.Mint(), sb)
	cert := func(p podid.ID, serial, node string, notAfter time.Time, ago time.Duration) {
		if _, err := h.st.InsertIssuedCert(h.ctx(), store.IssuedCert{Serial: serial, PodID: p, Incarnation: e.ID, Node: node,
			SPIFFEID: "spiffe://run.test/x", HPKEPublicKey: []byte{1}, NotAfter: notAfter}); err != nil {
			t.Fatal(err)
		}
		h.exec(`UPDATE issued_certs SET issued_at = now() - make_interval(secs => $2) WHERE serial = $1`, serial, ago.Seconds())
	}
	end := time.Date(2026, 11, 20, 9, 15, 0, 0, time.UTC)
	cert(api0, "a1", "n9", end.Add(-24*time.Hour), time.Hour)
	cert(api0, "a2", "n1", end, time.Minute)
	cert(pg, "p1", "n2", end.Add(time.Hour), time.Minute)

	ids, err := h.svc.Identities(h.ctx(), tenantA, "stage")
	if err != nil {
		t.Fatal(err)
	}
	if ids.TrustDomain != "run.test" {
		t.Errorf("trust domain %q", ids.TrustDomain)
	}
	base := "spiffe://run.test/tenant/" + tenantA + "/env/" + e.ID.String() + "/"
	type pd = PodIdentity
	want := []WorkloadIdentity{
		{"service", "api", base + "service/api", []pd{{api0.String(), "2026-11-20T09:15:00Z", "n1"}, {api1.String(), "", "n2"}}},
		{"service", "dev", base + "sandbox/" + sb.String(), []pd{{sb.String(), "", ""}}},
		{"service", "web", base + "service/web", []pd{}},
		{"job", "migrate", base + "job/migrate", []pd{}},
		{"addon", "pg", base + "addon/pg", []pd{{pg.String(), "2026-11-20T10:15:00Z", "n2"}}},
		{"addon", "q", base + "addon/q", []pd{}},
		{"sandbox", sb.String(), base + "sandbox/" + sb.String(), []pd{{sb.String(), "", ""}}},
		{"service", "old", base + "service/old", []pd{{old.String(), "", "n1"}}},
	}
	// The pods of a workload by their IDs.
	if api0.String() > api1.String() {
		want[0].Pods[0], want[0].Pods[1] = want[0].Pods[1], want[0].Pods[0]
	}
	gb, _ := json.Marshal(ids.Workloads)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Errorf("workloads\n%s\nwant\n%s", gb, wb)
	}
	for _, key := range []string{"hpke", "serial", "PRIVATE", "CERTIFICATE"} {
		if b, _ := json.Marshal(ids); strings.Contains(strings.ToLower(string(b)), strings.ToLower(key)) {
			t.Errorf("the answer names %s: %s", key, b)
		}
	}

	_, err = h.svc.Identities(h.ctx(), tenantB, "stage")
	wantStatus(t, err, http.StatusNotFound, "no such environment")

	h.svc.d.Issuer = nil
	if ids, err = h.svc.Identities(h.ctx(), tenantA, e.ID.String()); err != nil || ids.TrustDomain != "" ||
		len(ids.Workloads) != len(want) || ids.Workloads[0].SPIFFEID != "" || ids.Workloads[6].SPIFFEID != "" {
		t.Errorf("without an issuer: %+v %v", ids, err)
	}

	// An environment without a deployment: its sandboxes alone.
	h.create("empty", nil)
	other := h.sandbox(tenantA, "empty")
	if ids, err = h.svc.Identities(h.ctx(), tenantA, "empty"); err != nil || len(ids.Workloads) != 1 ||
		ids.Workloads[0].Name != other.String() || len(ids.Workloads[0].Pods) != 1 {
		t.Errorf("empty: %+v %v", ids, err)
	}
}
