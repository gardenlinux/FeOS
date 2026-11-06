// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

func mkEnv(t *testing.T, s *store.Store, owner, name string) store.Environment {
	t.Helper()
	e, err := s.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: owner, Name: name,
		Labels: map[string]string{"app": name}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func mkInstance(t *testing.T, s *store.Store, env store.Environment, mod ...func(*store.NewSandbox)) store.Sandbox {
	t.Helper()
	n := store.NewSandbox{ID: podid.Mint(), Owner: env.Owner, Image: "alpine", VCPUs: 1, MemoryMiB: 256, EnvironmentID: env.ID,
		PodTemplate: &model.PodTemplate{SecurityGroups: []string{model.EnvGroupID(env.ID)}}}
	for _, m := range mod {
		m(&n)
	}
	sb, created, err := s.CreateSandbox(bg, n)
	if err != nil || !created {
		t.Fatalf("create instance: %v %v", err, created)
	}
	return sb
}

// The instance columns of sandboxes: inserted, read back, the writable
// ones written beside the lifecycle, the fixed ones refused; the tenant's
// listing leaves instance rows out.
func TestSandboxInstanceColumns(t *testing.T) {
	s, d := newStore(t)
	mkTenant(t, s, "acme")
	env := mkEnv(t, s, "acme", "stage")
	idle := 90
	tpl := &model.PodTemplate{
		Command: []string{"/app"},
		Env: map[string]model.TemplateValue{"A": model.Literal("a"), "B": model.SecretValue("pw", 2, model.EscapeURL),
			"C": {Parts: []model.TemplatePart{{Value: "x"}, {Secret: "pw", Version: 2}}}},
		Volumes:        []model.TemplateVolume{{Name: "data", VolumeID: uuid.New()}},
		Mounts:         []model.TemplateMount{{Volume: "data", Path: "/data"}},
		SecurityGroups: []string{model.EnvGroupID(env.ID)},
		Labels:         map[string]string{model.LabelObject: "addon/pg"},
	}
	sb := mkInstance(t, s, env, func(n *store.NewSandbox) {
		n.PodTemplate, n.PinNode, n.IdleAfterS, n.ImageRef = tpl, "n1", &idle, "alpine@sha256:abc"
	})
	got := get(t, s, sb.ID)
	tj, _ := json.Marshal(tpl)
	gj, _ := json.Marshal(got.PodTemplate)
	if got.EnvironmentID != env.ID || string(tj) != string(gj) || got.PinNode != "n1" || got.IdleAfterS == nil ||
		*got.IdleAfterS != 90 || got.Address != "" || got.ImageRef != "alpine@sha256:abc" || !got.IsInstance() ||
		!got.HasGroups() {
		t.Fatalf("instance row %+v (template %s)", got, gj)
	}
	plain := mkSandbox(t, s, "acme")
	if plain.IsInstance() || plain.HasGroups() || plain.PodTemplate != nil || plain.IdleAfterS != nil || plain.PinNode != "" {
		t.Fatalf("plain row %+v", plain)
	}
	if l, err := s.ListSandboxes(bg, "acme"); err != nil || len(l) != 1 || l[0].ID != plain.ID {
		t.Fatalf("the tenant's listing: %v %v", l, err)
	}
	if l, err := s.ListSandboxes(bg, ""); err != nil || len(l) != 2 {
		t.Fatalf("every row: %v %v", l, err)
	}

	// Writable without a gen: pin_node, idle_after_s (to NULL and back),
	// address.
	ch, unsubscribe := d.Subscribe(db.ChanSandbox)
	defer unsubscribe()
	after := got
	after.PinNode, after.IdleAfterS, after.Address = "n2", nil, "fd00::7"
	w, err := s.UpdateSandbox(bg, got, after)
	if err != nil || w.Gen != got.Gen || w.PinNode != "n2" || w.IdleAfterS != nil || w.Address != "fd00::7" {
		t.Fatalf("update %+v %v", w, err)
	}
	expectNoPayload(t, ch, sb.ID.String())
	zero := 0
	after = w
	after.IdleAfterS = &zero
	if w, err = s.UpdateSandbox(bg, w, after); err != nil || w.IdleAfterS == nil || *w.IdleAfterS != 0 {
		t.Fatalf("idle 0: %+v %v", w, err)
	}
	// Fixed: environment_id and pod_template.
	after = w
	after.EnvironmentID = uuid.New()
	if _, err := s.UpdateSandbox(bg, w, after); err == nil || !strings.Contains(err.Error(), "environment_id") {
		t.Fatalf("environment_id written: %v", err)
	}
	after = w
	after.PodTemplate = &model.PodTemplate{Command: []string{"other"}}
	if _, err := s.UpdateSandbox(bg, w, after); err == nil || !strings.Contains(err.Error(), "pod_template") {
		t.Fatalf("pod_template written: %v", err)
	}
	after.PodTemplate = nil
	if _, err := s.UpdateSandbox(bg, w, after); err == nil || !strings.Contains(err.Error(), "pod_template") {
		t.Fatalf("pod_template cleared: %v", err)
	}
	// A copy of the template is the same template.
	same := *w.PodTemplate
	after.PodTemplate = &same
	if _, err := s.UpdateSandbox(bg, w, after); err != nil {
		t.Fatal(err)
	}
	// The instance row has no token door.
	exec(t, s, "INSERT INTO sandbox_tokens (hash, sandbox_id, label) VALUES ('h-inst', $1, 'x')", sb.ID)
	exec(t, s, "INSERT INTO sandbox_tokens (hash, sandbox_id, label) VALUES ('h-plain', $1, 'x')", plain.ID)
	if _, _, err := s.ResolveSandboxToken(bg, "h-inst"); !errors.Is(err, model.ErrUnknownToken) {
		t.Fatalf("an instance row's token resolved: %v", err)
	}
	if id, _, err := s.ResolveSandboxToken(bg, "h-plain"); err != nil || id != plain.ID {
		t.Fatalf("plain token: %v %v", id, err)
	}
}

// SetSandboxAddress writes an op's answer once, for the row's incarnation,
// with the env-groups hint for a group member only.
func TestSetSandboxAddress(t *testing.T) {
	s, d := newStore(t)
	mkTenant(t, s, "acme")
	env := mkEnv(t, s, "acme", "stage")
	inst := mkInstance(t, s, env)
	attachable := mkSandbox(t, s, "acme")
	exec(t, s, `UPDATE sandboxes SET pod_template = '{"command":["sleep","infinity"],"securityGroups":["sbx-x"]}' WHERE id = $1`,
		attachable.ID)
	plain := mkSandbox(t, s, "acme")
	hints, unsubscribe := d.Subscribe(db.ChanLeader)
	defer unsubscribe()

	set := func(sb store.Sandbox, inc uuid.UUID, addr string) bool {
		t.Helper()
		var wrote bool
		must(t, s.InTx(bg, func(tx *store.Store) error {
			var err error
			wrote, err = tx.SetSandboxAddress(bg, sb.ID, inc, addr)
			return err
		}))
		return wrote
	}
	if !set(inst, inst.Incarnation, "fd00::1") || get(t, s, inst.ID).Address != "fd00::1" {
		t.Fatal("not written")
	}
	expectPayload(t, hints, store.HintEnvGroups)
	if set(inst, inst.Incarnation, "fd00::1") {
		t.Fatal("the same address written again")
	}
	if set(inst, uuid.New(), "fd00::2") || get(t, s, inst.ID).Address != "fd00::1" {
		t.Fatal("another incarnation's address written")
	}
	if !set(attachable, attachable.Incarnation, "fd00::3") {
		t.Fatal("not written")
	}
	expectPayload(t, hints, store.HintEnvGroups)
	if !set(plain, plain.Incarnation, "fd00::4") {
		t.Fatal("not written")
	}
	expectNoPayload(t, hints, store.HintEnvGroups)
	if g := get(t, s, inst.ID).Gen; g != inst.Gen {
		t.Fatalf("gen %d", g)
	}
}

// Lost grace clears the address, with the hint when a member had one.
func TestLostGraceClearsAddress(t *testing.T) {
	s, d := newStore(t)
	mkTenant(t, s, "acme")
	mkNode(t, s, "n1")
	env := mkEnv(t, s, "acme", "stage")
	inst := mkInstance(t, s, env)
	exec(t, s, `UPDATE sandboxes SET node = 'n1', client_state = 'unknown', materialised = true, address = 'fd00::1'
		WHERE id = $1`, inst.ID)
	exec(t, s, "UPDATE nodes SET status = 'down', down_since = now() - interval '1 hour' WHERE name = 'n1'")
	hints, unsubscribe := d.Subscribe(db.ChanLeader)
	defer unsubscribe()
	var lost []store.LostRow
	must(t, s.InTx(bg, func(tx *store.Store) error {
		var err error
		lost, err = tx.LostGrace(bg, time.Minute, 10)
		return err
	}))
	if len(lost) != 1 || lost[0].State != model.StateLost {
		t.Fatalf("lost %+v", lost)
	}
	if a := get(t, s, inst.ID).Address; a != "" {
		t.Fatalf("address %q", a)
	}
	expectPayload(t, hints, store.HintEnvGroups)
}

// Per-row idle: idle_after_s over the instance's interval, 0 never.
func TestIdleAfterPerRow(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkNode(t, s, "n1")
	env := mkEnv(t, s, "acme", "stage")
	zero, minute := 0, 60
	never := mkInstance(t, s, env, func(n *store.NewSandbox) { n.IdleAfterS = &zero })
	short := mkInstance(t, s, env, func(n *store.NewSandbox) { n.IdleAfterS = &minute })
	global := mkSandbox(t, s, "acme")
	for _, id := range []podid.ID{never.ID, short.ID, global.ID} {
		place(t, s, id, "n1", model.StateRunning)
		exec(t, s, "UPDATE sandboxes SET last_active_at = now() - interval '10 minutes' WHERE id = $1", id)
	}
	ids := func(idle time.Duration) []podid.ID {
		t.Helper()
		rows, err := s.IdleSandboxes(bg, idle, []string{"n1"}, 10)
		must(t, err)
		var out []podid.ID
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}
	recheck := func(id podid.ID, idle time.Duration) bool {
		t.Helper()
		ok, err := s.IdleRecheck(bg, id, idle)
		must(t, err)
		return ok
	}
	// Global off: only the row with its own minute.
	if got := ids(0); !slices.Equal(got, []podid.ID{short.ID}) {
		t.Fatalf("global 0: %v", got)
	}
	// Global an hour: still only the minute row; at 5 minutes the global
	// row too; never the 0 row.
	if got := ids(time.Hour); !slices.Equal(got, []podid.ID{short.ID}) {
		t.Fatalf("global 1h: %v", got)
	}
	if got := ids(5 * time.Minute); len(got) != 2 || slices.Contains(got, never.ID) {
		t.Fatalf("global 5m: %v", got)
	}
	if recheck(never.ID, time.Second) || !recheck(short.ID, time.Hour) || recheck(global.ID, 0) || recheck(global.ID, time.Hour) ||
		!recheck(global.ID, time.Minute) {
		t.Fatal("recheck")
	}
}

// A tenant with an environment is not deleted.
func TestDeleteTenantNeedsNoEnvironments(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	env := mkEnv(t, s, "acme", "stage")
	if ok, err := s.DeleteTenantIfEmpty(bg, "acme"); err != nil || ok {
		t.Fatalf("deleted with an environment: %v %v", ok, err)
	}
	if ok, err := s.DeleteEnvironment(bg, env.ID); err != nil || !ok {
		t.Fatalf("delete environment: %v %v", ok, err)
	}
	if ok, err := s.DeleteTenantIfEmpty(bg, "acme"); err != nil || !ok {
		t.Fatalf("delete tenant: %v %v", ok, err)
	}
}

func TestEnvironments(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkTenant(t, s, "other")
	e := mkEnv(t, s, "acme", "stage")
	if e.Deployment != 0 || e.Deployed != 0 || e.Deleting || e.Gen != 0 || e.Labels["app"] != "stage" || e.CreatedAt.IsZero() {
		t.Fatalf("new environment %+v", e)
	}
	// The same name of the owner, or the same ID: ErrExists; another
	// owner's name is free; an unknown owner ErrNoRow.
	if _, err := s.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: "acme", Name: "stage"}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("name taken: %v", err)
	}
	if _, err := s.InsertEnvironment(bg, store.NewEnvironment{ID: e.ID, Owner: "acme", Name: "prod"}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("id taken: %v", err)
	}
	o, err := s.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: "other", Name: "stage"})
	if err != nil || len(o.Labels) != 0 || o.Labels == nil {
		t.Fatalf("other's: %+v %v", o, err)
	}
	if _, err := s.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: "nobody", Name: "x"}); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown owner: %v", err)
	}
	if got, err := s.GetEnvironment(bg, e.ID); err != nil || got.Name != "stage" {
		t.Fatalf("get %+v %v", got, err)
	}
	if _, err := s.GetEnvironment(bg, uuid.New()); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("get unknown: %v", err)
	}
	if got, err := s.LockEnvironmentByName(bg, "other", "stage", store.NoLock); err != nil || got.ID != o.ID {
		t.Fatalf("by name %+v %v", got, err)
	}
	prod := mkEnv(t, s, "acme", "prod")
	if l, err := s.ListEnvironments(bg, "acme", nil); err != nil || len(l) != 2 || l[0].ID != e.ID {
		t.Fatalf("list %v %v", l, err)
	}
	if l, err := s.ListEnvironments(bg, "acme", map[string]string{"app": "prod"}); err != nil || len(l) != 1 || l[0].ID != prod.ID {
		t.Fatalf("list by label %v %v", l, err)
	}
	if l, err := s.ListEnvironments(bg, "", nil); err != nil || len(l) != 3 {
		t.Fatalf("list all %v %v", l, err)
	}

	// Update in a transaction with the row locked; a stale gen is refused.
	must(t, s.InTx(bg, func(tx *store.Store) error {
		before, err := tx.LockEnvironment(bg, e.ID, store.ForUpdate)
		if err != nil {
			return err
		}
		after := before
		after.Deployment, after.Labels = 1, map[string]string{"app": "stage", "tier": "1"}
		w, err := tx.UpdateEnvironment(bg, before, after)
		if err != nil {
			return err
		}
		if w.Deployment != 1 || w.Labels["tier"] != "1" || w.Gen != before.Gen {
			t.Errorf("updated %+v", w)
		}
		return nil
	}))
	gen, err := s.BumpEnvironmentGen(bg, e.ID)
	if err != nil || gen != 1 {
		t.Fatalf("bump %d %v", gen, err)
	}
	cur, _ := s.GetEnvironment(bg, e.ID)
	stale := cur
	stale.Gen = 0
	after := cur
	after.Deployed = 1
	if _, err := s.UpdateEnvironment(bg, stale, after); err == nil {
		t.Fatal("a stale gen was written")
	}
	after = cur
	after.Gen, after.Deployed = cur.Gen+1, 1
	if w, err := s.UpdateEnvironment(bg, cur, after); err != nil || w.Gen != 2 || w.Deployed != 1 {
		t.Fatalf("gen written %+v %v", w, err)
	}
	after = cur
	after.Name = "renamed"
	if _, err := s.UpdateEnvironment(bg, cur, after); err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("name written: %v", err)
	}
	if _, err := s.BumpEnvironmentGen(bg, uuid.New()); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("bump unknown: %v", err)
	}

	// A deleting tenant's environments are marked.
	marked, err := s.MarkTenantEnvironmentsDeleting(bg, "acme")
	if err != nil || len(marked) != 2 {
		t.Fatalf("marked %v %v", marked, err)
	}
	if d, err := s.DeletingEnvironments(bg, 10); err != nil || len(d) != 2 {
		t.Fatalf("deleting %v %v", d, err)
	}
	if n, err := s.CountEnvironmentsOfOwner(bg, "acme"); err != nil || n != 2 {
		t.Fatalf("count %d %v", n, err)
	}
	// An instance row holds its environment.
	mkInstance(t, s, prod)
	if _, err := s.DeleteEnvironment(bg, prod.ID); err == nil {
		t.Fatal("an environment with an instance row went")
	}
}

func TestEnvDeploymentsAndTags(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	e := mkEnv(t, s, "acme", "stage")
	spec := json.RawMessage(`{"services":{"web":{"image":"x"}}}`)
	r1, err := s.InsertDeployment(bg, store.EnvDeployment{EnvID: e.ID, Number: 1, Spec: spec, Author: "me", Message: "first",
		Annotations: map[string]string{"fecode.dev/revision": "r1"}, Origin: model.OriginApply})
	if err != nil || r1.State != model.DeploymentWaiting || r1.Annotations["fecode.dev/revision"] != "r1" ||
		!strings.Contains(string(r1.Spec), `"web"`) || !r1.StartedAt.IsZero() {
		t.Fatalf("deployment 1 %+v %v", r1, err)
	}
	if _, err := s.InsertDeployment(bg, store.EnvDeployment{EnvID: e.ID, Number: 1, Spec: spec, Origin: model.OriginApply}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("number taken: %v", err)
	}
	if _, err := s.InsertDeployment(bg, store.EnvDeployment{EnvID: uuid.New(), Number: 1, Spec: spec, Origin: model.OriginApply}); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown environment: %v", err)
	}
	r2, err := s.InsertDeployment(bg, store.EnvDeployment{EnvID: e.ID, Number: 2, Spec: spec, Base: 1, Origin: model.OriginRevert,
		OriginRef: "1"})
	if err != nil || len(r2.Annotations) != 0 || r2.Annotations == nil {
		t.Fatalf("deployment 2 %+v %v", r2, err)
	}
	if l, err := s.ListDeployments(bg, e.ID, 10); err != nil || len(l) != 2 || l[0].Number != 2 || l[0].Spec != nil {
		t.Fatalf("list %+v %v", l, err)
	}
	if l, _ := s.ListDeployments(bg, e.ID, 1); len(l) != 1 {
		t.Fatalf("limit %v", l)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	after := r1
	after.State, after.StartedAt = model.DeploymentDeploying, now
	w, err := s.UpdateDeployment(bg, r1, after)
	if err != nil || w.State != model.DeploymentDeploying || !w.StartedAt.Equal(now) {
		t.Fatalf("update %+v %v", w, err)
	}
	after = w
	after.Message = "changed"
	if _, err := s.UpdateDeployment(bg, w, after); err == nil {
		t.Fatal("a message was written")
	}
	sup, err := s.ReplaceDeployments(bg, e.ID, 2, model.DeploymentWaiting, model.DeploymentDeploying)
	if err != nil || !slices.Equal(sup, []int{1}) {
		t.Fatalf("replaced %v %v", sup, err)
	}
	if got, err := s.GetDeployment(bg, e.ID, 1); err != nil || got.State != model.DeploymentReplaced || got.FinishedAt.IsZero() {
		t.Fatalf("deployment 1 after %+v %v", got, err)
	}
	if _, err := s.GetDeployment(bg, e.ID, 9); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown deployment: %v", err)
	}

	// Tags.
	tag, err := s.PutTag(bg, store.EnvTag{EnvID: e.ID, Tag: "good", Number: 1, MovedBy: "me"})
	if err != nil || tag.Number != 1 {
		t.Fatalf("tag %+v %v", tag, err)
	}
	if tag, err = s.PutTag(bg, store.EnvTag{EnvID: e.ID, Tag: "good", Number: 2, MovedBy: "you"}); err != nil ||
		tag.Number != 2 || tag.MovedBy != "you" {
		t.Fatalf("moved %+v %v", tag, err)
	}
	if _, err := s.PutTag(bg, store.EnvTag{EnvID: e.ID, Tag: "bad", Number: 7}); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("tag of an unknown deployment: %v", err)
	}
	if l, err := s.ListTags(bg, e.ID); err != nil || len(l) != 1 {
		t.Fatalf("tags %v %v", l, err)
	}
	if got, err := s.GetTag(bg, e.ID, "good"); err != nil || got.Number != 2 {
		t.Fatalf("get tag %+v %v", got, err)
	}
	if ok, err := s.DeleteTag(bg, e.ID, "good"); err != nil || !ok {
		t.Fatalf("delete tag %v %v", ok, err)
	}
	if _, err := s.GetTag(bg, e.ID, "good"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("deleted tag: %v", err)
	}
}

func TestEnvSecretsAndRouteTokens(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	e := mkEnv(t, s, "acme", "stage")
	put := func(name string, version int, system bool) store.EnvSecret {
		t.Helper()
		sec, err := s.InsertSecret(bg, store.EnvSecret{EnvID: e.ID, Name: name, Version: version, Sealed: []byte{1, byte(version)},
			System: system, CreatedBy: "me"})
		if err != nil {
			t.Fatal(err)
		}
		return sec
	}
	put("token", 1, false)
	put("token", 2, false)
	put("other", 1, false)
	put("addon.pg.password", 1, true)
	put("addon.pg.role.app.password", 1, true)
	put("addon.pgx.password", 1, true)
	if _, err := s.InsertSecret(bg, store.EnvSecret{EnvID: e.ID, Name: "token", Version: 2, Sealed: []byte{1}}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("version taken: %v", err)
	}
	if _, err := s.InsertSecret(bg, store.EnvSecret{EnvID: e.ID, Name: "addon.x", Version: 1, Sealed: []byte{1}}); err == nil {
		t.Fatal("a tenant secret with the addon prefix")
	}
	if got, err := s.LatestSecret(bg, e.ID, "token"); err != nil || got.Version != 2 || got.Sealed[1] != 2 {
		t.Fatalf("latest %+v %v", got, err)
	}
	if _, err := s.LatestSecret(bg, e.ID, "none"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("latest of none: %v", err)
	}
	l, err := s.ListSecrets(bg, e.ID, false)
	if err != nil || len(l) != 2 || l[0].Name != "other" || l[1].Name != "token" || l[1].Version != 2 {
		t.Fatalf("tenant secrets %+v %v", l, err)
	}
	if l, err := s.ListSecrets(bg, e.ID, true); err != nil || len(l) != 3 {
		t.Fatalf("system secrets %+v %v", l, err)
	}
	// Soft delete: gone from the list, still resolving.
	if n, err := s.DeleteSecret(bg, e.ID, "token"); err != nil || n != 2 {
		t.Fatalf("delete %d %v", n, err)
	}
	if l, _ := s.ListSecrets(bg, e.ID, false); len(l) != 1 {
		t.Fatalf("after delete %+v", l)
	}
	if got, err := s.GetSecret(bg, e.ID, "token", 1); err != nil || got.DeletedAt.IsZero() {
		t.Fatalf("deleted version %+v %v", got, err)
	}
	if got, err := s.LatestSecret(bg, e.ID, "token"); err != nil || got.Version != 2 {
		t.Fatalf("latest after delete %+v %v", got, err)
	}
	// Purge a removed addon's credentials, not another's.
	if n, err := s.PurgeSecrets(bg, e.ID, "addon.pg."); err != nil || n != 2 {
		t.Fatalf("purge %d %v", n, err)
	}
	if l, _ := s.ListSecrets(bg, e.ID, true); len(l) != 1 || l[0].Name != "addon.pgx.password" {
		t.Fatalf("after purge %+v", l)
	}

	// Route tokens.
	if _, err := s.InsertRouteToken(bg, "hash1", e.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertRouteToken(bg, "hash1", e.ID, ""); !errors.Is(err, store.ErrExists) {
		t.Fatalf("hash taken: %v", err)
	}
	if _, err := s.InsertRouteToken(bg, "hash2", uuid.New(), ""); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown environment: %v", err)
	}
	rt, err := s.ResolveRouteToken(bg, "hash1")
	if err != nil || rt.EnvID != e.ID || !rt.LastUsedAt.IsZero() {
		t.Fatalf("resolve %+v %v", rt, err)
	}
	must(t, s.TouchRouteToken(bg, "hash1"))
	if rt, _ = s.ResolveRouteToken(bg, "hash1"); rt.LastUsedAt.IsZero() {
		t.Fatal("not touched")
	}
	if n, err := s.DeleteRouteTokens(bg, e.ID); err != nil || n != 1 {
		t.Fatalf("revoke %d %v", n, err)
	}
	if _, err := s.ResolveRouteToken(bg, "hash1"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("revoked: %v", err)
	}
}

func TestEnvInstancesAndJobRuns(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	e := mkEnv(t, s, "acme", "stage")
	sb := mkInstance(t, s, e)
	i, err := s.InsertEnvInstance(bg, store.EnvInstance{SandboxID: sb.ID, EnvID: e.ID, Kind: "service", Object: "web",
		Replica: 1, TemplateHash: "h1", Deployment: 3})
	if err != nil || i.Phase != model.PhaseStarting || i.Exits == nil || len(i.Exits) != 0 || i.LastExit != nil ||
		!i.ReadySince.IsZero() {
		t.Fatalf("instance %+v %v", i, err)
	}
	if _, err := s.InsertEnvInstance(bg, store.EnvInstance{SandboxID: sb.ID, EnvID: e.ID, Kind: "service", Object: "web",
		TemplateHash: "h1", Deployment: 3}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("instance twice: %v", err)
	}
	if _, err := s.InsertEnvInstance(bg, store.EnvInstance{SandboxID: podid.Mint(), EnvID: e.ID, Kind: "service", Object: "web",
		TemplateHash: "h1", Deployment: 3}); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("instance of no row: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	code := 137
	after := i
	after.Phase, after.ReadySince, after.Restarts, after.LastExit = model.PhaseExited, now, 1, &code
	after.Exits = []store.EnvExit{{At: now, Code: &code}, {At: now.Add(time.Second)}}
	after.NextRestartAt, after.Detail = now.Add(time.Minute), "exited"
	w, err := s.UpdateEnvInstance(bg, i, after)
	if err != nil || w.Phase != model.PhaseExited || *w.LastExit != 137 || len(w.Exits) != 2 || *w.Exits[0].Code != 137 ||
		w.Exits[1].Code != nil || !w.ReadySince.Equal(now) || w.Detail != "exited" {
		t.Fatalf("updated %+v %v", w, err)
	}
	if again, err := s.UpdateEnvInstance(bg, w, w); err != nil || !again.UpdatedAt.Equal(w.UpdatedAt) {
		t.Fatalf("a write of nothing: %v", err)
	}
	after = w
	after.TemplateHash = "h2"
	if _, err := s.UpdateEnvInstance(bg, w, after); err == nil {
		t.Fatal("a template hash was written")
	}
	after = w
	after.LastExit, after.ReadySince = nil, time.Time{}
	if w, err = s.UpdateEnvInstance(bg, w, after); err != nil || w.LastExit != nil || !w.ReadySince.IsZero() {
		t.Fatalf("cleared %+v %v", w, err)
	}
	rows, err := s.ListEnvInstanceRows(bg, e.ID)
	if err != nil || len(rows) != 1 || rows[0].Instance.SandboxID != sb.ID || rows[0].Sandbox.ID != sb.ID ||
		rows[0].Instance.Detail != "exited" || rows[0].Sandbox.EnvironmentID != e.ID {
		t.Fatalf("rows %+v %v", rows, err)
	}
	if l, err := s.ListEnvInstances(bg, e.ID); err != nil || len(l) != 1 {
		t.Fatalf("list %v %v", l, err)
	}
	if got, err := s.LockEnvInstance(bg, sb.ID, store.NoLock); err != nil || got.Phase != model.PhaseExited {
		t.Fatalf("get %+v %v", got, err)
	}

	// Job runs outlive their rows.
	run, err := s.InsertJobRun(bg, store.EnvJobRun{EnvID: e.ID, Job: "migrate", TemplateHash: "j1", Deployment: 3, SandboxID: sb.ID})
	if err != nil || run.ID == 0 || run.Phase != model.RunRunning || run.Attempt != 1 || run.LogTail == nil {
		t.Fatalf("run %+v %v", run, err)
	}
	if ok, _ := s.JobSucceeded(bg, e.ID, "migrate", "j1"); ok {
		t.Fatal("succeeded while running")
	}
	if got, err := s.JobRunOfSandbox(bg, sb.ID, store.NoLock); err != nil || got.ID != run.ID {
		t.Fatalf("run of sandbox %+v %v", got, err)
	}
	zero := 0
	afterRun := run
	afterRun.Phase, afterRun.ExitCode, afterRun.LogTail, afterRun.FinishedAt = model.RunSucceeded, &zero, []string{"a", "b"}, now
	afterRun.SandboxID = ""
	if run, err = s.UpdateJobRun(bg, run, afterRun); err != nil || run.Phase != model.RunSucceeded || *run.ExitCode != 0 ||
		!slices.Equal(run.LogTail, []string{"a", "b"}) || !run.SandboxID.IsZero() {
		t.Fatalf("finished %+v %v", run, err)
	}
	if ok, err := s.JobSucceeded(bg, e.ID, "migrate", "j1"); err != nil || !ok {
		t.Fatalf("succeeded %v %v", ok, err)
	}
	if ok, _ := s.JobSucceeded(bg, e.ID, "migrate", "j2"); ok {
		t.Fatal("another template succeeded")
	}
	second, err := s.InsertJobRun(bg, store.EnvJobRun{EnvID: e.ID, Job: "migrate", TemplateHash: "j2", Deployment: 4, Attempt: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.LatestJobRun(bg, e.ID, "migrate"); err != nil || got.ID != second.ID {
		t.Fatalf("latest %+v %v", got, err)
	}
	if l, err := s.ListJobRuns(bg, e.ID, "", 10); err != nil || len(l) != 2 || l[0].ID != second.ID {
		t.Fatalf("runs %v %v", l, err)
	}
	if _, err := s.LatestJobRun(bg, e.ID, "seed"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("never ran: %v", err)
	}

	// The sandbox row's deletion takes the instance with it, not the run.
	if _, err := s.DeleteSandbox(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetEnvInstance(bg, sb.ID); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("instance after its row: %v", err)
	}
	if _, err := s.GetJobRun(bg, run.ID); err != nil {
		t.Fatalf("run after its row: %v", err)
	}
}

func TestEnvAttachedVolumesIncidents(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	e := mkEnv(t, s, "acme", "stage")
	box := mkSandbox(t, s, "acme")

	a, err := s.InsertAttached(bg, store.EnvAttached{EnvID: e.ID, Service: "web", SandboxID: box.ID, TemplateHash: "t1"})
	if err != nil || a.Phase != model.PhaseStarting || a.SandboxIncarnation != uuid.Nil || a.ProcessID != "" {
		t.Fatalf("attached %+v %v", a, err)
	}
	if _, err := s.InsertAttached(bg, store.EnvAttached{EnvID: e.ID, Service: "web", SandboxID: box.ID}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("twice: %v", err)
	}
	after := a
	after.SandboxIncarnation, after.ProcessID, after.Phase = box.Incarnation, "p1", model.PhaseReady
	if a, err = s.UpdateAttached(bg, a, after); err != nil || a.ProcessID != "p1" || a.SandboxIncarnation != box.Incarnation {
		t.Fatalf("updated %+v %v", a, err)
	}
	if l, err := s.AttachedOfSandbox(bg, box.ID); err != nil || len(l) != 1 {
		t.Fatalf("of sandbox %v %v", l, err)
	}
	if l, err := s.ListAttached(bg, e.ID); err != nil || len(l) != 1 {
		t.Fatalf("list %v %v", l, err)
	}
	after = a
	after.Service = "api"
	if _, err := s.UpdateAttached(bg, a, after); err == nil {
		t.Fatal("a service was renamed")
	}
	if ok, err := s.DeleteAttached(bg, e.ID, "web"); err != nil || !ok {
		t.Fatalf("delete %v %v", ok, err)
	}
	if _, err := s.GetAttached(bg, e.ID, "web"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("deleted: %v", err)
	}

	// Volumes.
	vid := uuid.New()
	v, err := s.InsertEnvVolume(bg, store.EnvVolume{EnvID: e.ID, Object: "addon/pg", VolumeID: vid, SizeMiB: 1024})
	if err != nil || v.Provisioned || v.Node != "" || !v.Holder.IsZero() {
		t.Fatalf("volume %+v %v", v, err)
	}
	if _, err := s.InsertEnvVolume(bg, store.EnvVolume{EnvID: e.ID, Object: "addon/other", VolumeID: vid, SizeMiB: 1}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("volume id taken: %v", err)
	}
	holder := podid.Mint()
	afterV := v
	afterV.Node, afterV.Provisioned, afterV.Holder = "n1", true, holder
	if v, err = s.UpdateEnvVolume(bg, v, afterV); err != nil || v.Node != "n1" || !v.Provisioned || v.Holder != holder {
		t.Fatalf("provisioned %+v %v", v, err)
	}
	afterV = v
	afterV.SizeMiB = 2048
	if _, err := s.UpdateEnvVolume(bg, v, afterV); err == nil {
		t.Fatal("a volume was resized")
	}
	if got, err := s.LockEnvVolumeByID(bg, vid, store.NoLock); err != nil || got.Object != "addon/pg" {
		t.Fatalf("by id %+v %v", got, err)
	}
	afterV = v
	afterV.Deleting = true
	if v, err = s.UpdateEnvVolume(bg, v, afterV); err != nil {
		t.Fatal(err)
	}
	if l, err := s.DeletingEnvVolumes(bg, 10); err != nil || len(l) != 1 {
		t.Fatalf("deleting %v %v", l, err)
	}
	if l, err := s.ListEnvVolumes(bg, e.ID); err != nil || len(l) != 1 {
		t.Fatalf("list %v %v", l, err)
	}
	if _, err := s.DeleteEnvironment(bg, e.ID); err == nil {
		t.Fatal("an environment with a volume went")
	}
	if ok, err := s.DeleteEnvVolume(bg, e.ID, "addon/pg"); err != nil || !ok {
		t.Fatalf("delete volume %v %v", ok, err)
	}

	// Incidents: one open per (object, deployment, condition).
	in, opened, err := s.OpenIncident(bg, store.EnvIncident{EnvID: e.ID, Object: "service/web", Deployment: 2,
		Condition: model.ConditionCrashLoop, Summary: "3 exits", Evidence: json.RawMessage(`{"exits":[1]}`)})
	if err != nil || !opened || in.ID == 0 || !in.ClosedAt.IsZero() {
		t.Fatalf("open %+v %v %v", in, opened, err)
	}
	again, opened, err := s.OpenIncident(bg, store.EnvIncident{EnvID: e.ID, Object: "service/web", Deployment: 2,
		Condition: model.ConditionCrashLoop, Summary: "other"})
	if err != nil || opened || again.ID != in.ID || again.Summary != "3 exits" {
		t.Fatalf("again %+v %v %v", again, opened, err)
	}
	if _, _, err := s.OpenIncident(bg, store.EnvIncident{EnvID: e.ID, Object: "service/web", Deployment: 2,
		Condition: "bogus"}); err == nil {
		t.Fatal("an unknown condition")
	}
	other, opened, err := s.OpenIncident(bg, store.EnvIncident{EnvID: e.ID, Object: "service/web", Deployment: 3,
		Condition: model.ConditionCrashLoop})
	if err != nil || !opened || string(other.Evidence) != "{}" {
		t.Fatalf("other deployment %+v %v", other, err)
	}
	afterI := in
	afterI.Summary, afterI.Evidence = "4 exits", json.RawMessage(`{"exits": [1, 2]}`)
	if in, err = s.UpdateIncident(bg, in, afterI); err != nil || in.Summary != "4 exits" || !strings.Contains(string(in.Evidence), "2") {
		t.Fatalf("updated %+v %v", in, err)
	}
	if n, err := s.CountOpenIncidents(bg, e.ID); err != nil || n != 2 {
		t.Fatalf("open %d %v", n, err)
	}
	if ok, err := s.CloseIncident(bg, in.ID); err != nil || !ok {
		t.Fatalf("close %v %v", ok, err)
	}
	if ok, _ := s.CloseIncident(bg, in.ID); ok {
		t.Fatal("closed twice")
	}
	if l, err := s.ListIncidents(bg, e.ID, true); err != nil || len(l) != 1 || l[0].ID != other.ID {
		t.Fatalf("open list %v %v", l, err)
	}
	if l, err := s.ListIncidents(bg, e.ID, false); err != nil || len(l) != 2 || l[0].ID != other.ID {
		t.Fatalf("all %v %v", l, err)
	}
	if got, err := s.GetIncident(bg, e.ID, in.ID); err != nil || got.ClosedAt.IsZero() {
		t.Fatalf("closed %+v %v", got, err)
	}
	if _, err := s.GetIncident(bg, uuid.New(), in.ID); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("another environment's: %v", err)
	}
	// Closed, the same condition opens anew.
	if n, opened, err := s.OpenIncident(bg, store.EnvIncident{EnvID: e.ID, Object: "service/web", Deployment: 2,
		Condition: model.ConditionCrashLoop}); err != nil || !opened || n.ID == in.ID {
		t.Fatalf("reopened %+v %v %v", n, opened, err)
	}
}

// The admin listing's count of a tenant's sandboxes leaves its
// environments' instance rows out, as the tenant's sandbox listing does;
// its environments are counted apart (one being deleted is none).
func TestTenantCountLeavesInstancesOut(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkSandbox(t, s, "acme")
	mkInstance(t, s, mkEnv(t, s, "acme", "stage"))
	gone := mkEnv(t, s, "acme", "gone")
	after := gone
	after.Deleting = true
	if _, err := s.UpdateEnvironment(bg, gone, after); err != nil {
		t.Fatal(err)
	}
	mkEnv(t, s, "acme", "prod")
	list, err := s.ListTenants(bg)
	if err != nil || len(list) != 1 || list[0].SandboxCount != 1 || list[0].EnvironmentCount != 2 {
		t.Fatalf("tenants %+v %v", list, err)
	}
}

// publish makes an environment public or not through SetEnvironmentPublic.
func publish(t *testing.T, s *store.Store, id uuid.UUID, public bool) store.Environment {
	t.Helper()
	e, err := s.SetEnvironmentPublic(bg, id, public)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// ingressEnv is the ferun_ingress payload of an environment.
func ingressEnv(id uuid.UUID) string { return "env:" + id.String() }

// Publishing claims the natural host label, sets ingress_group and
// notifies; unpublishing and publishing again keep both. TestClaimHostLabel
// has the labels across tenants.
func TestSetEnvironmentPublic(t *testing.T) {
	s, d := newStore(t)
	mkTenant(t, s, "acme-io")
	ch, unsubscribe := d.Subscribe(db.ChanIngress)
	defer unsubscribe()

	web := mkEnv(t, s, "acme-io", "web")
	if web.Public || web.IngressGroup || web.HostLabel != "" {
		t.Fatalf("new environment %+v", web)
	}
	e := publish(t, s, web.ID, true)
	if !e.Public || !e.IngressGroup || e.HostLabel != "web-acme-io" || !e.UpdatedAt.After(web.UpdatedAt) {
		t.Fatalf("published %+v", e)
	}
	expectPayload(t, ch, ingressEnv(web.ID))
	if got, err := s.GetEnvironment(bg, web.ID); err != nil || got.Public != e.Public || got.HostLabel != e.HostLabel {
		t.Fatalf("read back %+v %v", got, err)
	}
	// Publishing again changes nothing and writes nothing.
	if again := publish(t, s, web.ID, true); !again.UpdatedAt.Equal(e.UpdatedAt) || again.HostLabel != e.HostLabel {
		t.Fatalf("published twice %+v", again)
	}
	expectNoPayload(t, ch, ingressEnv(web.ID))
	e = publish(t, s, web.ID, false)
	if e.Public || !e.IngressGroup || e.HostLabel != "web-acme-io" {
		t.Fatalf("unpublished %+v", e)
	}
	expectPayload(t, ch, ingressEnv(web.ID))
	if e = publish(t, s, web.ID, true); !e.Public || e.HostLabel != "web-acme-io" {
		t.Fatalf("published again %+v", e)
	}
	// The summaries carry the new columns too.
	if l, err := s.EnvSummaries(bg, "acme-io", nil); err != nil || len(l) != 1 || !l[0].Public || !l[0].IngressGroup ||
		l[0].HostLabel != "web-acme-io" {
		t.Fatalf("summaries %+v %v", l, err)
	}

	// A name too long for the natural label: the fallback.
	long := mkEnv(t, s, "acme-io", strings.Repeat("a", 60))
	_, ok, fallback := model.HostLabels(long.Name, long.Owner, long.ID)
	if ok {
		t.Fatal("a natural label of 63+ characters is usable")
	}
	if e := publish(t, s, long.ID, true); e.HostLabel != fallback || len(e.HostLabel) != 63 {
		t.Fatalf("long name %+v (fallback %s)", e, fallback)
	}

	// The CHECK refuses public without a label; UpdateEnvironment never
	// writes the columns; an unknown environment is ErrNoRow.
	plain := mkEnv(t, s, "acme-io", "plain")
	for _, sql := range []string{"UPDATE environments SET public = true WHERE id = $1",
		"UPDATE environments SET public = true, ingress_group = true WHERE id = $1"} {
		if _, err := s.Q().Exec(bg, sql, plain.ID); err == nil || !strings.Contains(err.Error(), "environments_public_ready") {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	after := plain
	after.Public = true
	if _, err := s.UpdateEnvironment(bg, plain, after); err == nil || !strings.Contains(err.Error(), "public") {
		t.Fatalf("public written: %v", err)
	}
	after = plain
	after.HostLabel = "x"
	if _, err := s.UpdateEnvironment(bg, plain, after); err == nil || !strings.Contains(err.Error(), "host_label") {
		t.Fatalf("host_label written: %v", err)
	}
	if _, err := s.SetEnvironmentPublic(bg, uuid.New(), true); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown environment: %v", err)
	}
	// In the caller's transaction, a claim is rolled back with it.
	if err := s.InTx(bg, func(tx *store.Store) error {
		if _, err := tx.SetEnvironmentPublic(bg, plain.ID, true); err != nil {
			return err
		}
		return errors.New("roll back")
	}); err == nil {
		t.Fatal("no error")
	}
	var n int
	must(t, s.Q().QueryRow(bg, "SELECT count(*) FROM host_labels WHERE env_id = $1", plain.ID).Scan(&n))
	if got, _ := s.GetEnvironment(bg, plain.ID); n != 0 || got.Public || got.HostLabel != "" {
		t.Fatalf("rolled back: %d labels, %+v", n, got)
	}
}

// The writes the ingress's table follows send ferun_ingress "env:<id>":
// the ports admitted to an instance (once), a service instance's insert,
// phase or deployment and deletion, an environment's deployment, deployed or
// deleting, a tenant's deletion and an environment's.
func TestIngressNotifications(t *testing.T) {
	s, d := newStore(t)
	mkTenant(t, s, "acme")
	e := mkEnv(t, s, "acme", "stage")
	ch, unsubscribe := d.Subscribe(db.ChanIngress)
	defer unsubscribe()
	want := ingressEnv(e.ID)

	sb := mkInstance(t, s, e)
	i, err := s.InsertEnvInstance(bg, store.EnvInstance{SandboxID: sb.ID, EnvID: e.ID, Kind: "service", Object: "web",
		TemplateHash: "h1", Deployment: 1})
	if err != nil || i.IngressPorts == nil || len(i.IngressPorts) != 0 {
		t.Fatalf("insert %+v %v", i, err)
	}
	expectPayload(t, ch, want)

	// SetIngressPorts writes sorted and unique ports once.
	if ok, err := s.SetIngressPorts(bg, sb.ID, []int{8080, 80, 8080}); err != nil || !ok {
		t.Fatalf("set ports %v %v", ok, err)
	}
	expectPayload(t, ch, want)
	if got, _ := s.GetEnvInstance(bg, sb.ID); !slices.Equal(got.IngressPorts, []int{80, 8080}) {
		t.Fatalf("ports %v", got.IngressPorts)
	}
	if ok, err := s.SetIngressPorts(bg, sb.ID, []int{80, 8080}); err != nil || ok {
		t.Fatalf("set the same ports %v %v", ok, err)
	}
	expectNoPayload(t, ch, want)
	if ok, err := s.SetIngressPorts(bg, sb.ID, nil); err != nil || !ok {
		t.Fatalf("clear ports %v %v", ok, err)
	}
	expectPayload(t, ch, want)
	if ok, err := s.SetIngressPorts(bg, podid.Mint(), []int{80}); err != nil || ok {
		t.Fatalf("ports of no instance %v %v", ok, err)
	}
	must(t, func() error { _, err := s.SetIngressPorts(bg, sb.ID, []int{443}); return err }())
	expectPayload(t, ch, want)

	// A service instance's phase or deployment notifies; its detail does not;
	// ingress_ports is never written by UpdateEnvInstance.
	i, _ = s.GetEnvInstance(bg, sb.ID)
	after := i
	after.Detail = "starting"
	if i, err = s.UpdateEnvInstance(bg, i, after); err != nil {
		t.Fatal(err)
	}
	expectNoPayload(t, ch, want)
	after = i
	after.Phase = model.PhaseReady
	if i, err = s.UpdateEnvInstance(bg, i, after); err != nil {
		t.Fatal(err)
	}
	expectPayload(t, ch, want)
	after = i
	after.Deployment = 2
	if i, err = s.UpdateEnvInstance(bg, i, after); err != nil || !slices.Equal(i.IngressPorts, []int{443}) {
		t.Fatalf("deployment %+v %v", i, err)
	}
	expectPayload(t, ch, want)
	after = i
	after.IngressPorts = []int{80}
	if _, err := s.UpdateEnvInstance(bg, i, after); err == nil || !strings.Contains(err.Error(), "ingress_ports") {
		t.Fatalf("ingress_ports written: %v", err)
	}

	// A job's instance never notifies.
	job := mkInstance(t, s, e)
	j, err := s.InsertEnvInstance(bg, store.EnvInstance{SandboxID: job.ID, EnvID: e.ID, Kind: "job", Object: "migrate",
		TemplateHash: "j1", Deployment: 1})
	must(t, err)
	after = j
	after.Phase = model.PhaseSucceeded
	if _, err := s.UpdateEnvInstance(bg, j, after); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DeleteEnvInstance(bg, job.ID); err != nil || !ok {
		t.Fatalf("delete job instance %v %v", ok, err)
	}
	expectNoPayload(t, ch, want)

	// The service instance's deletion notifies; a second one finds none.
	if ok, err := s.DeleteEnvInstance(bg, sb.ID); err != nil || !ok {
		t.Fatalf("delete instance %v %v", ok, err)
	}
	expectPayload(t, ch, want)
	if ok, err := s.DeleteEnvInstance(bg, sb.ID); err != nil || ok {
		t.Fatalf("delete again %v %v", ok, err)
	}

	// The environment: deployment, deployed and deleting notify, labels do
	// not.
	cur, _ := s.GetEnvironment(bg, e.ID)
	ea := cur
	ea.Labels = map[string]string{"app": "x"}
	if cur, err = s.UpdateEnvironment(bg, cur, ea); err != nil {
		t.Fatal(err)
	}
	expectNoPayload(t, ch, want)
	ea = cur
	ea.Deployment = 1
	if cur, err = s.UpdateEnvironment(bg, cur, ea); err != nil {
		t.Fatal(err)
	}
	expectPayload(t, ch, want)
	ea = cur
	ea.Deployed = 1
	if cur, err = s.UpdateEnvironment(bg, cur, ea); err != nil {
		t.Fatal(err)
	}
	expectPayload(t, ch, want)

	// A deleting tenant's environments, each; the deletion.
	prod := mkEnv(t, s, "acme", "prod")
	marked, err := s.MarkTenantEnvironmentsDeleting(bg, "acme")
	if err != nil || len(marked) != 2 {
		t.Fatalf("marked %v %v", marked, err)
	}
	expectPayload(t, ch, want)
	expectPayload(t, ch, ingressEnv(prod.ID))
	if ok, err := s.DeleteEnvironment(bg, prod.ID); err != nil || !ok {
		t.Fatalf("delete %v %v", ok, err)
	}
	expectPayload(t, ch, ingressEnv(prod.ID))
	if ok, err := s.DeleteEnvironment(bg, prod.ID); err != nil || ok {
		t.Fatalf("delete again %v %v", ok, err)
	}
	expectNoPayload(t, ch, ingressEnv(prod.ID))
	// UpdateEnvironment's deleting.
	other := mkEnv(t, s, "acme", "other")
	oa := other
	oa.Deleting = true
	if _, err := s.UpdateEnvironment(bg, other, oa); err != nil {
		t.Fatal(err)
	}
	expectPayload(t, ch, ingressEnv(other.ID))
}

// An attached service keeps the sandbox it runs in awake (ADR 0018, "An
// attached service idles as an image service does"): while it starts,
// until its awake_until; with an idle, while the sandbox was active within
// it; with idle 0 (never), always; with neither, not at all. Only a
// process in the sandbox's current incarnation counts, and the idle
// loop and its recheck agree.
func TestAttachedServicesKeepTheirSandboxAwake(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkNode(t, s, "n1")
	env := mkEnv(t, s, "acme", "preview")
	sb := mkSandbox(t, s, "acme")
	place(t, s, sb.ID, "n1", model.StateRunning)
	inc := get(t, s, sb.ID).Incarnation
	exec(t, s, "UPDATE sandboxes SET last_active_at = now() - interval '5 minutes' WHERE id = $1", sb.ID)
	idle := func() bool {
		t.Helper()
		rows, err := s.IdleSandboxes(bg, 10*time.Second, []string{"n1"}, 10)
		must(t, err)
		ok, err := s.IdleRecheck(bg, sb.ID, 10*time.Second)
		must(t, err)
		if (len(rows) == 1) != ok {
			t.Fatalf("the loop (%d rows) and the recheck (%v) disagree", len(rows), ok)
		}
		return ok
	}
	if !idle() {
		t.Fatal("a sandbox without attached services idles after its own interval")
	}
	a, err := s.InsertAttached(bg, store.EnvAttached{EnvID: env.ID, Service: "web", SandboxID: sb.ID, SandboxIncarnation: inc,
		ProcessID: "p1"})
	must(t, err)
	if !idle() {
		t.Fatal("an attached service without a claim kept the sandbox awake")
	}
	// It starts: awake until its budget is spent.
	starting := a
	starting.AwakeUntil = time.Now().Add(10 * time.Minute)
	a, err = s.UpdateAttached(bg, a, starting)
	must(t, err)
	if idle() {
		t.Fatal("a starting service did not keep its sandbox awake")
	}
	spent := a
	spent.AwakeUntil = time.Now().Add(-time.Second)
	a, err = s.UpdateAttached(bg, a, spent)
	must(t, err)
	if !idle() {
		t.Fatal("a spent budget kept the sandbox awake")
	}
	// Ready with an idle of 15 minutes: awake within it, not after.
	fifteen := 15 * 60
	ready := a
	ready.AwakeUntil, ready.Phase, ready.IdleAfterS = time.Time{}, model.PhaseReady, &fifteen
	a, err = s.UpdateAttached(bg, a, ready)
	must(t, err)
	if a.IdleAfterS == nil || *a.IdleAfterS != fifteen || !a.AwakeUntil.IsZero() {
		t.Fatalf("written: %+v", a)
	}
	if idle() {
		t.Fatal("active 5 minutes ago, within its 15 minutes: paused")
	}
	exec(t, s, "UPDATE sandboxes SET last_active_at = now() - interval '20 minutes' WHERE id = $1", sb.ID)
	if !idle() {
		t.Fatal("past its idle, the sandbox did not idle")
	}
	// Never: awake while its process runs.
	zero := 0
	never := a
	never.IdleAfterS = &zero
	a, err = s.UpdateAttached(bg, a, never)
	must(t, err)
	if idle() {
		t.Fatal("idle never: paused")
	}
	// Its process gone, or of an older incarnation: no claim.
	gone := a
	gone.ProcessID = ""
	a, err = s.UpdateAttached(bg, a, gone)
	must(t, err)
	if !idle() {
		t.Fatal("a service without a process kept the sandbox awake")
	}
	back := a
	back.ProcessID = "p2"
	a, err = s.UpdateAttached(bg, a, back)
	must(t, err)
	exec(t, s, "UPDATE sandboxes SET incarnation = gen_random_uuid() WHERE id = $1", sb.ID)
	if !idle() {
		t.Fatal("a process of the old incarnation kept the new one awake")
	}
}
