// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// A sandbox's groups change by a move (ADR 0035): a running sandbox is
// hibernated and restored in its new groups (feosd gives it a new address;
// the test node may give the freed one again), the restore carrying them and the template made of them, its old groups
// losing it and its new ones gaining it; a paused one is hibernated and
// stays so, and its next use restores it in its groups.
func TestSandboxMove(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("dev")
	r := h.apply(env.ID, `{"securityGroups":{"db":{"in":[{"protocol":"tcp","ports":[5432],"from":{"group":"tools"}}]},
		"tools":{}}}`)
	h.waitDeployment(env.ID, r, model.DeploymentDeployed, 60*time.Second)
	sb := h.sandboxIn(env, podid.Mint())
	first := ""
	eventually(t, 10*time.Second, "the sandbox's address", func() bool {
		first = h.addressOf(sb.ID)
		return first != ""
	})
	ids, err := h.st.SecurityGroupIDs(bg, env.ID)
	if err != nil {
		t.Fatal(err)
	}
	def, tools := model.SecurityGroupID(ids["default"]), model.SecurityGroupID(ids["tools"])
	eventually(t, 30*time.Second, "the sandbox in default", func() bool {
		g := h.group("n1", def)
		return g != nil && slices.Equal(g.Members, []string{first})
	})

	// Running: moved.
	if ok, err := h.st.SetSandboxGroups(bg, sb.ID, []string{"tools"}); err != nil || !ok {
		t.Fatalf("set the groups %v %v", ok, err)
	}
	var moved store.Sandbox
	eventually(t, 60*time.Second, "the sandbox moved", func() bool {
		moved, err = h.st.GetSandbox(bg, sb.ID)
		return err == nil && !moved.GroupsPending && moved.ClientState == model.StateRunning && moved.Op == "" &&
			moved.Address != "" && moved.CheckpointHead != ""
	})
	if want := model.SandboxTemplate(env.ID, []uuid.UUID{ids["tools"]}); !reflect.DeepEqual(moved.PodTemplate, want) {
		t.Errorf("template %+v, want %+v", moved.PodTemplate, want)
	}
	if moved.Incarnation != sb.Incarnation {
		t.Errorf("another incarnation: %+v", moved)
	}
	n := h.pod("n1", sb.ID).GetSpec().GetNetwork()
	if !slices.Equal(n.GetSecurityGroups(), h.podGroups(env.ID, "tools")) ||
		!slices.Equal(n.GetNameTables(), []string{model.EnvTableID(env.ID)}) {
		t.Errorf("the restored pod's network %v", n)
	}
	eventually(t, 30*time.Second, "the groups' members after the move", func() bool {
		h.c.notify(bg, HintGroups)
		dg, tg, eg := h.group("n1", def), h.group("n1", tools), h.group("n1", model.EnvGroupID(env.ID))
		return dg != nil && tg != nil && eg != nil && len(dg.Members) == 0 &&
			slices.Equal(tg.Members, []string{moved.Address}) && slices.Equal(eg.Members, []string{moved.Address})
	})

	// Paused: hibernated, and restored in its groups at its next use.
	if err := h.eng.Pause(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.st.SetSandboxGroups(bg, sb.ID, []string{"default", "tools"}); err != nil || !ok {
		t.Fatalf("set the groups %v %v", ok, err)
	}
	eventually(t, 60*time.Second, "the paused sandbox hibernated", func() bool {
		s, err := h.st.GetSandbox(bg, sb.ID)
		return err == nil && s.ClientState == model.StateHibernated && s.Op == "" && s.GroupsPending
	})
	never(t, 2*time.Second, "the hibernated sandbox restored without a use", func() bool {
		s, err := h.st.GetSandbox(bg, sb.ID)
		return err != nil || s.ClientState != model.StateHibernated
	})
	ctx, cancel := context.WithTimeout(bg, 60*time.Second)
	defer cancel()
	if _, err := h.eng.Ensure(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	again, err := h.st.GetSandbox(bg, sb.ID)
	if err != nil || again.GroupsPending || !slices.Equal(again.PodTemplate.SecurityGroups, h.podGroups(env.ID, "default", "tools")) {
		t.Fatalf("restored %+v %v", again, err)
	}
	if n := h.pod("n1", sb.ID).GetSpec().GetNetwork(); !slices.Equal(n.GetSecurityGroups(), h.podGroups(env.ID, "default", "tools")) {
		t.Errorf("the restored pod's groups %v", n.GetSecurityGroups())
	}
	// Unchanged groups move nothing.
	before := again
	never(t, 2*time.Second, "a sandbox moved without a change", func() bool {
		s, err := h.st.GetSandbox(bg, sb.ID)
		return err != nil || s.Gen != before.Gen && s.ClientState != model.StateRunning
	})
}

// legacyBox is a sandbox as migration 0032 leaves it: its pod made before
// ADR 0035, with no template (a plain sandbox) or an attachable one's,
// which names its group and table sbx-<id> (put on the node here, as the
// version before put them); in [default], not yet pending.
func (h *harness) legacyBox(env store.Environment, attachable bool) store.Sandbox {
	h.t.Helper()
	id := podid.Mint()
	var tmpl *model.PodTemplate
	if attachable {
		legacy := legacySandboxPrefix + id.String()
		tmpl = &model.PodTemplate{Command: slices.Clone(model.SandboxCommand), SecurityGroups: []string{legacy},
			NameTables: []string{legacy}}
		cc := h.conn("n1")
		if _, err := networkv1.NewSecurityGroupServiceClient(cc).PutSecurityGroup(bg, &networkv1.PutSecurityGroupRequest{
			Group: &networkv1.SecurityGroup{Id: legacy, Version: 1}}); err != nil {
			h.t.Fatal(err)
		}
		if _, err := networkv1.NewNameServiceClient(cc).PutNameTable(bg, &networkv1.PutNameTableRequest{
			Table: &networkv1.NameTable{Id: legacy, Version: 1}}); err != nil {
			h.t.Fatal(err)
		}
	}
	if _, _, err := h.st.CreateSandbox(bg, store.NewSandbox{ID: id, Owner: env.Owner, Image: testImage, ImageRef: testImage,
		VCPUs: 1, MemoryMiB: 512, EnvironmentID: env.ID, SecurityGroups: []string{"default"}, PodTemplate: tmpl}); err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bg, 60*time.Second)
	defer cancel()
	if _, err := h.eng.Ensure(ctx, id); err != nil {
		h.t.Fatalf("ensure the legacy sandbox: %v", err)
	}
	sb, err := h.st.GetSandbox(bg, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return sb
}

// The move of the rollout (ADR 0035): the sandboxes migration 0032 marks,
// whose pods run under no groups or under their old sbx-<id> group and
// table. A paused attachable one resumes under its old group before its
// move (nothing builds sbx- groups any more; the node keeps it while the
// pod names it). Once marked, a running one is hibernated and restored in
// env-<id> and sg-default with the environment's table, also when its
// first restore fails (it is owed it and tried again); the paused one is
// hibernated and takes its groups at its restore; the sbx group and table
// leave the node once no pod names them.
func TestSandboxMoveAfterMigration(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("sandboxes")
	plain := h.legacyBox(env, false)
	attachable := h.legacyBox(env, true)
	legacy := legacySandboxPrefix + attachable.ID.String()
	if g := h.pod("n1", attachable.ID).GetSpec().GetNetwork().GetSecurityGroups(); !slices.Equal(g, []string{legacy}) {
		t.Fatalf("the legacy pod's groups %v", g)
	}

	// Paused before its move, it resumes in its old group.
	if err := h.eng.Pause(bg, attachable.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bg, 60*time.Second)
	defer cancel()
	if _, err := h.eng.Ensure(ctx, attachable.ID); err != nil {
		t.Fatalf("resume the legacy sandbox: %v", err)
	}
	if err := h.eng.Pause(bg, attachable.ID); err != nil {
		t.Fatal(err)
	}

	// The first restore of a move fails: the sandbox is owed it.
	var mu sync.Mutex
	failed := 0
	n1.Script().SetHook(podv1.PodService_RestorePod_FullMethodName, func(context.Context, string, any) error {
		mu.Lock()
		defer mu.Unlock()
		if failed == 0 {
			failed++
			return status.Error(codes.Unavailable, "the node has no time for a restore")
		}
		return nil
	})
	h.exec(`UPDATE sandboxes SET groups_pending = true WHERE id = ANY($1)`, []podid.ID{plain.ID, attachable.ID})
	h.c.notify(bg, HintGroups)

	eventually(t, 60*time.Second, "the running sandbox moved", func() bool {
		s, err := h.st.GetSandbox(bg, plain.ID)
		return err == nil && !s.GroupsPending && s.ClientState == model.StateRunning && s.Op == "" && s.Address != ""
	})
	mu.Lock()
	if failed != 1 {
		t.Errorf("%d failed restores", failed)
	}
	mu.Unlock()
	eventually(t, 10*time.Second, "the restore no longer owed", func() bool {
		var marked bool
		err := h.d.Pool.QueryRow(bg, `SELECT move_restore FROM sandboxes WHERE id = $1`, plain.ID).Scan(&marked)
		return err == nil && !marked
	})
	moved, err := h.st.GetSandbox(bg, plain.ID)
	if err != nil {
		t.Fatal(err)
	}
	n := h.pod("n1", plain.ID).GetSpec().GetNetwork()
	if !slices.Equal(n.GetSecurityGroups(), h.podGroups(env.ID, "default")) ||
		!slices.Equal(n.GetNameTables(), []string{model.EnvTableID(env.ID)}) {
		t.Errorf("the moved pod's network %v", n)
	}
	eventually(t, 60*time.Second, "the paused sandbox hibernated", func() bool {
		s, err := h.st.GetSandbox(bg, attachable.ID)
		return err == nil && s.ClientState == model.StateHibernated && s.Op == "" && s.GroupsPending
	})
	if _, err := h.eng.Ensure(ctx, attachable.ID); err != nil {
		t.Fatalf("restore the hibernated sandbox: %v", err)
	}
	if n := h.pod("n1", attachable.ID).GetSpec().GetNetwork(); !slices.Equal(n.GetSecurityGroups(), h.podGroups(env.ID, "default")) ||
		!slices.Equal(n.GetNameTables(), []string{model.EnvTableID(env.ID)}) {
		t.Errorf("the restored pod's network %v", n)
	}
	restored, err := h.st.GetSandbox(bg, attachable.ID)
	if err != nil {
		t.Fatal(err)
	}
	def := h.podGroups(env.ID, "default")[1]
	eventually(t, 30*time.Second, "both in default, the sbx group and table gone", func() bool {
		h.c.notify(bg, HintGroups)
		g := h.group("n1", def)
		return g != nil && slices.Equal(g.Members, sortedOf(moved.Address, restored.Address)) &&
			h.group("n1", legacy) == nil && h.table("n1", legacy) == nil
	})
}

func sortedOf(s ...string) []string {
	slices.Sort(s)
	return s
}
