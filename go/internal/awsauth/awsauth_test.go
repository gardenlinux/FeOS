// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package awsauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A token is the presigned connect of the user at the endpoint: the
// query RDS checks, signed for rds-db in the region, the session token
// within the signature.
func TestRDSToken(t *testing.T) {
	c := Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET", SessionToken: "SESSION/+="}
	at := time.Date(2026, 11, 17, 19, 30, 0, 0, time.UTC)
	tok := RDSToken(c, "eu-west-1", "db.example.eu-west-1.rds.amazonaws.com", 5432, "ferun", at)
	host, raw, ok := strings.Cut(tok, "/?")
	if !ok || host != "db.example.eu-west-1.rds.amazonaws.com:5432" {
		t.Fatalf("token %q: want <host>:<port>/?<query>", tok)
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"Action": "connect", "DBUser": "ferun", "X-Amz-Algorithm": "AWS4-HMAC-SHA256",
		"X-Amz-Credential": "AKID/20261117/eu-west-1/rds-db/aws4_request", "X-Amz-Date": "20261117T193000Z",
		"X-Amz-Expires": "900", "X-Amz-SignedHeaders": "host", "X-Amz-Security-Token": "SESSION/+=",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if len(q.Get("X-Amz-Signature")) != 64 || strings.Contains(raw, "+") || strings.Contains(raw, "SESSION/") {
		t.Fatalf("signature or escaping wrong: %q", raw)
	}
	if again := RDSToken(c, "eu-west-1", "db.example.eu-west-1.rds.amazonaws.com", 5432, "ferun", at); again != tok {
		t.Fatal("the same input gave another token")
	}
	other := c
	other.SecretAccessKey = "OTHER"
	if RDSToken(other, "eu-west-1", "db.example.eu-west-1.rds.amazonaws.com", 5432, "ferun", at) == tok {
		t.Fatal("another secret gave the same signature")
	}
}

// A Pod Identity's credentials are fetched with the token file's content
// and kept until five minutes before they expire.
func TestSourcePodIdentity(t *testing.T) {
	calls := 0
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "pod-token" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"AccessKeyId":"AK","SecretAccessKey":"SK","Token":"ST","Expiration":"` + exp + `"}`))
	}))
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("pod-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": srv.URL, "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": file}
	s := NewSource()
	s.getenv = func(k string) string { return env[k] }
	for range 2 {
		c, err := s.Get(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if c.AccessKeyID != "AK" || c.SecretAccessKey != "SK" || c.SessionToken != "ST" {
			t.Fatalf("credentials %+v", c)
		}
	}
	if calls != 1 {
		t.Fatalf("%d fetches; want one, then the cached credentials", calls)
	}
	env = map[string]string{"AWS_ACCESS_KEY_ID": "STATIC", "AWS_SECRET_ACCESS_KEY": "KEY"}
	if c, err := s.Get(context.Background()); err != nil || c.AccessKeyID != "STATIC" {
		t.Fatalf("static keys: %+v, %v", c, err)
	}
	env = map[string]string{}
	s = NewSource()
	s.getenv = func(k string) string { return env[k] }
	if _, err := s.Get(context.Background()); err == nil {
		t.Fatal("no credentials gave no error")
	}
}
