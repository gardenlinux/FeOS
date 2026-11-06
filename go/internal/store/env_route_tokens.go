// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// EnvRouteToken is a row of env_route_tokens: a bearer of an environment's
// route door, stored by its hash (HashToken) only.
type EnvRouteToken struct {
	Hash       string
	EnvID      uuid.UUID
	CreatedAt  time.Time
	LastUsedAt time.Time // zero: never
}

const routeTokenCols = "hash, env_id, created_at, last_used_at"

func scanRouteToken(row pgx.Row) (EnvRouteToken, error) {
	var t EnvRouteToken
	var used *time.Time
	if err := row.Scan(&t.Hash, &t.EnvID, &t.CreatedAt, &used); err != nil {
		return EnvRouteToken{}, err
	}
	t.LastUsedAt = timeOf(used)
	return t, nil
}

// InsertRouteToken stores a route token of the environment by its hash,
// made by madeBy (a principal, ADR 0031); ErrNoRow for an unknown
// environment, ErrExists for a hash taken.
func (s *Store) InsertRouteToken(ctx context.Context, hash string, envID uuid.UUID, madeBy string) (EnvRouteToken, error) {
	t, err := scanRouteToken(s.q.QueryRow(ctx, `INSERT INTO env_route_tokens (hash, env_id, made_by) VALUES ($1, $2, $3)
		RETURNING `+routeTokenCols, hash, envID, madeBy))
	if err != nil {
		return EnvRouteToken{}, insertError("insert route token of environment "+envID.String(), err)
	}
	return t, nil
}

// ResolveRouteToken reads the route token of a hash; ErrNoRow if there is
// none.
func (s *Store) ResolveRouteToken(ctx context.Context, hash string) (EnvRouteToken, error) {
	t, err := scanRouteToken(s.q.QueryRow(ctx, "SELECT "+routeTokenCols+" FROM env_route_tokens WHERE hash = $1", hash))
	if err != nil {
		return EnvRouteToken{}, noRow(err, ErrNoRow)
	}
	return t, nil
}

// TouchRouteToken sets last_used_at = now(), at most once a minute.
func (s *Store) TouchRouteToken(ctx context.Context, hash string) error {
	_, err := s.q.Exec(ctx, `UPDATE env_route_tokens SET last_used_at = now() WHERE hash = $1
		AND (last_used_at IS NULL OR last_used_at < now() - interval '`+tokenTouchInterval+`')`, hash)
	return err
}

// DeleteRouteTokens revokes every route token of the environment and
// returns how many there were.
func (s *Store) DeleteRouteTokens(ctx context.Context, envID uuid.UUID) (int64, error) {
	ct, err := s.q.Exec(ctx, "DELETE FROM env_route_tokens WHERE env_id = $1", envID)
	return ct.RowsAffected(), err
}
