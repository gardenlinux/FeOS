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
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	volumev1 "github.com/gardenlinux/feos/go/gen/feos/volume/v1"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/env/envdeps"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// execLog records the Execs of the environments' pods, which exit 0.
type execLog struct {
	mu    sync.Mutex
	calls []*containerv1.ExecRequest
	pods  []podid.ID
}

func (l *execLog) fn(_ context.Context, pod podid.ID, req *containerv1.ExecRequest, _ testnode.ExecOutput) (int32, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, req)
	l.pods = append(l.pods, pod)
	return 0, false, nil
}

// find returns the recorded Execs whose command's first word is argv0.
func (l *execLog) find(argv0 string) []*containerv1.ExecRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*containerv1.ExecRequest
	for _, c := range l.calls {
		if len(c.Command) > 0 && c.Command[0] == argv0 {
			out = append(out, c)
		}
	}
	return out
}

const postgresSpec = `{"addons":{"postgres":{"type":"postgres","version":"17","plan":"dev",
	"options":{"databases":["app"],"roles":["svc"]}}}}`

// An addon (the postgres blueprint) comes up with its credentials as
// system secrets, its volume created once on its node with the plan's
// size, its row pinned there, its env group put before its pod, its probe
// passing and its provisioning run with the credentials in its env; a
// plan change recreates it on the same node with the same volume.
func TestAddonLifecycle(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	execs := &execLog{}
	n1.Script().SetExec(execs.fn)
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, postgresSpec)
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)

	// Credentials: 32 hex characters, version 1, system.
	var creds []string
	for _, name := range []string{"addon.postgres.password", "addon.postgres.role.svc.password"} {
		sec, err := h.st.GetSecret(bg, env.ID, name, 1)
		if err != nil || !sec.System {
			t.Fatalf("secret %s: %+v %v", name, sec, err)
		}
		plain, err := h.sealer.Open(sec.Sealed, envdeps.SecretAAD(env.ID, name, 1))
		if err != nil || len(plain) != 32 || strings.Trim(string(plain), "0123456789abcdef") != "" {
			t.Fatalf("credential %s: %q %v", name, plain, err)
		}
		creds = append(creds, string(plain))
	}
	if tenant, _ := h.st.ListSecrets(bg, env.ID, false); len(tenant) != 0 {
		t.Errorf("the credentials are listed as the tenant's: %+v", tenant)
	}

	rows := h.rowsOf(env.ID, spec.KindAddon, "postgres")
	if len(rows) != 1 || !ready(rows[0]) {
		t.Fatalf("rows %+v", rows)
	}
	first := rows[0]
	if first.Sandbox.PinNode != "n1" || first.Sandbox.IdleAfterS == nil || *first.Sandbox.IdleAfterS != 0 ||
		first.Instance.ProvisionedHash == "" || first.Instance.Deployment != r1 || first.Sandbox.Address == "" {
		t.Errorf("instance %+v / %+v", first.Instance, first.Sandbox)
	}
	tmpl := first.Sandbox.PodTemplate
	if tmpl == nil || len(tmpl.Volumes) != 1 || tmpl.Mounts[0].Path != "/var/lib/postgresql/data" ||
		tmpl.Env["POSTGRES_PASSWORD"].Parts[0].Secret != "addon.postgres.password" || tmpl.HasCredential() ||
		tmpl.Labels[model.LabelObject] != "addon/postgres" || tmpl.Labels[model.LabelTemplate] != first.Instance.TemplateHash {
		t.Fatalf("template %+v", tmpl)
	}
	vol, err := h.st.GetEnvVolume(bg, env.ID, "addon/postgres")
	if err != nil || vol.SizeMiB != 1024 || !vol.Provisioned || vol.Node != "n1" || vol.Holder != first.Sandbox.ID ||
		vol.VolumeID != tmpl.Volumes[0].VolumeID {
		t.Fatalf("volume %+v %v", vol, err)
	}
	nv := n1.Volumes()
	if len(nv) != 1 || nv[0].VolumeId != vol.VolumeID.String() || nv[0].SizeBytes != 1024<<20 ||
		nv[0].PodId != first.Sandbox.ID.String() || nv[0].State != volumev1.VolumeState_VOLUME_STATE_ATTACHED ||
		nv[0].Labels[model.LabelObject] != "addon/postgres" {
		t.Fatalf("the node's volumes %v", nv)
	}
	pod := h.pod("n1", first.Sandbox.ID)
	if pod == nil || !slices.Equal(pod.Spec.GetNetwork().GetSecurityGroups(), h.podGroups(env.ID)) {
		t.Fatalf("pod %v", pod)
	}
	if pod.Spec.Containers[0].Env["POSTGRES_PASSWORD"] != creds[0] {
		t.Error("the pod's env has not the credential")
	}
	g := h.group("n1", model.EnvGroupID(env.ID))
	if g == nil || !slices.Contains(g.Members, first.Sandbox.Address) {
		t.Fatalf("env group %v", g)
	}
	// The probe ran; the provisioning ran with the credentials in its env,
	// not in its argv.
	if len(execs.find("pg_isready")) == 0 {
		t.Error("the probe never ran")
	}
	prov := execs.find("sh")
	if len(prov) == 0 {
		t.Fatal("the provisioning never ran")
	}
	p := prov[0]
	if p.Env["PGPASSWORD"] != creds[0] || p.Env["FERUN_ROLE_PASSWORD_svc"] != creds[1] {
		t.Errorf("provisioning env %v", p.Env)
	}
	for _, a := range p.Command {
		if strings.Contains(a, creds[0]) || strings.Contains(a, creds[1]) {
			t.Error("a credential is in the provisioning's argv")
		}
	}

	// A plan change recreates it, on the same node, with the same volume.
	r2 := h.apply(env.ID, strings.Replace(postgresSpec, `"dev"`, `"small"`, 1))
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 60*time.Second)
	rows = h.rowsOf(env.ID, spec.KindAddon, "postgres")
	if len(rows) != 1 || rows[0].Sandbox.ID == first.Sandbox.ID || !ready(rows[0]) {
		t.Fatalf("after the recreate %+v", rows)
	}
	second := rows[0]
	if second.Sandbox.Node != "n1" || second.Sandbox.PinNode != "n1" || second.Sandbox.MemoryMiB != 1024 ||
		second.Sandbox.PodTemplate.Volumes[0].VolumeID != vol.VolumeID || second.Instance.ProvisionedHash == "" {
		t.Fatalf("second %+v %+v", second.Instance, second.Sandbox)
	}
	vol2, err := h.st.GetEnvVolume(bg, env.ID, "addon/postgres")
	if err != nil || vol2.SizeMiB != 1024 || vol2.Node != "n1" || vol2.Holder != second.Sandbox.ID {
		t.Fatalf("volume after the recreate %+v %v", vol2, err)
	}
	if _, err := h.st.GetSandbox(bg, first.Sandbox.ID); err == nil {
		t.Error("the old instance's row stays")
	}
	nv = n1.Volumes()
	if len(nv) != 1 || nv[0].PodId != second.Sandbox.ID.String() || nv[0].SizeBytes != 1024<<20 {
		t.Fatalf("the node's volumes after the recreate %v", nv)
	}
	// The credentials stayed.
	for i, name := range []string{"addon.postgres.password", "addon.postgres.role.svc.password"} {
		if v, err := h.c.ResolveSecret(bg, env.ID, name, 1); err != nil || v != creds[i] {
			t.Errorf("credential %s changed: %v", name, err)
		}
	}
	// A role option alone reprovisions it, without a recreate.
	r3 := h.apply(env.ID, strings.Replace(strings.Replace(postgresSpec, `"dev"`, `"small"`, 1), `["svc"]`, `["svc","ops"]`, 1))
	h.waitDeployment(env.ID, r3, model.DeploymentDeployed, 60*time.Second)
	rows = h.rowsOf(env.ID, spec.KindAddon, "postgres")
	if len(rows) != 1 || rows[0].Sandbox.ID != second.Sandbox.ID || rows[0].Instance.ProvisionedHash == second.Instance.ProvisionedHash {
		t.Fatalf("after a role was added %+v", rows)
	}
	if _, err := h.st.GetSecret(bg, env.ID, "addon.postgres.role.ops.password", 1); err != nil {
		t.Errorf("the new role's credential: %v", err)
	}
	if rel := h.deployment(env.ID, r1); rel.State != model.DeploymentReplaced {
		t.Errorf("deployment %d is %s", r1, rel.State)
	}
	if e, _ := h.st.GetEnvironment(bg, env.ID); e.Deployed != r3 {
		t.Errorf("deployed %d", e.Deployed)
	}

	// Removed, the addon loses its instance, its volume and its credentials.
	r4 := h.apply(env.ID, `{}`)
	h.waitDeployment(env.ID, r4, model.DeploymentDeployed, 60*time.Second)
	eventually(t, 60*time.Second, "the volume gone", func() bool {
		_, err := h.st.GetEnvVolume(bg, env.ID, "addon/postgres")
		return err != nil && len(n1.Volumes()) == 0
	})
	if secs, _ := h.st.ListSecrets(bg, env.ID, true); len(secs) != 0 {
		t.Errorf("credentials stay: %+v", secs)
	}
}

// Without --secret-key-file an addon is not deployed: its deployment stays
// deploying with the reason, and no incident opens.
func TestAddonWithoutSealer(t *testing.T) {
	h := newHarness(t, options{noSealer: true})
	h.addNode("n1")
	env := h.newEnv("stage")
	r := h.apply(env.ID, postgresSpec)
	eventually(t, 30*time.Second, "the reason", func() bool {
		rel := h.deployment(env.ID, r)
		return rel.State == model.DeploymentDeploying && rel.Reason == "secrets need --secret-key-file"
	})
	if rows := h.rows(env.ID); len(rows) != 0 {
		t.Errorf("rows %+v", rows)
	}
	if is := h.incidents(env.ID, false); len(is) != 0 {
		t.Errorf("incidents %+v", is)
	}
	if _, err := h.c.ResolveSecret(bg, env.ID, "x", 1); err == nil || err.Error() != "secrets need --secret-key-file" {
		t.Errorf("resolve without a sealer: %v", err)
	}
}

const valkeySpec = `{"addons":{"cache":{"type":"valkey","version":"8","plan":"dev"}}}`

// An addon whose row goes is inserted again on its volume's node once the
// old pod let go of the volume; a node without the volume fails the
// create in the prepare hook and opens volume_unavailable, which closes
// once the addon runs again.
func TestAddonVolumeUnavailable(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, valkeySpec)
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	first := h.waitRows(env.ID, spec.KindAddon, "cache", 1, 10*time.Second, "ready", ready)[0]
	vol, err := h.st.GetEnvVolume(bg, env.ID, "addon/cache")
	if err != nil || vol.SizeMiB != 256 || vol.Holder != first.Sandbox.ID {
		t.Fatalf("volume %+v %v", vol, err)
	}
	if first.Sandbox.MemoryMiB != 256 {
		t.Errorf("a 128 MiB plan's pod has %d MiB", first.Sandbox.MemoryMiB)
	}
	// The node loses the volume.
	n1.Script().SetHook(volumev1.VolumeService_GetVolume_FullMethodName, func(context.Context, string, any) error {
		return status.Error(codes.NotFound, "no volume "+vol.VolumeID.String())
	})
	if _, _, err := h.eng.Delete(bg, "", first.Sandbox.ID); err != nil {
		t.Fatal(err)
	}
	inc := h.incident(env.ID, "addon/cache", model.ConditionVolumeUnavailable, 60*time.Second)
	if !strings.Contains(inc.Summary, "node n1 does not have volume "+vol.VolumeID.String()) {
		t.Errorf("incident %+v", inc)
	}
	rows := h.rowsOf(env.ID, spec.KindAddon, "cache")
	if len(rows) != 1 || rows[0].Sandbox.PinNode != "n1" || rows[0].Sandbox.ClientState == model.StateRunning {
		t.Fatalf("rows %+v", rows)
	}
	// The volume is back: the addon runs again with it, the incident closes.
	n1.Script().SetHook(volumev1.VolumeService_GetVolume_FullMethodName, nil)
	second := h.waitRows(env.ID, spec.KindAddon, "cache", 1, 60*time.Second, "ready again", ready)[0]
	if second.Sandbox.ID == first.Sandbox.ID || second.Sandbox.Node != "n1" {
		t.Fatalf("second %+v", second.Sandbox)
	}
	eventually(t, 30*time.Second, "the incident closed", func() bool { return len(h.incidents(env.ID, true)) == 0 })
	if nv := n1.Volumes(); len(nv) != 1 || nv[0].PodId != second.Sandbox.ID.String() {
		t.Errorf("the node's volume %v", nv)
	}
	if v2, _ := h.st.GetEnvVolume(bg, env.ID, "addon/cache"); v2.Holder != second.Sandbox.ID || v2.VolumeID != vol.VolumeID {
		t.Errorf("volume %+v", v2)
	}
}

// cacheWebSpec is a valkey addon and a service web that renders its URL.
func cacheWebSpec() string {
	return fmt.Sprintf(`{"addons":{"cache":{"type":"valkey","version":"8","plan":"dev"}},
		"services":{"web":{"image":%q,"env":{"CACHE":{"addon":"cache","key":"url"}}}}}`, testImage)
}

// An addon whose pod a node that is up no longer has (its row lost with
// lost_from empty) is replaced on that node once the node reports the
// volume free (§13.7): the new row is pinned there and holds the same
// volume, and its new address rolls the dependents.
func TestAddonLostOnLiveNode(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, cacheWebSpec())
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	first := h.waitRows(env.ID, spec.KindAddon, "cache", 1, 10*time.Second, "ready", ready)[0]
	web := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0]
	vol, err := h.st.GetEnvVolume(bg, env.ID, "addon/cache")
	if err != nil || vol.Node != "n1" || vol.Holder != first.Sandbox.ID {
		t.Fatalf("volume %+v %v", vol, err)
	}
	if err := n1.Script().ForgetPod(first.Sandbox.ID); err != nil {
		t.Fatal(err)
	}
	// A pod that is not ferund's takes the lost pod's address (the test
	// node hands out the lowest free one), so that the new addon's differs.
	if _, err := podv1.NewPodServiceClient(h.conn("n1")).CreatePod(bg, &podv1.CreatePodRequest{PodId: uuid.NewString(),
		Spec: &podv1.PodSpec{
			Resources:  &podv1.Resources{Vcpus: 1, MemoryMib: 256},
			Containers: []*podv1.ContainerSpec{{Name: "main", ImageRef: testImage, Command: []string{"sleep", "infinity"}}},
			Network:    &podv1.PodNetwork{},
		}}); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.Sync(bg, first.Sandbox.ID); err != nil { // finds it lost, sooner than the engine's loops
		t.Fatal(err)
	}
	second := h.waitRows(env.ID, spec.KindAddon, "cache", 1, 60*time.Second, "replaced and ready", func(r store.EnvInstanceRow) bool {
		return r.Sandbox.ID != first.Sandbox.ID && ready(r)
	})[0]
	if second.Sandbox.Node != "n1" || second.Sandbox.PinNode != "n1" || second.Sandbox.Address == first.Sandbox.Address {
		t.Errorf("the new addon on %s, pinned to %s, address %s (was %s)", second.Sandbox.Node, second.Sandbox.PinNode,
			second.Sandbox.Address, first.Sandbox.Address)
	}
	if _, err := h.st.GetSandbox(bg, first.Sandbox.ID); err == nil {
		t.Error("the lost row stays")
	}
	if v2, err := h.st.GetEnvVolume(bg, env.ID, "addon/cache"); err != nil || v2.VolumeID != vol.VolumeID ||
		v2.Holder != second.Sandbox.ID || v2.Node != "n1" {
		t.Errorf("volume %+v %v", v2, err)
	}
	if nv := n1.Volumes(); len(nv) != 1 || nv[0].VolumeId != vol.VolumeID.String() || nv[0].PodId != second.Sandbox.ID.String() {
		t.Errorf("the node's volumes %v", nv)
	}
	eventually(t, 60*time.Second, "web rolled to the new address", func() bool {
		rows := h.rowsOf(env.ID, spec.KindService, "web")
		if len(rows) != 1 || rows[0].Sandbox.ID == web.Sandbox.ID || !ready(rows[0]) {
			return false
		}
		p := h.pod("n1", rows[0].Sandbox.ID)
		return p != nil && strings.Contains(p.Spec.Containers[0].Env["CACHE"], "["+second.Sandbox.Address+"]")
	})
	for _, i := range h.incidents(env.ID, false) {
		if i.Condition == model.ConditionVolumeUnavailable {
			t.Errorf("incident %+v", i)
		}
	}
}

// A lost addon on a node that is up, with its volume free there, is
// recreated there when a new deployment changes its template, as a lost one
// is replaced: R does not wait for it to run again, which it never would.
func TestAddonLostOnLiveNodeRecreated(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, valkeySpec)
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	first := h.waitRows(env.ID, spec.KindAddon, "cache", 1, 10*time.Second, "ready", ready)[0]
	vol, err := h.st.GetEnvVolume(bg, env.ID, "addon/cache")
	if err != nil {
		t.Fatal(err)
	}
	// The node loses the pod, and the next deployment changes the addon's
	// template before the step replaced it.
	h.exec(`UPDATE nodes SET draining = true WHERE name = 'n1'`) // no replacement for now
	if err := n1.Script().ForgetPod(first.Sandbox.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.Sync(bg, first.Sandbox.ID); err != nil {
		t.Fatal(err)
	}
	if sb, _ := h.st.GetSandbox(bg, first.Sandbox.ID); sb.ClientState != model.StateLost {
		t.Fatalf("the addon is %s after the sync", sb.ClientState)
	}
	r2 := h.apply(env.ID, `{"addons":{"cache":{"type":"valkey","version":"8","plan":"dev",
		"options":{"maxmemoryPolicy":"allkeys-lru"}}}}`)
	h.exec(`UPDATE nodes SET draining = false, drain_started_at = NULL WHERE name = 'n1'`)
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 60*time.Second)
	second := h.waitRows(env.ID, spec.KindAddon, "cache", 1, 10*time.Second, "ready", ready)[0]
	if second.Sandbox.ID == first.Sandbox.ID || second.Sandbox.Node != "n1" || second.Sandbox.PinNode != "n1" ||
		second.Instance.TemplateHash == first.Instance.TemplateHash || second.Instance.Deployment != r2 {
		t.Errorf("the recreated addon %+v / %+v", second.Instance, second.Sandbox)
	}
	if v2, err := h.st.GetEnvVolume(bg, env.ID, "addon/cache"); err != nil || v2.VolumeID != vol.VolumeID ||
		v2.Holder != second.Sandbox.ID {
		t.Errorf("volume %+v %v", v2, err)
	}
	if nv := n1.Volumes(); len(nv) != 1 || nv[0].PodId != second.Sandbox.ID.String() {
		t.Errorf("the node's volumes %v", nv)
	}
}

// A lost addon whose node is up is not replaced while that node does not
// have its volume, or has it attached to another pod: the row stays lost
// and volume_unavailable says what the node answered; the volume is left
// alone. Once the volume is free there, the addon is replaced.
func TestAddonLostOnLiveNodeVolumeHeld(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, valkeySpec)
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	first := h.waitRows(env.ID, spec.KindAddon, "cache", 1, 10*time.Second, "ready", ready)[0]
	vol, err := h.st.GetEnvVolume(bg, env.ID, "addon/cache")
	if err != nil {
		t.Fatal(err)
	}
	// The node loses the pod, and another pod (not ferund's) takes the
	// volume before anyone asks for it.
	gate := make(chan struct{})
	n1.Script().SetHook(volumev1.VolumeService_GetVolume_FullMethodName, func(ctx context.Context, _ string, _ any) error {
		select {
		case <-gate:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err := n1.Script().ForgetPod(first.Sandbox.ID); err != nil {
		t.Fatal(err)
	}
	other := uuid.New()
	pc := podv1.NewPodServiceClient(h.conn("n1"))
	if _, err := pc.CreatePod(bg, &podv1.CreatePodRequest{PodId: other.String(), Spec: &podv1.PodSpec{
		Resources:  &podv1.Resources{Vcpus: 1, MemoryMib: 256},
		Containers: []*podv1.ContainerSpec{{Name: "main", ImageRef: testImage, Command: []string{"sleep", "infinity"}}},
		Volumes:    []*podv1.PodVolume{{Name: "data", VolumeId: vol.VolumeID.String()}},
	}}); err != nil {
		t.Fatal(err)
	}
	close(gate)
	if err := h.eng.Sync(bg, first.Sandbox.ID); err != nil {
		t.Fatal(err)
	}
	inc := h.incident(env.ID, "addon/cache", model.ConditionVolumeUnavailable, 60*time.Second)
	want := fmt.Sprintf("addon cache is lost on node n1 and not replaced there: volume %s is attached to pod %s there",
		vol.VolumeID, other)
	if inc.Summary != want {
		t.Errorf("incident %q, want %q", inc.Summary, want)
	}
	never(t, 3*time.Second, "the lost addon replaced", func() bool {
		rows := h.rowsOf(env.ID, spec.KindAddon, "cache")
		return len(rows) != 1 || rows[0].Sandbox.ID != first.Sandbox.ID || rows[0].Sandbox.ClientState != model.StateLost
	})
	if nv := n1.Volumes(); len(nv) != 1 || nv[0].PodId != other.String() {
		t.Errorf("the node's volumes %v", nv)
	}

	// The other pod goes, and the node loses the volume: still not
	// replaced.
	n1.Script().SetHook(volumev1.VolumeService_GetVolume_FullMethodName, func(context.Context, string, any) error {
		return status.Error(codes.NotFound, "no volume "+vol.VolumeID.String())
	})
	if _, err := pc.DeletePod(bg, &podv1.DeletePodRequest{PodId: other.String()}); err != nil {
		t.Fatal(err)
	}
	never(t, 3*time.Second, "the lost addon replaced without its volume", func() bool {
		rows := h.rowsOf(env.ID, spec.KindAddon, "cache")
		return len(rows) != 1 || rows[0].Sandbox.ID != first.Sandbox.ID
	})

	// The volume is free there: the addon is replaced with it, and the
	// incident closes.
	n1.Script().SetHook(volumev1.VolumeService_GetVolume_FullMethodName, nil)
	second := h.waitRows(env.ID, spec.KindAddon, "cache", 1, 60*time.Second, "replaced and ready", func(r store.EnvInstanceRow) bool {
		return r.Sandbox.ID != first.Sandbox.ID && ready(r)
	})[0]
	if nv := n1.Volumes(); len(nv) != 1 || nv[0].PodId != second.Sandbox.ID.String() {
		t.Errorf("the node's volumes %v", nv)
	}
	eventually(t, 30*time.Second, "the incident closed", func() bool { return len(h.incidents(env.ID, true)) == 0 })
}

// An addon counts as ready only while its row runs (§13.7): with its
// phase still ready and its row unknown (its node out of touch), the
// deployment waits for it ("addon cache is unknown …") and its deploy job
// does not start against an addon that may be down.
func TestAddonReadyOnlyWhileLive(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, valkeySpec)
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	addon := h.waitRows(env.ID, spec.KindAddon, "cache", 1, 10*time.Second, "ready", ready)[0]
	n1.Script().SetHook(podv1.PodService_GetPod_FullMethodName, func(_ context.Context, _ string, req any) error {
		if r, ok := req.(*podv1.GetPodRequest); ok && r.GetPodId() == addon.Sandbox.ID.String() {
			return status.Error(codes.Unavailable, "out of touch")
		}
		return nil
	})
	h.exec(`UPDATE sandboxes SET client_state = 'unknown', status_description = 'out of touch' WHERE id = $1`, addon.Sandbox.ID)
	r2 := h.apply(env.ID, fmt.Sprintf(`{"addons":{"cache":{"type":"valkey","version":"8","plan":"dev"}},
		"jobs":{"migrate":{"image":%q,"command":["/app","migrate"]}}}`, testImage))
	never(t, 4*time.Second, "the deploy job started while its addon is unknown", func() bool {
		return len(h.rowsOf(env.ID, spec.KindJob, "migrate")) > 0
	})
	if inst, err := h.st.GetEnvInstance(bg, addon.Sandbox.ID); err != nil || inst.Phase != model.PhaseReady {
		t.Fatalf("the addon's phase %q %v: the test did not hold it", inst.Phase, err)
	}
	h.c.mu.Lock()
	pending := h.c.lastPending["pending/"+env.ID.String()]
	h.c.mu.Unlock()
	if !strings.Contains(pending, "addon cache is unknown (out of touch)") {
		t.Errorf("the deployment waits for %q", pending)
	}
	// Back in touch: the job runs, the deployment is deployed.
	n1.Script().SetHook(podv1.PodService_GetPod_FullMethodName, nil)
	finishRun(t, h, n1, env.ID, 0)
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 60*time.Second)
}

// A drained node's addon restores on another node with its volume (the
// pin gates creates only); the controller records the volume's
// new node and pins the row there.
func TestAddonMovesWithDrain(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, valkeySpec)
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	first := h.waitRows(env.ID, spec.KindAddon, "cache", 1, 10*time.Second, "ready", ready)[0]
	if first.Sandbox.Node != "n1" {
		t.Fatalf("on %s", first.Sandbox.Node)
	}
	n2 := h.addNode("n2")
	h.exec("UPDATE nodes SET draining = true, drain_started_at = now() WHERE name = 'n1'")
	eventually(t, 90*time.Second, "the addon on n2", func() bool {
		h.c.notify(bg, "drain")
		rows := h.rowsOf(env.ID, spec.KindAddon, "cache")
		return len(rows) == 1 && rows[0].Sandbox.ID == first.Sandbox.ID && rows[0].Sandbox.Node == "n2" && running(rows[0])
	})
	eventually(t, 30*time.Second, "the volume's node and the pin", func() bool {
		vol, err := h.st.GetEnvVolume(bg, env.ID, "addon/cache")
		rows := h.rowsOf(env.ID, spec.KindAddon, "cache")
		return err == nil && vol.Node == "n2" && len(rows) == 1 && rows[0].Sandbox.PinNode == "n2"
	})
	if nv := n2.Volumes(); len(nv) != 1 || nv[0].PodId != first.Sandbox.ID.String() {
		t.Errorf("n2's volumes %v", nv)
	}
	h.waitRows(env.ID, spec.KindAddon, "cache", 1, 30*time.Second, "ready on n2", ready)
	// The record the volume left on n1 goes once n1's stale pod is gone.
	eventually(t, 60*time.Second, "n1's record of the volume gone", func() bool {
		h.c.notify(bg, LoopVolumes)
		return len(n1.Volumes()) == 0
	})
	// The environment deleted, no record of its volume is left anywhere.
	h.exec(`UPDATE environments SET deleting = true WHERE id = $1`, env.ID)
	eventually(t, 60*time.Second, "the environment and its volume gone", func() bool {
		h.c.notify(bg, LoopVolumes)
		_, err := h.st.GetEnvironment(bg, env.ID)
		return errors.Is(err, store.ErrNoRow) && len(n1.Volumes()) == 0 && len(n2.Volumes()) == 0
	})
}

// An addon restored on another node whose pod stopped there before the
// controller moved its volume's record (env_volumes.node and pin_node
// still the old node): the step moves both to the node its pod is
// materialised on although it does not run, and it starts there again
// (§13.7; a start is never pinned).
func TestAddonVolumeFollowsStoppedRow(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, valkeySpec)
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	first := h.waitRows(env.ID, spec.KindAddon, "cache", 1, 10*time.Second, "ready", ready)[0]
	n2 := h.addNode("n2")
	h.exec("UPDATE nodes SET draining = true, drain_started_at = now() WHERE name = 'n1'")
	eventually(t, 90*time.Second, "the addon running on n2, its volume recorded there", func() bool {
		h.c.notify(bg, "drain")
		rows := h.rowsOf(env.ID, spec.KindAddon, "cache")
		vol, err := h.st.GetEnvVolume(bg, env.ID, "addon/cache")
		return len(rows) == 1 && rows[0].Sandbox.ID == first.Sandbox.ID && rows[0].Sandbox.Node == "n2" && running(rows[0]) &&
			err == nil && vol.Node == "n2" && rows[0].Sandbox.PinNode == "n2"
	})
	// n2 refuses to start it for now; its pod stops there; the record
	// says n1 again, as before the controller saw the restore.
	n2.Script().SetHook(podv1.PodService_StartPod_FullMethodName, func(context.Context, string, any) error {
		return status.Error(codes.FailedPrecondition, "not now")
	})
	if _, err := podv1.NewPodServiceClient(h.conn("n2")).StopPod(bg, &podv1.StopPodRequest{PodId: first.Sandbox.ID.String()}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the addon stopped on n2", func() bool {
		sb, err := h.st.GetSandbox(bg, first.Sandbox.ID)
		return err == nil && (sb.ClientState == model.StateStopped || sb.ClientState == model.StateFailed) && sb.Node == "n2"
	})
	h.exec(`UPDATE env_volumes SET node = 'n1' WHERE env_id = $1`, env.ID)
	h.exec(`UPDATE sandboxes SET pin_node = 'n1' WHERE id = $1`, first.Sandbox.ID)
	eventually(t, 30*time.Second, "the volume's node and the pin moved to n2 while it does not run", func() bool {
		vol, err := h.st.GetEnvVolume(bg, env.ID, "addon/cache")
		sb, err2 := h.st.GetSandbox(bg, first.Sandbox.ID)
		return err == nil && err2 == nil && vol.Node == "n2" && sb.PinNode == "n2" && sb.ClientState != model.StateRunning
	})
	n2.Script().SetHook(podv1.PodService_StartPod_FullMethodName, nil)
	again := h.waitRows(env.ID, spec.KindAddon, "cache", 1, 60*time.Second, "ready again", ready)[0]
	if again.Sandbox.ID != first.Sandbox.ID || again.Sandbox.Node != "n2" {
		t.Errorf("the addon after the start: %+v", again.Sandbox)
	}
}

// removeNode removes a node as the admin API does with force: its row
// stays, marked removed, its certificates revoked, its connections closed.
func (h *harness) removeNode(name string) {
	h.t.Helper()
	if err := h.st.InTx(bg, func(tx *store.Store) error {
		if _, err := tx.LockNode(bg, name, store.ForUpdate); err != nil {
			return err
		}
		if _, err := tx.RemoveNode(bg, name); err != nil {
			return err
		}
		return tx.Notify(bg, db.ChanLeader, "*")
	}); err != nil {
		h.t.Fatal(err)
	}
	eventually(h.t, 30*time.Second, "node "+name+" disconnected", func() bool { return !h.srv.Connected(name) })
}

// A removed node never serves its volumes again: an addon whose row went
// is not pinned there again (volume_unavailable), and a deleting tenant's
// environment and the tenant are deleted although their volume was on it.
func TestVolumeOfRemovedNode(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, valkeySpec)
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	first := h.waitRows(env.ID, spec.KindAddon, "cache", 1, 10*time.Second, "ready", ready)[0]
	h.removeNode("n1")
	if _, _, err := h.eng.Delete(bg, "", first.Sandbox.ID); err != nil {
		t.Fatal(err)
	}
	h.incident(env.ID, "addon/cache", model.ConditionVolumeUnavailable, 30*time.Second)
	never(t, 3*time.Second, "an addon row pinned to the removed node", func() bool {
		return len(h.rowsOf(env.ID, spec.KindAddon, "cache")) != 0
	})

	if _, err := h.st.MarkTenantDeleting(bg, testTenant); err != nil {
		t.Fatal(err)
	}
	eventually(t, 60*time.Second, "the environment gone", func() bool {
		_, err := h.st.GetEnvironment(bg, env.ID)
		return errors.Is(err, store.ErrNoRow)
	})
	var n int
	if err := h.d.Pool.QueryRow(bg, `SELECT count(*) FROM env_volumes`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d volume rows stay, %v", n, err)
	}
	// Nothing of the environment holds the tenant's deletion (the engine's
	// loop deletes it within its own interval).
	if deleted, err := h.st.DeleteTenantIfEmpty(bg, testTenant); err != nil || !deleted {
		t.Errorf("the tenant was not deleted: %v", err)
	}
}

// cacheServiceSpec is a valkey addon of version and a service web of
// version (which a new template rolls).
func cacheServiceSpec(valkey, web string) string {
	return fmt.Sprintf(`{"addons":{"cache":{"type":"valkey","version":%q,"plan":"dev"}},
		"services":{"web":{"image":%q,"command":["/app"],"env":{"VERSION":%q}}}}`, valkey, testImage, web)
}

// A deployment that upgraded an addon and then failed (a service's new
// instance crashed) keeps the addon as it recreated it: T falls back to
// the deployed deployment, whose older version is never started again on
// the data the newer one wrote, and the addon is not recreated a second
// time; the volume records the version that ran on it.
func TestFailedDeploymentKeepsAddon(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, cacheServiceSpec("7.2", "1"))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	old := h.waitRows(env.ID, spec.KindAddon, "cache", 1, 10*time.Second, "ready", ready)[0]
	web := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0]
	if vol, err := h.st.GetEnvVolume(bg, env.ID, "addon/cache"); err != nil || vol.AddonType != "valkey" || vol.DataVersion != "7.2" {
		t.Fatalf("volume %+v %v", vol, err)
	}

	r2 := h.apply(env.ID, cacheServiceSpec("8", "2"))
	upgraded := h.waitRows(env.ID, spec.KindAddon, "cache", 1, 60*time.Second, "upgraded", func(r store.EnvInstanceRow) bool {
		return r.Sandbox.ID != old.Sandbox.ID && ready(r)
	})[0]
	if upgraded.Sandbox.Image != "valkey/valkey:8-alpine" {
		t.Fatalf("the upgraded addon's image %s", upgraded.Sandbox.Image)
	}
	eventually(t, 30*time.Second, "the volume's data version 8", func() bool {
		vol, err := h.st.GetEnvVolume(bg, env.ID, "addon/cache")
		return err == nil && vol.DataVersion == "8"
	})
	// The new web instance crashes: R fails.
	for i := 1; i <= 3; i++ {
		var id podid.ID
		eventually(t, 60*time.Second, "the new web instance running", func() bool {
			for _, r := range h.rowsOf(env.ID, spec.KindService, "web") {
				if r.Sandbox.ID == web.Sandbox.ID || !running(r) || r.Instance.Phase == model.PhaseExited {
					continue
				}
				if p := h.pod("n1", r.Sandbox.ID); p != nil && len(p.Containers) == 1 &&
					p.Containers[0].State.String() == "CONTAINER_STATE_RUNNING" {
					id = r.Sandbox.ID
					return true
				}
			}
			return false
		})
		if err := n1.Script().ExitContainer(id, "main", 2); err != nil {
			t.Fatal(err)
		}
		eventually(t, 10*time.Second, "the new instance's exit", func() bool {
			inst, err := h.st.GetEnvInstance(bg, id)
			return err == nil && len(inst.Exits) == i
		})
	}
	h.waitDeployment(env.ID, r2, model.DeploymentFailed, 30*time.Second)
	never(t, 5*time.Second, "the addon was recreated back", func() bool {
		rows := h.rowsOf(env.ID, spec.KindAddon, "cache")
		return len(rows) != 1 || rows[0].Sandbox.ID != upgraded.Sandbox.ID ||
			rows[0].Instance.TemplateHash != upgraded.Instance.TemplateHash
	})
	vol, err := h.st.GetEnvVolume(bg, env.ID, "addon/cache")
	if err != nil || vol.Holder != upgraded.Sandbox.ID || vol.DataVersion != "8" {
		t.Fatalf("volume %+v %v", vol, err)
	}
	for _, i := range h.incidents(env.ID, true) {
		if i.Condition == model.ConditionAddonUnhealthy {
			t.Errorf("the kept addon is unhealthy: %+v", i)
		}
	}
	h.waitRows(env.ID, spec.KindService, "web", 1, 30*time.Second, "the old web serving", func(r store.EnvInstanceRow) bool {
		return r.Sandbox.ID == web.Sandbox.ID && ready(r)
	})
}

// An addon whose type changes loses its volume and its credentials, as a
// removal would, and gets new ones: postgres → valkey → postgres never
// mounts one's data under the other.
func TestAddonTypeChange(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	pg := func(version string) string {
		return fmt.Sprintf(`{"addons":{"db":{"type":"postgres","version":%q,"plan":"dev","options":{"databases":["app"]}}}}`, version)
	}
	volume := func(typ, version string) store.EnvVolume {
		t.Helper()
		var vol store.EnvVolume
		eventually(t, 30*time.Second, "the volume of "+typ, func() bool {
			var err error
			vol, err = h.st.GetEnvVolume(bg, env.ID, "addon/db")
			nv := n1.Volumes()
			return err == nil && vol.AddonType == typ && vol.DataVersion == version && len(nv) == 1 &&
				nv[0].VolumeId == vol.VolumeID.String()
		})
		return vol
	}
	password := func() string {
		t.Helper()
		v, err := h.c.ResolveSecret(bg, env.ID, "addon.db.password", 1)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	r1 := h.apply(env.ID, pg("16"))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	v1, p1 := volume("postgres", "16"), password()

	r2 := h.apply(env.ID, `{"addons":{"db":{"type":"valkey","version":"8","plan":"dev"}}}`)
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 60*time.Second)
	v2, p2 := volume("valkey", "8"), password()
	if v2.VolumeID == v1.VolumeID || p2 == p1 {
		t.Fatalf("valkey has postgres's volume or password: %+v", v2)
	}
	row := h.waitRows(env.ID, spec.KindAddon, "db", 1, 10*time.Second, "ready", ready)[0]
	if row.Sandbox.PodTemplate.Volumes[0].VolumeID != v2.VolumeID {
		t.Fatalf("the valkey row's template %+v", row.Sandbox.PodTemplate)
	}

	r3 := h.apply(env.ID, pg("17"))
	h.waitDeployment(env.ID, r3, model.DeploymentDeployed, 60*time.Second)
	if v3 := volume("postgres", "17"); v3.VolumeID == v2.VolumeID || v3.VolumeID == v1.VolumeID {
		t.Fatalf("postgres 17 mounts an old volume: %+v", v3)
	}
}

// An addon's target that its volume's data refuses (a major version the
// blueprint cannot run on it) is never started: a deploying deployment fails
// with the finding and the running instance stays; outside a rollout,
// addon_unhealthy opens and no instance starts on the data.
func TestAddonRefusedOnItsData(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	pg := func(version string) string {
		return fmt.Sprintf(`{"addons":{"db":{"type":"postgres","version":%q,"plan":"dev","options":{"databases":["app"]}}}}`, version)
	}
	r1 := h.apply(env.ID, pg("16"))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	first := h.waitRows(env.ID, spec.KindAddon, "db", 1, 10*time.Second, "ready", ready)[0]

	// The service's validation refuses this; a removal and a re-add that
	// the controller never saw apart would bring the same.
	r2 := h.apply(env.ID, pg("17"))
	rel := h.waitDeployment(env.ID, r2, model.DeploymentFailed, 30*time.Second)
	if !strings.Contains(rel.Reason, "addon db: changing postgres from 16 to 17 needs a data migration") {
		t.Errorf("reason %q", rel.Reason)
	}
	inc := h.incident(env.ID, "addon/db", model.ConditionRolloutFailed, 10*time.Second)
	if inc.Deployment != r2 {
		t.Errorf("incident %+v", inc)
	}
	never(t, 3*time.Second, "the addon was recreated", func() bool {
		rows := h.rowsOf(env.ID, spec.KindAddon, "db")
		return len(rows) != 1 || rows[0].Sandbox.ID != first.Sandbox.ID
	})

	// Data written by 17 (as if it had run), the instance gone: T, the
	// deployed 16, never starts on it.
	h.exec(`UPDATE env_volumes SET data_version = '17' WHERE env_id = $1`, env.ID)
	if _, _, err := h.eng.Delete(bg, "", first.Sandbox.ID); err != nil {
		t.Fatal(err)
	}
	unhealthy := h.incident(env.ID, "addon/db", model.ConditionAddonUnhealthy, 30*time.Second)
	if !strings.Contains(unhealthy.Summary, "changing postgres from 17 to 16") {
		t.Errorf("incident %+v", unhealthy)
	}
	never(t, 3*time.Second, "an instance started on the newer data", func() bool {
		return len(h.rowsOf(env.ID, spec.KindAddon, "db")) != 0
	})
}

// failingProvisioning makes the node's provisioning steps ("sh") exit
// with code's value, echoing the step's credential (and its URL escaping)
// on stderr as a failed psql statement may; 0 hands them to the node (exit
// 0). It returns how many provisioning steps ran.
func failingProvisioning(n *testnode.Node, code *atomic.Int32) *atomic.Int32 {
	var runs atomic.Int32
	n.Script().SetExec(func(_ context.Context, _ podid.ID, req *containerv1.ExecRequest, out testnode.ExecOutput) (int32, bool, error) {
		if len(req.Command) == 0 || req.Command[0] != "sh" {
			return 0, false, nil
		}
		runs.Add(1)
		c := code.Load()
		if c == 0 {
			return 0, false, nil
		}
		pw := req.Env["PGPASSWORD"]
		_ = out.Stderr([]byte(fmt.Sprintf("psql: error: password authentication failed for %s (%s)\n", pw, escapeUserinfo(pw))))
		return c, true, nil
	})
	return &runs
}

// A provisioning step that fails keeps the addon unprovisioned and the
// deployment deploying, with the step's output as the instance's detail and
// the credentials redacted from it; the step runs again after the retry
// wait, and once it succeeds the deployment is deployed.
func TestAddonProvisioningFails(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	var code atomic.Int32
	code.Store(1)
	runs := failingProvisioning(n1, &code)
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, postgresSpec)
	row := h.waitRows(env.ID, spec.KindAddon, "postgres", 1, 60*time.Second, "failing its provisioning", func(r store.EnvInstanceRow) bool {
		return strings.Contains(r.Instance.Detail, "provisioning step provision exited 1")
	})[0]
	pw, err := h.c.ResolveSecret(bg, env.ID, "addon.postgres.password", 1)
	if err != nil {
		t.Fatal(err)
	}
	if d := row.Instance.Detail; !strings.Contains(d, redactedMark) || strings.Contains(d, pw) ||
		strings.Contains(d, escapeUserinfo(pw)) || row.Instance.ProvisionedHash != "" {
		t.Fatalf("instance %+v", row.Instance)
	}
	eventually(t, 10*time.Second, "the step run again", func() bool { return runs.Load() >= 2 })
	if rel := h.deployment(env.ID, r1); rel.State != model.DeploymentDeploying {
		t.Fatalf("deployment %s while its addon is not provisioned", rel.State)
	}
	code.Store(0)
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	row = h.waitRows(env.ID, spec.KindAddon, "postgres", 1, 10*time.Second, "ready", ready)[0]
	if row.Instance.ProvisionedHash == "" || row.Instance.Detail != "" {
		t.Fatalf("instance after the provisioning %+v", row.Instance)
	}
}
