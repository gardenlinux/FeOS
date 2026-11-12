// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { ApiError } from '../api.js';
import { MeContext } from '../people.js';
import { ProviderDefinition, ProviderForm, ProviderListView, ProviderView, Providers } from './Providers.jsx';

// The access providers' pages (ADR 0054), as ferund answers them: the
// list with ferun first, a provider's page, the form; reads for every
// member, writes for the tenant's admin.

const T = 'acme';
const nav = () => {};
const ME = (role, environments = []) => ({
  me: { person: 'p', method: 'oidc', admin: false, tenants: [{ tenant: T, role, environments }] }, reload: async () => {},
});
const render = (me, el) => renderToStaticMarkup(<MeContext.Provider value={me}>{el}</MeContext.Provider>);
const text = (s) => s.replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/&quot;/g, '"').replace(/&lt;/g, '<')
  .replace(/&gt;/g, '>').replace(/&amp;/g, '&').replace(/\s+/g, ' ');

const LIST = {
  providers: [
    { name: 'ferun', kind: 'ferun', builtIn: true, environments: [{ id: 'e1', name: 'stage' }] },
    { name: 'fecode', kind: 'tenant', builtIn: false, url: 'https://fecode.dev/api/v1/visit',
      environments: [{ id: 'e2', name: 'kino--stage' }], createdAt: '2026-11-12T20:00:00Z', updatedAt: '2026-11-12T20:00:00Z' },
    { name: 'sso', kind: 'oidc', builtIn: false, issuer: 'https://login.example.com/', clientId: 'abc', clientSecret: { set: true },
      rule: { domains: ['example.com'] }, backchannelUri: 'https://auth.fecore.dev/backchannel/acme/sso', environments: [],
      createdAt: '2026-11-12T20:00:00Z', updatedAt: '2026-11-12T21:00:00Z' },
  ],
  kinds: ['tenant', 'ferun', 'oidc'],
  redirectUri: 'https://auth.fecore.dev/callback',
};
const [FERUN_P, FECODE, SSO] = LIST.providers;

describe('the providers list', () => {
  it('lists ferun first, built in, then the tenant\'s with kind, where, rule and use', () => {
    const out = render(ME('admin'), <ProviderListView tenant={T} list={LIST} err={null} navigate={nav} />);
    expect(out).toContain('<h1 class="">Access providers</h1>');
    expect(out.indexOf('>ferun</a>')).toBeLessThan(out.indexOf('>fecode</a>'));
    expect(out.indexOf('>fecode</a>')).toBeLessThan(out.indexOf('>sso</a>'));
    expect(out).toContain('<span class="badge prov-badge">built in</span>');
    expect(out).toContain('href="/tenants/acme/providers/fecode"');
    const all = text(out);
    expect(all).toContain('ferun built in FeRun sign-in this installation people with a grant on the environment 1 environment');
    expect(all).toContain('fecode your own service https://fecode.dev/api/v1/visit whom the service says 1 environment');
    expect(all).toContain('sso OpenID Connect https://login.example.com/ verified addresses at example.com no environment');
    expect(out).toContain('title="your own service, or an OpenID Connect issuer"');
    expect(out).not.toContain('not served by this installation');
    expect(out).not.toContain('No provider of the tenant');
  });

  it('greys the kinds this installation does not serve, and says when the tenant has made none', () => {
    const out = render(ME('admin'), <ProviderListView tenant={T} list={{ ...LIST, providers: [FERUN_P], kinds: ['tenant'] }} err={null} navigate={nav} />);
    expect(out).toContain('class="clickable prov-off"');
    expect(out).toContain('not served by this installation');
    expect(text(out)).toContain('No provider of the tenant\'s own yet.');
  });

  it('offers New provider to the tenant\'s admin only', () => {
    const read = render(ME('read'), <ProviderListView tenant={T} list={LIST} err={null} navigate={nav} />);
    expect(read).toContain('class="button primary disabled"');
    expect(read).toContain('title="needs admin on this tenant"');
    // An environment's admin alone reads them too: it chooses among them.
    const envAdmin = render(ME('', [{ id: 'e1', name: 'stage', role: 'admin' }]),
      <ProviderListView tenant={T} list={LIST} err={null} navigate={nav} />);
    expect(envAdmin).toContain('>fecode</a>');
    expect(envAdmin).toContain('title="needs admin on this tenant"');
  });

  it('says a ferund without them has no route for them', () => {
    const e = new ApiError('no such route: GET /api/v1/providers', 404);
    const out = text(render(ME('admin'), <ProviderListView tenant={T} list={null} err={e} navigate={nav} />));
    expect(out).toContain('Access providers are not available on this ferund: it has no route for them yet.');
    const other = text(render(ME('admin'), <ProviderListView tenant={T} list={null} err={new ApiError('boom', 500)} navigate={nav} />));
    expect(other).toContain('boom');
  });
});

describe("a provider's page", () => {
  const page = (p, extra = {}, me = ME('admin')) => render(me, <ProviderView tenant={T} name={p ? p.name : 'nope'} p={p} err={null}
    note={null} kinds={LIST.kinds} redirectUri={LIST.redirectUri} navigate={nav} onDeleted={nav} {...extra} />);

  it("shows ferun as built in: its rule, the environments that name it, nothing to edit or delete", () => {
    const out = page(FERUN_P);
    expect(out).toContain('<span class="badge acc">built in</span>');
    expect(out).not.toContain('Edit');
    expect(out).not.toContain('Delete provider');
    expect(out).toContain('href="/tenants/acme/environments/stage/access"');
    expect(text(out)).toContain('rule a grant of at least read on the environment or the tenant, asked at every renewal');
  });

  it("shows a service's address and how it is asked, Edit and Delete for the tenant's admin", () => {
    const out = page(FECODE);
    expect(text(out)).toContain('address https://fecode.dev/api/v1/visit');
    expect(text(out)).toContain('a POST of the visit\'s ID, no credential');
    expect(out).toContain('href="/tenants/acme/providers/fecode/edit"');
    expect(out).toContain('Delete provider');
    const read = page(FECODE, {}, ME('read'));
    expect(read).toContain('class="button primary disabled"');
    expect(read).toMatch(/<button class=" danger" disabled="" title="needs admin on this tenant">/);
  });

  it("shows an issuer's client, its secret only as set, and the URIs ferund names to register", () => {
    const out = page(SSO);
    const all = text(out);
    expect(all).toContain('issuer https://login.example.com/');
    expect(all).toContain('client ID abc');
    expect(out).toContain('<span class="cred-set">set</span>');
    expect(all).toContain('redirect URI https://auth.fecore.dev/callback');
    expect(all).toContain('back-channel logout https://auth.fecore.dev/backchannel/acme/sso');
    expect(all).toContain('No environment admits its visitors.');
    // Without ferun-auth its kind is not served.
    expect(text(page(SSO, { kinds: ['tenant'] }))).toContain('Its kind is not served by this installation');
  });

  it('says a provider the tenant does not have, and a ferund without the routes', () => {
    expect(text(page(null, { name: 'nope', err: new ApiError('no such provider', 404) }))).toContain('No provider nope in this tenant.');
    expect(text(page(null, { err: new ApiError('no such route: GET /api/v1/providers/x', 404) })))
      .toContain('Access providers are not available on this ferund');
  });

  it('never composes a URI to register: without ferund\'s, it says none', () => {
    const out = text(render(ME('admin'), <ProviderDefinition p={{ ...SSO, backchannelUri: '' }} redirectUri="" />));
    expect(out).toContain('redirect URI —');
    expect(out).toContain('back-channel logout —');
  });
});

describe('the form', () => {
  const form = (p, kinds = LIST.kinds, me = ME('admin')) => render(me, <ProviderForm tenant={T} p={p} kinds={kinds}
    redirectUri={LIST.redirectUri} navigate={nav} />);

  it('makes a new one, a service of the tenant first, its address explained', () => {
    const out = form(null);
    expect(out).toContain('New access provider');
    expect(out).toContain('placeholder="members"');
    expect(out).toContain('<button type="button" class="on" title="">your own service</button>');
    const all = text(out);
    expect(all).toContain('the browser comes there with a challenge (a GET), ferund with a visit');
    expect(all).toContain('{"admitted": true} or {"admitted": false}');
    expect(all).toContain('ferund does not dial the cluster\'s or the nodes\' networks');
    expect(out).toMatch(/<button type="submit" class="primary">/);
  });

  it('disables OpenID Connect where ferun-auth does not run', () => {
    const out = form(null, ['tenant']);
    expect(out).toContain('disabled="" title="not served by this installation">OpenID Connect — not served by this installation</button>');
  });

  it("edits an issuer's provider: the secret kept when left empty, the rule, the URIs to register", () => {
    const out = form(SSO);
    expect(out).toContain('Edit access provider');
    expect(out).not.toContain('placeholder="members"');
    expect(out).toContain('type="password"');
    expect(out).toContain('placeholder="set — leave empty to keep"');
    expect(out).not.toContain('abc"' + ' type="password"');
    expect(out).toContain('<button type="button" class="on">email domains</button>');
    expect(out).toContain('>example.com</textarea>');
    const all = text(out);
    expect(all).toContain('https://auth.fecore.dev/callback');
    expect(all).toContain('https://auth.fecore.dev/backchannel/acme/sso');
    expect(all).toContain('A change of the issuer, the client ID or the rule ends the visits it admitted');
  });

  it("disables Save below the tenant's admin", () => {
    expect(form(FECODE, LIST.kinds, ME('write'))).toMatch(/<button type="submit" class="primary" disabled="" title="needs admin on this tenant">/);
  });

  it('shows ferun\'s page, never its form', () => {
    const out = render(ME('admin'), <Providers tenant={T} sel="ferun" edit navigate={nav} />);
    expect(out).not.toContain('Edit access provider');
  });
});

// The pages call the providers' routes of the client and no other.
describe('the routes they call', () => {
  it('are the providers\' four', () => {
    const src = readFileSync(new URL('./Providers.jsx', import.meta.url), 'utf8');
    const used = [...new Set([...src.matchAll(/\bapi\.(\w+)\(/g)].map((m) => m[1]))].sort();
    expect(used).toEqual(['deleteProvider', 'getProvider', 'listProviders', 'putProvider']);
  });
});
