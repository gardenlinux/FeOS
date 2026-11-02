// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	volumev1 "github.com/gardenlinux/feos/go/gen/feos/volume/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/sandbox"
	"github.com/gardenlinux/feos/go/internal/store"
)

// PrepareHook is the engine's prepare hook (sandbox.PrepareHook) for a
// row with a pod template, an environment's instance or an attachable
// sandbox: it puts the row's security groups, and those their rules
// reference, on node (whole groups; what the node then holds of the
// ingress's groups is recorded, putGroups), and its name tables, and
// those they include (putNames); before a create it also makes sure
// the row's volumes are there: created on node when they never were
// (then provisioned, their node and the row's pin_node set in a
// transaction of its own), else present there and free (AVAILABLE,
// CHECKPOINTED without a pod, or held by this pod); otherwise it fails and
// opens volume_unavailable. A restore takes its volumes from the
// checkpoint: nothing is created before one.
func (c *Controller) PrepareHook(ctx context.Context, node string, sb store.Sandbox, kind sandbox.OpKind) error {
	if sb.PodTemplate == nil {
		return nil
	}
	if err := c.putGroups(ctx, node, sb); err != nil {
		return err
	}
	if err := c.putNames(ctx, node, sb); err != nil {
		return err
	}
	if kind != sandbox.OpCreate {
		return nil
	}
	for _, tv := range sb.PodTemplate.Volumes {
		if err := c.prepareVolume(ctx, node, sb, tv.VolumeID); err != nil {
			return err
		}
	}
	return nil
}

// volumeLabels are the labels a volume is created with: the same on every
// create, so that a repeated create finds it (CreateVolume compares them).
func volumeLabels(v store.EnvVolume) map[string]string {
	return map[string]string{model.LabelEnvironment: v.EnvID.String(), model.LabelObject: v.Object}
}

// prepareVolume is the volume half of the prepare hook for one volume.
func (c *Controller) prepareVolume(ctx context.Context, node string, sb store.Sandbox, id uuid.UUID) error {
	volumeID := id.String()
	vol, err := c.volumeByID(ctx, id)
	if err != nil {
		return err
	}
	cc, err := c.conn(ctx, node)
	if err != nil {
		return err
	}
	vc := volumev1.NewVolumeServiceClient(cc)
	if !vol.Provisioned {
		if _, err := vc.CreateVolume(ctx, &volumev1.CreateVolumeRequest{VolumeId: volumeID, SizeBytes: uint64(vol.SizeMiB) << 20,
			Labels: volumeLabels(vol)}); err != nil {
			err = fmt.Errorf("create volume %s on node %s: %w", volumeID, node, err)
			switch status.Code(err) {
			case codes.AlreadyExists, codes.FailedPrecondition, codes.ResourceExhausted, codes.InvalidArgument:
				// The node refuses it (one of the ID with another size,
				// no room): a fresh create goes elsewhere.
				return unfit{err}
			}
			return err
		}
		err := c.st.InTx(ctx, func(tx *store.Store) error {
			cur, err := tx.LockEnvVolume(ctx, vol.EnvID, vol.Object, store.ForUpdate)
			if err != nil {
				return err
			}
			if cur.Provisioned && cur.Node != node {
				return fmt.Errorf("volume %s was created on node %s meanwhile", volumeID, cur.Node)
			}
			after := cur
			after.Provisioned, after.Node, after.Holder = true, node, sb.ID
			if _, err := tx.UpdateEnvVolume(ctx, cur, after); err != nil {
				return err
			}
			_, err = tx.EnvCtlSetPin(ctx, sb.ID, node)
			return err
		})
		if err != nil {
			return fmt.Errorf("record volume %s on node %s: %w", volumeID, node, err)
		}
		c.log.Info("volume created", "volume", volumeID, "object", vol.Object, "node", node, "sandbox", sb.ID)
		return nil
	}
	why := ""
	got, err := vc.GetVolume(ctx, &volumev1.GetVolumeRequest{VolumeId: volumeID})
	switch {
	case status.Code(err) == codes.NotFound:
		why = fmt.Sprintf("node %s does not have volume %s", node, volumeID)
	case err != nil:
		return fmt.Errorf("read volume %s on node %s: %w", volumeID, node, err)
	case got.GetPodId() == sb.ID.String():
	case got.GetPodId() != "":
		why = fmt.Sprintf("volume %s is attached to pod %s on node %s", volumeID, got.GetPodId(), node)
	case got.GetState() == volumev1.VolumeState_VOLUME_STATE_AVAILABLE,
		got.GetState() == volumev1.VolumeState_VOLUME_STATE_CHECKPOINTED:
	default:
		why = fmt.Sprintf("volume %s is %s on node %s", volumeID, got.GetState(), node)
	}
	if why == "" {
		if vol.Holder != sb.ID {
			err := c.st.InTx(ctx, func(tx *store.Store) error {
				cur, err := tx.LockEnvVolume(ctx, vol.EnvID, vol.Object, store.ForUpdate)
				if err != nil {
					return err
				}
				after := cur
				after.Holder = sb.ID
				_, err = tx.UpdateEnvVolume(ctx, cur, after)
				return err
			})
			if err != nil {
				return fmt.Errorf("record volume %s's holder: %w", volumeID, err)
			}
		}
		return nil
	}
	if r, err := c.st.GetEnvInstance(ctx, sb.ID); err == nil {
		c.volumeIncident(ctx, vol, r.Deployment, why)
	}
	return unfit{errors.New(why)}
}

// unfit is a prepare hook's error for a node that cannot take the pod (a
// volume it refuses, or has not, or holds for another pod): it wraps
// sandbox.ErrNodeUnfit, so that an unpinned create avoids the node. Any
// other error of the hook is a plain rollback.
type unfit struct{ err error }

func (e unfit) Error() string   { return e.err.Error() }
func (e unfit) Unwrap() []error { return []error{e.err, sandbox.ErrNodeUnfit} }

// volumeByID reads an env volume by its volume ID.
func (c *Controller) volumeByID(ctx context.Context, id uuid.UUID) (store.EnvVolume, error) {
	out, err := c.st.LockEnvVolumeByID(ctx, id, store.NoLock)
	if err != nil {
		return store.EnvVolume{}, fmt.Errorf("volume %s: %w", id, err)
	}
	return out, nil
}

// volumeIncident opens volume_unavailable for a volume's addon, outside
// the leader's fence: the hook runs wherever the op runs.
func (c *Controller) volumeIncident(ctx context.Context, vol store.EnvVolume, deployment int, why string) {
	err := c.st.InTx(ctx, func(tx *store.Store) error {
		_, err := openIncident(ctx, tx, vol.EnvID, vol.Object, deployment, model.ConditionVolumeUnavailable, why, evidence{})
		return err
	})
	if err != nil && ctx.Err() == nil {
		c.rl.warn("incident", "cannot open volume_unavailable", "object", vol.Object, "err", err)
	}
}
