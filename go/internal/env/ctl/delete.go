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
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// volumesBatch is how many deleting volumes one env-volumes pass takes.
const volumesBatch = 64

// volumesLoop is env-volumes: the volumes marked deleting go from their
// nodes once no pod holds them (FAILED_PRECONDITION: later; NOT_FOUND:
// done), then their rows. A volume never created on a node, or of a node
// that is gone or removed, only loses its row. A volume that has to wait
// goes to the end of the queue, so that those that wait long never hold
// the batch. Then the copies left on nodes go (sweepVolumes).
func (c *Controller) volumesLoop(ctx context.Context) error {
	vols, err := c.st.DeletingEnvVolumes(ctx, volumesBatch)
	if err != nil {
		return err
	}
	errs := []error{c.sweepVolumes(ctx)}
	for _, vol := range vols {
		gone, err := c.deleteVolume(ctx, vol)
		if err != nil && ctx.Err() == nil {
			errs = append(errs, fmt.Errorf("volume %s of %s: %w", vol.VolumeID, vol.Object, err))
		}
		if !gone && ctx.Err() == nil {
			if err := c.fenced(ctx, func(tx *store.Store) error {
				return tx.EnvCtlTouchVolume(ctx, vol.EnvID, vol.Object)
			}); err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
				errs = append(errs, fmt.Errorf("volume %s of %s: %w", vol.VolumeID, vol.Object, err))
			}
		}
	}
	return errors.Join(errs...)
}

// sweepVolumes deletes, on each connected node, the records of
// environments' volumes that no pod holds and that are not the volume as
// its row has it: a volume whose row is gone (its addon or environment
// was deleted while the record stayed elsewhere), or a copy on another
// node than the row's (a restore elsewhere took the volume over, and the
// node it left keeps a checkpointed record of it). DeleteVolume refuses a
// volume a pod holds by then (FAILED_PRECONDITION: a later pass), so that
// a restore or create that takes it meanwhile keeps it. A row not yet
// provisioned keeps every record of its volume: its create may be in
// flight.
func (c *Controller) sweepVolumes(ctx context.Context) error {
	if c.d.Conns == nil {
		return nil
	}
	var errs []error
	for _, node := range c.d.Conns.Nodes() {
		if err := c.sweepNodeVolumes(ctx, node); err != nil && ctx.Err() == nil {
			errs = append(errs, fmt.Errorf("node %s: %w", node, err))
		}
	}
	return errors.Join(errs...)
}

// sweepNodeVolumes is sweepVolumes for one node.
func (c *Controller) sweepNodeVolumes(ctx context.Context, node string) error {
	lctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	defer cancel()
	cc, err := c.conn(lctx, node)
	if err != nil {
		return err
	}
	vc := volumev1.NewVolumeServiceClient(cc)
	list, err := vc.ListVolumes(lctx, &volumev1.ListVolumesRequest{})
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for _, v := range list.GetVolumes() {
		if v.GetLabels()[model.LabelEnvironment] == "" || v.GetPodId() != "" {
			continue
		}
		if id, err := uuid.Parse(v.GetVolumeId()); err == nil {
			ids = append(ids, id)
		}
	}
	rows, err := c.st.EnvCtlVolumesByID(ctx, ids)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		vol, ok := rows[id]
		why := "its row is gone"
		switch {
		case !ok:
		case vol.Provisioned && vol.Node != "" && vol.Node != node:
			why = "the volume is on node " + vol.Node
		default:
			continue
		}
		dctx, dcancel := context.WithTimeout(ctx, c.t.nodeCall)
		_, err := vc.DeleteVolume(dctx, &volumev1.DeleteVolumeRequest{VolumeId: id.String()})
		dcancel()
		switch status.Code(err) {
		case codes.OK:
			c.log.Info("a volume's record left on a node deleted", "node", node, "volume", id, "why", why)
		case codes.NotFound, codes.FailedPrecondition:
		default:
			errs = append(errs, fmt.Errorf("delete volume %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// nodeGone reports whether a node will never serve its disks again: its
// row is gone, or it was removed (a removed node is never deleted and
// never connects again, and its disks are gone with it, as the engine's
// DropRemovedNodeTombstones assumes).
func (c *Controller) nodeGone(ctx context.Context, node string) (bool, error) {
	n, err := c.st.GetNode(ctx, node)
	switch {
	case errors.Is(err, store.ErrNoRow):
		return true, nil
	case err != nil:
		return false, err
	}
	return n.Status == model.NodeRemoved, nil
}

// deleteVolume deletes one volume marked deleting, and reports whether its
// row went.
func (c *Controller) deleteVolume(ctx context.Context, vol store.EnvVolume) (bool, error) {
	if vol.Provisioned && vol.Node != "" {
		gone, err := c.nodeGone(ctx, vol.Node)
		switch {
		case err != nil:
			return false, err
		case gone:
			c.log.Info("a volume's node is gone: its row goes without a node call", "environment", vol.EnvID,
				"object", vol.Object, "volume", vol.VolumeID, "node", vol.Node)
		case !c.connected(vol.Node):
			return false, nil
		default:
			cctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
			cc, err := c.conn(cctx, vol.Node)
			if err == nil {
				_, err = volumev1.NewVolumeServiceClient(cc).DeleteVolume(cctx, &volumev1.DeleteVolumeRequest{VolumeId: vol.VolumeID.String()})
			}
			cancel()
			switch status.Code(err) {
			case codes.OK, codes.NotFound:
			case codes.FailedPrecondition:
				return false, nil // a pod still holds it
			default:
				return false, err
			}
		}
	}
	err := c.fenced(ctx, func(tx *store.Store) error {
		_, err := tx.DeleteEnvVolume(ctx, vol.EnvID, vol.Object)
		return err
	})
	if err != nil {
		return false, err
	}
	c.log.Info("volume deleted", "environment", vol.EnvID, "object", vol.Object, "volume", vol.VolumeID, "node", vol.Node)
	return true, nil
}

// deleteBatch is how many deleting environments one env-delete pass
// takes.
const deleteBatch = 16

// deleteLoop is env-delete: the environments of deleting tenants are
// marked deleting; each deleting environment loses its instances
// (Engine.Delete), its attached services (their processes stop, a
// sleeping sandbox woken for it, before their rows and names go), its
// volumes (marked deleting) and its group and name table on every
// connected node; its row goes once no instance row, volume, group, name
// table or attached service still to clean is left (its deployments, secrets, runs, incidents
// and route tokens cascade). An environment whose pass could not finish
// (a volume on a node that is down, a sandbox that cannot be woken yet)
// is touched, so that it goes to the end of the queue: the oldest-touched
// come first, and those that wait long never hold the others back.
func (c *Controller) deleteLoop(ctx context.Context) error {
	if err := c.sweepTenantJobs(ctx); err != nil {
		return fmt.Errorf("sweep the tenant jobs: %w", err)
	}
	tenants, err := c.st.DeletingTenants(ctx)
	if err != nil {
		return err
	}
	for _, t := range tenants {
		var marked []string
		err := c.fenced(ctx, func(tx *store.Store) error {
			ids, err := tx.MarkTenantEnvironmentsDeleting(ctx, t)
			for _, id := range ids {
				marked = append(marked, id.String())
			}
			return err
		})
		if err != nil {
			return err
		}
		if len(marked) > 0 {
			c.log.Info("a deleting tenant's environments are deleted", "tenant", t, "environments", marked)
		}
	}
	envs, err := c.st.DeletingEnvironments(ctx, deleteBatch)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range envs {
		deleted, err := c.deleteEnvironment(ctx, e)
		if err != nil && ctx.Err() == nil {
			errs = append(errs, fmt.Errorf("environment %s: %w", e.ID, err))
		}
		if !deleted && ctx.Err() == nil {
			if err := c.fenced(ctx, func(tx *store.Store) error {
				return tx.EnvCtlTouchEnvironment(ctx, e.ID)
			}); err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
				errs = append(errs, fmt.Errorf("environment %s: %w", e.ID, err))
			}
		}
	}
	return errors.Join(errs...)
}

// deleteEnvironment is one pass of env-delete over an environment, and
// reports whether its row went.
func (c *Controller) deleteEnvironment(ctx context.Context, e store.Environment) (bool, error) {
	rows, err := c.st.EnvCtlSandboxes(ctx, e.ID)
	if err != nil {
		return false, err
	}
	for _, sb := range rows {
		if err := c.deleteInstance(ctx, sb); err != nil {
			return false, err
		}
	}
	// The attached services' processes stop (a sleeping sandbox is woken
	// for it) before their rows, and with them their names, go: cleanOld.
	attached, err := c.st.ListAttached(ctx, e.ID)
	if err != nil {
		return false, err
	}
	v := &view{env: e, target: emptySpec(), latestSpec: emptySpec()}
	cleaning := false
	for _, a := range attached {
		if !c.cleanOld(ctx, v, a, c.dropCleaned) {
			cleaning = true
			continue
		}
		gone, err := c.dropAttached(ctx, a)
		if err != nil {
			return false, err
		}
		cleaning = cleaning || !gone
	}
	if err := c.fenced(ctx, func(tx *store.Store) error {
		_, err := tx.EnvCtlMarkVolumesDeleting(ctx, e.ID)
		return err
	}); err != nil {
		return false, err
	}
	// The env group and the env table go from every node this instance
	// reaches, once no pod there names them any more (FAILED_PRECONDITION:
	// a later pass); a node out of reach keeps them until env-groups and
	// env-names find them of an environment that is gone.
	groupsGone := true
	if c.d.Conns != nil {
		for _, node := range c.d.Conns.Nodes() {
			gone, err := c.deleteGroup(ctx, node, model.EnvGroupID(e.ID))
			if err != nil {
				return false, err
			}
			tableGone, err := c.deleteTable(ctx, node, model.EnvTableID(e.ID))
			if err != nil {
				return false, err
			}
			groupsGone = groupsGone && gone && tableGone
		}
	}
	left, err := c.st.EnvCtlSandboxes(ctx, e.ID)
	if err != nil {
		return false, err
	}
	vols, err := c.st.ListEnvVolumes(ctx, e.ID)
	if err != nil {
		return false, err
	}
	if len(left) == 0 {
		// The pods are going: their volumes may go too (env-volumes
		// would, within its interval).
		for _, vol := range vols {
			if _, err := c.deleteVolume(ctx, vol); err != nil {
				return false, err
			}
		}
		if vols, err = c.st.ListEnvVolumes(ctx, e.ID); err != nil {
			return false, err
		}
	}
	if len(left) > 0 || len(vols) > 0 || !groupsGone || cleaning {
		return false, nil
	}
	deleted := false
	err = c.fenced(ctx, func(tx *store.Store) error {
		cur, err := tx.LockEnvironment(ctx, e.ID, store.ForUpdate)
		if errors.Is(err, store.ErrNoRow) {
			deleted = true
			return nil
		}
		if err != nil || !cur.Deleting {
			return err
		}
		if _, err := tx.DeleteEnvironment(ctx, e.ID); err != nil {
			return err
		}
		deleted = true
		c.log.Info("environment deleted", "environment", e.ID, "name", e.Name, "owner", e.Owner)
		return nil
	})
	return deleted && err == nil, err
}

// emptySpec is the spec of nothing.
func emptySpec() *spec.Spec { return &spec.Spec{} }
