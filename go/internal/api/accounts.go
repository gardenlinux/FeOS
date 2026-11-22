// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	fapi "github.com/gardenlinux/feos/go/ferun/api"
	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/quota"
	"github.com/gardenlinux/feos/go/internal/store"
)

// This file is ADR 0059's "A project is on an account": the accounts and
// their people, as the operators keep them and as an account's people see
// them, the account a project is on and its moves, and the account's
// invitations. Who may use an account: an operator, as admin of every
// account; a person by their row of it (member or admin); and, for the
// account default while it is open (--project-creation people), every
// signed-in person as a member that may put projects on it and read
// nothing of it. Reading an account's page needs a row of it or an
// operator: default's page is the operators'. An account a caller may
// not use is absent, as one that does not exist. Project tokens and
// integrations are on no account and move no project.

// The codes of the accounts' refusals.
const (
	// CodeAccountClosed: 409, the account takes no project, new or moved.
	CodeAccountClosed = "account_closed"
	// CodeAccountInUse: 409, an account a project is or was on is not
	// deleted.
	CodeAccountInUse = "account_in_use"
	// CodeDefaultAccount: 422, default is never closed or deleted and
	// holds no admin rows.
	CodeDefaultAccount = "default_account"
)

// accountRoutes adds the routes of accounts.
func (s *server) accountRoutes(operator func(string, apiHandler), person func(string, apiHandler),
	project func(need, string, apiHandler)) {
	operator("GET /api/v1/admin/accounts", s.adminListAccounts)
	operator("POST /api/v1/admin/accounts", s.adminCreateAccount)
	operator("GET /api/v1/admin/accounts/{account}", s.adminGetAccount)
	operator("PATCH /api/v1/admin/accounts/{account}", s.adminPatchAccount)
	operator("DELETE /api/v1/admin/accounts/{account}", s.adminDeleteAccount)
	operator("PUT /api/v1/admin/accounts/{account}/members/{person}", s.adminPutAccountMember)
	operator("DELETE /api/v1/admin/accounts/{account}/members/{person}", s.adminDeleteAccountMember)
	operator("POST /api/v1/admin/projects/{project}/move", s.adminMoveProject)

	person("GET /api/v1/accounts", s.listMyAccounts)
	person("GET /api/v1/accounts/{account}", s.getAccountPage)
	person("PUT /api/v1/accounts/{account}/members/{person}", s.putAccountMember)
	person("DELETE /api/v1/accounts/{account}/members/{person}", s.deleteAccountMember)
	person("GET /api/v1/accounts/{account}/invitations", s.listAccountInvitations)
	person("POST /api/v1/accounts/{account}/invitations", s.createAccountInvitation)
	person("DELETE /api/v1/accounts/{account}/invitations/{id}", s.revokeAccountInvitation)

	project(noIntegration(member), "GET /api/v1/project/account", s.getProjectAccount)
	project(noIntegration(onProject(admin)), "POST /api/v1/project/move", s.moveOwnProject)
}

// ------------------------------------------------------------- answers --

func refOf(a store.Account) fapi.AccountRef { return fapi.AccountRef{Name: a.ID, Title: a.Title} }

// personRef is a person as an account's page names them.
type personRef struct {
	Person string `json:"person"`
	Name   string `json:"name"`
	Email  string `json:"email"`
}

func personRefOf(p store.Person) personRef {
	return personRef{Person: p.ID.String(), Name: p.Name, Email: p.Email}
}

// accountMemberView is one of an account's people.
type accountMemberView struct {
	personRef
	Role     string `json:"role"`
	Disabled bool   `json:"disabled"`
}

// accountProjectView is a project on an account as its people see it:
// its name, maker (null: an operator's) and admins, since when it is
// there, and its use against its limits; nothing inside it.
type accountProjectView struct {
	Name     string      `json:"name"`
	MadeBy   *personRef  `json:"madeBy"`
	Admins   []personRef `json:"admins"`
	Since    string      `json:"since"`
	Deleting bool        `json:"deleting"`
	Limits   limitsView  `json:"limits"`
	Use      quota.Use   `json:"use"`
}

// moveBy is who brought a project to an account: the principal and a
// name to show (a person's name, else e-mail address; a Basic user's
// name; "FeRun" for FeRun itself).
type moveBy struct {
	Principal string `json:"principal"`
	Name      string `json:"name"`
}

// moveView is one of a project's accounts over time: From null for the
// account it was made on.
type moveView struct {
	Project string           `json:"project"`
	From    *fapi.AccountRef `json:"from"`
	To      fapi.AccountRef  `json:"to"`
	At      string           `json:"at"`
	By      moveBy           `json:"by"`
}

func moveOf(m store.AccountMove) moveView {
	v := moveView{Project: m.Project, To: fapi.AccountRef{Name: m.To, Title: m.ToTitle}, At: rfc3339(m.At),
		By: moveBy{Principal: m.By, Name: byName(m)}}
	if m.From != "" {
		v.From = &fapi.AccountRef{Name: m.From, Title: m.FromTitle}
	}
	return v
}

// byName is the name a move's principal shows as.
func byName(m store.AccountMove) string {
	switch {
	case m.By == "":
		return "FeRun"
	case strings.HasPrefix(m.By, "person:"):
		if m.ByName != "" {
			return m.ByName
		}
		return m.ByEmail
	case strings.HasPrefix(m.By, "basic:"):
		return strings.TrimPrefix(m.By, "basic:")
	}
	return ""
}

func movesOf(ms []store.AccountMove) []moveView {
	out := make([]moveView, 0, len(ms))
	for _, m := range ms {
		out = append(out, moveOf(m))
	}
	return out
}

// accountPage is an account as its people and the operators read it.
type accountPage struct {
	Name      string               `json:"name"`
	Title     string               `json:"title"`
	Reference string               `json:"reference"`
	Closed    bool                 `json:"closed"`
	Role      string               `json:"role"`
	Members   []accountMemberView  `json:"members"`
	Projects  []accountProjectView `json:"projects"`
	Moves     []moveView           `json:"moves"`
}

// adminAccountView is an account in the operators' list.
type adminAccountView struct {
	Name      string `json:"name"`
	Title     string `json:"title"`
	Reference string `json:"reference"`
	CreatedAt string `json:"createdAt"`
	MadeBy    string `json:"madeBy"`
	Closed    bool   `json:"closed"`
	ClosedAt  string `json:"closedAt,omitempty"`
	Projects  int64  `json:"projects"`
	Members   int64  `json:"members"`
}

func adminAccountOf(a store.AccountInfo) adminAccountView {
	return adminAccountView{Name: a.ID, Title: a.Title, Reference: a.Reference, CreatedAt: rfc3339(a.CreatedAt),
		MadeBy: a.MadeBy, Closed: a.Closed(), ClosedAt: rfc3339(a.ClosedAt), Projects: a.Projects, Members: a.Members}
}

// projectMoveAnswer is the answer of a move.
type projectMoveAnswer struct {
	Project string          `json:"project"`
	Account fapi.AccountRef `json:"account"`
	From    fapi.AccountRef `json:"from"`
	MovedAt string          `json:"movedAt"`
	Moved   bool            `json:"moved"`
}

// ------------------------------------------------------------- the rules --

func errNoAccount() error { return apierr.New(http.StatusNotFound, "no such account") }

func errAccountClosed(name string) error {
	return apierr.WithCode(http.StatusConflict, CodeAccountClosed, "account %s is closed: it takes no new project", name)
}

func errDefaultAccount(what string) error {
	return apierr.WithCode(http.StatusUnprocessableEntity, CodeDefaultAccount, "the account %s %s",
		store.DefaultAccount, what)
}

func errLastAccountAdmin() error {
	return apierr.WithCode(http.StatusConflict, CodeLastAdmin, "an account keeps an admin")
}

// defaultOpen reports whether every signed-in person may put projects on
// default (--project-creation people).
func (s *server) defaultOpen() bool { return s.cfg.ProjectCreation != ProjectCreationOperators }

// accountRole is a caller's role on an account: an operator's is admin;
// a person's their row's; default's, while it is open, member for a
// person without a row. explicit: the account's page is the caller's to
// read (a row of an account but default, or an operator: default's page
// is the operators', a row of it only lets a person put projects on it).
// "" is none.
func (s *server) accountRole(ctx context.Context, st *store.Store, person uuid.UUID, operator bool,
	account string) (role string, explicit bool, err error) {
	if operator {
		return store.AccountAdmin, true, nil
	}
	if person == uuid.Nil {
		return "", false, nil
	}
	role, err = st.AccountRole(ctx, account, person)
	if err != nil {
		return "", false, fmt.Errorf("account %s: %w", account, err)
	}
	if role != "" {
		return role, account != store.DefaultAccount, nil
	}
	if account == store.DefaultAccount && s.defaultOpen() {
		return store.AccountMember, false, nil
	}
	return "", false, nil
}

// myAccounts lists the accounts a session may put projects on or read,
// by name: an operator's are every account, as admin; a person's those of
// their rows, and default while it is open.
func (s *server) myAccounts(ctx context.Context, sess store.UISession) ([]fapi.MeAccount, error) {
	out := []fapi.MeAccount{}
	if sess.Admin {
		all, err := s.st.AllAccounts(ctx)
		if err != nil {
			return nil, internalf("accounts: %v", err)
		}
		for _, a := range all {
			out = append(out, fapi.MeAccount{Name: a.ID, Title: a.Title, Role: store.AccountAdmin, Explicit: true,
				Closed: a.Closed()})
		}
		return out, nil
	}
	if sess.PersonID == uuid.Nil {
		return out, nil
	}
	rows, err := s.st.PersonAccounts(ctx, sess.PersonID)
	if err != nil {
		return nil, internalf("accounts: %v", err)
	}
	onDefault := false
	for _, a := range rows {
		onDefault = onDefault || a.ID == store.DefaultAccount
		out = append(out, fapi.MeAccount{Name: a.ID, Title: a.Title, Role: a.Role, Explicit: a.ID != store.DefaultAccount,
			Closed: a.Closed()})
	}
	if !onDefault && s.defaultOpen() {
		d, err := s.st.GetAccount(ctx, store.DefaultAccount)
		if err != nil {
			return nil, internalf("account %s: %v", store.DefaultAccount, err)
		}
		out = append(out, fapi.MeAccount{Name: d.ID, Title: d.Title, Role: store.AccountMember, Closed: d.Closed()})
		sortAccounts(out)
	}
	return out, nil
}

func sortAccounts(as []fapi.MeAccount) {
	slices.SortFunc(as, func(a, b fapi.MeAccount) int { return strings.Compare(a.Name, b.Name) })
}

// fillProjectAccounts sets the account of each of a session's projects.
func (s *server) fillProjectAccounts(ctx context.Context, ps []fapi.MeProject) error {
	if len(ps) == 0 {
		return nil
	}
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.Project
	}
	accts, err := s.st.ProjectAccounts(ctx, names)
	if err != nil {
		return internalf("accounts of projects: %v", err)
	}
	for i := range ps {
		if a, ok := accts[ps[i].Project]; ok {
			ref := refOf(a)
			ps[i].Account = &ref
		}
	}
	return nil
}

// useAccount locks an account that is to take a project, new or moved,
// FOR SHARE: one that does not exist and one the caller may not put
// projects on are absent (404), a closed one 409 account_closed.
// operator and person are the caller.
func (s *server) useAccount(ctx context.Context, tx *store.Store, name string, person uuid.UUID,
	operator bool) (store.Account, error) {
	a, err := tx.LockAccount(ctx, name, store.ForShare)
	if isNoRow(err) {
		return store.Account{}, errNoAccount()
	}
	if err != nil {
		return store.Account{}, err
	}
	role, _, err := s.accountRole(ctx, tx, person, operator, name)
	if err != nil {
		return store.Account{}, err
	}
	if role == "" {
		return store.Account{}, errNoAccount()
	}
	if a.Closed() {
		return store.Account{}, errAccountClosed(name)
	}
	return a, nil
}

// readableAccountIn reads the account name for the session, in st with
// lock: absent (404) unless its page is the caller's (accountRole's
// explicit); role is theirs on it.
func (s *server) readableAccountIn(ctx context.Context, st *store.Store, sess store.UISession, name string,
	lock store.RowLock) (store.Account, string, error) {
	a, err := st.LockAccount(ctx, name, lock)
	if isNoRow(err) {
		return store.Account{}, "", errNoAccount()
	}
	if err != nil {
		return store.Account{}, "", err
	}
	role, explicit, err := s.accountRole(ctx, st, sess.PersonID, sess.Admin, name)
	if err != nil {
		return store.Account{}, "", err
	}
	if !explicit {
		return store.Account{}, "", errNoAccount()
	}
	return a, role, nil
}

// leavableAccountIn reads the account name for its member leaving it, in
// st with the row FOR NO KEY UPDATE: absent (404) unless the caller holds
// a role on it. A person with a row of default leaves it though its page
// is the operators'.
func (s *server) leavableAccountIn(ctx context.Context, st *store.Store, sess store.UISession, name string) error {
	_, err := st.LockAccount(ctx, name, store.ForNoKeyUpdate)
	if isNoRow(err) {
		return errNoAccount()
	}
	if err != nil {
		return err
	}
	role, _, err := s.accountRole(ctx, st, sess.PersonID, sess.Admin, name)
	if err != nil {
		return err
	}
	if role == "" {
		return errNoAccount()
	}
	return nil
}

// keepAccountAdmin runs change and refuses (409 last_admin) a change that
// leaves an account that had an admin without one. The caller holds the
// account's row FOR NO KEY UPDATE, which serialises its people's changes.
func keepAccountAdmin(ctx context.Context, tx *store.Store, account string, change func() error) error {
	before, err := tx.CountAccountAdmins(ctx, account)
	if err != nil {
		return err
	}
	if err := change(); err != nil {
		return err
	}
	after, err := tx.CountAccountAdmins(ctx, account)
	if err != nil {
		return err
	}
	if before > 0 && after == 0 {
		return errLastAccountAdmin()
	}
	return nil
}

// accountRoleOf reads a request's account role: member or admin, else
// 400.
func accountRoleOf(role string) error {
	if role != store.AccountMember && role != store.AccountAdmin {
		return apierr.New(http.StatusBadRequest, "role must be member or admin, got %q", role)
	}
	return nil
}

func higherAccountRole(a, b string) string {
	if a == store.AccountAdmin || b == store.AccountAdmin {
		return store.AccountAdmin
	}
	return store.AccountMember
}

// pagePerson reads the path's {person}.
func pagePerson(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue("person"))
	if err != nil {
		return uuid.Nil, apierr.New(http.StatusNotFound, "no such person")
	}
	return id, nil
}

// --------------------------------------------------------- the page --

// accountPageOf reads an account's page: its people, its projects with
// their use, its moves.
func (s *server) accountPageOf(ctx context.Context, a store.Account, role string) (accountPage, error) {
	page := accountPage{Name: a.ID, Title: a.Title, Reference: a.Reference, Closed: a.Closed(), Role: role,
		Members: []accountMemberView{}, Projects: []accountProjectView{}}
	ms, err := s.st.ListAccountMembers(ctx, a.ID)
	if err != nil {
		return page, fmt.Errorf("members of %s: %w", a.ID, err)
	}
	for _, m := range ms {
		page.Members = append(page.Members, accountMemberView{personRef: personRefOf(m.Person), Role: m.Role,
			Disabled: m.Disabled()})
	}
	ps, err := s.st.AccountProjects(ctx, a.ID)
	if err != nil {
		return page, fmt.Errorf("projects of %s: %w", a.ID, err)
	}
	for _, p := range ps {
		use, err := quota.Of(ctx, s.st, p.Project, uuid.Nil)
		if err != nil {
			return page, err
		}
		v := accountProjectView{Name: p.Project, Admins: make([]personRef, 0, len(p.Admins)), Since: rfc3339(p.Since),
			Deleting: p.Deleting, Limits: limitsOf(p.Limits), Use: use}
		if p.Maker.ID != uuid.Nil {
			m := personRefOf(p.Maker)
			v.MadeBy = &m
		}
		for _, ad := range p.Admins {
			v.Admins = append(v.Admins, personRefOf(ad))
		}
		page.Projects = append(page.Projects, v)
	}
	moves, err := s.st.AccountMoves(ctx, a.ID)
	if err != nil {
		return page, fmt.Errorf("moves of %s: %w", a.ID, err)
	}
	page.Moves = movesOf(moves)
	return page, nil
}

// ------------------------------------------------------------ person --

// listMyAccounts is GET /api/v1/accounts: the accounts the person may put
// projects on or read, as /auth/me lists them.
func (s *server) listMyAccounts(w http.ResponseWriter, r *http.Request) error {
	as, err := s.myAccounts(r.Context(), personSession(r))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, struct {
		Accounts []fapi.MeAccount `json:"accounts"`
	}{as})
}

// getAccountPage is GET /api/v1/accounts/{account}: the account's page,
// for its people and the operators; default's for the operators only.
func (s *server) getAccountPage(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	a, role, err := s.readableAccountIn(ctx, s.st, personSession(r), r.PathValue("account"), store.NoLock)
	if err != nil {
		return passOr(err, "account")
	}
	page, err := s.accountPageOf(ctx, a, role)
	if err != nil {
		return passOr(err, "account")
	}
	return writeJSON(w, http.StatusOK, page)
}

// adminOfAccount reads the path's account for a change by its admin, in
// tx with the row FOR NO KEY UPDATE: absent to whoever reads no page of
// it, 403 role_required to a member.
func (s *server) adminOfAccount(ctx context.Context, tx *store.Store, sess store.UISession,
	name string) (store.Account, error) {
	a, role, err := s.readableAccountIn(ctx, tx, sess, name, store.ForNoKeyUpdate)
	if err != nil {
		return store.Account{}, err
	}
	if role != store.AccountAdmin {
		return store.Account{}, apierr.RoleRequired(store.AccountAdmin, "account")
	}
	return a, nil
}

// putAccountMember is PUT /api/v1/accounts/{account}/members/{person}
// {role}: an admin of the account changes the role of one of its people
// (a person joins by an invitation, so another is 404); default holds no
// admin row (422), and an account keeps an admin (409).
func (s *server) putAccountMember(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Role string `json:"role"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	if err := accountRoleOf(req.Role); err != nil {
		return err
	}
	ctx, sess, name := r.Context(), personSession(r), r.PathValue("account")
	person, err := pagePerson(r)
	if err != nil {
		return err
	}
	by := sessionPrincipal(sess).String()
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		if _, err := s.adminOfAccount(ctx, tx, sess, name); err != nil {
			return err
		}
		if name == store.DefaultAccount && req.Role == store.AccountAdmin {
			return errDefaultAccount("has no admins but the operators")
		}
		if have, err := tx.AccountMemberRole(ctx, name, person); err != nil {
			return err
		} else if have == "" {
			return apierr.New(http.StatusNotFound, "no such member")
		}
		return keepAccountAdmin(ctx, tx, name, func() error {
			return tx.PutAccountMember(ctx, name, person, req.Role, by)
		})
	})
	if err != nil {
		return passOr(err, "account member")
	}
	s.recordMember(r, name, person, req.Role, by)
	return writeJSON(w, http.StatusOK, struct {
		Person string `json:"person"`
		Role   string `json:"role"`
	}{person.String(), req.Role})
}

// deleteAccountMember is DELETE /api/v1/accounts/{account}/members/{person}:
// an admin of the account removes one of its people, or a person leaves;
// an account keeps an admin (409).
func (s *server) deleteAccountMember(w http.ResponseWriter, r *http.Request) error {
	ctx, sess, name := r.Context(), personSession(r), r.PathValue("account")
	person, err := pagePerson(r)
	if err != nil {
		return err
	}
	var deleted bool
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		if person == sess.PersonID {
			if err := s.leavableAccountIn(ctx, tx, sess, name); err != nil {
				return err
			}
		} else if _, err := s.adminOfAccount(ctx, tx, sess, name); err != nil {
			return err
		}
		return keepAccountAdmin(ctx, tx, name, func() error {
			var err error
			deleted, err = tx.DeleteAccountMember(ctx, name, person)
			return err
		})
	})
	if err != nil {
		return passOr(err, "account member")
	}
	if deleted {
		s.recordMemberRemoved(r, name, person, sessionPrincipal(sess).String())
	}
	return writeJSON(w, http.StatusOK, struct {
		Deleted bool `json:"deleted"`
	}{deleted})
}

// listAccountInvitations is GET /api/v1/accounts/{account}/invitations:
// open, used and expired, for its admins (default's: the operators).
func (s *server) listAccountInvitations(w http.ResponseWriter, r *http.Request) error {
	ctx, sess, name := r.Context(), personSession(r), r.PathValue("account")
	if err := s.st.InTx(ctx, func(tx *store.Store) error {
		_, err := s.adminOfAccount(ctx, tx, sess, name)
		return err
	}); err != nil {
		return passOr(err, "account invitations")
	}
	invs, err := s.st.ListAccountInvitations(ctx, name)
	if err != nil {
		return fmt.Errorf("account invitations: %w", err)
	}
	out := struct {
		Invitations []invitationView `json:"invitations"`
	}{Invitations: make([]invitationView, 0, len(invs))}
	for _, i := range invs {
		out.Invitations = append(out.Invitations, invitationOf(i))
	}
	return writeJSON(w, http.StatusOK, out)
}

// createAccountInvitation is POST /api/v1/accounts/{account}/invitations
// {role}: a single-use link that makes whoever opens it signed in one of
// the account's people with the role (member or admin), valid for 7 days,
// by an admin of the account (default's: an operator, and for the role
// member only). A closed account may invite. The answer holds the secret
// and the UI's page of it once.
func (s *server) createAccountInvitation(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Role string `json:"role"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	if err := accountRoleOf(req.Role); err != nil {
		return err
	}
	ctx, sess, name := r.Context(), personSession(r), r.PathValue("account")
	secret, hash, err := mintProjectToken()
	if err != nil {
		return fmt.Errorf("invitation: %w", err)
	}
	var inv store.Invitation
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		if _, err := s.adminOfAccount(ctx, tx, sess, name); err != nil {
			return err
		}
		if name == store.DefaultAccount && req.Role == store.AccountAdmin {
			return errDefaultAccount("has no admins but the operators")
		}
		var err error
		inv, err = tx.InsertInvitation(ctx, store.Invitation{Hash: hash, AccountID: name, Role: req.Role,
			MadeBy: sessionPrincipal(sess).String()}, invitationTTL)
		if isNoRow(err) {
			return errNoAccount()
		}
		return err
	})
	if err != nil {
		return passOr(err, "invitation")
	}
	return writeJSON(w, http.StatusCreated, struct {
		invitationView
		Token string `json:"token"`
		URL   string `json:"url"`
	}{invitationOf(inv), secret, s.inviteURL(secret)})
}

// revokeAccountInvitation is DELETE
// /api/v1/accounts/{account}/invitations/{id}, by an admin of the
// account; one of another scope is absent.
func (s *server) revokeAccountInvitation(w http.ResponseWriter, r *http.Request) error {
	ctx, sess, name := r.Context(), personSession(r), r.PathValue("account")
	var ok bool
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		if _, err := s.adminOfAccount(ctx, tx, sess, name); err != nil {
			return err
		}
		inv, err := tx.GetInvitation(ctx, r.PathValue("id"))
		if isNoRow(err) || (err == nil && inv.AccountID != name) {
			return errNoInvitation()
		}
		if err != nil {
			return err
		}
		ok, err = tx.DeleteAccountInvitation(ctx, name, inv.Hash)
		return err
	})
	if err != nil {
		return passOr(err, "revoke invitation")
	}
	return writeJSON(w, http.StatusOK, struct {
		Revoked bool `json:"revoked"`
	}{ok})
}

// ---------------------------------------------------------- operators --

// adminListAccounts is GET /api/v1/admin/accounts: every account by name,
// with the number of projects on it and of its people.
func (s *server) adminListAccounts(w http.ResponseWriter, r *http.Request) error {
	as, err := s.st.ListAccounts(r.Context())
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	out := struct {
		Accounts []adminAccountView `json:"accounts"`
	}{Accounts: make([]adminAccountView, 0, len(as))}
	for _, a := range as {
		out.Accounts = append(out.Accounts, adminAccountOf(a))
	}
	return writeJSON(w, http.StatusOK, out)
}

// adminCreateAccount is POST /api/v1/admin/accounts {name, title?,
// reference?}: an operator makes an account.
func (s *server) adminCreateAccount(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Name      string `json:"name"`
		Title     string `json:"title"`
		Reference string `json:"reference"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	if !store.AccountNamePattern.MatchString(req.Name) {
		return apierr.New(http.StatusBadRequest, "account name must match %s", store.AccountNamePattern)
	}
	ctx := r.Context()
	a, _ := auth.AdminFrom(ctx)
	_, made, err := s.st.CreateAccount(ctx, store.Account{ID: req.Name, Title: req.Title, Reference: req.Reference,
		MadeBy: a.Principal().String()})
	if err != nil {
		return fmt.Errorf("create account: %w", err)
	}
	if !made {
		return apierr.WithCode(http.StatusConflict, fapi.CodeNameTaken, "an account named %s exists", req.Name)
	}
	info, err := s.st.GetAccountInfo(ctx, req.Name)
	if err != nil {
		return fmt.Errorf("create account: %w", err)
	}
	by := adminOfRequest(r)
	s.log.Info("account created", "account", req.Name, "by", by)
	s.record(ctx, store.ActivityRow{Kind: "account_created", Category: "lifecycle",
		Name:   "account " + req.Name + " created by " + by,
		Detail: detail(map[string]any{"account": req.Name, "title": req.Title, "reference": req.Reference, "by": by})})
	return writeJSON(w, http.StatusCreated, adminAccountOf(info))
}

// adminGetAccount is GET /api/v1/admin/accounts/{account}: its page, with
// the operator as admin.
func (s *server) adminGetAccount(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	a, err := s.st.GetAccount(ctx, r.PathValue("account"))
	if isNoRow(err) {
		return errNoAccount()
	}
	if err != nil {
		return fmt.Errorf("account: %w", err)
	}
	page, err := s.accountPageOf(ctx, a, store.AccountAdmin)
	if err != nil {
		return passOr(err, "account")
	}
	return writeJSON(w, http.StatusOK, page)
}

// adminPatchAccount is PATCH /api/v1/admin/accounts/{account} {title?,
// reference?, closed?}: an operator changes, closes or reopens an
// account; default is never closed (422).
func (s *server) adminPatchAccount(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Title     *string `json:"title"`
		Reference *string `json:"reference"`
		Closed    *bool   `json:"closed"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	ctx, name := r.Context(), r.PathValue("account")
	if name == store.DefaultAccount && req.Closed != nil && *req.Closed {
		return errDefaultAccount("is never closed")
	}
	admin, _ := auth.AdminFrom(ctx)
	var before, after store.Account
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		var err error
		before, err = tx.LockAccount(ctx, name, store.ForUpdate)
		if isNoRow(err) {
			return errNoAccount()
		}
		if err != nil {
			return err
		}
		after, err = tx.PatchAccount(ctx, name, store.AccountChange{Title: req.Title, Reference: req.Reference,
			Closed: req.Closed}, admin.Principal().String())
		return err
	})
	if err != nil {
		return passOr(err, "change account")
	}
	by := adminOfRequest(r)
	d := detail(map[string]any{"account": name, "title": after.Title, "reference": after.Reference, "by": by})
	if before.Title != after.Title || before.Reference != after.Reference {
		s.record(ctx, store.ActivityRow{Kind: "account_changed", Category: "lifecycle",
			Name: "account " + name + " changed by " + by, Detail: d})
	}
	switch {
	case !before.Closed() && after.Closed():
		s.log.Info("account closed", "account", name, "by", by)
		s.record(ctx, store.ActivityRow{Kind: "account_closed", Category: "lifecycle",
			Name: "account " + name + " closed by " + by, Detail: d})
	case before.Closed() && !after.Closed():
		s.log.Info("account reopened", "account", name, "by", by)
		s.record(ctx, store.ActivityRow{Kind: "account_reopened", Category: "lifecycle",
			Name: "account " + name + " reopened by " + by, Detail: d})
	}
	info, err := s.st.GetAccountInfo(ctx, name)
	if err != nil {
		return fmt.Errorf("change account: %w", err)
	}
	return writeJSON(w, http.StatusOK, adminAccountOf(info))
}

// adminDeleteAccount is DELETE /api/v1/admin/accounts/{account}: only an
// account no project is or was on (else 409; close it), never default
// (422).
func (s *server) adminDeleteAccount(w http.ResponseWriter, r *http.Request) error {
	ctx, name := r.Context(), r.PathValue("account")
	if name == store.DefaultAccount {
		return errDefaultAccount("is never deleted")
	}
	ok, err := s.st.DeleteAccount(ctx, name)
	if errors.Is(err, store.ErrInUse) {
		return apierr.WithCode(http.StatusConflict, CodeAccountInUse,
			"account %s holds or held a project: close it instead", name)
	}
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	if !ok {
		return errNoAccount()
	}
	by := adminOfRequest(r)
	s.log.Info("account deleted", "account", name, "by", by)
	s.record(ctx, store.ActivityRow{Kind: "account_deleted", Category: "lifecycle",
		Name: "account " + name + " deleted by " + by, Detail: detail(map[string]any{"account": name, "by": by})})
	return writeJSON(w, http.StatusOK, struct {
		Deleted bool `json:"deleted"`
	}{true})
}

// adminPutAccountMember is PUT
// /api/v1/admin/accounts/{account}/members/{person} {role}: an operator
// makes any person FeRun knows one of the account's people, or changes
// their role; admin on default is 422, and an account keeps an admin.
func (s *server) adminPutAccountMember(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Role string `json:"role"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	if err := accountRoleOf(req.Role); err != nil {
		return err
	}
	ctx, name := r.Context(), r.PathValue("account")
	person, err := pagePerson(r)
	if err != nil {
		return err
	}
	if name == store.DefaultAccount && req.Role == store.AccountAdmin {
		return errDefaultAccount("has no admins but the operators")
	}
	a, _ := auth.AdminFrom(ctx)
	by := a.Principal().String()
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		if _, err := tx.LockAccount(ctx, name, store.ForNoKeyUpdate); isNoRow(err) {
			return errNoAccount()
		} else if err != nil {
			return err
		}
		if _, err := tx.GetPerson(ctx, person); isNoRow(err) {
			return apierr.New(http.StatusNotFound, "no such person")
		} else if err != nil {
			return err
		}
		return keepAccountAdmin(ctx, tx, name, func() error {
			return tx.PutAccountMember(ctx, name, person, req.Role, by)
		})
	})
	if err != nil {
		return passOr(err, "account member")
	}
	s.recordMember(r, name, person, req.Role, by)
	return writeJSON(w, http.StatusOK, struct {
		Person string `json:"person"`
		Role   string `json:"role"`
	}{person.String(), req.Role})
}

// adminDeleteAccountMember is DELETE
// /api/v1/admin/accounts/{account}/members/{person}: an operator removes
// one of the account's people, unless the account would have no admin.
func (s *server) adminDeleteAccountMember(w http.ResponseWriter, r *http.Request) error {
	ctx, name := r.Context(), r.PathValue("account")
	person, err := pagePerson(r)
	if err != nil {
		return err
	}
	var deleted bool
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		if _, err := tx.LockAccount(ctx, name, store.ForNoKeyUpdate); isNoRow(err) {
			return errNoAccount()
		} else if err != nil {
			return err
		}
		return keepAccountAdmin(ctx, tx, name, func() error {
			var err error
			deleted, err = tx.DeleteAccountMember(ctx, name, person)
			return err
		})
	})
	if err != nil {
		return passOr(err, "account member")
	}
	if deleted {
		a, _ := auth.AdminFrom(ctx)
		s.recordMemberRemoved(r, name, person, a.Principal().String())
	}
	return writeJSON(w, http.StatusOK, struct {
		Deleted bool `json:"deleted"`
	}{deleted})
}

func (s *server) recordMember(r *http.Request, account string, person uuid.UUID, role, by string) {
	s.record(r.Context(), store.ActivityRow{Kind: "account_member_set", Category: "lifecycle",
		Name:   "person " + person.String() + " is " + role + " of account " + account + " by " + by,
		Detail: detail(map[string]any{"account": account, "person": person.String(), "role": role, "by": by})})
}

func (s *server) recordMemberRemoved(r *http.Request, account string, person uuid.UUID, by string) {
	s.record(r.Context(), store.ActivityRow{Kind: "account_member_removed", Category: "lifecycle",
		Name:   "person " + person.String() + " left account " + account + " by " + by,
		Detail: detail(map[string]any{"account": account, "person": person.String(), "by": by})})
}

// ----------------------------------------------------------- the move --

// moveProject moves a project to account for by (a person, or an
// operator): the project's row FOR UPDATE (unknown 404, being deleted
// 409), the account's FOR SHARE and usable by the mover (useAccount). It
// changes nothing of the project but its account (store.MoveProject).
func (s *server) moveProject(ctx context.Context, project, account string, by auth.Principal) (projectMoveAnswer, error) {
	var out projectMoveAnswer
	var at time.Time
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		t, err := tx.LockProject(ctx, project, store.ForUpdate)
		if isNoRow(err) {
			return errNoProject()
		}
		if err != nil {
			return err
		}
		if t.Deleting() {
			return apierr.New(http.StatusConflict, "project %s is being deleted", project)
		}
		cur, since, err := tx.ProjectAccount(ctx, project)
		if err != nil {
			return err
		}
		if cur.ID == account {
			// Already there: nothing is written, so a closed account,
			// which only refuses taking a project, answers too.
			role, _, err := s.accountRole(ctx, tx, by.Person, by.Operator, account)
			if err != nil {
				return err
			}
			if role == "" {
				return errNoAccount()
			}
			out = projectMoveAnswer{Project: project, Account: refOf(cur), From: refOf(cur), MovedAt: rfc3339(since)}
			return nil
		}
		to, err := s.useAccount(ctx, tx, account, by.Person, by.Operator)
		if err != nil {
			return err
		}
		from, moved, err := tx.MoveProject(ctx, project, account, by.String())
		if err != nil {
			return err
		}
		fa, err := tx.GetAccount(ctx, from)
		if err != nil {
			return err
		}
		_, at, err = tx.ProjectAccount(ctx, project)
		if err != nil {
			return err
		}
		out = projectMoveAnswer{Project: project, Account: refOf(to), From: refOf(fa), MovedAt: rfc3339(at), Moved: moved}
		return nil
	})
	if err != nil {
		return out, passOr(err, "move project")
	}
	if out.Moved {
		who := by.String()
		s.log.Info("project moved to another account", "project", project, "account", account,
			"from", out.From.Name, "by", who)
		s.record(ctx, store.ActivityRow{ProjectID: project, Kind: "project_account_moved", Category: "lifecycle",
			Name:   "project " + project + " moved to account " + account + " by " + who,
			Detail: detail(map[string]any{"project": project, "from": out.From.Name, "to": account, "by": who})})
	}
	return out, nil
}

// moveOwnProject is POST /api/v1/project/move {account}: an admin of the
// project who may put projects on the account moves it there, or an
// operator. A token is on no account (403 person_required); an
// integration moves no project (403 integration_refused, the route's
// need).
func (s *server) moveOwnProject(w http.ResponseWriter, r *http.Request) error {
	t, _ := auth.ProjectFrom(r.Context())
	if t.Principal.IsIntegration() {
		return errIntegrationRefused()
	}
	if t.Principal.Person == uuid.Nil {
		return apierr.WithCode(http.StatusForbidden, fapi.CodePersonRequired,
			"a token is on no account: a person or an operator moves a project")
	}
	var req struct {
		Account string `json:"account"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	out, err := s.moveProject(r.Context(), t.Name, req.Account, t.Principal)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

// adminMoveProject is POST /api/v1/admin/projects/{project}/move
// {account}: an operator moves a project to any open account.
func (s *server) adminMoveProject(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Account string `json:"account"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	a, _ := auth.AdminFrom(r.Context())
	out, err := s.moveProject(r.Context(), r.PathValue("project"), req.Account, a.Principal())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

// getProjectAccount is GET /api/v1/project/account: the account the
// project is on, since when, and its moves, newest first; never the
// account's reference, which is for its people and the operators.
func (s *server) getProjectAccount(w http.ResponseWriter, r *http.Request) error {
	ctx, project := r.Context(), projectOf(r)
	a, since, err := s.st.ProjectAccount(ctx, project)
	if isNoRow(err) {
		return errNoProject()
	}
	if err != nil {
		return fmt.Errorf("account of %s: %w", project, err)
	}
	ms, err := s.st.ProjectMoves(ctx, project)
	if err != nil {
		return fmt.Errorf("moves of %s: %w", project, err)
	}
	moves := movesOf(ms)
	if len(moves) == 0 {
		// A project made before its accounts were kept: on default since
		// it was made.
		moves = append(moves, moveView{Project: project, To: refOf(a), At: rfc3339(since),
			By: moveBy{Name: "FeRun"}})
	}
	return writeJSON(w, http.StatusOK, struct {
		Account fapi.AccountRef `json:"account"`
		Since   string          `json:"since"`
		Moves   []moveView      `json:"moves"`
	}{refOf(a), rfc3339(since), moves})
}
