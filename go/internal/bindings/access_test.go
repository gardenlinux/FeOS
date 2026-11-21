// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// authnRow is an attributed audit record of sb's feos-authn.
func authnRow(sb podid.ID, ts int64, kind, name, detail string) store.ActivityRow {
	return store.ActivityRow{TsNs: ts, SandboxID: sb, PodID: sb, ProjectID: "acme", Node: "n1",
		Source: store.SourceAuthn, Kind: kind, Category: "credential", Name: name, Detail: json.RawMessage(detail)}
}

func (r *rig) record(rows ...store.ActivityRow) {
	r.t.Helper()
	now := time.Now()
	if err := r.d.EnsurePartitions(bg, now.Add(-24*time.Hour), now.Add(24*time.Hour)); err != nil {
		r.t.Fatal(err)
	}
	if err := r.st.InsertActivity(bg, rows...); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) access(id podid.ID) accessOut {
	r.t.Helper()
	code, raw := r.projectCall(http.MethodGet, "/api/v1/sandboxes/"+id.String()+"/access", "")
	if code != http.StatusOK {
		r.t.Fatalf("GET access: HTTP %d %s", code, raw)
	}
	var out accessOut
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// The owner reads both sets with their use from the authn audit rows;
// the material never comes back.
func TestAccessReadsBindingsWithUse(t *testing.T) {
	r := newRig(t)
	r.putKube(setBody(tokenBinding("prod", 6443, "https://k8s.example.test:6443"),
		tokenBinding("staging", 7443, "https://k8s.example.test:7443")))
	r.putSSH(setBody(genBinding("gh", "ssh://git.example.test", "git", hostLine(t, newEd25519(t))),
		genBinding("ci", "ssh://git.example.test", "ci", hostLine(t, newEd25519(t)))))

	t0 := time.Date(2026, 10, 16, 12, 0, 0, 123456789, time.UTC).UnixNano()
	rows := []store.ActivityRow{
		authnRow(r.sb1, t0+1, "kube_request", "GET /api/v1/pods 200", `{"binding":"prod","status":200}`),
		authnRow(r.sb1, t0+3, "kube_request", "GET /api/v1/secrets 403", `{"binding":"prod","status":403}`),
		authnRow(r.sb1, t0+2, "kube_request", "GET /api/v1/x 302", `{"binding":"prod","status":302}`),
		authnRow(r.sb1, t0+4, "kube_request", "GET /version", `{"binding":"prod"}`), // no status: an error
		authnRow(r.sb1, t0+5, "ssh_signature", "ssh signature (gh)", `{"binding":"gh","user":"git"}`),
		authnRow(r.sb1, t0+6, "ssh_signature", "ssh signature (gh)", `{"binding":"gh","user":"git"}`),
		authnRow(r.sb1, t0+7, "ssh_deny", "denied: no_binding", `{"reason":"no_binding","detail":"host x"}`),
		authnRow(r.sb1, t0+8, "kube_deny", "denied: destination_denied",
			`{"binding":"staging","reason":"destination_denied","detail":"10.0.0.10 is internal"}`),
		// Another sandbox's rows, and rows of another source, never count.
		authnRow(r.sb2, t0+9, "kube_request", "GET / 200", `{"binding":"prod","status":200}`),
		{TsNs: t0 + 10, SandboxID: r.sb1, Source: store.SourceFerund, Kind: "kube_request", Name: "x",
			Detail: json.RawMessage(`{"binding":"prod","status":200}`)},
		// A foreign record carries no sandbox.
		{TsNs: t0 + 11, PodID: r.sb1, ForeignPod: true, Node: "n2", Source: store.SourceAuthn, Kind: "kube_deny",
			Name: "denied: foreign", Detail: json.RawMessage(`{"binding":"prod","reason":"foreign"}`)},
	}
	r.record(rows...)

	code, raw := r.projectCall(http.MethodGet, "/api/v1/sandboxes/"+r.sb1.String()+"/access", "")
	if code != http.StatusOK {
		t.Fatalf("GET access: %d %s", code, raw)
	}
	if strings.Contains(raw, kubeTestToken) || strings.Contains(raw, "PRIVATE KEY") {
		t.Fatalf("access leaks material: %s", raw)
	}
	var out accessOut
	_ = json.Unmarshal([]byte(raw), &out)
	if out.Kube == nil || len(out.Kube.Bindings) != 2 || out.SSH == nil || len(out.SSH.Bindings) != 2 {
		t.Fatalf("sets: %s", raw)
	}
	prod := out.Kube.Bindings[0]
	if prod.Name != "prod" || prod.Fingerprint == "" || prod.Use == nil || prod.Use.Uses != 4 || prod.Use.Errors != 2 ||
		prod.Use.LastUse != "GET /version" || prod.Use.LastUsedAt != "2026-10-16T12:00:00.123456793Z" {
		t.Errorf("kube prod: %+v use %+v", prod.kubeBindingOut, prod.Use)
	}
	if staging := out.Kube.Bindings[1]; staging.Name != "staging" || staging.Use != nil {
		t.Errorf("kube staging (never used: use is null): %+v", staging.Use)
	}
	var gh, ci *sshAccessOut
	for i := range out.SSH.Bindings {
		switch out.SSH.Bindings[i].Name {
		case "gh":
			gh = &out.SSH.Bindings[i]
		case "ci":
			ci = &out.SSH.Bindings[i]
		}
	}
	if gh == nil || gh.Use == nil || gh.Use.Uses != 2 || gh.Use.Errors != 0 || gh.Use.LastUse != "ssh signature (gh)" ||
		!strings.HasPrefix(gh.PublicKey, "ssh-ed25519 ") {
		t.Errorf("ssh gh: %+v", gh)
	}
	if ci == nil || ci.Use != nil {
		t.Errorf("ssh ci (never used): %+v", ci)
	}
	if len(out.Denies) != 2 {
		t.Fatalf("denies: %+v", out.Denies)
	}
	if d := out.Denies[0]; d.Kind != "kube" || d.Binding != "staging" || d.Reason != "destination_denied" ||
		d.Detail != "10.0.0.10 is internal" || d.Name != "denied: destination_denied" || d.At != "2026-10-16T12:00:00.123456797Z" {
		t.Errorf("newest deny: %+v", d)
	}
	if d := out.Denies[1]; d.Kind != "ssh" || d.Binding != "" || d.Reason != "no_binding" || d.Detail != "host x" {
		t.Errorf("second deny: %+v", d)
	}
	// The JSON shape FeCode's UI reads.
	for _, field := range []string{`"use":{"uses":4,"errors":2,"lastUsedAt":"2026-10-16T12:00:00.123456793Z","lastUse":"GET /version"}`,
		`"use":null`, `"denies":[{"kind":"kube","binding":"staging","reason":"destination_denied"`} {
		if !strings.Contains(raw, field) {
			t.Errorf("answer lacks %s: %s", field, raw)
		}
	}
}

// At most 20 denies, newest first.
func TestAccessDenyLimit(t *testing.T) {
	r := newRig(t)
	var rows []store.ActivityRow
	for i := range 25 {
		rows = append(rows, authnRow(r.sb1, int64(1000+i), "ssh_deny", fmt.Sprintf("deny %d", i), `{"reason":"r"}`))
	}
	r.record(rows...)
	out := r.access(r.sb1)
	if len(out.Denies) != accessDenyLimit || out.Denies[0].Name != "deny 24" || out.Denies[19].Name != "deny 5" {
		t.Fatalf("denies: %d, first %+v", len(out.Denies), out.Denies[0])
	}
	if out.Kube == nil || out.Kube.Bindings == nil || out.SSH == nil || out.SSH.Bindings == nil {
		t.Fatalf("empty sets must be present and empty: %+v", out)
	}
}

// Only the owner's project bearer gets in; a foreign or absent sandbox is
// 404, a non-UUID 400.
func TestAccessAuth(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct {
		name, path, auth string
		want             int
	}{
		{"sandbox token", "/api/v1/sandboxes/" + r.sb1.String() + "/access", "Bearer " + sb1Token, http.StatusUnauthorized},
		{"no token", "/api/v1/sandboxes/" + r.sb1.String() + "/access", "", http.StatusUnauthorized},
		{"unknown sandbox", "/api/v1/sandboxes/" + podid.Mint().String() + "/access", "Bearer " + projectToken, http.StatusNotFound},
		{"another project", "/api/v1/sandboxes/" + r.sb1.String() + "/access", "Bearer " + otherToken, http.StatusNotFound},
		{"not a uuid", "/api/v1/sandboxes/sb-9/access", "Bearer " + projectToken, http.StatusBadRequest},
	} {
		if code, body := r.call(http.MethodGet, c.path, c.auth, ""); code != c.want {
			t.Errorf("%s: HTTP %d %s, want %d", c.name, code, body, c.want)
		}
	}
	_, raw := r.call(http.MethodGet, "/api/v1/sandboxes/"+r.sb1.String()+"/access", "Bearer "+otherToken, "")
	if msg, _ := codeOf(raw); msg != "unknown sandbox "+r.sb1.String() {
		t.Errorf("foreign sandbox: %s", raw)
	}
}

// Off, neither surface exists: both sets are null, not empty, and the
// route still answers, whatever is stored.
func TestAccessWhenOff(t *testing.T) {
	r := newRigWith(t, false)
	r.record(authnRow(r.sb1, 1, "ssh_deny", "denied", `{"reason":"r"}`))
	code, raw := r.projectCall(http.MethodGet, "/api/v1/sandboxes/"+r.sb1.String()+"/access", "")
	if want := `{"kube":null,"ssh":null,"denies":[]}`; code != http.StatusOK || strings.TrimSpace(raw) != want {
		t.Fatalf("access off: %d %s, want %s", code, raw, want)
	}
	if code, _ := r.call(http.MethodGet, "/api/v1/sandboxes/"+r.sb1.String()+"/access", "Bearer "+otherToken, ""); code != http.StatusNotFound {
		t.Fatalf("access off, another project: %d", code)
	}
}
