// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// What needs a person in a tenant: Home's first list and the sidebar's
// counts. Put together in the browser from the lists the tenant's pages
// read anyway (summary.jsx): the environments' open incidents, the
// domains' states and warnings, the sandboxes that failed, the
// invitations about to expire and the limits nearly reached. Each item
// names where it is fixed.

import { invitationState, quotaRows } from './people.js';

const enc = encodeURIComponent;

// SEVERITY orders the items: what is broken before what will break
// before what is merely worth knowing.
export const SEVERITY = { bad: 0, warn: 1, info: 2 };

// DOMAIN_TROUBLE are the domain states that need someone, with the
// severity and the action that fixes them (domains.Derive's states, ADR
// 0021); served and issuing need no one.
const DOMAIN_TROUBLE = {
  unverified: ['warn', 'Show records'],
  lapsed: ['bad', 'Show records'],
  superseded: ['bad', 'Open domain'],
  unbound: ['info', 'Bind'],
  unpublished: ['info', 'Open domain'],
  inactive: ['bad', 'Show records'],
  'dns-moved': ['warn', 'Show records'],
  'waiting-for-dns': ['warn', 'Show records'],
  failed: ['bad', 'Open domain'],
};

// INVITATION_SOON is how close to its end an open invitation shows.
const INVITATION_SOON_MS = 48 * 3600 * 1000;

function ms(iso) {
  const t = Date.parse(iso || '');
  return Number.isNaN(t) ? null : t;
}

// inWords is a duration ahead, coarse: "in 70 h", "in 12 min".
export function inWords(deltaMs) {
  if (deltaMs <= 0) return 'now';
  const min = Math.round(deltaMs / 60000);
  if (min < 60) return `in ${Math.max(1, min)} min`;
  const h = Math.round(min / 60);
  if (h < 72) return `in ${h} h`;
  return `in ${Math.round(h / 24)} d`;
}

// attention is the list of items, most severe first, then newest first.
// in: {tenant, environments, incidents: {env name: [open incident]},
// sandboxes, domains, invitations, quota}; each may be missing (not
// read, or not offered to the role).
export function attention(input, now = Date.now()) {
  const t = enc(input.tenant || '');
  const items = [];

  for (const env of input.environments || []) {
    const open = (input.incidents && input.incidents[env.name]) || [];
    for (const inc of open) {
      items.push({
        key: `incident/${env.name}/${inc.id}`,
        severity: 'bad',
        title: inc.condition,
        object: `${env.name}${inc.object ? ' · ' + inc.object : ''}`,
        detail: inc.summary || '',
        since: inc.openedAt || '',
        to: `/tenants/${t}/environments/${enc(env.name)}/incidents/${enc(inc.id)}`,
        action: 'Open incident',
      });
    }
    if (open.length === 0 && Number(env.openIncidents) > 0) {
      items.push({
        key: `incidents/${env.name}`,
        severity: 'bad',
        title: `${env.openIncidents} open incident${env.openIncidents > 1 ? 's' : ''}`,
        object: env.name,
        detail: '',
        since: '',
        to: `/tenants/${t}/environments/${enc(env.name)}/incidents`,
        action: 'Open incidents',
      });
    }
    if (env.state === 'failed' && open.length === 0 && !(Number(env.openIncidents) > 0)) {
      items.push({
        key: `deployment/${env.name}/${env.deployment}`,
        severity: 'bad',
        title: `deployment ${env.deployment} failed`,
        object: env.name,
        detail: env.deployed ? `deployment ${env.deployed} keeps serving` : 'nothing serves',
        since: '',
        to: `/tenants/${t}/environments/${enc(env.name)}/deployments/${enc(env.deployment)}`,
        action: 'Open deployment',
      });
    }
  }

  for (const sb of input.sandboxes || []) {
    const st = sb.clientState;
    if (st !== 'failed' && st !== 'lost' && st !== 'unknown') continue;
    items.push({
      key: `sandbox/${sb.sandboxId}`,
      severity: st === 'unknown' ? 'warn' : 'bad',
      title: `sandbox ${st}`,
      object: sb.sandboxId,
      detail: sb.statusDescription || '',
      since: sb.updatedAt || '',
      // Its page is its environment's (ADR 0035); one without is found by
      // its ID.
      to: sb.environment && (sb.environment.name || sb.environment.id)
        ? `/tenants/${t}/environments/${enc(sb.environment.name || sb.environment.id)}/sandboxes/${enc(sb.sandboxId)}`
        : `/tenants/${t}/sandboxes/${enc(sb.sandboxId)}`,
      action: 'Open sandbox',
    });
  }

  for (const d of input.domains || []) {
    const trouble = DOMAIN_TROUBLE[d.state];
    if (!trouble) continue;
    const w = d.warnings || {};
    const parts = [];
    if (d.reason) parts.push(d.reason);
    const deact = ms(w.deactivatesAt);
    if (deact !== null) parts.push(`turns inactive ${inWords(deact - now)}`);
    items.push({
      key: `domain/${d.id || d.name}`,
      severity: trouble[0],
      title: d.state,
      object: d.name,
      detail: parts.join(' · '),
      since: w.recordMissingSince || w.dnsMovedSince || '',
      to: `/tenants/${t}/domains/${enc(d.id || d.name)}`,
      action: trouble[1],
    });
  }

  for (const inv of input.invitations || []) {
    if (invitationState(inv, now) !== 'open') continue;
    const exp = ms(inv.expiresAt);
    if (exp === null || exp - now > INVITATION_SOON_MS) continue;
    items.push({
      key: `invitation/${inv.id}`,
      severity: 'info',
      title: `invitation expires ${inWords(exp - now)}`,
      object: `${inv.role}${inv.environmentName || inv.environment ? ' · ' + (inv.environmentName || inv.environment) : ''}`,
      detail: 'not accepted yet',
      since: '',
      to: `/tenants/${t}/members`,
      action: 'Open people',
    });
  }

  if (input.quota) {
    for (const r of quotaRows(input.quota)) {
      if (!r.tone) continue;
      items.push({
        key: `quota/${r.key}`,
        severity: r.tone === 'bad' ? 'bad' : 'warn',
        title: r.tone === 'bad' ? `${r.label}: at the limit` : `${r.label}: ${Math.round(r.share * 100)} % of the limit`,
        object: '',
        detail: r.tone === 'bad' ? 'new ones are refused until some go' : '',
        since: '',
        to: `/tenants/${t}/usage`,
        action: 'Open usage',
      });
    }
  }

  return items.sort((a, b) => (SEVERITY[a.severity] - SEVERITY[b.severity])
    || ((ms(b.since) || 0) - (ms(a.since) || 0)));
}

// navCounts are the sidebar's numbers per view: what needs someone on
// Home, open incidents on the environments, domains in trouble.
export function navCounts(items, lists = {}) {
  const urgent = items.filter((i) => i.severity !== 'info').length;
  const incidents = items.filter((i) => i.key.startsWith('incident')).length;
  const domains = items.filter((i) => i.key.startsWith('domain/') && i.severity !== 'info').length;
  return {
    overview: urgent ? { n: urgent, tone: 'bad' } : null,
    environments: incidents ? { n: incidents, tone: 'bad' } : (lists.environments != null ? { n: lists.environments } : null),
    domains: domains ? { n: domains, tone: 'warn' } : (lists.domains != null ? { n: lists.domains } : null),
    sandboxes: lists.sandboxes != null ? { n: lists.sandboxes } : null,
    jobs: lists.jobs ? { n: lists.jobs } : null,
  };
}
