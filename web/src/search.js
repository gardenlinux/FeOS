// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The search's entries (⌘K, Palette.jsx): the pages the sign-in may
// open, the tenants to switch to, and the tenant's things by name or ID,
// from its summary (summary.jsx). "prod#13" or "prod 13" opens a
// deployment. Pure, so that it is tested without a browser.

import { envPath, jobPath, sandboxPath } from './routes.js';

const enc = encodeURIComponent;

// entries lists what the search offers: {key, kind, label, hint, to,
// words} with words the lower-case text it is matched against.
export function entries({ tenant, nav = [], platform = [], tenants = [], summary = {} }) {
  const out = [];
  const t = enc(tenant || '');
  for (const n of nav) {
    const to = n.view === 'environments' ? envPath(tenant) : `/tenants/${t}${n.path}`;
    // The settings' pages are tabs of Tenant settings and found by that.
    const label = n.group === 'manage' ? `Tenant settings · ${n.label}` : n.label;
    out.push({ key: 'nav/' + n.view, kind: 'page', label, hint: tenant, to, icon: n.icon });
  }
  for (const n of platform) {
    out.push({ key: 'platform/' + n.view, kind: 'page', label: n.label, hint: 'platform', to: n.path, icon: n.icon });
  }
  for (const tn of tenants) {
    if (tn.tenant === tenant) continue;
    out.push({ key: 'tenant/' + tn.tenant, kind: 'tenant', label: tn.tenant, hint: 'switch tenant', to: `/tenants/${enc(tn.tenant)}`, icon: 'building' });
  }
  for (const e of summary.environments || []) {
    out.push({ key: 'env/' + e.id, kind: 'environment', label: e.name, hint: (e.hosts || [])[0] || e.id, to: envPath(tenant, e.name), icon: 'layers' });
  }
  // A sandbox and a job are of an environment (ADR 0035), and their
  // pages its; one without (an older ferund) is found by its ID.
  for (const s of summary.sandboxes || []) {
    const e = envName(s);
    out.push({ key: 'sbx/' + s.sandboxId, kind: 'sandbox', label: s.sandboxId, hint: [e, s.owner, s.image].filter(Boolean).join(' · '),
      to: e ? sandboxPath(tenant, e, s.sandboxId) : `/tenants/${t}/sandboxes/${enc(s.sandboxId)}`, icon: 'box' });
  }
  for (const d of summary.domains || []) {
    out.push({ key: 'dom/' + (d.id || d.name), kind: 'domain', label: d.name, hint: d.state || '', to: `/tenants/${t}/domains/${enc(d.id || d.name)}`, icon: 'globe' });
  }
  for (const c of summary.credentials || []) {
    out.push({ key: 'cred/' + c.name, kind: 'credential', label: c.name, hint: c.upstream || '',
      to: `/tenants/${t}/credentials/${enc(c.name)}`, icon: 'plug' });
  }
  for (const j of summary.jobs || []) {
    const e = envName(j);
    out.push({ key: 'job/' + j.id, kind: 'job', label: j.id, hint: [e, j.image, j.phase].filter(Boolean).join(' · '),
      to: e ? jobPath(tenant, e, j.id) : `/tenants/${t}/jobs/${enc(j.id)}`, icon: 'jobs' });
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
export function search(all, q, { tenant = '', environments = [], limit = 12 } = {}) {
  const query = String(q || '').trim().toLowerCase();
  if (!query) return all.filter((e) => e.kind === 'page' || e.kind === 'tenant').slice(0, limit);
  const out = [];
  const m = query.match(DEPLOYMENT);
  if (m) {
    const env = environments.find((e) => e.name.toLowerCase() === m[1]);
    if (env) {
      out.push({ key: `dep/${env.id}/${m[2]}`, kind: 'deployment', label: `${env.name} · deployment ${m[2]}`, hint: '',
        to: envPath(tenant, env.name, 'deployments', m[2]), icon: 'history' });
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
