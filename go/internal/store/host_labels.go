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
// else the zone's name, as projects.zone holds it), and an environment's
// label lives in its project's zone.

// ClaimHostLabel claims the default host label of the environment e in
// its project's zone and returns it (ADR 0063): the natural label
// "<name>--<project>" when model.HostLabels finds it usable, that is
// unambiguous and a DNS label, else, or when another project owns it or
// another environment holds it in that zone, the fallback with the hash of
// e's ID. Since no other pair of name and project has the same
// usable natural label and no fallback has its form, the owner check
// below is a second guard. A label belongs to the project that first
// claimed it in its zone, forever, and is free again for that project
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
	zone, err := s.projectZone(ctx, e.Owner)
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
	// Only a SHA-256 collision gives the fallback to another project's
	// environment, or to a live one.
	return "", fmt.Errorf("claim a host label for environment %s: %s taken", e.ID, strings.Join(candidates, " and "))
}

// takeHostLabel registers label in zone to e: free, or e's project's with
// no environment holding it; false when another project owns it or another
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

// projectZone is projects.zone of the project ("": the apps zone).
func (s *Store) projectZone(ctx context.Context, project string) (string, error) {
	var zone string
	err := s.q.QueryRow(ctx, "SELECT coalesce(zone, '') FROM projects WHERE id = $1", project).Scan(&zone)
	if err != nil {
		return "", fmt.Errorf("the zone of project %s: %w", project, noRow(err, ErrNoRow))
	}
	return zone, nil
}

// HostLabelOwner returns the project host_labels registers label to in
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

// ErrHostLabelTaken: the label a project chose is another project's, or
// another environment holds it.
var ErrHostLabelTaken = errors.New("host label taken")

// SetEnvironmentHostLabel gives the environment e a default host label
// its project chose in its project's zone (model.ChosenHostLabel, which
// the caller checks), or the project's label, the project's address
// (model.ProjectLabel, label e.Owner; ADR 0063), and returns its row. It
// runs in the caller's transaction, with e's row and its project's
// locked, the project's FOR UPDATE when label is its own. The label e held
// is released first, and stays its project's like one of a deleted
// environment; the new one is claimed as ClaimHostLabel claims: free, or
// its project's with no environment holding it, else ErrHostLabelTaken.
// The project's label moves: the environment of the project that holds it
// gives it up and gets its natural label or its fallback, in the same
// transaction. A label e holds already changes nothing. A write sends
// ferun_ingress "env:<id>" for every environment whose label changed, so
// the instances serve the new hosts and drop the old ones.
func (s *Store) SetEnvironmentHostLabel(ctx context.Context, e Environment, label string) (Environment, error) {
	if e.HostLabel == label {
		return e, nil
	}
	zone, err := s.projectZone(ctx, e.Owner)
	if err != nil {
		return Environment{}, err
	}
	var holder *Environment
	if label == e.Owner {
		if holder, err = s.labelHolder(ctx, zone, label, e); err != nil {
			return Environment{}, err
		}
	}
	if _, err := s.q.Exec(ctx, "UPDATE host_labels SET env_id = NULL WHERE env_id = $1", e.ID); err != nil {
		return Environment{}, fmt.Errorf("release the host label of environment %s: %w", e.ID, err)
	}
	if holder != nil {
		if _, err := s.q.Exec(ctx, "UPDATE host_labels SET env_id = NULL WHERE env_id = $1", holder.ID); err != nil {
			return Environment{}, fmt.Errorf("release the host label of environment %s: %w", holder.ID, err)
		}
	}
	ok, err := s.takeHostLabel(ctx, zone, label, e)
	if err != nil {
		return Environment{}, err
	}
	if !ok {
		return Environment{}, fmt.Errorf("host label %s for environment %s: %w", label, e.ID, ErrHostLabelTaken)
	}
	if holder != nil {
		given, err := s.claimDefaultLabel(ctx, *holder, zone)
		if err != nil {
			return Environment{}, err
		}
		if _, err := s.writeHostLabel(ctx, holder.ID, given); err != nil {
			return Environment{}, err
		}
	}
	return s.writeHostLabel(ctx, e.ID, label)
}

// labelHolder returns the environment of e's project that holds label in
// zone, its row locked FOR UPDATE; nil when none does, or one of another
// project, whose label it stays.
func (s *Store) labelHolder(ctx context.Context, zone, label string, e Environment) (*Environment, error) {
	h, err := scanEnvironment(s.q.QueryRow(ctx, "SELECT "+environmentCols+` FROM environments
		WHERE id = (SELECT env_id FROM host_labels WHERE zone = $1 AND label = $2 AND owner = $3)
			AND id <> $4 FOR UPDATE`, zone, label, e.Owner, e.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the holder of host label %s: %w", label, err)
	}
	return &h, nil
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

// ZoneMove is what moving a project to another zone did to one of its
// environments' labels: From is the label it held in the zone before, To
// the one it holds in the new zone.
type ZoneMove struct {
	EnvID    uuid.UUID
	Name     string
	From, To string
}

// SetProjectZone moves the project to zone (projects.zone; "": the apps
// zone) and its environments' default hosts with it (ADR 0031): each
// environment that holds a label keeps it in the new zone where it may,
// else gets its natural label or its fallback there, as at a first
// publish. A label may move when it is free in zone or the project's
// there, and is one its project chose (model.ChosenHostLabel) or one its
// name or its project gave it (model.GivenHostLabel): a plain label of the
// zone owners before ADR 0063 does not. The labels in the old zone stay the
// project's, free for it alone, as a changed label's do. It locks the
// project FOR UPDATE, runs in a transaction (the caller's, when in one),
// sends ferun_ingress for every environment it moved and returns the
// moves, by name; ErrNoRow for an unknown project. A move to the zone the
// project has changes nothing.
func (s *Store) SetProjectZone(ctx context.Context, project, zone string) ([]ZoneMove, error) {
	var moves []ZoneMove
	err := s.InTx(ctx, func(tx *Store) error {
		moves = nil
		t, err := tx.LockProject(ctx, project, ForUpdate)
		if err != nil {
			return err
		}
		if t.Zone == zone {
			return nil
		}
		if _, err := tx.q.Exec(ctx, "UPDATE projects SET zone = $2 WHERE id = $1", project, nullString(zone)); err != nil {
			return fmt.Errorf("set the zone of project %s: %w", project, err)
		}
		rows, err := tx.q.Query(ctx, "SELECT "+environmentCols+` FROM environments
			WHERE owner = $1 AND host_label IS NOT NULL ORDER BY name FOR UPDATE`, project)
		if err != nil {
			return err
		}
		envs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Environment, error) { return scanEnvironment(r) })
		if err != nil {
			return fmt.Errorf("the environments of project %s: %w", project, err)
		}
		ids := make([]uuid.UUID, 0, len(envs))
		for _, e := range envs {
			if _, err := tx.q.Exec(ctx, "UPDATE host_labels SET env_id = NULL WHERE env_id = $1", e.ID); err != nil {
				return fmt.Errorf("release the host label of environment %s: %w", e.ID, err)
			}
			label := e.HostLabel
			ok := false
			if model.GivenHostLabel(label, e.Owner, e.ID) || model.ChosenHostLabel(label, e.Owner) {
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
		return nil, fmt.Errorf("move project %s to zone %q: %w", project, zone, err)
	}
	return moves, nil
}
