// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This file holds the typed queries of human sign-in (internal/api, §11
// "Human sign-in"): the sign-ins under way (auth_logins) and the UI
// sessions (ui_sessions), migration 0002.

// LoginTTL is how long a sign-in may take from its start to the callback.
const LoginTTL = 10 * time.Minute

// sessionSeenInterval is how often a session's last_seen_at is written at
// most.
const sessionSeenInterval = "1 minute"

// AuthLogin is a row of auth_logins: a sign-in under way.
type AuthLogin struct {
	StateHash string // sha256 hex of the state
	Verifier  string // the PKCE code verifier
	Nonce     string
	Next      string // the local path to return to
	CreatedAt time.Time
}

// InsertAuthLogin records a sign-in under way.
func (s *Store) InsertAuthLogin(ctx context.Context, l AuthLogin) error {
	_, err := s.q.Exec(ctx, `INSERT INTO auth_logins (state_hash, verifier, nonce, next) VALUES ($1, $2, $3, $4)`,
		l.StateHash, l.Verifier, l.Nonce, l.Next)
	return err
}

// TakeAuthLogin deletes and returns the sign-in of stateHash begun less
// than LoginTTL ago: a sign-in completes once. ErrNoRow when there is
// none (unknown, used or expired).
func (s *Store) TakeAuthLogin(ctx context.Context, stateHash string) (AuthLogin, error) {
	var l AuthLogin
	err := s.q.QueryRow(ctx, `DELETE FROM auth_logins WHERE state_hash = $1 AND created_at > now() - $2::interval
		RETURNING state_hash, verifier, nonce, next, created_at`, stateHash, interval(LoginTTL)).
		Scan(&l.StateHash, &l.Verifier, &l.Nonce, &l.Next, &l.CreatedAt)
	return l, noRow(err, ErrNoRow)
}

// DeleteStaleAuthLogins removes the sign-ins begun LoginTTL ago or
// earlier. It returns how many.
func (s *Store) DeleteStaleAuthLogins(ctx context.Context) (int64, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM auth_logins WHERE created_at <= now() - $1::interval", interval(LoginTTL))
	return tag.RowsAffected(), err
}

// UISession is a row of ui_sessions: a signed-in person.
type UISession struct {
	IDHash   string // sha256 hex of the session id
	Sub      string // the provider's stable user id
	Email    string
	Name     string
	Username string
	Picture  string
	Groups   []string
	// Admin marks an operator's session (--oidc-admin-group).
	Admin   bool
	IDToken string // the ID token, the sign-out's id_token_hint
	// PersonID is the person (people.id); uuid.Nil for a session of a
	// version before people (an operator's).
	PersonID uuid.UUID
	// SID is the provider's session (the ID token's sid), which a
	// back-channel logout names; "": none.
	SID        string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

// uiSessionColumns are the columns of a UISession, of the table alias p.
func uiSessionColumns(p string) string {
	return prefixed(p, []string{"id_hash", "sub", "email", "name", "username", "picture", "groups", "admin", "id_token",
		"person_id", "sid", "created_at", "expires_at", "last_seen_at"})
}

var uiSessionCols = uiSessionColumns("")

func scanUISession(row pgx.Row) (UISession, error) {
	var u UISession
	var person *uuid.UUID
	err := row.Scan(&u.IDHash, &u.Sub, &u.Email, &u.Name, &u.Username, &u.Picture, &u.Groups, &u.Admin, &u.IDToken,
		&person, &u.SID, &u.CreatedAt, &u.ExpiresAt, &u.LastSeenAt)
	u.Groups, u.PersonID = nonNil(u.Groups), uuidOf(person)
	return u, err
}

// InsertUISession stores a session that expires ttl from now() (the
// fields of u that the database sets are ignored) and returns it as
// stored.
func (s *Store) InsertUISession(ctx context.Context, u UISession, ttl time.Duration) (UISession, error) {
	return scanUISession(s.q.QueryRow(ctx, `INSERT INTO ui_sessions (id_hash, sub, email, name, username, picture, groups,
			admin, id_token, person_id, sid, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now() + $12::interval) RETURNING `+uiSessionCols,
		u.IDHash, u.Sub, u.Email, u.Name, u.Username, u.Picture, nonNil(u.Groups), u.Admin, u.IDToken,
		nullUUID(u.PersonID), u.SID, interval(ttl)))
}

// UISessionOf returns the session of idHash when it has not expired and
// its person, if it has one, is not disabled; ErrNoRow when there is none.
func (s *Store) UISessionOf(ctx context.Context, idHash string) (UISession, error) {
	u, err := scanUISession(s.q.QueryRow(ctx, "SELECT "+uiSessionColumns("s")+` FROM ui_sessions s
		LEFT JOIN people p ON p.id = s.person_id
		WHERE s.id_hash = $1 AND s.expires_at > now() AND p.disabled_at IS NULL`, idHash))
	return u, noRow(err, ErrNoRow)
}

// DeleteUISessionsBySID ends every session of the provider's session sid
// (a back-channel logout), and with sub only those of that subject. It
// returns how many.
func (s *Store) DeleteUISessionsBySID(ctx context.Context, sid, sub string) (int64, error) {
	if sid == "" {
		return 0, nil
	}
	tag, err := s.q.Exec(ctx, "DELETE FROM ui_sessions WHERE sid = $1 AND ($2 = '' OR sub = $2)", sid, sub)
	return tag.RowsAffected(), err
}

// TouchUISession sets a session's last_seen_at = now(), at most once a
// minute.
func (s *Store) TouchUISession(ctx context.Context, idHash string) error {
	_, err := s.q.Exec(ctx, `UPDATE ui_sessions SET last_seen_at = now() WHERE id_hash = $1
		AND last_seen_at < now() - interval '`+sessionSeenInterval+`'`, idHash)
	return err
}

// DeleteUISession removes a session (a sign-out) and returns it. ErrNoRow
// when there was none.
func (s *Store) DeleteUISession(ctx context.Context, idHash string) (UISession, error) {
	u, err := scanUISession(s.q.QueryRow(ctx, "DELETE FROM ui_sessions WHERE id_hash = $1 RETURNING "+uiSessionCols, idHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return UISession{}, ErrNoRow
	}
	return u, err
}

// DeleteExpiredUISessions removes the sessions past their expiry. It
// returns how many.
func (s *Store) DeleteExpiredUISessions(ctx context.Context) (int64, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM ui_sessions WHERE expires_at <= now()")
	return tag.RowsAffected(), err
}
