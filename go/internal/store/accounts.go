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
)

// This file holds the typed queries of accounts (ADR 0059, migration
// 0043): the accounts, their people, a project's accounts over time and
// its moves. A project without a current row in project_accounts (made by
// a release before, or by a test by SQL) is on DefaultAccount: every read
// says so, and a move writes that row first.

// DefaultAccount is the account every installation has: the one a project
// is on when nobody names another. It is never closed or deleted and holds
// no admin rows; its admins are the operators.
const DefaultAccount = "default"

// AccountNamePattern is the rule for account names (accounts.id), a
// project's.
var AccountNamePattern = ProjectNamePattern

// The roles on an account.
const (
	AccountMember = "member"
	AccountAdmin  = "admin"
)

// Account is a row of accounts: whom the operator charges.
type Account struct {
	ID    string // its name
	Title string
	// Reference is the operator's accounting's: a cost centre, an order, a
	// customer's number. FeRun reads nothing into it.
	Reference string
	CreatedAt time.Time
	MadeBy    string
	ClosedAt  time.Time // zero: open
	ClosedBy  string
}

// Closed reports whether the account takes no project.
func (a Account) Closed() bool { return !a.ClosedAt.IsZero() }

const accountCols = "id, title, reference, created_at, made_by, closed_at, closed_by"

func accountTargets(a *Account, closed **time.Time) []any {
	return []any{&a.ID, &a.Title, &a.Reference, &a.CreatedAt, &a.MadeBy, closed, &a.ClosedBy}
}

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	var closed *time.Time
	if err := row.Scan(accountTargets(&a, &closed)...); err != nil {
		return Account{}, err
	}
	a.ClosedAt = timeOf(closed)
	return a, nil
}

// CreateAccount inserts an account. It returns the new row and true, or
// the existing row and false when the name is taken.
func (s *Store) CreateAccount(ctx context.Context, a Account) (Account, bool, error) {
	row, err := scanAccount(s.q.QueryRow(ctx, `INSERT INTO accounts (id, title, reference, made_by)
		VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO NOTHING RETURNING `+accountCols, a.ID, a.Title, a.Reference, a.MadeBy))
	if err == nil {
		return row, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Account{}, false, err
	}
	row, err = s.GetAccount(ctx, a.ID)
	return row, false, err
}

// GetAccount reads an account; ErrNoRow if there is none.
func (s *Store) GetAccount(ctx context.Context, id string) (Account, error) {
	return s.LockAccount(ctx, id, NoLock)
}

// LockAccount reads an account and locks its row; ErrNoRow if there is
// none. A project's creation and its move hold the account that takes it
// FOR SHARE, so that it does not close in between.
func (s *Store) LockAccount(ctx context.Context, id string, lock RowLock) (Account, error) {
	a, err := scanAccount(s.q.QueryRow(ctx, "SELECT "+accountCols+" FROM accounts WHERE id = $1 "+string(lock), id))
	return a, noRow(err, ErrNoRow)
}

// AccountChange is what PatchAccount changes; nil: unchanged.
type AccountChange struct {
	Title, Reference *string
	// Closed closes (true) or reopens (false) the account; closing one
	// closed already keeps its moment.
	Closed *bool
}

// PatchAccount changes an account, by: its title, its reference, whether
// it is closed. ErrNoRow if there is none.
func (s *Store) PatchAccount(ctx context.Context, id string, c AccountChange, by string) (Account, error) {
	a, err := scanAccount(s.q.QueryRow(ctx, `UPDATE accounts SET title = coalesce($2, title),
			reference = coalesce($3, reference),
			closed_at = CASE WHEN $4::boolean IS NULL THEN closed_at WHEN $4 THEN coalesce(closed_at, now()) END,
			closed_by = CASE WHEN $4::boolean IS NULL THEN closed_by WHEN $4 AND closed_at IS NULL THEN $5
				WHEN $4 THEN closed_by ELSE '' END
		WHERE id = $1 RETURNING `+accountCols, id, c.Title, c.Reference, c.Closed, by))
	return a, noRow(err, ErrNoRow)
}

// DeleteAccount removes an account that no project is or was on, with its
// people and invitations. It reports whether there was one; one a
// project's row names is ErrInUse.
func (s *Store) DeleteAccount(ctx context.Context, id string) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM accounts WHERE id = $1", id)
	if isForeignKeyViolation(err) {
		return false, fmt.Errorf("delete account %s: %w", id, ErrInUse)
	}
	return tag.RowsAffected() > 0, err
}

// AccountInfo is an account with what the operators' list shows of it.
type AccountInfo struct {
	Account
	// Projects is the projects on it now; Members its people.
	Projects, Members int64
}

// ListAccounts returns every account by name, with the number of projects
// on it now and of its people.
func (s *Store) ListAccounts(ctx context.Context) ([]AccountInfo, error) {
	return s.accountInfos(ctx, "", nil)
}

// GetAccountInfo is one account of ListAccounts; ErrNoRow if there is
// none.
func (s *Store) GetAccountInfo(ctx context.Context, id string) (AccountInfo, error) {
	all, err := s.accountInfos(ctx, "WHERE a.id = $1", []any{id})
	if err != nil {
		return AccountInfo{}, err
	}
	if len(all) == 0 {
		return AccountInfo{}, ErrNoRow
	}
	return all[0], nil
}

// accountInfos is the accounts that where (with args) selects, by name,
// with their counts.
func (s *Store) accountInfos(ctx context.Context, where string, args []any) ([]AccountInfo, error) {
	rows, err := s.q.Query(ctx, `SELECT `+prefixed("a", []string{"id", "title", "reference", "created_at", "made_by",
		"closed_at", "closed_by"})+`,
			(SELECT count(*) FROM projects p LEFT JOIN project_accounts c ON c.project_id = p.id AND c.left_at IS NULL
				WHERE coalesce(c.account_id, '`+DefaultAccount+`') = a.id),
			(SELECT count(*) FROM account_members m WHERE m.account_id = a.id)
		FROM accounts a `+where+` ORDER BY a.id`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AccountInfo, error) {
		var ai AccountInfo
		var closed *time.Time
		err := r.Scan(append(accountTargets(&ai.Account, &closed), &ai.Projects, &ai.Members)...)
		ai.ClosedAt = timeOf(closed)
		return ai, err
	})
}

// AllAccounts returns every account by name, without counts.
func (s *Store) AllAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.q.Query(ctx, "SELECT "+accountCols+" FROM accounts ORDER BY id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Account, error) { return scanAccount(r) })
}

// PersonAccount is an account a person holds a row of, with its role.
type PersonAccount struct {
	Account
	Role string
}

// PersonAccounts returns the accounts a person holds a row of, by name.
func (s *Store) PersonAccounts(ctx context.Context, person uuid.UUID) ([]PersonAccount, error) {
	rows, err := s.q.Query(ctx, `SELECT `+prefixed("a", []string{"id", "title", "reference", "created_at", "made_by",
		"closed_at", "closed_by"})+`, m.role
		FROM account_members m JOIN accounts a ON a.id = m.account_id WHERE m.person_id = $1 ORDER BY a.id`, person)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PersonAccount, error) {
		var pa PersonAccount
		var closed *time.Time
		err := r.Scan(append(accountTargets(&pa.Account, &closed), &pa.Role)...)
		pa.ClosedAt = timeOf(closed)
		return pa, err
	})
}

// AccountRole is a person's role on an account by their row: "member",
// "admin", or "" without one or when they are disabled. Operators and the
// open default are the caller's to decide.
func (s *Store) AccountRole(ctx context.Context, account string, person uuid.UUID) (string, error) {
	var role string
	err := s.q.QueryRow(ctx, `SELECT m.role FROM account_members m JOIN people p ON p.id = m.person_id
		WHERE m.account_id = $1 AND m.person_id = $2 AND p.disabled_at IS NULL`, account, person).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// AccountMemberRole is a person's row's role on an account, disabled or
// not; "" without one.
func (s *Store) AccountMemberRole(ctx context.Context, account string, person uuid.UUID) (string, error) {
	var role string
	err := s.q.QueryRow(ctx, "SELECT role FROM account_members WHERE account_id = $1 AND person_id = $2",
		account, person).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// AccountPerson is one of an account's people.
type AccountPerson struct {
	Person
	Role      string
	MadeBy    string
	UpdatedAt time.Time
}

// ListAccountMembers returns an account's people by name and e-mail
// address.
func (s *Store) ListAccountMembers(ctx context.Context, account string) ([]AccountPerson, error) {
	rows, err := s.q.Query(ctx, `SELECT `+prefixed("p", []string{"id", "issuer", "sub", "email", "name", "first_seen",
		"last_seen", "disabled_at"})+`, m.role, m.made_by, m.updated_at
		FROM account_members m JOIN people p ON p.id = m.person_id WHERE m.account_id = $1
		ORDER BY lower(p.name), lower(p.email), p.id`, account)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AccountPerson, error) {
		var m AccountPerson
		var disabled *time.Time
		err := r.Scan(&m.ID, &m.Issuer, &m.Sub, &m.Email, &m.Name, &m.FirstSeen, &m.LastSeen, &disabled, &m.Role,
			&m.MadeBy, &m.UpdatedAt)
		m.DisabledAt = timeOf(disabled)
		return m, err
	})
}

// PutAccountMember gives a person the role on an account, by: a new row,
// or the existing one changed. ErrNoRow when the account or the person
// does not exist.
func (s *Store) PutAccountMember(ctx context.Context, account string, person uuid.UUID, role, by string) error {
	_, err := s.q.Exec(ctx, `INSERT INTO account_members (account_id, person_id, role, made_by) VALUES ($1, $2, $3, $4)
		ON CONFLICT (account_id, person_id) DO UPDATE SET role = excluded.role, made_by = excluded.made_by,
			updated_at = now()`, account, person, role, by)
	if isForeignKeyViolation(err) {
		return ErrNoRow
	}
	return err
}

// DeleteAccountMember removes a person from an account. It reports
// whether they were on it.
func (s *Store) DeleteAccountMember(ctx context.Context, account string, person uuid.UUID) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM account_members WHERE account_id = $1 AND person_id = $2", account, person)
	return tag.RowsAffected() > 0, err
}

// CountAccountAdmins counts an account's admins who are not disabled: an
// account that has one keeps one.
func (s *Store) CountAccountAdmins(ctx context.Context, account string) (int, error) {
	var n int
	err := s.q.QueryRow(ctx, `SELECT count(*) FROM account_members m JOIN people p ON p.id = m.person_id
		WHERE m.account_id = $1 AND m.role = 'admin' AND p.disabled_at IS NULL`, account).Scan(&n)
	return n, err
}

// ProjectAccount is the account a project is on now and since when; a
// project without a current row is on DefaultAccount since it was made.
// ErrNoRow if the project does not exist.
func (s *Store) ProjectAccount(ctx context.Context, project string) (Account, time.Time, error) {
	var a Account
	var closed *time.Time
	var since time.Time
	err := s.q.QueryRow(ctx, `SELECT `+prefixed("a", []string{"id", "title", "reference", "created_at", "made_by",
		"closed_at", "closed_by"})+`, coalesce(c.moved_at, p.created_at)
		FROM projects p LEFT JOIN project_accounts c ON c.project_id = p.id AND c.left_at IS NULL
		JOIN accounts a ON a.id = coalesce(c.account_id, '`+DefaultAccount+`')
		WHERE p.id = $1`, project).Scan(append(accountTargets(&a, &closed), &since)...)
	if err != nil {
		return Account{}, time.Time{}, noRow(err, ErrNoRow)
	}
	a.ClosedAt = timeOf(closed)
	return a, since, nil
}

// ProjectAccounts returns the account each named project is on now, by
// project; a project that does not exist is left out.
func (s *Store) ProjectAccounts(ctx context.Context, projects []string) (map[string]Account, error) {
	rows, err := s.q.Query(ctx, `SELECT p.id, `+prefixed("a", []string{"id", "title", "reference", "created_at",
		"made_by", "closed_at", "closed_by"})+`
		FROM projects p LEFT JOIN project_accounts c ON c.project_id = p.id AND c.left_at IS NULL
		JOIN accounts a ON a.id = coalesce(c.account_id, '`+DefaultAccount+`')
		WHERE p.id = ANY($1)`, projects)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Account{}
	for rows.Next() {
		var p string
		var a Account
		var closed *time.Time
		if err := rows.Scan(append([]any{&p}, accountTargets(&a, &closed)...)...); err != nil {
			return nil, err
		}
		a.ClosedAt = timeOf(closed)
		out[p] = a
	}
	return out, rows.Err()
}

// AccountMove is one row of a project's accounts: the project came to
// To at At, brought by By (a principal string; "": FeRun itself), from
// From ("": its first account, where it was made).
type AccountMove struct {
	Project         string
	From, FromTitle string
	To, ToTitle     string
	At              time.Time
	By              string
	ByName, ByEmail string // a person's, as read
}

// moveSelect reads the rows of project_accounts as moves: h is the rows
// with their predecessor's account (prev), newest first.
const moveSelect = `SELECT h.project_id, coalesce(h.prev, ''), coalesce(pa.title, ''), h.account_id, a.title,
		h.moved_at, h.moved_by, coalesce(p.name, ''), coalesce(p.email, '')
	FROM h JOIN accounts a ON a.id = h.account_id LEFT JOIN accounts pa ON pa.id = h.prev
	LEFT JOIN people p ON p.id = CASE WHEN h.moved_by ~ '^person:[0-9a-f-]{36}$' THEN substr(h.moved_by, 8)::uuid END`

func collectMoves(rows pgx.Rows, err error) ([]AccountMove, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AccountMove, error) {
		var m AccountMove
		err := r.Scan(&m.Project, &m.From, &m.FromTitle, &m.To, &m.ToTitle, &m.At, &m.By, &m.ByName, &m.ByEmail)
		return m, err
	})
}

// ProjectMoves returns a project's accounts over time as moves, newest
// first: the last is the account it was made on (From "").
func (s *Store) ProjectMoves(ctx context.Context, project string) ([]AccountMove, error) {
	return collectMoves(s.q.Query(ctx, `WITH h AS (SELECT id, project_id, account_id, moved_at, moved_by,
			lag(account_id) OVER (ORDER BY moved_at, id) AS prev
		FROM project_accounts WHERE project_id = $1) `+moveSelect+` ORDER BY h.moved_at DESC, h.id DESC`, project))
}

// AccountMoves returns the projects that came to an account and those that
// left it, newest first: a project made on it came to it from nowhere.
func (s *Store) AccountMoves(ctx context.Context, account string) ([]AccountMove, error) {
	return collectMoves(s.q.Query(ctx, `WITH h AS (SELECT id, project_id, account_id, moved_at, moved_by,
			lag(account_id) OVER (PARTITION BY project_id ORDER BY moved_at, id) AS prev
		FROM project_accounts
		WHERE project_id IN (SELECT project_id FROM project_accounts WHERE account_id = $1))
		`+moveSelect+` WHERE h.account_id = $1 OR h.prev = $1 ORDER BY h.moved_at DESC, h.id DESC`, account))
}

// AccountProject is a project on an account as the account's people see
// it: its name, maker and admins, its limits and since when it is there;
// nothing inside it.
type AccountProject struct {
	Project string
	// Maker is the person who made it; ID uuid.Nil: an operator's.
	Maker    Person
	Admins   []Person
	Limits   ProjectLimits
	Since    time.Time
	Deleting bool
}

// AccountProjects returns the projects on an account now, by name, each
// with its maker and the people not disabled who hold admin on it.
func (s *Store) AccountProjects(ctx context.Context, account string) ([]AccountProject, error) {
	rows, err := s.q.Query(ctx, `SELECT p.id, coalesce(m.id, '00000000-0000-0000-0000-000000000000'::uuid),
			coalesce(m.name, ''), coalesce(m.email, ''), p.limit_sandboxes, p.limit_environments,
			p.limit_running_jobs, p.limit_vcpus, p.limit_memory_mib, coalesce(c.moved_at, p.created_at),
			p.deleting_at IS NOT NULL
		FROM projects p LEFT JOIN project_accounts c ON c.project_id = p.id AND c.left_at IS NULL
		LEFT JOIN people m ON m.id = p.made_by
		WHERE coalesce(c.account_id, '`+DefaultAccount+`') = $1 ORDER BY p.id`, account)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AccountProject, error) {
		var ap AccountProject
		l := &ap.Limits
		err := r.Scan(&ap.Project, &ap.Maker.ID, &ap.Maker.Name, &ap.Maker.Email, &l.Sandboxes, &l.Environments,
			&l.RunningJobs, &l.VCPUs, &l.MemoryMiB, &ap.Since, &ap.Deleting)
		ap.Admins = []Person{}
		return ap, err
	})
	if err != nil || len(out) == 0 {
		return out, err
	}
	names := make([]string, len(out))
	at := map[string]int{}
	for i, ap := range out {
		names[i], at[ap.Project] = ap.Project, i
	}
	rows, err = s.q.Query(ctx, `SELECT g.project_id, `+prefixed("p", []string{"id", "issuer", "sub", "email", "name",
		"first_seen", "last_seen", "disabled_at"})+`
		FROM grants g JOIN people p ON p.id = g.person_id
		WHERE g.project_id = ANY($1) AND g.environment_id IS NULL AND g.role = 'admin' AND p.disabled_at IS NULL
		ORDER BY g.project_id, lower(p.name), lower(p.email), p.id`, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var project string
		var p Person
		var disabled *time.Time
		if err := rows.Scan(&project, &p.ID, &p.Issuer, &p.Sub, &p.Email, &p.Name, &p.FirstSeen, &p.LastSeen,
			&disabled); err != nil {
			return nil, err
		}
		p.DisabledAt = timeOf(disabled)
		ap := &out[at[project]]
		ap.Admins = append(ap.Admins, p)
	}
	return out, rows.Err()
}

// MoveProject puts a project on account, by: the account it is on now
// is left and account taken at one moment, now() or, when a move that
// began later committed first, the moment the current row was taken. It
// changes nothing else of the project. It runs in the caller's
// transaction, which holds the project's row FOR UPDATE and account's FOR
// SHARE, and returns the account it was on and whether it moved (false:
// it is on account already, and nothing is written). A project without a
// current row is on DefaultAccount; a move elsewhere writes that row
// first.
func (s *Store) MoveProject(ctx context.Context, project, account, by string) (from string, moved bool, err error) {
	if !s.tx {
		return "", false, errors.New("store: MoveProject outside a transaction")
	}
	var id int64
	err = s.q.QueryRow(ctx, `SELECT id, account_id FROM project_accounts WHERE project_id = $1 AND left_at IS NULL
		FOR UPDATE`, project).Scan(&id, &from)
	if errors.Is(err, pgx.ErrNoRows) {
		if account == DefaultAccount {
			if _, err := s.LockProject(ctx, project, NoLock); err != nil {
				return "", false, err
			}
			return DefaultAccount, false, nil
		}
		from = DefaultAccount
		err = s.q.QueryRow(ctx, `INSERT INTO project_accounts (project_id, account_id, moved_at, moved_by)
			SELECT id, $2, created_at, '' FROM projects WHERE id = $1 RETURNING id`, project, DefaultAccount).Scan(&id)
	}
	if err != nil {
		return "", false, noRow(err, ErrNoRow)
	}
	if from == account {
		return from, false, nil
	}
	var at time.Time
	if err := s.q.QueryRow(ctx, `UPDATE project_accounts SET left_at = greatest(now(), moved_at) WHERE id = $1
		RETURNING left_at`, id).Scan(&at); err != nil {
		return "", false, err
	}
	if _, err := s.q.Exec(ctx, `INSERT INTO project_accounts (project_id, account_id, moved_at, moved_by)
		VALUES ($1, $2, $3, $4)`, project, account, at, by); err != nil {
		return "", false, err
	}
	return from, true, nil
}
