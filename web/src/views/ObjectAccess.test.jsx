// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { ACCESS_SECRETS as SECRETS, ACCESS_SPEC as SPEC } from '../testing/access.js';
import { response } from '../testing/ferun.js';
import { readCatalog } from './catalog.js';
import { AccessCard, KeysCount } from './ObjectAccess.jsx';

// A service's and a job's Secrets section of its Config, and its count
// on its overview, with ferund's answers of the catalog, the identities
// and the bindings' use, and a spec whose names are theirs
// (testing/access.js).

const T = 'fecode';
const E = 'expensa-stage';
const nav = () => {};
const NOW = Date.parse('2026-11-19T12:00:00Z');
const CATALOG = readCatalog(response('GET_addons.200'));
const IDS = response('GET_environments_env_identities.200');
const USAGE = response('GET_environments_env_bindings_usage.200');

const html = (el) => renderToStaticMarkup(el);
const text = (el) => html(el).replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/&quot;/g, '"')
  .replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&').replace(/\s+/g, ' ');
const card = (kind, name, extra = {}) => (
  <AccessCard tenant={T} env={E} kind={kind} name={name} spec={SPEC} catalog={CATALOG} secrets={SECRETS} identities={IDS}
    usage={USAGE} now={NOW} navigate={nav} {...extra} />);

describe("a service's Secrets", () => {
  it('heads as Secrets, where its keys live, with the way to all of the environment\'s', () => {
    const out = html(card('service', 'api'));
    expect(text(card('service', 'api'))).toMatch(/^ Secrets where its keys live All of the environment's Identity /);
    expect(out).toContain(`href="/tenants/${T}/environments/${E}/secrets" class="small sg-all">All of the environment&#x27;s</a>`);
  });

  it('says first who it is: its SPIFFE ID, copyable, and when its first certificate ends, per pod on hover', () => {
    const out = html(card('service', 'api'));
    const all = text(card('service', 'api'));
    expect(out).toContain('<span class="mono small acc-spiffe">spiffe://run.fecore.dev/tenant/fecode/env/2f0c6d1e-5b0a-4c55-9d43-0d1b7a3e8f21/service/api</span>');
    expect(out).toContain('title="copy the SPIFFE ID"');
    expect(all).toMatch(/Identity .* cert until \S+ \S+ in 21h, the first of 2 pods/);
    expect(out).toContain('d7r2m4k9pqs on node-a: until 2026-11-20');
    expect(out).toContain('f8s3n5m1rtv on node-b: until 2026-11-20');
  });

  it('unfolds its certificate: the subject (CN the pod, OU the workload), the URI, the issuer, who reads which part, each pod', () => {
    const api = IDS.workloads.find((w) => w.kind === 'service' && w.name === 'api');
    const out = html(card('service', 'api'));
    const all = text(card('service', 'api'));
    expect(out).toContain('<details class="acc-certd"><summary>Certificate</summary>');
    expect(all).toContain(`Subject CN=run.fecore.dev/d7r2m4k9pqs,OU=${api.ou}`);
    expect(all).toContain('CN run.fecore.dev/d7r2m4k9pqs · the pod: each its own');
    expect(all).toContain(`OU ${api.ou} · the workload: every pod of it the same`);
    expect(all).toContain(`URI ${api.spiffeId} · the SPIFFE ID, its only name`);
    expect(all).toContain('Issuer CN=FeOS issuer run.fecore.dev');
    expect(all).toContain('Key Ed25519, made in the pod, never leaves it');
    // The postgres addon maps the OU to the role the reference names.
    expect(all).toContain('Read by db (postgres) maps the OU to role api_svc');
    expect(all).toMatch(/d7r2m4k9pqs 5f1c9a2e7b3d4c6e8a0b1c2d3e4f5a6b 2026-11-19 \S+ 2026-11-20 \S+ in 21h node-a/);
    expect(all).toContain('f8s3n5m1rtv a03e6d9c1b7f42e58d6c0b9a8f7e6d5c');
  });

  it("unfolds what a ferund of before the subject answers: the URI, the ends", () => {
    const old = { trustDomain: IDS.trustDomain, workloads: IDS.workloads.map(({ ou, pods, ...w }) => ({
      ...w, pods: pods.map(({ pod, notAfter, node }) => ({ pod, notAfter, node })) })) };
    const all = text(card('service', 'api', { identities: old }));
    expect(all).toContain('Certificate URI spiffe://');
    expect(all).not.toContain('Subject');
    expect(all).not.toContain('Issuer');
    expect(all).toMatch(/d7r2m4k9pqs … 2026-11-20 \S+ in 21h node-a/);
  });

  it('lists what it reaches sorted by where the key lives, each with its mark', () => {
    const out = html(card('service', 'api'));
    const marks = [...out.matchAll(/badge mark mark-(\w+)/g)].map((m) => m[1]);
    expect(marks).toEqual(['identity', 'offloaded', 'offloaded', 'offloaded', 'container', 'container']);
    const rows = [...out.matchAll(/<tr class="acc-row acc-(\w+)">/g)].map((m) => m[1]);
    expect(rows).toEqual(['addon', 'http', 'image', 'binding', 'addon', 'secret']);
  });

  it('an HTTP secret: the variable that gets it, its upstream as the code calls it, the pinned version, its 24 h', () => {
    const out = html(card('service', 'api'));
    const all = text(card('service', 'api'));
    expect(all).toContain('ERP_URL → api.vendor.example at its own name, intercepted at the node');
    expect(all).toContain('OAuth2 · HTTP secret erp v3 · latest v4 · its url');
    expect(out).toContain(`href="/tenants/${T}/environments/${E}/secrets/http/erp"`);
    expect(all).toContain('1,204 req/24h · 2 refused · 1 blocked');
    expect(out).toContain('<span class="bad"> · 2 refused · 1 blocked</span>');
    expect(out).toContain('last problem 2026-11-19');
    expect(out).toContain('403 denied_path: path /admin is not allowed by binding erp');
    expect(out).toContain('sent by service api 1,200 · sandbox c3kv9mwq2hd 4');
    // Its outbound requests in its logs, and the last problem's trace.
    expect(out).toContain(`href="/tenants/${T}/environments/${E}/objects/service/api/logs?source=egress&amp;since=1d"`);
    expect(out).toContain(`href="/tenants/${T}/environments/${E}/traces/4bf92f3577b34da6a3ce929d0e0e4736"`);
  });

  it('a binding of the spec, still shown: its upstream, its key from secrets, what becomes of it, its 24 h', () => {
    const out = html(card('service', 'api'));
    const all = text(card('service', 'api'));
    expect(out).toContain(`href="/tenants/${T}/environments/${E}/secrets/binding/mail"`);
    expect(all).toContain('mail → mail.example reached as http://mail at the node basic · secret mail-pass');
    expect(all).toContain('binding in the spec — becomes an HTTP secret');
    expect(out).toContain(`href="/tenants/${T}/environments/${E}/secrets/secret/mail-pass"`);
    expect(all).toContain('no request in 24h');
  });

  it('an HTTP secret a binding of the spec names, through it; one not in the environment says so', () => {
    const spec = { ...SPEC, services: { api: { ...SPEC.services.api, env: {}, bindings: ['gh'] } },
      bindings: { gh: { credentialRef: { name: 'models', version: 1 } } } };
    const all = text(card('service', 'api', { spec }));
    expect(all).toContain('binding gh → models.example reached as http://models at the node headers · HTTP secret models v1');
    expect(all).toContain('through binding gh in the spec — becomes a variable');
    const gone = text(card('service', 'api', { secrets: SECRETS.filter((x) => x.name !== 'erp') }));
    expect(gone).toContain('HTTP secret erp v3 · not in this environment');
  });

  it('an addon by its type and fields, a secret by its variables and pinned version, the registry login at the node', () => {
    const out = html(card('service', 'api'));
    const all = text(card('service', 'api'));
    expect(out).toContain(`href="/tenants/${T}/environments/${E}/objects/addon/db"`);
    expect(all).toContain('db → postgres by identity database app · role api_svc · DATABASE_URL');
    expect(all).toContain('bus → nats in the container BUS_URL');
    expect(all).toContain('SESSION_KEY ← session-key in the container secret v2');
    expect(out).toContain(`href="/tenants/${T}/environments/${E}/secrets/secret/session-key"`);
    expect(all).toContain('image pull ← ghcr-pull at the node the node pulls with it');
    // A plain variable hands over no key: no row.
    expect(all).not.toContain('PLAIN');
  });

  it('says the numbers are missing while the log store does not answer, and why', () => {
    const down = response('GET_environments_env_bindings_usage.200.unavailable');
    const all = text(card('service', 'api', { usage: down }));
    expect(all).toContain('No numbers of the last 24 h: the log store does not answer; try again shortly');
    expect(all).not.toContain('req/24h');
  });

  it('waits for the catalog before it marks an addon', () => {
    const out = html(card('service', 'api', { catalog: null }));
    expect(out).toContain('<td class="acc-mark"><span class="muted">…</span></td>');
  });

  it('one that names nothing says so in one line', () => {
    const all = text(card('service', 'web'));
    expect(all).toContain('It names no secret or addon: it holds no key and needs none.');
    expect(html(card('service', 'web'))).not.toContain('acc-tbl');
  });

  it('a service of an installation without an issuer has no identity', () => {
    const all = text(card('service', 'api', { identities: { trustDomain: '', workloads: [] } }));
    expect(all).toContain('Identity none: this installation issues no workload certificates');
    expect(text(card('service', 'api', { identities: null, identitiesErr: 'boom' }))).toContain('Identity not known: boom');
  });
});

describe("a job's Secrets", () => {
  it('its identity without a pod, its addon by identity with its role', () => {
    const all = text(card('job', 'migrate'));
    expect(all).toContain('spiffe://run.fecore.dev/tenant/fecode/env/2f0c6d1e-5b0a-4c55-9d43-0d1b7a3e8f21/job/migrate');
    expect(all).toContain('no pod now');
    expect(all).toContain('db → postgres by identity role owner · DATABASE_URL');
  });
});

describe("an object's keys on its overview", () => {
  const line = (kind, name, extra = {}) => (
    <KeysCount tenant={T} env={E} kind={kind} name={name} spec={SPEC} catalog={CATALOG} usage={USAGE} navigate={nav} {...extra} />);

  it('counts its targets by where the key lives and leads to its Config', () => {
    const all = text(line('service', 'api'));
    expect(all).toMatch(/^ Keys: 1 by identity · 3 at the node · 2 in the container /);
    expect(all).toContain('— each with its source, its identity and certificate: Config');
    expect(html(line('service', 'api'))).toContain(`href="/tenants/${T}/environments/${E}/objects/service/api/config"`);
    expect(text(line('job', 'migrate'))).toMatch(/^ Keys: 1 by identity — /);
  });

  it('names an HTTP secret or binding whose upstream refused or failed in the last 24 h', () => {
    expect(html(line('service', 'api'))).toContain('<span class="bad">erp: 2 refused · 1 blocked</span>');
    expect(html(line('service', 'api', { usage: null }))).not.toContain('class="bad"');
  });

  it('counts an addon once the catalog says its mark, and is not there for one that names nothing', () => {
    expect(text(line('service', 'api', { catalog: null }))).toMatch(/^ Keys: 3 at the node · 1 in the container /);
    expect(html(line('service', 'web'))).toBe('');
    expect(html(line('service', 'api', { spec: null }))).toBe('');
  });
});
