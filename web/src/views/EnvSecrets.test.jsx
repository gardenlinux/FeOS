// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { ACCESS_SECRETS, ACCESS_SPEC } from '../testing/access.js';
import { response } from '../testing/ferun.js';
import { readCatalog } from './catalog.js';
import { AddSecret, SecretsInventory } from './EnvSecrets.jsx';

// An environment's Secrets view (ADR 0055), with ferund's answers of the
// catalog and the bindings' use, and the spec and secrets of
// testing/access.js: every key once, grouped by where it lives, with
// what a writer may do and a reader may not.

const T = 'fecode';
const E = 'expensa-stage';
const nav = () => {};
const CATALOG = readCatalog(response('GET_addons.200'));
const USAGE = response('GET_environments_env_bindings_usage.200');
const W = { write: true, admin: false, del: false, clone: false, operator: false };
const R = { write: false, admin: false, del: false, clone: false, operator: false };

const html = (el) => renderToStaticMarkup(el);
const plain = (s) => s.replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/&quot;/g, '"')
  .replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&').replace(/\s+/g, ' ');
const text = (el) => plain(html(el));
const view = (extra = {}) => (
  <SecretsInventory project={T} env={E} deployment={7} spec={ACCESS_SPEC} secrets={ACCESS_SECRETS}
    catalog={CATALOG} usage={USAGE} ec={W} onPut={nav} onDelete={nav} navigate={nav} {...extra} />);
// group is the markup of one group of the view, by its place.
const group = (out, place) => {
  const i = out.indexOf(`id="inv-${place}"`);
  const j = out.indexOf('<section class="envsect inv-group"', i + 1);
  return out.slice(i, j < 0 ? undefined : j);
};
// row is the markup of a row, by its element ID.
const row = (out, id) => {
  const i = out.indexOf(`id="${id}"`);
  return out.slice(i, out.indexOf('</tr>', i));
};
// names are a group's rows' names, in their order.
const names = (g) => [...g.matchAll(/<tr id="inv-(\w+)-([^"]+)"/g)].map((m) => `${m[1]} ${m[2]}`);

describe('the Secrets view', () => {
  it('says the marks once, counts each place, and what stands out: a missing value, an API in trouble', () => {
    const out = html(view());
    expect(out.split('class="mark-legend"').length - 1).toBe(1);
    expect(text(view())).toContain('1 by identity 5 at the node 2 in the container 1 unused ▲ 1 missing ▲ 1 with problems');
    // The groups, by where the key lives, in their order.
    const order = ['identity', 'offloaded', 'container', 'unused'].map((g) => out.indexOf(`id="inv-${g}"`));
    expect(order).toEqual([...order].sort((a, b) => a - b));
    expect(order.every((i) => i > 0)).toBe(true);
    expect(out.indexOf('mark-legend')).toBeLessThan(order[0]);
    // Each heads with its mark.
    expect(group(out, 'offloaded')).toMatch(/^id="inv-offloaded"><div class="sect sect-row"><span class="badge mark mark-offloaded"[^>]*><svg[^]*?<\/svg> At the node<\/span>/);
  });

  it('shows each key exactly once, under where it lives', () => {
    const out = html(view());
    expect(names(group(out, 'identity'))).toEqual(['addon db']);
    expect(names(group(out, 'offloaded'))).toEqual(['http erp', 'http models', 'http spare-api', 'binding mail', 'secret ghcr-pull']);
    expect(names(group(out, 'container'))).toEqual(['secret session-key', 'addon bus']);
    expect(names(group(out, 'unused'))).toEqual(['secret leftover']);
    // The value secret a binding of the spec holds is in its row, no row of its own.
    expect(out).not.toContain('id="inv-secret-mail-pass"');
    for (const n of ['erp', 'models', 'spare-api', 'mail', 'ghcr-pull', 'session-key', 'leftover', 'db', 'bus']) {
      expect(out.match(new RegExp(`<tr id="inv-\\w+-${n}"`, 'g'))).toHaveLength(1);
    }
  });

  it('an HTTP secret: its URL and how it is called, its authentication, who uses it at which version, its 24 h', () => {
    const out = html(view());
    const erp = plain(row(out, 'inv-http-erp'));
    expect(erp).toMatch(/erp HTTP secret api\.vendor\.example intercepted OAuth2 service api · ERP_URL sandbox c3kv9mwq2hd · the latest v4 .* deployed pins v3, latest v4 1,204 req\/24h · 2 refused · 1 blocked/);
    expect(row(out, 'inv-http-erp')).toContain(`href="/projects/${T}/environments/${E}/secrets/http/erp"`);
    expect(row(out, 'inv-http-erp')).toContain(`href="/projects/${T}/environments/${E}/objects/service/api"`);
    expect(row(out, 'inv-http-erp')).toContain(`href="/projects/${T}/environments/${E}/sandboxes/c3kv9mwq2hd"`);
    expect(row(out, 'inv-http-erp')).toContain(`href="/projects/${T}/environments/${E}/logs?source=egress&amp;since=1d"`);
    // Check, and the deployment that moves the pins to the latest.
    expect(row(out, 'inv-http-erp')).toContain('Check</button>');
    expect(row(out, 'inv-http-erp')).toContain('Move to v4</button>');
    // By its name: http://<name>; one only a sandbox names; one no one uses.
    expect(plain(row(out, 'inv-http-models'))).toMatch(/models HTTP secret models\.example as http:\/\/models headers sandbox c3kv9mwq2hd · the latest v1/);
    expect(plain(row(out, 'inv-http-spare-api'))).toMatch(/spare-api HTTP secret spare\.example as http:\/\/spare-api basic no one v1 .* · unused/);
    expect(out.split('Move to v').length - 1).toBe(1);
  });

  it('a binding of the spec: what it reaches, its key, that it becomes an HTTP secret, its 24 h and Check', () => {
    const out = html(view());
    const mail = row(out, 'inv-binding-mail');
    expect(mail).toMatch(/^id="inv-binding-mail" class="inv-missing">/);
    expect(plain(mail)).toMatch(/mail binding in the spec mail\.example as http:\/\/mail basic key from secret mail-pass \(no value\) in the spec — becomes an HTTP secret service api missing its key has no value no request in 24h/);
    expect(mail).toContain('Check</button>');
    expect(mail).toMatch(/<button class="mini primary" title="the first version of secret mail-pass">.*Set mail-pass<\/button>/);
    expect(mail).not.toContain('New deployment');
    // Once an HTTP secret of its name is there, it says so.
    const taken = html(view({ secrets: [...ACCESS_SECRETS, { name: 'mail', type: 'http', version: 1, upstream: 'https://mail.example',
      mode: 'name', credential: { kind: 'basic' }, usedBy: { sandboxes: [], objects: [], latest: 0, deployed: 0 } }] }));
    expect(plain(row(taken, 'inv-binding-mail'))).toContain('in the spec — HTTP secret mail takes its place once a deployment references it');
    // Its key's value is set in its row.
    const editing = html(view({ editing: 'mail-pass' }));
    expect(editing).toContain('placeholder="the new value of mail-pass"');
  });

  it('a value secret: where it lives by its uses, who uses it and how, its version against the pin', () => {
    const out = html(view());
    expect(plain(row(out, 'inv-secret-session-key'))).toMatch(/session-key value service api · SESSION_KEY v3 .* the latest deployment pins v2, latest v3/);
    expect(plain(row(out, 'inv-secret-ghcr-pull'))).toMatch(/ghcr-pull value service api · image pull v1/);
    expect(plain(row(out, 'inv-secret-leftover'))).toMatch(/leftover value no one v1 .* · unused/);
    expect(row(out, 'inv-secret-session-key')).toContain('title="version 3 set by fecode:0193a000-aaaa-7bbb-8ccc-000000000001');
    // Never a value: no field of one until Set value opens it.
    expect(out.split('type="password"').length - 1).toBe(0);
    const editing = html(view({ editing: 'session-key' }));
    expect(editing.split('type="password"').length - 1).toBe(1);
    expect(editing).toContain('placeholder="the new value of session-key"');
    expect(editing).toContain('shown never again once stored');
  });

  it('addons: by identity with each user\'s role, or in the container with the password', () => {
    const out = html(view());
    expect(plain(row(out, 'inv-addon-db'))).toMatch(/db addon postgres job migrate · role owner service api · database app · role api_svc its certificate/);
    expect(plain(row(out, 'inv-addon-bus'))).toMatch(/bus addon nats service api its password/);
    expect(row(out, 'inv-addon-db')).toContain(`href="/projects/${T}/environments/${E}/objects/addon/db"`);
    // Without the catalog yet, the places an addon may take wait.
    expect(plain(group(html(view({ catalog: null })), 'identity'))).toContain('By identity · … ');
  });

  it('a writer sets, deletes, checks and adds; a reader sees, with the reason on each disabled button', () => {
    const w = html(view());
    expect(w).toContain('Set value');
    expect(w).toContain('Delete');
    expect(w).toContain('Add secret');
    const r = html(view({ ec: R }));
    expect(r).toMatch(/<button class="primary mini" disabled="" title="needs write on this environment">.*Add secret/);
    expect(row(r, 'inv-secret-session-key')).toMatch(/<button class="mini" disabled="" title="needs write on this environment">.*Set value/);
    expect(row(r, 'inv-http-erp')).toMatch(/disabled="" title="needs write on this environment">.*Check/);
    expect(row(r, 'inv-http-erp')).toMatch(/<button class="primary mini" disabled="" title="needs write on this environment">Move to v4/);
    expect(row(r, 'inv-binding-mail')).toMatch(/disabled="" title="needs write on this environment">.*Check/);
  });

  it("a binding a sandbox stated itself shows from its requests, at the node", () => {
    const usage = { ...USAGE, bindings: [...USAGE.bindings,
      { name: 'own', requests: 9, refused: 0, failing: 0, tokenFailed: 0, blocked: 0, lastAt: '2026-11-19T10:00:00Z', lastProblem: null,
        users: [{ kind: 'sandbox', name: 'c3kv9mwq2hd', requests: 9 }] }] };
    const g = group(html(view({ usage })), 'offloaded');
    expect(g).toContain('<tr id="inv-binding-own" class="inv-stray">');
    expect(plain(g)).toMatch(/own a sandbox's own binding .* sandbox c3kv9mwq2hd 9 req\/24h/);
    expect(text(view({ usage }))).toContain('6 at the node');
  });

  it('marks the row its address points at', () => {
    expect(html(view({ focus: 'binding/mail' }))).toContain('<tr id="inv-binding-mail" class="sel inv-missing">');
    expect(html(view({ focus: 'secret/leftover' }))).toContain('<tr id="inv-secret-leftover" class="sel">');
  });

  it('says why there are no numbers, and what an environment without a deployment holds', () => {
    const down = response('GET_environments_env_bindings_usage.200.unavailable');
    expect(text(view({ usage: down }))).toContain('No numbers of the last 24 h: the log store does not answer; try again shortly');
    const none = html(view({ spec: null, deployment: 0, usage: null }));
    expect(names(group(none, 'offloaded'))).toEqual(['http erp', 'http models', 'http spare-api']);
    expect(names(group(none, 'unused'))).toEqual(['secret ghcr-pull', 'secret leftover', 'secret session-key']);
    // ferund's usedBy still names the services an HTTP secret's deployments use it in.
    expect(plain(row(none, 'inv-http-erp'))).toContain('service api');
    expect(plain(group(none, 'identity'))).toContain('No addon admits a workload by its certificate.');
    expect(plain(group(none, 'container'))).toContain('No key in the container.');
  });
});

describe('Add secret', () => {
  const add = (initial) => <AddSecret project={T} env={E} ec={W} onPut={nav} navigate={nav} initial={initial} />;

  it('asks the kind first: an HTTP API (recommended for an API) or a value the code reads', () => {
    const out = html(add('ask'));
    const t = text(add('ask'));
    expect(t).toContain('What is it?');
    expect(t).toContain('An HTTP API the platform calls with the key recommended for an API');
    expect(t).toContain('A value your code reads');
    expect(out.indexOf('An HTTP API')).toBeLessThan(out.indexOf('A value your code reads'));
    expect(out).toContain('class="inv-add-choice rec"');
    expect(out).not.toContain('type="password"');
  });

  it('a value: its name and value here; at first only the button', () => {
    const v = html(add('value'));
    expect(v).toContain('placeholder="new secret&#x27;s name"');
    expect(v.split('type="password"').length - 1).toBe(1);
    expect(html(add(''))).toMatch(/^<button class="primary mini" title="a value, or an HTTP API and its key">.*Add secret<\/button>$/);
  });
});
