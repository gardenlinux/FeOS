// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import {
  activeTab, bucketOf, canPause, canResume, connectSnippets, createRequest, credentialsOf, filterSandboxes, fmtMem,
  newSandboxForm, ownerShown, sandboxMeta, sandboxTabs, sideSandboxes, byActivity, SIDE_FOLD, sizeWords, stateCounts, stateWords, toolSummary,
} from './sandboxes.js';

// The sandbox pages' derivations, from sandboxes as GET /api/v1/sandboxes
// renders them (go/internal/api, sandboxView).

const SB = {
  sandboxId: 'h3k9v2m7qpx', state: 'running', clientState: 'running', image: 'ghcr.io/kino/agent-base:2.4', node: 'n3',
  owner: 'kino', createdAt: '2026-11-03T10:00:00Z', updatedAt: '2026-11-06T09:00:00Z', desiredState: 'active',
  statusDescription: '', checkpointEpoch: '0', checkpointHead: '', checkpointAt: '', vcpus: 2, memoryMib: 4096,
  environment: { id: '0199f0b7-1c2d-7e3f-8a00-0000000000d1', name: 'kino-dev' }, reaches: 'all', egress: 'internet',
};
const as = (clientState, more = {}) => ({ ...SB, clientState, ...more });

describe('sizes', () => {
  it('renders whole GiB as such, the rest in MiB', () => {
    expect(fmtMem(4096)).toBe('4 GiB');
    expect(fmtMem(1536)).toBe('1536 MiB');
    expect(fmtMem(0)).toBe('');
    expect(sizeWords(SB)).toBe('2 vCPU, 4 GiB');
    expect(sizeWords({ vcpus: 1 })).toBe('1 vCPU');
    expect(sizeWords({})).toBe('');
  });
});

describe('stateWords', () => {
  it('says the API\'s word, and where it is and what it costs', () => {
    expect(stateWords(SB)).toEqual({ short: 'running', long: 'running on n3 · 2 vCPU, 4 GiB booked' });
    expect(stateWords(as('paused')).long).toBe('paused on n3 · its memory on the node\'s disk, no RAM · resumes on the next call');
    expect(stateWords(as('paused')).short).toBe('paused');
    expect(stateWords(as('hibernated', { node: '' }))).toEqual({ short: 'hibernated', long: 'hibernated in S3 · nothing on a node · resumes on any node' });
    expect(stateWords(as('lost')).long).toBe('no checkpoint · delete it or create anew');
    expect(stateWords(as('failed')).long).toBe('the next call tries to start it again, or delete it');
    expect(stateWords(as('unknown')).long).toBe('not known now · node n3 not reachable · a sync resolves it');
    expect(stateWords(as('resuming')).long).toBe('resuming on n3');
  });

  it('keeps an unnamed state as it is', () => {
    expect(stateWords(as('absent')).short).toBe('absent');
    expect(stateWords(null).short).toBe('unknown');
  });
});

describe('counts', () => {
  it('buckets the states as the list shows them', () => {
    expect(['running', 'paused', 'hibernated', 'resuming', 'pending', 'lost', 'stopped', 'odd'].map(bucketOf))
      .toEqual(['running', 'paused', 'hibernated', 'changing', 'changing', 'failed', 'other', 'other']);
    expect(stateCounts([SB, SB, as('paused'), as('failed'), as('hibernating')]))
      .toEqual({ running: 2, paused: 1, hibernated: 0, changing: 1, failed: 1, other: 0 });
    expect(stateCounts(null).running).toBe(0);
  });

  it('offers pause for a running sandbox and resume for a paused or hibernated one', () => {
    expect([canPause(SB), canResume(SB)]).toEqual([true, false]);
    expect([canPause(as('paused')), canResume(as('paused'))]).toEqual([false, true]);
    expect(canResume(as('hibernated'))).toBe(true);
    expect([canPause(as('resuming')), canResume(as('resuming'))]).toEqual([false, false]);
  });
});

describe('the list', () => {
  const list = [SB, as('paused', { sandboxId: 'q8w1z4c0dnr', node: 'n1', image: 'ghcr.io/kino/migrate:1',
    environment: { id: '0199f0b7-1c2d-7e3f-8a00-0000000000d2', name: 'kino-stage' }, reaches: [{ addon: 'db-admin' }] })];
  it('filters by environment, by its name or ID', () => {
    expect(filterSandboxes(list, '', 'kino-stage').map((s) => s.sandboxId)).toEqual(['q8w1z4c0dnr']);
    expect(filterSandboxes(list, '', '0199f0b7-1c2d-7e3f-8a00-0000000000d1').map((s) => s.sandboxId)).toEqual(['h3k9v2m7qpx']);
    expect(filterSandboxes(list, 'kino-dev').length).toBe(1);
    expect(filterSandboxes(list, 'db-admin').map((s) => s.sandboxId)).toEqual(['q8w1z4c0dnr']);
    expect(filterSandboxes(list, 'q8w1', 'kino-dev')).toEqual([]);
  });

  it('filters by ID, image, node, the state and its words', () => {
    expect(filterSandboxes(list, '').length).toBe(2);
    expect(filterSandboxes(list, 'q8w1').map((s) => s.sandboxId)).toEqual(['q8w1z4c0dnr']);
    expect(filterSandboxes(list, 'MIGRATE').length).toBe(1);
    expect(filterSandboxes(list, 'paused').length).toBe(1);
    expect(filterSandboxes(list, 'running').length).toBe(1);
    expect(filterSandboxes(null, 'x')).toEqual([]);
  });

  it('names an owner only when it is not the tenant', () => {
    expect(ownerShown(SB, 'kino')).toBe('');
    expect(ownerShown({ ...SB, owner: 'review-bot' }, 'kino')).toBe('review-bot');
  });
});

describe('tabs', () => {
  it("leaves the operators' tabs out for others", () => {
    expect(sandboxTabs({ operator: false }).map((t) => t.key)).toEqual(['overview', 'connect', 'credentials', 'settings']);
    expect(sandboxTabs({ operator: true }).map((t) => t.key))
      .toEqual(['overview', 'connect', 'credentials', 'network', 'activity', 'settings']);
  });

  it('maps earlier names and refuses what the role lacks', () => {
    expect(activeTab('tokens', {})).toBe('connect');
    expect(activeTab('access', {})).toBe('credentials');
    expect(activeTab('activity', { operator: false })).toBe('overview');
    expect(activeTab('activity', { operator: true })).toBe('activity');
    expect(activeTab('nope', {})).toBe('overview');
  });
});

describe('createRequest', () => {
  const empty = newSandboxForm('kino-dev');
  const opts = { environment: 'kino-dev', imageCredentials: null };
  it('leaves empty fields to the defaults: nothing of the environment, the internet', () => {
    expect(empty).toMatchObject({ reachMode: 'none', reaches: [], egress: undefined });
    expect(createRequest(empty)).toEqual(['', 0, 0, opts]);
  });

  it('takes a size, what it reaches, its egress and a registry login, and never attachable or groups', () => {
    const args = createRequest({ ...empty, image: ' ghcr.io/x/y:1 ', vcpus: '2', memoryMib: '4096',
      reachMode: 'some', reaches: [{ service: 'api', port: 'http' }, { addon: 'db' }],
      egress: { domains: ['api.stripe.com'], blocks: [{ cidr: '203.0.113.0/24', except: ['203.0.113.8/29'] }] },
      username: ' bot ', password: 's3cret ' });
    expect(args).toEqual(['ghcr.io/x/y:1', 2, 4096, {
      ...opts, reaches: [{ service: 'api', port: 'http' }, { addon: 'db' }],
      egress: { domains: ['api.stripe.com'], blocks: [{ cidr: '203.0.113.0/24', except: ['203.0.113.8/29'] }] },
      imageCredentials: { username: 'bot', password: 's3cret ' } }]);
    expect('attachable' in args[3]).toBe(false);
    expect('securityGroups' in args[3]).toBe(false);
  });

  it('sends reaches "all" for all, nothing for none or an empty list, and none as an egress word', () => {
    expect(createRequest({ ...empty, reachMode: 'all', reaches: [{ addon: 'db' }] })[3].reaches).toBe('all');
    expect('reaches' in createRequest({ ...empty, reachMode: 'none', reaches: [{ addon: 'db' }] })[3]).toBe(false);
    expect('reaches' in createRequest({ ...empty, reachMode: 'some', reaches: [] })[3]).toBe(false);
    expect(createRequest({ ...empty, egress: 'none' })[3].egress).toBe('none');
    expect('egress' in createRequest({ ...empty, egress: 'internet' })[3]).toBe(false);
  });

  it('names the tenant\'s credentials its pod gets, when any', () => {
    expect(createRequest({ ...empty, credentials: ['openai', 'github'] }))
      .toEqual(['', 0, 0, { ...opts, credentials: ['github', 'openai'] }]);
    expect(createRequest({ ...empty, credentials: [] })[3]).toEqual(opts);
  });

  it('needs an environment and an egress the plan would take', () => {
    expect(() => createRequest({ ...empty, environment: ' ' })).toThrow('choose the environment');
    expect(() => createRequest({ ...empty, egress: { domains: ['API.example.com'] } })).toThrow('not a domain: API.example.com');
    expect(() => createRequest({ ...empty, egress: { blocks: [{ cidr: '10.0.0.0' }] } })).toThrow('not an address block: 10.0.0.0');
  });

  it('refuses a bad size and half a login', () => {
    expect(() => createRequest({ ...empty, vcpus: '1.5' })).toThrow('vCPUs must be a whole number');
    expect(() => createRequest({ ...empty, memoryMib: '0' })).toThrow('memory must be');
    expect(() => createRequest({ ...empty, username: 'bot' })).toThrow('or neither');
    expect(() => credentialsOf('', '')).toThrow('needs a username and a password');
  });
});

describe('connectSnippets', () => {
  const at = { id: 'h3k9v2m7qpx', mcp: 'https://run.example.com/mcp', origin: 'https://run.example.com' };
  it('carries a fresh token', () => {
    const s = connectSnippets({ ...at, token: 'fst_abc' });
    expect(s.claude).toBe('claude mcp add --transport http ferun-h3k9v2m7qpx https://run.example.com/mcp '
      + '--header "Authorization: Bearer fst_abc"');
    expect(JSON.parse(s.mcpJson)).toEqual({ mcpServers: { 'ferun-h3k9v2m7qpx': {
      type: 'http', url: 'https://run.example.com/mcp', headers: { Authorization: 'Bearer fst_abc' } } } });
    expect(s.curl).toContain('POST https://run.example.com/api/v1/sandboxes/h3k9v2m7qpx/exec');
    expect(s.curl).toContain('-H "Authorization: Bearer fst_abc"');
    expect(s.curl).toContain('{"command": "uname -a", "timeout": 60}');
  });

  it('reads the token from the shell without one', () => {
    const s = connectSnippets(at);
    expect(s.claude).toContain('Bearer $FERUN_TOKEN"');
    expect(s.curl).toContain('Bearer $FERUN_TOKEN"');
    expect(s.mcpJson).toContain('Bearer <token>');
  });
});

describe('toolSummary', () => {
  it('lists the parameters, the required ones starred', () => {
    expect(toolSummary({ properties: { command: {}, timeout: {} }, required: ['command'] })).toBe('command*, timeout');
    expect(toolSummary(null)).toBe('');
  });
});

describe('the sidebar\'s sandboxes', () => {
  const sb = (id, clientState, updatedAt) => ({ sandboxId: id, clientState, updatedAt });
  const ids = (list) => list.map((s) => s.sandboxId);

  it('orders by the last activity, the newest first', () => {
    expect(ids(byActivity([sb('a', 'running', '2026-11-01'), sb('b', 'running', '2026-11-03'), sb('c', 'paused', '')])))
      .toEqual(['b', 'a', 'c']);
  });

  it('shows the ones awake first, and folds many hibernated, but the one open', () => {
    const few = [sb('h1', 'hibernated', '2026-11-05'), sb('r1', 'running', '2026-11-01'), sb('p1', 'paused', '2026-11-04')];
    expect(sideSandboxes(few)).toEqual({ shown: [few[2], few[1], few[0]], folded: 0 });
    const many = [sb('r1', 'running', '2026-11-01'), ...Array.from({ length: SIDE_FOLD }, (_, i) => sb(`h${i}`, 'hibernated', `2026-11-0${i + 2}`))];
    expect(ids(sideSandboxes(many).shown)).toEqual(['r1']);
    expect(sideSandboxes(many).folded).toBe(SIDE_FOLD);
    expect(ids(sideSandboxes(many, { selected: 'h1' }).shown)).toEqual(['r1', 'h1']);
    expect(sideSandboxes(many, { selected: 'h1' }).folded).toBe(SIDE_FOLD - 1);
    expect(sideSandboxes(many, { open: true }).folded).toBe(0);
    expect(sideSandboxes(null)).toEqual({ shown: [], folded: 0 });
  });

  it('says a state beside one only while it is not fine', () => {
    expect(sandboxMeta({ clientState: 'failed' })).toBe('failed');
    expect(sandboxMeta({ clientState: 'lost' })).toBe('lost');
    expect(sandboxMeta({ clientState: 'hibernated' })).toBe('');
    expect(sandboxMeta({ clientState: 'running' })).toBe('');
  });
});
