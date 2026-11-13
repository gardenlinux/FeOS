// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	toolv1 "github.com/gardenlinux/feos/go/gen/feos/tool/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/testnode"
	"github.com/gardenlinux/feos/go/internal/tools/toolstest"
)

var bg = context.Background()

func newRunner(env *toolstest.Env) *Runner {
	return NewRunner(env.Engine, env.Port, env.Recorder, env.DB, toolstest.InstanceID, env.Log)
}

// scriptTool makes the node answer the ToolService method with resp.
func scriptTool(env *toolstest.Env, method string, resp proto.Message) {
	env.Node.Script().SetTool(func(_ context.Context, _ podid.ID, m string, _ proto.Message) (proto.Message, bool, error) {
		if m == method {
			return resp, true, nil
		}
		return nil, false, nil
	})
}

// scriptExec makes the node answer every Exec with the frames f writes and
// its exit code.
func scriptExec(env *toolstest.Env, f func(req *containerv1.ExecRequest, out testnode.ExecOutput) int32) {
	env.Node.Script().SetExec(func(_ context.Context, _ podid.ID, req *containerv1.ExecRequest, out testnode.ExecOutput) (int32, bool, error) {
		return f(req, out), true, nil
	})
}

func image(n int) []byte { return bytes.Repeat([]byte{0x89}, n) }

// The answers of the ToolService tools rendered into the texts and exit
// codes the clients parse.
func TestRenderToolService(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	sb := env.NewSandbox()
	png := image(2049)
	for _, tc := range []struct {
		name, tool, args, method string
		resp                     proto.Message
		text                     string
		code                     int
		isError                  bool
		image                    *Image
	}{
		{"read text", ToolRead, `{"path":"/a.txt"}`, toolv1.ToolService_Read_FullMethodName,
			&toolv1.ReadResponse{Text: "one\ntwo\n", Truncated: true}, "one\ntwo\n", 0, false, nil},
		{"read error", ToolRead, `{"path":"/nope"}`, toolv1.ToolService_Read_FullMethodName,
			&toolv1.ReadResponse{Error: "no such file: /nope"}, "no such file: /nope\n", 1, true, nil},
		{"read image", ToolRead, `{"path":"/shot.png"}`, toolv1.ToolService_Read_FullMethodName,
			&toolv1.ReadResponse{Image: png, MediaType: "image/png", Width: 10, Height: 20},
			"/shot.png: image/png, 10x20 px, 3 KiB\n", 0, false, &Image{MediaType: "image/png", Data: png, Width: 10, Height: 20}},
		{"write", ToolWrite, `{"path":"/a.txt","content":"x"}`, toolv1.ToolService_Write_FullMethodName,
			&toolv1.WriteResponse{}, "wrote /a.txt\n", 0, false, nil},
		{"write error", ToolWrite, `{"path":"/ro/a","content":"x"}`, toolv1.ToolService_Write_FullMethodName,
			&toolv1.WriteResponse{Error: "read-only file system"}, "read-only file system\n", 1, true, nil},
		{"edit", ToolEdit, `{"path":"/a.txt","edits":[{"old":"a","new":"b"},{"old":"c","new":"d"}]}`, toolv1.ToolService_Edit_FullMethodName,
			&toolv1.EditResponse{Replacements: 2}, "edited /a.txt (2 replacements)\n", 0, false, nil},
		{"edit error", ToolEdit, `{"path":"/a.txt","edits":[{"old":"a","new":"b"}]}`, toolv1.ToolService_Edit_FullMethodName,
			&toolv1.EditResponse{Error: "edit 0: oldText not unique (occurrences: 2)"}, "edit 0: oldText not unique (occurrences: 2)\n", 1, true, nil},
		{"ls", ToolLs, `{"path":"/"}`, toolv1.ToolService_Ls_FullMethodName,
			&toolv1.LsResponse{Entries: []string{".git/", "a.txt", "src/"}}, ".git/\na.txt\nsrc/\n", 0, false, nil},
		{"ls truncated", ToolLs, `{"limit":2}`, toolv1.ToolService_Ls_FullMethodName,
			&toolv1.LsResponse{Entries: []string{"a", "b"}, Truncated: true}, "a\nb\n... [truncated]\n", 0, false, nil},
		{"ls empty", ToolLs, `{"path":"/empty"}`, toolv1.ToolService_Ls_FullMethodName,
			&toolv1.LsResponse{}, "", 0, false, nil},
		{"ls error", ToolLs, `{"path":"/workspace/nope"}`, toolv1.ToolService_Ls_FullMethodName,
			&toolv1.LsResponse{Error: "Path not found: /workspace/nope"}, "Path not found: /workspace/nope\n", 1, true, nil},
		{"rg", ToolRg, `{"pattern":"x"}`, toolv1.ToolService_Rg_FullMethodName,
			&toolv1.RgResponse{Text: "/a.go:3:x := 1\n", Matches: 1}, "/a.go:3:x := 1\n", 0, false, nil},
		{"rg truncated", ToolRg, `{"pattern":"x","limit":1}`, toolv1.ToolService_Rg_FullMethodName,
			&toolv1.RgResponse{Text: "/a.go:3:x\n", Matches: 1, Truncated: true}, "/a.go:3:x\n... [truncated after 1 lines]\n", 0, false, nil},
		{"rg no match", ToolRg, `{"pattern":"zzz"}`, toolv1.ToolService_Rg_FullMethodName,
			&toolv1.RgResponse{}, "", 1, false, nil},
		{"rg error", ToolRg, `{"pattern":"("}`, toolv1.ToolService_Rg_FullMethodName,
			&toolv1.RgResponse{Error: "regex parse error"}, "regex parse error\n", 2, true, nil},
		{"fd", ToolFd, `{"pattern":"go"}`, toolv1.ToolService_Fd_FullMethodName,
			&toolv1.FdResponse{Entries: []string{"/a.go", "/b.go"}}, "/a.go\n/b.go\n", 0, false, nil},
		{"fd truncated", ToolFd, `{"pattern":"go","limit":2}`, toolv1.ToolService_Fd_FullMethodName,
			&toolv1.FdResponse{Entries: []string{"/a.go", "/b.go"}, Truncated: true}, "/a.go\n/b.go\n... [truncated after 2 entries]\n", 0, false, nil},
		{"fd none", ToolFd, `{"pattern":"zzz"}`, toolv1.ToolService_Fd_FullMethodName,
			&toolv1.FdResponse{Truncated: true}, "", 1, false, nil},
		{"fd error", ToolFd, `{"pattern":"("}`, toolv1.ToolService_Fd_FullMethodName,
			&toolv1.FdResponse{Error: "bad pattern"}, "bad pattern\n", 2, true, nil},
		{"webfetch", ToolWebfetch, `{"url":"https://example.com/a"}`, toolv1.ToolService_Webfetch_FullMethodName,
			&toolv1.WebfetchResponse{Title: "Example", Status: 200, EffectiveUrl: "https://example.com/b", Text: "Body text."},
			"Example\n\n[200 https://example.com/b]\nBody text.", 0, false, nil},
		{"webfetch untitled", ToolWebfetch, `{"url":"https://example.com/a"}`, toolv1.ToolService_Webfetch_FullMethodName,
			&toolv1.WebfetchResponse{Text: "Body."}, "Body.", 0, false, nil},
		{"webfetch error", ToolWebfetch, `{"url":"https://example.com/x"}`, toolv1.ToolService_Webfetch_FullMethodName,
			&toolv1.WebfetchResponse{Status: 404, EffectiveUrl: "https://example.com/x", Error: "webfetch: HTTP 404"},
			"[404 https://example.com/x]\nwebfetch: HTTP 404", 1, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scriptTool(env, tc.method, tc.resp)
			var chunks int
			res, err := r.Call(bg, sb, tc.tool, json.RawMessage(tc.args), func(string) { chunks++ })
			if err != nil {
				t.Fatal(err)
			}
			if res.Text != tc.text || res.ExitCode != tc.code || res.IsError != tc.isError || res.Truncated {
				t.Errorf("got %q exit %d isError %v truncated %v; want %q exit %d isError %v",
					res.Text, res.ExitCode, res.IsError, res.Truncated, tc.text, tc.code, tc.isError)
			}
			switch {
			case tc.image == nil && res.Image != nil:
				t.Errorf("unexpected image %+v", res.Image)
			case tc.image != nil && (res.Image == nil || res.Image.MediaType != tc.image.MediaType || !bytes.Equal(res.Image.Data, tc.image.Data) ||
				res.Image.Width != tc.image.Width || res.Image.Height != tc.image.Height):
				t.Errorf("image %+v", res.Image)
			}
			if chunks == 0 && tc.text != "" {
				t.Error("no progress for the output")
			}
		})
	}
	env.Node.Script().SetTool(nil)
	if env.Engine.Holds(sb.ID) != 0 {
		t.Errorf("%d exec holds left", env.Engine.Holds(sb.ID))
	}
}

// A read of text says where its lines start and whether the file goes on,
// for MCP to number them; a read of nothing or of a picture says neither.
func TestReadLines(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	sb := env.NewSandbox()
	for _, tc := range []struct {
		name, args string
		resp       *toolv1.ReadResponse
		first      int
		more       bool
	}{
		{"whole", `{"path":"/a"}`, &toolv1.ReadResponse{Text: "a\nb\n"}, 1, false},
		{"offset", `{"path":"/a","offset":40,"limit":2}`, &toolv1.ReadResponse{Text: "a\nb\n", Truncated: true}, 40, true},
		{"past the end", `{"path":"/a","offset":99}`, &toolv1.ReadResponse{}, 0, false},
		{"image", `{"path":"/a.png"}`, &toolv1.ReadResponse{Image: image(8), MediaType: "image/png", Width: 1, Height: 1}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scriptTool(env, toolv1.ToolService_Read_FullMethodName, tc.resp)
			res, err := r.Call(bg, sb, ToolRead, json.RawMessage(tc.args), nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.FirstLine != tc.first || res.More != tc.more {
				t.Errorf("first line %d, more %v; want %d, %v", res.FirstLine, res.More, tc.first, tc.more)
			}
		})
	}
	env.Node.Script().SetTool(nil)
}

// The ToolService requests carry the main container, the pod's ID and the
// parameters as the guest takes them.
func TestToolServiceRequests(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	sb := env.NewSandbox()
	var got []proto.Message
	var pods []string
	env.Node.Script().SetTool(func(ctx context.Context, pod podid.ID, _ string, req proto.Message) (proto.Message, bool, error) {
		got = append(got, req)
		md, _ := metadata.FromIncomingContext(ctx)
		pods = append(pods, strings.Join(md.Get(nodelink.PodIDMetadata), ","))
		return nil, false, nil
	})
	calls := []struct{ tool, args string }{
		{ToolRead, `{"path":"/a","offset":3,"limit":4}`},
		{ToolLs, `{"path":"/","limit":7}`},
		{ToolRg, `{"pattern":"p","path":"/s","glob":"*.go","ignoreCase":true,"literal":true,"context":2,"limit":9}`},
		{ToolFd, `{"pattern":"p","path":"/s","glob":true,"limit":3}`},
		{ToolEdit, `{"path":"/a","edits":[{"old":"x","new":"y"}]}`},
		{ToolWrite, `{"path":"/b","content":"hi"}`},
	}
	for _, c := range calls {
		if _, err := r.Call(bg, sb, c.tool, json.RawMessage(c.args), nil); err != nil {
			t.Fatalf("%s: %v", c.tool, err)
		}
	}
	want := []proto.Message{
		&toolv1.ReadRequest{Container: "main", Path: "/a", Offset: 3, Limit: 4},
		&toolv1.LsRequest{Container: "main", Path: "/", Limit: 7},
		&toolv1.RgRequest{Container: "main", Pattern: "p", Path: "/s", Glob: "*.go", IgnoreCase: true, Literal: true, Context: 2, Limit: 9},
		&toolv1.FdRequest{Container: "main", Pattern: "p", Path: "/s", Glob: true, Limit: 3},
		&toolv1.EditRequest{Container: "main", Path: "/a", Edits: []*toolv1.EditOp{{Old: "x", New: "y"}}},
		&toolv1.WriteRequest{Container: "main", Path: "/b", Content: []byte("hi")},
	}
	if len(got) != len(want) {
		t.Fatalf("%d requests", len(got))
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("request %d: %v, want %v", i, got[i], want[i])
		}
		if pods[i] != sb.ID.String() {
			t.Errorf("request %d carried feos-pod-id %q", i, pods[i])
		}
	}
	// The node's own tools worked on the pod: the file written is read back.
	res, err := r.Call(bg, sb, ToolRead, json.RawMessage(`{"path":"/b"}`), nil)
	if err != nil || res.Text != "hi" {
		t.Errorf("read back %q, %v", res.Text, err)
	}
}

// A sandbox with a working directory: bash runs in it, and the file
// tools take a relative path, or none, from it; an absolute path stays
// as it is. The outputs name the path the guest was given. A sandbox
// without one passes paths on as they are, and bash runs in the root.
func TestWorkdir(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	sb := env.NewSandbox()
	env.Engine.SetWorkdir(sb.ID, "/workspace/app")
	var got []proto.Message
	env.Node.Script().SetTool(func(_ context.Context, _ podid.ID, _ string, req proto.Message) (proto.Message, bool, error) {
		got = append(got, req)
		return nil, false, nil
	})
	calls := []struct{ tool, args, text string }{
		{ToolWrite, `{"path":"docs/product.md","content":"hi"}`, "wrote /workspace/app/docs/product.md\n"},
		{ToolRead, `{"path":"docs/product.md"}`, "hi"},
		{ToolEdit, `{"path":"../b","edits":[{"old":"x","new":"y"}]}`, ""},
		{ToolLs, `{}`, ""},
		{ToolRg, `{"pattern":"p","path":"."}`, ""},
		{ToolFd, `{"pattern":"p","path":"src"}`, ""},
		{ToolRead, `{"path":"/etc/hostname"}`, ""},
	}
	for _, c := range calls {
		res, err := r.Call(bg, sb, c.tool, json.RawMessage(c.args), nil)
		if err != nil {
			t.Fatalf("%s: %v", c.tool, err)
		}
		if c.text != "" && res.Text != c.text {
			t.Errorf("%s %s: %q, want %q", c.tool, c.args, res.Text, c.text)
		}
	}
	paths := []string{"/workspace/app/docs/product.md", "/workspace/app/docs/product.md", "/workspace/b",
		"/workspace/app", "/workspace/app", "/workspace/app/src", "/etc/hostname"}
	if len(got) != len(paths) {
		t.Fatalf("%d requests", len(got))
	}
	for i, m := range got {
		if p := m.(interface{ GetPath() string }).GetPath(); p != paths[i] {
			t.Errorf("request %d (%s): path %q, want %q", i, calls[i].tool, p, paths[i])
		}
	}

	var dirs []string
	scriptExec(env, func(req *containerv1.ExecRequest, _ testnode.ExecOutput) int32 {
		dirs = append(dirs, req.GetWorkingDir())
		return 0
	})
	if _, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"pwd"}`), nil); err != nil {
		t.Fatal(err)
	}

	other := env.NewSandbox()
	got = nil
	if _, err := r.Call(bg, other, ToolWrite, json.RawMessage(`{"path":"docs/product.md","content":"hi"}`), nil); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].(*toolv1.WriteRequest).GetPath() != "docs/product.md" {
		t.Errorf("without a working directory: %v", got)
	}
	if _, err := r.Call(bg, other, ToolBash, json.RawMessage(`{"command":"pwd"}`), nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(dirs, ",") != "/workspace/app,/" {
		t.Errorf("bash ran in %v", dirs)
	}
}

// bash: the frames as they come, each stream capped at MaxToolOutput with
// a marker, the whole cut at MaxToolOutput; the exit code of the last
// frame.
func TestRenderExec(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	sb := env.NewSandbox()
	big := func(c byte, n int) []byte { return bytes.Repeat([]byte{c}, n) }

	// The command as sent.
	var req *containerv1.ExecRequest
	scriptExec(env, func(r *containerv1.ExecRequest, out testnode.ExecOutput) int32 {
		req = r
		_ = out.Stdout([]byte("out\n"))
		_ = out.Stderr([]byte("err\n"))
		return 3
	})
	res, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"make test"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "out\nerr\n" || res.ExitCode != 3 || !res.IsError || res.Truncated {
		t.Errorf("result %+v", res)
	}
	if req.GetContainerId() != "main" || strings.Join(req.GetCommand(), "|") != "bash|-c|make test" || req.GetWorkingDir() != "/" {
		t.Errorf("request %v", req)
	}

	// One stream past the cap: the chunk that crosses it is dropped and
	// the marker emitted once; the other stream goes on. Frames are 32 KiB
	// at most on the test node.
	scriptExec(env, func(_ *containerv1.ExecRequest, out testnode.ExecOutput) int32 {
		_ = out.Stdout(big('a', 60000))
		_ = out.Stdout([]byte("never"))
		_ = out.Stderr([]byte("tail\n"))
		return 0
	})
	res, err = r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"yes"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := string(big('a', 32768)) + "\n... [truncated]\n" + "tail\n"
	if res.Text != want || res.Truncated || res.ExitCode != 0 {
		t.Errorf("one stream past the cap: %d bytes, truncated %v (want %d bytes)", len(res.Text), res.Truncated, len(want))
	}

	// Both streams under their caps, together past it: the text is cut at
	// MaxToolOutput and says so.
	scriptExec(env, func(_ *containerv1.ExecRequest, out testnode.ExecOutput) int32 {
		_ = out.Stdout(big('o', 40000))
		_ = out.Stderr(big('e', 40000))
		return 1
	})
	res, err = r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"x"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Text) != MaxToolOutput || !res.Truncated || res.Text != string(big('o', 40000))+string(big('e', 10000)) {
		t.Errorf("both streams: %d bytes, truncated %v", len(res.Text), res.Truncated)
	}

	// Exactly at the cap the text takes nothing more and is not cut, as it
	// always was.
	scriptExec(env, func(_ *containerv1.ExecRequest, out testnode.ExecOutput) int32 {
		_ = out.Stdout(big('o', 20000))
		_ = out.Stdout(big('p', 30000))
		_ = out.Stderr([]byte("lost"))
		return 0
	})
	res, _ = r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"x"}`), nil)
	if len(res.Text) != MaxToolOutput || res.Truncated {
		t.Errorf("at the cap: %d bytes, truncated %v", len(res.Text), res.Truncated)
	}

	// No output, exit 0; the real command runs in the pod.
	env.Node.Script().SetExec(nil)
	res, err = r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":""}`), nil)
	if err != nil || res.Text != "" || res.ExitCode != 0 || res.IsError {
		t.Errorf("empty command %+v %v", res, err)
	}
	res, err = r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"printf 'a\\nb'; exit 42"}`), nil)
	if err != nil || res.Text != "a\nb" || res.ExitCode != 42 {
		t.Errorf("real command %+v %v", res, err)
	}
}

// fakeConns is a node connection whose Exec stream is given by the test.
type fakeConns struct {
	nodelink.Conns
	frames []*containerv1.ExecStreamFrame
	err    error
}

func (f *fakeConns) Conn(context.Context, string) (grpc.ClientConnInterface, error) { return f, nil }
func (f *fakeConns) Invoke(context.Context, string, any, any, ...grpc.CallOption) error {
	return status.Error(codes.Unimplemented, "fake")
}
func (f *fakeConns) NewStream(ctx context.Context, _ *grpc.StreamDesc, _ string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
	return &fakeStream{ctx: ctx, frames: f.frames, err: f.err}, nil
}

type fakeStream struct {
	grpc.ClientStream
	ctx    context.Context
	frames []*containerv1.ExecStreamFrame
	err    error
}

func (s *fakeStream) Context() context.Context { return s.ctx }
func (s *fakeStream) SendMsg(any) error        { return nil }
func (s *fakeStream) CloseSend() error         { return nil }
func (s *fakeStream) RecvMsg(m any) error {
	if len(s.frames) == 0 {
		if s.err != nil {
			return s.err
		}
		return io.EOF
	}
	proto.Merge(m.(proto.Message), s.frames[0])
	s.frames = s.frames[1:]
	return nil
}

// fakeEngine leases every sandbox on node n1.
type fakeEngine struct{ Sandboxes }

func (fakeEngine) Acquire(_ context.Context, id podid.ID, _ model.HoldKind) (model.Lease, func(), error) {
	return model.Lease{SandboxID: id, Node: "n1"}, func() {}, nil
}
func (fakeEngine) Reachable(podid.ID) {}

// A stream that ends without the exit code is an error, not exit 0; a
// stream that breaks after output keeps the output and is not made again.
func TestExecStreamEnds(t *testing.T) {
	sb := toolstest.Sandbox()
	out := &containerv1.ExecStreamFrame{Frame: &containerv1.ExecStreamFrame_Stdout{Stdout: []byte("partial\n")}}
	conns := &fakeConns{frames: []*containerv1.ExecStreamFrame{out}}
	r := NewRunner(fakeEngine{}, conns, nil, nil, "i1", nil)
	res, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"x"}`), nil)
	if err == nil || err.Error() != "exec stream ended without exit code" || res.Text != "partial\n" {
		t.Errorf("%+v %v", res, err)
	}
	conns.err = status.Error(codes.FailedPrecondition, "pod is POD_STATE_SUSPENDED")
	res, err = r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"x"}`), nil)
	if err == nil || err.Error() != "node n1: pod is POD_STATE_SUSPENDED" || res.Text != "partial\n" {
		t.Errorf("broken after output: %+v %v", res, err)
	}
	var ne *NodeError
	if !errors.As(err, &ne) || status.Code(err) != codes.FailedPrecondition {
		t.Errorf("error %T %v", err, err)
	}
}

// A call past its deadline is "timeout after <N>s", with the output it
// produced; the command in the guest is killed with the stream. Only the
// control plane ends the call: the node gets no deadline, which it could
// act on first and make the timeout look like a cancelled stream.
func TestTimeouts(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	sb := env.NewSandbox()

	killed := make(chan struct{})
	env.Node.Script().SetExec(func(ctx context.Context, _ podid.ID, _ *containerv1.ExecRequest, out testnode.ExecOutput) (int32, bool, error) {
		if d, ok := ctx.Deadline(); ok {
			t.Errorf("the node got the deadline %v", d)
		}
		_ = out.Stdout([]byte("start\n"))
		<-ctx.Done()
		close(killed)
		return 0, true, status.FromContextError(ctx.Err()).Err()
	})
	begin := time.Now()
	res, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"sleep 600","timeout":1}`), nil)
	var te *TimeoutError
	if !errors.As(err, &te) || err.Error() != "timeout after 1s" || res.Text != "start\n" {
		t.Fatalf("%+v %v", res, err)
	}
	if d := time.Since(begin); d < time.Second || d > 5*time.Second {
		t.Errorf("the call took %v", d)
	}
	select {
	case <-killed:
	case <-time.After(5 * time.Second):
		t.Error("the guest's command was not cancelled")
	}

	// A real command is killed too.
	env.Node.Script().SetExec(nil)
	res, err = r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"echo begun; sleep 600","timeout":1}`), nil)
	if err == nil || err.Error() != "timeout after 1s" || res.Text != "begun\n" {
		t.Errorf("%+v %v", res, err)
	}

	// A unary tool runs within the default timeout.
	r.defaultTimeout = time.Second
	env.Node.Script().SetHook(toolv1.ToolService_Read_FullMethodName, func(ctx context.Context, _ string, _ any) error {
		if d, ok := ctx.Deadline(); ok {
			t.Errorf("the node got the deadline %v", d)
		}
		<-ctx.Done()
		return status.FromContextError(ctx.Err()).Err()
	})
	_, err = r.Call(bg, sb, ToolRead, json.RawMessage(`{"path":"/a"}`), nil)
	if err == nil || err.Error() != "timeout after 1s" {
		t.Errorf("read: %v", err)
	}
	env.Node.Script().SetHook(toolv1.ToolService_Read_FullMethodName, nil)
	if env.Engine.Holds(sb.ID) != 0 {
		t.Errorf("%d holds left", env.Engine.Holds(sb.ID))
	}
}
