// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ImageBake is a row of image_bakes: a layer that holds Image, baked by a
// pod on Node with the guest GuestVersion over its blank data volume's
// layer L0 (ADR 0025).
type ImageBake struct {
	Image, GuestVersion, L0, Layer, Node string
}

// maxImageBakes bounds the layers a create names for its image.
const maxImageBakes = 16

// RecordImageBake records a bake a node reported, once: a report seen
// again changes nothing. It reports whether the row is new.
func (s *Store) RecordImageBake(ctx context.Context, b ImageBake) (bool, error) {
	tag, err := s.q.Exec(ctx, `INSERT INTO image_bakes (image, guest_version, l0, layer, node)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`, b.Image, b.GuestVersion, b.L0, b.Layer, b.Node)
	if err != nil {
		return false, fmt.Errorf("record the bake of %s: %w", b.Image, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ImageBakes returns the bakes of image, the newest first, at most 16.
func (s *Store) ImageBakes(ctx context.Context, image string) ([]ImageBake, error) {
	rows, err := s.q.Query(ctx, `SELECT image, guest_version, l0, layer, node FROM image_bakes
		WHERE image = $1 ORDER BY baked_at DESC, layer LIMIT $2`, image, maxImageBakes)
	if err != nil {
		return nil, fmt.Errorf("the bakes of %s: %w", image, err)
	}
	bakes, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ImageBake, error) {
		var b ImageBake
		err := r.Scan(&b.Image, &b.GuestVersion, &b.L0, &b.Layer, &b.Node)
		return b, err
	})
	if err != nil {
		return nil, fmt.Errorf("the bakes of %s: %w", image, err)
	}
	return bakes, nil
}
