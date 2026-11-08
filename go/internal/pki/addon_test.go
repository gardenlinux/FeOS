// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestIssueAddonServer(t *testing.T) {
	iss, _ := newCA(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	envID := uuid.MustParse("01a196e9-4ac8-71a8-94d7-f6aa5688b1fc")
	id := WorkloadID(testTD, "acme", envID, "addon", "postgres")
	if id != "spiffe://"+testTD+"/tenant/acme/env/01a196e9-4ac8-71a8-94d7-f6aa5688b1fc/addon/postgres" {
		t.Fatalf("WorkloadID = %s", id)
	}
	const workload = "wl-0123456789abcdef0123456789abcdef"
	before := time.Now()
	chain, leaf, err := iss.IssueAddonServer(pub, id, "xmxfgckmkt6", workload, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != id || len(leaf.DNSNames) != 0 || len(leaf.IPAddresses) != 0 {
		t.Errorf("names: URIs %v, DNS %v, IP %v", leaf.URIs, leaf.DNSNames, leaf.IPAddresses)
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("EKU %v", leaf.ExtKeyUsage)
	}
	// The subject PostgreSQL prints (RFC 2253, the encoding's order
	// reversed): an addon's map anchors on the OU.
	if got, want := leaf.Subject.String(), "CN="+testTD+"/xmxfgckmkt6,OU="+workload; got != want {
		t.Errorf("subject %q, want %q", got, want)
	}
	if len(leaf.Subject.Names) != 2 {
		t.Errorf("subject attributes %v, want CN and OU alone", leaf.Subject.Names)
	}
	checkBackdated(t, leaf, before, 24*time.Hour)
	if err := verifyLeaf(iss, leaf, x509.ExtKeyUsageServerAuth); err != nil {
		t.Errorf("verify: %v", err)
	}
	if err := verifyLeaf(iss, leaf, x509.ExtKeyUsageClientAuth); err == nil {
		t.Error("an addon's server certificate verified as a client's")
	}
	var ders [][]byte
	for rest := chain; ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			break
		}
		ders = append(ders, b.Bytes)
	}
	if len(ders) != 2 || string(ders[0]) != string(leaf.Raw) || string(ders[1]) != string(iss.cert.Raw) {
		t.Errorf("chain has %d certificates, want the leaf and the issuer", len(ders))
	}
	trust := string(iss.TrustPEM())
	if n := strings.Count(trust, "BEGIN CERTIFICATE"); n != len(iss.BundleDER())+1 {
		t.Errorf("trust has %d certificates, want the roots and the issuer", n)
	}

	for _, bad := range []struct{ id, pod, workload string }{
		{ControlPlaneID(testTD), "p", workload},
		{NodeID(testTD, "n1"), "p", workload},
		{"spiffe://other.example/tenant/acme/env/e/addon/db", "p", workload},
		{id, "", workload},
		{id, "p", ""},
		{id, "p,OU=wl-x", workload},
		{id, "p", "wl-x,CN=y"},
	} {
		if _, _, err := iss.IssueAddonServer(pub, bad.id, bad.pod, bad.workload, time.Hour); err == nil {
			t.Errorf("IssueAddonServer(%s, %q, %q) issued", bad.id, bad.pod, bad.workload)
		}
	}
}
