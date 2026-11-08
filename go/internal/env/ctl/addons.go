// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	volumev1 "github.com/gardenlinux/feos/go/gen/feos/volume/v1"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/env/addons"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// stepAddons brings the addons of T up (§4.3 step 2): credentials,
// volume, instance, recreate, provisioning.
func (c *Controller) stepAddons(ctx context.Context, s *stepState) {
	s.addons = map[string]*addonPlan{}
	for _, name := range slices.Sorted(maps.Keys(s.v.target.Addons)) {
		if !c.stepAddon(ctx, s, name, s.v.target.Addons[name]) {
			s.addonsReady = false
		}
	}
}

// stepAddon brings one addon up and reports whether it is ready and
// provisioned. Before any (re)create onto an existing volume, the volume's
// data decides (§13.5): a volume of another addon type is deleted first
// (removal plus add: the data the warning said is deleted is), and a
// version the blueprint refuses to run on the volume's data is never
// started. While R failed, an addon whose row runs R's template stays as
// R has it (fallbackAddon).
func (c *Controller) stepAddon(ctx context.Context, s *stepState, name string, a spec.Addon) bool {
	v := s.v
	obj := objectName(spec.KindAddon, name)
	cur, hasCur := v.addonRow(name)
	if hasCur && !v.targetIsLatest {
		a = c.fallbackAddon(v, name, a, cur)
	}
	bp, ok := c.d.Blueprint(a.Type)
	if !ok {
		s.wait(obj, "addon %s: no addon type %q", name, a.Type)
		return false
	}
	if old, ok := v.volumes[volumeObject(name)]; ok && old.AddonType != "" && old.AddonType != a.Type {
		c.replaceVolume(ctx, s, name, a.Type, old)
		return false
	}
	addon := specAddon(bp, name, a)
	if err := c.ensureCredentials(ctx, v.env.ID, name, bp.Credentials(addon)); err != nil {
		if errors.Is(err, errNoSealer) {
			s.reason = errNoSealer.Error()
		}
		s.wait(obj, "addon %s: its credentials: %v", name, err)
		return false
	}
	inst, err := bp.Render(addon, addons.Context{Env: v.env.Name, Name: name})
	if err != nil {
		s.wait(obj, "addon %s: %v", name, err)
		return false
	}
	var vol *store.EnvVolume
	if inst.Mount != "" {
		got, err := c.addonVolume(ctx, v, bp, addon)
		if err != nil {
			s.wait(obj, "addon %s: its volume: %v", name, err)
			return false
		}
		if got.Deleting {
			c.awaitVolume(ctx, v, name, got)
			s.wait(obj, "addon %s: its old volume is being deleted", name)
			return false
		}
		vol = &got
	}
	p := c.planAddon(v, name, a, vol, cur.Sandbox.Address)
	s.addons[name] = p
	if !p.ok() {
		s.wait(obj, "addon %s: %s", name, p.why())
		return false
	}
	if !hasCur || cur.Instance.TemplateHash != p.hash {
		if vol != nil {
			if problem := c.dataRefusal(ctx, v, p, *vol, cur, hasCur); problem != "" {
				c.refuseAddon(ctx, s, p, cur, hasCur, problem)
				return false
			}
		}
		if !hasCur {
			c.newAddon(ctx, s, p, vol)
		} else {
			c.recreateAddon(ctx, s, p, cur, vol)
		}
		return false
	}
	c.keep(ctx, v, cur, 0)
	sb := cur.Sandbox
	if vol != nil && vol.Provisioned && materialised(sb) && (vol.Node != sb.Node || sb.PinNode != sb.Node) {
		// Restored elsewhere: its volume came along, whether its pod runs
		// there or stopped or failed there since.
		c.moveVolume(ctx, v, *vol, sb)
	}
	if c.heal(ctx, cur.Sandbox, false) {
		// Replaced only on its volume's node, once that node is up and
		// reports the volume free (§13.7); otherwise never: its volume is
		// on the lost node, and Ensure syncs it when the node returns.
		replaced, why := c.replaceLostAddon(ctx, v, p, cur, vol)
		if replaced {
			s.wait(obj, "addon %s was lost; it is replaced on node %s", name, lostNode(sb))
			return false
		}
		c.ensure(ctx, sb.ID)
		if !v.isOpen(obj, model.ConditionVolumeUnavailable) {
			summary := fmt.Sprintf("addon %s is lost with node %s, which has its volume", name, lostNode(sb))
			if why != "" {
				summary = fmt.Sprintf("addon %s is lost on node %s and not replaced there: %s", name, lostNode(sb), why)
			}
			c.incident(ctx, v.env.ID, obj, v.targetNum, model.ConditionVolumeUnavailable, summary, evidence{})
		}
		s.wait(obj, "addon %s is lost", name)
		return false
	}
	if !live(cur, false) {
		s.wait(obj, "addon %s is %s", name, addonState(cur))
		return false
	}
	c.closeIncidents(ctx, v, obj, model.ConditionVolumeUnavailable)
	c.closeIncidents(ctx, v, obj, model.ConditionAddonUnhealthy)
	if vol != nil && vol.DataVersion != p.addon.Version {
		c.setDataVersion(ctx, v, *vol, p.addon.Version)
	}
	if cs, ok := p.bp.(addons.CertificateServer); ok && c.d.Issuer != nil {
		// Its server certificate and the grants (ADR 0038), into the
		// steps, whose hash then moves with either.
		tlsIn, err := c.addonTLS(ctx, v, name, cs, cur)
		if err != nil {
			s.wait(obj, "addon %s: its server certificate: %v", name, err)
			return false
		}
		p.renderSteps(addons.Context{Env: v.env.Name, Name: name, Address: cur.Sandbox.Address, TLS: tlsIn})
	}
	if cur.Instance.ProvisionedHash != p.stepsHash {
		c.provision(ctx, v, p, cur)
		s.wait(obj, "addon %s is being provisioned", name)
		return false
	}
	return true
}

// fallbackAddon is the addon a step keeps while R failed (T is the
// deployed deployment, §13.5): T's when the current row runs T's template;
// R's when it runs R's (R recreated it before it failed), so that it is
// kept, healed and provisioned as R has it, never recreated back (to an
// older version than its data, or for a second downtime); else T's.
func (c *Controller) fallbackAddon(v *view, name string, a spec.Addon, cur store.EnvInstanceRow) spec.Addon {
	ra, ok := v.latestSpec.Addons[name]
	if !ok {
		return a
	}
	var vol *store.EnvVolume
	if got, ok := v.volumes[volumeObject(name)]; ok {
		vol = &got
	}
	if tp := c.planAddon(v, name, a, vol, cur.Sandbox.Address); tp.ok() && tp.hash == cur.Instance.TemplateHash {
		return a
	}
	if rp := c.planAddon(v, name, ra, vol, cur.Sandbox.Address); rp.ok() && rp.hash == cur.Instance.TemplateHash {
		return ra
	}
	return a
}

// dataRefusal is the problem of the first error finding of the
// blueprint's Validate of the addon p against the data on its volume (the
// version that wrote it, the plan and options the current row runs): ""
// when the addon may run on it.
func (c *Controller) dataRefusal(ctx context.Context, v *view, p *addonPlan, vol store.EnvVolume, cur store.EnvInstanceRow,
	hasCur bool) string {
	if vol.DataVersion == "" {
		return ""
	}
	prev := p.addon
	prev.Version = vol.DataVersion
	if vol.AddonType != "" {
		prev.Type = vol.AddonType
	}
	if hasCur {
		if sp, err := c.deploymentSpec(ctx, v.env, cur.Instance.Deployment); err == nil {
			if ra, ok := sp.Addons[p.name]; ok && ra.Type == prev.Type {
				ran := specAddon(p.bp, p.name, ra)
				prev.Plan, prev.Options = ran.Plan, ran.Options
			}
		}
	}
	for _, f := range p.bp.Validate(&prev, p.addon) {
		if f.Severity == spec.SeverityError {
			return f.Problem
		}
	}
	return ""
}

// refuseAddon leaves an addon as it runs when its target cannot run on
// its volume's data: the current row stays (healed); a deploying R fails
// (rollout_failed), else addon_unhealthy opens, the finding the reason.
func (c *Controller) refuseAddon(ctx context.Context, s *stepState, p *addonPlan, cur store.EnvInstanceRow, hasCur bool,
	problem string) {
	v := s.v
	reason := fmt.Sprintf("addon %s: %s", p.name, problem)
	if hasCur {
		c.heal(ctx, cur.Sandbox, false)
	}
	s.wait(p.object(), "%s", reason)
	if deploying(v) {
		if err := c.failDeployment(ctx, v, reason, p.object(), model.ConditionRolloutFailed,
			evidence{Diff: c.deploymentDiff(ctx, v)}); err == nil {
			s.failed = true
		}
		return
	}
	if !v.isOpen(p.object(), model.ConditionAddonUnhealthy) {
		c.incident(ctx, v.env.ID, p.object(), v.targetNum, model.ConditionAddonUnhealthy, reason, evidence{})
	}
}

// replaceVolume deletes the volume of an addon whose type changed, as a
// removal would (§13.5): the volume is marked deleting and the old
// credentials go in one transaction, the old instance goes, and the
// volume once no pod holds it; the addon of the new type gets a new one.
func (c *Controller) replaceVolume(ctx context.Context, s *stepState, name, typ string, vol store.EnvVolume) {
	v := s.v
	if !vol.Deleting {
		err := c.fenced(ctx, func(tx *store.Store) error {
			cur, err := tx.LockEnvVolume(ctx, v.env.ID, vol.Object, store.ForUpdate)
			if err != nil {
				return err
			}
			if cur.VolumeID != vol.VolumeID || cur.Deleting {
				return nil
			}
			after := cur
			after.Deleting = true
			if _, err := tx.UpdateEnvVolume(ctx, cur, after); err != nil {
				return err
			}
			_, err = tx.PurgeSecrets(ctx, v.env.ID, "addon."+name+".")
			return err
		})
		if err != nil {
			s.wait(objectName(spec.KindAddon, name), "addon %s: %v", name, err)
			return
		}
		c.log.Info("an addon's type changed: its volume and credentials are deleted", "environment", v.env.ID, "addon", name,
			"from", vol.AddonType, "to", typ, "volume", vol.VolumeID)
	}
	for _, r := range v.rowsOf(spec.KindAddon, name) {
		if err := c.deleteInstance(ctx, r.Sandbox); err != nil && ctx.Err() == nil {
			c.rl.warn("remove", "cannot delete an addon's old instance", "sandbox", r.Sandbox.ID, "err", err)
		}
	}
	c.awaitVolume(ctx, v, name, vol)
	s.wait(objectName(spec.KindAddon, name), "addon %s: its %s volume is being deleted", name, vol.AddonType)
}

// awaitVolume deletes an addon's volume marked deleting once its addon
// has no row left (env-volumes would within its interval).
func (c *Controller) awaitVolume(ctx context.Context, v *view, name string, vol store.EnvVolume) {
	if len(v.rowsOf(spec.KindAddon, name)) > 0 {
		return
	}
	if _, err := c.deleteVolume(ctx, vol); err != nil && ctx.Err() == nil {
		c.rl.warn("volume", "cannot delete an addon's old volume", "environment", v.env.ID, "addon", name, "err", err)
	}
}

// setDataVersion records the version an addon instance ran ready with on
// its volume: the data's version from now on.
func (c *Controller) setDataVersion(ctx context.Context, v *view, vol store.EnvVolume, version string) {
	err := c.fenced(ctx, func(tx *store.Store) error {
		cur, err := tx.LockEnvVolume(ctx, v.env.ID, vol.Object, store.ForUpdate)
		if err != nil || cur.VolumeID != vol.VolumeID {
			return err
		}
		after := cur
		after.DataVersion = version
		_, err = tx.UpdateEnvVolume(ctx, cur, after)
		return err
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
		c.rl.warn("volume", "cannot record a volume's data version", "environment", v.env.ID, "object", vol.Object, "err", err)
		return
	}
	if err == nil {
		c.log.Info("addon volume's data version", "environment", v.env.ID, "object", vol.Object, "version", version)
	}
}

// addonVolume reads the addon's volume row, recording it with the plan's
// size at its first creation (never resized), the addon's type (fixed)
// and its version as the data's.
func (c *Controller) addonVolume(ctx context.Context, v *view, bp addons.Blueprint, a addons.Addon) (store.EnvVolume, error) {
	object := volumeObject(a.Name)
	if vol, ok := v.volumes[object]; ok {
		return vol, nil
	}
	plan, ok := bp.Plans()[a.Plan]
	if !ok {
		return store.EnvVolume{}, fmt.Errorf("no plan %q", a.Plan)
	}
	var out store.EnvVolume
	err := c.fenced(ctx, func(tx *store.Store) error {
		var err error
		out, err = tx.InsertEnvVolume(ctx, store.EnvVolume{EnvID: v.env.ID, Object: object, VolumeID: uuid.New(),
			SizeMiB: plan.VolumeMib, AddonType: a.Type, DataVersion: a.Version})
		return err
	})
	if errors.Is(err, store.ErrExists) {
		// Recorded meanwhile (the failed insert ended its transaction).
		out, err = c.st.GetEnvVolume(ctx, v.env.ID, object)
	}
	if err != nil {
		return store.EnvVolume{}, err
	}
	v.volumes[object] = out
	return out, nil
}

// newAddon inserts an addon's instance: pinned to its volume's node once
// the volume was created there, and only once the pod that held the
// volume last is gone from its node (no row, no tombstone): a volume is
// attached to one pod at a time.
func (c *Controller) newAddon(ctx context.Context, s *stepState, p *addonPlan, vol *store.EnvVolume) {
	v := s.v
	for _, r := range v.rowsOf(spec.KindAddon, p.name) {
		if r.Instance.Phase == model.PhaseStopping {
			s.wait(p.object(), "addon %s: its old instance is being deleted", p.name)
			return
		}
	}
	pin := ""
	if vol != nil {
		if !vol.Holder.IsZero() {
			if held, err := c.stillHolds(ctx, vol.Holder); err != nil || held {
				s.wait(p.object(), "addon %s: its old pod still holds its volume", p.name)
				return
			}
		}
		if vol.Provisioned {
			// Its data is on its node: a node that was removed never has it
			// again, and a row pinned there would never place.
			if gone, err := c.nodeGone(ctx, vol.Node); err != nil || gone {
				if gone && !v.isOpen(p.object(), model.ConditionVolumeUnavailable) {
					c.incident(ctx, v.env.ID, p.object(), v.targetNum, model.ConditionVolumeUnavailable,
						fmt.Sprintf("addon %s's volume is on node %s, which was removed", p.name, vol.Node), evidence{})
				}
				s.wait(p.object(), "addon %s: its volume's node %s was removed", p.name, vol.Node)
				return
			}
			pin = vol.Node
		}
	}
	if _, err := c.insertInstance(ctx, v, insert{plan: &p.podPlan, deployment: v.targetNum, pin: pin}); err != nil {
		s.wait(p.object(), "addon %s: %v", p.name, err)
		return
	}
	s.wait(p.object(), "addon %s is starting", p.name)
}

// stillHolds reports whether a row, or a tombstone of its pod, is left:
// its pod may still hold a volume.
func (c *Controller) stillHolds(ctx context.Context, id podid.ID) (bool, error) {
	_, err := c.st.GetSandbox(ctx, id)
	switch {
	case err == nil:
		return true, nil
	case !errors.Is(err, model.ErrNotFound):
		return false, err
	}
	ts, err := c.st.TombstonesOfPod(ctx, id)
	return len(ts) > 0, err
}

// recreateAddon replaces an addon whose template changed (a version, a
// plan, a rendered option): only while its instance runs on a ready node
// (else that instance is brought up first, a restore), or when it is lost
// on a node that is up and has its volume free (replaceLostAddon); the
// volume records the node and the old row as its holder, the old row
// goes, and newAddon inserts the new one on that node once the old pod
// is gone.
func (c *Controller) recreateAddon(ctx context.Context, s *stepState, p *addonPlan, cur store.EnvInstanceRow, vol *store.EnvVolume) {
	v := s.v
	sb := cur.Sandbox
	if c.heal(ctx, cur.Sandbox, false) {
		if replaced, _ := c.replaceLostAddon(ctx, v, p, cur, vol); replaced {
			s.wait(p.object(), "addon %s was lost; it is recreated on node %s", p.name, lostNode(sb))
			return
		}
		c.ensure(ctx, sb.ID)
		s.wait(p.object(), "addon %s is lost; it is recreated once it runs again", p.name)
		return
	}
	if !steady(sb) || !c.nodeReady(ctx, sb.Node) {
		if sb.Op == "" && sb.ClientState != model.StateRunning {
			c.ensure(ctx, sb.ID)
		}
		s.wait(p.object(), "addon %s: waits for its instance to run before it is recreated", p.name)
		return
	}
	err := c.fenced(ctx, func(tx *store.Store) error {
		inst, err := tx.LockEnvInstance(ctx, sb.ID, store.ForUpdate)
		if err != nil {
			return err
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		after := inst
		after.Phase, after.RetireAt = model.PhaseStopping, now
		if _, err := tx.UpdateEnvInstance(ctx, inst, after); err != nil {
			return err
		}
		if vol == nil {
			return nil
		}
		vv, err := tx.LockEnvVolume(ctx, v.env.ID, vol.Object, store.ForUpdate)
		if err != nil {
			return err
		}
		va := vv
		va.Holder = sb.ID
		if vv.Provisioned {
			va.Node = sb.Node
		}
		_, err = tx.UpdateEnvVolume(ctx, vv, va)
		return err
	})
	if err != nil {
		s.wait(p.object(), "addon %s: %v", p.name, err)
		return
	}
	c.log.Info("addon recreated", "environment", v.env.ID, "addon", p.name, "old", sb.ID, "node", sb.Node)
	if err := c.deleteInstance(ctx, sb); err != nil && ctx.Err() == nil {
		c.rl.warn("recreate", "cannot delete an addon's old instance", "sandbox", sb.ID, "err", err)
	}
	s.wait(p.object(), "addon %s is being recreated", p.name)
}

// addonState says why an addon instance is not ready: its phase, or,
// with a ready phase, its row's state (and op, and description).
func addonState(r store.EnvInstanceRow) string {
	sb := r.Sandbox
	switch {
	case r.Instance.Phase != model.PhaseReady:
		return r.Instance.Phase
	case sb.Op != "":
		return fmt.Sprintf("%s, %s in flight", sb.ClientState, sb.Op)
	case sb.StatusDescription != "":
		return fmt.Sprintf("%s (%s)", sb.ClientState, sb.StatusDescription)
	}
	return sb.ClientState
}

// replaceLostAddon replaces an addon lost on a node that is up (§13.7):
// the node lost the pod, and nothing would ever bring the row back.
// Only when that node (the volume's, since the volume is attached where
// the pod was) is ready and connected, its pod is not found there, and
// GetVolume there reports the volume free (no pod, or this pod;
// AVAILABLE or CHECKPOINTED), all read outside a transaction: then, as
// recreateAddon does, the volume records the node and the lost row as its
// holder, the row stops and goes, and newAddon inserts the new one on
// that node once the old pod is gone. It reports whether it replaced the
// row, and else what the node said when that kept it ("" when it never
// asked: no volume, the node down, removed or draining).
func (c *Controller) replaceLostAddon(ctx context.Context, v *view, p *addonPlan, cur store.EnvInstanceRow,
	vol *store.EnvVolume) (bool, string) {
	sb := cur.Sandbox
	node := lostNode(sb)
	if vol == nil || node == "" || !c.connected(node) || !c.nodeReady(ctx, node) {
		return false, ""
	}
	free, why, err := c.lostPodFree(ctx, node, sb.ID, *vol)
	if err != nil {
		if ctx.Err() == nil {
			c.rl.warn("lost", "cannot read a lost addon's pod or volume", "sandbox", sb.ID, "node", node, "err", err)
		}
		return false, ""
	}
	if !free {
		return false, why
	}
	err = c.fenced(ctx, func(tx *store.Store) error {
		row, err := tx.GetSandbox(ctx, sb.ID)
		if err != nil {
			return err
		}
		if row.ClientState != model.StateLost || row.Op != "" || lostNode(row) != node {
			return errNotLost
		}
		inst, err := tx.LockEnvInstance(ctx, sb.ID, store.ForUpdate)
		if err != nil {
			return err
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		after := inst
		after.Phase, after.RetireAt = model.PhaseStopping, now
		if _, err := tx.UpdateEnvInstance(ctx, inst, after); err != nil {
			return err
		}
		vv, err := tx.LockEnvVolume(ctx, v.env.ID, vol.Object, store.ForUpdate)
		if err != nil {
			return err
		}
		if vv.VolumeID != vol.VolumeID || vv.Deleting {
			return errNotLost
		}
		va := vv
		va.Holder = sb.ID
		if vv.Provisioned {
			va.Node = node
		}
		_, err = tx.UpdateEnvVolume(ctx, vv, va)
		return err
	})
	if err != nil {
		if !errors.Is(err, errNotLost) && !errors.Is(err, model.ErrNotFound) && ctx.Err() == nil {
			c.rl.warn("lost", "cannot replace a lost addon", "sandbox", sb.ID, "err", err)
		}
		return false, ""
	}
	c.log.Info("a lost addon is replaced on its volume's node", "environment", v.env.ID, "addon", p.name, "old", sb.ID,
		"node", node)
	if err := c.deleteInstance(ctx, sb); err != nil && ctx.Err() == nil {
		c.rl.warn("lost", "cannot delete a lost addon's instance", "sandbox", sb.ID, "err", err)
	}
	return true, ""
}

// errNotLost: the row or its volume changed since the step read them.
var errNotLost = errors.New("no longer lost as read")

// lostPodFree asks a node that is up whether a lost addon's volume is
// free there for a new pod: true when the pod is not found there and the
// volume is (not provisioned anywhere yet, or on the node with no pod or
// this pod, AVAILABLE or CHECKPOINTED); else why not ("" when the node
// still has the pod: Ensure syncs it back, it is not replaced).
func (c *Controller) lostPodFree(ctx context.Context, node string, pod podid.ID, vol store.EnvVolume) (bool, string, error) {
	_, err := c.getPod(ctx, node, pod)
	switch {
	case err == nil:
		return false, "", nil
	case status.Code(err) != codes.NotFound:
		return false, "", err
	}
	if !vol.Provisioned {
		return true, "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	defer cancel()
	cc, err := c.conn(ctx, node)
	if err != nil {
		return false, "", err
	}
	volumeID := vol.VolumeID.String()
	got, err := volumev1.NewVolumeServiceClient(cc).GetVolume(ctx, &volumev1.GetVolumeRequest{VolumeId: volumeID})
	switch {
	case status.Code(err) == codes.NotFound:
		return false, fmt.Sprintf("the node does not have volume %s", volumeID), nil
	case err != nil:
		return false, "", err
	case got.GetPodId() != "" && got.GetPodId() != pod.String():
		return false, fmt.Sprintf("volume %s is attached to pod %s there", volumeID, got.GetPodId()), nil
	case got.GetState() == volumev1.VolumeState_VOLUME_STATE_AVAILABLE,
		got.GetState() == volumev1.VolumeState_VOLUME_STATE_CHECKPOINTED:
		return true, "", nil
	}
	return false, fmt.Sprintf("volume %s is %s there", volumeID, got.GetState()), nil
}

// moveVolume records that an addon's volume is on the node its instance
// is materialised on now (a restore elsewhere took it along), and pins
// the row there.
func (c *Controller) moveVolume(ctx context.Context, v *view, vol store.EnvVolume, sb store.Sandbox) {
	err := c.fenced(ctx, func(tx *store.Store) error {
		cur, err := tx.LockEnvVolume(ctx, v.env.ID, vol.Object, store.ForUpdate)
		if err != nil {
			return err
		}
		after := cur
		after.Node = sb.Node
		if _, err := tx.UpdateEnvVolume(ctx, cur, after); err != nil {
			return err
		}
		_, err = tx.EnvCtlSetPin(ctx, sb.ID, sb.Node)
		return err
	})
	if err != nil && ctx.Err() == nil {
		c.rl.warn("volume", "cannot record a volume's node", "environment", v.env.ID, "object", vol.Object, "err", err)
		return
	}
	c.log.Info("addon volume moved", "environment", v.env.ID, "object", vol.Object, "node", sb.Node)
}

// provisionKey is the pool's key of an instance's provisioning.
func provisionKey(id podid.ID) string { return "provision/" + id.String() }

// provision runs an addon's provisioning steps in its container, off the
// loop: hold-free, the env carrying its credentials; on success the steps'
// hash is stored, on a failure the step's output (credentials redacted)
// is the instance's detail and the steps run again after provisionRetry.
func (c *Controller) provision(ctx context.Context, v *view, p *addonPlan, r store.EnvInstanceRow) {
	if c.d.Runner == nil {
		c.setDetail(ctx, r.Sandbox.ID, "provisioning needs the tools' runner, which this instance lacks")
		return
	}
	c.mu.Lock()
	failed, wait := c.provisionFailed[r.Sandbox.ID]
	c.mu.Unlock()
	if wait && v.now.Sub(failed) < c.t.provisionRetry && v.now.After(failed) {
		return
	}
	envID, l := v.env.ID, lease(r.Sandbox)
	steps, hash, name, keys := p.steps, p.stepsHash, p.name, p.credentials
	c.pool.run(ctx, provisionKey(r.Sandbox.ID), func(ctx context.Context) {
		secrets := c.credentialValues(ctx, envID, name, keys)
		for _, st := range steps {
			env, err := c.resolveEnv(ctx, envID, st.Env)
			if err != nil {
				c.provisionDone(ctx, r.Sandbox.ID, "", fmt.Sprintf("provisioning step %s: %v", st.Name, err))
				return
			}
			code, stdout, stderr, err := c.d.Runner.ExecDetached(ctx, l, st.Argv, env.vars, "/", c.t.stepTimeout)
			out := redact(strings.TrimSpace(stderr+"\n"+stdout), append(secrets, env.secrets...))
			switch {
			case err != nil:
				c.provisionDone(ctx, r.Sandbox.ID, "", fmt.Sprintf("provisioning step %s: %s", st.Name,
					redact(err.Error(), append(secrets, env.secrets...))))
				return
			case code != 0:
				c.provisionDone(ctx, r.Sandbox.ID, "", fmt.Sprintf("provisioning step %s exited %d: %s", st.Name, code,
					lastLines(out, 5)))
				return
			}
		}
		c.provisionDone(ctx, r.Sandbox.ID, hash, "")
		c.log.Info("addon provisioned", "environment", envID, "addon", name, "sandbox", r.Sandbox.ID)
	})
}

// provisionDone stores a provisioning's outcome: the steps' hash, or why
// it failed.
func (c *Controller) provisionDone(ctx context.Context, id podid.ID, hash, detail string) {
	c.mu.Lock()
	if hash == "" {
		c.provisionFailed[id] = c.dbNow(ctx)
	} else {
		delete(c.provisionFailed, id)
	}
	c.mu.Unlock()
	err := c.fenced(ctx, func(tx *store.Store) error {
		inst, err := tx.LockEnvInstance(ctx, id, store.ForUpdate)
		if err != nil {
			return err
		}
		after := inst
		after.Detail = detail
		if hash != "" {
			after.ProvisionedHash = hash
		}
		if _, err := tx.UpdateEnvInstance(ctx, inst, after); err != nil {
			return err
		}
		return tx.Notify(ctx, db.ChanLeader, HintReconcile)
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
		c.rl.warn("provision", "cannot store a provisioning's outcome", "sandbox", id, "err", err)
	}
}

// setDetail writes an instance's detail.
func (c *Controller) setDetail(ctx context.Context, id podid.ID, detail string) {
	err := c.fenced(ctx, func(tx *store.Store) error {
		inst, err := tx.LockEnvInstance(ctx, id, store.ForUpdate)
		if err != nil {
			return err
		}
		after := inst
		after.Detail = detail
		_, err = tx.UpdateEnvInstance(ctx, inst, after)
		return err
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
		c.rl.warn("detail", "cannot write an instance's detail", "sandbox", id, "err", err)
	}
}

// dbNow is the database's time, or the instance's when it cannot be read.
func (c *Controller) dbNow(ctx context.Context) time.Time {
	if t, err := c.st.Now(ctx); err == nil {
		return t
	}
	return time.Now()
}

// lastLines is the last n lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
