// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The pods of an environment's job run and instance, and of a tenant's
// job, are created with their log labels (ADR 0026), kept on their rows;
// a tenant's job's retention becomes ferun.retention_days.
func TestLogLabels(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, jobSpec("v1", 0))
	run := runningJob(t, h, env.ID)
	want := map[string]string{model.LogTenant: testTenant, model.LogInstance: run.String(),
		model.LogEnvironment: env.ID.String(), model.LogKind: spec.KindJob, model.LogObject: "migrate",
		model.LogReplica: "0", model.LogDeployment: strconv.Itoa(r1), model.LogAttempt: "1"}
	if got := n1.LogLabels(run); !maps.Equal(got, want) {
		t.Fatalf("the run's create: %v, want %v", got, want)
	}
	if sb, err := h.st.GetSandbox(bg, run); err != nil || !maps.Equal(sb.LogLabels, want) {
		t.Fatalf("the run's row: %v %v", sb.LogLabels, err)
	}
	if err := n1.Script().ExitContainer(run, "main", 0); err != nil {
		t.Fatal(err)
	}
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	api := h.waitRows(env.ID, spec.KindService, "api", 1, 10*time.Second, "running", running)[0].Sandbox.ID
	want = map[string]string{model.LogTenant: testTenant, model.LogInstance: api.String(),
		model.LogEnvironment: env.ID.String(), model.LogKind: spec.KindService, model.LogObject: "api",
		model.LogReplica: "0", model.LogDeployment: strconv.Itoa(r1)}
	if got := n1.LogLabels(api); !maps.Equal(got, want) {
		t.Fatalf("the service's create: %v, want %v", got, want)
	}

	rs := tenantJobSpecOf(time.Minute)
	rs.LogRetentionDays = 30
	job := h.createTenantJob(rs)
	if job.LogRetentionDays != 30 {
		t.Fatalf("the job's retention %d", job.LogRetentionDays)
	}
	h.runningPod(job.ID)
	want = map[string]string{model.LogTenant: testTenant, model.LogInstance: job.ID.String(),
		model.LogJob: job.ID.String(), model.LogAttempt: "1", model.LogRetentionDays: "30"}
	if got := n1.LogLabels(job.ID); !maps.Equal(got, want) {
		t.Fatalf("the job's create: %v, want %v", got, want)
	}
	// A sandbox of the tenant's own has none.
	sb, err := h.st.CreateMintedSandbox(bg, func(id podid.ID) store.NewSandbox {
		return store.NewSandbox{ID: id, Owner: testTenant, Image: testImage, VCPUs: 1, MemoryMiB: 512}
	})
	if err != nil || len(sb.LogLabels) != 0 || sb.LogLabels == nil {
		t.Fatalf("a tenant's sandbox: %v %v", sb.LogLabels, err)
	}
}

// redactValues are each value and its userinfo escaping, at least 4
// bytes, once, longest first; redact replaces exactly those.
func TestRedactValues(t *testing.T) {
	got := redactValues([]string{"p@ss:w0rd", "abc", "plain-value", "p@ss:w0rd", ""})
	want := []string{"p%40ss%3Aw0rd", "plain-value", "p@ss:w0rd"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	line := "user:p%40ss%3Aw0rd@db and p@ss:w0rd and abc"
	if r := redact(line, []string{"p@ss:w0rd", "abc"}); r != "user:"+redactedMark+"@db and "+redactedMark+" and abc" {
		t.Fatalf("redact %q", r)
	}
}

// A create with log labels names the values its node redacts
// (log_redact), asked anew at the create and stored nowhere: an
// environment's tenant secrets with their userinfo forms, a tenant's
// job's secrets and image credentials.
func TestLogRedact(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	const token = "s3cr3t/t0ken"
	h.putSecret(env.ID, "token", token)
	r1 := h.apply(env.ID, fmt.Sprintf(`{"services":{"web":{"image":%q,"env":{"TOKEN":{"secret":"token"}}}}}`, testImage))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	web := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "running", running)[0].Sandbox.ID
	if got := n1.LogRedact(web); !slices.Equal(got, []string{"s3cr3t%2Ft0ken", token}) {
		t.Fatalf("the instance's create: %q", got)
	}
	var stored int
	if err := h.d.Pool.QueryRow(bg, `SELECT count(*) FROM sandboxes WHERE log_labels::text LIKE '%t0ken%'`).Scan(&stored); err != nil ||
		stored != 0 {
		t.Fatalf("a secret on a row: %d %v", stored, err)
	}

	rs := tenantJobSpecOf(time.Minute)
	rs.Secrets = map[string]string{"GIT_TOKEN": "git-secret-value"}
	rs.ImageCredentials = &model.RegistryCredentials{Username: "build", Password: "registry-password"}
	job := h.createTenantJob(rs)
	h.runningPod(job.ID)
	if got := n1.LogRedact(job.ID); !slices.Equal(got, []string{"registry-password", "git-secret-value", "build"}) {
		t.Fatalf("the job's create: %q", got)
	}
	// A job without secrets names none.
	plain := h.createTenantJob(tenantJobSpecOf(time.Minute))
	h.runningPod(plain.ID)
	if got := n1.LogRedact(plain.ID); len(got) != 0 {
		t.Fatalf("a job without secrets: %q", got)
	}
}

// A log's partial entries are joined with the next entries of their
// source up to one that is not partial, the joined line cut to the
// line budget; a line still open at the end goes in as far as it came.
func TestLogTailParts(t *testing.T) {
	tl := logTail{b: logBudget{line: 16, total: 1 << 10}, max: 10}
	tl.addPart("a1 ", "stdout", true)
	tl.addPart("b", "stderr", false)
	tl.addPart("a2 ", "stdout", true)
	tl.addPart("c1 ", "stderr", true)
	tl.addPart("a3", "stdout", false)
	tl.addPart(strings.Repeat("y", 20), "stdout", true)
	tl.addPart(strings.Repeat("y", 20), "stdout", false)
	tl.addPart("c2", "stderr", true)
	want := []string{"b", "a1 a2 a3", "yyyyyyy" + cutMark, "c1 c2"}
	if got := tl.result(); !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The node's partial entries are joined as the sink's are.
func TestNodeLogParts(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	id := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0].Sandbox.ID
	for _, e := range []struct {
		text    string
		source  containerv1.LogEntry_Source
		partial bool
	}{{"GET / ", containerv1.LogEntry_STDOUT, true}, {"oops", containerv1.LogEntry_STDERR, false},
		{"200", containerv1.LogEntry_STDOUT, false}} {
		if err := n1.Script().AppendLogEntry(id, "main", e.text, e.source, e.partial); err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := h.c.LogTail(bg, env.ID, spec.KindService, "web", 0, 10)
	if err != nil || !slices.Equal(got[len(got)-2:], []string{"oops", "GET / 200"}) {
		t.Fatalf("logs %q %v", got, err)
	}
}

// fakeSink is the sink's GET /api/v1/logs: lines (oldest first) per
// selector, pages of at most limit, newest first; down answers 503.
type fakeSink struct {
	mu    sync.Mutex
	lines map[string][]sinkLine
	down  bool
	asked []string
}

func (f *fakeSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	f.asked = append(f.asked, r.Header.Get("Authorization")+" "+q.Encode())
	if f.down || r.URL.Path != "/api/v1/logs" || r.Header.Get("Authorization") != "Bearer tok" ||
		q.Get("direction") != "backward" || q.Get("container") != "main" || q.Get("since") == "" {
		http.Error(w, "no", http.StatusServiceUnavailable)
		return
	}
	var all []sinkLine
	for _, k := range []string{"instance", "job"} {
		if v := q.Get(k); v != "" {
			all = f.lines[k+"="+v]
		}
	}
	end := len(all)
	if c := q.Get("cursor"); c != "" {
		end, _ = strconv.Atoi(c)
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	start := max(end-limit, 0)
	out := struct {
		Lines  []sinkLine `json:"lines"`
		Cursor string     `json:"cursor"`
	}{Lines: append([]sinkLine{}, all[start:end]...)}
	if start > 0 {
		out.Cursor = strconv.Itoa(start)
	}
	_ = json.NewEncoder(w).Encode(out)
}

// set gives the stream of key whole stdout lines.
func (f *fakeSink) set(key string, lines []string, down bool) {
	var ls []sinkLine
	for _, l := range lines {
		ls = append(ls, sinkLine{Text: l, Source: "stdout"})
	}
	f.setLines(key, ls, down)
}

func (f *fakeSink) setLines(key string, lines []sinkLine, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines[key], f.down = lines, down
}

// With --sink-url, the logs calls read the sink with the caller's token:
// an instance's lines, in pages, also those of a paused one (no
// ErrPaused), a job run's and a tenant's job's (redacted of its secrets);
// without a token, without lines or with the sink down, the node's.
func TestSinkLogs(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	sink := &fakeSink{lines: map[string][]sinkLine{}}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	h.c.d.SinkURL = srv.URL + "/"
	ctx := auth.WithTenant(bg, auth.Tenant{Name: testTenant, Token: "tok"})

	env := h.newEnv("stage")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"services":{"web":{"image":%q},"idler":{"image":%q,"idle":"15m"}}}`,
		testImage, testImage))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	web := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0].Sandbox.ID
	_ = n1.Script().AppendLog(web, "main", "from the node")
	logs := func(ctx context.Context, name string, lines int) []string {
		t.Helper()
		out, source, err := h.c.LogTail(ctx, env.ID, spec.KindService, name, 0, lines)
		if err != nil || source != SourceContainer {
			t.Fatalf("logs of %s: %v %s", name, err, source)
		}
		return out
	}
	// No lines in the sink: the node's.
	if got := logs(ctx, "web", 10); !slices.Equal(got, []string{"from the node"}) {
		t.Fatalf("no sink lines: %q", got)
	}
	var many []string
	for i := range 1200 {
		many = append(many, fmt.Sprintf("line %d", i))
	}
	sink.set("instance="+web.String(), many, false)
	if got := logs(ctx, "web", 1100); len(got) != 1100 || got[0] != "line 100" || got[1099] != "line 1199" {
		t.Fatalf("sink lines: %d %q…%q", len(got), got[0], got[len(got)-1])
	}
	if got := logs(ctx, "web", 2); !slices.Equal(got, []string{"line 1198", "line 1199"}) {
		t.Fatalf("a short tail: %q", got)
	}
	// A line's partial parts are joined, across pages too, with other
	// sources' lines between them.
	sink.setLines("instance="+web.String(), append([]sinkLine{
		{Text: "first ", Source: "stdout", Partial: true}, {Text: "an error", Source: "stderr"},
		{Text: "second ", Source: "stdout", Partial: true}}, append(slices.Repeat([]sinkLine{{Text: "x", Source: "stdout",
		Partial: true}}, sinkPage), sinkLine{Text: "end", Source: "stdout"})...), false)
	if got := logs(ctx, "web", 2); len(got) != 2 || got[0] != "an error" ||
		got[1] != "first second "+strings.Repeat("x", sinkPage)+"end" {
		t.Fatalf("joined: %q", got)
	}
	// Without the caller's token, or with the sink down: the node's.
	if got := logs(bg, "web", 10); !slices.Equal(got, []string{"from the node"}) {
		t.Fatalf("no token: %q", got)
	}
	sink.set("instance="+web.String(), many, true)
	if got := logs(ctx, "web", 10); !slices.Equal(got, []string{"from the node"}) {
		t.Fatalf("the sink down: %q", got)
	}

	// A paused instance: ErrPaused unless the sink has its lines.
	idler := h.waitRows(env.ID, spec.KindService, "idler", 1, 10*time.Second, "ready", ready)[0].Sandbox.ID
	if err := h.eng.Pause(bg, idler); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "paused", func() bool {
		sb, _ := h.st.GetSandbox(bg, idler)
		return sb.ClientState == model.StatePaused && sb.Op == ""
	})
	if _, _, err := h.c.LogTail(ctx, env.ID, spec.KindService, "idler", 0, 5); !errors.Is(err, ErrPaused) {
		t.Fatalf("paused without sink lines: %v", err)
	}
	sink.set("instance="+idler.String(), []string{"before the pause"}, false)
	if got := logs(ctx, "idler", 5); !slices.Equal(got, []string{"before the pause"}) {
		t.Fatalf("paused: %q", got)
	}

	// A tenant's job: redacted of its secrets.
	rs := tenantJobSpecOf(time.Minute)
	rs.Secrets = map[string]string{"GIT_TOKEN": "s3cr3t-token-value"}
	job := h.createTenantJob(rs)
	h.runningPod(job.ID)
	sink.set("job="+job.ID.String(), []string{"cloning with s3cr3t-token-value", "done"}, false)
	got, err := h.c.TenantJobLog(ctx, testTenant, job.ID, 10)
	if err != nil || len(got) != 2 || got[0] != "cloning with "+redactedMark || got[1] != "done" {
		t.Fatalf("the job's log: %q %v", got, err)
	}
	// Once it has ended: the sink's lines as the node redacted them, else
	// its kept tail.
	if _, err := h.c.CancelTenantJob(bg, testTenant, job.ID); err != nil {
		t.Fatal(err)
	}
	sink.set("job="+job.ID.String(), []string{"cloning with [redacted]", "done", "cancelled"}, false)
	got, err = h.c.TenantJobLog(ctx, testTenant, job.ID, 10)
	if err != nil || !slices.Equal(got, []string{"cloning with [redacted]", "done", "cancelled"}) {
		t.Fatalf("the ended job's log: %q %v", got, err)
	}
	sink.set("job="+job.ID.String(), nil, false)
	if got, err = h.c.TenantJobLog(ctx, testTenant, job.ID, 10); err != nil || slices.Contains(got, "cancelled") {
		t.Fatalf("the ended job's kept tail: %q %v", got, err)
	}
	sink.mu.Lock()
	asked := slices.Clone(sink.asked)
	sink.mu.Unlock()
	if len(asked) == 0 || asked[0][:len("Bearer tok ")] != "Bearer tok " {
		t.Fatalf("asked %q", asked)
	}
}

// A job run's log is the sink's, its run's instance's, also once the run
// has ended and its row is gone.
func TestSinkJobRunLog(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	sink := &fakeSink{lines: map[string][]sinkLine{}}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	h.c.d.SinkURL = srv.URL
	ctx := auth.WithTenant(bg, auth.Tenant{Name: testTenant, Token: "tok"})
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, jobSpec("v1", 0))
	run := finishRun(t, h, n1, env.ID, 0, "kept")
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	got, _, err := h.c.LogTail(ctx, env.ID, spec.KindJob, "migrate", 0, 10)
	if err != nil || !slices.Equal(got, []string{"kept"}) {
		t.Fatalf("the kept tail: %q %v", got, err)
	}
	sink.set("instance="+run.String(), []string{"first", "kept"}, false)
	got, _, err = h.c.LogTail(ctx, env.ID, spec.KindJob, "migrate", 0, 10)
	if err != nil || !slices.Equal(got, []string{"first", "kept"}) {
		t.Fatalf("the sink's: %q %v", got, err)
	}
}
