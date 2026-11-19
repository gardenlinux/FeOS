// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"crypto"
	"crypto/x509"
	"crypto/x509/pkix"
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

// namedIssuer is a tdIssuer that says its CA's subject, as pki.Issuer
// does.
type namedIssuer struct{ tdIssuer }

func (i namedIssuer) Certificate() *x509.Certificate {
	return &x509.Certificate{Subject: pkix.Name{CommonName: "FeOS issuer " + string(i.tdIssuer)}}
}

// The identities: the latest deployment's services (an attached one
// with its sandbox's), jobs and addons, the environment's sandboxes, and
// an earlier object's pods still there; each pod with its newest
// certificate's serial, issue, end and node, and the subject and OU its
// certificates carry; another tenant's environment is a 404, and without
// an issuer every ID, OU and subject is "".
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
	if ids.Issuer != "" {
		t.Errorf("issuer %q of an issuer that names none", ids.Issuer)
	}
	ou := func(kind, name string) string { return model.WorkloadGroupID(e.ID, kind, name) }
	sbx := model.SandboxWorkloadGroupID(sb.String())
	type pd = PodIdentity
	p := func(id podid.ID, ou, serial, issued, notAfter, node string) pd {
		return pd{Pod: id.String(), Subject: "CN=run.test/" + id.String() + ",OU=" + ou, Serial: serial, IssuedAt: issued,
			NotAfter: notAfter, Node: node}
	}
	wl := func(kind, name, id, ou string, pods ...pd) WorkloadIdentity {
		return WorkloadIdentity{Kind: kind, Name: name, SPIFFEID: base + id, OU: ou, Pods: append([]pd{}, pods...)}
	}
	issued := func(serial string) string {
		var at time.Time
		if err := h.d.Pool.QueryRow(h.ctx(), `SELECT issued_at FROM issued_certs WHERE serial = $1`, serial).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return rfc3339(at)
	}
	api := []pd{p(api0, ou("service", "api"), "a2", issued("a2"), "2026-11-20T09:15:00Z", "n1"),
		p(api1, ou("service", "api"), "", "", "", "n2")}
	// The pods of a workload by their IDs.
	if api0.String() > api1.String() {
		api[0], api[1] = api[1], api[0]
	}
	want := []WorkloadIdentity{
		wl("service", "api", "service/api", ou("service", "api"), api...),
		wl("service", "dev", "sandbox/"+sb.String(), sbx, p(sb, sbx, "", "", "", "")),
		wl("service", "web", "service/web", ou("service", "web")),
		wl("job", "migrate", "job/migrate", ou("job", "migrate")),
		wl("addon", "pg", "addon/pg", ou("addon", "pg"), p(pg, ou("addon", "pg"), "p1", issued("p1"), "2026-11-20T10:15:00Z", "n2")),
		wl("addon", "q", "addon/q", ou("addon", "q")),
		wl("sandbox", sb.String(), "sandbox/"+sb.String(), sbx, p(sb, sbx, "", "", "", "")),
		wl("service", "old", "service/old", ou("service", "old"), p(old, ou("service", "old"), "", "", "", "n1")),
	}
	gb, _ := json.Marshal(ids.Workloads)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Errorf("workloads\n%s\nwant\n%s", gb, wb)
	}
	// What a certificate names, never a key or the certificate itself.
	for _, key := range []string{"hpke", "PRIVATE", "CERTIFICATE"} {
		if b, _ := json.Marshal(ids); strings.Contains(strings.ToLower(string(b)), strings.ToLower(key)) {
			t.Errorf("the answer names %s: %s", key, b)
		}
	}

	// The issuer's subject, of an issuer that says it.
	h.svc.d.Issuer = namedIssuer{tdIssuer("run.test")}
	if ids, err = h.svc.Identities(h.ctx(), tenantA, "stage"); err != nil || ids.Issuer != "CN=FeOS issuer run.test" {
		t.Errorf("issuer %q, %v", ids.Issuer, err)
	}

	_, err = h.svc.Identities(h.ctx(), tenantB, "stage")
	wantStatus(t, err, http.StatusNotFound, "no such environment")

	h.svc.d.Issuer = nil
	if ids, err = h.svc.Identities(h.ctx(), tenantA, e.ID.String()); err != nil || ids.TrustDomain != "" ||
		ids.Issuer != "" || len(ids.Workloads) != len(want) || ids.Workloads[0].SPIFFEID != "" ||
		ids.Workloads[0].OU != "" || ids.Workloads[0].Pods[0].Subject != "" || ids.Workloads[6].SPIFFEID != "" {
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
