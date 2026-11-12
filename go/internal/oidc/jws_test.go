// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/oidc/oidctest"
)

// A key set keeps the keys it can verify with, and the algorithm a key
// names binds it.
func TestKeySet(t *testing.T) {
	t.Parallel()
	rs := oidctest.NewKey(t, "r", AlgRS256)
	es := oidctest.NewKey(t, "e", AlgES256)
	ed := oidctest.NewKey(t, "d", AlgEdDSA)
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	weak := map[string]string{"kty": "RSA", "kid": "weak", "n": enc(small.N.Bytes()), "e": enc(big.NewInt(int64(small.E)).Bytes())}
	esAsRS := es.JWK()
	esAsRS["kid"], esAsRS["alg"] = "mislabelled", "RS256"
	enc2 := rs.JWK()
	enc2["kid"], enc2["use"] = "encryption", "enc"
	set, _ := json.Marshal(map[string]any{"keys": []map[string]string{rs.JWK(), es.JWK(), ed.JWK(), weak, esAsRS, enc2,
		{"kty": "oct", "kid": "hmac", "k": "c2VjcmV0"}}})
	keys, err := parseKeySet(set)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, k := range keys {
		got = append(got, k.kid+":"+k.alg)
	}
	if strings.Join(got, ",") != "r:RS256,e:ES256,d:EdDSA" {
		t.Fatalf("keys %v", got)
	}
	if _, err := parseKeySet([]byte(`{"keys":[{"kty":"oct","k":"eA"}]}`)); err == nil {
		t.Fatal("a set without a usable key")
	}

	payload := []byte(`{"sub":"x"}`)
	for _, k := range []oidctest.Key{rs, es, ed} {
		tok := k.Sign(t, map[string]any{"alg": k.Alg, "kid": k.Kid}, payload)
		if p, err := verifyJWS(tok, keys); err != nil || string(p) != string(payload) {
			t.Fatalf("%s: %s %v", k.Alg, p, err)
		}
		// Without a kid every key of the algorithm is tried.
		tok = k.Sign(t, map[string]any{"alg": k.Alg}, payload)
		if _, err := verifyJWS(tok, keys); err != nil {
			t.Fatalf("%s without kid: %v", k.Alg, err)
		}
	}
	for _, c := range []struct {
		name, token, msg string
	}{
		{"not compact", "a.b", "the token is not a compact JWS"},
		{"header", "!.e30.AA", "the token's header is not base64url"},
		{"key of another algorithm", rs.Sign(t, map[string]any{"alg": "ES256", "kid": "r"}, payload),
			"no key of the provider verifies the token's algorithm: ES256"},
		{"crit", rs.Sign(t, map[string]any{"alg": "RS256", "kid": "r", "crit": []string{"b64"}}, payload),
			`the token names critical header parameters: ["b64"]`},
		{"unknown kid", rs.Sign(t, map[string]any{"alg": "RS256", "kid": "zz"}, payload),
			"the token's key is not in the provider's key set"},
		{"signed by another key", oidctest.NewKey(t, "r", AlgRS256).Sign(t, map[string]any{"alg": "RS256", "kid": "r"}, payload),
			"the token's signature does not verify"},
	} {
		if _, err := verifyJWS(c.token, keys); err == nil || err.Error() != c.msg {
			t.Errorf("%s: %v, want %q", c.name, err, c.msg)
		}
	}
}
