// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

// Sandbox access over FeRun's MCP endpoint. This backend is an ordinary
// MCP client of ferund; it has no privileged way to run anything:
//
//   - Provisioning (once per conversation): POST /api/v1/sandboxes
//     without a sandbox ID; ferund mints the ID (ADR 0019) and returns it
//     with the sandbox's first token. The backend keeps no database: it
//     hands the ID to the browser as a signed sandbox reference
//     (sandboxref.go), bound to the user and the conversation, and the
//     browser sends it back with every chat request. A sandbox the
//     backend has no session for (it restarted) gets a new token from
//     POST /api/v1/sandboxes/{id}/tokens; a reference whose sandbox is
//     gone gets a new sandbox and a new reference.
//   - Execution: tools/call against /mcp with the sandbox token as
//     bearer, exactly the surface any other agent gets.
//   - The LLM's tool schemas come from GET /api/v1/tools; there are no
//     copies of them here.
//
// MCP sessions are cached per conversation; on 404 (ferund restarted, the
// session is gone) the client initializes again, once, with the same
// bearer, the path the MCP spec gives every client. The session is never
// the identity; the sandbox is, through its token. A chat request without
// a conversation ID gets a generated one (an anonymous throwaway sandbox,
// cleaned up by hand).

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxToolOutput caps one tool result in the conversation (~50 KiB).
const maxToolOutput = 50_000

// toolRunWait is how long a tool may run once its sandbox is up, what
// the chat used to allow a whole call. A call can first wait for its
// sandbox to be created (a cold image pull) or resumed; ferund announces
// that bound itself (preparationWaitMs on GET /api/v1/tools: its
// preparation budget plus its reply budget), and the call timeout is the
// two added up. toolRunWait is also the margin that lets ferund's own
// answer at the end of a failed preparation reach us before we give up:
// a client that cancels sooner cancels the preparation, and its retry
// starts the pull from the beginning.
const toolRunWait = 4 * time.Minute

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// bearerRT sets the Authorization header (tenant token or sandbox
// token). Deliberately without keep-alives: chat and sandbox traffic are
// often minutes apart, and a half-open connection that k3s conntrack
// dropped silently then eats the full client timeout (Go does not retry
// such POSTs). Fresh connections are cheap and safe here.
type bearerRT struct{ token string }

var bearerTransport = &http.Transport{DisableKeepAlives: true}

func (rt bearerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+rt.token)
	return bearerTransport.RoundTrip(r)
}

// sandboxClient is the provisioning client (tenant bearer) plus the
// cache of MCP sessions per conversation.
type sandboxClient struct {
	base      string // e.g. http://ferund.ferun.svc.cluster.local:8100
	ctrlToken string
	env       string // the environment its sandboxes are in (ADR 0035)
	refKey    []byte // signs the sandbox references (sandboxref.go)
	http      *http.Client
	// mu serializes provisioning, so that two requests of a conversation
	// never create two sandboxes.
	mu       sync.Mutex
	sessions map[convKey]*convSession
	// tools is the first tool list that arrived (loadedTools); nil until then.
	toolsMu sync.Mutex
	tools   *toolset
}

// toolset is one answer of GET /api/v1/tools: the definitions the LLM is
// offered, their names, and ferund's announced preparation wait.
type toolset struct {
	defs     []Tool
	known    map[string]bool
	prepWait time.Duration
}

// has: was the tool in the tool list? A nil toolset (no ferund
// configured) knows no tools.
func (ts *toolset) has(name string) bool { return ts != nil && ts.known[name] }

// convKey is a conversation: whose (the signed-in user's subject, empty
// without sign-in) and which.
type convKey struct{ subject, conv string }

type convSession struct {
	sandboxID string
	ref       string // the sandbox reference handed to the browser
	token     string
	conn      *mcp.ClientSession // nil until connected
	image     string             // the image passed at creation; empty: not known
}

// chatSandbox is one chat request's view of its conversation's sandbox:
// whose conversation, the reference the browser sent, the image, and how
// the browser learns a new reference. Its tool calls share it.
type chatSandbox struct {
	key   convKey
	ref   string
	image string
	onRef func(ref string) // may be nil

	mu   sync.Mutex
	told string
}

// tell passes ref on to the browser unless it has it already.
func (b *chatSandbox) tell(ref string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ref == b.ref || ref == b.told || b.onRef == nil {
		return
	}
	b.told = ref
	b.onRef(ref)
}

func newSandboxClient(base, ctrlToken, env string, refKey []byte) (*sandboxClient, error) {
	if len(refKey) == 0 {
		return nil, errors.New("no key for sandbox references")
	}
	return &sandboxClient{
		base:      strings.TrimRight(base, "/"),
		ctrlToken: ctrlToken,
		env:       env,
		refKey:    refKey,
		http: &http.Client{
			Transport: tracedTransport(bearerRT{ctrlToken}),
			Timeout:   30 * time.Second,
		},
		sessions: map[convKey]*convSession{},
	}, nil
}

// apiError is an answer of ferund's REST API other than 2xx.
type apiError struct {
	method, path string
	status       int
	body         string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.method, e.path, e.status, e.body)
}

// isNoSuchSandbox: ferund knows no sandbox of the ID (404), or takes no
// such ID (400).
func isNoSuchSandbox(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && (ae.status == http.StatusNotFound || ae.status == http.StatusBadRequest)
}

// provisioningAPI calls one route of ferund's tenant-scoped REST API.
// body may be empty for a GET.
func (s *sandboxClient) provisioningAPI(ctx context.Context, method, path, body string, out any) error {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, rdr)
	if err != nil {
		return err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return &apiError{method: method, path: path, status: resp.StatusCode,
			body: string(raw[:min(len(raw), 400)])}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// sessionFor returns a conversation's MCP session, lazily: it finds or
// provisions the sandbox and its token, then connects /mcp. It returns the
// session once the sandbox is known, even when connecting failed, so that
// the caller hands out its reference: a sandbox the browser never learns
// of would be made again with the next request. image only counts when
// the sandbox is created; it is immutable afterwards, and a later,
// different non-empty value is logged and ignored.
func (s *sandboxClient) sessionFor(ctx context.Context, b *chatSandbox) (*convSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs, ok := s.sessions[b.key]
	if ok {
		if b.image != "" && cs.image != "" && b.image != cs.image {
			log.Printf("conversation %s: image mismatch %q != %q, keeping %q",
				b.key.conv, b.image, cs.image, cs.image)
		}
	} else {
		id, token, err := s.adopt(ctx, b)
		if err != nil {
			return nil, err
		}
		image := ""
		if id == "" {
			if id, token, err = s.create(ctx, b.image); err != nil {
				return nil, err
			}
			image = b.image
			log.Printf("conversation %s: sandbox %s created (image=%q)", b.key.conv, id, b.image)
		}
		cs = &convSession{sandboxID: id, token: token, image: image,
			ref: signSandboxRef(s.refKey, b.key.subject, b.key.conv, id)}
		s.sessions[b.key] = cs
	}
	if cs.conn == nil {
		if err := s.connectSession(ctx, cs); err != nil {
			return cs, err
		}
		log.Printf("conversation %s: mcp session established (sandbox %s)", b.key.conv, cs.sandboxID)
	}
	return cs, nil
}

// adopt returns the sandbox the conversation has already, with a new
// token, or no ID when it has none (any more). That is the sandbox of a
// valid reference, or, without one, the sandbox a conversation from
// before ADR 0019 was given: its own UUID as the sandbox ID. Knowing such
// a conversation's random UUID was all it took to reach its sandbox
// before, and it is still enough for those; new sandboxes are only ever
// reached through a reference. Only ferund's 404 (or 400) means "none":
// any other failure is an error, not a reason to make a second sandbox.
func (s *sandboxClient) adopt(ctx context.Context, b *chatSandbox) (id, token string, err error) {
	if b.ref != "" {
		if id, ok := verifySandboxRef(s.refKey, b.key.subject, b.key.conv, b.ref); ok {
			return s.adoptID(ctx, b, id, "its reference")
		}
		log.Printf("conversation %s: invalid sandbox reference, ignored", b.key.conv)
	}
	if uuidPattern.MatchString(b.key.conv) {
		return s.adoptID(ctx, b, b.key.conv, "its conversation ID")
	}
	return "", "", nil
}

func (s *sandboxClient) adoptID(ctx context.Context, b *chatSandbox, id, via string) (string, string, error) {
	token, err := s.mintToken(ctx, id)
	if isNoSuchSandbox(err) {
		if via == "its reference" {
			log.Printf("conversation %s: sandbox %s is gone, making a new one", b.key.conv, id)
		}
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	log.Printf("conversation %s: sandbox %s by %s", b.key.conv, id, via)
	return id, token, nil
}

// create makes a sandbox; ferund mints its ID.
func (s *sandboxClient) create(ctx context.Context, image string) (id, token string, err error) {
	body, err := json.Marshal(struct {
		Image       string `json:"image"`
		Environment string `json:"environment"`
	}{image, s.env})
	if err != nil {
		return "", "", err
	}
	var created struct {
		Status struct {
			SandboxID string `json:"sandboxId"`
		} `json:"status"`
		Token string `json:"token"`
	}
	if err := s.provisioningAPI(ctx, http.MethodPost, "/api/v1/sandboxes", string(body), &created); err != nil {
		return "", "", err
	}
	id = created.Status.SandboxID
	if !validSandboxID(id) {
		return "", "", fmt.Errorf("POST /api/v1/sandboxes: no valid sandbox ID in the answer (%q)", id)
	}
	if token = created.Token; token == "" {
		if token, err = s.mintToken(ctx, id); err != nil {
			return "", "", err
		}
	}
	return id, token, nil
}

// mintToken gets a new token for a sandbox of the tenant.
func (s *sandboxClient) mintToken(ctx context.Context, id string) (string, error) {
	var minted struct {
		Token string `json:"token"`
	}
	if err := s.provisioningAPI(ctx, http.MethodPost,
		"/api/v1/sandboxes/"+url.PathEscape(id)+"/tokens",
		`{"label":"fechat-backend"}`, &minted); err != nil {
		return "", err
	}
	if minted.Token == "" {
		return "", fmt.Errorf("POST /api/v1/sandboxes/%s/tokens: no token in the answer", id)
	}
	return minted.Token, nil
}

func (s *sandboxClient) connectSession(ctx context.Context, cs *convSession) error {
	if cs.conn != nil {
		cs.conn.Close()
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "fechat-backend", Version: "v1"}, nil)
	conn, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: s.base + "/mcp",
		// no standalone SSE GET: ferund answers it with 405
		DisableStandaloneSSE: true,
		HTTPClient:           &http.Client{Transport: tracedTransport(bearerRT{cs.token})},
	}, nil)
	if err != nil {
		return err
	}
	cs.conn = conn
	return nil
}

func isSessionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "404") || strings.Contains(msg, "session not found")
}

// executeSandboxTool runs one tool over MCP in the conversation's sandbox
// and returns the collected output as a string.
//
// The call outlives the request it belongs to (a browser that goes away
// does not cut a command off), but stays in its trace.
func (s *sandboxClient) executeSandboxTool(ctx context.Context, ts *toolset, b *chatSandbox, tool, argsJSON string) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ts.callTimeout())
	defer cancel()

	cs, err := s.sessionFor(ctx, b)
	if cs != nil {
		b.tell(cs.ref)
	}
	if err != nil {
		// to the LLM and to the log: otherwise the error is seen nowhere
		log.Printf("conversation %s: provisioning failed: %v", b.key.conv, err)
		return "error: sandbox provisioning: " + err.Error()
	}
	var args map[string]any
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "error: invalid tool arguments: " + err.Error()
		}
	}
	res, err := cs.conn.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil && isSessionError(err) {
		// the spec's path after a lost session: initialize again, once
		s.mu.Lock()
		cerr := s.connectSession(ctx, cs)
		s.mu.Unlock()
		if cerr == nil {
			res, err = cs.conn.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		}
	}
	if err != nil {
		return "error: mcp: " + err.Error()
	}
	var out strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out.WriteString(tc.Text)
		}
	}
	result := out.String()
	if len(result) > maxToolOutput {
		result = result[:maxToolOutput] + "\n... [truncated]\n"
	}
	if res.IsError {
		result = "error: " + result
	}
	if strings.TrimSpace(result) == "" {
		result = "(no output)"
	}
	return result
}

// callTimeout bounds one tool call: ferund's preparation wait plus the
// tool's own run time (toolRunWait).
func (ts *toolset) callTimeout() time.Duration { return ts.prepWait + toolRunWait }

// loadedTools returns the tool list, fetching it on the first chat
// message rather than at startup: a backend that fetched it once at boot,
// while ferund was down, would run without tools for its whole lifetime.
// A failed fetch is not kept: that message fails and the next one asks
// again.
func (s *sandboxClient) loadedTools(ctx context.Context) (*toolset, error) {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	if s.tools == nil {
		ts, err := s.sandboxTools(ctx)
		if err != nil {
			return nil, err
		}
		s.tools = ts
		log.Printf("sandbox tools loaded via %s (%d tools)", s.base, len(ts.defs))
	}
	return s.tools, nil
}

// sandboxTools fetches the tool schemas from GET /api/v1/tools and
// builds the OpenAI tool definitions from them, the one source. The
// schemas are not per sandbox, which is why they come from the tenant's
// REST API and not from an MCP session (no sandbox token needed). The
// same answer carries ferund's preparation wait, which sizes every tool
// call's timeout (callTimeout); a ferund that does not announce it is
// refused rather than guessed at.
func (s *sandboxClient) sandboxTools(ctx context.Context) (*toolset, error) {
	var res struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
		PreparationWaitMs int64 `json:"preparationWaitMs"`
	}
	if err := s.provisioningAPI(ctx, http.MethodGet, "/api/v1/tools", "", &res); err != nil {
		return nil, err
	}
	if res.PreparationWaitMs <= 0 {
		return nil, fmt.Errorf("GET /api/v1/tools announces no preparationWaitMs: " +
			"ferund does not say how long bringing a sandbox up may take")
	}
	ts := &toolset{
		known:    map[string]bool{},
		prepWait: time.Duration(res.PreparationWaitMs) * time.Millisecond,
	}
	for _, t := range res.Tools {
		var schema any
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
			return nil, fmt.Errorf("tools %s: %w", t.Name, err)
		}
		var tool Tool
		tool.Type = "function"
		tool.Function.Name = t.Name
		tool.Function.Description = t.Description
		tool.Function.Parameters = schema
		ts.defs = append(ts.defs, tool)
		ts.known[t.Name] = true
	}
	return ts, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
