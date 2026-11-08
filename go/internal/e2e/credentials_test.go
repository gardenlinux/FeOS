// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/pki"
)

// Scenario 10: a running pod's credentials are issued to its node and
// opened there; the sealed bindings are bound to their version (the HPKE
// aad), so that a box with another version does not open; a pod on
// another node and a paused pod get none.
func TestCredentials(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "secret.key")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newCluster(t, func(cfg *config.Config) {
		cfg.SecretKeyFile = keyFile
		cfg.Bindings = true
	})
	c.start("a")
	c.start("b")
	nodes := map[string]*node{"n1": c.join("n1"), "n2": c.join("n2")}
	sb := c.create(nil)
	c.run(nil, sb, "true")
	on := nodes[c.row(sb.id).Node]
	other := nodes["n1"]
	if on == other {
		other = nodes["n2"]
	}

	r := c.call(nil, "PUT", sb.path("/kube"), sb.auth(),
		`{"bindings":[{"name":"prod","port":6443,"server":"https://203.0.113.20:6443","auth":{"type":"token","token":"e2e-kube-token"}}]}`)
	if r.code != 200 {
		t.Fatalf("PUT kube: %d %s", r.code, r.body)
	}

	creds, err := on.IssuePodCredentials(bg, sb.id)
	if err != nil {
		t.Fatal(err)
	}
	row := c.row(sb.id)
	kind, name, set := row.Workload()
	wantID, wantSubject := pki.PodIdentity(trustDomain, tenant, row.EnvironmentID, sb.id, kind, name, set)
	if uris := creds.Leaf.URIs; len(uris) != 1 || uris[0].String() != wantID || kind != pki.WorkloadSandbox {
		t.Errorf("the pod's identity %v, want %s (%s)", uris, wantID, kind)
	}
	if got := creds.Leaf.Subject.String(); got != wantSubject.String() || got != "CN="+trustDomain+"/"+sb.id.String()+",OU=wl-s-"+sb.id.String() {
		t.Errorf("the pod's subject %q", got)
	}
	kube := creds.Bindings.GetKube()
	if len(kube) != 1 || kube[0].GetName() != "prod" || kube[0].GetPort() != 6443 || kube[0].GetToken() != "e2e-kube-token" {
		t.Errorf("bindings %v", creds.Bindings)
	}
	var stored string
	if err := c.db.Pool.QueryRow(bg, "SELECT bindings_version FROM issued_certs WHERE serial = $1", pki.Serial(creds.Leaf)).
		Scan(&stored); err != nil || creds.Version == "" || stored != creds.Version {
		t.Errorf("version %q, issued_certs %q (%v)", creds.Version, stored, err)
	}

	// The box opens with its own version only.
	tc, _ := on.Certificate()
	id := identity{name: on.name, cert: tc, leaf: tc.Leaf, bundle: c.issuer.Bundle()}
	_, podKey, _ := ed25519.GenerateKey(rand.Reader)
	hk, err := pki.HPKEKEM().GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.CreateCSR(podKey, sb.id.String())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.rawUplink(c.pick().NodeAddr(), id).IssuePodCredentials(bg, &fleetv1.IssuePodCredentialsRequest{
		PodId: sb.id.String(), Csr: csr, HpkePublicKey: hk.PublicKey().Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	sealed := resp.GetBindings()
	if opened, err := pki.Open(hk, sb.id, sealed); err != nil || !proto.Equal(opened, creds.Bindings) {
		t.Fatalf("open: %v %v", opened, err)
	}
	tampered := proto.Clone(sealed).(*fleetv1.SealedBindings)
	tampered.Version += "0"
	if _, err := pki.Open(hk, sb.id, tampered); err == nil {
		t.Error("the box opened with another version")
	}

	// Another node gets nothing for the pod.
	_, err = other.IssuePodCredentials(bg, sb.id)
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "not placed on node "+other.name) {
		t.Errorf("credentials on another node: %v", err)
	}
	// A paused pod gets nothing.
	c.pause(nil, sb)
	_, err = on.IssuePodCredentials(bg, sb.id)
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "has no run on node "+on.name) {
		t.Errorf("credentials of a paused pod: %v", err)
	}
}
