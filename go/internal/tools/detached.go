// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strconv"
	"strings"
	"time"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	toolv1 "github.com/gardenlinux/feos/go/gen/feos/tool/v1"
	"github.com/gardenlinux/feos/go/internal/model"
)

// The Runner's detached calls (detached_types.go): what the environment
// controller runs in a pod's main container on its own behalf. Each takes
// the lease the caller read from the row: no Acquire, no hold, no
// activity row, no Touch, so that probes, provisioning and the
// supervisor of attached services never wake or keep awake what they
// look at. A call into a pod that is not where the lease says fails as
// the node answers it (a NodeError); the caller reads the row again.
// The calls count against the sandbox's exec cap on this instance as the
// tools' do.

// noCwdExit is the exit code of the detached start script when the
// process's working directory does not exist.
const noCwdExit = 3

// keptExited is how many records of processes that are over a detached
// start keeps of its name (the newest, by start time): the logs of the
// last few runs of an attached service stay readable, while a service
// that restarts for days leaves no more behind.
const keptExited = 3

// detachedStartScript is processStartScript with a check of the working
// directory first, so that a directory that does not exist yet (an
// attached service whose checkout is not there) is told apart from a
// failed start: it exits noCwdExit then. Before it starts a named
// process, it removes the records of processes of that name that are
// over (exited, stopped, or lost with a cold restore) but the newest
// argv[5] (keptExited): nothing else removes a record, and every restart
// of an attached service writes one. A running process keeps its record,
// and so does one whose start is not fully recorded yet (no started_at).
const detachedStartScript = `import os,shutil,sys
if sys.argv[4] and not os.path.isdir(sys.argv[4]):
    print("no such working directory: %s" % sys.argv[4]); raise SystemExit(3)
def prune(root, name, keep):
    def read(d,k):
        try: return open(os.path.join(d,k)).read().strip()
        except OSError: return ""
    def alive(d):
        try:
            pgid = int(read(d,"pgid") or 0)
            rest = open("/proc/%d/stat" % pgid).read().rsplit(") ",1)[1].split()
        except (OSError, ValueError, IndexError):
            return False
        want = read(d,"start")
        return rest[0] != "Z" and rest[2] == str(pgid) and (not want or rest[19] == want)
    over = []
    for tid in os.listdir(root) if os.path.isdir(root) else []:
        d = os.path.join(root,tid)
        if read(d,"name") != name or not read(d,"started_at").isdigit(): continue
        if read(d,"exit") == "" and alive(d): continue
        over.append((int(read(d,"started_at")), tid))
    for _,tid in sorted(over)[:-keep]:
        shutil.rmtree(os.path.join(root,tid), ignore_errors=True)
if sys.argv[3] and len(sys.argv) > 5:
    prune("` + processRoot + `", sys.argv[3], int(sys.argv[5]))
` + processStartScript

// detachedListScript prints the records of the processes named argv[1]
// (every one for ""), and of the ids argv[2:] whatever their names, as one
// compact JSON array: id, name, running and the exit code once there is
// one. The supervisor of attached services lists only its own name and
// the process its row records, so that neither the history of other
// processes nor the agent's own make the answer longer than a tool's
// output may be.
const detachedListScript = `import json,os,sys
root = "` + processRoot + `"
name = sys.argv[1]
ids = set(sys.argv[2:])
def read(d,k,default=""):
    try: return open(os.path.join(d,k)).read().strip()
    except OSError: return default
def alive(pgid, want_start=""):
    if not pgid: return False
    try:
        rest = open("/proc/%d/stat" % pgid).read().rsplit(") ",1)[1].split()
    except (OSError, IndexError):
        return False
    if rest[0] == "Z" or rest[2] != str(pgid): return False
    return not want_start or rest[19] == want_start
out = []
for tid in sorted(os.listdir(root)) if os.path.isdir(root) else []:
    d = os.path.join(root,tid)
    n = read(d,"name")
    if name and n != name and tid not in ids: continue
    exit_code = read(d,"exit")
    try: pgid = int(read(d,"pgid","0") or 0)
    except ValueError: pgid = 0
    row = {"id":tid,"name":n,"running":exit_code == "" and alive(pgid, read(d,"start"))}
    if exit_code.lstrip("-").isdigit(): row["exit_code"] = int(exit_code)
    out.append(row)
print(json.dumps(out,separators=(",",":")))
`

// ExecDetached runs argv in the main container of the lease's pod with env
// added to the container's environment, in cwd ("": /), for at most
// timeout (0: the tools' default): its exit code and its output, each
// stream capped at MaxToolOutput with a marker. A call past its deadline
// is a TimeoutError; a call the node refused a NodeError.
func (r *Runner) ExecDetached(ctx context.Context, lease model.Lease, argv []string, env map[string]string, cwd string,
	timeout time.Duration) (code int, stdout, stderr string, err error) {
	if len(argv) == 0 {
		return -1, "", "", errors.New("exec: no command")
	}
	if timeout <= 0 {
		timeout = r.defaultTimeout
	}
	if cwd == "" {
		cwd = "/"
	}
	if err := r.takeExec(lease.SandboxID); err != nil {
		return -1, "", "", err
	}
	defer r.giveExec(lease.SandboxID)
	conn, err := r.conns.Conn(ctx, lease.Node)
	if err != nil {
		return -1, "", "", err
	}
	actx, cancel := withLimit(ctx, lease.SandboxID, timeout)
	defer cancel()
	stream, err := containerv1.NewContainerServiceClient(conn).Exec(actx, &containerv1.ExecRequest{
		ContainerId: mainContainer, Command: argv, Env: env, WorkingDir: cwd,
	})
	if err != nil {
		return -1, "", "", r.callError(lease.Node, deadline(ctx, actx, timeout, err))
	}
	var out, errs strings.Builder
	var outCap, errCap streamCap
	code, exited := -1, false
	for {
		f, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return -1, out.String(), errs.String(), r.callError(lease.Node, deadline(ctx, actx, timeout, err))
		}
		switch fr := f.GetFrame().(type) {
		case *containerv1.ExecStreamFrame_Stdout:
			if s, ok := outCap.take(fr.Stdout); ok {
				out.WriteString(s)
			}
		case *containerv1.ExecStreamFrame_Stderr:
			if s, ok := errCap.take(fr.Stderr); ok {
				errs.WriteString(s)
			}
		case *containerv1.ExecStreamFrame_ExitCode:
			code, exited = int(fr.ExitCode), true
		}
	}
	if !exited {
		return -1, out.String(), errs.String(), errors.New("exec stream ended without exit code")
	}
	return code, out.String(), errs.String(), nil
}

// unaryDetached makes one ToolService call into the lease's pod within
// the tools' default timeout.
func (r *Runner) unaryDetached(ctx context.Context, lease model.Lease,
	call func(ctx context.Context, c toolv1.ToolServiceClient) error) error {
	conn, err := r.conns.Conn(ctx, lease.Node)
	if err != nil {
		return err
	}
	actx, cancel := withLimit(ctx, lease.SandboxID, r.defaultTimeout)
	defer cancel()
	if err := call(actx, toolv1.NewToolServiceClient(conn)); err != nil {
		return r.callError(lease.Node, deadline(ctx, actx, r.defaultTimeout, err))
	}
	return nil
}

// shellQuote quotes a word for a POSIX shell: as it is when it consists
// of characters no shell treats specially, else in single quotes.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.ContainsRune("-_./:=@%+,", c))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// shellCommand is argv as one shell command line, each word quoted.
func shellCommand(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = shellQuote(a)
	}
	return strings.Join(words, " ")
}

// StartProcessDetached starts a background process as process_start does
// with timeout 0 (no limit): argv shell-quoted into the start script's
// command, in p.Cwd ("": /), named p.Name, once the records of the
// processes of that name that are over went, but the newest keptExited.
// p.Env reaches the process only through ExecRequest.Env, which the
// script's process inherits, and with HOME=/root unless it sets HOME: no
// value of it is written to the container's disk (the process record
// keeps the command, the name and the times). A working directory that
// does not exist is an error wrapping ErrNoCwd. No hold is taken: the
// process does not keep the sandbox awake.
func (r *Runner) StartProcessDetached(ctx context.Context, lease model.Lease, p ProcessStart) (string, error) {
	if len(p.Argv) == 0 {
		return "", errors.New("start a process: no command")
	}
	env := maps.Clone(p.Env)
	if env == nil {
		env = map[string]string{}
	}
	if _, ok := env["HOME"]; !ok {
		env["HOME"] = "/root"
	}
	argv := []string{"python3", "-c", detachedStartScript, shellCommand(p.Argv), "0", p.Name, p.Cwd, strconv.Itoa(keptExited)}
	code, stdout, stderr, err := r.ExecDetached(ctx, lease, argv, env, "/", r.defaultTimeout)
	if err != nil {
		return "", err
	}
	switch {
	case code == noCwdExit && strings.HasPrefix(stdout, "no such working directory"):
		return "", fmt.Errorf("start %s in %s: %w", p.Name, p.Cwd, ErrNoCwd)
	case code != 0:
		return "", fmt.Errorf("start %s: exited %d: %s", p.Name, code, firstLine(stderr+stdout))
	}
	id, ok := parseHandle(stdout)
	if !ok {
		return "", fmt.Errorf("start %s: no handle in %q", p.Name, firstLine(stdout))
	}
	return id, nil
}

// detachedRow is a row of process_list's output with what the detached
// listing reads.
type detachedRow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Running  bool   `json:"running"`
	ExitCode *int   `json:"exit_code"`
}

// ProcessesDetached lists the background processes of the lease's main
// container named name (every one for ""), and those of ids whatever
// their names, running or not, by id.
func (r *Runner) ProcessesDetached(ctx context.Context, lease model.Lease, name string, ids ...string) ([]ProcessStatus, error) {
	argv := append([]string{"python3", "-c", detachedListScript, name}, ids...)
	code, stdout, stderr, err := r.ExecDetached(ctx, lease, argv, nil, "/", r.defaultTimeout)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("process_list exited with %d: %s", code, firstLine(stderr+stdout))
	}
	if strings.Contains(stdout, streamMarker) {
		return nil, errors.New("process_list: the output is longer than a tool's output may be")
	}
	var rows []detachedRow
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rows); err != nil {
		return nil, fmt.Errorf("process_list: unexpected output %q", firstLine(stdout))
	}
	out := make([]ProcessStatus, 0, len(rows))
	for _, row := range rows {
		out = append(out, ProcessStatus{ID: row.ID, Name: row.Name, Running: row.Running, ExitCode: row.ExitCode})
	}
	return out, nil
}

// ProcessLogsDetached returns the last lines lines (100 when not above 0)
// of a process's output, oldest first; an error wrapping ErrNoProcess for
// an id the container does not know.
func (r *Runner) ProcessLogsDetached(ctx context.Context, lease model.Lease, id string, lines int) ([]string, error) {
	if lines <= 0 {
		lines = 100
	}
	code, stdout, stderr, err := r.ExecDetached(ctx, lease,
		[]string{"python3", "-c", processLogsScript, id, strconv.Itoa(lines)}, nil, "/", r.defaultTimeout)
	if err != nil {
		return nil, err
	}
	switch {
	case code == 1 && strings.HasPrefix(stdout, unknownProcess):
		return nil, fmt.Errorf("process %s: %w", id, ErrNoProcess)
	case code != 0:
		return nil, fmt.Errorf("process_logs exited with %d: %s", code, firstLine(stderr+stdout))
	}
	if stdout == "(no output yet)\n" {
		return []string{}, nil
	}
	return strings.Split(strings.TrimSuffix(stdout, "\n"), "\n"), nil
}

// ReadDetached reads a file of the main container of the lease's pod as
// the read tool does (its first 2000 lines), also of a container whose
// process has exited: its root filesystem stays while the pod does. A
// file the guest cannot read as text (missing, too large, a picture) is
// an error with the guest's text.
func (r *Runner) ReadDetached(ctx context.Context, lease model.Lease, path string) (string, error) {
	var resp *toolv1.ReadResponse
	err := r.unaryDetached(ctx, lease, func(ctx context.Context, c toolv1.ToolServiceClient) error {
		var err error
		resp, err = c.Read(ctx, &toolv1.ReadRequest{Container: mainContainer, Path: path})
		return err
	})
	switch {
	case err != nil:
		return "", err
	case resp.GetError() != "":
		return "", errors.New(resp.GetError())
	case len(resp.GetImage()) > 0:
		return "", fmt.Errorf("%s is a picture (%s)", path, resp.GetMediaType())
	}
	return resp.GetText(), nil
}

// StopProcessDetached stops a process (TERM to its group, KILL after 2 s);
// nil when it was not running any more, an error wrapping ErrNoProcess for
// an id the container does not know.
func (r *Runner) StopProcessDetached(ctx context.Context, lease model.Lease, id string) error {
	code, stdout, stderr, err := r.ExecDetached(ctx, lease, []string{"python3", "-c", processStopScript, id}, nil, "/",
		r.defaultTimeout)
	if err != nil {
		return err
	}
	switch {
	case code == 1 && strings.HasPrefix(stdout, unknownProcess):
		return fmt.Errorf("process %s: %w", id, ErrNoProcess)
	case code != 0:
		return fmt.Errorf("process_stop exited with %d: %s", code, firstLine(stderr+stdout))
	}
	return nil
}
