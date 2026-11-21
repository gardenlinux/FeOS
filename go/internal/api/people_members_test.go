// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// as is a request of a person's session (cookie) in project ("": none),
// with the UI's CSRF header.
func as(cookie, project, method, path string, body any) request {
	h := http.Header{csrfHeader: {csrfValue}}
	if project != "" {
		h.Set(projectHeader, project)
	}
	return request{method: method, path: path, body: body, header: h,
		cookies: []*http.Cookie{{Name: cookieSession, Value: cookie}}}
}

// byToken is a request with a project token.
func byToken(tok, method, path string, body any) request {
	return request{method: method, path: path, body: body, header: http.Header{"Authorization": {bearerOf(tok)}}}
}

// A signed-in person makes a project and is its admin; the names the
// platform keeps are refused, --projects-per-person bounds the projects a
// person made (operators are not bound, concurrent creates do not pass
// it), and --project-creation operators keeps it to operators.
func TestPersonProjects(t *testing.T) {
	t.Parallel()
	const tz = "apps.run.example.com"
	w := newPeopleWorld(t, envOptions{zones: model.Zones{Apps: "apps.example.com", Project: tz}})
	e := w.e
	e.srv.cfg.ProjectsPerPerson = 2
	e.srv.cfg.ReservedProjectNames = []string{"fecode", "run"}
	p := w.person(t, "p", false)
	create := func(cookie, name string) *httptest.ResponseRecorder {
		return e.serve(as(cookie, "", "POST", "/api/v1/projects", map[string]string{"name": name}))
	}
	resp := create(p, "shop")
	expect(t, resp, http.StatusCreated, "")
	if got := decode[struct{ Name, Role string }](t, resp); got.Name != "shop" || got.Role != "admin" {
		t.Fatalf("created %v", got)
	}
	tn, err := e.st.GetProject(context.Background(), "shop")
	if err != nil || tn.MadeBy != w.persons["p"] || tn.Zone != tz {
		t.Fatalf("project %+v %v", tn, err)
	}
	expect(t, e.serve(as(p, "shop", "GET", "/api/v1/project", nil)), http.StatusOK, "")
	expect(t, create(p, "shop"), http.StatusConflict, errBody("project shop already exists"))
	for _, name := range []string{"operator", "admin", "api", "www", "fecode", "run"} {
		expect(t, create(p, name), http.StatusUnprocessableEntity,
			`{"error":"the project name `+name+` is reserved by the platform","code":"reserved_name"}`+"\n")
	}
	expect(t, create(p, "Shop!"), http.StatusBadRequest, "")
	expect(t, create(p, "shop2"), http.StatusCreated, "")
	expect(t, create(p, "shop3"), http.StatusUnprocessableEntity, `{"error":"the project's limit of projects is 2, `+
		`2 are in use and this asks for 1 more","code":"quota_exceeded","resource":"projects","limit":2,"use":2}`+"\n")
	op := w.person(t, "op", true)
	for _, name := range []string{"o1", "o2", "o3"} {
		expect(t, create(op, name), http.StatusCreated, "")
	}
	// An operator's project made here is a person's project: in the project
	// zone, with its maker.
	if tn, err := e.st.GetProject(context.Background(), "o1"); err != nil || tn.Zone != tz || tn.MadeBy != w.persons["op"] {
		t.Fatalf("operator's project %+v %v", tn, err)
	}
	// Concurrent creates of one person pass the bound no more than one
	// at a time does.
	q := w.person(t, "q", false)
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = create(q, "q"+string(rune('a'+i))).Code
		}()
	}
	wg.Wait()
	made := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			made++
		}
	}
	if made != 2 {
		t.Fatalf("concurrent creates: %v", codes)
	}

	e.srv.cfg.ProjectCreation = ProjectCreationOperators
	expect(t, create(w.person(t, "r", false), "r1"), http.StatusForbidden,
		errBody("on this platform only operators make projects"))
	expect(t, create(op, "o4"), http.StatusCreated, "")
	// A session needs the CSRF header.
	rq := as(p, "", "POST", "/api/v1/projects", map[string]string{"name": "x1"})
	rq.header.Del(csrfHeader)
	expect(t, e.serve(rq), http.StatusForbidden, errBody("missing X-Requested-With header"))
}

// Members see each other in their project, with the grants on what they
// may read; an admin of a scope changes the grants there; the last admin
// cannot be removed, lowered or leave; a token is an admin too.
func TestMembers(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	e := w.e
	ta, sr := w.person(t, "ta", false, store.Grant{Role: "admin"}), w.person(t, "sr", false,
		store.Grant{Role: "read", EnvironmentID: w.stage})
	ra := w.person(t, "ra", false, store.Grant{Role: "read"}, store.Grant{Role: "admin", EnvironmentID: w.stage})
	w.person(t, "pw", false, store.Grant{Role: "write", EnvironmentID: w.prod})

	type member struct {
		Person, Name, Email, Role string
		Environments              []struct{ ID, Name, Role string }
	}
	list := func(cookie string) map[string]string {
		resp := e.serve(as(cookie, testProject, "GET", "/api/v1/project/members", nil))
		expect(t, resp, http.StatusOK, "")
		out := map[string]string{}
		for _, m := range decode[struct{ Members []member }](t, resp).Members {
			gs := []string{m.Role + "@"}
			for _, g := range m.Environments {
				gs = append(gs, g.Role+"@"+g.Name)
			}
			out[m.Name] = strings.Join(gs, ",")
		}
		return out
	}
	// The stage reader sees every member, but not the grant on prod.
	if got := list(sr); got["pw"] != "@" || got["sr"] != "@,read@stage" || got["ta"] != "admin@" {
		t.Fatalf("stage reader sees %v", got)
	}
	if got := list(ta); got["pw"] != "@,write@prod" {
		t.Fatalf("admin sees %v", got)
	}
	// Another project's members are no business of this one's.
	expect(t, e.serve(as(sr, "other", "GET", "/api/v1/project/members", nil)), http.StatusNotFound,
		errBody("no such project"))

	pw := w.persons["pw"].String()
	grants := "/api/v1/project/members/" + pw + "/grants"
	put := func(cookie, path string, body map[string]string) *httptest.ResponseRecorder {
		return e.serve(as(cookie, testProject, "PUT", path, body))
	}
	// Grants by the admin of the scope, to members only.
	expect(t, put(ta, grants, map[string]string{"role": "read", "environment": "stage"}), http.StatusOK, "")
	expect(t, put(ta, "/api/v1/project/members/"+uuid.NewString()+"/grants", map[string]string{"role": "read"}),
		http.StatusNotFound, errBody("no such member"))
	expect(t, put(ta, grants, map[string]string{"role": "owner"}), http.StatusBadRequest, "")
	if got := list(ta); got["pw"] != "@,write@prod,read@stage" {
		t.Fatalf("after the grant %v", got)
	}
	expect(t, e.serve(as(ta, testProject, "DELETE", grants+"?environment="+w.stage.String(), nil)),
		http.StatusOK, `{"deleted":true}`+"\n")
	// A stage admin grants on stage, nowhere else; a stage reader not even
	// there, and prod is absent for them.
	expect(t, put(ra, grants, map[string]string{"role": "write", "environment": w.stage.String()}), http.StatusOK, "")
	expect(t, put(ra, grants, map[string]string{"role": "write", "environment": "prod"}), http.StatusForbidden,
		`{"error":"this needs the role admin on the environment","code":"role_required","role":"admin","scope":"environment"}`+"\n")
	expect(t, put(ra, grants, map[string]string{"role": "write"}), http.StatusForbidden, "")
	expect(t, e.serve(as(ra, testProject, "DELETE", grants, nil)), http.StatusForbidden, "")
	expect(t, put(sr, grants, map[string]string{"role": "read", "environment": "stage"}), http.StatusForbidden, "")
	expect(t, put(sr, grants, map[string]string{"role": "read", "environment": "prod"}), http.StatusNotFound,
		errBody("no such environment"))
	expect(t, e.serve(as(ra, testProject, "DELETE", grants+"?environment=stage", nil)), http.StatusOK,
		`{"deleted":true}`+"\n")

	// The last admin: a project a person made has them alone.
	p := w.person(t, "p", false)
	expect(t, e.serve(as(p, "", "POST", "/api/v1/projects", map[string]string{"name": "solo"})), http.StatusCreated, "")
	mine := "/api/v1/project/members/" + w.persons["p"].String() + "/grants"
	me := w.persons["p"].String()
	lastAdmin := `{"error":"project solo would have no admin left: make another member admin first","code":"last_admin"}` + "\n"
	expect(t, e.serve(as(p, "solo", "POST", "/api/v1/project/leave", nil)), http.StatusConflict, lastAdmin)
	expect(t, e.serve(as(p, "solo", "PUT", mine, map[string]string{"role": "write"})), http.StatusConflict, lastAdmin)
	expect(t, e.serve(as(p, "solo", "DELETE", mine, nil)), http.StatusConflict, lastAdmin)
	// A token of the project, admin, is an admin: then the person may go,
	// and the token is the last.
	resp := e.serve(as(p, "solo", "POST", "/api/v1/project/tokens", map[string]string{"role": "admin", "label": "ci"}))
	expect(t, resp, http.StatusCreated, "")
	minted := decode[struct{ Token, Hash, Role, MadeBy string }](t, resp)
	if minted.Role != "admin" || minted.MadeBy != "person:"+me || minted.Token == "" {
		t.Fatalf("minted %+v", minted)
	}
	expect(t, e.serve(as(p, "solo", "POST", "/api/v1/project/leave", nil)), http.StatusOK, `{"left":true}`+"\n")
	expect(t, e.serve(as(p, "solo", "GET", "/api/v1/project", nil)), http.StatusNotFound, errBody("no such project"))
	expect(t, e.serve(byToken(minted.Token, "POST", "/api/v1/project/leave", nil)), http.StatusBadRequest,
		errBody("a token does not leave its project: revoke it"))
	expect(t, e.serve(byToken(minted.Token, "DELETE", "/api/v1/project/tokens/"+minted.Hash, nil)), http.StatusConflict,
		lastAdmin)
	// The token mints a narrower one, lists and revokes it.
	resp = e.serve(byToken(minted.Token, "POST", "/api/v1/project/tokens", map[string]string{"role": "read"}))
	expect(t, resp, http.StatusCreated, "")
	ro := decode[struct{ Token, Hash string }](t, resp)
	expect(t, e.serve(byToken(ro.Token, "GET", "/api/v1/project/tokens", nil)), http.StatusForbidden, "")
	resp = e.serve(byToken(minted.Token, "GET", "/api/v1/project/tokens", nil))
	expect(t, resp, http.StatusOK, "")
	if n := len(decode[struct{ Tokens []any }](t, resp).Tokens); n != 2 {
		t.Fatalf("%d tokens", n)
	}
	expect(t, e.serve(byToken(minted.Token, "DELETE", "/api/v1/project/tokens/"+ro.Hash, nil)), http.StatusOK,
		`{"revoked":true}`+"\n")
	expect(t, e.serve(byToken(minted.Token, "POST", "/api/v1/project/tokens", map[string]string{"role": "admin",
		"environment": "nope"})), http.StatusNotFound, errBody("no such environment"))
	expect(t, e.serve(byToken(minted.Token, "POST", "/api/v1/project/tokens", nil)), http.StatusBadRequest, "")

	// A token that is admin on stage alone reaches no tokens.
	resp = e.serve(as(ta, testProject, "POST", "/api/v1/project/tokens", map[string]string{"role": "admin",
		"environment": "stage"}))
	expect(t, resp, http.StatusCreated, "")
	stageAdmin := decode[struct{ Token, Environment string }](t, resp)
	if stageAdmin.Environment != w.stage.String() {
		t.Fatalf("token scope %q", stageAdmin.Environment)
	}
	expect(t, e.serve(byToken(stageAdmin.Token, "POST", "/api/v1/project/tokens", map[string]string{"role": "read",
		"environment": "stage"})), http.StatusForbidden, "")
}

// An invitation is a single-use link, by an admin of its scope, valid 7
// days: whoever takes it first while signed in holds its grant (never
// lowering one they hold), nobody after; they are listed to the admins of
// their scope and revoked by them.
func TestInvitations(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	e := w.e
	ta := w.person(t, "ta", false, store.Grant{Role: "admin"})
	ra := w.person(t, "ra", false, store.Grant{Role: "read"}, store.Grant{Role: "admin", EnvironmentID: w.stage})
	type created struct {
		ID, Token, URL, Role, Environment, EnvironmentName, MadeBy, ExpiresAt, UsedAt, UsedByName string
	}
	invite := func(cookie, role, env string) created {
		t.Helper()
		body := map[string]string{"role": role}
		if env != "" {
			body["environment"] = env
		}
		resp := e.serve(as(cookie, testProject, "POST", "/api/v1/project/invitations", body))
		expect(t, resp, http.StatusCreated, "")
		return decode[created](t, resp)
	}
	accept := func(cookie, token string) *httptest.ResponseRecorder {
		return e.serve(as(cookie, "", "POST", "/api/v1/invitations/"+token+"/accept", nil))
	}
	preview := func(cookie, token string) *httptest.ResponseRecorder {
		return e.serve(as(cookie, "", "GET", "/api/v1/invitations/"+token, nil))
	}
	projectInv := invite(ta, "write", "")
	if projectInv.URL != testPublicURL+"/invite/"+projectInv.Token || projectInv.ID != store.HashToken(projectInv.Token) ||
		projectInv.MadeBy != "person:"+w.persons["ta"].String() || projectInv.ExpiresAt == "" {
		t.Fatalf("invitation %+v", projectInv)
	}
	stageInv := invite(ra, "read", "stage")
	if stageInv.EnvironmentName != "stage" || stageInv.Environment != w.stage.String() {
		t.Fatalf("stage invitation %+v", stageInv)
	}
	expect(t, e.serve(as(ra, testProject, "POST", "/api/v1/project/invitations", map[string]string{"role": "read"})),
		http.StatusForbidden, "")
	expect(t, e.serve(as(ra, testProject, "POST", "/api/v1/project/invitations", map[string]string{"role": "read",
		"environment": "prod"})), http.StatusForbidden, "")
	expect(t, e.serve(as(ra, testProject, "POST", "/api/v1/project/invitations", map[string]string{"role": "boss",
		"environment": "stage"})), http.StatusBadRequest, "")

	states := func(cookie string) string {
		resp := e.serve(as(cookie, testProject, "GET", "/api/v1/project/invitations", nil))
		expect(t, resp, http.StatusOK, "")
		var out []string
		for _, i := range decode[struct{ Invitations []created }](t, resp).Invitations {
			out = append(out, i.Role+"@"+i.EnvironmentName+":"+i.UsedByName)
		}
		return strings.Join(out, ",")
	}
	if got := states(ta); got != "write@:,read@stage:" {
		t.Fatalf("project admin lists %q", got)
	}
	if got := states(ra); got != "read@stage:" {
		t.Fatalf("stage admin lists %q", got)
	}

	// The first to take it holds the grant; they may take it again,
	// nobody else.
	n := w.person(t, "n", false)
	expect(t, preview(n, projectInv.Token), http.StatusOK, `{"project":"acme","role":"write","expiresAt":"`+
		projectInv.ExpiresAt+`"}`+"\n")
	expect(t, accept(n, projectInv.Token), http.StatusOK, `{"project":"acme","role":"write"}`+"\n")
	expect(t, accept(n, projectInv.Token), http.StatusOK, `{"project":"acme","role":"write"}`+"\n")
	expect(t, preview(n, projectInv.Token), http.StatusOK, "")
	x := w.person(t, "x", false)
	expect(t, accept(x, projectInv.Token), http.StatusGone, errBody("this invitation was used"))
	expect(t, preview(x, projectInv.Token), http.StatusNotFound, errBody("no such invitation"))
	expect(t, accept(x, "nope"), http.StatusNotFound, errBody("no such invitation"))
	expect(t, e.serve(as(n, testProject, "POST", "/api/v1/sandboxes", map[string]any{"environment": "box"})),
		http.StatusCreated, "")
	if got := states(ta); got != "write@:n,read@stage:" {
		t.Fatalf("after use %q", got)
	}
	// An environment's invitation gives that environment.
	expect(t, accept(x, stageInv.Token), http.StatusOK,
		`{"project":"acme","role":"read","environment":"`+w.stage.String()+`","environmentName":"stage"}`+"\n")
	expect(t, e.serve(as(x, testProject, "GET", "/api/v1/environments/stage", nil)), http.StatusOK, "")
	expect(t, e.serve(as(x, testProject, "GET", "/api/v1/environments/prod", nil)), http.StatusNotFound, "")
	// Taking one never lowers a role held.
	lower := invite(ta, "read", "")
	expect(t, accept(ta, lower.Token), http.StatusOK, `{"project":"acme","role":"admin"}`+"\n")
	// Expired.
	old := invite(ta, "read", "")
	if _, err := e.st.Q().Exec(context.Background(), "UPDATE invitations SET expires_at = now() WHERE hash = $1",
		old.ID); err != nil {
		t.Fatal(err)
	}
	expect(t, preview(x, old.Token), http.StatusNotFound, errBody("no such invitation"))
	expect(t, accept(x, old.Token), http.StatusGone, errBody("this invitation expired"))
	// Revoking: by an admin of the scope.
	a, b := invite(ta, "read", ""), invite(ta, "read", "prod")
	revoke := func(cookie, id string) *httptest.ResponseRecorder {
		return e.serve(as(cookie, testProject, "DELETE", "/api/v1/project/invitations/"+id, nil))
	}
	expect(t, revoke(ra, a.ID), http.StatusForbidden, "")
	// ra reads prod (a project read) but is no admin there; a stage
	// admin alone does not see prod's invitation at all.
	expect(t, revoke(ra, b.ID), http.StatusForbidden, "")
	sa := w.person(t, "sa", false, store.Grant{Role: "admin", EnvironmentID: w.stage})
	expect(t, revoke(sa, b.ID), http.StatusNotFound, errBody("no such invitation"))
	expect(t, revoke(ta, a.ID), http.StatusOK, `{"revoked":true}`+"\n")
	expect(t, accept(x, a.Token), http.StatusNotFound, errBody("no such invitation"))
	expect(t, e.serve(byToken(e.mintProjectToken("other"), "DELETE", "/api/v1/project/invitations/"+b.ID, nil)),
		http.StatusNotFound, errBody("no such invitation"))
	// Signed in only.
	expect(t, e.serve(request{method: "POST", path: "/api/v1/invitations/" + b.Token + "/accept"}),
		http.StatusUnauthorized, errBody("sign-in required"))
}

// An operator disables a person, whose sessions end and who reaches
// nothing; enabled again, their grants are as they were.
func TestDisablePerson(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	e := w.e
	c := w.person(t, "m", false, store.Grant{Role: "write"})
	id := w.persons["m"].String()
	resp := e.do("GET", "/api/v1/admin/people", adminAuth(), nil)
	expect(t, resp, http.StatusOK, "")
	if !strings.Contains(resp.Body.String(), `"id":"`+id+`"`) {
		t.Fatalf("people %s", resp.Body.String())
	}
	resp = e.do("POST", "/api/v1/admin/people/"+id+"/disable", adminAuth(), nil)
	expect(t, resp, http.StatusOK, "")
	if decode[personView](t, resp).DisabledAt == "" {
		t.Fatalf("not disabled: %s", resp.Body.String())
	}
	expect(t, e.serve(as(c, testProject, "GET", "/api/v1/project", nil)), http.StatusUnauthorized, "")
	expect(t, e.do("POST", "/api/v1/admin/people/"+id+"/enable", adminAuth(), nil), http.StatusOK, "")
	roles, _, err := e.st.PersonAccess(context.Background(), testProject, w.persons["m"])
	if err != nil || len(roles) != 1 || roles[0].Role != "write" {
		t.Fatalf("grants after enabling %v %v", roles, err)
	}
	expect(t, e.do("POST", "/api/v1/admin/people/"+uuid.NewString()+"/disable", adminAuth(), nil), http.StatusNotFound,
		errBody("no such person"))
	// A person who is no operator does not reach it.
	expect(t, e.serve(as(w.person(t, "z", false), "", "GET", "/api/v1/admin/people", nil)), http.StatusForbidden,
		errBody("operators only"))
}
