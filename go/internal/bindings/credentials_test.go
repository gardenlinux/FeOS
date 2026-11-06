// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// credentialBody is a credential of kind headers on the ERP's API.
func credentialBody(token string, rate int) string {
	b := map[string]any{"upstream": "https://api.erp.example.test", "pathPrefixes": []string{"/odata/"},
		"credential": map[string]any{"kind": "headers", "headers": map[string]any{"Authorization": token}}}
	if rate > 0 {
		b["rateLimit"] = map[string]any{"perSecond": rate}
	}
	raw, _ := json.Marshal(b)
	return string(raw)
}

type credentialAnswer struct {
	Name       string `json:"name"`
	Upstream   string `json:"upstream"`
	Version    int    `json:"version"`
	Credential struct {
		Kind    string                     `json:"kind"`
		Headers map[string]json.RawMessage `json:"headers"`
	} `json:"credential"`
	UsedBy struct {
		Sandboxes []string `json:"sandboxes"`
	} `json:"usedBy"`
}

func decodeCredential(t *testing.T, raw string) credentialAnswer {
	t.Helper()
	var a credentialAnswer
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return a
}

func TestCredentialVersionsAndSecrets(t *testing.T) {
	r := newRig(t)
	code, raw := r.tenantCall("PUT", "/api/v1/credentials/erp", credentialBody("Bearer one-secret-token", 0))
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	a := decodeCredential(t, raw)
	if a.Version != 1 || a.Name != "erp" || a.Upstream != "https://api.erp.example.test" {
		t.Fatalf("create answered %+v", a)
	}
	if strings.Contains(raw, "one-secret-token") || string(a.Credential.Headers["Authorization"]) != `{"set":true}` {
		t.Fatalf("an answer shows a secret: %s", raw)
	}

	// The same definition again is no new version.
	if code, raw = r.tenantCall("PUT", "/api/v1/credentials/erp", credentialBody("Bearer one-secret-token", 0)); code != http.StatusOK || decodeCredential(t, raw).Version != 1 {
		t.Fatalf("same again: %d %s", code, raw)
	}

	// {"set": true} keeps the secret; the rate makes version 2.
	keep := `{"upstream":"https://api.erp.example.test","pathPrefixes":["/odata/"],"rateLimit":{"perSecond":5},
		"credential":{"kind":"headers","headers":{"Authorization":{"set":true}}}}`
	if code, raw = r.tenantCall("PUT", "/api/v1/credentials/erp", keep); code != http.StatusCreated || decodeCredential(t, raw).Version != 2 {
		t.Fatalf("keep: %d %s", code, raw)
	}
	p, err := r.svc.OpenCredential(bg, "acme", "erp", 2, "erp")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.GetHeaders().GetHeaders()["Authorization"]; got != "Bearer one-secret-token" {
		t.Fatalf("version 2 kept %q", got)
	}
	if p, err = r.svc.OpenCredential(bg, "acme", "erp", 1, "x"); err != nil || p.GetName() != "x" {
		t.Fatalf("version 1 under another name: %v %v", p, err)
	}

	// Keeping a header the latest version lacks is refused.
	bad := `{"upstream":"https://api.erp.example.test","credential":{"kind":"headers","headers":{"X-Key":{"set":true}}}}`
	if code, raw = r.tenantCall("PUT", "/api/v1/credentials/erp", bad); code != http.StatusUnprocessableEntity {
		t.Fatalf("keep a missing header: %d %s", code, raw)
	}
	// Another tenant sees none of it.
	if code, raw = r.call("GET", "/api/v1/credentials/erp", "Bearer "+otherToken, ""); code != http.StatusNotFound {
		t.Fatalf("other tenant: %d %s", code, raw)
	}
	if code, raw = r.tenantCall("GET", "/api/v1/credentials", ""); code != http.StatusOK || !strings.Contains(raw, `"version":2`) {
		t.Fatalf("list: %d %s", code, raw)
	}
}

func TestSandboxCredentialsReachThePodAndRotate(t *testing.T) {
	r := newRig(t)
	if code, raw := r.tenantCall("PUT", "/api/v1/credentials/erp", credentialBody("Bearer v1-token", 0)); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	if err := r.svc.CheckSandboxCredentials(bg, "acme", []string{"nope"}); err == nil {
		t.Fatal("an unknown credential was taken")
	}
	if err := r.svc.SetSandboxCredentials(bg, "acme", r.sb1, []string{"erp"}); err != nil {
		t.Fatal(err)
	}
	pb, v1, err := r.svc.PodBindings(bg, r.sb1)
	if err != nil || pb == nil || len(pb.Http) != 1 {
		t.Fatalf("pod bindings: %v %v", pb, err)
	}
	if h := pb.Http[0]; h.GetName() != "erp" || h.GetHeaders().GetHeaders()["Authorization"] != "Bearer v1-token" {
		t.Fatalf("pod got %v", h)
	}
	part, err := r.svc.PodHTTP(bg, mustRow(t, r, r.sb1))
	if err != nil || part == nil || len(part.Names) != 1 || part.Names[0] != "erp" {
		t.Fatalf("clear part: %v %v", part, err)
	}

	// A new version reaches the pod: another version, the new token.
	if code, raw := r.tenantCall("PUT", "/api/v1/credentials/erp", credentialBody("Bearer v2-token", 0)); code != http.StatusCreated {
		t.Fatalf("rotate: %d %s", code, raw)
	}
	pb, v2, err := r.svc.PodBindings(bg, r.sb1)
	if err != nil || v2 == v1 || pb.Http[0].GetHeaders().GetHeaders()["Authorization"] != "Bearer v2-token" {
		t.Fatalf("after the rotation: %q %q %v %v", v1, v2, pb, err)
	}

	// A credential in use is not deleted; once unnamed it is.
	code, raw := r.tenantCall("DELETE", "/api/v1/credentials/erp", "")
	if _, c := codeOf(raw); code != http.StatusConflict || c != codeCredentialInUse {
		t.Fatalf("delete in use: %d %s", code, raw)
	}
	if err := r.svc.SetSandboxCredentials(bg, "acme", r.sb1, nil); err != nil {
		t.Fatal(err)
	}
	if code, raw = r.tenantCall("DELETE", "/api/v1/credentials/erp", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, raw)
	}
	if pb, _, err = r.svc.PodBindings(bg, r.sb1); err != nil || pb != nil {
		t.Fatalf("after the delete the pod has %v %v", pb, err)
	}
}

func TestSandboxCredentialNameTakenByItsOwnSet(t *testing.T) {
	r := newRig(t)
	if code, raw := r.tenantCall("PUT", "/api/v1/credentials/erp", credentialBody("Bearer x-token-1", 0)); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	own := `{"bindings":[` + mustJSON(t, headersBinding("erp", "https://api.erp.example.test")) + `]}`
	if code, raw := r.sandboxCall("PUT", "/http", own); code != http.StatusOK {
		t.Fatalf("own set: %d %s", code, raw)
	}
	if err := r.svc.SetSandboxCredentials(bg, "acme", r.sb1, []string{"erp"}); err == nil {
		t.Fatal("a credential named like the sandbox's own binding was taken")
	}
}

func mustRow(t *testing.T, r *rig, id podid.ID) store.Sandbox {
	t.Helper()
	row, err := r.st.GetSandbox(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
