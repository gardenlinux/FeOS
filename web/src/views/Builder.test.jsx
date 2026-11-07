// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { fixture, response } from '../testing/ferun.js';
import { JsonView, ObjectNav, ReviewPanel } from './Builder.jsx';
import { AddonForm, BindingForm, RouteForm, SecretForm, ServiceForm } from './BuilderForms.jsx';
import { checkNewer } from './pins.js';
import { builtinCatalog, readCatalog } from './catalog.js';
import { groupFindings } from './findings.js';
import { deployBlock, draftWords, proposeMessage, removeObject, setObject } from './draft.js';
import { referencesTo } from './refs.js';

// The builder's views, rendered with ferund's fixtures: the list of
// objects with their lines, changes and findings, the forms with typed
// fields and findings at their fields, the review with its diff,
// findings and deploy.

const STORED = response('GET_environments_env_deployments_n.200').spec;
const WRITTEN = fixture('POST_environments_env_deployments.422').request.spec;
const INVALID = response('POST_environments_env_deployments.422');
const PLAN = response('POST_environments_env_plan.200');

const html = (el) => renderToStaticMarkup(el);
const text = (el) => html(el).replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/&quot;/g, '"')
  .replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&').replace(/\s+/g, ' ');
const none = { global: [], objects: new Map() };
const nop = () => {};
const ctx = (spec, findings = [], extra = {}) => ({
  spec, catalog: builtinCatalog(), findings, setSecret: async () => 1, isNew: false, knownSecrets: ['internal-token'], pins: [],
  credentials: [], editSpec: nop, editAttached: false, tenant: '', navigate: null, ...extra,
});

describe('ObjectNav', () => {
  const nav = (props) => html(<ObjectNav spec={STORED} base={STORED} groups={none} open="" pins={[]} catalog={builtinCatalog()}
    marks={new Map()} onOpen={nop} onAdd={nop} {...props} />);

  it('lists the objects by kind with their lines, and adds per kind unless read-only', () => {
    const pins = [{ name: 'internal-token', pinned: 3, latest: 3, newer: false, missing: false }];
    const t = text(<ObjectNav spec={STORED} base={STORED} groups={none} open="services/web" pins={pins} catalog={builtinCatalog()}
      marks={new Map()} onOpen={nop} onAdd={nop} />);
    expect(t).toContain('Services 3');
    expect(t).toContain('Jobs 1');
    expect(t).toContain('Addons 2');
    expect(t).toContain('Routes 3');
    expect(t).toContain('Bindings 0');
    expect(t).toContain('Security groups 1');
    expect(t).toContain('default nothing in · internet out');
    expect(t).toContain('Secrets 1');
    expect(t).toContain('expense-service expense:1.4 · 2 replicas');
    expect(t).toContain('postgres postgres · 17 · small');
    expect(t).toContain('api-expense /api/expense → expense-service:http');
    expect(t).toContain('internal-token v3 of 3');
    expect(nav({ open: 'services/web' })).toContain('class="b-row-obj on"');
    expect((nav({}).match(/class="ghost mini b-add"/g) || []).length).toBe(7);
    expect(nav({ onAdd: null })).not.toContain('ghost mini b-add');
  });

  it('marks what the draft changes, what it removes, and the findings', () => {
    let draft = setObject(STORED, 'services', 'web', { ...STORED.services.web, replicas: 2 });
    draft = setObject(draft, 'addons', 'cache', { type: 'valkey', version: '8' });
    draft = removeObject(draft, 'jobs', 'migrate');
    const marks = new Map([['services/web', 'update'], ['addons/cache', 'add'], ['jobs/migrate', 'remove']]);
    const groups = groupFindings(INVALID.findings, draft);
    const out = html(<ObjectNav spec={draft} base={STORED} groups={groups} open="" catalog={null} marks={marks}
      pins={[{ name: 'internal-token', pinned: 0, latest: 0, missing: true }]} onOpen={nop} onAdd={nop} />);
    expect(out).toMatch(/<span class="b-mark m-update"[^>]*>~<\/span><span class="mono b-objname">web</);
    expect(out).toMatch(/<span class="b-mark m-add"[^>]*>\+<\/span><span class="mono b-objname">cache</);
    expect(out).toMatch(/class="b-row-obj gone"[^]*?b-mark m-remove[^>]*>−<\/span><span class="mono b-objname">migrate</);
    expect(out).toContain('removed by this draft');
    expect(out).toContain('class="b-row-obj sev-error"');
    expect(out).toContain('title="2 errors in the plan">2</span>');
    expect(out).toContain('title="no value yet">!</span>');
    expect(text(<ObjectNav spec={draft} base={STORED} groups={none} open="" pins={[]} catalog={null} marks={marks} onOpen={nop} onAdd={nop} />))
      .toContain('Jobs 0');
  });
});

describe('the forms', () => {
  it('show a pinned image as written with its digest, and findings at their fields', () => {
    const groups = groupFindings(INVALID.findings, WRITTEN);
    const f = groups.objects.get('services/expense-service');
    const out = text(<ServiceForm name="expense-service" obj={STORED.services['expense-service']}
      ctx={ctx(STORED, f)} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(out).toContain('pinned to sha256:863e2ebe9c38…');
    expect(out).toContain('Check for a newer image');
    expect(out).toContain('no addon mysql — declare the addon under addons, or reference one that is declared');
    expect(out).toContain('nothing would wake it: no route targets expense-service');
    for (const s of ['Container', 'Variables', 'Scaling & resources', 'Health check', 'Credentials & bindings', 'Rollout', 'Remove service expense-service']) {
      expect(out).toContain(s);
    }
    const markup = html(<ServiceForm name="expense-service" obj={STORED.services['expense-service']}
      ctx={ctx(STORED, f)} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(markup).toContain('value="ghcr.io/acme/expense:1.4"');
    // The finding of a variable sits in its row, the idle's under the idle.
    const row = markup.slice(markup.indexOf('value="EXPENSA_DATABASE_URL"'));
    expect(row.slice(0, row.indexOf('</tr>'))).toContain('no addon mysql');
    expect(markup.indexOf('nothing would wake it')).toBeGreaterThan(markup.indexOf('Pause when idle'));
    expect(markup.indexOf('nothing would wake it')).toBeLessThan(markup.indexOf('Health check'));
    const unpinned = text(<ServiceForm name="web" obj={checkNewer(STORED.services.web)} ctx={ctx(STORED)}
      onChange={nop} onRename={nop} onRemove={nop} />);
    expect(unpinned).toContain('from any registry; the plan resolves it to a digest');
    expect(unpinned).not.toContain('Check for a newer image');
  });

  it('give each variable its typed value: text, secret, addon output, service address', () => {
    const svc = { image: 'ghcr.io/acme/x:1', ports: [{ name: 'http', port: 8080 }], env: {
      LOG_LEVEL: 'info', FROM: { value: 'noreply@example.com' }, TOKEN: { secret: 'internal-token' },
      DATABASE_URL: { addon: 'postgres', key: 'url', role: 'expense_svc' }, APPROVAL: { service: 'approval-service', port: 'http', key: 'host' },
    } };
    const pins = [{ name: 'internal-token', pinned: 2, latest: 3, newer: true, missing: false }];
    const out = html(<ServiceForm name="x" obj={svc} ctx={ctx(STORED, [], { pins })} onChange={nop} onRename={nop} onRemove={nop} />);
    const rowOf = (name) => { const r = out.slice(out.indexOf(`value="${name}"`)); return r.slice(0, r.indexOf('</tr>')); };
    expect(rowOf('LOG_LEVEL')).toContain('<option value="text" selected="">Text</option>');
    expect(rowOf('LOG_LEVEL')).toContain('value="info"');
    expect(rowOf('FROM')).toContain('value="noreply@example.com"');
    expect(rowOf('TOKEN')).toContain('<option value="secret" selected="">Secret</option>');
    expect(rowOf('TOKEN')).toContain('<option value="internal-token" selected="">internal-token</option>');
    expect(rowOf('TOKEN')).toContain('v2 pinned, v3 newer');
    expect(rowOf('TOKEN')).toContain('+ New secret…');
    expect(rowOf('DATABASE_URL')).toContain('<option value="addon" selected="">Addon output</option>');
    expect(rowOf('DATABASE_URL')).toContain('<option value="postgres" selected="">postgres (postgres)</option>');
    expect(rowOf('DATABASE_URL')).toContain('<option value="url" selected="">url</option>');
    expect(rowOf('DATABASE_URL')).toContain('value="expense_svc"');
    expect(rowOf('APPROVAL')).toContain('<option value="service" selected="">Service address</option>');
    expect(rowOf('APPROVAL')).toContain('<option value="approval-service" selected="">approval-service</option>');
    expect(rowOf('APPROVAL')).toContain('<option value="host" selected="">host</option>');
    // A service's own address is no target.
    expect(rowOf('APPROVAL')).not.toContain('<option value="x">');
  });

  it('show the health check as its kind, the bindings as ticks, the idle as a choice', () => {
    const spec = setObject(STORED, 'bindings', 'stripe', { tenantCredential: { name: 'stripe', version: 1 } });
    const svc = { ...STORED.services.web, bindings: ['stripe', 'gone'], idle: '15m' };
    const creds = [{ name: 'stripe', upstream: 'https://api.stripe.com', version: 2 }, { name: 'github', upstream: 'https://api.github.com', version: 1 }];
    const out = html(<ServiceForm name="web" obj={svc} ctx={ctx(spec, [], { credentials: creds, tenant: 'kino' })} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(out).toContain('<button class="ghost mini on" aria-pressed="true">HTTP</button>');
    expect(out).toContain('value="/readyz"');
    expect(out).toMatch(/<input type="checkbox" checked=""\/><span class="mono">stripe<\/span><span class="muted small">credential stripe · v1/);
    expect(out).toContain('not in the draft');
    // The credential it gets already is not offered again.
    expect(out).toContain('<option value="github" selected="">github · api.github.com</option>');
    expect(out).not.toContain('<option value="stripe"');
    expect(out).toContain('href="/tenants/kino/credentials"');
    expect(out).toContain('<option value="after" selected="">After</option>');
    expect(out).toContain('value="15"');
    const none = html(<ServiceForm name="web" obj={{ image: 'x:1' }} ctx={ctx({})} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(none).toContain('<button class="ghost mini on" aria-pressed="true">None</button>');
    // Without bindings, the section stays folded.
    expect(none).toMatch(/aria-expanded="false"><svg[^]*?<\/svg> Credentials &amp; bindings/);
  });

  it('show an attached service read-only, or as a form for those who write the environment', () => {
    const o = { runtime: 'attached', sandbox: 'abc', command: ['npm', 'run', 'dev'] };
    const ro = text(<ServiceForm name="web" obj={o} ctx={ctx({})} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(ro).toContain('It runs in sandbox abc of this environment. Changing it needs write on the environment');
    expect(ro).toContain('in its sandbox\'s groups');
    expect(ro).toContain('npm run dev');
    expect(html(<ServiceForm name="web" obj={o} ctx={ctx({})} onChange={nop} onRename={nop} onRemove={nop} />)).not.toContain('<input');
    const rw = html(<ServiceForm name="web" obj={o} ctx={ctx({}, [], { editAttached: true })} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(rw).toContain('value="abc"');
    expect(rw).toContain('Remove service web');
  });

  it('offer an addon the catalog versions, plans and option fields; its type fixed once deployed', () => {
    const catalog = readCatalog({ addons: [{ type: 'postgres', versions: ['16', '17'], defaultVersion: '17', defaultPlan: 'small',
      plans: [{ name: 'small', vcpus: 1, memoryMib: 1024, volumeMib: 10240 }, { name: 'large', vcpus: 4, memoryMib: 8192, volumeMib: 51200 }],
      outputKeys: ['url'], outputFields: [],
      options: [{ name: 'databases', kind: 'list', required: false, default: [], description: '', values: [], pattern: '^[a-z]+$',
        reserved: ['postgres'], reservedPrefixes: [], unique: true }] }] });
    const out = html(<AddonForm name="postgres" obj={STORED.addons.postgres} ctx={ctx(STORED, [], { catalog })}
      onChange={nop} onRename={nop} onRemove={nop} />);
    expect(out).toMatch(/<label class="b-plancard on"><input type="radio" name="plan-postgres" checked=""\/><span class="strong">small/);
    expect(out).toContain('1 vCPU, 1024 MiB, volume 10 GiB');
    expect(out).toContain('4 vCPUs, 8192 MiB, volume 50 GiB');
    expect(out).toContain('<span class="mono strong b-fixed">postgres</span>');
    expect(out).toContain('a change recreates it (downtime)');
    expect(out).toContain('>databases<');
    expect(out).not.toContain('is reserved');
    expect(text(<AddonForm name="postgres" obj={STORED.addons.postgres} ctx={ctx(STORED, [], { catalog })} onChange={nop} onRename={nop} onRemove={nop} />))
      .toContain('approval-service · APPROVAL_DATABASE_URL → postgres · url (database expensa, role approval_svc)');
    const hinted = text(<AddonForm name="postgres" obj={{ ...STORED.addons.postgres, options: { databases: ['postgres', 'x'] } }}
      ctx={ctx(STORED, [], { catalog })} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(hinted).toContain('postgres is reserved');
    // roles is no field of this catalog's: it is JSON.
    expect(out).toContain('other options (JSON)');
    expect(out).toContain('&quot;roles&quot;');
    expect(out).not.toContain('answers no catalog');
    const added = html(<AddonForm name="postgres" obj={STORED.addons.postgres} ctx={ctx(STORED, [], { catalog })} added
      onChange={nop} onRename={nop} onRemove={nop} />);
    expect(added).toContain('<option value="postgres" selected="">postgres</option>');
    const builtin = text(<AddonForm name="nats" obj={STORED.addons.nats} ctx={ctx(STORED)} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(builtin).toContain('This ferund answers no catalog of addons');
  });

  it('offer a route the services and their ports', () => {
    const out = html(<RouteForm name="root" obj={STORED.routes.root} ctx={ctx(STORED)} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(out).toContain('<option value="web" selected="">web</option>');
    expect(out).toContain('<option value="http" selected="">http</option>');
    expect(out).toContain('Strip the path prefix');
  });

  it('show a binding of a tenant credential with its pinned version, or the binding stated here', () => {
    const creds = [{ name: 'stripe', upstream: 'https://api.stripe.com', mode: 'name', version: 3 }];
    const spec = setObject(setObject(STORED, 'bindings', 'stripe', { tenantCredential: { name: 'stripe', version: 2 } }),
      'services', 'web', { ...STORED.services.web, bindings: ['stripe'] });
    const t = text(<BindingForm name="stripe" obj={spec.bindings.stripe} ctx={ctx(spec, [], { credentials: creds, tenant: 'kino' })}
      onChange={nop} onRename={nop} onRemove={nop} />);
    expect(t).toContain('https://api.stripe.com · reached as http://stripe/');
    expect(t).toContain('v2 v3 is newer Use v3');
    expect(t).toContain('service web');
    const literal = html(<BindingForm name="gh" obj={{ upstream: 'https://api.github.com', credential: { kind: 'basic', user: 'bot', password: { secret: 'internal-token' } } }}
      ctx={ctx(STORED)} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(literal).toContain('value="https://api.github.com"');
    expect(literal).toContain('<option value="basic" selected="">Basic</option>');
    expect(literal).toContain('value="bot"');
    expect(literal).toContain('<option value="internal-token" selected="">internal-token</option>');
  });

  it('show a secret without its value, and a missing one with Set a value', () => {
    const refs = referencesTo(STORED, 'secrets', 'internal-token');
    const out = text(<SecretForm name="internal-token" pin={{ name: 'internal-token', pinned: 2, latest: 3, newer: true }}
      refs={refs} ctx={ctx(STORED)} onPin={nop} onRemove={nop} />);
    expect(out).toContain('version 3, pinned 2');
    expect(out).toContain('Use version 3');
    expect(out).toContain('expense-service · EXPENSA_INTERNAL_TOKEN → secret internal-token');
    expect(out).toContain('Save as a new version');
    const missing = text(<SecretForm name="gone" pin={{ name: 'gone', pinned: 0, latest: 0, missing: true }}
      refs={[]} ctx={ctx(STORED)} onPin={nop} onRemove={nop} />);
    expect(missing).toContain('missing: the environment has no such secret');
    expect(missing).toContain('Set a value');
  });
});

describe('ReviewPanel', () => {
  const draft = { spec: STORED, base: 6, name: '' };
  const panel = (plan, extra = {}) => html(<ReviewPanel isNew={false} draft={draft} plan={plan} planning={false} next={7}
    groups={none} pinned={null} applyFindings={null} pins={[]} onApply={nop} {...extra} />);
  const deployButton = (out) => /<button class="primary"[^>]*>/.exec(out)[0];

  it('lists the changes object by object with their effects and proposes the message', () => {
    const out = panel({ answer: PLAN, spec: STORED, base: 6 });
    const t = text(<ReviewPanel isNew={false} draft={draft} plan={{ answer: PLAN }} planning={false} next={7} groups={none}
      pinned={null} applyFindings={null} pins={[]} onApply={nop} />);
    expect(t).toContain('Review Deployment 7 against deployment 6');
    expect(t).toContain('the plan accepts it');
    expect(t).toContain('service expense-service changed replaces 2 pods');
    expect(t).toContain('job migrate changed restarts');
    expect(out).toContain(`value="${proposeMessage(PLAN.diff, STORED)}"`);
    expect(deployButton(out)).not.toContain('disabled');
    expect(t).toContain('Deploy as deployment 7');
    expect(out).toContain('pins the images at deploy');
  });

  it('has nothing to deploy when nothing changes', () => {
    const t = text(<ReviewPanel isNew={false} draft={draft} plan={{ answer: { ...PLAN, diff: [] } }} planning={false} next={7}
      groups={none} pinned={null} applyFindings={null} pins={[]} onApply={nop} />);
    expect(t).toContain('Nothing changes against the base.');
    expect(t).toContain('no changes to deploy');
    expect(deployButton(panel({ answer: { ...PLAN, diff: [] } }))).toContain('disabled');
  });

  it('asks again for data loss and downtime', () => {
    const diff = [{ kind: 'addon', name: 'nats', change: 'remove', fields: [], effect: 'deletes data' }];
    const out = panel({ answer: { ...PLAN, diff }, spec: STORED, base: 6 });
    expect(out).toContain('I accept: addon nats deletes data');
    expect(out).toContain('<span class="badge bad">deletes data</span>');
    expect(out).toContain('<li class="b-change heavy">');
    expect(deployButton(out)).toContain('disabled');
  });

  it('waits for the conflicts of a rebase', () => {
    const out = panel({ answer: PLAN, spec: STORED, base: 6 }, { conflicts: 1 });
    expect(out).toContain('decide the conflicts first');
    expect(deployButton(out)).toContain('disabled');
  });

  it('holds the deploy while images do not resolve or the plan has errors', () => {
    const unresolved = panel({ answer: { ...PLAN, images: {} }, spec: STORED, base: 6 },
      { pinned: { spec: STORED, unresolved: ['ghcr.io/acme/x:1'], supported: true } });
    expect(unresolved).toContain('These images do not resolve, so the builder does not deploy: ghcr.io/acme/x:1');
    expect(deployButton(unresolved)).toContain('disabled');
    const invalid = panel({ answer: { valid: false, findings: INVALID.findings, diff: [], base: 6, current: 6 }, spec: STORED, base: 6 });
    expect(invalid).toContain('2 errors');
    expect(invalid).toContain('errors block the deploy');
    expect(invalid).toContain(INVALID.findings[0].problem.replace(/'/g, '&#x27;'));
    expect(invalid).toContain('the plan has errors: fix them first');
    expect(deployButton(invalid)).toContain('disabled');
  });

  it('says a new environment adds everything', () => {
    const changes = [{ key: 'services', kind: 'service', name: 'web', change: 'add', unit: 'services/web' }];
    const t = text(<ReviewPanel isNew draft={{ spec: STORED, base: 0, name: 'shop' }} plan={{ answer: { valid: true, findings: [] } }}
      planning={false} next={1} changes={changes} groups={none} pinned={null} applyFindings={null} pins={[]} onApply={nop} />);
    expect(t).toContain('Review Deployment 1 of shop everything is added');
    expect(t).toContain('+ service web');
    expect(t).toContain('Create shop with deployment 1');
  });
});

describe('the band', () => {
  it('says which deployment the draft becomes and where it starts', () => {
    expect(draftWords({ base: 13, latest: 13, deployed: 13, state: 'deployed', changes: 0 }))
      .toEqual({ title: 'Draft of deployment 14', from: 'starts from deployment 13 (deployed)', behind: 0, count: 'no changes yet', next: 14 });
    expect(draftWords({ base: 13, latest: 13, deployed: 12, state: 'failed', changes: 3 }))
      .toMatchObject({ from: 'starts from deployment 13 (failed)', count: '3 changes' });
    expect(draftWords({ base: 11, latest: 13, deployed: 13, changes: 1 })).toMatchObject({
      title: 'Draft of deployment 14', from: 'starts from deployment 11 (replaced)', behind: 13, count: '1 change' });
    expect(draftWords({ base: 0, latest: 0 })).toMatchObject({ title: 'Draft of deployment 1', from: 'starts from an empty spec' });
    expect(draftWords({ isNew: true, changes: 2 })).toMatchObject({ title: 'Draft of deployment 1 of a new environment', next: 1, count: '2 changes' });
    expect(draftWords({ readOnly: true, latest: 13 })).toMatchObject({ title: 'Deployment 13', from: 'read-only' });
  });

  it('says why a draft cannot be deployed yet, in the order to fix it', () => {
    const ok = { answer: { valid: true }, diff: [{}] };
    expect(deployBlock(ok)).toBe('');
    expect(deployBlock({ ...ok, diff: [] })).toBe('no changes to deploy');
    expect(deployBlock({ ...ok, diff: [], isNew: true, name: 'shop' })).toBe('');
    expect(deployBlock({ ...ok, isNew: true, name: 'Shop!' })).toBe('name the environment first');
    expect(deployBlock({ ...ok, planning: true })).toBe('planning…');
    expect(deployBlock({ ...ok, answer: { valid: false } })).toBe('the plan has errors: fix them first');
    expect(deployBlock({ ...ok, heavy: 1 })).toBe('accept the data loss or downtime first');
    expect(deployBlock({ ...ok, heavy: 1, ack: true })).toBe('');
    expect(deployBlock({ ...ok, conflicts: 2, planning: true })).toBe('decide the conflicts first');
  });
});

describe('JsonView', () => {
  it('shows the draft in the API order', () => {
    const out = html(<JsonView spec={{ routes: { r: { port: 'http', path: '/' } }, services: {} }} onSpec={nop} />);
    expect(out).toContain('{\n  &quot;services&quot;: {},\n  &quot;routes&quot;: {\n    &quot;r&quot;: {\n      &quot;path&quot;: &quot;/&quot;');
  });
});

