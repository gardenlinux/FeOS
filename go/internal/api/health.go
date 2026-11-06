// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

// readyTimeout bounds one /readyz check.
const readyTimeout = 3 * time.Second

// healthz is GET /healthz: the process serves.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// readyz is GET /readyz: whether a balancer should send requests here
// (Deps.Ready): 200 "ready", or 503 with the reason.
func (s *server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.deps.Ready != nil {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()
		if err := s.deps.Ready(ctx); err != nil {
			http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ready\n"))
}

// ui serves the management UI to GET and HEAD: a file of the build when
// the path names one, a directory's own index.html when it has one (the
// architecture page at /docs/, web/public/docs), else the UI's index.html
// (the UI routes deep links itself).
// Without a build, a plain 404 says so. (It is mounted for every method,
// because the method-less patterns of the doors make a "GET /" pattern
// ambiguous to the mux.)
func (s *server) ui() http.Handler {
	h := s.uiFiles()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (s *server) uiFiles() http.Handler {
	ui := s.deps.UI
	if ui != nil {
		if _, err := fs.Stat(ui, "index.html"); err != nil {
			ui = nil
		}
	}
	if ui == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "the management UI is not part of this build of ferund", http.StatusNotFound)
		})
	}
	files := http.FileServerFS(ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if name != "" && fs.ValidPath(name) {
			if st, err := fs.Stat(ui, name); err == nil {
				// A directory with an index.html of its own is a page
				// beside the UI; the file server answers it, and
				// redirects /docs to /docs/ so that its links resolve.
				if !st.IsDir() || hasIndex(ui, name) {
					files.ServeHTTP(w, r)
					return
				}
			}
		}
		// The index for "/", directories and every route of the UI.
		http.ServeFileFS(w, r, ui, "index.html")
	})
}

// hasIndex reports whether the directory dir of fsys holds an index.html.
func hasIndex(fsys fs.FS, dir string) bool {
	st, err := fs.Stat(fsys, dir+"/index.html")
	return err == nil && !st.IsDir()
}
