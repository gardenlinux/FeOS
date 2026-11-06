// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import {
  behind, bindingOf, credentialPath, curlOf, emptyForm, formOf, hostOf, isName, keptOf, modeWords, needsDeployment,
  newCredentialPath, oldestPin, pinState, pinWords, pinnedSpec, preflightOf, preflightRows, prefixesOf, specSnippet,
  usedWords,
} from './credentials.js';

// The credentials' derivations, from credentials as GET
// /api/v1/credentials answers them (go/internal/bindings, credentialOut).

const github = {
  name: 'github', upstream: 'https://api.github.com', mode: 'name', caPem: '', pathPrefixes: ['/repos/kino/', '/user'],
  forwardedHost: false, rateLimit: { perSecond: 10, burst: 20 },
  credential: { kind: 'headers', headers: { Authorization: { set: true } } },
  version: 3, createdBy: 'malte@example.com', createdAt: '2026-11-06T10:00:00Z',
  usedBy: { sandboxes: ['h3k9v2m7qpx'], environments: [{ id: 'e1', name: 'prod', bindings: ['github'], latest: 2, deployed: 2 }] },
};

const partner = {
  name: 'partner', upstream: 'https://api.partner.example.net:8443', mode: 'name', caPem: '', pathPrefixes: [],
  forwardedHost: true, rateLimit: null,
  credential: { kind: 'oauth2', tokenUrl: 'https://auth.partner.example.net/token', clientId: 'kino', clientSecret: { set: true }, scope: 'a b' },
  version: 2,
};

describe('the form and the binding', () => {
  it('a new credential: the form as the binding, every value in it', () => {
    const f = { ...emptyForm(), name: 'github', upstream: 'https://api.github.com/', pathPrefixes: '/repos/kino/, /user',
      ratePerSecond: '10', headers: [{ name: 'Authorization', value: 'Bearer ghp_x', set: false }] };
    expect(bindingOf(f)).toEqual({
      upstream: 'https://api.github.com', mode: 'name', pathPrefixes: ['/repos/kino/', '/user'],
      rateLimit: { perSecond: 10, burst: 10 }, credential: { kind: 'headers', headers: { Authorization: 'Bearer ghp_x' } },
    });
  });

  it('a read as a form: the secret fields empty and marked set', () => {
    const f = formOf(github);
    expect(f.headers).toEqual([{ name: 'Authorization', value: '', set: true }]);
    expect(f.pathPrefixes).toBe('/repos/kino/ /user');
    expect([f.ratePerSecond, f.rateBurst]).toEqual(['10', '20']);
    expect(formOf(partner).set).toEqual({ clientSecret: true });
  });

  it('a secret field left empty is {"set": true}, a typed one its value', () => {
    expect(bindingOf(formOf(github)).credential).toEqual({ kind: 'headers', headers: { Authorization: { set: true } } });
    expect(keptOf(formOf(github))).toEqual(['headers.Authorization']);
    const f = { ...formOf(partner), clientSecret: 'n3w' };
    expect(bindingOf(f).credential).toEqual({ kind: 'oauth2', tokenUrl: 'https://auth.partner.example.net/token',
      clientId: 'kino', clientSecret: 'n3w', scope: 'a b' });
    expect(keptOf(f)).toEqual([]);
    expect(bindingOf(formOf(partner)).forwardedHost).toBe(true);
  });

  it('a new header needs its value; another kind keeps nothing', () => {
    const f = formOf(github);
    f.headers = [...f.headers, { name: 'X-Org', value: '', set: false }];
    expect(() => bindingOf(f)).toThrow('the header X-Org needs a value');
    const basic = { ...formOf(github), kind: 'basic', user: 'bot' };
    expect(bindingOf(basic).credential).toEqual({ kind: 'basic', user: 'bot' });
  });

  it('PEM fields end in one newline; forwarded host is the name mode\'s', () => {
    const f = { ...emptyForm(), kind: 'clientCertificate', upstream: 'https://mtls.example.com', mode: 'intercept',
      forwardedHost: true, certPem: '  CERT  ', keyPem: 'KEY\n\n', caPem: 'CA' };
    const b = bindingOf(f);
    expect(b.credential).toEqual({ kind: 'clientCertificate', certPem: 'CERT\n', keyPem: 'KEY\n' });
    expect(b.caPem).toBe('CA\n');
    expect(b.forwardedHost).toBeUndefined();
  });

  it('refuses a form no request should carry', () => {
    expect(() => bindingOf(emptyForm())).toThrow('the upstream is required');
    expect(() => bindingOf({ ...emptyForm(), upstream: 'https://x.example', headers: [] })).toThrow('add a header');
    expect(() => bindingOf({ ...formOf(github), rateBurst: '5', ratePerSecond: '' })).toThrow('a burst needs a rate');
    expect(() => bindingOf({ ...formOf(github), ratePerSecond: '1.5' })).toThrow('whole number');
  });

  it('names are DNS labels; prefixes split at blanks and commas', () => {
    expect(isName('github')).toBe(true);
    expect(isName('GitHub')).toBe(false);
    expect(isName('-x')).toBe(false);
    expect(prefixesOf(' /a, /b\n/c ')).toEqual(['/a', '/b', '/c']);
  });
});

describe('preflightOf', () => {
  it('a form with every value is preflighted as it is', () => {
    const f = { ...formOf(github), headers: [{ name: 'Authorization', value: 'Bearer new', set: true }] };
    expect(preflightOf(f, github)).toEqual({ how: 'form', binding: bindingOf(f) });
  });

  it('kept secrets and nothing else changed: the stored credential\'s check', () => {
    expect(preflightOf(formOf(github), github)).toEqual({ how: 'stored' });
  });

  it('kept secrets with a change: their values are needed', () => {
    expect(preflightOf({ ...formOf(github), pathPrefixes: '/orgs/' }, github)).toEqual({ how: 'needs', fields: ['headers.Authorization'] });
  });
});

describe('what the code sees', () => {
  it('calls the name in the name mode, the host in the intercept mode', () => {
    expect(curlOf(github)).toBe('curl -s http://github/repos/kino/');
    expect(curlOf({ ...github, mode: 'intercept' })).toBe('curl -s https://api.github.com/repos/kino/');
    expect(curlOf({ ...partner, mode: 'intercept' })).toBe('curl -s https://api.partner.example.net:8443/');
    expect(curlOf({ ...emptyForm(), pathPrefixes: '' })).toBe('curl -s http://<name>/');
    expect(modeWords(github)).toBe('http://github/…');
    expect(modeWords({ ...github, mode: 'intercept' })).toBe('intercepts api.github.com');
    expect(hostOf('https://API.example.com:443/')).toBe('api.example.com');
    expect(hostOf('nope')).toBe('');
  });
});

describe('the uses', () => {
  it('in words', () => {
    expect(usedWords(github.usedBy)).toBe('1 sandbox · 1 environment');
    expect(usedWords({ sandboxes: ['a', 'b'], environments: [] })).toBe('2 sandboxes');
    expect(usedWords(undefined)).toBe('unused');
  });

  it('an environment behind the latest, needing a deployment or rolling out', () => {
    const prod = { latest: 2, deployed: 2 };
    expect([behind(prod, 3), needsDeployment(prod, 3), pinState(prod, 3), oldestPin(prod)]).toEqual([true, true, 'behind', 2]);
    const rolling = { latest: 3, deployed: 2 };
    expect([behind(rolling, 3), needsDeployment(rolling, 3), pinState(rolling, 3), oldestPin(rolling)])
      .toEqual([true, false, 'rolling out', 2]);
    expect(pinState({ latest: 3, deployed: 3 }, 3)).toBe('current');
    // A deployed deployment pins it, the latest no longer: nothing to move.
    expect([behind({ latest: 0, deployed: 1 }, 3), needsDeployment({ latest: 0, deployed: 1 }, 3)]).toEqual([true, false]);
    expect(pinWords(prod)).toBe('v2');
    expect(pinWords(rolling)).toBe('latest deployment v3 · deployed v2');
    expect(pinWords({ latest: 0, deployed: 1 })).toBe('deployed v1');
  });

  it('moves every binding that names the credential, and nothing else', () => {
    const spec = {
      services: { api: { bindings: ['gh', 'pay'] } },
      bindings: {
        gh: { tenantCredential: { name: 'github', version: 2 }, upstream: 'https://api.github.com', mode: 'name' },
        gh2: { tenantCredential: { name: 'github', version: 1 } },
        pay: { upstream: 'https://api.stripe.com', credential: { kind: 'basic', user: 'u', password: { secret: 'stripe' } } },
      },
    };
    const out = pinnedSpec(spec, 'github', 3);
    expect(out.bindings).toEqual(['gh', 'gh2']);
    expect(out.spec.bindings.gh).toEqual({ tenantCredential: { name: 'github', version: 3 } });
    expect(out.spec.bindings.gh2).toEqual({ tenantCredential: { name: 'github', version: 3 } });
    expect(out.spec.bindings.pay).toEqual(spec.bindings.pay);
    expect(spec.bindings.gh.tenantCredential.version).toBe(2);
    expect(pinnedSpec({ services: {} }, 'github', 3).bindings).toEqual([]);
  });

  it('the spec\'s two places, as the builder takes them', () => {
    expect(specSnippet('github')).toBe(
      '"bindings": { "github": { "tenantCredential": { "name": "github" } } }\n"services": { "api": { "bindings": ["github"] } }');
  });
});

describe('preflightRows', () => {
  it('an ok answer: addresses, TLS, token and the probe', () => {
    const rows = preflightRows({
      ok: true, addresses: ['140.82.121.6'], tls: { subject: 'CN=api.github.com', issuer: 'CN=Sectigo', notAfter: '2027-02-14T23:59:59Z' },
      token: { expiresIn: 3600 }, probe: { status: 200, latencyMs: 84 }, error: null,
    }, { method: 'GET', path: '/user' });
    expect(rows.map((r) => [r.key, r.ok, r.text])).toEqual([
      ['addresses', true, '140.82.121.6'],
      ['TLS', true, 'CN=api.github.com · CN=Sectigo · until 2027-02-14'],
      ['token', true, 'fetched · expires in 3600s'],
      ['probe', true, 'GET /user → 200 in 84 ms'],
    ]);
  });

  it('a failure: its code with its message', () => {
    const rows = preflightRows({ ok: false, addresses: [], tls: null, token: null, probe: null,
      error: { code: 'destination_denied', message: 'the platform\'s own networks are no destination' } });
    expect(rows).toEqual([{ key: 'destination_denied', ok: false, text: 'the platform\'s own networks are no destination' }]);
    expect(preflightRows({ ok: true, addresses: [], probe: { status: 401, latencyMs: 12 } })[0].ok).toBe(false);
    expect(preflightRows(null)).toEqual([]);
  });
});

describe('paths', () => {
  it('the list, a credential, its form, a new one', () => {
    expect(credentialPath('kino')).toBe('/tenants/kino/credentials');
    expect(credentialPath('kino', 'github')).toBe('/tenants/kino/credentials/github');
    expect(credentialPath('kino', 'github', true)).toBe('/tenants/kino/credentials/github/edit');
    expect(newCredentialPath('kino')).toBe('/tenants/kino/new-credential');
  });
});
