// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"html/template"
	"net/http"
	"strconv"
)

// The texts of the ingress's pages about visits (ADR 0054).
const (
	pageBadLink       = "This sign-in link is not valid."
	pageTooLong       = "This sign-in took too long."
	pageRejected      = "Your sign-in was not admitted to this site."
	pageNoCookies     = "Signing in needs cookies for this site."
	pageOtherBrowser  = "This sign-in was started in another browser."
	pageUsed          = "This sign-in can no longer be used."
	pageNotNow        = "Signing in is not available right now."
	pageNotHere       = "Signing in is not available for this site."
	pageSignOutAsk    = "Sign out of this site?"
	pageSignedOutText = "You are signed out of "
)

// pageCSP is the Content-Security-Policy of every page: nothing loads,
// inline style only, a form posts to its own origin alone, and no page is
// framed.
const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// page is what one of the ingress's pages says: its text, a link to begin
// again (Retry, an absolute https URL; "" none) and whether it holds the
// sign-out form.
type page struct {
	Text    string
	Retry   string
	SignOut bool
}

// pageHTML is the one template of the pages: fixed, with the texts and
// the link escaped by html/template.
var pageHTML = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Text}}</title>
<style>
body{font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;line-height:1.5;color:#1f2328;background:#fff}
a{color:#0b57d0}
button{font:inherit;padding:.4rem 1rem}
@media (prefers-color-scheme:dark){body{color:#e6e6e6;background:#16181d}a{color:#8ab4f8}}
</style>
</head>
<body>
<p>{{.Text}}</p>
{{- if .Retry}}
<p><a href="{{.Retry}}">Try again</a></p>
{{- end}}
{{- if .SignOut}}
<form method="post" action="/.ferun/sign-out"><button type="submit">Sign out</button></form>
{{- end}}
</body>
</html>
`))

// writePage answers with a page: text/html, the CSP, no caching and no
// referrer, and Retry-After in seconds when that is not 0.
func writePage(w http.ResponseWriter, status int, p page, retryAfter int) {
	var b bytes.Buffer
	if err := pageHTML.Execute(&b, p); err != nil {
		http.Error(w, p.Text, status)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(b.Len()))
	h.Set("Content-Security-Policy", pageCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	ferunHeaders(h)
	if retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(retryAfter))
	}
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes())
}

// ferunHeaders are the headers of every answer under /.ferun/ but the
// keys, and of every answer that sets or clears a cookie of the ingress's:
// no cache keeps it, and no referrer leaves it.
func ferunHeaders(h http.Header) {
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
}
