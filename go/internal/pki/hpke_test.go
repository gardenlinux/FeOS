// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"crypto/x509"
	"net/url"
	"testing"

	"google.golang.org/protobuf/proto"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/podid"
)

func TestSealOpen(t *testing.T) {
	priv, err := HPKEKEM().GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.PublicKey().Bytes()
	if len(pub) != 32 {
		t.Fatalf("public key of %d bytes", len(pub))
	}
	pod := podid.Mint()
	b := &fleetv1.PodBindings{
		Kube: []*fleetv1.KubeBinding{{Name: "prod", Port: 6443, Server: "https://k8s.example.com",
			Credential: &fleetv1.KubeBinding_Token{Token: "secret"}}},
		Ssh: []*fleetv1.SshBinding{{Name: "git", User: "git", PrivateKey: "key"}},
	}
	sealed, err := Seal(pub, pod, b, "v7")
	if err != nil {
		t.Fatal(err)
	}
	if sealed.Version != "v7" || len(sealed.Enc) != 32 || len(sealed.Ciphertext) == 0 {
		t.Fatalf("sealed %v", sealed)
	}
	got, err := Open(priv, pod, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, b) {
		t.Errorf("opened %v, want %v", got, b)
	}

	// The version is authenticated: a changed one fails.
	changed := proto.Clone(sealed).(*fleetv1.SealedBindings)
	changed.Version = "v8"
	if _, err := Open(priv, pod, changed); err == nil {
		t.Error("opened with a changed version")
	}
	// So is the pod.
	if _, err := Open(priv, podid.Mint(), sealed); err == nil {
		t.Error("opened for another pod")
	}
	// Another run's key cannot open it.
	other, _ := HPKEKEM().GenerateKey()
	if _, err := Open(other, pod, sealed); err == nil {
		t.Error("opened with another key")
	}
	tampered := proto.Clone(sealed).(*fleetv1.SealedBindings)
	tampered.Ciphertext[0] ^= 1
	if _, err := Open(priv, pod, tampered); err == nil {
		t.Error("opened a tampered box")
	}
	if _, err := Open(priv, pod, nil); err == nil {
		t.Error("opened nothing")
	}
	// Empty bindings seal too.
	empty, err := Seal(pub, pod, &fleetv1.PodBindings{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Open(priv, pod, empty); err != nil || len(got.Kube)+len(got.Ssh) != 0 {
		t.Errorf("empty: %v %v", got, err)
	}
	if _, err := Seal([]byte("short"), pod, b, "v1"); err == nil {
		t.Error("sealed to a malformed key")
	}
	// Two seals to one key differ (a fresh encapsulation each time).
	again, _ := Seal(pub, pod, b, "v7")
	if string(again.Enc) == string(sealed.Enc) {
		t.Error("two seals share an encapsulated key")
	}
}

func TestParseNodeID(t *testing.T) {
	mk := func(uris ...string) *x509.Certificate {
		c := &x509.Certificate{}
		for _, s := range uris {
			u, err := url.Parse(s)
			if err != nil {
				t.Fatal(err)
			}
			c.URIs = append(c.URIs, u)
		}
		return c
	}
	if name, err := ParseNodeID(testTD, mk(NodeID(testTD, "n-1"))); err != nil || name != "n-1" {
		t.Errorf("ParseNodeID = %q %v", name, err)
	}
	for _, c := range []*x509.Certificate{
		nil,
		mk(),
		mk(NodeID(testTD, "a"), NodeID(testTD, "b")),
		mk(NodeID("other.test", "a")),
		mk(ControlPlaneID(testTD)),
		mk("spiffe://" + testTD + "/node/"),
		mk("spiffe://" + testTD + "/node/A"),
		mk("spiffe://" + testTD + "/node/a/b"),
		mk("spiffe://" + testTD + "/node/a?x=1"),
		mk("spiffe://" + testTD + ":1/node/a"),
		mk("https://" + testTD + "/node/a"),
		mk("spiffe://" + testTD + "/project/a/sandbox/b"),
	} {
		if name, err := ParseNodeID(testTD, c); err == nil {
			t.Errorf("ParseNodeID accepted %v as %q", c, name)
		}
	}
}
