// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// fakeLLM plays an OpenAI-compatible gateway: round 1 answers with text
// and a tool call, every later round with text alone. It records the
// messages the backend sent it in each round.
func fakeLLM(t *testing.T, seen *[][]Message) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("llm request: %v", err)
		}
		*seen = append(*seen, body.Messages)
		chunk := func(delta, finish string) {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":%s,\"finish_reason\":%q}]}\n\n", delta, finish)
		}
		if len(*seen) == 1 {
			chunk(`{"content":"Let me "}`, "")
			chunk(`{"content":"check."}`, "")
			chunk(`{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":"{\"command\":"}}]}`, "")
			chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"\"uname -r\"}"}}]}`, "")
			chunk(`{}`, "tool_calls")
		} else {
			chunk(`{"content":"Kernel is 6.12."}`, "")
			chunk(`{}`, "stop")
		}
		fmt.Fprintf(w, "data: %s\n\n", DONE)
	}))
}

// chatTurn sends one request to handleChat and returns the messages of
// all history events in order: what a client sends back.
func chatTurn(t *testing.T, cfg config, messages []Message) []Message {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"messages": messages, "conversation_id": "c"})
	rec := httptest.NewRecorder()
	handleChat(cfg)(rec, httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(string(body))))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body)
	}
	var history []Message
	sc := bufio.NewScanner(rec.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type     string    `json:"type"`
			Content  string    `json:"content"`
			Messages []Message `json:"messages"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("sse %q: %v", data, err)
		}
		if ev.Type == "error" {
			t.Fatalf("error event: %s", ev.Content)
		}
		history = append(history, ev.Messages...)
	}
	return history
}

// A turn's history events, sent back unchanged by the client, must give
// the LLM exactly the messages the backend itself used within the turn:
// tool calls and tool results must not get lost between turns.
func TestHistoryRoundTrip(t *testing.T) {
	var seen [][]Message
	llm := fakeLLM(t, &seen)
	defer llm.Close()
	cfg := config{BaseURL: llm.URL, Model: "m", SystemPromptFile: "/nonexistent"}

	user1 := Message{Role: "user", Content: "which kernel?"}
	history := chatTurn(t, cfg, []Message{user1})

	var call ToolCall
	call.ID, call.Type = "call_1", "function"
	call.Function.Name, call.Function.Arguments = "bash", `{"command":"uname -r"}`
	want := []Message{
		{Role: "assistant", Content: "Let me check.", ToolCalls: []ToolCall{call}},
		{Role: "tool", ToolCallID: "call_1", Name: "bash", Content: `{"error":"unknown tool"}`},
		{Role: "assistant", Content: "Kernel is 6.12."},
	}
	if !reflect.DeepEqual(history, want) {
		t.Fatalf("history events:\n got %+v\nwant %+v", history, want)
	}

	// Turn 2: the client sends the history back.
	user2 := Message{Role: "user", Content: "and the date?"}
	replay := append(append([]Message{user1}, history...), user2)
	chatTurn(t, cfg, replay)

	if len(seen) != 3 {
		t.Fatalf("llm calls = %d, want 3", len(seen))
	}
	// seen[1] is round 2 of turn 1: system, user1, assistant+tool_calls, tool.
	if got, want := seen[1][2:], want[:2]; !reflect.DeepEqual(got, want) {
		t.Errorf("round 2 context:\n got %+v\nwant %+v", got, want)
	}
	if got := seen[2][1:]; !reflect.DeepEqual(got, replay) {
		t.Errorf("turn 2 context:\n got %+v\nwant %+v", got, replay)
	}
}

func TestChatBodyTooLarge(t *testing.T) {
	rec := httptest.NewRecorder()
	body := io.MultiReader(strings.NewReader(`{"messages":[{"role":"user","content":"`),
		strings.NewReader(strings.Repeat("x", maxChatBody)), strings.NewReader(`"}]}`))
	handleChat(config{})(rec, httptest.NewRequest(http.MethodPost, "/api/v1/chat", body))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("HTTP %d, want 413", rec.Code)
	}
}

// Our LLM gateway hands out the tool call ids again in every round
// (bash:0, bash:1, ...). The frontend therefore matches results by seq:
// every call of an answer has its own, and tool carries the same one as
// its tool_start.
func TestToolEventSeqAcrossRounds(t *testing.T) {
	rounds := 0
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rounds++
		chunk := func(delta, finish string) {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":%s,\"finish_reason\":%q}]}\n\n", delta, finish)
		}
		if rounds <= 2 {
			for i := range 2 {
				chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":"bash:%d","function":{"name":"bash","arguments":"{\"command\":\"echo r%d-%d\"}"}}]}`,
					i, i, rounds, i), "")
			}
			chunk(`{}`, "tool_calls")
		} else {
			chunk(`{"content":"done"}`, "stop")
		}
		fmt.Fprintf(w, "data: %s\n\n", DONE)
	}))
	defer llm.Close()
	cfg := config{BaseURL: llm.URL, Model: "m", SystemPromptFile: "/nonexistent"}

	body := `{"messages":[{"role":"user","content":"go"}],"conversation_id":"c"}`
	rec := httptest.NewRecorder()
	handleChat(cfg)(rec, httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(body)))

	started := map[int]string{} // seq -> arguments
	settled := map[int]string{}
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type      string `json:"type"`
			Seq       *int   `json:"seq"`
			Arguments string `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("sse %q: %v", data, err)
		}
		if ev.Type != "tool_start" && ev.Type != "tool" {
			continue
		}
		if ev.Seq == nil {
			t.Fatalf("%s event without seq: %s", ev.Type, data)
		}
		seen := started
		if ev.Type == "tool" {
			seen = settled
		}
		if _, dup := seen[*ev.Seq]; dup {
			t.Fatalf("%s seq %d twice", ev.Type, *ev.Seq)
		}
		seen[*ev.Seq] = ev.Arguments
	}
	want := map[int]string{
		0: `{"command":"echo r1-0"}`, 1: `{"command":"echo r1-1"}`,
		2: `{"command":"echo r2-0"}`, 3: `{"command":"echo r2-1"}`,
	}
	if !reflect.DeepEqual(started, want) {
		t.Errorf("tool_start by seq:\n got %v\nwant %v", started, want)
	}
	if !reflect.DeepEqual(settled, want) {
		t.Errorf("tool by seq:\n got %v\nwant %v", settled, want)
	}
}

// The Vice City theme adds its note to the system prompt; no theme and
// unknown ones leave the prompt as it is.
func TestSystemPromptTheme(t *testing.T) {
	if got := systemPrompt("base", "gta"); got != "base\n\n"+vicePromptNote {
		t.Errorf("gta: got %q", got)
	}
	for _, theme := range []string{"", "cyber", "GTA", "other"} {
		if got := systemPrompt("base", theme); got != "base" {
			t.Errorf("%q: got %q", theme, got)
		}
	}
}
