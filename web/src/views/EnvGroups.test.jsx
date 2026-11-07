// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { ObjectGroups } from './EnvGroups.jsx';
import { accessWords, flowsOf, groupMembers, ingressRow, ingressWords, podGroups, reaches } from './groups.js';

// What an environment's groups say about who reaches whom (ADR 0034):
// members, reach, and an object's section on its Config view.

const SPEC = {
  securityGroups: {
    default: { in: [{ protocol: 'any', from: { group: 'default' } }] },
    web: { in: [{ ports: [8080], from: 'internet' }] },
    db: { in: [{ ports: [5432], from: { group: 'api' } }], out: [] },
    api: { in: [{ ports: [8080], from: { group: 'web' } }], out: [{ ports: [443], to: { cidr: '203.0.113.0/24' } }] },
  },
  services: {
    front: { image: 'f', securityGroups: ['web'] },
    back: { image: 'b', securityGroups: ['api', 'web'] },
    plain: { image: 'p' },
    preview: { runtime: 'attached', sandbox: 'sbx1', ports: [{ name: 'http', port: 3000 }] },
  },
  addons: { pg: { type: 'postgres', securityGroups: ['db'] } },
  jobs: { migrate: { image: 'm', securityGroups: ['api'] } },
  routes: { root: { path: '/', service: 'front', port: 'http' }, api: { path: '/api', service: 'back', port: 'http' } },
};

// row says a flow row as "peer-type:name access".
const row = (r) => `${r.peer.type}:${r.peer.name} ${r.paths ? ingressWords(r.paths) : accessWords(r.access)}`;

const text = (el) => renderToStaticMarkup(el).replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/\s+/g, ' ');

describe('members and reach', () => {
  it('puts objects into the groups they name, default for none, never an attached service', () => {
    expect(groupMembers(SPEC, 'web')).toEqual([{ kind: 'service', name: 'back' }, { kind: 'service', name: 'front' }]);
    expect(groupMembers(SPEC, 'default')).toEqual([{ kind: 'service', name: 'plain' }]);
    expect(groupMembers(SPEC, 'api')).toEqual([{ kind: 'service', name: 'back' }, { kind: 'job', name: 'migrate' }]);
    expect(groupMembers(SPEC, 'db')).toEqual([{ kind: 'addon', name: 'pg' }]);
    expect(podGroups({})).toEqual(['default']);
    expect(podGroups({ securityGroups: ['db'] })).toEqual(['db']);
  });

  it('reads what a group reaches from the other groups\' in rules', () => {
    expect(reaches(SPEC, 'api').map((r) => r.group)).toEqual(['db']);
    expect(reaches(SPEC, 'web').map((r) => r.group)).toEqual(['api']);
    expect(reaches(SPEC, 'default').map((r) => r.group)).toEqual(['default']);
  });
});

describe('the picture of a group', () => {
  it('puts who may connect on the left and what its members reach on the right', () => {
    const api = flowsOf(SPEC, ['api'], ['api']);
    expect(api.ins.map(row)).toEqual(['group:web tcp 8080']);
    expect(api.outs.map(row)).toEqual(['group:db tcp 5432', 'cidr:203.0.113.0/24 tcp 443']);
    expect(flowsOf(SPEC, ['web'], ['web']).outs.map(row)).toEqual(['group:api tcp 8080', 'internet:Internet all traffic']);
    const db = flowsOf(SPEC, ['db'], ['db']);
    expect(db.ins.map(row)).toEqual(['group:api tcp 5432']);
    expect(db.outs).toEqual([]);
  });

  it('says a group\'s own members as each other, and the internet when it has no out', () => {
    const d = flowsOf(SPEC, ['default'], ['default']);
    expect(d.ins.map(row)).toEqual(['self:default all traffic']);
    expect(d.outs.map(row)).toEqual(['internet:Internet all traffic']);
    expect(d.outs[0].byDefault).toBe(true);
  });

  it('merges the rules to one peer into a row, a protocol once', () => {
    const spec = { securityGroups: { g: { in: [
      { ports: [5432], from: { group: 'a' } }, { ports: [4222, 5432], from: { group: 'a' } }, { protocol: 'udp', ports: [53], from: { group: 'a' } },
      { protocol: 'icmp', from: { group: 'b' } }, { protocol: 'any', from: { group: 'b' } },
      { from: { group: 'c' } }, { ports: [80], from: { group: 'c' } },
      { ports: [22], from: { cidr: '203.0.113.0/24', except: ['203.0.113.8/29'] } }, { ports: [443], from: 'internet' },
    ], out: [] } } };
    const { ins } = flowsOf(spec, ['g'], ['g']);
    expect(ins.map(row)).toEqual([
      'internet:Internet tcp 443', 'cidr:203.0.113.0/24 tcp 22', 'group:a tcp 5432, 4222 · udp 53', 'group:b all traffic', 'group:c tcp all ports']);
    expect(ins[1].peer.except).toEqual(['203.0.113.8/29']);
  });

  it('shows the routes to its services as the ingress, by HTTP', () => {
    expect(row(ingressRow(SPEC, ['back', 'front']))).toBe('ingress:Ingress HTTP /, /api');
    expect(ingressRow(SPEC, ['plain'])).toBeNull();
    expect(ingressWords(['/a', '/b', '/c', '/d', '/e'])).toBe('HTTP /a, /b, /c +2');
  });

  it('joins the rows of an object\'s groups, each with the groups it comes by', () => {
    const r = flowsOf(SPEC, ['api', 'web']);
    expect(r.ins.map(row)).toEqual(['internet:Internet tcp 8080', 'group:web tcp 8080']);
    expect(r.ins[1].via).toEqual(['api']);
    expect(r.outs.map(row)).toEqual(['group:api tcp 8080', 'group:db tcp 5432', 'internet:Internet all traffic', 'cidr:203.0.113.0/24 tcp 443']);
    expect(r.outs[2].via).toEqual(['web']);
  });
});

describe("an object's groups on its Config", () => {
  const el = (kind, name, so) => (
    <ObjectGroups tenant="t" env="stage" spec={SPEC} kind={kind} name={name} so={so} navigate={() => {}} />);

  it('shows its groups and its picture: the ingress and who may connect, what it reaches', () => {
    const all = text(el('service', 'back', SPEC.services.back));
    expect(all).toContain('Security groups api web All groups');
    expect(all).toContain('Allowed in');
    expect(all).toContain('Ingress HTTP /api');
    expect(all).toContain('web tcp 8080');
    expect(all).toContain('back');
    expect(all).toContain('tcp 5432 db');
    expect(renderToStaticMarkup(el('service', 'back', SPEC.services.back))).toContain('href="/tenants/t/environments/stage/groups"');
  });

  it('says default and nothing gets in when an object names no group of a closed environment', () => {
    const all = text(<ObjectGroups tenant="t" env="e" spec={{}} kind="service" name="x" so={{ image: 'x' }} navigate={() => {}} />);
    expect(all).toContain('default');
    expect(all).toContain('Nothing gets in');
    expect(all).toContain('all traffic Internet');
  });

  it('sends an attached service to its sandbox', () => {
    const all = text(el('service', 'preview', SPEC.services.preview));
    expect(all).toContain('Runs in sandbox sbx1 , in its groups.');
  });
});
