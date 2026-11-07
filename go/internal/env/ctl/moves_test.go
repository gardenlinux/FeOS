// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

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
