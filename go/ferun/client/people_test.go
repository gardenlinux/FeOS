// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The people methods call their routes with the headers the context and
// the options name, and decode the answers.
func TestPeopleMethods(t *testing.T) {
	type call struct{ method, path, query, project, person, body string }
	var got []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, call{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("FeRun-Project"),
			r.Header.Get("FeRun-Person"), string(b)})
		switch r.URL.Path {
		case "/auth/me":
			_, _ = w.Write([]byte(`{"kind":"integration","integration":{"name":"fecode"},"projects":[{"project":"kino",` +
				`"role":"write","environments":[{"id":"e1","name":"prod","role":"read","protected":true,"plan":true}]}],"accounts":[]}`))
		case "/api/v1/project":
			_, _ = w.Write([]byte(`{"project":"kino","role":"","environments":["e1"]}`))
		case "/api/v1/project/members":
			_, _ = w.Write([]byte(`{"members":[{"person":"p1","subject":"ada"}],"integrations":[{"integration":"fecode","role":"admin"}]}`))
		case "/api/v1/integrations":
			_, _ = w.Write([]byte(`{"integrations":[{"name":"fecode","title":"FeCode","url":"https://fecode.dev"}]}`))
		case "/api/v1/environments/prod/protected":
			_, _ = w.Write([]byte(`{"id":"e1","name":"prod","protected":true,"changed":true}`))
		case "/api/v1/environments/prod/principals":
			_, _ = w.Write([]byte(`{"principals":[{"kind":"token","id":"h","label":"ci","projectRole":"write","environmentRole":"","losesWrite":true}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, WithBearer("a.b.c"), WithProject("kino"))
	ctx := ForPerson(context.Background(), "ada")
	me, err := c.Me(ctx)
	if err != nil || me.Kind != "integration" || me.Projects[0].Environments[0].Role != "read" {
		t.Fatalf("me %+v %v", me, err)
	}
	p, err := c.Project(ctx)
	if err != nil || p.Environments[0] != "e1" {
		t.Fatalf("project %+v %v", p, err)
	}
	ms, err := c.Members(ctx)
	if err != nil || ms.Members[0].Subject != "ada" || ms.Integrations[0].Role != "admin" {
		t.Fatalf("members %+v %v", ms, err)
	}
	is, err := c.Integrations(ctx)
	if err != nil || is.Integrations[0].Title != "FeCode" {
		t.Fatalf("integrations %+v %v", is, err)
	}
	pa, err := c.SetProtected(ctx, "prod", true)
	if err != nil || !pa.Changed {
		t.Fatalf("protected %+v %v", pa, err)
	}
	ps, err := c.Principals(ctx, "prod")
	if err != nil || !ps.Principals[0].LosesWrite {
		t.Fatalf("principals %+v %v", ps, err)
	}
	if len(got) != 6 || got[1].query != "filter=environments" || got[4].method != "PUT" {
		t.Fatalf("calls %+v", got)
	}
	var body struct{ Protected *bool }
	if err := json.Unmarshal([]byte(got[4].body), &body); err != nil || body.Protected == nil || !*body.Protected {
		t.Fatalf("body %q", got[4].body)
	}
	for _, g := range got {
		if g.project != "kino" || g.person != "ada" {
			t.Fatalf("headers %+v", g)
		}
	}
	if _, err := c.Principals(ctx, "nope"); !IsNotFound(err) {
		t.Fatalf("404: %v", err)
	}
}
