// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// updateFixtures rewrites the people fixtures of testdata/ferun from the
// handler tests' answers (go test ./internal/api -run People -update).
var updateFixtures = flag.Bool("update", false, "rewrite the people fixtures of testdata/ferun from the answers")

// peopleFixtures are the fixtures the handler tests of people,
// integrations and protection write (the contract's §2.16); the
// environments' fixture test does not replay them.
var peopleFixtures = map[string]bool{
	"GET_auth_me":                     true,
	"GET_project_members":             true,
	"GET_integrations":                true,
	"GET_admin_integrations":          true,
	"PUT_environments_env_protected":  true,
	"GET_environments_env_principals": true,
}

var (
	fixtureUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	fixtureHash = regexp.MustCompile(`\b[0-9a-f]{64}\b`)
	fixtureTime = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z`)
)

// fixtureNormal makes an answer stable across runs: the provider's issuer
// is FeCode ID's, every UUID and token hash is numbered in the order it
// first appears, every time is one fixed time.
func fixtureNormal(b []byte, issuer string) []byte {
	b = bytes.ReplaceAll(b, []byte(issuer), []byte("https://id.fecore.dev/"))
	ids := map[string]string{}
	b = fixtureUUID.ReplaceAllFunc(b, func(m []byte) []byte {
		if _, ok := ids[string(m)]; !ok {
			ids[string(m)] = fmt.Sprintf("0199a0b1-0000-7000-8000-%012d", len(ids)+1)
		}
		return []byte(ids[string(m)])
	})
	hashes := map[string]string{}
	b = fixtureHash.ReplaceAllFunc(b, func(m []byte) []byte {
		if _, ok := hashes[string(m)]; !ok {
			hashes[string(m)] = fmt.Sprintf("3f9a1c2b7d4e%052d", len(hashes)+1)
		}
		return []byte(hashes[string(m)])
	})
	return fixtureTime.ReplaceAll(b, []byte("2026-11-21T10:00:00Z"))
}

// writeFixture checks the answer w of method path (with query and the
// request body req) against the fixture name, after fixtureNormal; with
// -update it writes it.
func writeFixture(t *testing.T, name, issuer, method, path string, query url.Values, req any, w *httptest.ResponseRecorder) {
	t.Helper()
	type fx struct {
		Method   string              `json:"method"`
		Path     string              `json:"path"`
		Query    map[string][]string `json:"query"`
		Status   int                 `json:"status"`
		Request  json.RawMessage     `json:"request"`
		Response json.RawMessage     `json:"response"`
		Note     string              `json:"note,omitempty"`
	}
	reqRaw := json.RawMessage("null")
	if req != nil {
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		reqRaw = b
	}
	if query == nil {
		query = url.Values{}
	}
	f := fx{Method: method, Path: path, Query: query, Status: w.Code, Request: reqRaw,
		Response: json.RawMessage(fixtureNormal(bytes.TrimSpace(w.Body.Bytes()), issuer))}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join("testdata", "ferun", name+".json")
	if *updateFixtures {
		if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	have, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("fixture %s: %v (write it with -update)", name, err)
	}
	if got, want := canonicalJSON(t, buf.Bytes()), canonicalJSON(t, have); got != want {
		t.Fatalf("fixture %s differs (rewrite it with -update):\n%s\nwant\n%s", name, got, want)
	}
}

// isPeopleFixture reports a fixture of peopleFixtures by its file name.
func isPeopleFixture(name string) bool {
	key, _, _ := strings.Cut(name, ".")
	return peopleFixtures[key]
}
