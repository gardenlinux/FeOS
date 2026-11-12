// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { entries, search } from './search.js';
import { TENANT_NAV } from './people.js';

const summary = {
  environments: [{ id: 'e1', name: 'prod', hosts: ['kino.example.com'] }, { id: 'e2', name: 'preview-42', hosts: [] }],
  sandboxes: [{ sandboxId: 'h3k9v2m7qpx', owner: 'review-bot', image: 'agent-base:2.4', environment: { id: 'e2', name: 'preview-42' } },
    { sandboxId: 'q8w1z4c0dnr', image: 'agent-base:2.4' }],
  jobs: [{ id: 'a3ze6kkjvfx', image: 'builder:1', phase: 'succeeded', environment: { id: 'e1', name: 'prod' } }],
  domains: [{ id: 'd-2', name: 'shop.example.com', state: 'waiting-for-dns' }],
  credentials: [{ name: 'github', upstream: 'https://api.github.com' }],
  providers: [{ name: 'ferun', kind: 'ferun', builtIn: true }, { name: 'fecode', kind: 'tenant', url: 'https://fecode.dev/api/v1/visit' },
    { name: 'sso', kind: 'oidc', issuer: 'https://login.example.com/' }],
};
const all = entries({ tenant: 'kino', nav: TENANT_NAV, tenants: [{ tenant: 'kino' }, { tenant: 'acme' }], summary });

describe('search', () => {
  it('offers the pages and the other tenants without a query', () => {
    const r = search(all, '');
    expect(r[0]).toMatchObject({ kind: 'page', label: 'Home', to: '/tenants/kino' });
    expect(r.some((e) => e.kind === 'tenant' && e.label === 'acme')).toBe(true);
    expect(r.some((e) => e.kind === 'tenant' && e.label === 'kino')).toBe(false);
  });

  it('finds a sandbox by a prefix of its ID and by its owner, on its environment\'s page', () => {
    expect(search(all, 'h3k9')[0]).toMatchObject({ kind: 'sandbox', to: '/tenants/kino/environments/preview-42/sandboxes/h3k9v2m7qpx' });
    expect(search(all, 'review-bot')[0]).toMatchObject({ kind: 'sandbox' });
    // One without its environment (an older ferund) is found by its ID.
    expect(search(all, 'q8w1')[0]).toMatchObject({ kind: 'sandbox', to: '/tenants/kino/sandboxes/q8w1z4c0dnr' });
  });

  it('finds a job by its ID, in its environment\'s Activity', () => {
    expect(search(all, 'a3ze')[0]).toMatchObject({ kind: 'job', to: '/tenants/kino/environments/prod/jobs/a3ze6kkjvfx' });
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

  it('finds an access provider by its name, its kind and where it is (ADR 0054)', () => {
    expect(search(all, 'fecode')[0]).toMatchObject({ kind: 'provider', label: 'fecode', hint: 'tenant · https://fecode.dev/api/v1/visit',
      to: '/tenants/kino/providers/fecode', icon: 'lock' });
    expect(search(all, 'login.example')[0]).toMatchObject({ kind: 'provider', to: '/tenants/kino/providers/sso' });
    expect(search(all, 'ferun')[0]).toMatchObject({ kind: 'provider', hint: 'ferun', to: '/tenants/kino/providers/ferun' });
    expect(search(all, 'access prov')[0]).toMatchObject({ kind: 'page', label: 'Access providers', to: '/tenants/kino/providers' });
  });
});
