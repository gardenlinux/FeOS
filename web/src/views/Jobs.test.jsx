// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { MeContext } from '../people.js';
import { Jobs, PhaseLabel, RunForm } from './Jobs.jsx';
import { TenantSettings, Usage } from './TenantPages.jsx';

// The pages' first paint: their heads, and what a role may not do shown
// but disabled.

const ME = (role, admin = false) => ({
  me: { person: 'p', method: 'oidc', admin, tenants: [{ tenant: 'kino', role, environments: [] }] }, reload: async () => {},
});
const render = (me, el) => renderToStaticMarkup(<MeContext.Provider value={me}>{el}</MeContext.Provider>);

describe('Jobs', () => {
  it('shows a phase as a dot and a word', () => {
    expect(renderToStaticMarkup(<PhaseLabel phase="running" />)).toContain('class="sdot moving"');
    expect(renderToStaticMarkup(<PhaseLabel phase="deadline_exceeded" />)).toContain('deadline exceeded');
  });

  it('offers "Run a job" to write, disables it below', () => {
    const write = render(ME('write'), <Jobs tenant="kino" sel="" navigate={() => {}} />);
    expect(write).toContain('<span class="kind">Tenant</span><h1>Jobs</h1>');
    expect(write).toMatch(/<button class="primary" title="run a container to its end">/);
    const read = render(ME('read'), <Jobs tenant="kino" sel="" navigate={() => {}} />);
    expect(read).toMatch(/<button class="primary" disabled="" title="needs write on an environment">/);
    expect(read).toContain('kept 7 days after it ends');
  });

  it("heads a job's page with its kind and ID", () => {
    const out = render(ME('read'), <Jobs tenant="kino" sel="k2m7qpxh3v9" navigate={() => {}} />);
    expect(out).toContain('<span class="kind">Job</span><h1 class="mono">k2m7qpxh3v9</h1>');
  });
});

describe('RunForm', () => {
  const ENVS = [{ id: 'e1', name: 'kino-dev' }, { id: 'e2', name: 'kino-stage' }];
  it('asks for the environment first, then what it reaches of it (nothing) and its egress (the internet)', () => {
    const none = renderToStaticMarkup(<RunForm envs={ENVS} onRun={() => {}} onCancel={() => {}} />);
    expect(none).toContain('<option value="kino-stage">kino-stage</option>');
    expect(none).toContain('Choose the environment first.');
    const one = renderToStaticMarkup(<RunForm envs={ENVS} env="kino-dev" onRun={() => {}} onCancel={() => {}} />);
    expect(one).toContain('<option value="kino-dev" selected="">kino-dev</option>');
    expect(one).toContain('<div class="sg-seg" role="group" aria-label="reaches"><button type="button" class="on" aria-pressed="true">Nothing</button>');
    expect(one).toContain('The job reaches every address on the internet.');
    expect(one).not.toContain('security group');
  });
});

describe('Usage and Settings', () => {
  it('head their pages', () => {
    expect(render(ME('read'), <Usage tenant="kino" navigate={() => {}} />)).toContain('<span class="kind">Tenant</span><h1>Usage</h1>');
    const s = render(ME('read'), <TenantSettings tenant="kino" navigate={() => {}} />);
    expect(s).toContain('<span class="kind">Tenant</span><h1>Settings</h1>');
    expect(s).toContain('Leave this tenant');
    expect(s).not.toContain('Limits, zone and deletion');
  });

  it('send an operator to the platform instead of leaving', () => {
    const s = render(ME('', true), <TenantSettings tenant="kino" navigate={() => {}} />);
    expect(s).not.toContain('Leave this tenant');
    expect(s).toContain('Limits, zone and deletion');
    expect(s).toContain('href="/tenants"');
  });
});
