// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { entries, search } from './search.js';
import { PROJECT_NAV } from './people.js';

const summary = {
  environments: [{ id: 'e1', name: 'prod', hosts: ['kino.example.com'] }, { id: 'e2', name: 'preview-42', hosts: [] }],
  sandboxes: [{ sandboxId: 'h3k9v2m7qpx', owner: 'review-bot', image: 'agent-base:2.4', environment: { id: 'e2', name: 'preview-42' } },
    { sandboxId: 'q8w1z4c0dnr', image: 'agent-base:2.4' }],
  jobs: [{ id: 'a3ze6kkjvfx', image: 'builder:1', phase: 'succeeded', environment: { id: 'e1', name: 'prod' } }],
  domains: [{ id: 'd-2', name: 'shop.example.com', state: 'waiting-for-dns' }],
  providers: [{ name: 'ferun', kind: 'ferun', builtIn: true }, { name: 'fecode', kind: 'project', url: 'https://fecode.dev/api/v1/visit' },
    { name: 'sso', kind: 'oidc', issuer: 'https://login.example.com/' }],
};
const all = entries({ project: 'kino', nav: PROJECT_NAV, projects: [{ project: 'kino' }, { project: 'acme' }], summary });

describe('search', () => {
  it('offers the pages and the other projects without a query', () => {
    const r = search(all, '');
    expect(r[0]).toMatchObject({ kind: 'page', label: 'Home', to: '/projects/kino' });
    expect(r.some((e) => e.kind === 'project' && e.label === 'acme')).toBe(true);
    expect(r.some((e) => e.kind === 'project' && e.label === 'kino')).toBe(false);
  });

  it('finds a sandbox by a prefix of its ID and by its owner, on its environment\'s page', () => {
    expect(search(all, 'h3k9')[0]).toMatchObject({ kind: 'sandbox', to: '/projects/kino/environments/preview-42/sandboxes/h3k9v2m7qpx' });
    expect(search(all, 'review-bot')[0]).toMatchObject({ kind: 'sandbox' });
    // One without its environment (an older ferund) is found by its ID.
    expect(search(all, 'q8w1')[0]).toMatchObject({ kind: 'sandbox', to: '/projects/kino/sandboxes/q8w1z4c0dnr' });
  });

  it('finds a job by its ID, in its environment\'s Activity', () => {
    expect(search(all, 'a3ze')[0]).toMatchObject({ kind: 'job', to: '/projects/kino/environments/prod/jobs/a3ze6kkjvfx' });
  });

  it('opens a deployment as prod#13 or prod 13', () => {
    const opts = { project: 'kino', environments: summary.environments };
    expect(search(all, 'prod#13', opts)[0]).toMatchObject({ kind: 'deployment', to: '/projects/kino/environments/prod/deployments/13' });
    expect(search(all, 'prod 13', opts)[0]).toMatchObject({ kind: 'deployment' });
    expect(search(all, 'nope#13', opts).some((e) => e.kind === 'deployment')).toBe(false);
  });

  it('ranks names that start with the query before those that hold it', () => {
    // The pages "Project settings · …" start with it too.
    const labels = search(all, 'pr', { limit: 50 }).map((e) => e.label);
    const held = labels.findIndex((l) => !l.toLowerCase().startsWith('pr'));
    expect(held).toBeGreaterThan(labels.indexOf('preview-42'));
    expect(labels.indexOf('prod')).toBeLessThan(labels.indexOf('preview-42'));
    expect(labels.slice(held).every((l) => !l.toLowerCase().startsWith('pr'))).toBe(true);
    expect(search(all, 'example')[0]).toMatchObject({ label: 'prod' });
    expect(search(all, 'shop')[0]).toMatchObject({ kind: 'domain', to: '/projects/kino/domains/d-2' });
  });

  it('finds an access provider by its name, its kind and where it is (ADR 0054)', () => {
    expect(search(all, 'fecode')[0]).toMatchObject({ kind: 'provider', label: 'fecode', hint: 'project · https://fecode.dev/api/v1/visit',
      to: '/projects/kino/providers/fecode', icon: 'lock' });
    expect(search(all, 'login.example')[0]).toMatchObject({ kind: 'provider', to: '/projects/kino/providers/sso' });
    expect(search(all, 'ferun')[0]).toMatchObject({ kind: 'provider', hint: 'ferun', to: '/projects/kino/providers/ferun' });
    expect(search(all, 'access prov')[0]).toMatchObject({ kind: 'page', label: 'Access providers', to: '/projects/kino/providers' });
  });
});
