// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ferunauth

import (
	"encoding/json"
	"html/template"
	"net/http"
)

// The texts of ferun-auth's pages.
const (
	msgOpenInBrowser = "Open this link in your browser."
	msgNotValid      = "This sign-in link is not valid."
	msgUnavailable   = "Signing in is not available right now."
	msgNoFerun       = "Signing in with FeRun is not available here."
	msgIssuerRefused = "The sign-in was refused by the issuer."
	msgCannotFinish  = "This sign-in cannot be finished; start again from the site."
)

// pageCSP is the Content-Security-Policy of every page: nothing but its
// inline style, no frame, no form elsewhere (as the ingress's pages).
const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>Sign-in</title>
<style>
:root { color-scheme: light dark; }
body { font: 16px/1.5 system-ui, sans-serif; margin: 0; padding: 16px; background: Canvas; color: CanvasText; }
main { max-width: 32rem; margin: 15vh auto 0; }
h1 { font-size: 1.25rem; margin: 0 0 .5rem; }
p { margin: 0; opacity: .8; }
</style>
</head>
<body>
<main>
<h1>Sign-in</h1>
<p>{{.}}</p>
</main>
</body>
</html>
`))

// noStore sets the headers of every answer that must not be kept or
// leak where it came from.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

// page answers a fixed page with a status.
func page(w http.ResponseWriter, status int, msg string) {
	h := w.Header()
	noStore(w)
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", pageCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = pageTmpl.Execute(w, msg)
}

// notFound answers anything that is not one of ferun-auth's paths.
func notFound(w http.ResponseWriter) {
	noStore(w)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("no such path\n"))
}

// jsonAnswer answers a back-channel call.
func jsonAnswer(w http.ResponseWriter, status int, v any) {
	noStore(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// seeOther sends the browser on.
func seeOther(w http.ResponseWriter, to string) {
	noStore(w)
	w.Header().Set("Location", to)
	w.WriteHeader(http.StatusSeeOther)
}
