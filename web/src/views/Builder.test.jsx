// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { fixture, response } from '../testing/ferun.js';
import { JsonView, ObjectList, ReviewPanel, secretLines } from './Builder.jsx';
import { AddonForm, RouteForm, SecretForm, ServiceForm } from './BuilderForms.jsx';
import { checkNewer } from './pins.js';
import { builtinCatalog, readCatalog } from './catalog.js';
import { groupFindings } from './findings.js';
import { deployBlock, draftWords, proposeMessage, setObject } from './draft.js';
import { referencesTo } from './refs.js';

// The builder's views, rendered with ferund's fixtures: the list of
// objects with their references and findings, the forms with findings at
// their fields, the plan with its diff, findings and apply.

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
  spec, catalog: builtinCatalog(), findings, setSecret: async () => 1, isNew: false, knownSecrets: ['internal-token'], ...extra,
});

describe('ObjectList', () => {
  it('offers no adds without onAdd (a read-only builder)', () => {
    const out = html(<ObjectList spec={STORED} groups={none} open="" pins={[]} catalog={builtinCatalog()} onOpen={nop} onAdd={null} />);
    expect(out).not.toContain('addon</button>');
    expect(out).toContain('class="b-card"');
    expect(html(<JsonView spec={STORED} onSpec={nop} readOnly />)).toContain('readonly=""');
  });

  it('lists the objects by kind as cards with their references as lines', () => {
    const pins = [{ name: 'internal-token', pinned: 3, latest: 3, newer: false, missing: false }];
    const out = text(<ObjectList spec={STORED} groups={none} open="services/web" pins={pins} catalog={builtinCatalog()} onOpen={nop} onAdd={nop} />);
    expect(out).toContain('services · 3');
    expect(out).toContain('jobs · 1');
    expect(out).toContain('addons · 2');
    expect(out).toContain('routes · 3');
    expect(out).toContain('secrets · 1');
    expect(out).toContain('EXPENSA_DATABASE_URL → postgres · url (database expensa, role expense_svc)');
    expect(out).toContain('ghcr.io/acme/expensa-web:1.4 · pinned · http:8080');
    expect(out).toContain('/api/expense → expense-service:http');
    expect(out).toContain('pinned 3 · latest 3');
    expect(out).toContain('approval-service · APPROVAL_INTERNAL_TOKEN → secret internal-token');
    expect(html(<ObjectList spec={STORED} groups={none} open="services/web" pins={pins} catalog={null} onOpen={nop} onAdd={nop} />))
      .toContain('class="b-card on"');
  });

  it('marks the objects with findings, and secrets missing or newer than their pins', () => {
    const groups = groupFindings(INVALID.findings, WRITTEN);
    const pins = [{ name: 'gone', pinned: 0, latest: 0, newer: false, missing: true },
      { name: 'internal-token', pinned: 2, latest: 3, newer: true, missing: false }];
    const spec = setObject(WRITTEN, 'secrets', 'internal-token', { version: 2 });
    const out = html(<ObjectList spec={{ ...spec, services: { ...spec.services, x: { env: { T: { secret: 'gone' } } } } }}
      groups={groups} open="" pins={pins} catalog={null} onOpen={nop} onAdd={nop} />);
    expect(out).toContain('class="b-card error"');
    expect(out).toContain('2 errors');
    expect(out).toContain('<span class="badge bad">missing</span>');
    expect(out).toContain('<span class="badge warn">v3 newer</span>');
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
    const markup = html(<ServiceForm name="expense-service" obj={STORED.services['expense-service']}
      ctx={ctx(STORED, f)} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(markup).toContain('value="ghcr.io/acme/expense:1.4"');
    // The finding of a variable sits under its row, the idle's under idle.
    expect(markup.indexOf('no addon mysql')).toBeGreaterThan(markup.indexOf('value="EXPENSA_DATABASE_URL"'));
    expect(markup.indexOf('nothing would wake it')).toBeLessThan(markup.indexOf('variables'));
    const unpinned = text(<ServiceForm name="web" obj={checkNewer(STORED.services.web)} ctx={ctx(STORED)}
      onChange={nop} onRename={nop} onRemove={nop} />);
    expect(unpinned).toContain('any registry: the plan resolves it to a digest');
    expect(unpinned).not.toContain('Check for a newer image');
  });

  it('show an attached service without editing it', () => {
    const out = text(<ServiceForm name="web" obj={{ runtime: 'attached', sandbox: 'abc', command: ['npm'] }} ctx={ctx({})}
      onChange={nop} onRename={nop} onRemove={nop} />);
    expect(out).toContain('An attached service runs in sandbox abc');
  });

  it('offer an addon the catalog versions, plans and option fields', () => {
    const catalog = readCatalog({ addons: [{ type: 'postgres', versions: ['16', '17'], defaultVersion: '17', defaultPlan: 'small',
      plans: [{ name: 'small', vcpus: 1, memoryMib: 1024, volumeMib: 10240 }], outputKeys: ['url'], outputFields: [],
      options: [{ name: 'databases', kind: 'list', required: false, default: [], description: '', values: [], pattern: '^[a-z]+$',
        reserved: ['postgres'], reservedPrefixes: [], unique: true }] }] });
    const out = html(<AddonForm name="postgres" obj={STORED.addons.postgres} ctx={ctx(STORED, [], { catalog })}
      onChange={nop} onRename={nop} onRemove={nop} />);
    expect(out).toContain('<option value="small" selected="">small · 1 vCPU, 1024 MiB, volume 10 GiB</option>');
    expect(out).toContain('>databases<');
    expect(out).not.toContain('is reserved');
    const hinted = text(<AddonForm name="postgres" obj={{ ...STORED.addons.postgres, options: { databases: ['postgres', 'x'] } }}
      ctx={ctx(STORED, [], { catalog })} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(hinted).toContain('postgres is reserved');
    // roles is no field of this catalog's: it is JSON.
    expect(out).toContain('other options (JSON)');
    expect(out).toContain('&quot;roles&quot;');
    expect(out).not.toContain('answers no catalog');
    const builtin = text(<AddonForm name="nats" obj={STORED.addons.nats} ctx={ctx(STORED)} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(builtin).toContain('This ferund answers no catalog of addons');
  });

  it('offer a route the services and their ports', () => {
    const out = html(<RouteForm name="root" obj={STORED.routes.root} ctx={ctx(STORED)} onChange={nop} onRename={nop} onRemove={nop} />);
    expect(out).toContain('<option value="web" selected="">web</option>');
    expect(out).toContain('<option value="http" selected="">http</option>');
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
    expect(t).toContain('Review deployment 7 against deployment 6');
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
    expect(t).toContain('Review deployment 1 of a new environment');
    expect(t).toContain('everything is added with deployment 1');
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

  it('folds a secret card\'s registry logins into one line', () => {
    const spec = setObject(STORED, 'services', 'web', { ...STORED.services.web,
      imageCredentials: { username: 'ci', password: { secret: 'internal-token' } } });
    expect(secretLines(referencesTo(spec, 'secrets', 'internal-token'))).toEqual([
      'approval-service · APPROVAL_INTERNAL_TOKEN → secret internal-token',
      'expense-service · EXPENSA_INTERNAL_TOKEN → secret internal-token',
      'registry login of web',
    ]);
  });
});

describe('JsonView', () => {
  it('shows the draft in the API order', () => {
    const out = html(<JsonView spec={{ routes: { r: { port: 'http', path: '/' } }, services: {} }} onSpec={nop} />);
    expect(out).toContain('{\n  &quot;services&quot;: {},\n  &quot;routes&quot;: {\n    &quot;r&quot;: {\n      &quot;path&quot;: &quot;/&quot;');
  });
});

