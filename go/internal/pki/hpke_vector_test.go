// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// bindingsVectorPath is the vector of feos-authn's unsealing test: a
// PodBindings that Seal sealed to a fixed key, which feos-authn's Rust
// opens (crates/feos-authn/src/bindings.rs). This test opens it as well,
// so that a change to Seal that the Rust side does not follow fails
// here. FEOS_WRITE_BINDINGS_VECTOR=1 seals it anew.
var bindingsVectorPath = filepath.Join("..", "..", "..", "crates", "feos-authn", "testdata", "sealed-bindings.json")

// The vector's fixed run key: test data, never a real key.
const vectorHPKEPrivateKey = "6c1f5d7ae0b24b8f9a3e57c2d40b1e9f8a6d3c5b7e2f1a0d9c8b7a6f5e4d3c2b"

type bindingsVector struct {
	PodID         string `json:"podId"`
	HPKEPrivate   string `json:"hpkePrivateKey"`
	HPKEPublic    string `json:"hpkePublicKey"`
	Version       string `json:"version"`
	Enc           string `json:"enc"`
	Ciphertext    string `json:"ciphertext"`
	SealedByGoPki string `json:"sealedBy"`
}

func vectorBindings() *fleetv1.PodBindings {
	return &fleetv1.PodBindings{
		Kube: []*fleetv1.KubeBinding{{Name: "prod", Port: 6443, Server: "https://k8s.example.com",
			Credential: &fleetv1.KubeBinding_Token{Token: "kube-token"}}},
		Ssh: []*fleetv1.SshBinding{{Name: "git", Server: "ssh://git.example.com", User: "git",
			HostKeys:   []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"},
			PrivateKey: "ssh-key"}},
		Http: []*fleetv1.HttpBinding{
			{Name: "erp", Upstream: "https://api.erp.example", PathPrefixes: []string{"/odata/", "/api"},
				ForwardedHost: true, RateLimit: &fleetv1.RateLimit{PerSecond: 10, Burst: 20},
				Credential: &fleetv1.HttpBinding_Headers{Headers: &fleetv1.HeaderCredential{
					Headers: map[string]string{"X-API-Key": "erp-key"}}}},
			{Name: "pay", Upstream: "https://api.pay.example:8443", CaPem: "ca-pem",
				Credential: &fleetv1.HttpBinding_Basic{Basic: &fleetv1.BasicCredential{User: "u", Password: "p"}}},
			{Name: "graph", Upstream: "https://graph.example",
				Credential: &fleetv1.HttpBinding_Oauth2{Oauth2: &fleetv1.OAuthClientCredentials{
					TokenUrl: "https://login.example/token", ClientId: "id", ClientSecret: "secret",
					Scope: "read", Audience: "graph", ClientAuthInBody: true}}},
			{Name: "partner", Upstream: "https://partner.example", Mode: fleetv1.HttpBindingMode_HTTP_BINDING_MODE_INTERCEPT,
				Credential: &fleetv1.HttpBinding_ClientCertificate{ClientCertificate: &fleetv1.ClientCertificate{
					CertPem: "cert-pem", KeyPem: "key-pem"}}},
		},
		InterceptCas: []*fleetv1.InterceptCa{{CertPem: "old-ca", KeyPem: "old-key"}, {CertPem: "new-ca", KeyPem: "new-key"}},
	}
}

func TestBindingsVector(t *testing.T) {
	priv, err := HPKEKEM().NewPrivateKey(mustHex(t, vectorHPKEPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	pod, err := podid.Parse("k2v8d0h3q9x")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("FEOS_WRITE_BINDINGS_VECTOR") != "" {
		sealed, err := Seal(priv.PublicKey().Bytes(), pod, vectorBindings(), "v3")
		if err != nil {
			t.Fatal(err)
		}
		v := bindingsVector{
			PodID: pod.String(), HPKEPrivate: vectorHPKEPrivateKey,
			HPKEPublic: hex.EncodeToString(priv.PublicKey().Bytes()), Version: sealed.GetVersion(),
			Enc:           base64.StdEncoding.EncodeToString(sealed.GetEnc()),
			Ciphertext:    base64.StdEncoding.EncodeToString(sealed.GetCiphertext()),
			SealedByGoPki: "go/internal/pki.Seal (TestBindingsVector, FEOS_WRITE_BINDINGS_VECTOR=1)",
		}
		out, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bindingsVectorPath, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(bindingsVectorPath)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("no %s", bindingsVectorPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	var v bindingsVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	enc, err := base64.StdEncoding.DecodeString(v.Enc)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := base64.StdEncoding.DecodeString(v.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if v.PodID != pod.String() || v.HPKEPublic != hex.EncodeToString(priv.PublicKey().Bytes()) {
		t.Fatalf("the vector is for pod %s, key %s", v.PodID, v.HPKEPublic)
	}
	got, err := Open(priv, pod, &fleetv1.SealedBindings{Enc: enc, Ciphertext: ct, Version: v.Version})
	if err != nil {
		t.Fatalf("open the vector: %v", err)
	}
	if !proto.Equal(got, vectorBindings()) {
		t.Errorf("the vector holds %v, want %v", got, vectorBindings())
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
