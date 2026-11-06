// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package mcpsrv

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/proto"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	toolv1 "github.com/gardenlinux/feos/go/gen/feos/tool/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
	"github.com/gardenlinux/feos/go/internal/tools"
	"github.com/gardenlinux/feos/go/internal/tools/toolstest"
)

// The door's own answers, before the SDK sees a request.
func TestAuthClass(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{AllowedOrigins: []string{"https://app.example"}})
	u := h.http.URL
	for _, tc := range []struct {
		name, method, token, body string
		header                    []string
		status                    int
		want, contentType         string
		wantHeader                []string
	}{
		{"foreign origin", "POST", h.token, initializeBody, []string{"Origin", "https://evil.example"}, 403,
			"forbidden origin\n", "text/plain; charset=utf-8", nil},
		{"GET", "GET", h.token, "", nil, 405, "no server-initiated stream\n", "text/plain; charset=utf-8", nil},
		{"GET without a token", "GET", "", "", nil, 405, "no server-initiated stream\n", "", nil},
		{"no bearer", "POST", "", initializeBody, nil, 401, "missing bearer token\n", "",
			[]string{"WWW-Authenticate", `Bearer realm="sandbox"`}},
		{"basic", "POST", "", initializeBody, []string{"Authorization", "Basic dTpw"}, 401, "missing bearer token\n", "",
			[]string{"WWW-Authenticate", `Bearer realm="sandbox"`}},
		{"unknown token, initialize", "POST", "nope", initializeBody, nil, 401, "unknown sandbox token\n", "",
			[]string{"WWW-Authenticate", ""}},
		{"unknown token, discover", "POST", "nope", `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`, nil, 401,
			"unknown sandbox token\n", "", nil},
		{"unknown token, garbage", "POST", "nope", `{not json`, nil, 401, "unknown sandbox token\n", "", nil},
		{"unknown token, empty", "POST", "nope", ``, nil, 401, "unknown sandbox token\n", "", nil},
		{"unknown token, a batch with initialize", "POST", "nope", `[` + initializeBody + `]`, nil, 401, "unknown sandbox token\n", "", nil},
		{"unknown token, tools/call", "POST", "nope", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"bash","arguments":{"command":"x"}}}`, nil, 404,
			`{"jsonrpc":"2.0","id":7,"error":{"code":-32001,"message":"session not found: unknown sandbox token"}}`, "application/json", nil},
		{"unknown token, string id", "POST", "nope", `{"jsonrpc":"2.0","id": "a-1","method":"tools/list"}`, nil, 404,
			`{"jsonrpc":"2.0","id":"a-1","error":{"code":-32001,"message":"session not found: unknown sandbox token"}}`, "application/json", nil},
		{"unknown token, notification", "POST", "nope", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil, 404,
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"session not found: unknown sandbox token"}}`, "application/json", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, u, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			for i := 0; i+1 < len(tc.header); i += 2 {
				req.Header.Set(tc.header[i], tc.header[i+1])
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			got := body(t, resp)
			if resp.StatusCode != tc.status || got != tc.want {
				t.Errorf("%d %q, want %d %q", resp.StatusCode, got, tc.status, tc.want)
			}
			if tc.contentType != "" && resp.Header.Get("Content-Type") != tc.contentType {
				t.Errorf("content type %q", resp.Header.Get("Content-Type"))
			}
			for i := 0; i+1 < len(tc.wantHeader); i += 2 {
				if resp.Header.Get(tc.wantHeader[i]) != tc.wantHeader[i+1] {
					t.Errorf("%s: %q", tc.wantHeader[i], resp.Header.Get(tc.wantHeader[i]))
				}
			}
		})
	}

	// A lookup that fails is a 503 the clients retry.
	h.tokens.fail(1)
	resp := h.post(u, h.token, "", initializeBody)
	if got := body(t, resp); resp.StatusCode != 503 || got != "token lookup failed\n" || resp.Header.Get("Retry-After") != "1" {
		t.Errorf("lookup failure: %d %q %q", resp.StatusCode, got, resp.Header.Get("Retry-After"))
	}
	// An allowed Origin passes.
	resp = h.post(u, h.token, "", initializeBody, "Origin", "https://app.example")
	if got := body(t, resp); resp.StatusCode != 200 || !strings.Contains(got, `"name":"ferun"`) {
		t.Errorf("allowed origin: %d %q", resp.StatusCode, got)
	}
	// No allowlist: every Origin is refused.
	h2 := httptest.NewServer(h.newServer(Config{}))
	defer h2.Close()
	resp = h.post(h2.URL, h.token, "", initializeBody, "Origin", "https://app.example")
	if got := body(t, resp); resp.StatusCode != 403 || got != "forbidden origin\n" {
		t.Errorf("no allowlist: %d %q", resp.StatusCode, got)
	}
	if n := h.env.Engine.Calls("Acquire", h.sb.ID); n != 0 {
		t.Errorf("the sandbox was acquired %d times", n)
	}
}

// FeCode's client, with the options it passes (none: the server/discover
// path), and a client that only initialises: the baseline, the result
// rendering, the errors.
func TestClients(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{})
	for _, tc := range []struct {
		name string
		opts *mcp.ClientSessionOptions
	}{{"discover", nil}, {"initialize", legacy}} {
		t.Run(tc.name, func(t *testing.T) {
			cs := h.session(tc.opts)
			init := cs.InitializeResult()
			if init.ServerInfo == nil || init.ServerInfo.Name != "ferun" || init.ServerInfo.Version != "test" ||
				init.Instructions != "Tools run in an isolated, persistent sandboxed workspace. Files survive across sessions; the workspace is not shared with other sandboxes." {
				t.Errorf("server %+v instructions %q", init.ServerInfo, init.Instructions)
			}
			if init.Capabilities == nil || init.Capabilities.Tools == nil || init.Capabilities.Resources != nil || init.Capabilities.Prompts != nil {
				t.Errorf("capabilities %+v", init.Capabilities)
			}
			if tc.opts == nil && cs.ID() != "" {
				t.Errorf("a session id on the discover path: %q", cs.ID())
			}
			if tc.opts != nil && cs.ID() == "" {
				t.Error("no session id after initialize")
			}

			// tools/list: the baseline as it is, without touching the
			// sandbox.
			acquires := h.env.Engine.Calls("Acquire", h.sb.ID)
			list, err := cs.ListTools(bg, nil)
			if err != nil {
				t.Fatal(err)
			}
			// The SDK lists tools sorted by name, as it always did.
			specs := map[string]tools.Spec{}
			for _, s := range tools.Specs() {
				specs[s.Name] = s
			}
			if len(list.Tools) != len(specs) {
				t.Fatalf("%d tools", len(list.Tools))
			}
			for i, tool := range list.Tools {
				spec := specs[tool.Name]
				schema, _ := json.Marshal(tool.InputSchema)
				if tool.Description != spec.Description || !jsonEqual(schema, spec.InputSchema) {
					t.Errorf("tool %d: %s %q %s", i, tool.Name, tool.Description, schema)
				}
				if i > 0 && list.Tools[i-1].Name >= tool.Name {
					t.Errorf("tools/list is not sorted at %s", tool.Name)
				}
			}
			if h.env.Engine.Calls("Acquire", h.sb.ID) != acquires {
				t.Error("tools/list acquired the sandbox")
			}

			for _, c := range []struct {
				tool    string
				args    map[string]any
				text    string
				isError bool
			}{
				{"bash", map[string]any{"command": "echo hi"}, "hi\n", false},
				{"bash", map[string]any{"command": "echo oops >&2; exit 3"}, "oops\n\n(exit code 3)", true},
				{"bash", map[string]any{"command": "exit 3"}, "(no output)\n(exit code 3)", true},
				{"bash", map[string]any{"command": "printf '  \\n'"}, "(no output)", false},
				{"write", map[string]any{"path": "/w/a.txt", "content": "alpha\n"}, "wrote /w/a.txt\n", false},
				{"read", map[string]any{"path": "/w/a.txt"}, "     1\talpha\n", false},
				{"rg", map[string]any{"pattern": "zzz", "path": "/w"}, "(no output)\n(exit code 1)", false},
				{"rg", map[string]any{"pattern": "("}, "regex parse error: (\n            ^\nerror: unclosed group\n\n(exit code 2)", true},
				{"fd", map[string]any{"pattern": "zzz", "path": "/w"}, "(no output)\n(exit code 1)", false},
				{"edit", map[string]any{"path": "/w/a.txt", "edits": []any{}}, "error: edits must not be empty", true},
				{"bash", map[string]any{"command": 5}, "error: json: cannot unmarshal number into Go struct field BashParams.command of type string", true},
			} {
				text, isError, err := call(cs, c.tool, c.args)
				if err != nil {
					t.Fatalf("%s %v: %v", c.tool, c.args, err)
				}
				if c.tool == "rg" && c.args["pattern"] == "(" {
					// The node's own regex error text is not ours; the
					// rendering around it is.
					if !strings.HasSuffix(text, "\n\n(exit code 2)") || !isError {
						t.Errorf("rg error: %q %v", text, isError)
					}
					continue
				}
				if text != c.text || isError != c.isError {
					t.Errorf("%s %v: %q %v, want %q %v", c.tool, c.args, text, isError, c.text, c.isError)
				}
			}

			// An unknown tool is the SDK's error.
			if _, _, err := call(cs, "nope", nil); err == nil || !strings.Contains(err.Error(), "nope") {
				t.Errorf("unknown tool: %v", err)
			}
		})
	}
	if h.methods.has("initialize") != true || !h.methods.has("server/discover") {
		t.Errorf("methods seen: %v", h.methods.seen)
	}
}

// Truncation, pictures and failed calls, as MCP renders them.
func TestRendering(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{})
	cs := h.session(nil)

	h.env.Node.Script().SetExec(func(_ context.Context, _ podid.ID, _ *containerv1.ExecRequest, out testnode.ExecOutput) (int32, bool, error) {
		_ = out.Stdout([]byte(strings.Repeat("a", 40000)))
		_ = out.Stderr([]byte(strings.Repeat("b", 40000)))
		return 0, true, nil
	})
	text, isError, err := call(cs, "bash", map[string]any{"command": "x"})
	if err != nil || isError || text != strings.Repeat("a", 40000)+strings.Repeat("b", 10000)+"\n... [truncated]\n" {
		t.Errorf("truncated: %d bytes %v %v", len(text), isError, err)
	}
	h.env.Node.Script().SetExec(nil)

	png := []byte("\x89PNG not really")
	h.env.Node.Script().SetTool(func(_ context.Context, _ podid.ID, method string, _ proto.Message) (proto.Message, bool, error) {
		if method == toolv1.ToolService_Read_FullMethodName {
			return &toolv1.ReadResponse{Image: png, MediaType: "image/png", Width: 640, Height: 480}, true, nil
		}
		return nil, false, nil
	})
	res, err := cs.CallTool(bg, &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"path": "/s.png"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 2 || res.IsError {
		t.Fatalf("picture: %+v", res)
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); !ok || tc.Text != "/s.png: image/png, 640x480 px, 1 KiB\n" {
		t.Errorf("picture's line %+v", res.Content[0])
	}
	if ic, ok := res.Content[1].(*mcp.ImageContent); !ok || ic.MIMEType != "image/png" || string(ic.Data) != string(png) {
		t.Errorf("picture %+v", res.Content[1])
	}
	h.env.Node.Script().SetTool(nil)

	// Infrastructure failures: isError with "error: ", and the output so
	// far before it.
	h.env.Engine.FailAcquire(h.sb.ID, model.ErrLost)
	text, isError, err = call(cs, "bash", map[string]any{"command": "true"})
	if err != nil || !isError || text != "error: ensure sandbox: sandbox lost" {
		t.Errorf("lost: %q %v %v", text, isError, err)
	}
	text, isError, err = call(cs, "bash", map[string]any{"command": "echo begun; sleep 60", "timeout": 1})
	if err != nil || !isError || text != "begun\n\nerror: timeout after 1s" {
		t.Errorf("timeout: %q %v %v", text, isError, err)
	}
	h.env.Engine.FailAcquire(h.sb.ID, fmt.Errorf("%w (retryable): no feasible node", model.ErrNoCapacity))
	text, isError, _ = call(cs, "read", map[string]any{"path": "/a"})
	if !isError || text != "error: ensure sandbox: no capacity (retryable): no feasible node" {
		t.Errorf("no capacity: %q", text)
	}
	h.env.Engine.FailAcquire(h.sb.ID, hibernating{h.sb.ID})
	text, isError, err = call(cs, "bash", map[string]any{"command": "true"})
	if err != nil || !isError || text != "error: ensure sandbox: sandbox "+h.sb.ID.String()+" is hibernating; retry in a minute" {
		t.Errorf("hibernating: %q %v %v", text, isError, err)
	}

	// render on its own: the old texts.
	for _, tc := range []struct {
		res     tools.Result
		err     error
		text    string
		isError bool
	}{
		{tools.Result{Text: "x", ExitCode: 0}, nil, "x", false},
		{tools.Result{Text: "", ExitCode: 0}, nil, "(no output)", false},
		{tools.Result{Text: " \n", ExitCode: 2, IsError: true}, nil, "(no output)\n(exit code 2)", true},
		{tools.Result{Text: "abc", Truncated: true}, nil, "abc\n... [truncated]\n", false},
		{tools.Result{Text: "abc", Truncated: true}, fmt.Errorf("boom"), "abc\n... [truncated]\n\nerror: boom", true},
		{tools.Result{Text: "partial"}, fmt.Errorf("node n1: gone"), "partial\nerror: node n1: gone", true},
		{tools.Result{}, fmt.Errorf("x"), "error: x", true},
		// A read of text: numbered as cat -n numbers, and where to go on.
		{tools.Result{Text: "a\n\tb\n", FirstLine: 1}, nil, "     1\ta\n     2\t\tb\n", false},
		{tools.Result{Text: "no newline", FirstLine: 1}, nil, "     1\tno newline", false},
		{tools.Result{Text: "x\ny\n", FirstLine: 7, More: true}, nil,
			"     7\tx\n     8\ty\n... [the file goes on: read with offset=9 for more]\n", false},
		{tools.Result{Text: "ab\ncd", FirstLine: 1, Truncated: true}, nil,
			"     1\tab\n     2\tcd\n... [truncated]\n... [the file goes on: read with offset=2 for more]\n", false},
	} {
		r := render(tc.res, tc.err)
		if got := r.Content[0].(*mcp.TextContent).Text; got != tc.text || r.IsError != tc.isError {
			t.Errorf("render %+v %v: %q %v", tc.res, tc.err, got, r.IsError)
		}
	}
}

// The stream and tool budgets of the nodes' port are an isError answer.
func TestBudgetOverMCP(t *testing.T) {
	h := newHarness(t, toolstest.Options{MaxStreams: 2}, Config{}) // one long stream
	cs := h.session(nil)
	if _, _, err := call(cs, "bash", map[string]any{"command": "true"}); err != nil {
		t.Fatal(err)
	}
	started, _ := blockExec(h.env)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	go func() {
		_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "bash", Arguments: map[string]any{"command": "wait"}})
	}()
	wait(t, started, "the blocking exec")
	text, isError, err := call(h.session(nil), "bash", map[string]any{"command": "x"})
	if err != nil || !isError || text != "error: node n1: stream budget exhausted" {
		t.Errorf("%q %v %v", text, isError, err)
	}

	// The tool budget: one unary tool call at once.
	gate := make(chan struct{})
	defer close(gate)
	entered := make(chan struct{}, 10)
	h.env.Node.Script().SetHook(toolv1.ToolService_Read_FullMethodName, func(ctx context.Context, _ string, _ any) error {
		entered <- struct{}{}
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return nil
	})
	go func() {
		_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"path": "/a"}})
	}()
	wait(t, entered, "the blocking read")
	text, isError, err = call(h.session(nil), "read", map[string]any{"path": "/a"})
	if err != nil || !isError || text != "error: node n1: tool budget exhausted" {
		t.Errorf("tool budget: %q %v %v", text, isError, err)
	}
}

// A token revoked while a session uses it: the session's calls fail with
// "session not found" (FeCode drops the session and initialises again,
// which is refused), and a session with a newly minted token works.
func TestRevokedToken(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{})
	for _, opts := range []*mcp.ClientSessionOptions{nil, legacy} {
		token := h.tokens.mint(h.sb)
		cs, err := h.connect(h.http.URL, token, nil, opts)
		if err != nil {
			t.Fatal(err)
		}
		if text, _, err := call(cs, "bash", map[string]any{"command": "echo one"}); err != nil || text != "one\n" {
			t.Fatalf("%q %v", text, err)
		}
		h.tokens.revoke(token)
		_, _, err = call(cs, "bash", map[string]any{"command": "echo two"})
		if err == nil || !strings.Contains(err.Error(), "session not found: unknown sandbox token") {
			t.Fatalf("revoked: %v", err)
		}
		if _, err := h.connect(h.http.URL, token, nil, opts); err == nil {
			t.Error("a session opened with a revoked token")
		}
		cs.Close()
		fresh := h.tokens.mint(h.sb)
		cs, err = h.connect(h.http.URL, fresh, nil, opts)
		if err != nil {
			t.Fatal(err)
		}
		if text, _, err := call(cs, "bash", map[string]any{"command": "echo three"}); err != nil || text != "three\n" {
			t.Errorf("after the re-mint: %q %v", text, err)
		}
		cs.Close()
	}
}

// A database blip is a 503: the call fails, the session does not.
func TestDatabaseBlip(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{})
	for _, opts := range []*mcp.ClientSessionOptions{nil, legacy} {
		cs := h.session(opts)
		h.tokens.fail(1)
		if _, _, err := call(cs, "bash", map[string]any{"command": "echo x"}); err == nil {
			t.Fatal("the call went through a failed lookup")
		}
		if text, _, err := call(cs, "bash", map[string]any{"command": "echo after"}); err != nil || text != "after\n" {
			t.Errorf("after the blip: %q %v", text, err)
		}
	}
}

// Behind a local proxy that sends an external Host, as on a single
// machine, the door still serves.
func TestLoopbackProxy(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{})
	target, _ := url.Parse(h.http.URL)
	proxy := httptest.NewServer(&httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.Host = "run.fecode.dev"
	}, FlushInterval: -1})
	defer proxy.Close()
	for _, opts := range []*mcp.ClientSessionOptions{nil, legacy} {
		cs, err := h.connect(proxy.URL, h.token, nil, opts)
		if err != nil {
			t.Fatal(err)
		}
		if text, _, err := call(cs, "bash", map[string]any{"command": "echo proxied"}); err != nil || text != "proxied\n" {
			t.Errorf("%q %v", text, err)
		}
		cs.Close()
	}
}

// Session ids are signed for their token, and every door of the cluster
// verifies them.
func TestSessionIDs(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{})
	sid := h.initialize(h.http.URL, h.token)
	raw, err := base64.RawURLEncoding.DecodeString(sid)
	if err != nil || len(raw) != nonceLen+macLen {
		t.Fatalf("session id %q: %v", sid, err)
	}
	other := h.newServer(Config{})
	hash := h.tokens.m[h.token].TokenHash
	if !h.srv.verifySession(bg, sid, hash) || !other.verifySession(bg, sid, hash) {
		t.Error("the session id does not verify")
	}
	if h.srv.verifySession(bg, sid, strings.Repeat("0", 64)) {
		t.Error("the session id verifies for another token")
	}
	raw[0] ^= 1
	if h.srv.verifySession(bg, base64.RawURLEncoding.EncodeToString(raw), hash) || h.srv.verifySession(bg, "x", hash) {
		t.Error("a forged session id verifies")
	}
	if h.initialize(h.http.URL, h.token) == sid {
		t.Error("two sessions with one id")
	}
	// A session id that does not verify is served as none, never a 404.
	resp := h.post(h.http.URL, h.token, "garbage", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if got := body(t, resp); resp.StatusCode != 200 || !strings.Contains(got, `"bash"`) {
		t.Errorf("unverified session: %d %q", resp.StatusCode, got)
	}
	// A configured key is used as it is.
	keyed := New(Config{SessionKey: []byte(strings.Repeat("k", 32))}, h.runner, h.tokens.resolve, nil)
	s1, err := keyed.newSession(bg, hash)
	if err != nil || !keyed.verifySession(bg, s1, hash) || h.srv.verifySession(bg, s1, hash) {
		t.Errorf("a configured key: %v", err)
	}
	// Without a key there are no sessions, and no error for the client.
	keyless := httptest.NewServer(New(Config{}, h.runner, h.tokens.resolve, nil))
	defer keyless.Close()
	resp = h.post(keyless.URL, h.token, "", initializeBody)
	if got := body(t, resp); resp.StatusCode != 200 || resp.Header.Get(sessionIDHeader) != "" || !strings.Contains(got, "serverInfo") {
		t.Errorf("keyless: %d %q %q", resp.StatusCode, resp.Header.Get(sessionIDHeader), got)
	}
}

// A notifications/cancelled of a verified session reaches the call it
// names on another door of the cluster, through ferun_cancel; one of an
// unverified session, or naming another request, cancels nothing.
func TestCancelAcrossServers(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{})
	other := httptest.NewServer(h.newServer(Config{}))
	defer other.Close()
	if _, _, err := call(h.session(nil), "bash", map[string]any{"command": "true"}); err != nil {
		t.Fatal(err)
	}
	started, ended := blockExec(h.env)
	sid := h.initialize(h.http.URL, h.token)

	answered := make(chan string, 1)
	go func() {
		resp := h.post(h.http.URL, h.token, sid,
			`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"bash","arguments":{"command":"wait"}}}`,
			"Mcp-Protocol-Version", "2025-11-25")
		answered <- body(t, resp)
	}()
	wait(t, started, "the call")

	cancelled := func(id, session, token string) {
		resp := h.post(other.URL, token, session,
			fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%s,"reason":"user"}}`, id),
			"Mcp-Protocol-Version", "2025-11-25")
		if got := body(t, resp); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("cancel: %d %q", resp.StatusCode, got)
		}
	}
	otherToken := h.tokens.mint(h.env.NewSandbox())
	cancelled("6", sid, h.token)      // another request
	cancelled(`"5"`, sid, h.token)    // another id: a string
	cancelled("5", "", h.token)       // no session
	cancelled("5", sid, otherToken)   // a session of another token
	cancelled("5", "forged", h.token) // a session that does not verify
	select {
	case <-ended:
		t.Fatal("a cancel that names another call cancelled it")
	case <-time.After(500 * time.Millisecond):
	}
	cancelled("5", sid, h.token)
	wait(t, ended, "the cancellation")
	select {
	case got := <-answered:
		if !strings.Contains(got, `"id":5`) || !strings.Contains(got, "context canceled") {
			t.Errorf("answer of the cancelled call %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled call did not answer")
	}
}

// A client that gives up on a call cancels it in the guest, in every
// protocol version: the call is bound to its request.
func TestClientCancellation(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{})
	for _, opts := range []*mcp.ClientSessionOptions{nil, legacy} {
		cs := h.session(opts)
		if _, _, err := call(cs, "bash", map[string]any{"command": "true"}); err != nil {
			t.Fatal(err)
		}
		started, ended := blockExec(h.env)
		ctx, cancel := context.WithCancel(bg)
		done := make(chan error, 1)
		go func() {
			_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "bash", Arguments: map[string]any{"command": "wait"}})
			done <- err
		}()
		wait(t, started, "the call")
		cancel()
		wait(t, ended, "the guest's command to end")
		if err := <-done; err == nil {
			t.Error("the cancelled call returned no error")
		}
		h.env.Node.Script().SetExec(nil)
		// The session goes on.
		if text, _, err := call(cs, "bash", map[string]any{"command": "echo next"}); err != nil || text != "next\n" {
			t.Errorf("after the cancellation: %q %v", text, err)
		}
	}

	// A request without a session, of the old protocol, that the client
	// drops: its call ends with it.
	started, ended := blockExec(h.env)
	ctx, cancel := context.WithCancel(bg)
	req, _ := http.NewRequestWithContext(ctx, "POST", h.http.URL,
		strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"bash","arguments":{"command":"wait"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	go func() {
		if resp, err := (&http.Client{Transport: transport()}).Do(req); err == nil {
			_ = body(t, resp)
		}
	}()
	wait(t, started, "the raw call")
	cancel()
	wait(t, ended, "the dropped request's command to end")
	h.env.Node.Script().SetExec(nil)
	waitHolds(t, h, 0)
}

func waitHolds(t *testing.T, h *harness, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for h.env.Engine.Holds(h.sb.ID) != n {
		if time.Now().After(deadline) {
			t.Fatalf("%d holds, want %d", h.env.Engine.Holds(h.sb.ID), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A long call's response carries keepalive comments, outside the SDK's
// events, which clients ignore.
func TestKeepalive(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{KeepaliveInterval: 100 * time.Millisecond})
	h.env.Node.Script().SetExec(func(ctx context.Context, _ podid.ID, _ *containerv1.ExecRequest, out testnode.ExecOutput) (int32, bool, error) {
		time.Sleep(450 * time.Millisecond)
		_ = out.Stdout([]byte("late\n"))
		return 0, true, nil
	})
	resp := h.post(h.http.URL, h.token, "",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"bash","arguments":{"command":"x"}}}`,
		"Mcp-Protocol-Version", "2025-11-25")
	got := body(t, resp)
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	n := strings.Count(got, ": keepalive\n\n")
	data := sseData(got)
	if n < 2 || len(data) != 1 || !strings.Contains(data[0], `"text":"late\n"`) || !strings.HasSuffix(got, "\n\n") {
		t.Errorf("%d keepalives, data %q in %q", n, data, got)
	}
	if i := strings.Index(got, ": keepalive"); i < 0 || i > strings.Index(got, "data: ") {
		t.Errorf("no keepalive before the answer: %q", got)
	}
	// A quick call and a list get none; clients take a keepalive in stride.
	cs := h.session(legacy)
	if text, _, err := call(cs, "bash", map[string]any{"command": "x"}); err != nil || text != "late\n" {
		t.Errorf("through the SDK: %q %v", text, err)
	}
	h.env.Node.Script().SetExec(nil)
	resp = h.post(h.http.URL, h.token, "", `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`, "Mcp-Protocol-Version", "2025-11-25")
	if got := body(t, resp); strings.Contains(got, "keepalive") {
		t.Errorf("a list with a keepalive: %q", got)
	}
}

// With a progress token, a call reports progress on its timer and per
// chunk of output.
func TestProgress(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{ProgressInterval: 100 * time.Millisecond})
	for _, opts := range []*mcp.ClientSessionOptions{nil, legacy} {
		// The client handles notifications on a goroutine of their own, so
		// one may arrive after its call returned: the handler counts them
		// under mu by token, and the checks wait for late ones.
		var mu sync.Mutex
		var seen []float64 // progress under the token "tok"
		others := 0        // notifications under any other token
		cs, err := h.connect(h.http.URL, h.token, &mcp.ClientOptions{
			ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
				mu.Lock()
				defer mu.Unlock()
				if req.Params.ProgressToken == "tok" {
					seen = append(seen, req.Params.Progress)
				} else {
					others++
				}
			},
		}, opts)
		if err != nil {
			t.Fatal(err)
		}
		h.env.Node.Script().SetExec(func(ctx context.Context, _ podid.ID, _ *containerv1.ExecRequest, out testnode.ExecOutput) (int32, bool, error) {
			for i := range 3 {
				_ = out.Stdout([]byte(fmt.Sprintf("%d\n", i)))
				time.Sleep(150 * time.Millisecond)
			}
			return 0, true, nil
		})
		params := &mcp.CallToolParams{Name: "bash", Arguments: map[string]any{"command": "x"}}
		params.SetProgressToken("tok")
		res, err := cs.CallTool(bg, params)
		if err != nil || res.IsError {
			t.Fatalf("%+v %v", res, err)
		}
		var got []float64
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			mu.Lock()
			got = append([]float64(nil), seen...)
			mu.Unlock()
			if len(got) >= 5 || time.Now().After(deadline) {
				break
			}
		}
		if len(got) < 5 {
			t.Errorf("%d progress notifications", len(got))
		}
		for i := 1; i < len(got); i++ {
			if got[i] <= got[i-1] {
				t.Errorf("progress not increasing: %v", got)
				break
			}
		}
		// Without a token, none, under no token at all: once the first
		// call's notifications have all been handled (none for several
		// progress intervals), the count stays where it is.
		before := len(got)
		for {
			time.Sleep(300 * time.Millisecond)
			mu.Lock()
			n := len(seen)
			mu.Unlock()
			if n == before {
				break
			}
			before = n
		}
		if _, _, err := call(cs, "bash", map[string]any{"command": "x"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		mu.Lock()
		if others != 0 || len(seen) != before {
			t.Errorf("%d progress notifications without a token", others+len(seen)-before)
		}
		mu.Unlock()
		cs.Close()
	}
	h.env.Node.Script().SetExec(nil)
}

// The token's use is recorded, at most once a minute.
func TestTokenTouch(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{})
	token := uuid.NewString()
	sb := h.env.NewSandbox()
	sb.TokenHash = store.HashToken(token)
	if _, err := h.env.Store.CreateSandboxToken(bg, sb.TokenHash, sb.ID, "default", ""); err != nil {
		t.Fatal(err)
	}
	h.tokens.mu.Lock()
	h.tokens.m[token] = sb
	h.tokens.mu.Unlock()
	h.initialize(h.http.URL, token)
	first, err := h.env.Store.GetSandboxToken(bg, sb.TokenHash)
	if err != nil || first.LastUsedAt.IsZero() {
		t.Fatalf("not touched: %+v %v", first, err)
	}
	time.Sleep(10 * time.Millisecond)
	h.initialize(h.http.URL, token)
	again, _ := h.env.Store.GetSandboxToken(bg, sb.TokenHash)
	if !again.LastUsedAt.Equal(first.LastUsedAt) {
		t.Errorf("touched twice within a minute: %v %v", first.LastUsedAt, again.LastUsedAt)
	}
}

// hibernating is the engine's answer for a sandbox whose hibernation did
// not end within Ensure's wait: a text of its own that is model.ErrBusy.
type hibernating struct{ id podid.ID }

func (e hibernating) Error() string {
	return "sandbox " + e.id.String() + " is hibernating; retry in a minute"
}

func (e hibernating) Unwrap() error { return model.ErrBusy }
