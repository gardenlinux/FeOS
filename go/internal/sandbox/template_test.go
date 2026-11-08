// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// secrets is a fake secret resolver: values by environment, name and
// version, and the calls it answered.
type secrets struct {
	mu     sync.Mutex
	values map[string]string
	calls  int
	fail   error
}

func secretKeyOf(env uuid.UUID, name string, version int) string {
	return fmt.Sprintf("%s/%s/%d", env, name, version)
}

func (s *secrets) set(env uuid.UUID, name string, version int, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[string]string{}
	}
	s.values[secretKeyOf(env, name, version)] = value
}

func (s *secrets) resolve(_ context.Context, row store.Sandbox, name string, version int) (string, error) {
	env := row.EnvironmentID
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.fail != nil {
		return "", s.fail
	}
	v, ok := s.values[secretKeyOf(env, name, version)]
	if !ok {
		return "", fmt.Errorf("no secret %s version %d", name, version)
	}
	return v, nil
}

// environment inserts an environment of the test tenant.
func (h *harness) environment() uuid.UUID {
	h.t.Helper()
	id := uuid.New()
	h.exec("INSERT INTO environments (id, owner, name) VALUES ($1, $2, $3)", id, testTenant, "e"+id.String()[:8])
	return id
}

// instance inserts an instance row of env with the template.
func (h *harness) instance(env uuid.UUID, t *model.PodTemplate, mod ...func(*store.NewSandbox)) store.Sandbox {
	h.t.Helper()
	n := store.NewSandbox{ID: podid.Mint(), Owner: testTenant, Image: testImage, VCPUs: 1, MemoryMiB: 512,
		EnvironmentID: env, Instance: true, PodTemplate: t}
	for _, m := range mod {
		m(&n)
	}
	sb, created, err := h.st.CreateSandbox(bg, n)
	if err != nil || !created {
		h.t.Fatalf("create instance: %v (created %v)", err, created)
	}
	return sb
}

// webTemplate is a service's template of env: a command, a literal, a
// secret and a URL with an escaped password, the env group, and labels
// one of which a sandbox label overrides.
func webTemplate(env uuid.UUID) *model.PodTemplate {
	return &model.PodTemplate{
		Command: []string{"/app/web", "serve"},
		Env: map[string]model.TemplateValue{
			"PLAIN": model.Literal("plain value"),
			"TOKEN": model.SecretValue("internal-token", 3, model.EscapeNone),
			"DATABASE_URL": {Parts: []model.TemplatePart{
				{Value: "postgres://web:"},
				{Secret: "addon.postgres.role.web.password", Version: 1, Escape: model.EscapeURL},
				{Value: "@[fd6b::5]:5432/app?sslmode=disable"},
			}},
		},
		SecurityGroups: []string{model.EnvGroupID(env)},
		Labels: map[string]string{model.LabelEnvironment: env.String(), model.LabelObject: "service/web",
			model.LabelTemplate: "hash", LabelSandbox: "not-this-one"},
	}
}

// webSecrets are the values webTemplate's secrets resolve to.
func webSecrets(env uuid.UUID) *secrets {
	s := &secrets{}
	s.set(env, "internal-token", 3, "tok en")
	s.set(env, "addon.postgres.role.web.password", 1, "p@ss:w/rd?")
	return s
}

// wantWebSpec checks a pod spec built from webTemplate.
func wantWebSpec(t *testing.T, spec *podv1.PodSpec, sb store.Sandbox) {
	t.Helper()
	c := spec.GetContainers()
	if len(c) != 1 || c[0].GetName() != "main" || c[0].GetImageRef() != testImage ||
		strings.Join(c[0].GetCommand(), " ") != "/app/web serve" {
		t.Fatalf("containers %v", c)
	}
	env := c[0].GetEnv()
	wantURL := "postgres://web:p%40ss%3Aw%2Frd%3F@[fd6b::5]:5432/app?sslmode=disable"
	if len(env) != 3 || env["PLAIN"] != "plain value" || env["TOKEN"] != "tok en" || env["DATABASE_URL"] != wantURL {
		t.Fatalf("env %v", env)
	}
	u, err := url.Parse(env["DATABASE_URL"])
	if pw, _ := u.User.Password(); err != nil || pw != "p@ss:w/rd?" || u.User.Username() != "web" {
		t.Fatalf("the URL does not parse back: %v %v", u, err)
	}
	if g := spec.GetNetwork().GetSecurityGroups(); len(g) != 1 || g[0] != model.EnvGroupID(sb.EnvironmentID) {
		t.Fatalf("groups %v", g)
	}
	l := spec.GetLabels()
	if l[LabelSandbox] != sb.ID.String() || l[LabelTenant] != testTenant || l[LabelIncarnation] != sb.Incarnation.String() ||
		l[model.LabelObject] != "service/web" || l[model.LabelEnvironment] != sb.EnvironmentID.String() || len(l) != 6 {
		t.Fatalf("labels %v", l)
	}
	if spec.GetResources().GetVcpus() != uint32(sb.VCPUs) || spec.GetResources().GetMemoryMib() != uint64(sb.MemoryMiB) {
		t.Fatalf("resources %v", spec.GetResources())
	}
}

// createRequests are the CreatePodRequests among the calls.
func (c *calls) createRequests() []*podv1.CreatePodRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*podv1.CreatePodRequest
	for _, r := range c.reqs {
		if cr, ok := r.(*podv1.CreatePodRequest); ok {
			out = append(out, cr)
		}
	}
	return out
}

// A template row is created through Ensure from its template: the
// command, the resolved env (a URL's password escaped), the groups and
// the labels, the sandbox's winning.
func TestTemplateCreateSendsItsSpec(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	env := h.environment()
	sec := webSecrets(env)
	h.eng().SetSecretResolver(sec.resolve)
	creates := h.watchCalls("n1", podv1.PodService_CreatePod_FullMethodName, false)
	sb := h.instance(env, webTemplate(env))
	h.ensure(sb.ID)
	row := h.waitState(sb.ID, model.StateRunning, 10*time.Second)
	if row.PodTemplate == nil || row.EnvironmentID != env || !row.Instance || row.IsOwn() {
		t.Fatalf("row %+v", row)
	}
	reqs := creates.createRequests()
	if len(reqs) != 1 {
		t.Fatalf("%d creates", len(reqs))
	}
	wantWebSpec(t, reqs[0].GetSpec(), row)
	info := h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
	wantWebSpec(t, info.GetSpec(), row)
	// The same spec on every build: a retry or takeover creates the pod it
	// made.
	again, err := buildPodSpec(bg, row, sec.resolve, nil)
	if err != nil || !proto.Equal(again, reqs[0].GetSpec()) {
		t.Fatalf("a second build differs: %v", err)
	}
}

// A takeover of a dead instance's create of a template row sends the
// same spec, which the node takes for the pod it has.
func TestTemplateTakeoverSendsTheSameSpec(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	env := h.environment()
	sec := webSecrets(env)
	h.eng().SetSecretResolver(sec.resolve)
	sb := h.instance(env, webTemplate(env))
	h.deadInstance("dead")
	h.exec(`UPDATE sandboxes SET image_ref = $2, node = 'n1', op = 'create', op_id = gen_random_uuid(), op_owner = 'dead',
		op_deadline = now() + interval '1 minute', op_target = 'n1', op_from_state = 'pending', client_state = 'creating',
		gen = gen + 1 WHERE id = $1`, sb.ID, testImage)
	h.exec("INSERT INTO bookings (pod_id, node, vcpus, memory_mib, gated) VALUES ($1, 'n1', 1, 512, true)", sb.ID)
	row := h.row(sb.ID)
	first, err := buildPodSpec(bg, row, sec.resolve, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pods("n1").CreatePod(bg, &podv1.CreatePodRequest{PodId: sb.ID.String(), Spec: first}); err != nil {
		t.Fatal(err)
	}
	creates := h.watchCalls("n1", podv1.PodService_CreatePod_FullMethodName, false)
	h.ensure(sb.ID)
	row = h.waitState(sb.ID, model.StateRunning, 10*time.Second)
	h.activityNamed(sb.ID, activity.KindLifecycleStart, "create on node n1 (taken over from instance dead)")
	reqs := creates.createRequests()
	if len(reqs) != 1 || !proto.Equal(reqs[0].GetSpec(), first) {
		t.Fatalf("the takeover sent %d creates, the same spec: %v", len(reqs), len(reqs) == 1 && proto.Equal(reqs[0].GetSpec(), first))
	}
	wantWebSpec(t, h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING).GetSpec(), row)
}

func TestEscapeUserinfo(t *testing.T) {
	cases := map[string]string{
		"":                           "",
		"abcXYZ019-._~":              "abcXYZ019-._~",
		"!$&'()*+,;=":                "!$&'()*+,;=",
		"a:b":                        "a%3Ab",
		"@/?#[]%":                    "%40%2F%3F%23%5B%5D%25",
		"sp ace\"<>\\^`{|}":          "sp%20ace%22%3C%3E%5C%5E%60%7B%7C%7D",
		"\x00\x1f\x7f":               "%00%1F%7F",
		"ü€":                         "%C3%BC%E2%82%AC",
		"0123456789abcdef0123456789": "0123456789abcdef0123456789",
	}
	for in, want := range cases {
		if got := escapeUserinfo(in); got != want {
			t.Errorf("escapeUserinfo(%q) = %q, want %q", in, got, want)
		}
		// Every escaped value is userinfo that parses back to itself.
		u, err := url.Parse("postgres://" + escapeUserinfo(in) + ":" + escapeUserinfo(in) + "@[fd00::1]:5432/db")
		if err != nil {
			t.Errorf("parse with %q: %v", in, err)
			continue
		}
		if pw, _ := u.User.Password(); u.User.Username() != in || pw != in {
			t.Errorf("%q came back as %q:%q", in, u.User.Username(), pw)
		}
	}
}

func TestBuildPodSpec(t *testing.T) {
	env := uuid.New()
	sb := store.Sandbox{ID: podid.Mint(), Incarnation: uuid.New(), Owner: testTenant, ImageRef: testImage, VCPUs: 2,
		MemoryMiB: 1024, EnvironmentID: env, Instance: true}

	// No template: the sandbox's own pod.
	spec, err := buildPodSpec(bg, sb, nil, nil)
	if err != nil || strings.Join(spec.GetContainers()[0].GetCommand(), " ") != "sleep infinity" ||
		len(spec.GetNetwork().GetSecurityGroups()) != 0 || len(spec.GetLabels()) != 3 {
		t.Fatalf("plain spec %v %v", spec, err)
	}

	// An empty command sends none (the image's entrypoint and cmd run);
	// volumes and mounts; no env without one.
	vol := uuid.New()
	sb.PodTemplate = &model.PodTemplate{
		Volumes: []model.TemplateVolume{{Name: "data", VolumeID: vol}},
		Mounts:  []model.TemplateMount{{Volume: "data", Path: "/var/lib/postgresql/data", ReadOnly: true}},
	}
	spec, err = buildPodSpec(bg, sb, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := spec.GetContainers()[0]
	if c.GetCommand() != nil || c.GetEnv() != nil || len(spec.GetVolumes()) != 1 || spec.GetVolumes()[0].GetVolumeId() != vol.String() ||
		spec.GetVolumes()[0].GetName() != "data" || len(c.GetVolumeMounts()) != 1 ||
		c.GetVolumeMounts()[0].GetPath() != "/var/lib/postgresql/data" || !c.GetVolumeMounts()[0].GetReadOnly() {
		t.Fatalf("spec %v", spec)
	}

	// A secret without a resolver, a resolver's failure, an unmapped
	// credential and a secret of a row without an environment fail.
	sb.PodTemplate = &model.PodTemplate{Env: map[string]model.TemplateValue{"P": model.SecretValue("pw", 1, "")}}
	if _, err := buildPodSpec(bg, sb, nil, nil); !errors.Is(err, errNoResolver) {
		t.Fatalf("no resolver: %v", err)
	}
	boom := errors.New("boom")
	sec := &secrets{fail: boom}
	if _, err := buildPodSpec(bg, sb, sec.resolve, nil); !errors.Is(err, boom) || !strings.Contains(err.Error(), "env P: secret pw version 1") {
		t.Fatalf("resolver failure: %v", err)
	}
	sb.PodTemplate = &model.PodTemplate{Env: map[string]model.TemplateValue{"P": model.CredentialValue("password")}}
	if _, err := buildPodSpec(bg, sb, sec.resolve, nil); err == nil || !strings.Contains(err.Error(), "unmapped credential") {
		t.Fatalf("credential: %v", err)
	}
	sb.Instance = false
	sb.PodTemplate = &model.PodTemplate{Env: map[string]model.TemplateValue{"P": model.SecretValue("pw", 1, "")}}
	if _, err := buildPodSpec(bg, sb, (&secrets{}).resolve, nil); err == nil || !strings.Contains(err.Error(), "no environment's") {
		t.Fatalf("no environment: %v", err)
	}

	// A secret named twice is resolved once.
	sb.Instance = true
	sec = &secrets{}
	sec.set(env, "pw", 1, "x")
	sb.PodTemplate = &model.PodTemplate{Env: map[string]model.TemplateValue{
		"A": model.SecretValue("pw", 1, ""),
		"B": {Parts: []model.TemplatePart{{Value: "u:"}, {Secret: "pw", Version: 1, Escape: model.EscapeURL}}},
	}}
	spec, err = buildPodSpec(bg, sb, sec.resolve, nil)
	if err != nil || spec.GetContainers()[0].GetEnv()["B"] != "u:x" || sec.calls != 1 {
		t.Fatalf("spec %v %v, %d calls", spec, err, sec.calls)
	}
}

// A restore sends the row's template's security groups and name tables,
// which replace the checkpointed spec's (ADR 0035), beside its checkpoint
// and log labels; a row without a template sends neither.
func TestRestoreRequest(t *testing.T) {
	env := uuid.New()
	id := podid.Mint()
	row := store.Sandbox{ID: id, CheckpointHead: "snapshots/x/1/head.json",
		LogLabels: map[string]string{"ferun.tenant": "acme"}, PodTemplate: model.SandboxTemplate(env, id.String(), false, nil)}
	h := &OpHandle{id: row.ID, row: row}
	req := restoreRequest(h)
	if req.GetPodId() != row.ID.String() || req.GetHeadKey() != row.CheckpointHead ||
		req.GetLogLabels()["ferun.tenant"] != "acme" {
		t.Fatalf("request %v", req)
	}
	if !slices.Equal(req.GetSecurityGroups(), row.PodTemplate.SecurityGroups) ||
		!slices.Equal(req.GetNameTables(), []string{model.EnvTableID(env)}) {
		t.Fatalf("groups %v, tables %v", req.GetSecurityGroups(), req.GetNameTables())
	}
	req.SecurityGroups[0] = "x"
	if row.PodTemplate.SecurityGroups[0] == "x" {
		t.Fatal("the request shares the template's groups")
	}
	h.row.PodTemplate = nil
	if req := restoreRequest(h); len(req.GetSecurityGroups()) != 0 || len(req.GetNameTables()) != 0 {
		t.Fatalf("without a template %v", req)
	}
}

// A template's egress by name reaches the pod's network (ADR 0034,
// revised); without it, the network restricts nothing.
func TestPodSpecEgressNames(t *testing.T) {
	env := uuid.New()
	id := podid.Mint()
	sb := store.Sandbox{ID: id, Owner: testTenant, ImageRef: testImage, VCPUs: 1, MemoryMiB: 512, EnvironmentID: env,
		PodTemplate: model.SandboxTemplate(env, id.String(), true, []string{"api.stripe.com", "*.example.com"})}
	spec, err := buildPodSpec(bg, sb, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := spec.GetNetwork()
	if !n.GetRestrictEgressNames() || !slices.Equal(n.GetEgressDomains(), egressDomains([]string{"api.stripe.com", "*.example.com"}, testImage, nil)) ||
		!slices.Equal(n.GetSecurityGroups(), []string{model.EnvGroupID(env), model.SandboxWorkloadGroupID(id.String())}) {
		t.Fatalf("network %v", n)
	}
	sb.PodTemplate = model.SandboxTemplate(env, id.String(), false, nil)
	if spec, err = buildPodSpec(bg, sb, nil, nil); err != nil || spec.GetNetwork().GetRestrictEgressNames() ||
		len(spec.GetNetwork().GetEgressDomains()) != 0 {
		t.Fatalf("internet %v %v", spec.GetNetwork(), err)
	}
}

// A restricted pod may reach the hosts its image is pulled from: the
// guest pulls over the pod's network.
func TestRegistryHosts(t *testing.T) {
	for ref, want := range map[string][]string{
		"docker.io/library/postgres@sha256:aa": {"*.docker.com", "*.docker.io", "*.r2.cloudflarestorage.com", "docker.io"},
		"postgres:17":                          {"*.docker.com", "*.docker.io", "*.r2.cloudflarestorage.com", "docker.io"},
		"ghcr.io/kino/api:1.4":                 {"*.githubusercontent.com", "ghcr.io"},
		"oci.fecode.dev/kino/api@sha256:bb":    {"oci.fecode.dev"},
		"registry.example:5000/x/y:1":          {"registry.example"},
		"localhost:5000/x":                     nil,
	} {
		if got := registryHosts(ref); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", ref, got, want)
		}
	}
	got := egressDomains([]string{"api.stripe.com", "ghcr.io"}, "ghcr.io/a/b:1", nil)
	if !slices.Equal(got, []string{"*.githubusercontent.com", "api.stripe.com", "ghcr.io"}) {
		t.Fatalf("egressDomains %v", got)
	}
}

// --registry-egress adds the names a registry's pulls talk to.
func TestRegistryEgress(t *testing.T) {
	got := egressDomains(nil, "oci.fecode.dev/kino/api@sha256:aa", map[string][]string{"oci.fecode.dev": {"*.your-objectstorage.com"}})
	if !slices.Equal(got, []string{"*.your-objectstorage.com", "oci.fecode.dev"}) {
		t.Fatalf("%v", got)
	}
}
