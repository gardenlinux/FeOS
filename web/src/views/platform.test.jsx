// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { api } from '../api.js';
import { nodeRows } from './fleet.js';
import { IngressTable, PeopleTable } from './Platform.jsx';
import { JoinHow, JoinTokensTable } from './JoinTokens.jsx';
import {
  capacityBars, certTone, drainPreview, fleetCapacity, ingressRows, ingressState, instanceVersions, issuerHost,
  joinKindOf, joinTokenBody, joinTokenTitle, knownZones, limitsBody, limitsForm, limitsText, nodeDot, nodeVersions,
  peopleRows, seenWithin, versionSpread, zoneMoveText,
} from './platformData.js';

// The platform's pages: the nodes' bookings and a drain's preview, the
// ingresses, join tokens of two kinds, a tenant's limits and zone, the
// people, the control plane's versions; from the admin API's answers
// (go/internal/api/admin.go, members.go, quotas.go).

const GiB = 2 ** 30;
const NODE = {
  node: 'n1', status: 'ready', cpuNumcores: 16, memoryTotalbytes: String(64 * GiB), memAvailBytes: String(40 * GiB),
  slots: 32, vcpusCapacity: 64, memoryCapacityBytes: String(60 * GiB), cpuOvercommit: 4, sandboxesRunning: 10,
  sandboxesPaused: 4, sandboxesBooked: 12, vcpusBooked: 56, memoryBookedBytes: String(60 * GiB), objectStore: true,
  versions: { feosd: '0.6.1', kernel: '6.12', firecracker: ['1.10.1'], guest: ['0.6.1', '0.6.0'], diskd: '' },
};
const row = (n) => nodeRows([n])[0];

describe('nodeDot', () => {
  it('words the statuses, a drain amber and dashed, down and removed over a drain', () => {
    expect(nodeDot(row(NODE))).toMatchObject({ cls: 'running', word: 'ready' });
    expect(nodeDot(row({ ...NODE, draining: true }))).toMatchObject({ cls: 'draining', word: 'draining' });
    expect(nodeDot(row({ ...NODE, status: 'down', draining: true }))).toMatchObject({ cls: 'failed', word: 'down' });
    expect(nodeDot(row({ ...NODE, status: 'removed' }))).toMatchObject({ cls: 'absent', word: 'removed' });
    expect(nodeDot(row({ node: 'x', status: 'joined' }))).toMatchObject({ cls: 'paused', word: 'joined' });
  });
});

describe('capacityBars', () => {
  it('books vCPUs, memory and slots against the bounds, amber from 85%, red when full', () => {
    const [v, m, s] = capacityBars(row(NODE));
    expect(v).toMatchObject({ text: '56 / 64', pct: 88, tone: 'warn' });
    expect(m).toMatchObject({ text: '60.0 / 60.0 GiB', pct: 100, tone: 'bad' });
    expect(s).toMatchObject({ text: '12 / 32', pct: 38, tone: '' });
  });

  it('has no vCPU bound when CPU is not gated, and a bound of 0 is full', () => {
    const [v, , s] = capacityBars(row({ ...NODE, vcpusCapacity: 0, slots: 0 }));
    expect(v).toMatchObject({ text: '56 / ∞', share: null, pct: 0, tone: '' });
    expect(s).toMatchObject({ share: 1, tone: 'bad' });
  });

  it('sums the fleet over ready nodes that do not drain', () => {
    const rows = nodeRows([NODE, { ...NODE, node: 'n2' }, { ...NODE, node: 'n3', draining: true }, { ...NODE, node: 'n4', status: 'down' }]);
    const f = fleetCapacity(rows);
    expect(f.nodes).toBe(2);
    expect(f.bars[0].text).toBe('112 / 128');
    expect(f.bars[2].text).toBe('24 / 64');
    expect(fleetCapacity(nodeRows([NODE, { ...NODE, node: 'n2', vcpusCapacity: 0 }])).bars[0].text).toBe('112 / ∞');
    expect(fleetCapacity([]).bars).toEqual([]);
  });
});

describe('nodeVersions and versionSpread', () => {
  it('lists the known parts first, joins lists, leaves empty ones out', () => {
    expect(nodeVersions({ zeta: '1', ...NODE.versions })).toEqual([
      ['feosd', '0.6.1'], ['kernel', '6.12'], ['firecracker', '1.10.1'], ['guest', '0.6.1, 0.6.0'], ['zeta', '1'],
    ]);
    expect(nodeVersions(null)).toEqual([]);
    expect(nodeVersions([])).toEqual([]);
  });

  it('counts feosd versions over the nodes not removed, the most common first', () => {
    const nodes = [NODE, { ...NODE, node: 'b' }, { ...NODE, node: 'c', versions: { feosd: '0.6.0' } },
      { ...NODE, node: 'd', status: 'removed', versions: { feosd: '0.5.0' } }, { node: 'e', status: 'joined' }];
    expect(versionSpread(nodes)).toEqual([{ version: '0.6.1', count: 2 }, { version: '0.6.0', count: 1 }]);
  });
});

describe('drainPreview', () => {
  it('says how many sandboxes move and what becomes of them', () => {
    const p = drainPreview(row(NODE), '2 min');
    expect(p.pods).toBe(14);
    expect(p.lines[0]).toBe('14 sandboxes on the node (10 running, 4 paused) hibernate into the object store.');
    expect(p.lines.join(' ')).toContain('--drain-timeout (2 min) are cancelled');
    expect(p.lines.join(' ')).toContain('Placement booked 12 sandboxes on it');
    expect(p.blocked).toBe('');
  });

  it('says that nothing moves from an empty node', () => {
    const p = drainPreview(row({ ...NODE, sandboxesRunning: 0, sandboxesPaused: 0, sandboxesBooked: 0 }));
    expect(p.lines[0]).toBe('No sandbox runs or sleeps on the node: nothing moves.');
    expect(p.lines.join(' ')).not.toContain('Placement booked');
  });

  it('blocks a drain of a node without an object store', () => {
    expect(drainPreview(row({ ...NODE, objectStore: false })).blocked).toContain('no object store');
  });
});

describe('the ingresses', () => {
  const G = { name: 'b', status: 'joined', connected: true, lastSeen: '2026-11-06T10:00:00Z', leftAt: '' };

  it('words connected, left, not connected, joined and removed', () => {
    expect(ingressState(G).word).toBe('connected');
    expect(ingressState({ ...G, connected: false, leftAt: '2026-11-06T10:00:00Z' }).word).toBe('left');
    expect(ingressState({ ...G, connected: false }).word).toBe('not connected');
    expect(ingressState({ ...G, connected: false, lastSeen: '' }).word).toBe('joined');
    expect(ingressState({ ...G, status: 'removed', connected: false }).cls).toBe('absent');
  });

  it('orders the removed ones last, else by name', () => {
    const rows = ingressRows([{ ...G, name: 'c', status: 'removed' }, G, { ...G, name: 'a' }]);
    expect(rows.map((g) => g.name)).toEqual(['a', 'b', 'c']);
  });

  it('tones a certificate by its expiry', () => {
    const now = Date.parse('2026-11-06T12:00:00Z');
    expect(certTone('2026-11-06T11:00:00Z', now)).toBe('bad');
    expect(certTone('2026-11-06T20:00:00Z', now)).toBe('warn');
    expect(certTone('2026-11-09T12:00:00Z', now)).toBe('');
    expect(certTone('', now)).toBe('');
  });

  it('lists name, state, version, mesh address and certificate, and a remove only for one not removed', () => {
    const rows = ingressRows([{ ...G, name: 'ingress-0', version: 'v0.9.2', meshAddress: 'fd7a::1', certNotAfter: '2030-01-01T00:00:00Z' },
      { ...G, name: 'ingress-old', status: 'removed', connected: false }]);
    const out = renderToStaticMarkup(<IngressTable rows={rows} armed="" arm={() => {}} disarm={() => {}} onRemove={() => {}} />);
    expect(out).toContain('>ingress-0</td>');
    expect(out).toContain('>v0.9.2</td>');
    expect(out).toContain('<span class="net">fd7a::1</span>');
    expect(out).toContain('>revoked</span>');
    expect(out.match(/>Remove</g)).toHaveLength(1);
  });
});

describe('join tokens', () => {
  it('reads the kind from the query, a node by default', () => {
    expect(joinKindOf('?kind=ingress')).toBe('ingress');
    expect(joinKindOf('?kind=other')).toBe('node');
    expect(joinKindOf('')).toBe('node');
  });

  it('makes the create body with the kind and the name it is pinned to', () => {
    expect(joinTokenBody('ingress', ' ingress-2 ', 3600)).toEqual({ ttlSeconds: 3600, kind: 'ingress', name: 'ingress-2' });
    expect(joinTokenBody('x', '', 0)).toEqual({ ttlSeconds: 0, kind: 'node', name: '' });
  });

  it('names a fresh token by kind and name', () => {
    expect(joinTokenTitle({ kind: 'ingress', name: 'ingress-2' })).toBe('Join token for ingress ingress-2');
    expect(joinTokenTitle({ node: 'n7' })).toBe('Join token for node n7');
    expect(joinTokenTitle({ kind: 'node' })).toBe('Join token for a new node');
  });

  it('shows each token\'s kind; an older ferund\'s tokens without one are a node\'s', () => {
    const out = renderToStaticMarkup(<JoinTokensTable armed="" arm={() => {}} disarm={() => {}} onRevoke={() => {}} tokens={[
      { id: '11111111-aaaa', kind: 'ingress', node: 'ingress-2', expiresAt: '2099-01-01T00:00:00Z' },
      { id: '22222222-bbbb', node: '', expiresAt: '2099-01-01T00:00:00Z' },
    ]} />);
    expect(out).toContain('<span class="kindtag ingress">ingress</span>');
    expect(out).toContain('<span class="kindtag node">node</span>');
    expect(out).toContain('any new name');
  });

  it('tells where an ingress token goes', () => {
    expect(renderToStaticMarkup(<JoinHow kind="ingress" />)).toContain('<code>ferun-ingress-join</code>');
    expect(renderToStaticMarkup(<JoinHow kind="node" />)).toContain('feos-k8s');
  });
});

describe('a tenant\'s limits and zone', () => {
  it('fills the form with none as empty and 0 as a limit', () => {
    expect(limitsForm({ sandboxes: 0, memoryMib: 4096 }))
      .toEqual({ sandboxes: '0', environments: '', runningJobs: '', vcpus: '', memoryMib: '4096' });
  });

  it('sends every limit, null for none, 0 kept', () => {
    expect(limitsBody({ sandboxes: '0', environments: ' 3 ', runningJobs: '', vcpus: '', memoryMib: '8192' }))
      .toEqual({ sandboxes: 0, environments: 3, runningJobs: null, vcpus: null, memoryMib: 8192 });
    expect(() => limitsBody({ sandboxes: '-1' })).toThrow('sandboxes must be a whole number ≥ 0');
    expect(() => limitsBody({ vcpus: '1.5' })).toThrow('vCPUs');
  });

  it('sums the limits up in a line', () => {
    expect(limitsText({})).toBe('no limits');
    expect(limitsText({ sandboxes: 50, environments: 0, memoryMib: 65536 })).toBe('50 sandboxes · 0 envs · 64 GiB');
  });

  it('knows the zones the tenants name', () => {
    expect(knownZones([{ zone: 'b.example' }, { zone: 'a.example' }, { zone: '' }, { zone: 'b.example' }]))
      .toEqual(['a.example', 'b.example']);
  });

  it('says what a move did to a host', () => {
    expect(zoneMoveText({ hostLabel: 'kino', hostLabelBefore: 'kino', host: 'kino.u.example' })).toBe('now kino.u.example');
    expect(zoneMoveText({ hostLabel: 'stage--kino', hostLabelBefore: 'stage', host: 'stage--kino.u.example' }))
      .toBe('label stage → stage--kino, now stage--kino.u.example');
  });
});

describe('the people', () => {
  const P = [
    { id: 'p1', name: 'Malte', email: 'malte@example.com', issuer: 'https://id.example.com', sub: 'u1', lastSeen: '2026-11-06T11:00:00Z' },
    { id: 'p2', name: '', email: 'ex@example.org', issuer: 'https://login.example.org/realms/x', sub: 'u2', lastSeen: '2026-10-01T00:00:00Z', disabledAt: '2026-10-02T00:00:00Z' },
  ];

  it('filters by a search and by state', () => {
    expect(peopleRows(P, 'EXAMPLE.ORG').map((p) => p.id)).toEqual(['p2']);
    expect(peopleRows(P, '', 'disabled').map((p) => p.id)).toEqual(['p2']);
    expect(peopleRows(P, '', 'active').map((p) => p.id)).toEqual(['p1']);
    expect(peopleRows(P).length).toBe(2);
  });

  it('shows an issuer by its host, and counts who was seen lately', () => {
    expect(issuerHost('https://login.example.org/realms/x')).toBe('login.example.org');
    expect(issuerHost('not a url')).toBe('not a url');
    expect(seenWithin(P, 7 * 86400000, Date.parse('2026-11-06T12:00:00Z'))).toBe(1);
  });

  it('disables with two clicks, enables a disabled one, and not the operator themselves', () => {
    const render = (armed) => renderToStaticMarkup(
      <PeopleTable rows={P} me={{ person: 'p1' }} armed={armed} arm={() => {}} disarm={() => {}} onToggle={() => {}} />);
    const out = render('');
    expect(out).toMatch(/<button class="mini danger" disabled="" title="that is you: it would end your own session">Disable<\/button>/);
    expect(out).toContain('>Enable</button>');
    expect(out).toContain('>disabled <');
    expect(render('p:p2')).toContain('>enable?</button>');
  });
});

describe('the control plane', () => {
  it('counts the versions of the instances that are fresh', () => {
    expect(instanceVersions([
      { version: 'v2', fresh: true, live: true }, { version: 'v2', fresh: true, live: true },
      { version: 'v1', fresh: true, live: false }, { version: 'v0', fresh: false, live: false },
    ])).toEqual([{ version: 'v2', count: 2 }, { version: 'v1', count: 1 }]);
  });
});

describe('mintTenantToken', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('sends the role and the environment, and leaves out what is empty', async () => {
    const calls = [];
    vi.stubGlobal('sessionStorage', { getItem: () => null, setItem: () => {}, removeItem: () => {} });
    vi.stubGlobal('fetch', vi.fn(async (path, init) => {
      calls.push({ path, init });
      return { ok: true, status: 201, text: async () => '{"token":"t"}' };
    }));
    await api.mintTenantToken('acme', { label: 'ci', role: 'read', environment: 'prod' });
    await api.mintTenantToken('acme', { label: 'x' });
    expect(calls[0].path).toBe('/api/v1/admin/tenants/acme/tokens');
    expect(JSON.parse(calls[0].init.body)).toEqual({ label: 'ci', role: 'read', environment: 'prod' });
    expect(JSON.parse(calls[1].init.body)).toEqual({ label: 'x' });
  });
});
