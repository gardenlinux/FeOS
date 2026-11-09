// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { MeContext } from '../people.js';
import { Jobs, PhaseLabel, RunForm, withLabels } from './Jobs.jsx';
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

  it('offers "Run a job" to write on the environment, disables it below', () => {
    const write = render(ME('write'), <Jobs tenant="kino" env="kino-dev" sel="" write navigate={() => {}} />);
    expect(write).toMatch(/<button class="primary" title="run a container to its end">/);
    expect(write).toContain('A job made by a call');
    expect(write).not.toContain('tenant&#x27;s job');
    const read = render(ME('read'), <Jobs tenant="kino" env="kino-dev" sel="" navigate={() => {}} />);
    expect(read).toMatch(/<button class="primary" disabled="" title="needs write on this environment">/);
    expect(read).toContain('kept 7 days after it ends');
  });

  it("heads a job's page with its ID, back to the environment's jobs", () => {
    const out = render(ME('read'), <Jobs tenant="kino" env="kino-dev" sel="k2m7qpxh3v9" navigate={() => {}} />);
    expect(out).toContain('<span class="mono strong">job k2m7qpxh3v9</span>');
    expect(out).toContain('> jobs</button>');
  });
});

describe('the label filter', () => {
  it('keeps the jobs with every label', () => {
    const jobs = [{ id: 'a', labels: { kind: 'build', project: 'kino' } }, { id: 'b', labels: { kind: 'build' } }, { id: 'c' }];
    expect(withLabels(jobs, ['kind=build']).map((j) => j.id)).toEqual(['a', 'b']);
    expect(withLabels(jobs, ['kind=build', 'project=kino']).map((j) => j.id)).toEqual(['a']);
    expect(withLabels(jobs, ['url=a=b'])).toEqual([]);
    expect(withLabels(jobs, [])).toBe(jobs);
    expect(withLabels(null, ['kind=build'])).toBe(null);
  });
});

describe('RunForm', () => {
  it('runs in the environment given, reaching nothing of it and out to the internet', () => {
    const one = renderToStaticMarkup(<RunForm env="kino-dev" onRun={() => {}} onCancel={() => {}} />);
    expect(one).toContain('Run a job in <span class="mono">kino-dev</span>');
    expect(one).not.toContain('aria-label="environment"');
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
