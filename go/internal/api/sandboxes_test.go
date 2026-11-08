// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// statusFields are the 18 fields of a sandbox's status, in order.
var statusFields = []string{"sandboxId", "state", "image", "node", "owner", "createdAt", "updatedAt", "desiredState",
	"clientState", "statusDescription", "checkpointEpoch", "checkpointHead", "checkpointAt", "vcpus", "memoryMib",
	"environment", "reaches", "egress"}

// keysInOrder returns the keys of a JSON object in their order.
func keysInOrder(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", raw)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

var rfc3339Seconds = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`)

// A create inserts the row under an ID ferund mints (ADR 0019) and its
// first token in one go: 201 with the whole status, which carries the ID,
// and the token once. Every create makes a new sandbox.
func TestCreateSandbox(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	w := e.do("POST", "/api/v1/sandboxes", bearerOf(e.tenantToken),
		map[string]any{"environment": "box", "image": "img:1", "vcpus": 4, "memoryMib": 1024})
	expect(t, w, http.StatusCreated, "")
	var created struct {
		Status  json.RawMessage `json:"status"`
		Token   string          `json:"token"`
		Created bool            `json:"created"`
	}
	created = decode[struct {
		Status  json.RawMessage `json:"status"`
		Token   string          `json:"token"`
		Created bool            `json:"created"`
	}](t, w)
	if got := keysInOrder(t, w.Body.Bytes()); strings.Join(got, ",") != "status,token,created" {
		t.Fatalf("keys %v", got)
	}
	if got := keysInOrder(t, created.Status); strings.Join(got, ",") != strings.Join(statusFields, ",") {
		t.Fatalf("status keys %v", got)
	}
	var st map[string]any
	if err := json.Unmarshal(created.Status, &st); err != nil {
		t.Fatal(err)
	}
	id, _ := st["sandboxId"].(string)
	if pid, err := podid.Parse(id); err != nil || pid.IsLegacy() || pid.String() != id {
		t.Fatalf("sandboxId %q is not a short ID: %v", id, err)
	}
	want := map[string]any{"sandboxId": id, "state": "pending", "image": "img:1", "node": "", "owner": testTenant,
		"desiredState": "active", "clientState": "pending", "statusDescription": "awaiting first tool call",
		"checkpointEpoch": "0", "checkpointHead": "", "checkpointAt": "", "vcpus": 4.0, "memoryMib": 1024.0}
	for k, v := range want {
		if st[k] != v {
			t.Fatalf("%s = %#v, want %#v", k, st[k], v)
		}
	}
	for _, k := range []string{"createdAt", "updatedAt"} {
		if !rfc3339Seconds.MatchString(st[k].(string)) {
			t.Fatalf("%s = %q", k, st[k])
		}
	}
	if len(created.Token) != 43 || !created.Created {
		t.Fatalf("token %q created %v", created.Token, created.Created)
	}
	// The token works at once on a sandbox route.
	sb, err := ResolveSandboxToken(context.Background(), e.st.Q(), created.Token)
	if err != nil || sb.ID.String() != id || sb.Owner != testTenant {
		t.Fatalf("resolve: %+v %v", sb, err)
	}
	toks, err := e.st.ListSandboxTokens(context.Background(), sb.ID)
	if err != nil || len(toks) != 1 || toks[0].Label != "default" {
		t.Fatalf("tokens %+v %v", toks, err)
	}

	// Another create, with its environment only: a new sandbox with a new
	// ID and token; the defaults fill in. A sandboxId of null or "" names
	// none.
	seen := map[string]bool{id: true}
	for _, body := range []any{map[string]any{"environment": "box"}, map[string]any{"environment": "box", "sandboxId": nil},
		map[string]any{"environment": "box", "sandboxId": ""}} {
		w = e.do("POST", "/api/v1/sandboxes", bearerOf(e.tenantToken), body)
		expect(t, w, http.StatusCreated, "")
		minted := decode[struct {
			Status struct {
				SandboxID string `json:"sandboxId"`
				Image     string `json:"image"`
				Vcpus     int    `json:"vcpus"`
				MemoryMib int    `json:"memoryMib"`
			} `json:"status"`
			Token   string `json:"token"`
			Created bool   `json:"created"`
		}](t, w)
		pid, err := podid.Parse(minted.Status.SandboxID)
		if err != nil || pid.IsLegacy() || seen[minted.Status.SandboxID] || !minted.Created || minted.Token == "" ||
			minted.Status.Image != "ghcr.io/maltej/debian-ai:latest" || minted.Status.Vcpus != 2 || minted.Status.MemoryMib != 512 {
			t.Fatalf("minted %+v", minted)
		}
		seen[minted.Status.SandboxID] = true
	}
}

// clientIDMsg is the refusal of a create that names an ID.
const clientIDMsg = "sandboxId is not accepted: ferund mints a sandbox's ID; leave it out and keep status.sandboxId of the answer"

// The texts of a create's refusals.
func TestCreateSandboxRefusals(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	existing, _ := e.createSandbox()
	for _, c := range []struct {
		body   any
		status int
		msg    string
	}{
		// A client names no ID (ADR 0019): any sandboxId is refused, also
		// one that is valid, or one that exists.
		{map[string]any{"sandboxId": "sb-1"}, 400, clientIDMsg},
		{map[string]any{"sandboxId": "0199f0a2-7b1c-7d3e-8f00-1234567890ab"}, 400, clientIDMsg},
		{map[string]any{"sandboxId": "abcdefghjkm", "image": "img"}, 400, clientIDMsg},
		{map[string]any{"sandboxId": 7}, 400, clientIDMsg},
		{map[string]any{"sandboxId": existing}, 400, clientIDMsg},
		{map[string]any{"environment": "box", "vcpus": 33}, 400, "vcpus must be between 1 and 32 (0 = default 2)"},
		{map[string]any{"environment": "box", "vcpus": -1}, 400, "vcpus must be between 1 and 32 (0 = default 2)"},
		{map[string]any{"environment": "box", "memoryMib": 128}, 400, "memoryMib must be between 256 and 65536 (0 = default 512)"},
		{map[string]any{"environment": "box", "memoryMib": 70000}, 400, "memoryMib must be between 256 and 65536 (0 = default 512)"},
		{`[1]`, 400, "request body must be a JSON object: json: cannot unmarshal array into Go value of type map[string]json.RawMessage"},
		{`{"vcpus":"two"}`, 400, "invalid request body: json: cannot unmarshal string into Go struct field createSandboxRequest.vcpus of type int"},
	} {
		w := e.do("POST", "/api/v1/sandboxes", bearerOf(e.tenantToken), c.body)
		expect(t, w, c.status, errBody(c.msg))
	}
}

// The tenant routes see only the tenant's sandboxes: a foreign one is an
// unknown one; a sandbox's token is no tenant's, and a tenant's token no
// sandbox's.
func TestTenantScoping(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	id, tok := e.createSandbox()
	other := e.mintTenantToken(mustTenant(t, e, "other"))
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/sandboxes/" + id},
		{"DELETE", "/api/v1/sandboxes/" + id},
		{"POST", "/api/v1/sandboxes/" + id + "/pause"},
		{"POST", "/api/v1/sandboxes/" + id + "/resume"},
		{"GET", "/api/v1/sandboxes/" + id + "/tokens"},
		{"POST", "/api/v1/sandboxes/" + id + "/tokens"},
		{"DELETE", "/api/v1/sandboxes/" + id + "/tokens/abc"},
		{"GET", "/api/v1/sandboxes/" + id + "/access"},
	} {
		w := e.do(c.method, c.path, bearerOf(other), nil)
		expect(t, w, http.StatusNotFound, errBody("unknown sandbox "+id))
		w = e.do(c.method, c.path, bearerOf(tok), nil)
		expect(t, w, http.StatusUnauthorized, errBody("unknown tenant token"))
	}
	w := e.do("GET", "/api/v1/sandboxes/sb-1", bearerOf(e.tenantToken), nil)
	expect(t, w, http.StatusBadRequest, errBody(apierr.MsgSandboxID))
	w = e.do("GET", "/api/v1/sandboxes/"+podid.Mint().String(), "Basic eDp5", nil)
	expect(t, w, http.StatusUnauthorized, errBody("tenant bearer required"))

	// The list holds only the tenant's.
	w = e.do("GET", "/api/v1/sandboxes", bearerOf(other), nil)
	expect(t, w, http.StatusOK, `{"sandboxes":[]}`+"\n")
	w = e.do("GET", "/api/v1/sandboxes", bearerOf(e.tenantToken), nil)
	list := decode[struct {
		Sandboxes []map[string]any `json:"sandboxes"`
	}](t, w)
	if len(list.Sandboxes) != 1 || list.Sandboxes[0]["sandboxId"] != id {
		t.Fatalf("list %+v", list)
	}

	// Sandbox routes: the token must be the path's sandbox's.
	id2, tok2 := e.createSandbox()
	for _, c := range []struct {
		authz  string
		status int
		msg    string
	}{
		{"", 401, "sandbox token required"},
		{basicOf("a", "b"), 401, "sandbox token required"},
		{bearerOf(e.tenantToken), 404, "no such sandbox"},
		{bearerOf(tok2), 404, "no such sandbox"},
		{bearerOf("nope"), 404, "no such sandbox"},
	} {
		w := e.do("POST", "/api/v1/sandboxes/"+id+"/exec", c.authz, map[string]any{"command": "true"})
		expect(t, w, c.status, errBody(c.msg))
	}
	_ = id2
}

// Sandbox tokens: listed by hash, minted shown once, revoked by hash; a
// hash of another sandbox's token is not revoked through this one's path.
func TestSandboxTokens(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	id, first := e.createSandbox()
	id2, other := e.createSandbox()
	w := e.do("POST", "/api/v1/sandboxes/"+id+"/tokens", bearerOf(e.tenantToken), map[string]any{"label": "fecode-backend"})
	expect(t, w, http.StatusCreated, "")
	if got := keysInOrder(t, w.Body.Bytes()); strings.Join(got, ",") != "token,hashPrefix,createdAt" {
		t.Fatalf("keys %v", got)
	}
	minted := decode[mintedToken](t, w)
	if len(minted.Token) != 43 || minted.HashPrefix != store.HashToken(minted.Token)[:8] ||
		!rfc3339Seconds.MatchString(minted.CreatedAt) {
		t.Fatalf("minted %+v", minted)
	}
	w = e.do("GET", "/api/v1/sandboxes/"+id+"/tokens", bearerOf(e.tenantToken), nil)
	expect(t, w, http.StatusOK, "")
	list := decode[struct {
		Tokens []map[string]any `json:"tokens"`
	}](t, w)
	if len(list.Tokens) != 2 || list.Tokens[0]["label"] != "default" || list.Tokens[1]["label"] != "fecode-backend" {
		t.Fatalf("tokens %+v", list.Tokens)
	}
	if list.Tokens[0]["hash"] != store.HashToken(first) || list.Tokens[0]["sandboxId"] != id ||
		list.Tokens[0]["hashPrefix"] != store.HashToken(first)[:8] {
		t.Fatalf("token %+v", list.Tokens[0])
	}
	if _, ok := list.Tokens[0]["lastUsedAt"]; ok {
		t.Fatal("lastUsedAt of an unused token")
	}
	// Used once: lastUsedAt appears.
	e.do("GET", "/api/v1/sandboxes/"+id+"/processes", bearerOf(first), nil)
	w = e.do("GET", "/api/v1/sandboxes/"+id+"/tokens", bearerOf(e.tenantToken), nil)
	list = decode[struct {
		Tokens []map[string]any `json:"tokens"`
	}](t, w)
	if s, _ := list.Tokens[0]["lastUsedAt"].(string); !rfc3339Seconds.MatchString(s) {
		t.Fatalf("lastUsedAt %q", s)
	}

	w = e.do("DELETE", "/api/v1/sandboxes/"+id+"/tokens/"+store.HashToken(other), bearerOf(e.tenantToken), nil)
	expect(t, w, http.StatusNotFound, errBody("unknown token"))
	w = e.do("DELETE", "/api/v1/sandboxes/"+id+"/tokens/"+store.HashToken(first), bearerOf(e.tenantToken), nil)
	expect(t, w, http.StatusOK, `{"revoked":true}`+"\n")
	w = e.do("DELETE", "/api/v1/sandboxes/"+id+"/tokens/"+store.HashToken(first), bearerOf(e.tenantToken), nil)
	expect(t, w, http.StatusOK, `{"revoked":false}`+"\n")
	// The revoked token no longer reaches the sandbox; the other still does.
	w = e.do("GET", "/api/v1/sandboxes/"+id+"/processes", bearerOf(first), nil)
	expect(t, w, http.StatusNotFound, errBody("no such sandbox"))
	if _, err := ResolveSandboxToken(context.Background(), e.st.Q(), other); err != nil {
		t.Fatal(err)
	}
	w = e.do("POST", "/api/v1/sandboxes/"+podid.Mint().String()+"/tokens", bearerOf(e.tenantToken), nil)
	expect(t, w, http.StatusNotFound, "")
	_ = id2
}

// The status: updatedAt is the later of updated_at and last_active_at,
// int64s are strings, the node or "", the checkpoint fields.
func TestStatusRendering(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 1, 2, 3, 4, 5, 600, time.UTC)
	row := store.Sandbox{
		ID: podid.ID("0199f0a2-7b1c-7d3e-8f00-1234567890ab"), Owner: "acme", Image: "img", ClientState: "hibernated",
		StatusDescription: "x", Node: "n1", CheckpointEpoch: 1 << 40, CheckpointHead: "h", VCPUs: 3, MemoryMiB: 768,
		CreatedAt: created, UpdatedAt: created.Add(time.Minute), LastActiveAt: created.Add(time.Hour),
		CheckpointAt: created.Add(2 * time.Hour),
	}
	b, err := json.Marshal(statusOf(row))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"sandboxId":"0199f0a2-7b1c-7d3e-8f00-1234567890ab","state":"hibernated","image":"img","node":"n1",` +
		`"owner":"acme","createdAt":"2026-01-02T03:04:05Z","updatedAt":"2026-01-02T04:04:05Z","desiredState":"active",` +
		`"clientState":"hibernated","statusDescription":"x","checkpointEpoch":"1099511627776","checkpointHead":"h",` +
		`"checkpointAt":"2026-01-02T05:04:05Z","vcpus":3,"memoryMib":768}`
	if string(b) != want {
		t.Fatalf("status\n%s\nwant\n%s", b, want)
	}
	row.LastActiveAt = created
	if st := statusOf(row); st.UpdatedAt != "2026-01-02T03:05:05Z" {
		t.Fatalf("updatedAt %s", st.UpdatedAt)
	}
}

// Pause and resume answer the status as read before; a recheck that found
// the row changed is a 200; every other error a 500 "<op>: <err>", also a
// stopping instance's and a hibernating sandbox's.
func TestPauseResume(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	id, _ := e.createSandbox()
	w := e.do("POST", "/api/v1/sandboxes/"+id+"/pause", bearerOf(e.tenantToken), nil)
	expect(t, w, http.StatusOK, "")
	if st := decode[sandboxStatus](t, w); st.ClientState != "pending" || st.SandboxID != id {
		t.Fatalf("pause answer %+v", st)
	}
	stopping := fmt.Errorf("%w", model.ErrStopping)
	for _, c := range []struct {
		err    error
		status int
		body   string
	}{
		{fmt.Errorf("x: %w", model.ErrPrecondition), 200, ""},
		{errors.New("sandbox is creating"), 500, errBody("pause: sandbox is creating")},
		// §11: a sandbox on its way to sleep is not paused; the old pause
		// had no state check either, so there is no 200 to keep.
		{errors.New("sandbox is hibernating"), 500, errBody("pause: sandbox is hibernating")},
		{&stoppingErr{"instance i1 is stopping"}, 500, errBody("pause: instance i1 is stopping")},
		{model.ErrNotFound, 404, errBody("unknown sandbox " + id)},
	} {
		e.fake.mu.Lock()
		e.fake.pauseErr = c.err
		e.fake.mu.Unlock()
		w := e.do("POST", "/api/v1/sandboxes/"+id+"/pause", bearerOf(e.tenantToken), nil)
		expect(t, w, c.status, c.body)
	}
	for _, c := range []struct {
		err    error
		status int
		body   string
	}{
		{nil, 200, ""},
		{&stoppingErr{"instance i1 is stopping"}, 500, errBody("resume: instance i1 is stopping")},
		{fmt.Errorf("no capacity on node n1 for sandbox %s (retryable): slots: 50 of 50 used: %w", id, model.ErrNoCapacity),
			500, errBody("resume: no capacity on node n1 for sandbox " + id + " (retryable): slots: 50 of 50 used: no capacity")},
		{&busyErr{"sandbox " + id + " is hibernating; retry in a minute"}, 500,
			errBody("resume: sandbox " + id + " is hibernating; retry in a minute")},
		{model.ErrNotFound, 404, errBody("unknown sandbox " + id)},
	} {
		e.fake.mu.Lock()
		e.fake.resumeErr = c.err
		e.fake.mu.Unlock()
		w := e.do("POST", "/api/v1/sandboxes/"+id+"/resume", bearerOf(e.tenantToken), nil)
		expect(t, w, c.status, c.body)
	}
	_ = stopping
}

// stoppingErr is the engine's ErrStopping with its own text.
type stoppingErr struct{ msg string }

func (e *stoppingErr) Error() string { return e.msg }
func (e *stoppingErr) Unwrap() error { return model.ErrStopping }

// busyErr is the engine's ErrBusy with its own text: a hibernation that
// Ensure does not wait for.
type busyErr struct{ msg string }

func (e *busyErr) Error() string { return e.msg }
func (e *busyErr) Unwrap() error { return model.ErrBusy }

// The tool routes never map an error to another status: a failed call is
// a 502 "<tool>: <err>", with Retry-After for a stopping instance, a
// hibernating sandbox and an exhausted budget.
func TestToolRouteErrors(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		acquireErr error
		connErr    error
		body       string
		retryAfter string
	}{
		{"stopping", &stoppingErr{"instance i1 is stopping"}, nil,
			errBody("bash: ensure sandbox: instance i1 is stopping"), "5"},
		{"lost", model.ErrLost, nil, errBody("bash: ensure sandbox: sandbox lost"), ""},
		{"hibernating", &busyErr{"sandbox 5f0c6a3e-0000-4000-8000-000000000001 is hibernating; retry in a minute"}, nil,
			errBody("bash: ensure sandbox: sandbox 5f0c6a3e-0000-4000-8000-000000000001 is hibernating; retry in a minute"), "5"},
		{"budget", nil, status.Error(codes.ResourceExhausted, "node n1: stream budget exhausted"),
			errBody("bash: node n1: stream budget exhausted"), "5"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, envOptions{sandboxes: &fakeSandboxes{err: c.acquireErr, lease: model.Lease{Node: "n1"}}})
			e.conns.connErr = c.connErr
			id, tok := e.createSandbox()
			w := e.do("POST", "/api/v1/sandboxes/"+id+"/exec", bearerOf(tok), map[string]any{"command": "true"})
			expect(t, w, http.StatusBadGateway, c.body)
			if got := w.Header().Get("Retry-After"); got != c.retryAfter {
				t.Fatalf("Retry-After %q, want %q", got, c.retryAfter)
			}
		})
	}
}

// GET /api/v1/tools: the baseline in order with its schemas, and the
// preparation budget.
func TestListTools(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	w := e.do("GET", "/api/v1/tools", bearerOf(e.tenantToken), nil)
	expect(t, w, http.StatusOK, "")
	out := decode[struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
		PreparationWaitMs int64 `json:"preparationWaitMs"`
	}](t, w)
	if out.PreparationWaitMs != 425000 || len(out.Tools) != 12 || out.Tools[0].Name != "bash" {
		t.Fatalf("tools %d, first %q, wait %d", len(out.Tools), out.Tools[0].Name, out.PreparationWaitMs)
	}
	for _, tl := range out.Tools {
		var schema map[string]any
		if err := json.Unmarshal(tl.InputSchema, &schema); err != nil || tl.Description == "" {
			t.Fatalf("tool %s: schema %s", tl.Name, tl.InputSchema)
		}
	}
}

// Delete removes the row at once: the answer is the status read with
// desiredState deleted; a create right after is a new sandbox under a new
// ID with a token that resolves.
func TestDeleteRecreate(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{realEngine: true})
	id, tok := e.createSandbox()
	w := e.do("DELETE", "/api/v1/sandboxes/"+id, bearerOf(e.tenantToken), nil)
	expect(t, w, http.StatusOK, "")
	st := decode[sandboxStatus](t, w)
	if st.DesiredState != "deleted" || st.ClientState != "pending" || st.State != "pending" || st.SandboxID != id {
		t.Fatalf("delete answer %+v", st)
	}
	w = e.do("GET", "/api/v1/sandboxes/"+id, bearerOf(e.tenantToken), nil)
	expect(t, w, http.StatusNotFound, errBody("unknown sandbox "+id))
	w = e.do("DELETE", "/api/v1/sandboxes/"+id, bearerOf(e.tenantToken), nil)
	expect(t, w, http.StatusNotFound, errBody("unknown sandbox "+id))
	if _, err := ResolveSandboxToken(context.Background(), e.st.Q(), tok); !errors.Is(err, model.ErrUnknownToken) {
		t.Fatalf("old token: %v", err)
	}

	again, tok2 := e.createSandbox()
	if again == id || tok2 == tok {
		t.Fatalf("re-create %s %s", again, tok2)
	}
	if _, err := ResolveSandboxToken(context.Background(), e.st.Q(), tok2); err != nil {
		t.Fatal(err)
	}

	// A sandbox with a pod somewhere: its delete leaves a tombstone, and the
	// answer says absent.
	if _, err := e.st.InsertNode(context.Background(), "n1"); err != nil {
		t.Fatal(err)
	}
	row, err := e.st.GetSandbox(context.Background(), podid.ID(again))
	if err != nil {
		t.Fatal(err)
	}
	after := row
	after.Node, after.ClientState, after.Materialised = "n1", model.StatePaused, true
	if err := e.st.InTx(context.Background(), func(tx *store.Store) error {
		_, err := tx.UpdateSandbox(context.Background(), row, after)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w = e.do("DELETE", "/api/v1/sandboxes/"+again, bearerOf(e.tenantToken), nil)
	expect(t, w, http.StatusOK, "")
	st = decode[sandboxStatus](t, w)
	if st.ClientState != "absent" || st.State != "absent" || st.Node != "n1" || st.DesiredState != "deleted" {
		t.Fatalf("delete answer %+v", st)
	}
}

// A sandbox made before ADR 0019 keeps its UUID until it is deleted: the
// routes serve it beside sandboxes with short IDs, by its ID in either
// case, and its token opens its sandbox routes.
func TestLegacySandboxID(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	legacy := podid.ID("0199f0a2-7b1c-7d3e-8f00-1234567890ab")
	if _, created, err := e.st.CreateSandbox(ctx, store.NewSandbox{ID: legacy, Owner: testTenant, Image: "img",
		VCPUs: 1, MemoryMiB: 512, EnvironmentID: e.box}); err != nil || !created {
		t.Fatalf("legacy row: %v %v", created, err)
	}
	tok, hash, err := mintSandboxToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.CreateSandboxToken(ctx, hash, legacy, firstTokenLabel, ""); err != nil {
		t.Fatal(err)
	}
	short, _ := e.createSandbox()

	for _, path := range []string{legacy.String(), strings.ToUpper(legacy.String())} {
		w := e.do("GET", "/api/v1/sandboxes/"+path, bearerOf(e.tenantToken), nil)
		expect(t, w, http.StatusOK, "")
		if st := decode[sandboxStatus](t, w); st.SandboxID != legacy.String() {
			t.Fatalf("GET %s: %+v", path, st)
		}
	}
	w := e.do("GET", "/api/v1/sandboxes", bearerOf(e.tenantToken), nil)
	list := decode[struct {
		Sandboxes []map[string]any `json:"sandboxes"`
	}](t, w)
	if len(list.Sandboxes) != 2 || list.Sandboxes[0]["sandboxId"] != legacy.String() || list.Sandboxes[1]["sandboxId"] != short {
		t.Fatalf("list %+v", list)
	}
	w = e.do("GET", "/api/v1/sandboxes/"+legacy.String()+"/tokens", bearerOf(e.tenantToken), nil)
	expect(t, w, http.StatusOK, "")

	// Its token is its sandbox's, whatever the case of the path's ID; the
	// exec gets past the class to the engine, which has no sandbox here.
	sb, err := ResolveSandboxToken(ctx, e.st.Q(), tok)
	if err != nil || sb.ID != legacy {
		t.Fatalf("resolve: %+v %v", sb, err)
	}
	for _, path := range []string{legacy.String(), strings.ToUpper(legacy.String())} {
		w = e.do("POST", "/api/v1/sandboxes/"+path+"/exec", bearerOf(tok), map[string]any{"command": "true"})
		if w.Code == http.StatusNotFound || w.Code == http.StatusBadRequest {
			t.Fatalf("exec on %s: %d %s", path, w.Code, w.Body)
		}
	}
	w = e.do("POST", "/api/v1/sandboxes/"+short+"/exec", bearerOf(tok), map[string]any{"command": "true"})
	expect(t, w, http.StatusNotFound, errBody("no such sandbox"))

	w = e.do("DELETE", "/api/v1/sandboxes/"+legacy.String(), bearerOf(e.tenantToken), nil)
	expect(t, w, http.StatusOK, "")
	w = e.do("GET", "/api/v1/sandboxes/"+legacy.String(), bearerOf(e.tenantToken), nil)
	expect(t, w, http.StatusNotFound, errBody("unknown sandbox "+legacy.String()))
}

// A path's sandbox ID is a short ID or a UUID; anything else is a 400.
func TestSandboxIDFormats(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	for _, bad := range []string{"sb-1", "ABCDEFGHJKM", "0bcdefghjkm", "abcdefghjkl", "0199f0a27b1c7d3e8f001234567890ab"} {
		w := e.do("GET", "/api/v1/sandboxes/"+bad, bearerOf(e.tenantToken), nil)
		expect(t, w, http.StatusBadRequest, errBody(apierr.MsgSandboxID))
	}
	for _, unknown := range []string{"abcdefghjkm", "0199f0a2-7b1c-7d3e-8f00-1234567890ab"} {
		w := e.do("GET", "/api/v1/sandboxes/"+unknown, bearerOf(e.tenantToken), nil)
		expect(t, w, http.StatusNotFound, errBody("unknown sandbox "+unknown))
	}
}
