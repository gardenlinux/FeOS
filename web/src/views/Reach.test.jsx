// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { AddonForm, JobForm, RouteForm, ServiceForm } from './BuilderForms.jsx';
import { EgressEditor, ReachFlow, ReachesPicker } from './Reach.jsx';
import { PlanReach } from './Builder.jsx';
import { PodTables } from './EnvPods.jsx';
import { InUse } from './EnvTabs.jsx';
import { CreateForm, ReachBox } from './Sandboxes.jsx';
import { RunForm } from './Jobs.jsx';
import { builtinCatalog } from './catalog.js';
import { ApiError } from '../api.js';

// Reachability in the console (ADR 0034 revised, ADR 0035): the
// builder's uses, egress and a route's clients, the plan's edges, what a
// sandbox reaches and its change in place; an environment's sandboxes and
// jobs, and the delete they block.

const html = (el) => renderToStaticMarkup(el);
const text = (el) => html(el).replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/&quot;/g, '"')
  .replace(/&amp;/g, '&').replace(/\s+/g, ' ');
const nop = () => {};

const SPEC = {
  services: {
    api: {
      image: 'api:1', ports: [{ name: 'http', port: 8080 }],
      env: { CATALOG_URL: { service: 'catalog', port: 'http', key: 'url' } },
      uses: [{ service: 'legacy', port: 'http' }, { addon: 'db' }],
      egress: { domains: ['api.stripe.com', '*.githubusercontent.com'], blocks: [{ cidr: '203.0.113.0/24', except: ['203.0.113.8/29'] }] },
    },
    catalog: { image: 'catalog:1', ports: [{ name: 'http', port: 8080 }] },
    legacy: { image: 'legacy:1', ports: [{ name: 'http', port: 80 }] },
    dev: { runtime: 'attached', sandbox: 'h3k9v2m7qpx', command: ['npm', 'run', 'dev'], ports: [{ name: 'http', port: 3000 }] },
  },
  jobs: { migrate: { image: 'api:1', on: 'deploy', egress: 'none' } },
  addons: { db: { type: 'postgres' } },
  routes: {
    api: { path: '/api', service: 'api', port: 'http', from: ['203.0.113.0/24', '2001:db8::/32'] },
    root: { path: '/', service: 'catalog', port: 'http' },
  },
};
const ctx = (extra = {}) => ({
  spec: SPEC, catalog: builtinCatalog(), findings: [], setSecret: async () => 1, isNew: false, knownSecrets: [], pins: [],
  credentials: [], editSpec: nop, editAttached: true, tenant: 'kino', navigate: null, sandboxes: null, ...extra,
});
const form = (F, name, obj, extra) => <F name={name} obj={obj} ctx={ctx(extra)} onChange={nop} onRename={nop} onRemove={nop} />;
const pressed = (label) => `<button type="button" class="on" aria-pressed="true">${label}</button>`;

describe('the builder\'s reachability', () => {
  it('shows a service\'s references, its uses and its egress, and no security group', () => {
    const out = html(form(ServiceForm, 'api', SPEC.services.api));
    const t = text(form(ServiceForm, 'api', SPEC.services.api));
    expect(t).toContain('Reachability what it references, it reaches; nothing else gets in');
    expect(t).toContain('From its variables CATALOG_URL → catalog:http · url');
    expect(out).toMatch(/<select aria-label="what it uses">.*?<option value="service:legacy" selected="">legacy<\/option>/);
    expect(out).toMatch(/<option value="addon:db" selected="">db \(postgres\)<\/option>/);
    expect(out).toContain('<option value="http" selected="">http</option>');
    expect(out).not.toContain('<option value="service:api"');
    expect(out).toContain(pressed('Only these'));
    expect(out).toMatch(/<textarea[^>]*aria-label="domains"[^>]*>api.stripe.com\n\*.githubusercontent.com<\/textarea>/);
    expect(out).toContain('value="203.0.113.0/24"');
    expect(out).toContain('value="203.0.113.8/29"');
    expect(t).not.toMatch(/security group/i);
  });

  it('says the internet when egress is left out, nothing for none, and marks what the plan would refuse', () => {
    const plain = html(form(ServiceForm, 'catalog', SPEC.services.catalog));
    expect(plain).toContain(pressed('The internet'));
    expect(text(form(ServiceForm, 'catalog', SPEC.services.catalog))).toContain('No variable references a service or an addon.');
    const job = html(form(JobForm, 'migrate', SPEC.jobs.migrate));
    expect(job).toContain(pressed('Nothing'));
    expect(text(form(JobForm, 'migrate', SPEC.jobs.migrate))).toContain('Each run reaches nothing outside the environment');
    const bad = text(<EgressEditor value={{ domains: ['Not.A.Domain'] }} onChange={nop} />);
    expect(bad).toContain('not a domain: Not.A.Domain');
    expect(text(<EgressEditor value={{}} onChange={nop} who="the sandbox" />)).toContain('Nothing yet: the sandbox reaches nothing outside.');
  });

  it('gives an addon neither, and says that its references are who reaches it', () => {
    const t = text(form(AddonForm, 'db', SPEC.addons.db));
    expect(t).not.toContain('Egress');
    expect(t).toContain('Used by api · uses → db Exactly these reach it');
    expect(text(<AddonForm name="kv" obj={{ type: 'valkey' }} ctx={ctx()} onChange={nop} onRename={nop} onRemove={nop} />))
      .toContain('Nothing references it yet, so nothing reaches it');
  });

  it('takes a route\'s clients: every client, or only its blocks', () => {
    const only = html(form(RouteForm, 'api', SPEC.routes.api));
    expect(only).toContain(pressed('Only these blocks'));
    expect(only).toMatch(/<textarea[^>]*aria-label="from"[^>]*>203.0.113.0\/24\n2001:db8::\/32<\/textarea>/);
    const every = html(form(RouteForm, 'root', SPEC.routes.root));
    expect(every).toContain(pressed('Every client'));
    expect(text(form(RouteForm, 'root', SPEC.routes.root))).toContain('Every client reaches it while the environment is public; one that admits visitors asks for a visit next.');
    expect(text(form(RouteForm, 'x', { ...SPEC.routes.root, from: ['office'] }))).toContain('not an address block: office');
  });

  it('lists the plan\'s edges by caller, with where each comes from', () => {
    const reach = {
      edges: [
        { from: 'ingress', to: 'service/api', port: 'http', number: 8080, via: 'route api' },
        { from: 'service/api', to: 'service/catalog', port: 'http', number: 8080, via: 'env CATALOG_URL' },
        { from: 'service/api', to: 'addon/db', port: 'postgres', number: 5432, via: 'uses' },
      ],
      egress: [{ object: 'service/api', mode: 'restricted', domains: ['api.stripe.com'], blocks: [] }, { object: 'job/migrate', mode: 'none' }],
      routes: [{ route: 'api', service: 'api', port: 'http', from: ['203.0.113.0/24'] }, { route: 'root', service: 'catalog', port: 'http', from: [] }],
    };
    const t = text(<PlanReach reach={reach} />);
    expect(t).toContain('Reachability 3 edges');
    expect(t).toContain('the ingress reaches api http 8080 route api');
    expect(t).toContain('service api reaches catalog http 8080 env CATALOG_URL db postgres 5432 uses');
    expect(t).toContain('Routes for some clients only: api (203.0.113.0/24).');
    expect(t).toContain('Egress not the internet: api only api.stripe.com; migrate nothing outside.');
    expect(text(<PlanReach reach={{ edges: [] }} />)).toContain('No object references another');
  });
});

describe('what a sandbox reaches', () => {
  it('offers nothing, some entries of the deployed deployment, or all', () => {
    const some = html(<ReachesPicker spec={SPEC} mode="some" list={[{ addon: 'db' }, { service: 'gone', port: 'http' }]} onChange={nop} />);
    expect(some).toContain(pressed('Only these'));
    expect(some).toMatch(/<input type="checkbox"\/><svg[^]*?<\/svg><span class="mono sg-name">api:http<\/span><span class="muted small sg-words">8080<\/span>/);
    expect(some).toMatch(/<input type="checkbox" checked=""\/><svg[^]*?<\/svg><span class="mono sg-name">db<\/span><span class="muted small sg-words">postgres/);
    expect(some).toContain('<span class="mono sg-name">dev:http</span><span class="muted small sg-words">3000 · attached</span>');
    expect(some).toContain('class="sg-choice on bad"');
    expect(text(<ReachesPicker spec={SPEC} mode="some" list={[{ service: 'gone', port: 'http' }]} onChange={nop} />))
      .toContain('gone:http not in the deployed deployment');
    expect(html(<ReachesPicker spec={null} mode="some" list={[]} onChange={nop} />)).toContain('reading the environment&#x27;s deployment…');
    expect(text(<ReachesPicker spec={SPEC} mode="none" list={[]} onChange={nop} />)).toContain('Alone on the network');
    expect(text(<ReachesPicker spec={SPEC} mode="all" list={[]} onChange={nop} />)).toContain('Every port of every pod of the environment');
  });

  it('changes in place, never a move', () => {
    const sb = { sandboxId: 'h3k9v2m7qpx', environment: { id: 'e1', name: 'kino-dev' }, reaches: [{ addon: 'db' }], egress: 'none' };
    const closed = text(<ReachBox sb={sb} write busy="" act={nop} />);
    expect(closed).toContain('Reaches db · out: nothing outside Change');
    expect(html(<ReachBox sb={sb} write={false} busy="" act={nop} />)).toContain('disabled="" title="needs write on its environment"');
    const open = text(<ReachBox sb={sb} write busy="" act={nop} open />);
    expect(open).toContain('Changed in place');
    expect(open).toContain('It keeps its address and its connections');
    expect(open).not.toContain('new address');
    expect(open).not.toContain('Move');
    expect(html(<ReachBox sb={sb} write busy="" act={nop} open />)).toMatch(/<button class="primary" disabled="" title="what it reaches now">/);
  });

  it('draws a pod: what it reaches, and nothing outside for egress none', () => {
    const rows = [{ key: 'a', peer: { type: 'addon', name: 'db' }, what: 'postgres 5432', via: ['reaches'] }];
    const t = text(<ReachFlow ins={[]} outs={rows} egress="none"><span>box</span></ReachFlow>);
    expect(t).toContain('Reached by Reaches');
    expect(t).toContain('Nothing gets in box postgres 5432 db Nothing outside the environment');
    expect(text(<ReachFlow ins={[]} outs={[]} egress="internet"><span>box</span></ReachFlow>)).toContain('Reaches nothing');
  });
});

describe('an environment\'s sandboxes and jobs', () => {
  it('lists both, with what they reach', () => {
    const sandboxes = [{ sandboxId: 'h3k9v2m7qpx', clientState: 'paused', image: 'agent:1', reaches: [{ addon: 'db-admin' }],
      updatedAt: '2026-11-06T09:00:00Z' }];
    const jobs = [{ id: 'k2m7qpxh3v9aaaa', phase: 'running', image: 'report:3', reaches: 'all', createdAt: '2026-11-06T09:00:00Z' }];
    const out = html(<PodTables tenant="kino" env="kino-dev" sandboxes={sandboxes} jobs={jobs} write navigate={nop} />);
    expect(out).toContain('href="/tenants/kino/environments/kino-dev/sandboxes/h3k9v2m7qpx"');
    expect(out).toContain('href="/tenants/kino/environments/kino-dev/sandboxes/new"');
    expect(out).toContain('href="/tenants/kino/environments/kino-dev/sandboxes"');
    expect(out).toContain('<th>reaches</th>');
    expect(text(<PodTables tenant="kino" env="kino-dev" sandboxes={sandboxes} jobs={jobs} write navigate={nop} />))
      .toContain('db-admin');
    expect(out).toContain('all of the environment');
    expect(out).not.toContain('groups');
    expect(out).toContain('1, 1 on their way');
    expect(out).toContain('href="/tenants/kino/environments/kino-dev/jobs/k2m7qpxh3v9aaaa"');
    expect(out).toContain('href="/tenants/kino/environments/kino-dev/jobs"');
    expect(text(<PodTables tenant="kino" env="kino-dev" sandboxes={sandboxes} jobs={jobs} write navigate={nop} />))
      .toContain('jobs made by a call');
    expect(out).not.toContain('of the tenant');
    const empty = text(<PodTables tenant="kino" env="kino-dev" sandboxes={[]} jobs={[]} navigate={nop} />);
    expect(empty).toContain('No sandbox in this environment.');
    expect(empty).not.toContain('new sandbox');
    // The overview shows the newest jobs; the rest are in Activity's Jobs.
    const many = Array.from({ length: 8 }, (_, i) => ({ id: `job${i}`, phase: 'succeeded', image: 'b:1', reaches: [],
      createdAt: `2026-11-06T09:0${i}:00Z` }));
    const shown = html(<PodTables tenant="kino" env="kino-dev" sandboxes={[]} jobs={many} navigate={nop} />);
    expect(shown).toContain('/jobs/job7"');
    expect(shown).toContain('/jobs/job3"');
    expect(shown).not.toContain('/jobs/job2"');
  });

  it('says which sandboxes and jobs keep a delete from happening', () => {
    const e = new ApiError('environment kino-dev is in use', 409,
      { code: 'environment_in_use', sandboxes: ['h3k9v2m7qpx', 'q8w1z4c0dnr'], jobs: ['k2m7qpxh3v9'] });
    const out = html(<InUse error={e} tenant="kino" env="kino-dev" navigate={nop} />);
    expect(out).toContain('href="/tenants/kino/environments/kino-dev/sandboxes/q8w1z4c0dnr"');
    expect(out).toContain('href="/tenants/kino/environments/kino-dev/jobs/k2m7qpxh3v9"');
    expect(text(<InUse error={e} tenant="kino" env="kino-dev" navigate={nop} />)).toContain('Delete the sandboxes and cancel the jobs first');
    expect(html(<InUse error={new ApiError('x', 409, { code: 'busy' })} tenant="kino" env="kino-dev" navigate={nop} />)).toBe('');
  });

  it('makes a sandbox and runs a job in the environment open, which is no choice', () => {
    const sbx = html(<CreateForm busy="" defaults={null} env="kino-dev" onSubmit={nop} onCancel={nop} />);
    expect(sbx).toContain('<span class="mono">kino-dev</span>');
    expect(sbx).not.toContain('aria-label="environment"');
    const job = html(<RunForm env="kino-dev" onRun={nop} onCancel={nop} />);
    expect(job).toContain('Run a job in <span class="mono">kino-dev</span>');
    expect(job).not.toContain('aria-label="environment"');
  });
});
