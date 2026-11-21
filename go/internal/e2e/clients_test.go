// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Scenario 2: a sandbox created over REST runs commands over REST and MCP
// with byte-exact output, whichever instance each request reaches; FeCode's
// go-sdk client with nil options (the server/discover path) and an
// initialize-only client both work through a balancer that sends every
// request to another instance.
func TestRESTAndMCP(t *testing.T) {
	t.Parallel()
	c := newCluster(t, nil)
	a := c.start("a")
	b := c.start("b")
	c.join("n1")

	sb := c.create(a)
	// FeCode's wake-up call, then commands, each on another instance.
	for _, tc := range []struct {
		in      *instance
		command string
		body    string
	}{
		{b, "true", `{"exitCode":0,"output":""}`},
		{a, "echo hello", `{"exitCode":0,"output":"hello\n"}`},
		{b, "printf 'x\\ny'; exit 3", `{"exitCode":3,"output":"x\ny"}`},
		{a, "echo oops >&2", `{"exitCode":0,"output":"oops\n"}`},
	} {
		r := c.exec(tc.in, sb, tc.command)
		if r.code != http.StatusOK || r.body != tc.body+"\n" {
			t.Errorf("exec %q on %s: %d %s, want %s", tc.command, tc.in.name, r.code, r.body, tc.body)
		}
	}
	if row := c.row(sb.id); row.ClientState != model.StateRunning || row.Node != "n1" || row.ImageRef != testImage {
		t.Fatalf("row %+v", row)
	}

	for _, opts := range []*mcp.ClientSessionOptions{nil, legacy} {
		cs := c.mcpSession(lbURL+"/mcp", sb.token, opts)
		if init := cs.InitializeResult(); init.ServerInfo == nil || init.ServerInfo.Name != "ferun" || init.ServerInfo.Version != "e2e" {
			t.Errorf("server info %+v", init.ServerInfo)
		}
		for _, tc := range []struct {
			command string
			text    string
			isError bool
		}{
			{"echo hi", "hi\n", false},
			{"echo oops >&2; exit 3", "oops\n\n(exit code 3)", true},
			{"exit 3", "(no output)\n(exit code 3)", true},
			{"printf 'a\\nb'", "a\nb", false},
		} {
			text, isError, err := callTool(cs, "bash", map[string]any{"command": tc.command})
			if err != nil || text != tc.text || isError != tc.isError {
				t.Errorf("bash %q (opts %v): %q %v %v, want %q %v", tc.command, opts, text, isError, err, tc.text, tc.isError)
			}
		}
		if text, isError, err := callTool(cs, "write", map[string]any{"path": "/w/a.txt", "content": "one\ntwo\n"}); err != nil || isError {
			t.Errorf("write: %q %v %v", text, isError, err)
		}
		if text, isError, err := callTool(cs, "bash", map[string]any{"command": "cat w/a.txt"}); err != nil || isError || text != "one\ntwo\n" {
			t.Errorf("read back: %q %v %v", text, isError, err)
		}
		list, err := cs.ListTools(bg, nil)
		if err != nil || len(list.Tools) == 0 {
			t.Errorf("tools/list: %v", err)
		}
	}
	// REST lists the same tools.
	r := c.call(nil, "GET", "/api/v1/tools", projectAuth, "")
	var tools struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
		PreparationWaitMs int64 `json:"preparationWaitMs"`
	}
	if err := json.Unmarshal([]byte(r.body), &tools); err != nil || r.code != 200 || len(tools.Tools) == 0 ||
		tools.PreparationWaitMs != config.PreparationBudget.Milliseconds() {
		t.Errorf("tools: %d %s", r.code, r.body)
	}
}

// Scenario 2: a token revoked in the middle of a session fails the next
// call with "session not found", and a re-minted token opens a working
// session again; a database blip during a call fails that call and not the
// session; /mcp works behind a loopback reverse proxy that sends an
// external Host.
func TestMCPSessions(t *testing.T) {
	t.Parallel()
	c := newCluster(t, nil)
	a := c.start("a")
	c.start("b")
	c.join("n1")
	sb := c.create(nil)
	c.run(nil, sb, "true")

	mint := func() string {
		t.Helper()
		r := c.call(nil, "POST", sb.path("/tokens"), projectAuth, `{"label":"fecode-backend"}`)
		var out struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal([]byte(r.body), &out); err != nil || r.code != http.StatusCreated || out.Token == "" {
			t.Fatalf("mint: %d %s", r.code, r.body)
		}
		return out.Token
	}
	for _, opts := range []*mcp.ClientSessionOptions{nil, legacy} {
		token := mint()
		cs := c.mcpSession(lbURL+"/mcp", token, opts)
		if text, _, err := callTool(cs, "bash", map[string]any{"command": "echo one"}); err != nil || text != "one\n" {
			t.Fatalf("before the revocation: %q %v", text, err)
		}
		r := c.call(nil, "DELETE", sb.path("/tokens/"+store.HashToken(token)), projectAuth, "")
		if r.code != http.StatusOK || r.body != `{"revoked":true}`+"\n" {
			t.Fatalf("revoke: %d %s", r.code, r.body)
		}
		_, _, err := callTool(cs, "bash", map[string]any{"command": "echo two"})
		if err == nil || !strings.Contains(err.Error(), "session not found: unknown sandbox token") {
			t.Fatalf("a call with the revoked token: %v", err)
		}
		if _, err := c.mcpConnect(lbURL+"/mcp", token, opts); err == nil {
			t.Fatal("a session opened with the revoked token")
		}
		cs = c.mcpSession(lbURL+"/mcp", mint(), opts)
		if text, _, err := callTool(cs, "bash", map[string]any{"command": "echo three"}); err != nil || text != "three\n" {
			t.Fatalf("after the re-mint: %q %v", text, err)
		}

		// The blip: the token table is locked for longer than the
		// statement's lock timeout.
		tx, err := c.db.Pool.Begin(bg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(bg, "LOCK TABLE sandbox_tokens IN ACCESS EXCLUSIVE MODE"); err != nil {
			t.Fatal(err)
		}
		_, _, err = callTool(cs, "bash", map[string]any{"command": "echo blip"})
		_ = tx.Rollback(bg)
		// The answer is the 503 of a failed token lookup, which the
		// client keeps the session through; FeCode drops a session on
		// an error that says 404 or "session not found".
		if err == nil || !strings.Contains(err.Error(), "Service Unavailable") ||
			strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "session not found") {
			t.Fatalf("the call in the blip: %v; want the 503 of a failed token lookup", err)
		}
		if text, _, err := callTool(cs, "bash", map[string]any{"command": "echo after"}); err != nil || text != "after\n" {
			t.Fatalf("after the blip: %q %v", text, err)
		}
	}

	// A local proxy in front of one instance that sends the external
	// Host, as on a single machine.
	target, _ := url.Parse("http://" + a.HTTPAddr())
	proxy := httptest.NewServer(&httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.Host = "run.fecode.dev"
	}, FlushInterval: -1})
	t.Cleanup(proxy.Close)
	for _, opts := range []*mcp.ClientSessionOptions{nil, legacy} {
		cs := c.mcpSession(proxy.URL+"/mcp", sb.token, opts)
		if text, _, err := callTool(cs, "bash", map[string]any{"command": "echo proxied"}); err != nil || text != "proxied\n" {
			t.Errorf("through the proxy: %q %v", text, err)
		}
	}
}

// Scenario 11: with the bindings off, their routes and preflights are the
// API's 404 "no such route" and the access route is the empty object; a
// tool call into a lost sandbox is "error: ensure sandbox: sandbox lost"
// over MCP and a 502 "bash: ensure sandbox: sandbox lost" over REST.
func TestBindingsOffAndLostSandbox(t *testing.T) {
	t.Parallel()
	c := newCluster(t, nil)
	c.start("a")
	c.start("b")
	n1 := c.join("n1")
	sb := c.create(nil)
	c.run(nil, sb, "true")

	for _, tc := range []struct{ method, path, auth string }{
		{"PUT", sb.path("/kube"), sb.auth()},
		{"GET", sb.path("/kube"), sb.auth()},
		{"DELETE", sb.path("/kube"), sb.auth()},
		{"PUT", sb.path("/ssh"), sb.auth()},
		{"GET", sb.path("/ssh"), sb.auth()},
		{"DELETE", sb.path("/ssh"), sb.auth()},
		{"POST", sb.path("/ssh/prod/check"), sb.auth()},
		{"POST", "/api/v1/kube/preflight", projectAuth},
		{"POST", "/api/v1/ssh/preflight", projectAuth},
	} {
		body := ""
		if tc.method != "GET" && tc.method != "DELETE" {
			body = `{"bindings":[]}`
		}
		r := c.call(nil, tc.method, tc.path, tc.auth, body)
		want := `{"error":"no such route: ` + tc.method + ` ` + tc.path + `"}` + "\n"
		if r.code != http.StatusNotFound || r.body != want {
			t.Errorf("%s %s: %d %s, want 404 %s", tc.method, tc.path, r.code, r.body, want)
		}
	}
	if r := c.call(nil, "GET", sb.path("/access"), projectAuth, ""); r.code != http.StatusOK ||
		r.body != `{"kube":null,"ssh":null,"denies":[]}`+"\n" {
		t.Errorf("access: %d %s", r.code, r.body)
	}

	// The sandbox's node goes for good: its sandbox is lost.
	if r := c.call(nil, "DELETE", "/api/v1/admin/nodes/n1?force=true", adminAuth, ""); r.code != http.StatusOK {
		t.Fatalf("remove: %d %s", r.code, r.body)
	}
	_ = n1.ended(t, 60*time.Second)
	eventually(t, 60*time.Second, "the sandbox lost", func() bool {
		c.wake("sandbox-down-sweep", "sandbox-lost-grace")
		return c.row(sb.id).ClientState == model.StateLost
	})
	for _, opts := range []*mcp.ClientSessionOptions{nil, legacy} {
		cs := c.mcpSession(lbURL+"/mcp", sb.token, opts)
		text, isError, err := callTool(cs, "bash", map[string]any{"command": "true"})
		if err != nil || !isError || text != "error: ensure sandbox: sandbox lost" {
			t.Errorf("MCP into the lost sandbox: %q %v %v", text, isError, err)
		}
	}
	r := c.exec(nil, sb, "true")
	if r.code != http.StatusBadGateway || r.body != `{"error":"bash: ensure sandbox: sandbox lost"}`+"\n" {
		t.Errorf("REST into the lost sandbox: %d %s", r.code, r.body)
	}
	// FeCode then reads the state, deletes the lost sandbox and creates a
	// new one at once; with no node left, its first call
	// is refused for capacity, and nothing is placed.
	r = c.call(nil, "GET", sb.path(""), projectAuth, "")
	if !strings.Contains(r.body, `"clientState":"lost"`) {
		t.Errorf("state: %d %s", r.code, r.body)
	}
	if r := c.call(nil, "DELETE", sb.path(""), projectAuth, ""); r.code != http.StatusOK {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	again := c.create(nil)
	ctx, cancel := context.WithTimeout(bg, time.Minute)
	defer cancel()
	r, err := c.execErr(ctx, nil, again, "true")
	if err != nil || r.code != http.StatusBadGateway || !strings.Contains(r.body, "ensure sandbox: no capacity") {
		t.Errorf("the new sandbox without a node: %v %d %s", err, r.code, r.body)
	}
	if row := c.row(again.id); row.ClientState != model.StatePending || row.Node != "" {
		t.Errorf("the new sandbox %+v", row)
	}
}
