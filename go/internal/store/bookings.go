// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// Booking is a row of bookings: a pod's size on a node, what it uses
// there. Bookings follow resource use; a pod that runs is booked even past
// the gates (Gated false).
type Booking struct {
	PodID     podid.ID
	Node      string
	VCPUs     int
	MemoryMiB int
	// DiskGiB is the pod's disk of its own size (Sandbox.DiskGiB), which
	// it books on the node's disk; 0 for the guest's.
	DiskGiB  int
	Gated    bool // false: booked because the pod uses the node, past the gates
	BookedAt time.Time
}

const bookingCols = "pod_id, node, vcpus, memory_mib, disk_gib, gated, booked_at"

func scanBooking(row pgx.Row) (Booking, error) {
	var b Booking
	err := row.Scan(&b.PodID, &b.Node, &b.VCPUs, &b.MemoryMiB, &b.DiskGiB, &b.Gated, &b.BookedAt)
	return b, err
}

// GetBooking reads a pod's booking; ErrNoRow if it has none.
func (s *Store) GetBooking(ctx context.Context, podID podid.ID) (Booking, error) {
	b, err := scanBooking(s.q.QueryRow(ctx, "SELECT "+bookingCols+" FROM bookings WHERE pod_id = $1", podID))
	return b, noRow(err, ErrNoRow)
}

// UpsertBooking books the pod on b.Node with its size (booked_at = now()),
// replacing a booking on another node; model.ErrNotFound when the sandbox
// does not exist.
func (s *Store) UpsertBooking(ctx context.Context, b Booking) error {
	_, err := s.q.Exec(ctx, `INSERT INTO bookings (pod_id, node, vcpus, memory_mib, disk_gib, gated)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (pod_id) DO UPDATE SET node = excluded.node, vcpus = excluded.vcpus,
			memory_mib = excluded.memory_mib, disk_gib = excluded.disk_gib, gated = excluded.gated, booked_at = now()`,
		b.PodID, b.Node, b.VCPUs, b.MemoryMiB, b.DiskGiB, b.Gated)
	if isForeignKeyViolation(err) {
		return model.ErrNotFound
	}
	return err
}

// DeleteBooking frees a pod's booking. It reports whether there was one.
func (s *Store) DeleteBooking(ctx context.Context, podID podid.ID) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM bookings WHERE pod_id = $1", podID)
	return tag.RowsAffected() > 0, err
}

// ListBookings returns the bookings on node, oldest first.
func (s *Store) ListBookings(ctx context.Context, node string) ([]Booking, error) {
	rows, err := s.q.Query(ctx, "SELECT "+bookingCols+" FROM bookings WHERE node = $1 ORDER BY booked_at, pod_id", node)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Booking, error) { return scanBooking(r) })
}

// BookingSums is what is booked on a node.
type BookingSums struct {
	Count     int64
	VCPUs     int64
	MemoryMiB int64
	// DiskGiB sums the disks of their own size.
	DiskGiB int64
}

// SumBookings sums the bookings on node, without the pod exclude (""
// excludes nothing): what an admission of exclude rechecks the gates
// against, under the node's row lock.
func (s *Store) SumBookings(ctx context.Context, node string, exclude podid.ID) (BookingSums, error) {
	var b BookingSums
	err := s.q.QueryRow(ctx, `SELECT count(*), coalesce(sum(vcpus), 0), coalesce(sum(memory_mib), 0),
		coalesce(sum(disk_gib), 0) FROM bookings WHERE node = $1 AND pod_id <> $2`, node, exclude).Scan(&b.Count,
		&b.VCPUs, &b.MemoryMiB, &b.DiskGiB)
	return b, err
}

// SumAllBookings sums the bookings of every node, without the pod exclude
// ("" excludes nothing).
func (s *Store) SumAllBookings(ctx context.Context, exclude podid.ID) (map[string]BookingSums, error) {
	rows, err := s.q.Query(ctx, `SELECT node, count(*), sum(vcpus), sum(memory_mib), sum(disk_gib) FROM bookings
		WHERE pod_id <> $1 GROUP BY node`, exclude)
	if err != nil {
		return nil, err
	}
	out := map[string]BookingSums{}
	var node string
	var b BookingSums
	_, err = pgx.ForEachRow(rows, []any{&node, &b.Count, &b.VCPUs, &b.MemoryMiB, &b.DiskGiB}, func() error {
		out[node] = b
		return nil
	})
	return out, err
}

// NodeSnapshot is a node as placement scores it for one pod: the row, its
// latest telemetry, what is booked there without the pod, and the
// pod-specific gates (a tombstone of the pod there, a TAP prefix taken).
type NodeSnapshot struct {
	Node Node
	// Telemetry is the zero value (HasTelemetry false) before the first
	// report.
	Telemetry    Telemetry
	HasTelemetry bool
	Booked       BookingSums
	// Tombstoned: a tombstone for this pod exists on the node.
	Tombstoned bool
	// TAPConflict is a sandbox on the node whose pod's TAP device has the
	// pod's name and may exist ("" if none): only a pod with a UUID has
	// one (ADR 0019).
	TAPConflict podid.ID
}

// PlacementSnapshot reads every node that is not removed, ordered by name,
// as placement of podID scores it, in one statement without locks.
func (s *Store) PlacementSnapshot(ctx context.Context, podID podid.ID) ([]NodeSnapshot, error) {
	rows, err := s.q.Query(ctx, "SELECT "+nodeColumns("n")+`,
			t.node IS NOT NULL, coalesce(t.last_seen, 'epoch'::timestamptz), coalesce(t.load1, 0),
			coalesce(t.memory_available_bytes, 0), coalesce(t.disk_available_bytes, 0),
			coalesce(t.inventory_hash, ''),
			coalesce(b.cnt, 0), coalesce(b.vcpus, 0), coalesce(b.mem, 0), coalesce(b.disk, 0),
			EXISTS (SELECT 1 FROM pod_tombstones pt WHERE pt.pod_id = $1::text AND pt.node = n.name),
			(SELECT s.id FROM sandboxes s WHERE s.node = n.name AND `+tapShares(1)+`
				AND `+tapHolds+` ORDER BY s.id LIMIT 1)
		FROM nodes n
		LEFT JOIN node_telemetry t ON t.node = n.name
		LEFT JOIN (SELECT node, count(*) AS cnt, sum(vcpus) AS vcpus, sum(memory_mib) AS mem, sum(disk_gib) AS disk
			FROM bookings WHERE pod_id <> $1::text GROUP BY node) b ON b.node = n.name
		WHERE n.status <> 'removed' ORDER BY n.name`, podID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (NodeSnapshot, error) {
		var ns NodeSnapshot
		targets, finish := nodeScanTargets(&ns.Node)
		var load1 float32
		var tap *podid.ID
		targets = append(targets, &ns.HasTelemetry, &ns.Telemetry.LastSeen, &load1,
			&ns.Telemetry.MemoryAvailableBytes, &ns.Telemetry.DiskAvailableBytes, &ns.Telemetry.InventoryHash,
			&ns.Booked.Count, &ns.Booked.VCPUs, &ns.Booked.MemoryMiB, &ns.Booked.DiskGiB, &ns.Tombstoned, &tap)
		if err := r.Scan(targets...); err != nil {
			return NodeSnapshot{}, err
		}
		finish()
		ns.Telemetry.Node = ns.Node.Name
		ns.Telemetry.Load1 = float64(load1)
		if !ns.HasTelemetry {
			ns.Telemetry = Telemetry{}
		}
		ns.TAPConflict = idOf(tap)
		return ns, nil
	})
}
