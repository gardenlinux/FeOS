// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/model"
)

// This file holds the typed queries of visits (ADR 0054, migration 0034):
// visits, ferund's record that a provider admitted a subject to an
// environment, and visit_answers, the challenges providers answered with
// the single redemption of an accepted one's code; and the cluster
// secrets the visits' keys are made in. Every time is the database's.

// Visit is a row of visits.
type Visit struct {
	ID, EnvID uuid.UUID
	// Tenant is the environment's owner; Provider the provider's name
	// (model.ProviderFerun for the builtin), ProviderKind its kind.
	Tenant, Provider, ProviderKind string
	Host, Subject, Email, Name     string
	// Session is the provider's session the visit hangs on; "": none.
	Session string
	// PersonID is the person whose grant admits a visit of kind ferun;
	// uuid.Nil otherwise.
	PersonID                   uuid.UUID
	CreatedAt, EndsAt, AskedAt time.Time
	// Binding is the hash of the visit cookie's secret; "" until the
	// code is redeemed.
	Binding string
	// EndedAt is zero while the visit is open; EndReason says why it
	// ended (model.Visit…).
	EndedAt   time.Time
	EndReason string
	// Now is the database's now() at the statement that read the row.
	Now time.Time
}

// Open reports whether the visit has not ended.
func (v Visit) Open() bool { return v.EndedAt.IsZero() }

const visitCols = `id, env_id, tenant, provider, provider_kind, host, subject, email, name, session, person_id,
	created_at, ends_at, asked_at, binding, ended_at, end_reason, now()`

func scanVisit(row pgx.Row) (Visit, error) {
	var v Visit
	var person *uuid.UUID
	var ended *time.Time
	if err := row.Scan(&v.ID, &v.EnvID, &v.Tenant, &v.Provider, &v.ProviderKind, &v.Host, &v.Subject, &v.Email,
		&v.Name, &v.Session, &person, &v.CreatedAt, &v.EndsAt, &v.AskedAt, &v.Binding, &ended, &v.EndReason,
		&v.Now); err != nil {
		return Visit{}, err
	}
	v.PersonID, v.EndedAt = uuidOf(person), timeOf(ended)
	return v, nil
}

// NewVisit is what an accepted challenge's visit is made of.
type NewVisit struct {
	ID, EnvID                            uuid.UUID
	Tenant, Provider, ProviderKind, Host string
	Subject, Email, Name, Session        string
	PersonID                             uuid.UUID
	// EndsAt is the provider's end; the visit ends at the earlier of it
	// and now() + MaxVisit (zero: now() + MaxVisit).
	EndsAt   time.Time
	MaxVisit time.Duration
}

// InsertVisit inserts a visit, open, asked now, without a binding.
func (s *Store) InsertVisit(ctx context.Context, n NewVisit) (Visit, error) {
	v, err := scanVisit(s.q.QueryRow(ctx, `INSERT INTO visits (id, env_id, tenant, provider, provider_kind, host,
			subject, email, name, session, person_id, ends_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
			least(coalesce($12::timestamptz, 'infinity'), now() + $13::interval))
		RETURNING `+visitCols, n.ID, n.EnvID, n.Tenant, n.Provider, n.ProviderKind, n.Host, n.Subject, n.Email, n.Name,
		n.Session, nullUUID(n.PersonID), nullTime(n.EndsAt), interval(n.MaxVisit)))
	if err != nil {
		return Visit{}, insertError("insert visit "+n.ID.String(), err)
	}
	return v, nil
}

// GetVisit reads a visit; ErrNoRow if there is none.
func (s *Store) GetVisit(ctx context.Context, id uuid.UUID) (Visit, error) {
	v, err := scanVisit(s.q.QueryRow(ctx, "SELECT "+visitCols+" FROM visits WHERE id = $1", id))
	return v, noRow(err, ErrNoRow)
}

// BindVisit sets the binding of an open visit (its code's redemption: a
// repeated one within the window replaces it) and returns the row;
// ErrNoRow when there is no open visit of that ID.
func (s *Store) BindVisit(ctx context.Context, id uuid.UUID, binding string) (Visit, error) {
	v, err := scanVisit(s.q.QueryRow(ctx, "UPDATE visits SET binding = $2 WHERE id = $1 AND ended_at IS NULL RETURNING "+
		visitCols, id, binding))
	return v, noRow(err, ErrNoRow)
}

// EndVisit ends an open visit with reason and reports whether it was
// open.
func (s *Store) EndVisit(ctx context.Context, id uuid.UUID, reason string) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE visits SET ended_at = now(), end_reason = $2 WHERE id = $1 AND ended_at IS NULL`,
		id, reason)
	if err != nil {
		return false, fmt.Errorf("end visit %s: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}

// ClaimVisitAsk claims the next ask of an open visit's provider: asked_at
// = now() when the last ask is at least every ago, so that of the
// renewals of one visit at once, and within every, one asks. It runs as
// one statement and holds no lock afterwards outside a transaction.
func (s *Store) ClaimVisitAsk(ctx context.Context, id uuid.UUID, every time.Duration) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE visits SET asked_at = now()
		WHERE id = $1 AND ended_at IS NULL AND asked_at <= now() - $2::interval`, id, interval(every))
	if err != nil {
		return false, fmt.Errorf("claim the ask of visit %s: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}

// EndEnvironmentVisits ends the open visits of an environment with reason
// and returns how many.
func (s *Store) EndEnvironmentVisits(ctx context.Context, env uuid.UUID, reason string) (int, error) {
	tag, err := s.q.Exec(ctx, `UPDATE visits SET ended_at = now(), end_reason = $2 WHERE env_id = $1 AND ended_at IS NULL`,
		env, reason)
	if err != nil {
		return 0, fmt.Errorf("end the visits of environment %s: %w", env, err)
	}
	return int(tag.RowsAffected()), nil
}

// EndProviderVisits ends the open visits of a tenant's provider (of kind
// tenant or oidc) with reason and returns how many.
func (s *Store) EndProviderVisits(ctx context.Context, tenant, provider, reason string) (int, error) {
	tag, err := s.q.Exec(ctx, `UPDATE visits SET ended_at = now(), end_reason = $3
		WHERE tenant = $1 AND provider = $2 AND provider_kind <> 'ferun' AND ended_at IS NULL`, tenant, provider, reason)
	if err != nil {
		return 0, fmt.Errorf("end the visits of provider %s of %s: %w", provider, tenant, err)
	}
	return int(tag.RowsAffected()), nil
}

// EndSessionVisits ends with reason the open visits of a provider's
// session and returns how many: of kind ferun, every tenant's (tenant and
// provider are ignored); of another kind, those of tenant's provider.
func (s *Store) EndSessionVisits(ctx context.Context, kind, tenant, provider, session, reason string) (int, error) {
	if session == "" {
		return 0, errors.New("end the visits of a session: no session")
	}
	var sql string
	args := []any{kind, session, reason}
	if kind == model.ProviderKindFerun {
		sql = `UPDATE visits SET ended_at = now(), end_reason = $3
			WHERE provider_kind = $1 AND session = $2 AND ended_at IS NULL`
	} else {
		sql = `UPDATE visits SET ended_at = now(), end_reason = $3
			WHERE provider_kind = $1 AND session = $2 AND ended_at IS NULL AND tenant = $4 AND provider = $5`
		args = append(args, tenant, provider)
	}
	tag, err := s.q.Exec(ctx, sql, args...)
	if err != nil {
		return 0, fmt.Errorf("end the visits of a session: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// VisitAnswer is a row of visit_answers: a challenge a provider answered.
type VisitAnswer struct {
	ChallengeID string
	EnvID       uuid.UUID
	Host        string
	// Outcome is visit.OutcomeAccepted or visit.OutcomeRejected; VisitID
	// is the accepted one's visit.
	Outcome string
	VisitID uuid.UUID
	// Binding is the challenge's binding.
	Binding string
	// CodeExpiresAt is the code's end, RedeemedAt the code's first
	// redemption (zero: none).
	CodeExpiresAt, RedeemedAt time.Time
	AnsweredBy                string
	CreatedAt                 time.Time
	// ExpiresAt is the challenge's end.
	ExpiresAt time.Time
	// Now is the database's now() at the statement that read the row.
	Now time.Time
}

const visitAnswerCols = `challenge_id, env_id, host, outcome, visit_id, binding, code_expires_at, redeemed_at,
	answered_by, created_at, expires_at, now()`

func scanVisitAnswer(row pgx.Row) (VisitAnswer, error) {
	var a VisitAnswer
	var visit *uuid.UUID
	var redeemed *time.Time
	if err := row.Scan(&a.ChallengeID, &a.EnvID, &a.Host, &a.Outcome, &visit, &a.Binding, &a.CodeExpiresAt, &redeemed,
		&a.AnsweredBy, &a.CreatedAt, &a.ExpiresAt, &a.Now); err != nil {
		return VisitAnswer{}, err
	}
	a.VisitID, a.RedeemedAt = uuidOf(visit), timeOf(redeemed)
	return a, nil
}

// NewVisitAnswer is a provider's answer to a challenge.
type NewVisitAnswer struct {
	ChallengeID string
	EnvID       uuid.UUID
	Host        string
	Outcome     string
	VisitID     uuid.UUID
	Binding     string
	AnsweredBy  string
	// CodeLifetime is how long the code lives from now().
	CodeLifetime time.Duration
	// ExpiresAt is the challenge's end.
	ExpiresAt time.Time
}

// InsertVisitAnswer records an answer to a challenge; one answered before
// is ErrExists.
func (s *Store) InsertVisitAnswer(ctx context.Context, n NewVisitAnswer) (VisitAnswer, error) {
	a, err := scanVisitAnswer(s.q.QueryRow(ctx, `INSERT INTO visit_answers (challenge_id, env_id, host, outcome,
			visit_id, binding, code_expires_at, answered_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, now() + $7::interval, $8, $9) RETURNING `+visitAnswerCols, n.ChallengeID, n.EnvID,
		n.Host, n.Outcome, nullUUID(n.VisitID), n.Binding, interval(n.CodeLifetime), n.AnsweredBy, n.ExpiresAt))
	if err != nil {
		return VisitAnswer{}, insertError("insert the answer of a challenge", err)
	}
	return a, nil
}

// GetVisitAnswer reads the answer of a challenge; ErrNoRow if there is
// none.
func (s *Store) GetVisitAnswer(ctx context.Context, challengeID string) (VisitAnswer, error) {
	a, err := scanVisitAnswer(s.q.QueryRow(ctx, "SELECT "+visitAnswerCols+" FROM visit_answers WHERE challenge_id = $1",
		challengeID))
	return a, noRow(err, ErrNoRow)
}

// RedeemVisitAnswer redeems an accepted challenge's code with the
// challenge's binding: the first redemption, or a repeated one within
// window of the first, while the code lives. It returns the visit, or
// ok false when the code is not to be redeemed (used, expired, another
// binding, rejected or unknown: GetVisitAnswer tells).
func (s *Store) RedeemVisitAnswer(ctx context.Context, challengeID, binding string, window time.Duration) (
	uuid.UUID, bool, error) {
	var visit uuid.UUID
	err := s.q.QueryRow(ctx, `UPDATE visit_answers SET redeemed_at = coalesce(redeemed_at, now())
		WHERE challenge_id = $1 AND outcome = 'accepted' AND binding = $2 AND code_expires_at > now()
			AND (redeemed_at IS NULL OR redeemed_at > now() - $3::interval)
		RETURNING visit_id`, challengeID, binding, interval(window)).Scan(&visit)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("redeem a code: %w", err)
	}
	return visit, true, nil
}

// VisitSweep is what SweepVisits did.
type VisitSweep struct {
	Ended, Deleted, Answers int64
}

// SweepVisits ends with reason the open visits past their end, at their
// end, deletes the visits that ended more than keep ago, and the answers
// of challenges that ended more than keepAnswers ago.
func (s *Store) SweepVisits(ctx context.Context, reason string, keep, keepAnswers time.Duration) (VisitSweep, error) {
	var out VisitSweep
	tag, err := s.q.Exec(ctx, `UPDATE visits SET ended_at = ends_at, end_reason = $1
		WHERE ended_at IS NULL AND ends_at <= now()`, reason)
	if err != nil {
		return out, fmt.Errorf("end expired visits: %w", err)
	}
	out.Ended = tag.RowsAffected()
	if tag, err = s.q.Exec(ctx, `DELETE FROM visits WHERE coalesce(ended_at, ends_at) < now() - $1::interval`,
		interval(keep)); err != nil {
		return out, fmt.Errorf("delete old visits: %w", err)
	}
	out.Deleted = tag.RowsAffected()
	if tag, err = s.q.Exec(ctx, `DELETE FROM visit_answers WHERE expires_at < now() - $1::interval`,
		interval(keepAnswers)); err != nil {
		return out, fmt.Errorf("delete old answers: %w", err)
	}
	out.Answers = tag.RowsAffected()
	return out, nil
}

// EnsureClusterSecret makes the cluster secret name with value unless it
// exists (INSERT … ON CONFLICT DO NOTHING) and returns the stored value,
// so that every instance that makes one at once ends with the winner's.
func (s *Store) EnsureClusterSecret(ctx context.Context, name string, value []byte) ([]byte, error) {
	if _, err := s.q.Exec(ctx, "INSERT INTO cluster_secrets (name, value) VALUES ($1, $2) ON CONFLICT DO NOTHING",
		name, value); err != nil {
		return nil, fmt.Errorf("make cluster secret %s: %w", name, err)
	}
	v, err := s.ClusterSecret(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("read cluster secret %s: %w", name, err)
	}
	return v, nil
}
