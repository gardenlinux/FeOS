// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/model"
)

// maxRequestBody caps what a tool route reads of a JSON request, which
// may be larger than the call it makes (escapes): it bounds a stuck or
// hostile client, and the call's own bound is the guest's
// (maxGuestRequest). A file write's body is the content and is read up to
// that bound. A larger body is refused, never cut.
const maxRequestBody = 32 << 20

// retryAfter is the Retry-After of a tool route's 502 for a cause worth
// retrying that says no time of its own.
const retryAfter = 5

// The headers of the calls with an id (ADR 0023): the request's id, and
// the answer's state and replay.
const (
	idempotencyHeader  = "Idempotency-Key"
	callStateHeader    = "Ferun-Call-State"
	callReplayedHeader = "Ferun-Call-Replayed"
)

// msgBadCallKey is the text of a 400 for a call id outside the alphabet or
// the length.
const msgBadCallKey = "Idempotency-Key must be 1 to 255 characters of A-Z a-z 0-9 . _ ~ : @ -"

// REST serves the tool baseline over REST: each handler fills a tool's
// parameters from its request and makes the same call that MCP makes
// (CallKeyed). The routes are of the sandbox class: the API has
// authenticated the sandbox's token and put the sandbox into the
// request's context.
//
// A tool that ran is a 200 whatever its exit code, which is a field of the
// answer: {"exitCode", "output", "truncated"?, "image"?}. A call that
// failed, the sandbox not prepared or the node not answering, is a 502
// {"error": "<tool>: <err>"}, with a Retry-After when a retry is worth it
// (Retryable): the time the error asks for (apierr.RetryAfterOf; 60 for a
// hibernation in flight), else 5. These routes map no other cause of a
// failed call to another status. A bad request is a 400. A body larger
// than 32 MiB is a 413, and so is a write, an edit or a command larger
// than the guest takes (TooLargeError), with its limit in the text. A
// client that hung up gets nothing.
//
// Every tool route takes the call's id in the Idempotency-Key header
// (ADR 0023): one value of 1 to 255 characters of A-Z a-z 0-9 . _ ~ : @ -,
// bare or as one quoted sf-string, else a 400 bad_idempotency_key. It is
// honoured on the tools with a side effect (exec, PUT and PATCH of a file,
// POST of a process, DELETE of a process) and ignored on the others. The
// answer of a call with an id carries Ferun-Call-State (done,
// interrupted, not_run, running), and Ferun-Call-Replayed: true when it
// is another request's call, attached to or read from its record. Beside
// the answers above, such a call is:
//   - 422 call_id_conflict: the id names another call in the sandbox;
//   - 400 call_timeout_too_long: a bash timeout over MaxKeyedTimeout;
//   - 503 call_running, Retry-After 1: the call runs on another instance
//     and this one stops (ask again);
//   - 502 call_interrupted, without Retry-After: the call may have run and
//     stopped without a result;
//   - 502 without Retry-After when it ran (done) and failed; a call that
//     did not run (not_run) keeps the Retry-After rule above.
//
// A concurrent request with the same id attaches to the call, where the
// IETF Idempotency-Key draft answers 409. GET …/calls/{key} reads a
// call's record with a wait (Call), DELETE …/calls/{key} cancels it
// (CancelCall).
type REST struct {
	r *Runner
}

// NewREST returns the REST handlers of r.
func NewREST(r *Runner) *REST { return &REST{r: r} }

// toolImage is a picture in a tool route's answer; data is base64.
type toolImage struct {
	MediaType string `json:"mediaType"`
	Data      []byte `json:"data"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

// toolResult is a tool route's answer. The exit code is a field rather than
// a line of the text, as MCP has it: the caller here is a program. The
// output is cut at MaxToolOutput bytes without a marker; truncated says so.
type toolResult struct {
	ExitCode  int        `json:"exitCode"`
	Output    string     `json:"output"`
	Truncated bool       `json:"truncated,omitempty"`
	Image     *toolImage `json:"image,omitempty"`
}

// IdempotencyKey is the request's call id (and a project's job's create key): its Idempotency-Key header,
// trimmed, with one pair of surrounding quotes taken off (an RFC 8941
// sf-string, which holds no escape in this alphabet), or "" without the
// header. More than one value, or one that is not ValidCallKey, is a 400
// bad_idempotency_key.
func IdempotencyKey(r *http.Request) (string, error) {
	vs := r.Header.Values(idempotencyHeader)
	switch len(vs) {
	case 0:
		return "", nil
	case 1:
	default:
		return "", apierr.WithCode(http.StatusBadRequest, "bad_idempotency_key", "one Idempotency-Key, got %d", len(vs))
	}
	key := strings.TrimSpace(vs[0])
	if len(key) > 1 && key[0] == '"' && key[len(key)-1] == '"' {
		key = key[1 : len(key)-1]
	}
	if !ValidCallKey(key) {
		return "", apierr.WithCode(http.StatusBadRequest, "bad_idempotency_key", msgBadCallKey)
	}
	return key, nil
}

// run makes the call of tool with params (one of the parameter structs),
// with the request's call id, and answers it.
func (h *REST) run(w http.ResponseWriter, r *http.Request, tool string, params any) {
	key, err := IdempotencyKey(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	args, err := json.Marshal(params)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	h.answer(w, r, tool, func(ctx context.Context, sb auth.Sandbox) (Result, CallInfo, error) {
		return h.r.CallKeyed(ctx, sb, tool, args, key, nil)
	})
}

// answer runs call, a call of tool, for the request's sandbox and answers
// it, with the headers of a call with an id when it is one.
func (h *REST) answer(w http.ResponseWriter, r *http.Request, tool string,
	call func(ctx context.Context, sb auth.Sandbox) (Result, CallInfo, error)) {
	sb, ok := auth.SandboxFrom(r.Context())
	if !ok {
		apierr.Write(w, apierr.New(http.StatusUnauthorized, "sandbox token required"))
		return
	}
	res, info, err := call(r.Context(), sb)
	if r.Context().Err() != nil {
		// The client hung up. A call without an id was cancelled with its
		// request; one with an id runs on, and its record tells.
		return
	}
	if info.Keyed && info.State != "" {
		w.Header().Set(callStateHeader, info.State)
		if info.Replay {
			w.Header().Set(callReplayedHeader, "true")
		}
	}
	var (
		conflict *CallIDConflictError
		tooLong  *KeyedTimeoutError
		askAgain *AskAgainError
		tooLarge *TooLargeError
	)
	switch {
	case errors.As(err, &conflict):
		apierr.Write(w, apierr.WithCode(http.StatusUnprocessableEntity, "call_id_conflict", "%s: %v", tool, err))
		return
	case errors.As(err, &tooLong):
		apierr.Write(w, apierr.WithCode(http.StatusBadRequest, "call_timeout_too_long", "%s: %v", tool, err))
		return
	case errors.Is(err, ErrBadCallKey):
		apierr.Write(w, apierr.WithCode(http.StatusBadRequest, "bad_idempotency_key", msgBadCallKey))
		return
	case errors.As(err, &askAgain):
		apierr.Write(w, apierr.WithCode(http.StatusServiceUnavailable, "call_running", "%s: %v", tool, err).RetryAfter(1))
		return
	case errors.As(err, &tooLarge):
		apierr.Write(w, apierr.New(http.StatusRequestEntityTooLarge, "%s: %v", tool, err))
		return
	case errors.Is(err, model.ErrExpired):
		apierr.Write(w, apierr.WithCode(http.StatusGone, apierr.CodeExpired, "%s: %v", tool, err))
		return
	case err != nil:
		e := apierr.New(http.StatusBadGateway, "%s: %v", tool, err)
		switch {
		case info.State == CallInterrupted:
			// It may have run: a retry is the client's decision, after a
			// look at the sandbox.
			e.Code = "call_interrupted"
		case info.State == CallDone:
			// It ran, and failed: the same id answers the same again.
		case Retryable(err):
			e.RetryAfter(apierr.RetryAfterOf(err, retryAfter))
		}
		apierr.Write(w, e)
		return
	}
	out := toolResult{ExitCode: res.ExitCode, Output: res.Text, Truncated: res.Truncated}
	if img := res.Image; img != nil {
		out.Image = &toolImage{MediaType: img.MediaType, Data: img.Data, Width: img.Width, Height: img.Height}
	}
	apierr.WriteJSON(w, http.StatusOK, out)
}

// Exec is POST /api/v1/sandboxes/{id}/exec {command, timeout?}: bash.
func (h *REST) Exec(w http.ResponseWriter, r *http.Request) {
	var p BashParams
	if _, err := decodeJSON(r, &p); err != nil {
		apierr.Write(w, err)
		return
	}
	if p.Command == "" {
		apierr.Write(w, apierr.New(http.StatusBadRequest, "command is required"))
		return
	}
	h.run(w, r, ToolBash, p)
}

// toolPath is the container path a /files/ or /dirs/ URL names: its tail
// with the leading "/" put back. A URL cannot carry the double slash an
// absolute path written verbatim would need, so the tail is taken as
// rooted; an empty tail is the root.
func toolPath(r *http.Request) string { return "/" + r.PathValue("path") }

// ReadFile is GET /api/v1/sandboxes/{id}/files/{path...}?offset=&limit=:
// read.
func (h *REST) ReadFile(w http.ResponseWriter, r *http.Request) {
	p := ReadParams{Path: toolPath(r)}
	if p.Path == "/" {
		apierr.Write(w, apierr.New(http.StatusBadRequest, "path is required"))
		return
	}
	var err error
	if p.Offset, err = queryCount(r, "offset"); err != nil {
		apierr.Write(w, err)
		return
	}
	if p.Limit, err = queryCount(r, "limit"); err != nil {
		apierr.Write(w, err)
		return
	}
	h.run(w, r, ToolRead, p)
}

// WriteFile is PUT /api/v1/sandboxes/{id}/files/{path...}: write, with the
// request body as the file's content, byte for byte: it never becomes the
// JSON string of the tool's arguments, which would turn bytes that are not
// UTF-8 into U+FFFD.
func (h *REST) WriteFile(w http.ResponseWriter, r *http.Request) {
	path := toolPath(r)
	if path == "/" {
		apierr.Write(w, apierr.New(http.StatusBadRequest, "path is required"))
		return
	}
	key, err := IdempotencyKey(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	body, err := readAtMost(r, maxGuestRequest, func() error {
		return apierr.New(http.StatusRequestEntityTooLarge, "%s: %v", ToolWrite, &TooLargeError{Tool: ToolWrite})
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	h.answer(w, r, ToolWrite, func(ctx context.Context, sb auth.Sandbox) (Result, CallInfo, error) {
		return h.r.writeFileKeyed(ctx, sb, path, body, key)
	})
}

// EditFile is PATCH /api/v1/sandboxes/{id}/files/{path...} {"edits":[{old,
// new}]}: edit.
func (h *REST) EditFile(w http.ResponseWriter, r *http.Request) {
	var p EditParams
	if _, err := decodeJSON(r, &p); err != nil {
		apierr.Write(w, err)
		return
	}
	p.Path = toolPath(r)
	if p.Path == "/" {
		apierr.Write(w, apierr.New(http.StatusBadRequest, "path is required"))
		return
	}
	if len(p.Edits) == 0 {
		apierr.Write(w, apierr.New(http.StatusBadRequest, "edits is required and must not be empty"))
		return
	}
	for i, e := range p.Edits {
		if e.Old == "" {
			apierr.Write(w, apierr.New(http.StatusBadRequest, "edits[%d].old is required", i))
			return
		}
	}
	h.run(w, r, ToolEdit, p)
}

// Dirs is GET /api/v1/sandboxes/{id}/dirs/{path...}?limit=: ls. An empty
// tail lists the root.
func (h *REST) Dirs(w http.ResponseWriter, r *http.Request) {
	p := LsParams{Path: toolPath(r)}
	var err error
	if p.Limit, err = queryCount(r, "limit"); err != nil {
		apierr.Write(w, err)
		return
	}
	h.run(w, r, ToolLs, p)
}

// Search is GET /api/v1/sandboxes/{id}/search?q=&kind=content|name&path=
// &glob=&ignoreCase=&literal=&context=&limit=: rg for content (the
// default), fd for names. The path is taken as written, not rooted.
func (h *REST) Search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pattern := q.Get("q")
	if pattern == "" {
		apierr.Write(w, apierr.New(http.StatusBadRequest, "q is required"))
		return
	}
	limit, err := queryCount(r, "limit")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	switch kind := q.Get("kind"); kind {
	case "", "content":
		p := RgParams{Pattern: pattern, Path: q.Get("path"), Glob: q.Get("glob"), Limit: limit}
		if p.IgnoreCase, err = queryBool(r, "ignoreCase"); err != nil {
			apierr.Write(w, err)
			return
		}
		if p.Literal, err = queryBool(r, "literal"); err != nil {
			apierr.Write(w, err)
			return
		}
		if p.Context, err = queryCount(r, "context"); err != nil {
			apierr.Write(w, err)
			return
		}
		h.run(w, r, ToolRg, p)
	case "name":
		// fd's glob is a mode, rg's a file filter: the kind decides which.
		p := FdParams{Pattern: pattern, Path: q.Get("path"), Limit: limit}
		if p.Glob, err = queryBool(r, "glob"); err != nil {
			apierr.Write(w, err)
			return
		}
		h.run(w, r, ToolFd, p)
	default:
		apierr.Write(w, apierr.New(http.StatusBadRequest, "kind must be content or name, got %q", kind))
	}
}

// Fetch is POST /api/v1/sandboxes/{id}/fetch {url}: webfetch.
func (h *REST) Fetch(w http.ResponseWriter, r *http.Request) {
	var p WebFetchParams
	if _, err := decodeJSON(r, &p); err != nil {
		apierr.Write(w, err)
		return
	}
	if p.URL == "" {
		apierr.Write(w, apierr.New(http.StatusBadRequest, "url is required"))
		return
	}
	h.run(w, r, ToolWebfetch, p)
}

// StartProcess is POST /api/v1/sandboxes/{id}/processes {command, timeout,
// name?, cwd?}: process_start. timeout is required; 0 is one of its values
// (no limit), so its absence is told from it.
func (h *REST) StartProcess(w http.ResponseWriter, r *http.Request) {
	var p ProcessStartParams
	sent, err := decodeJSON(r, &p)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if p.Command == "" {
		apierr.Write(w, apierr.New(http.StatusBadRequest, "command is required"))
		return
	}
	if _, ok := sent["timeout"]; !ok {
		apierr.Write(w, apierr.New(http.StatusBadRequest,
			"timeout is required (seconds the process may run; 0 = no limit)"))
		return
	}
	h.run(w, r, ToolProcessStart, p)
}

// ListProcesses is GET /api/v1/sandboxes/{id}/processes: process_list.
func (h *REST) ListProcesses(w http.ResponseWriter, r *http.Request) {
	h.run(w, r, ToolProcessList, struct{}{})
}

// ProcessLogs is GET /api/v1/sandboxes/{id}/processes/{pid}/logs?lines=:
// process_logs.
func (h *REST) ProcessLogs(w http.ResponseWriter, r *http.Request) {
	p := ProcessLogsParams{ID: r.PathValue("pid")}
	var err error
	if p.Lines, err = queryInt(r, "lines"); err != nil {
		apierr.Write(w, err)
		return
	}
	h.run(w, r, ToolProcessLogs, p)
}

// StopProcess is DELETE /api/v1/sandboxes/{id}/processes/{pid}:
// process_stop.
func (h *REST) StopProcess(w http.ResponseWriter, r *http.Request) {
	h.run(w, r, ToolProcessStop, ProcessStopParams{ID: r.PathValue("pid")})
}

// callRecord is the answer of the record route: a call with an id, and,
// once it ended, its outcome.
type callRecord struct {
	Key       string `json:"key"`
	Tool      string `json:"tool"`
	State     string `json:"state"`
	Instance  string `json:"instance"`
	StartedAt string `json:"startedAt"`
	*callOutcome
}

// callOutcome is how a call with an id ended, in its record: its result
// and error as stored, and both as MCP renders them (RenderText), which
// a client that recovers the call shows in place of the live answer.
type callOutcome struct {
	FinishedAt string `json:"finishedAt"`
	ExitCode   int    `json:"exitCode"`
	Output     string `json:"output"`
	Truncated  bool   `json:"truncated"`
	IsError    bool   `json:"isError"`
	Error      string `json:"error"`
	Text       string `json:"text"`
}

// callKey is the call id a /calls/{key} URL names; one that is not
// ValidCallKey is a 400 bad_idempotency_key.
func callKey(r *http.Request) (string, error) {
	key := r.PathValue("key")
	if !ValidCallKey(key) {
		return "", apierr.WithCode(http.StatusBadRequest, "bad_idempotency_key", msgBadCallKey)
	}
	return key, nil
}

// Call is GET /api/v1/sandboxes/{id}/calls/{key}?wait=N: a keyed call's
// record (ADR 0023); it never runs anything.
//
// The record of a call that ended comes at once; of one that runs, when it
// ends or after N seconds (0 to 60, default 0) with state running, and at
// once on a stopping instance. It is {key, tool, state, instance,
// startedAt}, and once the call ended also {finishedAt, exitCode, output,
// truncated, isError, error, text}, where text and isError are MCP's
// rendering of the answer (RenderText). A key no call has (never made, or
// past its retention) is a 404 no_such_call. A wait while the sandbox has
// as many requests waiting for calls with an id on this instance as it
// may is a 502 with Retry-After (ErrCallCap). A client that hung up gets
// nothing.
func (h *REST) Call(w http.ResponseWriter, r *http.Request) {
	sb, ok := auth.SandboxFrom(r.Context())
	if !ok {
		apierr.Write(w, apierr.New(http.StatusUnauthorized, "sandbox token required"))
		return
	}
	key, err := callKey(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	wait, err := queryInt(r, "wait")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if most := int(maxRecordWait / time.Second); wait < 0 || wait > most {
		apierr.Write(w, apierr.New(http.StatusBadRequest, "wait must be 0 to %d seconds, got %d", most, wait))
		return
	}
	rec, err := h.r.WaitCall(r.Context(), sb.ID, key, time.Duration(wait)*time.Second)
	if r.Context().Err() != nil {
		return
	}
	switch {
	case errors.Is(err, ErrNoCall):
		apierr.Write(w, apierr.WithCode(http.StatusNotFound, "no_such_call", "no call with this id in the sandbox"))
		return
	case err != nil:
		apierr.Write(w, apierr.New(http.StatusBadGateway, "the call's record: %v", err).RetryAfter(retryAfter))
		return
	}
	out := callRecord{Key: rec.Key, Tool: rec.Tool, State: rec.State, Instance: rec.Instance,
		StartedAt: rec.StartedAt.UTC().Format(time.RFC3339Nano)}
	if rec.State != CallRunning {
		text, isError := RenderText(rec.Result, storedError(rec.Err))
		out.callOutcome = &callOutcome{FinishedAt: rec.FinishedAt.UTC().Format(time.RFC3339Nano),
			ExitCode: rec.Result.ExitCode, Output: rec.Result.Text, Truncated: rec.Result.Truncated,
			IsError: isError, Error: rec.Err, Text: text}
	}
	apierr.WriteJSON(w, http.StatusOK, out)
}

// CancelCall is DELETE /api/v1/sandboxes/{id}/calls/{key}: cancel the
// running call of key.
//
// It is a 202 {"cancelled": true} when a call of the key runs, on
// whichever instance: the call ends done with "the call was cancelled by
// its caller", even when it had not reached the sandbox yet. It is a 404
// no_such_call when none runs (none was made, or it ended). The cancel
// does not depend on the request once it arrived: a client that hangs up
// does not stop it.
func (h *REST) CancelCall(w http.ResponseWriter, r *http.Request) {
	sb, ok := auth.SandboxFrom(r.Context())
	if !ok {
		apierr.Write(w, apierr.New(http.StatusUnauthorized, "sandbox token required"))
		return
	}
	key, err := callKey(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), bookkeepingTimeout)
	defer cancel()
	cancelled, err := h.r.CancelCall(ctx, sb.ID, key)
	switch {
	case err != nil:
		apierr.Write(w, apierr.New(http.StatusBadGateway, "cancel the call: %v", err).RetryAfter(retryAfter))
	case !cancelled:
		apierr.Write(w, apierr.WithCode(http.StatusNotFound, "no_such_call", "no running call with this id in the sandbox"))
	default:
		apierr.WriteJSON(w, http.StatusAccepted, map[string]bool{"cancelled": true})
	}
}

// decodeJSON reads a JSON object from the request body into v, one of the
// parameter structs, and returns which keys were sent: absent and zero
// differ where a schema requires a field whose zero value is legal. An
// empty body is {}.
func decodeJSON(r *http.Request, v any) (map[string]json.RawMessage, error) {
	raw, err := readBody(r)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(raw)) == "" {
		raw = []byte("{}")
	}
	sent := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &sent); err != nil {
		return nil, apierr.New(http.StatusBadRequest, "request body must be a JSON object: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return nil, apierr.New(http.StatusBadRequest, "invalid request body: %v", err)
	}
	return sent, nil
}

// readBody reads the request body, at most maxRequestBody bytes: a larger
// one is a 413, not a body cut short.
func readBody(r *http.Request) ([]byte, error) {
	return readAtMost(r, maxRequestBody, func() error {
		return apierr.New(http.StatusRequestEntityTooLarge, "request body larger than %d MiB", maxRequestBody>>20)
	})
}

// readAtMost reads the request body, at most limit bytes: a larger one is
// tooLarge's error, not a body cut short.
func readAtMost(r *http.Request, limit int, tooLarge func() error) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil {
		return nil, apierr.New(http.StatusBadRequest, "read body: %v", err)
	}
	if len(body) > limit {
		return nil, tooLarge()
	}
	return body, nil
}

// queryInt reads an optional integer query parameter.
func queryInt(r *http.Request, name string) (int, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, apierr.New(http.StatusBadRequest, "%s must be an integer, got %q", name, s)
	}
	return n, nil
}

// queryCount reads an optional count (an offset, a limit, lines of
// context), which must not be negative: the request is wrong, so it is a
// 400 with the text MCP's arguments get, never a failed call.
func queryCount(r *http.Request, name string) (int, error) {
	n, err := queryInt(r, name)
	if err != nil {
		return 0, err
	}
	if err := notNegative(name, n); err != nil {
		return 0, apierr.New(http.StatusBadRequest, "%v", err)
	}
	return n, nil
}

// queryBool reads an optional boolean query parameter; a bare flag
// ("?literal") is true.
func queryBool(r *http.Request, name string) (bool, error) {
	q := r.URL.Query()
	if !q.Has(name) {
		return false, nil
	}
	s := q.Get(name)
	if s == "" {
		return true, nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, apierr.New(http.StatusBadRequest, "%s must be a boolean, got %q", name, s)
	}
	return b, nil
}
