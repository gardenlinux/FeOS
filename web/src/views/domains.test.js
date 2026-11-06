// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import {
  STATES, STEPS, callout, dnsNow, domainDot, domainSteps, graceEnd, isServed, mayBind, monoSplit, pickDomain,
  routing, stateLine, stateWords,
} from './domains.js';

// A domain as GET /api/v1/domains/{id} answers it (domains.Domain),
// verified, bound, routed and served; each test changes what it needs.

const NOW = Date.parse('2026-11-06T12:00:00Z');
const H = 3600 * 1000;
const at = (ms) => new Date(NOW + ms).toISOString().replace(/\.\d+Z$/, 'Z');
const TOKEN = 'k3m9q2x7v4b8n1c6p0z5w2r8t4y7u3e1';
const NO_WARN = { recordMissingSince: null, dnsMovedSince: null, deactivatesAt: null, checkFailing: '', certificateFailing: '' };

function domain(o = {}) {
  return {
    id: '0193d1a0-0000-7000-8000-000000000002', name: 'shop.example.com', labels: {},
    environment: '0193c4d0-5e6f-7a01-8b2c-3d4e5f6a7b01', verification: 'verified', state: 'served', reason: '',
    records: {
      txt: { name: '_ferun-challenge.shop.example.com', value: TOKEN }, cname: 'edge.run.example.com',
      addresses: ['203.0.113.47', '2001:db8::47'], apex: null,
    },
    found: { txt: [TOKEN], cname: ['edge.run.example.com'], addresses: ['203.0.113.47', '2001:db8::47'] },
    warnings: NO_WARN,
    certificate: { notBefore: at(-30 * H), notAfter: at(114 * H), issuedAt: at(-30 * H), renewAfter: at(50 * H), serial: '04a1' },
    createdAt: at(-48 * H), verifiedAt: at(-24 * H), boundAt: at(-24 * H), checkedAt: at(-H / 6), nextCheckAt: at(H / 5),
    ...o,
  };
}

const status = (d, env = 'prod') => Object.fromEntries(domainSteps(d, env, NOW).map((s) => [s.key, s.status]));
const subs = (d, env = 'prod') => Object.fromEntries(domainSteps(d, env, NOW).map((s) => [s.key, s.sub]));

describe('domainSteps', () => {
  it('has the six stations for every state of domains.Derive', () => {
    for (const state of STATES) {
      const steps = domainSteps(domain({ state }), 'prod', NOW);
      expect(steps.map((s) => s.key)).toEqual(STEPS);
      for (const s of steps) {
        expect(['done', 'wait', 'warn', 'bad', 'todo']).toContain(s.status);
        expect(s.sub).toBeTruthy();
      }
    }
  });

  it('served: every station passed', () => {
    expect(status(domain())).toEqual({ claimed: 'done', verified: 'done', bound: 'done', routed: 'done', certificate: 'done', served: 'done' });
    expect(subs(domain()).bound).toBe('to prod');
  });

  it('unverified waits at verified; a binding before it counts', () => {
    const d = domain({ state: 'unverified', verification: 'unverified', certificate: null, found: { txt: [], cname: [], addresses: [] } });
    expect(status(d)).toEqual({ claimed: 'done', verified: 'wait', bound: 'done', routed: 'todo', certificate: 'todo', served: 'todo' });
    expect(status({ ...d, environment: null }).bound).toBe('todo');
  });

  it('lapsed and superseded fail at verified; superseded cannot be bound', () => {
    expect(status(domain({ state: 'lapsed', verification: 'lapsed' })).verified).toBe('bad');
    const s = domainSteps(domain({ state: 'superseded', verification: 'superseded', environment: null }), '', NOW);
    expect(s[1].status).toBe('bad');
    expect(s[2]).toEqual({ key: 'bound', status: 'todo', sub: 'cannot be bound' });
    expect(s[3].status).toBe('todo');
  });

  it('unbound and unpublished need someone at bound', () => {
    expect(status(domain({ state: 'unbound', environment: null })).bound).toBe('warn');
    expect(subs(domain({ state: 'unpublished', reason: 'the environment is not public' })).bound).toBe('prod is not public');
    expect(subs(domain({ state: 'unpublished', reason: 'the environment is being deleted' })).bound).toBe('prod is being deleted');
  });

  it('waiting-for-dns says where the name points', () => {
    const d = domain({ state: 'waiting-for-dns', certificate: null, found: { txt: [TOKEN], cname: ['old-host.example.net'], addresses: ['198.51.100.9'] } });
    expect(status(d)).toEqual({ claimed: 'done', verified: 'done', bound: 'done', routed: 'warn', certificate: 'todo', served: 'todo' });
    expect(subs(d).routed).toBe('CNAME points to old-host.example.net');
    expect(subs({ ...d, found: { txt: [], cname: [], addresses: ['198.51.100.9'] } }).routed).toBe('resolves to 198.51.100.9');
    expect(subs({ ...d, found: { txt: [], cname: [], addresses: [] } }).routed).toBe('no record yet');
  });

  it('dns-moved is still served; inactive is not', () => {
    const moved = domain({ state: 'dns-moved', found: { txt: [TOKEN], cname: [], addresses: ['198.51.100.9'] } });
    expect(status(moved)).toMatchObject({ routed: 'warn', certificate: 'warn', served: 'done' });
    expect(status(domain({ state: 'inactive' }))).toMatchObject({ routed: 'bad', served: 'todo' });
  });

  it('issuing waits at the certificate, failed fails there, a failing renewal warns', () => {
    expect(status(domain({ state: 'issuing', certificate: null }))).toMatchObject({ routed: 'done', certificate: 'wait', served: 'todo' });
    expect(status(domain({ state: 'failed', certificate: null, reason: 'rate limited by the CA: …' }))).toMatchObject({ certificate: 'bad', served: 'todo' });
    expect(status(domain({ warnings: { ...NO_WARN, certificateFailing: 'rate limited' } }))).toMatchObject({ certificate: 'warn', served: 'done' });
  });

  it('a missing TXT record of a served domain warns at verified', () => {
    expect(status(domain({ warnings: { ...NO_WARN, recordMissingSince: at(-2 * H), deactivatesAt: at(30 * H) } })).verified).toBe('warn');
  });
});

describe('callout', () => {
  it('a served domain without warnings has none', () => {
    expect(callout(domain(), 'prod', NOW)).toBeNull();
  });

  it('unverified names the TXT record and the next check', () => {
    const c = callout(domain({ state: 'unverified', verification: 'unverified' }), 'prod', NOW);
    expect(c.tone).toBe('warn');
    expect(c.text).toContain(`Set the TXT record _ferun-challenge.shop.example.com to ${TOKEN}`);
    expect(c.text).toContain('FeRun looks again in 12 min.');
    expect(c.mono).toContain(TOKEN);
  });

  it('waiting-for-dns names the CNAME and the grace with what ends it', () => {
    const c = callout(domain({
      state: 'waiting-for-dns', certificate: null,
      found: { txt: [TOKEN], cname: ['old-host.example.net'], addresses: [] },
      warnings: { ...NO_WARN, dnsMovedSince: at(-2 * H), deactivatesAt: at(30 * H) },
    }), 'prod', NOW);
    expect(c.text).toMatch(/^The CNAME of shop\.example\.com points to old-host\.example\.net since /);
    expect(c.text).toContain("can't order a certificate until it points to the edge: a CNAME to edge.run.example.com.");
    expect(c.text).toContain('The grace ends in 30 h, then the domain turns inactive.');
    expect(c.mono).toContain('old-host.example.net');
  });

  it('a missing TXT record of a served domain lapses the claim', () => {
    const c = callout(domain({ warnings: { ...NO_WARN, recordMissingSince: at(-2 * H), deactivatesAt: at(30 * H) } }), 'prod', NOW);
    expect(c.tone).toBe('warn');
    expect(c.text).toContain('then the claim lapses');
  });

  it('issuing says the default host serves meanwhile', () => {
    expect(callout(domain({ state: 'issuing', certificate: null }), 'prod', NOW).text)
      .toContain('until then prod answers at its default host');
  });

  it('failed carries the order\'s error; superseded and inactive are bad', () => {
    expect(callout(domain({ state: 'failed', reason: 'rate limited by the CA: x' }), 'prod', NOW))
      .toMatchObject({ tone: 'bad', text: expect.stringContaining('Ordering the certificate failed: rate limited by the CA: x.') });
    expect(callout(domain({ state: 'superseded' }), '', NOW).tone).toBe('bad');
    expect(callout(domain({ state: 'inactive' }), 'prod', NOW).tone).toBe('bad');
    expect(callout(domain({ state: 'lapsed' }), 'prod', NOW).tone).toBe('bad');
  });

  it('a failing lookup is added to any state, and alone makes a served one warn', () => {
    const c = callout(domain({ warnings: { ...NO_WARN, checkFailing: 'lookup timed out' } }), 'prod', NOW);
    expect(c).toMatchObject({ tone: 'warn', text: 'The last lookup failed: lookup timed out; what it found before stays.' });
  });

  it('every state has words', () => {
    for (const state of STATES) {
      if (state === 'served') continue;
      expect(callout(domain({ state }), 'prod', NOW)).not.toBeNull();
    }
  });
});

describe('stateLine', () => {
  it('a served domain says nothing more', () => {
    expect(stateLine(domain(), NOW)).toBe('');
    expect(stateWords(domain())).toBe('');
  });

  it('adds the deadlines and failures', () => {
    const line = stateLine(domain({
      state: 'waiting-for-dns', warnings: { ...NO_WARN, dnsMovedSince: at(-2 * H), deactivatesAt: at(30 * H), checkFailing: 'x' },
    }), NOW);
    expect(line).toMatch(/^waiting for the name to point to the edge · resolves elsewhere since .* · turns inactive in 30 h · its lookups fail$/);
    expect(stateLine(domain({ warnings: { ...NO_WARN, recordMissingSince: at(-H), deactivatesAt: at(31 * H), certificateFailing: 'x' } }), NOW))
      .toMatch(/^TXT record missing since .* · lapses in 31 h · renewing its certificate fails$/);
  });
});

describe('graceEnd', () => {
  it('the earlier grace decides', () => {
    expect(graceEnd(NO_WARN)).toBe('');
    expect(graceEnd({ recordMissingSince: at(-H) })).toBe('the claim lapses');
    expect(graceEnd({ dnsMovedSince: at(-H) })).toBe('the domain turns inactive');
    expect(graceEnd({ recordMissingSince: at(-H), dnsMovedSince: at(-2 * H) })).toBe('the domain turns inactive');
  });
});

describe('routing and dnsNow', () => {
  it('a CNAME to the edge or the edge\'s addresses route; others do not', () => {
    expect(routing(domain()).ok).toBe(true);
    expect(routing(domain({ found: { txt: [], cname: [], addresses: ['203.0.113.47'] } })).ok).toBe(true);
    expect(routing(domain({ found: { txt: [], cname: [], addresses: ['203.0.113.47', '198.51.100.9'] } })).ok).toBe(false);
    expect(routing(domain({ found: { txt: [], cname: ['a.example.net', 'b.example.net'], addresses: ['198.51.100.9'] } })))
      .toMatchObject({ ok: false, shows: 'CNAME a.example.net → b.example.net' });
  });

  it('says what DNS shows now, or that it was not checked', () => {
    expect(dnsNow(domain())).toEqual({ txt: { ok: true, text: 'found' }, route: { ok: true, text: 'edge.run.example.com' } });
    expect(dnsNow(domain({ found: { txt: ['other'], cname: [], addresses: [] } })))
      .toEqual({ txt: { ok: false, text: 'other: other' }, route: { ok: false, text: 'nothing' } });
    expect(dnsNow(domain({ checkedAt: null })).txt.ok).toBeNull();
  });
});

describe('domainDot', () => {
  it('maps every state to a dot', () => {
    expect(domainDot('served')).toBe('running');
    expect(domainDot('issuing')).toBe('moving');
    expect(domainDot('waiting-for-dns')).toBe('unknown');
    expect(domainDot('unbound')).toBe('absent');
    expect(domainDot('failed')).toBe('failed');
    for (const s of STATES) expect(domainDot(s)).toBeTruthy();
  });
});

describe('pickDomain', () => {
  const a = domain({ id: 'a', name: 'x.example.com', verification: 'superseded', createdAt: '2026-11-01T00:00:00Z' });
  const b = domain({ id: 'b', name: 'x.example.com', verification: 'verified', createdAt: '2026-10-01T00:00:00Z' });
  const c = domain({ id: 'c', name: 'y.example.com', verification: 'unverified', createdAt: '2026-10-01T00:00:00Z' });
  it('by ID, else by name: the verified claim, else the newest', () => {
    expect(pickDomain([a, b, c], 'a')).toBe(a);
    expect(pickDomain([a, b, c], 'x.example.com')).toBe(b);
    expect(pickDomain([a, c], 'x.example.com')).toBe(a);
    expect(pickDomain([a, b], 'z.example.com')).toBeNull();
    expect(pickDomain(null, 'a')).toBeNull();
  });
});

describe('mayBind', () => {
  const admin = { role: 'admin', environments: [] };
  const envAdmin = { role: 'read', environments: [{ id: 'e1', name: 'prod', role: 'admin' }, { id: 'e2', name: 'stage', role: 'write' }] };
  it('needs admin on the environments it leaves and joins', () => {
    expect(mayBind(admin, 'e1', 'e2')).toBe(true);
    expect(mayBind(envAdmin, '', 'e1')).toBe(true);
    expect(mayBind(envAdmin, 'e1', '')).toBe(true);
    expect(mayBind(envAdmin, 'e1', 'e2')).toBe(false);
    expect(mayBind(envAdmin, 'e2', '')).toBe(false);
  });
});

describe('isServed', () => {
  it('served and dns-moved are served', () => {
    expect(STATES.filter((s) => isServed({ state: s }))).toEqual(['dns-moved', 'served']);
  });
});

describe('monoSplit', () => {
  it('splits at the names, the longest first', () => {
    expect(monoSplit('Set _x.a.example to t at a.example.', ['a.example', '_x.a.example', 't'])).toEqual([
      { text: 'Set ', mono: false }, { text: '_x.a.example', mono: true }, { text: ' to ', mono: false },
      { text: 't', mono: true }, { text: ' at ', mono: false }, { text: 'a.example', mono: true }, { text: '.', mono: false },
    ]);
    expect(monoSplit('prod is not product', ['prod']).filter((p) => p.mono)).toEqual([{ text: 'prod', mono: true }]);
    expect(monoSplit('plain', [])).toEqual([{ text: 'plain', mono: false }]);
  });
});
