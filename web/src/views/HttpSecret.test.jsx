// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { ACCESS_HTTP } from '../testing/access.js';
import {
  CodeSees, Definition, HttpSecretChoice, HttpSecretForm, HttpSecrets, PreflightRows, SandboxHttpList, rebinds,
} from './HttpSecret.jsx';
import { formOf } from './httpSecret.js';

// An HTTP secret's pages (ADR 0055; the credentials of ADR 0033 before):
// the pieces the form and the page share, the pages as the Secrets view
// holds them, and the sandbox's side.

const html = (el) => renderToStaticMarkup(el);
const text = (el) => html(el).replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/&amp;/g, '&').replace(/\s+/g, ' ');
const nav = () => {};
const WRITE = { write: true };
const READ = { write: false };

const github = {
  name: 'github', type: 'http', upstream: 'https://api.github.com', mode: 'name', caPem: '', pathPrefixes: ['/user'], forwardedHost: false,
  rateLimit: null, credential: { kind: 'headers', headers: { Authorization: { set: true } } }, version: 3,
  usedBy: { sandboxes: [], objects: [], latest: 0, deployed: 0 },
};

describe('CodeSees', () => {
  it('the request without a key, and the masked echo', () => {
    const out = html(<CodeSees c={{ name: 'github', upstream: 'https://api.github.com', mode: 'name', pathPrefixes: ['/user'] }} />);
    expect(out).toContain('curl -s http://github/user');
    expect(out).toContain('# in the pod, no key anywhere');
    expect(out).toContain('redacted-by-FeOS-binding');
    expect(out).toContain('FeOS speaks TLS to the upstream');
  });

  it('the intercept mode calls the API\'s own host', () => {
    const out = html(<CodeSees c={{ name: 'openai', upstream: 'https://api.openai.com', mode: 'intercept', pathPrefixes: [] }} />);
    expect(out).toContain('curl -s https://api.openai.com/');
    expect(out).toContain('interception CA');
  });
});

describe('PreflightRows', () => {
  it('a failure shows its code and message', () => {
    const out = html(<PreflightRows result={{ ok: false, addresses: [], error: { code: 'tls_failed', message: 'x509: unknown authority' } }} />);
    expect(out).toContain('tls_failed');
    expect(out).toContain('x509: unknown authority');
    expect(out).toContain('✗');
  });
});

describe('Definition', () => {
  it('how the code calls it, its authentication; a secret field only as set', () => {
    const erp = ACCESS_HTTP.find((c) => c.name === 'erp');
    const t = text(<Definition cred={erp} />);
    expect(t).toContain('called as its own name, intercepted api.vendor.example:443');
    expect(t).toContain('authentication OAuth2 client credentials');
    expect(t).toContain('client secret set');
    expect(t).toContain('paths /odata/');
    expect(text(<Definition cred={github} />)).toContain("called as the secret's name http://github/…");
  });
});

describe("an environment's HTTP secrets", () => {
  it('has no list of its own: the list is the Secrets view\'s (EnvSecrets.test.jsx)', () => {
    expect(html(<HttpSecrets tenant="kino" env="prod" sel="" ec={WRITE} navigate={nav} />)).toBe('');
  });

  it("a new one's form: the API first, back to the environment's secrets", () => {
    const out = html(<HttpSecrets tenant="kino" env="prod" sel="new" ec={WRITE} navigate={nav} />);
    expect(out).toContain('New HTTP secret');
    expect(out).toContain('</svg> secrets</button>');
    expect(out).toContain('href="/tenants/kino/environments/prod/secrets">Cancel</a>');
    expect(out).toContain('A service gets it by a variable that references its URL');
    expect(out).toContain('An HTTP API the platform calls with the key');
    expect(out).toMatch(/<button type="submit" class="primary">/);
    // The API (upstream, how it is called) comes before its credential.
    expect(out.indexOf('how the code calls it')).toBeLessThan(out.indexOf('<legend>credential'));
    expect(out).not.toMatch(/>Credential</);
    const ro = html(<HttpSecrets tenant="kino" env="prod" sel="new" ec={READ} navigate={nav} />);
    expect(ro).toMatch(/<button type="submit" class="primary" disabled="" title="needs write on this environment">/);
  });

  it("a new version's form keeps the secret fields it leaves empty, its mode's change too", () => {
    const out = html(<HttpSecretForm tenant="kino" env="prod" cred={github} ec={WRITE} navigate={nav} />);
    expect(out).toContain('Edit HTTP secret');
    expect(out).toContain('version 3 → 4');
    expect(out).toContain('set — leave empty to keep');
    expect(out).toContain('Kept from version 3: <span class="mono">headers.Authorization</span>');
    expect(out).toContain('A change of the mode keeps the key: it goes to the same API.');
    expect(out).not.toContain('A service gets it by a variable');
  });

  it('a change of the origin, the CA or the token URL sends every secret field again', () => {
    const f = formOf(github);
    expect(rebinds(f, github)).toBe(false);
    expect(rebinds({ ...f, mode: 'intercept', forwardedHost: true, pathPrefixes: '/x', ratePerSecond: '3' }, github)).toBe(false);
    expect(rebinds({ ...f, upstream: 'https://api.github.com/' }, github)).toBe(false);
    expect(rebinds({ ...f, upstream: 'https://api.github.com:443' }, github)).toBe(false);
    expect(rebinds({ ...f, upstream: 'https://evil.example' }, github)).toBe(true);
    expect(rebinds({ ...f, caPem: '-----BEGIN CERTIFICATE-----' }, github)).toBe(true);
    const erp = ACCESS_HTTP.find((c) => c.name === 'erp');
    expect(rebinds({ ...formOf(erp), tokenUrl: 'https://other.example/token' }, erp)).toBe(true);
    expect(rebinds(f, null)).toBe(false);
  });
});

describe("a sandbox's HTTP secrets", () => {
  it('names the HTTP secrets of its environment, each at the latest, and offers the others', () => {
    const out = html(<SandboxHttpList tenant="kino" env="prod" names={['models']} all={ACCESS_HTTP} write why={undefined} navigate={nav} />);
    const t = text(<SandboxHttpList tenant="kino" env="prod" names={['models']} all={ACCESS_HTTP} write why={undefined} navigate={nav} />);
    expect(t).toContain('models https://models.example http://models/ v1 · the latest Remove');
    expect(out).toContain('href="/tenants/kino/environments/prod/secrets/http/models"');
    expect(out).toContain('<option value="erp">erp · https://api.vendor.example</option>');
    expect(out).not.toContain('<option value="models">');
    expect(t).toContain("the environment's secrets →");
    expect(text(<SandboxHttpList tenant="kino" env="prod" names={[]} all={[]} write navigate={nav} />))
      .toContain('It names no HTTP secret. the environment has no HTTP secret yet');
  });

  it("a new sandbox's form picks them", () => {
    const out = html(<HttpSecretChoice tenant="kino" env="prod" list={ACCESS_HTTP} value={['erp']} onChange={nav} navigate={nav} />);
    expect(out).toContain('<legend>HTTP secrets <span class="muted">· optional</span></legend>');
    expect(out).toMatch(/<input type="checkbox" checked=""\/><span><b class="mono">erp<\/b>/);
    expect(out).toContain('The sandbox gets each at its latest version');
    const none = html(<HttpSecretChoice tenant="kino" env="prod" list={[]} value={[]} onChange={nav} navigate={nav} />);
    expect(none).toContain('The environment has no HTTP secret yet.');
    expect(none).toContain('href="/tenants/kino/environments/prod/secrets/http/new"');
  });
});
