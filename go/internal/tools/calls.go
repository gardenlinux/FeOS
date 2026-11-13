// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	toolv1 "github.com/gardenlinux/feos/go/gen/feos/tool/v1"
)

// mainContainer is the sandbox's container that tools work in.
const mainContainer = "main"

// call is one tool call made from its arguments: what runs in the guest and
// how its answer becomes output.
//
// An exec tool (bash and the process tools) runs argv through
// ContainerService.Exec in the main container with the sandbox's working
// directory as its own; its stdout and stderr frames are the output, each
// stream capped at MaxToolOutput with a marker, and its last frame is the
// exit code. Every other tool is one unary ToolService call, and unary
// renders its answer; it is given the working directory (dir), against
// which it takes a relative path (at).
type call struct {
	tool    string
	timeout time.Duration
	argv    []string
	unary   func(ctx context.Context, c toolv1.ToolServiceClient, dir string, o *output) (code int, err error)

	// The background-process bookkeeping of the call (§9 Holds).
	startTimeout time.Duration // process_start: the process's own timeout; 0: no limit
	processID    string        // process_logs, process_stop: the process
}

// prepare checks a call's arguments and makes the call. Its errors are the
// texts the clients have seen for bad arguments; nothing has run yet.
func prepare(tool string, args json.RawMessage, defaultTimeout time.Duration) (*call, error) {
	c := &call{tool: tool, timeout: defaultTimeout}
	switch tool {
	case ToolBash:
		var p BashParams
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, err
		}
		if p.Timeout > 0 {
			c.timeout = seconds(p.Timeout)
		}
		c.argv = []string{"bash", "-c", p.Command}

	case ToolProcessStart:
		var p ProcessStartParams
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, err
		}
		if strings.TrimSpace(p.Command) == "" {
			return nil, errors.New("command is required")
		}
		if p.Timeout < 0 {
			return nil, errors.New("timeout must be 0 (no limit) or a number of seconds")
		}
		c.startTimeout = seconds(p.Timeout)
		c.argv = []string{"python3", "-c", processStartScript, p.Command, fmt.Sprint(p.Timeout), p.Name, p.Cwd}

	case ToolProcessList:
		c.argv = []string{"python3", "-c", processListScript}

	case ToolProcessLogs:
		var p ProcessLogsParams
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, err
		}
		if p.Lines <= 0 {
			p.Lines = 100
		}
		c.processID = p.ID
		c.argv = []string{"python3", "-c", processLogsScript, p.ID, fmt.Sprint(p.Lines)}

	case ToolProcessStop:
		var p ProcessStopParams
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, err
		}
		c.processID = p.ID
		c.argv = []string{"python3", "-c", processStopScript, p.ID}

	case ToolRead:
		var p ReadParams
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, err
		}
		if err := notNegative("offset", p.Offset, "limit", p.Limit); err != nil {
			return nil, err
		}
		c.unary = func(ctx context.Context, tc toolv1.ToolServiceClient, dir string, o *output) (int, error) {
			file := at(dir, p.Path)
			resp, err := tc.Read(ctx, &toolv1.ReadRequest{
				Container: mainContainer, Path: file, Offset: uint32(p.Offset), Limit: uint32(p.Limit),
			})
			if err != nil {
				return -1, err
			}
			if resp.Error != "" {
				o.emit(resp.Error + "\n")
				return 1, nil
			}
			if len(resp.Image) > 0 {
				// The line goes where text goes (a text-only client, the
				// activity log) and says what the picture beside it is.
				o.emit(fmt.Sprintf("%s: %s, %dx%d px, %d KiB\n", file,
					resp.MediaType, resp.Width, resp.Height, (len(resp.Image)+1023)/1024))
				o.emitImage(&Image{MediaType: resp.MediaType, Data: resp.Image, Width: int(resp.Width), Height: int(resp.Height)})
				return 0, nil
			}
			o.emit(resp.Text)
			if resp.Text != "" {
				o.lines(max(p.Offset, 1), resp.Truncated)
			}
			return 0, nil
		}

	case ToolWrite:
		var p WriteParams
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, err
		}
		return prepareWrite(p.Path, []byte(p.Content), defaultTimeout)

	case ToolEdit:
		var p EditParams
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, err
		}
		if len(p.Edits) == 0 {
			return nil, errors.New("edits must not be empty")
		}
		edits := make([]*toolv1.EditOp, len(p.Edits))
		for i, ed := range p.Edits {
			edits[i] = &toolv1.EditOp{Old: ed.Old, New: ed.New}
		}
		req := &toolv1.EditRequest{Container: mainContainer, Path: p.Path, Edits: edits}
		if proto.Size(req) > maxGuestRequest {
			return nil, &TooLargeError{Tool: tool}
		}
		c.unary = func(ctx context.Context, tc toolv1.ToolServiceClient, dir string, o *output) (int, error) {
			req.Path = at(dir, p.Path)
			resp, err := tc.Edit(ctx, req)
			if err != nil {
				return -1, err
			}
			if resp.Error != "" {
				o.emit(resp.Error + "\n")
				return 1, nil
			}
			o.emit(fmt.Sprintf("edited %s (%d replacements)\n", req.Path, resp.Replacements))
			return 0, nil
		}

	case ToolLs:
		var p LsParams
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, err
		}
		if err := notNegative("limit", p.Limit); err != nil {
			return nil, err
		}
		c.unary = func(ctx context.Context, tc toolv1.ToolServiceClient, dir string, o *output) (int, error) {
			resp, err := tc.Ls(ctx, &toolv1.LsRequest{Container: mainContainer, Path: at(dir, p.Path), Limit: uint32(p.Limit)})
			if err != nil {
				return -1, err
			}
			if resp.Error != "" {
				o.emit(resp.Error + "\n")
				return 1, nil
			}
			if len(resp.Entries) > 0 {
				o.emit(strings.Join(resp.Entries, "\n") + "\n")
			}
			if resp.Truncated {
				o.emit("... [truncated]\n")
			}
			return 0, nil
		}

	case ToolRg:
		var p RgParams
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, err
		}
		if p.Pattern == "" {
			return nil, errors.New("pattern is required")
		}
		if err := notNegative("context", p.Context, "limit", p.Limit); err != nil {
			return nil, err
		}
		c.unary = func(ctx context.Context, tc toolv1.ToolServiceClient, dir string, o *output) (int, error) {
			resp, err := tc.Rg(ctx, &toolv1.RgRequest{
				Container: mainContainer, Pattern: p.Pattern, Path: at(dir, p.Path), Glob: p.Glob,
				IgnoreCase: p.IgnoreCase, Literal: p.Literal, Context: uint32(p.Context), Limit: uint32(p.Limit),
			})
			if err != nil {
				return -1, err
			}
			if resp.Error != "" {
				o.emit(resp.Error + "\n")
				return 2, nil
			}
			if resp.Text != "" {
				o.emit(resp.Text)
			}
			if resp.Truncated {
				o.emit(fmt.Sprintf("... [truncated after %d lines]\n", resp.Matches))
			}
			if resp.Matches == 0 {
				return 1, nil
			}
			return 0, nil
		}

	case ToolFd:
		var p FdParams
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, err
		}
		if p.Pattern == "" {
			return nil, errors.New("pattern is required")
		}
		if err := notNegative("limit", p.Limit); err != nil {
			return nil, err
		}
		c.unary = func(ctx context.Context, tc toolv1.ToolServiceClient, dir string, o *output) (int, error) {
			resp, err := tc.Fd(ctx, &toolv1.FdRequest{
				Container: mainContainer, Pattern: p.Pattern, Path: at(dir, p.Path), Glob: p.Glob, Limit: uint32(p.Limit),
			})
			if err != nil {
				return -1, err
			}
			if resp.Error != "" {
				o.emit(resp.Error + "\n")
				return 2, nil
			}
			if len(resp.Entries) == 0 {
				return 1, nil
			}
			o.emit(strings.Join(resp.Entries, "\n") + "\n")
			if resp.Truncated {
				o.emit(fmt.Sprintf("... [truncated after %d entries]\n", len(resp.Entries)))
			}
			return 0, nil
		}

	case ToolWebfetch:
		var p WebFetchParams
		if err := json.Unmarshal(args, &p); err != nil || p.URL == "" {
			return nil, errors.New("webfetch: bad params")
		}
		c.unary = func(ctx context.Context, tc toolv1.ToolServiceClient, dir string, o *output) (int, error) {
			resp, err := tc.Webfetch(ctx, &toolv1.WebfetchRequest{Url: p.URL})
			if err != nil {
				return -1, err
			}
			head := ""
			if resp.Title != "" {
				head = resp.Title + "\n\n"
			}
			if resp.Status != 0 {
				head += fmt.Sprintf("[%d %s]\n", resp.Status, resp.EffectiveUrl)
			}
			o.emit(head + resp.Text)
			if resp.Error != "" {
				o.emit(resp.Error)
				return 1, nil
			}
			return 0, nil
		}

	default:
		return nil, fmt.Errorf("unknown tool %q", tool)
	}
	if c.argv != nil {
		n := 0
		for _, a := range c.argv {
			n += len(a)
		}
		if n > maxGuestRequest {
			return nil, &TooLargeError{Tool: tool}
		}
	}
	return c, nil
}

// at is a file tool's path as the guest takes it, in a sandbox whose
// working directory is dir: an absolute path as it is, a relative one
// relative to dir, none dir itself. A sandbox without a working directory
// ("") passes every path on as it is, and the guest takes a relative one
// from the root.
func at(dir, p string) string {
	if dir == "" || strings.HasPrefix(p, "/") {
		return p
	}
	return path.Join(dir, p)
}

// prepareWrite makes the write of content into path. The content is bytes
// whatever they hold: REST's file write calls it with the request's body,
// which a JSON string, the tool's argument, could not carry unless it is
// UTF-8 text.
func prepareWrite(path string, content []byte, defaultTimeout time.Duration) (*call, error) {
	req := &toolv1.WriteRequest{Container: mainContainer, Path: path, Content: content}
	if proto.Size(req) > maxGuestRequest {
		return nil, &TooLargeError{Tool: ToolWrite}
	}
	return &call{tool: ToolWrite, timeout: defaultTimeout,
		unary: func(ctx context.Context, tc toolv1.ToolServiceClient, dir string, o *output) (int, error) {
			req.Path = at(dir, path)
			resp, err := tc.Write(ctx, req)
			if err != nil {
				return -1, err
			}
			if resp.Error != "" {
				o.emit(resp.Error + "\n")
				return 1, nil
			}
			o.emit(fmt.Sprintf("wrote %s\n", req.Path))
			return 0, nil
		}}, nil
}

// writeArgs are the arguments prepareWrite's call is recorded with in the
// activity log: the write tool's own when the content is UTF-8 text, as
// MCP's call of it has them; for other bytes, which no JSON string carries
// unchanged, the path and the size.
func writeArgs(path string, content []byte) json.RawMessage {
	var v any = WriteParams{Path: path, Content: string(content)}
	if !utf8.Valid(content) {
		v = struct {
			Path string `json:"path"`
			Size int    `json:"size"`
		}{path, len(content)}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// maxGuestRequest is the largest request a call sends into the guest. The
// guest's services decode at most 4 MiB a message (tonic's default, which
// every deployed guest has, restored ones included), and refuse a larger
// one with a text that names their limit, not the call's; this leaves room
// for the request's framing. A larger call is refused before it runs.
const maxGuestRequest = 4<<20 - 64<<10

// TooLargeError is a call whose request is larger than the guest takes
// (maxGuestRequest): a write's content, an edit's edits, a command.
// Nothing has run.
type TooLargeError struct {
	Tool string
}

func (e *TooLargeError) Error() string {
	what, one := "the command is", "call"
	switch e.Tool {
	case ToolWrite:
		what, one = "the content is", "write"
	case ToolEdit:
		what, one = "the edits are", "edit"
	}
	return fmt.Sprintf("%s larger than %d KiB, the most one %s carries into the sandbox", what, maxGuestRequest>>10, one)
}

// maxTimeoutSeconds bounds a timeout an argument names: the largest the
// pods' protocol carries (an Exec's timeout_seconds is a uint32, about 136
// years). A larger number of seconds would overflow a time.Duration and
// wrap around to a deadline in the past.
const maxTimeoutSeconds int64 = math.MaxUint32

// seconds is a timeout argument as a duration, at most maxTimeoutSeconds.
func seconds(n int) time.Duration {
	return time.Duration(min(int64(n), maxTimeoutSeconds)) * time.Second
}

// notNegative refuses a negative count: the guest takes counts unsigned,
// and a negative one would wrap around to a huge one. pairs alternates a
// parameter's name and its value.
func notNegative(pairs ...any) error {
	for i := 0; i+1 < len(pairs); i += 2 {
		if n, _ := pairs[i+1].(int); n < 0 {
			return fmt.Errorf("%s must not be negative, got %d", pairs[i], n)
		}
	}
	return nil
}

// processHandle is what process_start prints: its first line.
type processHandle struct {
	ID string `json:"id"`
}

// parseHandle reads the process id from process_start's output.
func parseHandle(out string) (string, bool) {
	line, _, _ := strings.Cut(out, "\n")
	var h processHandle
	if err := json.Unmarshal([]byte(line), &h); err != nil || h.ID == "" {
		return "", false
	}
	return h.ID, true
}

// processRow is one entry of process_list's output.
type processRow struct {
	ID      string `json:"id"`
	Running bool   `json:"running"`
}

// noProcesses is process_list's output when there are none.
const noProcesses = "no background processes"

// parseProcessList reads process_list's output: the processes and whether
// each runs. ok is false for an output that is neither of its forms.
func parseProcessList(out string) (rows []processRow, ok bool) {
	trimmed := strings.TrimSpace(out)
	if trimmed == noProcesses {
		return nil, true
	}
	if !strings.HasPrefix(trimmed, "[") {
		return nil, false
	}
	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		return nil, false
	}
	return rows, true
}

// unknownProcess is the prefix of process_logs' and process_stop's answer
// for a process id the sandbox does not know.
const unknownProcess = "no such process: "
