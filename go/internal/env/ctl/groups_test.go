// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// parseGroup reads the controller's groups in their canonical form: env
// and tenant's groups, ing groups (of a sandbox with a short ID, or one
// made before ADR 0019 with a UUID) and ferun-ingress; name tables are
// env groups' IDs only. sbx groups are gone (ADR 0035).
func TestParseGroup(t *testing.T) {
	envID := uuid.MustParse("0193e3a0-0000-7000-8000-000000000001")
	if ref, ok := parseGroup(model.EnvGroupID(envID)); !ok || ref != (groupRef{kind: groupEnv, env: envID}) {
		t.Errorf("parse env group: %+v %v", ref, ok)
	}
	if ref, ok := parseGroup(model.SecurityGroupID(envID)); !ok || ref != (groupRef{kind: groupTenant, sg: envID}) {
		t.Errorf("parse a tenant's group: %+v %v", ref, ok)
	}
	if _, ok := parseTable(model.SecurityGroupID(envID)); ok {
		t.Errorf("a tenant's group parsed as a name table")
	}
	short := podid.Mint()
	legacy := podid.ID(uuid.NewString())
	for _, id := range []podid.ID{short, legacy} {
		if ref, ok := parseGroup(model.IngressGroupID(id)); !ok || ref != (groupRef{kind: groupIng, sbx: id}) {
			t.Errorf("parse ing group of %s: %+v %v", id, ref, ok)
		}
		if _, ok := parseTable(model.IngressGroupID(id)); ok {
			t.Errorf("an ing group parsed as a name table")
		}
		if ref, ok := parseGroup("sbx-" + id.String()); ok {
			t.Errorf("an sbx group parsed: %+v", ref)
		}
	}
	if ref, ok := parseGroup(model.IngressPeersGroup); !ok || ref != (groupRef{kind: groupIngressPeers}) {
		t.Errorf("parse ferun-ingress: %+v %v", ref, ok)
	}
	if _, ok := parseTable(model.IngressPeersGroup); ok {
		t.Error("ferun-ingress parsed as a name table")
	}
	for _, bad := range []string{"default", "env-x", "sg-x", "sg-" + strings.ToUpper(envID.String()), "sg-", "sg-" + short.String(),
		"foo-" + uuid.NewString(), "env-" + short.String(),
		"ing-x", "ing-", "ing-" + strings.ToUpper(short.String()), "ing-" + short.String() + "0", "ing-" + envID.String()[:35],
		"ferun-ingressx", "ferun-ingress-", "ferun", "Ferun-ingress", "ing" + short.String(), "ingress-" + short.String()} {
		if ref, ok := parseGroup(bad); ok {
			t.Errorf("%s parsed: %+v", bad, ref)
		}
	}
}

// The env group is on a node before the pod that names it (the node
// checks), with the instances' addresses as members; a member that comes
// or goes is put again with a new version; a group a node has with other
// content is put again.
func TestEnvGroups(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	id := model.EnvGroupID(env.ID)
	r1 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	one := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)
	var g1 *networkv1.SecurityGroup
	eventually(t, 30*time.Second, "the group with its member", func() bool {
		g1 = h.group("n1", id)
		return g1 != nil && slices.Equal(g1.Members, []string{one[0].Sandbox.Address})
	})
	if !slices.EqualFunc(g1.Rules, envRules(id), func(a, b *networkv1.Rule) bool {
		return sameContent(
			&networkv1.SecurityGroup{Rules: []*networkv1.Rule{a}}, &networkv1.SecurityGroup{Rules: []*networkv1.Rule{b}})
	}) {
		t.Errorf("rules %v", g1.Rules)
	}
	r2 := h.apply(env.ID, serviceSpec(2, "1", ""))
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 30*time.Second)
	two := h.waitRows(env.ID, spec.KindService, "web", 2, 10*time.Second, "ready", ready)
	var g2 *networkv1.SecurityGroup
	eventually(t, 30*time.Second, "the group with both members", func() bool {
		g2 = h.group("n1", id)
		return g2 != nil && len(g2.Members) == 2 && slices.Contains(g2.Members, two[1].Sandbox.Address)
	})
	if g2.Version <= g1.Version {
		t.Errorf("version %d after %d", g2.Version, g1.Version)
	}
	e, _ := h.st.GetEnvironment(bg, env.ID)
	if uint64(e.Gen) != g2.Version {
		t.Errorf("gen %d, version %d", e.Gen, g2.Version)
	}
	r3 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitDeployment(env.ID, r3, model.DeploymentDeployed, 30*time.Second)
	var g3 *networkv1.SecurityGroup
	eventually(t, 30*time.Second, "the group without the stopped member", func() bool {
		g3 = h.group("n1", id)
		return g3 != nil && slices.Equal(g3.Members, []string{one[0].Sandbox.Address})
	})
	if g3.Version <= g2.Version {
		t.Errorf("version %d after %d", g3.Version, g2.Version)
	}
	// Someone else's content at the same version: put again, newer.
	tampered := &networkv1.SecurityGroup{Id: id, Version: g3.Version, Rules: egressTable()}
	if _, err := networkv1.NewSecurityGroupServiceClient(h.conn("n1")).PutSecurityGroup(bg,
		&networkv1.PutSecurityGroupRequest{Group: tampered}); err != nil {
		t.Fatal(err)
	}
	h.c.notify(bg, HintGroups)
	eventually(t, 30*time.Second, "the group repaired", func() bool {
		g := h.group("n1", id)
		return g != nil && g.Version > g3.Version && len(g.Rules) == 1 && len(g.Members) == 1
	})
	// A group of an environment that is gone goes from the node.
	gone := model.EnvGroupID(uuid.New())
	if _, err := networkv1.NewSecurityGroupServiceClient(h.conn("n1")).PutSecurityGroup(bg,
		&networkv1.PutSecurityGroupRequest{Group: &networkv1.SecurityGroup{Id: gone, Version: 1}}); err != nil {
		t.Fatal(err)
	}
	h.c.notify(bg, HintGroups)
	eventually(t, 30*time.Second, "the stale group gone", func() bool { return h.group("n1", gone) == nil })
}

// Deleting an environment leaves nothing: its instances and their pods,
// its volume on its node, its attached rows, its group on the node, its
// row. A deleting tenant's environments are deleted too.
func TestEnvDeletion(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, `{"addons":{"cache":{"type":"valkey","version":"8","plan":"dev"}},
		"services":{"api":{"image":"`+testImage+`","replicas":2}}}`)
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	var pods []podid.ID
	for _, r := range h.rows(env.ID) {
		pods = append(pods, r.Sandbox.ID)
	}
	if len(pods) != 3 || len(n1.Volumes()) != 1 || h.group("n1", model.EnvGroupID(env.ID)) == nil {
		t.Fatalf("before: %d pods, %d volumes", len(pods), len(n1.Volumes()))
	}
	h.exec(`UPDATE environments SET deleting = true WHERE id = $1`, env.ID)
	eventually(t, 60*time.Second, "the environment gone", func() bool {
		_, err := h.st.GetEnvironment(bg, env.ID)
		return errors.Is(err, store.ErrNoRow)
	})
	for _, p := range pods {
		if info := h.pod("n1", p); info != nil {
			t.Errorf("pod %s stays: %v", p, info.State)
		}
	}
	if len(n1.Volumes()) != 0 {
		t.Errorf("volumes stay: %v", n1.Volumes())
	}
	// env-groups may have put the group again from rows it read just
	// before they went; its next pass removes it as of an environment
	// that is gone.
	h.c.notify(bg, HintGroups)
	eventually(t, 30*time.Second, "the env group gone", func() bool { return h.group("n1", model.EnvGroupID(env.ID)) == nil })
	var n int
	if err := h.d.Pool.QueryRow(bg, `SELECT (SELECT count(*) FROM sandboxes WHERE environment_id IS NOT NULL)
		+ (SELECT count(*) FROM env_volumes) + (SELECT count(*) FROM env_secrets) + (SELECT count(*) FROM env_deployments)`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d rows stay, %v", n, err)
	}

	// A deleting tenant's environment.
	if _, _, err := h.st.CreateTenant(bg, "t2"); err != nil {
		t.Fatal(err)
	}
	e2, err := h.st.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: "t2", Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	r := h.apply(e2.ID, serviceSpec(1, "1", ""))
	h.waitDeployment(e2.ID, r, model.DeploymentDeployed, 30*time.Second)
	if _, err := h.st.MarkTenantDeleting(bg, "t2"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 60*time.Second, "the tenant's environment gone", func() bool {
		_, err := h.st.GetEnvironment(bg, e2.ID)
		return errors.Is(err, store.ErrNoRow)
	})
	// A deleting tenant gets no new instance.
	if _, _, err := h.st.CreateTenant(bg, "t3"); err != nil {
		t.Fatal(err)
	}
	e3, err := h.st.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: "t3", Name: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.MarkTenantDeleting(bg, "t3"); err != nil {
		t.Fatal(err)
	}
	v, err := h.c.load(bg, e3.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v != nil {
		t.Fatal("a view of an environment without a deployment")
	}
	ids, err := h.st.SecurityGroupIDs(bg, e3.ID)
	if err != nil {
		t.Fatal(err)
	}
	vw := &view{env: e3, target: &spec.Spec{}, latestSpec: &spec.Spec{}, groupIDs: ids}
	p := h.c.planService(vw, "web", spec.Service{Image: testImage})
	if _, err := h.c.insertInstance(bg, vw, insert{plan: p, deployment: 1}); !errors.Is(err, errTenantDeleting) {
		t.Errorf("an insert for a deleting tenant: %v", err)
	}
}

// LogTail reads a replica's container log with tail_lines, a job's kept
// tail, refuses a paused instance (ErrPaused, without waking it) and maps
// a node without logs to ErrNoContainerLogs; RestartService restarts the
// replicas one at a time.
func TestLogsAndRestart(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"services":{"web":{"image":%q,"replicas":2},
		"idler":{"image":%q,"idle":"15m"}},"jobs":{"once":{"image":%q,"on":"manual"}}}`, testImage, testImage, testImage))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	rows := h.waitRows(env.ID, spec.KindService, "web", 2, 10*time.Second, "ready", ready)
	for i := range 5 {
		_ = n1.Script().AppendLog(rows[1].Sandbox.ID, "main", fmt.Sprintf("line %d", i))
	}
	lines, source, err := h.c.LogTail(bg, env.ID, spec.KindService, "web", 1, 2)
	if err != nil || source != SourceContainer || !slices.Equal(lines, []string{"line 3", "line 4"}) {
		t.Fatalf("logs %q %s %v", lines, source, err)
	}
	if _, _, err := h.c.LogTail(bg, env.ID, spec.KindService, "web", 5, 2); !errors.Is(err, ErrNoObject) ||
		err.Error() != "service web has no replica 5 (it has 2)" {
		t.Errorf("an unknown replica: %v", err)
	}
	if _, _, err := h.c.LogTail(bg, env.ID, spec.KindAddon, "pg", 0, 2); err != ErrNoObject {
		t.Errorf("an unknown addon: %v", err)
	}
	if _, _, err := h.c.LogTail(bg, uuid.New(), spec.KindService, "web", 0, 2); err != ErrNoObject {
		t.Errorf("an unknown environment: %v", err)
	}
	// A node that predates the logs.
	n1.Script().SetHook(containerv1.ContainerService_StreamContainerLogs_FullMethodName, func(context.Context, string, any) error {
		return status.Error(codes.Unimplemented, "unknown method")
	})
	if _, _, err := h.c.LogTail(bg, env.ID, spec.KindService, "web", 0, 2); !errors.Is(err, ErrNoContainerLogs) {
		t.Errorf("an old node: %v", err)
	}
	n1.Script().SetHook(containerv1.ContainerService_StreamContainerLogs_FullMethodName, nil)
	// A paused (idle) instance is not woken for its logs.
	idler := h.waitRows(env.ID, spec.KindService, "idler", 1, 10*time.Second, "ready", ready)[0]
	if err := h.eng.Pause(bg, idler.Sandbox.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "paused", func() bool {
		sb, _ := h.st.GetSandbox(bg, idler.Sandbox.ID)
		return sb.ClientState == model.StatePaused && sb.Op == ""
	})
	if _, _, err := h.c.LogTail(bg, env.ID, spec.KindService, "idler", 0, 2); !errors.Is(err, ErrPaused) {
		t.Errorf("a paused instance: %v", err)
	}
	never(t, 3*time.Second, "the idle instance was woken", func() bool {
		sb, _ := h.st.GetSandbox(bg, idler.Sandbox.ID)
		return sb.ClientState != model.StatePaused
	})
	// A job's log once its row is gone: the tail its run kept.
	if err := h.c.RunJob(bg, env.ID, "once"); err != nil {
		t.Fatal(err)
	}
	var job podid.ID
	eventually(t, 30*time.Second, "the job running", func() bool {
		for _, r := range h.rowsOf(env.ID, spec.KindJob, "once") {
			if running(r) && h.pod("n1", r.Sandbox.ID) != nil {
				job = r.Sandbox.ID
				return true
			}
		}
		return false
	})
	_ = n1.Script().AppendLog(job, "main", "job says hi")
	if err := n1.Script().ExitContainer(job, "main", 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the job's row gone", func() bool {
		_, err := h.st.GetSandbox(bg, job)
		return err != nil
	})
	if lines, _, err := h.c.LogTail(bg, env.ID, spec.KindJob, "once", 0, 10); err != nil || !slices.Equal(lines, []string{"job says hi"}) {
		t.Errorf("the job's kept log %q %v", lines, err)
	}

	// RestartService: one replica at a time, each once the one before is
	// ready again.
	var mu sync.Mutex
	var starts []string
	n1.Script().SetHook(podv1.PodService_StartPod_FullMethodName, func(_ context.Context, _ string, req any) error {
		mu.Lock()
		defer mu.Unlock()
		starts = append(starts, req.(*podv1.StartPodRequest).GetPodId())
		return nil
	})
	if err := h.c.RestartService(bg, env.ID, "web"); err != nil {
		t.Fatal(err)
	}
	if err := h.c.RestartService(bg, env.ID, "nope"); err != ErrNoObject {
		t.Errorf("restart an unknown service: %v", err)
	}
	eventually(t, 60*time.Second, "both replicas restarted", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(starts) >= 2
	})
	mu.Lock()
	if starts[0] != rows[0].Sandbox.ID.String() || starts[1] != rows[1].Sandbox.ID.String() {
		t.Errorf("restarts %v", starts)
	}
	mu.Unlock()
	h.waitRows(env.ID, spec.KindService, "web", 2, 30*time.Second, "ready after the restarts", ready)
}

// An instance pending without capacity for long opens no_capacity, which
// closes once its object places.
func TestNoCapacity(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1", func(c *testnode.Config) { c.MemoryBytes = 8 << 30 })
	env := h.newEnv("stage")
	h.apply(env.ID, fmt.Sprintf(`{"services":{"big":{"image":%q,"resources":{"memoryMib":32768}}}}`, testImage))
	inc := h.incident(env.ID, "service/big", model.ConditionNoCapacity, 30*time.Second)
	if !strings.Contains(inc.Summary, "found no capacity") {
		t.Errorf("summary %q", inc.Summary)
	}
	r2 := h.apply(env.ID, fmt.Sprintf(`{"services":{"big":{"image":%q,"resources":{"memoryMib":512}}}}`, testImage))
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 30*time.Second)
	eventually(t, 30*time.Second, "the incident closed", func() bool { return len(h.incidents(env.ID, true)) == 0 })
}

// An environment's ID taken again after its deletion is another
// environment: the controller brings up the new one's deployment 1, not the
// spec it read of the old one's.
func TestEnvIDReused(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"services":{"old":{"image":%q}}}`, testImage))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	h.exec(`UPDATE environments SET deleting = true WHERE id = $1`, env.ID)
	eventually(t, 60*time.Second, "the environment gone", func() bool {
		_, err := h.st.GetEnvironment(bg, env.ID)
		return errors.Is(err, store.ErrNoRow)
	})
	again, err := h.st.InsertEnvironment(bg, store.NewEnvironment{ID: env.ID, Owner: testTenant, Name: "stage"})
	if err != nil {
		t.Fatal(err)
	}
	r := h.apply(again.ID, fmt.Sprintf(`{"services":{"new":{"image":%q}}}`, testImage))
	h.waitDeployment(again.ID, r, model.DeploymentDeployed, 30*time.Second)
	rows := h.rows(again.ID)
	if len(rows) != 1 || rows[0].Instance.Object != "new" {
		t.Fatalf("rows of the new environment %+v", rows)
	}
}

// The tenant's groups (ADR 0034, 0035): each pod of the environment, its
// instances, sandboxes and tenants' jobs' pods, names env-<id> and the
// sg-<row> of its groups and the env table; each group is on the node with
// its rules from the spec (peers that are groups by their sg-<row>, a group
// that only a rule names too) and the pods that name it as members; the env
// group takes nothing in, reaches its members and has every pod as one.
func TestTenantGroups(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	r := h.apply(env.ID, fmt.Sprintf(`{"securityGroups":{
		"web":{"in":[{"protocol":"tcp","ports":[8080],"from":{"cidr":"203.0.113.0/24"}}]},
		"db":{"in":[{"protocol":"tcp","ports":[5432],"from":{"group":"web"}},
			{"protocol":"tcp","ports":[5432],"from":{"group":"tools"}},
			{"protocol":"tcp","ports":[5432],"from":{"group":"ops"}}],"out":[]},
		"tools":{},"ops":{}},
		"services":{"web":{"image":%q,"securityGroups":["web"]},"store":{"image":%q,"securityGroups":["db"]}}}`,
		testImage, testImage))
	h.waitDeployment(env.ID, r, model.DeploymentDeployed, 60*time.Second)
	ids, err := h.st.SecurityGroupIDs(bg, env.ID)
	if err != nil {
		t.Fatal(err)
	}
	sg := func(name string) string { return model.SecurityGroupID(ids[name]) }
	web := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0]
	st := h.waitRows(env.ID, spec.KindService, "store", 1, 10*time.Second, "ready", ready)[0]
	sb := h.sandboxIn(env, podid.Mint(), "tools")
	rs := tenantJobSpecOf(time.Hour)
	rs.EnvID, rs.SecurityGroups = env.ID, []string{"db"}
	job := h.createTenantJob(rs)
	h.runningPod(job.ID)

	envGroup := model.EnvGroupID(env.ID)
	for id, want := range map[podid.ID][]string{web.Sandbox.ID: {envGroup, sg("web")}, st.Sandbox.ID: {envGroup, sg("db")},
		sb.ID: {envGroup, sg("tools")}, job.ID: {envGroup, sg("db")}} {
		n := h.pod("n1", id).GetSpec().GetNetwork()
		if !slices.Equal(n.GetSecurityGroups(), want) || !slices.Equal(n.GetNameTables(), []string{model.EnvTableID(env.ID)}) {
			t.Errorf("pod %s: %v, want the groups %v", id, n, want)
		}
	}
	addrs := map[podid.ID]string{}
	for _, id := range []podid.ID{web.Sandbox.ID, st.Sandbox.ID, sb.ID, job.ID} {
		addrs[id] = h.addressOf(id)
	}
	sorted := func(ids ...podid.ID) []string {
		var out []string
		for _, id := range ids {
			out = append(out, addrs[id])
		}
		return members(out)
	}
	h.c.notify(bg, HintGroups)
	eventually(t, 30*time.Second, "the groups and their members", func() bool {
		eg, wg, dg, tg, og := h.group("n1", envGroup), h.group("n1", sg("web")), h.group("n1", sg("db")),
			h.group("n1", sg("tools")), h.group("n1", sg("ops"))
		return eg != nil && wg != nil && dg != nil && tg != nil && og != nil &&
			slices.Equal(eg.Members, sorted(web.Sandbox.ID, st.Sandbox.ID, sb.ID, job.ID)) &&
			slices.Equal(wg.Members, sorted(web.Sandbox.ID)) && slices.Equal(dg.Members, sorted(st.Sandbox.ID, job.ID)) &&
			slices.Equal(tg.Members, sorted(sb.ID)) && len(og.Members) == 0
	})
	in, out := networkv1.Direction_DIRECTION_INGRESS, networkv1.Direction_DIRECTION_EGRESS
	count := func(g *networkv1.SecurityGroup, dir networkv1.Direction) int {
		n := 0
		for _, r := range g.Rules {
			if r.Direction == dir {
				n++
			}
		}
		return n
	}
	if g := h.group("n1", envGroup); !proto.Equal(&networkv1.SecurityGroup{Rules: g.Rules},
		&networkv1.SecurityGroup{Rules: envRules(envGroup)}) {
		t.Errorf("the env group's rules %v", g.Rules)
	}
	dg := h.group("n1", sg("db"))
	var from []string
	for _, r := range dg.Rules {
		if r.Direction != in || r.Protocol != networkv1.Protocol_PROTOCOL_TCP || len(r.Ports) != 1 || r.Ports[0].First != 5432 {
			t.Errorf("db's rule %v", r)
		}
		from = append(from, r.GetGroup())
	}
	if !slices.Equal(from, []string{sg("web"), sg("tools"), sg("ops")}) {
		t.Errorf("db lets in %v", from)
	}
	if wg := h.group("n1", sg("web")); count(wg, in) != 1 || wg.Rules[0].GetBlock().GetCidr() != "203.0.113.0/24" ||
		count(wg, out) != 2 {
		t.Errorf("web's rules %v", wg.Rules)
	}
	if tg := h.group("n1", sg("tools")); count(tg, in) != 0 || count(tg, out) != 2 ||
		tg.Rules[0].GetBlock().GetCidr() != "0.0.0.0/0" || tg.Rules[1].GetBlock().GetCidr() != "2000::/3" {
		t.Errorf("tools' rules %v", tg.Rules)
	}
	// A rule changed in a new deployment reaches the node; the pods stay.
	r2 := h.apply(env.ID, fmt.Sprintf(`{"securityGroups":{
		"web":{"in":[{"protocol":"tcp","ports":[8080],"from":{"cidr":"203.0.113.0/24"}}]},
		"db":{"in":[{"protocol":"tcp","ports":[5432],"from":{"group":"web"}}],"out":[]},
		"tools":{},"ops":{}},
		"services":{"web":{"image":%q,"securityGroups":["web"]},"store":{"image":%q,"securityGroups":["db"]}}}`,
		testImage, testImage))
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 60*time.Second)
	h.c.notify(bg, HintGroups)
	eventually(t, 30*time.Second, "db's new rules", func() bool {
		g := h.group("n1", sg("db"))
		return g != nil && len(g.Rules) == 1 && g.Rules[0].GetGroup() == sg("web")
	})
	if rows := h.rowsOf(env.ID, spec.KindService, "store"); len(rows) != 1 || rows[0].Sandbox.ID != st.Sandbox.ID {
		t.Errorf("store rolled for a rule: %v", rows)
	}
}
