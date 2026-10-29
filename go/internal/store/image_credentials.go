// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// ImageCredentialsAAD is the additional data of a sandbox's sealed image
// credentials: "sandbox_image_credentials|<sandbox id>". A blob copied to
// another sandbox does not open.
func ImageCredentialsAAD(id podid.ID) []byte {
	return []byte("sandbox_image_credentials|" + id.String())
}

// SetImageCredentials keeps the sandbox's sealed image credentials,
// replacing any (ADR 0025).
func (s *Store) SetImageCredentials(ctx context.Context, id podid.ID, sealed []byte) error {
	if _, err := s.q.Exec(ctx, `INSERT INTO sandbox_image_credentials (sandbox_id, sealed) VALUES ($1, $2)
		ON CONFLICT (sandbox_id) DO UPDATE SET sealed = excluded.sealed, updated_at = now()`, id, sealed); err != nil {
		return fmt.Errorf("keep the image credentials of %s: %w", id, err)
	}
	return nil
}

// DeleteImageCredentials removes the sandbox's image credentials; it
// reports whether it had any.
func (s *Store) DeleteImageCredentials(ctx context.Context, id podid.ID) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM sandbox_image_credentials WHERE sandbox_id = $1", id)
	if err != nil {
		return false, fmt.Errorf("delete the image credentials of %s: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ImageCredentials returns the sandbox's sealed image credentials; nil
// when it has none.
func (s *Store) ImageCredentials(ctx context.Context, id podid.ID) ([]byte, error) {
	var sealed []byte
	err := s.q.QueryRow(ctx, "SELECT sealed FROM sandbox_image_credentials WHERE sandbox_id = $1", id).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the image credentials of %s: %w", id, err)
	}
	return sealed, nil
}
