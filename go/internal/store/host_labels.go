// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/model"
)

// A default host is a label in a zone (ADR 0021, ADR 0031): host_labels
// registers each label per zone (host_labels.zone: "" for the apps zone,
// else the zone's name, as tenants.zone holds it), and an environment's
// label lives in its tenant's zone.

// ClaimHostLabel claims the default host label of the environment e in
// its tenant's zone and returns it (D12 of the ingress's design): the
// natural label "<name>-<tenant>" when model.HostLabels finds it usable,
// that is unambiguous and a DNS label, else, or when another tenant owns
// it or another environment holds it in that zone, the fallback with the
// hash of e's ID. Since no other pair of name and tenant has the same
// usable natural label and no fallback has its form, the owner check
// below is a second guard. A label belongs to the tenant that first
// claimed it in its zone, forever, and is free again for that tenant
// alone once its environment is gone (host_labels.env_id ON DELETE SET
// NULL). A label e holds already is returned as it is. It runs in the
// caller's transaction, with e's row locked; the caller writes the label
// to environments.host_label.
func (s *Store) ClaimHostLabel(ctx context.Context, e Environment) (string, error) {
	var held string
	err := s.q.QueryRow(ctx, "SELECT label FROM host_labels WHERE env_id = $1", e.ID).Scan(&held)
	if err == nil {
		return held, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("claim a host label for environment %s: %w", e.ID, err)
	}
	zone, err := s.tenantZone(ctx, e.Owner)
	if err != nil {
		return "", err
	}
	return s.claimDefaultLabel(ctx, e, zone)
}

// claimDefaultLabel claims e's natural label in zone, else its fallback
// (ClaimHostLabel), e holding none.
func (s *Store) claimDefaultLabel(ctx context.Context, e Environment, zone string) (string, error) {
	natural, naturalOK, fallback := model.HostLabels(e.Name, e.Owner, e.ID)
	candidates := []string{fallback}
	if naturalOK {
		candidates = []string{natural, fallback}
	}
	for _, label := range candidates {
		ok, err := s.takeHostLabel(ctx, zone, label, e)
		if err != nil {
			return "", err
		}
		if ok {
			return label, nil
		}
	}
	// Only a SHA-256 collision gives the fallback to another tenant's
	// environment, or to a live one.
	return "", fmt.Errorf("claim a host label for environment %s: %s taken", e.ID, strings.Join(candidates, " and "))
}

// takeHostLabel registers label in zone to e: free, or e's tenant's with
// no environment holding it; false when another tenant owns it or another
// environment holds it. e holds no label at the time.
func (s *Store) takeHostLabel(ctx context.Context, zone, label string, e Environment) (bool, error) {
	var got string
	err := s.q.QueryRow(ctx, `INSERT INTO host_labels (zone, label, owner, env_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT (zone, label) DO UPDATE SET env_id = excluded.env_id, assigned_at = now()
			WHERE host_labels.owner = excluded.owner AND host_labels.env_id IS NULL
		RETURNING label`, zone, label, e.Owner, e.ID).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim host label %s for environment %s: %w", label, e.ID, err)
	}
	return true, nil
}

// tenantZone is tenants.zone of the tenant ("": the apps zone).
func (s *Store) tenantZone(ctx context.Context, tenant string) (string, error) {
	var zone string
	err := s.q.QueryRow(ctx, "SELECT coalesce(zone, '') FROM tenants WHERE id = $1", tenant).Scan(&zone)
	if err != nil {
		return "", fmt.Errorf("the zone of tenant %s: %w", tenant, noRow(err, ErrNoRow))
	}
	return zone, nil
}

// HostLabelOwner returns the tenant host_labels registers label to in
// zone ("": the apps zone), the one that first claimed it there; ErrNoRow
// when none did.
func (s *Store) HostLabelOwner(ctx context.Context, zone, label string) (string, error) {
	var owner string
	err := s.q.QueryRow(ctx, "SELECT owner FROM host_labels WHERE zone = $1 AND label = $2", zone, label).Scan(&owner)
	if err = noRow(err, ErrNoRow); err != nil && !errors.Is(err, ErrNoRow) {
		return "", fmt.Errorf("the owner of host label %s: %w", label, err)
	}
	return owner, err
}

// ErrHostLabelTaken: the label a tenant chose is another tenant's, or
// another environment holds it.
var ErrHostLabelTaken = errors.New("host label taken")

// SetEnvironmentHostLabel gives the environment e the default host label
// its tenant chose (model.ChosenHostLabel or, for a tenant that owns the
// apps zone and has its hosts there, model.PlainHostLabel, which the
// caller checks) in its tenant's zone and returns its row. It runs in the
// caller's transaction, with e's row and its tenant's locked. The label e
// held is released first, and stays its tenant's like one of a deleted
// environment; the chosen one is claimed as ClaimHostLabel claims: free,
// or its tenant's with no environment holding it, else ErrHostLabelTaken.
// A label e holds already changes nothing. A write sends ferun_ingress
// "env:<id>", so the instances serve the new host and drop the old one.
func (s *Store) SetEnvironmentHostLabel(ctx context.Context, e Environment, label string) (Environment, error) {
	if e.HostLabel == label {
		return e, nil
	}
	zone, err := s.tenantZone(ctx, e.Owner)
	if err != nil {
		return Environment{}, err
	}
	if _, err := s.q.Exec(ctx, "UPDATE host_labels SET env_id = NULL WHERE env_id = $1", e.ID); err != nil {
		return Environment{}, fmt.Errorf("release the host label of environment %s: %w", e.ID, err)
	}
	ok, err := s.takeHostLabel(ctx, zone, label, e)
	if err != nil {
		return Environment{}, err
	}
	if !ok {
		return Environment{}, fmt.Errorf("host label %s for environment %s: %w", label, e.ID, ErrHostLabelTaken)
	}
	return s.writeHostLabel(ctx, e.ID, label)
}

// writeHostLabel writes environments.host_label and tells the ingress.
func (s *Store) writeHostLabel(ctx context.Context, id uuid.UUID, label string) (Environment, error) {
	out, err := scanEnvironment(s.q.QueryRow(ctx, `UPDATE environments SET host_label = $2, updated_at = now()
		WHERE id = $1 RETURNING `+environmentCols, id, label))
	if err != nil {
		return Environment{}, fmt.Errorf("set the host label of environment %s: %w", id, err)
	}
	return out, s.notifyIngressEnvs(ctx, []uuid.UUID{id})
}

// ZoneMove is what moving a tenant to another zone did to one of its
// environments' labels: From is the label it held in the zone before, To
// the one it holds in the new zone.
type ZoneMove struct {
	EnvID    uuid.UUID
	Name     string
	From, To string
}

// SetTenantZone moves the tenant to zone (tenants.zone; "": the apps
// zone) and its environments' default hosts with it (ADR 0031): each
// environment that holds a label keeps it in the new zone where it may,
// else gets its natural label or its fallback there, as at a first
// publish. A label may move when it is free in zone or the tenant's
// there, and is not a plain label unless plainOK (the tenant owns the new
// zone's plain names, model.Zones.PlainLabels); its natural label and its
// fallback, also of a name it had before a rename, are never plain ones. The labels in the old zone stay the
// tenant's, free for it alone, as a changed label's do. It locks the
// tenant FOR UPDATE, runs in a transaction (the caller's, when in one),
// sends ferun_ingress for every environment it moved and returns the
// moves, by name; ErrNoRow for an unknown tenant. A move to the zone the
// tenant has changes nothing.
func (s *Store) SetTenantZone(ctx context.Context, tenant, zone string, plainOK bool) ([]ZoneMove, error) {
	var moves []ZoneMove
	err := s.InTx(ctx, func(tx *Store) error {
		moves = nil
		t, err := tx.LockTenant(ctx, tenant, ForUpdate)
		if err != nil {
			return err
		}
		if t.Zone == zone {
			return nil
		}
		if _, err := tx.q.Exec(ctx, "UPDATE tenants SET zone = $2 WHERE id = $1", tenant, nullString(zone)); err != nil {
			return fmt.Errorf("set the zone of tenant %s: %w", tenant, err)
		}
		rows, err := tx.q.Query(ctx, "SELECT "+environmentCols+` FROM environments
			WHERE owner = $1 AND host_label IS NOT NULL ORDER BY name FOR UPDATE`, tenant)
		if err != nil {
			return err
		}
		envs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Environment, error) { return scanEnvironment(r) })
		if err != nil {
			return fmt.Errorf("the environments of tenant %s: %w", tenant, err)
		}
		ids := make([]uuid.UUID, 0, len(envs))
		for _, e := range envs {
			if _, err := tx.q.Exec(ctx, "UPDATE host_labels SET env_id = NULL WHERE env_id = $1", e.ID); err != nil {
				return fmt.Errorf("release the host label of environment %s: %w", e.ID, err)
			}
			label := e.HostLabel
			// Its own is one its name gave it, the one it has or one it
			// had before a rename.
			own := model.GivenHostLabel(label, e.Owner, e.ID)
			ok := false
			if own || model.ChosenHostLabel(label) || (plainOK && model.PlainHostLabel(label)) {
				if ok, err = tx.takeHostLabel(ctx, zone, label, e); err != nil {
					return err
				}
			}
			if !ok {
				if label, err = tx.claimDefaultLabel(ctx, e, zone); err != nil {
					return err
				}
			}
			if label != e.HostLabel {
				if _, err := tx.q.Exec(ctx, "UPDATE environments SET host_label = $2, updated_at = now() WHERE id = $1",
					e.ID, label); err != nil {
					return fmt.Errorf("set the host label of environment %s: %w", e.ID, err)
				}
			}
			moves = append(moves, ZoneMove{EnvID: e.ID, Name: e.Name, From: e.HostLabel, To: label})
			ids = append(ids, e.ID)
		}
		return tx.notifyIngressEnvs(ctx, ids)
	})
	if err != nil {
		return nil, fmt.Errorf("move tenant %s to zone %q: %w", tenant, zone, err)
	}
	return moves, nil
}
