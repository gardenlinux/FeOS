// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
)

// A key set keeps the keys it can verify with, and the algorithm a key
// names binds it.
func TestKeySet(t *testing.T) {
	t.Parallel()
	rs := newTestKey(t, "r", algRS256)
	es := newTestKey(t, "e", algES256)
	ed := newTestKey(t, "d", algEdDSA)
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	weak := map[string]string{"kty": "RSA", "kid": "weak", "n": enc(small.N.Bytes()), "e": enc(big.NewInt(int64(small.E)).Bytes())}
	esAsRS := es.jwk()
	esAsRS["kid"], esAsRS["alg"] = "mislabelled", "RS256"
	enc2 := rs.jwk()
	enc2["kid"], enc2["use"] = "encryption", "enc"
	set, _ := json.Marshal(map[string]any{"keys": []map[string]string{rs.jwk(), es.jwk(), ed.jwk(), weak, esAsRS, enc2,
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
	for _, k := range []testKey{rs, es, ed} {
		tok := k.sign(t, map[string]any{"alg": k.alg, "kid": k.kid}, payload)
		if p, err := verifyJWS(tok, keys); err != nil || string(p) != string(payload) {
			t.Fatalf("%s: %s %v", k.alg, p, err)
		}
		// Without a kid every key of the algorithm is tried.
		tok = k.sign(t, map[string]any{"alg": k.alg}, payload)
		if _, err := verifyJWS(tok, keys); err != nil {
			t.Fatalf("%s without kid: %v", k.alg, err)
		}
	}
	for _, c := range []struct {
		name, token, msg string
	}{
		{"not compact", "a.b", "the token is not a compact JWS"},
		{"header", "!.e30.AA", "the token's header is not base64url"},
		{"key of another algorithm", rs.sign(t, map[string]any{"alg": "ES256", "kid": "r"}, payload),
			"no key of the provider verifies the token's algorithm: ES256"},
		{"crit", rs.sign(t, map[string]any{"alg": "RS256", "kid": "r", "crit": []string{"b64"}}, payload),
			`the token names critical header parameters: ["b64"]`},
		{"unknown kid", rs.sign(t, map[string]any{"alg": "RS256", "kid": "zz"}, payload),
			"the token's key is not in the provider's key set"},
		{"signed by another key", newTestKey(t, "r", algRS256).sign(t, map[string]any{"alg": "RS256", "kid": "r"}, payload),
			"the token's signature does not verify"},
	} {
		if _, err := verifyJWS(c.token, keys); err == nil || err.Error() != c.msg {
			t.Errorf("%s: %v, want %q", c.name, err, c.msg)
		}
	}
}
