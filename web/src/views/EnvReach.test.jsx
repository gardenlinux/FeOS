// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { ObjectReachPicture, podsOf } from './EnvReach.jsx';

// An object's row of the environment's reachability on its Config view
// (ADR 0034, revised), drawn from the deployed deployment's answer.

const SPEC = {
  services: {
    api: { image: 'api:1', ports: [{ name: 'http', port: 8080 }] },
    catalog: { image: 'catalog:1', ports: [{ name: 'http', port: 8080 }], egress: 'none' },
    preview: { runtime: 'attached', sandbox: 'sbx1', ports: [{ name: 'http', port: 3000 }] },
  },
  addons: { db: { type: 'postgres' } },
};
const REACH = {
  edges: [
    { from: 'ingress', to: 'service/api', port: 'http', number: 8080, via: 'route api' },
    { from: 'service/api', to: 'addon/db', port: 'postgres', number: 5432, via: 'env DATABASE_URL' },
    { from: 'service/api', to: 'service/catalog', port: 'http', number: 8080, via: 'env CATALOG_URL' },
    { from: 'service/api', to: 'service/preview', port: 'http', number: 3000, via: 'uses' },
  ],
  egress: [{ object: 'service/api', mode: 'internet' }, { object: 'service/catalog', mode: 'none' }],
  routes: [{ route: 'api', service: 'api', port: 'http', from: ['203.0.113.0/24'] }],
};

const html = (el) => renderToStaticMarkup(el);
const text = (el) => html(el).replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/\s+/g, ' ');
const pic = (kind, name, extra = {}) => (
  <ObjectReachPicture tenant="t" env="stage" spec={SPEC} reach={REACH} kind={kind} name={name} navigate={() => {}} {...extra} />);

describe("an object's reachability on its Config", () => {
  it('shows who reaches it, with the route\'s clients, and what it reaches, with links', () => {
    const t = text(pic('service', 'api'));
    expect(t).toContain('Reached by');
    expect(t).toContain('Ingress 203.0.113.0/24 http 8080');
    expect(t).toContain('http 8080 catalog');
    expect(t).toContain('postgres 5432 db');
    expect(t).toContain('all traffic Internet');
    expect(html(pic('service', 'api'))).toContain('href="/tenants/t/environments/stage/objects/addon/db/config"');
    expect(html(pic('service', 'api'))).toContain('title="postgres 5432 to addon db (by env DATABASE_URL)"');
  });

  it('closes what nothing references, and says nothing outside for egress none', () => {
    const t = text(pic('service', 'catalog'));
    expect(t).toContain('api http 8080');
    expect(t).toContain('Nothing outside the environment');
    const db = text(pic('addon', 'db'));
    expect(db).toContain('api postgres 5432');
    expect(db).toContain('Nothing outside the environment');
  });

  it('sends an attached service to its sandbox, and says what is not there yet', () => {
    expect(text(pic('service', 'preview'))).toContain('Runs in sandbox sbx1 : its calls are the sandbox\'s pod\'s');
    expect(html(pic('service', 'preview'))).toContain('href="/tenants/t/sandboxes/sbx1"');
    expect(text(pic('service', 'gone'))).toContain('Not in the deployed deployment.');
    expect(text(pic('service', 'api', { reach: null }))).toContain('answers no reachability');
    expect(text(pic('service', 'api', { n: 0 }))).toContain('Not deployed yet.');
  });

  it('reads the sandboxes and jobs as pods', () => {
    expect(podsOf([{ sandboxId: 's1', reaches: 'all' }], [{ id: 'j1', reaches: [], egress: 'none' }])).toEqual([
      { kind: 'sandbox', id: 's1', reaches: 'all', egress: undefined }, { kind: 'jobrun', id: 'j1', reaches: [], egress: 'none' }]);
  });
});
