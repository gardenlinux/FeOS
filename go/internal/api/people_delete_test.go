// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	fapi "github.com/gardenlinux/feos/go/ferun/api"

	"github.com/gardenlinux/feos/go/internal/store"
)

// deleteBody is DELETE /api/v1/project's body naming project.
func deleteBody(project string) fapi.DeleteProjectRequest {
	return fapi.DeleteProjectRequest{Name: project}
}

// deletedBy reads the project_deleted row of project: its principal, or
// "" without one.
func deletedBy(t *testing.T, e *env, project string) string {
	t.Helper()
	var by string
	if err := e.st.Q().QueryRow(context.Background(), `SELECT coalesce((SELECT string_agg(detail->>'by', ',')
		FROM activity WHERE kind = 'project_deleted' AND project_id = $1), '')`, project).Scan(&by); err != nil {
		t.Fatal(err)
	}
	return by
}

// deleting reports whether project is being deleted, and gone whether it
// is no more.
func deleting(t *testing.T, e *env, project string) (deleting, gone bool) {
	t.Helper()
	p, err := e.st.GetProject(context.Background(), project)
	if isNoRow(err) {
		return false, true
	}
	if err != nil {
		t.Fatal(err)
	}
	return p.Deleting(), false
}

// integrationID is the ID of the integration w.integration registered as
// name.
func (w *peopleWorld) integrationID(t *testing.T, name string) uuid.UUID {
	t.Helper()
	in, err := w.e.st.IntegrationBySubject(context.Background(), w.p.Issuer, "client-"+name)
	if err != nil {
		t.Fatal(err)
	}
	return in.ID
}

// The project's admin deletes it, in their session: 202, the project
// being deleted until its environments have gone, gone at once without
// any; recorded as theirs; the name again is 404 then. A protected
// environment does not hold an admin back: admin counts as admin there.
func TestProjectAdminDeletesProject(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	e := w.e
	if _, _, err := e.st.SetEnvironmentProtected(context.Background(), w.prod, true); err != nil {
		t.Fatal(err)
	}
	ada := w.person(t, "ada", false, store.Grant{Role: "admin"})
	resp := e.serve(asSession("DELETE", "/api/v1/project", ada, deleteBody(testProject)))
	expect(t, resp, http.StatusAccepted, `{"project":"acme","deleting":true}`+"\n")
	writeFixture(t, "DELETE_project.202", w.p.Issuer, "DELETE", "/api/v1/project", nil, deleteBody(testProject), resp)
	if d, gone := deleting(t, e, testProject); !d || gone {
		t.Fatalf("deleting %v, gone %v", d, gone)
	}
	if by := deletedBy(t, e, testProject); by != "person:"+w.persons["ada"].String() {
		t.Fatalf("recorded by %q", by)
	}
	expect(t, e.serve(asSession("DELETE", "/api/v1/project", ada, deleteBody(testProject))), http.StatusNotFound,
		errBody("no such project"))

	// A project the person made, with no environment, goes at once.
	expect(t, e.serve(as(ada, "", "POST", "/api/v1/projects", map[string]string{"name": "solo"})), http.StatusCreated, "")
	expect(t, e.serve(as(ada, "solo", "DELETE", "/api/v1/project", deleteBody("solo"))), http.StatusAccepted,
		`{"project":"solo","deleting":true}`+"\n")
	if _, gone := deleting(t, e, "solo"); !gone {
		t.Fatal("solo is still there")
	}
}

// An operator's session deletes any project; an integration with admin on
// the project deletes it for a person it names who is its admin, recorded
// as both.
func TestProjectDeletedByOperatorAndIntegration(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		call func(t *testing.T, w *peopleWorld) (request, string)
	}{
		{"operator", func(t *testing.T, w *peopleWorld) (request, string) {
			op := w.person(t, "op", true)
			return asSession("DELETE", "/api/v1/project", op, deleteBody(testProject)), "person:" + w.persons["op"].String()
		}},
		{"integration naming an admin", func(t *testing.T, w *peopleWorld) (request, string) {
			w.providerPerson(t, "ada", store.Grant{Role: "admin"})
			jwt := w.integration(t, "fecode", store.Grant{Role: "admin"})
			return asIntegration("DELETE", "/api/v1/project", jwt, testProject, "ada", deleteBody(testProject)),
				"integration:" + w.integrationID(t, "fecode").String() + "/person:" + w.persons["ada"].String()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newPeopleWorld(t, envOptions{})
			if _, _, err := w.e.st.SetEnvironmentProtected(context.Background(), w.prod, true); err != nil {
				t.Fatal(err)
			}
			rq, by := c.call(t, w)
			expect(t, w.e.serve(rq), http.StatusAccepted, `{"project":"acme","deleting":true}`+"\n")
			if d, _ := deleting(t, w.e, testProject); !d {
				t.Fatal("not being deleted")
			}
			if got := deletedBy(t, w.e, testProject); got != by {
				t.Fatalf("recorded by %q, want %q", got, by)
			}
		})
	}
}

// Everyone else is refused and nothing changes: a writer of the project
// (one with admin on every environment too), a project token, the admin
// one too, an integration naming nobody whatever its grant, an integration
// with write naming an admin; a name that is not the project's is 400
// name_mismatch, for its admin too.
func TestProjectDeletionRefused(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	e := w.e
	if _, _, err := e.st.SetEnvironmentProtected(context.Background(), w.prod, true); err != nil {
		t.Fatal(err)
	}
	body := deleteBody(testProject)
	tw := w.person(t, "tw", false, store.Grant{Role: "write"})
	ew := w.person(t, "ew", false, store.Grant{Role: "write"}, store.Grant{Role: "admin", EnvironmentID: w.stage},
		store.Grant{Role: "admin", EnvironmentID: w.prod}, store.Grant{Role: "admin", EnvironmentID: e.box})
	ada := w.person(t, "ada", false, store.Grant{Role: "admin"})
	w.providerPerson(t, "pa", store.Grant{Role: "admin"})
	alone := w.integration(t, "alone", store.Grant{Role: "admin"})
	writer := w.integration(t, "writer", store.Grant{Role: "write"})
	roleRequired := `{"error":"this needs the role admin on the project","code":"role_required","role":"admin",` +
		`"scope":"project"}` + "\n"
	for _, c := range []struct {
		name string
		rq   request
		code int
		want string
	}{
		{"project write", asSession("DELETE", "/api/v1/project", tw, body), http.StatusForbidden, roleRequired},
		{"project write, admin on every environment", asSession("DELETE", "/api/v1/project", ew, body),
			http.StatusForbidden, roleRequired},
		{"admin token", byToken(e.projectToken, "DELETE", "/api/v1/project", body), http.StatusForbidden,
			`{"error":"a project token deletes no project: a person who is its admin does, or an integration for one it names",` +
				`"code":"person_required"}` + "\n"},
		{"integration naming nobody", asIntegration("DELETE", "/api/v1/project", alone, testProject, "", body),
			http.StatusForbidden, `{"error":"an integration deletes a project only for a person it names who is the project's admin",` +
				`"code":"integration_refused"}` + "\n"},
		{"integration with write naming an admin", asIntegration("DELETE", "/api/v1/project", writer, testProject, "pa", body),
			http.StatusForbidden, roleRequired},
		{"another name", asSession("DELETE", "/api/v1/project", ada, deleteBody("other")), http.StatusBadRequest,
			`{"error":"the body names the project \"other\", the call is for acme: send {\"name\": \"acme\"}",` +
				`"code":"name_mismatch"}` + "\n"},
		{"no name", asSession("DELETE", "/api/v1/project", ada, nil), http.StatusBadRequest, ""},
	} {
		resp := e.serve(c.rq)
		if resp.Code != c.code || (c.want != "" && resp.Body.String() != c.want) {
			t.Errorf("%s: %d %s", c.name, resp.Code, resp.Body)
		}
	}
	if d, gone := deleting(t, e, testProject); d || gone {
		t.Fatalf("deleting %v, gone %v", d, gone)
	}
	if by := deletedBy(t, e, testProject); by != "" {
		t.Fatalf("recorded by %q", by)
	}
}

// /auth/me's delete is the rule the route checks: true for the project's
// admin, an operator and an integration with admin naming an admin;
// false for a writer, a project token, an integration naming nobody and
// one with write naming an admin.
func TestAuthMeDeleteProject(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	e := w.e
	if _, _, err := e.st.SetEnvironmentProtected(context.Background(), w.prod, true); err != nil {
		t.Fatal(err)
	}
	w.providerPerson(t, "pa", store.Grant{Role: "admin"})
	admin := w.integration(t, "admin", store.Grant{Role: "admin"})
	writer := w.integration(t, "writer", store.Grant{Role: "write"})
	me := func(rq request) *httptest.ResponseRecorder {
		rq.method, rq.path = "GET", "/auth/me"
		return e.serve(rq)
	}
	for _, c := range []struct {
		name string
		resp *httptest.ResponseRecorder
		want bool
	}{
		{"project admin", me(asSession("", "", w.person(t, "ada", false, store.Grant{Role: "admin"}), nil)), true},
		{"project write", me(asSession("", "", w.person(t, "tw", false, store.Grant{Role: "write"}), nil)), false},
		{"operator", me(asSession("", "", w.person(t, "op", true), nil)), true},
		{"admin token", e.do("GET", "/auth/me", bearerOf(e.projectToken), nil), false},
		{"integration naming nobody", e.do("GET", "/auth/me", bearerOf(admin), nil), false},
		{"integration naming an admin", me(asIntegration("", "", admin, "", "pa", nil)), true},
		{"integration with write naming an admin", me(asIntegration("", "", writer, "", "pa", nil)), false},
	} {
		expect(t, c.resp, http.StatusOK, "")
		var got *fapi.MeProject
		m := decode[fapi.Me](t, c.resp)
		for i := range m.Projects {
			if m.Projects[i].Project == testProject {
				got = &m.Projects[i]
			}
		}
		if got == nil || got.Delete != c.want {
			t.Errorf("%s: %s", c.name, c.resp.Body)
		}
	}
}
