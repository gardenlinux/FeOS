// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gardenlinux/feos/go/internal/db"
)

// This file and the other env_*.go files hold the typed access to the
// environments' tables (migration 0006, ADR 0018): the environments, their
// deployments, tags, sealed secrets, the controller's view of instances, job
// runs, attached services, addon volumes, incidents and route tokens. They
// are the basic reads and writes; the service and the controller keep the
// queries of their own domain in files of their own. No writer here sends
// a notification, except ferun_ingress, which the writes that the
// ingress's table follows send themselves (ADR 0021): an environment's
// publicity, deployments and deletion, and its service instances. The callers
// send ferun_leader "env-reconcile" or "env-groups" in their transaction
// when the controller is to act.

// ErrExists is returned by an insert whose primary or unique key another
// row of the table holds.
var ErrExists = errors.New("the row exists")

// ErrRowChanged is returned by UpdateEnvironment when the row's gen is no
// longer the one the caller read.
var ErrRowChanged = errors.New("the row changed since it was read")

// isUniqueViolation reports a write of a key another row holds (SQLSTATE
// 23505).
func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// insertError maps an insert's error: a held key is ErrExists, a row it
// names that does not exist (a foreign key) ErrNoRow.
func insertError(what string, err error) error {
	switch {
	case isUniqueViolation(err):
		return fmt.Errorf("%s: %w", what, ErrExists)
	case isForeignKeyViolation(err):
		return fmt.Errorf("%s: %w", what, ErrNoRow)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// jsonText is v as JSON text for a "$n::jsonb" parameter.
func jsonText(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// stringMap returns m, or an empty map for nil: the jsonb maps are NOT
// NULL and '{}' when empty.
func stringMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// nullInt is a nullable integer column's argument.
func nullInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// updates collects the columns an Update of an env table writes: the
// key's arguments first, then one "col = $n" per changed column.
type updates struct {
	args []any
	sets []string
}

func newUpdates(keys ...any) *updates { return &updates{args: keys} }

// set adds col = v when changed.
func (u *updates) set(col string, changed bool, v any) {
	if !changed {
		return
	}
	u.args = append(u.args, v)
	u.sets = append(u.sets, fmt.Sprintf("%s = $%d", col, len(u.args)))
}

// setJSON adds col = v as jsonb when changed.
func (u *updates) setJSON(col string, changed bool, v any) error {
	if !changed {
		return nil
	}
	j, err := jsonText(v)
	if err != nil {
		return fmt.Errorf("%s: %w", col, err)
	}
	u.args = append(u.args, j)
	u.sets = append(u.sets, fmt.Sprintf("%s = $%d::jsonb", col, len(u.args)))
	return nil
}

func (u *updates) empty() bool { return len(u.sets) == 0 }

// clause is the SET list, with updated_at = now() when the table has it.
func (u *updates) clause(updatedAt bool) string {
	sets := u.sets
	if updatedAt {
		sets = append(sets, "updated_at = now()")
	}
	return strings.Join(sets, ", ")
}

// fixed refuses an Update that changes a column it never writes.
func fixed(what string, cols map[string]bool) error {
	for c, changed := range cols {
		if changed {
			return fmt.Errorf("update %s: column %s is not written", what, c)
		}
	}
	return nil
}

// Environment is a row of environments.
type Environment struct {
	ID     uuid.UUID
	Owner  string // the tenant
	Name   string // a DNS label, never a UUID
	Labels map[string]string
	// Deployment is the latest desired deployment, Deployed the latest deployed
	// one; 0: none.
	Deployment, Deployed int
	Deleting             bool
	// Gen is the version of the env security group (env-<id>):
	// BumpEnvironmentGen when its rules or members change.
	Gen                  int64
	CreatedAt, UpdatedAt time.Time
	// Public: the ingress serves it at its default host, without a route
	// token (its tenant's decision, SetEnvironmentPublic). IngressGroup:
	// its image services' pods name ing-<sandbox id>; set at the first
	// publish, never cleared. HostLabel is its default host's label under
	// the apps zone, given at the first publish and changed only when its
	// tenant chooses one (SetEnvironmentHostLabel); "": never published.
	Public, IngressGroup bool
	HostLabel            string
}

// environmentColumns are the columns of an Environment, each prefixed with
// p and a dot when p is not empty.
func environmentColumns(p string) string {
	cols := prefixed(p, []string{"id", "owner", "name", "labels", "deployment", "deployed", "deleting", "gen",
		"created_at", "updated_at", "public", "ingress_group"})
	if p != "" {
		p += "."
	}
	return cols + ", coalesce(" + p + "host_label, '')"
}

var environmentCols = environmentColumns("")

// environmentTargets are the scan targets of environmentColumns for e, and
// the function that finishes the scan.
func environmentTargets(e *Environment) ([]any, func()) {
	return []any{&e.ID, &e.Owner, &e.Name, &e.Labels, &e.Deployment, &e.Deployed, &e.Deleting, &e.Gen,
			&e.CreatedAt, &e.UpdatedAt, &e.Public, &e.IngressGroup, &e.HostLabel}, func() {
			e.Labels = stringMap(e.Labels)
		}
}

func scanEnvironment(row pgx.Row) (Environment, error) {
	var e Environment
	targets, finish := environmentTargets(&e)
	if err := row.Scan(targets...); err != nil {
		return Environment{}, err
	}
	finish()
	return e, nil
}

func collectEnvironments(rows pgx.Rows, err error) ([]Environment, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Environment, error) { return scanEnvironment(r) })
}

// NewEnvironment is what an environment's create sets.
type NewEnvironment struct {
	ID     uuid.UUID
	Owner  string
	Name   string
	Labels map[string]string
}

// InsertEnvironment inserts an environment (the caller holds its tenant
// FOR SHARE and refuses a deleting one). An ID or (owner, name) that
// another row holds is ErrExists, an unknown owner ErrNoRow.
func (s *Store) InsertEnvironment(ctx context.Context, n NewEnvironment) (Environment, error) {
	labels, err := jsonText(stringMap(n.Labels))
	if err != nil {
		return Environment{}, fmt.Errorf("insert environment %s: %w", n.Name, err)
	}
	e, err := scanEnvironment(s.q.QueryRow(ctx, `WITH e AS (INSERT INTO environments (id, owner, name, labels)
			VALUES ($1, $2, $3, $4::jsonb) RETURNING *)
		SELECT `+environmentCols+` FROM e`, n.ID, n.Owner, n.Name, labels))
	if err != nil {
		return Environment{}, insertError("insert environment "+n.Name, err)
	}
	return e, nil
}

// GetEnvironment reads an environment; ErrNoRow if there is none.
func (s *Store) GetEnvironment(ctx context.Context, id uuid.UUID) (Environment, error) {
	return s.LockEnvironment(ctx, id, NoLock)
}

// LockEnvironment reads an environment and locks its row with lock;
// ErrNoRow if there is none.
func (s *Store) LockEnvironment(ctx context.Context, id uuid.UUID, lock RowLock) (Environment, error) {
	e, err := scanEnvironment(s.q.QueryRow(ctx, "SELECT "+environmentCols+" FROM environments WHERE id = $1 "+string(lock), id))
	if err != nil {
		return Environment{}, noRow(err, ErrNoRow)
	}
	return e, nil
}

// LockEnvironmentByName reads the environment of owner named name and
// locks its row with lock; ErrNoRow if there is none.
func (s *Store) LockEnvironmentByName(ctx context.Context, owner, name string, lock RowLock) (Environment, error) {
	e, err := scanEnvironment(s.q.QueryRow(ctx, "SELECT "+environmentCols+` FROM environments
		WHERE owner = $1 AND name = $2 `+string(lock), owner, name))
	if err != nil {
		return Environment{}, noRow(err, ErrNoRow)
	}
	return e, nil
}

// ListEnvironments returns the environments of owner (every one for "")
// that carry every label of labels, oldest first.
func (s *Store) ListEnvironments(ctx context.Context, owner string, labels map[string]string) ([]Environment, error) {
	want, err := jsonText(stringMap(labels))
	if err != nil {
		return nil, err
	}
	return collectEnvironments(s.q.Query(ctx, "SELECT "+environmentCols+` FROM environments
		WHERE ($1 = '' OR owner = $1) AND labels @> $2::jsonb ORDER BY created_at, id`, owner, want))
}

// DeletingEnvironments returns up to limit environments being deleted,
// the least recently written (or touched, EnvCtlTouchEnvironment) first,
// so that those whose deletion waits never hold the others back.
func (s *Store) DeletingEnvironments(ctx context.Context, limit int) ([]Environment, error) {
	return collectEnvironments(s.q.Query(ctx, "SELECT "+environmentCols+` FROM environments
		WHERE deleting ORDER BY updated_at, id LIMIT $1`, limit))
}

// UpdateEnvironment writes the columns in which after differs from before,
// the row as the caller read it (normally locked FOR UPDATE in the same
// transaction): labels, deployment, deployed, deleting and gen; updated_at =
// now(). id, owner, name, created_at, public, ingress_group and host_label
// are never written (RenameEnvironment writes the name,
// SetEnvironmentPublic the last three). A row whose gen is no longer
// before's is not written (ErrRowChanged); when nothing differs, before is
// returned unwritten. A write of deployment, deployed or deleting sends
// ferun_ingress "env:<id>".
func (s *Store) UpdateEnvironment(ctx context.Context, before, after Environment) (Environment, error) {
	what := "environment " + before.ID.String()
	if err := fixed(what, map[string]bool{"id": before.ID != after.ID, "owner": before.Owner != after.Owner,
		"name": before.Name != after.Name, "created_at": !before.CreatedAt.Equal(after.CreatedAt),
		"public": before.Public != after.Public, "ingress_group": before.IngressGroup != after.IngressGroup,
		"host_label": before.HostLabel != after.HostLabel}); err != nil {
		return Environment{}, err
	}
	u := newUpdates(before.ID, before.Gen)
	if err := u.setJSON("labels", !maps.Equal(before.Labels, after.Labels), stringMap(after.Labels)); err != nil {
		return Environment{}, err
	}
	u.set("deployment", before.Deployment != after.Deployment, after.Deployment)
	u.set("deployed", before.Deployed != after.Deployed, after.Deployed)
	u.set("deleting", before.Deleting != after.Deleting, after.Deleting)
	u.set("gen", before.Gen != after.Gen, after.Gen)
	if u.empty() {
		return before, nil
	}
	e, err := scanEnvironment(s.q.QueryRow(ctx, "UPDATE environments SET "+u.clause(true)+
		" WHERE id = $1 AND gen = $2 RETURNING "+environmentCols, u.args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Environment{}, fmt.Errorf("update %s: %w", what, ErrRowChanged)
	}
	if err != nil {
		return Environment{}, fmt.Errorf("update %s: %w", what, err)
	}
	if before.Deployment != after.Deployment || before.Deployed != after.Deployed || before.Deleting != after.Deleting {
		if err := s.notifyIngressEnvs(ctx, []uuid.UUID{e.ID}); err != nil {
			return Environment{}, fmt.Errorf("update %s: %w", what, err)
		}
	}
	return e, nil
}

// RenameEnvironment gives the environment the name name and returns its
// row (ADR 0037): the caller holds the row FOR UPDATE and checked the
// name. A name another environment of its owner holds is ErrExists, an
// unknown ID ErrNoRow. Nothing else of the row changes: its host label,
// claimed from the name it had at its first publish, stays. It sends
// ferun_ingress "env:<id>", so that the ingress's table names it anew.
func (s *Store) RenameEnvironment(ctx context.Context, id uuid.UUID, name string) (Environment, error) {
	e, err := scanEnvironment(s.q.QueryRow(ctx, `UPDATE environments SET name = $2, updated_at = now()
		WHERE id = $1 RETURNING `+environmentCols, id, name))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Environment{}, fmt.Errorf("rename environment %s: %w", id, ErrNoRow)
	case isUniqueViolation(err):
		return Environment{}, fmt.Errorf("rename environment %s to %s: %w", id, name, ErrExists)
	case err != nil:
		return Environment{}, fmt.Errorf("rename environment %s: %w", id, err)
	}
	if err := s.notifyIngressEnvs(ctx, []uuid.UUID{id}); err != nil {
		return Environment{}, fmt.Errorf("rename environment %s: %w", id, err)
	}
	return e, nil
}

// SetEnvironmentPublic makes the environment public or not and returns
// its row; ErrNoRow if there is none. It runs in the caller's transaction
// (in one of its own on a Store outside one), with the row locked FOR
// UPDATE. The first publish also claims the environment's default host
// label (ClaimHostLabel) and sets ingress_group; unpublishing keeps both,
// so that publishing again names the same host and rolls no pod. A write
// sends ferun_ingress "env:<id>"; a call that changes nothing writes
// nothing. The caller refuses a deleting environment or tenant and sends
// the controller's hints.
func (s *Store) SetEnvironmentPublic(ctx context.Context, id uuid.UUID, public bool) (Environment, error) {
	var out Environment
	err := s.InTx(ctx, func(tx *Store) error {
		e, err := tx.LockEnvironment(ctx, id, ForUpdate)
		if err != nil {
			return err
		}
		if e.Public == public {
			out = e
			return nil
		}
		label := e.HostLabel
		if public && label == "" {
			if label, err = tx.ClaimHostLabel(ctx, e); err != nil {
				return err
			}
		}
		if out, err = scanEnvironment(tx.q.QueryRow(ctx, `UPDATE environments SET public = $2,
				ingress_group = ingress_group OR $2, host_label = coalesce(host_label, $3), updated_at = now()
			WHERE id = $1 RETURNING `+environmentCols, id, public, nullString(label))); err != nil {
			return err
		}
		return tx.notifyIngressEnvs(ctx, []uuid.UUID{id})
	})
	if err != nil {
		return Environment{}, fmt.Errorf("set environment %s public %v: %w", id, public, err)
	}
	return out, nil
}

// BumpEnvironmentGen increments the version of the environment's security
// group and returns it; ErrNoRow if there is no such environment.
func (s *Store) BumpEnvironmentGen(ctx context.Context, id uuid.UUID) (int64, error) {
	var gen int64
	err := s.q.QueryRow(ctx, "UPDATE environments SET gen = gen + 1, updated_at = now() WHERE id = $1 RETURNING gen", id).Scan(&gen)
	return gen, noRow(err, ErrNoRow)
}

// MarkTenantEnvironmentsDeleting marks the environments of owner deleting
// (a deleting tenant's) and returns those it marked, each with
// ferun_ingress "env:<id>".
func (s *Store) MarkTenantEnvironmentsDeleting(ctx context.Context, owner string) ([]uuid.UUID, error) {
	rows, err := s.q.Query(ctx, `UPDATE environments SET deleting = true, updated_at = now()
		WHERE owner = $1 AND NOT deleting RETURNING id`, owner)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	if err := s.notifyIngressEnvs(ctx, ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// DeleteEnvironment removes the environment row; its deployments, tags,
// secrets, job runs, attached services, incidents and route tokens
// cascade, and its host label becomes free for its tenant
// (host_labels.env_id ON DELETE SET NULL). It fails while an instance
// row, an env_instances row or a volume names it. It reports whether a row
// was there, and sends ferun_ingress "env:<id>" when one was.
func (s *Store) DeleteEnvironment(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM environments WHERE id = $1", id)
	if err != nil {
		return false, fmt.Errorf("delete environment %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := s.notifyIngressEnvs(ctx, []uuid.UUID{id}); err != nil {
		return false, fmt.Errorf("delete environment %s: %w", id, err)
	}
	return true, nil
}

// notifyIngressEnvs sends ferun_ingress "env:<id>" for each id, or "*" for
// more than notifyBatch ids.
func (s *Store) notifyIngressEnvs(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > notifyBatch {
		return s.Notify(ctx, db.ChanIngress, IngressPayloadAll)
	}
	for _, id := range ids {
		if err := s.Notify(ctx, db.ChanIngress, IngressEnvPayload(id)); err != nil {
			return err
		}
	}
	return nil
}

// CountEnvironmentsOfOwner counts a tenant's environments.
func (s *Store) CountEnvironmentsOfOwner(ctx context.Context, owner string) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx, "SELECT count(*) FROM environments WHERE owner = $1", owner).Scan(&n)
	return n, err
}
