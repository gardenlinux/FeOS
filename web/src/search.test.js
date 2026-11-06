// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { entries, search } from './search.js';
import { TENANT_NAV } from './people.js';

const summary = {
  environments: [{ id: 'e1', name: 'prod', hosts: ['kino.example.com'] }, { id: 'e2', name: 'preview-42', hosts: [] }],
  sandboxes: [{ sandboxId: 'h3k9v2m7qpx', owner: 'review-bot', image: 'agent-base:2.4' }],
  domains: [{ id: 'd-2', name: 'shop.example.com', state: 'waiting-for-dns' }],
  credentials: [{ name: 'github', upstream: 'https://api.github.com' }],
};
const all = entries({ tenant: 'kino', nav: TENANT_NAV, tenants: [{ tenant: 'kino' }, { tenant: 'acme' }], summary });

describe('search', () => {
  it('offers the pages and the other tenants without a query', () => {
    const r = search(all, '');
    expect(r[0]).toMatchObject({ kind: 'page', label: 'Home', to: '/tenants/kino' });
    expect(r.some((e) => e.kind === 'tenant' && e.label === 'acme')).toBe(true);
    expect(r.some((e) => e.kind === 'tenant' && e.label === 'kino')).toBe(false);
  });

  it('finds a sandbox by a prefix of its ID and by its owner', () => {
    expect(search(all, 'h3k9')[0]).toMatchObject({ kind: 'sandbox', to: '/tenants/kino/sandboxes/h3k9v2m7qpx' });
    expect(search(all, 'review-bot')[0]).toMatchObject({ kind: 'sandbox' });
  });

  it('opens a deployment as prod#13 or prod 13', () => {
    const opts = { tenant: 'kino', environments: summary.environments };
    expect(search(all, 'prod#13', opts)[0]).toMatchObject({ kind: 'deployment', to: '/tenants/kino/environments/prod/deployments/13' });
    expect(search(all, 'prod 13', opts)[0]).toMatchObject({ kind: 'deployment' });
    expect(search(all, 'nope#13', opts).some((e) => e.kind === 'deployment')).toBe(false);
  });

  it('ranks names that start with the query before those that hold it', () => {
    expect(search(all, 'pr').map((e) => e.label).slice(0, 2)).toEqual(['prod', 'preview-42']);
    expect(search(all, 'example')[0]).toMatchObject({ label: 'prod' });
    expect(search(all, 'shop')[0]).toMatchObject({ kind: 'domain', to: '/tenants/kino/domains/d-2' });
    expect(search(all, 'git')[0]).toMatchObject({ kind: 'credential', to: '/tenants/kino/credentials/github' });
  });
});
