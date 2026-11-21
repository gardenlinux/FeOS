// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/model"
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
		Bindings  []string `json:"bindings"`
		Latest    int      `json:"latest"`
		Deployed  int      `json:"deployed"`
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

// credPath is the path of a credential of acme's environment env.
func credPath(env, name string) string {
	p := "/api/v1/environments/" + env + "/credentials"
	if name != "" {
		p += "/" + name
	}
	return p
}

func TestCredentialVersionsAndSecrets(t *testing.T) {
	r := newRig(t)
	code, raw := r.projectCall("PUT", credPath("box", "erp"), credentialBody("Bearer one-secret-token", 0))
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
	if !strings.Contains(raw, `"usedBy":{"sandboxes":[],"objects":[],"bindings":[],"latest":0,"deployed":0}`) {
		t.Fatalf("an unused credential's uses: %s", raw)
	}

	// The same definition again is no new version.
	if code, raw = r.projectCall("PUT", credPath("box", "erp"), credentialBody("Bearer one-secret-token", 0)); code != http.StatusOK || decodeCredential(t, raw).Version != 1 {
		t.Fatalf("same again: %d %s", code, raw)
	}

	// {"set": true} keeps the secret; the rate makes version 2.
	keep := `{"upstream":"https://api.erp.example.test","pathPrefixes":["/odata/"],"rateLimit":{"perSecond":5},
		"credential":{"kind":"headers","headers":{"Authorization":{"set":true}}}}`
	if code, raw = r.projectCall("PUT", credPath("box", "erp"), keep); code != http.StatusCreated || decodeCredential(t, raw).Version != 2 {
		t.Fatalf("keep: %d %s", code, raw)
	}
	p, err := r.svc.OpenCredential(bg, r.box, "erp", 2, "erp")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.GetHeaders().GetHeaders()["Authorization"]; got != "Bearer one-secret-token" {
		t.Fatalf("version 2 kept %q", got)
	}
	if p, err = r.svc.OpenCredential(bg, r.box, "erp", 1, "x"); err != nil || p.GetName() != "x" {
		t.Fatalf("version 1 under another name: %v %v", p, err)
	}

	// Keeping a header the latest version lacks is refused.
	bad := `{"upstream":"https://api.erp.example.test","credential":{"kind":"headers","headers":{"X-Key":{"set":true}}}}`
	if code, raw = r.projectCall("PUT", credPath("box", "erp"), bad); code != http.StatusUnprocessableEntity {
		t.Fatalf("keep a missing header: %d %s", code, raw)
	}
	if code, raw = r.projectCall("GET", credPath("box", ""), ""); code != http.StatusOK || !strings.Contains(raw, `"version":2`) {
		t.Fatalf("list: %d %s", code, raw)
	}
}

// A credential is its environment's: another environment of the project
// has none of it, another project does not see the environment, and a
// version sealed for one environment does not open for another.
func TestCredentialsAreTheEnvironments(t *testing.T) {
	r := newRig(t)
	if code, raw := r.projectCall("PUT", credPath("box", "erp"), credentialBody("Bearer box-token-1", 0)); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	code, raw := r.projectCall("GET", credPath("live", "erp"), "")
	if _, c := codeOf(raw); code != http.StatusNotFound || c != codeNoCredential {
		t.Fatalf("another environment: %d %s", code, raw)
	}
	if code, raw = r.projectCall("GET", credPath("live", ""), ""); code != http.StatusOK || raw != `{"credentials":[]}`+"\n" {
		t.Fatalf("another environment's list: %d %q", code, raw)
	}
	if code, raw = r.call("GET", credPath("box", "erp"), "Bearer "+otherToken, ""); code != http.StatusNotFound ||
		strings.Contains(raw, "erp.example") {
		t.Fatalf("another project's box: %d %s", code, raw)
	}
	if _, err := r.svc.OpenCredential(bg, r.live, "erp", 0, "erp"); err == nil {
		t.Fatal("box's credential opened in live")
	}

	// The same name in live is another credential, versioned on its own.
	if code, raw = r.projectCall("PUT", credPath("live", "erp"), credentialBody("Bearer live-token-1", 0)); code != http.StatusCreated ||
		decodeCredential(t, raw).Version != 1 {
		t.Fatalf("live's own: %d %s", code, raw)
	}
	p, err := r.svc.OpenCredential(bg, r.live, "erp", 0, "erp")
	if err != nil || p.GetHeaders().GetHeaders()["Authorization"] != "Bearer live-token-1" {
		t.Fatalf("live's credential: %v %v", p, err)
	}

	// A row copied into another environment does not open: the sealing
	// names the environment.
	row, err := r.st.EnvCredential(bg, r.box, "erp", 1)
	if err != nil {
		t.Fatal(err)
	}
	row.EnvID, row.Name = r.live, "copied"
	if err := r.st.InsertEnvCredential(bg, row); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.OpenCredential(bg, r.live, "copied", 0, "copied"); err == nil {
		t.Fatal("a version sealed for box opened in live")
	}

	// A copy, as a clone makes it into its new environment, does:
	// version 1, sealed for that one. Live, whose erp had a version 1,
	// takes no copy as its version 1 again, not even after a delete.
	fresh := dbtest.Environment(t, r.st.Q(), "acme", "fresh")
	c, err := r.svc.CopyCredential(bg, r.box, "erp", fresh)
	if err != nil || c.Version != 1 || c.EnvID != fresh {
		t.Fatalf("copy: %+v %v", c, err)
	}
	if err := r.st.InsertEnvCredential(bg, c); err != nil {
		t.Fatal(err)
	}
	if p, err := r.svc.OpenCredential(bg, fresh, "erp", 1, "erp"); err != nil ||
		p.GetHeaders().GetHeaders()["Authorization"] != "Bearer box-token-1" {
		t.Fatalf("the copy: %v %v", p, err)
	}
	reused, err := r.svc.CopyCredential(bg, r.box, "erp", r.live)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := r.st.DeleteEnvCredential(bg, r.live, "erp"); !ok || err != nil {
		t.Fatalf("delete live's: %v %v", ok, err)
	}
	if err := r.st.InsertEnvCredential(bg, reused); err == nil {
		t.Fatal("live took a copy as the version 1 its deleted erp had")
	}
	if _, err := r.svc.CopyCredential(bg, r.box, "nope", r.live); err == nil {
		t.Fatal("a copy of a credential box lacks")
	}
}

func TestSandboxCredentialsReachThePodAndRotate(t *testing.T) {
	r := newRig(t)
	if code, raw := r.projectCall("PUT", credPath("box", "erp"), credentialBody("Bearer v1-token", 0)); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	if err := r.svc.CheckSandboxCredentials(bg, r.box, []string{"nope"}); err == nil {
		t.Fatal("an unknown credential was taken")
	}
	if err := r.svc.SetSandboxCredentials(bg, "acme", r.box, r.sb1, []string{"erp"}); err != nil {
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
	code, raw := r.projectCall("GET", credPath("box", "erp"), "")
	if a := decodeCredential(t, raw); code != http.StatusOK || len(a.UsedBy.Sandboxes) != 1 || a.UsedBy.Sandboxes[0] != r.sb1.String() {
		t.Fatalf("uses: %d %s", code, raw)
	}

	// A new version reaches the pod: another version, the new token.
	if code, raw := r.projectCall("PUT", credPath("box", "erp"), credentialBody("Bearer v2-token", 0)); code != http.StatusCreated {
		t.Fatalf("rotate: %d %s", code, raw)
	}
	pb, v2, err := r.svc.PodBindings(bg, r.sb1)
	if err != nil || v2 == v1 || pb.Http[0].GetHeaders().GetHeaders()["Authorization"] != "Bearer v2-token" {
		t.Fatalf("after the rotation: %q %q %v %v", v1, v2, pb, err)
	}

	// A credential of the same name in another environment changes
	// nothing for the sandbox.
	if code, raw := r.projectCall("PUT", credPath("live", "erp"), credentialBody("Bearer live-token", 0)); code != http.StatusCreated {
		t.Fatalf("live: %d %s", code, raw)
	}
	if pb, v3, err := r.svc.PodBindings(bg, r.sb1); err != nil || v3 != v2 ||
		pb.Http[0].GetHeaders().GetHeaders()["Authorization"] != "Bearer v2-token" {
		t.Fatalf("after live's write: %q %v %v", v3, pb, err)
	}

	// A credential in use is not deleted; once unnamed it is.
	code, raw = r.projectCall("DELETE", credPath("box", "erp"), "")
	if _, c := codeOf(raw); code != http.StatusConflict || c != codeCredentialInUse || !strings.Contains(raw, r.sb1.String()) {
		t.Fatalf("delete in use: %d %s", code, raw)
	}
	if err := r.svc.SetSandboxCredentials(bg, "acme", r.box, r.sb1, nil); err != nil {
		t.Fatal(err)
	}
	if code, raw = r.projectCall("DELETE", credPath("box", "erp"), ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, raw)
	}
	if pb, _, err = r.svc.PodBindings(bg, r.sb1); err != nil || pb != nil {
		t.Fatalf("after the delete the pod has %v %v", pb, err)
	}
	if code, raw = r.projectCall("DELETE", credPath("box", "erp"), ""); code != http.StatusNotFound {
		t.Fatalf("delete again: %d %s", code, raw)
	}
}

// A sandbox names credentials of its own environment only.
func TestSandboxNamesItsEnvironmentsCredentials(t *testing.T) {
	r := newRig(t)
	if code, raw := r.projectCall("PUT", credPath("live", "erp"), credentialBody("Bearer live-token", 0)); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	err := r.svc.SetSandboxCredentials(bg, "acme", r.box, r.sb1, []string{"erp"})
	if code, c := apiCode(err); code != http.StatusUnprocessableEntity || c != codeNoCredential {
		t.Fatalf("live's credential in a sandbox of box: %v", err)
	}
	if code, c := apiCode(r.svc.CheckSandboxCredentials(bg, r.box, []string{"erp"})); code != http.StatusUnprocessableEntity ||
		c != codeNoCredential {
		t.Fatal("the check took live's credential for box")
	}
	// A row naming it (what an older instance could have written) gives
	// the pod nothing: the names resolve in the sandbox's environment.
	if err := r.st.SetSandboxCredentialNames(bg, r.sb1, []string{"erp"}); err != nil {
		t.Fatal(err)
	}
	if pb, _, err := r.svc.PodBindings(bg, r.sb1); err != nil || pb != nil {
		t.Fatalf("the pod got %v %v", pb, err)
	}
}

func TestSandboxCredentialNameTakenByItsOwnSet(t *testing.T) {
	r := newRig(t)
	if code, raw := r.projectCall("PUT", credPath("box", "erp"), credentialBody("Bearer x-token-1", 0)); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	own := `{"bindings":[` + mustJSON(t, headersBinding("erp", "https://api.erp.example.test")) + `]}`
	if code, raw := r.sandboxCall("PUT", "/http", own); code != http.StatusOK {
		t.Fatalf("own set: %d %s", code, raw)
	}
	if err := r.svc.SetSandboxCredentials(bg, "acme", r.box, r.sb1, []string{"erp"}); err == nil {
		t.Fatal("a credential named like the sandbox's own binding was taken")
	}
}

// fakeUsers are an environment's deployments' uses of its credentials.
type fakeUsers map[uuid.UUID]map[string]CredentialUse

func (f fakeUsers) CredentialUses(_ context.Context, _ *store.Store, env uuid.UUID) (map[string]CredentialUse, error) {
	return f[env], nil
}

// A binding of the environment's deployment that pins a credential keeps
// it from being deleted, and the answer says which and at what version.
func TestCredentialUsedByADeployment(t *testing.T) {
	r := newRig(t)
	if code, raw := r.projectCall("PUT", credPath("box", "erp"), credentialBody("Bearer d-token-1", 0)); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	r.svc.SetCredentialUsers(fakeUsers{r.box: {"erp": {Bindings: []string{"erp-api"}, Latest: 1, Deployed: 1}}})
	code, raw := r.projectCall("GET", credPath("box", "erp"), "")
	if a := decodeCredential(t, raw); code != http.StatusOK || len(a.UsedBy.Bindings) != 1 || a.UsedBy.Bindings[0] != "erp-api" ||
		a.UsedBy.Latest != 1 || a.UsedBy.Deployed != 1 || len(a.UsedBy.Sandboxes) != 0 {
		t.Fatalf("uses: %d %s", code, raw)
	}
	code, raw = r.projectCall("DELETE", credPath("box", "erp"), "")
	if _, c := codeOf(raw); code != http.StatusConflict || c != codeCredentialInUse || !strings.Contains(raw, "binding erp-api") {
		t.Fatalf("delete in use: %d %s", code, raw)
	}
	// Another environment's uses are not box's.
	r.svc.SetCredentialUsers(fakeUsers{r.live: {"erp": {Bindings: []string{"erp-api"}, Latest: 1}}})
	if code, raw = r.projectCall("DELETE", credPath("box", "erp"), ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, raw)
	}
}

// The check of a credential: the stored latest version, or a draft of it,
// a secret field {"set": true} taken from the stored one; a draft stores
// nothing.
func TestCheckCredentialStoredAndDraft(t *testing.T) {
	r := newRig(t)
	r.allowLoopback()
	f, ca := newFakeAPI(t)
	def := map[string]any{"upstream": f.srv.URL, "caPem": ca, "pathPrefixes": []string{"/odata/"},
		"credential": map[string]any{"kind": "headers", "headers": map[string]any{"X-API-Key": httpAPIKey}}}
	if code, raw := r.projectCall("PUT", credPath("box", "api"), mustJSON(t, def)); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	probe := map[string]any{"method": "GET", "path": "/odata/ok"}
	check := func(name string, body map[string]any) (int, HTTPPreflightResult, string) {
		t.Helper()
		code, raw := r.projectCall("POST", credPath("box", name)+"/check", mustJSON(t, body))
		var res HTTPPreflightResult
		if code == http.StatusOK {
			if err := json.Unmarshal([]byte(raw), &res); err != nil {
				t.Fatal(err)
			}
		}
		if strings.Contains(raw, httpAPIKey) || strings.Contains(raw, "never answered") {
			t.Fatalf("the check answered a secret or a body: %s", raw)
		}
		return code, res, raw
	}

	// The stored version.
	if code, res, raw := check("api", map[string]any{"probe": probe}); code != http.StatusOK || !res.OK ||
		res.Probe == nil || res.Probe.Status != 200 {
		t.Fatalf("stored: %d %s", code, raw)
	}
	// A draft that keeps the key and narrows the paths: checked with the
	// stored key, and not stored.
	draft := map[string]any{"upstream": f.srv.URL, "caPem": ca, "pathPrefixes": []string{"/odata/ok"},
		"credential": map[string]any{"kind": "headers", "headers": map[string]any{"X-API-Key": map[string]any{"set": true}}}}
	if code, res, raw := check("api", map[string]any{"binding": draft, "probe": probe}); code != http.StatusOK || !res.OK ||
		res.Probe == nil || res.Probe.Status != 200 {
		t.Fatalf("draft kept: %d %s", code, raw)
	}
	if c, err := r.st.EnvCredential(bg, r.box, "api", 0); err != nil || c.Version != 1 || len(c.PathPrefixes) != 1 ||
		c.PathPrefixes[0] != "/odata/" {
		t.Fatalf("the draft was stored: %+v %v", c, err)
	}
	// A draft with another key: refused by the upstream.
	wrong := map[string]any{"upstream": f.srv.URL, "caPem": ca,
		"credential": map[string]any{"kind": "headers", "headers": map[string]any{"X-API-Key": "wrong"}}}
	if code, res, raw := check("api", map[string]any{"binding": wrong, "probe": probe}); code != http.StatusOK || res.OK ||
		res.Error == nil || res.Error.Code != codeUnauthorized {
		t.Fatalf("draft wrong: %d %s", code, raw)
	}
	// A draft of a credential not made yet: complete, it is checked; with
	// {"set": true} there is nothing to keep, 422 as on a PUT.
	if code, res, raw := check("new", map[string]any{"binding": def, "probe": probe}); code != http.StatusOK || !res.OK {
		t.Fatalf("new draft: %d %s", code, raw)
	}
	if code, _, raw := check("new", map[string]any{"binding": draft}); code != http.StatusUnprocessableEntity {
		t.Fatalf("new draft keeping a secret: %d %s", code, raw)
	}
	// A draft's flaw is the preflight's answer; no draft of a credential
	// not made is 404.
	if code, res, raw := check("api", map[string]any{"binding": map[string]any{"upstream": "http://plain.example.test",
		"credential": map[string]any{"kind": "headers", "headers": map[string]any{"X-API-Key": "k"}}}}); code != http.StatusOK ||
		res.OK || res.Error == nil {
		t.Fatalf("flawed draft: %d %s", code, raw)
	}
	if code, _, raw := check("new", map[string]any{}); code != http.StatusNotFound {
		t.Fatalf("stored check of nothing: %d %s", code, raw)
	}
	if code, _, raw := check("api", map[string]any{"binding": map[string]any{"name": "other"}}); code != http.StatusBadRequest {
		t.Fatalf("a draft of another name: %d %s", code, raw)
	}
	if _, err := r.st.EnvCredential(bg, r.box, "new", 0); err == nil {
		t.Fatal("a draft made a credential")
	}
}

// apiCode is an API error's status and code; 0 for another error or nil.
func apiCode(err error) (int, string) {
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		return 0, ""
	}
	return ae.Status, ae.Code
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

// A credential deleted and made again goes on from the versions it had:
// a number names one definition for as long as the environment lives,
// so that a pin of the old version 1 never opens the new one. The store
// refuses a version the name had.
func TestCredentialVersionsGoOnAfterADelete(t *testing.T) {
	r := newRig(t)
	for i, token := range []string{"Bearer old-1", "Bearer old-2"} {
		code, raw := r.projectCall("PUT", credPath("box", "erp"), credentialBody(token, 0))
		if code != http.StatusCreated || decodeCredential(t, raw).Version != i+1 {
			t.Fatalf("version %d: %d %s", i+1, code, raw)
		}
	}
	if code, raw := r.projectCall("DELETE", credPath("box", "erp"), ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, raw)
	}
	code, raw := r.projectCall("PUT", credPath("box", "erp"), credentialBody("Bearer new", 0))
	if code != http.StatusCreated || decodeCredential(t, raw).Version != 3 {
		t.Fatalf("made again: %d %s, want version 3", code, raw)
	}
	for _, v := range []int{1, 2} {
		if _, err := r.svc.OpenCredential(bg, r.box, "erp", v, "erp"); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("version %d of the deleted credential: %v, want none", v, err)
		}
	}
	if b, err := r.svc.OpenCredential(bg, r.box, "erp", 3, "erp"); err != nil ||
		b.GetHeaders().GetHeaders()["Authorization"] != "Bearer new" {
		t.Fatalf("version 3: %v %v", b, err)
	}
	// Another environment's name of the same credential has marks of its
	// own.
	if code, raw := r.projectCall("PUT", credPath("live", "erp"), credentialBody("Bearer live", 0)); code != http.StatusCreated ||
		decodeCredential(t, raw).Version != 1 {
		t.Fatalf("live's first: %d %s", code, raw)
	}
	err := r.st.InTx(bg, func(tx *store.Store) error {
		c, err := tx.EnvCredential(bg, r.box, "erp", 3)
		if err != nil {
			return err
		}
		c.Version = 1
		return tx.InsertEnvCredential(bg, c)
	})
	if err == nil || !strings.Contains(err.Error(), "version 1 is not above every version the name had") {
		t.Fatalf("an insert of a used version: %v", err)
	}
}

// The HTTP secrets (ADR 0055) share their environment's names with its
// value secrets: an HTTP secret's write to a value's name is 409
// secret_kind, a sandbox that names a value 422 secret_kind, one that
// names neither 422 no_secret, and the secrets' check refuses a value.
func TestHTTPSecretsAndValues(t *testing.T) {
	r := newRig(t)
	if _, err := r.st.InsertSecret(bg, store.EnvSecret{EnvID: r.box, Name: "token", Version: 1, Sealed: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	code, raw := r.projectCall("PUT", credPath("box", "token"), credentialBody("Bearer t-1", 0))
	if _, c := codeOf(raw); code != http.StatusConflict || c != CodeSecretKind || !strings.Contains(raw, "secret token is a value") {
		t.Fatalf("an HTTP secret over a value: %d %s", code, raw)
	}
	if _, created, err := r.svc.PutHTTPSecret(bg, r.box, "erp", []byte(credentialBody("Bearer e-1", 0)), "malte"); err != nil ||
		!created {
		t.Fatalf("put: %v %v", created, err)
	}
	h, err := r.svc.HTTPSecret(bg, r.box, "erp")
	if err != nil || h.CreatedBy != "malte" || h.Def.Upstream != "https://api.erp.example.test" ||
		h.Def.Credential.Headers["Authorization"] != (SecretSet{Set: true}) {
		t.Fatalf("the HTTP secret: %+v %v", h, err)
	}

	for _, c := range []struct {
		names  []string
		status int
		code   string
	}{{[]string{"token"}, http.StatusUnprocessableEntity, CodeSecretKind},
		{[]string{"nope"}, http.StatusUnprocessableEntity, CodeNoSecret}} {
		if st, got := apiCode(r.svc.CheckSandboxSecrets(bg, r.box, c.names)); st != c.status || got != c.code {
			t.Errorf("check %v: %d %s", c.names, st, got)
		}
		if st, got := apiCode(r.svc.SetSandboxSecrets(bg, "acme", r.box, r.sb1, c.names)); st != c.status || got != c.code {
			t.Errorf("set %v: %d %s", c.names, st, got)
		}
	}
	// The credentials' alias keeps its code.
	if st, got := apiCode(r.svc.CheckSandboxCredentials(bg, r.box, []string{"token"})); st != http.StatusUnprocessableEntity ||
		got != codeNoCredential {
		t.Errorf("the alias: %d %s", st, got)
	}
	if err := r.svc.SetSandboxSecrets(bg, "acme", r.box, r.sb1, []string{"erp"}); err != nil {
		t.Fatalf("a sandbox names erp: %v", err)
	}
	if names, err := r.svc.SandboxCredentialNames(bg, r.sb1); err != nil || len(names) != 1 || names[0] != "erp" {
		t.Fatalf("its names: %v %v", names, err)
	}

	code, raw = r.projectCall("POST", "/api/v1/environments/box/secrets/token/check", "")
	if _, c := codeOf(raw); code != http.StatusConflict || c != CodeSecretKind {
		t.Fatalf("a value's check: %d %s", code, raw)
	}
	code, raw = r.projectCall("POST", "/api/v1/environments/box/secrets/nope/check", "")
	if _, c := codeOf(raw); code != http.StatusNotFound || c != CodeNoSecret {
		t.Fatalf("no secret's check: %d %s", code, raw)
	}
	// A draft as the secrets' PUT sends it, its type among it.
	draft := `{"binding":{"type":"http","upstream":"https://api.erp.example.test","credential":{"kind":"headers",` +
		`"headers":{"Authorization":{"set":true}}}}}`
	code, raw = r.projectCall("POST", "/api/v1/environments/box/secrets/erp/check", draft)
	if code != http.StatusOK || !strings.Contains(raw, `"addresses"`) {
		t.Fatalf("a draft's check: %d %s", code, raw)
	}

	ok, err := r.svc.DeleteHTTPSecret(bg, r.box, "erp")
	if st, c := apiCode(err); ok || st != http.StatusConflict || c != codeSecretInUse || !strings.Contains(err.Error(), "sandbox "+r.sb1.String()) {
		t.Fatalf("delete while a sandbox names it: %v %v", ok, err)
	}
	if err := r.svc.SetSandboxSecrets(bg, "acme", r.box, r.sb1, []string{}); err != nil {
		t.Fatal(err)
	}
	if ok, err := r.svc.DeleteHTTPSecret(bg, r.box, "erp"); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if ok, err := r.svc.DeleteHTTPSecret(bg, r.box, "erp"); err != nil || ok {
		t.Fatalf("delete again: %v %v", ok, err)
	}
}
