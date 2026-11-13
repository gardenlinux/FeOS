// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Which build of the UI a page runs: the files under /assets/ that its
// index.html names, the entry script and the stylesheets. Vite puts a
// hash of their content into their names, so a deployment of ferund that
// changes the UI changes them, and one that does not leaves them as they
// were. A dev server's page names none: ''.
//
// doc is a document: the page's own, read when the app starts, or an
// index.html parsed with DOMParser.
export function buildOf(doc) {
  const names = [
    ...[...doc.querySelectorAll('script[type="module"][src]')].map((s) => s.getAttribute('src')),
    ...[...doc.querySelectorAll('link[rel="stylesheet"][href]')].map((l) => l.getAttribute('href')),
  ].filter((n) => n.startsWith('/assets/'));
  return [...new Set(names)].sort().join(' ');
}
