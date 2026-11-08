// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// toolsAPI serves GET /api/v1/tools the way ferund does, with body as the
// JSON answer.
func toolsAPI(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tools" || r.Header.Get("Authorization") != "Bearer tok" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A tool call waits as long as ferund says bringing a sandbox up may
// take, plus the tool's own run time. The 4 minutes the chat used to
// allow the whole call were below the control plane's 7-minute
// preparation budget: a cold pull longer than that was cancelled on every
// attempt and every retry started it over.
func TestToolCallWaitsForTheAnnouncedPreparation(t *testing.T) {
	const announced = 425_000 // a 7 min preparation budget + 5 s reply budget
	api := toolsAPI(t, `{"tools":[{"name":"bash","description":"run","inputSchema":{"type":"object"}}],`+
		`"preparationWaitMs":425000}`)
	c, err := newSandboxClient(api.URL, "tok", "sandboxes", testRefKey)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := c.sandboxTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ts.defs) != 1 || !ts.has("bash") {
		t.Fatalf("tools: %+v", ts.defs)
	}
	want := announced*time.Millisecond + toolRunWait
	if got := ts.callTimeout(); got != want {
		t.Fatalf("call timeout %s, want the announced preparation wait plus %s", got, toolRunWait)
	}
}

// No announcement, no guess: a ferund that does not announce it is
// refused, which the chat request reports as its error.
func TestToolsWithoutPreparationWaitAreRefused(t *testing.T) {
	api := toolsAPI(t, `{"tools":[{"name":"bash","description":"run","inputSchema":{"type":"object"}}]}`)
	c, err := newSandboxClient(api.URL, "tok", "sandboxes", testRefKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.sandboxTools(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "preparationWaitMs") {
		t.Fatalf("a ferund without preparationWaitMs was accepted: %v", err)
	}
}

// A ferund that is down when a chat request arrives fails that request
// with its error. The backend used to fetch the tool list once at startup
// and, if the control plane was down then, run without tools for its
// whole lifetime, while the system prompt still promised bash, so the
// model called it anyway and got "unknown tool" on every call.
func TestChatFailsWhileFeRunIsDown(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	c, err := newSandboxClient(down.URL, "tok", "sandboxes", testRefKey)
	if err != nil {
		t.Fatal(err)
	}
	sb = c
	t.Cleanup(func() { sb = nil })

	rec := httptest.NewRecorder()
	handleChat(config{SystemPromptFile: "/nonexistent"})(rec, httptest.NewRequest(http.MethodPost,
		"/api/v1/chat", strings.NewReader(`{"messages":[{"role":"user","content":"ping 1.1.1.1"}]}`)))
	if body := rec.Body.String(); !strings.Contains(body, `"type":"error"`) ||
		!strings.Contains(body, "ferund unreachable") {
		t.Fatalf("chat answered without a ferund: %s", body)
	}
}

// The tool list is loaded on the first message that can reach ferund
// and kept from then on: a failed load is not remembered.
func TestToolListLoadsOnceTheAPIIsUp(t *testing.T) {
	up := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"tools":[{"name":"bash","description":"run","inputSchema":{"type":"object"}}],"preparationWaitMs":1000}`))
	}))
	t.Cleanup(api.Close)
	c, err := newSandboxClient(api.URL, "tok", "sandboxes", testRefKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.loadedTools(context.Background()); err == nil {
		t.Fatal("tool list loaded from a ferund that is down")
	}
	up = true
	if ts, err := c.loadedTools(context.Background()); err != nil || !ts.has("bash") {
		t.Fatalf("ferund up, tool list not loaded: %v", err)
	}
	up = false
	if ts, err := c.loadedTools(context.Background()); err != nil || !ts.has("bash") {
		t.Fatalf("loaded tool list was not kept: %v", err)
	}
}
