// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// TestUIFiles: a file of the build is served as it is, a directory with an
// index.html of its own as that page, everything else as the UI's index.
func TestUIFiles(t *testing.T) {
	s := &server{deps: Deps{UI: fstest.MapFS{
		"index.html":      {Data: []byte("the UI")},
		"assets/app.js":   {Data: []byte("app")},
		"docs/index.html": {Data: []byte("the architecture page")},
	}}}
	h := s.ui()
	for _, tc := range []struct {
		path     string
		code     int
		body     string
		location string
	}{
		{path: "/", code: http.StatusOK, body: "the UI"},
		{path: "/tenants/acme/environments", code: http.StatusOK, body: "the UI"},
		{path: "/assets/app.js", code: http.StatusOK, body: "app"},
		{path: "/assets/", code: http.StatusOK, body: "the UI"},
		{path: "/docs/", code: http.StatusOK, body: "the architecture page"},
		{path: "/docs", code: http.StatusMovedPermanently, location: "docs/"},
		{path: "/docs/index.html", code: http.StatusMovedPermanently, location: "./"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		res := rec.Result()
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != tc.code {
			t.Errorf("GET %s: %d, want %d", tc.path, res.StatusCode, tc.code)
			continue
		}
		if tc.location != "" {
			if got := res.Header.Get("Location"); got != tc.location {
				t.Errorf("GET %s: Location %q, want %q", tc.path, got, tc.location)
			}
			continue
		}
		if string(body) != tc.body {
			t.Errorf("GET %s: %q, want %q", tc.path, body, tc.body)
		}
	}
}
