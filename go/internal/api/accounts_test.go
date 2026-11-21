// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	fapi "github.com/gardenlinux/feos/go/ferun/api"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/store"
)

var updateAccountFixtures = flag.Bool("update-account-fixtures", false,
	"rewrite the accounts' fixtures in testdata/ferun from the handlers' answers")

// accountFixtures are the fixtures of testdata/ferun the accounts' tests
// write and check (TestEnvironmentFixtures leaves them out).
var accountFixtures = map[string]bool{
	"GET_accounts": true, "GET_accounts_account": true, "GET_project_account": true, "POST_project_move": true,
	"GET_admin_accounts": true, "GET_invitations_token": true,
}

// The account of the tests besides default.
const testAccount = "cc-4711"

// accountWorld is a peopleWorld with the account cc-4711 ("Team X") and
// people of each kind: an operator, the account's admin and member (both
// admins of testProject), a person on no account (an admin of testProject
// too: default's implicit member, a stranger to cc-4711) and a disabled
// member of cc-4711.
type accountWorld struct {
	*peopleWorld
	op, aa, am, pi, dis string // session cookies
	jwt                 string // an integration's bearer
}

func newAccountWorld(t *testing.T) *accountWorld {
	t.Helper()
	w := &accountWorld{peopleWorld: newPeopleWorld(t, envOptions{}),
		jwt: bearerOf("eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln")}
	e := w.e
	expect(t, e.do("POST", "/api/v1/admin/accounts", adminAuth(),
		map[string]string{"name": testAccount, "title": "Team X", "reference": "CC 4711"}), http.StatusCreated, "")
	admin := store.Grant{Role: "admin"}
	w.op = w.person(t, "op", true)
	w.aa = w.person(t, "aa", false, admin)
	w.am = w.person(t, "am", false, admin)
	w.pi = w.person(t, "pi", false, admin)
	w.dis = w.person(t, "dis", false)
	for name, role := range map[string]string{"aa": "admin", "am": "member", "dis": "member"} {
		expect(t, e.do("PUT", "/api/v1/admin/accounts/"+testAccount+"/members/"+w.persons[name].String(), adminAuth(),
			map[string]string{"role": role}), http.StatusOK, "")
	}
	expect(t, e.do("POST", "/api/v1/admin/people/"+w.persons["dis"].String()+"/disable", adminAuth(), nil),
		http.StatusOK, "")
	return w
}

// caller is one kind of caller of the person class.
type caller struct {
	name string
	rq   func(method, path string, body any) request
}

func (w *accountWorld) callers() []caller {
	session := func(c string) func(method, path string, body any) request {
		return func(method, path string, body any) request { return as(c, "", method, path, body) }
	}
	return []caller{
		{"operator", session(w.op)},
		{"explicit admin", session(w.aa)},
		{"explicit member", session(w.am)},
		{"implicit default member", session(w.pi)},
		{"disabled", session(w.dis)},
		{"project token", func(method, path string, body any) request {
			return byToken(w.e.projectToken, method, path, body)
		}},
		{"integration", func(method, path string, body any) request {
			return request{method: method, path: path, body: body, header: http.Header{"Authorization": {w.jwt}}}
		}},
	}
}

// statusOf serves rq and reads the answer as status and error text.
func (w *accountWorld) statusOf(rq request) (int, string) {
	resp := w.e.serve(rq)
	var b struct{ Error, Code string }
	_ = json.Unmarshal(resp.Body.Bytes(), &b)
	if b.Code != "" {
		return resp.Code, b.Code
	}
	return resp.Code, b.Error
}

// Every person-class route of accounts answers each kind of caller as
// the rules say: an operator is admin of every account, a row's role is
// the person's, default's implicit member may use it and read nothing of
// it, a stranger finds no account, a disabled person is signed out, a
// project token is no person and an integration is refused by the class.
func TestAccountRoutesByCaller(t *testing.T) {
	t.Parallel()
	w := newAccountWorld(t)
	acc := "/api/v1/accounts/" + testAccount
	am := w.persons["am"].String()
	nobody := uuid.New().String()
	// The wants by caller, in callers' order: operator, explicit admin,
	// explicit member, implicit default member (a stranger to cc-4711),
	// disabled, project token, integration.
	const signIn, refused = "401 sign-in required", "403 integration_refused"
	cases := []struct {
		method, path string
		body         any
		want         [7]string
	}{
		{"GET", "/api/v1/accounts", nil, [7]string{"200", "200", "200", "200", signIn, signIn, refused}},
		{"GET", acc, nil, [7]string{"200", "200", "200", "404 no such account", signIn, signIn, refused}},
		{"GET", "/api/v1/accounts/default", nil,
			[7]string{"200", "404 no such account", "404 no such account", "404 no such account", signIn, signIn, refused}},
		{"GET", "/api/v1/accounts/nope", nil,
			[7]string{"404 no such account", "404 no such account", "404 no such account", "404 no such account", signIn,
				signIn, refused}},
		{"PUT", acc + "/members/" + am, map[string]string{"role": "member"},
			[7]string{"200", "200", "403 role_required", "404 no such account", signIn, signIn, refused}},
		{"PUT", acc + "/members/" + nobody, map[string]string{"role": "member"},
			[7]string{"404 no such member", "404 no such member", "403 role_required", "404 no such account", signIn,
				signIn, refused}},
		{"DELETE", acc + "/members/" + nobody, nil,
			[7]string{"200", "200", "403 role_required", "404 no such account", signIn, signIn, refused}},
		{"GET", acc + "/invitations", nil,
			[7]string{"200", "200", "403 role_required", "404 no such account", signIn, signIn, refused}},
		{"POST", acc + "/invitations", map[string]string{"role": "member"},
			[7]string{"201", "201", "403 role_required", "404 no such account", signIn, signIn, refused}},
		{"DELETE", acc + "/invitations/nope", nil,
			[7]string{"404 no such invitation", "404 no such invitation", "403 role_required", "404 no such account",
				signIn, signIn, refused}},
		{"GET", "/api/v1/accounts/default/invitations", nil,
			[7]string{"200", "404 no such account", "404 no such account", "404 no such account", signIn, signIn,
				refused}},
		{"POST", "/api/v1/accounts/default/invitations", map[string]string{"role": "member"},
			[7]string{"201", "404 no such account", "404 no such account", "404 no such account", signIn, signIn,
				refused}},
		{"GET", "/api/v1/invitations/t0k3n", nil,
			[7]string{"404 no such invitation", "404 no such invitation", "404 no such invitation",
				"404 no such invitation", signIn, signIn, refused}},
	}
	var bad []string
	for _, c := range cases {
		for i, cl := range w.callers() {
			status, text := w.statusOf(cl.rq(c.method, c.path, c.body))
			want := c.want[i]
			wantStatus, wantText, _ := strings.Cut(want, " ")
			if itoa(status) != wantStatus || (wantText != "" && text != wantText) {
				bad = append(bad, cl.name+": "+c.method+" "+c.path+": "+itoa(status)+" "+text+", want "+want)
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatal(strings.Join(bad, "\n"))
	}

	// The project-class routes: every member reads the account, a person
	// or an operator moves (here to where it is), a token is on no
	// account; an integration is refused by the routes' need.
	for _, c := range []struct {
		name, cookie, token string
		move                string
	}{
		{"operator", w.op, "", "200"},
		{"explicit admin", w.aa, "", "200"},
		{"explicit member", w.am, "", "200"},
		{"implicit default member", w.pi, "", "200"},
		{"project token", "", w.e.projectToken, "403 person_required"},
	} {
		rq := func(method, path string, body any) request {
			if c.token != "" {
				return byToken(c.token, method, path, body)
			}
			return as(c.cookie, testProject, method, path, body)
		}
		if status, text := w.statusOf(rq("GET", "/api/v1/project/account", nil)); status != http.StatusOK {
			t.Errorf("%s: read the account: %d %s", c.name, status, text)
		}
		status, text := w.statusOf(rq("POST", "/api/v1/project/move", map[string]string{"account": "default"}))
		if got := strings.TrimSpace(itoa(status) + " " + text); status < 300 && itoa(status) != c.move ||
			status >= 300 && got != c.move {
			t.Errorf("%s: move: %s, want %s", c.name, got, c.move)
		}
	}
	status, _ := w.statusOf(as(w.dis, testProject, "GET", "/api/v1/project/account", nil))
	if status != http.StatusUnauthorized {
		t.Errorf("disabled: read the account: %d", status)
	}
	for _, rt := range w.e.srv.routes() {
		if rt.pattern != "GET /api/v1/project/account" && rt.pattern != "POST /api/v1/project/move" {
			continue
		}
		for _, p := range []auth.Principal{{Integration: uuid.New()}, {Integration: uuid.New(), For: w.persons["aa"]}} {
			r := httptest.NewRequest("GET", "/", nil)
			_, err := w.e.srv.allow(r, auth.Project{Name: testProject, Principal: p, Role: auth.RoleAdmin}, *rt.need)
			if code := errCodeOf(err); code != codeIntegrationRefused {
				t.Errorf("%s: an integration: %v", rt.pattern, err)
			}
		}
	}
	// The move refuses an integration by itself too.
	r := httptest.NewRequest("POST", "/api/v1/project/move", strings.NewReader(`{"account":"default"}`))
	r = r.WithContext(auth.WithProject(r.Context(), auth.Project{Name: testProject,
		Principal: auth.Principal{Integration: uuid.New(), For: w.persons["aa"]}, Role: auth.RoleAdmin}))
	if err := w.e.srv.moveOwnProject(httptest.NewRecorder(), r); errCodeOf(err) != codeIntegrationRefused {
		t.Errorf("the move of an integration: %v", err)
	}

	// The operators' routes: an operator's session and Basic; a person's
	// session is no operator's, a token and an integration no admin.
	for _, c := range []struct {
		name string
		rq   request
		want int
	}{
		{"operator", as(w.op, "", "GET", "/api/v1/admin/accounts", nil), http.StatusOK},
		{"basic", request{method: "GET", path: "/api/v1/admin/accounts", header: http.Header{"Authorization": {adminAuth()}}},
			http.StatusOK},
		{"explicit admin", as(w.aa, "", "GET", "/api/v1/admin/accounts", nil), http.StatusForbidden},
		{"project token", byToken(w.e.projectToken, "GET", "/api/v1/admin/accounts", nil), http.StatusUnauthorized},
		{"integration", request{method: "GET", path: "/api/v1/admin/accounts",
			header: http.Header{"Authorization": {w.jwt}}}, http.StatusUnauthorized},
	} {
		if status, text := w.statusOf(c.rq); status != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, status, text, c.want)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// errCodeOf is an API error's code, else its text.
func errCodeOf(err error) string {
	if err == nil {
		return ""
	}
	var b struct{ Error, Code string }
	rec := httptest.NewRecorder()
	writeError(rec, err)
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if b.Code != "" {
		return b.Code
	}
	return b.Error
}

// The operators make, change, close, reopen and delete accounts and add
// their people; default is never closed or deleted and holds no admin;
// an account a project was on is not deleted; an account keeps an admin.
func TestAdminAccounts(t *testing.T) {
	t.Parallel()
	w := newAccountWorld(t)
	e := w.e
	do := func(method, path string, body any) (int, string) {
		t.Helper()
		return w.statusOf(request{method: method, path: path, body: body, header: http.Header{"Authorization": {adminAuth()}}})
	}
	check := func(gotStatus int, gotText string, want int, wantText string) {
		t.Helper()
		if gotStatus != want || (wantText != "" && gotText != wantText) {
			t.Fatalf("%d %s, want %d %s", gotStatus, gotText, want, wantText)
		}
	}
	st, txt := do("POST", "/api/v1/admin/accounts", map[string]string{"name": "Bad Name"})
	check(st, txt, http.StatusBadRequest, "account name must match ^[a-z0-9][a-z0-9.-]{1,62}$")
	st, txt = do("POST", "/api/v1/admin/accounts", map[string]string{"name": testAccount})
	check(st, txt, http.StatusConflict, CodeNameTaken)
	resp := e.do("POST", "/api/v1/admin/accounts", adminAuth(), map[string]string{"name": "spare"})
	expect(t, resp, http.StatusCreated, "")
	if got := keysInOrder(t, resp.Body.Bytes()); strings.Join(got, ",") !=
		"name,title,reference,createdAt,madeBy,closed,projects,members" {
		t.Fatalf("keys %v", got)
	}
	if a := decode[adminAccountView](t, resp); a.MadeBy != "basic:"+testAdmin || a.Closed || a.Projects != 0 {
		t.Fatalf("made %+v", a)
	}

	resp = e.do("GET", "/api/v1/admin/accounts", adminAuth(), nil)
	expect(t, resp, http.StatusOK, "")
	list := decode[struct{ Accounts []adminAccountView }](t, resp).Accounts
	if len(list) != 3 || list[0].Name != testAccount || list[0].Members != 3 || list[0].Reference != "CC 4711" ||
		list[1].Name != "default" || list[1].Projects != 2 || list[2].Name != "spare" {
		t.Fatalf("accounts %+v", list)
	}
	w.fixture(t, "GET_admin_accounts.200", request{method: "GET", path: "/api/v1/admin/accounts"}, resp,
		"every account with the number of projects on it and of its people")

	resp = e.do("PATCH", "/api/v1/admin/accounts/"+testAccount, adminAuth(), map[string]any{"title": "Team Z",
		"closed": true})
	expect(t, resp, http.StatusOK, "")
	if a := decode[adminAccountView](t, resp); a.Title != "Team Z" || a.Reference != "CC 4711" || !a.Closed ||
		a.ClosedAt == "" {
		t.Fatalf("closed %+v", a)
	}
	// A closed account takes no project, new or moved; it may invite.
	closed := `account cc-4711 is closed: it takes no new project`
	resp = e.do("POST", "/api/v1/admin/projects", adminAuth(), map[string]string{"name": "teamz", "account": testAccount})
	expect(t, resp, http.StatusConflict, `{"error":"`+closed+`","code":"account_closed"}`+"\n")
	resp = e.serve(as(w.aa, "", "POST", "/api/v1/projects", map[string]string{"name": "teamz", "account": testAccount}))
	expect(t, resp, http.StatusConflict, "")
	resp = e.do("POST", "/api/v1/admin/projects/"+testProject+"/move", adminAuth(), map[string]string{"account": testAccount})
	expect(t, resp, http.StatusConflict, `{"error":"`+closed+`","code":"account_closed"}`+"\n")
	expect(t, e.serve(as(w.aa, "", "POST", "/api/v1/accounts/"+testAccount+"/invitations",
		map[string]string{"role": "member"})), http.StatusCreated, "")
	resp = e.do("PATCH", "/api/v1/admin/accounts/"+testAccount, adminAuth(), map[string]any{"closed": false})
	expect(t, resp, http.StatusOK, "")
	if a := decode[adminAccountView](t, resp); a.Closed || a.ClosedAt != "" {
		t.Fatalf("reopened %+v", a)
	}
	st, txt = do("PATCH", "/api/v1/admin/accounts/default", map[string]any{"closed": true})
	check(st, txt, http.StatusUnprocessableEntity, CodeDefaultAccount)
	st, txt = do("PATCH", "/api/v1/admin/accounts/nope", map[string]any{"title": "x"})
	check(st, txt, http.StatusNotFound, "no such account")
	resp = e.do("PATCH", "/api/v1/admin/accounts/default", adminAuth(), map[string]any{"title": "Everyone"})
	expect(t, resp, http.StatusOK, "")

	// An operator's project on an account; the list by account.
	resp = e.do("POST", "/api/v1/admin/projects", adminAuth(), map[string]string{"name": "teamz", "account": testAccount})
	expect(t, resp, http.StatusCreated, "")
	if a := decode[struct{ Account accountRef }](t, resp).Account; a != (accountRef{Name: testAccount, Title: "Team Z"}) {
		t.Fatalf("on %+v", a)
	}
	st, txt = do("POST", "/api/v1/admin/projects", map[string]string{"name": "lost", "account": "nope"})
	check(st, txt, http.StatusNotFound, "no such account")
	resp = e.do("GET", "/api/v1/admin/projects?account="+testAccount, adminAuth(), nil)
	expect(t, resp, http.StatusOK, "")
	ps := decode[struct {
		Projects []struct {
			Name    string
			Account accountRef
		}
	}](t, resp).Projects
	if len(ps) != 1 || ps[0].Name != "teamz" || ps[0].Account.Name != testAccount {
		t.Fatalf("on cc-4711 %+v", ps)
	}
	resp = e.do("GET", "/api/v1/admin/projects", adminAuth(), nil)
	if !strings.Contains(resp.Body.String(), `"account":{"name":"default","title":"Everyone"}`) {
		t.Fatalf("list %s", resp.Body)
	}

	st, txt = do("DELETE", "/api/v1/admin/accounts/"+testAccount, nil)
	check(st, txt, http.StatusConflict, CodeAccountInUse)
	st, txt = do("DELETE", "/api/v1/admin/accounts/default", nil)
	check(st, txt, http.StatusUnprocessableEntity, CodeDefaultAccount)
	st, txt = do("DELETE", "/api/v1/admin/accounts/nope", nil)
	check(st, txt, http.StatusNotFound, "no such account")
	expect(t, e.do("DELETE", "/api/v1/admin/accounts/spare", adminAuth(), nil), http.StatusOK, `{"deleted":true}`+"\n")

	// People: any person FeRun knows; default has no admin; the last
	// admin stays.
	aa, pi := w.persons["aa"].String(), w.persons["pi"].String()
	st, txt = do("PUT", "/api/v1/admin/accounts/"+testAccount+"/members/"+uuid.New().String(),
		map[string]string{"role": "member"})
	check(st, txt, http.StatusNotFound, "no such person")
	st, txt = do("PUT", "/api/v1/admin/accounts/default/members/"+pi, map[string]string{"role": "admin"})
	check(st, txt, http.StatusUnprocessableEntity, CodeDefaultAccount)
	st, txt = do("PUT", "/api/v1/admin/accounts/"+testAccount+"/members/"+pi, map[string]string{"role": "owner"})
	check(st, txt, http.StatusBadRequest, `role must be member or admin, got "owner"`)
	st, txt = do("PUT", "/api/v1/admin/accounts/"+testAccount+"/members/"+aa, map[string]string{"role": "member"})
	check(st, txt, http.StatusConflict, CodeLastAdmin)
	st, txt = do("DELETE", "/api/v1/admin/accounts/"+testAccount+"/members/"+aa, nil)
	check(st, txt, http.StatusConflict, CodeLastAdmin)
	expect(t, e.do("PUT", "/api/v1/admin/accounts/default/members/"+pi, adminAuth(), map[string]string{"role": "member"}),
		http.StatusOK, `{"person":"`+pi+`","role":"member"}`+"\n")
	expect(t, e.do("DELETE", "/api/v1/admin/accounts/default/members/"+pi, adminAuth(), nil), http.StatusOK,
		`{"deleted":true}`+"\n")
	expect(t, e.do("DELETE", "/api/v1/admin/accounts/default/members/"+pi, adminAuth(), nil), http.StatusOK,
		`{"deleted":false}`+"\n")

	resp = e.do("GET", "/api/v1/admin/accounts/"+testAccount, adminAuth(), nil)
	expect(t, resp, http.StatusOK, "")
	if p := decode[accountPage](t, resp); p.Role != "admin" || p.Reference != "CC 4711" || len(p.Members) != 3 ||
		len(p.Projects) != 1 {
		t.Fatalf("page %+v", p)
	}
	w.activity(t, "account_created", "account_changed", "account_closed", "account_reopened", "account_deleted",
		"account_member_set", "account_member_removed")
}

// activity checks that the activity log holds a row of each kind.
func (w *accountWorld) activity(t *testing.T, kinds ...string) {
	t.Helper()
	for _, k := range kinds {
		var n int
		if err := w.e.d.Pool.QueryRow(context.Background(), "SELECT count(*) FROM activity WHERE kind = $1", k).
			Scan(&n); err != nil || n == 0 {
			t.Errorf("activity %s: %d %v", k, n, err)
		}
	}
}

// An account's admin changes its people's roles and removes them, a
// member leaves, and the account keeps an admin; its page shows its
// people, its projects with maker, admins and use, and its moves, and
// never to default's implicit members.
func TestAccountPeople(t *testing.T) {
	t.Parallel()
	w := newAccountWorld(t)
	e := w.e
	acc := "/api/v1/accounts/" + testAccount
	aa, am := w.persons["aa"].String(), w.persons["am"].String()
	expect(t, e.serve(as(w.aa, "", "PUT", acc+"/members/"+am, map[string]string{"role": "owner"})),
		http.StatusBadRequest, "")
	expect(t, e.serve(as(w.aa, "", "PUT", acc+"/members/"+am, map[string]string{"role": "admin"})), http.StatusOK,
		`{"person":"`+am+`","role":"admin"}`+"\n")
	expect(t, e.serve(as(w.am, "", "PUT", acc+"/members/"+aa, map[string]string{"role": "member"})), http.StatusOK, "")
	expect(t, e.serve(as(w.am, "", "PUT", acc+"/members/"+am, map[string]string{"role": "member"})),
		http.StatusConflict, `{"error":"an account keeps an admin","code":"last_admin"}`+"\n")
	expect(t, e.serve(as(w.am, "", "DELETE", acc+"/members/"+am, nil)), http.StatusConflict, "")
	expect(t, e.serve(as(w.aa, "", "DELETE", acc+"/members/"+am, nil)), http.StatusForbidden, "")
	// default's admins are the operators, also on the person routes.
	pi := w.persons["pi"].String()
	expect(t, e.do("PUT", "/api/v1/admin/accounts/default/members/"+pi, adminAuth(), map[string]string{"role": "member"}),
		http.StatusOK, "")
	resp := e.serve(as(w.op, "", "PUT", "/api/v1/accounts/default/members/"+pi, map[string]string{"role": "admin"}))
	expect(t, resp, http.StatusUnprocessableEntity, "")
	// A member leaves.
	expect(t, e.serve(as(w.aa, "", "DELETE", acc+"/members/"+aa, nil)), http.StatusOK, `{"deleted":true}`+"\n")
	expect(t, e.serve(as(w.aa, "", "GET", acc, nil)), http.StatusNotFound, errBody("no such account"))

	// The page: a project on the account with its maker and admins.
	resp = e.serve(as(w.am, "", "POST", "/api/v1/projects", map[string]string{"name": "teamx", "account": testAccount}))
	expect(t, resp, http.StatusCreated, "")
	mkEnvRow(t, e, "teamx", "prod")
	resp = e.serve(as(w.am, "", "GET", acc, nil))
	expect(t, resp, http.StatusOK, "")
	page := decode[accountPage](t, resp)
	if page.Name != testAccount || page.Title != "Team X" || page.Reference != "CC 4711" || page.Role != "admin" ||
		page.Closed || len(page.Members) != 2 || len(page.Projects) != 1 || len(page.Moves) != 1 {
		t.Fatalf("page %+v", page)
	}
	ms := map[string]accountMemberView{}
	for _, m := range page.Members {
		ms[m.Name] = m
	}
	if ms["am"].Role != "admin" || !ms["dis"].Disabled || ms["dis"].Email != "dis@example.test" {
		t.Fatalf("members %+v", page.Members)
	}
	p := page.Projects[0]
	if p.Name != "teamx" || p.MadeBy == nil || p.MadeBy.Name != "am" || len(p.Admins) != 1 || p.Admins[0].Person != am ||
		p.Use.Environments != 1 || p.Limits.Sandboxes != nil || p.Since == "" {
		t.Fatalf("project %+v", p)
	}
	if m := page.Moves[0]; m.Project != "teamx" || m.From != nil || m.To.Name != testAccount || m.By.Name != "am" ||
		m.By.Principal != "person:"+am {
		t.Fatalf("move %+v", m)
	}
	w.fixture(t, "GET_accounts_account.200", request{method: "GET", path: acc}, resp,
		"an account's page for its people and the operators: its people, the projects on it with maker, admins and use, the projects that came and left")
	if strings.Contains(resp.Body.String(), "stage") {
		t.Fatal("the page shows what is inside a project")
	}
	// default's page is the operators', and its explicit members'.
	expect(t, e.serve(as(w.am, "", "GET", "/api/v1/accounts/default", nil)), http.StatusNotFound, "")
	expect(t, e.serve(as(w.pi, "", "GET", "/api/v1/accounts/default", nil)), http.StatusOK, "")
	resp = e.serve(as(w.op, "", "GET", "/api/v1/accounts/default", nil))
	expect(t, resp, http.StatusOK, "")
	if p := decode[accountPage](t, resp); p.Role != "admin" || len(p.Projects) != 2 || p.Projects[0].MadeBy != nil {
		t.Fatalf("default's page %+v", p)
	}
}

// An account's invitation makes whoever takes it one of its people, the
// higher of the role held and the role given; it is used once; default's
// are the operators' and of the role member only.
func TestAccountInvitationFlow(t *testing.T) {
	t.Parallel()
	w := newAccountWorld(t)
	e := w.e
	acc := "/api/v1/accounts/" + testAccount
	invite := func(cookie, account, role string) (string, string) {
		t.Helper()
		resp := e.serve(as(cookie, "", "POST", "/api/v1/accounts/"+account+"/invitations", map[string]string{"role": role}))
		expect(t, resp, http.StatusCreated, "")
		out := decode[struct{ ID, Role, ExpiresAt, Token, URL string }](t, resp)
		if out.Role != role || out.URL != testPublicURL+"/invite/"+out.Token || out.ExpiresAt == "" {
			t.Fatalf("invitation %+v", out)
		}
		return out.Token, out.ID
	}
	expect(t, e.serve(as(w.aa, "", "POST", acc+"/invitations", map[string]string{"role": "read"})),
		http.StatusBadRequest, errBody(`role must be member or admin, got "read"`))
	expect(t, e.serve(as(w.op, "", "POST", "/api/v1/accounts/default/invitations", map[string]string{"role": "admin"})),
		http.StatusUnprocessableEntity, "")
	tok, _ := invite(w.aa, testAccount, "member")
	resp := e.serve(as(w.pi, "", "GET", "/api/v1/invitations/"+tok, nil))
	expect(t, resp, http.StatusOK, "")
	if got := keysInOrder(t, resp.Body.Bytes()); strings.Join(got, ",") != "account,role,expiresAt" {
		t.Fatalf("keys %v", got)
	}
	if !strings.Contains(resp.Body.String(), `"account":{"name":"cc-4711","title":"Team X"},"role":"member"`) {
		t.Fatalf("preview %s", resp.Body)
	}
	w.fixture(t, "GET_invitations_token.account.200", request{method: "GET", path: "/api/v1/invitations/{token}"}, resp,
		"an account's invitation, to the signed-in person who holds its link")
	expect(t, e.serve(as(w.pi, "", "POST", "/api/v1/invitations/"+tok+"/accept", nil)), http.StatusOK,
		`{"account":"cc-4711","role":"member"}`+"\n")
	// Again: the same answer; to another, it is used.
	expect(t, e.serve(as(w.pi, "", "POST", "/api/v1/invitations/"+tok+"/accept", nil)), http.StatusOK,
		`{"account":"cc-4711","role":"member"}`+"\n")
	other := w.person(t, "other", false)
	expect(t, e.serve(as(other, "", "POST", "/api/v1/invitations/"+tok+"/accept", nil)), http.StatusGone,
		errBody("this invitation was used"))
	expect(t, e.serve(as(other, "", "GET", "/api/v1/invitations/"+tok, nil)), http.StatusNotFound, "")
	expect(t, e.serve(as(w.pi, "", "GET", acc, nil)), http.StatusOK, "")
	// An admin's invitation to a member makes an admin; a member's to an
	// admin keeps the admin.
	tok, _ = invite(w.aa, testAccount, "admin")
	expect(t, e.serve(as(w.am, "", "POST", "/api/v1/invitations/"+tok+"/accept", nil)), http.StatusOK,
		`{"account":"cc-4711","role":"admin"}`+"\n")
	tok, _ = invite(w.aa, testAccount, "member")
	expect(t, e.serve(as(w.aa, "", "POST", "/api/v1/invitations/"+tok+"/accept", nil)), http.StatusOK,
		`{"account":"cc-4711","role":"admin"}`+"\n")
	// Revoked, it is gone.
	tok, id := invite(w.aa, testAccount, "member")
	expect(t, e.serve(as(w.am, "", "DELETE", acc+"/invitations/"+id, nil)), http.StatusOK, `{"revoked":true}`+"\n")
	expect(t, e.serve(as(other, "", "POST", "/api/v1/invitations/"+tok+"/accept", nil)), http.StatusNotFound, "")
	resp = e.serve(as(w.aa, "", "GET", acc+"/invitations", nil))
	expect(t, resp, http.StatusOK, "")
	invs := decode[struct{ Invitations []invitationView }](t, resp).Invitations
	if len(invs) != 3 || invs[0].UsedByName != "pi" || invs[0].Environment != "" {
		t.Fatalf("invitations %+v", invs)
	}
	// A project's invitation is none of the account's.
	resp = e.serve(as(w.aa, testProject, "POST", "/api/v1/project/invitations", map[string]string{"role": "read"}))
	expect(t, resp, http.StatusCreated, "")
	pid := decode[struct{ ID string }](t, resp).ID
	expect(t, e.serve(as(w.aa, "", "DELETE", acc+"/invitations/"+pid, nil)), http.StatusNotFound, "")

	// default's invitation: its explicit member may make projects on it
	// when only operators and its people may.
	tok, _ = invite(w.op, "default", "member")
	e.srv.cfg.ProjectCreation = ProjectCreationOperators
	expect(t, e.serve(as(other, "", "POST", "/api/v1/projects", map[string]string{"name": "o1"})), http.StatusForbidden,
		errBody("on this platform only operators make projects"))
	expect(t, e.serve(as(other, "", "POST", "/api/v1/invitations/"+tok+"/accept", nil)), http.StatusOK,
		`{"account":"default","role":"member"}`+"\n")
	expect(t, e.serve(as(other, "", "POST", "/api/v1/projects", map[string]string{"name": "o1"})), http.StatusCreated, "")
}

// A person makes a project on an account they may use, default when the
// request names none; the account and its use are in the answer, the
// record and /auth/me.
func TestProjectOnAccount(t *testing.T) {
	t.Parallel()
	w := newAccountWorld(t)
	e := w.e
	create := func(cookie string, body map[string]string) *httptest.ResponseRecorder {
		return e.serve(as(cookie, "", "POST", "/api/v1/projects", body))
	}
	resp := create(w.am, map[string]string{"name": "teamx", "account": testAccount})
	expect(t, resp, http.StatusCreated, "")
	if got := keysInOrder(t, resp.Body.Bytes()); strings.Join(got, ",") != "name,createdAt,role,limits,account" {
		t.Fatalf("keys %v", got)
	}
	if a := decode[struct{ Account accountRef }](t, resp).Account; a != (accountRef{Name: testAccount, Title: "Team X"}) {
		t.Fatalf("on %+v", a)
	}
	expect(t, create(w.pi, map[string]string{"name": "pix", "account": testAccount}), http.StatusNotFound,
		errBody("no such account"))
	expect(t, create(w.pi, map[string]string{"name": "pix", "account": "nope"}), http.StatusNotFound, "")
	resp = create(w.pi, map[string]string{"name": "pix"})
	expect(t, resp, http.StatusCreated, "")
	if a := decode[struct{ Account accountRef }](t, resp).Account; a.Name != "default" {
		t.Fatalf("on %+v", a)
	}
	var d string
	if err := e.d.Pool.QueryRow(context.Background(), `SELECT detail->>'account' FROM activity
		WHERE kind = 'project_created' AND project_id = 'teamx'`).Scan(&d); err != nil || d != testAccount {
		t.Fatalf("record %q %v", d, err)
	}

	// /auth/me: the person's accounts and each project's account.
	resp = e.serve(as(w.am, "", "GET", "/auth/me", nil))
	expect(t, resp, http.StatusOK, "")
	m := decode[struct {
		Projects []fapi.MeProject
		Accounts []meAccount
	}](t, resp)
	if len(m.Accounts) != 2 || m.Accounts[0] != (meAccount{Name: testAccount, Title: "Team X", Role: "member", Explicit: true, Closed: false}) ||
		m.Accounts[1] != (meAccount{Name: "default", Title: "Default", Role: "member", Explicit: false, Closed: false}) {
		t.Fatalf("accounts %+v", m.Accounts)
	}
	on := map[string]string{}
	for _, p := range m.Projects {
		if p.Account == nil {
			t.Fatalf("project %s without its account", p.Project)
		}
		on[p.Project] = p.Account.Name
	}
	if on[testProject] != "default" || on["teamx"] != testAccount {
		t.Fatalf("projects %+v", on)
	}
	resp = e.serve(as(w.op, "", "GET", "/auth/me", nil))
	m = decode[struct {
		Projects []fapi.MeProject
		Accounts []meAccount
	}](t, resp)
	if len(m.Accounts) != 2 || m.Accounts[0].Role != "admin" || !m.Accounts[1].Explicit || len(m.Projects) != 4 {
		t.Fatalf("operator's %+v", m)
	}
	// default closed to people: the implicit member is none.
	e.srv.cfg.ProjectCreation = ProjectCreationOperators
	resp = e.serve(as(w.pi, "", "GET", "/api/v1/accounts", nil))
	expect(t, resp, http.StatusOK, `{"accounts":[]}`+"\n")
	resp = e.serve(as(w.am, "", "GET", "/api/v1/accounts", nil))
	expect(t, resp, http.StatusOK, "")
	w.fixture(t, "GET_accounts.200", request{method: "GET", path: "/api/v1/accounts"}, resp,
		"the accounts the person may put projects on or read; default with explicit false while it is open to everyone (here it is not)")
	expect(t, create(w.am, map[string]string{"name": "teamy", "account": testAccount}), http.StatusCreated, "")
	expect(t, create(w.am, map[string]string{"name": "teamd"}), http.StatusForbidden,
		errBody("on this platform only operators make projects"))
}

// A project moves to another account by an admin of it who may put
// projects there, or an operator, and nothing else of it changes; to
// where it is writes nothing; a closed account takes none, and one leaves
// a closed account as any other; every move is a row of its history.
func TestProjectMove(t *testing.T) {
	t.Parallel()
	w := newAccountWorld(t)
	e := w.e
	ctx := context.Background()
	// What a move must not change.
	tok := w.token(t, "write", w.stage)
	writer := w.person(t, "writer", false, store.Grant{Role: "write"})
	mkEnvRow(t, e, testProject, "web")
	if _, err := e.d.Pool.Exec(ctx, `INSERT INTO host_labels (zone, label, owner, env_id)
		SELECT '', 'web--acme', $1, id FROM environments WHERE owner = $1 AND name = 'web'`, testProject); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.Pool.Exec(ctx, "UPDATE projects SET limit_sandboxes = 3 WHERE id = $1", testProject); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var parts []string
		for _, q := range []string{
			"SELECT coalesce(jsonb_agg(to_jsonb(p) ORDER BY id), '[]') FROM projects p",
			"SELECT coalesce(jsonb_agg(to_jsonb(g) - 'id' ORDER BY project_id, person_id, token_hash, environment_id), '[]') FROM grants g",
			"SELECT coalesce(jsonb_agg(to_jsonb(t) - 'last_used_at' ORDER BY hash), '[]') FROM project_tokens t",
			"SELECT coalesce(jsonb_agg(to_jsonb(h) ORDER BY zone, label), '[]') FROM host_labels h",
			"SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY id), '[]') FROM environments e",
			"SELECT coalesce(jsonb_agg(to_jsonb(i) ORDER BY hash), '[]') FROM invitations i",
		} {
			var s string
			if err := e.d.Pool.QueryRow(ctx, q).Scan(&s); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, "\n")
	}
	before := snapshot()
	rows := func() int {
		t.Helper()
		var n int
		if err := e.d.Pool.QueryRow(ctx, "SELECT count(*) FROM project_accounts WHERE project_id = $1", testProject).
			Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	move := func(cookie, account string) *httptest.ResponseRecorder {
		return e.serve(as(cookie, testProject, "POST", "/api/v1/project/move", map[string]string{"account": account}))
	}

	resp := move(w.aa, testAccount)
	expect(t, resp, http.StatusOK, "")
	if got := keysInOrder(t, resp.Body.Bytes()); strings.Join(got, ",") != "project,account,from,movedAt,moved" {
		t.Fatalf("keys %v", got)
	}
	out := decode[projectMoveAnswer](t, resp)
	if !out.Moved || out.Account != (accountRef{Name: testAccount, Title: "Team X"}) || out.From.Name != "default" ||
		out.MovedAt == "" || out.Project != testProject {
		t.Fatalf("moved %+v", out)
	}
	w.fixture(t, "POST_project_move.200", request{method: "POST", path: "/api/v1/project/move",
		body: map[string]string{"account": testAccount}}, resp,
		"an admin of the project who may put projects on the account moves it there; moved false when it is there already")
	if after := snapshot(); after != before {
		t.Fatalf("a move changed more than the project's account:\n%s\nwant\n%s", after, before)
	}
	if n := rows(); n != 2 {
		t.Fatalf("%d rows", n)
	}
	// The project's token still works, its hosts and grants stay.
	expect(t, e.do("GET", "/api/v1/project?filter=environments", bearerOf(tok), nil), http.StatusOK, "")
	// To where it is: nothing written.
	resp = move(w.am, testAccount)
	expect(t, resp, http.StatusOK, "")
	if out := decode[projectMoveAnswer](t, resp); out.Moved || out.From.Name != testAccount {
		t.Fatalf("again %+v", out)
	}
	if n := rows(); n != 2 {
		t.Fatalf("%d rows after a move to where it is", n)
	}
	// A stranger to cc-4711 moves it back to default, which is open to
	// them, but cannot bring it to cc-4711.
	expect(t, move(w.pi, "default"), http.StatusOK, "")
	expect(t, move(w.pi, testAccount), http.StatusNotFound, errBody("no such account"))
	expect(t, move(w.pi, "nope"), http.StatusNotFound, errBody("no such account"))
	// A token, admin or not, is on no account.
	expect(t, e.serve(byToken(e.projectToken, "POST", "/api/v1/project/move", map[string]string{"account": "default"})),
		http.StatusForbidden, `{"error":"a token is on no account: a person or an operator moves a project",`+
			`"code":"person_required"}`+"\n")
	// A write member is no admin of the project.
	resp = move(writer, "default")
	expect(t, resp, http.StatusForbidden, "")
	// An operator, by the admin route and by the project's.
	resp = e.do("POST", "/api/v1/admin/projects/"+testProject+"/move", adminAuth(), map[string]string{"account": testAccount})
	expect(t, resp, http.StatusOK, "")
	if out := decode[projectMoveAnswer](t, resp); !out.Moved || out.From.Name != "default" {
		t.Fatalf("by the operator %+v", out)
	}
	expect(t, e.do("POST", "/api/v1/admin/projects/nope/move", adminAuth(), map[string]string{"account": "default"}),
		http.StatusNotFound, errBody("no such project"))
	// A project leaves a closed account.
	expect(t, e.do("PATCH", "/api/v1/admin/accounts/"+testAccount, adminAuth(), map[string]any{"closed": true}),
		http.StatusOK, "")
	expect(t, move(w.op, "default"), http.StatusOK, "")
	expect(t, move(w.op, testAccount), http.StatusConflict, "")

	// The history, newest first, read by every member and the project's
	// tokens; never the account's reference.
	resp = e.do("GET", "/api/v1/project/account", bearerOf(tok), nil)
	expect(t, resp, http.StatusOK, "")
	pa := decode[struct {
		Account accountRef
		Since   string
		Moves   []moveView
	}](t, resp)
	if pa.Account.Name != "default" || len(pa.Moves) != 5 || pa.Since != pa.Moves[0].At {
		t.Fatalf("account %+v", pa)
	}
	var path []string
	for _, m := range slices.Backward(pa.Moves) {
		from := "-"
		if m.From != nil {
			from = m.From.Name
		}
		path = append(path, from+">"+m.To.Name+" by "+m.By.Name)
	}
	if got := strings.Join(path, ", "); got != "->default by FeRun, default>cc-4711 by aa, cc-4711>default by pi, "+
		"default>cc-4711 by admin, cc-4711>default by op" {
		t.Fatalf("history %s", got)
	}
	if strings.Contains(resp.Body.String(), "CC 4711") {
		t.Fatal("a member reads the account's reference")
	}
	w.fixture(t, "GET_project_account.200", request{method: "GET", path: "/api/v1/project/account"}, resp,
		"the account the project is on, since when, and its accounts over time, newest first; the first came from no account")
	w.activity(t, "project_account_moved")
	// The account's page shows them as came and left.
	resp = e.serve(as(w.aa, "", "GET", "/api/v1/accounts/"+testAccount, nil))
	if p := decode[accountPage](t, resp); len(p.Moves) != 4 || len(p.Projects) != 0 {
		t.Fatalf("page %+v", p)
	}
	if after := snapshot(); after != before {
		t.Fatalf("the moves changed more than the project's account:\n%s\nwant\n%s", after, before)
	}
}

// A project made before its accounts were kept is on default since it
// was made, and moves from there.
func TestProjectAccountWithoutRow(t *testing.T) {
	t.Parallel()
	w := newAccountWorld(t)
	e := w.e
	if _, err := e.d.Pool.Exec(context.Background(), "DELETE FROM project_accounts WHERE project_id = $1",
		testProject); err != nil {
		t.Fatal(err)
	}
	resp := e.serve(as(w.aa, testProject, "GET", "/api/v1/project/account", nil))
	expect(t, resp, http.StatusOK, "")
	pa := decode[struct {
		Account accountRef
		Since   string
		Moves   []moveView
	}](t, resp)
	if pa.Account.Name != "default" || len(pa.Moves) != 1 || pa.Moves[0].From != nil || pa.Moves[0].By.Name != "FeRun" {
		t.Fatalf("account %+v", pa)
	}
	expect(t, e.serve(as(w.aa, testProject, "POST", "/api/v1/project/move", map[string]string{"account": testAccount})),
		http.StatusOK, "")
	resp = e.serve(as(w.aa, testProject, "GET", "/api/v1/project/account", nil))
	if pa := decode[struct {
		Since string
		Moves []moveView
	}](t, resp); len(pa.Moves) != 2 || pa.Moves[0].At != pa.Since || pa.Moves[1].From != nil {
		t.Fatalf("moves %+v", pa)
	}
}

// fixture checks a handler's answer against the fixture of that name in
// testdata/ferun: the same status and the same shape (keys, nested), the
// values being the test's; -update-account-fixtures rewrites it from the
// answer.
func (w *accountWorld) fixture(t *testing.T, name string, rq request, resp *httptest.ResponseRecorder, note string) {
	t.Helper()
	file := filepath.Join("testdata", "ferun", name+".json")
	u, err := url.Parse(rq.path)
	if err != nil {
		t.Fatal(err)
	}
	var reqBody json.RawMessage = []byte("null")
	if rq.body != nil {
		reqBody, _ = json.Marshal(rq.body)
	}
	f := fixture{Method: rq.method, Path: u.Path, Query: map[string][]string(u.Query()), Status: resp.Code,
		Request: reqBody, Response: json.RawMessage(bytes.TrimSpace(resp.Body.Bytes())), Note: note}
	if *updateAccountFixtures {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(struct {
			Method   string              `json:"method"`
			Path     string              `json:"path"`
			Query    map[string][]string `json:"query"`
			Status   int                 `json:"status"`
			Request  json.RawMessage     `json:"request"`
			Note     string              `json:"note"`
			Response json.RawMessage     `json:"response"`
		}{f.Method, f.Path, f.Query, f.Status, f.Request, f.Note, f.Response}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%v (go test -run %s -update-account-fixtures writes it)", err, t.Name())
	}
	var want fixture
	if err := strictDecode(b, &want); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	if want.Method != f.Method || want.Path != f.Path || want.Status != f.Status {
		t.Fatalf("%s: %s %s %d, the answer %s %s %d", file, want.Method, want.Path, want.Status, f.Method, f.Path, f.Status)
	}
	var got, exp any
	if err := json.Unmarshal(f.Response, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want.Response, &exp); err != nil {
		t.Fatal(err)
	}
	if g, x := shapeOf(got), shapeOf(exp); g != x {
		t.Fatalf("%s: the answer's shape\n%s\nwant\n%s", file, g, x)
	}
}

// shapeOf is a JSON value's shape: objects' keys with their values'
// shapes, an array as the shape of its first element.
func shapeOf(v any) string {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+":"+shapeOf(x[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		if len(x) == 0 {
			return "[]"
		}
		return "[" + shapeOf(x[0]) + "]"
	case string:
		return "s"
	case float64:
		return "n"
	case bool:
		return "b"
	case nil:
		return "null"
	}
	return "?"
}
