// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// EnvExit is one exit of an instance's main container, as env_instances
// keeps the last five: when, and its code (nil: the node reported none).
type EnvExit struct {
	At   time.Time `json:"at"`
	Code *int      `json:"code"`
}

// EnvInstance is a row of env_instances: the controller's view of an
// instance row of sandboxes (it goes with the row, ON DELETE CASCADE).
// Its sandbox, environment, kind, object, replica and template hash are
// fixed: a changed template is a new instance.
type EnvInstance struct {
	SandboxID    podid.ID
	EnvID        uuid.UUID
	Kind         string // "service", "addon", "job"
	Object       string // the object's name in the spec
	Replica      int
	TemplateHash string
	// Release is the release whose target it was last kept for.
	Release       int
	Phase         string    // model.Phase*
	ReadySince    time.Time // zero: NULL
	RetireAt      time.Time // zero: NULL
	Restarts      int
	LastExit      *int // nil: NULL
	ProbeFailures int
	NextRestartAt time.Time // zero: NULL
	Exits         []EnvExit
	// ProvisionedHash is an addon's hash of the provisioning steps last
	// run.
	ProvisionedHash string
	Detail          string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// IngressPorts are the TCP ports, ascending, that the instance's group
	// ing-<sandbox id> opens to the ingress as last put on or listed by its
	// node (SetIngressPorts; never written by UpdateEnvInstance).
	IngressPorts []int
}

func envInstanceColumns(p string) string {
	cols := []string{"sandbox_id", "env_id", "kind", "object", "replica", "template_hash", "release", "phase",
		"ready_since", "retire_at", "restarts", "last_exit", "probe_failures", "next_restart_at", "exits",
		"provisioned_hash", "detail", "created_at", "updated_at", "ingress_ports"}
	return prefixed(p, cols)
}

var envInstanceCols = envInstanceColumns("")

// prefixed joins cols, each prefixed with p and a dot when p is not empty.
func prefixed(p string, cols []string) string {
	if p != "" {
		cols = slices.Clone(cols)
		for i, c := range cols {
			cols[i] = p + "." + c
		}
	}
	return strings.Join(cols, ", ")
}

// envInstanceTargets are the scan targets of envInstanceCols for i, and
// the function that finishes the scan.
func envInstanceTargets(i *EnvInstance) ([]any, func()) {
	var ready, retire, next *time.Time
	return []any{&i.SandboxID, &i.EnvID, &i.Kind, &i.Object, &i.Replica, &i.TemplateHash, &i.Release, &i.Phase,
			&ready, &retire, &i.Restarts, &i.LastExit, &i.ProbeFailures, &next, &i.Exits,
			&i.ProvisionedHash, &i.Detail, &i.CreatedAt, &i.UpdatedAt, &i.IngressPorts}, func() {
			i.ReadySince, i.RetireAt, i.NextRestartAt = timeOf(ready), timeOf(retire), timeOf(next)
			i.Exits, i.IngressPorts = nonNil(i.Exits), nonNil(i.IngressPorts)
		}
}

func scanEnvInstance(row pgx.Row) (EnvInstance, error) {
	var i EnvInstance
	targets, finish := envInstanceTargets(&i)
	if err := row.Scan(targets...); err != nil {
		return EnvInstance{}, err
	}
	finish()
	return i, nil
}

func collectEnvInstances(rows pgx.Rows, err error) ([]EnvInstance, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnvInstance, error) { return scanEnvInstance(r) })
}

// InsertEnvInstance inserts the controller's row of an instance row of
// sandboxes, which must exist (else ErrNoRow); phase starting when empty;
// ingress_ports empty. One that exists is ErrExists. A service's sends
// ferun_ingress "env:<env id>".
func (s *Store) InsertEnvInstance(ctx context.Context, i EnvInstance) (EnvInstance, error) {
	if i.Phase == "" {
		i.Phase = model.PhaseStarting
	}
	exits, err := jsonText(nonNil(i.Exits))
	if err != nil {
		return EnvInstance{}, err
	}
	out, err := scanEnvInstance(s.q.QueryRow(ctx, `INSERT INTO env_instances (sandbox_id, env_id, kind, object, replica,
			template_hash, release, phase, ready_since, retire_at, restarts, last_exit, probe_failures, next_restart_at,
			exits, provisioned_hash, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::jsonb, $16, $17)
		RETURNING `+envInstanceCols,
		i.SandboxID, i.EnvID, i.Kind, i.Object, i.Replica, i.TemplateHash, i.Release, i.Phase, nullTime(i.ReadySince),
		nullTime(i.RetireAt), i.Restarts, nullInt(i.LastExit), i.ProbeFailures, nullTime(i.NextRestartAt), exits,
		i.ProvisionedHash, i.Detail))
	if err != nil {
		return EnvInstance{}, insertError("insert instance "+i.SandboxID.String(), err)
	}
	if err := s.notifyIngressInstance(ctx, out.EnvID, out.Kind); err != nil {
		return EnvInstance{}, fmt.Errorf("insert instance %s: %w", i.SandboxID, err)
	}
	return out, nil
}

// GetEnvInstance reads the controller's row of an instance; ErrNoRow if
// there is none.
func (s *Store) GetEnvInstance(ctx context.Context, sandboxID podid.ID) (EnvInstance, error) {
	return s.LockEnvInstance(ctx, sandboxID, NoLock)
}

// LockEnvInstance reads the controller's row of an instance and locks it
// with lock; ErrNoRow if there is none.
func (s *Store) LockEnvInstance(ctx context.Context, sandboxID podid.ID, lock RowLock) (EnvInstance, error) {
	i, err := scanEnvInstance(s.q.QueryRow(ctx, "SELECT "+envInstanceCols+" FROM env_instances WHERE sandbox_id = $1 "+
		string(lock), sandboxID))
	if err != nil {
		return EnvInstance{}, noRow(err, ErrNoRow)
	}
	return i, nil
}

// ListEnvInstances returns the environment's instances by kind, object,
// replica and creation.
func (s *Store) ListEnvInstances(ctx context.Context, envID uuid.UUID) ([]EnvInstance, error) {
	return collectEnvInstances(s.q.Query(ctx, "SELECT "+envInstanceCols+` FROM env_instances
		WHERE env_id = $1 ORDER BY kind, object, replica, created_at`, envID))
}

// EnvInstanceRow is an instance with its row of sandboxes.
type EnvInstanceRow struct {
	Instance EnvInstance
	Sandbox  Sandbox
}

// ListEnvInstanceRows returns the environment's instances with their rows
// of sandboxes, as ListEnvInstances orders them.
func (s *Store) ListEnvInstanceRows(ctx context.Context, envID uuid.UUID) ([]EnvInstanceRow, error) {
	rows, err := s.q.Query(ctx, "SELECT "+sandboxColumns("s")+", "+envInstanceColumns("i")+` FROM env_instances i
		JOIN sandboxes s ON s.id = i.sandbox_id WHERE i.env_id = $1 ORDER BY i.kind, i.object, i.replica, i.created_at`, envID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnvInstanceRow, error) {
		var out EnvInstanceRow
		targets, finish := envInstanceTargets(&out.Instance)
		sb, err := scanSandbox(extraScan{row: r, extra: targets})
		if err != nil {
			return EnvInstanceRow{}, err
		}
		finish()
		out.Sandbox = sb
		return out, nil
	})
}

// UpdateEnvInstance writes the columns in which after differs from before,
// the row as the caller read it (normally locked FOR UPDATE): release,
// phase, ready_since, retire_at, restarts, last_exit, probe_failures,
// next_restart_at, exits, provisioned_hash and detail;
// updated_at = now(). The fixed columns and ingress_ports
// (SetIngressPorts) are refused. When nothing differs, before is returned
// unwritten. A service's write of phase or release sends ferun_ingress
// "env:<env id>".
func (s *Store) UpdateEnvInstance(ctx context.Context, before, after EnvInstance) (EnvInstance, error) {
	what := "instance " + before.SandboxID.String()
	if err := fixed(what, map[string]bool{"sandbox_id": before.SandboxID != after.SandboxID,
		"env_id": before.EnvID != after.EnvID, "kind": before.Kind != after.Kind, "object": before.Object != after.Object,
		"replica": before.Replica != after.Replica, "template_hash": before.TemplateHash != after.TemplateHash,
		"ingress_ports": !slices.Equal(before.IngressPorts, after.IngressPorts)}); err != nil {
		return EnvInstance{}, err
	}
	u := newUpdates(before.SandboxID)
	u.set("release", before.Release != after.Release, after.Release)
	u.set("phase", before.Phase != after.Phase, after.Phase)
	u.set("ready_since", !before.ReadySince.Equal(after.ReadySince), nullTime(after.ReadySince))
	u.set("retire_at", !before.RetireAt.Equal(after.RetireAt), nullTime(after.RetireAt))
	u.set("restarts", before.Restarts != after.Restarts, after.Restarts)
	u.set("last_exit", !sameIntPtr(before.LastExit, after.LastExit), nullInt(after.LastExit))
	u.set("probe_failures", before.ProbeFailures != after.ProbeFailures, after.ProbeFailures)
	u.set("next_restart_at", !before.NextRestartAt.Equal(after.NextRestartAt), nullTime(after.NextRestartAt))
	if err := u.setJSON("exits", !sameExits(before.Exits, after.Exits), nonNil(after.Exits)); err != nil {
		return EnvInstance{}, err
	}
	u.set("provisioned_hash", before.ProvisionedHash != after.ProvisionedHash, after.ProvisionedHash)
	u.set("detail", before.Detail != after.Detail, after.Detail)
	if u.empty() {
		return before, nil
	}
	i, err := scanEnvInstance(s.q.QueryRow(ctx, "UPDATE env_instances SET "+u.clause(true)+
		" WHERE sandbox_id = $1 RETURNING "+envInstanceCols, u.args...))
	if err != nil {
		return EnvInstance{}, fmt.Errorf("update %s: %w", what, noRow(err, ErrNoRow))
	}
	if before.Phase != after.Phase || before.Release != after.Release {
		if err := s.notifyIngressInstance(ctx, i.EnvID, i.Kind); err != nil {
			return EnvInstance{}, fmt.Errorf("update %s: %w", what, err)
		}
	}
	return i, nil
}

func sameExits(a, b []EnvExit) bool {
	return slices.EqualFunc(a, b, func(x, y EnvExit) bool { return x.At.Equal(y.At) && sameIntPtr(x.Code, y.Code) })
}

// DeleteEnvInstance removes the controller's row of an instance (the
// sandbox row's deletion does so too, whose ferun_sandbox the ingress's
// table follows) and reports whether it was there. A service's sends
// ferun_ingress "env:<env id>".
func (s *Store) DeleteEnvInstance(ctx context.Context, sandboxID podid.ID) (bool, error) {
	var envID uuid.UUID
	var kind string
	err := s.q.QueryRow(ctx, "DELETE FROM env_instances WHERE sandbox_id = $1 RETURNING env_id, kind", sandboxID).
		Scan(&envID, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, s.notifyIngressInstance(ctx, envID, kind)
}

// SetIngressPorts records the ports the instance's group ing-<sandbox id>
// opens to the ingress, as put on or listed by its node: sorted and
// without duplicates, written only when they differ, with ferun_ingress
// "env:<env id>". It reports whether it wrote; false too for an instance
// without a row.
func (s *Store) SetIngressPorts(ctx context.Context, id podid.ID, ports []int) (bool, error) {
	ports = slices.Compact(slices.Sorted(slices.Values(ports)))
	var envID uuid.UUID
	err := s.q.QueryRow(ctx, `UPDATE env_instances SET ingress_ports = $2::integer[]
		WHERE sandbox_id = $1 AND ingress_ports IS DISTINCT FROM $2::integer[] RETURNING env_id`, id, nonNil(ports)).Scan(&envID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("set the ingress ports of instance %s: %w", id, err)
	}
	if err := s.notifyIngressEnvs(ctx, []uuid.UUID{envID}); err != nil {
		return false, fmt.Errorf("set the ingress ports of instance %s: %w", id, err)
	}
	return true, nil
}

// notifyIngressInstance sends ferun_ingress "env:<env id>" for a write of
// an instance of kind, when it is a service: the only kind the ingress's
// table reads.
func (s *Store) notifyIngressInstance(ctx context.Context, envID uuid.UUID, kind string) error {
	if kind != "service" {
		return nil
	}
	return s.notifyIngressEnvs(ctx, []uuid.UUID{envID})
}
