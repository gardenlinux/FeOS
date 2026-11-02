// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// IngressRoutingRow is one row of IngressRouting: a public environment
// and, when HasInstance, one of its service instances that is not
// stopping, with its sandbox and its node. An environment without such an
// instance has one row with HasInstance false and the instance's fields
// zero.
type IngressRoutingRow struct {
	EnvID     uuid.UUID
	Owner     string // the tenant
	Name      string
	HostLabel string
	// Deployment is the environment's latest desired deployment, Deployed its
	// latest deployed one (0: none).
	Deployment, Deployed int
	CreatedAt            time.Time

	HasInstance bool
	SandboxID   podid.ID
	Object      string // the service's name in the spec
	Replica     int
	// InstanceDeployment is the deployment whose target the instance was last
	// kept for: its own deployment.
	InstanceDeployment int
	Phase              string // model.Phase*
	// IngressPorts are the ports the instance's group ing-<sandbox id>
	// admits on its node (EnvInstance.IngressPorts), ascending.
	IngressPorts []int
	Address      string // the pod's IPv6; "": none observed
	ClientState  string // model.State*
	Op           string // "": none
	Node         string // "": none
	// NodeGroupVersion is the version of the security group
	// ferun-ingress that the instance's node holds (0: none).
	NodeGroupVersion int64
	// IdleAfterS is the sandbox's idle_after_s: nil is NULL (the
	// platform's --idle-pause), 0 never.
	IdleAfterS *int
}

// IngressRouting returns what the ingress's table is built from (ADR
// 0021): every environment that is public, not deleting, of a tenant that
// is not deleting, with a deployment and a host label, each with its service
// instances that are not stopping; envIDs limits it to those environments
// (nil: every one; empty: none). The rows are ordered by environment, then
// service, replica and sandbox. An environment it leaves out is one the
// ingress does not serve, whichever of the conditions it fails.
func (s *Store) IngressRouting(ctx context.Context, envIDs []uuid.UUID) ([]IngressRoutingRow, error) {
	if envIDs != nil && len(envIDs) == 0 {
		return nil, nil
	}
	rows, err := s.q.Query(ctx, `SELECT e.id, e.owner, e.name, e.host_label, e.deployment, e.deployed, e.created_at,
			i.sandbox_id, i.object, i.replica, i.deployment, i.phase, i.ingress_ports,
			s.address, s.client_state, s.op, coalesce(s.node, ''), coalesce(n.ingress_group_version, 0), s.idle_after_s
		FROM environments e
		JOIN tenants t ON t.id = e.owner AND t.deleting_at IS NULL
		LEFT JOIN env_instances i ON i.env_id = e.id AND i.kind = 'service' AND i.phase <> 'stopping'
		LEFT JOIN sandboxes s ON s.id = i.sandbox_id
		LEFT JOIN nodes n ON n.name = s.node
		WHERE e.public AND NOT e.deleting AND e.deployment > 0 AND e.host_label IS NOT NULL
			AND ($1::uuid[] IS NULL OR e.id = ANY($1))
		ORDER BY e.id, i.object, i.replica, i.sandbox_id`, envIDs)
	if err != nil {
		return nil, fmt.Errorf("read the ingress's routing: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (IngressRoutingRow, error) {
		var row IngressRoutingRow
		var sandbox, object, phase, address, state, op *string
		var replica, deployment *int
		if err := r.Scan(&row.EnvID, &row.Owner, &row.Name, &row.HostLabel, &row.Deployment, &row.Deployed, &row.CreatedAt,
			&sandbox, &object, &replica, &deployment, &phase, &row.IngressPorts,
			&address, &state, &op, &row.Node, &row.NodeGroupVersion, &row.IdleAfterS); err != nil {
			return IngressRoutingRow{}, err
		}
		if sandbox == nil {
			// No instance: the LEFT JOINs' columns are NULL, and the
			// instance's fields stay zero.
			return row, nil
		}
		row.HasInstance = true
		row.SandboxID = podid.ID(*sandbox)
		row.Object, row.Replica, row.InstanceDeployment, row.Phase = deref(object), deref(replica), deref(deployment), deref(phase)
		row.Address, row.ClientState, row.Op = deref(address), deref(state), deref(op)
		row.IngressPorts = nonNil(row.IngressPorts)
		return row, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the ingress's routing: %w", err)
	}
	return out, nil
}

// deref is *p, or the zero value for nil (a LEFT JOIN's NULL).
func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
