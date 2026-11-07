// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { ObjectGroups } from './EnvGroups.jsx';
import { groupMembers, objectReach, outsideWords, podGroups, reaches } from './groups.js';

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
};

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
    expect(outsideWords({})).toEqual(['any to the internet']);
    expect(outsideWords({ out: [] })).toEqual([]);
  });

  it('joins the reach of an object\'s groups without repeats', () => {
    const r = objectReach(SPEC, ['api', 'web']);
    expect(r.inbound.map((x) => x.words)).toEqual(['tcp 8080 from group web', 'tcp 8080 from the internet']);
    expect(r.inside.map((x) => x.words)).toEqual(['tcp 5432 of group db', 'tcp 8080 of group api']);
    expect(r.outside.map((x) => x.words)).toEqual(['tcp 443 to 203.0.113.0/24', 'any to the internet']);
    expect(objectReach(SPEC, ['db']).outside).toEqual([]);
  });
});

describe("an object's groups on its Config", () => {
  const el = (kind, name, so) => (
    <ObjectGroups tenant="t" env="stage" spec={SPEC} kind={kind} name={name} so={so} navigate={() => {}} />);

  it('shows its groups, what reaches it and what it reaches', () => {
    const all = text(el('service', 'back', SPEC.services.back));
    expect(all).toContain('security groups');
    expect(all).toContain('api');
    expect(all).toContain('tcp 8080 from group web · api');
    expect(all).toContain('tcp 5432 of group db · api');
    expect(renderToStaticMarkup(el('service', 'back', SPEC.services.back))).toContain('href="/tenants/t/environments/stage/groups"');
  });

  it('says default and nothing when an object names no group of a closed environment', () => {
    const all = text(<ObjectGroups tenant="t" env="e" spec={{}} kind="service" so={{ image: 'x' }} navigate={() => {}} />);
    expect(all).toContain('default');
    expect(all).toContain('nothing: no pod reaches it');
    expect(all).toContain('any to the internet');
  });

  it('sends an attached service to its sandbox', () => {
    const all = text(el('service', 'preview', SPEC.services.preview));
    expect(all).toContain('An attached service is in the groups of its sandbox sbx1');
  });
});
