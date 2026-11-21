// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"os"
	"testing"
)

// The tests' fixtures and helpers. The spec's own tests (Parse, the
// types' JSON forms, MergePatch) are the module's, in
// github.com/gardenlinux/feos/go/ferun/spec, which keeps the same ones.

// The spec of the contract's §3 (the Expensa example).
const expensa = `{
  "services": {
    "expense-service": {
      "runtime": "image",
      "image": "ghcr.io/acme/expense:1.4",
      "command": ["/app/expense-service"],
      "env": {
        "EXPENSA_HTTP_ADDR": "[::]:8080",
        "EXPENSA_DATABASE_URL": {"addon": "postgres", "key": "url", "role": "expense_svc", "database": "expensa"},
        "EXPENSA_NATS_URL": {"addon": "nats", "key": "url"},
        "EXPENSA_INTERNAL_TOKEN": {"secret": "internal-token"},
        "EXPENSA_APPROVAL_URL": {"service": "approval-service", "port": "http", "key": "url"}
      },
      "ports": [{"name": "http", "port": 8080}],
      "replicas": 2,
      "resources": {"vcpus": 1, "memoryMib": 512},
      "readiness": {"http": {"port": "http", "path": "/readyz"}},
      "idle": "15m",
      "rollout": {"timeout": "10m"}
    },
    "web": {
      "runtime": "attached",
      "sandbox": "0193a000-0000-7000-8000-00000000000a",
      "command": ["npm", "run", "dev", "--", "--host", "::", "--port", "5173"],
      "cwd": "/workspace/expensa/web",
      "env": {},
      "ports": [{"name": "http", "port": 5173}],
      "readiness": {"tcp": {"port": "http"}}
    }
  },
  "jobs": {
    "migrate": {"image": "ghcr.io/acme/expense:1.4", "command": ["/app/expense-service", "migrate"],
                "env": {}, "on": "release", "timeout": "10m", "retries": 1,
                "resources": {"vcpus": 1, "memoryMib": 512}}
  },
  "addons": {
    "postgres": {"type": "postgres", "version": "17", "plan": "small",
                 "options": {"databases": ["expensa"], "roles": ["expense_svc", "approval_svc"]}},
    "nats": {"type": "nats", "version": "2", "plan": "small", "options": {"jetstream": true}}
  },
  "routes": {
    "api-expense": {"path": "/api/expense", "service": "expense-service", "port": "http", "strip": false},
    "root": {"path": "/", "service": "web", "port": "http"}
  },
  "secrets": {"internal-token": {"version": 3}}
}`

// mustParse parses a spec that has no findings.
func mustParse(t *testing.T, raw string) Spec {
	t.Helper()
	s, fs := Parse([]byte(raw))
	if len(fs) > 0 {
		t.Fatalf("findings %+v", fs)
	}
	return s
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// canonical is b decoded and encoded again: keys sorted, no space.
func canonical(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
