// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import {
  DEFAULT_WORDS, checkGroups, envRef, groupChoices, groupNames, groupOf, groupWords, inEnv, inUse, isGroupName, newRule,
  objectGroups, outMode, parsePorts, peerType, peerWords, portsText, retypePeer, ruleWords, sameGroups, takesPorts,
  toggleGroup, withProtocol,
} from './groups.js';
import { orderSpec, renameObject, specText } from './draft.js';
import { referencesTo, refLine, removeWithRefs } from './refs.js';
import { ApiError } from '../api.js';

// Security groups as the spec has them (ADR 0034), the example of the
// ADR, normalised as ferund writes it: default's out, the objects'
// lists.

const SPEC = {
  services: {
    web: { image: 'web:1', securityGroups: ['web'] },
    api: { image: 'api:1', securityGroups: ['api'] },
    worker: { image: 'worker:1' },
  },
  jobs: { migrate: { image: 'api:1', on: 'deploy', securityGroups: ['migrate'] } },
  addons: { db: { type: 'postgres', securityGroups: ['db'] } },
  securityGroups: {
    default: { out: [{ protocol: 'any', to: 'internet' }] },
    web: { in: [{ ports: [8080], from: { cidr: '203.0.113.0/24' } }] },
    api: { in: [{ ports: [9000], from: { group: 'web' } }], out: [{ to: 'internet' }] },
    db: {
      in: [{ protocol: 'tcp', ports: [5432], from: { group: 'api' } }, { ports: [5432], from: { group: 'migrate' } }],
      out: [],
    },
    migrate: {},
  },
};

describe('names and choices', () => {
  it('takes 1 to 32 of a-z, 0-9 and -, from a letter or digit', () => {
    expect(['default', 'db', '9lives', 'a-b', 'x'.repeat(32)].every(isGroupName)).toBe(true);
    expect(['', '-a', 'A', 'a_b', 'x'.repeat(33)].some(isGroupName)).toBe(false);
  });

  it('lists default first, written or not', () => {
    expect(groupNames(SPEC)).toEqual(['default', 'api', 'db', 'migrate', 'web']);
    expect(groupNames({})).toEqual(['default']);
    expect(groupOf({}, 'default')).toEqual({});
    expect(groupOf({}, 'web')).toBeUndefined();
  });

  it('offers a sandbox the deployed deployment\'s groups, in words', () => {
    expect(groupChoices({})).toEqual([{ name: 'default', words: DEFAULT_WORDS }]);
    const c = groupChoices(SPEC);
    expect(c.map((x) => x.name)).toEqual(['default', 'api', 'db', 'migrate', 'web']);
    expect(c[0].words).toBe('nothing in, internet out');
    expect(c[1].words).toBe('in: tcp 9000 from group web · out: tcp to the internet');
    expect(c[2].words).toBe('in: tcp 5432 from group api; tcp 5432 from group migrate · nothing out');
    expect(c[4].words).toBe('in: tcp 8080 from 203.0.113.0/24 · internet out');
    // The migrated default of an existing environment: open as it was.
    expect(groupWords({ in: [{ protocol: 'any', from: { group: 'default' } }] })).toBe('in: any from group default · internet out');
  });

  it('checks a choice as ferund does', () => {
    expect(checkGroups(['default'])).toEqual([]);
    expect(checkGroups([])).toEqual(['name at least one security group']);
    expect(checkGroups(['a', 'b', 'c', 'd', 'e', 'f'])).toEqual(['at most 5 security groups']);
    expect(checkGroups(['default', 'gone'], groupChoices(SPEC))).toEqual(['the environment has no group gone']);
    expect(sameGroups(['a', 'b'], ['b', 'a'])).toBe(true);
    expect(sameGroups(['a'], ['a', 'b'])).toBe(false);
  });

  it('toggles one to five, in the order of the choices', () => {
    const choices = ['default', 'api', 'db'];
    expect(toggleGroup(['default'], 'db', true, choices)).toEqual(['default', 'db']);
    expect(toggleGroup(['db', 'default'], 'api', true, choices)).toEqual(['default', 'api', 'db']);
    const one = ['default'];
    expect(toggleGroup(one, 'default', false, choices)).toBe(one);
    const five = ['a', 'b', 'c', 'd', 'e'];
    expect(toggleGroup(five, 'f', true, [])).toBe(five);
    expect(objectGroups({})).toEqual(['default']);
    expect(objectGroups(SPEC.services.web)).toEqual(['web']);
  });
});

describe('rules', () => {
  it('reads ports and ranges as typed', () => {
    expect(parsePorts('8080, 9000-9010 53')).toEqual({ ports: [8080, '9000-9010', 53], bad: [] });
    expect(parsePorts('80-80')).toEqual({ ports: [80], bad: [] });
    expect(parsePorts('0, 70000, 10-5, http')).toEqual({ ports: [], bad: ['0', '70000', '10-5', 'http'] });
    expect(parsePorts('')).toEqual({ ports: [], bad: [] });
    expect(portsText([8080, '9000-9010'])).toBe('8080, 9000-9010');
    expect(portsText(undefined)).toBe('');
  });

  it('gives icmp and any no ports', () => {
    expect([takesPorts(undefined), takesPorts('udp'), takesPorts('icmp'), takesPorts('any')]).toEqual([true, true, false, false]);
    expect(withProtocol({ protocol: 'tcp', ports: [80], from: 'internet' }, 'icmp')).toEqual({ protocol: 'icmp', from: 'internet' });
    expect(withProtocol({ ports: [80], from: 'internet' }, 'udp')).toEqual({ protocol: 'udp', ports: [80], from: 'internet' });
  });

  it('tells and retypes a peer', () => {
    expect([peerType('internet'), peerType({ group: 'a' }), peerType({ cidr: '' }), peerType(7)]).toEqual(['internet', 'group', 'cidr', 'other']);
    expect(retypePeer('internet', 'group', ['api'])).toEqual({ group: 'api' });
    expect(retypePeer({ group: 'a' }, 'cidr')).toEqual({ cidr: '' });
    expect(retypePeer({ cidr: '10.0.0.0/8' }, 'internet')).toBe('internet');
    const p = { group: 'a' };
    expect(retypePeer(p, 'group')).toBe(p);
    expect(peerWords({ cidr: '203.0.113.0/24', except: ['203.0.113.8/29'] })).toBe('203.0.113.0/24 except 203.0.113.8/29');
  });

  it('says a rule and a group\'s out', () => {
    expect(ruleWords({ protocol: 'any', ports: [1], to: 'internet' }, 'out')).toBe('any to the internet');
    expect(ruleWords({ from: { group: 'web' } }, 'in')).toBe('tcp from group web');
    expect(newRule('in')).toEqual({ protocol: 'tcp', ports: [8080], from: { group: 'default' } });
    expect(newRule('out')).toEqual({ protocol: 'tcp', ports: [443], to: 'internet' });
    expect([outMode({}), outMode({ out: [] }), outMode(SPEC.securityGroups.api)]).toEqual(['default', 'none', 'rules']);
  });
});

describe('the spec and its references', () => {
  it('orders a group\'s fields and the objects\' lists as the API does', () => {
    const spec = orderSpec({ securityGroups: { a: { out: [], in: [] } }, services: { w: { securityGroups: ['a'], image: 'x' } } });
    expect(Object.keys(spec)).toEqual(['services', 'securityGroups']);
    expect(Object.keys(spec.securityGroups.a)).toEqual(['in', 'out']);
    expect(Object.keys(spec.services.w)).toEqual(['image', 'securityGroups']);
    expect(specText(SPEC)).toContain('"securityGroups": [\n        "web"\n      ]');
  });

  it('names the members and the rules that name a group', () => {
    const refs = referencesTo(SPEC, 'securityGroups', 'api');
    expect(refs.map((r) => [r.from.key, r.from.name, r.field, refLine(r)])).toEqual([
      ['services', 'api', 'securityGroups[0]', 'in group api'],
      ['securityGroups', 'db', 'in[0].from', 'a rule in from group api'],
    ]);
  });

  it('renames a group with its members and the rules that name it', () => {
    const out = renameObject(SPEC, 'securityGroups', 'api', 'backend');
    expect(Object.keys(out.securityGroups)).toEqual(['default', 'web', 'backend', 'db', 'migrate']);
    expect(out.services.api.securityGroups).toEqual(['backend']);
    expect(out.securityGroups.db.in[0]).toEqual({ protocol: 'tcp', ports: [5432], from: { group: 'backend' } });
    expect(out.securityGroups.db.in[1]).toBe(SPEC.securityGroups.db.in[1]);
    expect(out.securityGroups.web).toBe(SPEC.securityGroups.web);
    expect(SPEC.services.api.securityGroups).toEqual(['api']);
  });

  it('removes a group with its entries and the rules that name it', () => {
    const out = removeWithRefs(SPEC, 'securityGroups', 'migrate');
    expect(out.securityGroups.migrate).toBeUndefined();
    // A list left empty goes: the job is in default.
    expect('securityGroups' in out.jobs.migrate).toBe(false);
    expect(out.securityGroups.db.in).toEqual([{ protocol: 'tcp', ports: [5432], from: { group: 'api' } }]);
    expect(out.securityGroups.db.out).toEqual([]);
  });
});

describe('the environment of a sandbox or a job', () => {
  it('reads its environment and matches it by name or ID', () => {
    const sb = { environment: { id: 'e1', name: 'kino-dev' } };
    expect(envRef(sb)).toEqual({ id: 'e1', name: 'kino-dev' });
    expect(envRef({})).toBeNull();
    expect([inEnv(sb, 'kino-dev'), inEnv(sb, 'e1'), inEnv(sb, 'stage'), inEnv(sb, '')]).toEqual([true, true, false, false]);
  });

  it('reads a refused delete: what keeps the environment', () => {
    const e = new ApiError('environment kino-dev is in use', 409,
      { error: 'x', code: 'environment_in_use', sandboxes: ['h3k9v2m7qpx'], jobs: ['k2m7qpxh3v9'] });
    expect(inUse(e)).toEqual({ sandboxes: ['h3k9v2m7qpx'], jobs: ['k2m7qpxh3v9'] });
    expect(inUse(new ApiError('x', 409, { code: 'environment_in_use' }))).toEqual({ sandboxes: [], jobs: [] });
    expect(inUse(new ApiError('x', 409, { code: 'busy' }))).toBeNull();
    expect(inUse(new ApiError('x', 403, { code: 'environment_in_use' }))).toBeNull();
  });
});
