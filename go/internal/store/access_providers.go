// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This file holds the typed queries of a project's access providers (ADR
// 0054, migration 0034): access_providers, of kind project (the project's
// own service at url) and of kind oidc (an issuer with a sealed client
// secret and a rule). The builtin provider ferun has no row: an
// environment that names it has a NULL access_provider.

// ErrInUse is returned by DeleteAccessProvider for a provider an
// environment names (the foreign key of environments).
var ErrInUse = errors.New("the row is named by another")

// AccessProvider is a row of access_providers.
type AccessProvider struct {
	Project, Name string
	// Kind is model.ProviderKindProject or model.ProviderKindOIDC.
	Kind string
	// URL is a project provider's https URL; "" for oidc.
	URL string
	// Issuer, ClientID and ClientSecretSealed are an oidc provider's,
	// the secret sealed under the AAD AccessProviderAAD; Rule its rule
	// as JSON ({} for project).
	Issuer, ClientID   string
	ClientSecretSealed []byte
	Rule               json.RawMessage
	MadeBy             string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// AccessProviderAAD is the AAD an oidc provider's client secret is sealed
// under: "access_providers|<project>|<name>".
func AccessProviderAAD(project, name string) []byte {
	return []byte("access_providers|" + project + "|" + name)
}

const accessProviderCols = `project, name, kind, url, issuer, client_id, client_secret_sealed, rule, made_by,
	created_at, updated_at`

func scanAccessProvider(row pgx.Row) (AccessProvider, error) {
	var p AccessProvider
	var rule []byte
	if err := row.Scan(&p.Project, &p.Name, &p.Kind, &p.URL, &p.Issuer, &p.ClientID, &p.ClientSecretSealed, &rule,
		&p.MadeBy, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return AccessProvider{}, err
	}
	p.Rule = json.RawMessage(rule)
	return p, nil
}

// ruleText is a rule as the jsonb column takes it: {} for none.
func ruleText(r json.RawMessage) string {
	if len(r) == 0 {
		return "{}"
	}
	return string(r)
}

// ListAccessProviders returns the providers of project, by name.
func (s *Store) ListAccessProviders(ctx context.Context, project string) ([]AccessProvider, error) {
	rows, err := s.q.Query(ctx, "SELECT "+accessProviderCols+" FROM access_providers WHERE project = $1 ORDER BY name",
		project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AccessProvider, error) { return scanAccessProvider(r) })
}

// GetAccessProvider reads a provider of project; ErrNoRow if there is none.
func (s *Store) GetAccessProvider(ctx context.Context, project, name string) (AccessProvider, error) {
	return s.LockAccessProvider(ctx, project, name, NoLock)
}

// LockAccessProvider reads a provider of project and locks its row with
// lock; ErrNoRow if there is none.
func (s *Store) LockAccessProvider(ctx context.Context, project, name string, lock RowLock) (AccessProvider, error) {
	p, err := scanAccessProvider(s.q.QueryRow(ctx, "SELECT "+accessProviderCols+
		" FROM access_providers WHERE project = $1 AND name = $2 "+string(lock), project, name))
	return p, noRow(err, ErrNoRow)
}

// CountAccessProviders counts the providers of project.
func (s *Store) CountAccessProviders(ctx context.Context, project string) (int, error) {
	var n int
	err := s.q.QueryRow(ctx, "SELECT count(*) FROM access_providers WHERE project = $1", project).Scan(&n)
	return n, err
}

// InsertAccessProvider inserts a provider; a name its project has is
// ErrExists, an unknown project ErrNoRow. The database checks the kind's
// fields (access_providers_project_kind, access_providers_oidc_kind).
func (s *Store) InsertAccessProvider(ctx context.Context, p AccessProvider) (AccessProvider, error) {
	out, err := scanAccessProvider(s.q.QueryRow(ctx, `INSERT INTO access_providers (project, name, kind, url, issuer,
			client_id, client_secret_sealed, rule, made_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9) RETURNING `+accessProviderCols, p.Project, p.Name, p.Kind,
		p.URL, p.Issuer, p.ClientID, p.ClientSecretSealed, ruleText(p.Rule), p.MadeBy))
	switch {
	case isUniqueViolation(err):
		return AccessProvider{}, fmt.Errorf("insert provider %s of %s: %w", p.Name, p.Project, ErrExists)
	case isForeignKeyViolation(err):
		return AccessProvider{}, fmt.Errorf("insert provider %s of %s: %w", p.Name, p.Project, ErrNoRow)
	case err != nil:
		return AccessProvider{}, fmt.Errorf("insert provider %s of %s: %w", p.Name, p.Project, err)
	}
	return out, nil
}

// UpdateAccessProvider writes a provider's kind, fields, secret, rule and
// made_by, with updated_at = now(), and sends ferun_ingress "env:<id>" for
// every environment that names it (the table carries its address);
// ErrNoRow if there is none. The caller decides whether anything changed
// and ends the provider's visits (EndProviderVisits) where a change asks
// for it.
func (s *Store) UpdateAccessProvider(ctx context.Context, p AccessProvider) (AccessProvider, error) {
	var out AccessProvider
	err := s.InTx(ctx, func(tx *Store) error {
		var err error
		out, err = scanAccessProvider(tx.q.QueryRow(ctx, `UPDATE access_providers SET kind = $3, url = $4, issuer = $5,
				client_id = $6, client_secret_sealed = $7, rule = $8::jsonb, made_by = $9, updated_at = now()
			WHERE project = $1 AND name = $2 RETURNING `+accessProviderCols, p.Project, p.Name, p.Kind, p.URL, p.Issuer,
			p.ClientID, p.ClientSecretSealed, ruleText(p.Rule), p.MadeBy))
		if err != nil {
			return noRow(err, ErrNoRow)
		}
		ids, err := tx.EnvironmentsNamingProvider(ctx, p.Project, p.Name)
		if err != nil {
			return err
		}
		return tx.notifyIngressEnvs(ctx, ids)
	})
	if err != nil {
		return AccessProvider{}, fmt.Errorf("update provider %s of %s: %w", p.Name, p.Project, err)
	}
	return out, nil
}

// DeleteAccessProvider deletes a provider of project and reports whether
// there was one; one an environment names is ErrInUse.
func (s *Store) DeleteAccessProvider(ctx context.Context, project, name string) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM access_providers WHERE project = $1 AND name = $2", project, name)
	if isForeignKeyViolation(err) {
		return false, fmt.Errorf("delete provider %s of %s: %w", name, project, ErrInUse)
	}
	if err != nil {
		return false, fmt.Errorf("delete provider %s of %s: %w", name, project, err)
	}
	return tag.RowsAffected() > 0, nil
}

// EnvironmentsNamingProvider returns the IDs of the environments of project
// whose access is visitors of the provider name, "" for the provider
// ferun (a NULL access_provider), oldest first.
func (s *Store) EnvironmentsNamingProvider(ctx context.Context, project, name string) ([]uuid.UUID, error) {
	rows, err := s.q.Query(ctx, `SELECT id FROM environments
		WHERE owner = $1 AND access = 'visitors' AND coalesce(access_provider, '') = $2 ORDER BY created_at, id`,
		project, name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// ProviderEnvironment is an environment whose access is visitors of a
// provider.
type ProviderEnvironment struct {
	ID   uuid.UUID
	Name string
	// Provider is the provider's name, "" for ferun.
	Provider string
}

// ProviderEnvironments returns the environments of project whose access is
// visitors, with the provider each names, by name.
func (s *Store) ProviderEnvironments(ctx context.Context, project string) ([]ProviderEnvironment, error) {
	rows, err := s.q.Query(ctx, `SELECT id, name, coalesce(access_provider, '') FROM environments
		WHERE owner = $1 AND access = 'visitors' ORDER BY name`, project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ProviderEnvironment, error) {
		var e ProviderEnvironment
		err := r.Scan(&e.ID, &e.Name, &e.Provider)
		return e, err
	})
}
