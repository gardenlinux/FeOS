// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { attention, inWords, navCounts } from './attention.js';

const NOW = Date.parse('2026-11-06T12:00:00Z');
const H = 3600 * 1000;
const iso = (msAgo) => new Date(NOW - msAgo).toISOString();

describe('attention', () => {
  it('puts what is broken before what will break before what is worth knowing, each with its place', () => {
    const items = attention({
      tenant: 'kino',
      environments: [{ name: 'prod', openIncidents: 1, state: 'failed', deployment: 13, deployed: 12 }],
      incidents: { prod: [{ id: '12', object: 'service/api', condition: 'crash_loop', summary: 'exited 3 times', openedAt: iso(H) }] },
      sandboxes: [{ sandboxId: 'f1g6n3w9yzp', clientState: 'failed', statusDescription: 'the registry refused it', updatedAt: iso(2 * H) },
        { sandboxId: 'h3k9v2m7qpx', clientState: 'paused' }],
      domains: [{ id: 'd-2', name: 'shop.example.com', state: 'waiting-for-dns', reason: 'the CNAME points elsewhere',
        warnings: { recordMissingSince: iso(2 * H), deactivatesAt: new Date(NOW + 70 * H).toISOString() } },
      { id: 'd-1', name: 'kino.example.com', state: 'served' }],
      invitations: [{ id: 'i1', role: 'write', environmentName: 'prod', expiresAt: new Date(NOW + 20 * H).toISOString() },
        { id: 'i2', role: 'read', expiresAt: new Date(NOW + 100 * H).toISOString() }],
      quota: { limits: { sandboxes: 10 }, use: { sandboxes: 9 } },
    }, NOW);
    expect(items.map((i) => [i.severity, i.title, i.to])).toEqual([
      ['bad', 'crash_loop', '/tenants/kino/environments/prod/incidents/12'],
      ['bad', 'sandbox failed', '/tenants/kino/sandboxes/f1g6n3w9yzp'],
      ['warn', 'waiting-for-dns', '/tenants/kino/domains/d-2'],
      ['warn', 'sandboxes: 90 % of the limit', '/tenants/kino/usage'],
      ['info', 'invitation expires in 20 h', '/tenants/kino/members'],
    ]);
    expect(items[2].detail).toBe('the CNAME points elsewhere · turns inactive in 70 h');
  });

  it('names a failed deployment without an incident, and a count it could not read one by one', () => {
    const items = attention({
      tenant: 't',
      environments: [{ name: 'stage', state: 'failed', deployment: 4, deployed: 3, openIncidents: 0 },
        { name: 'prod', state: 'deployed', deployment: 2, openIncidents: 2 }],
    }, NOW);
    expect(items.map((i) => i.title)).toEqual(['deployment 4 failed', '2 open incidents']);
    expect(items[0].detail).toBe('deployment 3 keeps serving');
  });

  it('is empty when nothing needs anyone', () => {
    expect(attention({ tenant: 't', environments: [{ name: 'prod', state: 'deployed', openIncidents: 0 }] }, NOW)).toEqual([]);
  });

  it('counts for the sidebar', () => {
    const items = attention({
      tenant: 't',
      incidents: { prod: [{ id: '1', condition: 'crash_loop' }] },
      environments: [{ name: 'prod', openIncidents: 1 }],
      domains: [{ id: 'd', name: 'a.example', state: 'unbound' }],
    }, NOW);
    expect(navCounts(items, { sandboxes: 4, environments: 1, domains: 1, jobs: 0 })).toEqual({
      overview: { n: 1, tone: 'bad' },
      environments: { n: 1, tone: 'bad' },
      domains: { n: 1 },
      sandboxes: { n: 4 },
      jobs: null,
    });
  });

  it('says a time ahead coarsely', () => {
    expect(inWords(-1)).toBe('now');
    expect(inWords(12 * 60000)).toBe('in 12 min');
    expect(inWords(70 * H)).toBe('in 70 h');
    expect(inWords(100 * H)).toBe('in 4 d');
    expect(inWords(20 * H)).toBe('in 20 h');
  });
});
