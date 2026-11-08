// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var testRefKey = newRefKey("a session secret of at least thirty-two bytes")

func TestSandboxRefRoundTrip(t *testing.T) {
	for _, id := range []string{"k3m9x0abcde", "0194b6d2-7c1e-7a35-9f2b-1c2d3e4f5a6b"} {
		ref := signSandboxRef(testRefKey, "user-1", "conv-1", id)
		if !strings.HasPrefix(ref, "v1."+id+".") {
			t.Fatalf("reference %q", ref)
		}
		if got, ok := verifySandboxRef(testRefKey, "user-1", "conv-1", ref); !ok || got != id {
			t.Fatalf("verify(%q) = %q, %v", ref, got, ok)
		}
	}
	// without sign-in, the subject is empty
	ref := signSandboxRef(testRefKey, "", "conv-1", "k3m9x0abcde")
	if got, ok := verifySandboxRef(testRefKey, "", "conv-1", ref); !ok || got != "k3m9x0abcde" {
		t.Fatalf("no subject: %q, %v", got, ok)
	}
}

// A reference holds only for the user and the conversation it was handed
// out for, under this backend's key, unchanged.
func TestSandboxRefRefused(t *testing.T) {
	ref := signSandboxRef(testRefKey, "user-1", "conv-1", "k3m9x0abcde")
	mac := ref[strings.LastIndex(ref, ".")+1:]
	flip := []byte(mac)
	flip[0] = 'A'
	if mac[0] == 'A' {
		flip[0] = 'B'
	}
	cases := map[string]struct{ subject, conv, ref string }{
		"other user":            {"user-2", "conv-1", ref},
		"no user":               {"", "conv-1", ref},
		"other conversation":    {"user-1", "conv-2", ref},
		"other sandbox":         {"user-1", "conv-1", strings.Replace(ref, "k3m9x0abcde", "k3m9x0abcdf", 1)},
		"tampered MAC":          {"user-1", "conv-1", "v1.k3m9x0abcde." + string(flip)},
		"other version":         {"user-1", "conv-1", "v2" + strings.TrimPrefix(ref, "v1")},
		"other key":             {"user-1", "conv-1", signSandboxRef(newRefKey("another secret, also long enough to count"), "user-1", "conv-1", "k3m9x0abcde")},
		"no MAC":                {"user-1", "conv-1", "v1.k3m9x0abcde"},
		"empty":                 {"user-1", "conv-1", ""},
		"a bare sandbox ID":     {"user-1", "conv-1", "k3m9x0abcde"},
		"an ID with a slash":    {"user-1", "conv-1", signSandboxRef(testRefKey, "user-1", "conv-1", "../admin")},
		"fields shifted":        {"user-1c", "onv-1", ref},
		"truncated MAC":         {"user-1", "conv-1", ref[:len(ref)-2]},
		"MAC with padding":      {"user-1", "conv-1", ref + "="},
		"trailing extra fields": {"user-1", "conv-1", ref + ".x"},
	}
	for name, c := range cases {
		if id, ok := verifySandboxRef(testRefKey, c.subject, c.conv, c.ref); ok {
			t.Errorf("%s: accepted, sandbox %q", name, id)
		}
	}
}

// fakeFeRun is ferund's tenant API as FeChat uses it: creates that mint
// the ID (and refuse to be given one, as ADR 0019 has it), tokens, and
// /mcp, whose one tool, whoami, answers the sandbox of the bearer token.
type fakeFeRun struct {
	srv *httptest.Server

	mu        sync.Mutex
	sandboxes map[string]bool   // existing sandboxes
	tokens    map[string]string // token -> sandbox
	creates   int
	mints     []string // the sandboxes a token was minted for
	noToken   bool     // a create answers without a token
	mintFails int      // status of every token mint, when set
	n         int
}

func newFakeFeRun(t *testing.T) *fakeFeRun {
	t.Helper()
	f := &fakeFeRun{sandboxes: map[string]bool{}, tokens: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "tenant", http.StatusUnauthorized)
			return
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, ok := req["sandboxId"]; ok {
			http.Error(w, `{"error":"sandboxId is minted by ferund"}`, http.StatusBadRequest)
			return
		}
		// Like ferund since ADR 0035: a sandbox is in an environment.
		if req["environment"] != "sandboxes" {
			http.Error(w, `{"error":"environment is required"}`, http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.creates++
		f.n++
		id := fmt.Sprintf("k%010d", f.n)
		f.sandboxes[id] = true
		token := ""
		if !f.noToken {
			token = f.tokenLocked(id)
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"sandboxId": id, "state": "pending"}, "token": token, "created": true})
	})
	mux.HandleFunc("POST /api/v1/sandboxes/{id}/tokens", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.mintFails != 0 {
			http.Error(w, "down", f.mintFails)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" || !f.sandboxes[id] {
			http.Error(w, `{"error":"unknown sandbox"}`, http.StatusNotFound)
			return
		}
		f.mints = append(f.mints, id)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": f.tokenLocked(id)})
	})
	whoami := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		f.mu.Lock()
		id := f.tokens[strings.TrimPrefix(req.Extra.Header.Get("Authorization"), "Bearer ")]
		f.mu.Unlock()
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: id}}}, nil
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-ferund", Version: "v1"}, nil)
	server.AddTool(&mcp.Tool{Name: "whoami", InputSchema: map[string]any{"type": "object"}}, whoami)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		_, ok := f.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "token", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFeRun) tokenLocked(id string) string {
	f.n++
	token := fmt.Sprintf("sbx-token-%d", f.n)
	f.tokens[token] = id
	return token
}

func (f *fakeFeRun) counts() (creates int, mints []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates, append([]string(nil), f.mints...)
}

// backend is a fresh backend process's sandbox client: no sessions yet.
func (f *fakeFeRun) backend(t *testing.T) *sandboxClient {
	t.Helper()
	c, err := newSandboxClient(f.srv.URL, "tok", "sandboxes", testRefKey)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var testTools = &toolset{known: map[string]bool{"whoami": true}, prepWait: time.Second}

// chat runs one request's tool calls (calls of them, in parallel) and
// returns the sandbox they ran in and the references the request handed
// out.
func chat(t *testing.T, c *sandboxClient, subject, conv, ref string, calls int) (string, []string) {
	t.Helper()
	var mu sync.Mutex
	var refs []string
	b := &chatSandbox{key: convKey{subject, conv}, ref: ref, onRef: func(r string) {
		mu.Lock()
		refs = append(refs, r)
		mu.Unlock()
	}}
	outs := make([]string, calls)
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i] = c.executeSandboxTool(testTools, b, "whoami", "{}")
		}()
	}
	wg.Wait()
	for _, o := range outs[1:] {
		if o != outs[0] {
			t.Fatalf("one request's tool calls ran in different sandboxes: %q", outs)
		}
	}
	return outs[0], refs
}

// A conversation gets a sandbox whose ID ferund mints, and a reference to
// it; its next requests, with the reference, stay in it, after a restart
// of the backend too, without a second sandbox.
func TestConversationKeepsItsSandbox(t *testing.T) {
	f := newFakeFeRun(t)
	c := f.backend(t)
	const user, conv = "user-1", "conv-a"

	id, refs := chat(t, c, user, conv, "", 4)
	if creates, _ := f.counts(); creates != 1 || !validSandboxID(id) || strings.HasPrefix(id, "error") {
		t.Fatalf("first request: sandbox %q, %d creates", id, creates)
	}
	if len(refs) != 1 {
		t.Fatalf("first request handed out %d references, want 1: %q", len(refs), refs)
	}
	ref := refs[0]
	if got, ok := verifySandboxRef(testRefKey, user, conv, ref); !ok || got != id {
		t.Fatalf("reference %q is not for sandbox %s", ref, id)
	}

	// the next request, same backend: the session, no new reference
	if again, refs := chat(t, c, user, conv, ref, 2); again != id || len(refs) != 0 {
		t.Fatalf("second request: sandbox %q (want %s), references %q", again, id, refs)
	}
	// a request whose reference got lost: the backend still knows the
	// conversation, and hands the reference out again
	if again, refs := chat(t, c, user, conv, "", 1); again != id || len(refs) != 1 || refs[0] != ref {
		t.Fatalf("request without its reference: sandbox %q, references %q", again, refs)
	}

	// the backend restarts: the reference brings the sandbox back, with a
	// new token
	c = f.backend(t)
	if again, refs := chat(t, c, user, conv, ref, 3); again != id || len(refs) != 0 {
		t.Fatalf("after a restart: sandbox %q (want %s), references %q", again, id, refs)
	}
	creates, mints := f.counts()
	if creates != 1 || len(mints) != 1 || mints[0] != id {
		t.Fatalf("after a restart: %d creates, tokens minted for %q", creates, mints)
	}
}

// A reference that is not this user's and this conversation's is not
// followed: the conversation gets a sandbox of its own, and nobody else's
// sandbox gets a token.
func TestForeignReferenceIsNotFollowed(t *testing.T) {
	f := newFakeFeRun(t)
	theirs, refs := chat(t, f.backend(t), "user-1", "conv-a", "", 1)
	if len(refs) != 1 {
		t.Fatalf("references %q", refs)
	}
	stolen := refs[0]
	tampered := strings.Replace(stolen, theirs, "k9999999999", 1)
	for name, c := range map[string]struct{ subject, conv, ref string }{
		"other user":         {"user-2", "conv-a", stolen},
		"other conversation": {"user-1", "conv-b", stolen},
		"tampered":           {"user-1", "conv-c", tampered},
	} {
		got, refs := chat(t, f.backend(t), c.subject, c.conv, c.ref, 1)
		if got == theirs || len(refs) != 1 || refs[0] == c.ref {
			t.Errorf("%s: sandbox %q (the other's: %s), references %q", name, got, theirs, refs)
		}
	}
	if _, mints := f.counts(); len(mints) != 0 {
		t.Fatalf("tokens minted for %q", mints)
	}
}

// A reference whose sandbox is gone gets a new sandbox and a new
// reference.
func TestReferenceToAGoneSandbox(t *testing.T) {
	f := newFakeFeRun(t)
	id, refs := chat(t, f.backend(t), "", "conv-a", "", 1)
	f.mu.Lock()
	delete(f.sandboxes, id)
	f.mu.Unlock()
	again, newRefs := chat(t, f.backend(t), "", "conv-a", refs[0], 1)
	if again == id || !validSandboxID(again) || len(newRefs) != 1 {
		t.Fatalf("gone sandbox %s: now %q, references %q", id, again, newRefs)
	}
	if got, ok := verifySandboxRef(testRefKey, "", "conv-a", newRefs[0]); !ok || got != again {
		t.Fatalf("new reference %q", newRefs[0])
	}
}

// ferund failing is no reason to make another sandbox: the tool call
// fails, and the conversation keeps its reference.
func TestFailingFeRunMakesNoSecondSandbox(t *testing.T) {
	f := newFakeFeRun(t)
	_, refs := chat(t, f.backend(t), "", "conv-a", "", 1)
	f.mu.Lock()
	f.mintFails = http.StatusServiceUnavailable
	f.mu.Unlock()
	out, newRefs := chat(t, f.backend(t), "", "conv-a", refs[0], 1)
	if creates, _ := f.counts(); creates != 1 || !strings.HasPrefix(out, "error: sandbox provisioning") || len(newRefs) != 0 {
		t.Fatalf("%d creates, answer %q, references %q", creates, out, newRefs)
	}
}

// A create that answers no token: one is minted.
func TestCreateWithoutToken(t *testing.T) {
	f := newFakeFeRun(t)
	f.noToken = true
	id, refs := chat(t, f.backend(t), "", "conv-a", "", 1)
	if _, mints := f.counts(); !validSandboxID(id) || len(refs) != 1 || len(mints) != 1 || mints[0] != id {
		t.Fatalf("sandbox %q, references %q, tokens minted for %q", id, refs, mints)
	}
}

// A conversation from before ADR 0019 has its UUID as its sandbox ID and
// no reference: it stays in its sandbox, and gets a reference to it.
func TestLegacyConversationKeepsItsSandbox(t *testing.T) {
	f := newFakeFeRun(t)
	const conv = "0194b6d2-7c1e-4a35-9f2b-1c2d3e4f5a6b"
	f.sandboxes[conv] = true
	id, refs := chat(t, f.backend(t), "user-1", conv, "", 1)
	if creates, _ := f.counts(); id != conv || creates != 0 || len(refs) != 1 {
		t.Fatalf("legacy conversation: sandbox %q, %d creates, references %q", id, creates, refs)
	}
	if got, ok := verifySandboxRef(testRefKey, "user-1", conv, refs[0]); !ok || got != conv {
		t.Fatalf("reference %q", refs[0])
	}
	// a new conversation (a UUID without a sandbox) gets a new one
	other, _ := chat(t, f.backend(t), "user-1", "0194b6d2-7c1e-4a35-9f2b-ffffffffffff", "", 1)
	if creates, _ := f.counts(); other == conv || creates != 1 {
		t.Fatalf("new conversation: sandbox %q, %d creates", other, creates)
	}
}

// The chat route hands the reference out as a "sandbox" event and takes
// it back as sandbox_ref.
func TestChatHandsOutTheReference(t *testing.T) {
	f := newFakeFeRun(t)
	sb = f.backend(t)
	t.Cleanup(func() { sb = nil })
	sb.tools = testTools
	turns := 0
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turns++
		if turns%2 == 1 {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c0","function":{"name":"whoami","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		} else {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
		}
		fmt.Fprintf(w, "data: %s\n\n", DONE)
	}))
	t.Cleanup(llm.Close)
	cfg := config{BaseURL: llm.URL, Model: "m", SystemPromptFile: "/nonexistent"}

	request := func(ref string) (events []map[string]any) {
		body, _ := json.Marshal(map[string]any{
			"messages":        []Message{{Role: "user", Content: "who"}},
			"conversation_id": "conv-a",
			"sandbox_ref":     ref,
		})
		rec := httptest.NewRecorder()
		handleChat(cfg)(rec, httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(string(body))))
		for _, line := range strings.Split(rec.Body.String(), "\n") {
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				var ev map[string]any
				if err := json.Unmarshal([]byte(data), &ev); err != nil {
					t.Fatalf("sse %q: %v", data, err)
				}
				events = append(events, ev)
			}
		}
		return events
	}
	var ref, id string
	for _, ev := range request("") {
		switch ev["type"] {
		case "sandbox":
			ref, _ = ev["ref"].(string)
			if ev["conversation_id"] != "conv-a" {
				t.Fatalf("sandbox event %v", ev)
			}
		case "tool":
			id, _ = ev["output"].(string)
		}
	}
	if got, ok := verifySandboxRef(testRefKey, "", "conv-a", ref); !ok || got != id {
		t.Fatalf("sandbox event %q for sandbox %q", ref, id)
	}
	sb = f.backend(t) // a restart
	sb.tools = testTools
	for _, ev := range request(ref) {
		if ev["type"] == "sandbox" {
			t.Fatalf("a valid reference was handed out again: %v", ev)
		}
		if ev["type"] == "tool" && ev["output"] != id {
			t.Fatalf("after a restart the tool ran in %v, want %s", ev["output"], id)
		}
	}
	if creates, _ := f.counts(); creates != 1 {
		t.Fatalf("%d creates", creates)
	}
}
