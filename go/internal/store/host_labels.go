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

// ClaimHostLabel claims the default host label of the environment e under
// the apps zone and returns it (D12 of the ingress's design): the natural
// label "<name>-<tenant>" when model.HostLabels finds it usable, that is
// unambiguous and a DNS label, else, or when another tenant owns it or
// another environment holds it, the fallback with the hash of e's ID.
// Since no other pair of name and tenant has the same usable natural
// label and no fallback has its form, the owner check below is a second
// guard. A label belongs to the tenant that first claimed it, forever,
// and is free again for that tenant alone once its environment is gone
// (host_labels.env_id ON DELETE SET NULL). A label e holds already is
// returned as it is. It runs in the caller's transaction, with e's row
// locked; the caller writes the label to environments.host_label.
func (s *Store) ClaimHostLabel(ctx context.Context, e Environment) (string, error) {
	var held string
	err := s.q.QueryRow(ctx, "SELECT label FROM host_labels WHERE env_id = $1", e.ID).Scan(&held)
	if err == nil {
		return held, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("claim a host label for environment %s: %w", e.ID, err)
	}
	natural, naturalOK, fallback := model.HostLabels(e.Name, e.Owner, e.ID)
	candidates := []string{fallback}
	if naturalOK {
		candidates = []string{natural, fallback}
	}
	for _, label := range candidates {
		var got string
		err := s.q.QueryRow(ctx, `INSERT INTO host_labels (label, owner, env_id) VALUES ($1, $2, $3)
			ON CONFLICT (label) DO UPDATE SET env_id = excluded.env_id, assigned_at = now()
				WHERE host_labels.owner = excluded.owner AND host_labels.env_id IS NULL
			RETURNING label`, label, e.Owner, e.ID).Scan(&got)
		if err == nil {
			return got, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("claim host label %s for environment %s: %w", label, e.ID, err)
		}
	}
	// Only a SHA-256 collision gives the fallback to another tenant's
	// environment, or to a live one.
	return "", fmt.Errorf("claim a host label for environment %s: %s taken", e.ID, strings.Join(candidates, " and "))
}

// ErrHostLabelTaken: the label a tenant chose is another tenant's, or
// another environment holds it.
var ErrHostLabelTaken = errors.New("host label taken")

// SetEnvironmentHostLabel gives the environment e the default host label
// its tenant chose (model.ChosenHostLabel, which the caller checks) and
// returns its row. It runs in the caller's transaction, with e's row
// locked. The label e held is released first, and stays its tenant's like
// one of a deleted environment; the chosen one is claimed as ClaimHostLabel
// claims: free, or its tenant's with no environment holding it, else
// ErrHostLabelTaken. A label e holds already changes nothing. A write sends
// ferun_ingress "env:<id>", so the instances serve the new host and drop
// the old one.
func (s *Store) SetEnvironmentHostLabel(ctx context.Context, e Environment, label string) (Environment, error) {
	if e.HostLabel == label {
		return e, nil
	}
	if _, err := s.q.Exec(ctx, "UPDATE host_labels SET env_id = NULL WHERE env_id = $1", e.ID); err != nil {
		return Environment{}, fmt.Errorf("release the host label of environment %s: %w", e.ID, err)
	}
	var got string
	err := s.q.QueryRow(ctx, `INSERT INTO host_labels (label, owner, env_id) VALUES ($1, $2, $3)
		ON CONFLICT (label) DO UPDATE SET env_id = excluded.env_id, assigned_at = now()
			WHERE host_labels.owner = excluded.owner AND host_labels.env_id IS NULL
		RETURNING label`, label, e.Owner, e.ID).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return Environment{}, fmt.Errorf("host label %s for environment %s: %w", label, e.ID, ErrHostLabelTaken)
	}
	if err != nil {
		return Environment{}, fmt.Errorf("claim host label %s for environment %s: %w", label, e.ID, err)
	}
	out, err := scanEnvironment(s.q.QueryRow(ctx, `UPDATE environments SET host_label = $2, updated_at = now()
		WHERE id = $1 RETURNING `+environmentCols, e.ID, label))
	if err != nil {
		return Environment{}, fmt.Errorf("set the host label of environment %s: %w", e.ID, err)
	}
	return out, s.notifyIngressEnvs(ctx, []uuid.UUID{e.ID})
}
