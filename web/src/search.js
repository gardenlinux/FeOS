// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The search's entries (⌘K, Palette.jsx): the pages the sign-in may
// open, the projects to switch to, and the project's things by name or ID,
// from its summary (summary.jsx). "prod#13" or "prod 13" opens a
// deployment. Pure, so that it is tested without a browser.

import { envPath, jobPath, sandboxPath } from './routes.js';

const enc = encodeURIComponent;

// entries lists what the search offers: {key, kind, label, hint, to,
// words} with words the lower-case text it is matched against.
export function entries({ project, nav = [], platform = [], projects = [], summary = {} }) {
  const out = [];
  const t = enc(project || '');
  for (const n of nav) {
    const to = n.view === 'environments' ? envPath(project) : `/projects/${t}${n.path}`;
    // The settings' pages are tabs of Project settings and found by that.
    const label = n.group === 'manage' ? `Project settings · ${n.label}` : n.label;
    out.push({ key: 'nav/' + n.view, kind: 'page', label, hint: project, to, icon: n.icon });
  }
  for (const n of platform) {
    out.push({ key: 'platform/' + n.view, kind: 'page', label: n.label, hint: 'platform', to: n.path, icon: n.icon });
  }
  for (const tn of projects) {
    if (tn.project === project) continue;
    out.push({ key: 'project/' + tn.project, kind: 'project', label: tn.project, hint: 'switch project', to: `/projects/${enc(tn.project)}`, icon: 'building' });
  }
  for (const e of summary.environments || []) {
    out.push({ key: 'env/' + e.id, kind: 'environment', label: e.name, hint: (e.hosts || [])[0] || e.id, to: envPath(project, e.name), icon: 'layers' });
  }
  // A sandbox and a job are of an environment (ADR 0035), and their
  // pages its; one without (an older ferund) is found by its ID.
  for (const s of summary.sandboxes || []) {
    const e = envName(s);
    out.push({ key: 'sbx/' + s.sandboxId, kind: 'sandbox', label: s.sandboxId, hint: [e, s.owner, s.image].filter(Boolean).join(' · '),
      to: e ? sandboxPath(project, e, s.sandboxId) : `/projects/${t}/sandboxes/${enc(s.sandboxId)}`, icon: 'box' });
  }
  for (const d of summary.domains || []) {
    out.push({ key: 'dom/' + (d.id || d.name), kind: 'domain', label: d.name, hint: d.state || '', to: `/projects/${t}/domains/${enc(d.id || d.name)}`, icon: 'globe' });
  }
  // The access providers (ADR 0054), ferun among them.
  for (const p of summary.providers || []) {
    out.push({ key: 'prov/' + p.name, kind: 'provider', label: p.name, hint: [p.kind, p.url || p.issuer].filter(Boolean).join(' · '),
      to: `/projects/${t}/providers/${enc(p.name)}`, icon: 'lock' });
  }
  for (const j of summary.jobs || []) {
    const e = envName(j);
    out.push({ key: 'job/' + j.id, kind: 'job', label: j.id, hint: [e, j.image, j.phase].filter(Boolean).join(' · '),
      to: e ? jobPath(project, e, j.id) : `/projects/${t}/jobs/${enc(j.id)}`, icon: 'jobs' });
  }
  return out.map((e) => ({ ...e, words: `${e.label} ${e.hint || ''} ${e.kind}`.toLowerCase() }));
}

// envName is the name of the environment a sandbox or a job is of, ''
// without one.
function envName(x) {
  const e = x && x.environment;
  return (e && (e.name || e.id)) || '';
}

// DEPLOYMENT matches "prod#13" and "prod 13".
const DEPLOYMENT = /^(.+?)(?:#|\s+)(\d+)$/;

// search ranks the entries for a query: a deployment's address first,
// then names that start with it, then names and hints that hold it. An
// empty query offers the pages.
export function search(all, q, { project = '', environments = [], limit = 12 } = {}) {
  const query = String(q || '').trim().toLowerCase();
  if (!query) return all.filter((e) => e.kind === 'page' || e.kind === 'project').slice(0, limit);
  const out = [];
  const m = query.match(DEPLOYMENT);
  if (m) {
    const env = environments.find((e) => e.name.toLowerCase() === m[1]);
    if (env) {
      out.push({ key: `dep/${env.id}/${m[2]}`, kind: 'deployment', label: `${env.name} · deployment ${m[2]}`, hint: '',
        to: envPath(project, env.name, 'deployments', m[2]), icon: 'history' });
    }
  }
  const starts = [];
  const holds = [];
  for (const e of all) {
    if (e.label.toLowerCase().startsWith(query)) starts.push(e);
    else if (e.words.includes(query)) holds.push(e);
  }
  return [...out, ...starts, ...holds].slice(0, limit);
}
