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
  httpSecrets: [], editSpec: nop, editAttached: false, project: '', navigate: null, ...extra,
});

// HTTP are an environment's HTTP secrets (ADR 0055) as GET …/secrets
// answers them.
const HTTP = [
  { name: 's4', type: 'http', version: 3, upstream: 'https://api.s4.example', mode: 'name', credential: { kind: 'headers' } },
  { name: 'openai', type: 'http', version: 1, upstream: 'https://api.openai.com', mode: 'intercept', credential: { kind: 'headers' } },
];

describe('ObjectNav', () => {
  const nav = (props) => html(<ObjectNav spec={STORED} base={STORED} groups={none} open="" pins={[]} catalog={builtinCatalog()}
    marks={new Map()} onOpen={nop} onAdd={nop} {...props} />);

  it('lists the objects by kind with their lines, and adds per kind unless read-only; no binding is added', () => {
    const pins = [{ name: 'internal-token', pinned: 3, latest: 3, newer: false, missing: false }];
    const t = text(<ObjectNav spec={STORED} base={STORED} groups={none} open="services/web" pins={pins} catalog={builtinCatalog()}
      marks={new Map()} onOpen={nop} onAdd={nop} />);
    expect(t).toContain('Services 3');
    expect(t).toContain('Jobs 1');
    expect(t).toContain('Addons 2');
    expect(t).toContain('Routes 3');
    // Bindings in the spec give way to HTTP secrets (ADR 0055): no group
    // without one, and never an Add.
    expect(t).not.toContain('Bindings');
    expect(t).not.toContain('Security groups');
    expect(t).toContain('Secrets 1');
    expect(t).toContain('expense-service expense:1.4 · 2 replicas');
    expect(t).toContain('postgres postgres · 17 · small');
    expect(t).toContain('api-expense /api/expense → expense-service:http');
    expect(t).toContain('internal-token v3 of 3');
    expect(nav({ open: 'services/web' })).toContain('class="b-row-obj on"');
    expect((nav({}).match(/class="ghost mini b-add"/g) || []).length).toBe(5);
    expect(nav({ onAdd: null })).not.toContain('ghost mini b-add');
    const legacy = setObject(STORED, 'bindings', 'mail', { upstream: 'https://mail.example' });
    const lt = text(<ObjectNav spec={legacy} base={STORED} groups={none} open="" pins={[]} catalog={builtinCatalog()}
      marks={new Map()} onOpen={nop} onAdd={nop} />);
    expect(lt).toMatch(/Bindings 1 [+~−]? ?mail mail\.example/);
    expect((nav({ spec: legacy }).match(/class="ghost mini b-add"/g) || []).length).toBe(5);
    // An HTTP secret's pin says its kind.
    const http = [{ name: 'internal-token', type: 'http', pinned: 3, latest: 4, newer: true, missing: false }];
    expect(text(<ObjectNav spec={STORED} base={STORED} groups={none} open="" pins={http} catalog={builtinCatalog()}
      marks={new Map()} onOpen={nop} onAdd={nop} />)).toContain('internal-token HTTP · v3 · v4 newer');
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
    for (const s of ['Container', 'Variables', 'Scaling & resources', 'Health check', 'Rollout', 'Remove service expense-service']) {
      expect(out).toContain(s);
    }
    // No binding of the spec: no section of them.
    expect(out).not.toContain('Bindings in the spec');
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
      S4_URL: { secret: 's4', key: 'url' },
    } };
    const pins = [{ name: 'internal-token', pinned: 2, latest: 3, newer: true, missing: false }];
    const out = html(<ServiceForm name="x" obj={svc} ctx={ctx(STORED, [], { pins })} onChange={nop} onRename={nop} onRemove={nop} />);
    const rowOf = (name) => { const r = out.slice(out.indexOf(`value="${name}"`)); return r.slice(0, r.indexOf('</tr>')); };
    expect(rowOf('LOG_LEVEL')).toContain('<option value="text" selected="">Text</option>');
    expect(rowOf('LOG_LEVEL')).toContain('value="info"');
    expect(rowOf('FROM')).toContain('value="noreply@example.com"');
    expect(rowOf('TOKEN')).toContain('<option value="secret" selected="">Secret value</option>');
    expect(rowOf('TOKEN')).toContain('<option value="internal-token" selected="">internal-token</option>');
    // The HTTP secrets are no value to pick here (ADR 0055).
    expect(rowOf('TOKEN')).not.toContain('<option value="s4"');
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
    // An HTTP secret's URL: {secret, key}, with what the variable gets.
    expect(rowOf('S4_URL')).toContain('<option value="httpUrl" selected="">URL of an HTTP secret</option>');
    expect(rowOf('S4_URL')).toContain('<option value="httpHost">Host of an HTTP secret</option>');
  });

  it("give a variable an HTTP secret's URL or host from the environment's HTTP secrets (ADR 0055)", () => {
    const svc = { image: 'ghcr.io/acme/x:1', env: { S4_URL: { secret: 's4', key: 'url' }, OPENAI_HOST: { secret: 'openai', key: 'host' },
      GONE_URL: { secret: 'gone', key: 'url' } } };
    const spec = { ...STORED, secrets: { ...STORED.secrets, s4: { version: 2, type: 'http' } } };
    const pins = [{ name: 's4', type: 'http', pinned: 2, latest: 3, newer: true, missing: false }];
    const c = ctx(spec, [], { httpSecrets: HTTP, pins, project: 'kino', env: 'prod', knownSecrets: ['internal-token', 's4', 'openai'] });
    const out = html(<ServiceForm name="x" obj={svc} ctx={c} onChange={nop} onRename={nop} onRemove={nop} />);
    const rowOf = (name) => { const r = out.slice(out.indexOf(`value="${name}"`)); return r.slice(0, r.indexOf('</tr>')); };
    const t = (name) => rowOf(name).replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ');
    expect(rowOf('S4_URL')).toContain('<option value="s4" selected="">s4 · api.s4.example</option>');
    expect(rowOf('S4_URL')).toContain('<option value="openai">openai · api.openai.com</option>');
    expect(t('S4_URL')).toContain('gets http://s4');
    expect(t('S4_URL')).toContain('v2 pinned, v3 newer use v3');
    expect(rowOf('S4_URL')).toContain('href="/projects/kino/environments/prod/secrets/http/s4"');
    // In the intercept mode its own host; a host alone for key host.
    expect(rowOf('OPENAI_HOST')).toContain('<option value="httpHost" selected="">Host of an HTTP secret</option>');
    expect(t('OPENAI_HOST')).toContain('gets api.openai.com');
    // One the environment has not, and the way to a new one.
    expect(t('GONE_URL')).toContain('not an HTTP secret of the environment');
    expect(rowOf('GONE_URL')).toContain('href="/projects/kino/environments/prod/secrets/http/new"');
    // The value secrets stay the Secret value's choice.
    const tok = html(<ServiceForm name="x" obj={{ env: { T: { secret: 'internal-token' } } }} ctx={c} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(tok).not.toContain('<option value="s4"');
    expect(tok).not.toContain('<option value="openai"');
    // A new environment has none yet.
    const fresh = text(<ServiceForm name="x" obj={{ env: { U: { secret: '', key: 'url' } } }} ctx={ctx({}, [], { isNew: true })}
      onChange={nop} onRename={nop} onRemove={nop} />);
    expect(fresh).toContain('a new environment has no HTTP secrets yet: make them in its Secrets once it exists');
  });

  it('show the health check as its kind, the bindings of the spec read-only, the idle as a choice', () => {
    const spec = setObject(STORED, 'bindings', 'stripe', { credentialRef: { name: 'stripe', version: 1 } });
    const svc = { ...STORED.services.web, bindings: ['stripe', 'gone'], idle: '15m' };
    const out = html(<ServiceForm name="web" obj={svc} ctx={ctx(spec, [], { httpSecrets: HTTP, project: 'kino', env: 'prod' })}
      onChange={nop} onRename={nop} onRemove={nop} />);
    expect(out).toContain('<button class="ghost mini on" aria-pressed="true">HTTP</button>');
    expect(out).toContain('value="/readyz"');
    // A binding of the spec (ADR 0055): shown, no tick, no new one.
    expect(out).toContain('Bindings in the spec');
    expect(out).toMatch(/<span class="mono">stripe<\/span> <span class="muted small">HTTP secret stripe · v1<\/span>/);
    expect(out).toContain('not in the draft');
    expect(out).toContain('Bindings in the spec give way to HTTP secrets');
    const section = out.slice(out.indexOf('Bindings in the spec'), out.indexOf('Rollout'));
    expect(section).not.toContain('type="checkbox"');
    expect(section).not.toContain('<select');
    expect(out).toContain('<option value="after" selected="">After</option>');
    expect(out).toContain('value="15"');
    const none = html(<ServiceForm name="web" obj={{ image: 'x:1' }} ctx={ctx({})} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(none).toContain('<button class="ghost mini on" aria-pressed="true">None</button>');
    expect(none).not.toContain('Bindings in the spec');
  });

  it('show an attached service read-only, or as a form for those who write the environment', () => {
    const o = { runtime: 'attached', sandbox: 'abc', command: ['npm', 'run', 'dev'] };
    const ro = text(<ServiceForm name="web" obj={o} ctx={ctx({})} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(ro).toContain('It runs in sandbox abc of this environment. Changing it needs write on the environment');
    expect(ro).toContain('npm run dev');
    expect(html(<ServiceForm name="web" obj={o} ctx={ctx({})} onChange={nop} onRename={nop} onRemove={nop} />)).not.toContain('<input');
    const rw = html(<ServiceForm name="web" obj={o} ctx={ctx({}, [], { editAttached: true })} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(rw).toContain('value="abc"');
    expect(rw).toContain('Remove service web');
    // Its reachability is its sandbox's pod's: no uses, no egress of its own.
    expect(text(<ServiceForm name="web" obj={o} ctx={ctx({}, [], { editAttached: true })} onChange={nop} onRename={nop} onRemove={nop} />))
      .toContain('What else the pod reaches, and its egress, are the sandbox');
    expect(rw).not.toContain('aria-label="egress"');
    const own = text(<ServiceForm name="web" obj={{ ...o, uses: [{ addon: 'db' }] }} ctx={ctx({}, [], { editAttached: true })}
      onChange={nop} onRename={nop} onRemove={nop} />);
    expect(own).toContain('It names uses, which an attached service may not');
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
    expect(out).toContain('placeholder="every host" value=""');
    const hosted = html(<RouteForm name="git" obj={{ ...STORED.routes.root, host: 'git.example.com' }} ctx={ctx(STORED)}
      onChange={nop} onRename={nop} onRemove={nop} />);
    expect(hosted).toContain('value="git.example.com"');
  });

  it('show a binding of the spec read-only, with what it becomes and its findings', () => {
    const spec = setObject(setObject(STORED, 'bindings', 's4', { upstream: 'https://api.s4.example', credential: { kind: 'headers' } }),
      'services', 'web', { ...STORED.services.web, bindings: ['s4'] });
    const findings = [{ path: 'bindings.s4', severity: 'warning', problem: 'bindings in the spec give way to HTTP secrets (ADR 0055)',
      remedy: 'make an HTTP secret s4 and reference it by a variable {secret, key: url}' }];
    const c = ctx(spec, findings, { httpSecrets: HTTP, project: 'kino', env: 'prod' });
    const out = html(<BindingForm name="s4" obj={spec.bindings.s4} ctx={c} onChange={nop} onRename={nop} onRemove={nop} />);
    const t = text(<BindingForm name="s4" obj={spec.bindings.s4} ctx={c} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(t).toContain('Binding in the spec s4');
    expect(t).toContain('It becomes an HTTP secret');
    expect(t).toContain('HTTP secret s4 exists (api.s4.example, v3): reference it by a variable.');
    expect(out).toContain('href="/projects/kino/environments/prod/secrets/http/s4"');
    expect(t).toContain('service web');
    expect(t).toContain('make an HTTP secret s4 and reference it by a variable {secret, key: url}');
    expect(t).toContain('Remove binding s4');
    // Read-only: no field to change.
    expect(out).not.toContain('<input');
    expect(out).not.toContain('<select');
    // Without an HTTP secret of its name, the way to a new one; none in a new environment.
    const other = html(<BindingForm name="mail" obj={{ upstream: 'https://mail.example' }} ctx={c} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(other).toContain('href="/projects/kino/environments/prod/secrets/http/new"');
    const fresh = html(<BindingForm name="mail" obj={{ upstream: 'https://mail.example' }} ctx={ctx(spec, [], { isNew: true, project: 'kino' })}
      onChange={nop} onRename={nop} onRemove={nop} />);
    expect(fresh).not.toContain('href=');
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

  it("show an HTTP secret's pin and uses, never a value field: its key is written on its page", () => {
    const spec = setObject(STORED, 'services', 'web', { ...STORED.services.web, env: { S4_URL: { secret: 's4', key: 'url' } } });
    const refs = referencesTo(spec, 'secrets', 's4');
    const c = ctx(spec, [], { httpSecrets: HTTP, project: 'kino', env: 'prod' });
    const pin = { name: 's4', type: 'http', pinned: 2, latest: 3, newer: true, missing: false };
    const out = html(<SecretForm name="s4" pin={pin} refs={refs} ctx={c} onPin={nop} onRemove={nop} />);
    const t = text(<SecretForm name="s4" pin={pin} refs={refs} ctx={c} onPin={nop} onRemove={nop} />);
    expect(t).toContain('HTTP secret s4');
    expect(t).toContain('version 3, pinned 2 Use version 3');
    expect(t).toContain('upstream https://api.s4.example called as http://s4');
    expect(t).toContain('web · S4_URL → url of HTTP secret s4');
    expect(out).toContain('href="/projects/kino/environments/prod/secrets/http/s4"');
    expect(out).not.toContain('type="password"');
    expect(t).not.toContain('Save as a new version');
  });

  it('show what the code expects of a secret, and offer it for a missing one (ADR 0058)', () => {
    const spec = setObject(STORED, 'services', 'web', { ...STORED.services.web, env: { GH: { secret: 'gh', key: 'url' } } });
    const refs = referencesTo(spec, 'secrets', 'gh');
    const c = ctx(spec, [{ severity: 'error', path: 'secrets.gh.mode', rest: 'mode', problem: 'the mode differs', remedy: 'r' }],
      { httpSecrets: HTTP, project: 'kino', env: 'prod' });
    const pin = { name: 'gh', type: 'http', pinned: 0, latest: 0, newer: false, missing: true,
      expect: { type: 'http', mode: 'intercept' } };
    const out = html(<SecretForm name="gh" pin={pin} refs={refs} ctx={c} onPin={nop} onRemove={nop} />);
    const t = text(<SecretForm name="gh" pin={pin} refs={refs} ctx={c} onPin={nop} onRemove={nop} />);
    expect(t).toContain('the code expects an HTTP secret in the intercept mode');
    expect(t).toContain('the mode differs');
    expect(t).toContain('New HTTP secret gh, in the intercept mode');
    expect(out).toContain('href="/projects/kino/environments/prod/secrets/http/new?name=gh&amp;mode=intercept"');
    const value = text(<SecretForm name="internal-token" pin={{ name: 'internal-token', pinned: 2, latest: 3, expect: { type: 'value' } }}
      refs={[]} ctx={ctx(STORED)} onPin={nop} onRemove={nop} />);
    expect(value).toContain('the code expects a value');
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

