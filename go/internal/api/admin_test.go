// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/fleet"
	"github.com/gardenlinux/feos/go/internal/leader"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The admin class with Basic: neither way configured is a 503; a wrong
// password names the user (and the paste mistake); a tenant's bearer is no
// admin's.
func TestAdminBasic(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	for _, c := range []struct {
		authz  string
		status int
		msg    string
	}{
		{"", 401, "admin sign-in required"},
		{bearerOf(e.tenantToken), 401, "admin sign-in required"},
		{basicOf(testAdmin, "wrong"), 401, `admin basic auth failed for user "admin"`},
		{basicOf("nobody", testPassword), 401, `admin basic auth failed for user "nobody"`},
		{basicOf(testAdmin, testPassword+",dev:other"), 401, `admin basic auth failed for user "admin" — the password looks ` +
			`like the rest of the --admin-users list; that value holds comma-separated user:password pairs, pass exactly one to curl -u`},
		{"Basic !!!", 401, "admin basic auth failed: the Authorization header is not Basic <base64 user:password>"},
	} {
		w := e.do("GET", "/api/v1/admin/tenants", c.authz, nil)
		expect(t, w, c.status, errBody(c.msg))
		if got := w.Header().Get("WWW-Authenticate"); got != `Basic realm="ferun"` {
			t.Fatalf("WWW-Authenticate %q", got)
		}
	}
	expect(t, e.do("GET", "/api/v1/admin/tenants", adminAuth(), nil), http.StatusOK, "")

	none := newEnv(t, envOptions{noAdminUsers: true})
	w := none.do("GET", "/api/v1/admin/tenants", adminAuth(), nil)
	expect(t, w, http.StatusServiceUnavailable, errBody("no admin sign-in configured (--oidc-issuer or --admin-users)"))
}

// Tenants: the list shows hashes, never tokens; a create shows its token
// once; the refusals' texts; a deleting tenant's creates and mints are 409
// and its tokens stop resolving at once.
func TestTenants(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	w := e.do("POST", "/api/v1/admin/tenants", adminAuth(), map[string]any{"name": "beta"})
	expect(t, w, http.StatusCreated, "")
	if got := keysInOrder(t, w.Body.Bytes()); strings.Join(got, ",") != "name,token,createdAt,sandboxCount" {
		t.Fatalf("keys %v", got)
	}
	created := decode[struct {
		Name, Token, CreatedAt string
		SandboxCount           int
	}](t, w)
	if created.Name != "beta" || len(created.Token) != 64 || !rfc3339Seconds.MatchString(created.CreatedAt) {
		t.Fatalf("created %+v", created)
	}
	// The token works as the tenant's.
	expect(t, e.do("GET", "/api/v1/sandboxes", bearerOf(created.Token), nil), http.StatusOK, `{"sandboxes":[]}`+"\n")

	for _, c := range []struct {
		name   string
		status int
		msg    string
	}{
		{"Bad_Name", 400, "tenant name must match ^[a-z0-9][a-z0-9.-]{1,62}$"},
		{"x", 400, "tenant name must match ^[a-z0-9][a-z0-9.-]{1,62}$"},
		{"beta", 409, "tenant beta already exists"},
	} {
		w := e.do("POST", "/api/v1/admin/tenants", adminAuth(), map[string]any{"name": c.name})
		expect(t, w, c.status, errBody(c.msg))
	}

	e.createSandbox()
	w = e.do("GET", "/api/v1/admin/tenants", adminAuth(), nil)
	expect(t, w, http.StatusOK, "")
	list := decode[struct {
		Tenants []struct {
			Name             string            `json:"name"`
			CreatedAt        string            `json:"createdAt"`
			SandboxCount     int               `json:"sandboxCount"`
			EnvironmentCount *int              `json:"environmentCount"`
			Tokens           []json.RawMessage `json:"tokens"`
		} `json:"tenants"`
	}](t, w)
	if len(list.Tenants) != 2 || list.Tenants[0].Name != testTenant || list.Tenants[1].Name != "beta" ||
		list.Tenants[0].SandboxCount != 1 || len(list.Tenants[1].Tokens) != 1 ||
		list.Tenants[0].EnvironmentCount == nil || *list.Tenants[0].EnvironmentCount != 0 {
		t.Fatalf("tenants %+v", list.Tenants)
	}
	if strings.Contains(w.Body.String(), created.Token) {
		t.Fatal("the list shows a token")
	}
	var tok map[string]any
	if err := json.Unmarshal(list.Tenants[1].Tokens[0], &tok); err != nil {
		t.Fatal(err)
	}
	if tok["hash"] != store.HashToken(created.Token) || tok["hashPrefix"] != store.HashToken(created.Token)[:8] ||
		tok["label"] != "default" {
		t.Fatalf("token %+v", tok)
	}

	// The tenant's own token routes.
	w = e.do("POST", "/api/v1/admin/tenants/beta/tokens", adminAuth(), map[string]any{"label": "ci"})
	expect(t, w, http.StatusCreated, "")
	second := decode[mintedToken](t, w)
	expect(t, e.do("GET", "/api/v1/sandboxes", bearerOf(second.Token), nil), http.StatusOK, "")
	w = e.do("GET", "/api/v1/admin/tenants/beta/tokens", adminAuth(), nil)
	toks := decode[struct {
		Tokens []map[string]any `json:"tokens"`
	}](t, w)
	if len(toks.Tokens) != 2 || toks.Tokens[1]["label"] != "ci" {
		t.Fatalf("tokens %+v", toks)
	}
	w = e.do("DELETE", "/api/v1/admin/tenants/beta/tokens/"+store.HashToken(second.Token), adminAuth(), nil)
	expect(t, w, http.StatusOK, `{"revoked":true}`+"\n")
	expect(t, e.do("GET", "/api/v1/sandboxes", bearerOf(second.Token), nil), http.StatusUnauthorized,
		errBody("unknown tenant token"))
	expect(t, e.do("GET", "/api/v1/admin/tenants/nope/tokens", adminAuth(), nil), http.StatusNotFound,
		errBody("unknown tenant nope"))
	expect(t, e.do("POST", "/api/v1/admin/tenants/nope/tokens", adminAuth(), nil), http.StatusNotFound,
		errBody("unknown tenant nope"))

	// Being deleted: creates and mints are 409, its tokens are unknown.
	if _, err := e.st.Q().Exec(context.Background(), "UPDATE tenants SET deleting_at = now() WHERE id = 'beta'"); err != nil {
		t.Fatal(err)
	}
	expect(t, e.do("GET", "/api/v1/sandboxes", bearerOf(created.Token), nil), http.StatusUnauthorized,
		errBody("unknown tenant token"))
	expect(t, e.do("POST", "/api/v1/admin/tenants", adminAuth(), map[string]any{"name": "beta"}), http.StatusConflict,
		errBody("tenant beta is being deleted"))
	expect(t, e.do("POST", "/api/v1/admin/tenants/beta/tokens", adminAuth(), nil), http.StatusConflict,
		errBody("tenant beta is being deleted"))
	// A sandbox create of a tenant whose deletion began after its token
	// resolved: refused under the tenant's row lock.
	if _, err := e.st.Q().Exec(context.Background(), "UPDATE tenants SET deleting_at = now() WHERE id = $1", testTenant); err != nil {
		t.Fatal(err)
	}
	req := map[string]any{}
	w = e.serve(request{method: "POST", path: "/api/v1/sandboxes", body: req,
		header: http.Header{"Authorization": {bearerOf(e.tenantToken)}}})
	expect(t, w, http.StatusUnauthorized, errBody("unknown tenant token"))
}

// A sandbox create that finds its tenant being deleted (it began after the
// token resolved) is a 409.
func TestCreateWhileTenantDeleting(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	var resp int
	err := e.st.InTx(context.Background(), func(tx *store.Store) error {
		_, err := tx.Q().Exec(context.Background(), "UPDATE tenants SET deleting_at = now() WHERE id = $1", testTenant)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Resolution ignores the deleting tenant; the create's own check is
	// reached through a token resolved before (simulated by the handler
	// with the tenant in the context).
	s := e.srv
	r := newTenantRequest(t, "POST", "/api/v1/sandboxes", testTenant)
	w := recorder()
	if err := s.createSandbox(w, r); err != nil {
		writeError(w, err)
	}
	resp = w.Code
	if resp != http.StatusConflict || w.Body.String() != errBody("tenant acme is being deleted") {
		t.Fatalf("%d %s", resp, w.Body.String())
	}
}

// A tenant's deletion on an instance that does not lead: its tokens go at
// once, its sandboxes in batches of 256, then the tenant.
func TestDeleteTenantInBatches(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{realEngine: true})
	ctx := context.Background()
	for range 300 {
		if _, _, err := e.st.CreateSandbox(ctx, store.NewSandbox{ID: podid.Mint(), Owner: testTenant, Image: "img",
			VCPUs: 1, MemoryMiB: 256}); err != nil {
			t.Fatal(err)
		}
	}
	w := e.do("DELETE", "/api/v1/admin/tenants/"+testTenant, adminAuth(), nil)
	expect(t, w, http.StatusOK, `{"deleted":true}`+"\n")
	if n, err := e.st.CountSandboxesOfOwner(ctx, testTenant); err != nil || n != 0 {
		t.Fatalf("left %d %v", n, err)
	}
	if _, err := e.st.GetTenant(ctx, testTenant); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("tenant: %v", err)
	}
	expect(t, e.do("GET", "/api/v1/sandboxes", bearerOf(e.tenantToken), nil), http.StatusUnauthorized,
		errBody("unknown tenant token"))
	w = e.do("DELETE", "/api/v1/admin/tenants/"+testTenant, adminAuth(), nil)
	expect(t, w, http.StatusOK, `{"deleted":false}`+"\n")
}

// The nodes view: the fields of a node, its telemetry, its bookings, its
// connections here and its certificate; vcpusCapacity by the gate's
// formula (0 = not gated).
func TestNodes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	for _, n := range []string{"n1", "n2"} {
		if _, err := e.st.InsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.st.UpdateNodeStatic(ctx, "n1", store.NodeStatic{Hostname: "h1", Arch: "x86_64", CPUModel: "cpu",
		CPUCores: 3, MemoryTotalBytes: 8 << 30, DiskTotalBytes: 100 << 30, Versions: json.RawMessage(`{"feosd":"1"}`),
		ObjectStore: true, ObjectStoreID: "os1", PodPrefix: "fd00::/64"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Q().Exec(ctx, `UPDATE nodes SET mesh_subnet = 7, mesh_prefix = 'fd5e:1b2c:93a0:7::/64',
		mesh_public_key = decode(repeat('ab', 32), 'hex'), mesh_endpoint = '10.0.0.11:51820' WHERE name = 'n1'`); err != nil {
		t.Fatal(err)
	}
	overcommit := 1.5
	if _, err := e.st.UpdateNodeSettings(ctx, "n1", store.NodeSettings{CPUOvercommit: &overcommit}); err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	if _, err := e.st.UpdateNodeSettings(ctx, "n2", store.NodeSettings{CPUOvercommit: &zero}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.UpsertTelemetry(ctx, "n1", store.TelemetryReport{Load1: 0.5, MemoryAvailableBytes: 4 << 30,
		DiskAvailableBytes: 50 << 30, PodsRunning: 2, PodsSuspended: 1, InventoryHash: "hash"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.MarkNodeReady(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
	sb, _, err := e.st.CreateSandbox(ctx, store.NewSandbox{ID: podid.Mint(), Owner: testTenant, Image: "img", VCPUs: 2, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpsertBooking(ctx, store.Booking{PodID: sb.ID, Node: "n1", VCPUs: 2, MemoryMiB: 512, Gated: true}); err != nil {
		t.Fatal(err)
	}
	notAfter := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)
	if _, err := e.st.InsertNodeCert(ctx, store.NodeCert{Serial: "abc123", Node: "n1", Kind: store.CertJoin, KeyHash: "k",
		NotBefore: time.Now().Add(-time.Minute), NotAfter: notAfter}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Q().Exec(ctx, "UPDATE nodes SET current_serial = 'abc123' WHERE name = 'n1'"); err != nil {
		t.Fatal(err)
	}
	e.conns.set("n1", true, 2)

	w := e.do("GET", "/api/v1/admin/nodes", adminAuth(), nil)
	expect(t, w, http.StatusOK, "")
	var out struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Nodes) != 2 {
		t.Fatalf("nodes %d", len(out.Nodes))
	}
	n1, n2 := out.Nodes[0], out.Nodes[1]
	want := map[string]any{
		"node": "n1", "status": "ready", "draining": false, "hostname": "h1", "arch": "x86_64", "cpuModelname": "cpu",
		"cpuNumcores": 3.0, "memoryTotalbytes": "8589934592", "diskTotalbytes": "107374182400", "slots": 50.0,
		"memHeadroomBytes": "1073741824", "vcpusCapacity": 4.0, "memoryCapacityBytes": "7516192768",
		"cpuOvercommit": 1.5, "memAvailBytes": "4294967296", "diskAvailBytes": "53687091200", "load1": 0.5,
		"sandboxesRunning": 2.0, "sandboxesPaused": 1.0, "inventoryHash": "hash", "downSinceNs": "0",
		"sandboxesBooked": 1.0, "vcpusBooked": 2.0, "memoryBookedBytes": "536870912", "objectStore": true,
		"objectStoreId": "os1", "podPrefix": "fd00::/64", "meshPrefix": "fd5e:1b2c:93a0:7::/64",
		"meshEndpoint": "10.0.0.11:51820", "connected": true, "connections": 2.0, "certSerial": "abc123",
		"certNotAfter": notAfter.Format(time.RFC3339), "identityForked": false, "removedAt": "", "stoppingUntil": "",
		"drainStartedAt": "", "instanceId": "", "startId": "",
	}
	for k, v := range want {
		if n1[k] != v {
			t.Errorf("n1 %s = %#v, want %#v", k, n1[k], v)
		}
	}
	if v, _ := n1["versions"].(map[string]any); v["feosd"] != "1" {
		t.Errorf("versions %#v", n1["versions"])
	}
	for _, k := range []string{"registeredAtNs", "lastSeenAtNs"} {
		if s, _ := n1[k].(string); len(s) < 18 {
			t.Errorf("%s = %#v", k, n1[k])
		}
	}
	if n2["vcpusCapacity"] != 0.0 || n2["connected"] != false || n2["connections"] != 0.0 || n2["lastSeenAtNs"] != "0" ||
		n2["status"] != "joined" || n2["meshPrefix"] != "" || n2["meshEndpoint"] != "" {
		t.Errorf("n2 %+v", n2)
	}
}

// The nodes view carries the instance's defaults, which the UI explains
// the fleet and fills the create form with: the configured timings (the
// telemetry interval config's when the Config names none), the default
// size and image, and the bounds of a create. Without nodes too.
func TestNodeDefaults(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	w := e.do("GET", "/api/v1/admin/nodes", adminAuth(), nil)
	expect(t, w, http.StatusOK, "")
	want := `{"nodes":[],"defaults":{"telemetryIntervalSeconds":5,"nodeTtlSeconds":20,"lostGraceSeconds":420,` +
		`"drainTimeoutSeconds":5400,"defaultVcpus":2,"defaultMemoryMib":512,"defaultImage":"ghcr.io/maltej/debian-ai:latest",` +
		`"minVcpus":1,"maxVcpus":32,"minMemoryMib":256,"maxMemoryMib":65536}}` + "\n"
	if w.Body.String() != want {
		t.Fatalf("answer\n%s\nwant\n%s", w.Body.String(), want)
	}
	e.srv.cfg.TelemetryInterval = 1500 * time.Millisecond
	if d := e.srv.nodeDefaults(); d.TelemetryIntervalSeconds != 1.5 {
		t.Fatalf("defaults %+v", d)
	}
}

// Drain and undrain under the node's lock; a down node is not drained;
// PATCH of the settings; removal with and without force; certificates.
func TestNodeChanges(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	if _, err := e.st.InsertNode(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
	w := e.do("POST", "/api/v1/admin/nodes/n1/drain", adminAuth(), nil)
	expect(t, w, http.StatusOK, "")
	n := decode[map[string]any](t, w)
	if n["draining"] != true || n["drainStartedAt"] == "" {
		t.Fatalf("drained %+v", n)
	}
	w = e.do("POST", "/api/v1/admin/nodes/n1/undrain", adminAuth(), nil)
	if n := decode[map[string]any](t, w); n["draining"] != false || n["drainStartedAt"] != "" {
		t.Fatalf("undrained %+v", n)
	}
	expect(t, e.do("POST", "/api/v1/admin/nodes/nope/drain", adminAuth(), nil), http.StatusNotFound, errBody("unknown node nope"))
	if _, err := e.st.Q().Exec(ctx, "UPDATE nodes SET status = 'down', down_since = now() WHERE name = 'n1'"); err != nil {
		t.Fatal(err)
	}
	expect(t, e.do("POST", "/api/v1/admin/nodes/n1/drain", adminAuth(), nil), http.StatusConflict, errBody("node n1 is down"))

	w = e.do("PATCH", "/api/v1/admin/nodes/n1", adminAuth(), map[string]any{"slots": 7, "memHeadroomBytes": "2048",
		"cpuOvercommit": 0})
	expect(t, w, http.StatusOK, "")
	if n := decode[map[string]any](t, w); n["slots"] != 7.0 || n["memHeadroomBytes"] != "2048" || n["cpuOvercommit"] != 0.0 {
		t.Fatalf("patched %+v", n)
	}
	w = e.do("PATCH", "/api/v1/admin/nodes/n1", adminAuth(), map[string]any{"memHeadroomBytes": 4096})
	if n := decode[map[string]any](t, w); n["slots"] != 7.0 || n["memHeadroomBytes"] != "4096" {
		t.Fatalf("patched %+v", n)
	}
	expect(t, e.do("PATCH", "/api/v1/admin/nodes/n1", adminAuth(), map[string]any{"slots": -1}), http.StatusBadRequest,
		errBody("slots, memHeadroomBytes and cpuOvercommit must not be negative"))
	expect(t, e.do("PATCH", "/api/v1/admin/nodes/nope", adminAuth(), map[string]any{"slots": 1}), http.StatusNotFound,
		errBody("unknown node nope"))

	// Certificates, newest first.
	for i, serial := range []string{"s1", "s2"} {
		if _, err := e.st.InsertNodeCert(ctx, store.NodeCert{Serial: serial, Node: "n1", Kind: store.CertRenew, KeyHash: "k",
			PreviousSerial: "s0", Peer: "10.0.0.1:1", NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		_ = i
		time.Sleep(5 * time.Millisecond)
	}
	w = e.do("GET", "/api/v1/admin/nodes/n1/certs", adminAuth(), nil)
	expect(t, w, http.StatusOK, "")
	certs := decode[struct {
		Certs []map[string]any `json:"certs"`
	}](t, w)
	if len(certs.Certs) != 2 || certs.Certs[0]["serial"] != "s2" || certs.Certs[1]["previousSerial"] != "s0" ||
		certs.Certs[0]["revokedAt"] != "" || certs.Certs[0]["peer"] != "10.0.0.1:1" {
		t.Fatalf("certs %+v", certs.Certs)
	}
	expect(t, e.do("GET", "/api/v1/admin/nodes/nope/certs", adminAuth(), nil), http.StatusNotFound, errBody("unknown node nope"))

	// Removal: refused while sandboxes are on it, unless forced.
	sb, _, err := e.st.CreateSandbox(ctx, store.NewSandbox{ID: podid.Mint(), Owner: testTenant, Image: "img", VCPUs: 1, MemoryMiB: 256})
	if err != nil {
		t.Fatal(err)
	}
	after := sb
	after.Node, after.ClientState = "n1", model.StateUnknown
	if err := e.st.InTx(ctx, func(tx *store.Store) error { _, err := tx.UpdateSandbox(ctx, sb, after); return err }); err != nil {
		t.Fatal(err)
	}
	expect(t, e.do("DELETE", "/api/v1/admin/nodes/n1", adminAuth(), nil), http.StatusConflict,
		errBody("node n1 has 1 sandboxes; move them or pass ?force=true"))
	w = e.do("DELETE", "/api/v1/admin/nodes/n1?force=true", adminAuth(), nil)
	expect(t, w, http.StatusOK, "")
	if n := decode[map[string]any](t, w); n["status"] != "removed" || n["removedAt"] == "" {
		t.Fatalf("removed %+v", n)
	}
	c, err := e.st.GetNodeCert(ctx, "s1")
	if err != nil || c.RevokedReason != store.RevokedRemoved {
		t.Fatalf("cert %+v %v", c, err)
	}
	expect(t, e.do("DELETE", "/api/v1/admin/nodes/nope", adminAuth(), nil), http.StatusNotFound, errBody("unknown node nope"))
	acts, err := e.st.ListNodeActivity(ctx, "n1", store.ActivityQuery{Limit: 10})
	if err != nil || len(acts) != 1 || acts[0].Kind != "node_removed" {
		t.Fatalf("activity %+v %v", acts, err)
	}
}

// An undrain clears the drain loop's progress text at once, and the loop's
// statement (SetNodeDescriptionIfDraining) does not write it back for an
// undrained node: neither a pass that runs after the undrain nor one that
// was blocked behind the undrain's row lock. Another writer's text stays.
func TestUndrainClearsDrainText(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	if _, err := e.st.InsertNode(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
	description := func() string {
		t.Helper()
		n, err := e.st.GetNode(ctx, "n1")
		if err != nil {
			t.Fatal(err)
		}
		return n.StatusDescription
	}
	for _, text := range []string{"draining: 3 remain", "drained", "cannot drain: the node has no object store"} {
		expect(t, e.do("POST", "/api/v1/admin/nodes/n1/drain", adminAuth(), nil), http.StatusOK, "")
		if err := e.st.SetNodeDescriptionIfDraining(ctx, "n1", text); err != nil {
			t.Fatal(err)
		}
		w := e.do("GET", "/api/v1/admin/nodes", adminAuth(), nil)
		if n := decode[struct {
			Nodes []map[string]any `json:"nodes"`
		}](t, w).Nodes[0]; n["statusDescription"] != text {
			t.Fatalf("draining node %+v", n)
		}
		w = e.do("POST", "/api/v1/admin/nodes/n1/undrain", adminAuth(), nil)
		expect(t, w, http.StatusOK, "")
		if n := decode[map[string]any](t, w); n["draining"] != false || n["statusDescription"] != "" {
			t.Fatalf("undrained %q: %+v", text, n)
		}
		if d := description(); d != "" {
			t.Fatalf("undrained %q: description %q", text, d)
		}
		// A late pass of the drain loop.
		if err := e.st.SetNodeDescriptionIfDraining(ctx, "n1", "draining: 2 remain"); err != nil {
			t.Fatal(err)
		}
		if d := description(); d != "" {
			t.Fatalf("a pass after the undrain wrote %q", d)
		}
	}

	// A pass of the drain loop that queues behind the undrain on the node's
	// row lock re-checks draining once the undrain commits, and writes
	// nothing. The test holds the row lock itself, lets the undrain
	// request and then the pass queue behind it (seen waiting in
	// pg_stat_activity, in that order), and releases it.
	expect(t, e.do("POST", "/api/v1/admin/nodes/n1/drain", adminAuth(), nil), http.StatusOK, "")
	if err := e.st.SetNodeDescriptionIfDraining(ctx, "n1", "draining: 5 remain"); err != nil {
		t.Fatal(err)
	}
	// waiting waits until n sessions of this test's database wait for a
	// lock with a statement that starts with prefix.
	waiting := func(prefix string, n int) error {
		deadline := time.Now().Add(10 * time.Second)
		for {
			var got int
			if err := e.st.Q().QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock'
				AND ltrim(query) LIKE $1 || '%'`, prefix).Scan(&got); err != nil {
				return err
			}
			if got >= n {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%d sessions wait with %q, want %d", got, prefix, n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	undrained := make(chan *httptest.ResponseRecorder, 1)
	passDone := make(chan error, 1)
	err := e.st.InTx(ctx, func(tx *store.Store) error {
		if _, err := tx.LockNode(ctx, "n1", store.ForUpdate); err != nil {
			return err
		}
		go func() { undrained <- e.do("POST", "/api/v1/admin/nodes/n1/undrain", adminAuth(), nil) }()
		if err := waiting("SELECT ", 1); err != nil {
			return fmt.Errorf("the undrain: %w", err)
		}
		go func() { passDone <- e.st.SetNodeDescriptionIfDraining(ctx, "n1", "draining: 4 remain") }()
		if err := waiting("UPDATE nodes SET status_description", 1); err != nil {
			return fmt.Errorf("the pass: %w", err)
		}
		select {
		case <-undrained:
			return errors.New("the undrain did not wait for the row lock")
		case err := <-passDone:
			return fmt.Errorf("the pass did not wait for the row lock: %v", err)
		default:
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w := <-undrained
	expect(t, w, http.StatusOK, "")
	if n := decode[map[string]any](t, w); n["draining"] != false || n["statusDescription"] != "" {
		t.Fatalf("undrained %+v", n)
	}
	if err := <-passDone; err != nil {
		t.Fatal(err)
	}
	if d := description(); d != "" {
		t.Fatalf("a pass queued behind the undrain wrote %q", d)
	}

	// Another writer's text is not the drain's: an undrain keeps it.
	expect(t, e.do("POST", "/api/v1/admin/nodes/n1/drain", adminAuth(), nil), http.StatusOK, "")
	if err := e.st.SetNodeDescription(ctx, "n1", "heartbeat expired"); err != nil {
		t.Fatal(err)
	}
	w = e.do("POST", "/api/v1/admin/nodes/n1/undrain", adminAuth(), nil)
	if n := decode[map[string]any](t, w); n["statusDescription"] != "heartbeat expired" {
		t.Fatalf("undrained %+v", n)
	}
}

func TestIsDrainDescription(t *testing.T) {
	t.Parallel()
	for d, want := range map[string]bool{
		"draining: 1 remain": true, "drained": true, "cannot drain: the node has no object store": true,
		"": false, "heartbeat expired": false, "node n1 down since 2026-01-01": false,
	} {
		if got := isDrainDescription(d); got != want {
			t.Errorf("%q: %v, want %v", d, got, want)
		}
	}
}

// Join tokens: made by fleet (shown once, pinned or not), listed without
// secrets, revoked by id.
func TestJoinTokens(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	base := t.TempDir()
	if err := pki.InitCA(pki.InitCAOptions{RootDir: filepath.Join(base, "root"), Dir: filepath.Join(base, "ca"),
		TrustDomain: "feos.test", DNSDomains: []string{"example.test"}}); err != nil {
		t.Fatal(err)
	}
	iss, err := pki.LoadIssuer(filepath.Join(base, "ca"), "feos.test")
	if err != nil {
		t.Fatal(err)
	}
	fl := fleet.New(fleet.Config{TrustDomain: "feos.test"}, fleet.Deps{DB: d, Issuer: iss})
	e := newEnv(t, envOptions{db: d, fleet: fl})
	w := e.do("POST", "/api/v1/admin/join-tokens", adminAuth(), map[string]any{"ttlSeconds": 600, "node": "n7"})
	expect(t, w, http.StatusCreated, "")
	if got := keysInOrder(t, w.Body.Bytes()); strings.Join(got, ",") != "id,token,expiresAt,kind,name,node" {
		t.Fatalf("keys %v", got)
	}
	jt := decode[map[string]string](t, w)
	if !strings.HasPrefix(jt["token"], "feos-join-") || !strings.HasSuffix(jt["token"], "."+iss.JoinPin()) || jt["node"] != "n7" ||
		jt["kind"] != "node" || jt["name"] != "n7" {
		t.Fatalf("join token %+v", jt)
	}
	exp, err := time.Parse(time.RFC3339, jt["expiresAt"])
	if err != nil || exp.Sub(time.Now()) > 11*time.Minute || exp.Sub(time.Now()) < 9*time.Minute {
		t.Fatalf("expiresAt %s %v", jt["expiresAt"], err)
	}
	w = e.do("POST", "/api/v1/admin/join-tokens", adminAuth(), nil)
	expect(t, w, http.StatusCreated, "")
	def := decode[map[string]string](t, w)
	exp, _ = time.Parse(time.RFC3339, def["expiresAt"])
	if exp.Sub(time.Now()) < 59*time.Minute || def["node"] != "" {
		t.Fatalf("default %+v", def)
	}
	expect(t, e.do("POST", "/api/v1/admin/join-tokens", adminAuth(), map[string]any{"node": "Bad_Name"}), http.StatusBadRequest, "")
	expect(t, e.do("POST", "/api/v1/admin/join-tokens", adminAuth(), map[string]any{"ttlSeconds": -1}), http.StatusBadRequest, "")

	w = e.do("GET", "/api/v1/admin/join-tokens", adminAuth(), nil)
	expect(t, w, http.StatusOK, "")
	if strings.Contains(w.Body.String(), "feos-join-") {
		t.Fatal("the list shows a secret")
	}
	list := decode[struct {
		JoinTokens []map[string]string `json:"joinTokens"`
	}](t, w)
	if len(list.JoinTokens) != 2 || list.JoinTokens[1]["node"] != "n7" || list.JoinTokens[1]["kind"] != "node" ||
		list.JoinTokens[1]["createdBy"] != testAdmin ||
		list.JoinTokens[1]["usedAt"] != "" {
		t.Fatalf("list %+v", list.JoinTokens)
	}
	expect(t, e.do("DELETE", "/api/v1/admin/join-tokens/"+jt["id"], adminAuth(), nil), http.StatusOK, `{"revoked":true}`+"\n")
	expect(t, e.do("DELETE", "/api/v1/admin/join-tokens/"+jt["id"], adminAuth(), nil), http.StatusOK, `{"revoked":false}`+"\n")
	expect(t, e.do("DELETE", "/api/v1/admin/join-tokens/nope", adminAuth(), nil), http.StatusOK, `{"revoked":false}`+"\n")
}

// The instances and who leads.
func TestInstances(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	ctx := context.Background()
	st := store.New(d)
	for _, id := range []string{"i1", "i2"} {
		if err := st.UpsertInstance(ctx, store.InstanceInfo{ID: id, Address: ":8100", NodeAddress: id + ":7443", Version: "v1"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetInstanceStopping(ctx, "i2"); err != nil {
		t.Fatal(err)
	}
	el := leader.NewElector(d, "i1", dbtest.Logger(t))
	e := newEnv(t, envOptions{db: d})
	e.srv.deps.Elector = el
	w := e.do("GET", "/api/v1/admin/instances", adminAuth(), nil)
	expect(t, w, http.StatusOK, "")
	out := decode[struct {
		Leader    *map[string]any  `json:"leader"`
		Instances []map[string]any `json:"instances"`
	}](t, w)
	if out.Leader != nil || len(out.Instances) != 2 || out.Instances[0]["live"] != true || out.Instances[1]["live"] != false ||
		out.Instances[1]["stopping"] == "" || out.Instances[0]["nodeAddress"] != "i1:7443" {
		t.Fatalf("instances %+v", out)
	}
	if _, ok, err := st.AcquireLease(ctx, "leader", "i1", 15*time.Second); err != nil || !ok {
		t.Fatalf("lease %v %v", ok, err)
	}
	w = e.do("GET", "/api/v1/admin/instances", adminAuth(), nil)
	out = decode[struct {
		Leader    *map[string]any  `json:"leader"`
		Instances []map[string]any `json:"instances"`
	}](t, w)
	if out.Leader == nil || (*out.Leader)["id"] != "i1" || (*out.Leader)["epoch"] != 1.0 || (*out.Leader)["since"] == "" {
		t.Fatalf("leader %+v", out.Leader)
	}
}

// An operator moves a tenant to the tenant zone and back (ADR 0031): the
// answer names each environment's label and host, the listing the
// tenant's zone; a zone the platform does not serve is a 400, an unknown
// tenant a 404, a deleting one a 409.
func TestSetTenantZone(t *testing.T) {
	t.Parallel()
	const tz = "apps.run.example.com"
	e := newEnv(t, envOptions{zones: model.Zones{Apps: "apps.example.com", Tenant: tz}})
	expect(t, e.do("POST", "/api/v1/admin/tenants", adminAuth(), map[string]any{"name": "beta"}), http.StatusCreated, "")
	ctx := t.Context()
	web, err := e.st.InsertEnvironment(ctx, store.NewEnvironment{ID: uuid.New(), Owner: "beta", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.SetEnvironmentPublic(ctx, web.ID, true); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		Tenant, Zone string
		Environments []struct{ ID, Name, HostLabel, HostLabelBefore, Host string }
	}
	w := e.do("PUT", "/api/v1/admin/tenants/beta/zone", adminAuth(), map[string]any{"zone": tz})
	expect(t, w, http.StatusOK, "")
	a := decode[answer](t, w)
	if a.Tenant != "beta" || a.Zone != tz || len(a.Environments) != 1 || a.Environments[0].ID != web.ID.String() ||
		a.Environments[0].HostLabel != "web-beta" || a.Environments[0].HostLabelBefore != "web-beta" ||
		a.Environments[0].Host != "web-beta."+tz {
		t.Fatalf("moved %+v", a)
	}
	list := decode[struct {
		Tenants []struct{ Name, Zone string }
	}](t, e.do("GET", "/api/v1/admin/tenants", adminAuth(), nil))
	zones := map[string]string{}
	for _, tn := range list.Tenants {
		zones[tn.Name] = tn.Zone
	}
	if zones["beta"] != tz || zones[testTenant] != "apps.example.com" {
		t.Fatalf("listing %+v", list)
	}
	if tn, err := e.st.GetTenant(ctx, "beta"); err != nil || tn.Zone != tz {
		t.Fatalf("row %+v %v", tn, err)
	}
	w = e.do("PUT", "/api/v1/admin/tenants/beta/zone", adminAuth(), map[string]any{"zone": "apps.example.com"})
	expect(t, w, http.StatusOK, "")
	if a := decode[answer](t, w); a.Zone != "apps.example.com" || a.Environments[0].Host != "web-beta.apps.example.com" {
		t.Fatalf("moved back %+v", a)
	}
	for _, bad := range []string{"", "other.example.com"} {
		expect(t, e.do("PUT", "/api/v1/admin/tenants/beta/zone", adminAuth(), map[string]any{"zone": bad}),
			http.StatusBadRequest, errBody(`zone must be one the platform serves, apps.example.com or `+tz+`; got "`+bad+`"`))
	}
	expect(t, e.do("PUT", "/api/v1/admin/tenants/nope/zone", adminAuth(), map[string]any{"zone": tz}),
		http.StatusNotFound, errBody("unknown tenant nope"))
	if _, err := e.st.MarkTenantDeleting(ctx, "beta"); err != nil {
		t.Fatal(err)
	}
	expect(t, e.do("PUT", "/api/v1/admin/tenants/beta/zone", adminAuth(), map[string]any{"zone": tz}),
		http.StatusConflict, errBody("tenant beta is being deleted"))
}
