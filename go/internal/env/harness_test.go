// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/seal"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The tests' tenants.
const (
	tenantA = "acme"
	tenantB = "other"
)

// stageSpec is a spec the real blueprints accept: two image services
// (one idle behind the root route), a release job, postgres and nats, a
// secret.
const stageSpec = `{
  "services": {
    "api": {"image": "ghcr.io/acme/api:1", "command": ["/api"],
      "env": {"DB": {"addon": "pg", "key": "url", "role": "api_svc", "database": "app"}, "TOKEN": {"secret": "token"},
        "Q": {"addon": "q", "key": "url"}, "ADDR": "[::]:8080"},
      "ports": [{"name": "http", "port": 8080}], "replicas": 2, "readiness": {"http": {"port": "http", "path": "/readyz"}}},
    "web": {"image": "ghcr.io/acme/web:1", "ports": [{"name": "http", "port": 3000}], "idle": "15m",
      "env": {"API": {"service": "api", "port": "http", "key": "url"}}}
  },
  "jobs": {"migrate": {"image": "ghcr.io/acme/api:1", "command": ["/api", "migrate"],
    "env": {"DB": {"addon": "pg", "key": "url", "role": "api_svc", "database": "app"}}}},
  "addons": {"pg": {"type": "postgres", "version": "17", "options": {"databases": ["app"], "roles": ["api_svc"]}},
    "q": {"type": "nats", "version": "2"}},
  "routes": {"api": {"path": "/api", "service": "api", "port": "http", "strip": true},
    "root": {"path": "/", "service": "web", "port": "http"}}
}`

// fakeEngine is the sandbox engine of the service's tests: it resolves
// images from their names and records what it is asked.
type fakeEngine struct {
	mu       sync.Mutex
	bad      map[string]string
	resolved []string
	// restartErr is Restart's answer.
	restartErr error
}

func digestOf(image string) string {
	sum := sha256.Sum256([]byte(image))
	repo, _, _ := strings.Cut(image, ":")
	return repo + "@sha256:" + hex.EncodeToString(sum[:])
}

func (f *fakeEngine) Ensure(context.Context, podid.ID) (model.Lease, error) {
	return model.Lease{}, nil
}
func (f *fakeEngine) Delete(context.Context, string, podid.ID) (store.Sandbox, bool, error) {
	return store.Sandbox{}, false, nil
}
func (f *fakeEngine) Restart(context.Context, podid.ID, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restartErr
}
func (f *fakeEngine) ResolveImage(_ context.Context, image string, _ model.RegistryCredentials) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, image)
	if e, ok := f.bad[image]; ok {
		return "", errors.New(e)
	}
	return digestOf(image), nil
}
func (f *fakeEngine) Acquire(_ context.Context, id podid.ID, _ model.HoldKind) (model.Lease, func(), error) {
	return model.Lease{SandboxID: id}, func() {}, nil
}
func (f *fakeEngine) Touch(podid.ID)                       {}
func (f *fakeEngine) Sync(context.Context, podid.ID) error { return nil }
func (f *fakeEngine) State(context.Context, podid.ID) (string, string, error) {
	return model.StateRunning, "n1", nil
}

var _ Engine = (*fakeEngine)(nil)

// forward is a request the fake port door forwarded.
type forward struct {
	sandbox       podid.ID
	port          int
	path, rawPath string
	query         string
	authz, host   string
	// credentials are the headers the door was told to strip.
	credentials []string
}

// fakePorts is the port door: ForwardTo answers 200 with what it was
// asked.
type fakePorts struct {
	mu    sync.Mutex
	calls []forward
}

func (f *fakePorts) Probe(context.Context, model.Lease, int, string) (int, error) {
	return 0, errors.New("no probes here")
}
func (f *fakePorts) DialPod(context.Context, model.Lease, int) (net.Conn, error) {
	return nil, errors.New("no dials here")
}
func (f *fakePorts) ForwardTo(w http.ResponseWriter, r *http.Request, id podid.ID, port int, path, rawPath string,
	credentials []string) {
	f.mu.Lock()
	f.calls = append(f.calls, forward{id, port, path, rawPath, r.URL.RawQuery, r.Header.Get("Authorization"), r.Host,
		credentials})
	f.mu.Unlock()
	_, _ = fmt.Fprintf(w, "%s %d %s", id, port, path)
}

func (f *fakePorts) last(t *testing.T) forward {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("nothing was forwarded")
	}
	return f.calls[len(f.calls)-1]
}

var _ Ports = (*fakePorts)(nil)

// harness is a service over a fresh database.
type harness struct {
	t      *testing.T
	d      *db.DB
	st     *store.Store
	svc    *Service
	eng    *fakeEngine
	ports  *fakePorts
	sealer *seal.Sealer
}

type harnessOptions struct {
	noSealer bool
	noPorts  bool
	// appsZone is Deps.AppsZone.
	appsZone string
}

func newHarness(t *testing.T, o harnessOptions) *harness {
	t.Helper()
	d := dbtest.New(t)
	st := store.New(d)
	h := &harness{t: t, d: d, st: st, eng: &fakeEngine{bad: map[string]string{}}, ports: &fakePorts{}}
	deps := Deps{InstanceID: "i1", DB: d, Store: st, Engine: h.eng, AppsZone: o.appsZone, Log: dbtest.Logger(t)}
	if !o.noSealer {
		sl, err := seal.New(make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		h.sealer, deps.Sealer = sl, sl
	}
	if !o.noPorts {
		deps.Ports = h.ports
	}
	svc, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	for _, tn := range []string{tenantA, tenantB} {
		if _, _, err := st.CreateTenant(t.Context(), tn); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"n1", "n2"} {
		if _, err := st.InsertNode(t.Context(), n); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *harness) ctx() context.Context { return h.t.Context() }

// create creates an environment of tenantA.
func (h *harness) create(name string, labels map[string]string) EnvStatus {
	h.t.Helper()
	st, created, err := h.svc.CreateEnv(h.ctx(), tenantA, CreateEnvRequest{Name: name, Labels: labels})
	if err != nil || !created {
		h.t.Fatalf("create %s: %v %v", name, created, err)
	}
	return st
}

// putSecret sets a secret of tenantA's environment.
func (h *harness) putSecret(env, name, value string) int {
	h.t.Helper()
	v, err := h.svc.PutSecret(h.ctx(), tenantA, env, name, PutSecretRequest{Value: value, Author: "tester"})
	if err != nil {
		h.t.Fatalf("put secret %s: %v", name, err)
	}
	return v.Version
}

// apply applies a spec to tenantA's environment and wants a release.
func (h *harness) apply(env, raw string, base int) ApplyAnswer {
	h.t.Helper()
	a, err := h.svc.Apply(h.ctx(), tenantA, env, ApplyRequest{Spec: json.RawMessage(raw), Base: base, Author: "tester"})
	if err != nil {
		h.t.Fatalf("apply on %d: %v", base, describe(err))
	}
	return a
}

// describe is an error with an invalid spec's findings.
func describe(err error) string {
	var ise *InvalidSpecError
	if errors.As(err, &ise) {
		var b strings.Builder
		b.WriteString(err.Error())
		for _, f := range ise.Findings {
			fmt.Fprintf(&b, "\n  %s %s: %s", f.Severity, f.Path, f.Problem)
		}
		return b.String()
	}
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// env reads an environment row of tenantA.
func (h *harness) env(ref string) store.Environment {
	h.t.Helper()
	e, err := resolveEnv(h.ctx(), h.st, tenantA, ref, store.NoLock)
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

// exec runs SQL.
func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.d.Pool.Exec(h.ctx(), sql, args...); err != nil {
		h.t.Fatalf("%s: %v", sql, err)
	}
}

// instance inserts an instance row of an environment with its
// controller's row: a sandbox in state on node with address.
func (h *harness) instance(envID uuid.UUID, kind, object string, replica, release int, phase, state, node, address string) podid.ID {
	h.t.Helper()
	id := podid.Mint()
	zero := 0
	if _, _, err := h.st.CreateSandbox(h.ctx(), store.NewSandbox{ID: id, Owner: tenantA, Image: "img", VCPUs: 1, MemoryMiB: 512,
		EnvironmentID: envID, PodTemplate: &model.PodTemplate{SecurityGroups: []string{model.EnvGroupID(envID)}},
		IdleAfterS: &zero}); err != nil {
		h.t.Fatal(err)
	}
	var nodeArg any
	if node != "" {
		nodeArg = node
	}
	h.exec(`UPDATE sandboxes SET client_state = $2, node = $3, address = $4 WHERE id = $1`, id, state, nodeArg, address)
	if _, err := h.st.InsertEnvInstance(h.ctx(), store.EnvInstance{SandboxID: id, EnvID: envID, Kind: kind, Object: object,
		Replica: replica, TemplateHash: "h", Release: release, Phase: phase}); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// sandbox inserts an ordinary sandbox of tenant, attachable or not, under
// a short ID.
func (h *harness) sandbox(tenant string, attachable bool) podid.ID {
	h.t.Helper()
	return h.sandboxWithID(tenant, attachable, podid.Mint())
}

// legacySandbox inserts a sandbox as sandbox does under a UUID: one made
// before ADR 0019.
func (h *harness) legacySandbox(tenant string, attachable bool) podid.ID {
	h.t.Helper()
	return h.sandboxWithID(tenant, attachable, podid.ID(uuid.NewString()))
}

func (h *harness) sandboxWithID(tenant string, attachable bool, id podid.ID) podid.ID {
	h.t.Helper()
	n := store.NewSandbox{ID: id, Owner: tenant, Image: "img", VCPUs: 1, MemoryMiB: 512}
	if attachable {
		n.PodTemplate = &model.PodTemplate{Command: []string{"sleep", "infinity"},
			SecurityGroups: []string{model.SandboxGroupID(id)}}
	}
	if _, _, err := h.st.CreateSandbox(h.ctx(), n); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// statusOf is an error's HTTP status and message as WriteError answers.
func statusOf(err error) (int, string) {
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return ae.Status, ae.Msg
	}
	var be bodyError
	if errors.As(err, &be) {
		return be.Status(), be.Error()
	}
	if err == nil {
		return 0, ""
	}
	return 500, err.Error()
}

// wantStatus checks an error's status and a part of its message.
func wantStatus(t *testing.T, err error, status int, msg string) {
	t.Helper()
	got, text := statusOf(err)
	if got != status || !strings.Contains(text, msg) {
		t.Fatalf("error %d %q (%v), want %d with %q", got, text, err, status, msg)
	}
}

// hasFinding reports a finding of severity at path whose problem has
// problem.
func hasFinding(fs []spec.Finding, sev spec.Severity, path, problem string) bool {
	for _, f := range fs {
		if f.Severity == sev && f.Path == path && strings.Contains(f.Problem, problem) {
			return true
		}
	}
	return false
}

// waitHint waits for a ferun_leader payload.
func waitHint(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case p := <-ch:
			if p == want {
				return
			}
		case <-deadline:
			t.Fatalf("no hint %q", want)
		}
	}
}
