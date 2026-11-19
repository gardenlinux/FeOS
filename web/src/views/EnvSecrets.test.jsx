// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { ACCESS_CREDENTIALS, ACCESS_SPEC } from '../testing/access.js';
import { response } from '../testing/ferun.js';
import { readCatalog } from './catalog.js';
import { SecretsInventory } from './EnvSecrets.jsx';

// An environment's Secrets view, with ferund's answers of the catalog and
// the bindings' use, and the spec and credentials of testing/access.js:
// every key by where it lives, grouped by kind, with what a writer may
// do and a reader may not.

const T = 'fecode';
const E = 'expensa-stage';
const nav = () => {};
const CATALOG = readCatalog(response('GET_addons.200'));
const USAGE = response('GET_environments_env_bindings_usage.200');
const SECRETS = [
  { name: 'session-key', version: 3, createdBy: 'fecode:0193a000-aaaa-7bbb-8ccc-000000000001', createdAt: '2026-11-19T08:00:00Z' },
  { name: 'ghcr-pull', version: 1, createdBy: 'fecode:x', createdAt: '2026-11-01T08:00:00Z' },
  { name: 'leftover', version: 1, createdBy: 'fecode:x', createdAt: '2026-10-01T08:00:00Z' },
];
const W = { write: true, admin: false, del: false, clone: false, operator: false };
const R = { write: false, admin: false, del: false, clone: false, operator: false };

const html = (el) => renderToStaticMarkup(el);
const text = (el) => html(el).replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/&quot;/g, '"')
  .replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&').replace(/\s+/g, ' ');
const view = (extra = {}) => (
  <SecretsInventory tenant={T} env={E} deployment={7} spec={ACCESS_SPEC} secrets={SECRETS} credentials={ACCESS_CREDENTIALS}
    catalog={CATALOG} usage={USAGE} ec={W} onPut={nav} onDelete={nav} navigate={nav} {...extra} />);
// group is the markup of one group of the view.
const group = (out, id) => {
  const i = out.indexOf(`id="inv-${id}"`);
  const j = out.indexOf('<section class="envsect inv-group"', i + 1);
  return out.slice(i, j < 0 ? undefined : j);
};
const plain = (s) => s.replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/&amp;/g, '&').replace(/\s+/g, ' ');

describe('the Secrets view', () => {
  it('says the marks once, then counts what it holds; a missing value and a binding in trouble stand out', () => {
    const out = html(view());
    expect(out.split('class="mark-legend"').length - 1).toBe(1);
    expect(text(view())).toContain('4 secrets ▲ 1 missing 2 credentials 2 bindings of deployment 7 ▲ 1 with problems');
    expect(out.indexOf('mark-legend')).toBeLessThan(out.indexOf('id="inv-secrets"'));
    // The groups, in their order.
    const order = ['secrets', 'credentials', 'bindings', 'addons'].map((g) => out.indexOf(`id="inv-${g}"`));
    expect(order).toEqual([...order].sort((a, b) => a - b));
    expect(order.every((i) => i > 0)).toBe(true);
  });

  it('secrets: each with where it lives, who uses it and how, its version against what the deployment pins', () => {
    const g = group(html(view()), 'secrets');
    const all = plain(g);
    expect(all).toMatch(/session-key in the container service api · SESSION_KEY v3 .* the latest deployment pins v2, latest v3/);
    expect(g).toContain(`href="/tenants/${T}/environments/${E}/objects/service/api"`);
    expect(all).toMatch(/ghcr-pull offloaded service api · image pull v1/);
    expect(all).toMatch(/leftover — no one v1 .* · unused/);
    expect(g).toContain('title="version 3 set by fecode:0193a000-aaaa-7bbb-8ccc-000000000001');
    // Never a value: the one field of a value is the new secret's, empty;
    // a new version's opens on Set value, never shown again.
    expect(g.split('type="password"').length - 1).toBe(1);
    const editing = group(html(view({ editing: 'session-key' })), 'secrets');
    expect(editing.split('type="password"').length - 1).toBe(2);
    expect(editing).toContain('placeholder="the new value of session-key"');
    expect(editing).toContain('shown never again once stored');
  });

  it('a secret the spec names without a value is missing, its row marked, its first value one click away', () => {
    const g = group(html(view()), 'secrets');
    expect(g).toContain('<tr id="inv-secret-mail-pass" class="inv-missing">');
    expect(g).toContain('<span class="badge bad">missing</span> <span class="bad">named by the spec, no value</span>');
    expect(plain(g)).toContain('mail-pass offloaded service api · binding mail');
    expect(g).toContain(`href="/tenants/${T}/environments/${E}/secrets/binding/mail"`);
    expect(g).toContain('<button class="mini primary" title="its first version">');
    // A missing one has nothing to delete.
    const row = g.slice(g.indexOf('inv-secret-mail-pass'), g.indexOf('</tr>', g.indexOf('inv-secret-mail-pass')));
    expect(row).not.toContain('Delete');
  });

  it('a writer sets, deletes and adds; a reader sees, with the reason on each disabled button', () => {
    const w = group(html(view()), 'secrets');
    expect(w).toContain('Set value');
    expect(w).toContain('Delete');
    expect(w).toContain('Add secret');
    const r = html(view({ ec: R }));
    expect(group(r, 'secrets')).toMatch(/<button class="mini" disabled="" title="needs write on this environment">.*Set value/);
    expect(group(r, 'secrets')).toContain('<form class="inlineform envsecret-form" title="needs write on this environment">');
    expect(group(r, 'credentials')).toMatch(/<button class="mini" disabled="" title="needs write on this environment">.*New credential/);
    expect(group(r, 'bindings')).toMatch(/disabled="" title="needs write on this environment">.*Check/);
    expect(r).not.toContain('Add binding');
    expect(r).not.toContain('Edit in New deployment');
    expect(group(r, 'credentials')).toMatch(/<button class="primary mini" disabled="" title="needs write on this environment">Update to v4/);
  });

  it('credentials: what each reaches, its users and sandboxes, the version its deployments pin and Update to vN', () => {
    const g = group(html(view()), 'credentials');
    const all = plain(g);
    expect(g).toContain(`href="/tenants/${T}/environments/${E}/credentials/erp"`);
    expect(all).toMatch(/erp offloaded api\.vendor\.example intercepts api\.vendor\.example · OAuth2 client credentials service api through binding erp v4 .* deployed pins v3, latest v4 Update to v4/);
    expect(all).toMatch(/models offloaded models\.example http:\/\/models\/… · headers sandbox c3kv9mwq2hd v1/);
    expect(g).toContain(`href="/tenants/${T}/environments/${E}/sandboxes/c3kv9mwq2hd"`);
    expect(g).toContain('title="state an API and its credential once"');
    expect(g.split('Update to v').length - 1).toBe(1);
  });

  it('bindings: where each key comes from, who names it, its last 24 h, Check and its form', () => {
    const g = group(html(view()), 'bindings');
    const all = plain(g);
    expect(all).toMatch(/erp offloaded api\.vendor\.example intercepted OAuth2 · credential erp v3 · latest v4 service api 1,204 req\/24h · 2 refused · 1 blocked/);
    expect(all).toMatch(/mail offloaded mail\.example as http:\/\/mail basic · secret mail-pass service api no request in 24h/);
    expect(g).toContain(`href="/tenants/${T}/environments/${E}/logs?source=egress&amp;since=1d"`);
    expect(g).toContain(`href="/tenants/${T}/environments/${E}/builder/binding/erp"`);
    expect(g).toContain('Add binding');
    expect(g.split('Check</button>').length - 1).toBe(2);
  });

  it("a binding a sandbox named itself shows from its requests, with its credential and the sandbox", () => {
    const usage = { ...USAGE, bindings: [...USAGE.bindings,
      { name: 'models', requests: 9, refused: 0, failing: 0, tokenFailed: 0, blocked: 0, lastAt: '2026-11-19T10:00:00Z', lastProblem: null,
        users: [{ kind: 'sandbox', name: 'c3kv9mwq2hd', requests: 9 }] }] };
    const g = group(html(view({ usage })), 'bindings');
    expect(g).toContain('<tr id="inv-binding-models" class="inv-stray">');
    expect(plain(g)).toMatch(/models offloaded a sandbox's own credential models sandbox c3kv9mwq2hd 9 req\/24h/);
  });

  it('addons: by identity or with a password, each user with its role', () => {
    const g = group(html(view()), 'addons');
    const all = plain(g);
    expect(all).toMatch(/db by identity postgres job migrate · role owner service api · database app · role api_svc/);
    expect(all).toMatch(/bus in the container nats service api/);
    expect(g).toContain(`href="/tenants/${T}/environments/${E}/objects/addon/db"`);
    // Without the catalog yet, it waits.
    expect(plain(group(html(view({ catalog: null })), 'addons'))).toContain('Addons · … ');
  });

  it('marks the row its address points at', () => {
    expect(html(view({ focus: 'binding/erp' }))).toContain('<tr id="inv-binding-erp" class="sel">');
    expect(html(view({ focus: 'secret/mail-pass' }))).toContain('<tr id="inv-secret-mail-pass" class="sel inv-missing">');
  });

  it('says why there are no numbers, and what an environment without a deployment holds', () => {
    const down = response('GET_environments_env_bindings_usage.200.unavailable');
    expect(text(view({ usage: down }))).toContain('No numbers of the last 24 h: the log store does not answer; try again shortly');
    const none = html(view({ spec: null, deployment: 0, credentials: [], usage: null }));
    expect(plain(group(none, 'secrets'))).toMatch(/session-key — no one v3 .* · unused/);
    expect(plain(group(none, 'bindings'))).toContain("No bindings: no service's calls carry a credential of the platform.");
    expect(plain(group(none, 'credentials'))).toContain('No credential in this environment yet.');
  });
});
