// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// FeChat's backend: a chat proxy to an OpenAI-compatible LLM with tool
// calling, the tools running in a sandbox over FeRun's MCP endpoint, and
// SSE streaming to the frontend. Sandboxes are provisioned through
// ferund's REST API, which mints their IDs; the browser keeps a
// conversation's sandbox as a signed sandbox reference (sandboxref.go).
//
// Configuration from the environment (Kubernetes Secret and ConfigMap):
//
//	OPENAI_API_KEY     (Secret)    API key of the LLM gateway
//	OPENAI_BASE_URL    (Secret)    the gateway's OpenAI-compatible base URL, ending in /v1
//	MODEL              (ConfigMap) e.g. kimi-k3-sovereign
//	SYSTEM_PROMPT_FILE (ConfigMap volume) path of the system prompt; without
//	                   the file, defaultSystemPrompt
//	FERUN_URL          ferund's base URL, default http://ferund.ferun.svc.cluster.local:8100
//	FERUN_TOKEN        (Secret)    a FeRun tenant token, for provisioning
//	FERUN_ENVIRONMENT  the tenant's environment the sandboxes are in (FeRun
//	                   ADR 0035), by name or ID; required with FERUN_URL
//	OIDC_ISSUER        the IdP, https://id.fecode.dev/ (auth.go); unset, the
//	                   chat is open to everybody: for development only
//	OIDC_CLIENT_ID     (Secret)    FeChat's client there
//	OIDC_CLIENT_SECRET (Secret)
//	PUBLIC_URL         FeChat's own URL, https://chat.fecode.dev
//	SESSION_SECRET     (Secret)    at least 32 bytes; signs the session cookies
//	                   and the sandbox references
//	PORT               listen port, default 8080
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// defaultSystemPrompt is the system prompt where SYSTEM_PROMPT_FILE names
// no file, as on a platform that mounts none.
const defaultSystemPrompt = `You are FeChat, a helpful chat assistant - you have access to tools for a per-conversation Linux sandbox.

Whenever the user asks about files, directories, code, commands or anything else that lives in the sandbox, USE THE TOOLS. Never describe, quote or invent command output or file content you have not actually retrieved with a tool call in this conversation. If information is missing, call a tool - do not guess.

Tools: bash (run any shell command), read / write / edit (file contents), ls (list directories), rg (search file contents), fd (find files by name), webfetch (fetch a URL and get its main readable content - use it for documentation, articles and research).

The sandbox image is provided by the customer, so check what's available before relying on specific tooling. Working directory is /workspace. Answer concisely, English by default.`

// vicePromptNote joins the system prompt while the SPA shows its Vice City
// theme (the bolt in the logo switches it): flavour only, never at the cost
// of the answer.
const vicePromptNote = `The user has switched FeChat to its Vice City theme, after Grand Theft Auto VI (state of Leonida, Vice City, out 19 November 2026). Keep every answer exactly as correct, complete and concise as without it, but you may give it a light Vice City touch: talk about a task like a crew planning a job, a wink at neon, sunsets, palm trees, Florida-Man headlines or the game's satire - one short phrase, not more. Code, commands, file contents and facts stay untouched. Never encourage real crime.`

// systemPrompt is the conversation's system prompt: the base, and the
// note of the theme the SPA shows, if it has one.
func systemPrompt(base, theme string) string {
	if theme == "gta" {
		return base + "\n\n" + vicePromptNote
	}
	return base
}

type config struct {
	APIKey           string
	BaseURL          string
	Model            string
	SystemPromptFile string
	Port             string
	FeRunURL         string // ferund's base URL; empty disables the sandbox tools
	FeRunToken       string // FeRun tenant token, for provisioning
	FeRunEnvironment string // the environment the sandboxes are in
	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	PublicURL        string
	SessionSecret    string
}

func loadConfig() config {
	return config{
		APIKey:           os.Getenv("OPENAI_API_KEY"),
		BaseURL:          strings.TrimSuffix(os.Getenv("OPENAI_BASE_URL"), "/"),
		Model:            os.Getenv("MODEL"),
		SystemPromptFile: getenv("SYSTEM_PROMPT_FILE", "/etc/fechat/system-prompt.txt"),
		Port:             getenv("PORT", "8080"),
		FeRunURL:         getenv("FERUN_URL", "http://ferund.ferun.svc.cluster.local:8100"),
		FeRunToken:       os.Getenv("FERUN_TOKEN"),
		FeRunEnvironment: os.Getenv("FERUN_ENVIRONMENT"),
		OIDCIssuer:       os.Getenv("OIDC_ISSUER"),
		OIDCClientID:     os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
		PublicURL:        os.Getenv("PUBLIC_URL"),
		SessionSecret:    os.Getenv("SESSION_SECRET"),
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// --- OpenAI API types ---

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type Tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
	} `json:"function"`
}

const maxChatBody = 32 << 20 // request limit of /api/v1/chat

// sb is the client of ferund (nil when the sandbox tools are disabled).
var sb *sandboxClient

func executeTool(ctx context.Context, ts *toolset, b *chatSandbox, callID, name, argsJSON string) string {
	ctx, span := tracer.Start(ctx, "execute_tool "+name, trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", name),
		attribute.String("gen_ai.tool.call.id", callID),
		attribute.String("fechat.conversation.id", b.key.conv)))
	defer span.End()
	out := `{"error":"unknown tool"}`
	if ts.has(name) {
		out = sb.executeSandboxTool(ctx, ts, b, name, argsJSON)
	}
	if strings.HasPrefix(out, "error: ") || out == `{"error":"unknown tool"}` {
		span.SetStatus(codes.Error, firstLine(out))
	}
	return out
}

// firstLine is s up to its first line break, at most 200 bytes: a span's
// status, not the whole output.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// --- The LLM's chat stream ---

type streamResult struct {
	Deltas      chan string
	Content     string // all content deltas of the round, for the history
	ToolCalls   []ToolCall
	FinishTools bool
	Err         error
}

// llmClient calls the LLM gateway: a client span per request, and the
// trace goes along.
var llmClient = &http.Client{Transport: tracedTransport(http.DefaultTransport)}

// callLLM starts a streamed chat completion. Content deltas go out on the
// channel, tool calls are accumulated.
//
// Each call is a span in the GenAI conventions (gen_ai.*): the model,
// the reason the answer ended and the tokens, which the gateway reports in
// the stream's last chunk (stream_options.include_usage).
func callLLM(ctx context.Context, cfg config, messages []Message, tools []Tool) *streamResult {
	res := &streamResult{Deltas: make(chan string, 64)}
	ctx, span := tracer.Start(ctx, "chat "+cfg.Model, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.String("gen_ai.provider.name", "openai"),
			attribute.String("gen_ai.request.model", cfg.Model),
			attribute.Int("fechat.messages", len(messages)),
			attribute.Int("fechat.tools", len(tools))))

	go func() {
		defer close(res.Deltas)
		defer func() {
			if res.Err != nil {
				span.RecordError(res.Err)
				span.SetStatus(codes.Error, firstLine(res.Err.Error()))
			}
			span.End()
		}()

		body := map[string]any{
			"model":          cfg.Model,
			"messages":       messages,
			"tools":          tools,
			"tool_choice":    "auto",
			"stream":         true,
			"stream_options": map[string]bool{"include_usage": true},
		}
		payload, _ := json.Marshal(body)

		req, err := http.NewRequestWithContext(ctx, "POST", cfg.BaseURL+"/chat/completions", bytes.NewReader(payload))
		if err != nil {
			res.Err = err
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		req.Header.Set("Accept", "text/event-stream")

		resp, err := llmClient.Do(req)
		if err != nil {
			res.Err = err
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			res.Err = fmt.Errorf("LLM gateway: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
			return
		}
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		type deltaTC struct {
			Index    int    `json:"index"`
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		var tcs []ToolCall
		var finish string
		var content strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(line[5:])
			if data == DONE {
				break
			}
			var chunk struct {
				Model string `json:"model"`
				Usage *struct {
					Prompt     int `json:"prompt_tokens"`
					Completion int `json:"completion_tokens"`
				} `json:"usage"`
				Choices []struct {
					Delta struct {
						Content   string    `json:"content"`
						ToolCalls []deltaTC `json:"tool_calls"`
					} `json:"delta"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}
			if chunk.Model != "" {
				span.SetAttributes(attribute.String("gen_ai.response.model", chunk.Model))
			}
			if chunk.Usage != nil {
				span.SetAttributes(
					attribute.Int("gen_ai.usage.input_tokens", chunk.Usage.Prompt),
					attribute.Int("gen_ai.usage.output_tokens", chunk.Usage.Completion))
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			c := chunk.Choices[0]
			if c.Delta.Content != "" {
				content.WriteString(c.Delta.Content)
				res.Deltas <- c.Delta.Content
			}
			for _, d := range c.Delta.ToolCalls {
				for len(tcs) <= d.Index {
					var tc ToolCall
					tc.Type = "function"
					tcs = append(tcs, tc)
				}
				if d.ID != "" {
					tcs[d.Index].ID = d.ID
				}
				if d.Function.Name != "" {
					tcs[d.Index].Function.Name = d.Function.Name
				}
				tcs[d.Index].Function.Arguments += d.Function.Arguments
			}
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
		}
		if err := scanner.Err(); err != nil {
			res.Err = err
			return
		}
		res.Content = content.String()
		res.ToolCalls = tcs
		span.SetAttributes(
			attribute.StringSlice("gen_ai.response.finish_reasons", []string{finish}),
			attribute.Int("fechat.tool_calls", len(tcs)))
		res.FinishTools = finish == "tool_calls"
	}()
	return res
}

// DONE ends the LLM gateway's SSE stream.
var DONE = string([]byte{91, 68, 79, 78, 69, 93})

// sseEvent writes one SSE event to the client.
func sseEvent(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func handleChat(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Messages       []Message `json:"messages"`
			ConversationID string    `json:"conversation_id"`
			// The conversation's sandbox image. Only the value at the
			// sandbox's creation counts; empty means ferund's default.
			Image string `json:"image"`
			// The conversation's sandbox reference, as the last "sandbox"
			// event handed it out; empty for a new conversation.
			SandboxRef string `json:"sandbox_ref"`
			// The SPA's theme: "gta" adds vicePromptNote, anything else
			// nothing.
			Theme string `json:"theme"`
		}
		// The history carries the tool outputs (each up to maxToolOutput),
		// so it is much larger than the text of the conversation.
		err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChatBody)).Decode(&req)
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "conversation too large", http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil || len(req.Messages) == 0 || validImageRef(req.Image) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		promptBytes, err := os.ReadFile(cfg.SystemPromptFile)
		prompt := strings.TrimSpace(string(promptBytes))
		if err != nil || prompt == "" {
			prompt = defaultSystemPrompt
		}
		prompt = systemPrompt(prompt, req.Theme)

		messages := append([]Message{{Role: "system", Content: prompt}}, req.Messages...)
		if req.ConversationID == "" {
			req.ConversationID = r.Header.Get("X-Conversation-Id")
		}
		if req.ConversationID == "" {
			req.ConversationID = newUUID()
			log.Printf("no conversation_id -> anonymous conversation %s", req.ConversationID)
		}
		log.Printf("chat request conversation=%s messages=%d", req.ConversationID, len(req.Messages))
		trace.SpanFromContext(r.Context()).SetAttributes(
			attribute.String("fechat.conversation.id", req.ConversationID),
			attribute.Int("fechat.messages", len(req.Messages)))
		// sseMu writes SSE frames to the stream one at a time, while tool
		// calls run in parallel.
		var sseMu sync.Mutex
		u, _ := userFrom(r.Context()) // no sign-in (development): no subject
		box := &chatSandbox{
			key:   convKey{subject: u.Subject, conv: req.ConversationID},
			ref:   req.SandboxRef,
			image: req.Image,
			// The browser keeps the reference with the conversation and
			// sends it back with the next request.
			onRef: func(ref string) {
				sseMu.Lock()
				defer sseMu.Unlock()
				sseEvent(w, map[string]string{"type": "sandbox", "conversation_id": req.ConversationID, "ref": ref})
			},
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		// The tools come from ferund's GET /api/v1/tools (no copies here),
		// loaded on the first message. While they cannot be loaded the
		// message fails: the system prompt promises these tools, and a model
		// offered none calls them anyway ("unknown tool").
		var ts *toolset
		var defs []Tool
		if sb != nil {
			t, err := sb.loadedTools(r.Context())
			if err != nil {
				log.Printf("tool list: %v", err)
				sseEvent(w, map[string]string{"type": "error", "content": "ferund unreachable: " + err.Error()})
				return
			}
			ts, defs = t, t.defs
		}
		// seq numbers the tool calls of the whole answer across all rounds;
		// tool_start and tool carry it, and the frontend matches a result to
		// its chip by it alone. The provider's id cannot do that: it is only
		// unique within a round, and our gateway hands out bash:0, bash:1,
		// ... again in every round.
		seq := 0
		for round := 0; round < 8; round++ {
			res := callLLM(r.Context(), cfg, messages, defs)
			for d := range res.Deltas {
				sseEvent(w, map[string]string{"type": "delta", "content": d})
			}
			if res.Err != nil {
				log.Printf("llm error: %v", res.Err)
				sseEvent(w, map[string]string{"type": "error", "content": "LLM error: " + res.Err.Error()})
				return
			}
			// history events: the backend is stateless and the client carries
			// the history, exactly these messages, unchanged. A client that
			// sends back only the answer text shows the model, from turn 2 on,
			// its own answers with results but not a single tool_call, and the
			// model learns from them to claim tool use instead of calling tools.
			if !res.FinishTools || len(res.ToolCalls) == 0 {
				if res.Content != "" {
					sseEvent(w, map[string]any{
						"type":     "history",
						"messages": []Message{{Role: "assistant", Content: res.Content}},
					})
				}
				break
			}
			roundStart := len(messages)
			messages = append(messages, Message{Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls})
			for i, tc := range res.ToolCalls {
				// announce the tool's start right away (the frontend shows it pending)
				sseEvent(w, map[string]any{
					"type":      "tool_start",
					"seq":       seq + i,
					"id":        tc.ID,
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				})
			}
			// Run all tool calls of the round in parallel; their results go
			// into the history in call order (tool_call_id).
			outputs := make([]string, len(res.ToolCalls))
			var wg sync.WaitGroup
			for i, tc := range res.ToolCalls {
				wg.Add(1)
				go func(i int, tc ToolCall) {
					defer wg.Done()
					output := executeTool(r.Context(), ts, box, tc.ID, tc.Function.Name, tc.Function.Arguments)
					outputs[i] = output
					sseMu.Lock()
					sseEvent(w, map[string]any{
						"type":      "tool",
						"seq":       seq + i,
						"id":        tc.ID,
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
						"output":    output,
					})
					sseMu.Unlock()
				}(i, tc)
			}
			wg.Wait()
			seq += len(res.ToolCalls)
			for i, tc := range res.ToolCalls {
				messages = append(messages, Message{
					Role:       "tool",
					ToolCallID: tc.ID,
					Name:       tc.Function.Name,
					Content:    outputs[i],
				})
			}
			// The round (the assistant's tool calls and all their results) goes
			// out as one event: only complete rounds are a valid history.
			sseEvent(w, map[string]any{"type": "history", "messages": messages[roundStart:]})
		}
		sseEvent(w, map[string]string{"type": "done"})
	}
}

func main() {
	cfg := loadConfig()
	// SIGTERM ends the server gracefully, and the last spans go out.
	stop, _ := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	flush := setupTracing(stop)
	if cfg.APIKey == "" {
		log.Fatal("OPENAI_API_KEY is not set")
	}
	if cfg.BaseURL == "" {
		log.Fatal("OPENAI_BASE_URL is not set")
	}
	if cfg.Model == "" {
		log.Fatal("MODEL is not set")
	}
	if cfg.FeRunURL != "" {
		if cfg.FeRunEnvironment == "" {
			log.Fatal("FERUN_ENVIRONMENT is not set: the environment the sandboxes are in")
		}
		c, err := newSandboxClient(cfg.FeRunURL, cfg.FeRunToken, cfg.FeRunEnvironment, newRefKey(cfg.SessionSecret))
		if err != nil {
			log.Fatalf("ferund: %v", err)
		}
		sb = c
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	chat, imageCheck := handleChat(cfg), handleImageCheck()
	if cfg.OIDCIssuer != "" {
		a, err := newAuth(cfg.OIDCIssuer, cfg.OIDCClientID, cfg.OIDCClientSecret, cfg.PublicURL, cfg.SessionSecret)
		if err != nil {
			log.Fatalf("sign-in: %v", err)
		}
		a.routes(mux)
		chat, imageCheck = a.require(chat), a.require(imageCheck)
	} else {
		log.Printf("OIDC_ISSUER is not set: the chat is open to everybody (for development only)")
	}
	mux.HandleFunc("/api/v1/chat", chat)
	mux.HandleFunc("/api/v1/image-check", imageCheck)
	log.Printf("fechat-backend listening on :%s (model=%s, llm=%s, ferun=%q)", cfg.Port, cfg.Model, cfg.BaseURL, cfg.FeRunURL)
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: tracedServer(mux)}
	go func() {
		<-stop.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
	flush()
}
