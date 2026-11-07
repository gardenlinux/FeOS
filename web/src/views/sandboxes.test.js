// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import {
  activeTab, bucketOf, canPause, canResume, connectSnippets, createRequest, credentialsOf, filterSandboxes, fmtMem,
  ownerShown, sandboxTabs, sizeWords, stateCounts, stateWords, toolSummary,
} from './sandboxes.js';

// The sandbox pages' derivations, from sandboxes as GET /api/v1/sandboxes
// renders them (go/internal/api, sandboxView).

const SB = {
  sandboxId: 'h3k9v2m7qpx', state: 'running', clientState: 'running', image: 'ghcr.io/kino/agent-base:2.4', node: 'n3',
  owner: 'kino', createdAt: '2026-11-03T10:00:00Z', updatedAt: '2026-11-06T09:00:00Z', desiredState: 'active',
  statusDescription: '', checkpointEpoch: '0', checkpointHead: '', checkpointAt: '', vcpus: 2, memoryMib: 4096,
  attachable: false,
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
  const list = [SB, as('paused', { sandboxId: 'q8w1z4c0dnr', node: 'n1', image: 'ghcr.io/kino/migrate:1' })];
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
  const empty = { image: '', vcpus: '', memoryMib: '', attachable: false, username: '', password: '' };
  it('leaves empty fields to the defaults', () => {
    expect(createRequest(empty)).toEqual(['', 0, 0, { attachable: false, imageCredentials: null }]);
  });

  it('takes a size, attachable and a registry login', () => {
    expect(createRequest({ ...empty, image: ' ghcr.io/x/y:1 ', vcpus: '2', memoryMib: '4096', attachable: true,
      username: ' bot ', password: 's3cret ' }))
      .toEqual(['ghcr.io/x/y:1', 2, 4096, { attachable: true, imageCredentials: { username: 'bot', password: 's3cret ' } }]);
  });

  it('names the tenant\'s credentials its pod gets, when any', () => {
    expect(createRequest({ ...empty, credentials: ['openai', 'github'] }))
      .toEqual(['', 0, 0, { attachable: false, imageCredentials: null, credentials: ['github', 'openai'] }]);
    expect(createRequest({ ...empty, credentials: [] })[3]).toEqual({ attachable: false, imageCredentials: null });
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
